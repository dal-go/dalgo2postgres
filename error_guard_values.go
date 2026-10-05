package dalgo2postgres

import (
	"context"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	dalrecord "github.com/dal-go/record"
	"github.com/dal-go/record/update"
)

// The values the adapter hands out whose methods can return an error: the transactions a
// worker is given and the readers a query returns. Each wraps what the DALgo layer under it
// handed to the adapter, so that the error of every one of their methods passes
// [leaveAdapter] as the error of a method of [Database] does.

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
type guardedRecordsReader struct{ dal.RecordsReader }

func (r guardedRecordsReader) Next() (dalrecord.Record, error) {
	return guarded(r.RecordsReader.Next())
}
func (r guardedRecordsReader) Cursor() (string, error) { return guarded(r.RecordsReader.Cursor()) }
func (r guardedRecordsReader) Close() error            { return leaveAdapter(r.RecordsReader.Close()) }

// guardedRecordsetReader is a recordset reader whose every error is guarded.
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

// guardedReadTransaction is the transaction a worker of RunReadonlyTransaction is given.
type guardedReadTransaction struct {
	guardedReads
	tx dal.ReadTransaction
}

var _ dal.ReadTransaction = guardedReadTransaction{}

func newGuardedReadTransaction(tx dal.ReadTransaction) guardedReadTransaction {
	return guardedReadTransaction{guardedReads: guardedReads{tx}, tx: tx}
}

func (g guardedReadTransaction) Options() dal.TransactionOptions { return g.tx.Options() }

// Select is the optional read method SQL adapters' transactions have always had; it reads as
// ExecuteQueryToRecordsReader does.
func (g guardedReadTransaction) Select(ctx context.Context, query dal.Query) (dal.Reader, error) {
	reader, err := g.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, err // a literal nil, not a nil reader in a non-nil interface
	}
	return reader, nil
}

// guardedReadwriteTransaction is the transaction a worker of RunReadwriteTransaction is given.
type guardedReadwriteTransaction struct {
	guardedReads
	guardedWrites
	tx dal.ReadwriteTransaction
}

var _ dal.ReadwriteTransaction = guardedReadwriteTransaction{}

func newGuardedReadwriteTransaction(tx dal.ReadwriteTransaction) guardedReadwriteTransaction {
	return guardedReadwriteTransaction{guardedReads: guardedReads{tx}, guardedWrites: guardedWrites{tx}, tx: tx}
}

func (g guardedReadwriteTransaction) ID() string                      { return g.tx.ID() }
func (g guardedReadwriteTransaction) Options() dal.TransactionOptions { return g.tx.Options() }

// Select is the same optional method as that of the read transaction.
func (g guardedReadwriteTransaction) Select(ctx context.Context, query dal.Query) (dal.Reader, error) {
	reader, err := g.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, err // a literal nil, not a nil reader in a non-nil interface
	}
	return reader, nil
}
