package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The tests in this file are the first evidence from a real server for how the typed
// compiler of dalgo2sql types a constant: each Go type of a constant meets each column
// type, and the output of the run (go test -v, as the Conformance job runs it) lists the
// statement the driver sent, the argument it was handed and what the server answered.
// Every line is asserted, not only printed. A constant of another type than its column
// is a clear error from the server, never a wrong match; where the server refuses, the
// refusal is an honest finding about the dialect, and where it answers with rows the rows
// are the ones the caller meant.
//
// They run against PostgreSQL and skip unless DALGO2POSTGRES_TEST_DSN is set; CI fails
// when any TestTypeMatrixIntegration_ test skips or is missing.

// observation is what one structured read did on the server: the statement the driver
// sent and its arguments (the catalog lookup left out), the rows the reader returned and
// the error it ended with, whether it came from starting the read or from reading a row.
type observation struct {
	statement string
	args      []any
	rows      []map[string]any
	err       error
	code      string // the SQLSTATE, when the server refused
	message   string // the server's message, when it refused
}

// observe runs q through the records reader to its end, or to the error that ends it.
func observe(t *testing.T, f *queryFixture, q dal.Query) observation {
	t.Helper()
	f.trace.reset()
	var o observation
	reader, err := f.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		o.err = err
	} else {
		for {
			rec, err := reader.Next()
			if errors.Is(err, dal.ErrNoMoreRecords) {
				break
			}
			if err != nil {
				o.err = err
				break
			}
			o.rows = append(o.rows, rec.Data().(map[string]any))
		}
		_ = reader.Close()
	}
	for _, sent := range f.trace.sent() {
		if !isCatalogStatement(sent.sql) {
			o.statement, o.args = sent.sql, valueArguments(sent.args)
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(o.err, &pgErr) {
		o.code, o.message = pgErr.Code, pgErr.Message
	}
	return o
}

// valueArguments are the arguments of a statement that are values. The driver hands the
// tracer, ahead of them, the options it gives the query (the result formats it asks for),
// which are no argument of the statement.
func valueArguments(args []any) []any {
	var out []any
	for _, arg := range args {
		switch arg.(type) {
		case pgx.QueryResultFormats, pgx.QueryResultFormatsByOID, pgx.QueryExecMode:
			continue
		}
		out = append(out, arg)
	}
	return out
}

// ids is the id of every row the read returned.
func (o observation) ids() []int {
	out := make([]int, 0, len(o.rows))
	for _, row := range o.rows {
		if id, ok := row["id"].(int64); ok {
			out = append(out, int(id))
		}
	}
	return out
}

// result says in words what the read ended with.
func (o observation) result() string {
	switch {
	case o.code != "":
		return fmt.Sprintf("refused by the server, SQLSTATE %s: %s", o.code, o.message)
	case o.err != nil:
		return "error: " + o.err.Error()
	}
	return fmt.Sprintf("rows %v", o.ids())
}

// argsText writes the arguments with their Go types, as the driver was handed them.
func argsText(args []any) string {
	if len(args) == 0 {
		return "none"
	}
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = fmt.Sprintf("%T(%v)", arg, arg)
	}
	return strings.Join(parts, ", ")
}

// cellOutcome is what one filter must give: the ids of the rows it matches, or the
// SQLSTATE of the server's refusal.
type cellOutcome struct {
	ids  []int
	code string
}

func rowsOf(ids ...int) cellOutcome   { return cellOutcome{ids: ids} }
func refused(code string) cellOutcome { return cellOutcome{code: code} }

func (c cellOutcome) String() string {
	if c.code != "" {
		return "refused, SQLSTATE " + c.code
	}
	return fmt.Sprintf("rows %v", c.ids)
}

// matches says whether the observation is the outcome.
func (c cellOutcome) matches(o observation) bool {
	if c.code != "" {
		return o.code == c.code
	}
	if o.err != nil || len(o.ids()) != len(c.ids) {
		return false
	}
	return len(c.ids) == 0 || reflect.DeepEqual(o.ids(), c.ids)
}

// check asserts that a read gave the outcome, and records the statement, the arguments
// and the result in the output of the run either way.
func check(t *testing.T, label string, o observation, want cellOutcome) {
	t.Helper()
	t.Logf("%s\n    statement: %s\n    arguments: %s\n    result:    %s", label, o.statement, argsText(o.args), o.result())
	if !want.matches(o) {
		t.Errorf("%s: got %s, want %s", label, o.result(), want)
	}
}

// openTypeFixture opens a fixture whose sessions run in the time zone given, so what a
// time.Time means against a column without a zone does not depend on the server's default.
func openTypeFixture(t *testing.T, schema, timeZone string, ddl ...string) *queryFixture {
	t.Helper()
	return openQueryFixtureWithParams(t, map[string]string{"timezone": timeZone}, dalgo2sql.DbOptions{}, schema, IdentifierFoldLower, ddl)
}

