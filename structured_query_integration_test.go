package dalgo2postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The tests in this file run against a real PostgreSQL server and skip unless
// DALGO2POSTGRES_TEST_DSN is set (see testDSN); CI sets it, and its Conformance
// job fails when any TestStructuredQueryIntegration_ test skips or is missing. They
// are the first place the typed compiler's statements and the catalog queries of
// dalgo2sql meet a server. Each creates its own schemas and drops them afterwards.

// tracedStatement is one statement the driver sent: its text, its arguments and
// the backend process that ran it.
type tracedStatement struct {
	sql  string
	args []any
	pid  uint32
}

// queryTrace is a pgx.QueryTracer that records every statement sent through the
// handle it is attached to. The hooks run on the driver's goroutine while a
// statement is in flight, so a test can end a context at an exact point.
type queryTrace struct {
	mu         sync.Mutex
	statements []tracedStatement
	atStart    func(statement string)
	atEnd      func(statement string)
}

func (q *queryTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	q.mu.Lock()
	q.statements = append(q.statements, tracedStatement{sql: data.SQL, args: data.Args, pid: conn.PgConn().PID()})
	hook := q.atStart
	q.mu.Unlock()
	if hook != nil {
		hook(data.SQL)
	}
	return ctx
}

func (q *queryTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	q.mu.Lock()
	hook := q.atEnd
	statement := ""
	if n := len(q.statements); n > 0 {
		statement = q.statements[n-1].sql
	}
	q.mu.Unlock()
	if hook != nil {
		hook(statement)
	}
}

// onStart and onEnd set the hooks; a hook sees the text of the statement.
func (q *queryTrace) onStart(hook func(statement string)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.atStart = hook
}

func (q *queryTrace) onEnd(hook func(statement string)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.atEnd = hook
}

// isCatalogStatement says whether statement is dalgo2sql's catalog lookup. It holds
// JOINs of its own, so a test that looks for the JOIN of a query skips it.
func isCatalogStatement(statement string) bool {
	return strings.HasPrefix(statement, "WITH RECURSIVE s(name)")
}

// sent returns the statements recorded so far.
func (q *queryTrace) sent() []tracedStatement {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]tracedStatement(nil), q.statements...)
}

// reset forgets the statements recorded so far.
func (q *queryTrace) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.statements = nil
}

// last returns the text of the last statement sent.
func (q *queryTrace) last(t *testing.T) string {
	t.Helper()
	sent := q.sent()
	if len(sent) == 0 {
		t.Fatal("no statement was sent")
	}
	return sent[len(sent)-1].sql
}

// queryFixture is a Database over a traced handle whose search_path is one schema
// of its own.
type queryFixture struct {
	db     *Database
	sqlDB  *sql.DB // the handle under db: every statement it sends is traced
	admin  *sql.DB // an untraced handle for DDL and for a second session
	schema string
	trace  *queryTrace
}

// openQueryFixture creates schema (and extraSchemas) and runs ddl in the first,
// whose objects are then addressed unqualified through the search_path.
func openQueryFixture(t *testing.T, schema string, mode IdentifierMode, ddl []string, extraSchemas ...string) *queryFixture {
	t.Helper()
	dsn := testDSN(t)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.RuntimeParams["search_path"] = schema
	admin := stdlib.OpenDB(*cfg)

	trace := &queryTrace{}
	tracedCfg := cfg.Copy()
	tracedCfg.Tracer = trace
	sqlDB := stdlib.OpenDB(*tracedCfg)

	ctx := context.Background()
	for _, name := range append([]string{schema}, extraSchemas...) {
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+exactIdent(name)+` CASCADE`); err != nil {
			t.Fatalf("DROP SCHEMA: %v", err)
		}
		if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+exactIdent(name)); err != nil {
			t.Fatalf("CREATE SCHEMA: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
		for _, name := range append([]string{schema}, extraSchemas...) {
			_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+exactIdent(name)+` CASCADE`)
		}
		_ = admin.Close()
	})
	for _, statement := range ddl {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%v\nstatement: %s", err, statement)
		}
	}

	options := []Option{WithSchema(schema), WithIdentifierMode(mode)}
	resolved, err := resolveSettings(dalgo2sql.DbOptions{}, options)
	if err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	return &queryFixture{
		db:     newDatabaseFromSQL(sqlDB, dal.NewSchema(nil, nil), resolved, options),
		sqlDB:  sqlDB,
		admin:  admin,
		schema: schema,
		trace:  trace,
	}
}

// rows runs q and returns every record's data.
func (f *queryFixture) rows(t *testing.T, q dal.StructuredQuery) []map[string]any {
	t.Helper()
	reader, err := f.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v\nstatements sent: %v", err, f.trace.sent())
	}
	return readAll(t, reader)
}

// column picks one key of every row.
func column(rows []map[string]any, name string) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row[name])
	}
	return out
}

