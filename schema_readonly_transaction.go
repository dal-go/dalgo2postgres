package dalgo2postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"
)

type catalogQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (d *Database) catalogQueries() catalogQueryer {
	if d.catalog != nil {
		return d.catalog
	}
	return d.sqlDB
}

var errSchemaReadonly = errors.New("PostgreSQL read-only schema transaction could not be verified")
var errSchemaClosed = errors.New("PostgreSQL schema transaction is closed")

// RunReadonlySchemaTransaction runs catalog reads on one verified native
// read-only transaction, always rolled back. It never retries in read-write
// mode. The supplied narrow reader retains configured identifier/schema rules
// and optional catalog capabilities, but no DB, raw SQL, DDL or Close methods.
// Reader calls inherit the original lifetime even if passed Background; the
// reader is invalidated before rollback when worker returns, panics or exits.
// This verifies native mode, not caller permissions or credential privileges.
func (d *Database) RunReadonlySchemaTransaction(ctx context.Context, worker func(context.Context, dbschema.SchemaReader) error) (err error) {
	if d == nil || d.sqlDB == nil || ctx == nil || worker == nil {
		return errSchemaReadonly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	txCtx, txCancel := context.WithCancel(ctx)
	defer txCancel()
	tx, err := d.sqlDB.BeginTx(txCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return leaveAdapter(err)
	}
	var view *readonlySchemaView
	defer func() {
		if view != nil {
			view.closed.Store(true)
			view.cancel()
		}
		rollbackErr := tx.Rollback()
		// database/sql may already have rolled back a cancelled transaction.
		if ctx.Err() != nil && errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if rollbackErr != nil {
			if err == nil {
				err = rollbackErr
			} else {
				err = dal.NewRollbackError(rollbackErr, err)
			}
		}
		err = leaveTransaction(ctx, err)
	}()
	if err = verifySchemaReadonly(txCtx, tx); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(txCtx)
	view = &readonlySchemaView{lifetime: lifetime, cancel: cancel, db: &Database{sqlDB: d.sqlDB, catalog: tx, schema: d.schema, identifierMode: d.identifierMode, identifierModeSet: d.identifierModeSet}}
	err = markOwn(worker(lifetime, view))
	return err
}

func verifySchemaReadonly(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SHOW transaction_read_only")
	if err != nil {
		return err
	}
	var mode string
	var scanErr error
	valid := false
	if rows.Next() {
		scanErr = rows.Scan(&mode)
		if scanErr == nil {
			valid = mode == "on" && !rows.Next()
		}
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return readErr
	}
	if scanErr != nil {
		return scanErr
	}
	if !valid {
		return errSchemaReadonly
	}
	return closeErr
}

type readonlySchemaView struct {
	db       *Database
	lifetime context.Context
	cancel   context.CancelFunc
	closed   atomic.Bool
}

func (v *readonlySchemaView) context(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil || v.closed.Load() {
		return nil, nil, errSchemaClosed
	}
	if err := v.lifetime.Err(); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var merged context.Context
	var cancel context.CancelFunc
	if deadline, ok := v.lifetime.Deadline(); ok {
		merged, cancel = context.WithDeadline(ctx, deadline)
	} else {
		merged, cancel = context.WithCancel(ctx)
	}
	stop := context.AfterFunc(v.lifetime, cancel)
	return merged, func() { stop(); cancel() }, nil
}
func schemaValue[T any](ctx context.Context, v *readonlySchemaView, read func(context.Context, *Database) (T, error)) (value T, err error) {
	merged, cancel, err := v.context(ctx)
	if err != nil {
		return value, err
	}
	defer cancel()
	value, err = read(merged, v.db)
	if v.closed.Load() {
		var zero T
		return zero, errSchemaClosed
	}
	if lifetimeErr := v.lifetime.Err(); lifetimeErr != nil {
		var zero T
		return zero, lifetimeErr
	}
	if contextErr := merged.Err(); contextErr != nil {
		var zero T
		return zero, contextErr
	}
	return value, err
}

func (v *readonlySchemaView) ListCollections(ctx context.Context, parent *record.Key) ([]dal.CollectionRef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dal.CollectionRef, error) {
		return db.ListCollections(ctx, parent)
	})
}

func (v *readonlySchemaView) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) (*dbschema.CollectionDef, error) {
		return db.DescribeCollection(ctx, ref)
	})
}

func (v *readonlySchemaView) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dbschema.IndexDef, error) { return db.ListIndexes(ctx, ref) })
}

func (v *readonlySchemaView) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dbschema.ConstraintDef, error) {
		return db.ListConstraints(ctx, ref)
	})
}

func (v *readonlySchemaView) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dbschema.Referrer, error) {
		return db.ListReferrers(ctx, ref)
	})
}

func (v *readonlySchemaView) ListSchemas(ctx context.Context) ([]string, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]string, error) { return db.ListSchemas(ctx) })
}

func (v *readonlySchemaView) ListSchemaCollections(ctx context.Context, schema string) ([]dal.CollectionRef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dal.CollectionRef, error) {
		return db.ListSchemaCollections(ctx, schema)
	})
}

func (v *readonlySchemaView) ListViews(ctx context.Context) ([]dal.CollectionRef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dal.CollectionRef, error) { return db.ListViews(ctx) })
}

func (v *readonlySchemaView) ListSchemaViews(ctx context.Context, schema string) ([]dal.CollectionRef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dal.CollectionRef, error) {
		return db.ListSchemaViews(ctx, schema)
	})
}

func (v *readonlySchemaView) ListSourceViews(ctx context.Context) ([]dbschema.SourceViewDef, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]dbschema.SourceViewDef, error) {
		return db.ListSourceViews(ctx)
	})
}

func (v *readonlySchemaView) NonDeterministicTextColumns(ctx context.Context, ref *dal.CollectionRef) ([]string, error) {
	return schemaValue(ctx, v, func(ctx context.Context, db *Database) ([]string, error) {
		return db.NonDeterministicTextColumns(ctx, ref)
	})
}
