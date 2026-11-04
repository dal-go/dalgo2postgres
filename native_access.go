package dalgo2postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// minNativeAccessVersion is the oldest server ReadNativeAccess reads:
// PostgreSQL 13 (server_version_num 130000).
const minNativeAccessVersion = 130000

// nativeAccessSQL reads, in one statement and so from one snapshot, every
// catalog fact ReadNativeAccess needs about one table, as one JSON document.
// The schema and the table name are bound parameters; neither is ever part of
// this text.
//
// Every relation it reads is readable by PUBLIC in a default installation, so
// any role that can connect to the database can run it, with no superuser,
// pg_read_all_settings or pg_read_all_data membership: pg_roles (never
// rolpassword), pg_auth_members, pg_database, pg_class, pg_namespace,
// pg_attribute and pg_inherits, plus current_setting('server_version_num'). A
// DBA who has revoked SELECT on any of them from PUBLIC must grant it back to
// the catalog credential, or the statement fails and the answer is an error,
// never "nobody".
//
// It targets PostgreSQL 13 and later. Two version differences are absorbed in
// the text: the per-grant inherit_option and set_option of pg_auth_members
// (PostgreSQL 16) are read through to_jsonb, so they are NULL before 16 rather
// than a missing-column error; and pg_database_owner (14) is matched by name.
//
// The table's relatives in pg_inherits, its ancestors (a partitioned parent,
// or an inheritance parent) and its descendants (partitions, inheritance
// children), are read with their owners and privileges: a query on an ancestor
// reads the table's rows, and a query on a descendant reads its share of them.
//
// A NULL relacl or nspacl means the default privileges, which acldefault
// spells out (the owner holds every privilege, PUBLIC none on a table; the
// owner holds USAGE and CREATE on a schema). Grantee 0 is PUBLIC.
const nativeAccessSQL = `
	WITH RECURSIVE tbl AS (
		SELECT c.oid, c.relowner, c.relrowsecurity, c.relforcerowsecurity,
		       n.nspowner, n.nspacl
		FROM pg_catalog.pg_class AS c
		JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		WHERE n.nspname = $1::text AND c.relname = $2::text
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
	), up(oid) AS (
		SELECT i.inhparent FROM pg_catalog.pg_inherits AS i JOIN tbl AS t ON i.inhrelid = t.oid
		UNION
		SELECT i.inhparent FROM pg_catalog.pg_inherits AS i JOIN up AS u ON i.inhrelid = u.oid
	), down(oid) AS (
		SELECT i.inhrelid FROM pg_catalog.pg_inherits AS i JOIN tbl AS t ON i.inhparent = t.oid
		UNION
		SELECT i.inhrelid FROM pg_catalog.pg_inherits AS i JOIN down AS d ON i.inhparent = d.oid
	), rel AS (
		SELECT c.oid, c.relowner, c.relacl, n.nspname, c.relname, k.relation
		FROM (SELECT t.oid, 'self' AS relation FROM tbl AS t
		      UNION ALL SELECT u.oid, 'ancestor' FROM up AS u
		      UNION ALL SELECT d.oid, 'descendant' FROM down AS d) AS k
		JOIN pg_catalog.pg_class AS c ON c.oid = k.oid
		JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
	)
	SELECT pg_catalog.json_build_object(
		'version', pg_catalog.current_setting('server_version_num')::int,
		'asOf', pg_catalog.statement_timestamp(),
		'databaseOwner', (SELECT d.datdba::bigint FROM pg_catalog.pg_database AS d
		                  WHERE d.datname = pg_catalog.current_database()),
		'roles', (SELECT pg_catalog.json_agg(pg_catalog.json_build_object(
		              'oid', r.oid::bigint, 'name', r.rolname, 'super', r.rolsuper,
		              'inherit', r.rolinherit, 'createRole', r.rolcreaterole,
		              'login', r.rolcanlogin, 'bypassRls', r.rolbypassrls) ORDER BY r.oid)
		          FROM pg_catalog.pg_roles AS r),
		'memberships', (SELECT pg_catalog.json_agg(pg_catalog.json_build_object(
		                    'role', m.roleid::bigint, 'member', m.member::bigint,
		                    'admin', m.admin_option,
		                    'inherit', pg_catalog.to_jsonb(m) -> 'inherit_option',
		                    'set', pg_catalog.to_jsonb(m) -> 'set_option') ORDER BY m.roleid, m.member)
		                FROM pg_catalog.pg_auth_members AS m),
		'table', (SELECT pg_catalog.json_build_object(
		              'oid', t.oid::bigint, 'owner', t.relowner::bigint, 'rowSecurity', t.relrowsecurity,
		              'forceRowSecurity', t.relforcerowsecurity, 'schemaOwner', t.nspowner::bigint)
		          FROM tbl AS t),
		'relatives', (SELECT pg_catalog.json_agg(pg_catalog.json_build_object(
		                  'oid', r.oid::bigint, 'schema', r.nspname, 'name', r.relname,
		                  'relation', r.relation, 'owner', r.relowner::bigint) ORDER BY r.relation, r.nspname, r.relname)
		              FROM rel AS r WHERE r.relation <> 'self'),
		'tableAcl', (SELECT pg_catalog.json_agg(pg_catalog.json_build_object(
		                 'rel', r.oid::bigint, 'grantee', a.grantee::bigint, 'privilege', a.privilege_type)
		                 ORDER BY r.oid, a.grantee, a.privilege_type)
		             FROM rel AS r
		             CROSS JOIN LATERAL pg_catalog.aclexplode(
		                 COALESCE(r.relacl, pg_catalog.acldefault('r', r.relowner))) AS a
		             WHERE a.privilege_type IN ('SELECT', 'INSERT', 'UPDATE', 'DELETE', 'TRUNCATE')),
		'columnAcl', (SELECT pg_catalog.json_agg(pg_catalog.json_build_object(
		                  'rel', r.oid::bigint, 'column', att.attname, 'grantee', a.grantee::bigint,
		                  'privilege', a.privilege_type)
		                  ORDER BY r.oid, att.attnum, a.grantee, a.privilege_type)
		              FROM rel AS r
		              JOIN pg_catalog.pg_attribute AS att
		                ON att.attrelid = r.oid AND att.attnum > 0 AND NOT att.attisdropped
		               AND att.attacl IS NOT NULL
		              CROSS JOIN LATERAL pg_catalog.aclexplode(att.attacl) AS a
		              WHERE a.privilege_type IN ('SELECT', 'INSERT', 'UPDATE')),
		'schemaUsage', (SELECT pg_catalog.json_agg(a.grantee::bigint ORDER BY a.grantee)
		                FROM tbl AS t
		                CROSS JOIN LATERAL pg_catalog.aclexplode(
		                    COALESCE(t.nspacl, pg_catalog.acldefault('n', t.nspowner))) AS a
		                WHERE a.privilege_type = 'USAGE')
	)::text`

