package dalgo2postgres

import (
	"context"
	"database/sql"
	"errors"
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
	resolved, err := resolveSettings(dalgo2sql.DbOptions{}, options)
	if err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	return newDatabaseFromSQL(sqlDB, dal.NewSchema(nil, nil), resolved, options), mock
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

func TestResolveSettings(t *testing.T) {
	const (
		exact = dalgo2sql.IdentifierCaseExact
		fold  = dalgo2sql.IdentifierCaseFoldLower
	)
	for _, tc := range []struct {
		name     string
		opts     dalgo2sql.DbOptions
		options  []Option
		wantMode IdentifierMode
		wantCase dalgo2sql.IdentifierCase
	}{
		{"nothing set: the default folds to lower case, as this package's DDL does", dalgo2sql.DbOptions{}, nil, IdentifierFoldLower, fold},
		{"nil options are ignored", dalgo2sql.DbOptions{}, []Option{nil, WithSchema("sales"), nil}, IdentifierFoldLower, fold},
		{"DbOptions.IdentifierCase exact", dalgo2sql.DbOptions{IdentifierCase: exact}, nil, IdentifierExact, exact},
		{"DbOptions.IdentifierCase fold-lower", dalgo2sql.DbOptions{IdentifierCase: fold}, nil, IdentifierFoldLower, fold},
		{"WithIdentifierMode(IdentifierExact)", dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierExact)}, IdentifierExact, exact},
		{"WithIdentifierMode(IdentifierFoldLower), set explicitly", dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierFoldLower)}, IdentifierFoldLower, fold},
		{"the last WithIdentifierMode wins", dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierExact), WithIdentifierMode(IdentifierFoldLower)}, IdentifierFoldLower, fold},
		{"both say exact", dalgo2sql.DbOptions{IdentifierCase: exact}, []Option{WithIdentifierMode(IdentifierExact)}, IdentifierExact, exact},
		{"both say fold-lower", dalgo2sql.DbOptions{IdentifierCase: fold}, []Option{WithIdentifierMode(IdentifierFoldLower)}, IdentifierFoldLower, fold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSettings(tc.opts, tc.options)
			if err != nil {
				t.Fatalf("resolveSettings: %v", err)
			}
			if got.mode != tc.wantMode || got.db.IdentifierCase != tc.wantCase {
				t.Errorf("mode = %v, IdentifierCase = %q; want %v, %q", got.mode, got.db.IdentifierCase, tc.wantMode, tc.wantCase)
			}
			// What the caller passed never decides the dialect or the placeholder style.
			if got.db.StructuredQueryDialect != "postgres" || got.db.Placeholder != dalgo2sql.PlaceholderDollar || got.db.IsAlreadyExists == nil {
				t.Errorf("db options = %+v, want the postgres dialect, dollar placeholders and the duplicate-key hook", got.db)
			}
		})
	}
}

func TestResolveSettings_ForcesTheDialectWhateverTheCallerSets(t *testing.T) {
	for _, dialect := range []string{"", "sqlite", "mysql", "nonsense", "postgres"} {
		got, err := resolveSettings(dalgo2sql.DbOptions{StructuredQueryDialect: dialect}, nil)
		if err != nil || got.db.StructuredQueryDialect != "postgres" {
			t.Errorf("StructuredQueryDialect %q: got dialect %q, err %v; want postgres", dialect, got.db.StructuredQueryDialect, err)
		}
	}
}

// fakeCompiler and fakeHintTranslator stand for a compiler and a hint translator a
// caller puts in DbOptions; neither is ever called, since the constructors refuse them.
type fakeCompiler struct{}

func (fakeCompiler) CompileNativeStructuredQuery(dal.StructuredQuery, dalgo2sql.NativeJoinHintFragments) (string, []any, error) {
	return "", nil, nil
}

type fakeHintTranslator struct{}

func (fakeHintTranslator) TranslateNativeJoinHints(dal.FromSource) (dalgo2sql.NativeJoinHintFragments, error) {
	return dalgo2sql.NativeJoinHintFragments{}, nil
}

