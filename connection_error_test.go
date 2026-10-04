package dalgo2postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// Secrets used by the DSNs below. They are distinctive so a substring check
// cannot match ordinary error text.
const (
	secretUser     = "zelda_admin"
	secretPassword = "hunter2-Tr1cky"
	visibleHost    = "db.warehouse.example"
	visibleDB      = "orders_prod"
)

// dsnCase is a parseable connection string in a form the driver accepts, with
// the parts an error may name.
type dsnCase struct {
	dsn      string
	password string // the password as it is spelled once decoded
	port     string // the port the driver reads: the default when the string has none
}

func dsnCases() map[string]dsnCase {
	return map[string]dsnCase{
		"keyword":          {"host=" + visibleHost + " port=6432 user=" + secretUser + " password=" + secretPassword + " dbname=" + visibleDB + " sslmode=disable", secretPassword, "6432"},
		"keyword quoted":   {"host=" + visibleHost + " user='" + secretUser + "' password='pa ss\\'w0rd-Qq9' dbname=" + visibleDB, "pa ss'w0rd-Qq9", "5432"},
		"url":              {"postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + ":6432/" + visibleDB + "?sslmode=disable&application_name=billing-sync", secretPassword, "6432"},
		"url encoded":      {"postgresql://" + secretUser + ":p%40ss%2Fw0rd-Zz8@" + visibleHost + "/" + visibleDB, "p@ss/w0rd-Zz8", "5432"},
		"url without user": {"postgres://" + visibleHost + "/" + visibleDB + "?password=" + secretPassword + "&user=" + secretUser, secretPassword, "5432"},
	}
}

// secretsOf lists every fragment that must never reach error text for a case.
func (c dsnCase) secrets() []string {
	return []string{secretUser, c.password, c.dsn, "billing-sync", "w0rd-Qq9", "w0rd-Zz8", "p%40ss%2Fw0rd-Zz8"}
}

// pgDriverEnv is every environment variable pgx reads as a connection setting
// (parseEnvSettings in pgconn/config.go). A test that parses a string, asserts
// what the driver read from it or runs the real driver must not depend on them:
// a PGPORT changes the port it expects, a PGSERVICE makes a parsable string
// unparsable, and a PGHOST can point the real driver at a server.
var pgDriverEnv = []string{
	"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGAPPNAME",
	"PGCONNECT_TIMEOUT", "PGSSLMODE", "PGSSLKEY", "PGSSLCERT", "PGSSLSNI", "PGSSLROOTCERT",
	"PGSSLPASSWORD", "PGSSLNEGOTIATION", "PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE",
	"PGTZ", "PGOPTIONS", "PGMINPROTOCOLVERSION", "PGMAXPROTOCOLVERSION", "PGCHANNELBINDING",
	"PGREQUIREAUTH",
}

// clearPGEnv empties every variable of [pgDriverEnv] for the test (the driver
// ignores an empty one), and restores them afterwards.
func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, name := range pgDriverEnv {
		t.Setenv(name, "")
	}
}

func connectionErrorOf(t *testing.T, err error) *ConnectionError {
	t.Helper()
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("error %T %q is not a *ConnectionError", err, err)
	}
	return connErr
}

// assertNoSecrets checks that err is a *ConnectionError whose text holds none of
// secrets and that nothing of the driver's error is reachable from it.
func assertNoSecrets(t *testing.T, err error, secrets []string) *ConnectionError {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	connErr := connectionErrorOf(t, err)
	text := err.Error()
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("error text leaks %q: %s", s, text)
		}
	}
	// %v, %+v and %s of the error must not differ from Error().
	for _, verb := range []string{"%v", "%+v", "%s"} {
		if got := fmt.Sprintf(verb, err); got != text {
			t.Errorf("fmt %s = %q, want the same as Error() %q", verb, got, text)
		}
	}
	// Callers print error chains, so the chain must hold nothing else.
	if inner := errors.Unwrap(err); inner != nil {
		t.Errorf("errors.Unwrap(err) = %T %q, want nil: the original error must not be reachable", inner, inner)
	}
	return connErr
}

var errDriverBoom = errors.New("driver boom")

// echoing is a careless driver error: it repeats everything it was given.
func echoing(dsn string, more ...string) error {
	return errors.New("cannot open " + dsn + " " + strings.Join(more, " ") + ": " + errDriverBoom.Error())
}