// typeMatrixDDL is the table of the matrix: one column of each type, two rows of values
// and a row of NULLs. The values of the two rows are told apart by every type, so a filter
// that matches the wrong row, or both, is seen.
var typeMatrixDDL = []string{
	`CREATE TABLE tm (
		id   integer PRIMARY KEY,
		i2   smallint,
		i4   integer,
		i8   bigint,
		num  numeric,
		f4   real,
		f8   double precision,
		b    boolean,
		d    date,
		ts   timestamp,
		tstz timestamptz,
		u    uuid,
		j    jsonb,
		t    text
	)`,
	`INSERT INTO tm VALUES
		(1, 1, 1, 1, 1.5, 1.5, 1.5, true,  '2020-01-01', '2020-01-01 00:00:00', '2020-01-01 00:00:00+00',
			'11111111-1111-1111-1111-111111111111', '{"a": 1}', 'one'),
		(2, 2, 2, 2, 2,   2,   2,   false, '2021-06-15', '2021-06-15 12:30:00', '2021-06-15 12:30:00+00',
			'22222222-2222-2222-2222-222222222222', '{"a": 2}', '2')`,
	`INSERT INTO tm (id) VALUES (3)`,
}

// The Go types of constant the dialect binds, in the order the outcomes of a column list
// them. A string is two kinds, one that is a value of the column's type and one that is
// not; nil is two, ==, which the compiler writes as IS NULL, and >, which it binds.
const (
	kindInt = iota
	kindUint
	kindFloat32
	kindFloat64
	kindWholeFloat64
	kindString
	kindBadString
	kindBool
	kindTime
	kindBytes
	kindNil
	kindNilCompared
	kindCount
)

// matrixKinds says, for each kind, the rule the dialect has for it: the text it writes
// after the column and the operator, and the Go type the driver is handed.
var matrixKinds = [kindCount]struct {
	label    string
	operator dal.Operator
	sql      string // the operator as the statement writes it
	marker   string // what stands for the constant in the statement
	argType  string // the Go type of the argument ("" for none)
}{
	kindInt:          {"int", dal.Equal, "=", "$1::bigint", "int64"},
	kindUint:         {"uint", dal.Equal, "=", "$1::numeric", "string"},
	kindFloat32:      {"float32", dal.Equal, "=", "$1::numeric", "string"},
	kindFloat64:      {"float64", dal.Equal, "=", "$1::numeric", "string"},
	kindWholeFloat64: {"float64 holding a whole number", dal.Equal, "=", "$1::numeric", "string"},
	kindString:       {"string that is a value of the type", dal.Equal, "=", "$1", "string"},
	kindBadString:    {"string that is no value of the type", dal.Equal, "=", "$1", "string"},
	kindBool:         {"bool", dal.Equal, "=", "$1::boolean", "bool"},
	kindTime:         {"time.Time", dal.Equal, "=", "$1::timestamptz", "time.Time"},
	kindBytes:        {"[]byte", dal.Equal, "=", "$1::bytea", "[]uint8"},
	kindNil:          {"nil with ==", dal.Equal, "IS NULL", "", ""},
	kindNilCompared:  {"nil with >", dal.GreaterThen, ">", "$1", "<nil>"},
}

// matrixColumn is one column of tm, the constants that stand for its second row in the
// Go types the matrix tries, and what the server answers to each.
type matrixColumn struct {
	name    string
	sqlType string
	// str and badStr are a string that is a value of the type (the second row's) and
	// one that is not.
	str, badStr string
	// when is the time.Time that stands for the second row, where one can.
	when time.Time
	// second is where the second row sorts: after the first, or before it.
	secondSortsFirst bool
	// values are what a projection returns for the first and the second row.
	values [2]any
	// outcomes are the answers to a filter of each kind of constant.
	outcomes [kindCount]cellOutcome
}

// constant is the value of the given kind that the column is filtered by.
func (c matrixColumn) constant(kind int) any {
	switch kind {
	case kindInt:
		return 2
	case kindUint:
		return uint(2)
	case kindFloat32:
		return float32(1.5)
	case kindFloat64:
		return 1.5
	case kindWholeFloat64:
		return 2.0
	case kindString:
		return c.str
	case kindBadString:
		return c.badStr
	case kindBool:
		return true
	case kindTime:
		return c.when
	case kindBytes:
		return []byte{2}
	}
	return nil
}

var (
	noon2021     = time.Date(2021, 6, 15, 12, 30, 0, 0, time.UTC)
	midnight2021 = time.Date(2021, 6, 15, 0, 0, 0, 0, time.UTC)
	jan2020      = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// The outcomes of a family of types. A number is compared with a number whatever
	// the types (an integer against 1.5 is a miss, not an error); a type that no number,
	// boolean, time or byte string converts to refuses them all with 42883 (no such
	// operator); a string is parsed as the column's type by the server, so one that is
	// no value of it is refused with 22P02 (or 22007 for a date or a time), and nil
	// matches the NULL row only with ==.
	integerOutcomes = [kindCount]cellOutcome{
		kindInt: rowsOf(2), kindUint: rowsOf(2), kindFloat32: rowsOf(), kindFloat64: rowsOf(), kindWholeFloat64: rowsOf(2),
		kindString: rowsOf(2), kindBadString: refused("22P02"), kindBool: refused("42883"), kindTime: refused("42883"),
		kindBytes: refused("42883"), kindNil: rowsOf(3), kindNilCompared: rowsOf(),
	}
	fractionalOutcomes = [kindCount]cellOutcome{
		kindInt: rowsOf(2), kindUint: rowsOf(2), kindFloat32: rowsOf(1), kindFloat64: rowsOf(1), kindWholeFloat64: rowsOf(2),
		kindString: rowsOf(2), kindBadString: refused("22P02"), kindBool: refused("42883"), kindTime: refused("42883"),
		kindBytes: refused("42883"), kindNil: rowsOf(3), kindNilCompared: rowsOf(),
	}
	numberless = func(str, bad cellOutcome, when cellOutcome, bytes cellOutcome) [kindCount]cellOutcome {
		return [kindCount]cellOutcome{
			kindInt: refused("42883"), kindUint: refused("42883"), kindFloat32: refused("42883"), kindFloat64: refused("42883"),
			kindWholeFloat64: refused("42883"), kindString: str, kindBadString: bad, kindBool: refused("42883"),
			kindTime: when, kindBytes: bytes, kindNil: rowsOf(3), kindNilCompared: rowsOf(),
		}
	}
)

