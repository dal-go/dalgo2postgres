package dalgo2postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// Runs against a real PostgreSQL server and skips unless DALGO2POSTGRES_TEST_DSN
// is set (see testDSN); CI sets it.
func TestNonDeterministicTextColumnsIntegration(t *testing.T) {
	schemaName := uniqueTable(t, "pg04")
	db := openSchemaTestDB(t, schemaName, IdentifierExact)
	s := exactIdent(schemaName)

	// citext ships with the contrib modules of the postgres image CI uses. It is
	// created once in the test database and left in place (another run may be
	// using it), hence IF NOT EXISTS and no cleanup.
	execAll(t, db,
		`CREATE EXTENSION IF NOT EXISTS citext`,
		`CREATE COLLATION `+s+`."ci_ai" (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
		`CREATE TABLE `+s+`."Customer" (
			"Id" integer PRIMARY KEY,
			"Email" citext,
			"Name" text COLLATE `+s+`."ci_ai",
			"Plain" text,
			"Code" text COLLATE "C",
			"Gone" citext,
			"Age" integer)`,
		`CREATE TABLE `+s+`."Plain" ("Id" integer PRIMARY KEY, "Label" text)`,
		`ALTER TABLE `+s+`."Customer" DROP COLUMN "Gone"`,
		`CREATE INDEX "Customer_Email_idx" ON `+s+`."Customer" ("Email")`,
	)

	ctx := context.Background()
	colsOf := func(d *Database, ref dal.CollectionRef) ([]string, error) {
		return d.NonDeterministicTextColumns(ctx, &ref)
	}

	got, err := colsOf(db, dal.NewRootCollectionRef("Customer", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Email", "Name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Customer: got %v, want %v", got, want)
	}

	got, err = colsOf(db, dal.NewRootCollectionRef("Plain", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("Plain: got %#v, want an empty non-nil slice", got)
	}

	// A missing table, and an index name (not a table or view), are not found:
	// never an empty list that would read as "equality is exact".
	for _, name := range []string{"NoSuchTable", "Customer_Email_idx"} {
		got, err = colsOf(db, dal.NewRootCollectionRef(name, ""))
		if got != nil || err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("%s: got %v, %v; want nil and a not-found error", name, got, err)
		}
	}

	// A reference that names its own schema is read from that schema, even on a
	// Database configured for another one.
	other := openSchemaTestDB(t, uniqueTable(t, "pg04b"), IdentifierExact)
	got, err = colsOf(other, dal.NewQualifiedRootCollectionRef(schemaName, "Customer", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Email", "Name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("qualified Customer: got %v, want %v", got, want)
	}
	if _, err = colsOf(other, dal.NewRootCollectionRef("Customer", "")); err == nil {
		t.Fatal("unqualified Customer on the other schema: want not found")
	}
}
