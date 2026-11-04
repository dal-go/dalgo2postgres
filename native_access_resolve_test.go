package dalgo2postgres

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// A typical application database, as pg_catalog shows it (role OIDs are made
// up; 6171 and 6181 are the real OIDs of the built-in roles).
const (
	oidPostgres    = 10
	oidAppOwner    = 100 // NOLOGIN, owns billing.invoices
	oidApp         = 101 // the application's login
	oidAnalysts    = 102 // NOLOGIN group
	oidReaders     = 103 // NOLOGIN group holding SELECT; analysts is a member
	oidAna         = 104 // analyst login, member of analysts
	oidAuditor     = 105 // NOINHERIT login, member of readers
	oidBypassSvc   = 106 // BYPASSRLS login with SELECT
	oidReporting   = 107 // login with SELECT
	oidEtl         = 108 // login with INSERT
	oidLead        = 109 // admin of analysts, with neither INHERIT nor SET
	oidIdle        = 110 // login with no privilege
	oidSupport     = 111 // login with column SELECT
	oidBackup      = 112 // member of pg_read_all_data
	oidDba         = 113 // member of admins_su
	oidAdminsSu    = 114 // NOLOGIN superuser role
	oidManager     = 115 // CREATEROLE login
	oidPgDbOwner   = 6171
	oidReadAll     = 6181
	oidReadFiles   = 4569
	oidWriteFiles  = 4570
	oidExecProgram = 4571
	oidUnknownOID  = 999
)

var nativeAsOf = time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func edge(role, member int64, inherit, set bool) pgMembership {
	return pgMembership{Role: role, Member: member, Inherit: ptr(inherit), Set: ptr(set)}
}

func acl(grantee int64, privileges ...string) []pgACL {
	out := make([]pgACL, len(privileges))
	for i, p := range privileges {
		out[i] = pgACL{Grantee: grantee, Privilege: p}
	}
	return out
}

// appCatalog is billing.invoices on PostgreSQL 17: FORCE row security, schema
// owned by pg_database_owner with USAGE for PUBLIC (the PostgreSQL 15+ default
// of the public schema).
func appCatalog() *pgCatalog {
	return &pgCatalog{
		Version:       170011,
		AsOf:          nativeAsOf,
		DatabaseOwner: ptr(int64(oidPostgres)),
		Roles: []pgRole{
			{OID: oidPostgres, Name: "postgres", Super: true, Inherit: true, CreateRole: true, Login: true, BypassRLS: true},
			{OID: oidAppOwner, Name: "app_owner", Inherit: true},
			{OID: oidApp, Name: "app", Inherit: true, Login: true},
			{OID: oidAnalysts, Name: "analysts", Inherit: true},
			{OID: oidReaders, Name: "readers", Inherit: true},
			{OID: oidAna, Name: "ana", Inherit: true, Login: true},
			{OID: oidAuditor, Name: "auditor", Login: true},
			{OID: oidBypassSvc, Name: "bypass_svc", Inherit: true, Login: true, BypassRLS: true},
			{OID: oidReporting, Name: "reporting", Inherit: true, Login: true},
			{OID: oidEtl, Name: "etl", Inherit: true, Login: true},
			{OID: oidLead, Name: "lead", Inherit: true, Login: true},
			{OID: oidIdle, Name: "idle", Inherit: true, Login: true},
			{OID: oidSupport, Name: "support", Inherit: true, Login: true},
			{OID: oidBackup, Name: "backup", Inherit: true, Login: true},
			{OID: oidDba, Name: "dba", Inherit: true, Login: true},
			{OID: oidAdminsSu, Name: "admins_su", Super: true, Inherit: true},
			{OID: oidManager, Name: "manager", Inherit: true, CreateRole: true, Login: true},
			{OID: oidPgDbOwner, Name: "pg_database_owner", Inherit: true},
			{OID: oidReadAll, Name: "pg_read_all_data", Inherit: true},
			{OID: oidReadFiles, Name: "pg_read_server_files", Inherit: true},
			{OID: oidWriteFiles, Name: "pg_write_server_files", Inherit: true},
			{OID: oidExecProgram, Name: "pg_execute_server_program", Inherit: true},
		},
		Memberships: []pgMembership{
			edge(oidReaders, oidAnalysts, true, true),
			edge(oidAnalysts, oidAna, true, true),
			edge(oidReaders, oidAuditor, false, true),
			{Role: oidAnalysts, Member: oidLead, Admin: true, Inherit: ptr(false), Set: ptr(false)},
			edge(oidReadAll, oidBackup, true, true),
			edge(oidAdminsSu, oidDba, true, true),
		},
		Table: &pgTable{Owner: oidAppOwner, RowSecurity: true, ForceRowSecurity: true, SchemaOwner: oidPgDbOwner},
		TableACL: slices.Concat(
			acl(oidAppOwner, "DELETE", "INSERT", "SELECT", "TRUNCATE", "UPDATE"),
			acl(oidApp, "DELETE", "INSERT", "SELECT", "UPDATE"),
			acl(oidReaders, "SELECT"),
			acl(oidBypassSvc, "SELECT"),
			acl(oidReporting, "SELECT"),
			acl(oidEtl, "INSERT"),
		),
		ColumnACL: []pgColumnACL{
			{Column: "name", Grantee: oidSupport, Privilege: "SELECT"},
			{Column: "email", Grantee: oidSupport, Privilege: "SELECT"},
			{Column: "email", Grantee: oidApp, Privilege: "SELECT"},
		},
		SchemaUsage: []int64{0, oidPgDbOwner},
	}
}

