package dalgo2postgres

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

const (
	pg16Version         = 160000
	pg14Version         = 140000
	publicGrantee       = int64(0)
	publicPrincipalName = "PUBLIC"
	roleDatabaseOwner   = "pg_database_owner"
	roleReadAllData     = "pg_read_all_data"
	roleWriteAllData    = "pg_write_all_data"
)

// Gap codes ReadNativeAccess reports.
const (
	gapUnresolvedMembership = "unresolved-membership"
	gapUnresolvedGrantee    = "unresolved-grantee"
	gapUnresolvedOwner      = "unresolved-owner"
)

// nativeCovered is what a PostgreSQL native answer reads.
var nativeCovered = []string{
	"roles", "role-membership", "ownership", "superusers", "builtin-roles",
	"collection-privileges", "field-privileges", "everyone-privileges",
	"namespace-access", "row-security-flags",
}

// nativeBlindSpots is what the PostgreSQL catalog cannot show.
var nativeBlindSpots = []string{
	dbschema.BlindSpotConnectionRules,  // pg_hba.conf: who may connect from where, and how
	dbschema.BlindSpotExternalIdentity, // cloud IAM logins mapped onto roles
	dbschema.BlindSpotReplicasBackups,
	dbschema.BlindSpotSharedCredential,
	dbschema.BlindSpotDefinerCode,   // SECURITY DEFINER functions, views owned by other roles
	dbschema.BlindSpotRowPredicates, // row-security policy expressions are not read
	"managed-service-admin-roles",   // e.g. rds_superuser, cloudsqlsuperuser: not rolsuper
	"inheritance-and-partitions",    // a parent's privileges reach child rows; children are separate tables
}

var tableOperations = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE"}

var operationOf = map[string]dbschema.NativeOperation{
	"SELECT":   dbschema.NativeRead,
	"INSERT":   dbschema.NativeInsert,
	"UPDATE":   dbschema.NativeUpdate,
	"DELETE":   dbschema.NativeDelete,
	"TRUNCATE": dbschema.NativeTruncate,
}

var operationRank = map[dbschema.NativeOperation]int{
	dbschema.NativeRead: 0, dbschema.NativeInsert: 1, dbschema.NativeUpdate: 2,
	dbschema.NativeDelete: 3, dbschema.NativeTruncate: 4,
}

var routeRank = map[dbschema.NativeRoute]int{
	dbschema.NativeRouteSelf: 0, dbschema.NativeRouteInherited: 1,
	dbschema.NativeRouteSwitchRole: 2, dbschema.NativeRouteSelfGrant: 3,
}

var confidenceRank = map[dbschema.NativeConfidence]int{
	dbschema.NativeVerified: 0, dbschema.NativeInferred: 1, dbschema.NativeUnknown: 2,
}

// reachMode is how a principal holds a role while membership is followed.
type reachMode int

const (
	// modeAuto: the role's privileges are the principal's automatically
	// (a chain of inheriting grants).
	modeAuto reachMode = iota
	// modeSetTo: the principal can SET ROLE to the role (a chain of grants
	// with SET).
	modeSetTo
	// modeViaSet: after SET ROLE to some role, this one's privileges are
	// inherited.
	modeViaSet
	// modeAdmin: reachable once the principal grants itself a membership it
	// administers.
	modeAdmin
)

type reachKey struct {
	role int64
	mode reachMode
}

// roleReach is the best way a principal reaches one role.
type roleReach struct {
	privRoute dbschema.NativeRoute // to use the role's privileges
	privVia   []string
	attr      bool                 // the principal can act as the role (its attributes apply)
	attrRoute dbschema.NativeRoute // to act as the role
	attrVia   []string
}

type nativeResolver struct {
	cat   *pgCatalog
	pg16  bool
	roles map[int64]pgRole
	edges map[int64][]pgMembership // by member
	gaps  []dbschema.NativeAccessGap
}

