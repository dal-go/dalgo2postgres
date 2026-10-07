package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	dalrecord "github.com/dal-go/record"
	"github.com/dal-go/record/update"
	"github.com/jackc/pgx/v5/pgconn"
)

// An error from the adapter never holds the connection's configuration. A connection that
// fails, at open or at any later call, is a classified error with a fixed sentence.
//
// The tests of this file hold that rule for every call that can return an error: each is made
// against a stand-in whose connection fails in pgx's own way (see error_guard_standin_test.go),
// with a marker in each part of the configuration, and every error it yields, and everything
// reachable from it, must be free of the markers. TestErrorsHoldNoConfiguration_TableNamesEveryErrorReturningMethod
// fails when a method that can return an error is not in the table, so a method added later
// cannot be forgotten.

// errorCall is one call of the table. run makes the call against its stand-in and returns every
// error it yielded: the one it returned, the one it put on a record, the one of the transaction
// that ended.
type errorCall struct {
	// plain says the call never reaches a connection: its error is a constant of DALgo, not a
	// failure of the connection, so it must hold no marker but need not be a *ConnectionError.
	plain bool
	// inChain says DALgo's own diagnostic (a join's) carries the failure: the error is the
	// diagnostic, whose text names the step and ends in the fixed sentence, and the classified
	// *ConnectionError is in its chain.
	inChain bool
	run     func(t *testing.T) []error
}

// standIn is the connection the calls of the table are made against: how it fails.
type standIn struct {
	name string
	// failure is the error the connection yields.
	failure func(t *testing.T) error
	// handle is a Database whose every call that needs a connection fails with that error.
	handle func(t *testing.T) *Database
}

// standIns are the connections the table is made against: one that cannot be made (a handle that
// opened lazily, over a network that refuses every dial), and each way a connection that was
// made is lost.
func standIns() []standIn {
	ins := []standIn{{
		name:    "connect failure",
		failure: func(t *testing.T) error { return connectFailure(t, refuseEveryDial) },
		handle:  func(t *testing.T) *Database { return lazyDatabase(t, refuseEveryDial) },
	}}
	for _, dropped := range droppedFailures() {
		ins = append(ins, standIn{
			name:    dropped.name,
			failure: func(*testing.T) error { return dropped.err() },
			handle:  func(t *testing.T) *Database { return transactionalDatabase(t, dropped.err()) },
		})
	}
	return ins
}

// onLazyDatabase runs call against a Database whose connection fails.
func (s standIn) onLazyDatabase(call func(ctx context.Context, db *Database) []error) errorCall {
	return errorCall{run: func(t *testing.T) []error {
		return call(context.Background(), s.handle(t))
	}}
}

// onEmbeddedDB runs call against the dal.DB the exported DB field of a Database whose connection
// fails holds: DALgo's layer over the guarded backend, called without the Database's own methods.
func (s standIn) onEmbeddedDB(call func(ctx context.Context, db dal.DB) []error) errorCall {
	return errorCall{run: func(t *testing.T) []error {
		return call(context.Background(), s.handle(t).DB)
	}}
}

// onEmbeddedBackend runs call against the backend DALgo's layer of the exported DB field is built
// over, called without any layer above it.
func (s standIn) onEmbeddedBackend(call func(ctx context.Context, backend dal.Backend) []error) errorCall {
	return errorCall{run: func(t *testing.T) []error {
		return call(context.Background(), dal.BackendOf(s.handle(t).DB))
	}}
}

// inReadonlyTransaction runs call inside a read transaction whose statements all fail with the
// stand-in's failure. The commit fails the same way, and its error is one of the call's.
func (s standIn) inReadonlyTransaction(call func(ctx context.Context, tx dal.ReadTransaction) []error) errorCall {
	return errorCall{run: func(t *testing.T) []error {
		db := transactionalDatabase(t, s.failure(t))
		var errs []error
		outer := db.RunReadonlyTransaction(context.Background(), func(ctx context.Context, tx dal.ReadTransaction) error {
			errs = call(ctx, tx)
			return nil
		})
		return append(errs, outer)
	}}
}

