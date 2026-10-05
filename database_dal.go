package dalgo2postgres

import (
	"context"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	dalrecord "github.com/dal-go/record"
	"github.com/dal-go/record/update"
)

// Compile-time assertion: *Database must satisfy dal.DB.
var _ dal.DB = (*Database)(nil)

// --- dal.DB delegation ---
//
// The errors of the methods below pass leaveAdapter (see error_guard.go) in the method itself, and
// so does every error of a value they hand out: the transaction a worker is given and the readers a
// query returns. The two transaction methods are the exception: they call the embedded DB field as
// it is, which is DALgo's layer over the guarded backend (see guardDatabase), and their errors pass
// the same function in that backend. The constructors assign the DB field: a Database whose DB
// field was assigned by other code has, in its two transaction methods, the guard of the DB that
// code assigned. The error a worker returns is the other exception: it is the caller's own and
// comes back as it is.

// RunReadonlyTransaction runs f in a read transaction. The transaction f is given is DALgo's, over a
// transaction of the adapter whose methods answer as the methods of the Database do. The error f
// returns is its own and is returned as it is, whatever it holds; what the transaction adds around
// it (the begin, the commit, the rollback) is guarded. When f failed and the transaction could not
// then be rolled back, the error is a *ConnectionError if the connection failed (f's error holds one,
// or the rollback failed on the connection), and otherwise DALgo's rollback error, which names f's
// error and the rollback's. When the context ended and f failed, the result is f's error as it is,
// whatever the rollback said: a statement that ends on its context makes the driver close its
// connection and database/sql rolls the transaction back itself, so the rollback finds a finished
// transaction or a closed connection, and neither is a failure of the connection.
func (d *Database) RunReadonlyTransaction(ctx context.Context, f dal.ROTxWorker, opts ...dal.TransactionOption) error {
	return d.DB.RunReadonlyTransaction(ctx, f, opts...)
}

// RunReadwriteTransaction runs f in a read-write transaction. The transaction f is given is DALgo's
// (dal.WithoutValidation recognises it), over a transaction of the adapter whose methods answer as
// the methods of the Database do. The error f returns is returned as it is, as for
// [Database.RunReadonlyTransaction], which also says what the other errors of the call are.
func (d *Database) RunReadwriteTransaction(ctx context.Context, f dal.RWTxWorker, opts ...dal.TransactionOption) error {
	return d.DB.RunReadwriteTransaction(ctx, f, opts...)
}

func (d *Database) Get(ctx context.Context, record dalrecord.Record) error {
	return guardedReads{d.DB}.Get(ctx, record)
}

func (d *Database) GetMulti(ctx context.Context, records []dalrecord.Record) error {
	return guardedReads{d.DB}.GetMulti(ctx, records)
}

func (d *Database) Exists(ctx context.Context, key *dalrecord.Key) (bool, error) {
	return guardedReads{d.DB}.Exists(ctx, key)
}

func (d *Database) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	return guardedReads{d.DB}.ExecuteQueryToRecordsReader(ctx, query)
}

func (d *Database) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, opts ...recordset.Option) (dal.RecordsetReader, error) {
	return guardedReads{d.DB}.ExecuteQueryToRecordsetReader(ctx, query, opts...)
}

// --- what the database declares it runs on the server ---

// Compile-time assertions: a *Database declares the capabilities its backend does.
var (
	_ dal.QueryCapabilitiesProvider = (*Database)(nil)
	_ dal.NativeJoinProvider        = (*Database)(nil)
	_ dal.JoinFieldsProvider        = (*Database)(nil)
)

// The three methods below answer for the *Database itself. dal.As asks a value, then
// the backend a dal.DB delegates to, but a *Database embeds the dal.DB interface and
// is not a dal.DB built by dal.NewDB: dal.BackendOf returns the *Database, never the
// dalgo2sql database under d.DB. Without these methods dal.As[dal.QueryCapabilitiesProvider]
// is false on a *Database, and a consumer that asks it (to count natively, to plan a
// join) concludes the database runs nothing on the server. Queries executed through the
// *Database were never affected: they go through d.DB, which plans from its backend.
//
// Each method forwards to what d.DB declares, found with dal.As, and falls back to
// declaring nothing when it declares nothing (a zero Database, whose d.DB is nil).

// QueryCapabilities reports what dalgo2sql's PostgreSQL dialect runs on the server:
// GROUP BY, HAVING, ORDER BY and the aggregates, and no more.
func (d *Database) QueryCapabilities() dal.QueryCapabilities {
	if provider, ok := dal.As[dal.QueryCapabilitiesProvider](d.DB); ok {
		return provider.QueryCapabilities()
	}
	return dal.QueryCapabilities{}
}

