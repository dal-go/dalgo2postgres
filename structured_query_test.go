package dalgo2postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// The tests in this file run the real constructor body (newDatabase), the real
// dalgo2sql and the real typed PostgreSQL compiler over a go-sqlmock handle, so
// they pin the statements this adapter sends without a server. The server's own
// answer to those statements is pinned by the integration tests
// (structured_query_integration_test.go), which only CI can run.

// catalogStatement is the marker an expectation uses for the catalog lookup the
// typed compiler runs before each structured read. Its text is dalgo2sql's; a
// test checks only that the statement is the recursive catalog query.
const catalogStatement = "<catalog query>"

// statementMatcher matches the catalog lookup by its opening and every other
// statement exactly (after collapsing white space).
func statementMatcher(expected, actual string) error {
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if expected == catalogStatement {
		if !strings.HasPrefix(collapse(actual), "WITH RECURSIVE s(name) AS (VALUES ") {
			return fmt.Errorf("statement %q is not the catalog query", collapse(actual))
		}
		return nil
	}
	if collapse(expected) != collapse(actual) {
		return fmt.Errorf("statement\n got: %s\nwant: %s", collapse(actual), collapse(expected))
	}
	return nil
}

func newStructuredMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(statementMatcher)))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = sqlDB.Close()
	})
	return sqlDB, mock
}

// openMockedDatabase runs the body of both public constructors over sqlDB.
func openMockedDatabase(t *testing.T, sqlDB *sql.DB, opts dalgo2sql.DbOptions, options ...Option) *Database {
	t.Helper()
	clearPGEnv(t)
	db, err := newDatabase("host=h", dal.NewSchema(nil, nil), opts, options,
		func(string, string) (*sql.DB, error) { return sqlDB, nil })
	if err != nil {
		t.Fatalf("newDatabase: %v", err)
	}
	return db
}

// catalogColumns are the columns of the catalog lookup's answer.
var catalogColumns = []string{"name", "attname", "data_type", "category", "type_oid", "type_elem", "attnotnull", "nondeterministic"}

// catalogColumn is one column of a source in a mocked catalog answer.
type catalogColumn struct {
	name, dataType, category string
	oid                      int64
	notNull                  bool
}

var (
	intColumn  = func(name string, notNull bool) catalogColumn { return catalogColumn{name, "integer", "N", 23, notNull} }
	textColumn = func(name string) catalogColumn { return catalogColumn{name, "text", "S", 25, false} }
	numColumn  = func(name string) catalogColumn { return catalogColumn{name, "numeric", "N", 1700, false} }
)

// catalogRows answers the catalog lookup for one relation, written as the
// statement will write it (the bound text, quoted).
func catalogRows(relation string, columns ...catalogColumn) *sqlmock.Rows {
	rows := sqlmock.NewRows(catalogColumns)
	for _, c := range columns {
		rows.AddRow(relation, c.name, c.dataType, c.category, c.oid, int64(0), c.notNull, false)
	}
	return rows
}

// albumCatalog is the catalog of a table whose columns are written in lower
// case, as this package's DDL stores them.
func albumCatalog(relation string) *sqlmock.Rows {
	return catalogRows(relation, intColumn("albumid", true), textColumn("title"), numColumn("price"))
}

func albumFrom() dal.FromSource { return dal.From(dal.NewRootCollectionRef("Album", "")) }

func titleColumn() dal.Column { return dal.Column{Expression: dal.NewFieldRef("", "Title")} }

func readAll(t *testing.T, reader dal.RecordsReader) []map[string]any {
	t.Helper()
	defer func() { _ = reader.Close() }()
	var out []map[string]any
	for {
		rec, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, rec.Data().(map[string]any))
	}
}

const hostile = "O'Brien'; DROP TABLE album; --"

// A structured query through the constructors' body can never reach the legacy
// text emitter: whatever the caller sets, the statement is the typed compiler's,
// names are quoted, and the value is a bound argument.
func TestNewDatabase_StructuredQueryNeverReachesTheLegacyEmitter(t *testing.T) {
	for _, dialect := range []string{"", "sqlite", "mysql", "nonsense", "postgres"} {
		t.Run(fmt.Sprintf("StructuredQueryDialect %q", dialect), func(t *testing.T) {
			sqlDB, mock := newStructuredMock(t)
			db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{StructuredQueryDialect: dialect})
			mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
			mock.ExpectQuery(`SELECT "title" FROM "album" WHERE ("title" = $1 AND "albumid" > $2::bigint)`).
				WithArgs(hostile, int64(7)).
				WillReturnRows(sqlmock.NewRows([]string{"title"}).AddRow("Seven"))

			q := albumFrom().NewQuery().
				Where(dal.WhereField("Title", dal.Equal, hostile), dal.WhereField("AlbumId", dal.GreaterThen, 7)).
				SelectColumns(titleColumn())
			reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
			if err != nil {
				t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
			}
			rows := readAll(t, reader)
			if len(rows) != 1 || rows[0]["Title"] != "Seven" {
				t.Errorf("rows = %v, want one row titled Seven (named as the query asked)", rows)
			}
		})
	}
}

var _ = driver.Value(nil)
