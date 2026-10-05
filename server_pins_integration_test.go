package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/jackc/pgx/v5/pgconn"
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

// The statements of the next two tests, and of the LEFT JOIN case further down, end with
// "id" AS "__dalgo_record_id": with dalgo2sql v0.26.3 the compiler adds the catalog's
// one-column primary key to the select list, under a hidden name, when the select list does
// not name it, so that each record is keyed by it (README, "How records are keyed"). The
// fixtures keep their primary keys and the expected statements say so.
//
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
		if want := `SELECT "f"."to_json" AS "a", "f"."row_to_json" AS "b", "f"."count" AS "c", "id" AS "__dalgo_record_id" FROM "t_fn" AS "f"`; o.statement != want {
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
		if want := `SELECT "a" AS "Total", "b" AS "TOTAL", "id" AS "__dalgo_record_id" FROM "t_fold"`; o.statement != want {
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
// default, cannot be compared by the server: it cannot decide which collation the
// comparison uses and refuses the statement with 42P22. The catalog tells the compiler each
// column's collation (dalgo2sql v0.26.5), so such a join is declined to DALgo's own engine,
// which compares the two texts itself and answers; a pair in which only one column has a
// collation of its own is compared by the server in that one, and answers. (Up to v0.26.4
// the compiler planned the first pair on the server, which refused it.) The control below
// states the server's refusal of the statement the compiler no longer sends, so the reason
// for the plan is a fact about the server and not only about the compiler.
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
	t.Run(`control: the server refuses collation "C" against "POSIX" with 42P22`, func(t *testing.T) {
		var id int
		err := f.admin.QueryRowContext(context.Background(),
			`SELECT l.id FROM c_c AS l INNER JOIN c_p AS r ON l.k = r.k`).Scan(&id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42P22" {
			t.Errorf("error = %v, want the server's 42P22 (indeterminate collation)", err)
		}
	})
	t.Run(`collation "C" against "POSIX": declined to DALgo's engine, which answers`, func(t *testing.T) {
		q := join("c_c", "c_p")
		plan, err := dal.PlanJoin(context.Background(), q, joiner)
		t.Logf("plan: %+v, %v", plan, err)
		if err != nil || plan.Strategy == dal.JoinNative {
			t.Errorf("plan = %+v, err = %v; want a plan that is not native: the server cannot compare the two", plan, err)
		}
		o := observe(t, f, q)
		t.Logf("statements: %v\n    rows: %v (error: %v)", statementsSent(f), o.rows, o.err)
		// DALgo's engine returns the integer as its own Go type, not the reader's int64, so
		// the rows are compared as JSON holds them.
		got, normalizeErr := normalizeRows(o.rows)
		if o.err != nil || normalizeErr != nil || rowDifference(got, []map[string]any{{"id": 1.0}}) != "" {
			t.Errorf(`collation "C" against "POSIX", in DALgo's engine: rows = %v, error %v, want the one row with id 1`, o.rows, o.err)
		}
		for _, statement := range statementsSent(f) {
			if strings.Contains(statement, " JOIN ") {
				t.Errorf("a statement joins on the server: %s", statement)
			}
		}
	})
	t.Run(`collation "C" against the default: the column with a collation of its own decides`, func(t *testing.T) {
		q := join("c_c", "c_d")
		plan, err := dal.PlanJoin(context.Background(), q, joiner)
		t.Logf("plan: %+v, %v", plan, err)
		if err != nil || plan.Strategy != dal.JoinNative {
			t.Errorf("plan = %+v, err = %v; want the join planned on the server", plan, err)
		}
		check(t, "collation C against the default", observe(t, f, q), rowsOf(1))
	})
}

// A run-time error in the middle of the rows (a text that is no number, under the cast
// the arithmetic puts on its operands, at the fourth of five rows) must reach the caller
// as an error, from the records reader and from the recordset reader, not as a shorter
// result that passes for a complete one.
//
// With dalgo2sql v0.26.0 only the records reader did it. The recordset reader answered
// dal.ErrNoMoreRecords after the third row, the end of the rows, and the read-all helper
// returned the three rows and no error; inside a read transaction the caller saw only the
// commit failing with "commit unexpectedly resulted in rollback". Since v0.26.1 the
// recordset reader asks the stream for its error, and both readers give the same answer:
// three rows, then the server's 22P02, row by row, through the read-all helper, and out of
// a read transaction.
func TestServerPinsIntegration_RuntimeErrorAtRowN(t *testing.T) {
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
	// logStatement writes the statement the driver was handed for the last read.
	logStatement := func(t *testing.T) {
		t.Helper()
		t.Logf("statement: %s", strings.Join(statementsSent(f), "; "))
	}
	// theServersError says the error is the server's refusal of the cast, not the end of
	// the rows and not another error.
	theServersError := func(t *testing.T, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "22P02" || errors.Is(err, dal.ErrNoMoreRecords) {
			t.Errorf("error = %v, want the server's 22P02 (invalid input syntax for type double precision)", err)
		}
	}

	t.Run("records reader, row by row: the error reaches the caller", func(t *testing.T) {
		f.trace.reset()
		reader, err := f.db.ExecuteQueryToRecordsReader(ctx, query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		delivered := 0
		for {
			if _, err = reader.Next(); err != nil {
				break
			}
			delivered++
		}
		logStatement(t)
		t.Logf("rows delivered before the error: %d; the caller saw: %v", delivered, err)
		theServersError(t, err)
		if delivered != 3 {
			t.Errorf("rows delivered = %d, want the three before the failing row", delivered)
		}
	})
	t.Run("records reader, read-all helper: the error reaches the caller", func(t *testing.T) {
		_, onTheDatabase := dal.ExecuteQueryAndReadAllToRecords(ctx, query(), f.db)
		t.Logf("on the database handle the caller saw: %v", onTheDatabase)
		theServersError(t, onTheDatabase)
		inATransaction := f.db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
			_, err := dal.ExecuteQueryAndReadAllToRecords(ctx, query(), tx)
			return err
		})
		t.Logf("in a read transaction the caller saw: %v", inATransaction)
		theServersError(t, inATransaction)
	})
	t.Run("recordset reader, row by row: the error reaches the caller", func(t *testing.T) {
		f.trace.reset()
		reader, err := f.db.ExecuteQueryToRecordsetReader(ctx, query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		delivered := 0
		for {
			if _, _, err = reader.Next(); err != nil {
				break
			}
			delivered++
		}
		logStatement(t)
		t.Logf("rows delivered before the error: %d; the caller saw: %v", delivered, err)
		theServersError(t, err)
		if delivered != 3 {
			t.Errorf("rows delivered = %d, want the three before the failing row", delivered)
		}
	})
	t.Run("recordset reader, read-all helper: the error reaches the caller", func(t *testing.T) {
		f.trace.reset()
		rs, onTheDatabase := dal.ExecuteQueryAndReadAllToRecordset(ctx, query(), f.db)
		logStatement(t)
		t.Logf("on the database handle the caller saw: %v (recordset returned with the error: %v)", onTheDatabase, rs != nil)
		theServersError(t, onTheDatabase)
		var rows int
		inATransaction := f.db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
			rs, err := dal.ExecuteQueryAndReadAllToRecordset(ctx, query(), tx)
			if rs != nil {
				rows = rs.RowsCount()
			}
			return err
		})
		t.Logf("in a read transaction the caller saw: %v", inATransaction)
		theServersError(t, inATransaction)
		if inATransaction != nil && strings.Contains(inATransaction.Error(), "commit unexpectedly resulted in rollback") {
			t.Errorf("in a read transaction the caller saw %q: the cause of the failure is lost (rows read: %d)", inATransaction, rows)
		}
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
		if want := `SELECT "nn"."id" AS "id", "nn"."id" AS "__dalgo_record_id" FROM "nn" LEFT JOIN "nn_b" AS "b" ON ("nn"."id" = "b"."nnid") ORDER BY "b"."bonus" ASC NULLS FIRST, "nn"."id" ASC`; o.statement != want {
			t.Errorf("statement = %s, want %s", o.statement, want)
		}
	})
}

