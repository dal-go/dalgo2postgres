package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// These tests run against a real PostgreSQL server and skip unless
// DALGO2POSTGRES_TEST_DSN is set (see testDSN); CI sets it, on PostgreSQL 17 and 18.

// fieldNames lists the names of the fields of def, in order.
func fieldNames(def *dbschema.CollectionDef) []string {
	names := make([]string, 0, len(def.Fields))
	for _, f := range def.Fields {
		names = append(names, string(f.Name))
	}
	return names
}

// indexNames lists the names of indexes.
func indexNames(indexes []dbschema.IndexDef) []string {
	names := make([]string, 0, len(indexes))
	for _, ix := range indexes {
		names = append(names, ix.Name)
	}
	return names
}

// The entries that create, alter and drop refuse a name that is not a plain
// identifier of at most 63 bytes, at every position of a name, before any
// statement. This test calls every entry at every position with the refused names
// (the name of an object that exists plus one byte, a double quote, 32 two-byte
// characters) on a table of 63 bytes with a column and an index of 63 bytes and
// rows, and a second, unrelated table. The handle is traced: a refused call sends
// no statement, not even the beginning of a transaction. Afterwards every object is
// still there with its rows and its columns, and the names of 63 bytes then work in
// every entry.
func TestDDLNamesIntegration_RefusedNamesLeaveEveryObjectAsItWas(t *testing.T) {
	objects := ddlObjects{table: nameOf63("ddl09_t_"), column: nameOf63("ddl09_c_"), index: nameOf63("ddl09_ix_")}
	const other = "ddl09_other"
	f := openQueryFixture(t, uniqueTable(t, "ddl09"), IdentifierFoldLower, []string{
		`CREATE TABLE ` + objects.table + ` (id integer PRIMARY KEY, label text, ` + objects.column + ` text)`,
		`CREATE INDEX ` + objects.index + ` ON ` + objects.table + ` (label)`,
		`INSERT INTO ` + objects.table + ` (id, label, ` + objects.column + `) VALUES (1, 'one', 'a'), (2, 'two', 'b')`,
		`CREATE TABLE ` + other + ` (id integer PRIMARY KEY, note text)`,
		`INSERT INTO ` + other + ` (id, note) VALUES (1, 'kept'), (2, 'kept too')`,
	})
	ctx := context.Background()
	tableRef := dal.NewRootCollectionRef(objects.table, "")

	// assertIntact checks every object of the fixture: the rows of both tables, the
	// columns and the index of the first, and that no other table is there.
	assertIntact := func(when string, columns []string, indexes []string) {
		t.Helper()
		for table, rows := range map[string]int{objects.table: 2, other: 2} {
			var n int
			if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != rows {
				t.Errorf("%s: %s has %d rows (error %v), want %d", when, table, n, err, rows)
			}
		}
		def, err := f.db.DescribeCollection(ctx, &tableRef)
		if err != nil {
			t.Fatalf("%s: DescribeCollection: %v", when, err)
		}
		if got := fieldNames(def); !reflect.DeepEqual(got, columns) {
			t.Errorf("%s: columns = %v, want %v", when, got, columns)
		}
		listed, err := f.db.ListIndexes(ctx, &tableRef)
		if err != nil {
			t.Fatalf("%s: ListIndexes: %v", when, err)
		}
		if got := indexNames(listed); !reflect.DeepEqual(got, indexes) {
			t.Errorf("%s: indexes = %v, want %v", when, got, indexes)
		}
		refs, err := f.db.ListCollections(ctx, nil)
		if err != nil {
			t.Fatalf("%s: ListCollections: %v", when, err)
		}
		if got, want := collectionNames(refs), []string{other, objects.table}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: tables = %v, want %v", when, got, want)
		}
	}
	original := []string{"id", "label", objects.column}
	assertIntact("fixture", original, []string{objects.index})

	valid := map[string]string{
		"collection": objects.table, "field": objects.column, "primary key": objects.column, "index": objects.index,
	}
	for _, p := range ddlPositionsOn(objects) {
		for _, refused := range []struct{ what, name string }{
			{"the name of an object and one more byte", valid[p.kind] + "x"},
			{"a double quote", `a"b`},
			{"32 two-byte characters", strings.Repeat("é", 32)},
		} {
			f.trace.reset()
			err := p.call(ctx, f.db, refused.name)
			if !errors.Is(err, dalgo2sql.ErrUnsafeName) {
				t.Errorf("%s %s %s with %s: error = %v, want a refusal that matches dalgo2sql.ErrUnsafeName",
					p.entry, p.op, p.where, refused.what, err)
			}
			if sent := f.trace.sent(); len(sent) != 0 {
				t.Errorf("%s %s %s with %s: %d statements were sent, want none", p.entry, p.op, p.where, refused.what, len(sent))
			}
		}
	}
	assertIntact("after the refused names", original, []string{objects.index})

	// The names of 63 bytes work in every entry.
	created, createdColumn, createdIndex := nameOf63("ddl09_n_"), nameOf63("ddl09_nc_"), nameOf63("ddl09_ni_")
	if err := f.db.CreateCollection(ctx, dbschema.CollectionDef{
		Name:       created,
		Fields:     []dbschema.FieldDef{{Name: "id", Type: dbschema.Int}, {Name: dal.FieldName(createdColumn), Type: dbschema.String, Nullable: true}},
		PrimaryKey: []dal.FieldName{"id"},
		Indexes:    []dbschema.IndexDef{{Name: createdIndex, Fields: []dal.FieldName{dal.FieldName(createdColumn)}}},
	}); err != nil {
		t.Fatalf("CreateCollection with names of 63 bytes: %v", err)
	}
	createdRef := dal.NewRootCollectionRef(created, "")
	if def, err := f.db.DescribeCollection(ctx, &createdRef); err != nil || !reflect.DeepEqual(fieldNames(def), []string{"id", createdColumn}) {
		t.Errorf("DescribeCollection of the created table = %v, %v; want its two columns", def, err)
	}
	if indexes, err := f.db.ListIndexes(ctx, &createdRef); err != nil || !reflect.DeepEqual(indexNames(indexes), []string{createdIndex}) {
		t.Errorf("ListIndexes of the created table = %v, %v; want its index", indexes, err)
	}

	added, renamed, altered := nameOf63("ddl09_ac_"), nameOf63("ddl09_rc_"), nameOf63("ddl09_ai_")
	if err := f.db.AlterCollection(ctx, objects.table,
		ddl.AddField(dbschema.FieldDef{Name: dal.FieldName(added), Type: dbschema.Int, Nullable: true}),
		ddl.RenameField(dal.FieldName(added), dal.FieldName(renamed)),
		ddl.ModifyField(dal.FieldName(renamed), dbschema.FieldDef{Type: dbschema.Float, Nullable: true}),
		ddl.AddIndex(dbschema.IndexDef{Name: altered, Fields: []dal.FieldName{dal.FieldName(renamed)}}),
		ddl.DropIndex(objects.index),
		ddl.DropField(dal.FieldName(objects.column)),
	); err != nil {
		t.Fatalf("AlterCollection with names of 63 bytes: %v", err)
	}
	if err := f.db.DropCollection(ctx, created); err != nil {
		t.Fatalf("DropCollection with a name of 63 bytes: %v", err)
	}
	assertIntact("after the names of 63 bytes", []string{"id", "label", renamed}, []string{altered})
}