// pre16 rewrites the catalog as PostgreSQL 15 shows it: no per-grant options.
func pre16(cat *pgCatalog) *pgCatalog {
	cat.Version = 150007
	for i := range cat.Memberships {
		cat.Memberships[i].Inherit, cat.Memberships[i].Set = nil, nil
	}
	return cat
}

var invoicesRef = dal.NewQualifiedRootCollectionRef("billing", "invoices", "")

func resolve(cat *pgCatalog) *dbschema.NativeAccess { return resolveNativeAccess(invoicesRef, cat) }

func principalNamed(t *testing.T, a *dbschema.NativeAccess, name string) dbschema.NativePrincipal {
	t.Helper()
	for _, p := range a.Principals {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("principal %q not listed; listed: %v", name, principalNames(a))
	return dbschema.NativePrincipal{}
}

func principalNames(a *dbschema.NativeAccess) []string {
	names := make([]string, len(a.Principals))
	for i, p := range a.Principals {
		names[i] = p.Name
	}
	return names
}

// describe writes a principal's privileges as
// "read[email,name] direct/self verified" lines, one per grant.
func describe(p dbschema.NativePrincipal) []string {
	var out []string
	for _, priv := range p.Privileges {
		op := string(priv.Operation)
		if len(priv.Fields) > 0 {
			op += "[" + strings.Join(priv.Fields, ",") + "]"
		}
		for _, g := range priv.Grants {
			line := fmt.Sprintf("%s %s/%s %s", op, g.Source, g.Route, g.Confidence)
			if len(g.Via) > 0 {
				line += " via " + strings.Join(g.Via, ">")
			}
			out = append(out, line)
		}
	}
	return out
}

func TestNativeAccess_PrincipalsOfATypicalAppDatabase(t *testing.T) {
	t.Parallel()
	got := resolve(appCatalog())
	want := []string{"ana", "app", "app_owner", "auditor", "backup", "bypass_svc", "dba", "etl", "lead",
		"postgres", "reporting", "support"}
	if names := principalNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("principals: got %v, want %v (idle and manager hold nothing on 17)", names, want)
	}
	if got.AsOf != nativeAsOf || got.Collection != invoicesRef {
		t.Fatalf("asOf/collection: got %v %v", got.AsOf, got.Collection)
	}
	if want := (dbschema.NativeRowSecurity{Enabled: true, AppliesToOwner: true}); got.RowSecurity != want {
		t.Fatalf("row security: got %+v", got.RowSecurity)
	}
}

func TestNativeAccess_PrivilegesAndTheirSources(t *testing.T) {
	t.Parallel()
	got := resolve(appCatalog())
	for _, tt := range []struct {
		principal string
		want      []string
		kind      dbschema.NativePrincipalKind
		conf      dbschema.NativeConfidence
	}{
		{"reporting", []string{"read direct/self verified"}, dbschema.NativeLogin, dbschema.NativeVerified},
		{"etl", []string{"insert direct/self verified"}, dbschema.NativeLogin, dbschema.NativeVerified},
		{"app", []string{"read direct/self verified", "insert direct/self verified", "update direct/self verified",
			"delete direct/self verified"}, dbschema.NativeLogin, dbschema.NativeVerified},
		{"support", []string{"read[email,name] direct/self verified"}, dbschema.NativeLogin, dbschema.NativeVerified},
		{"ana", []string{"read membership/inherited inferred via analysts>readers"}, dbschema.NativeLogin, dbschema.NativeInferred},
		{"auditor", []string{"read membership/switch-role inferred via readers"}, dbschema.NativeLogin, dbschema.NativeInferred},
		{"lead", []string{"read membership/self-grant inferred via analysts>readers"}, dbschema.NativeLogin, dbschema.NativeInferred},
		{"backup", []string{"read builtin-role/inherited inferred via pg_read_all_data"}, dbschema.NativeLogin, dbschema.NativeInferred},
		{"app_owner", []string{"read direct/self verified", "read ownership/self verified",
			"insert direct/self verified", "insert ownership/self verified",
			"update direct/self verified", "update ownership/self verified",
			"delete direct/self verified", "delete ownership/self verified",
			"truncate direct/self verified", "truncate ownership/self verified"}, dbschema.NativeGroup, dbschema.NativeVerified},
		{"dba", []string{"read superuser/switch-role inferred via admins_su", "insert superuser/switch-role inferred via admins_su",
			"update superuser/switch-role inferred via admins_su", "delete superuser/switch-role inferred via admins_su",
			"truncate superuser/switch-role inferred via admins_su"}, dbschema.NativeLogin, dbschema.NativeInferred},
	} {
		t.Run(tt.principal, func(t *testing.T) {
			p := principalNamed(t, got, tt.principal)
			if lines := describe(p); !reflect.DeepEqual(lines, tt.want) {
				t.Fatalf("got %q\nwant %q", lines, tt.want)
			}
			if p.Kind != tt.kind || p.Confidence != tt.conf {
				t.Fatalf("kind/confidence: got %s %s", p.Kind, p.Confidence)
			}
		})
	}
}