// resolveNativeAccess works out the answer from one catalog read. It is pure.
func resolveNativeAccess(ref dal.CollectionRef, cat *pgCatalog) *dbschema.NativeAccess {
	r := newNativeResolver(cat)
	answer := &dbschema.NativeAccess{
		Collection:  ref,
		AsOf:        cat.AsOf,
		RowSecurity: dbschema.NativeRowSecurity{Enabled: cat.Table.RowSecurity, AppliesToOwner: cat.Table.RowSecurity && cat.Table.ForceRowSecurity},
	}
	for _, role := range cat.Roles {
		if !role.Login && role.OID != cat.Table.Owner {
			continue
		}
		if p, ok := r.principal(role); ok {
			answer.Principals = append(answer.Principals, p)
		}
	}
	slices.SortFunc(answer.Principals, func(a, b dbschema.NativePrincipal) int { return strings.Compare(a.Name, b.Name) })
	if p, ok := r.everyone(); ok {
		answer.Principals = append(answer.Principals, p)
	}
	if _, ok := r.roles[cat.Table.Owner]; !ok {
		r.gap(gapUnresolvedOwner, fmt.Sprintf("table owner oid %d is not in pg_roles", cat.Table.Owner))
	}
	answer.Coverage = dbschema.NativeAccessCoverage{
		Level:      dbschema.NativeCoveragePartial,
		Covered:    slices.Clone(nativeCovered),
		BlindSpots: slices.Clone(nativeBlindSpots),
		Gaps:       r.gaps,
	}
	if len(r.gaps) > 0 || len(answer.Principals) == 0 {
		answer.Coverage.Level = dbschema.NativeCoverageUnknown
	}
	return answer
}

func newNativeResolver(cat *pgCatalog) *nativeResolver {
	r := &nativeResolver{cat: cat, pg16: cat.Version >= pg16Version,
		roles: make(map[int64]pgRole, len(cat.Roles)), edges: make(map[int64][]pgMembership)}
	for _, role := range cat.Roles {
		r.roles[role.OID] = role
	}
	for _, m := range cat.Memberships {
		_, roleOK := r.roles[m.Role]
		_, memberOK := r.roles[m.Member]
		if !roleOK || !memberOK {
			r.gap(gapUnresolvedMembership, fmt.Sprintf("membership of oid %d in oid %d names a role not in pg_roles", m.Member, m.Role))
			continue
		}
		r.edges[m.Member] = append(r.edges[m.Member], m)
	}
	for member := range r.edges {
		slices.SortFunc(r.edges[member], func(a, b pgMembership) int {
			return strings.Compare(r.roles[a.Role].Name, r.roles[b.Role].Name)
		})
	}
	// The database owner is an implicit, inheriting member of
	// pg_database_owner (PostgreSQL 14+); pg_auth_members does not list it.
	if cat.Version >= pg14Version && cat.DatabaseOwner != nil {
		if pgdo, ok := r.roleNamed(roleDatabaseOwner); ok {
			yes, no := true, false
			r.edges[*cat.DatabaseOwner] = append(r.edges[*cat.DatabaseOwner],
				pgMembership{Role: pgdo.OID, Member: *cat.DatabaseOwner, Inherit: &yes, Set: &no})
		}
	}
	for _, acl := range cat.TableACL {
		r.checkGrantee(acl.Grantee)
	}
	for _, acl := range cat.ColumnACL {
		r.checkGrantee(acl.Grantee)
	}
	return r
}

func (r *nativeResolver) gap(code, detail string) {
	r.gaps = append(r.gaps, dbschema.NativeAccessGap{Code: code, Detail: detail})
}

func (r *nativeResolver) checkGrantee(oid int64) {
	if _, ok := r.roles[oid]; !ok && oid != publicGrantee {
		r.gap(gapUnresolvedGrantee, fmt.Sprintf("a privilege is granted to oid %d, which is not in pg_roles", oid))
	}
}

func (r *nativeResolver) roleNamed(name string) (pgRole, bool) {
	for _, role := range r.cat.Roles {
		if role.Name == name {
			return role, true
		}
	}
	return pgRole{}, false
}

// inherits reports whether the member of e uses the role's privileges
// automatically. From PostgreSQL 16 the grant says; a missing option on 16 is
// read as true so that the answer over-reports rather than under-reports.
// Before 16 the member's rolinherit decides.
func (r *nativeResolver) inherits(e pgMembership) bool {
	if e.Inherit != nil {
		return *e.Inherit
	}
	return r.pg16 || r.roles[e.Member].Inherit
}

