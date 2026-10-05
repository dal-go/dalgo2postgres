package dalgo2postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	dalrecord "github.com/dal-go/record"
)

// The guard sits under DALgo's framework layer, on the backend, so that the transaction a worker
// is given is DALgo's own over a guarded one. These tests pin what that placement must keep: the
// answers of the backend, the one layer of validation and hooks, dal.WithoutValidation, and the
// declining of what a backend under the guard does not say.

// strictOrder is record data that DALgo validates before it is written.
type strictOrder struct{ Name string }

func (o *strictOrder) Validate() error {
	if o.Name == "" {
		return errors.New("an order needs a name")
	}
	return nil
}

func invalidOrder() dalrecord.Record {
	return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("orders", "1"), &strictOrder{})
}

// countedSaves counts the writes that reach the before-save hooks of DALgo's write pipeline. The
// registry is process-wide and append-only, so the hook is registered once and stays.
var (
	countedSaves   atomic.Int64
	registerCounts sync.Once
)

// A write in a transaction is validated once, by DALgo's framework layer, and its hooks run once;
// dal.WithoutValidation reaches the statement.
func TestGuardedBackend_KeepsValidationAndWithoutValidation(t *testing.T) {
	ctx := context.Background()
	registerCounts.Do(func() {
		dal.AddBeforeSaveHook(func(context.Context, dalrecord.Record) error { countedSaves.Add(1); return nil })
	})
	t.Run("a write that fails validation never reaches the server", func(t *testing.T) {
		db, mock := mockedDatabase(t)
		mock.ExpectBegin()
		mock.ExpectRollback()
		err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			return tx.Insert(ctx, invalidOrder())
		})
		if err == nil || !strings.Contains(err.Error(), "an order needs a name") {
			t.Errorf("err = %v, want the validation error", err)
		}
	})
	t.Run("dal.WithoutValidation reaches the server", func(t *testing.T) {
		db, mock := mockedDatabase(t)
		mock.ExpectBegin()
		mock.ExpectExec("").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
		err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			return dal.WithoutValidation(tx).Insert(ctx, invalidOrder())
		})
		if err != nil {
			t.Errorf("err = %v, want the write to reach the server", err)
		}
	})
	t.Run("a hook runs once for a write in a transaction", func(t *testing.T) {
		db, mock := mockedDatabase(t)
		mock.ExpectBegin()
		mock.ExpectExec("").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
		before := countedSaves.Load()
		err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			return tx.Insert(ctx, dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("orders", "1"), &strictOrder{Name: "n"}))
		})
		if runs := countedSaves.Load() - before; err != nil || runs != 1 {
			t.Errorf("err = %v, hook runs = %d; want no error and one run", err, runs)
		}
	})
}

// The backend answers what the backend under it answers.
func TestGuardedBackend_AnswersAsTheBackendUnderIt(t *testing.T) {
	db, _ := mockedDatabase(t)
	if name := db.DB.Adapter().Name(); name != "dalgo2sql" {
		t.Errorf("adapter of the delegate = %q, want dalgo2sql", name)
	}
	if db.DB.ID() != "" || db.DB.Schema() == nil || !db.DB.SupportsConcurrentConnections() {
		t.Errorf("ID = %q, schema = %v, concurrent = %v", db.DB.ID(), db.DB.Schema(), db.DB.SupportsConcurrentConnections())
	}
	if got := db.QueryCapabilities(); !got.GroupBy || !got.OrderBy {
		t.Errorf("QueryCapabilities = %+v, want what the PostgreSQL dialect runs", got)
	}
}

// The backend has no InsertMulti and no UpdateRecord, so DALgo does not take it for one that
// writes outside a transaction through its own pipeline: the writes of the Database outside a
// transaction go to the statement as they always have.
func TestGuardedBackend_IsNotAWriteSession(t *testing.T) {
	if _, ok := any(newGuardedBackend(nil)).(dal.WriteSession); ok {
		t.Error("the guarded backend is a dal.WriteSession: DALgo would put a validation pipeline on the writes outside a transaction")
	}
	db, mock := mockedDatabase(t)
	mock.ExpectExec("").WillReturnResult(sqlmock.NewResult(1, 1))
	if err := db.Insert(context.Background(), invalidOrder()); err != nil {
		t.Errorf("Insert outside a transaction = %v, want the statement to run", err)
	}
}

// bareBackend is a backend that says nothing optional, and bareTransaction a transaction.
type (
	bareBackend     struct{ dal.Backend }
	bareTransaction struct{ dal.ReadTransaction }
)

