package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
	"github.com/dal-go/dalgo2sql"
)

// The statements that create, alter and drop tables and indexes check every name
// they write with one rule, before the transaction begins and before any
// statement: a plain identifier (ASCII letters, digits and underscores, not
// starting with a digit) of at most 63 bytes, which is all PostgreSQL keeps of an
// identifier, after the lower-casing the package applies. A refused name is an
// error that matches dalgo2sql.ErrUnsafeName.
//
// These tests run on a go-sqlmock handle that matches statements exactly. A
// refusal is run on a handle with no expectation, so a transaction or a statement
// would fail the call with another error than the refusal; a name that is
// accepted is run on a handle that expects the transaction and each statement, in
// order, written as asserted below. The real server's side is in
// TestDDLNamesIntegration_RefusedNamesLeaveEveryObjectAsItWas.

// newDDLMock returns a Database over a go-sqlmock handle that matches a statement
// by its exact text. Every expectation must have been met by the end of the test.
func newDDLMock(t *testing.T) (*Database, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = sqlDB.Close()
	})
	resolved, err := resolveSettings(dalgo2sql.DbOptions{}, nil)
	if err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	return newDatabaseFromSQL(sqlDB, dal.NewSchema(nil, nil), resolved, nil), mock
}

// ddlPosition is one place a name can have in a call of an entry that creates,
// alters or drops: the name goes to call, which puts it there and leaves every
// other name valid, and sent is the statements the call writes for a name that
// is accepted (in the spelling the statement carries).
type ddlPosition struct {
	entry string // the method of ddl.SchemaModifier the call is made through
	op    string // the method of ddl.Applier the operation reaches, for AlterCollection
	where string // the place of the name
	kind  string // what a refusal calls the name: collection, field, primary key or index
	call  func(ctx context.Context, d *Database, name string) error
	sent  func(name string) []string
}

// ddlObjects are the names of the objects the calls of ddlPositionsOn are made on,
// and the other names they put beside the one under test: the table, one column
// of it, an index (another of its names, "id" and "g", are fixed). The server test
// gives them the names of objects that exist.
type ddlObjects struct{ table, column, index string }

// ddlPositions is ddlPositionsOn the objects of the unit tests.
func ddlPositions() []ddlPosition {
	return ddlPositionsOn(ddlObjects{table: "t", column: "f", index: "ix"})
}