// inReadwriteTransaction is inReadonlyTransaction for a read-write transaction.
func (s standIn) inReadwriteTransaction(call func(ctx context.Context, tx dal.ReadwriteTransaction) []error) errorCall {
	return errorCall{run: func(t *testing.T) []error {
		db := transactionalDatabase(t, s.failure(t))
		var errs []error
		outer := db.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			errs = call(ctx, tx)
			return nil
		})
		return append(errs, outer)
	}}
}

// onReader runs call against a records reader whose rows end in the stand-in's failure.
func (s standIn) onReader(plain bool, call func(reader dal.RecordsReader) []error) errorCall {
	return errorCall{plain: plain, run: func(t *testing.T) []error {
		db, mock := mockedDatabase(t)
		failure := s.failure(t)
		mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1).RowError(0, failure).CloseError(failure))
		reader, err := db.ExecuteQueryToRecordsReader(context.Background(), textQuery())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
		}
		return call(reader)
	}}
}

// onRecordsetReader is onReader for a recordset reader.
func (s standIn) onRecordsetReader(plain bool, call func(reader dal.RecordsetReader) []error) errorCall {
	return errorCall{plain: plain, run: func(t *testing.T) []error {
		db, mock := mockedDatabase(t)
		failure := s.failure(t)
		mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1).RowError(0, failure).CloseError(failure))
		reader, err := db.ExecuteQueryToRecordsetReader(context.Background(), textQuery())
		if err != nil {
			t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
		}
		return call(reader)
	}}
}

// joinQuery is a query with a join, so that deciding whether the server runs it reads the catalog.
func joinQuery() dal.StructuredQuery {
	on := dal.NewComparison(dal.NewFieldRef("a", "id"), dal.Equal, dal.NewFieldRef("b", "id"))
	return dal.From(dal.NewRootCollectionRef("orders", "a")).
		Join(dal.NewJoinedSource(dal.NewRootCollectionRef("customers", "b"), dal.JoinInner, on)).
		NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("b", "id")})
}

// selectOrders is a structured query over the declared table.
func selectOrders() dal.Query {
	return dal.From(dal.NewRootCollectionRef("orders", "")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "id")})
}

func someUpdates() []update.Update { return []update.Update{update.ByFieldName("status", "shipped")} }

func ordersRef() dal.CollectionRef { return dal.NewRootCollectionRef("orders", "") }

