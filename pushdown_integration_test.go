package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// Pushdown: a grouped query and a join that DALgo's own engine cannot answer, because they
// are past its bounds, are answered by the server in one statement. DALgo's engine is in
// memory and bounded (dalgo v0.89.6, dal/aggregation_execute.go and dal/join_execute.go):
// an aggregation stops at 100,000 groups with "group limit 100000 exceeded", a join at
// 10,000 rows (and at 16 MiB of rows) with a join_plan error, and neither returns a partial
// result. So an answer past those bounds cannot have been made in memory, and the tests
// below ask for one: 150,000 groups out of 200,000 rows, and 50,000 joined rows of 400 bytes
// of text each, which is 20,000,000 bytes. The negative control runs the very same queries
// against the same tables through a database with no dialect, which has no native route,
// and they are refused; the same shapes over a handful of rows are answered by both, with
// the same rows, so the refusal is the bound and not the shape.
//
// They run against PostgreSQL and skip unless DALGO2POSTGRES_TEST_DSN is set; CI fails when a
// TestPushdownIntegration_ test skips or is missing.

const (
	// The bounds of DALgo's engine the data is past. The constants are unexported in dalgo, so
	// they are written here, with their names; the control below fails if a bound ever rises
	// past what this data is.
	engineMaxGroups    = 100_000  // defaultMaxAggregationGroups
	engineMaxJoinRows  = 10_000   // maxJoinRows
	engineMaxJoinBytes = 16 << 20 // maxJoinBytes

	factRows     = 200_000 // rows of pd_fact
	factModulus  = 150_000 // its groups: grp = id % factModulus
	joinRows     = 50_000  // rows of pd_order and of pd_customer
	noteLength   = 400     // bytes of the text of each order
	smallFact    = 1_000   // rows of pd_fact_small
	smallJoinRow = 100     // rows of pd_order_small and of pd_customer_small
)

// pushdownDDL builds the tables with generate_series: pd_fact has 200,000 rows with 150,000
// groups of grp (the first 50,000 have two rows, the others one), pd_order and pd_customer
// have 50,000 rows each, one customer to an order, and the *_small tables are the same
// shapes with a few rows.
func pushdownDDL() []string {
	n := strconv.Itoa
	return []string{
		`CREATE TABLE pd_fact AS SELECT g AS id, g % ` + n(factModulus) + ` AS grp, 1 + g % 7 AS v FROM generate_series(1, ` + n(factRows) + `) AS g`,
		`ALTER TABLE pd_fact ADD PRIMARY KEY (id)`,
		`CREATE TABLE pd_fact_small AS SELECT g AS id, g % ` + n(factModulus) + ` AS grp, 1 + g % 7 AS v FROM generate_series(1, ` + n(smallFact) + `) AS g`,
		`ALTER TABLE pd_fact_small ADD PRIMARY KEY (id)`,
		`CREATE TABLE pd_customer AS SELECT g AS id, 'customer ' || g AS name FROM generate_series(1, ` + n(joinRows) + `) AS g`,
		`ALTER TABLE pd_customer ADD PRIMARY KEY (id)`,
		`CREATE TABLE pd_order AS SELECT g AS id, g AS customer_id, repeat('x', ` + n(noteLength) + `) AS note FROM generate_series(1, ` + n(joinRows) + `) AS g`,
		`ALTER TABLE pd_order ADD PRIMARY KEY (id)`,
		`CREATE TABLE pd_customer_small AS SELECT g AS id, 'customer ' || g AS name FROM generate_series(1, ` + n(smallJoinRow) + `) AS g`,
		`ALTER TABLE pd_customer_small ADD PRIMARY KEY (id)`,
		`CREATE TABLE pd_order_small AS SELECT g AS id, g AS customer_id, repeat('x', ` + n(noteLength) + `) AS note FROM generate_series(1, ` + n(smallJoinRow) + `) AS g`,
		`ALTER TABLE pd_order_small ADD PRIMARY KEY (id)`,
	}
}

