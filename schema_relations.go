package dalgo2postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// relkindMaterializedView is pg_class.relkind of a materialized view.
const relkindMaterializedView = "m"

// relationVisibleSQL is the privilege test information_schema.tables applies
// (a relation is listed to a role that owns it, or holds any privilege on it or
// on one of its columns), spelled over pg_class aliased c. The reader applies it to
// the relations information_schema leaves out as well, so a materialized view
// is listed to the roles a table of the same grants would be.
const relationVisibleSQL = `(pg_catalog.pg_has_role(c.relowner, 'USAGE')
	      OR pg_catalog.has_table_privilege(c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
	      OR pg_catalog.has_any_column_privilege(c.oid, 'SELECT, INSERT, UPDATE, REFERENCES'))`

// relationKindsSQL are the relkinds the reader lists: a table (r), a partitioned
// table (p), a foreign table (f), a view (v) and a materialized view (m). A
// partition is a table (r) as far as PostgreSQL is concerned, so it is listed
// exactly as information_schema.tables lists it. An index, a sequence, a
// composite type and a TOAST table are not collections.
const relationKindsSQL = `c.relkind IN ('r', 'p', 'f', 'v', 'm')`

// listRelationsSQL lists the collections of one schema ($1) in name order, and
// whether each is a view or a materialized view. The name order is the one
// information_schema.tables gave: both columns are of type name, which sorts in
// the C collation.
const listRelationsSQL = `SELECT c.relname::text, c.relkind IN ('v', 'm') AS is_view
	FROM pg_catalog.pg_class AS c
	JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
	WHERE n.nspname = $1
	  AND ` + relationKindsSQL + `
	  AND NOT pg_catalog.pg_is_other_temp_schema(n.oid)
	  AND ` + relationVisibleSQL + `
	ORDER BY c.relname`

// relationKindSQL reads the kind of one relation: the schema is $1 and the name
// $2. It answers no row for a name that is not one of the listed kinds.
const relationKindSQL = `SELECT c.relkind::text
	FROM pg_catalog.pg_class AS c
	JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
	WHERE n.nspname = $1 AND c.relname = $2
	  AND ` + relationKindsSQL + `
	  AND NOT pg_catalog.pg_is_other_temp_schema(n.oid)
	  AND ` + relationVisibleSQL

// listSchemasSQL lists the schemas a role can use, but not the system schemas:
// pg_catalog, information_schema, pg_toast, and the pg_temp_N and pg_toast_temp_N
// schemas of the sessions that have temporary objects. The backslashes escape the
// underscore LIKE would otherwise read as any one character.
const listSchemasSQL = `SELECT n.nspname::text
	FROM pg_catalog.pg_namespace AS n
	WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
	  AND n.nspname NOT LIKE 'pg\_temp\_%'
	  AND n.nspname NOT LIKE 'pg\_toast\_temp\_%'
	  AND pg_catalog.has_schema_privilege(n.oid, 'USAGE')
	ORDER BY n.nspname`

// informationSchemaColumnsSQL reads the columns of a table, view, partitioned
// table or foreign table (schema $1, name $2) in table order. information_schema
// reports a generated column's expression in generation_expression and leaves its
// column_default empty, and an identity column's column_default is empty too.
const informationSchemaColumnsSQL = `SELECT column_name, data_type, udt_name,
	        character_maximum_length, numeric_precision, numeric_scale,
	        is_nullable, column_default, is_identity, is_generated, generation_expression
	 FROM information_schema.columns
	 WHERE table_schema = $1 AND table_name = $2
	 ORDER BY ordinal_position`

// materializedViewColumnsSQL reads the columns of a materialized view (schema
// $1, name $2) in the shape informationSchemaColumnsSQL answers, because
// information_schema.columns does not list them. The type spelling follows the
// views of information_schema: ARRAY for an array, USER-DEFINED for a type outside
// pg_catalog (a domain or an enum), otherwise the name format_type gives. A
// materialized view has no default, identity or generated column, so those are
// the constants information_schema would give a plain column.
const materializedViewColumnsSQL = `SELECT a.attname::text,
	        CASE WHEN t.typelem <> 0 AND t.typlen = -1 THEN 'ARRAY'
	             WHEN tn.nspname = 'pg_catalog' THEN pg_catalog.format_type(t.oid, NULL)
	             ELSE 'USER-DEFINED' END,
	        t.typname::text,
	        CASE WHEN t.typname IN ('varchar', 'bpchar') AND a.atttypmod > 4 THEN a.atttypmod - 4 END,
	        CASE WHEN t.typname = 'numeric' AND a.atttypmod > 4 THEN ((a.atttypmod - 4) >> 16) & 65535 END,
	        CASE WHEN t.typname = 'numeric' AND a.atttypmod > 4 THEN (a.atttypmod - 4) & 65535 END,
	        CASE WHEN a.attnotnull THEN 'NO' ELSE 'YES' END,
	        NULL::text, 'NO', 'NEVER', NULL::text
	 FROM pg_catalog.pg_attribute AS a
	 JOIN pg_catalog.pg_class AS c ON c.oid = a.attrelid
	 JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
	 JOIN pg_catalog.pg_type AS t ON t.oid = a.atttypid
	 JOIN pg_catalog.pg_namespace AS tn ON tn.oid = t.typnamespace
	 WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
	 ORDER BY a.attnum`

