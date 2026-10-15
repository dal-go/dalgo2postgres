package dalgo2postgres

import (
	"testing"

	"github.com/dal-go/dalgo2sql"
)

// The rule every read keeps (see unique_keys_test.go): within one read, no two records have
// the same key. This test runs the reads of uniqueKeysCases on PostgreSQL, over tables whose
// rows repeat their non-key values, where the unit test runs them over a stand-in.
//
// It runs against PostgreSQL and skips unless DALGO2POSTGRES_TEST_DSN is set; CI fails when
// TestStructuredQueryIntegration_EveryReadKeepsItsKeysUnique skips or is missing.

func TestStructuredQueryIntegration_EveryReadKeepsItsKeysUnique(t *testing.T) {
	// Not parallel: connects to the shared DALGO2POSTGRES_TEST_DSN database.
	ddl := []string{
		`CREATE TABLE invoice (invoiceid integer PRIMARY KEY, customerid integer NOT NULL, billingcountry text, total numeric)`,
		`INSERT INTO invoice SELECT i, 1 + (i % 59), (ARRAY['USA', 'France', 'Brazil'])[1 + i % 3], 0.99 + (i % 3) FROM generate_series(1, 412) AS i`,
		`CREATE TABLE customer (customerid integer PRIMARY KEY, country text)`,
		`INSERT INTO customer SELECT i, CASE i % 2 WHEN 0 THEN 'USA' ELSE 'France' END FROM generate_series(1, 59) AS i`,
		`CREATE TABLE compositepk (region text NOT NULL, code integer NOT NULL, label text, PRIMARY KEY (region, code))`,
		`INSERT INTO compositepk VALUES ('eu', 1, 'same'), ('eu', 2, 'same'), ('us', 1, 'same'), ('us', 2, 'same')`,
		`CREATE TABLE nopk (name text, qty integer)`,
		`INSERT INTO nopk VALUES ('bolt', 10), ('bolt', 10), ('nut', 10), ('bolt', 20), ('bolt', 10)`,
		`CREATE VIEW invoiceview AS SELECT invoiceid, billingcountry FROM invoice`,
	}
	f := openQueryFixtureWithOptions(t, dalgo2sql.DbOptions{}, "test_unique_keys", IdentifierFoldLower, ddl)
	for _, tc := range uniqueKeysCases() {
		t.Run(tc.name, func(t *testing.T) {
			assertUniqueKeysRead(t, readUniqueKeysRecords(t, f.db, tc.query), tc, false)
		})
	}
}
