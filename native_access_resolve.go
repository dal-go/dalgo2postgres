package dalgo2postgres

import (
	"cmp"
	"fmt"
	"maps"
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
)

// Gap codes ReadNativeAccess reports.
const (
	gapUnresolvedMembership    = "unresolved-membership"
	gapUnresolvedGrantee       = "unresolved-grantee"
	gapUnresolvedOwner         = "unresolved-owner"
	gapUnresolvedDatabaseOwner = "unresolved-database-owner"
	gapUnresolvedRelative      = "unresolved-relative"
)

// nativeCovered is what a PostgreSQL native answer reads.
var nativeCovered = []string{
	"roles", "role-membership", "ownership", "superusers", "builtin-roles",
	"collection-privileges", "field-privileges", "everyone-privileges",
	"namespace-access", "row-security-flags", "inheritance-and-partitions",
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
}

var allOperations = []dbschema.NativeOperation{
	dbschema.NativeRead, dbschema.NativeInsert, dbschema.NativeUpdate, dbschema.NativeDelete, dbschema.NativeTruncate,
}

var operationOf = map[string]dbschema.NativeOperation{
	"SELECT":   dbschema.NativeRead,
	"INSERT":   dbschema.NativeInsert,
	"UPDATE":   dbschema.NativeUpdate,
	"DELETE":   dbschema.NativeDelete,
	"TRUNCATE": dbschema.NativeTruncate,
}

// builtinRole is what a predefined role reaches on every table.
type builtinRole struct {
	operations []dbschema.NativeOperation
	// bypassesRowSecurity: the role reaches the data outside SQL (the
	// relation's files, or the server's operating-system user).
	bypassesRowSecurity bool
}

// builtinRoles are the predefined roles that reach every table. The
// server-file roles reach the data through the server's files or programs:
// pg_read_server_files reads the relation's files, and the other two run as,
// or write as, the server's operating-system user, which reaches everything.
var builtinRoles = map[string]builtinRole{
	"pg_read_all_data":          {operations: allOperations[:1]},
	"pg_write_all_data":         {operations: allOperations[1:4]},
	"pg_read_server_files":      {operations: allOperations[:1], bypassesRowSecurity: true},
	"pg_write_server_files":     {operations: allOperations, bypassesRowSecurity: true},
	"pg_execute_server_program": {operations: allOperations, bypassesRowSecurity: true},
}

var operationRank = map[dbschema.NativeOperation]int{
	dbschema.NativeRead: 0, dbschema.NativeInsert: 1, dbschema.NativeUpdate: 2,
	dbschema.NativeDelete: 3, dbschema.NativeTruncate: 4,
}

var routeRank = map[dbschema.NativeRoute]int{
	dbschema.NativeRouteSelf: 0, dbschema.NativeRouteInherited: 1,
	dbschema.NativeRouteSwitchRole: 2, dbschema.NativeRouteSelfGrant: 3,
}

// routeTier orders routes by how current they are: the principal's own, then
// inherited, then those that need an action first (SET ROLE or a self-grant).
func routeTier(route dbschema.NativeRoute) int {
	return min(routeRank[route], 2)
}

