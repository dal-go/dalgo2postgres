package dalgo2postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
)

// Every call that creates, alters or drops builds all of its statements before
// its transaction begins, and every name a statement writes passes [checkName] on
// the way: a call with a name that is not a plain identifier of at most 63 bytes
// is an error that matches dalgo2sql.ErrUnsafeName, and nothing is sent, not even
// the beginning of a transaction.

// ddlStep is one statement of a call that creates, alters or drops. what is the
// words an error of the statement is reported under.
type ddlStep struct {
	statement string
	what      string
}

// execSteps runs the statements of one call, in order, in one transaction: an error
// of any rolls the transaction back.
func (d *Database) execSteps(ctx context.Context, steps []ddlStep) error {
	return d.inTx(ctx, func(tx *sql.Tx) error {
		for _, s := range steps {
			if _, err := tx.ExecContext(ctx, s.statement); err != nil {
				return fmt.Errorf("dalgo2postgres: %s: %w", s.what, err)
			}
		}
		return nil
	})
}

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
	steps := []ddlStep{{createSQL, fmt.Sprintf("CreateCollection exec %q", createSQL)}}
	for _, idx := range c.Indexes {
		if idx.Collection == "" {
			idx.Collection = c.Name
		}
		s, ierr := buildCreateIndexSQL(idx, o)
		if ierr != nil {
			return ierr
		}
		steps = append(steps, ddlStep{s, fmt.Sprintf("CreateCollection index exec %q", s)})
	}
	return d.execSteps(ctx, steps)
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
	table, err := checkName(positionCollection, name)
	if err != nil {
		return leaveAdapter(err)
	}
	o := ddl.ResolveOptions(opts...)
	return leaveAdapter(d.execSteps(ctx, []ddlStep{{buildDropTableSQL(table, o), "DropCollection exec"}}))
}

// AlterCollection applies ops in order inside a single transaction.
// Partial failures roll back and leave the collection untouched.
func (d *Database) AlterCollection(ctx context.Context, name string, ops ...ddl.AlterOp) error {
	return leaveAdapter(d.alterCollection(ctx, name, ops))
}

// alterCollection is the body of [Database.AlterCollection]. The table is checked,
// and the statement of every operation is built, before the transaction begins.
func (d *Database) alterCollection(ctx context.Context, name string, ops []ddl.AlterOp) error {
	table, err := checkName(positionCollection, name)
	if err != nil {
		return err
	}
	a := &postgresAlterApplier{table: table}
	for _, op := range ops {
		if err := op.ApplyTo(ctx, a); err != nil {
			return err
		}
	}
	return d.execSteps(ctx, a.steps)
}

// postgresAlterApplier implements ddl.Applier for one AlterCollection call: each
// ApplyXxx method builds the statements of its operation and collects them. None
// is sent until every operation of the call has been built, so one operation
// that is refused refuses the whole call. One instance per AlterCollection call.
type postgresAlterApplier struct {
	table checkedName
	steps []ddlStep
}

func (a *postgresAlterApplier) add(statement, what string) {
	a.steps = append(a.steps, ddlStep{statement, what})
}

func (a *postgresAlterApplier) ApplyAddField(ctx context.Context, f dbschema.FieldDef, opts ddl.Options) error {
	sqlStmt, err := buildAlterTableAddColumnSQL(a.table, f)
	if err != nil {
		return err
	}
	a.add(sqlStmt, fmt.Sprintf("ApplyAddField %q", f.Name))
	return nil
}

func (a *postgresAlterApplier) ApplyDropField(ctx context.Context, name dal.FieldName, opts ddl.Options) error {
	col, err := checkName(positionField, string(name))
	if err != nil {
		return err
	}
	a.add(buildAlterTableDropColumnSQL(a.table, col), fmt.Sprintf("ApplyDropField %q", name))
	return nil
}

func (a *postgresAlterApplier) ApplyRenameField(ctx context.Context, oldName, newName dal.FieldName, opts ddl.Options) error {
	from, err := checkName(positionField, string(oldName))
	if err != nil {
		return err
	}
	to, err := checkName(positionField, string(newName))
	if err != nil {
		return err
	}
	a.add(buildAlterTableRenameColumnSQL(a.table, from, to), fmt.Sprintf("ApplyRenameField %q->%q", oldName, newName))
	return nil
}

func (a *postgresAlterApplier) ApplyAddIndex(ctx context.Context, idx dbschema.IndexDef, opts ddl.Options) error {
	if idx.Collection == "" {
		idx.Collection = a.table.String()
	}
	sqlStmt, err := buildCreateIndexSQL(idx, opts)
	if err != nil {
		return err
	}
	a.add(sqlStmt, fmt.Sprintf("ApplyAddIndex %q", idx.Name))
	return nil
}

func (a *postgresAlterApplier) ApplyDropIndex(ctx context.Context, name string, opts ddl.Options) error {
	index, err := checkName(positionIndex, name)
	if err != nil {
		return err
	}
	a.add(buildDropIndexSQL(index, opts), fmt.Sprintf("ApplyDropIndex %q", name))
	return nil
}

// ApplyModifyField alters a column's type in PostgreSQL using ALTER COLUMN … TYPE.
// PostgreSQL supports direct type alteration (unlike SQLite's create-new/copy/drop dance).
func (a *postgresAlterApplier) ApplyModifyField(ctx context.Context, name dal.FieldName, newDef dbschema.FieldDef, opts ddl.Options) error {
	col, err := checkName(positionField, string(name))
	if err != nil {
		return err
	}
	sqlType, err := postgresTypeFor(newDef)
	if err != nil {
		return fmt.Errorf("dalgo2postgres: ApplyModifyField %q: %w", name, err)
	}
	a.add(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s", a.table.quoted(), col.quoted(), sqlType),
		fmt.Sprintf("ApplyModifyField %q", name))
	// Update nullability separately if needed.
	nullability := "SET NOT NULL"
	if newDef.Nullable {
		nullability = "DROP NOT NULL"
	}
	a.add(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s %s", a.table.quoted(), col.quoted(), nullability),
		fmt.Sprintf("ApplyModifyField nullability %q", name))
	return nil
}
