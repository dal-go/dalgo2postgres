package dalgo2postgres

import (
	"context"
	"fmt"
	"strings"

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
		 WHERE table_schema = 'public'
		   AND table_name   = $1
		   AND constraint_type IN ('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY')
		 ORDER BY constraint_type, constraint_name`,
		strings.ToLower(ref.Name()),
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

// ListReferrers returns the collections that reference ref via foreign keys.
// It queries information_schema.referential_constraints for tables that have
// a FOREIGN KEY pointing to the named table.
func (d *Database) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT DISTINCT ccu.table_name AS referrer_table, kcu.column_name AS referrer_col
		 FROM information_schema.referential_constraints rc
		 JOIN information_schema.key_column_usage kcu
		   ON kcu.constraint_name = rc.constraint_name
		  AND kcu.table_schema    = rc.constraint_schema
		 JOIN information_schema.constraint_column_usage ccu
		   ON ccu.constraint_name = rc.unique_constraint_name
		  AND ccu.table_schema    = rc.unique_constraint_schema
		 WHERE rc.constraint_schema    = 'public'
		   AND ccu.table_name          = $1
		 ORDER BY referrer_table, referrer_col`,
		strings.ToLower(ref.Name()),
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