// Honesty rule: superusers and the table's owner can always reach the table.
func TestNativeAccess_SuperuserAndOwnerAlwaysReach(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.TableACL = nil // the owner revoked everything, even from itself
	cat.ColumnACL = nil
	got := resolve(cat)
	if names := principalNames(got); !reflect.DeepEqual(names, []string{"app_owner", "backup", "dba", "postgres"}) {
		t.Fatalf("got %v", names)
	}
	owner := principalNamed(t, got, "app_owner")
	if !owner.Owner || owner.Superuser || len(owner.Privileges) != 5 || owner.Privileges[0].Grants[0].Source != dbschema.NativeFromOwnership {
		t.Fatalf("owner: %+v", owner)
	}
	su := principalNamed(t, got, "postgres")
	if !su.Superuser || len(su.Privileges) != 5 || su.Confidence != dbschema.NativeVerified ||
		su.Privileges[4].Grants[0].Source != dbschema.NativeFromSuperuser || su.NamespaceAccess != dbschema.NativeNamespaceGranted {
		t.Fatalf("superuser: %+v", su)
	}
}

// Honesty rule: BYPASSRLS and superusers bypass row security, even FORCE; FORCE
// subjects the owner to it; without FORCE the owner bypasses it.
func TestNativeAccess_RowSecurityEffects(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name           string
		enabled, force bool
		want           map[string]dbschema.NativeRowSecurityEffect
	}{
		{"forced", true, true, map[string]dbschema.NativeRowSecurityEffect{
			"bypass_svc": dbschema.NativeRowSecurityBypassed, "postgres": dbschema.NativeRowSecurityBypassed,
			"dba": dbschema.NativeRowSecurityBypassed, "app_owner": dbschema.NativeRowSecurityApplies,
			"app": dbschema.NativeRowSecurityApplies, "ana": dbschema.NativeRowSecurityApplies}},
		{"not forced", true, false, map[string]dbschema.NativeRowSecurityEffect{
			"bypass_svc": dbschema.NativeRowSecurityBypassed, "app_owner": dbschema.NativeRowSecurityBypassed,
			"app": dbschema.NativeRowSecurityApplies}},
		{"disabled", false, false, map[string]dbschema.NativeRowSecurityEffect{
			"bypass_svc": dbschema.NativeRowSecurityOff, "app_owner": dbschema.NativeRowSecurityOff,
			"app": dbschema.NativeRowSecurityOff}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cat := appCatalog()
			cat.Table.RowSecurity, cat.Table.ForceRowSecurity = tt.enabled, tt.force
			got := resolve(cat)
			for name, want := range tt.want {
				if effect := principalNamed(t, got, name).RowSecurity; effect != want {
					t.Errorf("%s: got %s, want %s", name, effect, want)
				}
			}
			if got.RowSecurity.AppliesToOwner != (tt.enabled && tt.force) {
				t.Errorf("AppliesToOwner: got %v", got.RowSecurity.AppliesToOwner)
			}
		})
	}
}

// Honesty rule: BYPASSRLS alone grants no privilege.
func TestNativeAccess_BypassRLSWithoutPrivilegeIsNotListed(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.TableACL = slices.DeleteFunc(cat.TableACL, func(a pgACL) bool { return a.Grantee == oidBypassSvc })
	if slices.Contains(principalNames(resolve(cat)), "bypass_svc") {
		t.Fatal("a BYPASSRLS role with no privilege is listed")
	}
}

