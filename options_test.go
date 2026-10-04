package dalgo2postgres

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// newSchemaMockDatabase returns a Database over a go-sqlmock handle so schema-reader
// queries run without a PostgreSQL server. Expectations are ordered, and every
// one must have been met by the end of the test.
func newSchemaMockDatabase(t *testing.T, options ...Option) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = sqlDB.Close()
	})
	return newDatabaseFromSQL(sqlDB, "mock", dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, options), mock
}

func TestNewDatabaseFromSQL_Defaults(t *testing.T) {
	d, _ := newSchemaMockDatabase(t)
	if got := d.schemaName(); got != "public" {
		t.Errorf("schemaName() = %q, want %q", got, "public")
	}
	if got := d.resolveName("Album"); got != "album" {
		t.Errorf("default resolveName(Album) = %q, want folded %q (backwards compatible with the lower-casing DDL)", got, "album")
	}
	if d.DB == nil {
		t.Error("embedded dal.DB must be set")
	}
	if d.dsn != "mock" {
		t.Errorf("dsn = %q, want %q", d.dsn, "mock")
	}
}

func TestNewDatabaseFromSQL_Options(t *testing.T) {
	d, _ := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	if got := d.schemaName(); got != "sales" {
		t.Errorf("schemaName() = %q, want %q", got, "sales")
	}
	if got := d.resolveName("Album"); got != "Album" {
		t.Errorf("exact resolveName(Album) = %q, want %q", got, "Album")
	}
}

func TestWithSchema_EmptyKeepsDefault(t *testing.T) {
	d, _ := newSchemaMockDatabase(t, WithSchema("sales"), WithSchema(""))
	if got := d.schemaName(); got != "public" {
		t.Errorf("schemaName() = %q, want %q after WithSchema(\"\")", got, "public")
	}
}

func TestWithIdentifierMode_FoldLowerExplicit(t *testing.T) {
	d, _ := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact), WithIdentifierMode(IdentifierFoldLower))
	if got := d.resolveName("Album"); got != "album" {
		t.Errorf("resolveName(Album) = %q, want %q", got, "album")
	}
}

func TestZeroValueDatabaseUsesDefaults(t *testing.T) {
	var d Database
	if got := d.schemaName(); got != "public" {
		t.Errorf("zero Database schemaName() = %q, want public", got)
	}
}
