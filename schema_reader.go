package dalgo2postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	dalrecord "github.com/dal-go/record"
)

// ListCollections returns user-defined tables in the current schema ('public')
// in alphabetical order. The parent *dalrecord.Key is ignored — Postgres has a flat
// table namespace within a schema.
func (d *Database) ListCollections(ctx context.Context, parent *dalrecord.Key) ([]dal.CollectionRef, error) {
	_ = parent // ignored
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT table_name
		 FROM information_schema.tables
		 WHERE table_schema = 'public'
		   AND table_type = 'BASE TABLE'
		 ORDER BY table_name`,
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: ListCollections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []dal.CollectionRef
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: ListCollections scan: %w", scanErr)
		}
		out = append(out, dal.NewRootCollectionRef(name, ""))
	}
	return out, rows.Err()
}

// DescribeCollection returns the full schema definition for the named table.
// It queries information_schema for columns and primary-key membership.
func (d *Database) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	return describeCollectionImpl(ctx, d.sqlDB, strings.ToLower(ref.Name()))
}

// ListIndexes returns the non-primary-key indexes on the named table via pg_indexes.
func (d *Database) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return listIndexesImpl(ctx, d.sqlDB, strings.ToLower(ref.Name()))
}

// ---- DescribeCollection impl ----

// describeCollectionImpl is the inner reader, factored so tests can reuse it.
func describeCollectionImpl(ctx context.Context, db *sql.DB, name string) (*dbschema.CollectionDef, error) {
	// 1. Confirm the table exists.
	var found string
	probeErr := db.QueryRowContext(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name = $1`,
		name,
	).Scan(&found)
	if probeErr == sql.ErrNoRows {
		return nil, newCollectionNotFoundError(name)
	}
	if probeErr != nil {
		return nil, fmt.Errorf("dalgo2postgres: DescribeCollection probe %q: %w", name, probeErr)
	}

	// 2. Enumerate primary key columns.
	pkCols, err := listPrimaryKeyColumns(ctx, db, name)
	if err != nil {
		return nil, err
	}
	pkSet := make(map[string]bool, len(pkCols))
	for _, c := range pkCols {
		pkSet[c] = true
	}

	// 3. Enumerate columns from information_schema (including numeric precision/scale
	//    and character max length for proper type round-trip).
	rows, err := db.QueryContext(ctx,
		`SELECT column_name, data_type, udt_name,
		        character_maximum_length, numeric_precision, numeric_scale,
		        is_nullable
		 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1
		 ORDER BY ordinal_position`,
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: DescribeCollection columns %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var fields []dbschema.FieldDef
	for rows.Next() {
		var (
			colName    string
			dataType   string
			udtName    string
			charMaxLen sql.NullInt64
			numPrec    sql.NullInt64
			numScale   sql.NullInt64
			isNullable string
		)
		if scanErr := rows.Scan(&colName, &dataType, &udtName, &charMaxLen, &numPrec, &numScale, &isNullable); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: DescribeCollection column scan: %w", scanErr)
		}

		t, _, length, ok := dbschemaTypeFromPostgres(dataType, udtName)
		if !ok {
			return nil, &dbschema.NotSupportedError{
				Op:      "DescribeCollection",
				Backend: "dalgo2postgres",
				Reason:  fmt.Sprintf("column %q has unrecognized Postgres type %q (udt_name=%q)", colName, dataType, udtName),
			}
		}

		// Derive precision from numeric_precision/scale columns — more reliable than
		// parsing data_type strings which may omit the (p,s) suffix for NUMERIC.
		var precision *dbschema.Precision
		if numPrec.Valid && numScale.Valid && t == dbschema.Decimal {
			precision = &dbschema.Precision{
				Total: int(numPrec.Int64),
				Scale: int(numScale.Int64),
			}
		}

		var fieldLength *int
		if length != nil {
			fieldLength = length
		} else if charMaxLen.Valid {
			n := int(charMaxLen.Int64)
			fieldLength = &n
		}

		nullable := isNullable == "YES" && !pkSet[colName]
		f := dbschema.FieldDef{
			Name:      dal.FieldName(colName),
			Type:      t,
			Precision: precision,
			Length:    fieldLength,
			Nullable:  nullable,
		}
		fields = append(fields, f)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("dalgo2postgres: DescribeCollection rows: %w", rowsErr)
	}

	// 4. Build PrimaryKey slice in ordinal order.
	pk := make([]dal.FieldName, len(pkCols))
	for i, c := range pkCols {
		pk[i] = dal.FieldName(c)
	}

	indexes, err := listIndexesImpl(ctx, db, name)
	if err != nil {
		return nil, err
	}
	foreignKeys, err := readForeignKeys(ctx, db, name)
	if err != nil {
		return nil, err
	}

	return &dbschema.CollectionDef{
		Name:        name,
		Fields:      fields,
		PrimaryKey:  pk,
		Indexes:     indexes,
		ForeignKeys: foreignKeys,
	}, nil
}