// sets reports whether the member of e can SET ROLE to the role: always before
// PostgreSQL 16, per grant from 16 (missing read as true).
func (r *nativeResolver) sets(e pgMembership) bool {
	return e.Set == nil || *e.Set
}

// reach follows membership from principal p.
func (r *nativeResolver) reach(p int64) map[int64]*roleReach {
	via := map[reachKey][]string{{p, modeAuto}: nil, {p, modeSetTo}: nil}
	queue := []reachKey{{p, modeAuto}, {p, modeSetTo}}
	push := func(k reachKey, path []string) {
		if _, seen := via[k]; seen {
			return
		}
		via[k] = path
		queue = append(queue, k)
	}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		path := via[k]
		role := r.roles[k.role]
		if (k.mode == modeSetTo || k.mode == modeAdmin) && role.CreateRole && !role.Super && !r.pg16 {
			// Before 16, CREATEROLE may grant membership in any non-superuser role.
			for _, target := range r.cat.Roles {
				if !target.Super {
					push(reachKey{target.OID, modeAdmin}, appendPath(path, target.Name))
				}
			}
		}
		for _, e := range r.edges[k.role] {
			next := appendPath(path, r.roles[e.Role].Name)
			inh, set := r.inherits(e), r.sets(e)
			switch k.mode {
			case modeAuto:
				if inh {
					push(reachKey{e.Role, modeAuto}, next)
				}
			case modeSetTo:
				if set {
					push(reachKey{e.Role, modeSetTo}, next)
				}
				if inh {
					push(reachKey{e.Role, modeViaSet}, next)
				}
			case modeViaSet:
				if inh {
					push(reachKey{e.Role, modeViaSet}, next)
				}
			case modeAdmin:
				if inh || set {
					push(reachKey{e.Role, modeAdmin}, next)
				}
			}
			if e.Admin {
				push(reachKey{e.Role, modeAdmin}, next)
			}
		}
	}
	out := map[int64]*roleReach{}
	for k, path := range via {
		rr := out[k.role]
		if rr == nil {
			rr = &roleReach{privRoute: dbschema.NativeRouteSelfGrant, privVia: path}
			out[k.role] = rr
		}
		route := routeFor(k, p)
		if better(route, path, rr.privRoute, rr.privVia) {
			rr.privRoute, rr.privVia = route, path
		}
		if k.mode == modeSetTo || k.mode == modeAdmin {
			if !rr.attr || better(route, path, rr.attrRoute, rr.attrVia) {
				rr.attr, rr.attrRoute, rr.attrVia = true, route, path
			}
		}
	}
	return out
}

func appendPath(path []string, name string) []string {
	return append(slices.Clone(path), name)
}

func routeFor(k reachKey, p int64) dbschema.NativeRoute {
	switch {
	case k.role == p && k.mode != modeAdmin:
		return dbschema.NativeRouteSelf
	case k.mode == modeAuto:
		return dbschema.NativeRouteInherited
	case k.mode == modeAdmin:
		return dbschema.NativeRouteSelfGrant
	default:
		return dbschema.NativeRouteSwitchRole
	}
}

func better(route dbschema.NativeRoute, via []string, than dbschema.NativeRoute, thanVia []string) bool {
	if routeRank[route] != routeRank[than] {
		return routeRank[route] < routeRank[than]
	}
	if len(via) != len(thanVia) {
		return len(via) < len(thanVia)
	}
	return strings.Join(via, "\x00") < strings.Join(thanVia, "\x00")
}

// privilegeSet collects a principal's grants.
type privilegeSet struct {
	table   map[dbschema.NativeOperation][]dbschema.NativeGrant
	columns map[columnKey][]string
}

type columnKey struct {
	op    dbschema.NativeOperation
	grant string // encoded grant
}

func newPrivilegeSet() *privilegeSet {
	return &privilegeSet{table: map[dbschema.NativeOperation][]dbschema.NativeGrant{}, columns: map[columnKey][]string{}}
}

