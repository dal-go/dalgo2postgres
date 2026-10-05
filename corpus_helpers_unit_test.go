package dalgo2postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5/pgconn"
)

// These tests need no server: they test the helpers of the corpus, pushdown, access and probe
// tests, so that a check that never fails is noticed here and not only on a server.

func TestCellOf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		sqlType string
		want    any
		wantErr bool
	}{
		{"a NULL", nil, "integer", nil, false},
		{"a whole number for an integer", json.Number("42"), "integer", int64(42), false},
		{"a number for a double precision", json.Number("1.5"), "double precision", 1.5, false},
		{"a number for a text column", json.Number("1"), `text COLLATE "C"`, nil, true},
		{"a fraction for an integer", json.Number("1.5"), "integer", nil, true},
		{"a text", "x", `text COLLATE "C"`, "x", false},
	} {
		got, err := cellOf(tc.value, tc.sqlType)
		if (err != nil) != tc.wantErr || (!tc.wantErr && got != tc.want) {
			t.Errorf("%s: cellOf = %v, %v; want %v (error %v)", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestDiagnosticOfAndMatchesDiagnostic(t *testing.T) {
	query := fmt.Errorf("wrapped: %w", &dal.QueryValidationError{Category: "scope", Path: "columns[0]", Message: "m"})
	join := &dal.JoinValidationError{Category: "join_scope", Path: "from.joins[0]", Message: "j"}
	if got, ok := diagnosticOf(query); !ok || got.Category != "scope" || got.Path != "columns[0]" || got.Message != "m" {
		t.Errorf("diagnosticOf(query) = %+v, %v", got, ok)
	}
	if got, ok := diagnosticOf(join); !ok || got.Category != "join_scope" {
		t.Errorf("diagnosticOf(join) = %+v, %v", got, ok)
	}
	if _, ok := diagnosticOf(errors.New("plain")); ok {
		t.Error("a plain error is no diagnostic")
	}
	if !matchesDiagnostic(query, fixtureDiagnostic{Category: "scope", Path: "columns[0]"}) {
		t.Error("the diagnostic does not match its own category and path")
	}
	for _, want := range []fixtureDiagnostic{{Category: "shape", Path: "columns[0]"}, {Category: "scope", Path: "columns[1]"}} {
		if matchesDiagnostic(query, want) {
			t.Errorf("the diagnostic matches %+v, want a mismatch", want)
		}
	}
	if matchesDiagnostic(nil, fixtureDiagnostic{}) {
		t.Error("no error matches a diagnostic")
	}
}

func TestStatementHelpers(t *testing.T) {
	if got := countJoins(`SELECT * FROM "a" INNER JOIN "b" ON 1 LEFT JOIN "c" ON 1`); got != 2 {
		t.Errorf("countJoins = %d, want 2", got)
	}
	for statement, want := range map[string]bool{
		`SELECT * FROM "Customer" AS "c"`:                         false,
		`SELECT * FROM "a" INNER JOIN "b" ON ("a"."x" = "b"."x")`: true,
		`SELECT * FROM "a" WHERE "x" IN (SELECT "y" FROM "b")`:    true,
		`select 1 where exists (select 1)`:                        true,
	} {
		if got := notALeafRead(statement); got != want {
			t.Errorf("notALeafRead(%q) = %v, want %v", statement, got, want)
		}
	}
}

func TestNamesInAndContainsArgument(t *testing.T) {
	rows := []map[string]any{{"n": "b"}, {"n": "a"}, {"n": 3}}
	if got := namesIn(rows, "n"); strings.Join(got, ",") != "3,a,b" {
		t.Errorf("namesIn = %v, want the values sorted", got)
	}
	if !containsArgument([]any{1, "x", []any{"deep"}, []string{"listed"}}, "deep") || !containsArgument([]any{[]string{"listed"}}, "listed") ||
		!containsArgument([]any{"x"}, "x") {
		t.Error("an argument that is there was not found")
	}
	if containsArgument([]any{1, "x", []any{"deep"}, []string{"listed"}}, "absent") {
		t.Error("an argument that is not there was found")
	}
}

func TestClassifyProbe(t *testing.T) {
	server := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "22021"})
	for name, tc := range map[string]struct {
		run  adapterRun
		want string
	}{
		"rows":                   {adapterRun{rows: []map[string]any{{"id": 1}}}, "rows"},
		"an empty result":        {adapterRun{}, "an empty result"},
		"the server's refusal":   {adapterRun{err: server, statements: []string{"SELECT 1"}}, "the server's refusal, SQLSTATE 22021"},
		"an error before a read": {adapterRun{err: errors.New("x")}, "an error built before any statement"},
		"an error after a read":  {adapterRun{err: errors.New("x"), statements: []string{"SELECT 1"}}, "an error built by DALgo's engine after plain reads"},
	} {
		if got := classifyProbe(tc.run); got != tc.want {
			t.Errorf("%s: classifyProbe = %q, want %q", name, got, tc.want)
		}
	}
}

func TestProbeWorldNames(t *testing.T) {
	exact, fold := probeWorld{mode: IdentifierExact}, probeWorld{mode: IdentifierFoldLower}
	if exact.stored("Naïve") != "Naïve" || fold.stored("Naïve") != "naïve" {
		t.Errorf("stored = %q and %q", exact.stored("Naïve"), fold.stored("Naïve"))
	}
	if got := fold.quotedIn(`A"B`); got != `"a""b"` {
		t.Errorf("quotedIn = %s, want a quoted, lower-cased name with the quote doubled", got)
	}
	// Every probe that can be a name is one PostgreSQL holds (63 bytes, no NUL), and the two
	// that cannot are the NUL byte and the 64 bytes.
	for _, probe := range probes {
		holds := len(probe.text) <= 63 && !strings.ContainsRune(probe.text, 0)
		if probe.name != holds {
			t.Errorf("probe %q: name = %v, want %v", probe.label, probe.name, holds)
		}
	}
}

func TestExpectedGroupsAndJoinRows(t *testing.T) {
	// 200,000 rows in 150,000 groups; a group of one row whose v is 1 fails the HAVING
	// (SUM(v) > 1): the ids from 50,001 to 149,999 that are multiples of 7, 14,286 of them.
	groups := expectedGroups(factRows)
	if want := factModulus - 14286; len(groups) != want {
		t.Errorf("groups = %d, want %d", len(groups), want)
	}
	if len(groups) <= engineMaxGroups {
		t.Errorf("the expected answer has %d groups, not past the engine's %d", len(groups), engineMaxGroups)
	}
	var rows int64
	for _, g := range groups {
		rows += g["n"].(int64)
	}
	if want := int64(factRows - 14286); rows != want {
		t.Errorf("rows in the groups = %d, want %d", rows, want)
	}
	if joinRows*noteLength <= engineMaxJoinBytes {
		t.Errorf("the join's text is %d bytes, not past the engine's %d", joinRows*noteLength, engineMaxJoinBytes)
	}
	good := []map[string]any{{"order_id": int64(1), "customer": "customer 1", "note": strings.Repeat("x", noteLength)}}
	if got := checkJoinRows(t, good, 1); got != noteLength {
		t.Errorf("checkJoinRows = %d bytes, want %d", got, noteLength)
	}
	if got := subqueryDDL(); len(got) != len(subqueryTables) || !strings.Contains(got[0], `CREATE TABLE "Customer" ("CustomerId" integer, "FirstName" text COLLATE "C", "Country" text COLLATE "C", PRIMARY KEY ("CustomerId"))`) {
		t.Errorf("subqueryDDL = %v", got)
	}
	if got := pushdownDDL(); len(got) != 12 || !strings.Contains(got[0], "generate_series(1, 200000)") {
		t.Errorf("pushdownDDL = %v", got)
	}
}

func TestCompareJoinRun(t *testing.T) {
	want := []map[string]any{{"id": 1.0, "name": "Ada"}}
	one := []string{`SELECT * FROM "a" INNER JOIN "b" ON 1`}
	for _, tc := range []struct {
		name     string
		run      adapterRun
		joins    int
		observed []string
	}{
		{"as the fixture says", adapterRun{statements: one, rows: []map[string]any{{"id": int64(1), "name": "Ada"}}}, 1, nil},
		{"a failed read", adapterRun{err: errors.New("boom")}, 1, []string{differsInError}},
		{"two statements", adapterRun{statements: append(one, one[0]), rows: []map[string]any{{"id": int64(1), "name": "Ada"}}}, 1, []string{differsInStatements}},
		{"another number of JOINs", adapterRun{statements: one, rows: []map[string]any{{"id": int64(1), "name": "Ada"}}}, 2, []string{differsInStatements}},
		{"other rows", adapterRun{statements: one, rows: []map[string]any{{"id": int64(2), "name": "Ada"}}}, 1, []string{differsInRows}},
		{"other statements and other rows", adapterRun{rows: nil}, 1, []string{differsInStatements, differsInRows}},
	} {
		observed, notes, err := compareJoinRun(tc.run, want, tc.joins)
		if err != nil || !reflect.DeepEqual(observed, tc.observed) || len(notes) != len(tc.observed) {
			t.Errorf("%s: observed = %v, notes = %v, error %v; want %v", tc.name, observed, notes, err, tc.observed)
		}
	}
	if _, _, err := compareJoinRun(adapterRun{statements: one, rows: []map[string]any{{"bad": make(chan int)}}}, want, 1); err == nil {
		t.Error("rows that JSON cannot hold were compared, want the error")
	}
}

func TestCorrelatedCostProblems(t *testing.T) {
	outer := `SELECT * FROM "Truth" AS "t"`
	inner := `SELECT * FROM "Membership" AS "m"`
	if problems := correlatedCostProblems("case", "Membership", 2, []string{outer, inner, inner}); len(problems) != 0 {
		t.Errorf("problems = %q, want none for one outer read and one inner read per outer row", problems)
	}
	wantProblem(t, correlatedCostProblems("case", "Membership", 2, []string{outer, inner}), "sent 2 statements, want 3")
	wantProblem(t, correlatedCostProblems("case", "Membership", 1, []string{outer, `SELECT * FROM "Other" AS "m"`}), "want an unfiltered read of every column of Membership")
	wantProblem(t, correlatedCostProblems("case", "Membership", 1, []string{outer, inner + ` WHERE "m"."SetName" = $1`}), "want an unfiltered read")
}

// The recordset reader's rows are read into maps by column name, as the records reader's are.
func TestReadRecordsetRows(t *testing.T) {
	open := func(t *testing.T, rows *sqlmock.Rows) dal.RecordsetReader {
		t.Helper()
		sqlDB, mock := newStructuredMock(t)
		db := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		mock.ExpectQuery(catalogStatement).WithArgs(`"album"`).WillReturnRows(albumCatalog(`"album"`))
		mock.ExpectQuery(`SELECT "albumid", "title" FROM "album"`).WillReturnRows(rows)
		reader, err := db.ExecuteQueryToRecordsetReader(context.Background(),
			albumFrom().NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "AlbumId")}, titleColumn()))
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
		}
		t.Cleanup(func() { _ = reader.Close() })
		return reader
	}
	typed := func() *sqlmock.Rows {
		return sqlmock.NewRowsWithColumnDefinition(
			sqlmock.NewColumn("albumid").OfType("INT4", int64(0)),
			sqlmock.NewColumn("title").OfType("TEXT", ""))
	}
	t.Run("every row, by the name the query asked for, and a NULL as nil", func(t *testing.T) {
		rows, err := readRecordsetRows(open(t, typed().AddRow(int64(1), "First").AddRow(int64(2), nil)))
		want := []map[string]any{{"AlbumId": int64(1), "Title": "First"}, {"AlbumId": int64(2), "Title": nil}}
		if err != nil || !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %v, error %v; want %v", rows, err, want)
		}
	})
	t.Run("no row is no rows", func(t *testing.T) {
		rows, err := readRecordsetRows(open(t, typed()))
		if err != nil || len(rows) != 0 {
			t.Errorf("rows = %v, error %v; want none", rows, err)
		}
	})
	t.Run("an error in the middle of the rows keeps the rows before it and is the reader's", func(t *testing.T) {
		boom := errors.New("boom")
		rows, err := readRecordsetRows(open(t, typed().AddRow(int64(1), "First").AddRow(int64(2), "Second").RowError(1, boom)))
		if !errors.Is(err, boom) || len(rows) != 1 || rows[0]["Title"] != "First" {
			t.Errorf("rows = %v, error %v; want the first row and the reader's error", rows, err)
		}
	})
}