// What the backend under the guard does not say, the guard does not say either: it declares no
// capability, declines a join, supplies no fields and has no write outside a transaction.
func TestGuardedBackend_DeclinesWhatTheBackendUnderItDoesNotSay(t *testing.T) {
	ctx := context.Background()
	backend := newGuardedBackend(bareBackend{})
	if got := backend.QueryCapabilities(); got != (dal.QueryCapabilities{}) {
		t.Errorf("QueryCapabilities = %+v, want none", got)
	}
	if err := backend.CanExecuteJoin(ctx, joinQuery()); !errors.Is(err, dal.ErrNotSupported) {
		t.Errorf("CanExecuteJoin = %v, want dal.ErrNotSupported", err)
	}
	if fields, err := backend.JoinFields(ctx, dal.NewRootCollectionRef("orders", "a")); fields != nil || err != nil {
		t.Errorf("JoinFields = %v, %v; want no fields and no error", fields, err)
	}
	key := dalrecord.NewKeyWithID("orders", "1")
	for name, err := range map[string]error{
		"Set":         backend.Set(ctx, ordersRecord()),
		"SetMulti":    backend.SetMulti(ctx, []dalrecord.Record{ordersRecord()}),
		"Insert":      backend.Insert(ctx, ordersRecord()),
		"Upsert":      backend.Upsert(ctx, ordersRecord()),
		"Delete":      backend.Delete(ctx, key),
		"DeleteMulti": backend.DeleteMulti(ctx, []*dalrecord.Key{key}),
		"Update":      backend.Update(ctx, key, someUpdates()),
		"UpdateMulti": backend.UpdateMulti(ctx, []*dalrecord.Key{key}, someUpdates()),
	} {
		if !errors.Is(err, dal.ErrNotImplementedYet) {
			t.Errorf("%s = %v, want dal.ErrNotImplementedYet", name, err)
		}
	}
	// The same for a transaction that answers neither question.
	tx := newGuardedReadTransaction(bareTransaction{})
	if err := tx.CanExecuteJoin(ctx, joinQuery()); !errors.Is(err, dal.ErrNotSupported) {
		t.Errorf("a transaction's CanExecuteJoin = %v, want dal.ErrNotSupported", err)
	}
	if fields, err := tx.JoinFields(ctx, dal.NewRootCollectionRef("orders", "a")); fields != nil || err != nil {
		t.Errorf("a transaction's JoinFields = %v, %v; want no fields and no error", fields, err)
	}
}

// joiningInner answers the two questions about a join as a backend or a transaction that has a
// view of the schema does.
type joiningInner struct {
	canExecute error
	fields     []string
	fieldsErr  error
}

func (j joiningInner) CanExecuteJoin(context.Context, dal.StructuredQuery) error { return j.canExecute }
func (j joiningInner) JoinFields(context.Context, dal.RecordsetSource) ([]string, error) {
	return j.fields, j.fieldsErr
}

// The questions about a join are the answers of the value under the guard, with their errors guarded.
func TestGuardedJoins_ForwardTheAnswersOfTheValueUnderThem(t *testing.T) {
	ctx := context.Background()
	source := dal.NewRootCollectionRef("orders", "a")
	answers := guardedJoins{joiningInner{fields: []string{"id", "status"}}}
	if err := answers.CanExecuteJoin(ctx, joinQuery()); err != nil {
		t.Errorf("CanExecuteJoin = %v, want the join accepted", err)
	}
	if fields, err := answers.JoinFields(ctx, source); err != nil || len(fields) != 2 || fields[0] != "id" {
		t.Errorf("JoinFields = %v, %v; want the fields of the value under the guard", fields, err)
	}
	declined := dal.ErrNotSupported
	if err := (guardedJoins{joiningInner{canExecute: declined}}).CanExecuteJoin(ctx, joinQuery()); !errors.Is(err, declined) {
		t.Errorf("CanExecuteJoin = %v, want the decline unchanged", err)
	}
	failure := connectFailure(t, refuseEveryDial)
	failing := guardedJoins{joiningInner{canExecute: failure, fieldsErr: failure}}
	err := failing.CanExecuteJoin(ctx, joinQuery())
	if _, ok := err.(*ConnectionError); !ok {
		t.Errorf("CanExecuteJoin = %T %v, want a *ConnectionError", err, err)
	}
	assertHoldsNoConfiguration(t, "CanExecuteJoin", err)
	_, err = failing.JoinFields(ctx, source)
	if _, ok := err.(*ConnectionError); !ok {
		t.Errorf("JoinFields = %T %v, want a *ConnectionError", err, err)
	}
	assertHoldsNoConfiguration(t, "JoinFields", err)
}
