package dalgo2postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

func TestListCollections_Basic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	tbl1 := uniqueTable(t, "lc1")
	tbl2 := uniqueTable(t, "lc2")
	defer dropTable(t, db, tbl1)
	defer dropTable(t, db, tbl2)

	for _, stmt := range []string{
		`CREATE TABLE ` + quoteIdent(tbl1) + ` ("id" TEXT NOT NULL PRIMARY KEY)`,
		`CREATE TABLE ` + quoteIdent(tbl2) + ` ("id" TEXT NOT NULL PRIMARY KEY)`,
	} {
		if _, err := db.sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.ListCollections(ctx, nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	found := make(map[string]bool)
	for _, ref := range got {
		found[ref.Name()] = true
	}
	for _, want := range []string{tbl1, tbl2} {
		if !found[want] {
			t.Errorf("expected %q in ListCollections, not found; got %+v", want, got)
		}
	}
}

func TestDescribeCollection_NotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ref := dal.NewRootCollectionRef("no_such_table_xyz_123", "")
	got, err := db.DescribeCollection(ctx, &ref)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Errorf("expected nil CollectionDef on error, got %+v", got)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error message, got: %s", err.Error())
	}
}

func TestDescribeCollection_BasicRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tbl := uniqueTable(t, "desc")
	defer dropTable(t, db, tbl)

	stmt := `CREATE TABLE ` + quoteIdent(tbl) + ` (
		"id"         TEXT NOT NULL,
		"email"      TEXT NOT NULL,
		"age"        BIGINT,
		"score"      DOUBLE PRECISION,
		"active"     BOOLEAN,
		"created_at" TIMESTAMPTZ,
		"balance"    NUMERIC(10,2),
		"payload"    BYTEA,
		PRIMARY KEY ("id")
	)`
	if _, err := db.sqlDB.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}

	ref := dal.NewRootCollectionRef(tbl, "")
	got, err := db.DescribeCollection(ctx, &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if got.Name != tbl {
		t.Errorf("Name = %q, want %q", got.Name, tbl)
	}
	if len(got.Fields) != 8 {
		t.Fatalf("Fields len = %d, want 8; fields = %+v", len(got.Fields), got.Fields)
	}

	byName := make(map[string]dbschema.FieldDef)
	for _, f := range got.Fields {
		byName[string(f.Name)] = f
	}

	check := func(name string, want dbschema.Type, wantNullable bool) {
		t.Helper()
		f, ok := byName[name]
		if !ok {
			t.Errorf("field %q missing from DescribeCollection result", name)
			return
		}
		if f.Type != want {
			t.Errorf("field %q: Type = %v, want %v", name, f.Type, want)
		}
		if f.Nullable != wantNullable {
			t.Errorf("field %q: Nullable = %v, want %v", name, f.Nullable, wantNullable)
		}
	}
	check("id", dbschema.String, false)
	check("email", dbschema.String, false)
	check("age", dbschema.Int, true)
	check("score", dbschema.Float, true)
	check("active", dbschema.Bool, true)
	check("created_at", dbschema.Time, true)
	check("balance", dbschema.Decimal, true)
	check("payload", dbschema.Bytes, true)

	bal := byName["balance"]
	if bal.Precision == nil || bal.Precision.Total != 10 || bal.Precision.Scale != 2 {
		t.Errorf("balance Precision = %+v, want (10,2)", bal.Precision)
	}

	if len(got.PrimaryKey) != 1 || string(got.PrimaryKey[0]) != "id" {
		t.Errorf("PrimaryKey = %v, want [id]", got.PrimaryKey)
	}
	if got.SourceDefinition == nil || got.SourceDefinition.Dialect != "postgres" {
		t.Fatalf("SourceDefinition = %+v, want PostgreSQL native metadata", got.SourceDefinition)
	}
	columns := make(map[string]dbschema.SourceColumnDef, len(got.SourceDefinition.Columns))
	for _, column := range got.SourceDefinition.Columns {
		columns[column.Name] = column
	}
	if column := columns["created_at"]; column.DeclaredType != "timestamp with time zone" || column.NotNull {
		t.Errorf("created_at source metadata = %+v, want nullable timestamp with time zone", column)
	}
	if column := columns["id"]; column.PrimaryKeyPosition != 1 || !column.NotNull {
		t.Errorf("id source metadata = %+v, want primary-key position 1 and NOT NULL", column)
	}
}

func TestListIndexes_ExcludesPK(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tbl := uniqueTable(t, "lidx")
	defer dropTable(t, db, tbl)

	for _, stmt := range []string{
		`CREATE TABLE ` + quoteIdent(tbl) + ` ("id" TEXT NOT NULL, "email" TEXT, PRIMARY KEY ("id"))`,
		`CREATE INDEX ` + quoteIdent(tbl+"_email_idx") + ` ON ` + quoteIdent(tbl) + ` ("email")`,
		`CREATE UNIQUE INDEX ` + quoteIdent(tbl+"_email_uq") + ` ON ` + quoteIdent(tbl) + ` ("email")`,
	} {
		if _, err := db.sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	ref := dal.NewRootCollectionRef(tbl, "")
	got, err := db.ListIndexes(ctx, &ref)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 user-defined indexes (PK excluded), got %d: %+v", len(got), got)
	}
	wantNames := map[string]bool{
		tbl + "_email_idx": false,
		tbl + "_email_uq":  true,
	}
	for _, idx := range got {
		wantUnique, known := wantNames[idx.Name]
		if !known {
			t.Errorf("unexpected index %q", idx.Name)
			continue
		}
		if idx.Unique != wantUnique {
			t.Errorf("index %q: Unique = %v, want %v", idx.Name, idx.Unique, wantUnique)
		}
	}
}
