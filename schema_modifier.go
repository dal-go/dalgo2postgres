package dalgo2postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
)

// CreateCollection creates a table and its inline indexes transactionally.
// On any error, the transaction rolls back and no schema state remains.
func (d *Database) CreateCollection(ctx context.Context, c dbschema.CollectionDef, opts ...ddl.Option) error {
	return leaveAdapter(d.createCollection(ctx, c, opts...))
}

// createCollection is the body of [Database.CreateCollection].
func (d *Database) createCollection(ctx context.Context, c dbschema.CollectionDef, opts ...ddl.Option) error {
	o := ddl.ResolveOptions(opts...)
	createSQL, err := buildCreateTableSQL(c, o)
	if err != nil {
		return err
	}
	indexSQLs := make([]string, 0, len(c.Indexes))
	for _, idx := range c.Indexes {
		if idx.Collection == "" {
			idx.Collection = c.Name
		}
		s, ierr := buildCreateIndexSQL(idx, o)
		if ierr != nil {
			return ierr
		}
		indexSQLs = append(indexSQLs, s)
	}

	return d.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, createSQL); err != nil {
			return fmt.Errorf("dalgo2postgres: CreateCollection exec %q: %w", createSQL, err)
		}
		for _, s := range indexSQLs {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("dalgo2postgres: CreateCollection index exec %q: %w", s, err)
			}
		}
		return nil
	})
}

func (d *Database) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dalgo2postgres: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dalgo2postgres: commit tx: %w", err)
	}
	return nil
}

// DropCollection drops the table and all its indexes (Postgres cascades automatically).
func (d *Database) DropCollection(ctx context.Context, name string, opts ...ddl.Option) error {
	o := ddl.ResolveOptions(opts...)
	sqlStmt := buildDropTableSQL(name, o)
	return leaveAdapter(d.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, sqlStmt); err != nil {
			return fmt.Errorf("dalgo2postgres: DropCollection exec: %w", err)
		}
		return nil
	}))
}

// AlterCollection applies ops in order inside a single transaction.
// Partial failures roll back and leave the collection untouched.
func (d *Database) AlterCollection(ctx context.Context, name string, ops ...ddl.AlterOp) error {
	return leaveAdapter(d.inTx(ctx, func(tx *sql.Tx) error {
		a := &postgresAlterApplier{ctx: ctx, tx: tx, table: name}
		for _, op := range ops {
			if err := op.ApplyTo(ctx, a); err != nil {
				return err
			}
		}
		return nil
	}))
}

// postgresAlterApplier implements ddl.Applier for the in-flight
// AlterCollection transaction. One instance per AlterCollection call.
// All ApplyXxx methods run against the same *sql.Tx — rollback on any error
// undoes the whole batch.
type postgresAlterApplier struct {
	ctx   context.Context
	tx    *sql.Tx
	table string
}

func (a *postgresAlterApplier) ApplyAddField(ctx context.Context, f dbschema.FieldDef, opts ddl.Options) error {
	sqlStmt, err := buildAlterTableAddColumnSQL(a.table, f)
	if err != nil {
		return err
	}
	if _, err := a.tx.ExecContext(ctx, sqlStmt); err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyAddField %q: %w", f.Name, err)
	}
	return nil
}

func (a *postgresAlterApplier) ApplyDropField(ctx context.Context, name dal.FieldName, opts ddl.Options) error {
	sqlStmt := buildAlterTableDropColumnSQL(a.table, name)
	if _, err := a.tx.ExecContext(ctx, sqlStmt); err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyDropField %q: %w", name, err)
	}
	return nil
}

func (a *postgresAlterApplier) ApplyRenameField(ctx context.Context, oldName, newName dal.FieldName, opts ddl.Options) error {
	sqlStmt := buildAlterTableRenameColumnSQL(a.table, oldName, newName)
	if _, err := a.tx.ExecContext(ctx, sqlStmt); err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyRenameField %q->%q: %w", oldName, newName, err)
	}
	return nil
}

func (a *postgresAlterApplier) ApplyAddIndex(ctx context.Context, idx dbschema.IndexDef, opts ddl.Options) error {
	if idx.Collection == "" {
		idx.Collection = a.table
	}
	sqlStmt, err := buildCreateIndexSQL(idx, opts)
	if err != nil {
		return err
	}
	if _, err := a.tx.ExecContext(ctx, sqlStmt); err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyAddIndex %q: %w", idx.Name, err)
	}
	return nil
}

func (a *postgresAlterApplier) ApplyDropIndex(ctx context.Context, name string, opts ddl.Options) error {
	sqlStmt := buildDropIndexSQL(name, opts)
	if _, err := a.tx.ExecContext(ctx, sqlStmt); err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyDropIndex %q: %w", name, err)
	}
	return nil
}

// ApplyModifyField alters a column's type in PostgreSQL using ALTER COLUMN … TYPE.
// PostgreSQL supports direct type alteration (unlike SQLite's create-new/copy/drop dance).
func (a *postgresAlterApplier) ApplyModifyField(ctx context.Context, name dal.FieldName, newDef dbschema.FieldDef, opts ddl.Options) error {
	if err := validateIdent(a.table); err != nil {
		return err
	}
	if err := validateIdent(string(name)); err != nil {
		return err
	}
	sqlType, err := postgresTypeFor(newDef)
	if err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyModifyField %q: %w", name, err)
	}
	stmt := fmt.Sprintf(
		"ALTER TABLE %s ALTER COLUMN %s TYPE %s",
		quoteIdent(a.table), quoteIdent(string(name)), sqlType,
	)
	if _, err := a.tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyModifyField %q: %w", name, err)
	}
	// Update nullability separately if needed.
	var nullStmt string
	if newDef.Nullable {
		nullStmt = fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL",
			quoteIdent(a.table), quoteIdent(string(name)))
	} else {
		nullStmt = fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL",
			quoteIdent(a.table), quoteIdent(string(name)))
	}
	if _, err := a.tx.ExecContext(ctx, nullStmt); err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyModifyField nullability %q: %w", name, err)
	}
	return nil
}