func grantFor(source dbschema.NativeGrantSource, route dbschema.NativeRoute, via []string) dbschema.NativeGrant {
	confidence := dbschema.NativeInferred
	if route == dbschema.NativeRouteSelf {
		confidence = dbschema.NativeVerified
	}
	return dbschema.NativeGrant{Source: source, Route: route, Via: via, Confidence: confidence}
}

func (s *privilegeSet) addTable(op dbschema.NativeOperation, g dbschema.NativeGrant) {
	if !slices.ContainsFunc(s.table[op], func(h dbschema.NativeGrant) bool { return encodeGrant(h) == encodeGrant(g) }) {
		s.table[op] = append(s.table[op], g)
	}
}

func (s *privilegeSet) addColumn(op dbschema.NativeOperation, column string, g dbschema.NativeGrant) {
	k := columnKey{op: op, grant: encodeGrant(g)}
	if !slices.Contains(s.columns[k], column) {
		s.columns[k] = append(s.columns[k], column)
	}
}

func encodeGrant(g dbschema.NativeGrant) string {
	return strings.Join(append([]string{string(g.Source), string(g.Route), string(g.Confidence)}, g.Via...), "\x00")
}

func decodeGrant(s string) dbschema.NativeGrant {
	parts := strings.Split(s, "\x00")
	g := dbschema.NativeGrant{Source: dbschema.NativeGrantSource(parts[0]), Route: dbschema.NativeRoute(parts[1]),
		Confidence: dbschema.NativeConfidence(parts[2])}
	if len(parts) > 3 {
		g.Via = parts[3:]
	}
	return g
}

func (r *nativeResolver) addACL(s *privilegeSet, grantee int64, g dbschema.NativeGrant) {
	for _, acl := range r.cat.TableACL {
		if acl.Grantee == grantee {
			s.addTable(operationOf[acl.Privilege], g)
		}
	}
	for _, acl := range r.cat.ColumnACL {
		if acl.Grantee == grantee {
			s.addColumn(operationOf[acl.Privilege], acl.Column, g)
		}
	}
}

func addAll(s *privilegeSet, ops []string, g dbschema.NativeGrant) {
	for _, op := range ops {
		s.addTable(operationOf[op], g)
	}
}

// principal works out one role's native access. ok is false when the role can
// exercise no privilege on the table.
func (r *nativeResolver) principal(role pgRole) (dbschema.NativePrincipal, bool) {
	t := r.cat.Table
	reached := r.reach(role.OID)
	set := newPrivilegeSet()
	superuser, bypass, owner, namespace := false, false, false, false
	var superRoute dbschema.NativeRoute
	var superVia []string
	for oid, rr := range reached {
		held := r.roles[oid]
		source := dbschema.NativeFromMembership
		if oid == role.OID {
			source = dbschema.NativeFromDirect
		}
		g := grantFor(source, rr.privRoute, rr.privVia)
		r.addACL(set, oid, g)
		if oid == t.Owner {
			owner = true
			addAll(set, tableOperations, grantFor(dbschema.NativeFromOwnership, rr.privRoute, rr.privVia))
		}
		builtin := grantFor(dbschema.NativeFromBuiltinRole, rr.privRoute, rr.privVia)
		switch held.Name {
		case roleReadAllData:
			addAll(set, tableOperations[:1], builtin)
			namespace = true
		case roleWriteAllData:
			addAll(set, tableOperations[1:4], builtin)
			namespace = true
		}
		if oid == t.SchemaOwner || slices.Contains(r.cat.SchemaUsage, oid) {
			namespace = true
		}
		if rr.attr && held.Super && (!superuser || better(rr.attrRoute, rr.attrVia, superRoute, superVia)) {
			superuser, superRoute, superVia = true, rr.attrRoute, rr.attrVia
		}
		if rr.attr && held.BypassRLS {
			bypass = true
		}
	}
	if superuser {
		addAll(set, tableOperations, grantFor(dbschema.NativeFromSuperuser, superRoute, superVia))
		namespace = true
	}
	r.addACL(set, publicGrantee, grantFor(dbschema.NativeFromEveryone, dbschema.NativeRouteSelf, nil))
	if slices.Contains(r.cat.SchemaUsage, publicGrantee) {
		namespace = true
	}
	privileges := set.privileges()
	if len(privileges) == 0 {
		return dbschema.NativePrincipal{}, false
	}
	kind := dbschema.NativeGroup
	if role.Login {
		kind = dbschema.NativeLogin
	}
	effect := dbschema.NativeRowSecurityApplies
	switch {
	case !t.RowSecurity:
		effect = dbschema.NativeRowSecurityOff
	case superuser, bypass, owner && !t.ForceRowSecurity:
		effect = dbschema.NativeRowSecurityBypassed
	}
	return dbschema.NativePrincipal{
		Name: role.Name, Kind: kind, Superuser: role.Super, Owner: role.OID == t.Owner,
		Privileges: privileges, RowSecurity: effect, NamespaceAccess: namespaceAccess(namespace),
		Confidence: weakest(privileges),
	}, true
}