// A keys-only read of a table is sent with an ORDER BY on the primary key, and the server
// orders text by the column's collation, which for a table that declares none is the
// database's: in CI en_US.utf8, which puts "São Paulo" before "Shanghai" and "Sindh", where
// Go's sort.Strings, byte by byte, puts it last. The shared suite of dal-go/dalgo asserts
// the keys-only read of its cities in Go's order (TestEndToEnd/query/SELECT_ID_FROM_Cities),
// so with the key column in the database's collation that sub-test fails on this server;
// the fixture of TestEndToEnd gives the key COLLATE "C" and it passes.
//
// This is a DIVERGENCE from the shared suite's keys-only contract, not a rule and not a
// defect of this package, reported for dal-go/dalgo (does the contract say the order is
// Go's?) and dal-go/dalgo2sql (which writes the ORDER BY on the key): a real table does not
// declare COLLATE "C". The test pins what the server does with the statement shown, prints
// the database's collation, and runs the same rows under COLLATE "C" as the control that
// equals sort.Strings. If the database's collation orders like C the pin shows nothing, and
// the test fails and says so rather than skipping.
func TestServerPinsIntegration_KeysOnlyReadFollowsTheDatabasesCollation(t *testing.T) {
	rows := `('Shanghai_Shanghai'), ('Sindh_Karachi'), ('São Paulo_São Paulo')`
	f := openQueryFixture(t, "test_pin_keys_collation", IdentifierFoldLower, []string{
		`CREATE TABLE kc_default (id text PRIMARY KEY)`,
		`INSERT INTO kc_default VALUES ` + rows,
		`CREATE TABLE kc_c (id text COLLATE "C" PRIMARY KEY)`,
		`INSERT INTO kc_c VALUES ` + rows,
	})
	var collate, provider string
	if err := f.admin.QueryRowContext(context.Background(),
		`SELECT datcollate, datlocprovider::text FROM pg_database WHERE datname = current_database()`).Scan(&collate, &provider); err != nil {
		t.Fatalf("pg_database: %v", err)
	}
	t.Logf("the database's collation: datcollate = %s, datlocprovider = %s", collate, provider)

	keys := func(t *testing.T, table string) (statement string, ids []string) {
		t.Helper()
		statement, got := keysOf(t, f, dal.From(dal.NewRootCollectionRef(table, "")).NewQuery().SelectKeysOnly(reflect.String))
		for _, row := range got {
			id, _ := row.id.(string)
			ids = append(ids, id)
		}
		t.Logf("statement: %s\n    keys, in the order the server returned them: %v", statement, ids)
		if !strings.HasSuffix(statement, `ORDER BY "id" ASC`) {
			t.Errorf("statement = %s, want one ending ORDER BY \"id\" ASC", statement)
		}
		return statement, ids
	}
	goOrder := []string{"Shanghai_Shanghai", "Sindh_Karachi", "São Paulo_São Paulo"}
	if !sort.StringsAreSorted(goOrder) {
		t.Fatalf("the fixture is not in Go's order: %v", goOrder)
	}
	t.Run("in the database's collation the server's order is not Go's", func(t *testing.T) {
		_, ids := keys(t, "kc_default")
		if reflect.DeepEqual(ids, goOrder) {
			t.Fatalf("keys = %v: either the database's collation (%s) orders like C, so this pin shows nothing and the job must run on a database whose collation is not C or POSIX, "+
				"or a keys-only read is now in Go's order: then assert Go's order here, drop COLLATE \"C\" from the dalgotest_cities table in end2endDDL, and delete the limit in the README", ids, collate)
		}
		if want := []string{"São Paulo_São Paulo", "Shanghai_Shanghai", "Sindh_Karachi"}; !reflect.DeepEqual(ids, want) {
			t.Errorf("keys = %v, want the linguistic order of %s: %v. This differs from sort.Strings, which the shared suite's keys-only contract asserts: a divergence reported for dal-go/dalgo and dal-go/dalgo2sql. "+
				"If the keys are now in Go's order, a keys-only read no longer follows the database's collation: assert Go's order for kc_default here, "+
				"drop COLLATE \"C\" from the dalgotest_cities table in end2endDDL (end2end_test.go), and delete the limit in the README", ids, collate, want)
		}
	})
	t.Run(`control: the same keys under COLLATE "C" are in Go's order`, func(t *testing.T) {
		_, ids := keys(t, "kc_c")
		if !reflect.DeepEqual(ids, goOrder) {
			t.Errorf("keys = %v, want Go's order %v", ids, goOrder)
		}
	})
}

