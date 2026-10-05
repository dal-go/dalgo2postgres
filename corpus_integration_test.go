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

// The join fixtures' negative cases are refused by the parser with the fixture's diagnostic:
// no server is involved, and they are the half of the join corpus that is about what is
// never sent.
func TestFixturesJoinNegativeCasesAreRefusedWithTheFixturesDiagnostic(t *testing.T) {
	for _, name := range []string{"forward-alias", "unknown-alias"} {
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

// Each join fixture runs as one statement in the server, and its rows are the fixture's. The
// chinook-nested document is a join of three tables, one inside the other, with a filter,
// an order and a limit; chinook-hinted is the same with algorithm hints and a second join,
// which the server plans as it likes and which returns the same rows; chinook-wildcard
// selects a wildcard of one source with exclusions. The statement is asserted with the
// statement recorder: the catalog lookup, then exactly one statement, which holds the
// joins.
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
	seen := map[string]bool{}
	for _, tc := range []struct {
		name  string
		want  []map[string]any
		joins int // JOINs the one statement holds
	}{
		{"chinook-nested", nestedRows, 2},
		{"chinook-hinted", nestedRows, 3},
		{"chinook-wildcard", wildcardRows, 2},
	} {
		seen[tc.name] = true
		t.Run(tc.name, func(t *testing.T) {
			query := parseFixtureQuery(t, filepath.Join("testdata", "joins", tc.name+".dtql.yaml"))
			run := runThroughAdapter(f, query)
			var observed []string
			var notes []string
			switch {
			case run.err != nil:
				observed = append(observed, differsInError)
				notes = append(notes, "the read failed: "+run.err.Error())
			default:
				if len(run.statements) != 1 || countJoins(run.statements[0]) != tc.joins {
					observed = append(observed, differsInStatements)
					notes = append(notes, fmt.Sprintf("%d statements, want one with %d JOINs", len(run.statements), tc.joins))
				}
				got, err := normalizeRows(run.rows)
				if err != nil {
					t.Fatalf("rows: %v", err)
				}
				if difference := rowDifference(got, tc.want); difference != "" {
					observed = append(observed, differsInRows)
					notes = append(notes, "rows: "+difference)
				}
			}
			for i, statement := range run.statements {
				t.Logf("statement %d of %d: %s", i+1, len(run.statements), statement)
			}
			t.Logf("%s differs in %v %s", tc.name, observed, strings.Join(notes, "; "))
			key := "joins/" + tc.name
			var entry *divergence
			if d, ok := listed[key]; ok {
				entry = &d
			}
			if verdict := divergenceVerdict(key, observed, entry); verdict != "" {
				t.Error(verdict)
			}
		})
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
func loadSubqueryDataset(t *testing.T, f *queryFixture) {
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
	for name := range dataset.Tables {
		if _, known := schema.Tables[name]; !known {
			t.Errorf("the dataset has the table %s, which the schema has not", name)
		}
	}
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

// notALeafRead says whether a statement is more than a plain read of one source: it holds a
// join, a subquery or an EXISTS, which DALgo's own engine never sends.
func notALeafRead(statement string) bool {
	upper := strings.ToUpper(statement)
	return strings.Contains(upper, " JOIN ") || strings.Contains(upper, "(SELECT") || strings.Contains(upper, "EXISTS")
}

// The subquery fixtures run through DALgo's own engine over PostgreSQL leaf reads: a query with
// a subquery is never sent to the server as one, the engine reads each source with a plain
// statement and combines the rows itself, so the answer is the fixture's whatever the server.
// The tables and the rows are the fixtures' schema and dataset in PostgreSQL's types. Each
// case is compared as the join fixtures are: its rows (numbers within 1e-9) or its
// diagnostic (category and path), and the statements of a query with a subquery must each be a
// plain read.
//
// Two expectation files hold no query of their own and are not run: the null truth table of
// IN and NOT IN, which the runnable in-correlated and not-in-correlated cases carry, and the
// bound on the rows EXISTS reads per outer row, which the engine measures on rows it holds
// and no statement of the server shows.
func TestFixturesIntegration_SubqueriesRunInDALgoOverLeafReads(t *testing.T) {
	f := openQueryFixture(t, "test_fx_subqueries", IdentifierExact, subqueryDDL())
	loadSubqueryDataset(t, f)
	listed := loadDivergences(t)
	seen := map[string]bool{}
	for _, tc := range readSubquerySuite(t) {
		if tc.Input == "" {
			t.Logf("%s holds no query of its own (%s): it is not run", tc.Name, tc.Expectation)
			continue
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			var observed, notes []string
			var statements []string
			document, err := os.ReadFile(filepath.Join("testdata", "subqueries", tc.Input))
			if err != nil {
				t.Fatal(err)
			}
			query, err := dtql.Deserialize(document)
			var run adapterRun
			if err == nil {
				run = runThroughAdapter(f, query)
				err, statements = run.err, run.statements
			}
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
				for _, statement := range statements {
					if notALeafRead(statement) {
						observed = append(observed, differsInStatements)
						notes = append(notes, "a statement is more than a plain read: "+statement)
						break
					}
				}
			}
			for i, statement := range statements {
				t.Logf("statement %d of %d: %s", i+1, len(statements), statement)
			}
			t.Logf("%s differs in %v %s", tc.Name, observed, strings.Join(notes, "; "))
			key := "subqueries/" + tc.Name
			var entry *divergence
			if d, ok := listed[key]; ok {
				entry = &d
			}
			if verdict := divergenceVerdict(key, observed, entry); verdict != "" {
				t.Error(verdict)
			}
		})
	}
	for _, key := range unusedDivergences(listed, "subqueries", seen) {
		t.Errorf("%s is listed in testdata/postgres-divergences.json and the subquery suite has no such case", key)
	}
}
