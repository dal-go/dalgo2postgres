package dalgo2postgres

import (
	"context"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// ListConstraints returns a best-effort survey of constraints on the table via
// information_schema, in the schema the reference names, else the configured
// schema:
//   - PRIMARY KEY constraint (one row if any PK columns exist)
//   - UNIQUE constraints
//   - FOREIGN KEY constraints
//
// CHECK constraints are NOT enumerated here; read them from DescribeCollection
// if needed.
func (d *Database) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return guarded(d.listConstraints(ctx, ref))
}

// listConstraints is the reader behind [Database.ListConstraints].
func (d *Database) listConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	schema, name, err := d.lookupNames(ref)
	if err != nil {
		return nil, err
	}
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT constraint_name, constraint_type
		 FROM information_schema.table_constraints
		 WHERE table_schema = $1::text
		   AND table_name   = $2::text
		   AND constraint_type IN ('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY')
		 ORDER BY constraint_type, constraint_name`,
		schema, name,
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: ListConstraints %s: %w", collectionLabel(ref.Schema(), ref.Name()), err)
	}
	defer func() { _ = rows.Close() }()

	var out []dbschema.ConstraintDef
	for rows.Next() {
		var name, ctype string
		if scanErr := rows.Scan(&name, &ctype); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: ListConstraints scan: %w", scanErr)
		}
		var ct string
		switch ctype {
		case "PRIMARY KEY":
			ct = "primary-key"
		case "UNIQUE":
			ct = "unique"
		case "FOREIGN KEY":
			ct = "foreign-key"
		default:
			ct = ctype
		}
		out = append(out, dbschema.ConstraintDef{Name: name, Type: ct})
	}
	return out, rows.Err()
}

// ListReferrers returns the foreign keys that reference ref, one Referrer per
// foreign key, with the referencing columns in key order. Only referrers in the
// schema of ref (the schema the reference names, else the configured one) are
// reported, because the reader addresses a single schema. It reads pg_catalog
// rather than information_schema: constraint names are unique only per table in
// PostgreSQL, so information_schema joins on the name alone mix up tables that
// reuse a name.
func (d *Database) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return guarded(d.listReferrers(ctx, ref))
}

// listReferrers is the reader behind [Database.ListReferrers].
func (d *Database) listReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	schema, name, err := d.lookupNames(ref)
	if err != nil {
		return nil, err
	}
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT c.oid, source.relname AS referrer_table, source_column.attname AS referrer_col
		 FROM pg_catalog.pg_constraint AS c
		 JOIN pg_catalog.pg_class AS source ON source.oid = c.conrelid
		 JOIN pg_catalog.pg_namespace AS source_ns ON source_ns.oid = source.relnamespace
		 JOIN pg_catalog.pg_class AS target ON target.oid = c.confrelid
		 JOIN pg_catalog.pg_namespace AS target_ns ON target_ns.oid = target.relnamespace
		 JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS source_key(attnum, position) ON true
		 JOIN pg_catalog.pg_attribute AS source_column
		   ON source_column.attrelid = source.oid AND source_column.attnum = source_key.attnum
		 WHERE c.contype = 'f'
		   AND source_ns.nspname = $1::text
		   AND target_ns.nspname = $1::text
		   AND target.relname    = $2::text
		 ORDER BY source.relname, c.conname, c.oid, source_key.position`,
		schema, name,
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: ListReferrers %s: %w", collectionLabel(ref.Schema(), ref.Name()), err)
	}
	defer func() { _ = rows.Close() }()

	var out []dbschema.Referrer
	var lastID int64 = -1
	for rows.Next() {
		var id int64
		var tbl, col string
		if scanErr := rows.Scan(&id, &tbl, &col); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: ListReferrers scan: %w", scanErr)
		}
		if id != lastID {
			out = append(out, dbschema.Referrer{Collection: referrerRef(ref, tbl)})
			lastID = id
		}
		referrer := &out[len(out)-1]
		referrer.Fields = append(referrer.Fields, dal.FieldName(col))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []dbschema.Referrer{}
	}
	return out, nil
}

// referrerRef is the collection reference of a referencing table. Referrers
// are read from the schema of the queried reference, so when that reference
// named its schema the referrer names it too: passed back to the reader, it
// addresses the table that was found, not a same-named table in the configured
// schema.
func referrerRef(queried *dal.CollectionRef, table string) dal.CollectionRef {
	if schema := queried.Schema(); schema != "" {
		return dal.NewQualifiedRootCollectionRef(schema, table, "")
	}
	return dal.NewRootCollectionRef(table, "")
}
