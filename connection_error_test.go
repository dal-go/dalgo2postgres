package dalgo2postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// Secrets used by every DSN below. They are distinctive so a substring check
// cannot match ordinary error text.
const (
	secretUser     = "zelda_admin"
	secretPassword = "hunter2-Tr1cky"
	visibleHost    = "db.internal.example"
	visibleDB      = "orders_prod"
)

// dsnCases are parseable connection strings in each form the driver accepts.
func dsnCases() map[string]string {
	return map[string]string{
		"keyword":          "host=" + visibleHost + " port=6432 user=" + secretUser + " password=" + secretPassword + " dbname=" + visibleDB + " sslmode=disable",
		"keyword quoted":   "host=" + visibleHost + " user='" + secretUser + "' password='pa ss\\'w0rd-Qq9' dbname=" + visibleDB,
		"url":              "postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + ":6432/" + visibleDB + "?sslmode=disable&application_name=billing-sync",
		"url encoded":      "postgresql://" + secretUser + ":p%40ss%2Fw0rd-Zz8@" + visibleHost + "/" + visibleDB,
		"url without user": "postgres://" + visibleHost + "/" + visibleDB + "?password=" + secretPassword + "&user=" + secretUser,
	}
}

// secretsOf lists every fragment that must never reach error text for a case.
func secretsOf(name, dsn string) []string {
	out := []string{secretUser, dsn}
	switch name {
	case "keyword quoted":
		out = append(out, "pa ss'w0rd-Qq9", "w0rd-Qq9")
	case "url encoded":
		out = append(out, "p@ss/w0rd-Zz8", "p%40ss%2Fw0rd-Zz8", "w0rd-Zz8")
	default:
		out = append(out, secretPassword)
	}
	if name == "keyword" || name == "url" {
		out = append(out, "6432")
	}
	if name == "url" {
		out = append(out, "billing-sync")
	}
	return out
}

func assertNoSecrets(t *testing.T, err error, secrets []string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	text := err.Error()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("error text leaks %q: %s", s, text)
		}
	}
	// %v, %+v and %s of the error must not differ from Error().
	for _, verb := range []string{"%v", "%+v", "%s"} {
		if got := fmt.Sprintf(verb, err); got != text {
			t.Errorf("fmt %s = %q, want the same as Error() %q", verb, got, text)
		}
	}
}

var errDriverBoom = errors.New("driver boom")

