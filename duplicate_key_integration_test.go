package dalgo2postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5/pgconn"
)

// newMockDatabase builds a *Database around a sqlmock-backed *sql.DB rather
// than a live PostgreSQL server, wiring it up exactly the way
// NewDatabaseWithOptions does (same DbOptions defaulting via
// applyPostgresDbOptionDefaults), so it exercises this package's real
// Database.Insert -> backendWriter -> dalgo2sql production code path. This
// repo has no CI service container and this sandbox has neither Docker nor
// a local PostgreSQL install (see TestConformance's doc comment), so this
// is how the IsAlreadyExists wiring is verified end-to-end without a live
// server: only the database/sql driver layer is faked, everything above it
// — Database.Insert, dal.As-based backendWriter recovery, dalgo2sql's
// execInsert choke point, and this package's IsAlreadyExists classifier —
// is the real, unmodified production code.
func newMockDatabase(t *testing.T, tbl string) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	opts := dalgo2sql.DbOptions{
		Recordsets: map[string]*dalgo2sql.Recordset{
			tbl: dalgo2sql.NewRecordset(tbl, dalgo2sql.Table, []dal.FieldRef{dal.Field("ID")}),
		},
	}
	applyPostgresDbOptionDefaults(&opts)

	innerDB := dalgo2sql.NewDatabase(sqlDB, dal.NewSchema(nil, nil), opts)
	return &Database{DB: innerDB, sqlDB: sqlDB}, mock
}

type mockRecordData struct {
	Name string
}

// TestDatabase_Insert_DuplicateKey_EndToEnd proves that a *Database
// constructed the same way NewDatabaseWithOptions builds one rejects an
// Insert over an existing key with record.IsAlreadyExists when the
// underlying driver reports SQLSTATE 23505 — the same assertion
// dalgotest's unconditional conformance check makes against a live server
// (see TestConformance) — using a mocked driver error instead of a real
// PostgreSQL instance.
func TestDatabase_Insert_DuplicateKey_EndToEnd(t *testing.T) {
	const tbl = "widgets"

	t.Run("23505 unique_violation is reported as record.IsAlreadyExists", func(t *testing.T) {
		db, mock := newMockDatabase(t, tbl)
		pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
		mock.ExpectExec("INSERT INTO widgets(ID, Name) VALUES ($1, $2)").
			WithArgs("id1", "w1").
			WillReturnError(pgErr)

		record := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "id1"), &mockRecordData{Name: "w1"})
		err := db.Insert(context.Background(), record)

		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !dalrecord.IsAlreadyExists(err) {
			t.Errorf("expected record.IsAlreadyExists(err) = true, got false; err = %v", err)
		}
		if !errors.Is(err, pgErr) {
			t.Errorf("expected the original *pgconn.PgError to remain in the chain; err = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("23503 foreign_key_violation is NOT reported as record.IsAlreadyExists", func(t *testing.T) {
		db, mock := newMockDatabase(t, tbl)
		pgErr := &pgconn.PgError{Code: "23503", Message: "insert or update on table violates foreign key constraint"}
		mock.ExpectExec("INSERT INTO widgets(ID, Name) VALUES ($1, $2)").
			WithArgs("id1", "w1").
			WillReturnError(pgErr)

		record := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "id1"), &mockRecordData{Name: "w1"})
		err := db.Insert(context.Background(), record)

		if !errors.Is(err, pgErr) {
			t.Fatalf("expected the original error unchanged, got: %v", err)
		}
		if dalrecord.IsAlreadyExists(err) {
			t.Errorf("expected record.IsAlreadyExists(err) = false for a foreign-key violation; err = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("a non-PgError failure is NOT reported as record.IsAlreadyExists", func(t *testing.T) {
		db, mock := newMockDatabase(t, tbl)
		otherErr := errors.New("connection reset by peer")
		mock.ExpectExec("INSERT INTO widgets(ID, Name) VALUES ($1, $2)").
			WithArgs("id1", "w1").
			WillReturnError(otherErr)

		record := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "id1"), &mockRecordData{Name: "w1"})
		err := db.Insert(context.Background(), record)

		if !errors.Is(err, otherErr) {
			t.Fatalf("expected the original error unchanged, got: %v", err)
		}
		if dalrecord.IsAlreadyExists(err) {
			t.Errorf("expected record.IsAlreadyExists(err) = false for an unrelated error; err = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}