// listPrimaryKeyColumns returns the primary key column names in key ordinal order.
func listPrimaryKeyColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT kcu.column_name
		 FROM information_schema.table_constraints tc
		 JOIN information_schema.key_column_usage kcu
		   ON tc.constraint_name = kcu.constraint_name
		  AND tc.table_schema    = kcu.table_schema
		  AND tc.table_name      = kcu.table_name
		 WHERE tc.constraint_type = 'PRIMARY KEY'
		   AND tc.table_schema    = 'public'
		   AND tc.table_name      = $1
		 ORDER BY kcu.ordinal_position`,
		table,
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: listPrimaryKeyColumns %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var col string
		if scanErr := rows.Scan(&col); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: listPrimaryKeyColumns scan: %w", scanErr)
		}
		out = append(out, col)
	}
	return out, rows.Err()
}

// ---- ListIndexes impl ----

// listIndexesImpl returns non-primary-key indexes on the table via pg_indexes.
func listIndexesImpl(ctx context.Context, db *sql.DB, name string) ([]dbschema.IndexDef, error) {
	// pg_indexes holds precomputed indexdef DDL. We exclude indexes that are
	// the backing store for PRIMARY KEY constraints by joining to pg_constraint.
	rows, err := db.QueryContext(ctx,
		`SELECT i.indexname, i.indexdef
		 FROM pg_indexes i
		 WHERE i.schemaname = 'public'
		   AND i.tablename  = $1
		   AND NOT EXISTS (
		       SELECT 1 FROM pg_constraint c
		       JOIN pg_class t ON t.oid = c.conrelid
		       WHERE c.contype = 'p'
		         AND c.conname = i.indexname
		         AND t.relname = i.tablename
		   )
		 ORDER BY i.indexname`,
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: listIndexesImpl %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var out []dbschema.IndexDef
	for rows.Next() {
		var (
			ixName string
			ixDef  string
		)
		if scanErr := rows.Scan(&ixName, &ixDef); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: listIndexesImpl scan: %w", scanErr)
		}
		unique := isUniqueIndexDef(ixDef)
		fields := parseIndexDefFields(ixDef)
		out = append(out, dbschema.IndexDef{
			Name:       ixName,
			Collection: name,
			Fields:     fields,
			Unique:     unique,
		})
	}
	return out, rows.Err()
}

// isUniqueIndexDef reports whether the pg_indexes.indexdef string describes a
// UNIQUE INDEX.
func isUniqueIndexDef(def string) bool {
	// indexdef starts with "CREATE UNIQUE INDEX ..." or "CREATE INDEX ..."
	for i := 0; i+7 <= len(def); i++ {
		if def[i] == 'U' || def[i] == 'u' {
			if len(def) > i+6 {
				word := def[i : i+6]
				if word == "UNIQUE" || word == "unique" {
					return true
				}
			}
		}
	}
	return false
}

// parseIndexDefFields extracts the column list from a pg_indexes.indexdef like:
//
//	CREATE [UNIQUE] INDEX name ON table USING btree (col1, col2)
//
// Returns a minimal slice — sufficient for round-trip tests.  Complex
// expressions (functions, collations, sort orders) are left as-is in the
// field name string; callers that need exact SQL should query pg_attribute
// directly.
func parseIndexDefFields(def string) []dal.FieldName {
	open := lastIndexByte(def, '(')
	close := lastIndexByte(def, ')')
	if open < 0 || close < 0 || close <= open {
		return nil
	}
	inner := def[open+1 : close]
	parts := splitAndTrim(inner, ',')
	out := make([]dal.FieldName, 0, len(parts))
	for _, p := range parts {
		// Strip optional direction keywords and double-quotes.
		p = trimSuffix(p, " ASC")
		p = trimSuffix(p, " DESC")
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, dal.FieldName(p))
		}
	}
	return out
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func splitAndTrim(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	// Case-insensitive version
	ls, lsuf := strings.ToUpper(s), strings.ToUpper(suffix)
	if len(ls) >= len(lsuf) && ls[len(ls)-len(lsuf):] == lsuf {
		return s[:len(s)-len(suffix)]
	}
	return s
}
