package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// The tests in this file pin, one test each, what the server does with a statement the
// typed compiler of dalgo2sql writes, where a review of the dialect asked what the
// server would answer. Each states the answer and asserts it, with the statement in the
// output of the run. Where the server refuses a statement the compiler accepts, the
// refusal is pinned as it is and is a finding for dalgo2sql, not something this package
// works around.
//
// They run against PostgreSQL and skip unless DALGO2POSTGRES_TEST_DSN is set; CI fails
// when any TestServerPinsIntegration_ test skips or is missing.

// statementsSent are the statements the driver was handed apart from the catalog lookup.
func statementsSent(f *queryFixture) []string {
	var out []string
	for _, sent := range f.trace.sent() {
		if !isCatalogStatement(sent.sql) {
			out = append(out, sent.sql)
		}
	}
	return out
}

// The compiler writes a column whose name is its source's alias as SELECT "x" FROM "t"
// AS "x". PostgreSQL reads a name that is both a column and a table alias as the
// column, so the answer is the column's values and not the whole row. A name that is an
// alias and no column is the whole row to the server (the control below), and the
// compiler refuses it before the server: the catalog says what the columns are.
func TestServerPinsIntegration_ColumnNamedLikeItsAliasIsTheColumn(t *testing.T) {
	f := openQueryFixture(t, "test_pin_alias", IdentifierFoldLower, []string{
		`CREATE TABLE t_alias (x integer, y text)`,
		`INSERT INTO t_alias VALUES (1, 'one'), (2, 'two')`,
	})
	t.Run("the column x of the source aliased x", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("t_alias", "x")).NewQuery().
			OrderBy(dal.AscendingField("x")).
			SelectColumns(dal.Column{Expression: field("x")})
		o := observe(t, f, q)
		t.Logf("statement: %s\n    result: %v", o.statement, o.rows)
		if want := `SELECT "x" FROM "t_alias" AS "x" ORDER BY "x" ASC NULLS FIRST`; o.statement != want {
			t.Errorf("statement = %s, want %s", o.statement, want)
		}
		if want := []map[string]any{{"x": int64(1)}, {"x": int64(2)}}; o.err != nil || !reflect.DeepEqual(o.rows, want) {
			t.Errorf("rows = %v, err = %v; want the column's values %v, not the whole row", o.rows, o.err, want)
		}
	})
	t.Run("control: an alias that is no column is the whole row to the server", func(t *testing.T) {
		var row string
		if err := f.admin.QueryRowContext(context.Background(), `SELECT "z"::text FROM t_alias AS "z" ORDER BY 1 LIMIT 1`).Scan(&row); err != nil {
			t.Fatalf("control: %v", err)
		}
		t.Logf(`control: SELECT "z"::text FROM t_alias AS "z" is %q`, row)
		if row != "(1,one)" {
			t.Errorf("control = %q, want the whole row (1,one)", row)
		}
	})
	t.Run("the compiler refuses it before the server", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("t_alias", "z")).NewQuery().SelectColumns(dal.Column{Expression: field("z")})
		o := observe(t, f, q)
		t.Logf("result: %s", o.result())
		if o.err == nil || !strings.Contains(o.err.Error(), "is not a column of source") {
			t.Errorf("error = %v, want one saying z is not a column of the source", o.err)
		}
		if sent := statementsSent(f); len(sent) != 0 {
			t.Errorf("statements sent = %v, want none beyond the catalog lookup", sent)
		}
	})
}