// everyone is the PUBLIC pseudo-principal, listed when PUBLIC holds a
// privilege on the table: it stands for every role, including roles created
// later.
func (r *nativeResolver) everyone() (dbschema.NativePrincipal, bool) {
	set := newPrivilegeSet()
	r.addACL(set, publicGrantee, grantFor(dbschema.NativeFromEveryone, dbschema.NativeRouteSelf, nil))
	privileges := set.privileges()
	if len(privileges) == 0 {
		return dbschema.NativePrincipal{}, false
	}
	effect := dbschema.NativeRowSecurityOff
	if r.cat.Table.RowSecurity {
		effect = dbschema.NativeRowSecurityApplies
	}
	return dbschema.NativePrincipal{
		Name: publicPrincipalName, Kind: dbschema.NativeEveryone, Privileges: privileges, RowSecurity: effect,
		NamespaceAccess: namespaceAccess(slices.Contains(r.cat.SchemaUsage, publicGrantee)),
		Confidence:      weakest(privileges),
	}, true
}

func namespaceAccess(granted bool) dbschema.NativeNamespaceAccess {
	if granted {
		return dbschema.NativeNamespaceGranted
	}
	return dbschema.NativeNamespaceMissing
}

// privileges lists the set in a fixed order. A column privilege is left out
// when the same operation is held on the whole table.
func (s *privilegeSet) privileges() []dbschema.NativePrivilege {
	var out []dbschema.NativePrivilege
	for op, grants := range s.table {
		out = append(out, newPrivilege(op, nil, grants))
	}
	for k, columns := range s.columns {
		if _, whole := s.table[k.op]; whole {
			continue
		}
		slices.Sort(columns)
		out = append(out, newPrivilege(k.op, columns, []dbschema.NativeGrant{decodeGrant(k.grant)}))
	}
	slices.SortFunc(out, func(a, b dbschema.NativePrivilege) int {
		return cmp.Or(cmp.Compare(operationRank[a.Operation], operationRank[b.Operation]),
			cmp.Compare(len(a.Fields), len(b.Fields)),
			strings.Compare(strings.Join(a.Fields, ","), strings.Join(b.Fields, ",")),
			strings.Compare(encodeGrant(a.Grants[0]), encodeGrant(b.Grants[0])))
	})
	return out
}

func newPrivilege(op dbschema.NativeOperation, fields []string, grants []dbschema.NativeGrant) dbschema.NativePrivilege {
	slices.SortFunc(grants, func(a, b dbschema.NativeGrant) int {
		return cmp.Or(cmp.Compare(routeRank[a.Route], routeRank[b.Route]), strings.Compare(encodeGrant(a), encodeGrant(b)))
	})
	confidence := dbschema.NativeUnknown
	for _, g := range grants {
		if confidenceRank[g.Confidence] < confidenceRank[confidence] {
			confidence = g.Confidence
		}
	}
	return dbschema.NativePrivilege{Operation: op, Fields: fields, Confidence: confidence, Grants: grants}
}

func weakest(privileges []dbschema.NativePrivilege) dbschema.NativeConfidence {
	confidence := dbschema.NativeVerified
	for _, p := range privileges {
		if confidenceRank[p.Confidence] > confidenceRank[confidence] {
			confidence = p.Confidence
		}
	}
	return confidence
}
