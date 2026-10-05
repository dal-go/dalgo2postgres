package dalgo2postgres

import (
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/end2end"
	"github.com/dal-go/dalgo/end2end/models"
	"github.com/dal-go/dalgo2sql"
)

// end2endDDL creates the tables the shared suite of dal-go/dalgo (the end2end
// package) reads and writes, in lower case as this package's DDL stores names. The
// suite's key reads and writes spell the collection and the fields as the suite does
// (DalgoE2E_E2ETest1, StringProp) and dalgo2sql writes those names unquoted, so the
// server folds them to these; its structured queries are compiled in the mode that
// folds names, which writes the very same lower-case names, quoted.
var end2endDDL = []string{
	`CREATE TABLE dalgoe2e_e2etest1 (id varchar(10) PRIMARY KEY, stringprop text, integerprop integer)`,
	`CREATE TABLE dalgoe2e_e2etest2 (id varchar(10) PRIMARY KEY, stringprop text, integerprop integer)`,
	`CREATE TABLE nonexistingkind (id varchar(10) PRIMARY KEY, stringprop text, integerprop integer)`,
	`CREATE TABLE dalgotest_cities (
		id            text PRIMARY KEY,
		name          text,
		state         text,
		country       text,
		population    integer,
		areasqkm      integer,
		iscapital     boolean,
		hasairport    boolean,
		founded       timestamptz,
		lastupdatedat timestamptz
	)`,
}

// TestEndToEnd runs the shared suite of dal-go/dalgo, end2end.TestDalgoDB, against
// PostgreSQL in the mode that folds names (IdentifierFoldLower), with its query half
// on: the key reads and writes, the structured queries over the cities fixture (keys
// only, ordering, filters, projection, GROUP BY with HAVING, on both read paths), the
// access conditions and the field lists of the access layer.
//
// It runs in this repository's Conformance job, which fails when a sub-test of it
// skips and is not in the job's allow-list of skips (see the workflow).
func TestEndToEnd(t *testing.T) {
	testDSN(t) // a skip shows on this test, not only on its subtests
	recordsets := map[string]*dalgo2sql.Recordset{}
	for _, collection := range []string{end2end.E2ETestKind1, end2end.E2ETestKind2, "NonExistingKind", models.CitiesCollection} {
		recordsets[collection] = dalgo2sql.NewRecordset(collection, dalgo2sql.Table, []dal.FieldRef{dal.Field("ID")})
	}
	f := openQueryFixtureWithOptions(t, dalgo2sql.DbOptions{Recordsets: recordsets}, "test_e2e", IdentifierFoldLower, end2endDDL)
	end2end.TestDalgoDB(t, f.db, nil, false)
}
