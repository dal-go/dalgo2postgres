package dalgo2postgres

import (
	"errors"
	"strings"

	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5/pgconn"
)

// Every error that leaves this package passes [leaveAdapter]. A connection that fails, at
// any call, is reported as a [*ConnectionError]: a fixed sentence chosen by the kind of
// failure, the SQLSTATE of a server answer, and nothing of the cause. The rule is kept in
// one place on purpose: the exported methods of [Database] call it, and so do the methods of
// the backend, the transactions and the readers under DALgo's framework layer (see
// error_guard_values.go), which are the values handed out whose methods can return an error.
// TestErrorsHoldNoConfiguration_TableNamesEveryErrorReturningMethod fails when a method that
// can return an error is not covered.

// connectionStates are the SQLSTATE codes, besides the whole of classes 08 (connection
// exception) and 28 (invalid authorization specification), with which a server answers a
// connection, or ends one. They were checked against the PostgreSQL error-codes appendix:
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

// connectionFailureOf returns the [*ConnectionError] that stands for err when err is a
// failure of the connection, and nil otherwise. It is one when its chain holds the driver's
// connect error or its configuration error (each holds the configuration, and pgx's text
// names the user and the database), or a server answer of a connection class. The chain is
// walked through Unwrap() error and Unwrap() []error, so an error of one attempt among
// several is found. An error that holds a [*ConnectionError] already is not one: it is
// safe as it is.
func connectionFailureOf(err error) *ConnectionError {
	if err == nil {
		return nil
	}
	var connectErr *pgconn.ConnectError
	var configErr *pgconn.ParseConfigError
	if errors.As(err, &connectErr) || errors.As(err, &configErr) || anyInTree(err, answersAConnection) {
		return newConnectionError("", err)
	}
	return nil
}

// answersAConnection reports whether err is a server error of a connection class.
func answersAConnection(err error) bool {
	serverErr, ok := err.(*pgconn.PgError)
	return ok && isConnectionState(serverErr.Code)
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
