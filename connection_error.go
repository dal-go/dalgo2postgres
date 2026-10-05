package dalgo2postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// FailureKind says what kind of failure a [ConnectionError] reports. It decides
// the sentence the error's text uses.
type FailureKind int

const (
	// FailureOther is any failure the kinds below do not describe. It is the
	// zero value.
	FailureOther FailureKind = iota
	// FailureInvalidDSN: the driver cannot parse the connection string, or a file
	// or service the string names cannot be read.
	FailureInvalidDSN
	// FailureMisread: the driver would accept the string but read it as something
	// else than was meant (a URL in quotes, with a leading space or with another
	// scheme, or a key=value string whose pairs are not separated by spaces). It is
	// refused before any connection is attempted.
	FailureMisread
	// FailureNetwork: the server could not be reached (name resolution, refused or
	// reset connection).
	FailureNetwork
	// FailureTLS: the TLS handshake with the server failed.
	FailureTLS
	// FailureTimeout: the attempt timed out or was canceled.
	FailureTimeout
	// FailureServer: the server answered with an error. [ConnectionError.SQLState]
	// holds its SQLSTATE code.
	FailureServer
)

// kindSentences is the sentence of each [FailureKind], indexed by the kind.
var kindSentences = [...]string{
	FailureOther:      "the connection failed",
	FailureInvalidDSN: "the connection string cannot be parsed, or a file or service it names cannot be read",
	FailureMisread: "the connection string is not read as intended: " +
		"a URL must start with exactly postgres:// or postgresql:// (no quotes, no leading space, no other scheme), " +
		"a password with a special character must be percent-encoded, " +
		"and in a key=value string the pairs are separated by spaces and a value with a space must be in single quotes",
	FailureNetwork: "the server could not be reached",
	FailureTLS:     "the TLS handshake with the server failed",
	FailureTimeout: "the connection timed out or was canceled",
	FailureServer:  "the server rejected the connection",
}

// serverSentences is the sentence of a [FailureServer] whose SQLSTATE code is
// known; any other code gets the kind's own sentence.
var serverSentences = map[string]string{
	"28000": "the server refused the connection: the user is not authorized",
	"28P01": "the server refused the connection: password authentication failed",
	"3D000": "the server refused the connection: the database does not exist",
	"53300": "the server refused the connection: too many connections",
	"57P03": "the server refused the connection: it is not accepting connections yet",
}

// sentence returns the fixed sentence for the failure.
func (k FailureKind) sentence(sqlState string) string {
	if k == FailureServer {
		if s, ok := serverSentences[sqlState]; ok {
			return s
		}
	}
	if k < 0 || int(k) >= len(kindSentences) {
		k = FailureOther
	}
	return kindSentences[k]
}

// ConnectionError is the error a connection that fails is reported as: by
// [NewDatabase] and [NewDatabaseWithOptions] when the connection to PostgreSQL
// cannot be opened or verified, and by every other call of this package (a read, a
// write, a transaction, the schema reader, a reader a query returned) when its
// connection fails later, for example when the pool must make a connection again.
//
// Its text is built only from a fixed sentence chosen by Kind and from the parts
// below that passed a strict check. Nothing of the connection string but those
// parts, and nothing from the driver's or the server's own message, is ever
// copied into it: those hold the user name, the password and other settings, and
// callers print errors. For the same reason the driver's error is not reachable
// through [errors.Unwrap], [errors.Is] or [errors.As]; branch on Kind and
// SQLState instead. The one thing kept of it is whether the attempt ended because
// its context was canceled or its deadline passed: [errors.Is] finds
// [context.Canceled] and [context.DeadlineExceeded] in the error of such an
// attempt.
//
// A part that is empty was not known, or could not be named safely. An error of
// a call after the connection was opened names no part at all.
type ConnectionError struct {
	// Kind is what failed.
	Kind FailureKind
	// SQLState is the five-character SQLSTATE code of a server error (Kind is
	// [FailureServer]), for example "28P01" for a rejected password.
	SQLState string
	// Host is the server's host name or address, named only when the connection
	// string names exactly one server and the name passes the check.
	Host string
	// Port is the server's port, under the same conditions as Host.
	Port string
	// Database is the database name, named only when it passes the check.
	Database string

	op       string // the step that failed: sql.Open or PingContext; empty for a refusal and for a call after the open
	canceled bool   // the attempt ended because its context was canceled
	timedOut bool   // the attempt ended because its context's deadline passed
}

// Is reports the two context errors of the attempt that failed, and nothing else.
func (e *ConnectionError) Is(target error) bool {
	return (e.canceled && target == context.Canceled) || (e.timedOut && target == context.DeadlineExceeded) //nolint:errorlint // the sentinels themselves
}

