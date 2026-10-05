package dalgo2postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/stdlib"
)

// The stand-ins below give the tests of "an error holds no configuration" a database that has
// no server and a network that dials nothing: a connection configuration with a marker in the
// user, the password, the database, the host and one runtime parameter, whose dial function is
// replaced. A failure of such a configuration is pgx's own error, made by pgconn.ConnectConfig,
// so it holds exactly what a real one holds.

// The markers. Each is a string that no sentence, name or code of the adapter contains.
const (
	markUser     = "USERMARK-d41"
	markPassword = "PWMARK-d41"
	markDatabase = "DBMARK-d41"
	markHost     = "HOSTMARK-d41.example"
	markParam    = "PARAMMARK-d41"
)

var configMarkers = []string{markUser, markPassword, markDatabase, markHost, markParam}

// errDialRefused is what the stand-in network answers to every dial: a dial that failed, as the
// operating system's does.
var errDialRefused error = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("the stand-in network refuses every dial")}

func refuseEveryDial(context.Context, string, string) (net.Conn, error) { return nil, errDialRefused }

// standInConfig is a configuration with a marker in each of its parts. Names resolve to the
// loopback address without a lookup, and every dial is answered by dial.
func standInConfig(t *testing.T, dial pgconn.DialFunc) *pgx.ConnConfig {
	t.Helper()
	clearPGEnv(t)
	cfg, err := pgx.ParseConfig("postgres://" + markUser + ":" + markPassword + "@" + markHost + ":5432/" + markDatabase +
		"?sslmode=disable&application_name=" + markParam)
	if err != nil {
		t.Fatalf("the stand-in's configuration does not parse: %v", err)
	}
	cfg.LookupFunc = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	cfg.DialFunc = dial
	return cfg
}

// connectFailure is the error pgx returns when it cannot connect with the stand-in configuration:
// a *pgconn.ConnectError holding the whole configuration.
func connectFailure(t *testing.T, dial pgconn.DialFunc) error {
	t.Helper()
	cfg := standInConfig(t, dial)
	conn, err := pgconn.ConnectConfig(context.Background(), &cfg.Config)
	if err == nil {
		_ = conn.Close(context.Background())
		t.Fatal("the stand-in's connection succeeded")
	}
	return err
}

// serverAnswers is a dial function whose server answers the startup message with an error of the
// given SQLSTATE code and message, over an in-memory pipe: nothing is dialled.
func serverAnswers(code, message string) pgconn.DialFunc {
	return func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		// A test that stops half way must not hold the pipe for ever.
		deadline := time.Now().Add(30 * time.Second)
		_ = client.SetDeadline(deadline)
		_ = server.SetDeadline(deadline)
		go func() {
			defer func() { _ = server.Close() }()
			backend := pgproto3.NewBackend(server, server)
			if _, err := backend.ReceiveStartupMessage(); err != nil {
				return
			}
			backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code, Message: message})
			_ = backend.Flush()
		}()
		return client, nil
	}
}

// ordersRecordsets declares the table the record calls of the tests use, keyed by "id".
func ordersRecordsets() map[string]*dalgo2sql.Recordset {
	return map[string]*dalgo2sql.Recordset{
		"orders": dalgo2sql.NewRecordset("orders", dalgo2sql.Table, []dal.FieldRef{dal.Field("id")}),
	}
}

// databaseOver wraps an already open handle the way the constructors do, with the table the
// record calls use declared.
func databaseOver(t *testing.T, sqlDB *sql.DB) *Database {
	t.Helper()
	resolved, err := resolveSettings(dalgo2sql.DbOptions{Recordsets: ordersRecordsets()}, nil)
	if err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	return newDatabaseFromSQL(sqlDB, dal.NewSchema(nil, nil), resolved, nil)
}

// lazyDatabase is a Database that opened lazily, as one does that is verified by nothing but its
// first call, over a network that refuses every dial: every call that needs a connection fails
// with pgx's own error.
func lazyDatabase(t *testing.T, dial pgconn.DialFunc) *Database {
	t.Helper()
	sqlDB := stdlib.OpenDB(*standInConfig(t, dial))
	t.Cleanup(func() { _ = sqlDB.Close() })
	return databaseOver(t, sqlDB)
}

// failingStatements is a database/sql connector whose connections open and begin a transaction,
// and then fail every statement and every commit with failure. It is for the values the adapter
// hands out that live on a connection (a transaction), which a network that refuses every dial
// never lets exist.
type failingStatements struct{ failure error }

func (c failingStatements) Connect(context.Context) (driver.Conn, error) {
	return failingConn(c), nil
}
func (failingStatements) Driver() driver.Driver { return failingDriver{} }

type failingDriver struct{}