// matrixColumns is the matrix, a column to a row.
var matrixColumns = []matrixColumn{
	{name: "i2", sqlType: "smallint", str: "2", badStr: "x", when: noon2021,
		values: [2]any{int64(1), int64(2)}, outcomes: integerOutcomes},
	{name: "i4", sqlType: "integer", str: "2", badStr: "x", when: noon2021,
		values: [2]any{int64(1), int64(2)}, outcomes: integerOutcomes},
	{name: "i8", sqlType: "bigint", str: "2", badStr: "x", when: noon2021,
		values: [2]any{int64(1), int64(2)}, outcomes: integerOutcomes},
	{name: "num", sqlType: "numeric", str: "2", badStr: "x", when: noon2021,
		values: [2]any{1.5, 2.0}, outcomes: fractionalOutcomes},
	{name: "f4", sqlType: "real", str: "2", badStr: "x", when: noon2021,
		values: [2]any{1.5, 2.0}, outcomes: fractionalOutcomes},
	{name: "f8", sqlType: "double precision", str: "2", badStr: "x", when: noon2021,
		values: [2]any{1.5, 2.0}, outcomes: fractionalOutcomes},
	{name: "b", sqlType: "boolean", str: "false", badStr: "x", when: noon2021, secondSortsFirst: true,
		values: [2]any{true, false},
		// The bool constant is true, which is the first row's value.
		outcomes: func() [kindCount]cellOutcome {
			o := numberless(rowsOf(2), refused("22P02"), refused("42883"), refused("42883"))
			o[kindBool] = rowsOf(1)
			return o
		}()},
	{name: "d", sqlType: "date", str: "2021-06-15", badStr: "x", when: midnight2021,
		values:   [2]any{jan2020, midnight2021},
		outcomes: numberless(rowsOf(2), refused("22007"), rowsOf(2), refused("42883"))},
	{name: "ts", sqlType: "timestamp", str: "2021-06-15 12:30:00", badStr: "x", when: noon2021,
		values:   [2]any{jan2020, noon2021},
		outcomes: numberless(rowsOf(2), refused("22007"), rowsOf(2), refused("42883"))},
	{name: "tstz", sqlType: "timestamptz", str: "2021-06-15T12:30:00Z", badStr: "x", when: noon2021,
		values:   [2]any{jan2020, noon2021},
		outcomes: numberless(rowsOf(2), refused("22007"), rowsOf(2), refused("42883"))},
	{name: "u", sqlType: "uuid", str: "22222222-2222-2222-2222-222222222222", badStr: "x", when: noon2021,
		values:   [2]any{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"},
		outcomes: numberless(rowsOf(2), refused("22P02"), refused("42883"), refused("42883"))},
	{name: "j", sqlType: "jsonb", str: `{"a": 2}`, badStr: "x", when: noon2021,
		values:   [2]any{`{"a": 1}`, `{"a": 2}`},
		outcomes: numberless(rowsOf(2), refused("22P02"), refused("42883"), refused("42883"))},
	{name: "t", sqlType: "text", str: "2", badStr: "no such text", when: noon2021, secondSortsFirst: true,
		values: [2]any{"one", "2"},
		// Any string is a text, so there is no string a text column refuses.
		outcomes: func() [kindCount]cellOutcome {
			o := numberless(rowsOf(2), rowsOf(), refused("42883"), refused("42883"))
			return o
		}()},
}

// filterQuery is SELECT id FROM tm WHERE column <operator> constant ORDER BY id.
func filterQuery(table, column string, operator dal.Operator, constant any) dal.StructuredQuery {
	return dal.From(dal.NewRootCollectionRef(table, "")).NewQuery().
		Where(dal.NewComparison(field(column), operator, dal.Constant{Value: constant})).
		OrderBy(dal.AscendingField("id")).
		SelectColumns(dal.Column{Expression: field("id")})
}

// Each Go type of constant against each column type, as a filter. For every pair the
// output lists the statement, the argument and the answer, and the test asserts all
// three: the marker the dialect writes for the type (the rule), the type of the argument
// the driver is handed, and the rows or the SQLSTATE of the refusal. A constant of another
// type than its column is refused by the server, or compared as numbers, and is never a
// match on the wrong row.
func TestTypeMatrixIntegration_FilterByColumnTypeAndConstantType(t *testing.T) {
	f := openTypeFixture(t, "test_tm_filter", "UTC", typeMatrixDDL...)
	type tally struct {
		matched, empty int
		refusals       map[string]int
	}
	var tallies [kindCount]tally
	for i := range tallies {
		tallies[i].refusals = map[string]int{}
	}
	for _, col := range matrixColumns {
		t.Run(col.name+" "+col.sqlType, func(t *testing.T) {
			for kind := range kindCount {
				rule := matrixKinds[kind]
				o := observe(t, f, filterQuery("tm", col.name, rule.operator, col.constant(kind)))
				label := fmt.Sprintf("%s %s %s", col.name+" "+col.sqlType, rule.sql, rule.label)
				check(t, label, o, col.outcomes[kind])

				// The rule: what the statement writes for this kind of constant, and
				// what the driver is handed.
				where := fmt.Sprintf(`"%s" %s %s`, col.name, rule.sql, rule.marker)
				if rule.marker == "" {
					where = fmt.Sprintf(`"%s" %s`, col.name, rule.sql)
				}
				if want := fmt.Sprintf(`SELECT "id" FROM "tm" WHERE %s ORDER BY "id" ASC`, where); o.statement != want {
					t.Errorf("%s: statement = %s, want %s", label, o.statement, want)
				}
				switch {
				case rule.argType == "" && len(o.args) != 0:
					t.Errorf("%s: arguments = %s, want none", label, argsText(o.args))
				case rule.argType != "" && (len(o.args) != 1 || fmt.Sprintf("%T", o.args[0]) != rule.argType):
					t.Errorf("%s: arguments = %s, want one of type %s", label, argsText(o.args), rule.argType)
				}
				switch {
				case o.code != "":
					tallies[kind].refusals[o.code]++
				case len(o.rows) > 0:
					tallies[kind].matched++
				default:
					tallies[kind].empty++
				}
			}
		})
	}
	t.Run("rules", func(t *testing.T) {
		for kind := range kindCount {
			rule := matrixKinds[kind]
			text, handed := rule.marker, rule.argType
			if text == "" {
				text, handed = "no marker, the compiler writes "+rule.sql, "no argument"
			}
			refusals, total := make([]string, 0, len(tallies[kind].refusals)), 0
			for code, n := range tallies[kind].refusals {
				refusals = append(refusals, fmt.Sprintf("%s x%d", code, n))
				total += n
			}
			sort.Strings(refusals)
			if len(refusals) == 0 {
				refusals = append(refusals, "no refusal")
			}
			t.Logf("rule: a %s is written as %s and handed to the driver as %s. Over %d column types: rows matched %d times, nothing matched %d times, refused by the server %d times (%s)",
				rule.label, text, handed, len(matrixColumns), tallies[kind].matched, tallies[kind].empty, total, strings.Join(refusals, ", "))
		}
	})
}

// Every Go integer and float type the dialect binds, not only int, uint and float64: the
// dialect types a constant by its kind, so all signed integers are written alike and
// handed over as int64, all unsigned integers as numeric text, and a float of either size
// as numeric text of its own shortest decimal. Each runs against a column of the type
// that matches it best and finds the row whose value is 2.
func TestTypeMatrixIntegration_EveryIntegerAndFloatType(t *testing.T) {
	f := openTypeFixture(t, "test_tm_sizes", "UTC", typeMatrixDDL...)
	for _, tc := range []struct {
		column   string
		constant any
		marker   string
		argType  string
	}{
		{"i8", int(2), "$1::bigint", "int64"},
		{"i8", int8(2), "$1::bigint", "int64"},
		{"i8", int16(2), "$1::bigint", "int64"},
		{"i8", int32(2), "$1::bigint", "int64"},
		{"i8", int64(2), "$1::bigint", "int64"},
		{"i8", uint(2), "$1::numeric", "string"},
		{"i8", uint8(2), "$1::numeric", "string"},
		{"i8", uint16(2), "$1::numeric", "string"},
		{"i8", uint32(2), "$1::numeric", "string"},
		{"i8", uint64(2), "$1::numeric", "string"},
		{"i2", int8(2), "$1::bigint", "int64"},
		{"i4", int32(2), "$1::bigint", "int64"},
		{"num", float32(2), "$1::numeric", "string"},
		{"num", float64(2), "$1::numeric", "string"},
		{"f4", float32(2), "$1::numeric", "string"},
		{"f8", float64(2), "$1::numeric", "string"},
	} {
		label := fmt.Sprintf("%s %T(%v)", tc.column, tc.constant, tc.constant)
		o := observe(t, f, filterQuery("tm", tc.column, dal.Equal, tc.constant))
		check(t, label, o, rowsOf(2))
		if want := fmt.Sprintf(`SELECT "id" FROM "tm" WHERE "%s" = %s ORDER BY "id" ASC`, tc.column, tc.marker); o.statement != want {
			t.Errorf("%s: statement = %s, want %s", label, o.statement, want)
		}
		if len(o.args) != 1 || fmt.Sprintf("%T", o.args[0]) != tc.argType {
			t.Errorf("%s: arguments = %s, want one of type %s", label, argsText(o.args), tc.argType)
		}
	}
}

// A projection returns each column type as one Go type, and ORDER BY sorts it with the NULL
// row first ascending and last descending, as DALgo does, the NULLS clause written
// because no column of tm but the key is NOT NULL.
func TestTypeMatrixIntegration_ProjectionAndOrderBy(t *testing.T) {
	f := openTypeFixture(t, "test_tm_project", "UTC", typeMatrixDDL...)
	sameValue := func(got, want any) bool {
		if wantTime, ok := want.(time.Time); ok {
			gotTime, ok := got.(time.Time)
			return ok && gotTime.Equal(wantTime)
		}
		return reflect.DeepEqual(got, want)
	}
	for _, col := range matrixColumns {
		t.Run(col.name+" "+col.sqlType, func(t *testing.T) {
			projection := dal.From(dal.NewRootCollectionRef("tm", "")).NewQuery().
				Where(dal.WhereField("id", dal.LessThen, 4)).
				OrderBy(dal.AscendingField("id")).
				SelectColumns(dal.Column{Expression: field(col.name)})
			o := observe(t, f, projection)
			var got []any
			for _, row := range o.rows {
				got = append(got, row[col.name])
			}
			t.Logf("projection of %s %s\n    statement: %s\n    arguments: %s\n    records reader: %s",
				col.name, col.sqlType, o.statement, argsText(o.args), valuesText(got))
			if o.err != nil || len(got) != 3 {
				t.Fatalf("projection: rows = %v, err = %v; want three rows", got, o.err)
			}
			if !sameValue(got[0], col.values[0]) || !sameValue(got[1], col.values[1]) || got[2] != nil {
				t.Errorf("projection of %s = %s, want %s", col.name, valuesText(got), valuesText([]any{col.values[0], col.values[1], nil}))
			}
			// The recordset reader types each column from what the driver reports for it, and
			// returns the same Go types as the records reader.
			reader, err := f.db.ExecuteQueryToRecordsetReader(context.Background(), projection)
			if err != nil {
				t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
			}
			defer func() { _ = reader.Close() }()
			var viaRecordset []any
			for {
				row, rs, err := reader.Next()
				if errors.Is(err, dal.ErrNoMoreRecords) {
					break
				}
				if err != nil {
					t.Fatalf("recordset Next: %v", err)
				}
				values, err := row.Data(rs)
				if err != nil {
					t.Fatalf("row.Data: %v", err)
				}
				viaRecordset = append(viaRecordset, values[0])
			}
			t.Logf("    recordset reader: %s", valuesText(viaRecordset))
			if len(viaRecordset) != 3 {
				t.Fatalf("recordset reader returned %d rows, want 3", len(viaRecordset))
			}
			// KNOWN DEFECT of the recordset reader, not a rule: a typed column of a recordset
			// holds no NULL, so the reader puts the zero value of the column's type where the
			// database has NULL (dalgo2sql reader_recordset.go, DefaultValue), and a caller of
			// the recordset reader cannot tell a NULL from 0, false, "" or the zero time. The
			// records reader above returns nil for the same cell, and that is asserted. The fix
			// is the work of dal-go/dalgo2sql (task SQL-W4) and no release has it yet; this
			// pin FAILS when the reader returns nil, and the line to assert instead is then
			// "viaRecordset[2] == nil", as for the records reader.
			zero := reflect.Zero(reflect.TypeOf(col.values[0])).Interface()
			t.Logf("    KNOWN DEFECT of the recordset reader (dal-go/dalgo2sql, task SQL-W4 is fixing it; not a rule): the NULL row reads as %v, the zero value of the type; the records reader above returns nil for it", valuesText([]any{zero}))
			if viaRecordset[2] == nil {
				t.Errorf("recordset projection of %s = %s: the NULL row is now nil, so the defect is fixed in dalgo2sql: assert viaRecordset[2] == nil here, as for the records reader, and delete this pin and the README's known limit", col.name, valuesText(viaRecordset))
			} else if !sameValue(viaRecordset[0], col.values[0]) || !sameValue(viaRecordset[1], col.values[1]) || !sameValue(viaRecordset[2], zero) {
				t.Errorf("recordset projection of %s = %s, want %s: the values with the type's zero value, %v, where the row is NULL (the known defect)",
					col.name, valuesText(viaRecordset), valuesText([]any{col.values[0], col.values[1], zero}), zero)
			}

			ascending, descending := []int{3, 1, 2}, []int{2, 1, 3}
			if col.secondSortsFirst {
				ascending, descending = []int{3, 2, 1}, []int{1, 2, 3}
			}
			for _, order := range []struct {
				name   string
				by     dal.OrderExpression
				clause string
				want   []int
			}{
				{"ascending", dal.AscendingField(col.name), `ORDER BY "` + col.name + `" ASC NULLS FIRST`, ascending},
				{"descending", dal.DescendingField(col.name), `ORDER BY "` + col.name + `" DESC NULLS LAST`, descending},
			} {
				q := dal.From(dal.NewRootCollectionRef("tm", "")).NewQuery().OrderBy(order.by).
					SelectColumns(dal.Column{Expression: field("id")})
				o := observe(t, f, q)
				check(t, fmt.Sprintf("ORDER BY %s %s %s", col.name, col.sqlType, order.name), o, rowsOf(order.want...))
				if want := `SELECT "id" FROM "tm" ` + order.clause; o.statement != want {
					t.Errorf("ORDER BY %s: statement = %s, want %s", order.name, o.statement, want)
				}
			}
		})
	}
}

// valuesText writes values with their Go types.
func valuesText(values []any) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprintf("%T(%v)", value, value)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// edgeDDL is the table of the edge values: the largest and smallest of each integer type,
// an integer above 2^53 in a bigint and in a numeric, a real and a double precision that
// hold 0.1, and the three special values of numeric.
var edgeDDL = []string{
	`CREATE TABLE tm_edge (
		id     integer PRIMARY KEY,
		i2     smallint,
		i4     integer,
		i8     bigint,
		num    numeric,
		numbig numeric(20,0),
		f4     real,
		f8     double precision
	)`,
	`INSERT INTO tm_edge VALUES
		(1, 32767,  2147483647,  9007199254740993,    1.5, 9007199254740993, 0.1, 0.1),
		(2, -32768, -2147483648, 9223372036854775807, 2,   NULL,             1.5, 1.5)`,
	`INSERT INTO tm_edge (id, num) VALUES (3, 'Infinity'), (4, '-Infinity'), (5, 'NaN')`,
}

// edgeRow is a row of tm_edge read into a struct, each column into the integer or float
// field of its own size.
type edgeRow struct {
	ID     int64
	I2     int16
	I4     int32
	I8     int64
	Num    float64
	NumBig int64
	F4     float32
	F8     float64
}

// Values at the edge of what a type holds. A bigint above 2^53 is matched and read back
// exactly, by an int64, a uint64 and a string, and is not matched by the float64 that is
// its nearest neighbour; 'Infinity'::numeric (the server is 17) is matched by a float
// infinity and read back as one; a float32 constant against a real column is compared as
// a number, so what a caller sees is pinned below.
func TestTypeMatrixIntegration_ValuesAtTheEdges(t *testing.T) {
	f := openTypeFixture(t, "test_tm_edge", "UTC", edgeDDL...)
	filter := func(column string, operator dal.Operator, constant any) observation {
		return observe(t, f, filterQuery("tm_edge", column, operator, constant))
	}
	t.Run("a bigint above 2^53 is matched exactly", func(t *testing.T) {
		const above = int64(9007199254740993) // 2^53 + 1
		for _, tc := range []struct {
			label    string
			operator dal.Operator
			constant any
			want     cellOutcome
		}{
			{"int64 2^53+1", dal.Equal, above, rowsOf(1)},
			{"int64 2^53, its neighbour", dal.Equal, above - 1, rowsOf()},
			{"uint64 2^53+1, bound as numeric text", dal.Equal, uint64(above), rowsOf(1)},
			{"float64 2^53+1, which is 2^53: the nearest float64", dal.Equal, float64(above), rowsOf()},
			{"string 9007199254740993", dal.Equal, "9007199254740993", rowsOf(1)},
			{"int64 max", dal.Equal, int64(math.MaxInt64), rowsOf(2)},
			{"uint64 max, which no bigint holds, is a miss and not an error", dal.Equal, uint64(math.MaxUint64), rowsOf()},
			{"int64 2^53 as a lower bound", dal.GreaterThen, above - 1, rowsOf(1, 2)},
		} {
			check(t, "i8 bigint "+tc.label, filter("i8", tc.operator, tc.constant), tc.want)
		}
		check(t, "numbig numeric(20,0) uint64 2^53+1", filter("numbig", dal.Equal, uint64(above)), rowsOf(1))
		check(t, "numbig numeric(20,0) float64 2^53+1, which is 2^53", filter("numbig", dal.Equal, float64(above)), rowsOf())
	})
	t.Run("a bigint above 2^53 is read back exactly into an integer field", func(t *testing.T) {
		q := dal.From(dal.NewRootCollectionRef("tm_edge", "")).NewQuery().Where(dal.WhereField("id", dal.Equal, 1)).
			SelectIntoRecord(func() dalrecord.Record {
				return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("tm_edge", ""), &edgeRow{})
			})
		reader, err := f.db.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		defer func() { _ = reader.Close() }()
		rec, err := reader.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got := *rec.Data().(*edgeRow)
		t.Logf("into a struct: %+v", got)
		if want := (edgeRow{ID: 1, I2: 32767, I4: 2147483647, I8: 9007199254740993, Num: 1.5, NumBig: 9007199254740993, F4: 0.1, F8: 0.1}); got != want {
			t.Errorf("row = %+v, want %+v", got, want)
		}

		// Into a map, a bigint is an int64 and is exact too; a numeric is a float64,
		// which holds 15 significant digits, so the same value in a numeric is rounded.
		o := observe(t, f, dal.From(dal.NewRootCollectionRef("tm_edge", "")).NewQuery().Where(dal.WhereField("id", dal.Equal, 1)).
			SelectColumns(dal.Column{Expression: field("i8")}, dal.Column{Expression: field("numbig")}))
		if o.err != nil || len(o.rows) != 1 {
			t.Fatalf("projection: %v, %v", o.rows, o.err)
		}
		t.Logf("into a map: i8 = %T(%v), numbig = %T(%v)", o.rows[0]["i8"], o.rows[0]["i8"], o.rows[0]["numbig"], o.rows[0]["numbig"])
		if o.rows[0]["i8"] != int64(9007199254740993) {
			t.Errorf("i8 = %v, want int64 9007199254740993", o.rows[0]["i8"])
		}
		if o.rows[0]["numbig"] != float64(9007199254740992) {
			t.Errorf("numbig = %v, want float64 9007199254740992 (the nearest float64)", o.rows[0]["numbig"])
		}
	})
	t.Run("'Infinity'::numeric is matched by a float infinity and read back as one", func(t *testing.T) {
		for _, tc := range []struct {
			label    string
			column   string
			constant any
			want     cellOutcome
		}{
			{"num numeric float64 +Inf", "num", math.Inf(1), rowsOf(3)},
			{"num numeric float64 -Inf", "num", math.Inf(-1), rowsOf(4)},
			{"num numeric float64 NaN, which numeric equals to itself", "num", math.NaN(), rowsOf(5)},
			{"num numeric float32 +Inf", "num", float32(math.Inf(1)), rowsOf(3)},
			{"i4 integer float64 +Inf, a miss and not an error", "i4", math.Inf(1), rowsOf()},
			{"f8 double precision float64 +Inf, a miss: no row holds it", "f8", math.Inf(1), rowsOf()},
		} {
			check(t, tc.label, filter(tc.column, dal.Equal, tc.constant), tc.want)
		}
		o := observe(t, f, dal.From(dal.NewRootCollectionRef("tm_edge", "")).NewQuery().OrderBy(dal.AscendingField("id")).
			SelectColumns(dal.Column{Expression: field("num")}))
		var got []any
		for _, row := range o.rows {
			got = append(got, row["num"])
		}
		t.Logf("num read back: %s", valuesText(got))
		if o.err != nil || len(got) != 5 {
			t.Fatalf("projection: %v, %v", got, o.err)
		}
		if got[0] != 1.5 || got[1] != 2.0 || !math.IsInf(got[2].(float64), 1) || !math.IsInf(got[3].(float64), -1) || !math.IsNaN(got[4].(float64)) {
			t.Errorf("num = %s, want 1.5, 2, +Inf, -Inf, NaN as float64", valuesText(got))
		}
	})
	t.Run("a float32 constant against a real column", func(t *testing.T) {
		// A float is bound as numeric from the shortest decimal text of its own size, so
		// float32(0.1) is the numeric 0.1. Against double precision that is the number
		// 0.1 compared with 0.1, and matches. Against real the server compares as double
		// precision too, and the real 0.1 widens to 0.10000000149011612, so no decimal
		// 0.1 equals it: the filter is a miss on a row that holds the value written. The
		// string "0.1", which the server reads as a real, matches; so does the float64 the
		// reader returned for the row, because it is the widened real exactly.
		const widened = float64(float32(0.1))
		for _, tc := range []struct {
			label    string
			column   string
			constant any
			want     cellOutcome
		}{
			{"f4 real float32 1.5, exact in binary", "f4", float32(1.5), rowsOf(2)},
			{"f4 real float32 0.1", "f4", float32(0.1), rowsOf()},
			{"f4 real float64 0.1", "f4", 0.1, rowsOf()},
			{"f4 real string 0.1", "f4", "0.1", rowsOf(1)},
			{"f4 real float64 as the reader returns it", "f4", widened, rowsOf(1)},
			{"f8 double precision float32 0.1", "f8", float32(0.1), rowsOf(1)},
			{"f8 double precision float64 0.1", "f8", 0.1, rowsOf(1)},
		} {
			check(t, tc.label, filter(tc.column, dal.Equal, tc.constant), tc.want)
		}
		o := observe(t, f, dal.From(dal.NewRootCollectionRef("tm_edge", "")).NewQuery().Where(dal.WhereField("id", dal.Equal, 1)).
			SelectColumns(dal.Column{Expression: field("f4")}, dal.Column{Expression: field("f8")}))
		if o.err != nil || len(o.rows) != 1 {
			t.Fatalf("projection: %v, %v", o.rows, o.err)
		}
		t.Logf("read back: f4 = %T(%v), f8 = %T(%v)", o.rows[0]["f4"], o.rows[0]["f4"], o.rows[0]["f8"], o.rows[0]["f8"])
		if o.rows[0]["f4"] != widened || o.rows[0]["f8"] != 0.1 {
			t.Errorf("f4 = %v, f8 = %v; want the real widened to float64 (%v) and the double 0.1", o.rows[0]["f4"], o.rows[0]["f8"], widened)
		}
	})
}