// Honesty rule: a PUBLIC grant reaches every login role, and PUBLIC is listed
// as one principal standing for roles created later too.
func TestNativeAccess_PublicGrantReachesEveryLoginRole(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.TableACL = append(cat.TableACL, acl(0, "SELECT")...)
	cat.ColumnACL = append(cat.ColumnACL, pgColumnACL{Column: "email", Grantee: 0, Privilege: "UPDATE"})
	got := resolve(cat)
	for _, role := range cat.Roles {
		if !role.Login {
			continue
		}
		p := principalNamed(t, got, role.Name)
		if !slices.Contains(describe(p), "read everyone/self verified") {
			t.Errorf("%s: no PUBLIC read in %q", role.Name, describe(p))
		}
	}
	last := got.Principals[len(got.Principals)-1]
	want := []string{"read everyone/self verified", "update[email] everyone/self verified"}
	if last.Name != "PUBLIC" || last.Kind != dbschema.NativeEveryone || !reflect.DeepEqual(describe(last), want) ||
		last.RowSecurity != dbschema.NativeRowSecurityApplies || last.NamespaceAccess != dbschema.NativeNamespaceGranted {
		t.Fatalf("PUBLIC: %+v %q", last, describe(last))
	}
	if idle := principalNamed(t, got, "idle"); idle.Confidence != dbschema.NativeVerified {
		t.Fatalf("idle: %+v", idle)
	}
	cat.Table.RowSecurity = false
	cat.SchemaUsage = []int64{oidPgDbOwner}
	last = resolve(cat).Principals[len(got.Principals)-1]
	if last.RowSecurity != dbschema.NativeRowSecurityOff || last.NamespaceAccess != dbschema.NativeNamespaceMissing {
		t.Fatalf("PUBLIC without RLS and USAGE: %+v", last)
	}
}

// A column privilege is listed only where the whole-table one is not held.
func TestNativeAccess_ColumnPrivilegeUnderTablePrivilegeIsFolded(t *testing.T) {
	t.Parallel()
	app := principalNamed(t, resolve(appCatalog()), "app")
	for _, priv := range app.Privileges {
		if len(priv.Fields) > 0 {
			t.Fatalf("app has SELECT on the table; got column privilege %+v", priv)
		}
	}
}

func TestNativeAccess_NamespaceAccess(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.SchemaUsage = []int64{oidPgDbOwner, oidReaders} // PUBLIC lost USAGE
	got := resolve(cat)
	for name, want := range map[string]dbschema.NativeNamespaceAccess{
		"reporting": dbschema.NativeNamespaceMissing, // SELECT, but cannot reach the schema
		"ana":       dbschema.NativeNamespaceGranted, // through readers
		"backup":    dbschema.NativeNamespaceGranted, // pg_read_all_data includes USAGE
		"postgres":  dbschema.NativeNamespaceGranted, // superuser, and owns the database
		"dba":       dbschema.NativeNamespaceGranted, // superuser through SET ROLE
	} {
		if got := principalNamed(t, got, name).NamespaceAccess; got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
}

// The database owner is an implicit member of pg_database_owner (14+).
func TestNativeAccess_DatabaseOwnerIsImplicitMemberOfPgDatabaseOwner(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.DatabaseOwner = ptr(int64(oidEtl))
	cat.TableACL = append(cat.TableACL, acl(oidPgDbOwner, "TRUNCATE")...)
	etl := principalNamed(t, resolve(cat), "etl")
	if want := []string{"insert direct/self verified", "truncate membership/inherited inferred via pg_database_owner"}; !reflect.DeepEqual(describe(etl), want) {
		t.Fatalf("got %q", describe(etl))
	}
	cat.Version = 130015 // no pg_database_owner semantics before 14
	if got := describe(principalNamed(t, resolve(cat), "etl")); len(got) != 1 {
		t.Fatalf("13: got %q", got)
	}
}

// Before PostgreSQL 16: rolinherit decides inheritance, SET ROLE is always
// possible, and CREATEROLE may grant itself any non-superuser role.
func TestNativeAccess_Before16(t *testing.T) {
	t.Parallel()
	got := resolve(pre16(appCatalog()))
	for _, tt := range []struct {
		principal string
		want      []string
	}{
		{"auditor", []string{"read membership/switch-role inferred via readers"}},
		{"lead", []string{"read membership/inherited inferred via analysts>readers"}},
	} {
		if lines := describe(principalNamed(t, got, tt.principal)); !reflect.DeepEqual(lines, tt.want) {
			t.Errorf("%s: got %q\nwant %q", tt.principal, lines, tt.want)
		}
	}
	// CREATEROLE could grant itself any non-superuser role before 16, including
	// the owner, and a login (dba) that can SET ROLE to a superuser role.
	manager := describe(principalNamed(t, got, "manager"))
	for _, want := range []string{"truncate ownership/self-grant inferred via app_owner",
		"read superuser/self-grant inferred via dba>admins_su", "read membership/self-grant inferred via readers"} {
		if !slices.Contains(manager, want) {
			t.Errorf("manager: %q missing from %q", want, manager)
		}
	}
}

// On 16+, a grant row without options (should not happen) is read as INHERIT
// and SET, so the answer over-reports rather than hides a holder.
func TestNativeAccess_MissingGrantOptionsOn16AreReadAsGranted(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Memberships = []pgMembership{{Role: oidReaders, Member: oidAuditor}}
	if lines := describe(principalNamed(t, resolve(cat), "auditor")); !reflect.DeepEqual(lines, []string{"read membership/inherited inferred via readers"}) {
		t.Fatalf("got %q", lines)
	}
}

// A role that can SET ROLE to another, NOINHERIT, role still reaches what that
// role inherits.
func TestNativeAccess_SwitchRoleThenInherit(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Memberships = append(cat.Memberships, edge(oidAnalysts, oidAuditor, false, true))
	// auditor -SET-> analysts -INHERIT-> readers; and directly -SET-> readers.
	lines := describe(principalNamed(t, resolve(cat), "auditor"))
	if !reflect.DeepEqual(lines, []string{"read membership/switch-role inferred via readers"}) {
		t.Fatalf("got %q", lines)
	}
}

// Honesty rule: what the catalog cannot show is always stated.
func TestNativeAccess_BlindSpotsAlwaysStated(t *testing.T) {
	t.Parallel()
	got := resolve(appCatalog())
	if got.Coverage.Level != dbschema.NativeCoveragePartial || len(got.Coverage.Gaps) != 0 {
		t.Fatalf("coverage: %+v", got.Coverage)
	}
	for _, spot := range []string{dbschema.BlindSpotConnectionRules, dbschema.BlindSpotExternalIdentity,
		dbschema.BlindSpotReplicasBackups, dbschema.BlindSpotSharedCredential, dbschema.BlindSpotDefinerCode,
		dbschema.BlindSpotRowPredicates} {
		if !slices.Contains(got.Coverage.BlindSpots, spot) {
			t.Errorf("blind spot %q not stated", spot)
		}
	}
	if !slices.Contains(got.Coverage.Covered, "role-membership") {
		t.Errorf("covered: %v", got.Coverage.Covered)
	}
}

// Honesty rule: input that does not resolve makes the answer unknown, never
// a shorter list that reads as "nobody else".
func TestNativeAccess_UnresolvedInputIsUnknownNeverNobody(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*pgCatalog)
		code   string
	}{
		{"membership names an unknown member", func(c *pgCatalog) {
			c.Memberships = append(c.Memberships, edge(oidReaders, oidUnknownOID, true, true))
		}, gapUnresolvedMembership},
		{"membership names an unknown role", func(c *pgCatalog) {
			c.Memberships = append(c.Memberships, edge(oidUnknownOID, oidAna, true, true))
		}, gapUnresolvedMembership},
		{"table grantee unknown", func(c *pgCatalog) { c.TableACL = append(c.TableACL, acl(oidUnknownOID, "SELECT")...) }, gapUnresolvedGrantee},
		{"column grantee unknown", func(c *pgCatalog) {
			c.ColumnACL = append(c.ColumnACL, pgColumnACL{Column: "email", Grantee: oidUnknownOID, Privilege: "SELECT"})
		}, gapUnresolvedGrantee},
		{"owner unknown", func(c *pgCatalog) { c.Table.Owner = oidUnknownOID }, gapUnresolvedOwner},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cat := appCatalog()
			tt.mutate(cat)
			got := resolve(cat)
			if got.Coverage.Level != dbschema.NativeCoverageUnknown || len(got.Coverage.Gaps) != 1 || got.Coverage.Gaps[0].Code != tt.code {
				t.Fatalf("coverage: %+v", got.Coverage)
			}
		})
	}
}

