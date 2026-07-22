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

func (d *Database) RunReadonlyTransaction(ctx context.Context, f dal.ROTxWorker, opts ...dal.TransactionOption) error {
	return d.innerDB.RunReadonlyTransaction(ctx, f, opts...)
}

func (d *Database) RunReadwriteTransaction(ctx context.Context, f dal.RWTxWorker, opts ...dal.TransactionOption) error {
	return d.innerDB.RunReadwriteTransaction(ctx, f, opts...)
}

func (d *Database) Get(ctx context.Context, record dalrecord.Record) error {
	return d.innerDB.Get(ctx, record)
}

func (d *Database) GetMulti(ctx context.Context, records []dalrecord.Record) error {
	return d.innerDB.GetMulti(ctx, records)
}

func (d *Database) Exists(ctx context.Context, key *dalrecord.Key) (bool, error) {
	return d.innerDB.Exists(ctx, key)
}

func (d *Database) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	return d.innerDB.ExecuteQueryToRecordsReader(ctx, query)
}

func (d *Database) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, opts ...recordset.Option) (dal.RecordsetReader, error) {
	return d.innerDB.ExecuteQueryToRecordsetReader(ctx, query, opts...)
}

// --- extra write methods delegated from dalgo2sql ---

// writeDB is the extended interface exposed by dalgo2sql's concrete type.
// Note: UpdateRecord is intentionally excluded — dalgo2sql's database type
// implements it only on transactions, not on the top-level database object.
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

func (d *Database) Set(ctx context.Context, record dalrecord.Record) error {
	return d.innerDB.(writeDB).Set(ctx, record)
}

func (d *Database) SetMulti(ctx context.Context, records []dalrecord.Record) error {
	return d.innerDB.(writeDB).SetMulti(ctx, records)
}

func (d *Database) Insert(ctx context.Context, record dalrecord.Record, opts ...dal.InsertOption) error {
	return d.innerDB.(writeDB).Insert(ctx, record, opts...)
}

func (d *Database) Upsert(ctx context.Context, record dalrecord.Record) error {
	return d.innerDB.(writeDB).Upsert(ctx, record)
}

func (d *Database) Delete(ctx context.Context, key *dalrecord.Key) error {
	return d.innerDB.(writeDB).Delete(ctx, key)
}

func (d *Database) DeleteMulti(ctx context.Context, keys []*dalrecord.Key) error {
	return d.innerDB.(writeDB).DeleteMulti(ctx, keys)
}

func (d *Database) Update(ctx context.Context, key *dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	return d.innerDB.(writeDB).Update(ctx, key, updates, preconditions...)
}

func (d *Database) UpdateMulti(ctx context.Context, keys []*dalrecord.Key, updates []update.Update, preconditions ...dal.Precondition) error {
	return d.innerDB.(writeDB).UpdateMulti(ctx, keys, updates, preconditions...)
}

// UpdateRecord is not supported at the database level by dalgo2sql; use
// Update with an explicit key instead, or call UpdateRecord inside a
// RunReadwriteTransaction where the transaction object does support it.
func (d *Database) UpdateRecord(ctx context.Context, record dalrecord.Record, updates []update.Update, preconditions ...dal.Precondition) error {
	return dal.ErrNotImplementedYet
}
