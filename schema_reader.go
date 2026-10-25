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

// ListCollections returns the tables, views, materialized views, partitioned
// tables and foreign tables of the configured schema ([DefaultSchema] unless
// [WithSchema] says otherwise) in alphabetical order, under the names PostgreSQL
// reports, each by a reference that names no schema. The parent *dalrecord.Key is
// ignored: it does not choose a schema. [Database.ListSchemas] and
// [Database.ListSchemaCollections] read the others.
func (d *Database) ListCollections(ctx context.Context, parent *dalrecord.Key) ([]dal.CollectionRef, error) {
	_ = parent // ignored
	return guarded(d.readRefs(ctx, d.schemaName(), "", false, "ListCollections"))
}

// DescribeCollection returns the full schema definition for the named table,
// view, materialized view, partitioned table or foreign table. It queries
// information_schema for columns and primary-key membership (a materialized
// view, which information_schema does not list, from pg_attribute). The
// collection is read from the schema the reference names, else from the
// configured schema.
//
// A column's default is recorded as [dbschema.DefaultLiteral] whose Value is the
// text of the expression as PostgreSQL stores it (`'new'::text`, `now()`,
// `nextval('t_id_seq'::regclass)`); a generated column's is the text
// `GENERATED ALWAYS AS (<expression>)`, and an identity column has
// AutoIncrement set and no default. See the README for what each reads as.
// A column whose PostgreSQL type has no dbschema counterpart (uuid, json,
// jsonb, arrays, enums, interval, inet, money, ...) is reported as a String
// field rather than failing the whole table.
func (d *Database) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	schema, name, err := d.lookupNames(ref)
	if err != nil {
		return nil, leaveAdapter(err)
	}
	return guarded(describeCollectionImpl(ctx, d.sqlDB, schema, name, collectionLabel(ref.Schema(), name)))
}

// ListIndexes returns the non-primary-key indexes on the named table via
// pg_indexes, from the schema the reference names, else the configured schema.
func (d *Database) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	schema, name, err := d.lookupNames(ref)
	if err != nil {
		return nil, leaveAdapter(err)
	}
	return guarded(listIndexesImpl(ctx, d.sqlDB, schema, name, collectionLabel(ref.Schema(), name)))
}

// ---- DescribeCollection impl ----