// The schema reader compares whole names in any database encoding: it sends each
// name as text, so the server compares the whole text with the stored name. This
// test makes a database of its own in the encoding EUC_JP, holding a table named by
// 21 accented letters. The table is found by its own name, and by no other: a name of
// 26 of the same letter, which passes the check of 63 bytes in Go and so reaches the
// server, is not the table's.
func TestSchemaReaderIntegration_NamesAreComparedWholeInAnyDatabaseEncoding(t *testing.T) {
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	database := uniqueTable(t, "enc")
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+exactIdent(database)+` WITH (FORCE)`); err != nil {
		t.Fatalf("DROP DATABASE: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+exactIdent(database)+` TEMPLATE template0 ENCODING 'EUC_JP' LC_COLLATE 'C' LC_CTYPE 'C'`); err != nil {
		t.Fatalf("CREATE DATABASE in the encoding EUC_JP: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+exactIdent(database)+` WITH (FORCE)`)
	})

	// The names go to the server as UTF-8 text, so the session says so: the server
	// then converts them to the encoding of the database, as it does for any client.
	inEncoding := cfg.Copy()
	inEncoding.Database = database
	inEncoding.RuntimeParams["client_encoding"] = "UTF8"
	name := stdlib.RegisterConnConfig(inEncoding)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(name) })
	db, err := NewDatabase(name, WithIdentifierMode(IdentifierExact))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var encoding string
	if err := db.sqlDB.QueryRowContext(ctx, `SHOW server_encoding`).Scan(&encoding); err != nil || encoding != "EUC_JP" {
		t.Fatalf("server_encoding = %q, %v; want EUC_JP", encoding, err)
	}

	whole := strings.Repeat("é", 21)
	if _, err := db.sqlDB.ExecContext(ctx, `CREATE TABLE `+exactIdent(whole)+` (id integer PRIMARY KEY, label text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	longer := strings.Repeat("é", 26)
	if len(longer) > maxIdentifierBytes {
		t.Fatalf("the name of 26 letters is %d bytes, want it to pass the check in Go and reach the server", len(longer))
	}

	byWhole, byLonger := dal.NewRootCollectionRef(whole, ""), dal.NewRootCollectionRef(longer, "")
	if def, err := db.DescribeCollection(ctx, &byWhole); err != nil || len(def.Fields) != 2 {
		t.Fatalf("DescribeCollection by the whole name = %v, %v; want its two columns", def, err)
	}
	if constraints, err := db.ListConstraints(ctx, &byWhole); err != nil || len(constraints) != 1 {
		t.Errorf("ListConstraints by the whole name = %v, %v; want its primary key", constraints, err)
	}
	if _, err := db.NonDeterministicTextColumns(ctx, &byWhole); err != nil {
		t.Errorf("NonDeterministicTextColumns by the whole name: %v", err)
	}

	if def, err := db.DescribeCollection(ctx, &byLonger); err == nil || def != nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("DescribeCollection by a longer name = %v, %v; want not found", def, err)
	}
	if constraints, err := db.ListConstraints(ctx, &byLonger); err != nil || len(constraints) != 0 {
		t.Errorf("ListConstraints by a longer name = %v, %v; want no constraint", constraints, err)
	}
	if referrers, err := db.ListReferrers(ctx, &byLonger); err != nil || len(referrers) != 0 {
		t.Errorf("ListReferrers by a longer name = %v, %v; want no referrer", referrers, err)
	}
	if _, err := db.NonDeterministicTextColumns(ctx, &byLonger); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("NonDeterministicTextColumns by a longer name: error = %v, want not found", err)
	}
}