// errorCalls is the table of every call that can return an error, keyed by the kind of value it
// is called on and its name. Each entry is made against a stand-in whose connection fails.
func errorCalls(s standIn) map[string]errorCall {
	onLazyDatabase, inReadonlyTransaction, inReadwriteTransaction := s.onLazyDatabase, s.inReadonlyTransaction, s.inReadwriteTransaction
	onReader, onRecordsetReader := s.onReader, s.onRecordsetReader
	onEmbeddedDB, onEmbeddedBackend := s.onEmbeddedDB, s.onEmbeddedBackend
	ref := ordersRef()
	key := dalrecord.NewKeyWithID("orders", "1")
	table := map[string]errorCall{
		// The Database itself: a handle that opened lazily over a network that refuses every dial.
		"Database.AlterCollection": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.AlterCollection(ctx, "orders")}
		}),
		"Database.CanExecuteJoin": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.CanExecuteJoin(ctx, joinQuery())}
		}),
		"Database.Close": {run: func(t *testing.T) []error {
			db, mock := mockedDatabase(t)
			mock.ExpectClose().WillReturnError(s.failure(t))
			return []error{db.Close()}
		}},
		"Database.CreateCollection": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.CreateCollection(ctx, dbschema.CollectionDef{
				Name: "orders", Fields: []dbschema.FieldDef{{Name: "id", Type: dbschema.String}}, PrimaryKey: []dal.FieldName{"id"},
			})}
		}),
		"Database.Delete": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.Delete(ctx, key)}
		}),
		"Database.DeleteMulti": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.DeleteMulti(ctx, []*dalrecord.Key{key})}
		}),
		"Database.DescribeCollection": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.DescribeCollection(ctx, &ref)
			return []error{err}
		}),
		"Database.DropCollection": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.DropCollection(ctx, "orders")}
		}),
		"Database.ExecuteQueryToRecordsReader": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ExecuteQueryToRecordsReader(ctx, selectOrders())
			return []error{err}
		}),
		"Database.ExecuteQueryToRecordsetReader": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ExecuteQueryToRecordsetReader(ctx, selectOrders())
			return []error{err}
		}),
		"Database.Exists": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.Exists(ctx, key)
			return []error{err}
		}),
		"Database.Get": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			rec := unreadRecord()
			err := db.Get(ctx, rec)
			return []error{err, rec.Error()}
		}),
		"Database.GetMulti": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			// One record of a table: the read puts its error on the record.
			rec := unreadRecord()
			var errs []error
			if err := db.GetMulti(ctx, []dalrecord.Record{rec}); err != nil {
				errs = append(errs, err)
			}
			return append(errs, rec.Error())
		}),
		"Database.Insert": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.Insert(ctx, ordersRecord())}
		}),
		"Database.JoinFields": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.JoinFields(ctx, dal.NewRootCollectionRef("orders", "a"))
			return []error{err}
		}),
		"Database.ListCollections": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListCollections(ctx, nil)
			return []error{err}
		}),
		"Database.ListConstraints": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListConstraints(ctx, &ref)
			return []error{err}
		}),
		"Database.ListIndexes": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListIndexes(ctx, &ref)
			return []error{err}
		}),
		"Database.ListReferrers": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListReferrers(ctx, &ref)
			return []error{err}
		}),
		"Database.ListSchemaCollections": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListSchemaCollections(ctx, "sales")
			return []error{err}
		}),
		"Database.ListSchemaViews": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListSchemaViews(ctx, "sales")
			return []error{err}
		}),
		"Database.ListSchemas": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListSchemas(ctx)
			return []error{err}
		}),
		"Database.ListViews": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListViews(ctx)
			return []error{err}
		}),
		"Database.ListSourceViews": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.ListSourceViews(ctx)
			return []error{err}
		}),
		"Database.NonDeterministicTextColumns": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			_, err := db.NonDeterministicTextColumns(ctx, &ref)
			return []error{err}
		}),
		"Database.RunReadonlyTransaction": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.RunReadonlyTransaction(ctx, func(context.Context, dal.ReadTransaction) error { return nil })}
		}),
		"Database.RunReadwriteTransaction": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.RunReadwriteTransaction(ctx, func(context.Context, dal.ReadwriteTransaction) error { return nil })}
		}),
		"Database.Set": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.Set(ctx, ordersRecord())}
		}),
		"Database.SetMulti": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.SetMulti(ctx, []dalrecord.Record{ordersRecord()})}
		}),
		"Database.Update": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.Update(ctx, key, someUpdates())}
		}),
		"Database.UpdateMulti": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.UpdateMulti(ctx, []*dalrecord.Key{key}, someUpdates())}
		}),
		"Database.UpdateRecord": {plain: true, run: func(t *testing.T) []error {
			return []error{s.handle(t).UpdateRecord(context.Background(), ordersRecord(), someUpdates())}
		}},
		"Database.Upsert": onLazyDatabase(func(ctx context.Context, db *Database) []error {
			return []error{db.Upsert(ctx, ordersRecord())}
		}),

		// The dal.DB of the exported DB field: DALgo's layer over the guarded backend.
		"DB.ExecuteQueryToRecordsReader": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			_, err := db.ExecuteQueryToRecordsReader(ctx, selectOrders())
			return []error{err}
		}),
		"DB.ExecuteQueryToRecordsetReader": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			_, err := db.ExecuteQueryToRecordsetReader(ctx, selectOrders())
			return []error{err}
		}),
		"DB.Exists": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			_, err := db.Exists(ctx, key)
			return []error{err}
		}),
		"DB.Get": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			rec := unreadRecord()
			err := db.Get(ctx, rec)
			return []error{err, rec.Error()}
		}),
		"DB.GetMulti": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			rec := unreadRecord()
			var errs []error
			if err := db.GetMulti(ctx, []dalrecord.Record{rec}); err != nil {
				errs = append(errs, err)
			}
			return append(errs, rec.Error())
		}),
		"DB.RunReadonlyTransaction": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			return []error{db.RunReadonlyTransaction(ctx, func(context.Context, dal.ReadTransaction) error { return nil })}
		}),
		"DB.RunReadwriteTransaction": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			return []error{db.RunReadwriteTransaction(ctx, func(context.Context, dal.ReadwriteTransaction) error { return nil })}
		}),
		"DB.Select": onEmbeddedDB(func(ctx context.Context, db dal.DB) []error {
			_, err := db.(interface {
				Select(context.Context, dal.Query) (dal.Reader, error)
			}).Select(ctx, textQuery())
			return []error{err}
		}),

		// The backend under DALgo's layer of the exported DB field, and the writes outside a
		// transaction it has.
		"Backend.CanExecuteJoin": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(dal.NativeJoinProvider).CanExecuteJoin(ctx, joinQuery())}
		}),
		"Backend.Delete": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).Delete(ctx, key)}
		}),
		"Backend.DeleteMulti": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).DeleteMulti(ctx, []*dalrecord.Key{key})}
		}),
		"Backend.ExecuteQueryToRecordsReader": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			_, err := backend.ExecuteQueryToRecordsReader(ctx, selectOrders())
			return []error{err}
		}),
		"Backend.ExecuteQueryToRecordsetReader": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			_, err := backend.ExecuteQueryToRecordsetReader(ctx, selectOrders())
			return []error{err}
		}),
		"Backend.Exists": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			_, err := backend.Exists(ctx, key)
			return []error{err}
		}),
		"Backend.Get": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			rec := unreadRecord()
			err := backend.Get(ctx, rec)
			return []error{err, rec.Error()}
		}),
		"Backend.GetMulti": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			rec := unreadRecord()
			var errs []error
			if err := backend.GetMulti(ctx, []dalrecord.Record{rec}); err != nil {
				errs = append(errs, err)
			}
			return append(errs, rec.Error())
		}),
		"Backend.Insert": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).Insert(ctx, ordersRecord())}
		}),
		"Backend.JoinFields": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			_, err := backend.(dal.JoinFieldsProvider).JoinFields(ctx, dal.NewRootCollectionRef("orders", "a"))
			return []error{err}
		}),
		"Backend.RunReadonlyTransaction": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.RunReadonlyTransaction(ctx, func(context.Context, dal.ReadTransaction) error { return nil })}
		}),
		"Backend.RunReadwriteTransaction": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.RunReadwriteTransaction(ctx, func(context.Context, dal.ReadwriteTransaction) error { return nil })}
		}),
		"Backend.Set": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).Set(ctx, ordersRecord())}
		}),
		"Backend.SetMulti": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).SetMulti(ctx, []dalrecord.Record{ordersRecord()})}
		}),
		"Backend.Update": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).Update(ctx, key, someUpdates())}
		}),
		"Backend.UpdateMulti": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).UpdateMulti(ctx, []*dalrecord.Key{key}, someUpdates())}
		}),
		"Backend.Upsert": onEmbeddedBackend(func(ctx context.Context, backend dal.Backend) []error {
			return []error{backend.(writeDB).Upsert(ctx, ordersRecord())}
		}),

		// The transaction handed to a worker of RunReadonlyTransaction.
		"ReadTransaction.ExecuteQueryToRecordsReader": inReadonlyTransaction(func(ctx context.Context, tx dal.ReadTransaction) []error {
			_, err := tx.ExecuteQueryToRecordsReader(ctx, textQuery())
			return []error{err}
		}),
		"ReadTransaction.ExecuteQueryToRecordsetReader": inReadonlyTransaction(func(ctx context.Context, tx dal.ReadTransaction) []error {
			_, err := tx.ExecuteQueryToRecordsetReader(ctx, textQuery())
			return []error{err}
		}),
		"ReadTransaction.Exists": inReadonlyTransaction(func(ctx context.Context, tx dal.ReadTransaction) []error {
			_, err := tx.Exists(ctx, key)
			return []error{err}
		}),
		"ReadTransaction.Get": inReadonlyTransaction(func(ctx context.Context, tx dal.ReadTransaction) []error {
			rec := unreadRecord()
			err := tx.Get(ctx, rec)
			return []error{err, rec.Error()}
		}),
		"ReadTransaction.GetMulti": inReadonlyTransaction(func(ctx context.Context, tx dal.ReadTransaction) []error {
			rec := unreadRecord()
			_ = tx.GetMulti(ctx, []dalrecord.Record{rec})
			return []error{rec.Error()}
		}),
		"ReadTransaction.Select": inReadonlyTransaction(func(ctx context.Context, tx dal.ReadTransaction) []error {
			_, err := tx.(interface {
				Select(context.Context, dal.Query) (dal.Reader, error)
			}).Select(ctx, textQuery())
			return []error{err}
		}),

		// The transaction handed to a worker of RunReadwriteTransaction.
		"ReadwriteTransaction.Delete": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.Delete(ctx, key)}
		}),
		"ReadwriteTransaction.DeleteMulti": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.DeleteMulti(ctx, []*dalrecord.Key{key})}
		}),
		"ReadwriteTransaction.ExecuteQueryToRecordsReader": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			_, err := tx.ExecuteQueryToRecordsReader(ctx, textQuery())
			return []error{err}
		}),
		"ReadwriteTransaction.ExecuteQueryToRecordsetReader": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			_, err := tx.ExecuteQueryToRecordsetReader(ctx, textQuery())
			return []error{err}
		}),
		"ReadwriteTransaction.Exists": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			_, err := tx.Exists(ctx, key)
			return []error{err}
		}),
		"ReadwriteTransaction.Get": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			rec := unreadRecord()
			err := tx.Get(ctx, rec)
			return []error{err, rec.Error()}
		}),
		"ReadwriteTransaction.GetMulti": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			rec := unreadRecord()
			_ = tx.GetMulti(ctx, []dalrecord.Record{rec})
			return []error{rec.Error()}
		}),
		"ReadwriteTransaction.Insert": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.Insert(ctx, ordersRecord())}
		}),
		"ReadwriteTransaction.InsertMulti": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.InsertMulti(ctx, []dalrecord.Record{ordersRecord()})}
		}),
		"ReadwriteTransaction.Select": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			_, err := tx.(interface {
				Select(context.Context, dal.Query) (dal.Reader, error)
			}).Select(ctx, textQuery())
			return []error{err}
		}),
		"ReadwriteTransaction.Set": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.Set(ctx, ordersRecord())}
		}),
		"ReadwriteTransaction.SetMulti": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.SetMulti(ctx, []dalrecord.Record{ordersRecord()})}
		}),
		"ReadwriteTransaction.Update": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.Update(ctx, key, someUpdates())}
		}),
		"ReadwriteTransaction.UpdateMulti": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.UpdateMulti(ctx, []*dalrecord.Key{key}, someUpdates())}
		}),
		"ReadwriteTransaction.UpdateRecord": inReadwriteTransaction(func(ctx context.Context, tx dal.ReadwriteTransaction) []error {
			return []error{tx.UpdateRecord(ctx, ordersRecord(), someUpdates())}
		}),

		// The readers of a query: the rows end in the stand-in's connect failure.
		"RecordsReader.Next": onReader(false, func(reader dal.RecordsReader) []error {
			_, err := reader.Next()
			return []error{err}
		}),
		"RecordsReader.Close": onReader(false, func(reader dal.RecordsReader) []error {
			return []error{reader.Close()}
		}),
		"RecordsReader.Cursor": onReader(true, func(reader dal.RecordsReader) []error {
			_, err := reader.Cursor()
			return []error{err}
		}),
		"RecordsetReader.Next": onRecordsetReader(false, func(reader dal.RecordsetReader) []error {
			_, _, err := reader.Next()
			return []error{err}
		}),
		"RecordsetReader.Close": onRecordsetReader(false, func(reader dal.RecordsetReader) []error {
			return []error{reader.Close()}
		}),
		"RecordsetReader.Cursor": onRecordsetReader(true, func(reader dal.RecordsetReader) []error {
			_, err := reader.Cursor()
			return []error{err}
		}),
	}
	return table
}