// albumDDL is the schema of the tests below, written in lower case as this
// package's DDL stores names. A table, a view, a materialized view, a partitioned
// table, a domain over a domain and NOT NULL columns.
var albumDDL = []string{
	`CREATE DOMAIN d_text AS text`,
	`CREATE DOMAIN d_code AS d_text CHECK (length(VALUE) <= 8)`,
	`CREATE TABLE artist (artistid integer PRIMARY KEY, name text NOT NULL, country text)`,
	`CREATE TABLE album (
		albumid  integer PRIMARY KEY,
		artistid integer NOT NULL REFERENCES artist (artistid),
		title    text NOT NULL,
		code     d_code,
		price    numeric(10,2),
		rating   double precision,
		released timestamptz,
		instock  boolean,
		cover    bytea,
		note     text
	)`,
	`INSERT INTO artist VALUES (1, 'Alice', 'UK'), (2, 'Bob', 'US'), (3, 'Cara', NULL)`,
	`INSERT INTO album (albumid, artistid, title, code, price, rating, released, instock, cover, note) VALUES
		(1, 1, 'First',  'A1', 10.50, 4.5, '2020-01-01T00:00:00Z', true,  '\x01', NULL),
		(2, 1, 'Second', 'A2', 20.00, 3.5, '2021-06-15T00:00:00Z', false, '\x02', 'good'),
		(3, 2, 'Third',  NULL, NULL,  NULL, NULL,                  NULL,  NULL,   'meh'),
		(4, 2, 'Fourth', 'B1', 5.25,  2.0, '2022-03-03T00:00:00Z', true,  '\x04', NULL),
		(5, 2, 'Dup',    'B2', 1.00,  1.0, '2019-09-09T00:00:00Z', true,  NULL,   'dup')`,
	`CREATE VIEW album_titles AS SELECT albumid, title FROM album`,
	`CREATE MATERIALIZED VIEW album_stats AS SELECT artistid, count(*) AS albums FROM album GROUP BY artistid`,
	`CREATE TABLE sales (saleid integer NOT NULL, albumid integer, qty integer) PARTITION BY RANGE (saleid)`,
	`CREATE TABLE sales_p1 PARTITION OF sales FOR VALUES FROM (0) TO (1000)`,
	`INSERT INTO sales (saleid, albumid, qty) VALUES (1, 1, 3), (2, 2, 1), (3, 1, 4)`,
}

func field(name string) dal.FieldRef { return dal.NewFieldRef("", name) }

func albumQuery() dal.IQueryBuilder {
	return albumFrom().NewQuery().OrderBy(dal.AscendingField("AlbumId"))
}

func titlesOf(rows []map[string]any) []any { return column(rows, "Title") }

// The catalog query (the recursive one over domains) and the suggestion query run
// on PostgreSQL 17 over a table, a view, a materialized view, a partitioned table,
// a domain over a domain and a NOT NULL column. Every one of them answers with its
// columns, in table order, so the statement the compiler writes lists them.
func TestStructuredQueryIntegration_CatalogOverEverySourceKind(t *testing.T) {
	f := openQueryFixture(t, "test_sq_catalog", IdentifierFoldLower, albumDDL)
	for _, tc := range []struct {
		name          string
		table         string
		exclude       string
		wantStatement string
		wantRows      int
	}{
		{"a table with a domain over a domain, NOT NULL columns and every type", "Album", "Cover",
			`SELECT "albumid", "artistid", "title", "code", "price", "rating", "released", "instock", "note" FROM "album"`, 5},
		{"a view", "Album_Titles", "Title", `SELECT "albumid" FROM "album_titles"`, 5},
		{"a materialized view", "Album_Stats", "Albums", `SELECT "artistid" FROM "album_stats"`, 2},
		{"a partitioned table", "Sales", "Qty", `SELECT "saleid", "albumid" FROM "sales"`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.trace.reset()
			q := dal.From(dal.NewRootCollectionRef(tc.table, "")).NewQuery().SelectColumns(dal.AllColumnsExcept(tc.exclude))
			if rows := f.rows(t, q); len(rows) != tc.wantRows {
				t.Errorf("rows = %d, want %d: %v", len(rows), tc.wantRows, rows)
			}
			if got := f.trace.last(t); got != tc.wantStatement {
				t.Errorf("statement =\n%s\nwant\n%s", got, tc.wantStatement)
			}
			// A plain select-all reads the same source.
			if rows := f.rows(t, dal.From(dal.NewRootCollectionRef(tc.table, "")).NewQuery().SelectColumns()); len(rows) != tc.wantRows {
				t.Errorf("select-all rows = %d, want %d", len(rows), tc.wantRows)
			}
		})
	}
	t.Run("a column of a domain over a domain is read and filtered", func(t *testing.T) {
		q := albumQuery().Where(dal.WhereField("Code", dal.Equal, "B1")).SelectColumns(titleColumn(), dal.Column{Expression: field("Code")})
		rows := f.rows(t, q)
		if want := []map[string]any{{"Title": "Fourth", "Code": "B1"}}; !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %v, want %v", rows, want)
		}
	})
}

