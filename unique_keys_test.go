package dalgo2postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dalgotest"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
)

// The rule every read keeps: within one read, no two records have the same key (a record
// with no key, which dalgotest.HasNoKey says, may repeat). dalgo2sql keys the records of a
// source nobody declared by the primary key the catalog reports when it is exactly one
// column, and by their ordinal otherwise; it once keyed every record by the constant
// "__dalgo_record_id". The fixtures here are ones a constant key cannot pass by accident:
// hundreds of rows whose non-key values repeat, and tables whose rows are all alike.
//
// uniqueKeysCases is the list of read paths, shared by the unit test below (a go-sqlmock
// stand-in for the server, which runs in plain `go test`) and by the server test
// TestStructuredQueryIntegration_EveryReadKeepsItsKeysUnique, which runs the same queries
// against PostgreSQL. A new read path is one more row.

const uniqueKeysInvoiceRows = 412

// uniqueKeysCase is one read path: a query, the number of records it returns and how the
// stand-in server answers it.
type uniqueKeysCase struct {
	name  string
	query dal.Query
	rows  int
	// ordinal says every record is keyed by its position, "0", "1", ...; keyed that the
	// records are keyed by the primary key of the base source and the key is not checked to
	// be the invoice id; otherwise they are keyed by the invoice id.
	ordinal, keyed bool
	// repeatedKey, when set, says a key repeats in this read today and why: a violation of the
	// rule in dalgo2sql, pinned here until it is fixed there.
	repeatedKey string
	// sources are the relations the catalog describes for the read, one entry for each time
	// it asks (a join plan describes each of its sources twice: its fields, then its scan).
	sources []uniqueKeysSource
	// scans are the answers of the stand-in server to the statements the read sends; a
	// read of one source sends one, and a join planned in DALgo one for each of its sources.
	scans []uniqueKeysScan
}

// uniqueKeysScan is the answer to one statement: the rows of the relation it reads (all of
// them when from is empty).
type uniqueKeysScan struct {
	from    string
	columns []string
	rows    int
	values  func(i int) []any
}

// uniqueKeysSource is a relation of the stand-in catalog.
type uniqueKeysSource struct {
	relation string // as the catalog names it, quoted
	columns  []catalogColumn
	pk       map[string]bool
	// also are the sources a combined answer describes.
	also []uniqueKeysSource
}

var (
	invoiceSource = uniqueKeysSource{relation: `"invoice"`,
		columns: []catalogColumn{intColumn("invoiceid", true), intColumn("customerid", true), textColumn("billingcountry"), numColumn("total")},
		pk:      map[string]bool{"invoiceid": true}}
	customerSource = uniqueKeysSource{relation: `"customer"`,
		columns: []catalogColumn{intColumn("customerid", true), textColumn("country")},
		pk:      map[string]bool{"customerid": true}}
	compositeSource = uniqueKeysSource{relation: `"compositepk"`,
		columns: []catalogColumn{textColumn("region"), intColumn("code", true), textColumn("label")},
		pk:      map[string]bool{"region": true, "code": true}}
	noPkSource = uniqueKeysSource{relation: `"nopk"`, columns: []catalogColumn{textColumn("name"), intColumn("qty", false)}}
	// A view has no primary key of its own, though its table has one.
	viewSource = uniqueKeysSource{relation: `"invoiceview"`, columns: []catalogColumn{intColumn("invoiceid", false), textColumn("billingcountry")}}
)

// both is the catalog's answer for two sources at once, as a join on the server asks for it.
func both(a, b uniqueKeysSource) uniqueKeysSource {
	return uniqueKeysSource{also: []uniqueKeysSource{a, b}}
}

func (s uniqueKeysSource) answer() *sqlmock.Rows {
	rows := sqlmock.NewRows(catalogColumns)
	for _, part := range s.also {
		for _, c := range part.columns {
			rows.AddRow(part.relation, c.name, c.dataType, c.category, c.oid, int64(0), c.notNull, false, part.pk[c.name], int64(0))
		}
	}
	for _, c := range s.columns {
		rows.AddRow(s.relation, c.name, c.dataType, c.category, c.oid, int64(0), c.notNull, false, s.pk[c.name], int64(0))
	}
	return rows
}

func uniqueKeysFrom(name string) dal.FromSource { return dal.From(dal.NewRootCollectionRef(name, "")) }

func uniqueKeysColumn(names ...string) []dal.Column {
	columns := make([]dal.Column, len(names))
	for i, name := range names {
		columns[i] = dal.Column{Expression: field(name)}
	}
	return columns
}

func invoiceValues(i int) []any {
	return []any{int64(i + 1), int64(1 + i%59), []string{"USA", "France", "Brazil"}[i%3], 0.99 + float64(i%3)}
}