// A qualified name x.f is a column of x when x has one, and the function f applied to the
// row of x when it has not (x.to_jsonb is to_jsonb(x)). The server prefers the column: a
// table with columns named like functions returns them as columns. A name that is no
// column is the whole row inside one value, the control below, and the compiler refuses it
// before the server.
func TestServerPinsIntegration_QualifiedColumnWinsOverAFunctionOfTheRow(t *testing.T) {
	f := openQueryFixture(t, "test_pin_function", IdentifierFoldLower, []string{
		`CREATE TABLE t_fn (id integer PRIMARY KEY, to_json text, row_to_json text, count integer)`,
		`INSERT INTO t_fn VALUES (1, 'tj', 'rtj', 7)`,
	})
	t.Run("a column named like a function is the column", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("t_fn", "f")).NewQuery().SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("f", "to_json"), Alias: "a"},
			dal.Column{Expression: dal.NewFieldRef("f", "row_to_json"), Alias: "b"},
			dal.Column{Expression: dal.NewFieldRef("f", "count"), Alias: "c"},
		)
		o := observe(t, f, q)
		t.Logf("statement: %s\n    result: %v", o.statement, o.rows)
		if want := `SELECT "f"."to_json" AS "a", "f"."row_to_json" AS "b", "f"."count" AS "c" FROM "t_fn" AS "f"`; o.statement != want {
			t.Errorf("statement = %s, want %s", o.statement, want)
		}
		if want := []map[string]any{{"a": "tj", "b": "rtj", "c": int64(7)}}; o.err != nil || !reflect.DeepEqual(o.rows, want) {
			t.Errorf("rows = %v, err = %v; want the columns %v", o.rows, o.err, want)
		}
	})
	t.Run("control: a name that is no column is a function of the row to the server", func(t *testing.T) {
		var row string
		if err := f.admin.QueryRowContext(context.Background(), `SELECT (f.to_jsonb)::text FROM t_fn AS f`).Scan(&row); err != nil {
			t.Fatalf("control: %v", err)
		}
		t.Logf("control: SELECT f.to_jsonb FROM t_fn AS f is %s", row)
		if !strings.Contains(row, `"id": 1`) || !strings.Contains(row, `"to_json": "tj"`) {
			t.Errorf("control = %s, want the whole row as JSON", row)
		}
	})
	t.Run("the compiler refuses it before the server", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("t_fn", "f")).NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("f", "to_jsonb"), Alias: "everything"})
		o := observe(t, f, q)
		t.Logf("result: %s", o.result())
		if o.err == nil || !strings.Contains(o.err.Error(), "is not a column of source") {
			t.Errorf("error = %v, want one saying to_jsonb is not a column of the source", o.err)
		}
		if sent := statementsSent(f); len(sent) != 0 {
			t.Errorf("statements sent = %v, want none beyond the catalog lookup", sent)
		}
	})
}

// Two aliases that are one name once names are folded to lower case (Total and TOTAL,
// which the statement writes as "total") are refused before the server in the mode that
// folds names: the server would return two columns of one name and the reader, which keys
// a record by name, would keep one of them. The exact mode writes them as two names and the
// server returns both.
func TestServerPinsIntegration_AliasesThatFoldTogetherAreRefusedBeforeTheServer(t *testing.T) {
	ddl := []string{
		`CREATE TABLE t_fold (id integer PRIMARY KEY, a text, b text)`,
		`INSERT INTO t_fold VALUES (1, 'x', 'y')`,
	}
	query := func() dal.StructuredQuery {
		return dal.From(dal.NewRootCollectionRef("t_fold", "")).NewQuery().SelectColumns(
			dal.Column{Expression: field("a"), Alias: "Total"},
			dal.Column{Expression: field("b"), Alias: "TOTAL"},
		)
	}
	t.Run("fold-lower: refused, nothing is sent after the catalog lookup", func(t *testing.T) {
		f := openQueryFixture(t, "test_pin_fold", IdentifierFoldLower, ddl)
		o := observe(t, f, query())
		t.Logf("result: %s", o.result())
		if !errors.Is(o.err, dal.ErrNotSupported) {
			t.Errorf("error = %v, want one matching dal.ErrNotSupported", o.err)
		}
		if sent := statementsSent(f); len(sent) != 0 {
			t.Errorf("statements sent = %v, want none beyond the catalog lookup", sent)
		}
	})
	t.Run("exact: two names, and the server returns both", func(t *testing.T) {
		f := openQueryFixture(t, "test_pin_fold_exact", IdentifierExact, ddl)
		o := observe(t, f, query())
		t.Logf("statement: %s\n    result: %v", o.statement, o.rows)
		if want := `SELECT "a" AS "Total", "b" AS "TOTAL" FROM "t_fold"`; o.statement != want {
			t.Errorf("statement = %s, want %s", o.statement, want)
		}
		if want := []map[string]any{{"Total": "x", "TOTAL": "y"}}; o.err != nil || !reflect.DeepEqual(o.rows, want) {
			t.Errorf("rows = %v, err = %v; want %v", o.rows, o.err, want)
		}
	})
}