// pgCatalog is the decoded result of [nativeAccessSQL].
type pgCatalog struct {
	Version       int            `json:"version"`
	AsOf          time.Time      `json:"asOf"`
	DatabaseOwner *int64         `json:"databaseOwner"`
	Roles         []pgRole       `json:"roles"`
	Memberships   []pgMembership `json:"memberships"`
	Table         *pgTable       `json:"table"`
	Relatives     []pgRelative   `json:"relatives"`
	TableACL      []pgACL        `json:"tableAcl"`
	ColumnACL     []pgColumnACL  `json:"columnAcl"`
	SchemaUsage   []int64        `json:"schemaUsage"`
}

// pgRelative is an ancestor or a descendant of the table in pg_inherits.
type pgRelative struct {
	OID      int64  `json:"oid"`
	Schema   string `json:"schema"`
	Name     string `json:"name"`
	Relation string `json:"relation"` // "ancestor" or "descendant"
	Owner    int64  `json:"owner"`
}

type pgRole struct {
	OID        int64  `json:"oid"`
	Name       string `json:"name"`
	Super      bool   `json:"super"`
	Inherit    bool   `json:"inherit"`
	CreateRole bool   `json:"createRole"`
	Login      bool   `json:"login"`
	BypassRLS  bool   `json:"bypassRls"`
}

