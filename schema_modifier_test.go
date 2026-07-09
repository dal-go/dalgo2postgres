package dalgo2postgres

import (
	"context"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
)

func TestCreateCollection_HappyPath(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tbl := uniqueTable(t, "users")
	defer dropTable(t, db, tbl)

	c := dbschema.CollectionDef{
		Name: tbl,
		Fields: []dbschema.FieldDef{
			{Name: dal.FieldName("id"), Type: dbschema.String, Nullable: false},
			{Name: dal.FieldName("email"), Type: dbschema.String, Nullable: false},
			{Name: dal.FieldName("age"), Type: dbschema.Int, Nullable: true},
			{Name: dal.FieldName("score"), Type: dbschema.Float, Nullable: true},
			{Name: dal.FieldName("active"), Type: dbschema.Bool, Nullable: true},
			{Name: dal.FieldName("created_at"), Type: dbschema.Time, Nullable: true},
			{Name: dal.FieldName("balance"), Type: dbschema.Decimal, Nullable: true},
			{Name: dal.FieldName("payload"), Type: dbschema.Bytes, Nullable: true},
		},
		PrimaryKey: []dal.FieldName{"id"},
		Indexes: []dbschema.IndexDef{
			{Name: tbl + "_email_idx", Collection: tbl, Fields: []dal.FieldName{"email"}},
		},
	}

	if err := ddl.CreateCollection(ctx, db, c); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	ref := dal.NewRootCollectionRef(tbl, "")
	got, err := db.DescribeCollection(ctx, &ref)
	if err != nil {
		t.Fatalf("DescribeCollection after Create: %v", err)
	}
	if len(got.Fields) != len(c.Fields) {
		t.Errorf("Fields len = %d, want %d", len(got.Fields), len(c.Fields))
	}
	if len(got.PrimaryKey) != 1 || string(got.PrimaryKey[0]) != "id" {
		t.Errorf("PrimaryKey = %v, want [id]", got.PrimaryKey)
	}
	if len(got.Indexes) != 1 {
		t.Errorf("Indexes len = %d, want 1; got %+v", len(got.Indexes), got.Indexes)
	}
}

func TestCreateCollection_IfNotExists(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tbl := uniqueTable(t, "ine")
	defer dropTable(t, db, tbl)

	c := dbschema.CollectionDef{
		Name:       tbl,
		Fields:     []dbschema.FieldDef{{Name: dal.FieldName("id"), Type: dbschema.String, Nullable: false}},
		PrimaryKey: []dal.FieldName{"id"},
	}
	if err := ddl.CreateCollection(ctx, db, c); err != nil {
		t.Fatalf("first CreateCollection: %v", err)
	}
	if err := ddl.CreateCollection(ctx, db, c, ddl.IfNotExists()); err != nil {
		t.Fatalf("CreateCollection with IfNotExists on existing table: %v", err)
	}
}

func TestDropCollection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tbl := uniqueTable(t, "drop")
	// Don't defer drop — the test drops it explicitly.

	c := dbschema.CollectionDef{
		Name:       tbl,
		Fields:     []dbschema.FieldDef{{Name: dal.FieldName("id"), Type: dbschema.String, Nullable: false}},
		PrimaryKey: []dal.FieldName{"id"},
	}
	if err := ddl.CreateCollection(ctx, db, c); err != nil {
		t.Fatal(err)
	}
	if err := ddl.DropCollection(ctx, db, tbl); err != nil {
		t.Fatalf("DropCollection: %v", err)
	}
	tables, _ := db.ListCollections(ctx, nil)
	for _, t2 := range tables {
		if t2.Name() == tbl {
			t.Errorf("%q still listed after DropCollection", tbl)
		}
	}
	// IfExists tolerates absent table.
	if err := ddl.DropCollection(ctx, db, tbl+"_nonexistent", ddl.IfExists()); err != nil {
		t.Errorf("DropCollection IfExists on missing table: %v", err)
	}
}

func TestAlterCollection_AddDropRenameField(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tbl := uniqueTable(t, "alter")
	defer dropTable(t, db, tbl)

	c := dbschema.CollectionDef{
		Name: tbl,
		Fields: []dbschema.FieldDef{
			{Name: dal.FieldName("id"), Type: dbschema.String, Nullable: false},
			{Name: dal.FieldName("email"), Type: dbschema.String, Nullable: true},
		},
		PrimaryKey: []dal.FieldName{"id"},
	}
	if err := ddl.CreateCollection(ctx, db, c); err != nil {
		t.Fatal(err)
	}

	err := ddl.AlterCollection(ctx, db, tbl,
		ddl.AddField(dbschema.FieldDef{Name: dal.FieldName("age"), Type: dbschema.Int, Nullable: true}),
		ddl.RenameField(dal.FieldName("email"), dal.FieldName("email_address")),
		ddl.DropField(dal.FieldName("age")),
	)
	if err != nil {
		t.Fatalf("AlterCollection: %v", err)
	}

	ref := dal.NewRootCollectionRef(tbl, "")
	got, err := db.DescribeCollection(ctx, &ref)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, f := range got.Fields {
		names[string(f.Name)] = true
	}
	if !names["id"] || !names["email_address"] {
		t.Errorf("expected fields id + email_address, got %+v", names)
	}
	if names["email"] || names["age"] {
		t.Errorf("unexpected residual fields after alter; got %+v", names)
	}
}

func TestAlterCollection_AddDropIndex(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tbl := uniqueTable(t, "aidx")
	defer dropTable(t, db, tbl)

	c := dbschema.CollectionDef{
		Name: tbl,
		Fields: []dbschema.FieldDef{
			{Name: dal.FieldName("id"), Type: dbschema.String, Nullable: false},
			{Name: dal.FieldName("email"), Type: dbschema.String, Nullable: true},
		},
		PrimaryKey: []dal.FieldName{"id"},
	}
	if err := ddl.CreateCollection(ctx, db, c); err != nil {
		t.Fatal(err)
	}

	idxName := tbl + "_email_idx"
	addIdx := dbschema.IndexDef{Name: idxName, Collection: tbl, Fields: []dal.FieldName{"email"}}
	if err := ddl.AlterCollection(ctx, db, tbl, ddl.AddIndex(addIdx), ddl.DropIndex(idxName)); err != nil {
		t.Fatalf("AlterCollection add+drop index: %v", err)
	}

	ref := dal.NewRootCollectionRef(tbl, "")
	idxs, _ := db.ListIndexes(ctx, &ref)
	for _, idx := range idxs {
		if idx.Name == idxName {
			t.Errorf("index %q still present after drop", idxName)
		}
	}
}