// A select-all over a join is one SELECT * over the join, and the server lists the columns
// of the base source first and then those of each joined source, in the order of the FROM
// clause and not by name: the base's columns are zid and zval, and a column of the joined
// table that sorts before them (aid) comes after.
func TestServerPinsIntegration_SelectAllOverAJoinListsTheBasesColumnsFirst(t *testing.T) {
	f := openQueryFixture(t, "test_pin_join_star", IdentifierFoldLower, []string{
		`CREATE TABLE j_z (zid integer PRIMARY KEY, zval text)`,
		`CREATE TABLE j_a (aid integer PRIMARY KEY, aval text, zref integer)`,
		`INSERT INTO j_z VALUES (1, 'z1'), (2, 'z2')`,
		`INSERT INTO j_a VALUES (10, 'a10', 1), (20, 'a20', 2)`,
	})
	on := dal.NewComparison(dal.NewFieldRef("z", "zid"), dal.Equal, dal.NewFieldRef("a", "zref"))
	q := dal.From(dal.NewRootCollectionRef("j_z", "z")).
		Join(dal.NewJoinedSource(dal.NewRootCollectionRef("j_a", "a"), dal.JoinInner, on)).
		NewQuery().
		OrderBy(dal.Ascending(dal.NewFieldRef("z", "zid"))).
		SelectColumns()

	joiner, ok := dal.As[dal.NativeJoinProvider](f.db)
	if !ok {
		t.Fatal("the Database declares no native join provider")
	}
	if plan, err := dal.PlanJoin(context.Background(), q, joiner); err != nil || plan.Strategy != dal.JoinNative {
		t.Fatalf("plan = %+v, err = %v; want the join run on the server", plan, err)
	}
	reader, err := f.db.ExecuteQueryToRecordsetReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsetReader: %v\nstatements sent: %v", err, f.trace.sent())
	}
	defer func() { _ = reader.Close() }()
	rs := reader.Recordset()
	var names []string
	for i := range rs.ColumnsCount() {
		names = append(names, rs.GetColumnByIndex(i).Name())
	}
	statements := statementsSent(f)
	t.Logf("statements: %v\n    columns, in the order the server lists them: %v", statements, names)
	want := `SELECT * FROM "j_z" AS "z" INNER JOIN "j_a" AS "a" ON ("z"."zid" = "a"."zref") ORDER BY "z"."zid" ASC`
	if len(statements) != 1 || statements[0] != want {
		t.Errorf("statements = %v, want exactly %s", statements, want)
	}
	if wantNames := []string{"zid", "zval", "aid", "aval", "zref"}; !reflect.DeepEqual(names, wantNames) {
		t.Errorf("columns = %v, want the base's first: %v", names, wantNames)
	}
}

