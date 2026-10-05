package dalgo2postgres

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// The tests of this file give the adapter a connection that was established and is lost in the
// middle of a call, through pgx itself: the server is a few lines over an in-memory pipe (nothing
// is dialled) that completes the startup, answers begin, commit and rollback as a server inside
// a transaction does, and does what the test says with the first statement it meets.

// sessionServer is a dial function whose server opens a session over an in-memory pipe and then
// serves the transaction control statements; onStatement is given the first other message and
// the pipe, and decides what the server does with it.
func sessionServer(onStatement func(backend *pgproto3.Backend, server net.Conn)) pgconn.DialFunc {
	return func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		// A test that stops half way must not hold the pipe for ever.
		deadline := time.Now().Add(30 * time.Second)
		_ = client.SetDeadline(deadline)
		_ = server.SetDeadline(deadline)
		go func() {
			defer func() { _ = server.Close() }()
			backend := pgproto3.NewBackend(server, server)
			if _, err := backend.ReceiveStartupMessage(); err != nil {
				return
			}
			backend.Send(&pgproto3.AuthenticationOk{})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if backend.Flush() != nil {
				return
			}
			for {
				msg, err := backend.Receive()
				if err != nil {
					return
				}
				query, isQuery := msg.(*pgproto3.Query)
				if !isQuery {
					onStatement(backend, server)
					return
				}
				switch sql := strings.ToLower(query.String); {
				case strings.HasPrefix(sql, "begin"):
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
				default: // commit and rollback
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.ToUpper(sql))})
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				}
				if backend.Flush() != nil {
					return
				}
			}
		}()
		return client, nil
	}
}

// dropsTheConnection is a server that ends the session when it meets a statement.
func dropsTheConnection(*pgproto3.Backend, net.Conn) {}

// shutsDownWith returns a server that answers a statement with a FATAL error of the given code and
// ends the session, as a server does that is shut down or whose session was ended.
func shutsDownWith(code string) func(*pgproto3.Backend, net.Conn) {
	return func(backend *pgproto3.Backend, _ net.Conn) {
		backend.Send(&pgproto3.ErrorResponse{
			Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code,
			Message: `terminating the connection of role "` + markUser + `"`,
		})
		_ = backend.Flush()
	}
}

// A connection that was established and drops in the middle of a call (the server ends the
// session, or the network resets it) is a connection error at any call: pgx returns the error of
// the read as it is, and it is not a connect error.
func TestADroppedConnectionIsAConnectionError(t *testing.T) {
	ctx := context.Background()
	t.Run("on the handle", func(t *testing.T) {
		db := lazyDatabase(t, sessionServer(dropsTheConnection))
		_, err := db.ListSchemas(ctx)
		if _, ok := err.(*ConnectionError); !ok {
			t.Fatalf("err is %T %q, want a *ConnectionError", err, err)
		}
		assertHoldsNoConfiguration(t, "ListSchemas", err)
	})
	t.Run("in a read transaction", func(t *testing.T) {
		db := lazyDatabase(t, sessionServer(dropsTheConnection))
		var inTx error
		err := db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
			_, inTx = tx.ExecuteQueryToRecordsReader(ctx, textQuery())
			return inTx
		})
		for what, e := range map[string]error{"the call in the transaction": inTx, "the transaction": err} {
			if _, ok := e.(*ConnectionError); !ok {
				t.Errorf("%s: err is %T %q, want a *ConnectionError", what, e, e)
			}
		}
	})
}

// A statement that ends because its context was canceled is the context's error: a
// reset or a closed connection is a failure of the connection, and a canceled call is not.
func TestAStatementThatEndsOnItsContextStaysAContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	met := make(chan struct{})
	// The server never answers: the statement ends when the test cancels its context, once the
	// server has the statement.
	waits := func(_ *pgproto3.Backend, server net.Conn) {
		close(met)
		buf := make([]byte, 1)
		_, _ = server.Read(buf) // until the client closes the pipe or the deadline of the pipe
	}
	db := lazyDatabase(t, sessionServer(waits))
	go func() {
		<-met
		cancel()
	}()
	_, err := db.ListSchemas(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want one that matches context.Canceled", err)
	}
	if _, ok := err.(*ConnectionError); ok {
		t.Errorf("a canceled statement was made a connection error: %v", err)
	}
}

// A transaction whose worker failed on a lost connection, and which cannot then be rolled back
// because the connection is gone (it always cannot), is a connection error from both transaction
// methods, whichever way the connection was lost, and holds nothing of what the worker met.
func TestATransactionLostInTheMiddleIsAConnectionError(t *testing.T) {
	ctx := context.Background()
	for name, onStatement := range map[string]func(*pgproto3.Backend, net.Conn){
		"the server shuts down": shutsDownWith("57P01"),
		"the server drops":      dropsTheConnection,
	} {
		t.Run(name+" in a read transaction", func(t *testing.T) {
			db := lazyDatabase(t, sessionServer(onStatement))
			err := db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
				_, err := tx.ExecuteQueryToRecordsReader(ctx, textQuery())
				return err
			})
			_ = assertAClassifiedConnectionError(t, err)
		})
		t.Run(name+" in a read-write transaction", func(t *testing.T) {
			db := lazyDatabase(t, sessionServer(onStatement))
			err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
				return tx.Insert(ctx, ordersRecord())
			})
			_ = assertAClassifiedConnectionError(t, err)
		})
	}
}

// assertAClassifiedConnectionError fails unless err is a *ConnectionError that holds no part of
// the configuration, and returns it (nil when it is not one).
func assertAClassifiedConnectionError(t *testing.T, err error) *ConnectionError {
	t.Helper()
	failure, ok := err.(*ConnectionError)
	if !ok {
		t.Errorf("err is %T %q, want a *ConnectionError", err, err)
	}
	assertHoldsNoConfiguration(t, "the transaction", err)
	var serverErr *pgconn.PgError
	if errors.As(err, &serverErr) {
		t.Errorf("errors.As finds the server's error: %v", serverErr)
	}
	return failure
}
