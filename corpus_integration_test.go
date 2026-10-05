package dalgo2postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"
)

// The query corpus on a real server: the DTQL fixtures of dal-go/dalgo that DataTug and OVDB
// send (joins, grouping, subqueries), vendored under testdata/joins and testdata/subqueries
// with their digests, run through this adapter against PostgreSQL. Each case is compared
// with its fixture, and a case that differs is listed in testdata/postgres-divergences.json
// with the reason: the tests fail for a difference that is not listed, and for a listed case
// that no longer differs.
//
// They run against PostgreSQL and skip unless DALGO2POSTGRES_TEST_DSN is set; CI fails when a
// TestFixturesIntegration_ test skips or is missing.

// adapterRun is what one read through the adapter did: the statements the driver was
// handed (the catalog lookup left out), the rows the reader returned and the error it ended
// with, from starting the read or from reading a row.
type adapterRun struct {
	statements []string
	rows       []map[string]any
	err        error
}

// runThroughAdapter reads q through the records reader of the fixture's database to its end.
func runThroughAdapter(f *queryFixture, q dal.Query) adapterRun {
	f.trace.reset()
	var run adapterRun
	reader, err := f.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		run.err = err
	} else {
		for {
			rec, err := reader.Next()
			if errors.Is(err, dal.ErrNoMoreRecords) {
				break
			}
			if err != nil {
				run.err = err
				break
			}
			data, _ := rec.Data().(map[string]any)
			run.rows = append(run.rows, data)
		}
		_ = reader.Close()
	}
	run.statements = statementsSent(f)
	return run
}

// readRecordsetRows reads a recordset reader to its end: each row is a map from the name of
// its column to its value, as the records reader's rows are, and the error is the one the
// reader ended with (the rows before it are kept).
func readRecordsetRows(reader dal.RecordsetReader) ([]map[string]any, error) {
	rs := reader.Recordset()
	names := make([]string, rs.ColumnsCount())
	for i := range names {
		names[i] = rs.GetColumnByIndex(i).Name()
	}
	var rows []map[string]any
	for {
		row, rowSet, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		values, err := row.Data(rowSet)
		if err != nil {
			return rows, err
		}
		data := make(map[string]any, len(names))
		for i, name := range names {
			data[name] = values[i]
		}
		rows = append(rows, data)
	}
}

// runThroughRecordsetReader is runThroughAdapter for the other public read of the database,
// the recordset reader (ExecuteQueryToRecordsetReader, the path DataTug reads).
func runThroughRecordsetReader(f *queryFixture, q dal.Query) adapterRun {
	f.trace.reset()
	var run adapterRun
	reader, err := f.db.ExecuteQueryToRecordsetReader(context.Background(), q)
	if err != nil {
		run.err = err
	} else {
		run.rows, run.err = readRecordsetRows(reader)
		_ = reader.Close()
	}
	run.statements = statementsSent(f)
	return run
}

// parseFixtureQuery reads a DTQL document of a fixture directory.
func parseFixtureQuery(t *testing.T, path string) dal.StructuredQuery {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	query, err := dtql.Deserialize(document)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return query
}

// fixtureRows reads the expected rows of a fixture, as JSON holds them.
func fixtureRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rows
}

// fixtureDiagnostic is the category and the path of the diagnostic a negative fixture expects
// (its .error.json holds a message too).
type fixtureDiagnostic struct{ Category, Path, Message string }

func readFixtureDiagnostic(t *testing.T, path string) fixtureDiagnostic {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want fixtureDiagnostic
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return want
}

// diagnosticOf is the category and the path of the structured diagnostic an error carries
// (a query or a join validation error), and whether it carries one.
func diagnosticOf(err error) (fixtureDiagnostic, bool) {
	var query *dal.QueryValidationError
	if errors.As(err, &query) {
		return fixtureDiagnostic{Category: query.Category, Path: query.Path, Message: query.Message}, true
	}
	var join *dal.JoinValidationError
	if errors.As(err, &join) {
		return fixtureDiagnostic{Category: join.Category, Path: join.Path, Message: join.Message}, true
	}
	return fixtureDiagnostic{}, false
}

// matchesDiagnostic says whether err is the diagnostic a negative fixture expects.
func matchesDiagnostic(err error, want fixtureDiagnostic) bool {
	got, ok := diagnosticOf(err)
	return ok && got.Category == want.Category && got.Path == want.Path
}

func countJoins(statement string) int { return strings.Count(statement, " JOIN ") }

