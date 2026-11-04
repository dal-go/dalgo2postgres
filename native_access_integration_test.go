package dalgo2postgres

import (
	"context"
	"fmt"
	"hash/crc32"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// nativeFixture creates, on the live server, a typical application database:
// an owner role, an application login, a readers group with an analysts group
// nested in it, a NOINHERIT auditor, a BYPASSRLS service, a superuser, an admin
// of analysts with neither INHERIT nor SET (16+), and a catalog login with no
// privilege at all. Roles are cluster-wide, so their names carry a prefix of
// this test and are dropped on cleanup.
type nativeFixture struct {
	db     *Database
	schema string
	prefix string
	pg16   bool
}

func (f nativeFixture) role(name string) string { return f.prefix + name }

func newNativeFixture(t *testing.T) nativeFixture {
	t.Helper()
	schema := uniqueTable(t, "na")
	db := openSchemaTestDB(t, schema, IdentifierExact)
	f := nativeFixture{db: db, schema: schema, prefix: fmt.Sprintf("na%08x_", crc32.ChecksumIEEE([]byte(t.Name())))}
	var version int
	if err := db.sqlDB.QueryRowContext(context.Background(), `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	f.pg16 = version >= pg16Version
	roles := []string{"owner", "app", "readers", "analysts", "ana", "auditor", "bypass", "su", "lead", "catalog"}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, r := range roles {
			_, _ = db.sqlDB.ExecContext(ctx, `DROP OWNED BY `+exactIdent(f.role(r))+` CASCADE`)
		}
		for _, r := range roles {
			_, _ = db.sqlDB.ExecContext(ctx, `DROP ROLE IF EXISTS `+exactIdent(f.role(r)))
		}
	})
	q := func(r string) string { return exactIdent(f.role(r)) }
	s := exactIdent(schema)
	statements := []string{
		`CREATE ROLE ` + q("owner") + ` NOLOGIN`,
		`CREATE ROLE ` + q("app") + ` LOGIN`,
		`CREATE ROLE ` + q("readers") + ` NOLOGIN`,
		`CREATE ROLE ` + q("analysts") + ` NOLOGIN`,
		`CREATE ROLE ` + q("ana") + ` LOGIN`,
		`CREATE ROLE ` + q("auditor") + ` LOGIN NOINHERIT`,
		`CREATE ROLE ` + q("bypass") + ` LOGIN BYPASSRLS`,
		`CREATE ROLE ` + q("su") + ` LOGIN SUPERUSER`,
		`CREATE ROLE ` + q("lead") + ` LOGIN`,
		`CREATE ROLE ` + q("catalog") + ` LOGIN PASSWORD 'catalog-only'`,
		`GRANT ` + q("readers") + ` TO ` + q("analysts"),
		`GRANT ` + q("analysts") + ` TO ` + q("ana"),
		`GRANT ` + q("readers") + ` TO ` + q("auditor"),
		`GRANT USAGE ON SCHEMA ` + s + ` TO ` + q("readers") + `, ` + q("app") + `, ` + q("bypass") + `, ` + q("owner"),
		`CREATE TABLE ` + s + `.invoices (id integer PRIMARY KEY, total numeric, email text)`,
		`ALTER TABLE ` + s + `.invoices OWNER TO ` + q("owner"),
		`ALTER TABLE ` + s + `.invoices ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE ` + s + `.invoices FORCE ROW LEVEL SECURITY`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + s + `.invoices TO ` + q("app"),
		`GRANT SELECT ON ` + s + `.invoices TO ` + q("readers") + `, ` + q("bypass"),
		`GRANT UPDATE (total) ON ` + s + `.invoices TO ` + q("ana"),
		`CREATE TABLE ` + s + `.notes (id integer PRIMARY KEY)`,
		`GRANT SELECT ON ` + s + `.notes TO PUBLIC`,
	}
	if f.pg16 {
		statements = append(statements, `GRANT `+q("analysts")+` TO `+q("lead")+` WITH ADMIN TRUE, INHERIT FALSE, SET FALSE`)
	}
	execAll(t, db, statements...)
	return f
}

// mine keeps the fixture's principals (and PUBLIC), dropping the roles other
// tests or the server itself own.
func (f nativeFixture) mine(a *dbschema.NativeAccess) map[string]dbschema.NativePrincipal {
	out := map[string]dbschema.NativePrincipal{}
	for _, p := range a.Principals {
		if name, ok := strings.CutPrefix(p.Name, f.prefix); ok {
			out[name] = p
		} else if p.Kind == dbschema.NativeEveryone {
			out[p.Name] = p
		}
	}
	return out
}

func (f nativeFixture) read(t *testing.T, db *Database, table string) *dbschema.NativeAccess {
	t.Helper()
	ref := dal.NewQualifiedRootCollectionRef(f.schema, table, "")
	got, err := db.ReadNativeAccess(context.Background(), &ref)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// catalogDB opens the fixture's catalog login: a role with no privilege but
// CONNECT, which is all ReadNativeAccess needs.
func (f nativeFixture) catalogDB(t *testing.T) *Database {
	t.Helper()
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(f.role("catalog"), "catalog-only")
	db, err := NewDatabase(u.String(), WithIdentifierMode(IdentifierExact))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSchemaReaderIntegration_NativeAccessOfATypicalAppDatabase(t *testing.T) {
	f := newNativeFixture(t)
	// Read with the least-privilege catalog login, not the test's superuser.
	got := f.read(t, f.catalogDB(t), "invoices")
	if got.Coverage.Level != dbschema.NativeCoveragePartial || !got.RowSecurity.Enabled || !got.RowSecurity.AppliesToOwner || got.AsOf.IsZero() {
		t.Fatalf("answer: %+v %+v", got.Coverage, got.RowSecurity)
	}
	mine := f.mine(got)
	want := []string{"ana", "app", "auditor", "bypass", "owner", "su"}
	if f.pg16 {
		want = []string{"ana", "app", "auditor", "bypass", "lead", "owner", "su"}
	}
	var names []string
	for name := range mine {
		names = append(names, name)
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("principals: got %v, want %v", names, want)
	}
	via := func(roles ...string) string {
		for i, r := range roles {
			roles[i] = f.role(r)
		}
		return " via " + strings.Join(roles, ">")
	}
	for _, tt := range []struct {
		name string
		want []string
	}{
		{"app", []string{"read direct/self verified", "insert direct/self verified", "update direct/self verified", "delete direct/self verified"}},
		{"ana", []string{"read membership/inherited inferred" + via("analysts", "readers"), "update[total] direct/self verified"}},
		{"auditor", []string{"read membership/switch-role inferred" + via("readers")}},
		{"bypass", []string{"read direct/self verified"}},
	} {
		if lines := describe(mine[tt.name]); !reflect.DeepEqual(lines, tt.want) {
			t.Errorf("%s: got %q\nwant %q", tt.name, lines, tt.want)
		}
	}
	if f.pg16 {
		if lines := describe(mine["lead"]); !reflect.DeepEqual(lines, []string{"read membership/self-grant inferred" + via("analysts", "readers")}) {
			t.Errorf("lead: got %q", lines)
		}
	}
	owner := mine["owner"]
	if !owner.Owner || owner.Kind != dbschema.NativeGroup || len(owner.Privileges) != 5 || owner.RowSecurity != dbschema.NativeRowSecurityApplies {
		t.Errorf("owner: %+v", owner)
	}
	su := mine["su"]
	if !su.Superuser || su.RowSecurity != dbschema.NativeRowSecurityBypassed || su.NamespaceAccess != dbschema.NativeNamespaceGranted {
		t.Errorf("superuser: %+v", su)
	}
	if mine["bypass"].RowSecurity != dbschema.NativeRowSecurityBypassed || mine["app"].RowSecurity != dbschema.NativeRowSecurityApplies {
		t.Errorf("row security: bypass %s, app %s", mine["bypass"].RowSecurity, mine["app"].RowSecurity)
	}
	if mine["ana"].NamespaceAccess != dbschema.NativeNamespaceGranted {
		t.Errorf("ana reaches the schema through readers: %s", mine["ana"].NamespaceAccess)
	}
	if _, listed := mine["PUBLIC"]; listed {
		t.Error("PUBLIC holds nothing on invoices")
	}
}

func TestSchemaReaderIntegration_NativeAccessPublicGrant(t *testing.T) {
	f := newNativeFixture(t)
	got := f.read(t, f.db, "notes")
	mine := f.mine(got)
	public := mine["PUBLIC"]
	if public.Kind != dbschema.NativeEveryone || !reflect.DeepEqual(describe(public), []string{"read everyone/self verified"}) ||
		public.NamespaceAccess != dbschema.NativeNamespaceMissing || public.RowSecurity != dbschema.NativeRowSecurityOff {
		t.Fatalf("PUBLIC: %+v %q", public, describe(public))
	}
	// Every login role holds PUBLIC's read; the catalog login, which holds no
	// USAGE on the schema, cannot use it.
	catalog := mine["catalog"]
	if !reflect.DeepEqual(describe(catalog), []string{"read everyone/self verified"}) || catalog.NamespaceAccess != dbschema.NativeNamespaceMissing {
		t.Fatalf("catalog: %+v %q", catalog, describe(catalog))
	}
}

// Honesty rule: a table that is not there is an error, never an empty answer.
func TestSchemaReaderIntegration_NativeAccessOfAMissingTable(t *testing.T) {
	f := newNativeFixture(t)
	ref := dal.NewQualifiedRootCollectionRef(f.schema, "nope", "")
	got, err := f.db.ReadNativeAccess(context.Background(), &ref)
	if got != nil || err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v, %v", got, err)
	}
}
