package dalgo2postgres

import (
	"errors"
	"io"
	"net"
	"strings"

	"github.com/dal-go/dalgo/dal"
	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5/pgconn"
)

// Every error of the adapter that leaves this package passes [leaveAdapter]. A connection that
// fails, at any call, is reported as a [*ConnectionError]: a fixed sentence chosen by the kind
// of failure, the SQLSTATE of a server answer, and nothing of the cause. The rule is kept in
// one place on purpose: the exported methods of [Database] call it, and so do the methods of
// the backend, the transactions and the readers under DALgo's framework layer (see
// error_guard_values.go), which are the values handed out whose methods can return an error.
// The one error that does not pass it is the one a worker returns from a transaction: it is the
// caller's own, not the adapter's, and comes back as it is (see [leaveTransaction]).
// TestErrorsHoldNoConfiguration_TableNamesEveryErrorReturningMethod fails when a method that
// can return an error is not covered.

// connectionStates are the SQLSTATE codes, besides the whole of classes 08 (connection
// exception) and 28 (invalid authorization specification), with which a server answers a
// connection, or ends one. Class 08 is taken whole, 08P01 (protocol_violation) included, although
// a server also reports 08P01 for a statement's own message (a wrong number of bound parameters,
// under an execution mode that sends them as written): at a connection the same code carries a
// message that names a setting the startup sent, so the code is never copied with its message.
// The codes below were checked against the PostgreSQL error-codes appendix:
//
//   - 3D000 invalid_catalog_name: the database does not exist. The message names it.
//   - 53300 too_many_connections: the message names the role or the database whose limit
//     was reached.
//   - 57P01 admin_shutdown, 57P02 crash_shutdown and 57P03 cannot_connect_now: the server
//     ends a session, or is not accepting one.
//
// Other codes of the appendix that also end a session (57P04 database_dropped, 57P05
// idle_session_timeout, 25P03 idle_in_transaction_session_timeout and 25P04
// transaction_timeout) name nothing in their messages and are left as the server's
// answers, as every statement error is. The messages of 28000, 28P01, 3D000 and 53300
// name a role or a database, which is why those are never copied.
var connectionStates = map[string]bool{
	"3D000": true,
	"53300": true,
	"57P01": true,
	"57P02": true,
	"57P03": true,
}

// isConnectionState reports whether a SQLSTATE code answers a connection: it is of class 08
// or 28, or one of [connectionStates]. The class is read from the first two bytes as sent, so
// a code of the wrong shape that claims a class is not trusted either.
func isConnectionState(code string) bool {
	return strings.HasPrefix(code, "08") || strings.HasPrefix(code, "28") || connectionStates[code]
}

// isConnectionFailure reports whether err itself, not what it wraps, is a failure of the connection:
//
//   - the driver's connect error or its configuration error, each of which holds the configuration
//     (and pgx's text names the user and the database);
//   - a server answer of a connection class (see [isConnectionState]);
//   - an error of the socket (*net.OpError: a reset, a broken pipe, a timeout, a TLS alert), the end
//     of the stream (io.ErrUnexpectedEOF, which is what pgx makes of a read that ends in the middle
//     of a message or between two) and the driver's closed connection (pgconn.ErrConnClosed): how a
//     connection that was established fails when it is lost. These hold no user, password or
//     database, but the error of a socket names the addresses of both ends.
//
// An error that merely answers net.Error is not one: context.DeadlineExceeded does, and a statement
// that ends on its context is the context's error, which the driver reports as such.
func isConnectionFailure(err error) bool {
	switch e := err.(type) {
	case *pgconn.ConnectError, *pgconn.ParseConfigError, *net.OpError:
		return true
	case *pgconn.PgError:
		return isConnectionState(e.Code)
	}
	return err == io.ErrUnexpectedEOF || err == pgconn.ErrConnClosed //nolint:errorlint // the sentinels themselves: the walk visits every error of the chain
}

// endedTransaction is the error of a transaction whose worker failed and which then could not be
// rolled back (dal.NewRollbackError). It names the worker's error and the rollback's, and has no
// Unwrap, so a walk of the chain does not reach either.
type endedTransaction interface {
	OriginalError() error
	RollbackError() error
}

