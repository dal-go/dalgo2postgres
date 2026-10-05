package dalgo2postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dal-go/dalgo/dal"
)

// nonDeterministicTextColumnsSQL lists the live columns of one table or view
// whose type is citext or whose collation is non-deterministic. The schema and
// the table name are bound parameters; neither is ever part of this text.
//
// A column qualifies when
//   - its type is citext (the extension may live in any schema, so the type is
//     matched by name), or
//   - its collation is non-deterministic (pg_collation.collisdeterministic is
//     false; the column exists from PostgreSQL 12, the release that added
//     non-deterministic collations).
//
// A column of the database default collation is deterministic, so a plain text
// table yields nothing. Dropped and system columns are skipped.
//
// The columns are LEFT JOINed to the relation, so a relation that exists but has
// no qualifying column still yields one row whose attname is NULL, while a
// relation that does not exist yields no row. That is how one statement tells
// "exact equality" from "no such table". Only tables, partitioned tables,
// views, materialized views and foreign tables count as relations; an index or
// composite type of the same name does not.
const nonDeterministicTextColumnsSQL = `
	SELECT a.attname
	FROM pg_catalog.pg_class AS c
	JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
	LEFT JOIN (
		pg_catalog.pg_attribute AS a
		JOIN pg_catalog.pg_type AS t ON t.oid = a.atttypid
		LEFT JOIN pg_catalog.pg_collation AS co ON co.oid = a.attcollation
	) ON a.attrelid = c.oid
	  AND a.attnum > 0 AND NOT a.attisdropped
	  AND (t.typname = 'citext' OR co.collisdeterministic = false)
	WHERE n.nspname = $1 AND c.relname = $2
	  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
	ORDER BY a.attnum`

// NonDeterministicTextColumns returns the columns of the referenced table (or
// view) whose equality is not exact: columns of type citext and columns with a
// non-deterministic collation (for example a case-insensitive ICU collation),
// in column order. Under such a column "A" = "a" can be true, so a comparison
// or a group-by on it does not mean what a byte-for-byte match means; callers
// use this list to know where a pushed-down comparison differs from Go's.
//
// The table is looked up in the schema the reference names
// ([dal.NewQualifiedRootCollectionRef]), else in the Database's schema
// ([DefaultSchema] unless [WithSchema] says otherwise), and its name is matched
// as the schema reader matches it (see [IdentifierMode]). Both are sent as bound
// parameters. A table with no such column yields an empty non-nil slice. A table
// that does not exist (under the name as [IdentifierMode] resolves it) yields a
// "not found" error, never an empty list, so an empty result means the table
// exists and none of its columns is inexact in the sense below.
//
// The match is on the column's own type and collation. A column whose type is a
// domain over citext, or an array of citext, is not reported; treat such
// columns as inexact yourself.
func (d *Database) NonDeterministicTextColumns(ctx context.Context, ref *dal.CollectionRef) ([]string, error) {
	name := d.resolveName(ref.Name())
	return guarded(nonDeterministicTextColumns(ctx, d.sqlDB, d.schemaFor(ref), name, collectionLabel(ref.Schema(), name)))
}

// nonDeterministicTextColumns is the reader behind
// [Database.NonDeterministicTextColumns], taking the already resolved schema and
// table name so tests reach it with a mocked handle. label names the table in
// error messages (see [collectionLabel]).
func nonDeterministicTextColumns(ctx context.Context, db *sql.DB, schema, table string, label quotedCollection) ([]string, error) {
	rows, err := db.QueryContext(ctx, nonDeterministicTextColumnsSQL, schema, table)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: non-deterministic text columns of %s: %w", label, err)
	}
	defer func() { _ = rows.Close() }()
	columns := []string{}
	tableExists := false
	for rows.Next() {
		var name sql.NullString
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: non-deterministic text columns of %s scan: %w", label, scanErr)
		}
		tableExists = true
		if name.Valid {
			columns = append(columns, name.String)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("dalgo2postgres: non-deterministic text columns of %s rows: %w", label, rowsErr)
	}
	if !tableExists {
		return nil, newQualifiedCollectionNotFoundError(label)
	}
	return columns, nil
}
