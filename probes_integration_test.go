package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/jackc/pgx/v5/pgconn"
)

// Probes: nothing a caller writes becomes SQL. A canary table, and queries in which the
// caller's text stands where a value, a field name, a collection name, an alias, an ORDER BY
// field, a GROUP BY field and a join key go, each in the exact and the fold-lower mode. The text
// is a quote, a double quote, doubled quotes, a semicolon with a second statement that would
// drop the canary, comment markers, a NUL byte, a name of 64 bytes (one more than a PostgreSQL
// identifier holds) and a non-ASCII name.
//
// The answer to every probe is rows, an empty result or an error the adapter builds before it
// sends anything; the one exception is a NUL byte in a value, which the server itself refuses
// as a data exception. After all of them the canary table exists with its rows unchanged, and
// the statement recorder shows the rest: outside a quoted identifier no statement holds a
// semicolon, a comment marker or a quote, every string value reached the server as a bound
// argument, and the statement of a value does not change with the value.
//
// They run against PostgreSQL and skip unless DALGO2POSTGRES_TEST_DSN is set; CI fails when a
// TestProbesIntegration_ test skips or is missing.

// probes are the texts a caller can write. name says whether PostgreSQL can hold the text as
// an identifier: not with a NUL byte, and not with more than 63 bytes.
var probes = []struct {
	label string
	text  string
	name  bool
}{
	{"a quote", `'`, true},
	{"a double quote", `"`, true},
	{"doubled quotes", `''`, true},
	{"doubled double quotes", `""`, true},
	{"a quote that ends a string, a semicolon and a second statement", `x'; DROP TABLE canary; --`, true},
	{"a double quote that ends an identifier, a semicolon and a second statement", `x"; DROP TABLE canary; --`, true},
	{"a semicolon and a second statement", `x; DROP TABLE canary`, true},
	{"a line comment marker", `--`, true},
	{"a block comment marker", `/* x */`, true},
	{"a NUL byte", "a\x00b; DROP TABLE canary", false},
	{"a name of 64 bytes", strings.Repeat("n", 64), false},
	{"a non-ASCII name", "naïve_名前", true},
}

// probeWorld is the canary fixture in the spelling of one identifier mode: the table canary,
// with its id and its note, and a column and a table named by each text that can be a name, so
// that a name that is quoted right is found, and rows come back.
type probeWorld struct {
	f    *queryFixture
	mode IdentifierMode
}

// stored is the name as the mode stores it, which is what the DDL creates.
func (w probeWorld) stored(name string) string {
	if w.mode == IdentifierFoldLower {
		return strings.ToLower(name)
	}
	return name
}

var canaryRows = []map[string]any{
	{"id": int64(1), "note": "one"},
	{"id": int64(2), "note": "two"},
	{"id": int64(3), "note": "O'Brien; DROP TABLE canary; --"},
}

func openProbeWorld(t *testing.T, mode IdentifierMode) probeWorld {
	t.Helper()
	w := probeWorld{mode: mode}
	columns := []string{`id integer PRIMARY KEY`, `note text COLLATE "C"`}
	var tables []string
	for _, probe := range probes {
		if probe.name {
			columns = append(columns, exactIdent(w.stored(probe.text))+` integer`)
			tables = append(tables, `CREATE TABLE `+exactIdent(w.stored(probe.text))+` (id integer PRIMARY KEY, note text)`,
				`INSERT INTO `+exactIdent(w.stored(probe.text))+` VALUES (1, 'named')`)
		}
	}
	ddl := []string{`CREATE TABLE canary (` + strings.Join(columns, ", ") + `)`}
	probeNames := make([]string, 0, len(probes))
	for _, probe := range probes {
		if probe.name {
			probeNames = append(probeNames, exactIdent(w.stored(probe.text)))
		}
	}
	// Each row's probe columns hold its id, so a filter, an order, a group and a join on one
	// of them is a real one.
	insert := func(id int, note string) string {
		values := []string{fmt.Sprint(id), `$$` + note + `$$`}
		for range probeNames {
			values = append(values, fmt.Sprint(id))
		}
		return `INSERT INTO canary VALUES (` + strings.Join(values, ", ") + `)`
	}
	ddl = append(ddl, insert(1, "one"), insert(2, "two"), insert(3, "O'Brien; DROP TABLE canary; --"))
	ddl = append(ddl, tables...)
	schema := "test_probes_exact"
	if mode == IdentifierFoldLower {
		schema = "test_probes_fold"
	}
	w.f = openQueryFixture(t, schema, mode, ddl)
	return w
}

// stripQuoted removes every quoted identifier from a statement, the quotes included: what is
// left is the statement's own text, with its markers, which is where no text of a caller may
// stand. A quote inside an identifier is a doubled quote.
func stripQuoted(statement string) string {
	var out strings.Builder
	for i := 0; i < len(statement); i++ {
		if statement[i] != '"' {
			out.WriteByte(statement[i])
			continue
		}
		i++
		for i < len(statement) {
			if statement[i] == '"' {
				if i+1 < len(statement) && statement[i+1] == '"' {
					i += 2
					continue
				}
				break
			}
			i++
		}
	}
	return out.String()
}

