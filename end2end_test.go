package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/end2end"
	"github.com/dal-go/dalgo/end2end/models"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
	"github.com/dal-go/record/update"
)

// end2endDDL creates the tables the shared suite of dal-go/dalgo (the end2end
// package) reads and writes, in lower case as this package's DDL stores names. The
// suite's key reads and writes spell the collection and the fields as the suite does
// (DalgoE2E_E2ETest1, StringProp) and dalgo2sql writes those names unquoted, so the
// server folds them to these; its structured queries are compiled in the mode that
// folds names, which writes the very same lower-case names, quoted.
//
// The id of a city is text compared as Go compares strings, byte by byte: the suite
// asserts the keys-only read in sort.Strings order, and the server orders text by the
// database's collation (en_US.utf8 in CI), which puts "São Paulo" before "Shanghai".
// So the column says COLLATE "C", which is the rule for any table whose order a caller
// compares with Go's: ORDER BY and MIN / MAX follow the column's collation, not DALgo's.
// The fixture avoids the divergence; it does not hide it:
// TestServerPinsIntegration_KeysOnlyReadFollowsTheDatabasesCollation pins the server's
// order for a key in the database's collation, with the COLLATE "C" control, and the
// divergence from the suite's keys-only contract is reported for dal-go/dalgo and
// dal-go/dalgo2sql.
var end2endDDL = []string{
	`CREATE TABLE dalgoe2e_e2etest1 (id varchar(10) PRIMARY KEY, stringprop text, integerprop integer)`,
	`CREATE TABLE dalgoe2e_e2etest2 (id varchar(10) PRIMARY KEY, stringprop text, integerprop integer)`,
	`CREATE TABLE nonexistingkind (id varchar(10) PRIMARY KEY, stringprop text, integerprop integer)`,
	`CREATE TABLE dalgotest_cities (
		id            text COLLATE "C" PRIMARY KEY,
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

// notRunInFoldLowerMode are the sub-tests of the shared suite that cannot run in the
// mode that folds names, with the reason. The workflow excludes exactly these from the
// go test command (-skip) and lists exactly these as the skips it allows: TestEndToEnd
// reports each one as a skip that names its reason, so the exclusion shows in the output
// and a rename or a removal in the suite is noticed. The reason of each is a fact this
// package asserts, in the test that is named with it.
var notRunInFoldLowerMode = []struct{ name, reason, pinnedBy string }{
	{"point_reads_follow_the_condition", "Exists on a row the policy allows is denied",
		"TestEndToEndIntegration_RowConditionsFailClosedOnStoredNames"},
	{"writes_follow_the_condition", "Update of a row the policy allows is denied",
		"TestEndToEndIntegration_UpdateUnderRowConditionsFailsClosedOnStoredNames"},
}

// TestEndToEnd runs the shared suite of dal-go/dalgo, end2end.TestDalgoDB, against
// PostgreSQL in the mode that folds names (IdentifierFoldLower), with its query half
// on: the key reads and writes, the structured queries over the cities fixture (keys
// only, ordering, filters, projection, GROUP BY with HAVING, on both read paths), the
// access conditions, the field lists and the sources of the access layer
// (query/access_sources).
//
// Two sub-tests of query/access_conditions do not run in this mode (see
// notRunInFoldLowerMode): the Conformance job excludes them by name, and this test
// reports them as skips that give the reason. Every other sub-test runs, and the job
// fails when any skips that is not in its allow-list.
func TestEndToEnd(t *testing.T) {
	testDSN(t) // a skip shows on this test, not only on its subtests
	recordsets := map[string]*dalgo2sql.Recordset{}
	for _, collection := range []string{end2end.E2ETestKind1, end2end.E2ETestKind2, "NonExistingKind", models.CitiesCollection} {
		recordsets[collection] = dalgo2sql.NewRecordset(collection, dalgo2sql.Table, []dal.FieldRef{dal.Field("ID")})
	}
	f := openQueryFixtureWithOptions(t, dalgo2sql.DbOptions{Recordsets: recordsets}, "test_e2e", IdentifierFoldLower, end2endDDL)
	end2end.TestDalgoDB(t, f.db, nil, false)

	t.Run("not_run_in_fold_lower_mode", func(t *testing.T) {
		for _, notRun := range notRunInFoldLowerMode {
			t.Run(notRun.name, func(t *testing.T) {
				t.Skipf("query/access_conditions/%s cannot run in fold-lower mode: %s, because a key read returns the "+
					"names as the table stores them (lower case) and the suite's policy spells them Country and "+
					"IsCapital; the access layer compares names exactly and denies. "+
					"%s pins that", notRun.name, notRun.reason, notRun.pinnedBy)
			})
		}
	})
}

// A key read returns the names as the table stores them, which this package's DDL
// writes in lower case, and the access layer compares the names of a row condition
// exactly. So in the mode that folds names a policy that spells a field in another case
// (the suite's Country) is not satisfied by the stored country: the denial fails
// closed, on a row the policy allows. That is the reason two sub-tests of the shared
// suite cannot run here (notRunInFoldLowerMode); the README says to fold the names an
// access check compares. The same condition spelled as stored is satisfied.
func TestEndToEndIntegration_RowConditionsFailClosedOnStoredNames(t *testing.T) {
	testDSN(t)
	recordsets := map[string]*dalgo2sql.Recordset{
		models.CitiesCollection: dalgo2sql.NewRecordset(models.CitiesCollection, dalgo2sql.Table, []dal.FieldRef{dal.Field("ID")}),
	}
	f := openQueryFixtureWithOptions(t, dalgo2sql.DbOptions{Recordsets: recordsets}, "test_e2e_names", IdentifierFoldLower, end2endDDL)
	ctx := context.Background()
	// A complete row: Get into a struct refuses a NULL for a string field.
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO dalgotest_cities VALUES
		('Tokyo_Tokyo', 'Tokyo', 'Tokyo', 'JP', 37400068, 2187, true, true, '1457-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	key := dalrecord.NewKeyWithID(models.CitiesCollection, "Tokyo_Tokyo")

	t.Run("a key read into a map returns the stored names", func(t *testing.T) {
		rec := dalrecord.NewRecordWithData(key, map[string]any{})
		if err := f.db.Get(ctx, rec); err != nil {
			t.Fatalf("Get: %v", err)
		}
		got := rec.Data().(map[string]any)
		if got["country"] != "JP" {
			t.Errorf("data = %v, want the column under its stored name country", got)
		}
		if _, spelledAsTheSuiteDoes := got["Country"]; spelledAsTheSuiteDoes {
			t.Errorf("data = %v holds Country: the names are not the stored ones", got)
		}
		t.Logf("a key read into a map returned the names %v", reflect.ValueOf(got).MapKeys())
	})
	t.Run("a struct target is reached under any case", func(t *testing.T) {
		city := &models.City{}
		if err := f.db.Get(ctx, dalrecord.NewRecordWithData(key, city)); err != nil || city.Country != "JP" {
			t.Errorf("Get into a struct: %v, Country = %q, want JP", err, city.Country)
		}
	})
	exists := func(t *testing.T, field string) (bool, error) {
		t.Helper()
		condition := dal.WhereField(field, dal.Equal, dal.NewParam("country"))
		secured := access.MustSecureDB(f.db, access.WithDatabasePolicies(access.MustPolicy("cities-by-country",
			access.Scope(models.CitiesCollection, access.AnyID, access.Allow(access.Get|access.Exists, "read-own-country").Where(condition)))))
		return secured.Exists(access.WithVariables(ctx, map[string]any{"country": "JP"}), key)
	}
	t.Run("a condition that spells the field as the suite does denies the row it allows", func(t *testing.T) {
		got, err := exists(t, "Country")
		if !errors.Is(err, access.ErrAccessDenied) || got {
			t.Errorf("Exists = %v, %v; want a denial: the condition on Country is not satisfied by the stored country", got, err)
		}
		t.Logf("Exists under a condition on Country: %v", err)
	})
	t.Run("the condition spelled as stored is satisfied", func(t *testing.T) {
		if got, err := exists(t, "country"); err != nil || !got {
			t.Errorf("Exists = %v, %v; want true", got, err)
		}
	})
}

// A write is held to the same rule. The access layer reads the row's pre-image through the
// session, which is a key read, and evaluates the condition on it, so an Update through a
// policy that spells the field as the suite does (Country) is denied for a row the policy
// allows, and the row is left as it was; the same policy spelled as stored admits it.
// This is the reason the sub-test writes_follow_the_condition cannot run here (the Exists
// case above is the one of point_reads_follow_the_condition).
//
// The denial is a failure to close only because the conditional rule is the only allow for
// the row. A conditional rule whose condition does not hold falls through to an
// unconditional allow when the policy has one, so a policy that narrows with a condition
// and then allows without one is decided by the wider rule: the README says so, and
// PG-03b (access checks) owns that case.
func TestEndToEndIntegration_UpdateUnderRowConditionsFailsClosedOnStoredNames(t *testing.T) {
	testDSN(t)
	recordsets := map[string]*dalgo2sql.Recordset{
		models.CitiesCollection: dalgo2sql.NewRecordset(models.CitiesCollection, dalgo2sql.Table, []dal.FieldRef{dal.Field("ID")}),
	}
	f := openQueryFixtureWithOptions(t, dalgo2sql.DbOptions{Recordsets: recordsets}, "test_e2e_update", IdentifierFoldLower, end2endDDL)
	ctx := context.Background()
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO dalgotest_cities VALUES
		('Tokyo_Tokyo', 'Tokyo', 'Tokyo', 'JP', 37400068, 2187, true, true, '1457-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	key := dalrecord.NewKeyWithID(models.CitiesCollection, "Tokyo_Tokyo")
	hasAirport := func(t *testing.T) bool {
		t.Helper()
		var got bool
		if err := f.admin.QueryRowContext(ctx, `SELECT hasairport FROM dalgotest_cities WHERE id = 'Tokyo_Tokyo'`).Scan(&got); err != nil {
			t.Fatalf("SELECT: %v", err)
		}
		return got
	}
	// edit updates hasairport of the row through a policy whose only allow is conditional
	// on the field given.
	edit := func(field string) error {
		condition := dal.WhereField(field, dal.Equal, dal.NewParam("country"))
		secured := access.MustSecureDB(f.db, access.WithDatabasePolicies(access.MustPolicy("cities-edit-own-country",
			access.Scope(models.CitiesCollection, access.AnyID, access.Allow(access.Update, "edit-own-country").Where(condition)))))
		return secured.RunReadwriteTransaction(access.WithVariables(ctx, map[string]any{"country": "JP"}),
			func(ctx context.Context, tx dal.ReadwriteTransaction) error {
				return tx.Update(ctx, key, []update.Update{update.ByFieldName("hasairport", false)})
			})
	}
	t.Run("an Update under a condition that spells the field as the suite does is denied and writes nothing", func(t *testing.T) {
		err := edit("Country")
		t.Logf("Update under a condition on Country: %v; hasairport is now %v", err, hasAirport(t))
		if !errors.Is(err, access.ErrAccessDenied) {
			t.Errorf("Update = %v, want a denial: the condition on Country is not satisfied by the stored country", err)
		}
		if !hasAirport(t) {
			t.Error("hasairport was written although the Update was denied")
		}
	})
	t.Run("the condition spelled as stored admits the Update", func(t *testing.T) {
		err := edit("country")
		t.Logf("Update under a condition on country: %v; hasairport is now %v", err, hasAirport(t))
		if err != nil {
			t.Errorf("Update = %v, want it admitted", err)
		}
		if hasAirport(t) {
			t.Error("hasairport is still true, want the Update written")
		}
	})
}