// relation is one collection of a schema: its name, and whether PostgreSQL keeps
// it as a view (a view or a materialized view) rather than as a table.
type relation struct {
	name string
	view bool
}

// listRelations reads the collections of one schema in name order. op names the
// public method in error messages.
func listRelations(ctx context.Context, db *sql.DB, schema, op string) ([]relation, error) {
	rows, err := db.QueryContext(ctx, listRelationsSQL, schema)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: %s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []relation
	for rows.Next() {
		var r relation
		if scanErr := rows.Scan(&r.name, &r.view); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: %s scan: %w", op, scanErr)
		}
		out = append(out, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("dalgo2postgres: %s rows: %w", op, rowsErr)
	}
	return out, nil
}

// relationRefs makes a reference of each relation, or of each view only. A
// reference names schema when it is not empty, and names none otherwise (the
// form [Database.ListCollections] has always answered). It is nil when nothing
// qualifies.
func relationRefs(relations []relation, schema string, viewsOnly bool) []dal.CollectionRef {
	var out []dal.CollectionRef
	for _, r := range relations {
		if viewsOnly && !r.view {
			continue
		}
		if schema == "" {
			out = append(out, dal.NewRootCollectionRef(r.name, ""))
		} else {
			out = append(out, dal.NewQualifiedRootCollectionRef(schema, r.name, ""))
		}
	}
	return out
}

// ListSchemas returns the schemas of the database that a role can use (it has
// USAGE on them), in name order, without the system schemas: pg_catalog,
// information_schema, pg_toast and the temporary schemas of the sessions
// (pg_temp_N, pg_toast_temp_N). A schema with no tables is listed. dbschema has no
// optional interface for listing schemas, so this method belongs to this adapter
// alone.
func (d *Database) ListSchemas(ctx context.Context) ([]string, error) {
	rows, err := d.sqlDB.QueryContext(ctx, listSchemasSQL)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: ListSchemas: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: ListSchemas scan: %w", scanErr)
		}
		out = append(out, name)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("dalgo2postgres: ListSchemas rows: %w", rowsErr)
	}
	return out, nil
}

// ListSchemaCollections returns the collections of the named schema, as
// [Database.ListCollections] returns those of the configured one, except that each
// reference names its schema, so handing it back to [Database.DescribeCollection]
// or [Database.ListIndexes] reads that schema. An empty schema means the configured
// one. The name is a bound parameter; it is not folded or checked against
// [Database.ListSchemas].
func (d *Database) ListSchemaCollections(ctx context.Context, schema string) ([]dal.CollectionRef, error) {
	return d.schemaRefs(ctx, schema, false, "ListSchemaCollections")
}

// ListViews returns the collections of the configured schema that are views,
// materialized views included, by the same reference [Database.ListCollections]
// gave each (so a view is among its collections). It is the method the DataTug
// schema provider looks for to tell a view from a table, which dbschema cannot.
// A foreign table and a partitioned table are tables, not views.
func (d *Database) ListViews(ctx context.Context) ([]dal.CollectionRef, error) {
	relations, err := listRelations(ctx, d.sqlDB, d.schemaName(), "ListViews")
	if err != nil {
		return nil, err
	}
	return relationRefs(relations, "", true), nil
}

// ListSchemaViews returns the views and materialized views of the named schema,
// by the references [Database.ListSchemaCollections] gives. An empty schema means
// the configured one.
func (d *Database) ListSchemaViews(ctx context.Context, schema string) ([]dal.CollectionRef, error) {
	return d.schemaRefs(ctx, schema, true, "ListSchemaViews")
}

// schemaRefs lists the collections (or only the views) of a schema, each by a
// reference that names it.
func (d *Database) schemaRefs(ctx context.Context, schema string, viewsOnly bool, op string) ([]dal.CollectionRef, error) {
	if schema == "" {
		schema = d.schemaName()
	}
	relations, err := listRelations(ctx, d.sqlDB, schema, op)
	if err != nil {
		return nil, err
	}
	return relationRefs(relations, schema, viewsOnly), nil
}

// generatedPrefix opens the text a generated column's default is recorded as.
const generatedPrefix = "GENERATED ALWAYS AS ("

// columnDefaultOf records a column's default the only way dbschema allows, as a
// [dbschema.DefaultLiteral] whose Value is the expression's text:
//   - a generated column (is_generated ALWAYS, with its generation_expression) as
//     `GENERATED ALWAYS AS (<expression>)`, which a plain default's text never
//     begins with;
//   - a column with a default as column_default, as PostgreSQL prints it;
//   - anything else (no default, an identity column) as nil.
func columnDefaultOf(columnDefault sql.NullString, isGenerated string, generation sql.NullString) dbschema.DefaultExpr {
	if isGenerated == "ALWAYS" && generation.Valid {
		return dbschema.DefaultLiteral{Value: generatedPrefix + generation.String + ")"}
	}
	if columnDefault.Valid {
		return dbschema.DefaultLiteral{Value: columnDefault.String}
	}
	return nil
}
