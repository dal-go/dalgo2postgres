package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The server's answers to a connection, met at the first call of a handle that opened lazily
// (nothing verified it at construction): a wrong password, a database that is not there, and a
// role that may not log in. Each is a connection error of the kind the server's answer names
// (FailureServer with its SQLSTATE), with a fixed sentence, and the name of the role or of the
// database, which the server's own message holds, is nowhere in it. These tests run in the
// Conformance job, on every server version of its matrix, and the job requires each to pass.

// uniqueName is a role or database name that nothing else uses, and that no sentence of the
// adapter contains.
func uniqueName(kind string) string {
	return fmt.Sprintf("dgo2pg_%s_%d_%d", kind, os.Getpid(), time.Now().UnixNano()%1_000_000_000)
}

// lazyDatabaseWith opens a handle to the server of the test DSN with the configuration changed
// by adjust. Nothing is sent when it is opened: the first call connects.
func lazyDatabaseWith(t *testing.T, adjust func(cfg *pgx.ConnConfig)) *Database {
	t.Helper()
	clearPGEnv(t)
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	adjust(cfg)
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return databaseOver(t, sqlDB)
}

// createRole creates a role on the server, and drops it when the test ends. The test DSN's user
// must be allowed to create roles (the Conformance job's is the superuser).
func createRole(t *testing.T, name, options string) {
	t.Helper()
	admin := openTestDB(t)
	ctx := context.Background()
	if _, err := admin.sqlDB.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s %s", quoteIdent(name), options)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.sqlDB.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+quoteIdent(name)) })
}

// firstCalls are calls that each need a connection. Each is the first call of a fresh handle.
var firstCalls = map[string]func(ctx context.Context, db *Database) []error{
	"ListSchemas": func(ctx context.Context, db *Database) []error {
		_, err := db.ListSchemas(ctx)
		return []error{err}
	},
	"DescribeCollection": func(ctx context.Context, db *Database) []error {
		ref := dal.NewRootCollectionRef("orders", "")
		_, err := db.DescribeCollection(ctx, &ref)
		return []error{err}
	},
	"Get": func(ctx context.Context, db *Database) []error {
		rec := unreadRecord()
		err := db.Get(ctx, rec)
		return []error{err, rec.Error()}
	},
	"ExecuteQueryToRecordsReader": func(ctx context.Context, db *Database) []error {
		_, err := db.ExecuteQueryToRecordsReader(ctx, selectOrders())
		return []error{err}
	},
	"RunReadonlyTransaction": func(ctx context.Context, db *Database) []error {
		return []error{db.RunReadonlyTransaction(ctx, func(context.Context, dal.ReadTransaction) error { return nil })}
	},
}

// assertClassified checks that each first call fails with the connection error the server's
// answer names, with its fixed sentence, and that nothing in it holds a name.
func assertClassified(t *testing.T, open func(t *testing.T) *Database, code string, names []string) {
	t.Helper()
	want := "dalgo2postgres: " + serverSentences[code] + " (SQLSTATE " + code + ")"
	for call, run := range firstCalls {
		t.Run(call, func(t *testing.T) {
			for i, err := range run(context.Background(), open(t)) {
				what := fmt.Sprintf("%s (error %d)", call, i+1)
				failure, ok := err.(*ConnectionError)
				if !ok {
					t.Errorf("%s: err is %T %q, want a *ConnectionError", what, err, err)
					continue
				}
				if failure.Kind != FailureServer || failure.SQLState != code {
					t.Errorf("%s: kind %d and SQLSTATE %q, want FailureServer and %q", what, failure.Kind, failure.SQLState, code)
				}
				if err.Error() != want {
					t.Errorf("%s: text = %q, want %q", what, err, want)
				}
				assertHoldsNone(t, what, err, names)
			}
		})
	}
}

func TestConnectionFailureIntegration_WrongPasswordAtTheFirstCall(t *testing.T) {
	testDSN(t) // skips the test, and not only its calls, when no server is given
	role := uniqueName("pw")
	createRole(t, role, "LOGIN PASSWORD 'the-right-password'")
	assertClassified(t, func(t *testing.T) *Database {
		return lazyDatabaseWith(t, func(cfg *pgx.ConnConfig) { cfg.User, cfg.Password = role, "a-wrong-password" })
	}, "28P01", []string{role, "a-wrong-password"})
}

func TestConnectionFailureIntegration_MissingDatabaseAtTheFirstCall(t *testing.T) {
	testDSN(t)
	database := uniqueName("nodb")
	assertClassified(t, func(t *testing.T) *Database {
		return lazyDatabaseWith(t, func(cfg *pgx.ConnConfig) { cfg.Database = database })
	}, "3D000", []string{database})
}