func (failingDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not used") }

type failingConn struct{ failure error }

func (c failingConn) Prepare(string) (driver.Stmt, error) { return nil, c.failure }
func (failingConn) Close() error                          { return nil }
func (c failingConn) Begin() (driver.Tx, error)           { return failingTx(c), nil }
func (c failingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return failingTx(c), nil
}

type failingTx struct{ failure error }

func (t failingTx) Commit() error { return t.failure }
func (failingTx) Rollback() error { return nil }

// transactionalDatabase is a Database whose connections fail every statement with failure.
func transactionalDatabase(t *testing.T, failure error) *Database {
	t.Helper()
	sqlDB := sql.OpenDB(failingStatements{failure})
	t.Cleanup(func() { _ = sqlDB.Close() })
	return databaseOver(t, sqlDB)
}

// anyStatement matches every statement a mock is asked for.
func anyStatement(string, string) error { return nil }

// mockedDatabase is a Database over a go-sqlmock handle that matches every statement.
func mockedDatabase(t *testing.T) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(anyStatement)))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
	return databaseOver(t, sqlDB), mock
}

// textQuery is a query the mocked handle runs as written.
func textQuery() dal.Query { return dal.NewTextQuery("SELECT n FROM orders", nil) }

// ordersRecord is a record of the declared table with a value to write.
func ordersRecord() dalrecord.Record {
	return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("orders", "1"), map[string]any{"status": "new"})
}

// unreadRecord is a record to read into: it has no data yet.
func unreadRecord() dalrecord.Record {
	return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("orders", "1"), map[string]any{})
}

// chainOf lists err and every error reachable from it through Unwrap() error and Unwrap() []error.
func chainOf(err error) []error {
	if err == nil {
		return nil
	}
	out := []error{err}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		out = append(out, chainOf(wrapped.Unwrap())...)
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			out = append(out, chainOf(inner)...)
		}
	}
	return out
}

// markerIn walks everything reachable from v (pointers, interfaces, structs with their
// unexported fields, slices, arrays and maps) and reports the first string that holds one of the
// markers, with the path to it.
func markerIn(v any, markers []string) (path, marker string, found bool) {
	return walkForMarker(reflect.ValueOf(v), "value", markers, map[uintptr]bool{})
}

func walkForMarker(v reflect.Value, path string, markers []string, seen map[uintptr]bool) (string, string, bool) {
	switch v.Kind() {
	case reflect.String:
		for _, m := range markers {
			if strings.Contains(v.String(), m) {
				return path, m, true
			}
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			for _, m := range markers {
				if strings.Contains(string(v.Bytes()), m) {
					return path, m, true
				}
			}
			return "", "", false
		}
		for i := 0; i < v.Len(); i++ {
			if p, m, ok := walkForMarker(v.Index(i), fmt.Sprintf("%s[%d]", path, i), markers, seen); ok {
				return p, m, true
			}
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if p, m, ok := walkForMarker(v.Index(i), fmt.Sprintf("%s[%d]", path, i), markers, seen); ok {
				return p, m, true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if p, m, ok := walkForMarker(it.Key(), path+"{key}", markers, seen); ok {
				return p, m, true
			}
			if p, m, ok := walkForMarker(it.Value(), path+"{value}", markers, seen); ok {
				return p, m, true
			}
		}
	case reflect.Interface:
		if !v.IsNil() {
			return walkForMarker(v.Elem(), path, markers, seen)
		}
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return "", "", false
		}
		seen[v.Pointer()] = true
		return walkForMarker(v.Elem(), path, markers, seen)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if p, m, ok := walkForMarker(v.Field(i), path+"."+v.Type().Field(i).Name, markers, seen); ok {
				return p, m, true
			}
		}
	}
	return "", "", false
}

// assertHoldsNoConfiguration is assertHoldsNone for the markers of the stand-in's configuration.
func assertHoldsNoConfiguration(t *testing.T, what string, err error) {
	t.Helper()
	assertHoldsNone(t, what, err, configMarkers)
}

// assertHoldsNone fails the test when err, or anything reachable from it, holds one of markers:
// not in the text of the error or of any error in its chain (%v, %+v and %#v), not in any value
// they point to, and not as one of the driver's types that hold the configuration.
func assertHoldsNone(t *testing.T, what string, err error, markers []string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: want an error, got nil", what)
		return
	}
	for _, e := range chainOf(err) {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			text := fmt.Sprintf(verb, e)
			for _, m := range markers {
				if strings.Contains(text, m) {
					t.Errorf("%s: %s of %T holds %q: %s", what, verb, e, m, text)
				}
			}
		}
	}
	if path, m, ok := markerIn(err, markers); ok {
		t.Errorf("%s: %q is reachable from the error at %s", what, m, path)
	}
	var connectErr *pgconn.ConnectError
	var configErr *pgconn.ParseConfigError
	var serverErr *pgconn.PgError
	switch {
	case errors.As(err, &connectErr):
		t.Errorf("%s: errors.As finds the driver's *pgconn.ConnectError", what)
	case errors.As(err, &configErr):
		t.Errorf("%s: errors.As finds the driver's *pgconn.ParseConfigError", what)
	case errors.As(err, &serverErr):
		t.Errorf("%s: errors.As finds the server's *pgconn.PgError", what)
	}
}
