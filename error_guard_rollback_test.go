package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dal-go/dalgo/dal"
	dalrecord "github.com/dal-go/record"
)

// A transaction whose worker failed and which cannot then be rolled back (DALgo's rollback error,
// which names both errors and unwraps to neither) is a connection error when either of them is a
// failure of the connection. The error the worker returns is its own: the adapter returns it as it
// is and does not read it for failures.

// transactionKind runs a worker that reads inside a transaction of one kind, and returns what the
// Database's method returned.
type transactionKind struct {
	name  string
	runIn func(ctx context.Context, db *Database, worker func(ctx context.Context, tx dal.ReadSession) error) error
}

// run is runIn under a context that never ends.
func (k transactionKind) run(db *Database, worker func(ctx context.Context, tx dal.ReadSession) error) error {
	return k.runIn(context.Background(), db, worker)
}

func transactionKinds() []transactionKind {
	return []transactionKind{
		{"read transaction", func(ctx context.Context, db *Database, worker func(context.Context, dal.ReadSession) error) error {
			return db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
				return worker(ctx, tx)
			})
		}},
		{"read-write transaction", func(ctx context.Context, db *Database, worker func(context.Context, dal.ReadSession) error) error {
			return db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
				return worker(ctx, tx)
			})
		}},
	}
}

// readsAStatement is a worker that returns the error of its one statement.
func readsAStatement(ctx context.Context, tx dal.ReadSession) error {
	_, err := tx.Exists(ctx, dalrecord.NewKeyWithID("orders", "1"))
	return err
}

// returnsItsOwn is a worker that returns an error of its own and meets no failure.
func returnsItsOwn(own error) func(context.Context, dal.ReadSession) error {
	return func(context.Context, dal.ReadSession) error { return own }
}

// rollbackShape is what DALgo's rollback error answers.
type rollbackShape interface {
	OriginalError() error
	RollbackError() error
}

func TestARollbackThatFailsOnTheConnection(t *testing.T) {
	rollbackConnectFailure := func(t *testing.T) error { return connectFailure(t, refuseEveryDial) }
	for _, kind := range transactionKinds() {
		t.Run(kind.name, func(t *testing.T) {
			t.Run("the statement failed on the connection and the rollback with the driver's connect error", func(t *testing.T) {
				db := rollbackFailingDatabase(t, connectFailure(t, refuseEveryDial), rollbackConnectFailure(t))
				err := kind.run(db, readsAStatement)
				_ = assertAClassifiedConnectionError(t, err)
			})
			t.Run("the statement was answered by a shutdown and the rollback says the connection is closed", func(t *testing.T) {
				db := rollbackFailingDatabase(t, serverError("57P01"), errors.New("conn closed"))
				failure := assertAClassifiedConnectionError(t, kind.run(db, readsAStatement))
				if failure != nil && (failure.Kind != FailureServer || failure.SQLState != "57P01") {
					t.Errorf("got kind %d and SQLSTATE %q, want what the statement met: FailureServer and 57P01", failure.Kind, failure.SQLState)
				}
			})
			t.Run("the worker failed on its own and the rollback on the driver's connect error", func(t *testing.T) {
				db := rollbackFailingDatabase(t, errors.New("unused"), rollbackConnectFailure(t))
				failure := assertAClassifiedConnectionError(t, kind.run(db, returnsItsOwn(errors.New("the worker's own error"))))
				if failure != nil && failure.Kind != FailureNetwork {
					t.Errorf("kind = %d, want FailureNetwork", failure.Kind)
				}
			})
			t.Run("the worker failed on its own and the rollback on a reset", func(t *testing.T) {
				db := rollbackFailingDatabase(t, errors.New("unused"), droppedFailures()[0].err())
				err := kind.run(db, returnsItsOwn(errors.New("the worker's own error")))
				_ = assertAClassifiedConnectionError(t, err)
			})
		})
	}
}

// A rollback that fails for another reason, after a worker that failed for another reason, is
// DALgo's rollback error, naming both errors: the worker's error is the very value the
// worker returned.
func TestARollbackThatFailsForAnotherReasonIsKept(t *testing.T) {
	for _, kind := range transactionKinds() {
		t.Run(kind.name, func(t *testing.T) {
			own, rollback := errors.New("the worker's own error"), errors.New("the transaction was already closed")
			db := rollbackFailingDatabase(t, errors.New("unused"), rollback)
			err := kind.run(db, returnsItsOwn(own))
			var both rollbackShape
			if !errors.As(err, &both) {
				t.Fatalf("err is %T %q, want DALgo's rollback error", err, err)
			}
			if both.OriginalError() != own || both.RollbackError() != rollback { //nolint:errorlint // the same values
				t.Errorf("original = %v, rollback = %v; want the worker's error and the rollback's, as they were",
					both.OriginalError(), both.RollbackError())
			}
		})
	}
}

