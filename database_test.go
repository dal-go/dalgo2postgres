package dalgo2postgres

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

const envDSN = "DALGO2POSTGRES_TEST_DSN"

// testDSN returns the DSN from the environment or skips the test.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(envDSN)
	if dsn == "" {
		t.Skipf("skipping DB test: %s not set", envDSN)
	}
	return dsn
}

// openTestDB opens a live Postgres connection and registers cleanup.
func openTestDB(t *testing.T) *Database {
	t.Helper()
	db, err := NewDatabase(testDSN(t))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openTestDBWithOpts opens a live Postgres connection with per-table recordset options.
func openTestDBWithOpts(t *testing.T, opts dalgo2sql.DbOptions) *Database {
	t.Helper()
	db, err := NewDatabaseWithOptions(testDSN(t), dal.NewSchema(nil, nil), opts)
	if err != nil {
		t.Fatalf("NewDatabaseWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// uniqueTable returns a test-unique table name so parallel tests don't clash.
func uniqueTable(t *testing.T, base string) string {
	// Replace characters not safe for Postgres identifiers.
	name := fmt.Sprintf("test_%s_%s", base, sanitizeName(t.Name()))
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func sanitizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			out = append(out, c)
		} else if c >= 'A' && c <= 'Z' {
			out = append(out, c+32) // to lower
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// dropTable drops a table unconditionally; used in test cleanup.
func dropTable(t *testing.T, db *Database, table string) {
	t.Helper()
	if _, err := db.sqlDB.ExecContext(context.Background(),
		`DROP TABLE IF EXISTS `+quoteIdent(table)); err != nil {
		t.Logf("dropTable %q: %v (non-fatal)", table, err)
	}
}

func TestNewDatabase_Connects(t *testing.T) {
	dsn := testDSN(t)
	db, err := NewDatabase(dsn)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()
	if db == nil {
		t.Fatal("NewDatabase returned nil")
	}
}

func TestNewDatabase_RejectsBadDSN(t *testing.T) {
	db, err := NewDatabase("postgres://nobody:wrong@127.0.0.1:15432/noexist?sslmode=disable")
	if err == nil {
		_ = db.Close()
		t.Fatal("NewDatabase: expected error on bad DSN, got nil")
	}
	if db != nil {
		t.Errorf("expected nil db on error, got %T", db)
	}
}

func TestDatabase_SupportsConcurrentConnections_True(t *testing.T) {
	db := openTestDB(t)
	if !db.SupportsConcurrentConnections() {
		t.Error("expected SupportsConcurrentConnections() == true for Postgres, got false")
	}
}
