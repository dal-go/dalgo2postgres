package dalgo2postgres

import (
	"context"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	dalrecord "github.com/dal-go/record"
	"github.com/dal-go/record/update"
)

// The values the adapter hands out whose methods can return an error: the transactions a
// worker is given and the readers a query returns. DALgo's framework layer (record
// validation, hooks, the plans of joins and aggregates) is built over a backend, and the
// transaction a worker is given is that layer's own, over the backend's. The guard is placed
// under that layer, on the backend: [guardedBackend] hands DALgo transactions and readers whose
// every method passes [leaveAdapter], so the transaction a worker holds answers as the methods
// of [Database] do and is still the transaction dal.WithoutValidation recognises.

// guardedReads is the read surface of a database or a transaction, with every error guarded.
type guardedReads struct{ session dal.ReadSession }

func (g guardedReads) Get(ctx context.Context, record dalrecord.Record) error {
	return leaveCall(g.session.Get(ctx, record), record)
}

func (g guardedReads) GetMulti(ctx context.Context, records []dalrecord.Record) error {
	return leaveCall(g.session.GetMulti(ctx, records), records...)
}

func (g guardedReads) Exists(ctx context.Context, key *dalrecord.Key) (bool, error) {
	return guarded(g.session.Exists(ctx, key))
}

func (g guardedReads) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	reader, err := g.session.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, leaveAdapter(err)
	}
	return guardedRecordsReader{reader}, nil
}

func (g guardedReads) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	reader, err := g.session.ExecuteQueryToRecordsetReader(ctx, query, options...)
	if err != nil {
		return nil, leaveAdapter(err)
	}
	return guardedRecordsetReader{reader}, nil
}

// guardedRecordsReader is a records reader whose every error is guarded. dal.ErrNoMoreRecords,
// which ends a result, is not a failure and is returned as it is.
//
// The guard recognises a failure of the connection by the type of an error in its chain (see
// isConnectionFailure). A layer under the readers that turns an error into text loses the type, and
// the guard cannot recognise what it can no longer see: a new version of dalgo2sql is checked
// against the reader entries of the table in error_guard_test.go before it is taken.
type guardedRecordsReader struct{ dal.RecordsReader }

func (r guardedRecordsReader) Next() (dalrecord.Record, error) {
	return guarded(r.RecordsReader.Next())
}
func (r guardedRecordsReader) Cursor() (string, error) { return guarded(r.RecordsReader.Cursor()) }
func (r guardedRecordsReader) Close() error            { return leaveAdapter(r.RecordsReader.Close()) }

// guardedRecordsetReader is a recordset reader whose every error is guarded; what is said of
// [guardedRecordsReader] holds for it.
type guardedRecordsetReader struct{ dal.RecordsetReader }

func (r guardedRecordsetReader) Next() (recordset.Row, recordset.Recordset, error) {
	row, rs, err := r.RecordsetReader.Next()
	return row, rs, leaveAdapter(err)
}

func (r guardedRecordsetReader) Cursor() (string, error) { return guarded(r.RecordsetReader.Cursor()) }
func (r guardedRecordsetReader) Close() error            { return leaveAdapter(r.RecordsetReader.Close()) }

// guardedWrites is the write surface of a transaction, with every error guarded.
type guardedWrites struct{ session dal.WriteSession }

func (g guardedWrites) Set(ctx context.Context, record dalrecord.Record) error {
	return leaveCall(g.session.Set(ctx, record), record)
}

func (g guardedWrites) SetMulti(ctx context.Context, records []dalrecord.Record) error {
	return leaveCall(g.session.SetMulti(ctx, records), records...)
}

func (g guardedWrites) Insert(ctx context.Context, record dalrecord.Record, options ...dal.InsertOption) error {
	return leaveCall(g.session.Insert(ctx, record, options...), record)
}

func (g guardedWrites) InsertMulti(ctx context.Context, records []dalrecord.Record, options ...dal.InsertOption) error {
	return leaveCall(g.session.InsertMulti(ctx, records, options...), records...)
}

func (g guardedWrites) Delete(ctx context.Context, key *dalrecord.Key) error {
	return leaveAdapter(g.session.Delete(ctx, key))
}

func (g guardedWrites) DeleteMulti(ctx context.Context, keys []*dalrecord.Key) error {
	return leaveAdapter(g.session.DeleteMulti(ctx, keys))
}

