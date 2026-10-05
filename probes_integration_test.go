package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5/pgconn"
)

// Probes: nothing a caller writes becomes SQL. A canary table, and queries in which the
// caller's text stands where a value (in a filter, an IN list and a HAVING), a field name (in
// a filter, the select list, an aggregate, ORDER BY and GROUP BY), a field's source qualifier,
// a collection name, the schema of a qualified collection, an alias (of a column, of the source
// and of a joined source) and a join key go, each in the exact and the fold-lower mode. The text
// is a quote, a double quote, doubled quotes, a semicolon with a second statement that would
// drop the canary, comment markers, a NUL byte, a name of 64 bytes (one more than a PostgreSQL
// identifier holds) and a non-ASCII name. A second test, in the same two modes, writes the same
// texts into the key paths (Exists, Get, Insert and Delete: the collection of a key, its ID, and
// the field names and values of its data), which are another emitter of SQL than the structured
// compiler, and the path a database written through OVDB takes.
//
// The answer to every probe is rows, an empty result or an error the adapter or DALgo builds (for
// a name PostgreSQL cannot hold, a NUL byte or 64 bytes; in a join DALgo's engine may have read the
// other sources first, with plain statements); the one exception is a NUL byte in a value, which
// the server itself refuses as a data exception. After all of them the canary table exists with its rows unchanged, and
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
	var tables, schemas []string
	for _, probe := range probes {
		if probe.name {
			columns = append(columns, exactIdent(w.stored(probe.text))+` integer`)
			tables = append(tables, `CREATE TABLE `+exactIdent(w.stored(probe.text))+` (id integer PRIMARY KEY, note text)`,
				`INSERT INTO `+exactIdent(w.stored(probe.text))+` VALUES (1, 'named')`)
			// A schema of the same name, with a table the qualified-collection slot reads.
			schemas = append(schemas, w.stored(probe.text))
			tables = append(tables, `CREATE TABLE `+exactIdent(w.stored(probe.text))+`.probe_tab (id integer PRIMARY KEY, note text)`,
				`INSERT INTO `+exactIdent(w.stored(probe.text))+`.probe_tab VALUES (1, 'in the schema')`)
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
	w.f = openQueryFixture(t, schema, mode, ddl, schemas...)
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
	// The schema of a qualified collection has a branch of its own in the compiler. A schema is
	// created for each text that can be a name, with the table probe_tab in it.
	{"the schema of a qualified collection", false, func(w probeWorld, text string) dal.StructuredQuery {
		return dal.From(dal.NewQualifiedRootCollectionRef(text, "probe_tab", "")).NewQuery().
			OrderBy(dal.AscendingField("id")).SelectColumns(dal.Column{Expression: field("id")})
	}},
	// An alias is also the qualifier of the fields that name its source, so the two slots below
	// write the text as both. A qualifier that names no source is refused by DALgo whatever the
	// text, and is not a slot of its own.
	{"the alias of a joined source, and the qualifier of its fields", false, func(w probeWorld, text string) dal.StructuredQuery {
		on := dal.NewComparison(dal.NewFieldRef("a", "id"), dal.Equal, dal.NewFieldRef(text, "id"))
		return dal.From(dal.NewRootCollectionRef("canary", "a")).
			Join(dal.NewJoinedSource(dal.NewRootCollectionRef("canary", text), dal.JoinInner, on)).
			NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("a", "id"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef(text, "note"), Alias: "note"})
	}},
	{"the alias of the source, and the qualifier of a field in a filter, an order and the select list", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom(text).NewQuery().
			Where(dal.NewComparison(dal.NewFieldRef(text, "id"), dal.GreaterThen, dal.NewConstant(0))).
			OrderBy(dal.Ascending(dal.NewFieldRef(text, "id"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef(text, "note")})
	}},
	{"a field in an aggregate", false, func(w probeWorld, text string) dal.StructuredQuery {
		return w.canaryFrom("").NewQuery().GroupBy(field("id")).OrderBy(dal.AscendingField("id")).
			SelectColumns(dal.Column{Expression: field("id")}, dal.SumAs(field(text), "total"))
	}},
	{"a value in a HAVING", true, func(w probeWorld, text string) dal.StructuredQuery {
		longest := dal.NewAggregate(dal.MAX, false, field("note"))
		return w.canaryFrom("").NewQuery().GroupBy(field("id")).
			Having(dal.NewComparison(longest, dal.Equal, dal.NewConstant(text))).
			OrderBy(dal.AscendingField("id")).
			SelectColumns(dal.Column{Expression: field("id")}, dal.Column{Expression: longest, Alias: "note"})
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
						// Every column and table the fixture holds is named by one of these texts, and
						// every row of the canary has a value in each, so the answer is rows.
						if run.err != nil {
							t.Errorf("%s: error = %v, want the quoted name to be found and rows to come back", label, run.err)
						} else if len(run.rows) == 0 {
							t.Errorf("%s: an empty result, want rows: the quoted name is found and the fixture holds a value for it", label)
						}
					default:
						// The adapter or DALgo builds the error. In a join DALgo's engine may have
						// read the other sources first, with plain statements, which the checks
						// below hold to the same rules; the server never sees the name.
						var pgErr *pgconn.PgError
						if run.err == nil || errors.As(run.err, &pgErr) {
							t.Errorf("%s: error = %v, want an error built by the adapter or DALgo, not the server's", label, run.err)
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
			// The tallies are asserted, so a slot that begins to answer differently shows. Of the 17
			// slots, 3 hold a value and 14 a name; of the 12 texts, 10 are names PostgreSQL can hold
			// (all but the NUL byte and the 64 bytes). The IN list holds "two" beside the text, so it
			// returns rows for 11 texts; the other two value slots match no row (22 empty results), and
			// the NUL byte in a value is the server's 22021 (3). The 10 names in 14 slots are 140 rows,
			// which with the IN list's 11 make 151. The 2 names PostgreSQL cannot hold in 14 slots are 28
			// errors: 8 in the four slots of a join, built by DALgo's engine after plain reads of the
			// other sources, and 20 built before any statement.
			wantTally := map[string]int{
				"rows": 151, "an empty result": 22, "the server's refusal, SQLSTATE 22021": 3,
				"an error built before any statement": 20, "an error built by DALgo's engine after plain reads": 8,
			}
			if !reflect.DeepEqual(tally, wantTally) {
				t.Errorf("answers = %v, want %v", tally, wantTally)
			}

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
	}
	var pgErr *pgconn.PgError
	if errors.As(run.err, &pgErr) {
		return "the server's refusal, SQLSTATE " + pgErr.Code
	}
	if len(run.statements) == 0 {
		return "an error built before any statement"
	}
	return "an error built by DALgo's engine after plain reads"
}

// plainName is what a key path accepts as a collection, field or primary-key name (README, "Key
// reads and writes"): ASCII letters, digits and underscores, not starting with a digit.
var plainName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isPlainName says whether a key path accepts text as a name: it is a plain identifier of at
// most 255 bytes. Anything else is refused with dalgo2sql.ErrUnsafeName before a statement.
func isPlainName(text string) bool { return len(text) <= 255 && plainName.MatchString(text) }

// postgresNameBytes is the longest identifier PostgreSQL keeps. It cuts a longer one to this many
// bytes (with a notice) and looks that up, so a name of 64 to 255 bytes, which a key path accepts
// and writes unquoted, addresses the table or column named by its first 63 bytes.
const postgresNameBytes = 63

// truncatedName is what the server makes of a name of 64 n's: the name of 63 n's. The key-path
// fixture holds a table and a column of that name, so the pin shows which one is addressed.
var truncatedName = strings.Repeat("n", postgresNameBytes)

// serverTruncates says whether text is a name a key path accepts and the server cuts.
func serverTruncates(text string) bool { return isPlainName(text) && len(text) > postgresNameBytes }

// keyPathMarkers must appear in no statement of a key path at all: it writes its names
// unquoted, so a name that is not a plain identifier would show as one of these, and a value
// is an argument, never text.
var keyPathMarkers = []string{";", "--", "/*", "'", `"`, "DROP"}

// keyPathProblems says what is wrong with the statements a key path sent: each must hold none
// of the markers, and, unless the text is a name the statement is allowed to write (a plain
// identifier as a collection or a field), none may hold the text of a probe longer than four
// bytes: a value is an argument and never text.
func keyPathProblems(label, text string, statements []tracedStatement, mayWriteText bool) []string {
	var problems []string
	for _, sent := range statements {
		for _, marker := range keyPathMarkers {
			if strings.Contains(sent.sql, marker) {
				problems = append(problems, fmt.Sprintf("%s: the statement holds %q: %s", label, marker, sent.sql))
			}
		}
		if !mayWriteText && len(text) > 4 && strings.Contains(sent.sql, text) {
			problems = append(problems, fmt.Sprintf("%s: the statement holds the text: %s", label, sent.sql))
		}
	}
	return problems
}

// The names of the known defect below: the key-path fixture holds a table, with one row, and a
// column of the 63 bytes the server keeps of a name of 64.
const (
	truncatedTableNote = "the table of 63 bytes"
	writtenThroughName = "written through the name of 64 bytes"
)

// fixedKeyPathTruncation is what a failing pin says to do: dalgo2sql has stopped sending the name.
const fixedKeyPathTruncation = "dalgo2sql now refuses a name of more than 63 bytes in a key path before any statement, as its typed dialect does: assert here dalgo2sql.ErrUnsafeName from Exists, Get, Insert and Delete of the collection and from Insert of the field, no statement sent, and the table and the column of 63 bytes untouched; then delete this pin, the known limit in the README and the 'cut by the server' tally"

// pinTruncatedCollection pins a KNOWN DEFECT of dal-go/dalgo2sql, a WRONG RESULT and not a
// refusal (issue or pull request: not yet filed): a key whose collection is a name of 64 bytes
// is written into the statement as given and unquoted, the server cuts it to 63 bytes, and the
// statement reads, writes and deletes in the table of that name. The table of 63 bytes of the
// fixture shows it: Exists and Get find its row, Insert writes into it, Delete removes from it.
// The typed compiler refuses the same name before any statement (the first probe test), and the
// plan for PostgreSQL is that a name over 63 bytes is refused. Each step FAILS when dalgo2sql
// refuses the name, and says what to assert then.
func pinTruncatedCollection(t *testing.T, f *queryFixture, text string) {
	t.Helper()
	ctx := context.Background()
	label := "the collection of 64 bytes"
	if text[:postgresNameBytes] != truncatedName {
		t.Fatalf("%s: the first %d bytes are %q, want the table of the fixture, %q", label, postgresNameBytes, text[:postgresNameBytes], truncatedName)
	}
	one := dalrecord.NewKeyWithID(text, "one")
	two := dalrecord.NewKeyWithID(text, "two")
	// step runs one operation and says what it sent; it reports true when the name was refused.
	step := func(what string, run func() error) (sent []tracedStatement, refused bool, err error) {
		f.trace.reset()
		err = run()
		sent = f.trace.sent()
		for _, problem := range keyPathProblems(label+": "+what, text, sent, true) {
			t.Error(problem)
		}
		t.Logf("KNOWN DEFECT of dal-go/dalgo2sql (a wrong result, not a refusal): %s of a key whose collection is %d bytes: error = %v; statements %v", what, len(text), err, sent)
		if errors.Is(err, dalgo2sql.ErrUnsafeName) || len(sent) == 0 {
			t.Errorf("%s: %s: error = %v with %d statements: %s", label, what, err, len(sent), fixedKeyPathTruncation)
			return sent, true, err
		}
		return sent, false, err
	}
	// want says whether the statements are exactly the one given, with the arguments given.
	want := func(what string, sent []tracedStatement, statement string, args ...any) {
		if len(sent) != 1 || sent[0].sql != statement || argsText(valueArguments(sent[0].args)) != argsText(args) {
			t.Errorf("%s: %s: statements = %v, want exactly %s with arguments %s", label, what, sent, statement, argsText(args))
		}
	}
	// stored reads the table of 63 bytes through the untraced handle.
	stored := func() map[string]string {
		rows, err := f.admin.QueryContext(ctx, `SELECT code, note FROM `+exactIdent(truncatedName)+` ORDER BY code`)
		if err != nil {
			t.Fatalf("%s: the table of %d bytes cannot be read: %v", label, postgresNameBytes, err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]string{}
		for rows.Next() {
			var code, note string
			if err := rows.Scan(&code, &note); err != nil {
				t.Fatal(err)
			}
			out[code] = note
		}
		return out
	}

	var exists bool
	sent, refused, err := step("Exists", func() (err error) { exists, err = f.db.Exists(ctx, one); return })
	if refused {
		return
	}
	want("Exists", sent, "SELECT 1 FROM "+text+" WHERE code = $1", "one")
	if err != nil || !exists {
		t.Errorf("%s: Exists = %v, %v; want true: the statement reads the table of %d bytes, which holds the key one", label, exists, err, postgresNameBytes)
	}

	read := dalrecord.NewRecordWithData(one, map[string]any{})
	sent, refused, err = step("Get", func() error { return f.db.Get(ctx, read) })
	if refused {
		return
	}
	want("Get", sent, "SELECT * FROM "+text+" WHERE code = $1", "one")
	if data, _ := read.Data().(map[string]any); err != nil || data["note"] != truncatedTableNote {
		t.Errorf("%s: Get: error = %v, note = %v; want the note %q of the row of the table of %d bytes", label, err, data["note"], truncatedTableNote, postgresNameBytes)
	}

	sent, refused, err = step("Insert", func() error {
		return f.db.Insert(ctx, dalrecord.NewRecordWithData(two, map[string]any{"note": writtenThroughName}))
	})
	if refused {
		return
	}
	want("Insert", sent, "INSERT INTO "+text+"(code, note) VALUES ($1, $2)", "two", writtenThroughName)
	if got := stored(); err != nil || got["two"] != writtenThroughName {
		t.Errorf("%s: Insert: error = %v; the table of %d bytes holds %v, want the row two written into it", label, err, postgresNameBytes, got)
	}

	sent, refused, err = step("Delete", func() error { return f.db.Delete(ctx, one) })
	if refused {
		return
	}
	want("Delete", sent, "DELETE FROM "+text+" WHERE code = $1", "one")
	if got := stored(); err != nil || !reflect.DeepEqual(got, map[string]string{"two": writtenThroughName}) {
		t.Errorf("%s: Delete: error = %v; the table of %d bytes holds %v, want only the row two: the row one was deleted from it", label, err, postgresNameBytes, got)
	}
}

// pinTruncatedField pins the same KNOWN DEFECT of dal-go/dalgo2sql for a field: a field of 64
// bytes is written as given and unquoted, and the server writes the value into the column of
// its first 63 bytes. It FAILS when dalgo2sql refuses the name, and says what to assert then.
func pinTruncatedField(t *testing.T, f *queryFixture, text string) {
	t.Helper()
	ctx := context.Background()
	label := "the field of 64 bytes"
	key := dalrecord.NewKeyWithID("probe_keys", "fresh")
	f.trace.reset()
	err := f.db.Insert(ctx, dalrecord.NewRecordWithData(key, map[string]any{text: "x"}))
	sent := f.trace.sent()
	for _, problem := range keyPathProblems(label, text, sent, true) {
		t.Error(problem)
	}
	t.Logf("KNOWN DEFECT of dal-go/dalgo2sql (a wrong result, not a refusal): Insert of a field of %d bytes: error = %v; statements %v", len(text), err, sent)
	if errors.Is(err, dalgo2sql.ErrUnsafeName) || len(sent) == 0 {
		t.Errorf("%s: Insert: error = %v with %d statements: %s", label, err, len(sent), fixedKeyPathTruncation)
		return
	}
	statement := "INSERT INTO probe_keys(code, " + text + ") VALUES ($1, $2)"
	if len(sent) != 1 || sent[0].sql != statement || argsText(valueArguments(sent[0].args)) != argsText([]any{"fresh", "x"}) {
		t.Errorf("%s: Insert: statements = %v, want exactly %s with arguments %s", label, sent, statement, argsText([]any{"fresh", "x"}))
	}
	var value string
	if scanErr := f.admin.QueryRowContext(ctx, `SELECT `+exactIdent(truncatedName)+` FROM probe_keys WHERE code = 'fresh'`).Scan(&value); err != nil || scanErr != nil || value != "x" {
		t.Errorf("%s: Insert: error = %v; the column of %d bytes holds %q (error %v), want x written into it", label, err, postgresNameBytes, value, scanErr)
	}
	// What the test wrote is taken away, so the key table is as it was.
	if _, err := f.admin.ExecContext(ctx, `DELETE FROM probe_keys WHERE code = 'fresh'`); err != nil {
		t.Fatalf("%s: the row written is not removed: %v", label, err)
	}
}

// The key paths (Exists, Get, Insert and Delete) are not the structured compiler: they write
// plain names unquoted and refuse every other name before a statement (dalgo2sql.ErrUnsafeName),
// and carry every value as an argument. The same texts as above are written as the collection
// of a key, as its ID, as the field names of its data and as the values of its data, against a
// table whose key is declared and a recordset declared under each text. A text that is not a
// plain identifier is refused with ErrUnsafeName and no statement; an ID and a value are
// arguments, found again exactly as written. The one text that is a plain identifier and more
// than 63 bytes (the name of 64 bytes) is not refused: the server cuts it to 63 bytes, so it is
// the name of a table and a column the fixture holds, and the key paths read, write and delete
// in them. That is a wrong result of dal-go/dalgo2sql, pinned by pinTruncatedCollection and
// pinTruncatedField with the statements and the rows. No statement of any of them holds a
// marker. After all of them the canary, the key table and the table of 63 bytes are as the
// pins leave them.
func TestProbesIntegration_NothingACallerWritesBecomesSQLInAKeyPath(t *testing.T) {
	testDSN(t) // a skip shows on this test, not only on its subtests
	for _, mode := range []struct {
		name string
		mode IdentifierMode
	}{{"exact mode", IdentifierExact}, {"fold-lower mode", IdentifierFoldLower}} {
		t.Run(mode.name, func(t *testing.T) {
			recordsets := map[string]*dalgo2sql.Recordset{
				"probe_keys": dalgo2sql.NewRecordset("probe_keys", dalgo2sql.Table, []dal.FieldRef{dal.Field("code")}),
			}
			for _, probe := range probes {
				recordsets[probe.text] = dalgo2sql.NewRecordset(probe.text, dalgo2sql.Table, []dal.FieldRef{dal.Field("code")})
			}
			schema := "test_probes_keys_exact"
			if mode.mode == IdentifierFoldLower {
				schema = "test_probes_keys_fold"
			}
			// The table and the column of 63 bytes are what the server makes of a name of 64: the
			// pins of the known defect above show which of them a key path reads and writes.
			f := openQueryFixtureWithOptions(t, dalgo2sql.DbOptions{Recordsets: recordsets}, schema, mode.mode, []string{
				`CREATE TABLE canary (id integer PRIMARY KEY, note text)`,
				`INSERT INTO canary VALUES (1, 'one'), (2, 'two')`,
				`CREATE TABLE probe_keys (code text PRIMARY KEY, note text, ` + truncatedName + ` text)`,
				`INSERT INTO probe_keys (code, note) VALUES ('one', 'first')`,
				`CREATE TABLE ` + truncatedName + ` (code text PRIMARY KEY, note text)`,
				`INSERT INTO ` + truncatedName + ` VALUES ('one', '` + truncatedTableNote + `')`,
			})
			ctx := context.Background()
			var server *pgconn.PgError
			tally := map[string]int{}
			for _, probe := range probes {
				text := probe.text
				hasNUL := strings.ContainsRune(text, 0)

				// The text as the collection of a key.
				label := "the collection " + probe.label
				var sent []tracedStatement
				switch {
				case serverTruncates(text):
					tally["a collection cut to 63 bytes by the server: a known defect"]++
					pinTruncatedCollection(t, f, text)
				case !isPlainName(text):
					collectionKey := dalrecord.NewKeyWithID(text, "one")
					f.trace.reset()
					exists, existsErr := f.db.Exists(ctx, collectionKey)
					getErr := f.db.Get(ctx, dalrecord.NewRecordWithData(collectionKey, map[string]any{}))
					sent = f.trace.sent()
					for _, problem := range keyPathProblems(label, text, sent, false) {
						t.Error(problem)
					}
					tally["a collection refused before a statement"]++
					if !errors.Is(existsErr, dalgo2sql.ErrUnsafeName) || !errors.Is(getErr, dalgo2sql.ErrUnsafeName) || exists || len(sent) != 0 {
						t.Errorf("%s: Exists = %v, %v; Get = %v; %d statements; want ErrUnsafeName from both and no statement", label, exists, existsErr, getErr, len(sent))
					}
				default:
					// A plain name of at most 63 bytes is the name of a table the fixture has not made.
					collectionKey := dalrecord.NewKeyWithID(text, "one")
					f.trace.reset()
					exists, existsErr := f.db.Exists(ctx, collectionKey)
					getErr := f.db.Get(ctx, dalrecord.NewRecordWithData(collectionKey, map[string]any{}))
					sent = f.trace.sent()
					for _, problem := range keyPathProblems(label, text, sent, true) {
						t.Error(problem)
					}
					tally["a collection that is a plain name and no table"]++
					t.Logf("%s: Exists = %v, %v; Get = %v; statements %v", label, exists, existsErr, getErr, sent)
					if exists || (existsErr == nil && getErr == nil) {
						t.Errorf("%s: Exists = %v, %v; Get = %v; want no row: the table of that name is not there", label, exists, existsErr, getErr)
					}
				}

				// The text as the ID of a key: an argument, found again exactly as written.
				idKey := dalrecord.NewKeyWithID("probe_keys", text)
				label = "the ID " + probe.label
				f.trace.reset()
				exists, existsErr := f.db.Exists(ctx, idKey)
				getErr := f.db.Get(ctx, dalrecord.NewRecordWithData(idKey, map[string]any{}))
				sent = f.trace.sent()
				for _, problem := range keyPathProblems(label, text, sent, false) {
					t.Error(problem)
				}
				for _, one := range sent {
					if !containsArgument(valueArguments(one.args), text) {
						t.Errorf("%s: arguments = %s, want the text as a bound argument", label, argsText(valueArguments(one.args)))
					}
				}
				if hasNUL {
					tally["an ID refused by the server"]++
					if !errors.As(existsErr, &server) || server.Code != "22021" || !errors.As(getErr, &server) || server.Code != "22021" {
						t.Errorf("%s: Exists = %v, %v; Get = %v; want the server's data exception 22021 from both", label, exists, existsErr, getErr)
					}
				} else {
					tally["an ID found again, or not found"]++
					if existsErr != nil || exists || !errors.Is(getErr, dalrecord.ErrRecordNotFound) {
						t.Errorf("%s: Exists = %v, %v; Get = %v; want no row for an ID nobody holds, and no error but not found", label, exists, existsErr, getErr)
					}
					// Written with the text as its ID and as the value of a field, it is read back as written.
					f.trace.reset()
					record := dalrecord.NewRecordWithData(idKey, map[string]any{"note": text})
					if err := f.db.Insert(ctx, record); err != nil {
						t.Errorf("%s: Insert: %v", label, err)
						continue
					}
					for _, problem := range keyPathProblems(label+" (Insert)", text, f.trace.sent(), false) {
						t.Error(problem)
					}
					for _, one := range f.trace.sent() {
						if args := valueArguments(one.args); len(args) != 2 || args[0] != text || args[1] != text {
							t.Errorf("%s: Insert arguments = %s, want the text twice, as the ID and as the note", label, argsText(args))
						}
					}
					back := dalrecord.NewRecordWithData(idKey, map[string]any{})
					if err := f.db.Get(ctx, back); err != nil {
						t.Errorf("%s: Get after Insert: %v", label, err)
					} else if data, _ := back.Data().(map[string]any); data["note"] != text {
						t.Errorf("%s: the note read back is %v, want %q exactly as written", label, data["note"], text)
					}
					if err := f.db.Delete(ctx, idKey); err != nil {
						t.Errorf("%s: Delete: %v", label, err)
					}
				}

				// The text as the name of a field of the data of a write.
				label = "the field " + probe.label
				switch {
				case serverTruncates(text):
					tally["a field cut to 63 bytes by the server: a known defect"]++
					pinTruncatedField(t, f, text)
				case !isPlainName(text):
					freshKey := dalrecord.NewKeyWithID("probe_keys", "fresh")
					f.trace.reset()
					err := f.db.Insert(ctx, dalrecord.NewRecordWithData(freshKey, map[string]any{text: "x"}))
					sent = f.trace.sent()
					for _, problem := range keyPathProblems(label, text, sent, false) {
						t.Error(problem)
					}
					tally["a field refused before a statement"]++
					if !errors.Is(err, dalgo2sql.ErrUnsafeName) || len(sent) != 0 {
						t.Errorf("%s: Insert = %v with %d statements; want ErrUnsafeName and no statement", label, err, len(sent))
					}
				default:
					// A plain name of at most 63 bytes is the name of a column the table has not got.
					freshKey := dalrecord.NewKeyWithID("probe_keys", "fresh")
					f.trace.reset()
					err := f.db.Insert(ctx, dalrecord.NewRecordWithData(freshKey, map[string]any{text: "x"}))
					sent = f.trace.sent()
					for _, problem := range keyPathProblems(label, text, sent, true) {
						t.Error(problem)
					}
					tally["a field that is a plain name and no column"]++
					t.Logf("%s: Insert = %v; statements %v", label, err, sent)
					if err == nil {
						t.Errorf("%s: Insert = nil, want the server's refusal of a column the table does not have", label)
					}
				}
			}
			t.Logf("answers: %v", tally)
			wantTally := map[string]int{
				"a collection refused before a statement":                    11,
				"a collection cut to 63 bytes by the server: a known defect": 1,
				"a field refused before a statement":                         11,
				"a field cut to 63 bytes by the server: a known defect":      1,
				"an ID found again, or not found":                            11,
				"an ID refused by the server":                                1,
			}
			if !reflect.DeepEqual(tally, wantTally) {
				t.Errorf("answers = %v, want %v", tally, wantTally)
			}

			// Nothing was left behind or taken: the key table holds its one row, and the canary its two.
			var codes []string
			rows, err := f.admin.QueryContext(ctx, `SELECT code FROM probe_keys ORDER BY code`)
			if err != nil {
				t.Fatalf("the key table cannot be read after the probes: %v", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var code string
				if err := rows.Scan(&code); err != nil {
					t.Fatal(err)
				}
				codes = append(codes, code)
			}
			if !reflect.DeepEqual(codes, []string{"one"}) {
				t.Errorf("the key table holds %v after the probes, want only the row it had", codes)
			}
			var n int
			if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM canary`).Scan(&n); err != nil || n != 2 {
				t.Errorf("the canary holds %d rows (error %v), want 2", n, err)
			}
		})
	}
}