func TestStripQuoted(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`SELECT "a" FROM "t" WHERE "a" = $1`, `SELECT  FROM  WHERE  = $1`},
		{`SELECT "x; DROP TABLE canary; --" FROM "t"`, `SELECT  FROM `},
		{`SELECT "a""b; --" FROM "t"`, `SELECT  FROM `},
		{`SELECT """" FROM "t"`, `SELECT  FROM `},
		{`SELECT 1; --`, `SELECT 1; --`},
		{`SELECT "unterminated`, `SELECT `},
	} {
		if got := stripQuoted(tc.in); got != tc.want {
			t.Errorf("stripQuoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// slot is a place a caller's text can stand in a query.
type slot struct {
	name  string
	value bool // the text is a value, not a name
	// query builds the query with the text in the slot.
	query func(w probeWorld, text string) dal.StructuredQuery
}

func (w probeWorld) canaryFrom(alias string) dal.FromSource {
	return dal.From(dal.NewRootCollectionRef("canary", alias))
}

var probeSlots = []slot{
	{"a value in a filter", true, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().WhereField("note", dal.Equal, text).SelectColumns(dal.Column{Expression: field("id")})
	}},
	{"a value in an IN list", true, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().WhereField("note", dal.In, []string{text, "two"}).
			SelectColumns(dal.Column{Expression: field("id")})
	}},
	{"a field in a filter", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().WhereField(text, dal.Equal, 1).SelectColumns(dal.Column{Expression: field("id")})
	}},
	{"a field in the select list", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().OrderBy(dal.AscendingField("id")).SelectColumns(dal.Column{Expression: field(text)})
	}},
	{"a collection", false, func(w probeWorld, text string) dal.StructuredQuery {
		return dal.From(dal.NewRootCollectionRef(text, "")).NewQuery().SelectColumns(dal.Column{Expression: field("id")})
	}},
	{"an alias of a column", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().OrderBy(dal.AscendingField("id")).SelectColumns(dal.Column{Expression: field("note"), Alias: text})
	}},
	{"an alias of the source", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom(text).NewQuery().OrderBy(dal.AscendingField("id")).SelectColumns(dal.Column{Expression: field("note")})
	}},
	{"an ORDER BY field", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().OrderBy(dal.AscendingField(text)).SelectColumns(dal.Column{Expression: field("id")})
	}},
	{"a GROUP BY field", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().GroupBy(field(text)).OrderBy(dal.AscendingField(text)).
			SelectColumns(dal.Column{Expression: field(text)}, dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"})
	}},
	{"a join key on the left", false, func(w probeWorld, text string) dal.StructuredQuery {
		on := dal.NewComparison(dal.NewFieldRef("a", text), dal.Equal, dal.NewFieldRef("b", "id"))
		return dal.From(dal.NewRootCollectionRef("canary", "a")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef("canary", "b"), dal.JoinInner, on)).
			NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("a", "id"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("b", "note"), Alias: "note"})
	}},
	{"a join key on the right", false, func(w probeWorld, text string) dal.StructuredQuery {
		on := dal.NewComparison(dal.NewFieldRef("a", "id"), dal.Equal, dal.NewFieldRef("b", text))
		return dal.From(dal.NewRootCollectionRef("canary", "a")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef("canary", "b"), dal.JoinInner, on)).
			NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("a", "id"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "note"), Alias: "note"})
	}},
	{"a collection of a joined source", false, func(w probeWorld, text string) dal.StructuredQuery {
		on := dal.NewComparison(dal.NewFieldRef("a", "id"), dal.Equal, dal.NewFieldRef("b", "id"))
		return dal.From(dal.NewRootCollectionRef("canary", "a")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef(text, "b"), dal.JoinInner, on)).
			NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("a", "id"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "note"), Alias: "note"})
	}},
}

// quotedIn is an identifier as the mode writes it: quoted, a quote doubled, and lower-cased in
// the fold-lower mode.
func (w probeWorld) quotedIn(text string) string { return exactIdent(w.stored(text)) }

