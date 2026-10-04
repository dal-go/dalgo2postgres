package dalgo2postgres

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// errUnparsable is the error for a connection string the driver cannot parse.
// It names nothing and carries no cause: the driver's parse error holds the
// whole string, and offers errors.Is and errors.As nothing else.
var errUnparsable = errors.New("dalgo2postgres: the connection string cannot be parsed")

// credentialSettings are the settings whose values are removed wherever they
// appear in driver text, as substrings. Every other setting value (the port, a
// timeout, sslmode, a runtime parameter) is removed only where it stands alone:
// see [redactTokens].
var credentialSettings = map[string]bool{
	"user": true, "password": true, "sslpassword": true, "sslkey": true, "sslcert": true,
	"sslrootcert": true, "passfile": true, "service": true, "servicefile": true, "options": true,
}

// dsnDetails is what may be said, and what must not be said, about a connection
// string in error text. Only the host and the database name may be named, and
// only when the string can be read with confidence.
type dsnDetails struct {
	parsed  bool     // pgx could parse the string
	target  string   // `host "h", database "d"`, or the part of it that may be named
	secrets []string // removed wherever they appear
	tokens  []string // removed where they stand alone
}

// inspectDSN reads dsn the way the driver does. It never fails: for a string the
// driver cannot parse (a name registered with stdlib.RegisterConnConfig, or
// garbage) nothing is named and the whole string is the only secret known.
func inspectDSN(dsn string) dsnDetails {
	details := dsnDetails{secrets: []string{dsn}}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return details
	}
	details.parsed = true
	details.secrets = append(details.secrets, cfg.User, cfg.Password)
	details.tokens = configPorts(&cfg.Config)
	for key, value := range settingsOf(dsn) {
		switch {
		case key == "host" || key == "dbname" || key == "database":
		case credentialSettings[key]:
			details.secrets = append(details.secrets, value)
		default:
			details.tokens = append(details.tokens, value)
		}
	}

	hosts := configHosts(&cfg.Config)
	if isMisSplitURL(dsn) {
		// The split into host and database cannot be trusted: it may hold the
		// user name or the password. Name nothing, and remove both from the text.
		details.secrets = append(details.secrets, hosts...)
		details.secrets = append(details.secrets, cfg.Database)
		return details
	}
	var named []string
	if isNameable(cfg.Host, "@?#") {
		named = append(named, fmt.Sprintf("host %q", cfg.Host))
	} else {
		details.secrets = append(details.secrets, cfg.Host)
	}
	if isNameable(cfg.Database, "@:/?#") {
		named = append(named, fmt.Sprintf("database %q", cfg.Database))
	} else {
		details.secrets = append(details.secrets, cfg.Database)
	}
	details.target = strings.Join(named, ", ")
	return details
}

// isNameable reports whether a host or a database name may be repeated in an
// error: it holds no white space and none of the characters of banned, which
// mark a connection string, or a piece of one, that was read wrongly.
func isNameable(value, banned string) bool {
	return !strings.ContainsAny(value, banned+" \t\r\n")
}

// isMisSplitURL reports whether a URL connection string is read by net/url in a
// way that cannot be trusted. A password that is not percent-encoded and holds
// a slash, a question mark or a number sign ends the authority early: the user
// name becomes the host, and the rest of the password and the real host become
// the database, the query or the fragment.
func isMisSplitURL(dsn string) bool {
	if !isURL(dsn) {
		return false
	}
	u, err := url.Parse(dsn)
	return err != nil || strings.Contains(dsn, "#") || strings.Contains(u.Path+u.RawQuery, "@")
}

func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// configSecrets lists the credentials of a connection configuration, and its
// hosts and database, which a driver error from a registered configuration
// may repeat.
func configSecrets(cfg *pgconn.Config) []string {
	return append(configHosts(cfg), cfg.User, cfg.Password, cfg.Database)
}

func configHosts(cfg *pgconn.Config) []string {
	hosts := []string{cfg.Host}
	for _, fallback := range cfg.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	return hosts
}

func configPorts(cfg *pgconn.Config) []string {
	ports := []string{strconv.Itoa(int(cfg.Port))}
	for _, fallback := range cfg.Fallbacks {
		ports = append(ports, strconv.Itoa(int(fallback.Port)))
	}
	return ports
}

// wrap describes a failure of op against the DSN: the host and database name
// when they may be named, and the driver's error text with every other part of
// the connection string removed.
func (d dsnDetails) wrap(op string, cause error) error {
	var parseErr *pgconn.ParseConfigError
	if errors.As(cause, &parseErr) {
		return errUnparsable
	}
	secrets, tokens := d.secrets, d.tokens
	var connectErr *pgconn.ConnectError
	if !d.parsed && errors.As(cause, &connectErr) {
		// A registered configuration: the driver knows what this package could not
		// read, and says so in its error.
		secrets = append(slices.Clone(secrets), configSecrets(connectErr.Config)...)
		tokens = configPorts(connectErr.Config)
	}
	where := ""
	if d.target != "" {
		where = " (" + d.target + ")"
	}
	return &connectionError{
		msg:   fmt.Sprintf("dalgo2postgres: %s%s: %s", op, where, redactTokens(redact(cause.Error(), secrets), tokens)),
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
	if isURL(dsn) {
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

// redactTokens replaces each value with [redactedMark] where it stands alone,
// that is where the characters on both sides of it are not part of a word
// (letters, digits, dot, underscore, hyphen). Short settings such as a port or a
// timeout would otherwise cut pieces out of an address or a code.
func redactTokens(text string, values []string) string {
	for _, value := range values {
		if value == "" {
			continue
		}
		var out strings.Builder
		pos := 0
		for {
			i := strings.Index(text[pos:], value)
			if i < 0 {
				break
			}
			start, end := pos+i, pos+i+len(value)
			if (start == 0 || !isWordByte(text[start-1])) && (end == len(text) || !isWordByte(text[end])) {
				out.WriteString(text[pos:start])
				out.WriteString(redactedMark)
				pos = end
			} else {
				out.WriteString(text[pos : start+1])
				pos = start + 1
			}
		}
		out.WriteString(text[pos:])
		text = out.String()
	}
	return text
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
}