// groupedQuery is SELECT grp, COUNT(*) AS n, SUM(v) AS total FROM table GROUP BY grp
// HAVING SUM(v) > 1 ORDER BY grp: every group of two rows or more passes, and so does a group
// of one row whose v is more than 1.
func groupedQuery(table string) dal.StructuredQuery {
	sum := dal.SumAs(field("v"), "").Expression
	return dal.From(dal.NewRootCollectionRef(table, "")).NewQuery().
		GroupBy(field("grp")).
		Having(dal.NewComparison(sum, dal.GreaterThen, dal.NewConstant(1))).
		OrderBy(dal.AscendingField("grp")).
		SelectColumns(
			dal.Column{Expression: field("grp")},
			dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"},
			dal.SumAs(field("v"), "total"),
		)
}

// expectedGroups is what groupedQuery must return for ids 1..rows: the groups of grp = id %
// factModulus with their count and sum of v = 1 + id % 7, those whose sum is more than 1, in
// the order of grp.
func expectedGroups(rows int) []map[string]any {
	type group struct{ n, total int64 }
	groups := map[int64]*group{}
	for id := 1; id <= rows; id++ {
		key := int64(id % factModulus)
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		g.n++
		g.total += int64(1 + id%7)
	}
	var out []map[string]any
	for key := int64(0); key < factModulus; key++ {
		if g := groups[key]; g != nil && g.total > 1 {
			out = append(out, map[string]any{"grp": key, "n": g.n, "total": float64(g.total)})
		}
	}
	return out
}

// joinQuery is the orders of a table joined to their customers, with the text of the order,
// in the order of the order's id.
func joinQuery(orders, customers string) dal.StructuredQuery {
	on := dal.NewComparison(dal.NewFieldRef("o", "customer_id"), dal.Equal, dal.NewFieldRef("c", "id"))
	return dal.From(dal.NewRootCollectionRef(orders, "o")).
		Join(dal.NewJoinedSource(dal.NewRootCollectionRef(customers, "c"), dal.JoinInner, on)).
		NewQuery().
		OrderBy(dal.Ascending(dal.NewFieldRef("o", "id"))).
		SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("o", "id"), Alias: "order_id"},
			dal.Column{Expression: dal.NewFieldRef("c", "name"), Alias: "customer"},
			dal.Column{Expression: dal.NewFieldRef("o", "note"), Alias: "note"},
		)
}

// checkJoinRows asserts the rows of joinQuery over n orders: the order ids in order, each
// customer the order's own, each text noteLength bytes. It returns the bytes of text.
func checkJoinRows(t *testing.T, rows []map[string]any, n int) (bytes int) {
	t.Helper()
	if len(rows) != n {
		t.Fatalf("the join returned %d rows, want %d", len(rows), n)
	}
	for i, row := range rows {
		id := int64(i + 1)
		note, _ := row["note"].(string)
		if row["order_id"] != id || row["customer"] != "customer "+strconv.FormatInt(id, 10) || len(note) != noteLength {
			t.Fatalf("row %d = order_id %v, customer %v, %d bytes of note; want order %d, its customer and %d bytes",
				i, row["order_id"], row["customer"], len(note), id, noteLength)
		}
		bytes += len(note)
	}
	return bytes
}