// ddlPositionsOn lists every place of a name in every entry. A refused name is put
// in the last element of each list, so that a check of the first element only
// does not pass for it.
func ddlPositionsOn(o ddlObjects) []ddlPosition {
	id := dbschema.FieldDef{Name: "id", Type: dbschema.Int}
	column, index := dal.FieldName(o.column), o.index
	q := quoteIdent
	createTable := `CREATE TABLE ` + q(o.table) + ` ("id" BIGINT NOT NULL)`
	// alter makes the call of one operation on the table.
	alter := func(op func(n string) ddl.AlterOp) func(context.Context, *Database, string) error {
		return func(ctx context.Context, d *Database, n string) error {
			return d.AlterCollection(ctx, o.table, op(n))
		}
	}
	return []ddlPosition{
		{"CreateCollection", "", "the collection", "collection",
			func(ctx context.Context, d *Database, n string) error {
				return d.CreateCollection(ctx, dbschema.CollectionDef{Name: n, Fields: []dbschema.FieldDef{id}})
			},
			func(n string) []string { return []string{`CREATE TABLE ` + q(n) + ` ("id" BIGINT NOT NULL)`} }},
		{"CreateCollection", "", "the last field", "field",
			func(ctx context.Context, d *Database, n string) error {
				return d.CreateCollection(ctx, dbschema.CollectionDef{Name: o.table,
					Fields: []dbschema.FieldDef{id, {Name: dal.FieldName(n), Type: dbschema.String}}})
			},
			func(n string) []string {
				return []string{`CREATE TABLE ` + q(o.table) + ` ("id" BIGINT NOT NULL, ` + q(n) + ` TEXT NOT NULL)`}
			}},
		{"CreateCollection", "", "the last primary-key name", "primary key",
			func(ctx context.Context, d *Database, n string) error {
				return d.CreateCollection(ctx, dbschema.CollectionDef{Name: o.table,
					Fields: []dbschema.FieldDef{id}, PrimaryKey: []dal.FieldName{"id", dal.FieldName(n)}})
			},
			func(n string) []string {
				return []string{`CREATE TABLE ` + q(o.table) + ` ("id" BIGINT NOT NULL, PRIMARY KEY ("id", ` + q(n) + `))`}
			}},
		{"CreateCollection", "", "the name of the last index", "index",
			func(ctx context.Context, d *Database, n string) error {
				return d.CreateCollection(ctx, dbschema.CollectionDef{Name: o.table, Fields: []dbschema.FieldDef{id},
					Indexes: []dbschema.IndexDef{{Name: index, Fields: []dal.FieldName{"id"}}, {Name: n, Fields: []dal.FieldName{"id"}}}})
			},
			func(n string) []string {
				return []string{createTable, `CREATE INDEX ` + q(index) + ` ON ` + q(o.table) + ` ("id")`,
					`CREATE INDEX ` + q(n) + ` ON ` + q(o.table) + ` ("id")`}
			}},
		{"CreateCollection", "", "the collection of the last index", "collection",
			func(ctx context.Context, d *Database, n string) error {
				return d.CreateCollection(ctx, dbschema.CollectionDef{Name: o.table, Fields: []dbschema.FieldDef{id},
					Indexes: []dbschema.IndexDef{{Name: index, Fields: []dal.FieldName{"id"}}, {Name: "iy", Collection: n, Fields: []dal.FieldName{"id"}}}})
			},
			func(n string) []string {
				return []string{createTable, `CREATE INDEX ` + q(index) + ` ON ` + q(o.table) + ` ("id")`,
					`CREATE INDEX "iy" ON ` + q(n) + ` ("id")`}
			}},
		{"CreateCollection", "", "the last field of the last index", "field",
			func(ctx context.Context, d *Database, n string) error {
				return d.CreateCollection(ctx, dbschema.CollectionDef{Name: o.table, Fields: []dbschema.FieldDef{id},
					Indexes: []dbschema.IndexDef{{Name: index, Fields: []dal.FieldName{"id", dal.FieldName(n)}}}})
			},
			func(n string) []string {
				return []string{createTable, `CREATE INDEX ` + q(index) + ` ON ` + q(o.table) + ` ("id", ` + q(n) + `)`}
			}},

		{"DropCollection", "", "the collection", "collection",
			func(ctx context.Context, d *Database, n string) error { return d.DropCollection(ctx, n) },
			func(n string) []string { return []string{`DROP TABLE ` + q(n)} }},

		{"AlterCollection", "", "the table, with no operation", "collection",
			func(ctx context.Context, d *Database, n string) error { return d.AlterCollection(ctx, n) },
			func(string) []string { return nil }},
		{"AlterCollection", "ApplyDropField", "the table, with an operation", "collection",
			func(ctx context.Context, d *Database, n string) error {
				return d.AlterCollection(ctx, n, ddl.DropField(column))
			},
			func(n string) []string { return []string{`ALTER TABLE ` + q(n) + ` DROP COLUMN ` + q(o.column)} }},
		{"AlterCollection", "ApplyAddField", "the field", "field",
			alter(func(n string) ddl.AlterOp {
				return ddl.AddField(dbschema.FieldDef{Name: dal.FieldName(n), Type: dbschema.String})
			}),
			func(n string) []string {
				return []string{`ALTER TABLE ` + q(o.table) + ` ADD COLUMN ` + q(n) + ` TEXT NOT NULL`}
			}},
		{"AlterCollection", "ApplyDropField", "the field", "field",
			alter(func(n string) ddl.AlterOp { return ddl.DropField(dal.FieldName(n)) }),
			func(n string) []string { return []string{`ALTER TABLE ` + q(o.table) + ` DROP COLUMN ` + q(n)} }},
		{"AlterCollection", "ApplyModifyField", "the field", "field",
			alter(func(n string) ddl.AlterOp {
				return ddl.ModifyField(dal.FieldName(n), dbschema.FieldDef{Name: column, Type: dbschema.Int})
			}),
			func(n string) []string {
				return []string{
					`ALTER TABLE ` + q(o.table) + ` ALTER COLUMN ` + q(n) + ` TYPE BIGINT`,
					`ALTER TABLE ` + q(o.table) + ` ALTER COLUMN ` + q(n) + ` SET NOT NULL`,
				}
			}},
		{"AlterCollection", "ApplyRenameField", "the old name", "field",
			alter(func(n string) ddl.AlterOp { return ddl.RenameField(dal.FieldName(n), "g") }),
			func(n string) []string {
				return []string{`ALTER TABLE ` + q(o.table) + ` RENAME COLUMN ` + q(n) + ` TO "g"`}
			}},
		{"AlterCollection", "ApplyRenameField", "the new name", "field",
			alter(func(n string) ddl.AlterOp { return ddl.RenameField(column, dal.FieldName(n)) }),
			func(n string) []string {
				return []string{`ALTER TABLE ` + q(o.table) + ` RENAME COLUMN ` + q(o.column) + ` TO ` + q(n)}
			}},
		{"AlterCollection", "ApplyAddIndex", "the index name", "index",
			alter(func(n string) ddl.AlterOp {
				return ddl.AddIndex(dbschema.IndexDef{Name: n, Fields: []dal.FieldName{column}})
			}),
			func(n string) []string {
				return []string{`CREATE INDEX ` + q(n) + ` ON ` + q(o.table) + ` (` + q(o.column) + `)`}
			}},
		{"AlterCollection", "ApplyAddIndex", "the collection of the index", "collection",
			alter(func(n string) ddl.AlterOp {
				return ddl.AddIndex(dbschema.IndexDef{Name: index, Collection: n, Fields: []dal.FieldName{column}})
			}),
			func(n string) []string {
				return []string{`CREATE INDEX ` + q(index) + ` ON ` + q(n) + ` (` + q(o.column) + `)`}
			}},
		{"AlterCollection", "ApplyAddIndex", "the last field of the index", "field",
			alter(func(n string) ddl.AlterOp {
				return ddl.AddIndex(dbschema.IndexDef{Name: index, Fields: []dal.FieldName{column, dal.FieldName(n)}})
			}),
			func(n string) []string {
				return []string{`CREATE INDEX ` + q(index) + ` ON ` + q(o.table) + ` (` + q(o.column) + `, ` + q(n) + `)`}
			}},
		{"AlterCollection", "ApplyDropIndex", "the index name", "index",
			alter(func(n string) ddl.AlterOp { return ddl.DropIndex(n) }),
			func(n string) []string { return []string{`DROP INDEX ` + q(n)} }},
	}
}