func (g guardedWrites) Update(ctx context.Context, key *dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	return leaveAdapter(g.session.Update(ctx, key, updates, preconditions...))
}

func (g guardedWrites) UpdateRecord(ctx context.Context, record dalrecord.Record, updates []update.Update, preconditions ...dal.Precondition) error {
	return leaveCall(g.session.UpdateRecord(ctx, record, updates, preconditions...), record)
}

func (g guardedWrites) UpdateMulti(ctx context.Context, keys []*dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	return leaveAdapter(g.session.UpdateMulti(ctx, keys, updates, preconditions...))
}

// guardedJoins forwards the two optional questions DALgo asks of a backend or of a transaction
// before it plans a join (see [dal.NativeJoinProvider] and [dal.JoinFieldsProvider]) to the value
// under it, which answers them with its own view of the schema. A value that declines to
// answer declines the join, and supplies no fields: DALgo then runs the join itself.
type guardedJoins struct{ inner any }

func (g guardedJoins) CanExecuteJoin(ctx context.Context, q dal.StructuredQuery) error {
	if provider, ok := g.inner.(dal.NativeJoinProvider); ok {
		return leaveAdapter(provider.CanExecuteJoin(ctx, q))
	}
	return dal.ErrNotSupported
}

func (g guardedJoins) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if provider, ok := g.inner.(dal.JoinFieldsProvider); ok {
		return guarded(provider.JoinFields(ctx, source))
	}
	return nil, nil
}

// selectOf is the optional read method SQL adapters' transactions have always had; it reads as
// ExecuteQueryToRecordsReader does.
func selectOf(ctx context.Context, reads guardedReads, query dal.Query) (dal.Reader, error) {
	reader, err := reads.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, err // a literal nil, not a nil reader in a non-nil interface
	}
	return reader, nil
}

// guardedReadTransaction is the read transaction DALgo's framework layer is given.
type guardedReadTransaction struct {
	guardedReads
	guardedJoins
	tx dal.ReadTransaction
}

var _ dal.ReadTransaction = guardedReadTransaction{}

func newGuardedReadTransaction(tx dal.ReadTransaction) guardedReadTransaction {
	return guardedReadTransaction{guardedReads: guardedReads{tx}, guardedJoins: guardedJoins{tx}, tx: tx}
}

func (g guardedReadTransaction) Options() dal.TransactionOptions { return g.tx.Options() }

func (g guardedReadTransaction) Select(ctx context.Context, query dal.Query) (dal.Reader, error) {
	return selectOf(ctx, g.guardedReads, query)
}

// guardedReadwriteTransaction is the read-write transaction DALgo's framework layer is given.
type guardedReadwriteTransaction struct {
	guardedReads
	guardedWrites
	guardedJoins
	tx dal.ReadwriteTransaction
}

var _ dal.ReadwriteTransaction = guardedReadwriteTransaction{}

func newGuardedReadwriteTransaction(tx dal.ReadwriteTransaction) guardedReadwriteTransaction {
	return guardedReadwriteTransaction{guardedReads: guardedReads{tx}, guardedWrites: guardedWrites{tx}, guardedJoins: guardedJoins{tx}, tx: tx}
}

func (g guardedReadwriteTransaction) ID() string                      { return g.tx.ID() }
func (g guardedReadwriteTransaction) Options() dal.TransactionOptions { return g.tx.Options() }

func (g guardedReadwriteTransaction) Select(ctx context.Context, query dal.Query) (dal.Reader, error) {
	return selectOf(ctx, g.guardedReads, query)
}

// guardedBackend is the backend DALgo's framework layer is built over: the SQL backend, with every
// error and every value it hands out guarded. It answers the optional questions DALgo asks of a
// backend (what the server runs, whether it runs a join, the fields of a wildcard) and the
// writes outside a transaction that [Database] looks for, each as the backend under it does, or
// declines when that backend does not say.
//
// It has not every method of [dal.WriteSession] (there is no InsertMulti and no UpdateRecord at
// this level, as in the backend under it), so the framework layer does not take it for a backend
// that writes outside a transaction: the writes below are reached through [writeDB] only.
type guardedBackend struct {
	guardedReads
	guardedJoins
	inner dal.Backend
}