// CanExecuteJoin reports whether the database runs q's joins on the server; a non-nil
// error declines it and DALgo runs the join in its own engine. A Database that
// declares no join support declines with [dal.ErrNotSupported].
func (d *Database) CanExecuteJoin(ctx context.Context, q dal.StructuredQuery) error {
	if provider, ok := dal.As[dal.NativeJoinProvider](d.DB); ok {
		return leaveAdapter(provider.CanExecuteJoin(ctx, q))
	}
	return dal.ErrNotSupported
}

// JoinFields lists the columns of source in table order, for the wildcard of a join
// DALgo runs itself. A Database that declares no join support returns none, which
// DALgo takes for a source that cannot supply its schema.
func (d *Database) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if provider, ok := dal.As[dal.JoinFieldsProvider](d.DB); ok {
		return guarded(provider.JoinFields(ctx, source))
	}
	return nil, nil
}

// --- extra write methods delegated from dalgo2sql ---

// writeDB is the extended interface exposed by dalgo2sql's concrete backend
// type. Note: UpdateRecord is intentionally excluded — dalgo2sql's database
// type implements it only on transactions, not on the top-level database
// object.
//
// None of these methods are part of dal.Backend, and dalgo2sql's backend
// does not satisfy dal.WriteSession in full at the database level either
// (it has no database-level InsertMulti or UpdateRecord), so dal.NewDB never
// wraps it in the validating write pipeline at this level: d.DB's dynamic
// type has none of these methods at all, and a plain "d.DB.(writeDB)"
// assertion always fails. dal.As is what recovers the concrete backend that
// does implement them, via dal.BackendOf — visibly and deliberately, the same
// way dal.WithoutValidation recovers an unvalidated write session.
//
// These direct, non-transactional writes were never run through validation
// before the sealed dal.DB change either, since dalgo2sql's database-level
// write path has no BeforeSave call of its own. Writes made through
// RunReadwriteTransaction above are validated, because that goes through
// d.DB's own RunReadwriteTransaction.
type writeDB interface {
	Set(ctx context.Context, record dalrecord.Record) error
	SetMulti(ctx context.Context, records []dalrecord.Record) error
	Insert(ctx context.Context, record dalrecord.Record, opts ...dal.InsertOption) error
	Upsert(ctx context.Context, record dalrecord.Record) error
	Delete(ctx context.Context, key *dalrecord.Key) error
	DeleteMulti(ctx context.Context, keys []*dalrecord.Key) error
	Update(ctx context.Context, key *dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error
	UpdateMulti(ctx context.Context, keys []*dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error
}

// backendWriter recovers writeDB from dalgo2sql's concrete backend via
// dal.As — see the writeDB doc comment above for why a plain assertion
// against d.DB itself cannot reach it.
func (d *Database) backendWriter() (writeDB, bool) {
	return dal.As[writeDB](d.DB)
}

func (d *Database) Set(ctx context.Context, record dalrecord.Record) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveCall(w.Set(ctx, record), record)
}

func (d *Database) SetMulti(ctx context.Context, records []dalrecord.Record) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveCall(w.SetMulti(ctx, records), records...)
}

func (d *Database) Insert(ctx context.Context, record dalrecord.Record, opts ...dal.InsertOption) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveCall(w.Insert(ctx, record, opts...), record)
}

func (d *Database) Upsert(ctx context.Context, record dalrecord.Record) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveCall(w.Upsert(ctx, record), record)
}

func (d *Database) Delete(ctx context.Context, key *dalrecord.Key) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveAdapter(w.Delete(ctx, key))
}

func (d *Database) DeleteMulti(ctx context.Context, keys []*dalrecord.Key) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveAdapter(w.DeleteMulti(ctx, keys))
}

func (d *Database) Update(ctx context.Context, key *dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveAdapter(w.Update(ctx, key, updates, preconditions...))
}

func (d *Database) UpdateMulti(ctx context.Context, keys []*dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	w, ok := d.backendWriter()
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return leaveAdapter(w.UpdateMulti(ctx, keys, updates, preconditions...))
}

// UpdateRecord is not supported at the database level by dalgo2sql; use
// Update with an explicit key instead, or call UpdateRecord inside a
// RunReadwriteTransaction where the transaction object does support it.
func (d *Database) UpdateRecord(ctx context.Context, record dalrecord.Record, updates []update.Update, preconditions ...dal.Precondition) error {
	return dal.ErrNotImplementedYet
}
