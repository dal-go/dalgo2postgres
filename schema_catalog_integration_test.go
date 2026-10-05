package dalgo2postgres

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// The tests in this file run against a real PostgreSQL server and skip unless
// DALGO2POSTGRES_TEST_DSN is set (see testDSN); CI sets it, on PostgreSQL 17 and
// 18. They are the evidence for the schema reader's schemas, views and defaults.

// pg05Fixture is a database with two schemas beside public:
//
//	first  (the schema the returned Database is configured for)
//	  "Order Items"  table whose name needs quoting, with a constant default, now(), a
//	                 sequence default, an identity column and a generated column
//	  "Open Orders"  view
//	  order_totals   materialized view
//	  events         partitioned table
//	  events_2026    partition of events
//	  remote_orders  foreign table
//	  hits_seq       sequence (not a collection)
//	second
//	  "Shipments"    table
//	  shipment_ids   view
type pg05Fixture struct {
	db            *Database
	first, second string
}

func newPG05Fixture(t *testing.T) pg05Fixture {
	t.Helper()
	first := uniqueTable(t, "pg05a")
	second := uniqueTable(t, "pg05b")
	db := openSchemaTestDB(t, first, IdentifierExact)
	ctx := context.Background()
	s1, s2 := exactIdent(first), exactIdent(second)

	if _, err := db.sqlDB.ExecContext(ctx, `CREATE SCHEMA `+s2); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	// The foreign data wrapper and server belong to the database, not to a schema,
	// so they are dropped by name; the foreign table goes with them.
	const wrapper, server = "dalgo_pg05_wrapper", "dalgo_pg05_server"
	dropForeign := func() {
		_, _ = db.sqlDB.ExecContext(context.Background(), `DROP SERVER IF EXISTS `+server+` CASCADE`)
		_, _ = db.sqlDB.ExecContext(context.Background(), `DROP FOREIGN DATA WRAPPER IF EXISTS `+wrapper+` CASCADE`)
	}
	dropForeign()
	t.Cleanup(func() {
		dropForeign()
		_, _ = db.sqlDB.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+s2+` CASCADE`)
	})

	execAll(t, db,
		`CREATE SEQUENCE `+s1+`.hits_seq`,
		`CREATE TABLE `+s1+`."Order Items" (
			"Id" integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			"Status" text NOT NULL DEFAULT 'new',
			"Code" varchar(20),
			"Made" timestamptz NOT NULL DEFAULT now(),
			"Hits" bigint NOT NULL DEFAULT nextval('`+s1+`.hits_seq'),
			"Qty" integer NOT NULL DEFAULT 1,
			"Price" numeric(10,2),
			"Double" integer GENERATED ALWAYS AS ("Qty" * 2) STORED,
			"Note" text
		)`,
		`CREATE VIEW `+s1+`."Open Orders" AS SELECT "Id", "Status" FROM `+s1+`."Order Items"`,
		`CREATE MATERIALIZED VIEW `+s1+`.order_totals AS SELECT "Id", "Code", "Price", "Made" FROM `+s1+`."Order Items"`,
		`CREATE TABLE `+s1+`.events (at timestamptz NOT NULL, kind text) PARTITION BY RANGE (at)`,
		`CREATE TABLE `+s1+`.events_2026 PARTITION OF `+s1+`.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`CREATE FOREIGN DATA WRAPPER `+wrapper,
		`CREATE SERVER `+server+` FOREIGN DATA WRAPPER `+wrapper,
		`CREATE FOREIGN TABLE `+s1+`.remote_orders (id integer) SERVER `+server,
		`CREATE TABLE `+s2+`."Shipments" ("ShipmentId" integer PRIMARY KEY)`,
		`CREATE VIEW `+s2+`.shipment_ids AS SELECT "ShipmentId" FROM `+s2+`."Shipments"`,
	)
	return pg05Fixture{db: db, first: first, second: second}
}

// qualified is what the reader answers for a collection that names its schema.
func qualified(schema string, names ...string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = schema + "|" + name
	}
	return out
}

// pg05Tables and pg05Views are the first schema's collections, in the order the
// server sorts names (the C collation: capitals first, an underscore after a prefix).
var (
	pg05Collections = []string{"Open Orders", "Order Items", "events", "events_2026", "order_totals", "remote_orders"}
	pg05Views       = []string{"Open Orders", "order_totals"}
)

func TestSchemaReaderIntegration_ListsEverySchemaButTheSystemOnes(t *testing.T) {
	f := newPG05Fixture(t)
	got, err := f.db.ListSchemas(context.Background())
	if err != nil {
		t.Fatalf("ListSchemas: %v", err)
	}
	want := []string{"public", f.first, f.second}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListSchemas = %v, want %v (no pg_catalog, information_schema, pg_toast or pg_temp_N)", got, want)
	}
}

func TestSchemaReaderIntegration_ListsViewsAndMaterializedViewsApartFromTables(t *testing.T) {
	f := newPG05Fixture(t)
	ctx := context.Background()

	// Unchanged callers: the configured schema, no schema on a reference. The view
	// is still listed (it always was); the materialized view, the partitioned
	// table, the partition and the foreign table are new.
	refs, err := f.db.ListCollections(ctx, nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if got := refNames(refs); !reflect.DeepEqual(got, prefix("|", pg05Collections)) {
		t.Errorf("ListCollections = %v, want %v", got, prefix("|", pg05Collections))
	}
	views, err := f.db.ListViews(ctx)
	if err != nil {
		t.Fatalf("ListViews: %v", err)
	}
	if got := refNames(views); !reflect.DeepEqual(got, prefix("|", pg05Views)) {
		t.Errorf("ListViews = %v, want %v (the view and the materialized view; not the foreign or partitioned table, not the partition)", got, prefix("|", pg05Views))
	}

	// The same schema asked for by name: every reference names it.
	named, err := f.db.ListSchemaCollections(ctx, f.first)
	if err != nil {
		t.Fatalf("ListSchemaCollections: %v", err)
	}
	if got := refNames(named); !reflect.DeepEqual(got, qualified(f.first, pg05Collections...)) {
		t.Errorf("ListSchemaCollections(first) = %v", got)
	}
	namedViews, err := f.db.ListSchemaViews(ctx, f.first)
	if err != nil {
		t.Fatalf("ListSchemaViews: %v", err)
	}
	if got := refNames(namedViews); !reflect.DeepEqual(got, qualified(f.first, pg05Views...)) {
		t.Errorf("ListSchemaViews(first) = %v", got)
	}

	// The other schema, which the Database is not configured for.
	other, err := f.db.ListSchemaCollections(ctx, f.second)
	if err != nil {
		t.Fatalf("ListSchemaCollections(second): %v", err)
	}
	if got := refNames(other); !reflect.DeepEqual(got, qualified(f.second, "Shipments", "shipment_ids")) {
		t.Errorf("ListSchemaCollections(second) = %v", got)
	}
	otherViews, err := f.db.ListSchemaViews(ctx, f.second)
	if err != nil {
		t.Fatalf("ListSchemaViews(second): %v", err)
	}
	if got := refNames(otherViews); !reflect.DeepEqual(got, qualified(f.second, "shipment_ids")) {
		t.Errorf("ListSchemaViews(second) = %v", got)
	}

	// A schema nobody has is no collections, not an error.
	if none, err := f.db.ListSchemaCollections(ctx, "no_such_schema_pg05"); err != nil || len(none) != 0 {
		t.Errorf("ListSchemaCollections(missing) = %v, %v; want none", none, err)
	}
}

func prefix(p string, names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = p + name
	}
	return out
}

func TestSchemaReaderIntegration_DescribesEveryListedCollectionOfEverySchema(t *testing.T) {
	f := newPG05Fixture(t)
	ctx := context.Background()
	for _, schema := range []string{f.first, f.second} {
		refs, err := f.db.ListSchemaCollections(ctx, schema)
		if err != nil {
			t.Fatalf("ListSchemaCollections(%s): %v", schema, err)
		}
		for _, ref := range refs {
			def, err := f.db.DescribeCollection(ctx, &ref)
			if err != nil {
				t.Errorf("DescribeCollection(%s.%s): %v", schema, ref.Name(), err)
				continue
			}
			if def.Name != ref.Name() || len(def.Fields) == 0 {
				t.Errorf("DescribeCollection(%s.%s) = %+v", schema, ref.Name(), def)
			}
		}
	}

	// The second schema's table, which a Database configured for the first never sees
	// by a bare name, is described through the reference that names its schema.
	shipments := dal.NewQualifiedRootCollectionRef(f.second, "Shipments", "")
	def, err := f.db.DescribeCollection(ctx, &shipments)
	if err != nil {
		t.Fatalf("DescribeCollection(second.Shipments): %v", err)
	}
	if !reflect.DeepEqual(def.PrimaryKey, []dal.FieldName{"ShipmentId"}) || len(def.Fields) != 1 || def.Fields[0].Type != dbschema.Int {
		t.Errorf("def = %+v", def)
	}
	bare := dal.NewRootCollectionRef("Shipments", "")
	if got, err := f.db.DescribeCollection(ctx, &bare); err == nil || got != nil {
		t.Errorf("DescribeCollection(Shipments) with no schema = %v, %v; want not found (the configured schema has none)", got, err)
	}

	// A materialized view has columns, with the types and sizes of its source.
	totals := dal.NewQualifiedRootCollectionRef(f.first, "order_totals", "")
	mv, err := f.db.DescribeCollection(ctx, &totals)
	if err != nil {
		t.Fatalf("DescribeCollection(order_totals): %v", err)
	}
	twenty := 20
	wantMV := []dbschema.FieldDef{
		{Name: "Id", Type: dbschema.Int, Nullable: true},
		{Name: "Code", Type: dbschema.String, Length: &twenty, Nullable: true},
		{Name: "Price", Type: dbschema.Decimal, Precision: &dbschema.Precision{Total: 10, Scale: 2}, Nullable: true},
		{Name: "Made", Type: dbschema.Time, Nullable: true},
	}
	if !reflect.DeepEqual(mv.Fields, wantMV) {
		t.Errorf("order_totals fields = %+v\nwant %+v", mv.Fields, wantMV)
	}
}

func TestSchemaReaderIntegration_ReadsDefaultsIdentityAndGeneratedColumns(t *testing.T) {
	f := newPG05Fixture(t)
	ref := dal.NewRootCollectionRef("Order Items", "")
	def, err := f.db.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	twenty := 20
	literal := func(text string) dbschema.DefaultExpr { return dbschema.DefaultLiteral{Value: text} }
	want := []dbschema.FieldDef{
		// identity: told by AutoIncrement, and it has no default
		{Name: "Id", Type: dbschema.Int, AutoIncrement: true},
		// a constant default, with the cast PostgreSQL prints
		{Name: "Status", Type: dbschema.String, Default: literal(`'new'::text`)},
		{Name: "Code", Type: dbschema.String, Length: &twenty, Nullable: true},
		{Name: "Made", Type: dbschema.Time, Default: literal(`now()`)},
		// a sequence default is a plain default to the catalog
		{Name: "Hits", Type: dbschema.Int, Default: literal(`nextval('` + f.first + `.hits_seq'::regclass)`)},
		{Name: "Qty", Type: dbschema.Int, Default: literal(`1`)},
		{Name: "Price", Type: dbschema.Decimal, Precision: &dbschema.Precision{Total: 10, Scale: 2}, Nullable: true},
		// a generated column: its expression, in the form GENERATED ALWAYS AS (...)
		{Name: "Double", Type: dbschema.Int, Nullable: true, Default: literal(`GENERATED ALWAYS AS (("Qty" * 2))`)},
		{Name: "Note", Type: dbschema.String, Nullable: true},
	}
	if !reflect.DeepEqual(def.Fields, want) {
		for i := range def.Fields {
			if i < len(want) && !reflect.DeepEqual(def.Fields[i], want[i]) {
				t.Errorf("field %d:\n got %+v\nwant %+v", i, def.Fields[i], want[i])
			}
		}
		t.Errorf("fields = %d, want %d", len(def.Fields), len(want))
	}
}
