package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// With dalgo2sql v0.26.3 the records of a read of a source nobody declared (no
// DbOptions.Recordsets entry, no DbOptions.PrimaryKey) are keyed by the primary key the
// catalog reports for the source when it is exactly one column, and by their ordinal in
// the result otherwise. The key is a column of the same catalog statement the compiler
// runs anyway (a correlated EXISTS over pg_catalog.pg_constraint), which no test had run
// on a server. The tests below run it: the output of each case holds the catalog statement
// and the rows the server answered for the case's source, and each case asserts the key
// every record carries and, where it is told, the statement the read sent.
//
// They run against PostgreSQL and skip unless DALGO2POSTGRES_TEST_DSN is set; CI fails
// when TestStructuredQueryIntegration_RecordKeysFromTheCatalog skips or is missing.

// keyedRow is one record of a read: the collection and the ID of its key and its data.
type keyedRow struct {
	collection string
	id         any
	data       map[string]any
}

// keysOf reads q through the records reader to its end and returns the records with their
// keys, and the statement the driver was handed for it (the catalog lookup left out).
func keysOf(t *testing.T, f *queryFixture, q dal.Query) (statement string, rows []keyedRow) {
	t.Helper()
	f.trace.reset()
	reader, err := f.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v\nstatements sent: %v", err, f.trace.sent())
	}
	defer func() { _ = reader.Close() }()
	for {
		rec, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		data, _ := rec.Data().(map[string]any)
		rows = append(rows, keyedRow{collection: rec.Key().Collection(), id: rec.Key().ID, data: data})
	}
	for _, sent := range f.trace.sent() {
		if !isCatalogStatement(sent.sql) {
			statement = sent.sql
		}
	}
	return statement, rows
}

