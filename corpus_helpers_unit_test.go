package dalgo2postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
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
