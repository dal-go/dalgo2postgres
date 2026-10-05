package dalgo2postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
	"github.com/dal-go/record/update"
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

// catalogColumns are the columns of the catalog lookup's answer. The last, pk, is the
// catalog's own primary key flag (dalgo2sql v0.26.3): the mocked sources declare none, so
// every row says false and the keys come from the options a test passes.
var catalogColumns = []string{"name", "attname", "data_type", "category", "type_oid", "type_elem", "attnotnull", "nondeterministic", "pk"}

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
		rows.AddRow(relation, c.name, c.dataType, c.category, c.oid, int64(0), c.notNull, false, false)
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

// Identifier case: this package's default folds names to lower case (its DDL does),
// the exact mode writes them as the query spells them, and DbOptions.IdentifierCase
// reaches the compiler as well as the option does.
func TestNewDatabase_IdentifierModesDecideHowNamesAreWritten(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      dalgo2sql.DbOptions
		options   []Option
		relation  string
		statement string
		columns   []catalogColumn
	}{
		{"the default folds to lower case", dalgo2sql.DbOptions{}, nil,
			`"album"`, `SELECT "title" FROM "album"`,
			[]catalogColumn{textColumn("title")}},
		{"WithIdentifierMode(IdentifierExact)", dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierExact)},
			`"Album"`, `SELECT "Title" FROM "Album"`,
			[]catalogColumn{textColumn("Title")}},
		{"DbOptions.IdentifierCase exact", dalgo2sql.DbOptions{IdentifierCase: dalgo2sql.IdentifierCaseExact}, nil,
			`"Album"`, `SELECT "Title" FROM "Album"`,
			[]catalogColumn{textColumn("Title")}},
		{"DbOptions.IdentifierCase fold-lower", dalgo2sql.DbOptions{IdentifierCase: dalgo2sql.IdentifierCaseFoldLower}, nil,
			`"album"`, `SELECT "title" FROM "album"`,
			[]catalogColumn{textColumn("title")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB, mock := newStructuredMock(t)
			db := openMockedDatabase(t, sqlDB, tc.opts, tc.options...)
			mock.ExpectQuery(catalogStatement).WithArgs(tc.relation).WillReturnRows(catalogRows(tc.relation, tc.columns...))
			mock.ExpectQuery(tc.statement).WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow("One"))
			reader, err := db.ExecuteQueryToRecordsReader(context.Background(), albumFrom().NewQuery().SelectColumns(titleColumn()))
			if err != nil {
				t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
			}
			if rows := readAll(t, reader); len(rows) != 1 || rows[0]["Title"] != "One" {
				t.Errorf("rows = %v, want one row keyed by the name the query asked for (Title)", rows)
			}
		})
	}
}

// Behaviour change (dalgo2sql v0.26.0): a table the catalog does not know is a
// *dalgo2sql.TableNotFoundError that names the nearest table and, in the exact
// mode, says names are case-sensitive; no statement is sent for it.
func TestNewDatabase_UnknownTableIsATableNotFoundErrorWithASuggestion(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{}, WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(catalogRows(`"album"`))
	mock.ExpectQuery("SELECT n.nspname::text, c.relname::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
		"WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND pg_catalog.has_table_privilege(c.oid, 'SELECT') " +
		"AND CASE WHEN $1::text = '' THEN pg_catalog.pg_table_is_visible(c.oid) AND n.nspname NOT IN ('pg_catalog', 'information_schema') " +
		"ELSE pg_catalog.lower(n.nspname::text) = pg_catalog.lower($1::text) END ORDER BY n.nspname, c.relname LIMIT 5000").
		WithArgs("").
		WillReturnRows(sqlmock.NewRows([]string{"nspname", "relname"}).AddRow("public", "Album").AddRow("public", "Artist"))

	q := dal.From(dal.NewRootCollectionRef("album", "")).NewQuery().SelectColumns(titleColumn())
	_, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	var notFound *dalgo2sql.TableNotFoundError
	if !errors.Is(err, dalgo2sql.ErrTableNotFound) || !errors.As(err, &notFound) {
		t.Fatalf("error = %v, want a table-not-found error", err)
	}
	if notFound.Name != "album" || notFound.SuggestedName != "Album" || !notFound.CaseSensitive {
		t.Errorf("error = %+v, want album not found, suggesting Album, case-sensitive", *notFound)
	}
	if want := `table "album" not found; did you mean "Album"? Table names are case-sensitive.`; !strings.Contains(err.Error(), want) {
		t.Errorf("message = %q, want it to contain %q", err, want)
	}
}

// Behaviour change (dalgo2sql v0.26.0): aggregation is declared native, so DALgo
// sends GROUP BY, HAVING and the aggregates to the server in one statement (SUM and
// AVG cast to double precision, COUNT, MIN and MAX native) instead of reading every
// row and aggregating in memory.
func TestNewDatabase_AggregationRunsOnTheServer(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
	mock.ExpectQuery(`SELECT "title", COUNT(*) AS "n", ((SUM("price"))::double precision) AS "total", MIN("albumid") AS "first" ` +
		`FROM "album" GROUP BY "title" HAVING COUNT(*) > $1::bigint`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"title", "n", "total", "first"}).AddRow("Dup", int64(2), 19.5, int64(3)))

	q := albumFrom().NewQuery().
		GroupBy(dal.NewFieldRef("", "Title")).
		Having(dal.NewComparison(dal.NewAggregate(dal.COUNT, false, dal.Star()), dal.GreaterThen, dal.NewConstant(1))).
		SelectColumns(
			titleColumn(),
			dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"},
			dal.SumAs(dal.NewFieldRef("", "Price"), "total"),
			dal.MinAs(dal.NewFieldRef("", "AlbumId"), "first"),
		)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	rows := readAll(t, reader)
	if len(rows) != 1 || rows[0]["Title"] != "Dup" || rows[0]["n"] != int64(2) || rows[0]["total"] != 19.5 || rows[0]["first"] != int64(3) {
		t.Errorf("rows = %v, want the one group the server returned, keyed as the query asked", rows)
	}
}