func TestOpenVerified_OpenErrorNamesHostPortAndDatabaseOnly(t *testing.T) {
	clearPGEnv(t)
	for name, tc := range dsnCases() {
		t.Run(name, func(t *testing.T) {
			echoed := echoing(tc.dsn, tc.password)
			open := func(driverName, gotDSN string) (*sql.DB, error) {
				if gotDSN != tc.dsn {
					t.Errorf("the driver got %q, want the DSN unchanged", gotDSN)
				}
				return nil, echoed
			}
			db, err := openVerified(tc.dsn, open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			connErr := assertNoSecrets(t, err, tc.secrets())
			want := ConnectionError{Kind: FailureOther, Host: visibleHost, Port: tc.port, Database: visibleDB, op: "sql.Open"}
			if *connErr != want {
				t.Errorf("error = %+v, want %+v", *connErr, want)
			}
			wantText := fmt.Sprintf("dalgo2postgres: sql.Open: the connection failed; host %q, port %s, database %q", visibleHost, tc.port, visibleDB)
			if err.Error() != wantText {
				t.Errorf("text = %q, want %q", err, wantText)
			}
			if errors.Is(err, echoed) || strings.Contains(err.Error(), errDriverBoom.Error()) {
				t.Errorf("the driver's error or its message is reachable from %q", err)
			}
		})
	}
}

func TestOpenVerified_PingErrorNamesHostPortAndDatabaseOnly(t *testing.T) {
	clearPGEnv(t)
	for name, tc := range dsnCases() {
		t.Run(name, func(t *testing.T) {
			echoed := echoing(tc.dsn, tc.password)
			mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectPing().WillReturnError(echoed)
			mock.ExpectClose()
			open := func(string, string) (*sql.DB, error) { return mockDB, nil }

			db, err := openVerified(tc.dsn, open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			connErr := assertNoSecrets(t, err, tc.secrets())
			want := ConnectionError{Kind: FailureOther, Host: visibleHost, Port: tc.port, Database: visibleDB, op: "PingContext"}
			if *connErr != want {
				t.Errorf("error = %+v, want %+v", *connErr, want)
			}
			if errors.Is(err, echoed) {
				t.Error("the driver's error is reachable")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("the handle was not closed after the failed ping: %v", err)
			}
		})
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
	db, err := openVerified(dsnCases()["url"].dsn, open)
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

func TestOpenVerified_UnparsableDSNNamesNothingFromIt(t *testing.T) {
	clearPGEnv(t)
	for name, dsn := range map[string]string{
		"url bad port":     "postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + ":notaport/" + visibleDB,
		"keyword bad port": "host=" + visibleHost + " user=" + secretUser + " password=" + secretPassword + " dbname=" + visibleDB + " port=notaport",
		"keyword spaced":   "host=" + visibleHost + " user=" + secretUser + " password = " + secretPassword + " dbname=" + visibleDB + " port=notaport",
	} {
		t.Run(name, func(t *testing.T) {
			// The real driver: it rejects the string when asked to connect, with
			// no server involved, and its own error quotes the string.
			db, err := openVerified(dsn, sql.Open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, dsn, "notaport"})
			want := ConnectionError{Kind: FailureInvalidDSN, op: "PingContext"} // the driver parses when it connects
			if *connErr != want {
				t.Errorf("error = %+v, want %+v: nothing may be named for a string that cannot be parsed", *connErr, want)
			}
			if !strings.Contains(err.Error(), "cannot be parsed") {
				t.Errorf("error %q should say the connection string cannot be parsed", err)
			}
		})
	}
}

func TestNewDatabase_UnparsableDSNErrorLeaksNothing(t *testing.T) {
	clearPGEnv(t)
	// Through the real constructors and the real driver, with no server involved:
	// parsing fails before any connection is attempted.
	dsn := "postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + ":notaport/" + visibleDB
	secrets := []string{secretUser, secretPassword, visibleHost, visibleDB, dsn, "notaport"}
	db, err := NewDatabase(dsn)
	if db != nil {
		t.Fatalf("db = %v, want nil", db)
	}
	_ = assertNoSecrets(t, err, secrets)
	db, err = NewDatabaseWithOptions(dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{})
	if db != nil {
		t.Fatalf("db = %v, want nil", db)
	}
	_ = assertNoSecrets(t, err, secrets)
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
	clearPGEnv(t)
	for name, dsn := range misSplitURLs("zelda-admin.invalid") {
		t.Run(name, func(t *testing.T) {
			if _, err := pgx.ParseConfig(dsn); err != nil {
				t.Fatalf("the driver should accept this string, as it does: %v", err)
			}
			_, err := openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, echoing(dsn) })
			// "zelda-admin.invalid" is a stand-in for a user name here: the driver
			// reads it as the host, and it passes the shape check of a host.
			connErr := assertNoSecrets(t, err, []string{secretPassword, visibleHost, visibleDB, "zelda-admin.invalid", dsn, secretUser})
			if connErr.Host != "" || connErr.Port != "" || connErr.Database != "" {
				t.Errorf("error %+v names a part taken from a split that cannot be trusted", *connErr)
			}
		})
	}
}

func TestNewDatabase_MisSplitURLLeaksNothing(t *testing.T) {
	clearPGEnv(t)
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
			_ = assertNoSecrets(t, err, []string{secretPassword, visibleHost, dsn, secretUser, visibleDB})
		})
	}
}

// misreadStrings are connection strings that pgx accepts but reads in a way that
// was not meant: it takes them for keyword/value strings (or splits a URL early),
// and the text before the first equals sign becomes the name of a setting. That
// name holds the user, the password and the real host, and the server echoes an
// unknown setting name in its error.
func misreadStrings() map[string]string {
	rest := secretUser + ":" + secretPassword + "@" + visibleHost + ":6432/" + visibleDB + "?sslmode=disable"
	return map[string]string{
		"double quoted URL":       `"postgres://` + rest + `"`,
		"leading space":           " postgres://" + rest,
		"upper case scheme":       "POSTGRES://" + rest,
		"foreign scheme":          "postgresql+psycopg2://" + rest,
		"question mark mis-split": "postgres://zelda-admin.invalid:?" + secretPassword + "@" + visibleHost + "/" + visibleDB,
	}
}