// The vendored fixture directories are those of the dalgo this package is built with, and
// are as their manifests say; a bump of dalgo fails the version check until the directories
// are vendored again on purpose (TestVendoredFixturesAreTheDalgoModulesAtTheRequiredVersion
// runs the same check without a server, for both directories at once; this is the join
// fixtures' own, kept beside the cases it guards).
func TestFixturesJoinsAreVendoredWithTheirDigests(t *testing.T) {
	for _, problem := range vendoredProblems(filepath.Join("testdata", "joins"), dalgoModule, requiredDalgoVersion(t)) {
		t.Error(problem)
	}
}

// The join fixtures a test runs. The positive ones run on the server (joins is the number of
// JOINs the one statement holds); the negative ones are refused by the parser. A fixture in
// testdata/joins that is in neither list is run by no test, and TestFixturesEveryJoinFixtureIsRunByATest
// fails for it.
var joinCases = []struct {
	name  string
	joins int
}{
	{"chinook-nested", 2},
	{"chinook-hinted", 3},
	{"chinook-wildcard", 2},
}

var joinNegativeFixtures = []string{"forward-alias", "unknown-alias"}

func joinFixtureNames() []string {
	names := append([]string(nil), joinNegativeFixtures...)
	for _, tc := range joinCases {
		names = append(names, tc.name)
	}
	return names
}

