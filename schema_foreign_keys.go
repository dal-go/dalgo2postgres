package dalgo2postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

func readForeignKeys(ctx context.Context, db *sql.DB, table string) ([]dbschema.ForeignKeyDef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.oid, c.conname, source_column.attname,
		       target_ns.nspname, target.relname, target_column.attname,
		       CASE c.confupdtype WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
		         WHEN 'c' THEN 'CASCADE' WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT' END,
		       CASE c.confdeltype WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
		         WHEN 'c' THEN 'CASCADE' WHEN 'n' THEN 'SET NULL' WHEN 'd' THEN 'SET DEFAULT' END
		FROM pg_catalog.pg_constraint AS c
		JOIN pg_catalog.pg_class AS source ON source.oid = c.conrelid
		JOIN pg_catalog.pg_namespace AS source_ns ON source_ns.oid = source.relnamespace
		JOIN pg_catalog.pg_class AS target ON target.oid = c.confrelid
		JOIN pg_catalog.pg_namespace AS target_ns ON target_ns.oid = target.relnamespace
		JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS source_key(attnum, position) ON true
		JOIN LATERAL unnest(c.confkey) WITH ORDINALITY AS target_key(attnum, position)
		  ON target_key.position = source_key.position
		JOIN pg_catalog.pg_attribute AS source_column
		  ON source_column.attrelid = source.oid AND source_column.attnum = source_key.attnum
		JOIN pg_catalog.pg_attribute AS target_column
		  ON target_column.attrelid = target.oid AND target_column.attnum = target_key.attnum
		WHERE c.contype = 'f' AND source_ns.nspname = 'public' AND source.relname = $1
		ORDER BY c.oid, source_key.position`, table)
	if err != nil {
		return nil, fmt.Errorf("dalgo2postgres: foreign keys for %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var keys []dbschema.ForeignKeyDef
	var lastID int64 = -1
	for rows.Next() {
		var id int64
		var name, from, targetNamespace, target, to, onUpdate, onDelete string
		if err := rows.Scan(&id, &name, &from, &targetNamespace, &target, &to, &onUpdate, &onDelete); err != nil {
			return nil, fmt.Errorf("dalgo2postgres: foreign key scan: %w", err)
		}
		if id != lastID {
			if targetNamespace == "public" {
				targetNamespace = ""
			}
			keys = append(keys, dbschema.ForeignKeyDef{
				Name: name, ReferencedCollection: target, ReferencedNamespace: targetNamespace,
				Enforcement: dbschema.ForeignKeyEnforcementUnknown,
				OnUpdate:    onUpdate, OnDelete: onDelete,
			})
			lastID = id
		}
		key := &keys[len(keys)-1]
		key.Fields = append(key.Fields, dal.FieldName(from))
		key.ReferencedFields = append(key.ReferencedFields, dal.FieldName(to))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dalgo2postgres: foreign key rows: %w", err)
	}
	return keys, nil
}