// Two text join keys whose columns carry different collations, neither the database's
// default, are accepted by the compiler, which compares the type's category (text with
// text), and planned onto the server. The server cannot compare them: it cannot decide
// which collation the comparison uses and refuses the statement with 42P22. A pair in
// which only one column has a collation of its own is compared in that one, and answers.
// The refusal is an honest one and is a finding for dalgo2sql, whose catalog facts carry
// the collation of a column (NonDeterministicCollation says only whether it is
// deterministic) and could decline such a join to DALgo's own engine.
func TestServerPinsIntegration_TextJoinKeysWithDifferentCollations(t *testing.T) {
	f := openQueryFixture(t, "test_pin_collation", IdentifierFoldLower, []string{
		`CREATE TABLE c_c (id integer PRIMARY KEY, k text COLLATE "C")`,
		`CREATE TABLE c_p (id integer PRIMARY KEY, k text COLLATE "POSIX")`,
		`CREATE TABLE c_d (id integer PRIMARY KEY, k text)`,
		`INSERT INTO c_c VALUES (1, 'x')`,
		`INSERT INTO c_p VALUES (1, 'x')`,
		`INSERT INTO c_d VALUES (1, 'x')`,
	})
	joiner, ok := dal.As[dal.NativeJoinProvider](f.db)
	if !ok {
		t.Fatal("the Database declares no native join provider")
	}
	join := func(left, right string) dal.StructuredQuery {
		on := dal.NewComparison(dal.NewFieldRef("l", "k"), dal.Equal, dal.NewFieldRef("r", "k"))
		return dal.From(dal.NewRootCollectionRef(left, "l")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef(right, "r"), dal.JoinInner, on)).
			NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("l", "id"), Alias: "id"})
	}
	for _, tc := range []struct {
		label       string
		left, right string
		want        cellOutcome
	}{
		{`collation "C" against "POSIX": the server cannot choose one`, "c_c", "c_p", refused("42P22")},
		{`collation "C" against the default: the column with a collation of its own decides`, "c_c", "c_d", rowsOf(1)},
	} {
		t.Run(tc.label, func(t *testing.T) {
			q := join(tc.left, tc.right)
			plan, err := dal.PlanJoin(context.Background(), q, joiner)
			t.Logf("plan: %+v, %v", plan, err)
			if err != nil || plan.Strategy != dal.JoinNative {
				t.Errorf("plan = %+v, err = %v; want the join planned on the server (the compiler accepts the pair by category)", plan, err)
			}
			check(t, tc.label, observe(t, f, q), tc.want)
		})
	}
}

// A run-time error in the middle of the rows (a text that is no number, under the cast
// the arithmetic puts on its operands, at the fourth of five rows) reaches the caller
// as an error from every way of reading: the records reader and the recordset reader,
// row by row, and the read-all helpers of both. A read that ended without the rows after the
// failure would be a shorter result that passes for a complete one.
func TestServerPinsIntegration_RuntimeErrorAtRowNReachesTheCaller(t *testing.T) {
	f := openQueryFixture(t, "test_pin_runtime", IdentifierFoldLower, []string{
		`CREATE TABLE rt (id integer PRIMARY KEY, v text)`,
		`INSERT INTO rt VALUES (1, '1'), (2, '2'), (3, '3'), (4, 'x'), (5, '5')`,
	})
	ctx := context.Background()
	query := func() dal.StructuredQuery {
		return dal.From(dal.NewRootCollectionRef("rt", "")).NewQuery().SelectColumns(
			dal.Column{Expression: field("id")},
			dal.Column{Expression: dal.Binary(field("v"), dal.Multiply, dal.NewConstant(2)), Alias: "twice"},
		)
	}
	wantCode := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || errors.Is(err, dal.ErrNoMoreRecords) {
			t.Errorf("error = %v, want the server's error, not the end of the rows", err)
			return
		}
		if text := err.Error(); !strings.Contains(text, "22P02") && !strings.Contains(text, "invalid input syntax") {
			t.Errorf("error = %v, want the server's 22P02 (invalid input syntax for type double precision)", err)
		}
	}
	t.Run("records reader, row by row", func(t *testing.T) {
		reader, err := f.db.ExecuteQueryToRecordsReader(ctx, query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		delivered := 0
		for {
			_, err = reader.Next()
			if err != nil {
				break
			}
			delivered++
		}
		t.Logf("rows delivered before the error: %d; the caller saw: %v", delivered, err)
		wantCode(t, err)
		if delivered > 3 {
			t.Errorf("rows delivered = %d, want at most the three before the failing row", delivered)
		}
	})
	t.Run("recordset reader, row by row", func(t *testing.T) {
		reader, err := f.db.ExecuteQueryToRecordsetReader(ctx, query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		delivered := 0
		for {
			_, _, err = reader.Next()
			if err != nil {
				break
			}
			delivered++
		}
		t.Logf("rows delivered before the error: %d; the caller saw: %v", delivered, err)
		wantCode(t, err)
		if delivered > 3 {
			t.Errorf("rows delivered = %d, want at most the three before the failing row", delivered)
		}
	})
	t.Run("read-all helpers in a transaction", func(t *testing.T) {
		var records int
		err := f.db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
			got, err := dal.ExecuteQueryAndReadAllToRecords(ctx, query(), tx)
			records = len(got)
			return err
		})
		t.Logf("records read: %d; the caller saw: %v", records, err)
		wantCode(t, err)
		err = f.db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
			rs, err := dal.ExecuteQueryAndReadAllToRecordset(ctx, query(), tx)
			if err == nil {
				t.Errorf("recordset read all = %d rows and no error, want the server's error", rs.RowsCount())
			}
			return err
		})
		t.Logf("recordset: the caller saw: %v", err)
		wantCode(t, err)
	})
	t.Run("the pool is whole afterwards", func(t *testing.T) {
		if got := f.sqlDB.Stats().InUse; got != 0 {
			t.Errorf("connections in use = %d, want 0", got)
		}
	})
}

