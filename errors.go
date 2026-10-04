package dalgo2postgres

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// newCollectionNotFoundError formats the standard "collection not
// found" error. The contract is content-based per the Feature spec:
// the message MUST contain the substring "not found" and the collection name.
// label is the collection as [collectionLabel] writes it.
func newCollectionNotFoundError(label string) error {
	return fmt.Errorf("dalgo2postgres: collection %s not found", label)
}

// collectionLabel writes a collection for an error message: the quoted name,
// preceded by the quoted schema when the collection reference named one
// (`"sales"."Album"`). A schema that only came from the Database's
// configuration is not repeated: the caller chose it once, for every table.
func collectionLabel(schema, name string) string {
	if schema == "" {
		return fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf("%q.%q", schema, name)
}

// redactedMark replaces every part of a connection string found in driver
// error text.
const redactedMark = "[redacted]"

// connectionError is an error about opening a connection. Its text has been
// redacted (see [dsnDetails.wrap]); the driver error stays reachable through
// Unwrap, so errors.Is and errors.As keep working (for example to read the
// SQLSTATE of a *pgconn.PgError). Callers that print the unwrapped driver error
// bypass the redaction and must not do so.
type connectionError struct {
	msg   string
	cause error
}

func (e *connectionError) Error() string { return e.msg }
func (e *connectionError) Unwrap() error { return e.cause }

// dsnDetails is what may be said, and what must not be said, about a connection
// string in error text. Only the host and the database name may be named.
type dsnDetails struct {
	parsed  bool
	target  string // `host "h", database "d"`; empty unless parsed
	secrets []string
	cause   error // why it could not be parsed
}

// inspectDSN parses dsn the way the driver does. Nothing taken from a DSN that
// cannot be parsed is ever named.
func inspectDSN(dsn string) dsnDetails {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return dsnDetails{cause: err}
	}
	secrets := []string{dsn, cfg.User, cfg.Password}
	if cfg.Port != 0 {
		secrets = append(secrets, strconv.Itoa(int(cfg.Port)))
	}
	for key, value := range settingsOf(dsn) {
		if key != "host" && key != "dbname" && key != "database" {
			secrets = append(secrets, value)
		}
	}
	return dsnDetails{
		parsed:  true,
		target:  fmt.Sprintf("host %q, database %q", cfg.Host, cfg.Database),
		secrets: secrets,
	}
}

// unparsable is the error for a connection string that cannot be parsed. It
// names nothing from the string, and neither does it repeat the driver's parse
// error, which quotes the string.
func (d dsnDetails) unparsable() error {
	return &connectionError{msg: "dalgo2postgres: the connection string cannot be parsed", cause: d.cause}
}

// wrap describes a failure of op against the DSN: the host and database name,
// and the driver's error text with every other part of the connection string
// removed.
func (d dsnDetails) wrap(op string, cause error) error {
	return &connectionError{
		msg:   fmt.Sprintf("dalgo2postgres: %s (%s): %s", op, d.target, redact(cause.Error(), d.secrets)),
		cause: cause,
	}
}

// redact replaces each secret, in its plain and its URL-escaped spelling, with
// [redactedMark]. Longer secrets are replaced first so that one containing
// another is not left half visible.
func redact(text string, secrets []string) string {
	var forms []string
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		forms = append(forms, secret)
		for _, escaped := range []string{url.QueryEscape(secret), url.PathEscape(secret)} {
			if escaped != secret {
				forms = append(forms, escaped)
			}
		}
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, form := range forms {
		text = strings.ReplaceAll(text, form, redactedMark)
	}
	return text
}

// settingsOf returns the settings a connection string carries, best effort:
// it is used only to find what to hide, so a malformed string yields what could
// be read. A URL puts the user, password and port in settings of their own.
func settingsOf(dsn string) map[string]string {
	settings := map[string]string{}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return settings
		}
		if u.User != nil {
			if name := u.User.Username(); name != "" {
				settings["user"] = name
			}
			if password, ok := u.User.Password(); ok {
				settings["password"] = password
			}
		}
		if port := u.Port(); port != "" {
			settings["port"] = port
		}
		for key, values := range u.Query() {
			settings[key] = strings.Join(values, ",")
		}
		return settings
	}
	return keywordSettings(dsn)
}

// keywordSettings reads a libpq keyword/value connection string:
// key = value pairs, a value optionally in single quotes, a backslash escaping
// the next character.
func keywordSettings(dsn string) map[string]string {
	settings := map[string]string{}
	rest := dsn
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		keyEnd := strings.IndexAny(rest, "= \t\r\n")
		if keyEnd < 0 {
			return settings
		}
		key := rest[:keyEnd]
		rest = strings.TrimLeft(rest[keyEnd:], " \t\r\n")
		if !strings.HasPrefix(rest, "=") {
			return settings
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		var value strings.Builder
		quoted := strings.HasPrefix(rest, "'")
		if quoted {
			rest = rest[1:]
		}
		i := 0
		for ; i < len(rest); i++ {
			c := rest[i]
			if c == '\\' && i+1 < len(rest) {
				i++
				c = rest[i]
			} else if quoted && c == '\'' || !quoted && strings.ContainsRune(" \t\r\n", rune(c)) {
				break
			}
			value.WriteByte(c)
		}
		settings[key] = value.String()
		rest = rest[min(i+1, len(rest)):]
	}
}
