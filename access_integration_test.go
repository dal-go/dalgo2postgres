package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	dalrecord "github.com/dal-go/record"
)

// The access layer of DALgo over this adapter, on a real server: reads through
// access.SecureReadSession, which rewrites a query and hands it to the adapter, and denies
// before the adapter what it must not run. The layer is the same for every adapter, and the
// shared suite of dal-go/dalgo runs it here too (TestEndToEnd, query/access_conditions,
// access_field_lists and access_sources, in fold-lower mode, with two sub-tests it cannot run
// there). These tests add what the shared suite does not count: the statements a denied query
// sent to the server (none), the text and the arguments of the statements an allowed query
// sent (the values are arguments), and the table left whole after a hostile value.
//
// They run in both identifier modes, with the names spelled as the mode stores them: the
// exact mode of DataTug on a database whose tables were created with quoted mixed-case names,
// and the fold-lower mode on this package's own tables. The access layer compares names
// exactly, so the policy spells them as the query does.
//
// They run against PostgreSQL and skip unless DALGO2POSTGRES_TEST_DSN is set; CI fails when a
// TestAccessIntegration_ test skips or is missing.

// accessWorld is a fixture with a Person table whose Secret is hidden by the field lists and
// an Invoice table, in the spelling of one identifier mode.
type accessWorld struct {
	f    *queryFixture
	name func(string) string // spells a name as the mode stores it
}

const hostileCountry = `x'; DROP TABLE "Person"; --`

func openAccessWorld(t *testing.T, mode IdentifierMode) accessWorld {
	t.Helper()
	name := func(s string) string { return s }
	schema := "test_access_exact"
	if mode == IdentifierFoldLower {
		name = strings.ToLower
		schema = "test_access_fold"
	}
	q := func(s string) string { return exactIdent(name(s)) }
	person, invoice := q("Person"), q("Invoice")
	ddl := []string{
		`CREATE TABLE ` + person + ` (` + q("PersonId") + ` integer PRIMARY KEY, ` + q("Name") + ` text COLLATE "C", ` + q("Country") + ` text COLLATE "C", ` + q("Secret") + ` text COLLATE "C")`,
		`CREATE TABLE ` + invoice + ` (` + q("InvoiceId") + ` integer PRIMARY KEY, ` + q("PersonId") + ` integer, ` + q("Total") + ` double precision)`,
		`INSERT INTO ` + person + ` VALUES (1, 'Ada', 'IE', 'ada-secret'), (2, 'Bob', 'US', 'bob-secret'), (3, 'Cy', 'O''Brien', 'cy-secret'),
			(4, 'Di', $$` + hostileCountry + `$$, 'di-secret'), (5, 'Eve', 'IE', 'eve-secret')`,
		`INSERT INTO ` + invoice + ` VALUES (10, 1, 5.5), (11, 1, 6.5), (12, 2, 7.5)`,
	}
	return accessWorld{f: openQueryFixture(t, schema, mode, ddl), name: name}
}

func (w accessWorld) field(name string) dal.FieldRef { return dal.NewFieldRef("", w.name(name)) }

// persons is a query over the Person table, with its columns selected by the caller.
func (w accessWorld) persons() dal.IQueryBuilder {
	return dal.From(dal.NewRootCollectionRef(w.name("Person"), "")).NewQuery()
}

