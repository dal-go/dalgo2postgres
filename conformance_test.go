package dalgo2postgres

import (
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dalgotest"
	"github.com/dal-go/dalgo2sql"
)

// TestConformance runs the shared dalgotest suite against a live PostgreSQL
// server, the same as every other DB-backed test in this package: it needs
// DALGO2POSTGRES_TEST_DSN (see testDSN in database_test.go) and skips when
// that is not set.
//
// This repo has no CI service container wired up for PostgreSQL, so as of
// this change the suite has been exercised locally only where
// DALGO2POSTGRES_TEST_DSN happened to be set — it has not been run in CI.
// Wiring a PostgreSQL service container into .github/workflows/ci.yml is a
// separate follow-up.
func TestConformance(t *testing.T) {
	tbl := uniqueTable(t, "conformance")
	opts := dalgo2sql.DbOptions{
		Recordsets: map[string]*dalgo2sql.Recordset{
			tbl: dalgo2sql.NewRecordset(tbl, dalgo2sql.Table, []dal.FieldRef{dal.Field("ID")}),
		},
	}

	// Column case matches dalgotest.Record's Go field names (ID, Name):
	// dalgo2sql derives column names from the struct via reflection.
	// PostgreSQL folds unquoted identifiers to lower case (see quoteIdent),
	// so the raw DDL below and the unquoted identifiers dalgo2sql's DML
	// emits fold to the same physical column regardless of the case used
	// here.
	setup := openTestDBWithOpts(t, opts)
	if _, err := setup.sqlDB.Exec(`CREATE TABLE ` + quoteIdent(tbl) + ` (
		ID   VARCHAR(255) NOT NULL PRIMARY KEY,
		Name VARCHAR(255)
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	t.Cleanup(func() { dropTable(t, setup, tbl) })

	dalgotest.RunConformance(t, func(t *testing.T) (dal.DB, func()) {
		return openTestDBWithOpts(t, opts), nil
	}, dalgotest.WithCollection(tbl))
}