// PostgreSQL 18 lets a not-null constraint be added NOT VALID: the constraint is enforced for the
// rows written from then on and does not vouch for the rows that were there, which may hold
// NULL. The catalog calls such a column NOT NULL (pg_attribute.attnotnull is true; the
// constraint's convalidated is false). The compiler writes no NULLS clause for a NOT NULL
// column, so that an index can serve the order, and relies on the server's default (NULLs last
// ascending, first descending), which is the opposite of DALgo's rule (NULLs first ascending
// and last descending). With ORDER BY and LIMIT the NULL rows that DALgo puts first would then be
// cut off: the first rows are not the ones DALgo returns.
//
// On PostgreSQL 17 the syntax does not exist, and the control asserts that it is refused (a
// syntax error), so the test runs and passes on both legs of the job and says which it is.
func TestServerPinsIntegration_NotValidNotNullColumnWithNullsUnderOrderByAndLimit(t *testing.T) {
	f := openQueryFixture(t, "test_pin_not_valid", IdentifierFoldLower, []string{
		`CREATE TABLE nv (id integer PRIMARY KEY, v integer)`,
		`INSERT INTO nv VALUES (1, 3), (2, NULL), (3, 1), (4, NULL), (5, 2)`,
	})
	ctx := context.Background()
	var major int
	if err := f.admin.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int / 10000`).Scan(&major); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	const addConstraint = `ALTER TABLE nv ADD CONSTRAINT nv_v_not_null NOT NULL v NOT VALID`
	if major < 18 {
		_, err := f.admin.ExecContext(ctx, addConstraint)
		var pgErr *pgconn.PgError
		t.Logf("PostgreSQL %d: %s -> %v", major, addConstraint, err)
		if !errors.As(err, &pgErr) || pgErr.Code != "42601" {
			t.Errorf("error = %v, want the server's syntax error 42601: PostgreSQL %d has no NOT VALID not-null constraint", err, major)
		}
		return
	}
	if _, err := f.admin.ExecContext(ctx, addConstraint); err != nil {
		t.Fatalf("%s: %v", addConstraint, err)
	}
	var notNull, validated bool
	if err := f.admin.QueryRowContext(ctx, `SELECT a.attnotnull, c.convalidated FROM pg_attribute a
		JOIN pg_constraint c ON c.conrelid = a.attrelid AND c.contype = 'n' AND a.attnum = ANY (c.conkey)
		WHERE a.attrelid = 'nv'::regclass AND a.attname = 'v'`).Scan(&notNull, &validated); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	t.Logf("PostgreSQL %d, column v: attnotnull = %v, constraint validated = %v; rows 2 and 4 hold NULL", major, notNull, validated)

	ordered := func(by dal.OrderExpression) dal.StructuredQuery {
		return dal.From(dal.NewRootCollectionRef("nv", "")).NewQuery().
			OrderBy(by, dal.AscendingField("id")).Limit(3).
			SelectColumns(dal.Column{Expression: field("id")})
	}
	for _, tc := range []struct {
		label string
		by    dal.OrderExpression
		want  []int
	}{
		{"ascending: the NULL rows first", dal.AscendingField("v"), []int{2, 4, 3}},
		{"descending: the NULL rows last", dal.DescendingField("v"), []int{1, 5, 3}},
	} {
		o := observe(t, f, ordered(tc.by))
		check(t, "ORDER BY v "+tc.label+", LIMIT 3", o, rowsOf(tc.want...))
	}
}