// Behaviour change (dalgo2sql v0.26.0): FIRST and LAST need a stable input order,
// which PostgreSQL does not promise and this adapter does not declare, so DALgo
// refuses the query itself and sends nothing to the server.
func TestNewDatabase_FirstAndLastAreRefusedBeforeAnythingIsSent(t *testing.T) {
	for name, column := range map[string]dal.Column{
		"FIRST": dal.FirstAs(dal.NewFieldRef("", "Title"), "x"),
		"LAST":  dal.LastAs(dal.NewFieldRef("", "Title"), "x"),
	} {
		t.Run(name, func(t *testing.T) {
			sqlDB, _ := newStructuredMock(t) // no expectation: any statement fails the test
			db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
			q := albumFrom().NewQuery().GroupBy(dal.NewFieldRef("", "AlbumId")).SelectColumns(column)
			_, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
			if err == nil || !strings.Contains(err.Error(), "FIRST/LAST require a provider-declared stable input order") {
				t.Errorf("error = %v, want the planner's refusal of FIRST/LAST", err)
			}
		})
	}
}

// Behaviour change (dalgo2sql v0.26.0): a read inside a transaction compiles
// with the dialect too (it used the legacy emitter before), and the catalog lookup
// runs on the transaction, so the two statements see one session.
func TestNewDatabase_ReadInATransactionUsesTheTypedCompiler(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	mock.ExpectBegin()
	mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
	mock.ExpectQuery(`SELECT "title" FROM "album" WHERE "albumid" = $1::bigint`).WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"title"}).AddRow("Seven"))
	mock.ExpectCommit()

	err := db.RunReadonlyTransaction(context.Background(), func(ctx context.Context, tx dal.ReadTransaction) error {
		q := albumFrom().NewQuery().Where(dal.WhereField("AlbumId", dal.Equal, 7)).SelectColumns(titleColumn())
		reader, err := tx.ExecuteQueryToRecordsReader(ctx, q)
		if err != nil {
			return err
		}
		if rows := readAll(t, reader); len(rows) != 1 || rows[0]["Title"] != "Seven" {
			t.Errorf("rows = %v, want the Seven row", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunReadonlyTransaction: %v", err)
	}
}

// Behaviour change (dalgo2sql v0.26.0): a NUMERIC column, which pgx delivers as
// text, reaches the caller as a float64.
func TestNewDatabase_NumericTextIsReadAsFloat64(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
	mock.ExpectQuery(`SELECT "price" FROM "album"`).WillReturnRows(
		sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("price").OfType("NUMERIC", "")).
			AddRow("12.50").AddRow("NaN").AddRow(nil))
	q := albumFrom().NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "Price")})
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	rows := readAll(t, reader)
	if len(rows) != 3 || rows[0]["Price"] != 12.5 || rows[2]["Price"] != nil {
		t.Fatalf("rows = %v, want 12.5 as a float64 and NULL as nil", rows)
	}
	if nan, ok := rows[1]["Price"].(float64); !ok || nan == nan {
		t.Errorf("NaN = %#v, want a float64 NaN", rows[1]["Price"])
	}
}

// Behaviour change (dalgo2sql v0.26.0): a text query's arguments are bound
// (dal.QueryArg used to be handed to the driver as a struct and failed every call).
func TestNewDatabase_TextQueryArgumentsAreBound(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	mock.ExpectQuery(`SELECT title FROM album WHERE albumid = $1 AND title = $2`).WithArgs(int64(7), hostile).
		WillReturnRows(sqlmock.NewRows([]string{"title"}).AddRow("Seven"))
	q := dal.NewTextQuery(`SELECT title FROM album WHERE albumid = $1 AND title = $2`, nil,
		dal.QueryArg{Value: int64(7)}, dal.QueryArg{Value: hostile})
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	if rows := readAll(t, reader); len(rows) != 1 {
		t.Errorf("rows = %v, want one", rows)
	}
}

// keyOptions give the recordsets of the key tests their primary key.
func keyOptions(tables ...string) dalgo2sql.DbOptions {
	recordsets := map[string]*dalgo2sql.Recordset{}
	for _, table := range tables {
		recordsets[table] = dalgo2sql.NewRecordset(table, dalgo2sql.Table, []dal.FieldRef{dal.Field("ID")})
	}
	return dalgo2sql.DbOptions{Recordsets: recordsets}
}