// Arithmetic is on double precision: the dialect casts both operands, so a product past
// the range of its integer type is a number and not the server's error 22003, and a
// bigint beyond 2^53 loses its last digits, as a float64 does. This pins what a caller sees.
func TestTypeMatrixIntegration_ArithmeticOnDoublePrecision(t *testing.T) {
	f := openTypeFixture(t, "test_tm_arithmetic", "UTC", edgeDDL...)
	ctx := context.Background()
	// The control: without the cast the server refuses an int4 product past its range.
	var product int32
	err := f.admin.QueryRowContext(ctx, `SELECT 2147483647::int4 * 2`).Scan(&product)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22003" {
		t.Errorf("SELECT 2147483647::int4 * 2 = %d, %v; want the server's 22003 (integer out of range)", product, err)
	}
	t.Logf("control, no cast: SELECT 2147483647::int4 * 2 is %v", err)

	for _, tc := range []struct {
		label string
		id    int
		expr  dal.Expression
		want  float64
	}{
		{"i4 * 2 past int4", 1, dal.Binary(field("i4"), dal.Multiply, dal.NewConstant(2)), float64(math.MaxInt32) * 2},
		{"i4 * i4 past int4", 1, dal.Binary(field("i4"), dal.Multiply, field("i4")), float64(math.MaxInt32) * float64(math.MaxInt32)},
		{"i2 * i2 past int2", 1, dal.Binary(field("i2"), dal.Multiply, field("i2")), float64(math.MaxInt16) * float64(math.MaxInt16)},
		{"i4 + i4 past int4", 1, dal.Binary(field("i4"), dal.Add, field("i4")), float64(math.MaxInt32) * 2},
		{"i4 - 1 below int4's minimum", 2, dal.Binary(field("i4"), dal.Subtract, dal.NewConstant(1)), float64(math.MinInt32) - 1},
		{"i8 * 2 past int8", 2, dal.Binary(field("i8"), dal.Multiply, dal.NewConstant(2)), float64(math.MaxInt64) * 2},
		{"i8 * 1 above 2^53: the last digit is lost", 1, dal.Binary(field("i8"), dal.Multiply, dal.NewConstant(1)), 9007199254740992},
		{"i4 / 2 is not an integer division", 1, dal.Binary(field("i4"), dal.Divide, dal.NewConstant(2)), float64(math.MaxInt32) / 2},
	} {
		q := dal.From(dal.NewRootCollectionRef("tm_edge", "")).NewQuery().Where(dal.WhereField("id", dal.Equal, tc.id)).
			SelectColumns(dal.Column{Expression: tc.expr, Alias: "result"})
		o := observe(t, f, q)
		t.Logf("%s\n    statement: %s\n    arguments: %s\n    result:    %s", tc.label, o.statement, argsText(o.args), valuesText(column(o.rows, "result")))
		if o.err != nil || len(o.rows) != 1 {
			t.Errorf("%s: rows = %v, err = %v", tc.label, o.rows, o.err)
			continue
		}
		if got, ok := o.rows[0]["result"].(float64); !ok || got != tc.want {
			t.Errorf("%s: result = %T(%v), want float64(%v)", tc.label, o.rows[0]["result"], o.rows[0]["result"], tc.want)
		}
		if !strings.Contains(o.statement, "::double precision") {
			t.Errorf("%s: statement = %s, want the operands cast to double precision", tc.label, o.statement)
		}
	}
}