// connectionFailureOf returns the [*ConnectionError] that stands for err when err is a failure of
// the connection, and nil otherwise. It is one when its chain holds an error that
// [isConnectionFailure] says is one. The chain is walked through Unwrap() error and
// Unwrap() []error, so an error of one attempt among several is found, and the kind and SQLSTATE
// code of the result are those of the error that matched, not of another in the same chain. An
// error that holds a [*ConnectionError] already is not one: it is safe as it is. The error of a
// transaction that could not be rolled back is read through the two errors it names (see
// [failureOfPart]).
func connectionFailureOf(err error) *ConnectionError {
	if err == nil {
		return nil
	}
	var ended endedTransaction
	if errors.As(err, &ended) {
		for _, part := range []error{ended.OriginalError(), ended.RollbackError()} {
			if failure := failureOfPart(part); failure != nil {
				return failure
			}
		}
	}
	if matched := findInTree(err, isConnectionFailure); matched != nil {
		return newConnectionErrorFrom("", matched, err)
	}
	return nil
}

// failureOfPart is the [*ConnectionError] that stands for one of the two errors an
// [endedTransaction] names: the one it already holds, else the one [connectionFailureOf] makes of
// it. The error the worker returned (see [workerError]) is the caller's own and is read for the
// one it holds already only, and not for a failure of its own.
func failureOfPart(part error) *ConnectionError {
	var held *ConnectionError
	if own, isOwn := part.(workerError); isOwn {
		if errors.As(own.err, &held) {
			return held
		}
		return nil
	}
	if errors.As(part, &held) {
		return held
	}
	return connectionFailureOf(part)
}

// leaveAdapter is the one function every error passes before it leaves this package. An
// error that stands for a failure of the connection (see [connectionFailureOf]) becomes a
// [*ConnectionError], which holds nothing of it. Any other error is returned as it is, the
// same value: a statement error is the server's answer to the statement and is not changed
// (its text can hold the server's message and detail), and DALgo's own errors
// (dal.ErrNoMoreRecords, a record not found) keep matching what they matched.
func leaveAdapter(err error) error {
	if failure := connectionFailureOf(err); failure != nil {
		return failure
	}
	return err
}

// workerError marks the error a worker returned from a transaction. It is the caller's own, not
// an error of the adapter, so it is returned as it is, whatever it holds: a worker that reads
// another database with the driver itself, or wraps what it met, finds its own error (and the
// sentinel it wrapped) in what the transaction method returns. It has no Unwrap on purpose, so that
// no walk of an error chain reaches what it holds, and it never leaves the package: see
// [leaveTransaction].
type workerError struct{ err error }

func (w workerError) Error() string { return w.err.Error() }

// markOwn marks the error a worker returned as the caller's own; nil stays nil.
func markOwn(err error) error {
	if err == nil {
		return nil
	}
	return workerError{err}
}

// leaveTransaction is [leaveAdapter] for the error a transaction method of the backend returns,
// whose worker returned errors marked by [markOwn]. What the transaction adds around the worker
// (the begin, the commit, the rollback) passes [leaveAdapter]; the error the worker returned is
// returned as it is. When the worker failed and the transaction could not then be rolled back, the
// error is the one [connectionFailureOf] reads from both (a failure of the connection in either),
// and otherwise DALgo's rollback error naming the two errors as they were.
func leaveTransaction(err error) error {
	switch e := err.(type) {
	case nil:
		return nil
	case workerError:
		return e.err
	case endedTransaction:
		if failure := connectionFailureOf(err); failure != nil {
			return failure
		}
		if own, isOwn := e.OriginalError().(workerError); isOwn {
			return dal.NewRollbackError(e.RollbackError(), own.err)
		}
		return err
	}
	return leaveAdapter(err)
}

// guarded is [leaveAdapter] for a call that answers a value and an error.
func guarded[T any](value T, err error) (T, error) { return value, leaveAdapter(err) }

// leaveRecords passes through [leaveAdapter] the error a call put on each of records. A read
// that fails puts its error on the record it was to fill, and the record prints it when it is
// asked for its data or for whether it exists. A record that was not found, or that has no
// error, is left as it is.
func leaveRecords(records ...dalrecord.Record) {
	for _, record := range records {
		if failure := connectionFailureOf(record.Error()); failure != nil {
			record.SetError(failure)
		}
	}
}

// leaveCall is [leaveAdapter] for a call that was given records and answers an error: the
// errors the call put on the records pass too.
func leaveCall(err error, records ...dalrecord.Record) error {
	leaveRecords(records...)
	return leaveAdapter(err)
}