// describeCollectionImpl is the inner reader, factored so tests can reuse it.
// label names the collection in error messages (see [collectionLabel]).
func describeCollectionImpl(ctx context.Context, db *sql.DB, schema, name string, label quotedCollection) (*dbschema.CollectionDef, error) {
	// 1. Confirm the table or view exists, and learn whether it is a materialized
	//    view: information_schema.columns does not list those.
	var kind string
	probeErr := db.QueryRowContext(ctx, relationKindSQL, schema, name).Scan(&kind)
	if probeErr == sql.ErrNoRows {
		return nil, newQualifiedCollectionNotFoundError(label)
	}
	if probeErr != nil {
		return nil, fmt.Errorf("dalgo2postgres: DescribeCollection probe %s: %w", label, probeErr)
	}
	columnsSQL := informationSchemaColumnsSQL
	if kind == relkindMaterializedView {
		columnsSQL = materializedViewColumnsSQL
	}

	// 2. Enumerate primary key columns.
	pkCols, err := listPrimaryKeyColumns(ctx, db, schema, name)
	if err != nil {
		return nil, err
	}
	pkSet := make(map[string]bool, len(pkCols))
	for _, c := range pkCols {
		pkSet[c] = true
	}

	// 3. Enumerate columns (including numeric precision/scale and character max
	//    length for proper type round-trip, and the default, identity and
	//    generation of each).
	rows, err := db.QueryContext(ctx, columnsSQL, schema, name)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: DescribeCollection columns %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var fields []dbschema.FieldDef
	var sourceColumns []dbschema.SourceColumnDef
	pkPosition := make(map[string]int, len(pkCols))
	for i, col := range pkCols {
		pkPosition[col] = i + 1
	}
	for rows.Next() {
		var (
			colName     string
			dataType    string
			udtName     string
			charMaxLen  sql.NullInt64
			numPrec     sql.NullInt64
			numScale    sql.NullInt64
			isNullable  string
			colDefault  sql.NullString
			isIdentity  string
			isGenerated string
			generation  sql.NullString
		)
		if scanErr := rows.Scan(&colName, &dataType, &udtName, &charMaxLen, &numPrec, &numScale, &isNullable,
			&colDefault, &isIdentity, &isGenerated, &generation); scanErr != nil {
			return nil, fmt.Errorf("dalgo2postgres: DescribeCollection column scan: %w", scanErr)
		}

		t, _, length := dbschemaTypeFromPostgres(dataType, udtName)

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
			Default:   columnDefaultOf(colDefault, isGenerated, generation),
			// An identity column is told by the catalog; a column that only defaults
			// to nextval() is a plain default and is not marked.
			AutoIncrement: isIdentity == "YES",
		}
		fields = append(fields, f)
		sourceColumns = append(sourceColumns, dbschema.SourceColumnDef{
			Name:               colName,
			DeclaredType:       dataType,
			NotNull:            isNullable == "NO",
			PrimaryKeyPosition: pkPosition[colName],
		})
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("dalgo2postgres: DescribeCollection rows: %w", rowsErr)
	}

	// 4. Build PrimaryKey slice in ordinal order.
	pk := make([]dal.FieldName, len(pkCols))
	for i, c := range pkCols {
		pk[i] = dal.FieldName(c)
	}

	indexes, err := listIndexesImpl(ctx, db, schema, name, label)
	if err != nil {
		return nil, err
	}
	foreignKeys, err := readForeignKeys(ctx, db, schema, name)
	if err != nil {
		return nil, err
	}

	return &dbschema.CollectionDef{
		Name:             name,
		Fields:           fields,
		PrimaryKey:       pk,
		Indexes:          indexes,
		ForeignKeys:      foreignKeys,
		SourceDefinition: &dbschema.SourceDefinition{Dialect: "postgres", Columns: sourceColumns},
	}, nil
}

// listPrimaryKeyColumns returns the primary key column names in key ordinal order.
func listPrimaryKeyColumns(ctx context.Context, db *sql.DB, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT a.attname::text
		 FROM pg_catalog.pg_constraint AS con
		 JOIN pg_catalog.pg_class AS c ON c.oid = con.conrelid
		 JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		 JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS key_column(attnum, position) ON true
		 JOIN pg_catalog.pg_attribute AS a
		   ON a.attrelid = c.oid AND a.attnum = key_column.attnum
		 WHERE con.contype = 'p'
		   AND n.nspname = $1::text
		   AND c.relname = $2::text
		   AND `+relationVisibleSQL+`
		 ORDER BY key_column.position`,
		schema, table,
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
// label names the collection in error messages (see [collectionLabel]).
func listIndexesImpl(ctx context.Context, db *sql.DB, schema, name string, label quotedCollection) ([]dbschema.IndexDef, error) {
	// pg_indexes holds precomputed indexdef DDL. We exclude indexes that are
	// the backing store for PRIMARY KEY constraints by joining to pg_constraint.
	rows, err := db.QueryContext(ctx,
		`SELECT i.indexname, i.indexdef
		 FROM pg_catalog.pg_indexes i
		 WHERE i.schemaname = $1::text
		   AND i.tablename  = $2::text
		   AND EXISTS (
		       SELECT 1 FROM pg_catalog.pg_class AS c
		       JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		       WHERE n.nspname = i.schemaname
		         AND c.relname = i.tablename
		         AND `+relationVisibleSQL+`
		   )
		   AND NOT EXISTS (
		       SELECT 1 FROM pg_catalog.pg_constraint c
		       JOIN pg_catalog.pg_class t ON t.oid = c.conrelid
		       JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
		       WHERE c.contype = 'p'
		         AND c.connamespace = t.relnamespace
		         AND c.conname = i.indexname
		         AND t.relname = i.tablename
		         AND n.nspname = i.schemaname
		   )
		 ORDER BY i.indexname`,
		schema, name,
	)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: listIndexesImpl %s: %w", label, err)
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