var (
	_ dal.Backend                   = guardedBackend{}
	_ dal.QueryCapabilitiesProvider = guardedBackend{}
	_ dal.NativeJoinProvider        = guardedBackend{}
	_ dal.JoinFieldsProvider        = guardedBackend{}
	_ writeDB                       = guardedBackend{}
)

func newGuardedBackend(inner dal.Backend) guardedBackend {
	return guardedBackend{guardedReads: guardedReads{inner}, guardedJoins: guardedJoins{inner}, inner: inner}
}

// guardDatabase puts the guard under the framework layer of a database dalgo2sql made: the
// backend of inner is guarded, and a framework layer is built over it again. inner's own layer is
// not kept: the transaction a worker is given has one layer of validation and hooks.
func guardDatabase(inner dal.DB) dal.DB {
	return dal.NewDB(newGuardedBackend(dal.BackendOf(inner)))
}

func (b guardedBackend) ID() string           { return b.inner.ID() }
func (b guardedBackend) Adapter() dal.Adapter { return b.inner.Adapter() }
func (b guardedBackend) Schema() dal.Schema   { return b.inner.Schema() }
func (b guardedBackend) SupportsConcurrentConnections() bool {
	return b.inner.SupportsConcurrentConnections()
}

// RunReadonlyTransaction runs f in a read transaction whose methods are guarded. The error f returns
// is the caller's own and is returned as it is; what the transaction adds around it passes the guard
// (see [leaveTransaction]).
func (b guardedBackend) RunReadonlyTransaction(ctx context.Context, f dal.ROTxWorker, opts ...dal.TransactionOption) error {
	return leaveTransaction(b.inner.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
		return markOwn(f(ctx, newGuardedReadTransaction(tx)))
	}, opts...))
}

// RunReadwriteTransaction is RunReadonlyTransaction for a read-write transaction.
func (b guardedBackend) RunReadwriteTransaction(ctx context.Context, f dal.RWTxWorker, opts ...dal.TransactionOption) error {
	return leaveTransaction(b.inner.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return markOwn(f(ctx, newGuardedReadwriteTransaction(tx)))
	}, opts...))
}

// QueryCapabilities declares what the backend under it declares, and nothing when it declares nothing.
func (b guardedBackend) QueryCapabilities() dal.QueryCapabilities {
	if provider, ok := b.inner.(dal.QueryCapabilitiesProvider); ok {
		return provider.QueryCapabilities()
	}
	return dal.QueryCapabilities{}
}

// write runs a write the backend under it has outside a transaction, which not every backend has.
func (b guardedBackend) write(call func(w writeDB) error) error {
	w, ok := b.inner.(writeDB)
	if !ok {
		return dal.ErrNotImplementedYet
	}
	return call(w)
}

func (b guardedBackend) Set(ctx context.Context, record dalrecord.Record) error {
	return b.write(func(w writeDB) error { return leaveCall(w.Set(ctx, record), record) })
}

func (b guardedBackend) SetMulti(ctx context.Context, records []dalrecord.Record) error {
	return b.write(func(w writeDB) error { return leaveCall(w.SetMulti(ctx, records), records...) })
}

func (b guardedBackend) Insert(ctx context.Context, record dalrecord.Record, options ...dal.InsertOption) error {
	return b.write(func(w writeDB) error { return leaveCall(w.Insert(ctx, record, options...), record) })
}

func (b guardedBackend) Upsert(ctx context.Context, record dalrecord.Record) error {
	return b.write(func(w writeDB) error { return leaveCall(w.Upsert(ctx, record), record) })
}

func (b guardedBackend) Delete(ctx context.Context, key *dalrecord.Key) error {
	return b.write(func(w writeDB) error { return leaveAdapter(w.Delete(ctx, key)) })
}

func (b guardedBackend) DeleteMulti(ctx context.Context, keys []*dalrecord.Key) error {
	return b.write(func(w writeDB) error { return leaveAdapter(w.DeleteMulti(ctx, keys)) })
}

func (b guardedBackend) Update(ctx context.Context, key *dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	return b.write(func(w writeDB) error { return leaveAdapter(w.Update(ctx, key, updates, preconditions...)) })
}

func (b guardedBackend) UpdateMulti(ctx context.Context, keys []*dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	return b.write(func(w writeDB) error { return leaveAdapter(w.UpdateMulti(ctx, keys, updates, preconditions...)) })
}