// An option set that cannot be honoured is an error naming it. The three native hooks
// are among them: the statements of a structured query, what the database declares it
// runs on the server and its join check belong to dalgo2sql's PostgreSQL dialect
// together, and a caller's own compiler beside them would be sent whole grouped queries
// and joins the dialect promised and it never did.
func TestResolveSettings_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    dalgo2sql.DbOptions
		options []Option
		want    []string // every one must be in the message
	}{
		{"an IdentifierCase this package does not define", dalgo2sql.DbOptions{IdentifierCase: "upper"}, nil, []string{"IdentifierCase", `"upper"`, `"exact"`, `"fold-lower"`}},
		{"an out-of-range IdentifierMode", dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierMode(7))}, []string{"IdentifierMode", "7"}},
		{"a negative IdentifierMode", dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierMode(-1))}, []string{"IdentifierMode", "-1"}},
		{"the two disagree: exact in DbOptions, fold-lower in the option", dalgo2sql.DbOptions{IdentifierCase: dalgo2sql.IdentifierCaseExact},
			[]Option{WithIdentifierMode(IdentifierFoldLower)}, []string{"disagree", `"exact"`}},
		{"the two disagree: fold-lower in DbOptions, exact in the option", dalgo2sql.DbOptions{IdentifierCase: dalgo2sql.IdentifierCaseFoldLower},
			[]Option{WithIdentifierMode(IdentifierExact)}, []string{"disagree", `"fold-lower"`}},
		{"a caller's NativeStructuredQueryCompiler", dalgo2sql.DbOptions{NativeStructuredQueryCompiler: fakeCompiler{}}, nil,
			[]string{"compiled by the PostgreSQL dialect", "DbOptions.NativeStructuredQueryCompiler"}},
		{"a caller's NativeJoinEligibility", dalgo2sql.DbOptions{NativeJoinEligibility: func(context.Context, dal.StructuredQuery) error { return nil }}, nil,
			[]string{"compiled by the PostgreSQL dialect", "DbOptions.NativeJoinEligibility"}},
		{"a caller's NativeJoinHintTranslator", dalgo2sql.DbOptions{NativeJoinHintTranslator: fakeHintTranslator{}}, nil,
			[]string{"compiled by the PostgreSQL dialect", "DbOptions.NativeJoinHintTranslator"}},
		{"a native hook beside a valid option set", dalgo2sql.DbOptions{NativeStructuredQueryCompiler: fakeCompiler{}, IdentifierCase: dalgo2sql.IdentifierCaseExact},
			[]Option{WithSchema("sales"), WithIdentifierMode(IdentifierExact)}, []string{"DbOptions.NativeStructuredQueryCompiler"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveSettings(tc.opts, tc.options)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestIdentifierModeFollowsTheResolvedSetting(t *testing.T) {
	// DbOptions.IdentifierCase alone decides how the schema reader matches names too,
	// so the two never disagree about a name.
	d, _ := func() (*Database, sqlmock.Sqlmock) {
		sqlDB, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		resolved, err := resolveSettings(dalgo2sql.DbOptions{IdentifierCase: dalgo2sql.IdentifierCaseExact}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return newDatabaseFromSQL(sqlDB, dal.NewSchema(nil, nil), resolved, nil), mock
	}()
	if got := d.resolveName("Album"); got != "Album" {
		t.Errorf("resolveName(Album) = %q, want Album: DbOptions.IdentifierCase exact must reach the reader", got)
	}
}

// An Option is a func over *Database, so it can set any field of the value it is given,
// the embedded dal.DB included. The constructor builds the Database, applies the options,
// and only then assigns the handles and the mode it resolved, so an option written by a
// caller cannot swap the dalgo2sql database for one built with no dialect (whose
// structured queries would reach the legacy text emitter), nor the *sql.DB under it, nor
// the identifier mode the settings resolved. The option below does all three: with the
// options applied last, the first statement the database sends would be the legacy
// emitter's and not the typed compiler's catalog query.
func TestNewDatabaseFromSQL_AnOptionCannotSwapTheHandlesTheConstructorAssigns(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	otherDB, otherMock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := otherMock.ExpectationsWereMet(); err != nil {
			t.Errorf("the handle an option swapped in was used: %v", err)
		}
		_ = otherDB.Close()
	})
	legacy := dalgo2sql.NewDatabase(otherDB, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{})
	swap := func(d *Database) {
		d.DB = legacy
		d.sqlDB = otherDB
		d.identifierMode = IdentifierExact
	}

	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{}, swap)

	if db.sqlDB != sqlDB {
		t.Error("an option replaced the *sql.DB under the Database")
	}
	if got := db.resolveName("Album"); got != "album" {
		t.Errorf("resolveName(Album) = %q, want album: an option replaced the identifier mode the settings resolved", got)
	}
	mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
	mock.ExpectQuery(`SELECT "title" FROM "album" WHERE "albumid" = $1::bigint`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"title"}).AddRow("Seven"))
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(),
		albumFrom().NewQuery().Where(dal.WhereField("AlbumId", dal.Equal, 7)).SelectColumns(titleColumn()))
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	if rows := readAll(t, reader); len(rows) != 1 || rows[0]["Title"] != "Seven" {
		t.Errorf("rows = %v, want one row titled Seven, read by the typed compiler's statement", rows)
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

// A refused option set never reaches the driver: the opener is not called.
func TestNewDatabase_NativeHooksAreRefusedBeforeConnecting(t *testing.T) {
	opener := func(string, string) (*sql.DB, error) {
		t.Error("the opener was called for an option set the constructor refuses")
		return nil, errors.New("unreachable")
	}
	db, err := newDatabase("host=h", dal.NewSchema(nil, nil), dalgo2sql.DbOptions{NativeStructuredQueryCompiler: fakeCompiler{}}, nil, opener)
	if db != nil || err == nil || !strings.Contains(err.Error(), "DbOptions.NativeStructuredQueryCompiler") {
		t.Fatalf("got %v, %v; want nil and an error naming the field", db, err)
	}
	if _, err := NewDatabaseWithOptions("not-a-dsn", dal.NewSchema(nil, nil),
		dalgo2sql.DbOptions{NativeJoinEligibility: func(context.Context, dal.StructuredQuery) error { return nil }}); err == nil ||
		!strings.Contains(err.Error(), "DbOptions.NativeJoinEligibility") {
		t.Fatalf("NewDatabaseWithOptions: err = %v, want one naming the field", err)
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