// Error returns the text, built from the fixed sentence, the SQLSTATE code and
// the named parts only.
func (e *ConnectionError) Error() string {
	var b strings.Builder
	b.WriteString("dalgo2postgres: ")
	if e.op != "" {
		b.WriteString(e.op)
		b.WriteString(": ")
	}
	b.WriteString(e.Kind.sentence(e.SQLState))
	if e.SQLState != "" {
		b.WriteString(" (SQLSTATE " + e.SQLState + ")")
	}
	var named []string
	if e.Host != "" {
		named = append(named, fmt.Sprintf("host %q", e.Host))
	}
	if e.Port != "" {
		named = append(named, "port "+e.Port)
	}
	if e.Database != "" {
		named = append(named, fmt.Sprintf("database %q", e.Database))
	}
	if len(named) > 0 {
		b.WriteString("; " + strings.Join(named, ", "))
	}
	return b.String()
}

// Limits of the parts that may be named. A part that passes them is still left
// out when it repeats the user name, the password or another setting value.
const (
	maxHostLength     = 253 // the longest DNS name
	maxDatabaseLength = 63  // the longest PostgreSQL identifier
)

var (
	// hostShape: letters, digits, dots, hyphens, colons (an IPv6 address) and
	// brackets.
	hostShape = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]+$`)
	// databaseShape: letters, digits, underscore and hyphen.
	databaseShape = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// sqlStateShape: a SQLSTATE code is five upper-case letters and digits.
	sqlStateShape = regexp.MustCompile(`^[0-9A-Z]{5}$`)
	// settingName is the spelling of a setting a server accepts: a plain name,
	// with dots for a custom setting of an extension.
	settingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

func isSettingName(key string) bool { return settingName.MatchString(key) }

// sharedRun is the length of a run of bytes that a named part may not share with
// a secret. A part that merely contains the user name, or that the password
// contains, would show the reader a piece of it.
const sharedRun = 4

// repeatsSecret reports whether part equals one of the secrets or shares a run
// of [sharedRun] bytes with one.
func repeatsSecret(part string, secrets []string) bool {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if part == secret {
			return true
		}
		for i := 0; i+sharedRun <= len(part); i++ {
			if strings.Contains(secret, part[i:i+sharedRun]) {
				return true
			}
		}
	}
	return false
}

// nameParts returns the host, port and database name of cfg, each only when it
// may be named: it passes its shape check, and it repeats neither the user name,
// the password nor a runtime setting. The host and port are named only when the
// configuration has one endpoint: with several, the failure cannot be pinned to
// one, and a string that was read wrongly can put pieces of itself into the list
// (a keyword string whose pairs are separated by commas makes each pair a host).
func nameParts(cfg *pgconn.Config) (host, port, database string) {
	secrets := []string{cfg.User, cfg.Password}
	for _, value := range cfg.RuntimeParams {
		secrets = append(secrets, value)
	}
	if singleEndpoint(cfg) {
		if len(cfg.Host) <= maxHostLength && hostShape.MatchString(cfg.Host) && !repeatsSecret(cfg.Host, secrets) {
			host = cfg.Host
		}
		if p := strconv.Itoa(int(cfg.Port)); !repeatsSecret(p, secrets) { // digits by construction
			port = p
		}
	}
	if len(cfg.Database) <= maxDatabaseLength && databaseShape.MatchString(cfg.Database) && !repeatsSecret(cfg.Database, secrets) {
		database = cfg.Database
	}
	return host, port, database
}

// singleEndpoint reports whether every fallback of cfg is the same host and port
// as the first (the driver adds one for the plain-text alternative of a TLS
// connection).
func singleEndpoint(cfg *pgconn.Config) bool {
	for _, fallback := range cfg.Fallbacks {
		if fallback.Host != cfg.Host || fallback.Port != cfg.Port {
			return false
		}
	}
	return true
}

// classify decides the kind of a failure from the types in its chain, never from
// its text. A server error is told by its SQLSTATE code, which is returned when it
// has the shape of one.
func classify(cause error) (kind FailureKind, sqlState string) {
	var parseErr *pgconn.ParseConfigError
	if errors.As(cause, &parseErr) {
		return FailureInvalidDSN, ""
	}
	var pgErr *pgconn.PgError
	if errors.As(cause, &pgErr) {
		if sqlStateShape.MatchString(pgErr.Code) {
			sqlState = pgErr.Code
		}
		return FailureServer, sqlState
	}
	switch {
	case isTimeout(cause):
		return FailureTimeout, ""
	case isTLS(cause):
		return FailureTLS, ""
	case isNetwork(cause):
		return FailureNetwork, ""
	}
	return FailureOther, ""
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isTLS(err error) bool {
	var (
		verify    *tls.CertificateVerificationError
		header    tls.RecordHeaderError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	if errors.As(err, &verify) || errors.As(err, &header) || errors.As(err, &authority) ||
		errors.As(err, &hostname) || errors.As(err, &invalid) {
		return true
	}
	// An alert of the server, or one this side sends, comes back from crypto/tls
	// as a *net.OpError around a type the package does not export. The driver
	// joins one error per attempt, so the alert may follow a failed dial: the whole
	// tree is walked, not only the first *net.OpError in it.
	return anyInTree(err, func(err error) bool {
		opErr, ok := err.(*net.OpError)
		return ok && (opErr.Op == "remote error" || opErr.Op == "local error")
	})
}

// anyInTree reports whether match is true for err or for any error reachable from
// it through Unwrap() error or Unwrap() []error.
func anyInTree(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}
	if match(err) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		return anyInTree(wrapped.Unwrap(), match)
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			if anyInTree(inner, match) {
				return true
			}
		}
	}
	return false
}

func isNetwork(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

// dsnInfo is what is known about a connection string before the driver is asked
// to connect with it.
type dsnInfo struct {
	cfg      *pgconn.Config // what the driver reads from the string; nil when it cannot parse it
	misread  bool           // the string is read as something else than was meant
	misSplit bool           // a URL whose password was not percent-encoded: its parts cannot be trusted
}

// inspectDSN reads dsn the way the driver does. It never fails: for a string the
// driver cannot parse (a name registered with stdlib.RegisterConnConfig, or
// garbage) nothing is known. A string the driver parses into a setting whose name
// is not a plain name is misread: the text before the first equals sign (the
// user, the password and the host of a URL in quotes) became that name, and
// connecting would send it to whatever server the driver reaches.
func inspectDSN(dsn string) dsnInfo {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return dsnInfo{}
	}
	for key := range cfg.RuntimeParams {
		if !isSettingName(key) {
			return dsnInfo{misread: true}
		}
	}
	if !isURL(dsn) && holdsKeywordPair(&cfg.Config) {
		return dsnInfo{misread: true}
	}
	return dsnInfo{cfg: &cfg.Config, misSplit: isMisSplitURL(dsn)}
}

// holdsKeywordPair reports whether the host, the user or the database of a
// keyword/value string holds an equals sign: a pair that was not separated from
// the one before it by white space, so that the driver took it for part of that
// value. A host list is checked entry by entry. A password or another setting may
// hold an equals sign as written, and is not looked at.
func holdsKeywordPair(cfg *pgconn.Config) bool {
	values := []string{cfg.Host, cfg.User, cfg.Database}
	for _, fallback := range cfg.Fallbacks {
		values = append(values, fallback.Host)
	}
	for _, value := range values {
		if strings.Contains(value, "=") {
			return true
		}
	}
	return false
}

// isMisSplitURL reports whether a URL connection string is read by net/url in a
// way that cannot be trusted. A password that is not percent-encoded and holds
// a slash or a number sign ends the authority early: the user name becomes the
// host, and the rest of the password and the real host become the database or
// the fragment. (A question mark does the same into the query, and pgx then
// reads the query as a setting with a misspelt name: see [inspectDSN].) The
// path is read as written: a correctly encoded %40 is not an at sign.
func isMisSplitURL(dsn string) bool {
	if !isURL(dsn) {
		return false
	}
	u, err := url.Parse(dsn)
	return err != nil || strings.Contains(dsn, "#") || strings.Contains(u.EscapedPath()+u.RawQuery, "@")
}

func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// newConnectionError builds the error for a failure from the types in cause's chain:
// its kind, and its SQLSTATE code when the server answered one. The cause itself goes no
// further: the result holds nothing of it but whether its context was canceled or its
// deadline passed. It is the one place a *ConnectionError is made from a cause, for the
// open and for every later call.
func newConnectionError(op string, cause error) *ConnectionError {
	kind, sqlState := classify(cause)
	return &ConnectionError{
		Kind:     kind,
		SQLState: sqlState,
		op:       op,
		canceled: errors.Is(cause, context.Canceled),
		timedOut: errors.Is(cause, context.DeadlineExceeded),
	}
}

// describe builds the error for a failure of op. The kind is decided from the types in
// cause's chain, and cause itself goes no further: the result holds nothing of it. The
// host, port and database are those of the driver's own configuration, the one it parsed
// from the string or, for a configuration registered with stdlib.RegisterConnConfig, the
// one in its connect error, and only when they may be named (see [nameParts]).
func (in dsnInfo) describe(op string, cause error) *ConnectionError {
	e := newConnectionError(op, cause)
	if e.Kind == FailureInvalidDSN || in.misSplit {
		return e // nothing to name: the string was not read, or not read with confidence
	}
	cfg := in.cfg
	var connectErr *pgconn.ConnectError
	if cfg == nil && errors.As(cause, &connectErr) {
		cfg = connectErr.Config
	}
	if cfg != nil {
		e.Host, e.Port, e.Database = nameParts(cfg)
	}
	return e
}