// The stand-in must be able to leak. If it could not, every test below would be green for a
// reason that has nothing to do with the adapter.
func TestErrorsHoldNoConfiguration_TheStandInFailureHoldsEveryMarker(t *testing.T) {
	failure := connectFailure(t, refuseEveryDial)
	var connectErr *pgconn.ConnectError
	if !errors.As(failure, &connectErr) {
		t.Fatalf("the stand-in's failure is %T, want pgx's *pgconn.ConnectError", failure)
	}
	for _, m := range []string{markUser, markDatabase, markHost} {
		if text := failure.Error(); !strings.Contains(text, m) {
			t.Errorf("the text of the stand-in's failure does not hold %q: %s", m, text)
		}
	}
	for _, m := range []string{markUser, markPassword, markDatabase, markHost, markParam} {
		found := false
		for _, e := range []string{connectErr.Config.User, connectErr.Config.Password, connectErr.Config.Database, connectErr.Config.Host, connectErr.Config.RuntimeParams["application_name"]} {
			found = found || strings.Contains(e, m)
		}
		if !found {
			t.Errorf("the configuration the stand-in's failure holds does not hold %q", m)
		}
	}
	if path, m, ok := markerIn(failure, configMarkers); !ok {
		t.Error("the walk of reachable values finds no marker in the stand-in's failure")
	} else if m == "" || path == "" {
		t.Errorf("the walk found a marker without saying which or where: %q at %q", m, path)
	}
	// Each marker is found by the walk when it is the only one there.
	for _, m := range configMarkers {
		if _, _, ok := markerIn(struct{ hidden map[string][]string }{map[string][]string{"k": {"x" + m}}}, configMarkers); !ok {
			t.Errorf("the walk does not find %q inside a map of slices behind an unexported field", m)
		}
	}
}