func TestConnectionFailureIntegration_RoleThatMayNotLogInAtTheFirstCall(t *testing.T) {
	testDSN(t)
	role := uniqueName("nologin")
	createRole(t, role, "NOLOGIN PASSWORD 'the-password'")
	assertClassified(t, func(t *testing.T) *Database {
		return lazyDatabaseWith(t, func(cfg *pgx.ConnConfig) { cfg.User, cfg.Password = role, "the-password" })
	}, "28000", []string{role, "the-password"})
}

// A statement error is the server's answer to the statement and is not changed: a constraint, a
// type and a syntax error each reach the caller as the server's *pgconn.PgError, whose message
// and detail are what the caller prints. The README says so.
func TestConnectionFailureIntegration_StatementErrorsKeepTheServersMessageAndDetail(t *testing.T) {
	table := uniqueName("stmt") // short enough that the server does not cut the name of the key it derives from it
	db := openTestDBWithOpts(t, dalgo2sql.DbOptions{Recordsets: map[string]*dalgo2sql.Recordset{
		table: dalgo2sql.NewRecordset(table, dalgo2sql.Table, []dal.FieldRef{dal.Field("id")}),
	}})
	ctx := context.Background()
	if _, err := db.sqlDB.ExecContext(ctx, "CREATE TABLE "+quoteIdent(table)+" (id text PRIMARY KEY, name text)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer dropTable(t, db, table)
	insert := func() error {
		return db.Insert(ctx, dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(table, "dup"), map[string]any{"name": "x"}))
	}
	if err := insert(); err != nil {
		t.Fatalf("the first insert: %v", err)
	}
	_, typeErr := db.ExecuteQueryToRecordsReader(ctx, dal.NewTextQuery("SELECT 'not a number'::integer", nil))
	_, syntaxErr := db.ExecuteQueryToRecordsReader(ctx, dal.NewTextQuery("SELEC 1", nil))

	for _, tc := range []struct {
		name, code string
		err        error
		message    string // a part of the server's message
		detail     string // a part of the server's detail
	}{
		{"constraint", "23505", insert(), `duplicate key value violates unique constraint "` + table + `_pkey"`, "Key (id)=(dup) already exists."},
		{"type", "22P02", typeErr, `invalid input syntax for type integer: "not a number"`, ""},
		{"syntax", "42601", syntaxErr, `syntax error at or near "SELEC"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var serverErr *pgconn.PgError
			if !errors.As(tc.err, &serverErr) {
				t.Fatalf("err = %v, want one that holds the server's *pgconn.PgError", tc.err)
			}
			if _, isConnection := tc.err.(*ConnectionError); isConnection {
				t.Errorf("a statement error was made a connection error: %v", tc.err)
			}
			if serverErr.Code != tc.code || !strings.Contains(serverErr.Message, tc.message) || !strings.Contains(serverErr.Detail, tc.detail) {
				t.Errorf("the server's error = %q %q %q, want code %s, message with %q and detail with %q",
					serverErr.Code, serverErr.Message, serverErr.Detail, tc.code, tc.message, tc.detail)
			}
			if !strings.Contains(tc.err.Error(), tc.message) {
				t.Errorf("the text %q does not hold the server's message", tc.err)
			}
		})
	}
}

// A session the server ends in the middle of a transaction (an administrator terminates the
// backend, as a shutdown does) is a connection error from both transaction methods: whichever way
// the end reaches the caller (the answer to the statement, the end of the stream, the commit that
// finds the connection closed), and whether the worker returns the statement's error or not, and
// the rollback that follows cannot succeed. The worker here ends its own session.
func TestConnectionFailureIntegration_ASessionTheServerEndsInATransaction(t *testing.T) {
	testDSN(t)
	for _, kind := range transactionKinds() {
		t.Run(kind.name, func(t *testing.T) {
			db := lazyDatabaseWith(t, func(*pgx.ConnConfig) {})
			err := kind.run(db, func(ctx context.Context, tx dal.ReadSession) error {
				reader, err := tx.ExecuteQueryToRecordsReader(ctx, dal.NewTextQuery("SELECT pg_terminate_backend(pg_backend_pid())", nil))
				if err != nil {
					return err
				}
				defer func() { _ = reader.Close() }()
				for {
					if _, err := reader.Next(); err != nil {
						if errors.Is(err, dal.ErrNoMoreRecords) {
							return nil
						}
						return err
					}
				}
			})
			failure := assertAClassifiedConnectionError(t, err)
			if failure != nil && failure.Kind == FailureServer && failure.SQLState != "57P01" {
				t.Errorf("SQLSTATE = %q, want 57P01 (admin_shutdown) when the server's answer reached the caller", failure.SQLState)
			}
		})
	}
}