// currentTier is 0 for a route in effect now (the principal's own, or
// inherited) and 1 for one that needs an action first (SET ROLE, a self-grant).
func currentTier(route dbschema.NativeRoute) int {
	return min(routeRank[route], 2) / 2
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

// aclEntry is one privilege of a grantee on the table or a relative; column is
// empty for a table privilege.
type aclEntry struct {
	op     dbschema.NativeOperation
	column string
	rel    int64
}

type nativeResolver struct {
	cat       *pgCatalog
	pg16      bool
	roles     map[int64]pgRole
	byName    map[string]pgRole
	edges     map[int64][]pgMembership // by member
	acl       map[int64][]aclEntry     // by grantee
	relatives map[int64]pgRelative
	gaps      []dbschema.NativeAccessGap
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
		roles: make(map[int64]pgRole, len(cat.Roles)), byName: make(map[string]pgRole, len(cat.Roles)),
		edges: map[int64][]pgMembership{}, acl: map[int64][]aclEntry{}, relatives: map[int64]pgRelative{}}
	for _, role := range cat.Roles {
		r.roles[role.OID] = role
		r.byName[role.Name] = role
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
	if cat.DatabaseOwner != nil {
		if _, ok := r.roles[*cat.DatabaseOwner]; !ok {
			r.gap(gapUnresolvedDatabaseOwner, fmt.Sprintf("database owner oid %d is not in pg_roles", *cat.DatabaseOwner))
		} else if pgdo, ok := r.byName[roleDatabaseOwner]; ok && cat.Version >= pg14Version {
			// The database owner is an implicit, inheriting member of
			// pg_database_owner (PostgreSQL 14+); pg_auth_members does not list it.
			yes, no := true, false
			r.edges[*cat.DatabaseOwner] = append(r.edges[*cat.DatabaseOwner],
				pgMembership{Role: pgdo.OID, Member: *cat.DatabaseOwner, Inherit: &yes, Set: &no})
		}
	}
	if _, ok := r.roles[cat.Table.Owner]; !ok {
		r.gap(gapUnresolvedOwner, fmt.Sprintf("table owner oid %d is not in pg_roles", cat.Table.Owner))
	}
	for _, rel := range cat.Relatives {
		r.relatives[rel.OID] = rel
		if _, ok := r.roles[rel.Owner]; !ok {
			r.gap(gapUnresolvedOwner, fmt.Sprintf("owner oid %d of %s %s.%s is not in pg_roles", rel.Owner, rel.Relation, rel.Schema, rel.Name))
		}
	}
	for _, a := range cat.TableACL {
		r.addEntry(a.Grantee, aclEntry{op: operationOf[a.Privilege], rel: a.Rel})
	}
	for _, a := range cat.ColumnACL {
		r.addEntry(a.Grantee, aclEntry{op: operationOf[a.Privilege], column: a.Column, rel: a.Rel})
	}
	return r
}

func (r *nativeResolver) addEntry(grantee int64, e aclEntry) {
	if _, ok := r.roles[grantee]; !ok && grantee != publicGrantee {
		r.gap(gapUnresolvedGrantee, fmt.Sprintf("a privilege is granted to oid %d, which is not in pg_roles", grantee))
		return
	}
	if _, ok := r.relatives[e.rel]; !ok && e.rel != r.cat.Table.OID {
		r.gap(gapUnresolvedRelative, fmt.Sprintf("a privilege names relation oid %d, which is neither the table nor a relative", e.rel))
		return
	}
	r.acl[grantee] = append(r.acl[grantee], e)
}

func (r *nativeResolver) gap(code, detail string) {
	r.gaps = append(r.gaps, dbschema.NativeAccessGap{Code: code, Detail: detail})
}

// relativeVia is the step a privilege on a relative adds to NativeGrant.Via:
// "ancestor:schema.name" (a query on it reads this table's rows) or
// "descendant:schema.name" (it holds a share of them).
func relativeVia(rel pgRelative) string {
	return rel.Relation + ":" + rel.Schema + "." + rel.Name
}

// grantKey identifies a grant within one privilege set.
type grantKey struct {
	source     dbschema.NativeGrantSource
	route      dbschema.NativeRoute
	confidence dbschema.NativeConfidence
	via        string
}

func keyOf(g dbschema.NativeGrant) grantKey {
	return grantKey{source: g.Source, route: g.Route, confidence: g.Confidence, via: strings.Join(g.Via, "\x00")}
}

// privilegeSet collects a principal's grants.
type privilegeSet struct {
	grants  map[grantKey]dbschema.NativeGrant
	table   map[dbschema.NativeOperation]map[grantKey]bool
	columns map[dbschema.NativeOperation]map[grantKey][]string
}

func newPrivilegeSet() *privilegeSet {
	return &privilegeSet{grants: map[grantKey]dbschema.NativeGrant{},
		table:   map[dbschema.NativeOperation]map[grantKey]bool{},
		columns: map[dbschema.NativeOperation]map[grantKey][]string{}}
}

func grantFor(source dbschema.NativeGrantSource, route dbschema.NativeRoute, via []string) dbschema.NativeGrant {
	confidence := dbschema.NativeInferred
	if route == dbschema.NativeRouteSelf {
		confidence = dbschema.NativeVerified
	}
	return dbschema.NativeGrant{Source: source, Route: route, Via: via, Confidence: confidence}
}

func inferred(g dbschema.NativeGrant) dbschema.NativeGrant {
	g.Confidence = dbschema.NativeInferred
	return g
}

func (s *privilegeSet) add(op dbschema.NativeOperation, column string, g dbschema.NativeGrant) {
	k := keyOf(g)
	s.grants[k] = g
	if column == "" {
		if s.table[op] == nil {
			s.table[op] = map[grantKey]bool{}
		}
		s.table[op][k] = true
		return
	}
	if s.columns[op] == nil {
		s.columns[op] = map[grantKey][]string{}
	}
	if !slices.Contains(s.columns[op][k], column) {
		s.columns[op][k] = append(s.columns[op][k], column)
	}
}

func (s *privilegeSet) addAll(ops []dbschema.NativeOperation, g dbschema.NativeGrant) {
	for _, op := range ops {
		s.add(op, "", g)
	}
}

// addACL adds the privileges grantee holds on the table and its relatives. A
// privilege on a relative is inferred, with the relative last in Via.
func (r *nativeResolver) addACL(s *privilegeSet, grantee int64, g dbschema.NativeGrant) {
	for _, e := range r.acl[grantee] {
		eg := g
		if rel, ok := r.relatives[e.rel]; ok {
			eg = inferred(g)
			eg.Via = appendPath(g.Via, relativeVia(rel))
		}
		s.add(e.op, e.column, eg)
	}
}

// principal works out one role's native access. ok is false when the role can
// exercise no privilege on the table.
func (r *nativeResolver) principal(role pgRole) (dbschema.NativePrincipal, bool) {
	t := r.cat.Table
	reached := r.reach(role.OID)
	set := newPrivilegeSet()
	superuser, bypass, owner := false, false, false
	namespaceTier := -1 // the most current route that reaches the schema; -1 for none
	reachNamespace := func(route dbschema.NativeRoute) {
		if tier := currentTier(route); namespaceTier < 0 || tier < namespaceTier {
			namespaceTier = tier
		}
	}
	var superRoute dbschema.NativeRoute
	var superVia []string
	for oid, rr := range reached {
		held := r.roles[oid]
		source := dbschema.NativeFromMembership
		if oid == role.OID {
			source = dbschema.NativeFromDirect
		}
		r.addACL(set, oid, grantFor(source, rr.privRoute, rr.privVia))
		ownership := grantFor(dbschema.NativeFromOwnership, rr.privRoute, rr.privVia)
		if oid == t.Owner {
			owner = true
			set.addAll(allOperations, ownership)
		}
		for _, rel := range r.cat.Relatives {
			if rel.Owner == oid {
				g := inferred(ownership)
				g.Via = appendPath(rr.privVia, relativeVia(rel))
				set.addAll(allOperations, g)
			}
		}
		if b, ok := builtinRoles[held.Name]; ok {
			set.addAll(b.operations, inferred(grantFor(dbschema.NativeFromBuiltinRole, rr.privRoute, rr.privVia)))
			reachNamespace(rr.privRoute)
			bypass = bypass || b.bypassesRowSecurity
		}
		if oid == t.SchemaOwner || slices.Contains(r.cat.SchemaUsage, oid) {
			reachNamespace(rr.privRoute)
		}
		if rr.attr && held.Super && (!superuser || better(rr.attrRoute, rr.attrVia, superRoute, superVia)) {
			superuser, superRoute, superVia = true, rr.attrRoute, rr.attrVia
		}
		if rr.attr && held.BypassRLS {
			bypass = true
		}
	}
	if superuser {
		set.addAll(allOperations, grantFor(dbschema.NativeFromSuperuser, superRoute, superVia))
		reachNamespace(superRoute)
	}
	r.addACL(set, publicGrantee, grantFor(dbschema.NativeFromEveryone, dbschema.NativeRouteSelf, nil))
	if slices.Contains(r.cat.SchemaUsage, publicGrantee) {
		reachNamespace(dbschema.NativeRouteSelf)
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
		Privileges: privileges, RowSecurity: effect,
		NamespaceAccess: namespaceAccess(namespaceTier, bestTier(privileges)),
		Confidence:      weakest(privileges),
	}, true
}

// everyone is the PUBLIC pseudo-principal, listed when PUBLIC holds a
// privilege on the table or a relative: it stands for every role, including
// roles created later.
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
	namespaceTier := -1
	if slices.Contains(r.cat.SchemaUsage, publicGrantee) {
		namespaceTier = 0
	}
	return dbschema.NativePrincipal{
		Name: publicPrincipalName, Kind: dbschema.NativeEveryone, Privileges: privileges, RowSecurity: effect,
		NamespaceAccess: namespaceAccess(namespaceTier, 0),
		Confidence:      weakest(privileges),
	}, true
}