// refusedDDLNames are the names every position refuses. A name of 32 two-byte
// characters is 64 bytes; it is refused for its characters too, so its test holds
// only that it is refused.
var refusedDDLNames = []struct {
	what, name string
	says       string // a part of the refusal, "" when either reason may be given
}{
	{"a double quote", `a"b`, "is not a plain identifier"},
	{"64 bytes", strings.Repeat("n", 64), "it keeps 63 bytes"},
	{"32 two-byte characters", strings.Repeat("é", 32), ""},
}

func TestDDLNames_EveryPositionRefusesAnUnsafeNameBeforeAnyStatement(t *testing.T) {
	for _, p := range ddlPositions() {
		for _, r := range refusedDDLNames {
			t.Run(p.entry+"/"+p.op+"/"+p.where+"/"+r.what, func(t *testing.T) {
				d, _ := newDDLMock(t) // no expectation: a transaction or a statement is not the refusal
				err := p.call(context.Background(), d, r.name)
				if !errors.Is(err, dalgo2sql.ErrUnsafeName) {
					t.Fatalf("error = %v, want a refusal that matches dalgo2sql.ErrUnsafeName", err)
				}
				if !strings.Contains(err.Error(), ": "+p.kind+" name ") {
					t.Errorf("error = %v, want it to call the name a %s name", err, p.kind)
				}
				if r.says != "" && !strings.Contains(err.Error(), r.says) {
					t.Errorf("error = %v, want it to say %q", err, r.says)
				}
			})
		}
	}
}