func uniqueKeysCases() []uniqueKeysCase {
	invoiceColumns := []string{"invoiceid", "customerid", "billingcountry", "total"}
	hidden := "__dalgo_record_id"
	join := func(left, right, leftAlias, rightAlias string) dal.FromSource {
		return dal.From(dal.NewRootCollectionRef(left, leftAlias)).Join(dal.NewJoinedSource(
			dal.NewRootCollectionRef(right, rightAlias), dal.JoinInner,
			dal.NewComparison(dal.NewFieldRef(leftAlias, "customerid"), dal.Equal, dal.NewFieldRef(rightAlias, "customerid"))))
	}
	one := func(columns []string, rows int, values func(i int) []any) []uniqueKeysScan {
		return []uniqueKeysScan{{columns: columns, rows: rows, values: values}}
	}
	return []uniqueKeysCase{
		{name: "select-all of a single-column primary key with repeated values",
			query: uniqueKeysFrom("invoice").NewQuery().SelectColumns(), rows: uniqueKeysInvoiceRows,
			sources: []uniqueKeysSource{invoiceSource}, scans: one(invoiceColumns, uniqueKeysInvoiceRows, invoiceValues)},
		{name: "select columns with the key",
			query: uniqueKeysFrom("invoice").NewQuery().SelectColumns(uniqueKeysColumn("invoiceid", "billingcountry")...), rows: uniqueKeysInvoiceRows,
			sources: []uniqueKeysSource{invoiceSource},
			scans: one([]string{"invoiceid", "billingcountry"}, uniqueKeysInvoiceRows,
				func(i int) []any { v := invoiceValues(i); return []any{v[0], v[2]} })},
		{name: "select columns without the key",
			query: uniqueKeysFrom("invoice").NewQuery().SelectColumns(uniqueKeysColumn("billingcountry", "total")...), rows: uniqueKeysInvoiceRows,
			sources: []uniqueKeysSource{invoiceSource},
			scans: one([]string{"billingcountry", "total", hidden}, uniqueKeysInvoiceRows,
				func(i int) []any { v := invoiceValues(i); return []any{v[2], v[3], v[0]} })},
		{name: "a composite primary key", query: uniqueKeysFrom("compositepk").NewQuery().SelectColumns(), rows: 4, ordinal: true,
			sources: []uniqueKeysSource{compositeSource},
			scans: one([]string{"region", "code", "label"}, 4,
				func(i int) []any { return []any{[]string{"eu", "us"}[i/2], int64(1 + i%2), "same"} })},
		{name: "a table with no primary key and rows that are all alike",
			query: uniqueKeysFrom("nopk").NewQuery().SelectColumns(), rows: 5, ordinal: true,
			sources: []uniqueKeysSource{noPkSource},
			scans:   one([]string{"name", "qty"}, 5, func(int) []any { return []any{"bolt", int64(10)} })},
		{name: "select columns of a table with no primary key",
			query: uniqueKeysFrom("nopk").NewQuery().SelectColumns(uniqueKeysColumn("name")...), rows: 5, ordinal: true,
			sources: []uniqueKeysSource{noPkSource},
			scans:   one([]string{"name"}, 5, func(int) []any { return []any{"bolt"} })},
		{name: "a view", query: uniqueKeysFrom("invoiceview").NewQuery().SelectColumns(), rows: uniqueKeysInvoiceRows, ordinal: true,
			sources: []uniqueKeysSource{viewSource},
			scans: one([]string{"invoiceid", "billingcountry"}, uniqueKeysInvoiceRows,
				func(i int) []any { return []any{int64(i + 1), "USA"} })},
		{name: "a GROUP BY over a table with a primary key",
			query: uniqueKeysFrom("invoice").NewQuery().GroupBy(field("billingcountry")).
				SelectColumns(dal.Column{Expression: field("billingcountry")}, dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"}),
			rows: 3, ordinal: true,
			sources: []uniqueKeysSource{invoiceSource},
			scans: one([]string{"billingcountry", "n"}, 3,
				func(i int) []any { return []any{[]string{"USA", "France", "Brazil"}[i], int64(137)} })},
		{name: "a one-to-many join",
			query: join("customer", "invoice", "c", "i").NewQuery().SelectColumns(
				dal.Column{Expression: dal.NewFieldRef("c", "country")}, dal.Column{Expression: dal.NewFieldRef("i", "total")}),
			rows:        uniqueKeysInvoiceRows,
			sources:     []uniqueKeysSource{both(customerSource, invoiceSource), both(customerSource, invoiceSource)},
			repeatedKey: "a join keys each record by the base row's key, so a one-to-many join repeats it (dalgo2sql v0.29.3, PostgreSQL)",
			scans: []uniqueKeysScan{{from: `"customer"`, columns: []string{"country", "total", hidden}, rows: uniqueKeysInvoiceRows,
				values: func(i int) []any { return []any{"USA", 0.99, int64(1 + i%59)} }}}},
		{name: "a many-to-one join",
			query: join("invoice", "customer", "i", "c").NewQuery().SelectColumns(
				dal.Column{Expression: dal.NewFieldRef("i", "total")}, dal.Column{Expression: dal.NewFieldRef("c", "country")}),
			rows:    uniqueKeysInvoiceRows,
			sources: []uniqueKeysSource{both(invoiceSource, customerSource), both(invoiceSource, customerSource)},
			keyed:   true,
			scans: []uniqueKeysScan{{from: `"invoice"`, columns: []string{"total", "country", hidden}, rows: uniqueKeysInvoiceRows,
				values: func(i int) []any { return []any{0.99, "USA", int64(i + 1)} }}}},
	}
}

// readUniqueKeysRecords reads every record of q through the records reader.
func readUniqueKeysRecords(t *testing.T, db dal.DB, q dal.Query) []dalrecord.Record {
	t.Helper()
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var records []dalrecord.Record
	for {
		r, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			return records
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		records = append(records, r)
	}
}

// repeatedKeyOf returns a key that two records of one read share, and whether there is one.
func repeatedKeyOf(records []dalrecord.Record) (string, bool) {
	seen := map[string]bool{}
	for _, r := range records {
		key := fmt.Sprintf("%s/%v", r.Key().Collection(), r.Key().ID)
		if seen[key] {
			return key, true
		}
		seen[key] = true
	}
	return "", false
}

// assertUniqueKeysRead holds a read's records to the rule, and to the key the case says. A
// server answers in no defined order, so the id of the record at a position is checked only
// when inOrder says the stand-in answered the rows in the order of their ids.
func assertUniqueKeysRead(t *testing.T, records []dalrecord.Record, tc uniqueKeysCase, inOrder bool) {
	t.Helper()
	if len(records) != tc.rows {
		t.Fatalf("read %d records, want %d", len(records), tc.rows)
	}
	if tc.repeatedKey != "" {
		// KNOWN VIOLATION of the rule, in dal-go/dalgo2sql and not in this adapter. It FAILS when
		// dalgo2sql keys a one-to-many join by ordinal: delete repeatedKey from the case, so
		// that dalgotest.AssertUniqueKeys holds the read.
		if key, repeated := repeatedKeyOf(records); !repeated {
			t.Fatalf("no key repeats any more: dalgo2sql keys this read uniquely now. Delete the case's repeatedKey (%s) and keep the rule", tc.repeatedKey)
		} else {
			t.Logf("KNOWN VIOLATION (%s): the key %s is shared by two records", tc.repeatedKey, key)
		}
		return
	}
	dalgotest.AssertUniqueKeys(t, records)
	for i, r := range records {
		if dalgotest.HasNoKey(r.Key()) {
			t.Fatalf("record %d has no key: a read of a recordset keys every record", i)
		}
		if r.Key().ID == "__dalgo_record_id" {
			t.Fatalf("record %d has the constant key %q", i, r.Key().ID)
		}
		switch {
		case tc.ordinal && r.Key().ID != fmt.Sprint(i):
			t.Fatalf("record %d has the key %v, want the ordinal %d", i, r.Key().ID, i)
		case inOrder && !tc.ordinal && !tc.keyed && r.Key().ID != int64(i+1):
			t.Fatalf("record %d has the key %v (%T), want the invoice id %d", i, r.Key().ID, r.Key().ID, i+1)
		}
	}
}

func driverValues(values []any) []driver.Value {
	out := make([]driver.Value, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// uniqueKeysMock matches the catalog lookup by its opening and any other statement as a SELECT.
func uniqueKeysMock(t *testing.T) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		actual = strings.Join(strings.Fields(actual), " ")
		if expected == catalogStatement {
			return statementMatcher(expected, actual)
		}
		// An expectation names the source its statement reads: "SELECT" and then `"invoice"`.
		parts := strings.Fields(expected)
		if !strings.HasPrefix(actual, "SELECT ") || (len(parts) > 1 && !strings.Contains(actual, " FROM "+parts[1])) {
			return fmt.Errorf("statement %q is not %q", actual, expected)
		}
		return nil
	})))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	// A join asks the catalog for its sources in an order of its own.
	mock.MatchExpectationsInOrder(false)
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = sqlDB.Close()
	})
	return openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{}), mock
}

// The stand-in server answers each read with rows as PostgreSQL would, so that the rule is
// checked in plain `go test`; the server test below runs the same reads on a server.
func TestEveryReadKeepsItsKeysUniqueWithinTheRead(t *testing.T) {
	// Not parallel: mutates process PG* environment variables via t.Setenv.
	for _, tc := range uniqueKeysCases() {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := uniqueKeysMock(t)
			for _, source := range tc.sources {
				expectation := mock.ExpectQuery(catalogStatement)
				if source.relation != "" {
					expectation.WithArgs(source.relation)
				}
				expectation.WillReturnRows(source.answer())
			}
			for _, scan := range tc.scans {
				answer := sqlmock.NewRows(scan.columns)
				for i := 0; i < scan.rows; i++ {
					answer.AddRow(driverValues(scan.values(i))...)
				}
				mock.ExpectQuery("SELECT " + scan.from).WillReturnRows(answer)
			}
			assertUniqueKeysRead(t, readUniqueKeysRecords(t, db, tc.query), tc, true)
		})
	}
}