// A GROUP BY with 150,000 groups and a HAVING over 200,000 rows is one statement on the
// server, and its answer has more groups than DALgo's engine holds.
func TestPushdownIntegration_GroupByOver150000GroupsRunsOnTheServer(t *testing.T) {
	f := openQueryFixture(t, "test_pd_group", IdentifierFoldLower, pushdownDDL())
	run := runThroughAdapter(f, groupedQuery("pd_fact"))
	if run.err != nil {
		t.Fatalf("the grouped query failed: %v\nstatements: %v", run.err, run.statements)
	}
	if len(run.statements) != 1 {
		t.Fatalf("%d statements were sent, want one: %v", len(run.statements), run.statements)
	}
	statement := run.statements[0]
	t.Logf("statement: %s\nrows: %d groups", statement, len(run.rows))
	for _, part := range []string{`FROM "pd_fact"`, `GROUP BY "grp"`, `HAVING`, `SUM("v")`, `COUNT(*)`} {
		if !strings.Contains(statement, part) {
			t.Errorf("statement = %s, want the server to group, filter the groups and aggregate: no %s", statement, part)
		}
	}
	if len(run.rows) <= engineMaxGroups {
		t.Errorf("the answer has %d groups, want more than the %d DALgo's engine holds: the proof proves nothing", len(run.rows), engineMaxGroups)
	}
	got, err := normalizeRows(run.rows)
	if err != nil {
		t.Fatal(err)
	}
	want, err := normalizeRows(expectedGroups(factRows))
	if err != nil {
		t.Fatal(err)
	}
	if difference := rowDifference(got, want); difference != "" {
		t.Errorf("groups differ from the ones computed here: %s", difference)
	}
	// The argument of the HAVING is bound, and nothing else is: one value in all.
	sent := f.trace.sent()
	if last := sent[len(sent)-1]; len(valueArguments(last.args)) != 1 {
		t.Errorf("arguments = %s, want the one bound value of the HAVING", argsText(valueArguments(last.args)))
	}
}

// A join over 50,000 rows is one statement on the server, and its answer has more rows than
// DALgo's engine joins (10,000) and more bytes than it holds (16 MiB).
func TestPushdownIntegration_JoinOver50000RowsRunsOnTheServer(t *testing.T) {
	f := openQueryFixture(t, "test_pd_join", IdentifierFoldLower, pushdownDDL())
	run := runThroughAdapter(f, joinQuery("pd_order", "pd_customer"))
	if run.err != nil {
		t.Fatalf("the join failed: %v\nstatements: %v", run.err, run.statements)
	}
	if len(run.statements) != 1 || countJoins(run.statements[0]) != 1 {
		t.Fatalf("statements = %v, want exactly one, with the JOIN", run.statements)
	}
	t.Logf("statement: %s", run.statements[0])
	bytes := checkJoinRows(t, run.rows, joinRows)
	t.Logf("%d joined rows, %d bytes of text", len(run.rows), bytes)
	if len(run.rows) <= engineMaxJoinRows || bytes <= engineMaxJoinBytes {
		t.Errorf("the answer has %d rows and %d bytes, want more than %d rows and %d bytes: the proof proves nothing",
			len(run.rows), bytes, engineMaxJoinRows, engineMaxJoinBytes)
	}
}

