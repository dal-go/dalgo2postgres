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
		selectName := func(from dal.FromSource) dal.StructuredQuery {
			return from.NewQuery().SelectColumns(dal.Column{Expression: personField("Name")})
		}

		for _, tc := range []struct {
			label  string
			query  dal.StructuredQuery
			slot   access.DecisionSlot
			hidden string
		}{
			{"the hidden field in the ON condition",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(personField("Secret"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotWhere, "Secret"},
			{"the hidden field on the right of the ON condition",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(invoiceField("i", "PersonId"), personField("Secret"))))),
				access.DecisionSlotWhere, "Secret"},
			{"the hidden field compared with a constant in the ON condition",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(personField("Secret"), dal.NewConstant("x"))))),
				access.DecisionSlotWhere, "Secret"},
			{"the hidden field at depth two",
				selectName(dal.From(person).Join(dal.NewJoinedFrom(
					dal.From(invoice("i")).Join(dal.NewJoinedSource(invoice("j"), dal.JoinInner, on(personField("Secret"), invoiceField("j", "PersonId")))),
					dal.JoinInner, on(personField("PersonId"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotWhere, "Secret"},
			{"the hidden field at depth three",
				selectName(dal.From(person).Join(dal.NewJoinedFrom(
					dal.From(invoice("i")).Join(dal.NewJoinedFrom(
						dal.From(invoice("j")).Join(dal.NewJoinedSource(invoice("k"), dal.JoinInner, on(personField("Secret"), invoiceField("k", "PersonId")))),
						dal.JoinInner, on(invoiceField("i", "InvoiceId"), invoiceField("j", "InvoiceId")))),
					dal.JoinInner, on(personField("PersonId"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotWhere, "Secret"},
			{"the hidden field in the scan order of the source",
				selectName(dal.From(person.WithScan(5, dal.AscendingField(w.name("Secret"))))),
				access.DecisionSlotFields, "Secret"},
			{"the hidden field in the scan order of a joined source's own query, qualified by the listed source",
				selectName(dal.From(person).Join(dal.NewJoinedSource(invoice("i").WithScan(5, dal.Descending(personField("Secret"))), dal.JoinInner,
					on(personField("PersonId"), invoiceField("i", "PersonId"))))),
				access.DecisionSlotFields, "Secret"},
			{"a filter on the hidden field",
				w.persons().WhereField(w.name("Secret"), dal.Equal, "ada-secret").SelectColumns(dal.Column{Expression: w.field("Name")}),
				access.DecisionSlotWhere, "Secret"},
			{"an order by the hidden field",
				w.persons().OrderBy(dal.AscendingField(w.name("Secret"))).SelectColumns(dal.Column{Expression: w.field("Name")}),
				access.DecisionSlotFields, "Secret"},
		} {
			f.trace.reset()
			_, err := readThrough(ctx, session, tc.query)
			t.Logf("%s: %v", tc.label, err)
			deniedBeforeTheServer(t, f, err, access.CodeColumnDenied, tc.slot, w.name(tc.hidden))
		}

		// The controls: the same joins over the listed fields run, in one statement.
		f.trace.reset()
		rows, err := readThrough(ctx, session, dal.From(person).Join(dal.NewJoinedSource(invoice("i"), dal.JoinInner, on(personField("PersonId"), invoiceField("i", "PersonId")))).
			NewQuery().OrderBy(dal.Ascending(personField("PersonId"))).
			SelectColumns(dal.Column{Expression: personField("Name"), Alias: "name"}))
		// (An ORDER BY on a field of the joined source, i.InvoiceId, is held to the list of the
		// listed source as well and is denied: it fails closed, and the denial is not asserted
		// here.)
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