// Every DTQL document in testdata/joins is run by a test: a fixture added by vendoring dalgo
// again is not left unrun.
func TestFixturesEveryJoinFixtureIsRunByATest(t *testing.T) {
	problems, err := unrunFixtures(filepath.Join("testdata", "joins"), joinFixtureNames())
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

// The join fixtures' negative cases are refused by the parser with the fixture's diagnostic:
// no server is involved, and they are the half of the join corpus that is about what is
// never sent.
func TestFixturesJoinNegativeCasesAreRefusedWithTheFixturesDiagnostic(t *testing.T) {
	for _, name := range joinNegativeFixtures {
		t.Run(name, func(t *testing.T) {
			document, err := os.ReadFile(filepath.Join("testdata", "joins", name+".dtql.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			want := readFixtureDiagnostic(t, filepath.Join("testdata", "joins", name+".error.json"))
			_, err = dtql.Deserialize(document)
			if !matchesDiagnostic(err, want) {
				got, _ := diagnosticOf(err)
				t.Errorf("error = %v (%+v), want the diagnostic %s at %s", err, got, want.Category, want.Path)
			}
		})
	}
}

// chinookDDL is the Chinook fragment the join fixtures read (three tables, three invoices),
// written for PostgreSQL by hand: quoted mixed-case names as DataTug's databases have them,
// and every text column COLLATE "C", so that ORDER BY compares the bytes as the fixtures'
// expected rows do. The fixtures name the schema main, which a PostgreSQL database has not;
// the fixture's schema is that, and its search_path.
var chinookDDL = []string{
	`CREATE TABLE "Invoice" ("InvoiceId" integer PRIMARY KEY, "CustomerId" integer NOT NULL, "Total" double precision NOT NULL)`,
	`CREATE TABLE "Customer" ("CustomerId" integer PRIMARY KEY, "FirstName" text COLLATE "C" NOT NULL, "SupportRepId" integer)`,
	`CREATE TABLE "Employee" ("EmployeeId" integer PRIMARY KEY, "FirstName" text COLLATE "C" NOT NULL)`,
	`INSERT INTO "Customer" ("CustomerId", "FirstName", "SupportRepId") VALUES (10, 'Ada', 100), (20, 'Bea', NULL)`,
	`INSERT INTO "Employee" ("EmployeeId", "FirstName") VALUES (100, 'Evan')`,
	`INSERT INTO "Invoice" ("InvoiceId", "CustomerId", "Total") VALUES (1, 10, 12.5), (2, 10, 8.0), (3, 20, 4.0)`,
}

// compareJoinRun says how the run of a join fixture differs from the fixture: an error, a
// number of statements or of JOINs that is not one statement with the joins the fixture
// has, and rows that are not the fixture's. It returns the kinds of difference, a sentence
// for each, and the error of normalizing the rows.
func compareJoinRun(run adapterRun, want []map[string]any, joins int) (observed, notes []string, err error) {
	if run.err != nil {
		return []string{differsInError}, []string{"the read failed: " + run.err.Error()}, nil
	}
	if len(run.statements) != 1 || countJoins(run.statements[0]) != joins {
		observed = append(observed, differsInStatements)
		notes = append(notes, fmt.Sprintf("%d statements, want one with %d JOINs", len(run.statements), joins))
	}
	got, err := normalizeRows(run.rows)
	if err != nil {
		return nil, nil, err
	}
	if difference := rowDifference(got, want); difference != "" {
		observed = append(observed, differsInRows)
		notes = append(notes, "rows: "+difference)
	}
	return observed, notes, nil
}

// checkAgainstDivergences holds a case to testdata/postgres-divergences.json: the kinds in
// which it differs from its fixture are the kinds the file lists for it (none when it has no
// entry), and a listed case does what its entry says it does.
func checkAgainstDivergences(t *testing.T, listed map[string]divergence, key string, observed []string, run adapterRun) {
	t.Helper()
	var entry *divergence
	if d, ok := listed[key]; ok {
		entry = &d
	}
	if verdict := divergenceVerdict(key, observed, entry); verdict != "" {
		t.Error(verdict)
		return
	}
	if entry != nil {
		for _, problem := range divergenceDetails(*entry, run.err, run.statements) {
			t.Error(problem)
		}
	}
}

// Each join fixture runs as one statement in the server, and its rows are the fixture's. The
// chinook-nested document is a join of three tables, one inside the other, with a filter,
// an order and a limit; chinook-hinted is the same with algorithm hints and a second join,
// which the server plans as it likes and which returns the same rows; chinook-wildcard
// selects a wildcard of one source with exclusions. The statement is asserted with the
// statement recorder: the catalog lookup, then exactly one statement, which holds the
// joins. Each runs through both public reads, the records reader and the recordset
// reader (the path DataTug reads), and must be the same through either.
func TestFixturesIntegration_JoinsRunAsOneStatementOnTheServer(t *testing.T) {
	f := openQueryFixture(t, "main", IdentifierExact, chinookDDL)
	listed := loadDivergences(t)
	nestedRows := fixtureRows(t, filepath.Join("testdata", "joins", "chinook-nested.rows.json"))
	// The wildcard fixture has no rows file of its own: its rows are those of the nested
	// join with the wildcard's columns in the place of customer (the fixture's exclusions
	// leave FirstName), as dalgo2sqlite asserts them.
	wildcardRows := []map[string]any{
		{"invoice_id": 3.0, "FirstName": "Bea", "employee": nil},
		{"invoice_id": 2.0, "FirstName": "Ada", "employee": "Evan"},
		{"invoice_id": 1.0, "FirstName": "Ada", "employee": "Evan"},
	}
	wants := map[string][]map[string]any{"chinook-nested": nestedRows, "chinook-hinted": nestedRows, "chinook-wildcard": wildcardRows}
	seen := map[string]bool{}
	for _, tc := range joinCases {
		seen[tc.name] = true
		want, known := wants[tc.name]
		if !known {
			t.Fatalf("%s: no rows are expected of it: add them", tc.name)
		}
		query := parseFixtureQuery(t, filepath.Join("testdata", "joins", tc.name+".dtql.yaml"))
		for _, reader := range []struct {
			label string
			run   func(*queryFixture, dal.Query) adapterRun
		}{{"", runThroughAdapter}, {"recordset reader", runThroughRecordsetReader}} {
			name := tc.name
			if reader.label != "" {
				name += "/" + strings.ReplaceAll(reader.label, " ", "_")
			}
			t.Run(name, func(t *testing.T) {
				run := reader.run(f, query)
				observed, notes, err := compareJoinRun(run, want, tc.joins)
				if err != nil {
					t.Fatalf("rows: %v", err)
				}
				for i, statement := range run.statements {
					t.Logf("statement %d of %d: %s", i+1, len(run.statements), statement)
				}
				t.Logf("%s differs in %v %s", name, observed, strings.Join(notes, "; "))
				checkAgainstDivergences(t, listed, "joins/"+tc.name, observed, run)
			})
		}
	}
	for _, key := range unusedDivergences(listed, "joins", seen) {
		t.Errorf("%s is listed in testdata/postgres-divergences.json and the join fixtures have no such case", key)
	}
}

// subqueryColumn is a column of the subquery fixtures' PostgreSQL tables.
type subqueryColumn struct{ name, sqlType string }

// subqueryTables is the schema of the subquery fixtures (testdata/subqueries/schema.json) for
// PostgreSQL, by hand: the columns in the order of the fixture's schema, whole numbers
// as integer, the amounts as double precision and every text COLLATE "C". Customer and
// Invoice have a primary key; the other tables have none, so their records are keyed by
// ordinal.
var subqueryTables = []struct {
	name       string
	columns    []subqueryColumn
	primaryKey string
}{
	{"Customer", []subqueryColumn{{"CustomerId", "integer"}, {"FirstName", `text COLLATE "C"`}, {"Country", `text COLLATE "C"`}}, "CustomerId"},
	{"Invoice", []subqueryColumn{{"InvoiceId", "integer"}, {"CustomerId", "integer"}, {"Total", "double precision"}, {"InvoiceDate", `text COLLATE "C"`}}, "InvoiceId"},
	{"CountryInfo", []subqueryColumn{{"Country", `text COLLATE "C"`}, {"Region", `text COLLATE "C"`}}, ""},
	{"Ledger", []subqueryColumn{{"CustomerId", "integer"}, {"Flag", `text COLLATE "C"`}}, ""},
	{"Membership", []subqueryColumn{{"SetName", `text COLLATE "C"`}, {"Value", "integer"}}, ""},
	{"Truth", []subqueryColumn{{"Name", `text COLLATE "C"`}, {"Value", "integer"}, {"SetName", `text COLLATE "C"`}}, ""},
}

// subqueryDDL is the CREATE TABLE of every table of subqueryTables.
func subqueryDDL() []string {
	var ddl []string
	for _, table := range subqueryTables {
		parts := make([]string, 0, len(table.columns)+1)
		for _, column := range table.columns {
			parts = append(parts, exactIdent(column.name)+" "+column.sqlType)
		}
		if table.primaryKey != "" {
			parts = append(parts, "PRIMARY KEY ("+exactIdent(table.primaryKey)+")")
		}
		ddl = append(ddl, "CREATE TABLE "+exactIdent(table.name)+" ("+strings.Join(parts, ", ")+")")
	}
	return ddl
}

// cellOf converts a value of the fixtures' dataset (decoded with UseNumber) for the column
// it goes into: a whole number for integer, a float for double precision, text as it is.
func cellOf(value any, sqlType string) (any, error) {
	number, isNumber := value.(json.Number)
	switch {
	case value == nil:
		return nil, nil
	case isNumber && sqlType == "integer":
		return strconv.ParseInt(number.String(), 10, 64)
	case isNumber && sqlType == "double precision":
		return strconv.ParseFloat(number.String(), 64)
	case isNumber:
		return nil, fmt.Errorf("a number for a column of type %s", sqlType)
	}
	return value, nil
}

// loadSubqueryDataset inserts the rows of the fixtures' dataset, each by a statement with
// bound values, and checks the dataset against the tables above: the fixture's schema names
// the same tables with the same columns, in the same order, and no row holds another column.
// It returns the number of rows of each table.
func loadSubqueryDataset(t *testing.T, f *queryFixture) map[string]int {
	t.Helper()
	read := func(name string, into any) {
		data, err := os.ReadFile(filepath.Join("testdata", "subqueries", name))
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var schema struct {
		Tables map[string][]string `json:"tables"`
	}
	var dataset struct {
		Tables map[string][]map[string]any `json:"tables"`
	}
	read("schema.json", &schema)
	read("dataset.json", &dataset)
	if len(schema.Tables) != len(subqueryTables) {
		t.Errorf("the fixture's schema has %d tables, the DDL here has %d: write the DDL of the new table", len(schema.Tables), len(subqueryTables))
	}
	for _, table := range subqueryTables {
		names := make([]string, len(table.columns))
		for i, column := range table.columns {
			names[i] = column.name
		}
		if want := schema.Tables[table.name]; strings.Join(want, ",") != strings.Join(names, ",") {
			t.Errorf("table %s: the fixture's schema has the columns %v, the DDL here %v", table.name, want, names)
		}
		columns := make([]string, len(names))
		placeholders := make([]string, len(names))
		for i, name := range names {
			columns[i], placeholders[i] = exactIdent(name), "$"+strconv.Itoa(i+1)
		}
		insert := "INSERT INTO " + exactIdent(table.name) + " (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
		for _, row := range dataset.Tables[table.name] {
			if len(row) != len(names) {
				t.Errorf("table %s: a row holds %d columns, want %d: %v", table.name, len(row), len(names), row)
			}
			args := make([]any, len(names))
			for i, column := range table.columns {
				cell, err := cellOf(row[column.name], column.sqlType)
				if err != nil {
					t.Fatalf("table %s, column %s: %v", table.name, column.name, err)
				}
				args[i] = cell
			}
			if _, err := f.admin.ExecContext(context.Background(), insert, args...); err != nil {
				t.Fatalf("%v\nstatement: %s\narguments: %v", err, insert, args)
			}
		}
	}
	counts := map[string]int{}
	for name, rows := range dataset.Tables {
		counts[name] = len(rows)
		if _, known := schema.Tables[name]; !known {
			t.Errorf("the dataset has the table %s, which the schema has not", name)
		}
	}
	return counts
}

// suiteCase is one case of testdata/subqueries/suite.json.
type suiteCase struct {
	Name        string `json:"name"`
	Input       string `json:"input"`
	Rows        string `json:"rows"`
	Error       string `json:"error"`
	Expectation string `json:"expectation"`
}

func readSubquerySuite(t *testing.T) []suiteCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "subqueries", "suite.json"))
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		SchemaVersion int         `json:"schemaVersion"`
		Cases         []suiteCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &suite); err != nil {
		t.Fatal(err)
	}
	if suite.SchemaVersion != 1 || len(suite.Cases) == 0 {
		t.Fatalf("suite.json: schemaVersion %d with %d cases", suite.SchemaVersion, len(suite.Cases))
	}
	return suite.Cases
}