// assertCall runs a call of the table and checks every error it yielded: it holds no part of the
// configuration and, unless the call never reaches a connection, it is a classified one.
func assertCall(t *testing.T, name string, call errorCall) {
	t.Helper()
	errs := call.run(t)
	if len(errs) == 0 {
		t.Fatal("the call yielded no error")
	}
	for i, err := range errs {
		what := fmt.Sprintf("%s (error %d of %d)", name, i+1, len(errs))
		assertHoldsNoConfiguration(t, what, err)
		if call.plain || err == nil {
			continue
		}
		var failure *ConnectionError
		if call.inChain && errors.As(err, &failure) {
			if !strings.HasSuffix(err.Error(), failure.Error()) {
				t.Errorf("%s: the text %q does not end in the fixed sentence %q", what, err, failure)
			}
			continue
		}
		if _, ok := err.(*ConnectionError); !ok {
			t.Errorf("%s: the error is %T %q, want a *ConnectionError", what, err, err)
		}
	}
}

// Every call of the table, against a connection that fails, yields only errors that hold no part
// of the configuration, and, unless the call never reaches a connection, a classified one.
func TestErrorsHoldNoConfiguration_EveryCall(t *testing.T) {
	for _, in := range standIns() {
		for name, call := range errorCalls(in) {
			t.Run(in.name+"/"+name, func(t *testing.T) { assertCall(t, name, call) })
		}
	}
}