// readThrough reads q through session to its end.
func readThrough(ctx context.Context, session dal.ReadSession, q dal.Query) ([]map[string]any, error) {
	reader, err := session.ExecuteQueryToRecordsReader(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	var rows []map[string]any
	for {
		rec, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		data, _ := rec.Data().(map[string]any)
		rows = append(rows, data)
	}
}

// namesIn are the values of one column, sorted.
func namesIn(rows []map[string]any, column string) []string {
	var out []string
	for _, row := range rows {
		out = append(out, fmt.Sprint(row[column]))
	}
	sort.Strings(out)
	return out
}

// deniedBeforeTheServer asserts that err is an access denial with the code and the slot
// given, that it names the column, and that no statement reached the server.
func deniedBeforeTheServer(t *testing.T, f *queryFixture, err error, code access.ReasonCode, slot access.DecisionSlot, column string) {
	t.Helper()
	if !errors.Is(err, access.ErrAccessDenied) {
		t.Errorf("error = %v, want an access denial", err)
		return
	}
	var denied *access.DeniedError
	if !errors.As(err, &denied) {
		t.Errorf("error = %v, want a *access.DeniedError", err)
		return
	}
	if denied.Decision.Code != code || denied.Decision.Slot != slot || !reflect.DeepEqual(denied.Decision.Columns, [][]string{{column}}) {
		t.Errorf("decision = code %v, slot %v, columns %v; want code %v, slot %v, column %s",
			denied.Decision.Code, denied.Decision.Slot, denied.Decision.Columns, code, slot, column)
	}
	if sent := f.trace.sent(); len(sent) != 0 {
		t.Errorf("%d statements reached the server for a denied query, want none: %v", len(sent), sent)
	}
}

func forEachMode(t *testing.T, run func(t *testing.T, w accessWorld)) {
	t.Helper()
	testDSN(t) // a skip shows on the test, not only on its subtests
	for _, mode := range []struct {
		name string
		mode IdentifierMode
	}{{"exact mode", IdentifierExact}, {"fold-lower mode", IdentifierFoldLower}} {
		t.Run(mode.name, func(t *testing.T) { run(t, openAccessWorld(t, mode.mode)) })
	}
}

// A row condition whose variable holds a quote returns exactly the rows it permits. The
// value, a text with a quote, a semicolon, a second statement and a comment marker, reaches
// the server as a bound argument and never as text: the statement is the same whatever the
// value holds, the table is whole afterwards, and a caller's own condition with a quote is
// compared as text too.
func TestAccessIntegration_RowConditionWithAQuoteReturnsExactlyThePermittedRows(t *testing.T) {
	forEachMode(t, func(t *testing.T, w accessWorld) {
		f := w.f
		byCountry := dal.WhereField(w.name("Country"), dal.Equal, dal.NewParam("country"))
		secured := access.SecureReadSession(f.db, access.MustPolicy("persons-by-country",
			access.Collection(w.name("Person"), access.Allow(access.Query, "query-own-country").Where(byCountry))))
		as := func(country string) context.Context {
			return access.WithVariables(context.Background(), map[string]any{"country": country})
		}
		query := func() dal.IQueryBuilder { return w.persons() }
		columns := dal.Column{Expression: w.field("Name")}
		for _, tc := range []struct {
			label   string
			country string
			query   dal.StructuredQuery
			want    []string
		}{
			{"a plain country", "IE", query().SelectColumns(columns), []string{"Ada", "Eve"}},
			{"a country with a quote", "O'Brien", query().SelectColumns(columns), []string{"Cy"}},
			{"a country that is a second statement and a comment", hostileCountry, query().SelectColumns(columns), []string{"Di"}},
			{"a country nobody has", "IE'; --", query().SelectColumns(columns), nil},
			{"the caller's own condition with a quote, conjoined", "IE", query().WhereField(w.name("Name"), dal.Equal, "O'Hare").SelectColumns(columns), nil},
			{"the caller's own condition, conjoined", "IE", query().WhereField(w.name("Name"), dal.Equal, "Eve").SelectColumns(columns), []string{"Eve"}},
		} {
			f.trace.reset()
			rows, err := readThrough(as(tc.country), secured, tc.query)
			if err != nil {
				t.Errorf("%s: %v", tc.label, err)
				continue
			}
			if got := namesIn(rows, w.name("Name")); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: names = %v, want %v", tc.label, got, tc.want)
			}
			for _, statement := range statementsSent(f) {
				t.Logf("%s: %s", tc.label, statement)
				if strings.Contains(statement, "Brien") || strings.Contains(statement, "DROP") || strings.Contains(statement, "O'Hare") {
					t.Errorf("%s: a value is written into the statement: %s", tc.label, statement)
				}
			}
			var bound []string
			for _, sent := range f.trace.sent() {
				if !isCatalogStatement(sent.sql) {
					for _, arg := range valueArguments(sent.args) {
						bound = append(bound, fmt.Sprint(arg))
					}
				}
			}
			if !contains(bound, tc.country) {
				t.Errorf("%s: arguments = %v, want the variable %q among them", tc.label, bound, tc.country)
			}
		}
		// The statement of the condition does not change with the value it holds.
		var shapes []string
		for _, country := range []string{"IE", "O'Brien", hostileCountry} {
			f.trace.reset()
			if _, err := readThrough(as(country), secured, query().SelectColumns(columns)); err != nil {
				t.Fatal(err)
			}
			shapes = append(shapes, strings.Join(statementsSent(f), ";"))
		}
		if shapes[0] != shapes[1] || shapes[1] != shapes[2] {
			t.Errorf("the statement depends on the value: %q", shapes)
		}
		var n int
		if err := f.admin.QueryRowContext(context.Background(), `SELECT count(*) FROM `+exactIdent(w.name("Person"))).Scan(&n); err != nil || n != 5 {
			t.Errorf("the Person table holds %d rows after the hostile values (error %v), want 5", n, err)
		}
	})
}

