package dalgo2postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// A statement of a transaction that ends because its context was canceled or its deadline passed
// is the context's error: the driver closes the connection of such a statement and database/sql
// rolls the transaction back from a goroutine of its own, so the rollback the adapter then asks for
// finds either a transaction that is finished or a connection that is closed. Which of the two it
// finds depends on scheduling; neither is a failure of the connection, and the result is the same.

// holdsTheFirstStatement is a server that keeps the first statement it meets unanswered, and
// reports that it has it.
func holdsTheFirstStatement() (server func(*pgproto3.Backend, net.Conn), met <-chan struct{}) {
	once, reached := sync.Once{}, make(chan struct{})
	return func(_ *pgproto3.Backend, conn net.Conn) {
		once.Do(func() { close(reached) })
		_, _ = conn.Read(make([]byte, 1)) // until the client closes the pipe, or the deadline of the pipe
	}, reached
}

// queriesUnderItsContext is a worker that returns the error of a statement it runs under the
// context of the transaction.
func queriesUnderItsContext(ctx context.Context, tx dal.ReadSession) error {
	_, err := tx.ExecuteQueryToRecordsReader(ctx, textQuery())
	return err
}

// endingContext is a context that ends while the server holds the statement of the worker.
type endingContext struct {
	name string
	// want is the context error the call is to answer.
	want error
	// start returns the context a call is made under; met is closed when the server has the statement.
	start func(met <-chan struct{}) (context.Context, context.CancelFunc)
}

func endingContexts() []endingContext {
	return []endingContext{
		{"canceled", context.Canceled, func(met <-chan struct{}) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-met:
					cancel()
				case <-ctx.Done():
				}
			}()
			return ctx, cancel
		}},
		// The deadline is short enough to pass while the server holds the statement; a run in which it
		// passes earlier ends in the same error, only not at the statement.
		{"deadline passed", context.DeadlineExceeded, func(<-chan struct{}) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 10*time.Millisecond)
		}},
	}
}

// runsOfAContextThatEnds is how many times each case is made. The order of the rollback and of the
// close of the connection is the scheduler's, and one processor makes both orders occur.
const runsOfAContextThatEnds = 60

func TestATransactionWhoseStatementEndsOnItsContextIsTheContextsError(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, ending := range endingContexts() {
		for _, kind := range transactionKinds() {
			t.Run(ending.name+" in a "+kind.name, func(t *testing.T) {
				wrong := 0
				var first error
				for range runsOfAContextThatEnds {
					server, met := holdsTheFirstStatement()
					db := lazyDatabase(t, sessionServer(server))
					ctx, cancel := ending.start(met)
					err := kind.runIn(ctx, db, queriesUnderItsContext)
					cancel()
					if !isTheContextsError(err, ending.want) {
						if wrong++; first == nil {
							first = err
						}
					}
				}
				if wrong > 0 {
					t.Errorf("%d of %d runs did not end in the context's error %v; the first ended in %T %q",
						wrong, runsOfAContextThatEnds, ending.want, first, first)
				}
			})
		}
	}
}

// isTheContextsError reports whether err is findable as the context's error and is not a
// connection error of another kind.
func isTheContextsError(err error, want error) bool {
	if !errors.Is(err, want) {
		return false
	}
	var failure *ConnectionError
	return !errors.As(err, &failure) || failure.Kind == FailureTimeout
}