// Behaviour change (dalgo2sql v0.26.0): key reads and writes refuse a collection,
// field or primary-key name that is not a plain identifier, with an error that
// matches dalgo2sql.ErrUnsafeName, and send no statement. A PostgreSQL table whose
// name needs quoting cannot be addressed by key. The reads (Get, GetMulti, Exists) are
// checked for a collection and a primary key; a map record's read names no field, so a
// field name never reaches one.
func TestNewDatabase_KeyWritesAndReadsRefuseUnsafeNames(t *testing.T) {
	const evil = `x"; DROP TABLE widgets; --`
	for _, tc := range []struct {
		name       string
		collection string
		pk         string
		data       map[string]any
		position   string
	}{
		{"a collection with a space", "Order Details", "ID", map[string]any{"Name": "x"}, "collection"},
		{"a collection that is quoted", `"widgets"`, "ID", map[string]any{"Name": "x"}, "collection"},
		{"a collection with an injection", evil, "ID", map[string]any{"Name": "x"}, "collection"},
		{"a field with a space", "widgets", "ID", map[string]any{"first name": "x"}, "field"},
		{"a field with an injection", "widgets", "ID", map[string]any{evil: "x"}, "field"},
		{"a primary key with a space", "widgets", "the id", map[string]any{"Name": "x"}, "primary key"},
		{"a non-ASCII field", "widgets", "ID", map[string]any{"Naïve": "x"}, "field"},
	} {
		recordsets := map[string]*dalgo2sql.Recordset{
			tc.collection: dalgo2sql.NewRecordset(tc.collection, dalgo2sql.Table, []dal.FieldRef{dal.Field(tc.pk)}),
		}
		key := dalrecord.NewKeyWithID(tc.collection, "id1")
		newRecord := func() dalrecord.Record { return dalrecord.NewRecordWithData(key, tc.data) }
		check := func(t *testing.T, err error) {
			t.Helper()
			if !errors.Is(err, dalgo2sql.ErrUnsafeName) || !strings.Contains(err.Error(), tc.position) {
				t.Errorf("error = %v, want one matching ErrUnsafeName that names the %s", err, tc.position)
			}
		}
		t.Run(tc.name, func(t *testing.T) {
			sqlDB, _ := newStructuredMock(t) // no expectation: any statement fails the test
			db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{Recordsets: recordsets})
			ctx := context.Background()
			check(t, db.Insert(ctx, newRecord()))
			check(t, db.Set(ctx, newRecord()))
			updated := "Name"
			if tc.position == "field" {
				for field := range tc.data {
					updated = field
				}
			}
			check(t, db.Update(ctx, key, []update.Update{update.ByFieldName(updated, "y")}))
			// The reads and Delete name no field of a map record: they select every column
			// and address the row by its primary key. A field name is not what they refuse.
			_, existsErr := db.Exists(ctx, key)
			getErr := db.Get(ctx, newRecord())
			getMultiErr := db.GetMulti(ctx, []dalrecord.Record{newRecord()})
			for name, err := range map[string]error{"Get": getErr, "GetMulti": getMultiErr, "Exists": existsErr, "Delete": db.Delete(ctx, key)} {
				if tc.position == "field" {
					if errors.Is(err, dalgo2sql.ErrUnsafeName) {
						t.Errorf("%s: error = %v, want no refusal of a field name it never writes", name, err)
					}
					continue
				}
				if !errors.Is(err, dalgo2sql.ErrUnsafeName) || !strings.Contains(err.Error(), tc.position) {
					t.Errorf("%s: error = %v, want one matching ErrUnsafeName that names the %s", name, err, tc.position)
				}
			}
		})
	}
}

