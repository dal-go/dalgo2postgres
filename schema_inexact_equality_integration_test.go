package dalgo2postgres

import (
	"context"
	"reflect"
	"testing"
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
	)

	ctx := context.Background()
	got, err := db.NonDeterministicTextColumns(ctx, "Customer")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Email", "Name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Customer: got %v, want %v", got, want)
	}

	got, err = db.NonDeterministicTextColumns(ctx, "Plain")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Plain: got %v, want nothing", got)
	}

	got, err = db.NonDeterministicTextColumns(ctx, "NoSuchTable")
	if err != nil || len(got) != 0 {
		t.Fatalf("missing table: got %v, %v; want nothing and no error", got, err)
	}
}