// endedContext returns a context that has ended with the given error: canceled, or its deadline passed.
func endedContext(deadline bool) context.Context {
	if deadline {
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
		cancel()
		return ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// When the context of a transaction has ended and the worker failed, the transaction method returns
// the worker's error as it is, whatever the rollback said: the rollback finds a transaction that
// database/sql has rolled back (sql.ErrTxDone), or a connection the driver has closed (pgx's "rollback
// failed: conn closed"), whichever came first, and neither is a failure of the connection.
func TestLeaveTransaction_AContextThatEndedIsTheWorkersError(t *testing.T) {
	rollbacks := map[string]error{
		"the transaction is finished":     sql.ErrTxDone,
		"the connection is closed":        fmt.Errorf("rollback failed: %w", pgconn.ErrConnClosed),
		"the stream of the rollback ends": fmt.Errorf("rollback failed: %w", io.ErrUnexpectedEOF),
		"a reset":                         droppedFailures()[0].err(),
		"the driver's connect error":      connectFailure(t, refuseEveryDial),
		"another error":                   errors.New("the rollback failed"),
	}
	for _, deadline := range []bool{false, true} {
		ctx, want := endedContext(deadline), context.Canceled
		if deadline {
			want = context.DeadlineExceeded
		}
		worker := fmt.Errorf("failed to get SQL reader: %w", want)
		for name, rollback := range rollbacks {
			t.Run(fmt.Sprintf("%v, deadline %v: %s", want, deadline, name), func(t *testing.T) {
				got := leaveTransaction(ctx, dal.NewRollbackError(rollback, markOwn(worker)))
				if got != worker { //nolint:errorlint // the same value
					t.Errorf("got %T %q, want the worker's error as it returned it", got, got)
				}
				if !errors.Is(got, want) {
					t.Errorf("errors.Is(%v) is false", want)
				}
			})
		}
	}
	t.Run("a worker's error that is not the context's is returned as it is too", func(t *testing.T) {
		worker := errors.New("the worker's own error")
		got := leaveTransaction(endedContext(false), dal.NewRollbackError(fmt.Errorf("rollback failed: %w", pgconn.ErrConnClosed), markOwn(worker)))
		if got != worker { //nolint:errorlint // the same value
			t.Errorf("got %T %q, want the worker's error as it returned it", got, got)
		}
	})
	t.Run("a classified error the worker holds is kept", func(t *testing.T) {
		held := &ConnectionError{Kind: FailureServer, SQLState: "57P01"}
		worker := fmt.Errorf("worker: %w", held)
		got := leaveTransaction(endedContext(false), dal.NewRollbackError(sql.ErrTxDone, markOwn(worker)))
		var found *ConnectionError
		if got != worker || !errors.As(got, &found) || found != held { //nolint:errorlint // the same values
			t.Errorf("got %T %q, want the worker's error, which holds the classified one", got, got)
		}
	})
}

// A context that has not ended changes nothing: a rollback that fails on the connection is a
// connection error, whatever the worker returned, and a worker that holds a context error of its
// own (a context it made itself) does not make the rollback's failure the context's.
func TestLeaveTransaction_AContextThatHasNotEndedChangesNothing(t *testing.T) {
	worker := fmt.Errorf("failed to get SQL reader: %w", context.Canceled)
	for name, rollback := range map[string]error{
		"the connection is closed": fmt.Errorf("rollback failed: %w", pgconn.ErrConnClosed),
		"a reset":                  droppedFailures()[0].err(),
	} {
		t.Run(name, func(t *testing.T) {
			got := leaveTransaction(context.Background(), dal.NewRollbackError(rollback, markOwn(worker)))
			if _, ok := got.(*ConnectionError); !ok {
				t.Errorf("got %T %q, want a *ConnectionError", got, got)
			}
		})
	}
	t.Run("a finished transaction is DALgo's rollback error, naming both", func(t *testing.T) {
		got := leaveTransaction(context.Background(), dal.NewRollbackError(sql.ErrTxDone, markOwn(worker)))
		var both rollbackShape
		if !errors.As(got, &both) || both.OriginalError() != worker || both.RollbackError() != sql.ErrTxDone { //nolint:errorlint // the same values
			t.Errorf("got %T %q, want DALgo's rollback error naming the worker's error and the rollback's", got, got)
		}
	})
}

// What a transaction adds around the worker is guarded whether or not its context has ended, and an
// error that is no rollback error of a worker is not read as one.
func TestLeaveTransaction_AContextThatEndedLeavesTheRestAlone(t *testing.T) {
	ended := endedContext(false)
	commit := fmt.Errorf("failed to commit transaction: %w", droppedFailures()[0].err())
	if _, ok := leaveTransaction(ended, commit).(*ConnectionError); !ok {
		t.Error("a commit that failed on the connection is not a *ConnectionError")
	}
	foreign := dal.NewRollbackError(sql.ErrTxDone, errors.New("an error that is no worker's"))
	if got := leaveTransaction(ended, foreign); got != foreign { //nolint:errorlint // the same value
		t.Errorf("a rollback error of no worker of this backend = %v, want the same error", got)
	}
	closed := dal.NewRollbackError(fmt.Errorf("rollback failed: %w", pgconn.ErrConnClosed), nil)
	if _, ok := leaveTransaction(ended, closed).(*ConnectionError); !ok {
		t.Error("a rollback with no worker's error, that failed on a closed connection, is not a *ConnectionError")
	}
	plainWorker := errors.New("the worker's own error")
	if got := leaveTransaction(ended, markOwn(plainWorker)); got != plainWorker { //nolint:errorlint // the same value
		t.Errorf("the worker's error = %v, want the same error", got)
	}
}