// Honesty rule: an answer with no principal at all is never "nobody".
func TestNativeAccess_EmptyAnswerIsUnknown(t *testing.T) {
	t.Parallel()
	cat := &pgCatalog{Version: 170000, Table: &pgTable{Owner: oidUnknownOID}}
	got := resolve(cat)
	if len(got.Principals) != 0 || got.Coverage.Level != dbschema.NativeCoverageUnknown {
		t.Fatalf("got %+v", got)
	}
	cat.Table.Owner = 0 // not a role, but also not a gap: still never "nobody"
	if got = resolve(cat); got.Coverage.Level != dbschema.NativeCoverageUnknown {
		t.Fatalf("got %+v", got.Coverage)
	}
}

// Two equally short routes to the same role pick one deterministically.
func TestNativeAccess_DeterministicRouteChoice(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Roles = append(cat.Roles, pgRole{OID: 200, Name: "zeta", Inherit: true}, pgRole{OID: 201, Name: "alpha", Inherit: true})
	cat.Memberships = append(cat.Memberships, edge(200, oidEtl, true, true), edge(201, oidEtl, true, true),
		edge(oidReaders, 200, true, true), edge(oidReaders, 201, true, true))
	for range 20 {
		lines := describe(principalNamed(t, resolve(cat), "etl"))
		if want := []string{"read membership/inherited inferred via alpha>readers", "insert direct/self verified"}; !reflect.DeepEqual(lines, want) {
			t.Fatalf("got %q", lines)
		}
	}
}

func TestNativeAccess_ConfidenceHelpers(t *testing.T) {
	t.Parallel()
	if got := weakest(nil); got != dbschema.NativeVerified {
		t.Fatalf("weakest(nil) = %s", got)
	}
	if got := newPrivilegeSet().newPrivilege(dbschema.NativeRead, nil, nil).Confidence; got != dbschema.NativeUnknown {
		t.Fatalf("no grants = %s", got)
	}
}