// The negative control. A database with no dialect has no native route: it declares no
// aggregation and accepts no join, so DALgo's engine runs the same queries in memory over
// plain reads of the tables, and over the bounds it refuses them. Without this test the two
// above would pass for any database that answers.
//
// The same queries over the _small tables, a few rows, are answered by the engine and by the
// server with the same rows: the refusal at scale is the bound, not the shape of the query
// and not the legacy reads.
func TestPushdownIntegration_WithoutTheNativeRouteTheSameQueriesAreRefused(t *testing.T) {
	f := openQueryFixture(t, "test_pd_control", IdentifierFoldLower, pushdownDDL())
	ctx := context.Background()
	inMemory := dalgo2sql.NewDatabase(f.sqlDB, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{})
	readRows := func(db dal.DB, q dal.StructuredQuery) ([]map[string]any, error) {
		records, err := dal.ExecuteQueryAndReadAllToRecords(ctx, q, db)
		rows := make([]map[string]any, 0, len(records))
		for _, rec := range records {
			data, _ := rec.Data().(map[string]any)
			rows = append(rows, data)
		}
		return rows, err
	}
	noServerSideWork := func(t *testing.T) {
		t.Helper()
		for _, statement := range statementsSent(f) {
			upper := strings.ToUpper(statement)
			if strings.Contains(upper, "GROUP BY") || strings.Contains(upper, " JOIN ") || strings.Contains(upper, "SUM(") {
				t.Errorf("a statement does server-side work: %s", statement)
			}
		}
	}

	t.Run("a grouped query past 100,000 groups is refused", func(t *testing.T) {
		f.trace.reset()
		rows, err := readRows(inMemory, groupedQuery("pd_fact"))
		t.Logf("error: %v (rows returned: %d); statements: %v", err, len(rows), statementsSent(f))
		if err == nil || !strings.Contains(err.Error(), "group limit") {
			t.Errorf("error = %v, want DALgo's group limit: the query was not answered by the engine", err)
		}
		if len(rows) != 0 {
			t.Errorf("%d rows were returned with the refusal, want none: a partial result is worse than none", len(rows))
		}
		noServerSideWork(t)
	})
	t.Run("a join past 10,000 rows is refused", func(t *testing.T) {
		f.trace.reset()
		rows, err := readRows(inMemory, joinQuery("pd_order", "pd_customer"))
		t.Logf("error: %v (rows returned: %d); statements: %v", err, len(rows), statementsSent(f))
		var join *dal.JoinValidationError
		var query *dal.QueryValidationError
		if err == nil || (!errors.As(err, &join) && !errors.As(err, &query)) {
			t.Errorf("error = %v, want DALgo's join bound: the query was not answered by the engine", err)
		}
		if len(rows) != 0 {
			t.Errorf("%d rows were returned with the refusal, want none", len(rows))
		}
		noServerSideWork(t)
	})
	t.Run("the engine's own leaf reads of the adapter refuse the join too", func(t *testing.T) {
		// The same refusal through the existing seam of DALgo: the generic engine over
		// the adapter's plain reads, the native join route not asked.
		f.trace.reset()
		reader, err := dal.ExecuteRecursiveQuery(ctx, f.db, joinQuery("pd_order", "pd_customer"))
		if err == nil {
			_, err = dal.ReadAllToRecords(ctx, reader)
		}
		t.Logf("error: %v; statements: %v", err, statementsSent(f))
		var join *dal.JoinValidationError
		var query *dal.QueryValidationError
		if err == nil || (!errors.As(err, &join) && !errors.As(err, &query)) {
			t.Errorf("error = %v, want DALgo's join bound", err)
		}
		noServerSideWork(t)
	})
	t.Run("the same shapes over a few rows are answered by both, with the same rows", func(t *testing.T) {
		native, err := readRows(f.db, groupedQuery("pd_fact_small"))
		if err != nil {
			t.Fatalf("native grouped query: %v", err)
		}
		f.trace.reset()
		memory, err := readRows(inMemory, groupedQuery("pd_fact_small"))
		if err != nil {
			t.Fatalf("in-memory grouped query: %v", err)
		}
		noServerSideWork(t)
		got, _ := normalizeRows(memory)
		want, _ := normalizeRows(native)
		if difference := rowDifference(got, want); difference != "" {
			t.Errorf("the engine's groups differ from the server's: %s", difference)
		}
		if wantRows, _ := normalizeRows(expectedGroups(smallFact)); rowDifference(want, wantRows) != "" {
			t.Errorf("the server's groups differ from the ones computed here: %s", rowDifference(want, wantRows))
		}

		nativeJoin, err := readRows(f.db, joinQuery("pd_order_small", "pd_customer_small"))
		if err != nil {
			t.Fatalf("native join: %v", err)
		}
		f.trace.reset()
		memoryJoin, err := readRows(inMemory, joinQuery("pd_order_small", "pd_customer_small"))
		if err != nil {
			t.Fatalf("in-memory join: %v", err)
		}
		noServerSideWork(t)
		checkJoinRows(t, nativeJoin, smallJoinRow)
		checkJoinRows(t, memoryJoin, smallJoinRow)
		if !reflect.DeepEqual(nativeJoin, memoryJoin) {
			t.Error("the engine's joined rows differ from the server's")
		}
	})
}