func TestOpenVerified_MisreadStringIsRefusedBeforeTheDriver(t *testing.T) {
	clearPGEnv(t)
	for name, dsn := range misreadStrings() {
		t.Run(name, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("the driver should accept this string, as it does: %v", err)
			}
			named := false
			for key := range cfg.RuntimeParams {
				named = named || strings.ContainsAny(key, "@/:")
			}
			if !named {
				t.Fatalf("expected pgx to take part of the string for a setting name: %v", cfg.RuntimeParams)
			}
			asked := false
			open := func(string, string) (*sql.DB, error) { asked = true; return nil, echoing(dsn) }
			db, err := openVerified(dsn, open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			if asked {
				t.Error("the driver was asked to connect with a string it misreads")
			}
			connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, "zelda-admin.invalid", dsn, "6432"})
			if want := (ConnectionError{Kind: FailureMisread}); *connErr != want {
				t.Errorf("error = %+v, want %+v", *connErr, want)
			}
		})
	}
}

func TestNewDatabase_MisreadStringLeaksNothing(t *testing.T) {
	clearPGEnv(t)
	// The real constructor and the real driver. The string is refused before any
	// connection is attempted, so no server (and no default local socket) is tried.
	for name, dsn := range misreadStrings() {
		t.Run(name, func(t *testing.T) {
			db, err := NewDatabase(dsn)
			if db != nil {
				_ = db.Close()
				t.Fatal("want an error")
			}
			connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, "zelda-admin.invalid", dsn})
			if connErr.Kind != FailureMisread || strings.Contains(err.Error(), "PingContext") {
				t.Errorf("the string was not refused before connecting: %v", err)
			}
		})
	}
}

// keywordStringsReadWrongly are keyword/value strings whose pairs are not
// separated by white space. The driver reads the rest of the string as the value
// of the first pair (or of a later one) and, for a list of hosts, as hosts.
func keywordStringsReadWrongly() map[string]string {
	creds := "user=" + secretUser + "%spassword=" + secretPassword
	join := func(sep string, pairs ...string) string { return strings.Join(pairs, sep) }
	return map[string]string{
		"semicolons":                 join(";", "host="+visibleHost, "port=6432", "user="+secretUser, "password="+secretPassword, "dbname="+visibleDB),
		"semicolons, user first":     join(";", "user="+secretUser, "password="+secretPassword, "host="+visibleHost, "port=6432", "dbname="+visibleDB),
		"semicolons, database last":  join(";", "host="+visibleHost, "dbname="+visibleDB, "user="+secretUser),
		"commas":                     join(",", "host="+visibleHost, "user="+secretUser, "password="+secretPassword, "dbname="+visibleDB) + " port=6432",
		"commas, user first":         join(",", "user="+secretUser, "password="+secretPassword, "host="+visibleHost, "dbname="+visibleDB),
		"commas, database first":     join(",", "dbname="+visibleDB, "user="+secretUser, "password="+secretPassword),
		"ampersands":                 join("&", "host="+visibleHost, "port=6432", "user="+secretUser, "password="+secretPassword, "dbname="+visibleDB),
		"ampersands, database first": join("&", "dbname="+visibleDB, "host="+visibleHost, "user="+secretUser, "password="+secretPassword),
		"ampersands, user first":     fmt.Sprintf(creds, "&") + "&host=" + visibleHost,
	}
}

func TestOpenVerified_KeywordStringWithAPairInAValueIsRefusedBeforeTheDriver(t *testing.T) {
	clearPGEnv(t)
	for name, dsn := range keywordStringsReadWrongly() {
		t.Run(name, func(t *testing.T) {
			calls := 0
			open := func(string, string) (*sql.DB, error) { calls++; return nil, echoing(dsn) }
			db, err := openVerified(dsn, open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			if calls != 0 {
				t.Errorf("the driver was called %d times with a string it reads wrongly", calls)
			}
			connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, dsn, "6432"})
			if want := (ConnectionError{Kind: FailureMisread}); *connErr != want {
				t.Errorf("error = %+v, want %+v", *connErr, want)
			}
			if want := "dalgo2postgres: " + FailureMisread.sentence(""); err.Error() != want {
				t.Errorf("text = %q, want the fixed sentence %q", err, want)
			}
		})
	}
}

func TestNewDatabase_KeywordStringWithAPairInAValueIsRefused(t *testing.T) {
	clearPGEnv(t)
	for name, dsn := range keywordStringsReadWrongly() {
		t.Run(name, func(t *testing.T) {
			db, err := NewDatabase(dsn)
			if db != nil {
				_ = db.Close()
				t.Fatal("want an error")
			}
			connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, dsn})
			if connErr.Kind != FailureMisread || strings.Contains(err.Error(), "PingContext") {
				t.Errorf("the string was not refused before connecting: %v", err)
			}
		})
	}
}