// A role that is a member through a chain gets a field privilege through it too.
func TestNativeAccess_FieldPrivilegeThroughMembership(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.ColumnACL = append(cat.ColumnACL, pgColumnACL{Column: "total", Grantee: oidReaders, Privilege: "UPDATE"})
	lines := describe(principalNamed(t, resolve(cat), "ana"))
	want := []string{"read membership/inherited inferred via analysts>readers",
		"update[total] membership/inherited inferred via analysts>readers"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("got %q", lines)
	}
}

// A database without pg_database_owner in pg_roles (a 14+ catalog read
// partially) adds no implicit membership.
func TestNativeAccess_NoPgDatabaseOwnerRole(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Roles = slices.DeleteFunc(cat.Roles, func(r pgRole) bool { return r.OID == oidPgDbOwner })
	cat.Table.SchemaOwner = oidAppOwner
	got := resolve(cat)
	if principalNamed(t, got, "postgres").NamespaceAccess != dbschema.NativeNamespaceGranted {
		t.Fatal("superuser lost namespace access")
	}
}

// pg_write_all_data (14+) grants INSERT, UPDATE and DELETE on every table, and
// USAGE on every schema.
func TestNativeAccess_WriteAllDataBuiltinRole(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Roles = append(cat.Roles, pgRole{OID: 6182, Name: "pg_write_all_data", Inherit: true})
	cat.Memberships = append(cat.Memberships, edge(6182, oidIdle, true, true))
	cat.SchemaUsage = nil
	idle := principalNamed(t, resolve(cat), "idle")
	want := []string{"insert builtin-role/inherited inferred via pg_write_all_data",
		"update builtin-role/inherited inferred via pg_write_all_data",
		"delete builtin-role/inherited inferred via pg_write_all_data"}
	if !reflect.DeepEqual(describe(idle), want) || idle.NamespaceAccess != dbschema.NativeNamespaceGranted {
		t.Fatalf("got %q %s", describe(idle), idle.NamespaceAccess)
	}
}

// H1: a current column privilege is never hidden behind a whole-table
// privilege that is only potential (here: a self-grant through ADMIN OPTION).
func TestNativeAccess_ColumnPrivilegeIsKeptBesideAPotentialTablePrivilege(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Memberships = append(cat.Memberships,
		pgMembership{Role: oidReaders, Member: oidSupport, Admin: true, Inherit: ptr(false), Set: ptr(false)})
	support := principalNamed(t, resolve(cat), "support")
	want := []string{"read membership/self-grant inferred via readers", "read[email,name] direct/self verified"}
	if lines := describe(support); !reflect.DeepEqual(lines, want) {
		t.Fatalf("got %q\nwant %q", lines, want)
	}
	if support.Confidence != dbschema.NativeInferred {
		t.Fatalf("confidence %s", support.Confidence)
	}
}

// L2: one operation on the same fields is one privilege listing every grant.
func TestNativeAccess_SameFieldsFromTwoGrantsAreOnePrivilege(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Roles = append(cat.Roles, pgRole{OID: 300, Name: "mailers", Inherit: true})
	cat.Memberships = append(cat.Memberships, edge(300, oidEtl, true, true))
	cat.ColumnACL = append(cat.ColumnACL,
		pgColumnACL{Column: "email", Grantee: oidEtl, Privilege: "SELECT"},
		pgColumnACL{Column: "email", Grantee: 300, Privilege: "SELECT"})
	etl := principalNamed(t, resolve(cat), "etl")
	want := []string{"read[email] direct/self verified", "read[email] membership/inherited inferred via mailers",
		"insert direct/self verified"}
	if lines := describe(etl); !reflect.DeepEqual(lines, want) {
		t.Fatalf("got %q\nwant %q", lines, want)
	}
	if len(etl.Privileges) != 2 || len(etl.Privileges[0].Grants) != 2 || etl.Privileges[0].Confidence != dbschema.NativeVerified {
		t.Fatalf("privileges: %+v", etl.Privileges)
	}
}