// A filter of each constant type compares against a column of its type, the value
// travelling as a bound argument: a statement of every case has no value in its text,
// and the injection string is compared as text and leaves the table in place.
func TestStructuredQueryIntegration_FilterWithEachConstantType(t *testing.T) {
	f := openQueryFixture(t, "test_sq_filters", IdentifierFoldLower, albumDDL)
	for _, tc := range []struct {
		name       string
		condition  dal.Condition
		wantTitles []any
	}{
		{"an int", dal.WhereField("AlbumId", dal.GreaterOrEqual, 4), []any{"Fourth", "Dup"}},
		{"an int64", dal.WhereField("AlbumId", dal.Equal, int64(3)), []any{"Third"}},
		{"an unsigned integer", dal.WhereField("AlbumId", dal.Equal, uint(2)), []any{"Second"}},
		{"a float against a double precision column", dal.WhereField("Rating", dal.GreaterThen, 3.0), []any{"First", "Second"}},
		{"a float against a numeric column", dal.WhereField("Price", dal.Equal, 10.5), []any{"First"}},
		{"a string", dal.WhereField("Title", dal.Equal, "Third"), []any{"Third"}},
		{"a string against a domain over a domain", dal.WhereField("Code", dal.Equal, "A2"), []any{"Second"}},
		{"the injection string", dal.WhereField("Note", dal.Equal, hostile), []any{}},
		{"a bool", dal.WhereField("InStock", dal.Equal, true), []any{"First", "Fourth", "Dup"}},
		{"a time", dal.WhereField("Released", dal.GreaterThen, time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)), []any{"Second", "Fourth"}},
		{"bytes", dal.NewComparison(field("Cover"), dal.Equal, dal.Constant{Value: []byte{2}}), []any{"Second"}},
		{"nil is IS NULL", dal.WhereField("Note", dal.Equal, nil), []any{"First", "Fourth"}},
		{"isNull", dal.NewIsNullCondition(field("Note")), []any{"First", "Fourth"}},
		{"isNotNull", dal.NewIsNotNullCondition(field("Note")), []any{"Second", "Third", "Dup"}},
		{"IN", dal.WhereField("AlbumId", dal.In, []int{1, 3}), []any{"First", "Third"}},
		{"an empty IN is false", dal.WhereField("AlbumId", dal.In, []int{}), []any{}},
		{"an empty NOT IN is true", dal.WhereField("AlbumId", dal.NotIn, []int{}), []any{"First", "Second", "Third", "Fourth", "Dup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.trace.reset()
			rows := f.rows(t, albumQuery().Where(tc.condition).SelectColumns(titleColumn()))
			if got := titlesOf(rows); !reflect.DeepEqual(got, tc.wantTitles) && !(len(got) == 0 && len(tc.wantTitles) == 0) {
				t.Errorf("titles = %v, want %v", got, tc.wantTitles)
			}
			for _, sent := range f.trace.sent() {
				if strings.Contains(sent.sql, "O'Brien") || strings.Contains(sent.sql, "DROP TABLE") {
					t.Errorf("a value reached the statement text: %s", sent.sql)
				}
			}
		})
	}
	var exists bool
	if err := f.admin.QueryRowContext(context.Background(), `SELECT to_regclass('album') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Errorf("the album table is gone after the injection string: exists=%v err=%v", exists, err)
	}
}

// ORDER BY: NULLs sort first ascending and last descending, as in DALgo, with the
// NULLS clause written only for a column that can hold one; LIMIT and OFFSET are
// bound.
func TestStructuredQueryIntegration_OrderByLimitOffset(t *testing.T) {
	f := openQueryFixture(t, "test_sq_order", IdentifierFoldLower, albumDDL)
	order := func(orders ...dal.OrderExpression) dal.IQueryBuilder {
		return albumFrom().NewQuery().OrderBy(orders...)
	}
	t.Run("a NOT NULL column has no NULLS clause", func(t *testing.T) {
		f.trace.reset()
		rows := f.rows(t, order(dal.DescendingField("AlbumId")).SelectColumns(titleColumn()))
		if want := []any{"Dup", "Fourth", "Third", "Second", "First"}; !reflect.DeepEqual(titlesOf(rows), want) {
			t.Errorf("titles = %v, want %v", titlesOf(rows), want)
		}
		if got := f.trace.last(t); !strings.Contains(got, `ORDER BY "albumid" DESC`) || strings.Contains(got, "NULLS") {
			t.Errorf("statement = %s, want ORDER BY of the primary key with no NULLS clause", got)
		}
	})
	t.Run("a nullable column ascending: NULLs first", func(t *testing.T) {
		f.trace.reset()
		rows := f.rows(t, order(dal.AscendingField("Note"), dal.AscendingField("AlbumId")).SelectColumns(titleColumn()))
		if want := []any{"First", "Fourth", "Dup", "Second", "Third"}; !reflect.DeepEqual(titlesOf(rows), want) {
			t.Errorf("titles = %v, want %v", titlesOf(rows), want)
		}
		if got := f.trace.last(t); !strings.Contains(got, `"note" ASC NULLS FIRST`) || strings.Contains(got, `"albumid" ASC NULLS`) {
			t.Errorf("statement = %s, want NULLS FIRST on note only", got)
		}
	})
	t.Run("a nullable column descending: NULLs last", func(t *testing.T) {
		f.trace.reset()
		rows := f.rows(t, order(dal.DescendingField("Note"), dal.AscendingField("AlbumId")).SelectColumns(titleColumn()))
		if want := []any{"Third", "Second", "Dup", "First", "Fourth"}; !reflect.DeepEqual(titlesOf(rows), want) {
			t.Errorf("titles = %v, want %v", titlesOf(rows), want)
		}
		if got := f.trace.last(t); !strings.Contains(got, `"note" DESC NULLS LAST`) {
			t.Errorf("statement = %s, want NULLS LAST on note", got)
		}
	})
	t.Run("LIMIT and OFFSET are bound arguments", func(t *testing.T) {
		f.trace.reset()
		rows := f.rows(t, albumQuery().Limit(2).Offset(1).SelectColumns(titleColumn()))
		if want := []any{"Second", "Third"}; !reflect.DeepEqual(titlesOf(rows), want) {
			t.Errorf("titles = %v, want %v", titlesOf(rows), want)
		}
		if got := f.trace.last(t); !strings.Contains(got, "LIMIT $") || !strings.Contains(got, "OFFSET $") {
			t.Errorf("statement = %s, want LIMIT and OFFSET as bound arguments", got)
		}
	})
}

// GROUP BY with HAVING and every aggregate runs as one statement on the server;
// SUM and AVG come back as float64, COUNT, MIN and MAX as integers.
func TestStructuredQueryIntegration_GroupByHavingAndAggregates(t *testing.T) {
	f := openQueryFixture(t, "test_sq_groups", IdentifierFoldLower, albumDDL)
	columns := []dal.Column{
		{Expression: field("ArtistId")},
		dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"},
		dal.SumAs(field("Price"), "total"),
		dal.AverageAs(field("Rating"), "avgrating"),
		dal.MinAs(field("AlbumId"), "lo"),
		dal.MaxAs(field("AlbumId"), "hi"),
	}
	count := dal.NewAggregate(dal.COUNT, false, dal.Star())
	grouped := func() dal.IQueryBuilder {
		return albumFrom().NewQuery().GroupBy(field("ArtistId")).OrderBy(dal.AscendingField("ArtistId"))
	}
	artist1 := map[string]any{"ArtistId": int64(1), "n": int64(2), "total": 30.5, "avgrating": 4.0, "lo": int64(1), "hi": int64(2)}
	artist2 := map[string]any{"ArtistId": int64(2), "n": int64(3), "total": 6.25, "avgrating": 1.5, "lo": int64(3), "hi": int64(5)}

	f.trace.reset()
	rows := f.rows(t, grouped().SelectColumns(columns...))
	if want := []map[string]any{artist1, artist2}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	f.trace.reset()
	rows = f.rows(t, grouped().Having(dal.NewComparison(count, dal.GreaterThen, dal.NewConstant(2))).SelectColumns(columns...))
	if want := []map[string]any{artist2}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows with HAVING = %v, want %v", rows, want)
	}
	sent := f.trace.sent()
	if len(sent) != 2 || !strings.Contains(sent[1].sql, "GROUP BY") || !strings.Contains(sent[1].sql, "HAVING") {
		t.Errorf("statements = %v, want the catalog lookup and one statement that groups and filters groups on the server", sent)
	}
}

// A two-table join is one statement on the server.
func TestStructuredQueryIntegration_Join(t *testing.T) {
	f := openQueryFixture(t, "test_sq_join", IdentifierFoldLower, albumDDL)
	on := dal.NewComparison(dal.NewFieldRef("a", "ArtistId"), dal.Equal, dal.NewFieldRef("r", "ArtistId"))
	q := dal.From(dal.NewRootCollectionRef("Album", "a")).
		Join(dal.NewJoinedSource(dal.NewRootCollectionRef("Artist", "r"), dal.JoinInner, on)).
		NewQuery().
		Where(dal.NewComparison(dal.NewFieldRef("a", "AlbumId"), dal.LessThen, dal.NewConstant(5))).
		OrderBy(dal.Ascending(dal.NewFieldRef("a", "AlbumId"))).
		SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("a", "Title"), Alias: "title"},
			dal.Column{Expression: dal.NewFieldRef("r", "Name"), Alias: "artist"},
		)
	f.trace.reset()
	rows := f.rows(t, q)
	want := []map[string]any{
		{"title": "First", "artist": "Alice"}, {"title": "Second", "artist": "Alice"},
		{"title": "Third", "artist": "Bob"}, {"title": "Fourth", "artist": "Bob"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	var joined int
	for _, sent := range f.trace.sent() {
		if !isCatalogStatement(sent.sql) && strings.Contains(sent.sql, " JOIN ") {
			joined++
		}
	}
	if joined != 1 {
		t.Errorf("%d statements join on the server, want exactly one: %v", joined, f.trace.sent())
	}
}

// Arithmetic reads both operands as double precision: 3 / 2 is 1.5, not 1, and a
// division by zero is NULL, not an error; a NULL operand gives NULL.
func TestStructuredQueryIntegration_ArithmeticWithTheCast(t *testing.T) {
	f := openQueryFixture(t, "test_sq_arithmetic", IdentifierFoldLower, albumDDL)
	columns := []dal.Column{
		{Expression: dal.Binary(field("Price"), dal.Multiply, dal.NewConstant(2)), Alias: "double"},
		{Expression: dal.Binary(field("AlbumId"), dal.Divide, dal.NewConstant(2)), Alias: "half"},
		{Expression: dal.Binary(field("AlbumId"), dal.Divide, dal.NewConstant(0)), Alias: "zero"},
	}
	for _, tc := range []struct {
		albumID int
		want    map[string]any
	}{
		{1, map[string]any{"double": 21.0, "half": 0.5, "zero": nil}},
		{3, map[string]any{"double": nil, "half": 1.5, "zero": nil}},
	} {
		rows := f.rows(t, albumFrom().NewQuery().Where(dal.WhereField("AlbumId", dal.Equal, tc.albumID)).SelectColumns(columns...))
		if want := []map[string]any{tc.want}; !reflect.DeepEqual(rows, want) {
			t.Errorf("album %d: rows = %v, want %v", tc.albumID, rows, want)
		}
	}
}

// DISTINCT: COUNT(DISTINCT x) is counted by the server too, in the same statement
// as the other aggregates (see the group test above for the plain forms).
func TestStructuredQueryIntegration_CountDistinct(t *testing.T) {
	f := openQueryFixture(t, "test_sq_distinct", IdentifierFoldLower, albumDDL)
	f.trace.reset()
	rows := f.rows(t, albumFrom().NewQuery().GroupBy(field("ArtistId")).OrderBy(dal.AscendingField("ArtistId")).
		SelectColumns(dal.Column{Expression: field("ArtistId")}, dal.CountDistinctAs(field("Code"), "codes"), dal.SumDistinctAs(field("AlbumId"), "ids")))
	want := []map[string]any{
		{"ArtistId": int64(1), "codes": int64(2), "ids": 3.0},
		{"ArtistId": int64(2), "codes": int64(2), "ids": 12.0},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if got := f.trace.last(t); !strings.Contains(got, "COUNT(DISTINCT") || !strings.Contains(got, "SUM(DISTINCT") {
		t.Errorf("statement = %s, want the DISTINCT aggregates written for the server", got)
	}
}

// A join runs on the server when each ON pair has the same type category (numbers
// with numbers) and is left to DALgo's own engine when the types differ (text with
// an integer): the plan DALgo makes from the database's answer.
func TestStructuredQueryIntegration_PlansJoinsOnTheServerOrInDALgo(t *testing.T) {
	f := openQueryFixture(t, "test_sq_plans", IdentifierFoldLower, albumDDL)
	joiner, ok := dal.As[dal.NativeJoinProvider](f.db.DB)
	if !ok {
		t.Fatal("the backend declares no native join provider")
	}
	joinQuery := func(left, right string) dal.StructuredQuery {
		on := dal.NewComparison(dal.NewFieldRef("a", left), dal.Equal, dal.NewFieldRef("r", right))
		return dal.From(dal.NewRootCollectionRef("Album", "a")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef("Artist", "r"), dal.JoinInner, on)).
			NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("r", "Name"), Alias: "artist"})
	}
	for _, tc := range []struct {
		name        string
		left, right string
		want        dal.JoinStrategy
	}{
		{"integer to integer", "ArtistId", "ArtistId", dal.JoinNative},
		{"numeric to integer: one category", "Price", "ArtistId", dal.JoinNative},
		{"text to integer: declined", "Title", "ArtistId", dal.JoinGeneric},
		{"boolean to integer: declined", "InStock", "ArtistId", dal.JoinGeneric},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := dal.PlanJoin(context.Background(), joinQuery(tc.left, tc.right), joiner)
			if err != nil || plan.Strategy != tc.want {
				t.Errorf("plan = %+v, err = %v; want %s", plan, err, tc.want)
			}
		})
	}
}

// A query with a subquery is run by DALgo's own engine over plain reads of its
// sources: no statement the server receives holds the subquery.
//
// DALgo's engine reads each source as the catalog names its columns and matches the
// names a query spells against them exactly, so in the mode that folds names such a
// query must spell them as the catalog stores them, in lower case here. (The first run
// of this test spelled them as Title and AlbumId and DALgo answered
// `field "Title" is unavailable in "a"`; the native path accepts any case.)
func TestStructuredQueryIntegration_SubqueryRunsInDALgoOverPlainReads(t *testing.T) {
	f := openQueryFixture(t, "test_sq_subquery", IdentifierFoldLower, albumDDL)
	alice := dal.From(dal.NewRootCollectionRef("artist", "r")).NewQuery().
		Where(
			dal.NewComparison(dal.NewFieldRef("r", "artistid"), dal.Equal, dal.NewFieldRef("a", "artistid")),
			dal.NewComparison(dal.NewFieldRef("r", "name"), dal.Equal, dal.NewConstant("Alice")),
		).
		SelectColumns(dal.Column{Expression: dal.NewFieldRef("r", "name")})
	q := dal.From(dal.NewRootCollectionRef("album", "a")).NewQuery().
		Where(dal.NewExistsCondition(alice)).
		OrderBy(dal.Ascending(dal.NewFieldRef("a", "albumid"))).
		SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "title"), Alias: "Title"})
	f.trace.reset()
	rows := f.rows(t, q)
	if want := []any{"First", "Second"}; !reflect.DeepEqual(titlesOf(rows), want) {
		t.Errorf("titles = %v, want %v", titlesOf(rows), want)
	}
	for _, sent := range f.trace.sent() {
		if !isCatalogStatement(sent.sql) && (strings.Contains(strings.ToUpper(sent.sql), "EXISTS") || strings.Contains(sent.sql, " JOIN ")) {
			t.Errorf("a statement carries the subquery: %s", sent.sql)
		}
	}
}

// A constant of a type the column does not have is a server error, not an empty
// result: a number against a text column is refused by the server (SQLSTATE 42883,
// no such operator), and the connection goes back to the pool.
func TestStructuredQueryIntegration_ConstantOfAnotherTypeThanItsColumnIsAServerError(t *testing.T) {
	f := openQueryFixture(t, "test_sq_mismatch", IdentifierFoldLower, albumDDL)
	_, err := f.db.ExecuteQueryToRecordsReader(context.Background(),
		albumQuery().Where(dal.WhereField("Title", dal.Equal, 42)).SelectColumns(titleColumn()))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42883" {
		t.Errorf("error = %v, want the server's 42883 (operator does not exist: text = bigint)", err)
	}
	if got := f.sqlDB.Stats().InUse; got != 0 {
		t.Errorf("connections in use after the refused statement = %d, want 0", got)
	}
}

// A select-all with an exclusion lists the columns of the table, in table order,
// minus the excluded ones in any case, in the mode that folds names.
func TestStructuredQueryIntegration_SelectAllWithAnExclusionFoldLower(t *testing.T) {
	f := openQueryFixture(t, "test_sq_exclusion", IdentifierFoldLower, albumDDL)
	f.trace.reset()
	q := albumFrom().NewQuery().Where(dal.WhereField("AlbumId", dal.Equal, 1)).SelectColumns(dal.AllColumnsExcept("Note", "COVER"))
	rows := f.rows(t, q)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", rows)
	}
	row := rows[0]
	released, _ := row["released"].(time.Time)
	if !released.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("released = %v", row["released"])
	}
	delete(row, "released")
	want := map[string]any{"albumid": int64(1), "artistid": int64(1), "title": "First", "code": "A1", "price": 10.5, "rating": 4.5, "instock": true}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("row = %v, want %v (the excluded columns absent)", row, want)
	}
	if got := f.trace.last(t); strings.Contains(got, "*") || strings.Contains(got, `"note"`) || strings.Contains(got, `"cover"`) {
		t.Errorf("statement = %s, want the columns listed without the excluded ones", got)
	}
}

// A table the database does not have is a table-not-found error that names the
// nearest table: the suggestion query ran on the server, in the mode that folds
// names and in the exact mode, where it says names are case-sensitive.
func TestStructuredQueryIntegration_UnknownTableIsNotFoundWithASuggestion(t *testing.T) {
	testDSN(t) // a skip shows on this test, not only on its subtests
	notFound := func(t *testing.T, f *queryFixture, name string) (*dalgo2sql.TableNotFoundError, error) {
		t.Helper()
		f.trace.reset()
		_, err := f.db.ExecuteQueryToRecordsReader(context.Background(), dal.From(dal.NewRootCollectionRef(name, "")).NewQuery().SelectColumns())
		var target *dalgo2sql.TableNotFoundError
		if !errors.Is(err, dalgo2sql.ErrTableNotFound) || !errors.As(err, &target) {
			t.Fatalf("error = %v, want a table-not-found error", err)
		}
		for _, sent := range f.trace.sent() {
			if strings.HasPrefix(sent.sql, "SELECT ") && strings.Contains(sent.sql, " FROM \"") {
				t.Errorf("a statement was sent for a table that is not there: %s", sent.sql)
			}
		}
		return target, err
	}
	t.Run("fold-lower", func(t *testing.T) {
		f := openQueryFixture(t, "test_sq_notfound", IdentifierFoldLower, albumDDL)
		target, err := notFound(t, f, "Albums")
		if target.Name != "Albums" || target.SuggestedName != "album" || target.CaseSensitive {
			t.Errorf("error = %+v, want Albums not found, suggesting album, not case-sensitive", *target)
		}
		if want := `failed to get SQL reader: table "Albums" not found; did you mean "album"?`; err.Error() != want {
			t.Errorf("message = %q, want %q", err, want)
		}
	})
	t.Run("exact", func(t *testing.T) {
		f := openQueryFixture(t, "test_sq_notfound_exact", IdentifierExact, []string{
			`CREATE TABLE "Album" ("AlbumId" integer PRIMARY KEY, "Title" text NOT NULL)`,
		})
		target, err := notFound(t, f, "album")
		if target.SuggestedName != "Album" || !target.CaseSensitive {
			t.Errorf("error = %+v, want album not found, suggesting Album, case-sensitive", *target)
		}
		if want := `failed to get SQL reader: table "album" not found; did you mean "Album"? Table names are case-sensitive.`; err.Error() != want {
			t.Errorf("message = %q, want %q", err, want)
		}
	})
}

// The exact mode reads a database whose tables were created with mixed-case names,
// and a collection reference that names its schema reads that schema.
func TestStructuredQueryIntegration_ExactModeAndQualifiedSchema(t *testing.T) {
	const schema, other = "test_sq_exact", "test_sq_exact_other"
	f := openQueryFixture(t, schema, IdentifierExact, []string{
		`CREATE TABLE "Album" ("AlbumId" integer PRIMARY KEY, "Title" text NOT NULL)`,
		`INSERT INTO "Album" VALUES (1, 'First'), (2, 'Second')`,
		`CREATE TABLE ` + exactIdent(other) + `."Album" ("AlbumId" integer PRIMARY KEY, "Title" text NOT NULL)`,
		`INSERT INTO ` + exactIdent(other) + `."Album" VALUES (1, 'Other')`,
	}, other)
	title := dal.Column{Expression: field("Title")}

	f.trace.reset()
	rows := f.rows(t, albumFrom().NewQuery().Where(dal.WhereField("AlbumId", dal.Equal, 2)).SelectColumns(title))
	if want := []map[string]any{{"Title": "Second"}}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if got := f.trace.last(t); !strings.Contains(got, `FROM "Album"`) || !strings.Contains(got, `"AlbumId"`) {
		t.Errorf("statement = %s, want the names written as the query spells them", got)
	}
	qualified := dal.From(dal.NewQualifiedRootCollectionRef(other, "Album", "")).NewQuery().SelectColumns(title)
	if rows := f.rows(t, qualified); !reflect.DeepEqual(rows, []map[string]any{{"Title": "Other"}}) {
		t.Errorf("qualified rows = %v, want the other schema's row", rows)
	}
	// The exact mode keeps the case of every spelling: albumid is not AlbumId.
	if _, err := f.db.ExecuteQueryToRecordsReader(context.Background(),
		albumFrom().NewQuery().Where(dal.WhereField("albumid", dal.Equal, 2)).SelectColumns(title)); err == nil {
		t.Error("a column spelled in another case was accepted in the exact mode")
	}
}

// With pgx under it a structured read takes one connection for its catalog lookup
// and its statement, keeps it while its reader is open, and gives it back when the
// reader is closed or read to its end, even when the caller never reads a row.
func TestStructuredQueryIntegration_ReadsRunOnOneLeasedConnection(t *testing.T) {
	f := openQueryFixture(t, "test_sq_lease", IdentifierFoldLower, albumDDL)
	f.sqlDB.SetMaxOpenConns(2)
	query := func() dal.StructuredQuery { return albumQuery().SelectColumns(titleColumn()) }
	ctx := context.Background()

	t.Run("the two statements run on one backend process, held until Close", func(t *testing.T) {
		f.trace.reset()
		reader, err := f.db.ExecuteQueryToRecordsReader(ctx, query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		sent := f.trace.sent()
		if len(sent) != 2 {
			t.Fatalf("statements = %v, want the catalog lookup and the statement", sent)
		}
		if sent[0].pid == 0 || sent[0].pid != sent[1].pid {
			t.Errorf("the catalog lookup ran on backend %d and the statement on %d, want one connection", sent[0].pid, sent[1].pid)
		}
		if got := f.sqlDB.Stats().InUse; got != 1 {
			t.Errorf("connections in use while the reader is open = %d, want 1", got)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := f.sqlDB.Stats().InUse; got != 0 {
			t.Errorf("connections in use after Close = %d, want 0", got)
		}
	})
	t.Run("given back at the end of the rows, with no Close", func(t *testing.T) {
		reader, err := f.db.ExecuteQueryToRecordsReader(ctx, query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		for {
			if _, err := reader.Next(); err != nil {
				if !errors.Is(err, dal.ErrNoMoreRecords) {
					t.Fatalf("Next: %v", err)
				}
				break
			}
		}
		if got := f.sqlDB.Stats().InUse; got != 0 {
			t.Errorf("connections in use at the end of the rows = %d, want 0", got)
		}
		_ = reader.Close()
	})
	t.Run("a pool of one connection serves a read: both statements share it", func(t *testing.T) {
		f.sqlDB.SetMaxOpenConns(1)
		defer f.sqlDB.SetMaxOpenConns(2)
		bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		reader, err := f.db.ExecuteQueryToRecordsReader(bounded, query())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader on a pool of one: %v", err)
		}
		// The next read has to wait for the connection the open reader holds: with the
		// pool exhausted it ends with its own context, not with a result.
		waiting, stopWaiting := context.WithTimeout(ctx, 300*time.Millisecond)
		defer stopWaiting()
		if _, err := f.db.ExecuteQueryToRecordsReader(waiting, query()); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a second read while the first reader is open: error = %v, want context.DeadlineExceeded", err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		again, err := f.db.ExecuteQueryToRecordsReader(bounded, query())
		if err != nil {
			t.Fatalf("a read after the reader was closed: %v", err)
		}
		_ = again.Close()
	})
}

// A context that ends before the catalog lookup, between it and the statement, or
// while the statement waits on the server returns the connection to the pool.
//
// What the caller sees differs. While the statement waits, it is the context's own
// error (context.DeadlineExceeded here). Before the statement is sent, the lease gives
// the connection back the moment the context ends, so the read can fail on the closed
// connection before the driver looks at the context: the caller sees the pool's
// `sql: connection is already closed` or the driver's `driver: bad connection`, or the
// context's error when the driver is first. Which of them depends on timing: CI saw
// `driver: bad connection` at the start of the catalog lookup, and between the two
// statements `sql: connection is already closed` on one run and `driver: bad connection`
// on the next. A caller must check its own context (ctx.Err()) rather than rely on
// errors.Is(err, ctx.Err()).
func TestStructuredQueryIntegration_ContextEndsBetweenAndDuringTheStatements(t *testing.T) {
	testDSN(t) // a skip shows on this test, not only on its subtests
	query := func() dal.StructuredQuery { return albumQuery().SelectColumns(titleColumn()) }
	endedReadError := func(err error) bool {
		return errors.Is(err, context.Canceled) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn)
	}
	assertPoolIsFree := func(t *testing.T, f *queryFixture) {
		t.Helper()
		if got := f.sqlDB.Stats().InUse; got != 0 {
			t.Errorf("connections in use after the failed read = %d, want 0", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := f.sqlDB.PingContext(ctx); err != nil {
			t.Errorf("the pool cannot serve a connection after the failed read: %v", err)
		}
	}

	t.Run("at the start of the catalog lookup", func(t *testing.T) {
		f := openQueryFixture(t, "test_sq_ctx_start", IdentifierFoldLower, albumDDL)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.trace.onStart(func(statement string) {
			if isCatalogStatement(statement) {
				cancel()
			}
		})
		_, err := f.db.ExecuteQueryToRecordsReader(ctx, query())
		if !endedReadError(err) {
			t.Errorf("error = %v, want the context's or the closed connection's", err)
		}
		t.Logf("the caller saw: %v", err)
		assertPoolIsFree(t, f)
	})
	t.Run("between the catalog lookup and the statement", func(t *testing.T) {
		f := openQueryFixture(t, "test_sq_ctx_between", IdentifierFoldLower, albumDDL)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.trace.onEnd(func(statement string) {
			if isCatalogStatement(statement) {
				cancel()
			}
		})
		_, err := f.db.ExecuteQueryToRecordsReader(ctx, query())
		if !endedReadError(err) {
			t.Errorf("error = %v, want the context's or the closed connection's", err)
		}
		// The driver is still handed the statement (the trace shows it) and fails on the
		// connection the lease has given back.
		t.Logf("the caller saw: %v", err)
		assertPoolIsFree(t, f)
	})
	t.Run("while the statement waits on the server", func(t *testing.T) {
		f := openQueryFixture(t, "test_sq_ctx_during", IdentifierFoldLower, albumDDL)
		// A second session holds the table: the catalog lookup does not touch it, the
		// statement waits for the lock.
		holder, err := f.admin.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		defer func() { _ = holder.Rollback() }()
		if _, err := holder.ExecContext(context.Background(), `LOCK TABLE album IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatalf("LOCK TABLE: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err = f.db.ExecuteQueryToRecordsReader(ctx, query())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want one matching context.DeadlineExceeded", err)
		}
		sent := f.trace.sent()
		if len(sent) != 2 || !isCatalogStatement(sent[0].sql) || isCatalogStatement(sent[1].sql) {
			t.Errorf("statements = %v, want the deadline to end the read while the second statement waited", sent)
		}
		assertPoolIsFree(t, f)
	})
}