func TestOpenVerified_KeywordStringsTheDriverReadsAsMeantAreNotRefused(t *testing.T) {
	clearPGEnv(t)
	for name, dsn := range map[string]string{
		"a password with an equals sign":        "host=" + visibleHost + " user=" + secretUser + " password='pa=ss;w0rd,x&y' dbname=" + visibleDB,
		"a setting value with an equals sign":   "host=" + visibleHost + " application_name='a=b' dbname=" + visibleDB,
		"several hosts":                         "host=h1.example,h2.example port=5432,5433 dbname=" + visibleDB,
		"a url with an equals sign in its path": "postgres://" + secretUser + ":" + secretPassword + "@" + visibleHost + "/a=b",
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			open := func(string, string) (*sql.DB, error) { calls++; return nil, echoing(dsn) }
			_, err := openVerified(dsn, open)
			if calls != 1 {
				t.Errorf("the driver was called %d times, want 1", calls)
			}
			if connErr := connectionErrorOf(t, err); connErr.Kind == FailureMisread {
				t.Errorf("a string the driver reads as meant was refused: %v", err)
			}
		})
	}
}

func TestOpenVerified_ParsedStringWithAnUnreadableFileOrServiceSaysSo(t *testing.T) {
	clearPGEnv(t)
	// The string is well formed; something it names is missing. The real driver
	// refuses it when asked, and the text says a file or service may be the cause.
	base := "host=" + visibleHost + " port=6432 user=" + secretUser + " password=" + secretPassword + " dbname=" + visibleDB
	for name, tc := range map[string]struct{ dsn, named string }{
		"missing root certificate": {base + " sslmode=verify-full sslrootcert=/nonexistent/ca.pem", "/nonexistent/ca.pem"},
		"unknown service":          {base + " service=nosuch", "nosuch"},
	} {
		t.Run(name, func(t *testing.T) {
			db, err := openVerified(tc.dsn, sql.Open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, tc.named, tc.dsn, visibleHost, visibleDB})
			if connErr.Kind != FailureInvalidDSN || !strings.Contains(err.Error(), "a file or service it names cannot be read") {
				t.Errorf("error %q should be an invalid-DSN error that says a file or service may be the cause", err)
			}
		})
	}
}

func TestNewDatabase_RegisteredConnConfigIsNotRefused(t *testing.T) {
	clearPGEnv(t)
	// A programmatic pgx config, registered with the driver, is not a string pgx
	// can parse. It must reach the driver, which knows it; the host, port and
	// database come from the driver's own error then.
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
	connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, name})
	want := ConnectionError{Kind: FailureNetwork, Host: "127.0.0.1", Port: "1", Database: visibleDB, op: "PingContext"}
	if *connErr != want {
		t.Errorf("error = %+v, want %+v", *connErr, want)
	}
	var driverErr *pgconn.ConnectError
	if errors.As(err, &driverErr) {
		t.Errorf("the driver's *pgconn.ConnectError, which holds the whole configuration, is reachable")
	}
}

func TestOpenVerified_CredentialsFromTheEnvironmentAreNeverNamed(t *testing.T) {
	clearPGEnv(t)
	// The driver takes the user and the password from PGUSER and PGPASSWORD when
	// the string has none. Neither is in the string, so only what the driver
	// parsed can tell they must not be named: here the database is the user name.
	t.Setenv("PGUSER", secretUser)
	t.Setenv("PGPASSWORD", secretPassword)
	t.Setenv("PGDATABASE", secretUser)
	dsn := "host=" + visibleHost
	_, err := openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, echoing(dsn, secretUser, secretPassword) })
	connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword})
	if connErr.Host != visibleHost || connErr.Database != "" {
		t.Errorf("error = %+v, want the host named and the database, which is the user name, left out", *connErr)
	}
}

func TestOpenVerified_SeveralEndpointsNameNeitherHostNorPort(t *testing.T) {
	clearPGEnv(t)
	// A failure cannot be pinned to one of several servers, and a string that was
	// read wrongly can fill the list with pieces of itself.
	dsn := "postgres://" + secretUser + ":" + secretPassword + "@h1.example:1111,h2.example:2222,h3.example:3333/" + visibleDB
	_, err := openVerified(dsn, func(string, string) (*sql.DB, error) { return nil, echoing(dsn) })
	connErr := assertNoSecrets(t, err, []string{"h1.example", "h2.example", "h3.example", "1111", "2222", "3333", secretUser, secretPassword})
	if connErr.Host != "" || connErr.Port != "" || connErr.Database != visibleDB {
		t.Errorf("error = %+v, want only the database named", *connErr)
	}
}