// Behaviour change (dalgo2sql v0.26.0): a name a key read or write accepts is
// written as given: unquoted and not folded, so PostgreSQL folds it to lower case
// itself. (The structured compiler quotes; the key paths do not.)
func TestNewDatabase_KeyPathsWritePlainNamesUnquoted(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, keyOptions("Widgets"))
	ctx := context.Background()
	key := dalrecord.NewKeyWithID("Widgets", "id1")

	mock.ExpectExec("INSERT INTO Widgets(ID, Name) VALUES ($1, $2)").WithArgs("id1", "w1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := db.Insert(ctx, dalrecord.NewRecordWithData(key, map[string]any{"Name": "w1"})); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Two fields: the assignments are separated by a comma (they were not before).
	mock.ExpectExec("UPDATE Widgets SET Name = $1, Size = $2 WHERE ID = $3").WithArgs("w2", int64(3), "id1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := db.Update(ctx, key, []update.Update{update.ByFieldName("Name", "w2"), update.ByFieldName("Size", int64(3))}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	mock.ExpectExec("DELETE FROM Widgets WHERE ID = $1").WithArgs("id1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := db.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// Behaviour change (dalgo2sql v0.26.0): a nested key addresses one table, whose
// name joins the key's collection and its parents' with an underscore, in every
// statement (it was the key's own collection before), and its primary key is looked
// up in the recordset of that name.
func TestNewDatabase_NestedKeyUsesOneJoinedTableName(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, keyOptions("pets_owners"))
	ctx := context.Background()
	owner := dalrecord.NewKeyWithID("owners", "o1")
	key := dalrecord.NewKeyWithParentAndID(owner, "pets", "p1")

	mock.ExpectExec("INSERT INTO pets_owners(ID, Name) VALUES ($1, $2)").WithArgs("p1", "Rex").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := db.Insert(ctx, dalrecord.NewRecordWithData(key, map[string]any{"Name": "Rex"})); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	mock.ExpectExec("DELETE FROM pets_owners WHERE ID = $1").WithArgs("p1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := db.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// A structured read holds one connection from its catalog lookup to the end of its
// rows: the reader gives it back when it is closed, when its rows are read to the
// end, when its statement fails, and when its context ends. (The integration test
// pins the same with pgx under it.)
func TestNewDatabase_StructuredReadLeasesOneConnection(t *testing.T) {
	query := func() dal.StructuredQuery { return albumFrom().NewQuery().SelectColumns(titleColumn()) }
	expectRead := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
		mock.ExpectQuery(`SELECT "title" FROM "album"`).WillReturnRows(sqlmock.NewRows([]string{"title"}).AddRow("a").AddRow("b"))
	}
	inUse := func(sqlDB *sql.DB) int { return sqlDB.Stats().InUse }

	t.Run("held while the reader is open, back on Close", func(t *testing.T) {
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		expectRead(mock)
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query())
		if err != nil {
			t.Fatal(err)
		}
		if got := inUse(sqlDB); got != 1 {
			t.Errorf("connections in use while the reader is open = %d, want 1", got)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := inUse(sqlDB); got != 0 {
			t.Errorf("connections in use after Close = %d, want 0", got)
		}
	})
	t.Run("back when the rows are read to the end", func(t *testing.T) {
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		expectRead(mock)
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query())
		if err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := reader.Next(); err != nil {
				if !errors.Is(err, dal.ErrNoMoreRecords) {
					t.Fatalf("Next: %v", err)
				}
				break
			}
		}
		if got := inUse(sqlDB); got != 0 {
			t.Errorf("connections in use at the end of the rows (no Close) = %d, want 0", got)
		}
		_ = reader.Close()
	})
	t.Run("back when the catalog lookup fails", func(t *testing.T) {
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		boom := errors.New("boom")
		mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnError(boom)
		if _, err := db.ExecuteQueryToRecordsReader(context.Background(), query()); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want the lookup's own", err)
		}
		if got := inUse(sqlDB); got != 0 {
			t.Errorf("connections in use after a failed lookup = %d, want 0", got)
		}
	})
	t.Run("back when the statement fails", func(t *testing.T) {
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		boom := errors.New("boom")
		mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
		mock.ExpectQuery(`SELECT "title" FROM "album"`).WillReturnError(boom)
		if _, err := db.ExecuteQueryToRecordsReader(context.Background(), query()); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want the statement's own", err)
		}
		if got := inUse(sqlDB); got != 0 {
			t.Errorf("connections in use after a failed statement = %d, want 0", got)
		}
	})
	t.Run("back when the context ends, with no Close", func(t *testing.T) {
		sqlDB, mock := newStructuredMock(t)
		sqlDB.SetMaxOpenConns(1) // the one connection is the reader's until the lease gives it back
		db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		expectRead(mock)
		ctx, cancel := context.WithCancel(context.Background())
		reader, err := db.ExecuteQueryToRecordsReader(ctx, query())
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		// Asking the pool for a connection waits until the lease has given the reader's
		// back: it returns once the context's end has been handled, or fails after the
		// bound if the lease never lets go.
		waiting, stopWaiting := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopWaiting()
		conn, err := sqlDB.Conn(waiting)
		if err != nil {
			t.Fatalf("the connection was not given back after the context ended: %v", err)
		}
		_ = conn.Close()
		_ = reader.Close()
	})
}

// The recordset reader, the second public read entry point, runs a structured query on
// the same leased connection as the records reader: held while the reader is open, back
// on Close and at the end of the rows, back when the read fails. (The integration test
// pins the same with pgx under it, and the types pgx reports for each column.)
func TestNewDatabase_RecordsetReaderLeasesOneConnection(t *testing.T) {
	query := func() dal.StructuredQuery {
		return albumFrom().NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "AlbumId")}, titleColumn())
	}
	typedRows := func() *sqlmock.Rows {
		return sqlmock.NewRowsWithColumnDefinition(
			sqlmock.NewColumn("albumid").OfType("INT4", int64(0)),
			sqlmock.NewColumn("title").OfType("TEXT", "")).
			AddRow(int64(1), "First").AddRow(int64(2), "Second")
	}
	expectRead := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
		mock.ExpectQuery(`SELECT "albumid", "title" FROM "album"`).WillReturnRows(typedRows())
	}
	open := func(t *testing.T) (*sql.DB, sqlmock.Sqlmock, *Database) {
		t.Helper()
		sqlDB, mock := newStructuredMock(t)
		return sqlDB, mock, openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	}
	nextRow := func(t *testing.T, reader dal.RecordsetReader) []any {
		t.Helper()
		row, rs, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			return nil
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		values, err := row.Data(rs)
		if err != nil {
			t.Fatalf("row.Data: %v", err)
		}
		return values
	}

	t.Run("held while the reader is open, back on Close", func(t *testing.T) {
		sqlDB, mock, db := open(t)
		expectRead(mock)
		reader, err := db.ExecuteQueryToRecordsetReader(context.Background(), query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
		}
		if got := sqlDB.Stats().InUse; got != 1 {
			t.Errorf("connections in use while the reader is open = %d, want 1", got)
		}
		if got := nextRow(t, reader); !reflect.DeepEqual(got, []any{int64(1), "First"}) {
			t.Errorf("first row = %v, want [1 First], named as the query asked", got)
		}
		if names := []string{reader.Recordset().GetColumnByIndex(0).Name(), reader.Recordset().GetColumnByIndex(1).Name()}; !reflect.DeepEqual(names, []string{"AlbumId", "Title"}) {
			t.Errorf("columns = %v, want AlbumId and Title", names)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := sqlDB.Stats().InUse; got != 0 {
			t.Errorf("connections in use after Close = %d, want 0", got)
		}
	})
	t.Run("back when the rows are read to the end, with no Close", func(t *testing.T) {
		sqlDB, mock, db := open(t)
		expectRead(mock)
		reader, err := db.ExecuteQueryToRecordsetReader(context.Background(), query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
		}
		for nextRow(t, reader) != nil {
		}
		if got := sqlDB.Stats().InUse; got != 0 {
			t.Errorf("connections in use at the end of the rows (no Close) = %d, want 0", got)
		}
		_ = reader.Close()
	})
	t.Run("back when the statement fails, and no reader is returned", func(t *testing.T) {
		sqlDB, mock, db := open(t)
		boom := errors.New("boom")
		mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
		mock.ExpectQuery(`SELECT "albumid", "title" FROM "album"`).WillReturnError(boom)
		reader, err := db.ExecuteQueryToRecordsetReader(context.Background(), query())
		if !errors.Is(err, boom) || reader != nil {
			t.Fatalf("ExecuteQueryToRecordsetReader = %v, %v; want no reader and the statement's own error", reader, err)
		}
		if got := sqlDB.Stats().InUse; got != 0 {
			t.Errorf("connections in use after a failed statement = %d, want 0", got)
		}
	})
}