// M1: privileges and ownership of the table's ancestors and descendants in
// pg_inherits reach its rows; they are inferred, with the relative last in Via.
func TestNativeAccess_PartitionsAndInheritance(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Relatives = []pgRelative{
		{OID: 501, Schema: "billing", Name: "invoices_all", Relation: "ancestor", Owner: oidAppOwner},
		{OID: 502, Schema: "billing", Name: "invoices_2026", Relation: "descendant", Owner: oidIdle},
	}
	cat.TableACL = append(cat.TableACL,
		pgACL{Rel: 501, Grantee: oidEtl, Privilege: "SELECT"},
		pgACL{Rel: 502, Grantee: 0, Privilege: "SELECT"})
	cat.ColumnACL = append(cat.ColumnACL, pgColumnACL{Rel: 501, Column: "total", Grantee: oidAna, Privilege: "UPDATE"})
	got := resolve(cat)
	if got.Coverage.Level != dbschema.NativeCoveragePartial || slices.Contains(got.Coverage.BlindSpots, "inheritance-and-partitions") ||
		!slices.Contains(got.Coverage.Covered, "inheritance-and-partitions") {
		t.Fatalf("coverage: %+v", got.Coverage)
	}
	for _, tt := range []struct {
		principal string
		want      string
	}{
		{"etl", "read direct/self inferred via ancestor:billing.invoices_all"},
		{"ana", "update[total] direct/self inferred via ancestor:billing.invoices_all"},
		{"idle", "truncate ownership/self inferred via descendant:billing.invoices_2026"},
		{"idle", "read everyone/self inferred via descendant:billing.invoices_2026"},
		{"app_owner", "read ownership/self inferred via ancestor:billing.invoices_all"},
		{"PUBLIC", "read everyone/self inferred via descendant:billing.invoices_2026"},
	} {
		if lines := describe(principalNamed(t, got, tt.principal)); !slices.Contains(lines, tt.want) {
			t.Errorf("%s: %q missing from %q", tt.principal, tt.want, lines)
		}
	}
	if c := principalNamed(t, got, "PUBLIC").Confidence; c != dbschema.NativeInferred {
		t.Errorf("PUBLIC confidence %s", c)
	}
}

// M2: members of the server-file roles reach the data outside SQL: reported
// as builtin-role, inferred, bypassing row security; before 16 CREATEROLE can
// grant itself those roles.
func TestNativeAccess_ServerFileRoles(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.SchemaUsage = []int64{oidPgDbOwner}
	cat.Memberships = append(cat.Memberships, edge(oidReadFiles, oidIdle, true, true), edge(oidExecProgram, oidEtl, true, true))
	got := resolve(cat)
	idle := principalNamed(t, got, "idle")
	if want := []string{"read builtin-role/inherited inferred via pg_read_server_files"}; !reflect.DeepEqual(describe(idle), want) ||
		idle.RowSecurity != dbschema.NativeRowSecurityBypassed || idle.NamespaceAccess != dbschema.NativeNamespaceGranted {
		t.Fatalf("idle: %q %s %s", describe(idle), idle.RowSecurity, idle.NamespaceAccess)
	}
	etl := describe(principalNamed(t, got, "etl"))
	for _, op := range []string{"read", "insert", "update", "delete", "truncate"} {
		if !slices.Contains(etl, op+" builtin-role/inherited inferred via pg_execute_server_program") {
			t.Errorf("etl: no %s through pg_execute_server_program in %q", op, etl)
		}
	}
	cat = pre16(appCatalog())
	manager := describe(principalNamed(t, resolve(cat), "manager"))
	for _, want := range []string{"read builtin-role/self-grant inferred via pg_read_server_files",
		"truncate builtin-role/self-grant inferred via pg_write_server_files"} {
		if !slices.Contains(manager, want) {
			t.Errorf("manager: %q missing from %q", want, manager)
		}
	}
}

// L1: schema USAGE reachable only through a self-grant or SET ROLE does not
// make a current privilege's namespace access "granted".
func TestNativeAccess_NamespaceOnlyThroughAPotentialRouteIsUnknown(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.SchemaUsage = []int64{oidPgDbOwner, oidAnalysts}
	cat.TableACL = append(cat.TableACL, acl(oidIdle, "SELECT")...)
	cat.Memberships = append(cat.Memberships,
		pgMembership{Role: oidAnalysts, Member: oidIdle, Admin: true, Inherit: ptr(false), Set: ptr(false)},
		edge(oidAnalysts, oidAuditor, false, true))
	got := resolve(cat)
	for name, want := range map[string]dbschema.NativeNamespaceAccess{
		"idle":    dbschema.NativeNamespaceUnknown, // direct SELECT; USAGE only by self-grant
		"auditor": dbschema.NativeNamespaceGranted, // SELECT and USAGE both after SET ROLE
		"lead":    dbschema.NativeNamespaceGranted, // both by self-grant
	} {
		if got := principalNamed(t, got, name).NamespaceAccess; got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
}

func TestNativeAccess_MoreUnresolvedInput(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*pgCatalog)
		code   string
	}{
		{"database owner unknown", func(c *pgCatalog) { c.DatabaseOwner = ptr(int64(oidUnknownOID)) }, gapUnresolvedDatabaseOwner},
		{"privilege on an unknown relation", func(c *pgCatalog) {
			c.TableACL = append(c.TableACL, pgACL{Rel: 777, Grantee: oidEtl, Privilege: "SELECT"})
		}, gapUnresolvedRelative},
		{"relative owner unknown", func(c *pgCatalog) {
			c.Relatives = []pgRelative{{OID: 501, Schema: "billing", Name: "p", Relation: "ancestor", Owner: oidUnknownOID}}
		}, gapUnresolvedOwner},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cat := appCatalog()
			tt.mutate(cat)
			got := resolve(cat)
			if got.Coverage.Level != dbschema.NativeCoverageUnknown || len(got.Coverage.Gaps) != 1 || got.Coverage.Gaps[0].Code != tt.code {
				t.Fatalf("coverage: %+v", got.Coverage)
			}
		})
	}
}