// TestNewDatabase_ReviewRound3Strings runs the strings of the third review round
// through the real constructor and the real driver. Nothing listens on port 1.
// A name, a host or a database that is not what it was meant to be must not drag
// the user name or the password along.
func TestNewDatabase_ReviewRound3Strings(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("PGHOST", "127.0.0.1")
	t.Setenv("PGPORT", "1")
	secrets := []string{secretUser, secretPassword, visibleDB}
	for name, dsn := range map[string]string{
		"semicolons":         "host=127.0.0.1;port=1;user=" + secretUser + ";password=" + secretPassword + ";dbname=" + visibleDB,
		"semicolon dbname":   "dbname=" + visibleDB + ";host=127.0.0.1;user=" + secretUser + ";password=" + secretPassword,
		"semicolon database": "database=" + visibleDB + ";host=127.0.0.1;user=" + secretUser + ";password=" + secretPassword,
		"commas":             "host=127.0.0.1,user=" + secretUser + ",password=" + secretPassword + ",dbname=" + visibleDB + " port=1 sslmode=disable",
		"ampersands":         "host=127.0.0.1&port=1&user=" + secretUser + "&password=" + secretPassword + "&dbname=" + visibleDB,
		"ampersands comma":   "host=127.0.0.1&user=" + secretUser + "&password=" + secretPassword + ",dbname=" + visibleDB + " port=1 sslmode=disable",
		"user cut to 63":     "user=" + secretUser + ";password=" + secretPassword + ";host=" + visibleHost + ";port=6432;dbname=" + visibleDB,
	} {
		t.Run(name, func(t *testing.T) {
			db, err := NewDatabase(dsn)
			if db != nil {
				_ = db.Close()
				t.Fatal("want an error")
			}
			connErr := assertNoSecrets(t, err, append(secrets, dsn, visibleHost))
			if connErr.Database != "" {
				t.Errorf("error %+v names a database that is a piece of the string", *connErr)
			}
		})
	}
}

func TestOpenVerified_ServerTextCutTo63BytesIsNotCopied(t *testing.T) {
	clearPGEnv(t)
	// PostgreSQL cuts the user name and the database name of the startup packet
	// to 63 bytes before it repeats them. A scrubber that knows only whole values
	// misses the cut form; this package copies no server text at all.
	tail := secretUser + ";password=" + secretPassword + ";host=" + visibleHost + ";port=6432;dbname=" + visibleDB
	misSplitDatabase := strings.Repeat("p", 20) + secretPassword + "@" + visibleHost + "/" + visibleDB
	longUser := strings.Repeat("u", 70) + secretUser
	for name, tc := range map[string]struct{ dsn, code, served string }{
		// A semicolon string whose user is the whole tail is refused before the
		// driver is asked (TestOpenVerified_KeywordStringWithAPairInAValueIsRefused).
		// A mis-split URL still reaches the driver: its database is the rest of the password and the real host.
		"database": {"postgres://127.0.0.1:/" + misSplitDatabase, "3D000", `FATAL: database "` + misSplitDatabase[:63] + `" does not exist`},
		// A correct string with a user name longer than 63 bytes.
		"correct url with a long user": {"postgres://" + longUser + ":x@127.0.0.1:1/db", "28P01", `FATAL: password authentication failed for user "` + longUser[:63] + `"`},
	} {
		t.Run(name, func(t *testing.T) {
			served := &pgconn.PgError{Severity: "FATAL", Code: tc.code, Message: tc.served}
			_, err := openVerified(tc.dsn, func(string, string) (*sql.DB, error) { return nil, served })
			connErr := assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, tail[:20], misSplitDatabase[:20], "uuuu", tc.served, served.Error()})
			if connErr.Kind != FailureServer || connErr.SQLState != tc.code {
				t.Errorf("error = %+v, want a server error with SQLSTATE %s", *connErr, tc.code)
			}
		})
	}
}

func TestNewDatabase_DatabaseEqualToACredentialIsNotNamed(t *testing.T) {
	clearPGEnv(t)
	// postgres:postgres@.../postgres: the database repeats the password, and
	// naming it would show the reader the password and that it equals the database.
	db, err := NewDatabase("postgres://postgres:postgres@127.0.0.1:1/postgres?sslmode=disable")
	if db != nil {
		_ = db.Close()
		t.Fatal("want a connection error")
	}
	connErr := assertNoSecrets(t, err, nil)
	want := ConnectionError{Kind: FailureNetwork, Host: "127.0.0.1", Port: "1", op: "PingContext"}
	if *connErr != want {
		t.Errorf("error = %+v, want %+v", *connErr, want)
	}
	if strings.Contains(strings.ReplaceAll(err.Error(), "dalgo2postgres", ""), "postgres") {
		t.Errorf("the text holds the credential: %q", err)
	}
}