// The error a worker returns is its own: a worker that reads another database with the driver
// itself, or wraps what it met, gets its error back as it is from both transaction methods, whether
// the transaction ends well or fails to roll back for another reason, and the sentinel it wrapped
// is found by errors.Is. What the transaction adds around it (the begin, the commit, the rollback)
// is guarded.
func TestTheErrorAWorkerReturnsIsItsOwn(t *testing.T) {
	sentinel := errors.New("the caller's own sentinel")
	for _, kind := range transactionKinds() {
		t.Run(kind.name, func(t *testing.T) {
			t.Run("a plain error", func(t *testing.T) {
				own := fmt.Errorf("process the order: %w", sentinel)
				err := kind.run(rollbackFailingDatabase(t, errors.New("unused"), nil), returnsItsOwn(own))
				if err != own { //nolint:errorlint // the same value
					t.Errorf("err = %v, want the worker's error as it returned it", err)
				}
			})
			t.Run("an error that wraps a connect error of a pool of its own", func(t *testing.T) {
				own := fmt.Errorf("%w: %w", sentinel, connectFailure(t, refuseEveryDial))
				err := kind.run(rollbackFailingDatabase(t, errors.New("unused"), nil), returnsItsOwn(own))
				if err != own || !errors.Is(err, sentinel) { //nolint:errorlint // the same value
					t.Errorf("err = %v, want the worker's error as it returned it, and the sentinel found in it", err)
				}
			})
			t.Run("an error that wraps a network error of its own", func(t *testing.T) {
				own := fmt.Errorf("%w: %w", sentinel, droppedFailures()[0].err())
				err := kind.run(rollbackFailingDatabase(t, errors.New("unused"), nil), returnsItsOwn(own))
				if err != own || !errors.Is(err, sentinel) { //nolint:errorlint // the same value
					t.Errorf("err = %v, want the worker's error as it returned it, and the sentinel found in it", err)
				}
			})
			t.Run("an error that cannot be compared", func(t *testing.T) {
				own := uncomparableError{sentinel}
				err := kind.run(rollbackFailingDatabase(t, errors.New("unused"), nil), returnsItsOwn(own))
				if !errors.Is(err, sentinel) {
					t.Errorf("err = %v, want one that matches the sentinel", err)
				}
			})
			t.Run("an error that wraps a network error of its own, the rollback failing for another reason", func(t *testing.T) {
				own := fmt.Errorf("%w: %w", sentinel, droppedFailures()[0].err())
				rollback := errors.New("the transaction was already closed")
				err := kind.run(rollbackFailingDatabase(t, errors.New("unused"), rollback), returnsItsOwn(own))
				var both rollbackShape
				if !errors.As(err, &both) || both.OriginalError() != own { //nolint:errorlint // the same value
					t.Errorf("err = %v, want DALgo's rollback error naming the worker's error as it returned it", err)
				}
			})
		})
	}
}

// uncomparableError is an error value that cannot be compared with ==, as an error that is a slice is.
type uncomparableError []error

func (e uncomparableError) Error() string   { return fmt.Sprint([]error(e)) }
func (e uncomparableError) Unwrap() []error { return e }

// A failure of the commit, and of the begin, is the adapter's own and is guarded.
func TestWhatATransactionAddsAroundTheWorkerIsGuarded(t *testing.T) {
	for _, kind := range transactionKinds() {
		t.Run(kind.name+" commit", func(t *testing.T) {
			db := transactionalDatabase(t, connectFailure(t, refuseEveryDial))
			_ = assertAClassifiedConnectionError(t, kind.run(db, func(context.Context, dal.ReadSession) error { return nil }))
		})
		t.Run(kind.name+" begin", func(t *testing.T) {
			db := lazyDatabase(t, refuseEveryDial)
			_ = assertAClassifiedConnectionError(t, kind.run(db, func(context.Context, dal.ReadSession) error { return nil }))
		})
	}
}