// pgMembership is one pg_auth_members row. Inherit and Set are nil before
// PostgreSQL 16, which has no per-grant options.
type pgMembership struct {
	Role    int64 `json:"role"`
	Member  int64 `json:"member"`
	Admin   bool  `json:"admin"`
	Inherit *bool `json:"inherit"`
	Set     *bool `json:"set"`
}

type pgTable struct {
	OID              int64 `json:"oid"`
	Owner            int64 `json:"owner"`
	RowSecurity      bool  `json:"rowSecurity"`
	ForceRowSecurity bool  `json:"forceRowSecurity"`
	SchemaOwner      int64 `json:"schemaOwner"`
}

// pgACL is one table privilege of the table (Rel is the table's OID) or of
// one of its relatives.
type pgACL struct {
	Rel       int64  `json:"rel"`
	Grantee   int64  `json:"grantee"`
	Privilege string `json:"privilege"`
}

type pgColumnACL struct {
	Rel       int64  `json:"rel"`
	Column    string `json:"column"`
	Grantee   int64  `json:"grantee"`
	Privilege string `json:"privilege"`
}

// ReadNativeAccess implements [dbschema.NativeAccessReader]: the PostgreSQL
// roles that can reach the referenced table, view, materialized view,
// partitioned table or foreign table through their own credentials, with the
// privileges they can exercise and why. The table is found as the schema reader
// finds it (reference schema, else [WithSchema]; name per [IdentifierMode]).
//
// It reads the catalog with one statement ([nativeAccessSQL]) and works the
// answer out in Go: role membership is followed transitively, respecting each
// grant's inheritance (rolinherit before PostgreSQL 16, inherit_option from 16),
// SET ROLE (always possible before 16, set_option from 16), ADMIN OPTION and,
// before 16, CREATEROLE, which let a role grant itself membership. Superusers and
// the owner always appear; PUBLIC appears as one principal of kind everyone and
// its privileges are added to every principal. A table that does not exist is a
// not-found error; a server older than PostgreSQL 13 is a
// *[dbschema.NotSupportedError]. See the README for the coverage statement.
func (d *Database) ReadNativeAccess(ctx context.Context, ref *dal.CollectionRef) (*dbschema.NativeAccess, error) {
	schema, name, err := d.lookupNames(ref)
	if err != nil {
		return nil, leaveAdapter(err)
	}
	label := collectionLabel(ref.Schema(), name)
	cat, err := readNativeCatalog(ctx, d.catalogQueries(), schema, name, label)
	if err != nil {
		return nil, leaveAdapter(err)
	}
	return resolveNativeAccess(*ref, cat), nil
}

func readNativeCatalog(ctx context.Context, db catalogQueryer, schema, name string, label quotedCollection) (*pgCatalog, error) {
	var doc string
	if err := db.QueryRowContext(ctx, nativeAccessSQL, schema, name).Scan(&doc); err != nil {
		return nil, fmt.Errorf("dalgo2postgres: native access of %s: %w", label, err)
	}
	var cat pgCatalog
	if err := json.Unmarshal([]byte(doc), &cat); err != nil {
		return nil, fmt.Errorf("dalgo2postgres: native access of %s decode: %w", label, err)
	}
	if cat.Version < minNativeAccessVersion {
		return nil, &dbschema.NotSupportedError{Op: "ReadNativeAccess", Backend: "dalgo2postgres",
			Reason: fmt.Sprintf("PostgreSQL 13 or later is required, server_version_num is %d", cat.Version)}
	}
	if cat.Table == nil {
		return nil, newQualifiedCollectionNotFoundError(label)
	}
	return &cat, nil
}

func (v *readonlySchemaView) ReadNativeAccess(ctx context.Context, ref *dal.CollectionRef) (*dbschema.NativeAccess, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) (*dbschema.NativeAccess, error) {
		return db.ReadNativeAccess(ctx, ref)
	})
}