// A NUMERIC with a fraction cannot be read into an integer field: the read is an error
// that names the value, not a truncated number. A whole NUMERIC can.
func TestTypeMatrixIntegration_NumericWithAFractionIntoAnIntegerField(t *testing.T) {
	f := openTypeFixture(t, "test_tm_numeric_int", "UTC", edgeDDL...)
	type numRow struct {
		ID     int64
		I2     any
		I4     any
		I8     any
		Num    int64
		NumBig any
		F4     any
		F8     any
	}
	read := func(id int) (*numRow, error) {
		q := dal.From(dal.NewRootCollectionRef("tm_edge", "")).NewQuery().Where(dal.WhereField("id", dal.Equal, id)).
			SelectIntoRecord(func() dalrecord.Record {
				return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("tm_edge", ""), &numRow{})
			})
		reader, err := f.db.ExecuteQueryToRecordsReader(context.Background(), q)
		if err != nil {
			return nil, err
		}
		defer func() { _ = reader.Close() }()
		rec, err := reader.Next()
		if err != nil {
			return nil, err
		}
		return rec.Data().(*numRow), nil
	}
	got, err := read(1)
	t.Logf("numeric 1.5 into an int64 field: %+v, %v", got, err)
	if err == nil || errors.Is(err, dal.ErrNoMoreRecords) || !strings.Contains(err.Error(), `column "num": value 1.5 is not an int64`) {
		t.Errorf("numeric 1.5 into an int64 field = %+v, %v; want an error that names the column and the value", got, err)
	}
	got, err = read(2)
	t.Logf("numeric 2 into an int64 field: %+v, %v", got, err)
	if err != nil || got == nil || got.Num != 2 {
		t.Errorf("numeric 2 into an int64 field = %+v, %v; want 2", got, err)
	}
}