// What the database declares it runs on the server: grouping, HAVING, ORDER BY and the
// aggregates with their DISTINCT forms, and no more. FIRST, LAST, a group-key order and
// a stable row order are not promised by PostgreSQL, so they stay false, and DALgo
// plans a grouped query natively and refuses FIRST and LAST.
//
// The answer is asked of the *Database this package hands out, which is what a consumer
// holds (dal.As on a *Database does not reach the dal.DB it embeds), and of the dal.DB
// under it, which is where DALgo plans a query; the two must agree.
func TestNewDatabase_DeclaresWhatItRunsOnTheServer(t *testing.T) {
	sqlDB, _ := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	want := dal.QueryCapabilities{
		GroupBy: true, Having: true, OrderBy: true,
		Aggregate: dal.AggregateCapabilities{
			Count: true, CountDistinct: true, Sum: true, SumDistinct: true, Avg: true, AvgDistinct: true, Min: true, Max: true,
		},
	}
	for name, holder := range map[string]dal.DB{"the Database": db, "the dal.DB under it": db.DB} {
		t.Run(name, func(t *testing.T) {
			provider, ok := dal.As[dal.QueryCapabilitiesProvider](holder)
			if !ok {
				t.Fatal("declares no query capabilities")
			}
			got := provider.QueryCapabilities()
			if got != want {
				t.Errorf("capabilities = %+v, want %+v", got, want)
			}
			grouped := albumFrom().NewQuery().GroupBy(field("Title")).SelectColumns(titleColumn(), dal.SumAs(field("Price"), "total"))
			if plan, err := dal.PlanAggregation(grouped, got); err != nil || plan.Strategy != dal.AggregationNative {
				t.Errorf("plan = %+v, err = %v; want a native aggregation", plan, err)
			}
			if _, ok := dal.As[dal.NativeJoinProvider](holder); !ok {
				t.Error("declares no native join provider")
			}
			if _, ok := dal.As[dal.JoinFieldsProvider](holder); !ok {
				t.Error("declares no join fields provider")
			}
		})
	}
}

// The Database answers the join questions with the database's own answers, not just
// with a method of the right name: a join of one type category is accepted, one across
// categories is declined, and a wildcard's columns come back in table order.
func TestNewDatabase_ForwardsTheJoinAnswers(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	ctx := context.Background()
	joiner, ok := dal.As[dal.NativeJoinProvider](db)
	if !ok {
		t.Fatal("the Database declares no native join provider")
	}
	joinOn := func(left, right string) dal.StructuredQuery {
		on := dal.NewComparison(dal.NewFieldRef("a", left), dal.Equal, dal.NewFieldRef("r", right))
		return dal.From(dal.NewRootCollectionRef("Album", "a")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef("Artist", "r"), dal.JoinInner, on)).
			NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("r", "Name"), Alias: "artist"})
	}
	joinCatalog := func() *sqlmock.Rows {
		rows := catalogRows(`"album"`, intColumn("albumid", true), textColumn("title"), intColumn("artistid", true))
		for _, c := range []catalogColumn{intColumn("artistid", true), textColumn("name")} {
			rows.AddRow(`"artist"`, c.name, c.dataType, c.category, c.oid, int64(0), c.notNull, false, false)
		}
		return rows
	}
	mock.ExpectQuery(catalogStatement).WillReturnRows(joinCatalog())
	if err := joiner.CanExecuteJoin(ctx, joinOn("ArtistId", "ArtistId")); err != nil {
		t.Errorf("a join of integer to integer: error = %v, want it accepted", err)
	}
	mock.ExpectQuery(catalogStatement).WillReturnRows(joinCatalog())
	if err := joiner.CanExecuteJoin(ctx, joinOn("Title", "ArtistId")); err == nil {
		t.Error("a join of text to integer was accepted, want it declined")
	}

	fields, ok := dal.As[dal.JoinFieldsProvider](db)
	if !ok {
		t.Fatal("the Database declares no join fields provider")
	}
	mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
	source := dal.NewRootCollectionRef("Album", "a")
	got, err := fields.JoinFields(ctx, source)
	if want := []string{"albumid", "title", "price"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("JoinFields = %v, %v; want %v", got, err, want)
	}
}