// A join, and a query that groups, that DALgo runs in its own engine or hands to the server, read
// the catalog and the rows through the layers under the adapter, which put the text of an error in
// a message of their own. What comes out holds no part of the configuration either.
func TestErrorsHoldNoConfiguration_JoinsAndGroupedQueries(t *testing.T) {
	id := dal.NewFieldRef("", "id")
	grouped := dal.From(dal.NewRootCollectionRef("orders", "")).NewQuery().GroupBy(id).
		SelectColumns(dal.Column{Expression: id}, dal.Column{Expression: dal.NewAggregate(dal.COUNT, false, dal.Star()), Alias: "n"})
	for _, in := range standIns() {
		for name, query := range map[string]dal.Query{"join": joinQuery(), "grouped": grouped} {
			t.Run(in.name+"/"+name+" on the database", func(t *testing.T) {
				call := in.onLazyDatabase(func(ctx context.Context, db *Database) []error {
					_, records := db.ExecuteQueryToRecordsReader(ctx, query)
					_, recordsets := db.ExecuteQueryToRecordsetReader(ctx, query)
					return []error{records, recordsets}
				})
				call.inChain = true
				assertCall(t, name, call)
			})
			t.Run(in.name+"/"+name+" in a transaction", func(t *testing.T) {
				call := in.inReadonlyTransaction(func(ctx context.Context, tx dal.ReadTransaction) []error {
					_, records := tx.ExecuteQueryToRecordsReader(ctx, query)
					_, recordsets := tx.ExecuteQueryToRecordsetReader(ctx, query)
					return []error{records, recordsets}
				})
				call.inChain = true
				assertCall(t, name, call)
			})
		}
	}
}