// catalogAnswer runs the catalog statement the last read sent again, with the arguments it
// was sent, on the untraced handle, writes the statement and the rows the server answers
// to the output, and returns the names of the columns the catalog marks as the primary key
// (the last column of its answer).
func catalogAnswer(t *testing.T, f *queryFixture) (primaryKey []string) {
	t.Helper()
	var statement string
	var args []any
	for _, sent := range f.trace.sent() {
		if isCatalogStatement(sent.sql) {
			statement, args = sent.sql, valueArguments(sent.args)
		}
	}
	if statement == "" {
		t.Fatal("no catalog statement was sent")
	}
	rows, err := f.admin.QueryContext(context.Background(), statement, args...)
	if err != nil {
		t.Fatalf("the catalog statement, run again: %v\nstatement: %s", err, statement)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	var lines []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("%v", values))
		if len(values) == 9 && values[8] == true {
			primaryKey = append(primaryKey, fmt.Sprint(values[1]))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	t.Logf("catalog statement: %s\n    arguments: %s\n    columns: %v\n    rows (name, attname, data_type, category, type_oid, type_elem, attnotnull, nondeterministic, pk):\n        %s\n    primary key columns: %v",
		statement, argsText(args), columns, strings.Join(lines, "\n        "), primaryKey)
	return primaryKey
}

// A source nobody declared is keyed by the primary key the catalog reports, when it is
// one column, and by ordinal when it is none, several, a view or a grouped query. A
// recordset the caller declared keys its source under any case of the name the query
// spells it in, in fold-lower mode, and wins over the catalog.
func TestStructuredQueryIntegration_RecordKeysFromTheCatalog(t *testing.T) {
	ddl := []string{
		// Rows are inserted out of the order of their keys, so an order by the key is seen.
		`CREATE TABLE ck_single (id integer PRIMARY KEY, payload text, other text)`,
		`INSERT INTO ck_single VALUES (30, 'c', 'z'), (10, 'a', 'x'), (20, 'b', 'y')`,
		`CREATE TABLE ck_include (id integer, payload text, other text, PRIMARY KEY (id) INCLUDE (payload))`,
		`INSERT INTO ck_include VALUES (3, 'c', 'z'), (1, 'a', 'x'), (2, 'b', 'y')`,
		`CREATE TABLE ck_composite (a integer, b integer, payload text, PRIMARY KEY (a, b))`,
		`INSERT INTO ck_composite VALUES (1, 2, 'a'), (1, 1, 'b'), (2, 1, 'c')`,
		`CREATE TABLE ck_none (id integer, payload text)`,
		`INSERT INTO ck_none VALUES (3, 'c'), (1, 'a'), (2, 'b')`,
		`CREATE VIEW ck_view AS SELECT id, payload FROM ck_single`,
		`CREATE MATERIALIZED VIEW ck_matview AS SELECT id, payload FROM ck_single`,
		`CREATE TABLE ck_decl (id integer PRIMARY KEY, code text, payload text)`,
		`INSERT INTO ck_decl VALUES (1, 'k-a', 'a'), (2, 'k-b', 'b'), (3, 'k-c', 'c')`,
		`CREATE TABLE ck_mixed (id integer PRIMARY KEY, code text, payload text)`,
		`INSERT INTO ck_mixed VALUES (1, 'm-a', 'a'), (2, 'm-b', 'b'), (3, 'm-c', 'c')`,
	}
	options := dalgo2sql.DbOptions{Recordsets: map[string]*dalgo2sql.Recordset{
		// Registered under a lower case name, as the fold-lower mount stores it.
		"ck_decl": dalgo2sql.NewRecordset("ck_decl", dalgo2sql.Table, []dal.FieldRef{dal.Field("code")}),
		// Registered under a name with capitals: the other direction of the lookup.
		"Ck_Mixed": dalgo2sql.NewRecordset("Ck_Mixed", dalgo2sql.Table, []dal.FieldRef{dal.Field("code")}),
	}}
	f := openQueryFixtureWithOptions(t, options, "test_record_keys", IdentifierFoldLower, ddl)

	payload := dal.Column{Expression: field("payload")}
	from := func(source string) dal.FromSource { return dal.From(dal.NewRootCollectionRef(source, "")) }
	ordered := func(source string) dal.IQueryBuilder {
		return from(source).NewQuery().OrderBy(dal.AscendingField("payload"))
	}
	// assertKeys checks the collection and the IDs of the records, in order.
	assertKeys := func(t *testing.T, got []keyedRow, collection string, ids ...any) {
		t.Helper()
		var gotIDs []any
		for _, row := range got {
			if row.collection != collection {
				t.Errorf("a record's collection = %q, want %q", row.collection, collection)
			}
			gotIDs = append(gotIDs, row.id)
		}
		t.Logf("record keys: %s/%v (Go types %v)", collection, gotIDs, typesOf(gotIDs))
		if !reflect.DeepEqual(gotIDs, ids) {
			t.Errorf("record IDs = %v (%v), want %v", gotIDs, typesOf(gotIDs), ids)
		}
	}
	// assertPrimaryKey checks what the catalog reported for the case's source.
	assertPrimaryKey := func(t *testing.T, want ...string) {
		t.Helper()
		got := catalogAnswer(t, f)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the catalog's primary key columns = %v, want %v", got, want)
		}
	}
	// assertData checks that the records carry the selected columns only.
	assertData := func(t *testing.T, got []keyedRow, want ...map[string]any) {
		t.Helper()
		var gotData []map[string]any
		for _, row := range got {
			gotData = append(gotData, row.data)
		}
		if !reflect.DeepEqual(gotData, want) {
			t.Errorf("data = %v, want %v (the hidden key column is not data)", gotData, want)
		}
	}

	t.Run("a single-column primary key keys the records, though the select list leaves it out", func(t *testing.T) {
		statement, got := keysOf(t, f, ordered("ck_single").SelectColumns(payload))
		t.Logf("statement: %s", statement)
		if want := `SELECT "payload", "id" AS "__dalgo_record_id" FROM "ck_single" ORDER BY "payload" ASC NULLS FIRST`; statement != want {
			t.Errorf("statement = %s, want %s", statement, want)
		}
		assertKeys(t, got, "ck_single", int64(10), int64(20), int64(30))
		assertData(t, got, map[string]any{"payload": "a"}, map[string]any{"payload": "b"}, map[string]any{"payload": "c"})
		assertPrimaryKey(t, "id")
	})
	t.Run("a single-column primary key the select list names is read from its own column", func(t *testing.T) {
		statement, got := keysOf(t, f, ordered("ck_single").SelectColumns(dal.Column{Expression: field("id")}, payload))
		t.Logf("statement: %s", statement)
		if want := `SELECT "id", "payload" FROM "ck_single" ORDER BY "payload" ASC NULLS FIRST`; statement != want {
			t.Errorf("statement = %s, want %s", statement, want)
		}
		assertKeys(t, got, "ck_single", int64(10), int64(20), int64(30))
		assertPrimaryKey(t, "id")
	})
	t.Run("a keys-only read is ordered by the primary key", func(t *testing.T) {
		statement, got := keysOf(t, f, from("ck_single").NewQuery().SelectKeysOnly(reflect.Int))
		t.Logf("statement: %s", statement)
		if !strings.Contains(statement, `ORDER BY "id" ASC`) {
			t.Errorf("statement = %s, want an ORDER BY on the primary key", statement)
		}
		// The rows were inserted as 30, 10, 20: the order is the statement's.
		assertKeys(t, got, "ck_single", int64(10), int64(20), int64(30))
		assertPrimaryKey(t, "id")
	})
	t.Run("PRIMARY KEY (id) INCLUDE (payload) is keyed by id, not by ordinal", func(t *testing.T) {
		statement, got := keysOf(t, f, ordered("ck_include").SelectColumns(dal.Column{Expression: field("other")}))
		t.Logf("statement: %s", statement)
		// The ORDER BY names payload, a column of the INCLUDE list that is not in the key.
		assertKeys(t, got, "ck_include", int64(1), int64(2), int64(3))
		assertPrimaryKey(t, "id")
	})
	t.Run("a composite primary key keys the records by ordinal", func(t *testing.T) {
		statement, got := keysOf(t, f, ordered("ck_composite").SelectColumns(payload))
		t.Logf("statement: %s", statement)
		if strings.Contains(statement, "__dalgo_record_id") {
			t.Errorf("statement = %s, want no hidden key column: a composite key is no one value", statement)
		}
		assertKeys(t, got, "ck_composite", "0", "1", "2")
		assertPrimaryKey(t, "a", "b")
	})
	t.Run("a table with no primary key keys the records by ordinal", func(t *testing.T) {
		statement, got := keysOf(t, f, ordered("ck_none").SelectColumns(payload))
		t.Logf("statement: %s", statement)
		if strings.Contains(statement, "__dalgo_record_id") {
			t.Errorf("statement = %s, want no hidden key column", statement)
		}
		assertKeys(t, got, "ck_none", "0", "1", "2")
		assertPrimaryKey(t)
	})
	t.Run("a view keys the records by ordinal, though its table has a primary key", func(t *testing.T) {
		statement, got := keysOf(t, f, ordered("ck_view").SelectColumns(payload))
		t.Logf("statement: %s", statement)
		if strings.Contains(statement, "__dalgo_record_id") {
			t.Errorf("statement = %s, want no hidden key column", statement)
		}
		assertKeys(t, got, "ck_view", "0", "1", "2")
		assertPrimaryKey(t)
	})
	t.Run("a materialized view keys the records by ordinal", func(t *testing.T) {
		_, got := keysOf(t, f, ordered("ck_matview").SelectColumns(payload))
		assertKeys(t, got, "ck_matview", "0", "1", "2")
		assertPrimaryKey(t)
	})
	t.Run("a grouped query keys its rows by ordinal, over a table with a primary key", func(t *testing.T) {
		q := from("ck_single").NewQuery().GroupBy(field("payload")).
			OrderBy(dal.AscendingField("payload")).
			SelectColumns(payload, dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"})
		statement, got := keysOf(t, f, q)
		t.Logf("statement: %s", statement)
		if strings.Contains(statement, "__dalgo_record_id") {
			t.Errorf("statement = %s, want no hidden key column: a group is no row of the table", statement)
		}
		assertKeys(t, got, "ck_single", "0", "1", "2")
		assertData(t, got,
			map[string]any{"payload": "a", "n": int64(1)},
			map[string]any{"payload": "b", "n": int64(1)},
			map[string]any{"payload": "c", "n": int64(1)})
	})
	// The recordset declared as ck_decl (its key is the column code, not the catalog's id)
	// is found by a query that spells the source in any case: the fold-lower mount writes
	// every spelling as the one table, and dalgo2sql looks the recordset up under the
	// spelling and under it folded. The declared key wins over the catalog's.
	for _, spelling := range []string{"ck_decl", "CK_DECL", "Ck_Decl"} {
		t.Run("a declared recordset is found when the query spells the source "+spelling, func(t *testing.T) {
			statement, got := keysOf(t, f, ordered(spelling).SelectColumns(payload))
			t.Logf("statement: %s", statement)
			if want := `SELECT "payload", "code" AS "__dalgo_record_id" FROM "ck_decl" ORDER BY "payload" ASC NULLS FIRST`; statement != want {
				t.Errorf("statement = %s, want %s", statement, want)
			}
			assertKeys(t, got, spelling, "k-a", "k-b", "k-c")
			assertPrimaryKey(t, "id") // the catalog says id; the recordset's code is the key
		})
	}
	// The other direction: a recordset registered under a name with capitals (Ck_Mixed) is
	// found by the query that spells it exactly so, and NOT by one that spells it in lower
	// case or in upper case: dalgo2sql looks the name up as spelled and then folded to
	// lower case, and never folds the registered names. The recordset is then ignored,
	// without an error, and the records are keyed by the catalog's id and not by the code
	// the recordset declares.
	//
	// KNOWN LIMIT of dal-go/dalgo2sql (reported for it; not a rule): the two cases that
	// are not found FAIL when dalgo2sql folds the registered names too, and say what to
	// assert instead. Until then a caller in fold-lower mode registers its recordsets under
	// lower case names (README, "How records are keyed").
	t.Run("a recordset registered as Ck_Mixed is found when the query spells the source Ck_Mixed", func(t *testing.T) {
		statement, got := keysOf(t, f, ordered("Ck_Mixed").SelectColumns(payload))
		t.Logf("statement: %s", statement)
		if want := `SELECT "payload", "code" AS "__dalgo_record_id" FROM "ck_mixed" ORDER BY "payload" ASC NULLS FIRST`; statement != want {
			t.Errorf("statement = %s, want %s", statement, want)
		}
		assertKeys(t, got, "Ck_Mixed", "m-a", "m-b", "m-c")
	})
	for _, spelling := range []string{"ck_mixed", "CK_MIXED"} {
		t.Run("KNOWN LIMIT: a recordset registered as Ck_Mixed is not found when the query spells the source "+spelling, func(t *testing.T) {
			statement, got := keysOf(t, f, ordered(spelling).SelectColumns(payload))
			t.Logf("statement: %s", statement)
			t.Logf("KNOWN LIMIT of dal-go/dalgo2sql (not a rule): the registered name is not folded, so the recordset is ignored without an error and the records are keyed by the catalog's id, not by the declared code")
			const fixed = "dalgo2sql now finds the recordset registered under a name with capitals: assert the declared key (statement with \"code\" AS \"__dalgo_record_id\", IDs m-a, m-b, m-c), and delete this pin and the README's known limit"
			if strings.Contains(statement, `"code"`) {
				t.Errorf("statement = %s: %s", statement, fixed)
			}
			if want := `SELECT "payload", "id" AS "__dalgo_record_id" FROM "ck_mixed" ORDER BY "payload" ASC NULLS FIRST`; statement != want {
				t.Errorf("statement = %s, want %s", statement, want)
			}
			assertKeys(t, got, spelling, int64(1), int64(2), int64(3))
		})
	}
}

// typesOf names the Go types of values.
func typesOf(values []any) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%T", v)
	}
	return out
}