// A Database that was never opened (the zero value, whose embedded dal.DB is nil)
// declares nothing and says so without panicking: no capabilities, a join declined with
// dal.ErrNotSupported, and no fields for a wildcard.
func TestZeroValueDatabaseDeclaresNothing(t *testing.T) {
	var db Database
	capabilities, ok := dal.As[dal.QueryCapabilitiesProvider](&db)
	if !ok {
		t.Fatal("a *Database does not implement dal.QueryCapabilitiesProvider")
	}
	if got := capabilities.QueryCapabilities(); got != (dal.QueryCapabilities{}) {
		t.Errorf("capabilities = %+v, want none", got)
	}
	joiner, ok := dal.As[dal.NativeJoinProvider](&db)
	if !ok {
		t.Fatal("a *Database does not implement dal.NativeJoinProvider")
	}
	ctx := context.Background()
	if err := joiner.CanExecuteJoin(ctx, albumFrom().NewQuery().SelectColumns()); !errors.Is(err, dal.ErrNotSupported) {
		t.Errorf("CanExecuteJoin = %v, want dal.ErrNotSupported", err)
	}
	fields, ok := dal.As[dal.JoinFieldsProvider](&db)
	if !ok {
		t.Fatal("a *Database does not implement dal.JoinFieldsProvider")
	}
	if got, err := fields.JoinFields(ctx, dal.NewRootCollectionRef("Album", "")); got != nil || err != nil {
		t.Errorf("JoinFields = %v, %v; want nothing, so that DALgo rejects a wildcard of its own accord", got, err)
	}
}

// Behaviour change (dalgo2sql v0.26.0): a keys-only query that names no order is
// ordered ascending by the primary key (it had no defined order before).
func TestNewDatabase_KeysOnlyQueryIsOrderedByThePrimaryKey(t *testing.T) {
	sqlDB, mock := newStructuredMock(t)
	db := openMockedDatabase(t, sqlDB, keyOptions("widgets"))
	mock.ExpectQuery(catalogStatement).WithArgs(`"widgets"`).WillReturnRows(catalogRows(`"widgets"`, textColumn("id"), textColumn("name")))
	mock.ExpectQuery(`SELECT * FROM "widgets" ORDER BY "id" ASC NULLS FIRST`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("a").AddRow("b"))
	q := dal.From(dal.NewRootCollectionRef("widgets", "")).NewQuery().SelectKeysOnly(reflect.String)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var ids []any
	for {
		rec, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		ids = append(ids, rec.Key().ID)
	}
	if want := []any{"a", "b"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("keys = %v, want %v", ids, want)
	}
}

type widgetRow struct {
	ID        string
	FirstName string
}

