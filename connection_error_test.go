package dalgo2postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
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
	} {
		t.Run(name, func(t *testing.T) {
			open := func(string, string) (*sql.DB, error) {
				t.Error("the driver must not be asked to open a DSN that cannot be parsed")
				return nil, errDriverBoom
			}
			db, err := openVerified(dsn, open)
			if db != nil {
				t.Errorf("db = %v, want nil", db)
			}
			assertNoSecrets(t, err, []string{secretUser, secretPassword, visibleHost, visibleDB, dsn, "notaport"})
			if !strings.Contains(err.Error(), "cannot be parsed") {
				t.Errorf("error %q should say the connection string cannot be parsed", err)
			}
			if errors.Unwrap(err) == nil {
				t.Errorf("the parse error should stay reachable through Unwrap")
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