func TestProbesIntegration_NothingACallerWritesBecomesSQL(t *testing.T) {
	testDSN(t) // a skip shows on this test, not only on its subtests
	for _, mode := range []struct {
		name string
		mode IdentifierMode
	}{{"exact mode", IdentifierExact}, {"fold-lower mode", IdentifierFoldLower}} {
		t.Run(mode.name, func(t *testing.T) {
			w := openProbeWorld(t, mode.mode)
			f := w.f
			var tally = map[string]int{}
			valueStatements := map[string]string{} // the statement of a value slot, by slot, for the first text
			for _, s := range probeSlots {
				for _, probe := range probes {
					label := s.name + " = " + probe.label
					f.trace.reset()
					run := runThroughAdapter(f, s.query(w, probe.text))
					sent := f.trace.sent()
					outcome := classifyProbe(run)
					tally[outcome]++
					t.Logf("%s: %s", label, outcome)

					// What may be the answer. A name that PostgreSQL can hold is found, quoted,
					// and rows or an empty result come back; one it cannot hold is an error built
					// before any statement. A value is compared as text, so its answer is rows
					// or an empty result, and for a NUL byte the server's own refusal.
					switch {
					case s.value && !strings.ContainsRune(probe.text, 0):
						if run.err != nil {
							t.Errorf("%s: error = %v, want rows or an empty result", label, run.err)
						}
					case s.value:
						var pgErr *pgconn.PgError
						if run.err == nil || !errors.As(run.err, &pgErr) || pgErr.Code != "22021" {
							t.Errorf("%s: error = %v, want the server's data exception 22021 (the text cannot hold a NUL byte)", label, run.err)
						}
					case probe.name:
						if run.err != nil {
							t.Errorf("%s: error = %v, want the quoted name to be found and rows to come back", label, run.err)
						}
					default:
						if run.err == nil || len(run.statements) != 0 {
							t.Errorf("%s: error = %v after %d statements, want an error built before any statement", label, run.err, len(run.statements))
						}
					}

					// The statements. Outside its quoted identifiers a statement holds no marker
					// of a caller's text, and no text of a caller outside the arguments.
					for _, one := range sent {
						if isCatalogStatement(one.sql) {
							if strings.Contains(one.sql, probe.text) && len(probe.text) > 4 {
								t.Errorf("%s: the catalog statement holds the text: %s", label, one.sql)
							}
							continue
						}
						bare := stripQuoted(one.sql)
						for _, marker := range []string{";", "--", "/*", "'", "DROP"} {
							if strings.Contains(bare, marker) {
								t.Errorf("%s: outside its quoted identifiers the statement holds %q: %s", label, marker, one.sql)
							}
						}
						if s.value {
							if len(valueArguments(one.args)) == 0 || !containsArgument(valueArguments(one.args), probe.text) {
								t.Errorf("%s: arguments = %s, want the text as a bound argument", label, argsText(valueArguments(one.args)))
							}
							if strings.Contains(one.sql, probe.text) && len(probe.text) > 4 {
								t.Errorf("%s: the statement holds the value: %s", label, one.sql)
							}
							if first, seen := valueStatements[s.name]; !seen {
								valueStatements[s.name] = one.sql
							} else if first != one.sql {
								t.Errorf("%s: the statement depends on the value:\n  %s\n  %s", label, first, one.sql)
							}
						} else if len(probe.text) > 0 && probe.name && strings.Contains(strings.ToUpper(one.sql), "DROP TABLE") &&
							!strings.Contains(one.sql, w.quotedIn(probe.text)) {
							t.Errorf("%s: a name is written without its quotes: %s", label, one.sql)
						}
					}
				}
			}
			t.Logf("answers: %v", tally)

			// The canary is whole after all of them.
			rows, err := f.admin.QueryContext(context.Background(), `SELECT id, note FROM canary ORDER BY id`)
			if err != nil {
				t.Fatalf("the canary table cannot be read after the probes: %v", err)
			}
			defer func() { _ = rows.Close() }()
			var got []map[string]any
			for rows.Next() {
				var id int64
				var note string
				if err := rows.Scan(&id, &note); err != nil {
					t.Fatal(err)
				}
				got = append(got, map[string]any{"id": id, "note": note})
			}
			if !reflect.DeepEqual(got, canaryRows) {
				t.Errorf("the canary holds %v after the probes, want %v", got, canaryRows)
			}
			var exists bool
			if err := f.admin.QueryRowContext(context.Background(), `SELECT to_regclass('canary') IS NOT NULL`).Scan(&exists); err != nil || !exists {
				t.Errorf("the canary table is gone (%v)", err)
			}
		})
	}
}

// containsArgument says whether one of the arguments is the string text.
func containsArgument(args []any, text string) bool {
	for _, arg := range args {
		switch v := arg.(type) {
		case string:
			if v == text {
				return true
			}
		case []any:
			if containsArgument(v, text) {
				return true
			}
		case []string:
			for _, item := range v {
				if item == text {
					return true
				}
			}
		}
	}
	return false
}

// classifyProbe says what the answer to a probe was, in the words of the contract.
func classifyProbe(run adapterRun) string {
	switch {
	case run.err == nil && len(run.rows) > 0:
		return "rows"
	case run.err == nil:
		return "an empty result"
	case len(run.statements) == 0:
		return "an error built before any statement"
	}
	var pgErr *pgconn.PgError
	if errors.As(run.err, &pgErr) {
		return "the server's refusal, SQLSTATE " + pgErr.Code
	}
	return "an error after a statement"
}