// Behaviour change (dalgo2sql v0.26.0): a record read by a structured query carries the
// collection of the query's base source and, when the recordset has a one-field primary
// key, that key's value as its ID. When the query selects columns and does not select the
// key, the statement gets a hidden column for it (written like any column, named
// __dalgo_record_id), which is not in the record's data. Without a configured key the ID
// is that column's name, a placeholder. Before, every record had the collection Unknown
// and an empty ID. A query that reads into a record of its own (SelectIntoRecord) fills
// that record, which is keyed the same way.
func TestNewDatabase_RecordsCarryTheirKey(t *testing.T) {
	widgets := func() dal.IQueryBuilder { return dal.From(dal.NewRootCollectionRef("widgets", "")).NewQuery() }
	nameColumn := dal.Column{Expression: dal.NewFieldRef("", "Name")}
	run := func(t *testing.T, opts dalgo2sql.DbOptions, catalog []catalogColumn, statement string, answer *sqlmock.Rows, q dal.StructuredQuery) []dalrecord.Record {
		t.Helper()
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, opts)
		mock.ExpectQuery(catalogStatement).WithArgs(`"widgets"`).WillReturnRows(catalogRows(`"widgets"`, catalog...))
		mock.ExpectQuery(statement).WillReturnRows(answer)
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		var out []dalrecord.Record
		for {
			rec, err := reader.Next()
			if errors.Is(err, dal.ErrNoMoreRecords) {
				return out
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			out = append(out, rec)
		}
	}
	idAndName := []catalogColumn{textColumn("id"), textColumn("name")}

	t.Run("a key the query does not select comes through the hidden column", func(t *testing.T) {
		got := run(t, keyOptions("widgets"), idAndName, `SELECT "name", "id" AS "__dalgo_record_id" FROM "widgets"`,
			sqlmock.NewRows([]string{"name", "__dalgo_record_id"}).AddRow("One", "w1").AddRow("Two", "w2"),
			widgets().SelectColumns(nameColumn))
		if len(got) != 2 {
			t.Fatalf("records = %d, want 2", len(got))
		}
		for i, want := range []struct{ id, name string }{{"w1", "One"}, {"w2", "Two"}} {
			key := got[i].Key()
			if key.Collection() != "widgets" || key.ID != want.id {
				t.Errorf("record %d: key = %v, want widgets/%s", i, key, want.id)
			}
			if data := got[i].Data(); !reflect.DeepEqual(data, map[string]any{"Name": want.name}) {
				t.Errorf("record %d: data = %v, want only Name (the hidden column is not data)", i, data)
			}
		}
	})
	t.Run("a key the query selects is read from its own column", func(t *testing.T) {
		got := run(t, keyOptions("widgets"), idAndName, `SELECT "id", "name" FROM "widgets"`,
			sqlmock.NewRows([]string{"id", "name"}).AddRow("w1", "One"),
			widgets().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "ID")}, nameColumn))
		if len(got) != 1 || got[0].Key().Collection() != "widgets" || got[0].Key().ID != "w1" {
			t.Fatalf("records = %v, want one keyed widgets/w1", got)
		}
		if data := got[0].Data(); !reflect.DeepEqual(data, map[string]any{"ID": "w1", "Name": "One"}) {
			t.Errorf("data = %v, want both selected columns", data)
		}
	})
	t.Run("no primary key configured, none in the catalog: the rows are keyed by their ordinal", func(t *testing.T) {
		// Up to dalgo2sql v0.26.0 such a record carried the name of the reader's helper column
		// as its ID, the same for every record, an identity of no row. With v0.26.3 the
		// catalog is asked for the key; this source has none (the mocked rows say so), so
		// the records are keyed by ordinal, "0", "1", ... The server tests of the catalog's
		// key are TestStructuredQueryIntegration_RecordKeysFromTheCatalog.
		got := run(t, dalgo2sql.DbOptions{}, idAndName, `SELECT "name" FROM "widgets"`,
			sqlmock.NewRows([]string{"name"}).AddRow("One").AddRow("Two"),
			widgets().SelectColumns(nameColumn))
		if len(got) != 2 {
			t.Fatalf("records = %d, want 2", len(got))
		}
		for i, record := range got {
			if key := record.Key(); key.Collection() != "widgets" || key.ID != strconv.Itoa(i) {
				t.Errorf("record %d: key = %v, want widgets/%d", i, key, i)
			}
		}
		if data := got[0].Data(); !reflect.DeepEqual(data, map[string]any{"Name": "One"}) {
			t.Errorf("data = %v, want only Name", data)
		}
	})
	t.Run("the catalog's primary key keys the records when nothing else names one", func(t *testing.T) {
		pk := textColumn("id")
		catalog := sqlmock.NewRows(catalogColumns).
			AddRow(`"widgets"`, "id", pk.dataType, pk.category, pk.oid, int64(0), false, false, true).
			AddRow(`"widgets"`, "name", "text", "S", int64(25), int64(0), false, false, false)
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		mock.ExpectQuery(catalogStatement).WithArgs(`"widgets"`).WillReturnRows(catalog)
		mock.ExpectQuery(`SELECT "name", "id" AS "__dalgo_record_id" FROM "widgets"`).
			WillReturnRows(sqlmock.NewRows([]string{"name", "__dalgo_record_id"}).AddRow("One", "w1"))
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(), widgets().SelectColumns(nameColumn))
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		record, err := reader.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if key := record.Key(); key.Collection() != "widgets" || key.ID != "w1" {
			t.Errorf("key = %v, want widgets/w1, the catalog's primary key", key)
		}
	})
	// dalgo2sql finds the recordset of the source in the mode that folds names under the
	// name the query spells and under that name folded to lower case, as the catalog lookup
	// folds it (v0.26.3; with v0.26.0 a query spelling Widgets did not find the recordset
	// registered as widgets, and every record carried the placeholder, silently).
	t.Run("the recordset is found by the source name as the query spells it, and folded", func(t *testing.T) {
		spelledWithACapital := dal.From(dal.NewRootCollectionRef("Widgets", "")).NewQuery().SelectColumns(nameColumn)
		got := run(t, keyOptions("widgets"), idAndName, `SELECT "name", "id" AS "__dalgo_record_id" FROM "widgets"`,
			sqlmock.NewRows([]string{"name", "__dalgo_record_id"}).AddRow("One", "w1").AddRow("Two", "w2"), spelledWithACapital)
		if len(got) != 2 {
			t.Fatalf("records = %d, want 2", len(got))
		}
		for i, record := range got {
			if key := record.Key(); key.Collection() != "Widgets" || key.ID != fmt.Sprintf("w%d", i+1) {
				t.Errorf("record %d: key = %v, want Widgets/w%d: the recordset registered as widgets is the one a query spelled Widgets finds", i, key, i+1)
			}
		}
	})
	// A grouped row is no row of the table, so it has no key to read: its ID is its
	// ordinal in the result, "0", "1", ..., with a primary key configured or not, and the
	// statement adds no hidden column for it.
	t.Run("the rows of a grouped query are keyed by their ordinal", func(t *testing.T) {
		grouped := widgets().GroupBy(dal.NewFieldRef("", "Name")).SelectColumns(nameColumn,
			dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"})
		for _, tc := range []struct {
			name string
			opts dalgo2sql.DbOptions
		}{
			{"with a primary key configured", keyOptions("widgets")},
			{"with none", dalgo2sql.DbOptions{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := run(t, tc.opts, idAndName, `SELECT "name", COUNT(*) AS "n" FROM "widgets" GROUP BY "name"`,
					sqlmock.NewRows([]string{"name", "n"}).AddRow("One", int64(2)).AddRow("Two", int64(1)).AddRow("Three", int64(5)),
					grouped)
				if len(got) != 3 {
					t.Fatalf("records = %d, want 3", len(got))
				}
				for i, record := range got {
					if key := record.Key(); key.Collection() != "widgets" || key.ID != strconv.Itoa(i) {
						t.Errorf("record %d: key = %v, want widgets/%d", i, key, i)
					}
				}
				if data := got[1].Data(); !reflect.DeepEqual(data, map[string]any{"Name": "Two", "n": int64(1)}) {
					t.Errorf("data = %v, want the group's columns only", data)
				}
			})
		}
	})
	t.Run("a record of the query's own is filled and keyed", func(t *testing.T) {
		into := func() dalrecord.Record {
			return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("widgets", ""), &widgetRow{})
		}
		got := run(t, keyOptions("widgets"), []catalogColumn{textColumn("id"), textColumn("first_name")}, `SELECT * FROM "widgets"`,
			sqlmock.NewRows([]string{"id", "first_name"}).AddRow("w9", "Ann"),
			widgets().SelectIntoRecord(into))
		if len(got) != 1 {
			t.Fatalf("records = %d, want 1", len(got))
		}
		if key := got[0].Key(); key.Collection() != "widgets" || key.ID != "w9" {
			t.Errorf("key = %v, want widgets/w9", key)
		}
		if data, ok := got[0].Data().(*widgetRow); !ok || *data != (widgetRow{ID: "w9", FirstName: "Ann"}) {
			t.Errorf("data = %#v, want the struct the query supplied, filled", got[0].Data())
		}
	})
}