func TestOpenVerified_OpenErrorNamesHostAndDatabaseOnly(t *testing.T) {
	for name, dsn := range dsnCases() {
		t.Run(name, func(t *testing.T) {
			// A driver error that echoes its whole input, as a careless one would.
			echoed := errors.New("cannot open " + dsn + ": " + errDriverBoom.Error())
			open := func(driverName, gotDSN string) (*sql.DB, error) {
				if gotDSN != dsn {
					t.Errorf("the driver got %q, want the DSN unchanged", gotDSN)
				}
				return nil, echoed
			}
			db, err := openVerified(dsn, open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			assertNoSecrets(t, err, secretsOf(name, dsn))
			for _, want := range []string{visibleHost, visibleDB, "sql.Open", errDriverBoom.Error()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if !errors.Is(err, echoed) {
				t.Errorf("errors.Is(err, driver error) = false, want the driver error wrapped")
			}
		})
	}
}

func TestOpenVerified_PingErrorNamesHostAndDatabaseOnly(t *testing.T) {
	for name, dsn := range dsnCases() {
		t.Run(name, func(t *testing.T) {
			echoed := errors.New("failed to connect to `" + dsn + "`: " + errDriverBoom.Error())
			mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectPing().WillReturnError(echoed)
			open := func(string, string) (*sql.DB, error) { return mockDB, nil }

			db, err := openVerified(dsn, open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			assertNoSecrets(t, err, secretsOf(name, dsn))
			for _, want := range []string{visibleHost, visibleDB, "PingContext", errDriverBoom.Error()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if !errors.Is(err, echoed) {
				t.Errorf("errors.Is(err, driver error) = false, want the driver error wrapped")
			}
		})
	}
}

func TestOpenVerified_UnparsableDSNNamesNothingFromIt(t *testing.T) {
	for name, dsn := range map[string]string{
		"url bad port":     "postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + ":notaport/" + visibleDB,
		"keyword bad port": "host=" + visibleHost + " user=" + secretUser + " password=" + secretPassword + " dbname=" + visibleDB + " port=notaport",
		"keyword spaced":   "host=" + visibleHost + " user=" + secretUser + " password = " + secretPassword + " dbname=" + visibleDB + " port=notaport",
	} {
		t.Run(name, func(t *testing.T) {
			// The real driver: it rejects the string when asked to connect, with
			// no server involved, and its error quotes the string.
			db, err := openVerified(dsn, sql.Open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, dsn, "notaport"})
			if !strings.Contains(err.Error(), "cannot be parsed") {
				t.Errorf("error %q should say the connection string cannot be parsed", err)
			}
			assertNoParseConfigError(t, err)
		})
	}
}

// assertNoParseConfigError checks that nothing in err's chain is a
// *pgconn.ParseConfigError, whose ConnString field holds the whole string.
func assertNoParseConfigError(t *testing.T, err error) {
	t.Helper()
	var parseErr *pgconn.ParseConfigError
	if errors.As(err, &parseErr) {
		t.Errorf("the error chain holds a *pgconn.ParseConfigError with ConnString %q", parseErr.ConnString)
	}
}

func TestOpenVerified_Success(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	var gotDriver string
	open := func(driverName, _ string) (*sql.DB, error) { gotDriver = driverName; return mockDB, nil }
	db, err := openVerified(dsnCases()["url"], open)
	if err != nil || db != mockDB {
		t.Fatalf("got %v, %v; want the opened handle and nil", db, err)
	}
	if gotDriver != "pgx" {
		t.Errorf("driver = %q, want pgx", gotDriver)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestNewDatabase_UnparsableDSNErrorLeaksNothing(t *testing.T) {
	// Through the real constructor and the real driver, with no server involved:
	// parsing fails before any connection is attempted.
	dsn := "postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + ":notaport/" + visibleDB
	db, err := NewDatabase(dsn)
	if db != nil {
		t.Fatalf("db = %v, want nil", db)
	}
	assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, dsn})
}

func TestSettingsOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
		want map[string]string
	}{
		{"keyword", "host=h  port = 5432 user=u password=p dbname=d", map[string]string{"host": "h", "port": "5432", "user": "u", "password": "p", "dbname": "d"}},
		{"keyword quoted and escaped", `password='a b\'c\\d' user=x\ y`, map[string]string{"password": `a b'c\d`, "user": "x y"}},
		{"keyword unterminated quote", `password='abc def`, map[string]string{"password": "abc def"}},
		{"keyword trailing backslash", `password=abc\`, map[string]string{"password": `abc\`}},
		{"keyword key without value", `lonely`, map[string]string{}},
		{"keyword word before the next pair", `lonely other=1`, map[string]string{}},
		{"keyword empty", ``, map[string]string{}},
		{"url", "postgres://u:p%40@h:5/d?sslmode=disable&application_name=a", map[string]string{"user": "u", "password": "p@", "port": "5", "sslmode": "disable", "application_name": "a"}},
		{"url without credentials", "postgresql://h/d", map[string]string{}},
		{"url user only", "postgres://u@h/d", map[string]string{"user": "u"}},
		{"url unparsable", "postgres://h:bad port/d", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := settingsOf(tc.dsn); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("settingsOf(%q) = %v, want %v", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	// Longer secrets go first, so a secret that contains another is not left half
	// visible; empty secrets are ignored; a secret is replaced in every form.
	got := redact("a secret-longer and secret and p@ss p%40ss", []string{"", "secret", "secret-longer", "p@ss"})
	want := "a " + redactedMark + " and " + redactedMark + " and " + redactedMark + " " + redactedMark
	if got != want {
		t.Errorf("redact = %q, want %q", got, want)
	}
	if got := redact("nothing to hide", nil); got != "nothing to hide" {
		t.Errorf("redact without secrets = %q", got)
	}
}

func TestConnectionError_Unwrap(t *testing.T) {
	e := &connectionError{msg: "m", cause: errDriverBoom}
	if e.Error() != "m" || !errors.Is(e, errDriverBoom) {
		t.Errorf("connectionError = %v, unwrap chain broken", e)
	}
}

// pgxText is the text pgx gives for a failed connection: the user and the
// database, then the server's reason, which repeats the user.
func pgxText(user, database string) string {
	return "failed to connect to `user=" + user + " database=" + database + "`: server error: FATAL: password authentication failed for user \"" + user + "\" (SQLSTATE 28P01)"
}

// passwordOf is the password each dsnCases entry carries, as it is spelled
// once decoded.
func passwordOf(name string) string {
	switch name {
	case "keyword quoted":
		return "pa ss'w0rd-Qq9"
	case "url encoded":
		return "p@ss/w0rd-Zz8"
	default:
		return secretPassword
	}
}

func TestOpenVerified_DriverTextWithPartsOnly(t *testing.T) {
	// pgx never echoes the connection string when a connection fails: it prints
	// the user, the database and the server's reasons. Only the per-part
	// redaction hides the user and the password in such text.
	for name, dsn := range dsnCases() {
		password := passwordOf(name)
		text := pgxText(secretUser, visibleDB) + "; the password " + password + " was rejected"
		check := func(t *testing.T, err error) {
			t.Helper()
			assertNoSecrets(t, err, []string{secretUser, password})
			for _, want := range []string{visibleDB, "SQLSTATE 28P01", "password authentication failed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lost %q", err, want)
				}
			}
		}
		t.Run(name+"/open", func(t *testing.T) {
			_, err := openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, errors.New(text) })
			check(t, err)
		})
		t.Run(name+"/ping", func(t *testing.T) {
			mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectPing().WillReturnError(errors.New(text))
			_, err = openVerified(dsn, func(string, string) (*sql.DB, error) { return mockDB, nil })
			check(t, err)
		})
	}
}

// misSplitURLs are URL connection strings whose password was not percent
// encoded. net/url ends the authority at the first slash, question mark or
// number sign, so the user name becomes the host and the rest of the password,
// with the real host, becomes the database name, or the query or the fragment.
func misSplitURLs(host string) map[string]string {
	real := "@" + visibleHost + "/" + visibleDB
	return map[string]string{
		"slash":                "postgres://" + host + ":/" + secretPassword + real,
		"digits then slash":    "postgres://" + host + ":2024/" + secretPassword + real,
		"number sign":          "postgres://" + host + ":#" + secretPassword + real,
		"question mark":        "postgres://" + host + ":?" + secretPassword + real,
		"at sign before slash": "postgres://" + secretUser + ":pa@" + host + ":1/" + secretPassword + real,
	}
}

func TestOpenVerified_MisSplitURLNamesNothing(t *testing.T) {
	for name, dsn := range misSplitURLs("zelda-admin.invalid") {
		t.Run(name, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("the driver should accept this string, as it does: %v", err)
			}
			// Text as pgx writes it, with what it took for the host and the database.
			echoed := errors.New("failed to connect to `user=" + cfg.User + " database=" + cfg.Database + "`: hostname resolving error: lookup " + cfg.Host + ": no such host")
			_, err = openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, echoed })
			// "zelda-admin.invalid" is a stand-in for a user name here, and
			// nothing of the password or the real host may be left.
			assertNoSecrets(t, err, []string{secretPassword, visibleHost, visibleDB, "zelda-admin.invalid", dsn, secretUser})
			if strings.Contains(err.Error(), "host \"") || strings.Contains(err.Error(), "database \"") {
				t.Errorf("error %q names a host or a database taken from an untrustworthy split", err)
			}
		})
	}
}

func TestNewDatabase_MisSplitURLLeaksNothing(t *testing.T) {
	// The real constructor and the real driver, against a closed port on this
	// machine: nothing listens there, so no server is involved.
	t.Setenv("PGPORT", "1")
	for name, dsn := range misSplitURLs("127.0.0.1") {
		t.Run(name, func(t *testing.T) {
			db, err := NewDatabase(dsn)
			if db != nil {
				_ = db.Close()
				t.Fatal("want a connection error")
			}
			assertNoSecrets(t, err, []string{secretPassword, visibleHost, dsn, secretUser})
		})
	}
}

func TestOpenVerified_OddDatabaseAndHostAreNotNamed(t *testing.T) {
	// A database name or host that carries what looks like a connection string
	// is not named, and is removed from the driver's text.
	for name, tc := range map[string]struct {
		dsn, host, database string
	}{
		"database is a URL":     {"host=h1 user=" + secretUser + " dbname=postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + "/" + visibleDB, "h1", "postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + "/" + visibleDB},
		"database with a space": {"host=h1 dbname='" + secretPassword + " " + visibleDB + "'", "h1", secretPassword + " " + visibleDB},
		"host with an at sign":  {"host=" + secretUser + "@" + secretPassword + " dbname=" + visibleDB, secretUser + "@" + secretPassword, visibleDB},
	} {
		t.Run(name, func(t *testing.T) {
			echoed := errors.New("failed to connect to `user=u database=" + tc.database + "`: lookup " + tc.host + ": no such host")
			_, err := openVerified(tc.dsn, func(string, string) (*sql.DB, error) { return nil, echoed })
			assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, tc.dsn})
		})
	}
}

func TestOpenVerified_PortSurvivesAsAddressAndIsHiddenAsPort(t *testing.T) {
	// Short setting values must not eat pieces of other words: with a timeout
	// of 1 the dialled address stays readable. The port is hidden, as a whole.
	dsn := "postgres://" + secretUser + ":" + secretPassword + "@127.0.0.1:15432/" + visibleDB + "?sslmode=disable&connect_timeout=1"
	echoed := errors.New(pgxText(secretUser, visibleDB) + ": dial error: dial tcp 127.0.0.1:15432: connect: connection refused (1 attempt)")
	_, err := openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, echoed })
	assertNoSecrets(t, err, []string{secretUser, secretPassword, "15432", "connect_timeout"})
	for _, want := range []string{"dial tcp 127.0.0.1:" + redactedMark + ":", "connection refused", "SQLSTATE 28P01"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "(1 attempt)") {
		t.Errorf("the timeout setting, a whole token here, should be hidden: %q", err)
	}
}

func TestNewDatabase_RegisteredConnConfigIsNotRefused(t *testing.T) {
	// A programmatic pgx config, registered with the driver, is not a string pgx
	// can parse. It must reach the driver, which knows it.
	cfg, err := pgx.ParseConfig("postgres://" + secretUser + ":" + secretPassword + "@127.0.0.1:1/" + visibleDB + "?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	name := stdlib.RegisterConnConfig(cfg)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(name) })

	db, err := NewDatabase(name)
	if db != nil {
		_ = db.Close()
		t.Fatal("want a connection error: nothing listens on port 1")
	}
	if strings.Contains(err.Error(), "cannot be parsed") {
		t.Fatalf("a registered config was refused as unparsable: %v", err)
	}
	if !strings.Contains(err.Error(), "PingContext") || !strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("error %q should be the ping failure", err)
	}
	assertNoSecrets(t, err, []string{secretUser, secretPassword, name})
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		t.Errorf("the driver's *pgconn.ConnectError should stay reachable through Unwrap")
	}
}

func TestRedactTokens(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"whole token", "dial 127.0.0.1:5 timeout 5", "dial 127.0.0.1:" + redactedMark + " timeout " + redactedMark},
		{"inside a word is kept", "x15y 15 a15", "x15y " + redactedMark + " a15"},
		{"at the edges", "15", redactedMark},
		{"adjacent matches", "15 15", redactedMark + " " + redactedMark},
		{"none", "nothing", "nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := "15"
			if tc.name == "whole token" {
				value = "5"
			}
			if got := redactTokens(tc.text, []string{"", value}); got != tc.want {
				t.Errorf("redactTokens(%q, %q) = %q, want %q", tc.text, value, got, tc.want)
			}
		})
	}
}

func TestOpenVerified_CredentialsFromTheEnvironmentAreHidden(t *testing.T) {
	// The driver takes the user and the password from PGUSER and PGPASSWORD when
	// the string has none, and says the user in its errors. Neither is in the
	// string, so only what the driver parsed can name them.
	t.Setenv("PGUSER", secretUser)
	t.Setenv("PGPASSWORD", secretPassword)
	dsn := "host=" + visibleHost + " dbname=" + visibleDB
	text := pgxText(secretUser, visibleDB) + "; the password " + secretPassword + " was rejected"
	_, err := openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, errors.New(text) })
	assertNoSecrets(t, err, []string{secretUser, secretPassword})
	if !strings.Contains(err.Error(), visibleDB) {
		t.Errorf("error %q lost the database name", err)
	}
}

func TestOpenVerified_EveryPortOfAMultiHostURLIsHidden(t *testing.T) {
	dsn := "postgres://" + secretUser + ":" + secretPassword + "@h1.example:1111,h2.example:2222,h3.example:3333/" + visibleDB
	echoed := errors.New("failed to connect: dial tcp h1.example:1111: refused; dial tcp h2.example:2222: refused; dial tcp h3.example:3333: refused")
	_, err := openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, echoed })
	assertNoSecrets(t, err, []string{"1111", "2222", "3333", secretUser, secretPassword})
	for _, want := range []string{"h1.example", "h2.example", "h3.example", "refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lost %q", err, want)
		}
	}
}
