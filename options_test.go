package dalgo2postgres

import (
	"strings"
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

func TestNilOptionIsIgnored(t *testing.T) {
	d, _ := newSchemaMockDatabase(t, nil, WithSchema("sales"), nil)
	if got := d.schemaName(); got != "sales" {
		t.Errorf("schemaName() = %q, want sales (nil options skipped, the rest applied)", got)
	}
}

func TestCheckOptions(t *testing.T) {
	if err := checkOptions([]Option{nil, WithSchema("sales"), WithIdentifierMode(IdentifierExact)}); err != nil {
		t.Errorf("valid options: %v", err)
	}
	if err := checkOptions(nil); err != nil {
		t.Errorf("no options: %v", err)
	}
	err := checkOptions([]Option{WithIdentifierMode(IdentifierMode(7))})
	if err == nil || !strings.Contains(err.Error(), "IdentifierMode") || !strings.Contains(err.Error(), "7") {
		t.Errorf("out-of-range mode: err = %v, want an error naming IdentifierMode and 7", err)
	}
}

func TestNewDatabaseWithOptions_RejectsUnknownIdentifierModeBeforeConnecting(t *testing.T) {
	// The DSN is unusable on purpose: the error must come from the option check,
	// before any connection is attempted.
	db, err := NewDatabaseWithOptions("not-a-dsn", dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, WithIdentifierMode(IdentifierMode(-1)))
	if db != nil || err == nil || !strings.Contains(err.Error(), "IdentifierMode") {
		t.Fatalf("got %v, %v; want nil and an IdentifierMode error", db, err)
	}
	if _, err := NewDatabase("not-a-dsn", WithIdentifierMode(IdentifierMode(99))); err == nil || !strings.Contains(err.Error(), "IdentifierMode") {
		t.Fatalf("NewDatabase: err = %v, want an IdentifierMode error", err)
	}
}

func TestSchemaFor(t *testing.T) {
	d, _ := newSchemaMockDatabase(t, WithSchema("sales"))
	plain := dal.NewRootCollectionRef("Album", "")
	qualified := dal.NewQualifiedRootCollectionRef("archive", "Album", "")
	if got := d.schemaFor(&plain); got != "sales" {
		t.Errorf("schemaFor(plain) = %q, want sales", got)
	}
	if got := d.schemaFor(&qualified); got != "archive" {
		t.Errorf("schemaFor(qualified) = %q, want archive", got)
	}
}