// fieldListSession is a secured session whose policy lists the fields of Person that may be
// read, without Secret, and allows Invoice whole.
func (w accessWorld) fieldListSession() dal.ReadSession {
	return access.SecureReadSession(w.f.db, access.MustPolicy("persons-fields",
		access.Collection(w.name("Person"), access.Allow(access.Query, "query-allowed-fields").Fields(w.name("PersonId"), w.name("Name"), w.name("Country"))),
		access.Collection(w.name("Invoice"), access.Allow(access.Query, "query-invoices")),
	))
}

// The alias of a hidden field is denied, and so is a projection whose every column is
// refused, with no statement sent. The positive control is the same policy reading an allowed
// column; an allowed column under the hidden field's own name never returns the stored field.
func TestAccessIntegration_HiddenFieldUnderAnAliasAndAnEmptyProjectionAreDenied(t *testing.T) {
	forEachMode(t, func(t *testing.T, w accessWorld) {
		f := w.f
		session := w.fieldListSession()
		ctx := context.Background()

		f.trace.reset()
		rows, err := readThrough(ctx, session, w.persons().OrderBy(dal.AscendingField(w.name("PersonId"))).SelectColumns(dal.Column{Expression: w.field("Name")}))
		if err != nil || !reflect.DeepEqual(namesIn(rows, w.name("Name")), []string{"Ada", "Bob", "Cy", "Di", "Eve"}) {
			t.Errorf("an allowed column: rows = %v, error %v; want the five names (the policy is not denying everything)", rows, err)
		}

		for _, tc := range []struct {
			label  string
			column []dal.Column
			hidden string
		}{
			{"a hidden field under an allowed alias", []dal.Column{{Alias: w.name("Name"), Expression: w.field("Secret")}}, w.name("Secret")},
			{"a hidden field under a new alias", []dal.Column{{Alias: "label", Expression: w.field("Secret")}}, w.name("Secret")},
			{"a hidden field beside an allowed one, under an allowed alias", []dal.Column{{Expression: w.field("Name")}, {Alias: w.name("Country"), Expression: w.field("Secret")}}, w.name("Secret")},
		} {
			f.trace.reset()
			_, err := readThrough(ctx, session, w.persons().SelectColumns(tc.column...))
			t.Logf("%s: %v", tc.label, err)
			deniedBeforeTheServer(t, f, err, access.CodeColumnDenied, access.DecisionSlotFields, tc.hidden)
		}

		// A projection whose every column is refused.
		f.trace.reset()
		_, err = readThrough(ctx, session, w.persons().SelectColumns(dal.Column{Expression: w.field("Secret")}))
		t.Logf("every column refused: %v", err)
		deniedBeforeTheServer(t, f, err, access.CodeColumnDenied, access.DecisionSlotFields, w.name("Secret"))
		f.trace.reset()
		_, err = readThrough(ctx, session, w.persons().SelectColumns(dal.Column{Expression: w.field("Secret")}, dal.Column{Alias: "where", Expression: w.field("Secret")}))
		deniedBeforeTheServer(t, f, err, access.CodeColumnDenied, access.DecisionSlotFields, w.name("Secret"))

		// An allowed column under the hidden field's own name: what comes back under it is
		// the allowed column's own value, never the stored secret.
		f.trace.reset()
		rows, err = readThrough(ctx, session, w.persons().SelectColumns(dal.Column{Alias: w.name("Secret"), Expression: w.field("Name")}))
		if err != nil {
			t.Fatalf("an allowed column under the hidden name: %v", err)
		}
		for _, row := range rows {
			if value, ok := row[w.name("Secret")].(string); ok && strings.HasSuffix(value, "-secret") {
				t.Errorf("a row carries the stored secret %q under the hidden name", value)
			}
		}
		if len(rows) != 5 {
			t.Errorf("rows = %v, want the five names under the alias", rows)
		}
	})
}