// partitioned puts billing.invoices under a partitioned parent in the given
// schema; idle reads the parent.
func partitioned(parentSchema string, rowSecurity, force bool, parentUsage []int64) *pgCatalog {
	cat := appCatalog()
	cat.Relatives = []pgRelative{{OID: 501, Schema: parentSchema, Name: "all", Relation: "ancestor", Owner: oidAppOwner,
		RowSecurity: rowSecurity, ForceRowSecurity: force, SchemaOwner: oidAppOwner, SchemaUsage: parentUsage}}
	cat.TableACL = append(cat.TableACL, pgACL{Rel: 501, Grantee: oidIdle, Privilege: "SELECT"})
	return cat
}

// Rows read through a relative follow that relative's row security.
func TestNativeAccess_RowSecurityFollowsTheRelative(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name               string
		rowSecurity, force bool
		ownerOfParent      bool
		want               dbschema.NativeRowSecurityEffect
	}{
		{"parent without row security", false, false, false, dbschema.NativeRowSecurityBypassed},
		{"parent with row security", true, false, false, dbschema.NativeRowSecurityApplies},
		{"owner of the parent", true, false, true, dbschema.NativeRowSecurityBypassed},
		{"owner of a forced parent", true, true, true, dbschema.NativeRowSecurityApplies},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cat := partitioned("billing", tt.rowSecurity, tt.force, nil)
			if tt.ownerOfParent {
				cat.Relatives[0].Owner = oidIdle
			}
			if got := principalNamed(t, resolve(cat), "idle").RowSecurity; got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
	cat := partitioned("billing", false, false, nil)
	cat.TableACL = append(cat.TableACL, pgACL{Rel: 501, Grantee: 0, Privilege: "SELECT"})
	if got := principalNamed(t, resolve(cat), "PUBLIC").RowSecurity; got != dbschema.NativeRowSecurityBypassed {
		t.Fatalf("PUBLIC through the parent: %s", got)
	}
}

// Namespace access is checked against the schema of the relation a grant comes
// through.
func TestNativeAccess_NamespaceFollowsTheRelative(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		tableUsage  []int64
		parentUsage []int64
		want        dbschema.NativeNamespaceAccess
	}{
		{"USAGE on the parent's schema only", []int64{oidPgDbOwner}, []int64{0}, dbschema.NativeNamespaceGranted},
		{"USAGE on the table's schema only", []int64{0, oidPgDbOwner}, nil, dbschema.NativeNamespaceMissing},
		{"neither", []int64{oidPgDbOwner}, nil, dbschema.NativeNamespaceMissing},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cat := partitioned("archive", true, false, tt.parentUsage)
			cat.SchemaUsage = tt.tableUsage
			if got := principalNamed(t, resolve(cat), "idle").NamespaceAccess; got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// A column grant usable after SET ROLE is not folded under a whole-table
// grant that needs a self-grant.
func TestNativeAccess_SwitchRoleColumnGrantIsKeptBesideASelfGrant(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Roles = append(cat.Roles, pgRole{OID: 400, Name: "colgrp", Inherit: true})
	cat.Memberships = append(cat.Memberships, edge(400, oidIdle, false, true),
		pgMembership{Role: oidReaders, Member: oidIdle, Admin: true, Inherit: ptr(false), Set: ptr(false)})
	cat.ColumnACL = append(cat.ColumnACL, pgColumnACL{Column: "email", Grantee: 400, Privilege: "SELECT"})
	want := []string{"read membership/self-grant inferred via readers", "read[email] membership/switch-role inferred via colgrp"}
	if got := describe(principalNamed(t, resolve(cat), "idle")); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// A descendant's whole-table grant (a share of the rows) does not absorb an
// ancestor's column grant (that column of every row).
func TestNativeAccess_DescendantGrantDoesNotAbsorbAnAncestorColumnGrant(t *testing.T) {
	t.Parallel()
	cat := appCatalog()
	cat.Relatives = []pgRelative{
		{OID: 501, Schema: "billing", Name: "all", Relation: "ancestor", Owner: oidAppOwner},
		{OID: 502, Schema: "billing", Name: "part", Relation: "descendant", Owner: oidAppOwner},
	}
	cat.ColumnACL = append(cat.ColumnACL, pgColumnACL{Rel: 501, Column: "email", Grantee: oidIdle, Privilege: "SELECT"})
	cat.TableACL = append(cat.TableACL, pgACL{Rel: 502, Grantee: oidIdle, Privilege: "SELECT"})
	want := []string{"read direct/self inferred via descendant:billing.part", "read[email] direct/self inferred via ancestor:billing.all"}
	if got := describe(principalNamed(t, resolve(cat), "idle")); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