func TestNewDatabase_SeamFailuresAndSuccess(t *testing.T) {
	t.Run("an invalid option is refused before the driver is asked", func(t *testing.T) {
		asked := false
		_, err := newDatabase("host=h", dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierMode(99))},
			func(string, string) (*sql.DB, error) { asked = true; return nil, nil })
		if err == nil || asked {
			t.Errorf("err = %v, asked = %v; want an error and no connection attempt", err, asked)
		}
		var connErr *ConnectionError
		if errors.As(err, &connErr) {
			t.Errorf("an option error is not a connection error: %v", err)
		}
	})
	t.Run("a failure to open is a connection error", func(t *testing.T) {
		_, err := newDatabase(dsnCases()["keyword"].dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, nil,
			func(string, string) (*sql.DB, error) { return nil, errDriverBoom })
		_ = assertNoSecrets(t, err, []string{secretUser, secretPassword})
	})
	t.Run("a verified connection becomes a Database", func(t *testing.T) {
		mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectPing()
		db, err := newDatabase(dsnCases()["keyword"].dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, []Option{WithSchema("sales")},
			func(string, string) (*sql.DB, error) { return mockDB, nil })
		if err != nil || db == nil || db.schemaName() != "sales" {
			t.Fatalf("got %v, %v; want a Database reading schema sales", db, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	echo := errors.New("echo " + secretUser + " " + secretPassword)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	noHost := &net.DNSError{Err: "no such host", Name: secretUser, IsNotFound: true}
	for _, tc := range []struct {
		name      string
		err       error
		wantKind  FailureKind
		wantState string
	}{
		{"parse error", pgconn.NewParseConfigError(secretPassword, "m", echo), FailureInvalidDSN, ""},
		{"parse error wrapped", fmt.Errorf("sql.Open: %w", pgconn.NewParseConfigError("", "m", nil)), FailureInvalidDSN, ""},
		{"password rejected", &pgconn.PgError{Code: "28P01", Message: echo.Error()}, FailureServer, "28P01"},
		{"unknown code of the right shape", &pgconn.PgError{Code: "XX000"}, FailureServer, "XX000"},
		{"code that is not a code", &pgconn.PgError{Code: "28P01 " + secretPassword}, FailureServer, ""},
		{"empty code", &pgconn.PgError{}, FailureServer, ""},
		{"server error wrapped by the driver", fmt.Errorf("server error: %w", &pgconn.PgError{Code: "3D000"}), FailureServer, "3D000"},
		{"deadline", fmt.Errorf("%w: %w", echo, context.DeadlineExceeded), FailureTimeout, ""},
		{"canceled", context.Canceled, FailureTimeout, ""},
		{"os deadline", fmt.Errorf("read: %w", os.ErrDeadlineExceeded), FailureTimeout, ""},
		{"net timeout", &net.OpError{Op: "dial", Err: timeoutError{}}, FailureTimeout, ""},
		{"dns timeout", &net.DNSError{IsTimeout: true}, FailureTimeout, ""},
		{"certificate verification", &tls.CertificateVerificationError{Err: echo}, FailureTLS, ""},
		{"tls over tcp", &net.OpError{Op: "remote error", Err: &tls.CertificateVerificationError{Err: echo}}, FailureTLS, ""},
		{"tls alert from the server", &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}, FailureTLS, ""},
		{"tls alert from this side", fmt.Errorf("connect: %w", &net.OpError{Op: "local error", Err: errors.New("tls: unexpected message")}), FailureTLS, ""},
		{"record header", tls.RecordHeaderError{Msg: echo.Error()}, FailureTLS, ""},
		{"unknown authority", x509.UnknownAuthorityError{}, FailureTLS, ""},
		{"hostname mismatch", x509.HostnameError{Certificate: &x509.Certificate{}, Host: secretUser}, FailureTLS, ""},
		{"invalid certificate", x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}, FailureTLS, ""},
		{"a dial that failed is not a tls alert", &net.OpError{Op: "dial", Err: errors.New("tls: bad certificate")}, FailureNetwork, ""},
		{"connection refused", refused, FailureNetwork, ""},
		{"no such host", noHost, FailureNetwork, ""},
		{"joined: the server's answer wins over a failed host", errors.Join(noHost, refused, &pgconn.PgError{Code: "28000"}), FailureServer, "28000"},
		{"joined: hosts that all failed", errors.Join(noHost, refused), FailureNetwork, ""},
		{"anything else", echo, FailureOther, ""},
		{"a message that merely says timeout is not a timeout", errors.New("timeout: " + secretPassword), FailureOther, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, state := classify(tc.err)
			if kind != tc.wantKind || state != tc.wantState {
				t.Errorf("classify = (%d, %q), want (%d, %q)", kind, state, tc.wantKind, tc.wantState)
			}
		})
	}
}

func TestSentences(t *testing.T) {
	seen := map[string]string{}
	record := func(name, sentence string) {
		t.Helper()
		if sentence == "" {
			t.Errorf("%s has no sentence", name)
		}
		if other, dup := seen[sentence]; dup {
			t.Errorf("%s and %s share the sentence %q", name, other, sentence)
		}
		seen[sentence] = name
	}
	for kind := range kindSentences {
		record(fmt.Sprintf("kind %d", kind), FailureKind(kind).sentence(""))
	}
	for code, want := range serverSentences {
		got := FailureServer.sentence(code)
		if got != want {
			t.Errorf("sentence(%s) = %q, want %q", code, got, want)
		}
		record("code "+code, got)
	}
	if got, want := FailureKind(len(kindSentences)+5).sentence(""), FailureOther.sentence(""); got != want {
		t.Errorf("an unknown kind says %q, want the generic sentence %q", got, want)
	}
	if got, want := FailureKind(-1).sentence(""), FailureOther.sentence(""); got != want {
		t.Errorf("a negative kind says %q, want the generic sentence %q", got, want)
	}
	if got, want := FailureNetwork.sentence("28P01"), kindSentences[FailureNetwork]; got != want {
		t.Errorf("a SQLSTATE changed the sentence of a network failure: %q", got)
	}
	// The refusal covers a keyword string too, and says how to write one.
	for _, want := range []string{"postgres://", "percent-encoded", "key=value", "separated by spaces", "single quotes"} {
		if got := FailureMisread.sentence(""); !strings.Contains(got, want) {
			t.Errorf("the sentence of a misread string %q does not mention %q", got, want)
		}
	}
}