// Behaviour change (dalgo2sql v0.26.0): Get into a struct matches a column to a field
// without regard to case and underscores (first_name reaches FirstName), and a column
// that no field matches is an error that names it (scany used to decide).
func TestNewDatabase_GetIntoAStructMatchesColumnsLeniently(t *testing.T) {
	get := func(t *testing.T, columns ...string) (*widgetRow, error) {
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, keyOptions("widgets"))
		row := sqlmock.NewRows(columns)
		values := make([]driver.Value, len(columns))
		for i := range values {
			values[i] = "x" + columns[i]
		}
		mock.ExpectQuery("SELECT ID, FirstName FROM widgets WHERE ID = $1").WithArgs("w1").WillReturnRows(row.AddRow(values...))
		data := &widgetRow{}
		record := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("widgets", "w1"), data)
		return data, db.Get(context.Background(), record)
	}
	t.Run("a column in another case and with an underscore", func(t *testing.T) {
		data, err := get(t, "first_name")
		if err != nil || data.FirstName != "xfirst_name" {
			t.Errorf("data = %+v, err = %v; want first_name to reach FirstName", data, err)
		}
	})
	t.Run("a column no field matches is named", func(t *testing.T) {
		_, err := get(t, "nickname")
		if err == nil || !strings.Contains(err.Error(), `column "nickname": no corresponding field in dalgo2postgres.widgetRow`) {
			t.Errorf("err = %v, want the column and the struct named", err)
		}
	})
}

// Behaviour change (dalgo2sql v0.26.0): an Insert whose key has an ID and whose
// recordset has no primary key is an error, not a panic.
func TestNewDatabase_InsertWithoutAPrimaryKeyIsAnErrorNotAPanic(t *testing.T) {
	sqlDB, _ := newStructuredMock(t) // no expectation: nothing may be sent
	db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
	record := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("widgets", "w1"), map[string]any{"Name": "x"})
	err := db.Insert(context.Background(), record)
	if err == nil || !strings.Contains(err.Error(), "primary key is not defined for recordset widgets") {
		t.Errorf("error = %v, want one that says no primary key is defined for widgets", err)
	}
}

// A batch with one record whose name is refused sends nothing at all: the whole batch
// is checked before its first statement (the good record would otherwise be written).
func TestNewDatabase_BatchesWithAnUnsafeNameSendNothing(t *testing.T) {
	sqlDB, mock := newStructuredMock(t) // only the begin and the rollback of a transaction are expected
	db := openMockedDatabase(t, sqlDB, keyOptions("widgets"))
	ctx := context.Background()
	good := func() dalrecord.Record {
		return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("widgets", "g1"), map[string]any{"Name": "x"})
	}
	bad := func() dalrecord.Record {
		return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("widgets", "b1"), map[string]any{"first name": "x"})
	}
	refused := func(err error) bool { return errors.Is(err, dalgo2sql.ErrUnsafeName) }
	// SetMulti on the database handle sends nothing: with dalgo2sql v0.26.3 and dalgo
	// v0.89.6 it refuses the unsafe name before it opens the transaction it opened (and
	// rolled back) with v0.26.0 and v0.89.4.
	if err := db.SetMulti(ctx, []dalrecord.Record{good(), bad()}); !refused(err) {
		t.Errorf("SetMulti: error = %v", err)
	}
	updates := []update.Update{update.ByFieldName("first name", "y")}
	keys := []*dalrecord.Key{dalrecord.NewKeyWithID("widgets", "g1"), dalrecord.NewKeyWithID("widgets", "g2")}
	if err := db.UpdateMulti(ctx, keys, updates); !refused(err) {
		t.Errorf("UpdateMulti: error = %v", err)
	}
	if err := db.DeleteMulti(ctx, []*dalrecord.Key{dalrecord.NewKeyWithID("widgets", "g1"), dalrecord.NewKeyWithID(`"widgets"`, "g2")}); !refused(err) {
		t.Errorf("DeleteMulti: error = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectRollback()
	err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.InsertMulti(ctx, []dalrecord.Record{good(), bad()})
	})
	if !refused(err) {
		t.Errorf("InsertMulti in a transaction: error = %v", err)
	}
}