// A column that is NOT NULL keeps its ORDER BY without a NULLS clause, which lets an
// index serve the order, and sorts as expected; a nullable column keeps NULLS FIRST; and a
// NOT NULL column on the nullable side of a LEFT JOIN keeps its NULLS clause too, because
// the join makes it NULL for a row with no partner, and those rows sort first.
func TestServerPinsIntegration_NotNullColumnKeepsItsOrderByWithoutANullsClause(t *testing.T) {
	f := openQueryFixture(t, "test_pin_not_null", IdentifierFoldLower, []string{
		`CREATE TABLE nn (id integer PRIMARY KEY, rank integer NOT NULL, label text)`,
		`CREATE TABLE nn_b (id integer PRIMARY KEY, nnid integer NOT NULL, bonus integer NOT NULL)`,
		`INSERT INTO nn VALUES (1, 2, 'b'), (2, 1, 'a'), (3, 2, NULL), (4, 3, 'd')`,
		`INSERT INTO nn_b VALUES (1, 1, 10), (2, 4, 5)`,
	})
	selectID := dal.Column{Expression: field("id")}
	t.Run("NOT NULL columns: no NULLS clause", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("nn", "")).NewQuery().
			OrderBy(dal.AscendingField("rank"), dal.DescendingField("id")).SelectColumns(selectID)
		o := observe(t, f, q)
		check(t, "ORDER BY rank, id DESC", o, rowsOf(2, 3, 1, 4))
		if want := `SELECT "id" FROM "nn" ORDER BY "rank" ASC, "id" DESC`; o.statement != want {
			t.Errorf("statement = %s, want %s", o.statement, want)
		}
	})
	t.Run("a nullable column: NULLS FIRST, and the NULL row first", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("nn", "")).NewQuery().
			OrderBy(dal.AscendingField("label"), dal.AscendingField("id")).SelectColumns(selectID)
		o := observe(t, f, q)
		check(t, "ORDER BY label, id", o, rowsOf(3, 2, 1, 4))
		if want := `SELECT "id" FROM "nn" ORDER BY "label" ASC NULLS FIRST, "id" ASC`; o.statement != want {
			t.Errorf("statement = %s, want %s", o.statement, want)
		}
	})
	t.Run("a NOT NULL column on the nullable side of a LEFT JOIN keeps NULLS FIRST", func(t *testing.T) {
		on := dal.NewComparison(dal.NewFieldRef("nn", "id"), dal.Equal, dal.NewFieldRef("b", "nnid"))
		q := dal.From(dal.NewRootCollectionRef("nn", "")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef("nn_b", "b"), dal.JoinLeft, on)).
			NewQuery().
			OrderBy(dal.Ascending(dal.NewFieldRef("b", "bonus")), dal.Ascending(dal.NewFieldRef("nn", "id"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("nn", "id"), Alias: "id"})
		o := observe(t, f, q)
		// Rows 2 and 3 have no partner, so their bonus is NULL and they come first; then
		// row 4 (bonus 5) and row 1 (bonus 10).
		check(t, "LEFT JOIN ORDER BY b.bonus, nn.id", o, rowsOf(2, 3, 4, 1))
		if want := `SELECT "nn"."id" AS "id" FROM "nn" LEFT JOIN "nn_b" AS "b" ON ("nn"."id" = "b"."nnid") ORDER BY "b"."bonus" ASC NULLS FIRST, "nn"."id" ASC`; o.statement != want {
			t.Errorf("statement = %s, want %s", o.statement, want)
		}
	})
}