func TestConnectionError_Error(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  ConnectionError
		want string
	}{
		{"zero value", ConnectionError{}, "dalgo2postgres: the connection failed"},
		{"refusal has no step", ConnectionError{Kind: FailureMisread}, "dalgo2postgres: " + kindSentences[FailureMisread]},
		{"server with a known code", ConnectionError{Kind: FailureServer, SQLState: "28P01", op: "PingContext"},
			"dalgo2postgres: PingContext: the server refused the connection: password authentication failed (SQLSTATE 28P01)"},
		{"server with an unknown code", ConnectionError{Kind: FailureServer, SQLState: "XX000", op: "PingContext"},
			"dalgo2postgres: PingContext: the server rejected the connection (SQLSTATE XX000)"},
		{"everything named", ConnectionError{Kind: FailureNetwork, Host: "db.example", Port: "5432", Database: "orders", op: "sql.Open"},
			`dalgo2postgres: sql.Open: the server could not be reached; host "db.example", port 5432, database "orders"`},
		{"host only", ConnectionError{Kind: FailureTLS, Host: "::1", op: "PingContext"},
			`dalgo2postgres: PingContext: the TLS handshake with the server failed; host "::1"`},
		{"database only", ConnectionError{Kind: FailureTimeout, Database: "orders", op: "PingContext"},
			`dalgo2postgres: PingContext: the connection timed out or was canceled; database "orders"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
			if inner := errors.Unwrap(&tc.err); inner != nil {
				t.Errorf("Unwrap = %v, want nil", inner)
			}
		})
	}
}

func TestNameParts(t *testing.T) {
	endpoint := func(host string, port uint16, database string) pgconn.Config {
		return pgconn.Config{Host: host, Port: port, Database: database}
	}
	with := func(cfg pgconn.Config, change func(*pgconn.Config)) pgconn.Config {
		change(&cfg)
		return cfg
	}
	for _, tc := range []struct {
		name                     string
		cfg                      pgconn.Config
		wantHost, wantPort, want string
	}{
		{"plain", endpoint("db.example.com", 5432, "orders"), "db.example.com", "5432", "orders"},
		{"ipv4 and hyphens", endpoint("10.0.0.7", 1, "my-db_1"), "10.0.0.7", "1", "my-db_1"},
		{"ipv6", endpoint("[::1]", 5432, "d"), "[::1]", "5432", "d"},
		{"host with an underscore is left out", endpoint("my_db", 5432, "orders"), "", "5432", "orders"},
		{"host with a semicolon", endpoint("127.0.0.1;port=1;user=u", 5432, "orders"), "", "5432", "orders"},
		{"socket directory", endpoint("/var/run/postgresql", 5432, "orders"), "", "5432", "orders"},
		{"empty host", endpoint("", 5432, "orders"), "", "5432", "orders"},
		{"host of 253 bytes", endpoint(strings.Repeat("a", 253), 5432, "orders"), strings.Repeat("a", 253), "5432", "orders"},
		{"host of 254 bytes", endpoint(strings.Repeat("a", 254), 5432, "orders"), "", "5432", "orders"},
		{"database with a dot", endpoint("h", 5432, "my.db"), "h", "5432", ""},
		{"database with a space", endpoint("h", 5432, "my db"), "h", "5432", ""},
		{"database with an equals sign", endpoint("h", 5432, "orders;host=x"), "h", "5432", ""},
		{"empty database", endpoint("h", 5432, ""), "h", "5432", ""},
		{"database of 63 bytes", endpoint("h", 5432, strings.Repeat("d", 63)), "h", "5432", strings.Repeat("d", 63)},
		{"database of 64 bytes", endpoint("h", 5432, strings.Repeat("d", 64)), "h", "5432", ""},
		{"database equal to the user", with(endpoint("h", 5432, "postgres"), func(c *pgconn.Config) { c.User = "postgres" }), "h", "5432", ""},
		{"database equal to the password", with(endpoint("h", 5432, "postgres"), func(c *pgconn.Config) { c.Password = "postgres" }), "h", "5432", ""},
		{"short database equal to a short user", with(endpoint("h", 5432, "db"), func(c *pgconn.Config) { c.User = "db" }), "h", "5432", ""},
		{"database that contains the user", with(endpoint("h", 5432, "myapp1_prod"), func(c *pgconn.Config) { c.User = "myapp1" }), "h", "5432", ""},
		{"database a password contains", with(endpoint("h", 5432, "orders"), func(c *pgconn.Config) { c.Password = "xorders99" }), "h", "5432", ""},
		{"database that shares four bytes with the user", with(endpoint("h", 5432, "app_db"), func(c *pgconn.Config) { c.User = "app_user" }), "h", "5432", ""},
		{"database that shares only three bytes", with(endpoint("h", 5432, "abcdb"), func(c *pgconn.Config) { c.User = "abcxuser" }), "h", "5432", "abcdb"},
		{"database that repeats a runtime setting", with(endpoint("h", 5432, "billing"), func(c *pgconn.Config) {
			c.RuntimeParams = map[string]string{"application_name": "billing-sync"}
		}), "h", "5432", ""},
		{"host equal to the password", with(endpoint("localhost", 5432, "d"), func(c *pgconn.Config) { c.Password = "localhost" }), "", "5432", "d"},
		{"host inside a password", with(endpoint("db.example.com", 5432, "d"), func(c *pgconn.Config) { c.Password = "pw-db.example.com-9" }), "", "5432", "d"},
		{"port equal to the password", with(endpoint("h", 5432, "d"), func(c *pgconn.Config) { c.Password = "5432" }), "h", "", "d"},
		{"port inside a password", with(endpoint("h", 5432, "d"), func(c *pgconn.Config) { c.Password = "x5432y" }), "h", "", "d"},
		{"short port equal to a short password", with(endpoint("h", 1, "d"), func(c *pgconn.Config) { c.Password = "1" }), "h", "", "d"},
		{"the plain-text alternative of a TLS connection is the same endpoint",
			with(endpoint("h", 5432, "d"), func(c *pgconn.Config) {
				c.Fallbacks = []*pgconn.FallbackConfig{{Host: "h", Port: 5432}}
			}), "h", "5432", "d"},
		{"a fallback on another host",
			with(endpoint("h", 5432, "d"), func(c *pgconn.Config) {
				c.Fallbacks = []*pgconn.FallbackConfig{{Host: "user=u", Port: 5432}}
			}), "", "", "d"},
		{"a fallback on another port",
			with(endpoint("h", 5432, "d"), func(c *pgconn.Config) {
				c.Fallbacks = []*pgconn.FallbackConfig{{Host: "h", Port: 5433}}
			}), "", "", "d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, database := nameParts(&tc.cfg)
			if host != tc.wantHost || port != tc.wantPort || database != tc.want {
				t.Errorf("nameParts = (%q, %q, %q), want (%q, %q, %q)", host, port, database, tc.wantHost, tc.wantPort, tc.want)
			}
		})
	}
}

func TestDescribe_ConnectErrorWithoutConfigNamesNothing(t *testing.T) {
	// A *pgconn.ConnectError built by hand has no configuration to read.
	err := (dsnInfo{}).describe("PingContext", &pgconn.ConnectError{})
	if err.Host != "" || err.Port != "" || err.Database != "" || err.Kind != FailureOther {
		t.Errorf("describe = %+v, want nothing named", *err)
	}
}

func TestDescribe_RegisteredConfigFromTheDriversOwnError(t *testing.T) {
	// A string pgx could not parse is described from the configuration in the
	// driver's own connect error.
	cfg := &pgconn.Config{Host: visibleHost, Port: 6432, Database: visibleDB, User: secretUser, Password: secretPassword}
	connectErr := &pgconn.ConnectError{Config: cfg}
	e := (dsnInfo{}).describe("sql.Open", connectErr)
	if e.Host != visibleHost || e.Port != "6432" || e.Database != visibleDB {
		t.Errorf("describe = %+v, want the parts of the driver's configuration", *e)
	}
	// When pgx did parse the string, its own configuration is the one used.
	own := &pgconn.Config{Host: "own.example", Port: 1, Database: "own"}
	e = (dsnInfo{cfg: own}).describe("sql.Open", connectErr)
	if e.Host != "own.example" || e.Database != "own" {
		t.Errorf("describe = %+v, want the parts of the configuration pgx read from the string", *e)
	}
}

func TestIsSettingName(t *testing.T) {
	for name, want := range map[string]bool{
		"application_name": true, "search_path": true, "_x": true, "my.setting": true, "A1": true,
		"": false, "1a": false, "a-b": false, "a b": false, "a@b": false, "a/b": false, "a:b": false, `"postgres://u:p@h/d?sslmode`: false,
	} {
		if got := isSettingName(name); got != want {
			t.Errorf("isSettingName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsMisSplitURL(t *testing.T) {
	for name, tc := range map[string]struct {
		dsn  string
		want bool
	}{
		"keyword string":     {"host=h user=u@x", false},
		"plain url":          {"postgres://u:p@h:5432/db?sslmode=disable", false},
		"encoded at sign":    {"postgresql://u:p@h/or%40ders", false},
		"at sign in path":    {"postgres://u:/p@h/db", true},
		"at sign in query":   {"postgres://u:?p@h/db", true},
		"number sign":        {"postgres://u:#p@h/db", true},
		"unparsable url":     {"postgres://h:bad port/d", true},
		"url with port only": {"postgres://h:5432", false},
	} {
		if got := isMisSplitURL(tc.dsn); got != tc.want {
			t.Errorf("%s: isMisSplitURL(%q) = %v, want %v", name, tc.dsn, got, tc.want)
		}
	}
}

func TestCollectionNotFoundErrors(t *testing.T) {
	// A bare name is quoted: the helper that reads NonDeterministicTextColumns
	// passes the table name it was given.
	for _, tc := range []struct{ name, want string }{
		{"Album", `dalgo2postgres: collection "Album" not found`},
		{`we"ird`, `dalgo2postgres: collection "we\"ird" not found`},
	} {
		if got := newCollectionNotFoundError(tc.name).Error(); got != tc.want {
			t.Errorf("not found error for %q = %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct{ schema, name, want string }{
		{"", "Album", `dalgo2postgres: collection "Album" not found`},
		{"sales", "Album", `dalgo2postgres: collection "sales"."Album" not found`},
	} {
		if got := newQualifiedCollectionNotFoundError(collectionLabel(tc.schema, tc.name)).Error(); got != tc.want {
			t.Errorf("qualified not found error = %q, want %q", got, tc.want)
		}
	}
}