// How the guard reads DALgo's rollback error by itself, whatever made it: each of the two errors it
// names is read, the one that is a failure of the connection (or holds a *ConnectionError already)
// stands for the whole, and the error marked as the worker's own is read only for a
// *ConnectionError it holds already.
func TestConnectionFailureOf_ATransactionThatCouldNotBeRolledBack(t *testing.T) {
	plain := errors.New("a plain error")
	classified := &ConnectionError{Kind: FailureServer, SQLState: "57P01"}
	connectErr := connectFailure(t, refuseEveryDial)
	reset := droppedFailures()[0].err()

	for name, in := range map[string]error{
		"the worker's error holds a classified one":     dal.NewRollbackError(plain, fmt.Errorf("worker: %w", classified)),
		"the rollback's error holds a classified one":   dal.NewRollbackError(fmt.Errorf("rollback: %w", classified), plain),
		"the worker's error is the driver's":            dal.NewRollbackError(plain, connectErr),
		"the rollback's error is the driver's":          dal.NewRollbackError(connectErr, plain),
		"the rollback's error is a reset":               dal.NewRollbackError(reset, plain),
		"there is no worker's error":                    dal.NewRollbackError(connectErr, nil),
		"it is wrapped":                                 fmt.Errorf("transaction: %w", dal.NewRollbackError(connectErr, plain)),
		"it is the worker's error of another":           dal.NewRollbackError(plain, dal.NewRollbackError(connectErr, plain)),
		"the worker's own error holds a classified one": dal.NewRollbackError(plain, markOwn(fmt.Errorf("worker: %w", classified))),
	} {
		t.Run(name, func(t *testing.T) {
			failure := connectionFailureOf(in)
			if failure == nil {
				t.Fatalf("connectionFailureOf(%v) = nil, want a *ConnectionError", in)
			}
			assertHoldsNoConfiguration(t, name, failure)
		})
	}
	t.Run("a classified error is returned as it is", func(t *testing.T) {
		if got := connectionFailureOf(dal.NewRollbackError(plain, fmt.Errorf("worker: %w", classified))); got != classified {
			t.Errorf("got %v, want the *ConnectionError the error holds", got)
		}
	})
	for name, in := range map[string]error{
		"neither is a failure of the connection":                dal.NewRollbackError(plain, plain),
		"the worker's own error is not read for a failure":      dal.NewRollbackError(plain, markOwn(connectErr)),
		"the worker's own network error is not read for one":    dal.NewRollbackError(plain, markOwn(reset)),
		"the worker's own error is not read, the rollback's is": dal.NewRollbackError(plain, markOwn(plain)),
		"an error that is no rollback error, and no failure":    plain,
		"nothing": nil,
		"the worker's own error alone, wrapped by nothing else":  markOwn(connectErr),
		"the worker's own error nested in a rollback error":      dal.NewRollbackError(nil, markOwn(connectErr)),
		"the worker's own error beside a rollback that is plain": dal.NewRollbackError(errors.New("closed"), markOwn(plain)),
	} {
		t.Run("is nil when "+name, func(t *testing.T) {
			if got := connectionFailureOf(in); got != nil {
				t.Errorf("connectionFailureOf(%v) = %v, want nil", in, got)
			}
		})
	}
}

// leaveTransaction returns the worker's error as it is, guards the rest, and keeps DALgo's rollback
// error (with the worker's error unmarked) when neither error it names is a failure of the connection.
func TestLeaveTransaction(t *testing.T) {
	plain, rollback := errors.New("a plain error"), errors.New("the rollback failed")
	if got := leaveTransaction(context.Background(), nil); got != nil {
		t.Errorf("leaveTransaction(nil) = %v", got)
	}
	if got := leaveTransaction(context.Background(), markOwn(plain)); got != plain { //nolint:errorlint // the same value
		t.Errorf("the worker's error = %v, want the same error", got)
	}
	if got := leaveTransaction(context.Background(), plain); got != plain { //nolint:errorlint // the same value
		t.Errorf("an error of the transaction that is no failure = %v, want the same error", got)
	}
	if _, ok := leaveTransaction(context.Background(), fmt.Errorf("failed to commit transaction: %w", droppedFailures()[0].err())).(*ConnectionError); !ok {
		t.Error("a commit that failed on the connection is not a *ConnectionError")
	}
	ownRollback := dal.NewRollbackError(rollback, markOwn(plain))
	var kept rollbackShape
	if !errors.As(leaveTransaction(context.Background(), ownRollback), &kept) || kept.OriginalError() != plain || kept.RollbackError() != rollback { //nolint:errorlint // the same values
		t.Errorf("leaveTransaction(%v) = %v, want a rollback error naming the worker's error as it returned it", ownRollback, leaveTransaction(context.Background(), ownRollback))
	}
	foreign := dal.NewRollbackError(rollback, plain)
	if got := leaveTransaction(context.Background(), foreign); got != foreign { //nolint:errorlint // the same value
		t.Errorf("a rollback error of no worker of this backend = %v, want the same error", got)
	}
	if got := leaveTransaction(context.Background(), dal.NewRollbackError(connectFailure(t, refuseEveryDial), markOwn(plain))); got == nil {
		t.Error("a rollback that failed on the connection is no error")
	} else if _, ok := got.(*ConnectionError); !ok {
		t.Errorf("a rollback that failed on the connection = %T, want a *ConnectionError", got)
	}
}

// The mark on the worker's error says what the error says, whatever shows it.
func TestWorkerErrorSaysWhatTheWorkersErrorSays(t *testing.T) {
	if got := markOwn(errors.New("the worker's text")).Error(); got != "the worker's text" {
		t.Errorf("text = %q", got)
	}
	if markOwn(nil) != nil {
		t.Error("markOwn(nil) is not nil")
	}
}