func TestDDLNames_EveryPositionAcceptsAName63BytesLongAndWritesIt(t *testing.T) {
	for _, p := range ddlPositions() {
		// The statement carries the name lower-cased, whatever its case.
		for _, name := range []string{strings.Repeat("n", 63), strings.Repeat("N", 63)} {
			t.Run(p.entry+"/"+p.op+"/"+p.where+"/"+name[:1], func(t *testing.T) {
				d, mock := newDDLMock(t)
				mock.ExpectBegin()
				for _, statement := range p.sent(strings.ToLower(name)) {
					mock.ExpectExec(statement).WillReturnResult(sqlmock.NewResult(0, 0))
				}
				mock.ExpectCommit()
				if err := p.call(context.Background(), d, name); err != nil {
					t.Fatalf("a name of 63 bytes: %v", err)
				}
			})
		}
	}
}

func TestDDLNames_AnOperationRefusedRefusesTheWholeCallBeforeItsFirstOperation(t *testing.T) {
	// The operations of a call are all built before the transaction begins, so a
	// valid operation ahead of a refused one is not sent either: the handle has no
	// expectation, and only the refusal is an answer of the call.
	ctx := context.Background()
	unsafe := func(err error) bool { return errors.Is(err, dalgo2sql.ErrUnsafeName) }
	for _, tc := range []struct {
		name    string
		ops     []ddl.AlterOp
		refused func(error) bool
	}{
		{"a field added, then one dropped", []ddl.AlterOp{
			ddl.AddField(dbschema.FieldDef{Name: "f", Type: dbschema.Int}),
			ddl.DropField(`a"b`),
		}, unsafe},
		{"an index dropped, then a field renamed", []ddl.AlterOp{
			ddl.DropIndex("ix"),
			ddl.RenameField("f", dal.FieldName(strings.Repeat("n", 64))),
		}, unsafe},
		{"a field dropped, then one added of a type there is none of", []ddl.AlterOp{
			ddl.DropField("f"),
			ddl.AddField(dbschema.FieldDef{Name: "g", Type: dbschema.Type(99)}),
		}, func(err error) bool { return err != nil && strings.Contains(err.Error(), "unknown dbschema.Type") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newDDLMock(t)
			if err := d.AlterCollection(ctx, "t", tc.ops...); !tc.refused(err) {
				t.Fatalf("error = %v, want the refusal of the operation that is refused", err)
			}
		})
	}
}

func TestDDLNames_EveryEntryAndOperationOfDDLHasItsPositionsRun(t *testing.T) {
	// A method added to ddl.SchemaModifier or to ddl.Applier (a new entry or a new
	// operation), or to Database under a name that creates, alters or drops, fails
	// here until the table above holds its positions.
	covered := map[string]bool{}
	for _, p := range ddlPositions() {
		covered[p.entry] = true
		if p.op != "" {
			covered[p.op] = true
		}
	}
	for _, iface := range []reflect.Type{
		reflect.TypeOf((*ddl.SchemaModifier)(nil)).Elem(),
		reflect.TypeOf((*ddl.Applier)(nil)).Elem(),
	} {
		for i := 0; i < iface.NumMethod(); i++ {
			if m := iface.Method(i).Name; !covered[m] {
				t.Errorf("%s.%s has no position in ddlPositions", iface.Name(), m)
			}
		}
	}
	database := reflect.TypeOf(&Database{})
	for i := 0; i < database.NumMethod(); i++ {
		m := database.Method(i).Name
		for _, prefix := range []string{"Create", "Drop", "Alter", "Rename", "Add", "Modify", "Truncate", "Apply"} {
			if strings.HasPrefix(m, prefix) && !covered[m] {
				t.Errorf("Database.%s looks like an entry that changes the schema and has no position in ddlPositions", m)
			}
		}
	}
}