func TestPlainNamesAndKeyPathProblems(t *testing.T) {
	for text, want := range map[string]bool{
		"canary": true, "_x9": true, "Mixed_Case": true, strings.Repeat("n", 64): true, strings.Repeat("n", 255): true,
		strings.Repeat("n", 256): false, "9lives": false, "": false, "a b": false, `a"b`: false, "naïve": false, "a\x00b": false, "a;b": false,
	} {
		if got := isPlainName(text); got != want {
			t.Errorf("isPlainName(%q) = %v, want %v", text, got, want)
		}
	}
	// Of the probes, only the name of 64 bytes is a plain name: it is what the key paths pass to the server.
	for _, probe := range probes {
		if want := probe.label == "a name of 64 bytes"; isPlainName(probe.text) != want {
			t.Errorf("probe %q: isPlainName = %v, want %v", probe.label, isPlainName(probe.text), want)
		}
	}
	clean := []tracedStatement{{sql: "SELECT 1 FROM probe_keys WHERE code = $1"}}
	if problems := keyPathProblems("l", "x'; DROP", clean, false); len(problems) != 0 {
		t.Errorf("problems = %q, want none", problems)
	}
	wantProblem(t, keyPathProblems("l", "x", []tracedStatement{{sql: "SELECT 1; SELECT 2"}}, false), `holds ";"`)
	wantProblem(t, keyPathProblems("l", "x", []tracedStatement{{sql: `SELECT "a" FROM t`}}, false), `holds "\""`)
	text := strings.Repeat("n", 64)
	named := []tracedStatement{{sql: "SELECT 1 FROM " + text + " WHERE code = $1"}}
	wantProblem(t, keyPathProblems("l", text, named, false), "holds the text")
	if problems := keyPathProblems("l", text, named, true); len(problems) != 0 {
		t.Errorf("problems = %q, want none for a plain name the statement may write", problems)
	}
	if problems := keyPathProblems("l", "abc", []tracedStatement{{sql: "SELECT abc"}}, false); len(problems) != 0 {
		t.Errorf("problems = %q, want none: a text of four bytes or fewer is not looked for", problems)
	}
}