// namespaceAccess compares how the schema is reached with how a privilege is
// ([currentTier]): granted when the schema is reached in effect now, or
// only by SET ROLE or a self-grant when every privilege is too; unknown when a
// privilege is in effect now but the schema is reached only by such a route;
// missing when nothing reaches it.
func namespaceAccess(namespaceTier, privilegeTier int) dbschema.NativeNamespaceAccess {
	switch {
	case namespaceTier < 0:
		return dbschema.NativeNamespaceMissing
	case namespaceTier <= privilegeTier:
		return dbschema.NativeNamespaceGranted
	default:
		return dbschema.NativeNamespaceUnknown
	}
}

func bestTier(privileges []dbschema.NativePrivilege) int {
	best := currentTier(dbschema.NativeRouteSelfGrant)
	for _, p := range privileges {
		for _, g := range p.Grants {
			best = min(best, currentTier(g.Route))
		}
	}
	return best
}

// privileges lists the set in a fixed order, one entry per operation and set
// of fields with every grant behind it. A column grant is folded into the
// whole-table privilege only when a whole-table grant of that operation is at
// least as current (route) and as certain (confidence); otherwise both are
// listed, so a current column privilege is never hidden behind a potential
// whole-table one.
func (s *privilegeSet) privileges() []dbschema.NativePrivilege {
	var out []dbschema.NativePrivilege
	for op, grants := range s.table {
		out = append(out, s.newPrivilege(op, nil, slices.Collect(maps.Keys(grants))))
	}
	for op, byGrant := range s.columns {
		groups := map[string][]grantKey{}
		for k, columns := range byGrant {
			if s.covered(op, s.grants[k]) {
				continue
			}
			slices.Sort(columns)
			fields := strings.Join(columns, "\x00")
			groups[fields] = append(groups[fields], k)
		}
		for fields, keys := range groups {
			out = append(out, s.newPrivilege(op, strings.Split(fields, "\x00"), keys))
		}
	}
	slices.SortFunc(out, func(a, b dbschema.NativePrivilege) int {
		return cmp.Or(cmp.Compare(operationRank[a.Operation], operationRank[b.Operation]),
			cmp.Compare(len(a.Fields), len(b.Fields)),
			strings.Compare(strings.Join(a.Fields, ","), strings.Join(b.Fields, ",")))
	})
	return out
}

// covered reports whether a whole-table grant of op is at least as current and
// as certain as the column grant g.
func (s *privilegeSet) covered(op dbschema.NativeOperation, g dbschema.NativeGrant) bool {
	for k := range s.table[op] {
		t := s.grants[k]
		if routeTier(t.Route) <= routeTier(g.Route) && confidenceRank[t.Confidence] <= confidenceRank[g.Confidence] {
			return true
		}
	}
	return false
}

func (s *privilegeSet) newPrivilege(op dbschema.NativeOperation, fields []string, keys []grantKey) dbschema.NativePrivilege {
	grants := make([]dbschema.NativeGrant, len(keys))
	for i, k := range keys {
		grants[i] = s.grants[k]
	}
	slices.SortFunc(grants, func(a, b dbschema.NativeGrant) int {
		ka, kb := keyOf(a), keyOf(b)
		return cmp.Or(cmp.Compare(routeRank[a.Route], routeRank[b.Route]),
			strings.Compare(string(ka.source), string(kb.source)),
			strings.Compare(string(ka.confidence), string(kb.confidence)),
			strings.Compare(ka.via, kb.via))
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