// subqueryStatements is how many statements each case of the suite sends to the server, the
// catalog lookup left out, as DALgo's engine reads each source with plain statements (the
// same on PostgreSQL 17 and 18, which it reads alike). A case that is refused before a read
// sends none; a case with a derived source reads the table before the one it cannot read
// the columns of. A change in the engine that makes a case cheaper or dearer fails here and
// is then taken on purpose. A correlated subquery is the dear one: see correlatedReads.
var subqueryStatements = map[string]int{
	"legacy-flat-json":              1,
	"legacy-join-hints":             1,
	"scalar-zero-null-one":          4,
	"scalar-many-rows":              2,
	"scalar-many-columns":           2,
	"derived-from-and-join":         0,
	"inner-order-and-limit":         0,
	"inner-order-offset-and-limit":  0,
	"in-correlated":                 8,
	"not-in-correlated":             8,
	"exists-short-circuit":          4,
	"not-exists-correlated":         4,
	"scope-shadowing":               2,
	"scope-multi-level-correlation": 6,
	"scope-ambiguous":               2,
	"scope-forward-join":            0,
	"scope-unknown-qualifier":       0,
	"customer-invoice-composition":  1,
}

// correlatedReads are the cases whose subquery is correlated to the outer row: DALgo's engine
// reads the whole inner table, every column and no filter, once for each row of the outer
// table, after the one read of the outer table. A correlated subquery costs one statement
// and one full read of the inner table per outer row, which the server cannot shorten.
var correlatedReads = []struct{ name, outer, inner string }{
	{"in-correlated", "Truth", "Membership"},
	{"not-in-correlated", "Truth", "Membership"},
	{"exists-short-circuit", "Customer", "Invoice"},
	{"not-exists-correlated", "Customer", "Invoice"},
}