// handedOutTypes are the types of the values the adapter hands out, keyed as the table keys its
// calls: the Database, the two values behind its exported DB field (DALgo's layer, and the backend
// under it), the transactions a worker is given and the readers a query returns.
func handedOutTypes(t *testing.T) map[string]reflect.Type {
	t.Helper()
	types := map[string]reflect.Type{"Database": reflect.TypeOf(&Database{})}
	db := transactionalDatabase(t, errors.New("unused"))
	types["DB"] = reflect.TypeOf(db.DB)
	types["Backend"] = reflect.TypeOf(dal.BackendOf(db.DB))
	if err := db.RunReadonlyTransaction(context.Background(), func(_ context.Context, tx dal.ReadTransaction) error {
		types["ReadTransaction"] = reflect.TypeOf(tx)
		return nil
	}); err == nil {
		t.Fatal("the stand-in's commit succeeded")
	}
	if err := db.RunReadwriteTransaction(context.Background(), func(_ context.Context, tx dal.ReadwriteTransaction) error {
		types["ReadwriteTransaction"] = reflect.TypeOf(tx)
		return nil
	}); err == nil {
		t.Fatal("the stand-in's commit succeeded")
	}
	mocked, mock := mockedDatabase(t)
	mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}))
	mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}))
	records, err := mocked.ExecuteQueryToRecordsReader(context.Background(), textQuery())
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	types["RecordsReader"] = reflect.TypeOf(records)
	_ = records.Close()
	recordsets, err := mocked.ExecuteQueryToRecordsetReader(context.Background(), textQuery())
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
	}
	types["RecordsetReader"] = reflect.TypeOf(recordsets)
	_ = recordsets.Close()
	return types
}

// The table names every exported method of the values the adapter hands out that can return an
// error, and every entry of the table is such a method. A method that a release of DALgo adds
// to an interface the adapter hands out (or that this package adds) is found here, whether the
// adapter guards it or not, and fails the test until it is in the table and its stand-in is made.
func TestErrorsHoldNoConfiguration_TableNamesEveryErrorReturningMethod(t *testing.T) {
	table := errorCalls(standIns()[0])
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	methods := map[string]bool{}
	for kind, ty := range handedOutTypes(t) {
		for i := 0; i < ty.NumMethod(); i++ {
			m := ty.Method(i)
			if !m.IsExported() {
				continue
			}
			for j := 0; j < m.Type.NumOut(); j++ {
				if m.Type.Out(j) == errorType {
					methods[kind+"."+m.Name] = true
				}
			}
		}
	}
	var missing, stale []string
	for name := range methods {
		if _, ok := table[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range table {
		if !methods[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("these methods can return an error and are not in the table of calls (add each to errorCalls with a stand-in that makes it fail): %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("these entries of the table of calls are not an error-returning method of a value the adapter hands out: %v", stale)
	}
}

// The declared interfaces the adapter hands out are covered whole: the types above must have at
// least their methods, so that a type that stopped implementing one is noticed here and not by a consumer.
func TestErrorsHoldNoConfiguration_HandedOutValuesHaveTheirInterfacesMethods(t *testing.T) {
	declared := map[string]reflect.Type{
		"DB":                   reflect.TypeOf((*dal.DB)(nil)).Elem(),
		"Backend":              reflect.TypeOf((*dal.Backend)(nil)).Elem(),
		"ReadTransaction":      reflect.TypeOf((*dal.ReadTransaction)(nil)).Elem(),
		"ReadwriteTransaction": reflect.TypeOf((*dal.ReadwriteTransaction)(nil)).Elem(),
		"RecordsReader":        reflect.TypeOf((*dal.RecordsReader)(nil)).Elem(),
		"RecordsetReader":      reflect.TypeOf((*dal.RecordsetReader)(nil)).Elem(),
	}
	handed := handedOutTypes(t)
	for kind, iface := range declared {
		if !handed[kind].Implements(iface) {
			t.Errorf("the %s the adapter hands out (%v) does not implement %v", kind, handed[kind], iface)
		}
	}
}
