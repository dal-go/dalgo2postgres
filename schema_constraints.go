package dalgo2postgres

import (
	"context"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// ListConstraints returns a best-effort survey of constraints on the table via
// information_schema:
//   - PRIMARY KEY constraint (one row if any PK columns exist)
//   - UNIQUE constraints
//   - FOREIGN KEY constraints
//
// CHECK constraints are NOT enumerated here; read them from DescribeCollection
// if needed.
func (d *Database) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT constraint_name, constraint_type
		 FROM information_schema.table_constraints
		 WHERE table_schema = $1
		   AND table_name   = $2
		   AND constraint_type IN ('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY')
		 ORDER BY constraint_type, constraint_name`,
		d.schemaName(), d.resolveName(ref.Name()),
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: ListConstraints %q: %w", ref.Name(), err)
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

// ListReferrers returns the collections that reference ref via foreign keys,
// with the referencing columns of each. Only referrers in the configured schema
// are reported, because the reader addresses a single schema. It reads
// pg_catalog rather than information_schema: constraint names are unique only
// per table in PostgreSQL, so information_schema joins on the name alone mix up
// tables that reuse a name.
func (d *Database) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT DISTINCT source.relname AS referrer_table, source_column.attname AS referrer_col
		 FROM pg_catalog.pg_constraint AS c
		 JOIN pg_catalog.pg_class AS source ON source.oid = c.conrelid
		 JOIN pg_catalog.pg_namespace AS source_ns ON source_ns.oid = source.relnamespace
		 JOIN pg_catalog.pg_class AS target ON target.oid = c.confrelid
		 JOIN pg_catalog.pg_namespace AS target_ns ON target_ns.oid = target.relnamespace
		 JOIN LATERAL unnest(c.conkey) AS source_key(attnum) ON true
		 JOIN pg_catalog.pg_attribute AS source_column
		   ON source_column.attrelid = source.oid AND source_column.attnum = source_key.attnum
		 WHERE c.contype = 'f'
		   AND source_ns.nspname = $1
		   AND target_ns.nspname = $1
		   AND target.relname    = $2
		 ORDER BY referrer_table, referrer_col`,
		d.schemaName(), d.resolveName(ref.Name()),
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: ListReferrers %q: %w", ref.Name(), err)
	}
	defer func() { _ = rows.Close() }()

	byTable := make(map[string][]dal.FieldName)
	var order []string
	for rows.Next() {
		var tbl, col string
		if scanErr := rows.Scan(&tbl, &col); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: ListReferrers scan: %w", scanErr)
		}
		if _, ok := byTable[tbl]; !ok {
			order = append(order, tbl)
		}
		byTable[tbl] = append(byTable[tbl], dal.FieldName(col))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]dbschema.Referrer, 0, len(order))
	for _, tbl := range order {
		out = append(out, dbschema.Referrer{
			Collection: dal.NewRootCollectionRef(tbl, ""),
			Fields:     byTable[tbl],
		})
	}
	return out, nil
}