// correlatedCostProblems says what is wrong with the statements a correlated case sent: they
// must be one read of the outer table and then one unfiltered read of the whole inner table
// per row of the outer table.
func correlatedCostProblems(name, inner string, outerRows int, statements []string) []string {
	var problems []string
	if want := 1 + outerRows; len(statements) != want {
		problems = append(problems, fmt.Sprintf("%s sent %d statements, want %d: one read of the outer table and one of %s for each of its %d rows", name, len(statements), want, inner, outerRows))
	}
	for i, statement := range statements {
		if i == 0 {
			continue
		}
		if !strings.HasPrefix(statement, "SELECT * FROM "+exactIdent(inner)+" AS ") || strings.Contains(strings.ToUpper(statement), " WHERE ") {
			problems = append(problems, fmt.Sprintf("%s: statement %d is %s, want an unfiltered read of every column of %s", name, i+1, statement, inner))
		}
	}
	return problems
}

// The subquery fixtures run through DALgo's own engine over PostgreSQL leaf reads: a query with
// a subquery is never sent to the server as one, the engine reads each source with a plain
// statement and combines the rows itself, so the answer is the fixture's whatever the server.
// The tables and the rows are the fixtures' schema and dataset in PostgreSQL's types. Each
// case is compared as the join fixtures are: its rows (numbers within 1e-9) or its
// diagnostic (category and path), and the statements of a query with a subquery must each be a
// plain read. The number of statements of each case is asserted (subqueryStatements), and a
// correlated subquery is shown to cost one unfiltered read of the inner table per outer row.
//
// Two expectation files hold no query of their own and are not run: the null truth table of
// IN and NOT IN, which the runnable in-correlated and not-in-correlated cases carry, and the
// bound on the rows EXISTS reads per outer row, which the engine measures on rows it holds
// and no statement of the server shows. Only the first is named in inputlessCases; a case
// with no input that is not named there fails.
func TestFixturesIntegration_SubqueriesRunInDALgoOverLeafReads(t *testing.T) {
	f := openQueryFixture(t, "test_fx_subqueries", IdentifierExact, subqueryDDL())
	tableRows := loadSubqueryDataset(t, f)
	listed := loadDivergences(t)
	seen := map[string]bool{}
	sent := map[string][]string{}
	cases := readSubquerySuite(t)
	for _, name := range unexpectedInputless(cases) {
		t.Errorf("%s holds no query of its own and is not one of the cases that may not be run (%v): run it or name it there", name, inputlessCases)
	}
	for _, tc := range cases {
		if tc.Input == "" {
			t.Logf("%s holds no query of its own (%s): it is not run", tc.Name, tc.Expectation)
			continue
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			var observed, notes []string
			document, err := os.ReadFile(filepath.Join("testdata", "subqueries", tc.Input))
			if err != nil {
				t.Fatal(err)
			}
			query, err := dtql.Deserialize(document)
			var run adapterRun
			if err == nil {
				run = runThroughAdapter(f, query)
				err = run.err
			}
			run.err = err
			sent[tc.Name] = run.statements
			switch {
			case tc.Error != "":
				want := readFixtureDiagnostic(t, filepath.Join("testdata", "subqueries", tc.Error))
				if !matchesDiagnostic(err, want) {
					got, _ := diagnosticOf(err)
					observed = append(observed, differsInError)
					notes = append(notes, fmt.Sprintf("error %v (%+v), want %s at %s", err, got, want.Category, want.Path))
				}
			case err != nil:
				observed = append(observed, differsInError)
				notes = append(notes, "the read failed: "+err.Error())
			default:
				got, normalizeErr := normalizeRows(run.rows)
				if normalizeErr != nil {
					t.Fatalf("rows: %v", normalizeErr)
				}
				if difference := rowDifference(got, fixtureRows(t, filepath.Join("testdata", "subqueries", tc.Rows))); difference != "" {
					observed = append(observed, differsInRows)
					notes = append(notes, "rows: "+difference)
				}
			}
			if query != nil && dal.HasSubquery(query) {
				for _, statement := range run.statements {
					if notALeafRead(statement) {
						observed = append(observed, differsInStatements)
						notes = append(notes, "a statement is more than a plain read: "+statement)
						break
					}
				}
			}
			for i, statement := range run.statements {
				t.Logf("statement %d of %d: %s", i+1, len(run.statements), statement)
			}
			t.Logf("%s differs in %v %s", tc.Name, observed, strings.Join(notes, "; "))
			checkAgainstDivergences(t, listed, "subqueries/"+tc.Name, observed, run)
			want, counted := subqueryStatements[tc.Name]
			switch {
			case !counted:
				t.Errorf("%s: no statement count in subqueryStatements: the case sent %d statements", tc.Name, len(run.statements))
			case len(run.statements) != want:
				t.Errorf("%s sent %d statements, want %d (subqueryStatements): a change in how DALgo's engine reads is taken on purpose", tc.Name, len(run.statements), want)
			}
		})
	}
	for _, key := range unusedDivergences(listed, "subqueries", seen) {
		t.Errorf("%s is listed in testdata/postgres-divergences.json and the subquery suite has no such case", key)
	}
	for name := range subqueryStatements {
		if !seen[name] {
			t.Errorf("subqueryStatements has %s and the subquery suite has no case of that name that is run", name)
		}
	}
	t.Run("a correlated subquery reads the whole inner table once for each row of the outer table", func(t *testing.T) {
		for _, tc := range correlatedReads {
			statements, ran := sent[tc.name]
			if !ran {
				t.Errorf("%s: the case did not run", tc.name)
				continue
			}
			rows, known := tableRows[tc.outer]
			if !known {
				t.Fatalf("%s: the dataset has no table %s", tc.name, tc.outer)
			}
			for _, problem := range correlatedCostProblems(tc.name, tc.inner, rows, statements) {
				t.Error(problem)
			}
			t.Logf("%s: %d statements for %d outer rows: one read of %s, then one unfiltered read of %s each", tc.name, len(statements), rows, tc.outer, tc.inner)
		}
	})
}