// A time.Time is an instant, and a column without a zone holds a wall clock: the server
// reads that wall clock in the session's time zone, so what matches depends on it. In a
// session in Tokyo the timestamp 2021-06-15 12:30 is 03:30 UTC, and the date 2021-06-15
// begins at 15:00 UTC the day before; in UTC (the matrix runs there) they are 12:30 and
// 00:00. A caller that filters a timestamp or a date by an instant must know the zone of
// its session.
func TestTypeMatrixIntegration_TimestampWithoutZoneFollowsTheSessionTimeZone(t *testing.T) {
	f := openTypeFixture(t, "test_tm_zone", "Asia/Tokyo",
		`CREATE TABLE tm_zone (id integer PRIMARY KEY, d date, ts timestamp)`,
		`INSERT INTO tm_zone VALUES (1, '2021-06-15', '2021-06-15 12:30:00')`)
	var zone string
	if err := f.sqlDB.QueryRowContext(context.Background(), `SHOW TimeZone`).Scan(&zone); err != nil || zone != "Asia/Tokyo" {
		t.Fatalf("SHOW TimeZone = %q, %v; want Asia/Tokyo", zone, err)
	}
	for _, tc := range []struct {
		label    string
		column   string
		constant time.Time
		want     cellOutcome
	}{
		{"ts timestamp, the instant 12:30 UTC, which is 21:30 in Tokyo", "ts", time.Date(2021, 6, 15, 12, 30, 0, 0, time.UTC), rowsOf()},
		{"ts timestamp, the instant 03:30 UTC, which is 12:30 in Tokyo", "ts", time.Date(2021, 6, 15, 3, 30, 0, 0, time.UTC), rowsOf(1)},
		{"d date, the instant midnight UTC, which is 09:00 in Tokyo", "d", time.Date(2021, 6, 15, 0, 0, 0, 0, time.UTC), rowsOf()},
		{"d date, the instant 15:00 UTC the day before, which is midnight in Tokyo", "d", time.Date(2021, 6, 14, 15, 0, 0, 0, time.UTC), rowsOf(1)},
	} {
		check(t, tc.label, observe(t, f, filterQuery("tm_zone", tc.column, dal.Equal, tc.constant)), tc.want)
	}
}