// A hidden field named in a join's ON condition, at any depth, in a filter, in an order and in
// a scan order is denied before the adapter: no statement of any kind reaches the server, the
// catalog lookup included. The control is the same shapes over the listed fields, which run.
// The last five cases pin a limit of dal-go/dalgo, not a leak: a field of the joined source is
// held to the listed source's list in the select list, WHERE, GROUP BY, HAVING and ORDER BY.
func TestAccessIntegration_HiddenFieldInAJoinOrAScanOrderIsDeniedWithNoStatement(t *testing.T) {
	forEachMode(t, func(t *testing.T, w accessWorld) {
		f := w.f
		session := w.fieldListSession()
		ctx := context.Background()
		person := dal.NewRootCollectionRef(w.name("Person"), "p")
		invoice := func(alias string) dal.CollectionRef { return dal.NewRootCollectionRef(w.name("Invoice"), alias) }
		personField := func(name string) dal.Expression { return dal.NewFieldRef("p", w.name(name)) }
		invoiceField := func(alias, name string) dal.Expression { return dal.NewFieldRef(alias, w.name(name)) }
		on := func(left, right dal.Expression) dal.Condition { return dal.NewComparison(left, dal.Equal, right) }
		selectedName := dal.Column{Expression: personField("Name")}
		selectName := func(from dal.FromSource) dal.StructuredQuery {
			return from.NewQuery().SelectColumns(selectedName)
		}
		// joined is Person joined to Invoice on the listed fields, which the list lets through.
		joined := func() dal.FromSource {
			return dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(personField("PersonId"), invoiceField("i", "PersonId"))))
		}

		for _, tc := range []struct {
			label  string
			query  dal.StructuredQuery
			slot   access.DecisionSlot
			hidden string
			// fixed is set for a case that pins a limit: what to assert once the query runs.
			fixed string
		}{
			{"the hidden field in the ON condition",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(personField("Secret"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotWhere, "Secret", ""},
			{"the hidden field on the right of the ON condition",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(invoiceField("i", "PersonId"), personField("Secret"))))),
				access.DecisionSlotWhere, "Secret", ""},
			{"the hidden field compared with a constant in the ON condition",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(personField("Secret"), dal.NewConstant("x"))))),
				access.DecisionSlotWhere, "Secret", ""},
			{"the hidden field at depth two",
				selectName(dal.From(person).Join(dal.NewJoinedFrom(
					dal.From(invoice("i")).Join(dal.NewJoinedSource(invoice("j"), dal.JoinInner, on(personField("Secret"), invoiceField("j", "PersonId")))),
					dal.JoinInner, on(personField("PersonId"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotWhere, "Secret", ""},
			{"the hidden field at depth three",
				selectName(dal.From(person).Join(dal.NewJoinedFrom(
					dal.From(invoice("i")).Join(dal.NewJoinedFrom(
						dal.From(invoice("j")).Join(dal.NewJoinedSource(invoice("k"), dal.JoinInner, on(personField("Secret"), invoiceField("k", "PersonId")))),
						dal.JoinInner, on(invoiceField("i", "InvoiceId"), invoiceField("j", "InvoiceId")))),
					dal.JoinInner, on(personField("PersonId"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotWhere, "Secret", ""},
			{"the hidden field in the scan order of the source",
				selectName(dal.From(person.WithScan(5, dal.AscendingField(w.name("Secret"))))),
				access.DecisionSlotFields, "Secret", ""},
			{"the hidden field in the scan order of a joined source's own query, qualified by the listed source",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i").WithScan(5, dal.Descending(personField("Secret"))), dal.JoinInner,
					on(personField("PersonId"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotFields, "Secret", ""},
			{"a filter on the hidden field",
				w.persons().WhereField(w.name("Secret"), dal.Equal, "ada-secret").SelectColumns(dal.Column{Expression: w.field("Name")}),
				access.DecisionSlotWhere, "Secret", ""},
			{"an order by the hidden field",
				w.persons().OrderBy(dal.AscendingField(w.name("Secret"))).SelectColumns(dal.Column{Expression: w.field("Name")}),
				access.DecisionSlotFields, "Secret", ""},
			// KNOWN LIMIT of dal-go/dalgo (a field list is held to the base source; it fails
			// closed and does not leak): the list of the listed source Person is applied to
			// every field of the select list, WHERE, GROUP BY, HAVING and ORDER BY of a join,
			// whatever the source its qualifier names (the access layer attributes only a
			// join's ON condition and a scan order to a source). So a column of the joined
			// source Invoice is denied by Person's list when the list does not hold its name,
			// though Invoice is allowed whole. When the access layer attributes these clauses
			// by source, each case below FAILS with the message in its last field, which says
			// what to assert instead.
			{"a field of the joined source in the order, which the listed source's list does not hold",
				joined().NewQuery().OrderBy(dal.Ascending(invoiceField("i", "InvoiceId"))).SelectColumns(selectedName),
				access.DecisionSlotFields, "InvoiceId",
				"the join runs: assert the names of the persons in the order of their invoices (Ada, Ada, Bob) and one statement with the JOIN"},
			{"a field of the joined source in the select list, which the listed source's list does not hold",
				joined().NewQuery().SelectColumns(dal.Column{Expression: invoiceField("i", "Total")}),
				access.DecisionSlotFields, "Total",
				"the join runs: assert the three totals (5.5, 6.5 and 7.5) and one statement with the JOIN"},
			{"a field of the joined source in WHERE, which the listed source's list does not hold",
				joined().NewQuery().Where(dal.NewComparison(invoiceField("i", "Total"), dal.GreaterThen, dal.NewConstant(1))).SelectColumns(selectedName),
				access.DecisionSlotWhere, "Total",
				"the join runs: assert the names Ada, Ada and Bob (all three invoices total more than 1) and one statement with the JOIN"},
			// A valid grouped query: the group key is the joined source's field and the only
			// column selected is COUNT(*), so the one field the list could refuse is the key.
			{"a field of the joined source in GROUP BY, which the listed source's list does not hold",
				joined().NewQuery().GroupBy(invoiceField("i", "Total")).
					SelectColumns(dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"}),
				access.DecisionSlotFields, "Total",
				"the grouped query runs: assert three groups (the totals 5.5, 6.5 and 7.5) of one invoice each and one statement with the JOIN and the GROUP BY"},
			{"a field of the joined source in HAVING, which the listed source's list does not hold",
				joined().NewQuery().GroupBy(personField("Name")).
					Having(dal.NewComparison(dal.NewAggregate(dal.SUM, false, invoiceField("i", "Total")), dal.GreaterThen, dal.NewConstant(1))).
					SelectColumns(selectedName),
				access.DecisionSlotWhere, "Total",
				"the grouped query runs: assert the names Ada (total 12) and Bob (total 7.5) and one statement with the JOIN and the HAVING"},
		} {
			f.trace.reset()
			_, err := readThrough(ctx, session, tc.query)
			t.Logf("%s: %v", tc.label, err)
			if tc.fixed != "" && err == nil {
				t.Errorf("%s: the query ran, where the known limit of dal-go/dalgo denies it (code column_denied, slot %v, column %s, no statement): dal-go/dalgo now attributes this clause to the source its field names; %s; then delete this case and the README's known limit",
					tc.label, tc.slot, w.name(tc.hidden), tc.fixed)
				continue
			}
			deniedBeforeTheServer(t, f, err, access.CodeColumnDenied, tc.slot, w.name(tc.hidden))
		}

		// The controls: the same joins over the listed fields run, in one statement.
		f.trace.reset()
		rows, err := readThrough(ctx, session, dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(personField("PersonId"), invoiceField("i", "PersonId")))).
			NewQuery().OrderBy(dal.Ascending(personField("PersonId"))).
			SelectColumns(dal.Column{Expression: personField("Name"), Alias: "name"}))
		// (A field of the joined source in the select list, WHERE, GROUP BY, HAVING or ORDER BY
		// is held to the list of the listed source as well and is denied: see the cases above.
		// The control therefore selects and orders by fields of the listed source only.)
		if err != nil {
			t.Fatalf("a join over the listed fields: %v\nstatements: %v", err, statementsSent(f))
		}
		if got := namesIn(rows, "name"); !reflect.DeepEqual(got, []string{"Ada", "Ada", "Bob"}) {
			t.Errorf("names = %v, want Ada, Ada, Bob", got)
		}
		if statements := statementsSent(f); len(statements) != 1 || countJoins(statements[0]) != 1 {
			t.Errorf("statements = %v, want one with the JOIN", statements)
		}
	})
}

// The direction in which a leak would be possible: a field list on the joined source. A joined
// source carries no field list of its own in this version of dal-go/dalgo, so a rule that lists
// the fields of one is refused before any query is run ("field rules on a joined source are not
// supported in this version"), whatever the query selects. Both cases fail closed with no
// statement, the catalog lookup included: the hidden Total of the joined Invoice is not read
// through the join, and neither are the fields the list allows, which is the limit. The policy is
// the one of the other tests with the lists swapped: Person allowed whole, Invoice held to
// InvoiceId and PersonId. When dal-go/dalgo supports field rules on a joined source the second
// case FAILS and says what to assert instead.
func TestAccessIntegration_AFieldListOnAJoinedSourceIsRefusedWithNoStatement(t *testing.T) {
	forEachMode(t, func(t *testing.T, w accessWorld) {
		f := w.f
		ctx := context.Background()
		session := access.SecureReadSession(f.db, access.MustPolicy("invoice-fields",
			access.Collection(w.name("Person"), access.Allow(access.Query, "query-persons")),
			access.Collection(w.name("Invoice"), access.Allow(access.Query, "query-allowed-invoice-fields").Fields(w.name("InvoiceId"), w.name("PersonId"))),
		))
		person := dal.NewRootCollectionRef(w.name("Person"), "p")
		invoice := dal.NewRootCollectionRef(w.name("Invoice"), "i")
		joined := func() dal.FromSource {
			return dal.From(person).Join(dal.NewJoinedSource(invoice, dal.JoinInner,
				dal.NewComparison(dal.NewFieldRef("p", w.name("PersonId")), dal.Equal, dal.NewFieldRef("i", w.name("PersonId")))))
		}
		// refused asserts an access denial and no statement, and returns the denial.
		refused := func(label string, err error) *access.DeniedError {
			t.Helper()
			t.Logf("%s: %v", label, err)
			var denied *access.DeniedError
			if !errors.Is(err, access.ErrAccessDenied) || !errors.As(err, &denied) {
				t.Errorf("%s: error = %v, want an access denial", label, err)
			}
			if sent := f.trace.sent(); len(sent) != 0 {
				t.Errorf("%s: %d statements reached the server for a denied query, want none: %v", label, len(sent), sent)
			}
			return denied
		}

		// The hidden field of the joined source, selected: the leak that must not happen, now and
		// whatever dal-go/dalgo does with a field rule on a joined source later.
		f.trace.reset()
		_, err := readThrough(ctx, session, joined().NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("i", w.name("Total"))}))
		_ = refused("the hidden field of the joined source in the select list", err)

		// A query that names only fields the lists allow is refused too: the rule is what the
		// access layer cannot apply, not the query.
		f.trace.reset()
		rows, err := readThrough(ctx, session, joined().NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("p", w.name("PersonId")))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("p", w.name("Name"))}, dal.Column{Expression: dal.NewFieldRef("i", w.name("InvoiceId")), Alias: "invoice"}))
		if err == nil {
			t.Errorf("the allowed fields of the joined source: the query ran and returned %d rows, where the known limit of dal-go/dalgo refuses a field rule on a joined source before any statement: dal-go/dalgo now supports it; assert here the rows (Ada with invoices 10 and 11, Bob with 12) and one statement with the JOIN, assert that the hidden Total is denied with the code column_denied, the slot fields, the column Total and no statement, and delete the README's known limit", len(rows))
			return
		}
		if denied := refused("the allowed fields of the joined source", err); denied != nil && !strings.Contains(denied.Decision.Explanation, "field rules on a joined source") {
			t.Errorf("the allowed fields of the joined source: explanation = %q, want the refusal of a field rule on a joined source", denied.Decision.Explanation)
		}
	})
}

// A session built with no policy denies every request: a query, a point read, an existence
// check, and none reaches the server. The same for a list whose only policy is nil. The
// control is a session with a policy that allows the read.
func TestAccessIntegration_SessionWithNoPolicyIsDenied(t *testing.T) {
	forEachMode(t, func(t *testing.T, w accessWorld) {
		f := w.f
		ctx := context.Background()
		key := dalrecord.NewKeyWithID(w.name("Person"), 1)
		for _, tc := range []struct {
			label   string
			session dal.ReadSession
		}{
			{"no policy", access.SecureReadSession(f.db)},
			{"a nil policy", access.SecureReadSession(f.db, nil)},
		} {
			f.trace.reset()
			_, err := readThrough(ctx, tc.session, w.persons().SelectColumns(dal.Column{Expression: w.field("Name")}))
			if !errors.Is(err, access.ErrAccessDenied) {
				t.Errorf("%s: a query: error = %v, want an access denial", tc.label, err)
			}
			if exists, err := tc.session.Exists(ctx, key); !errors.Is(err, access.ErrAccessDenied) || exists {
				t.Errorf("%s: Exists = %v, %v; want a denial", tc.label, exists, err)
			}
			rec := dalrecord.NewRecordWithData(key, map[string]any{})
			if err := tc.session.Get(ctx, rec); !errors.Is(err, access.ErrAccessDenied) {
				t.Errorf("%s: Get: error = %v, want an access denial", tc.label, err)
			}
			if sent := f.trace.sent(); len(sent) != 0 {
				t.Errorf("%s: %d statements reached the server, want none: %v", tc.label, len(sent), sent)
			}
		}
		allowed := access.SecureReadSession(f.db, access.MustPolicy("read-persons",
			access.Collection(w.name("Person"), access.Allow(access.Query, "query-persons"))))
		rows, err := readThrough(ctx, allowed, w.persons().OrderBy(dal.AscendingField(w.name("PersonId"))).SelectColumns(dal.Column{Expression: w.field("Name")}))
		if err != nil || len(rows) != 5 {
			t.Errorf("a session with a policy that allows the read: rows = %v, error %v; want five rows", rows, err)
		}
	})
}

// A conditional rule whose condition does not hold falls through to an unconditional allow
// when the policy has one, so a policy that narrows with a condition and then allows without
// one is decided by the wider rule: the caller who is narrowed to IE reads every row. That is
// how the access layer decides for every adapter, and it is why the README tells a policy that
// narrows with a condition not to leave a wider allow behind it. The control is the same
// condition alone, which narrows.
func TestAccessIntegration_UnconditionalAllowBehindAConditionDecidesTheRow(t *testing.T) {
	forEachMode(t, func(t *testing.T, w accessWorld) {
		f := w.f
		byCountry := dal.WhereField(w.name("Country"), dal.Equal, dal.NewParam("country"))
		narrow := access.Allow(access.Query, "query-own-country").Where(byCountry)
		wide := access.Allow(access.Query, "query-everything")
		alone := access.SecureReadSession(f.db, access.MustPolicy("only-the-condition", access.Collection(w.name("Person"), narrow)))
		behind := access.SecureReadSession(f.db, access.MustPolicy("condition-then-everything", access.Collection(w.name("Person"), narrow, wide)))
		ie := access.WithVariables(context.Background(), map[string]any{"country": "IE"})
		names := dal.Column{Expression: w.field("Name")}

		rows, err := readThrough(ie, alone, w.persons().SelectColumns(names))
		if err != nil || !reflect.DeepEqual(namesIn(rows, w.name("Name")), []string{"Ada", "Eve"}) {
			t.Errorf("the condition alone: rows = %v, error %v; want Ada and Eve", rows, err)
		}
		f.trace.reset()
		rows, err = readThrough(ie, behind, w.persons().SelectColumns(names))
		t.Logf("the condition with an unconditional allow behind it: %v, %v; statements: %v", namesIn(rows, w.name("Name")), err, statementsSent(f))
		if err != nil || !reflect.DeepEqual(namesIn(rows, w.name("Name")), []string{"Ada", "Bob", "Cy", "Di", "Eve"}) {
			t.Errorf("the condition with an unconditional allow behind it: rows = %v, error %v; want all five rows, decided by the wider rule", rows, err)
		}
		// With no variable the condition cannot be evaluated: alone it denies, and the wider
		// rule behind it still allows.
		_, err = readThrough(context.Background(), alone, w.persons().SelectColumns(names))
		if !errors.Is(err, access.ErrAccessDenied) {
			t.Errorf("the condition alone, with no variable: error = %v, want an access denial", err)
		}
	})
}