func TestDDLNames_AColumnTypeIsFromAClosedListOrRefused(t *testing.T) {
	ctx := context.Background()
	for _, typ := range []dbschema.Type{dbschema.Null, dbschema.Type(99), dbschema.Type(-1)} {
		field := dbschema.FieldDef{Name: "f", Type: typ}
		for name, call := range map[string]func(d *Database) error{
			"CreateCollection": func(d *Database) error {
				return d.CreateCollection(ctx, dbschema.CollectionDef{Name: "t", Fields: []dbschema.FieldDef{field}})
			},
			"AddField":    func(d *Database) error { return d.AlterCollection(ctx, "t", ddl.AddField(field)) },
			"ModifyField": func(d *Database) error { return d.AlterCollection(ctx, "t", ddl.ModifyField("f", field)) },
		} {
			t.Run(name+"/"+typ.String(), func(t *testing.T) {
				d, _ := newDDLMock(t)
				err := call(d)
				if err == nil || !strings.Contains(err.Error(), "dbschema.") {
					t.Fatalf("error = %v, want the refusal of the type, before any transaction", err)
				}
			})
		}
	}
}

func TestDDLNames_ADefaultAndAutoIncrementAreNotWrittenIntoAStatement(t *testing.T) {
	// A column's default is not read by this package's DDL: whatever a caller sets
	// as Default (or AutoIncrement) is not part of the statement, so no text of a
	// caller's becomes a default.
	d, mock := newDDLMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(`CREATE TABLE "t" ("f" TEXT NOT NULL, "g" BIGINT NOT NULL)`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	err := d.CreateCollection(context.Background(), dbschema.CollectionDef{Name: "t", Fields: []dbschema.FieldDef{
		{Name: "f", Type: dbschema.String, Default: dbschema.DefaultLiteral{Value: `x'); y --`}},
		{Name: "g", Type: dbschema.Int, AutoIncrement: true, Default: dbschema.DefaultCurrentTimestamp{}},
	}})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
}

func TestDDLNames_OptionsAndFlagsAreWrittenAsTheirKeywords(t *testing.T) {
	ctx := context.Background()
	id := dbschema.FieldDef{Name: "id", Type: dbschema.Int}
	for _, tc := range []struct {
		name string
		call func(d *Database) error
		sent []string
	}{
		{"IfNotExists on a table and its unique index", func(d *Database) error {
			return d.CreateCollection(ctx, dbschema.CollectionDef{Name: "t", Fields: []dbschema.FieldDef{id},
				Indexes: []dbschema.IndexDef{{Name: "ix", Fields: []dal.FieldName{"id"}, Unique: true}}}, ddl.IfNotExists())
		}, []string{
			`CREATE TABLE IF NOT EXISTS "t" ("id" BIGINT NOT NULL)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS "ix" ON "t" ("id")`,
		}},
		{"IfExists on a table", func(d *Database) error { return d.DropCollection(ctx, "t", ddl.IfExists()) },
			[]string{`DROP TABLE IF EXISTS "t"`}},
		{"IfExists on an index", func(d *Database) error {
			return d.AlterCollection(ctx, "t", ddl.DropIndex("ix", ddl.IfExists()))
		}, []string{`DROP INDEX IF EXISTS "ix"`}},
		{"a field modified to be nullable", func(d *Database) error {
			return d.AlterCollection(ctx, "t", ddl.ModifyField("f", dbschema.FieldDef{Type: dbschema.String, Nullable: true}))
		}, []string{
			`ALTER TABLE "t" ALTER COLUMN "f" TYPE TEXT`,
			`ALTER TABLE "t" ALTER COLUMN "f" DROP NOT NULL`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock := newDDLMock(t)
			mock.ExpectBegin()
			for _, statement := range tc.sent {
				mock.ExpectExec(statement).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectCommit()
			if err := tc.call(d); err != nil {
				t.Fatalf("call: %v", err)
			}
		})
	}
}

func TestDDLNames_AnIndexWithNoFieldIsRefused(t *testing.T) {
	d, _ := newDDLMock(t)
	err := d.CreateCollection(context.Background(), dbschema.CollectionDef{Name: "t",
		Fields:  []dbschema.FieldDef{{Name: "id", Type: dbschema.Int}},
		Indexes: []dbschema.IndexDef{{Name: "ix"}}})
	if err == nil || !strings.Contains(err.Error(), "must have at least one field") {
		t.Fatalf("error = %v, want the refusal of an index with no field", err)
	}
}

func TestDDLNames_AFailingStatementRollsBackAndIsReportedUnderItsOperation(t *testing.T) {
	d, mock := newDDLMock(t)
	boom := errors.New("boom")
	mock.ExpectBegin()
	mock.ExpectExec(`ALTER TABLE "t" DROP COLUMN "f"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP INDEX "ix"`).WillReturnError(boom)
	mock.ExpectRollback()
	err := d.AlterCollection(context.Background(), "t", ddl.DropField("f"), ddl.DropIndex("ix"))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `ApplyDropIndex "ix"`) {
		t.Fatalf("error = %v, want boom reported under ApplyDropIndex", err)
	}
}

func TestQuoteIdent_WritesOneIdentifierWhateverItIsGiven(t *testing.T) {
	// quoteIdent is safe on its own, without its caller's check: a double quote in
	// a name is doubled, so the result is one quoted identifier in any case.
	for name, want := range map[string]string{
		`abc`:    `"abc"`,
		`ABC`:    `"abc"`,
		`a"b`:    `"a""b"`,
		`"`:      `""""`,
		`""`:     `""""""`,
		`a";b`:   `"a"";b"`,
		`a"b"c`:  `"a""b""c"`,
		`é"É`:    `"é""é"`,
		``:       `""`,
		`a b;--`: `"a b;--"`,
	} {
		if got := quoteIdent(name); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", name, got, want)
		}
	}

	// Every text of up to four characters from an alphabet that holds the quote, the
	// separators and non-ASCII letters is one identifier: the outer quotes enclose it,
	// each quote inside is doubled, and reading it back gives the lower-cased name.
	alphabet := []string{`"`, `a`, `A`, `;`, ` `, `é`, `\`, `'`}
	var walk func(prefix string, depth int)
	walk = func(prefix string, depth int) {
		got := quoteIdent(prefix)
		read, ok := readQuotedIdentifier(got)
		if !ok || read != strings.ToLower(prefix) {
			t.Errorf("quoteIdent(%q) = %s: is not one quoted identifier of %q (reads %q, %v)", prefix, got, strings.ToLower(prefix), read, ok)
		}
		if depth == 0 {
			return
		}
		for _, a := range alphabet {
			walk(prefix+a, depth-1)
		}
	}
	walk("", 4)
}

// readQuotedIdentifier reads s as the one quoted identifier PostgreSQL reads it as:
// double quotes around it, and a double quote inside it only as a doubled pair.
// ok is false when s is anything else (text after the closing quote included).
func readQuotedIdentifier(s string) (name string, ok bool) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", false
	}
	inner := s[1 : len(s)-1]
	var sb strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] == '"' {
			if i+1 >= len(inner) || inner[i+1] != '"' {
				return "", false
			}
			i++
		}
		sb.WriteByte(inner[i])
	}
	return sb.String(), true
}
