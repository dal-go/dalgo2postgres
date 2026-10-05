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
						if run.err != nil {
							t.Errorf("%s: error = %v, want the quoted name to be found and rows to come back", label, run.err)
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

// The key paths (Exists, Get, Insert and Delete) are not the structured compiler: they write
// plain names unquoted and refuse every other name before a statement (dalgo2sql.ErrUnsafeName),
// and carry every value as an argument. The same texts as above are written as the collection
// of a key, as its ID, as the field names of its data and as the values of its data, against a
// table whose key is declared and a recordset declared under each text. A text that is not a
// plain identifier is refused with ErrUnsafeName and no statement; an ID and a value are
// arguments, found again exactly as written; the one text that is a plain identifier and no
// table (the name of 64 bytes) is the server's own refusal of a table or a column it does not
// have, and no statement of any of them holds a marker. After all of them the canary and the key
// table are as they were.
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
			f := openQueryFixtureWithOptions(t, dalgo2sql.DbOptions{Recordsets: recordsets}, schema, mode.mode, []string{
				`CREATE TABLE canary (id integer PRIMARY KEY, note text)`,
				`INSERT INTO canary VALUES (1, 'one'), (2, 'two')`,
				`CREATE TABLE probe_keys (code text PRIMARY KEY, note text)`,
				`INSERT INTO probe_keys VALUES ('one', 'first')`,
			})
			ctx := context.Background()
			var server *pgconn.PgError
			tally := map[string]int{}
			for _, probe := range probes {
				text := probe.text
				hasNUL := strings.ContainsRune(text, 0)

				// The text as the collection of a key.
				collectionKey := dalrecord.NewKeyWithID(text, "one")
				f.trace.reset()
				exists, existsErr := f.db.Exists(ctx, collectionKey)
				getErr := f.db.Get(ctx, dalrecord.NewRecordWithData(collectionKey, map[string]any{}))
				label := "the collection " + probe.label
				sent := f.trace.sent()
				for _, problem := range keyPathProblems(label, text, sent, isPlainName(text)) {
					t.Error(problem)
				}
				if !isPlainName(text) {
					tally["a collection refused before a statement"]++
					if !errors.Is(existsErr, dalgo2sql.ErrUnsafeName) || !errors.Is(getErr, dalgo2sql.ErrUnsafeName) || exists || len(sent) != 0 {
						t.Errorf("%s: Exists = %v, %v; Get = %v; %d statements; want ErrUnsafeName from both and no statement", label, exists, existsErr, getErr, len(sent))
					}
				} else {
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
				exists, existsErr = f.db.Exists(ctx, idKey)
				getErr = f.db.Get(ctx, dalrecord.NewRecordWithData(idKey, map[string]any{}))
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
				freshKey := dalrecord.NewKeyWithID("probe_keys", "fresh")
				f.trace.reset()
				err := f.db.Insert(ctx, dalrecord.NewRecordWithData(freshKey, map[string]any{text: "x"}))
				sent = f.trace.sent()
				for _, problem := range keyPathProblems(label, text, sent, isPlainName(text)) {
					t.Error(problem)
				}
				if !isPlainName(text) {
					tally["a field refused before a statement"]++
					if !errors.Is(err, dalgo2sql.ErrUnsafeName) || len(sent) != 0 {
						t.Errorf("%s: Insert = %v with %d statements; want ErrUnsafeName and no statement", label, err, len(sent))
					}
				} else {
					tally["a field that is a plain name and no column"]++
					t.Logf("%s: Insert = %v; statements %v", label, err, sent)
					if err == nil {
						t.Errorf("%s: Insert = nil, want the server's refusal of a column the table does not have", label)
					}
				}
			}
			t.Logf("answers: %v", tally)

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
