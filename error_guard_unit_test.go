package dalgo2postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	dalrecord "github.com/dal-go/record"
	"github.com/jackc/pgx/v5/pgconn"
)

// serverError is an error a server answers with, whose message and detail name a role and a
// database, as the messages of the connection classes do.
func serverError(code string) *pgconn.PgError {
	return &pgconn.PgError{
		Severity: "FATAL", Code: code,
		Message: `role "` + markUser + `" cannot use database "` + markDatabase + `"`,
		Detail:  "detail for " + markHost,
	}
}

// What leaveAdapter does to each kind of error: a failure of the connection becomes a
// *ConnectionError that holds nothing of the cause; every other error comes back as the very
// same value.
func TestLeaveAdapter(t *testing.T) {
	connectErr := connectFailure(t, refuseEveryDial)
	parseErr := pgconn.NewParseConfigError("postgres://"+markUser+":"+markPassword+"@"+markHost+"/"+markDatabase, "cannot parse "+markUser, errors.New(markPassword))
	statement := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint", Detail: "Key (id)=(1) already exists."}
	plain := errors.New("a plain error")

	t.Run("nil stays nil", func(t *testing.T) {
		if got := leaveAdapter(nil); got != nil {
			t.Errorf("leaveAdapter(nil) = %v, want nil", got)
		}
	})
	for name, in := range map[string]error{
		"a plain error":                         plain,
		"a statement error":                     statement,
		"a statement error, wrapped":            fmt.Errorf("insert: %w", statement),
		"dal.ErrNoMoreRecords":                  dal.ErrNoMoreRecords,
		"a context error":                       context.Canceled,
		"an error that holds a ConnectionError": fmt.Errorf("worker: %w", &ConnectionError{Kind: FailureNetwork}),
	} {
		t.Run("is returned as it is: "+name, func(t *testing.T) {
			if got := leaveAdapter(in); got != in { //nolint:errorlint // the same value
				t.Errorf("leaveAdapter(%v) = %v, want the same error", in, got)
			}
		})
	}

	for name, in := range map[string]error{
		"the driver's connect error":          connectErr,
		"the driver's configuration error":    parseErr,
		"a connect error, wrapped":            fmt.Errorf("failed to begin transaction: %w", connectErr),
		"a connect error, twice wrapped":      fmt.Errorf("a: %w", fmt.Errorf("b: %w", connectErr)),
		"a connect error, joined with others": errors.Join(plain, fmt.Errorf("c: %w", connectErr)),
		"a configuration error, wrapped":      fmt.Errorf("open: %w", parseErr),
	} {
		t.Run("becomes a connection error: "+name, func(t *testing.T) {
			got := leaveAdapter(in)
			failure, ok := got.(*ConnectionError)
			if !ok {
				t.Fatalf("leaveAdapter(%v) = %T, want a *ConnectionError", in, got)
			}
			if failure.Host != "" || failure.Port != "" || failure.Database != "" {
				t.Errorf("a failure after the open names %+v, want no host, port or database", *failure)
			}
			assertHoldsNoConfiguration(t, name, got)
			if inner := errors.Unwrap(got); inner != nil {
				t.Errorf("errors.Unwrap = %T, want nil", inner)
			}
		})
	}

	t.Run("a configuration error is an invalid connection string", func(t *testing.T) {
		got := leaveAdapter(parseErr).(*ConnectionError)
		if got.Kind != FailureInvalidDSN || got.SQLState != "" {
			t.Errorf("got %+v, want FailureInvalidDSN and no SQLSTATE", *got)
		}
	})
	t.Run("a refused dial is a network failure with its fixed sentence", func(t *testing.T) {
		got := leaveAdapter(connectErr)
		if want := "dalgo2postgres: the server could not be reached"; got.Error() != want {
			t.Errorf("text = %q, want %q", got, want)
		}
	})
}

// A server answer of a connection class is a connection error that keeps only its SQLSTATE; the
// server's message and detail, which name a role or a database, are not kept. An answer of any
// other class is the server's, unchanged.
func TestLeaveAdapter_ServerAnswersByClass(t *testing.T) {
	for _, code := range []string{
		"08000", "08001", "08003", "08004", "08006", "08007", "08P01", // class 08 whole
		"28000", "28P01", // class 28 whole
		"3D000", "53300", "57P01", "57P02", "57P03",
	} {
		t.Run("classified "+code, func(t *testing.T) {
			for _, in := range []error{serverError(code), fmt.Errorf("query: %w", serverError(code))} {
				got, ok := leaveAdapter(in).(*ConnectionError)
				if !ok {
					t.Fatalf("leaveAdapter(%v) is not a *ConnectionError", in)
				}
				if got.Kind != FailureServer || got.SQLState != code {
					t.Errorf("got kind %d and SQLSTATE %q, want FailureServer and %q", got.Kind, got.SQLState, code)
				}
				assertHoldsNoConfiguration(t, code, got)
				if !strings.Contains(got.Error(), "(SQLSTATE "+code+")") {
					t.Errorf("text %q does not say the SQLSTATE %s", got, code)
				}
			}
		})
	}
	for _, code := range []string{
		"23505", "23503", "23514", "22P02", "42601", "42P01", "42703", "42501", "40001", "40P01", "XX000",
		"53000", "53100", "53200", "53400", "57000", "57014", "57P04", "57P05", "25P03", "25P04", "3F000", "0A000", "",
	} {
		t.Run("not classified "+code, func(t *testing.T) {
			in := serverError(code)
			if got := leaveAdapter(in); got != error(in) { //nolint:errorlint // the same value
				t.Errorf("leaveAdapter(%v) = %v, want the server's error unchanged", in, got)
			}
		})
	}
	t.Run("a code of the wrong shape that claims a class is classified and keeps no code", func(t *testing.T) {
		got, ok := leaveAdapter(serverError("28P01 " + markPassword)).(*ConnectionError)
		if !ok || got.Kind != FailureServer || got.SQLState != "" {
			t.Fatalf("got %#v, want a FailureServer error with no SQLSTATE", got)
		}
		assertHoldsNoConfiguration(t, "wrong shape", got)
	})
	t.Run("the sentence of each code with one is the connection error's own", func(t *testing.T) {
		for code, sentence := range serverSentences {
			got := leaveAdapter(serverError(code))
			if want := "dalgo2postgres: " + sentence + " (SQLSTATE " + code + ")"; got.Error() != want {
				t.Errorf("text = %q, want %q", got, want)
			}
		}
	})
}

// A connection that ends because its context ended still answers errors.Is for the context's
// error, and only for that one.
func TestLeaveAdapter_ContextErrorsStayFindable(t *testing.T) {
	for name, tc := range map[string]struct {
		cause              error
		canceled, deadline bool
	}{
		"canceled":          {fmt.Errorf("dial: %w", context.Canceled), true, false},
		"deadline exceeded": {fmt.Errorf("dial: %w", context.DeadlineExceeded), false, true},
		"neither":           {errors.New("dial refused"), false, false},
	} {
		t.Run(name, func(t *testing.T) {
			dial := func(context.Context, string, string) (net.Conn, error) { return nil, tc.cause }
			got := leaveAdapter(connectFailure(t, dial))
			if _, ok := got.(*ConnectionError); !ok {
				t.Fatalf("got %T, want a *ConnectionError", got)
			}
			if errors.Is(got, context.Canceled) != tc.canceled || errors.Is(got, context.DeadlineExceeded) != tc.deadline {
				t.Errorf("errors.Is(canceled) = %v, errors.Is(deadline) = %v; want %v and %v",
					errors.Is(got, context.Canceled), errors.Is(got, context.DeadlineExceeded), tc.canceled, tc.deadline)
			}
			assertHoldsNoConfiguration(t, name, got)
		})
	}
	t.Run("through a call", func(t *testing.T) {
		db := lazyDatabase(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
			return nil, fmt.Errorf("dial: %w", context.DeadlineExceeded)
		})
		_, err := db.ListSchemas(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want one that matches context.DeadlineExceeded", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("err = %v matches context.Canceled too", err)
		}
	})
}

// The error of a connection that fails at the open is built the same way, so it answers
// errors.Is for its context's error too, and its text and fields are what they were.
func TestDescribe_ContextErrorsAreFindableAtTheOpenToo(t *testing.T) {
	failure := dsnInfo{}.describe("PingContext", fmt.Errorf("ping: %w", context.DeadlineExceeded))
	if !errors.Is(failure, context.DeadlineExceeded) || errors.Is(failure, context.Canceled) {
		t.Errorf("errors.Is: deadline %v, canceled %v; want true and false",
			errors.Is(failure, context.DeadlineExceeded), errors.Is(failure, context.Canceled))
	}
	if want := "dalgo2postgres: PingContext: the connection timed out or was canceled"; failure.Error() != want {
		t.Errorf("text = %q, want %q", failure, want)
	}
	if errors.Unwrap(failure) != nil {
		t.Error("the open's error unwraps to its cause")
	}
}

// The server's answers to a connection, in pgx's own shape (the error of a connection attempt that
// the server refused, over an in-memory pipe), are classified at any call, name nothing and keep
// their SQLSTATE.
func TestServerAnswersToAConnectionAreClassifiedAtAnyCall(t *testing.T) {
	for _, tc := range []struct {
		code string
		text string
	}{
		{"28P01", `password authentication failed for user "` + markUser + `"`},
		{"28000", `role "` + markUser + `" is not permitted to log in`},
		{"3D000", `database "` + markDatabase + `" does not exist`},
		{"53300", `too many connections for role "` + markUser + `"`},
		{"57P03", "the database system is starting up"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			raw := connectFailure(t, serverAnswers(tc.code, tc.text))
			var serverErr *pgconn.PgError
			if !errors.As(raw, &serverErr) || serverErr.Code != tc.code {
				t.Fatalf("the stand-in's failure is %v, want one that holds the server's error %s", raw, tc.code)
			}
			db := lazyDatabase(t, serverAnswers(tc.code, tc.text))
			_, err := db.ListSchemas(context.Background())
			failure, ok := err.(*ConnectionError)
			if !ok {
				t.Fatalf("err is %T %q, want a *ConnectionError", err, err)
			}
			if failure.Kind != FailureServer || failure.SQLState != tc.code {
				t.Errorf("got kind %d and SQLSTATE %q, want FailureServer and %q", failure.Kind, failure.SQLState, tc.code)
			}
			assertHoldsNoConfiguration(t, tc.code, err)
		})
	}
}

// A connection that ends mid-session (the server was shut down) is a connection error at any call.
func TestAServerShutdownAtACallIsAConnectionError(t *testing.T) {
	db, mock := mockedDatabase(t)
	mock.ExpectQuery("").WillReturnError(serverError("57P01"))
	_, err := db.ListSchemas(context.Background())
	failure, ok := err.(*ConnectionError)
	if !ok || failure.Kind != FailureServer || failure.SQLState != "57P01" {
		t.Fatalf("err = %#v, want a FailureServer error with SQLSTATE 57P01", err)
	}
	assertHoldsNoConfiguration(t, "57P01", err)
}

// A statement error is the server's answer to the statement: it is not changed, its message and
// detail are the server's, and the code and the type still tell what it is.
func TestStatementErrorsAreNotChanged(t *testing.T) {
	violation := &pgconn.PgError{
		Severity: "ERROR", Code: "23505",
		Message:        `duplicate key value violates unique constraint "orders_pkey"`,
		Detail:         "Key (id)=(1) already exists.",
		ConstraintName: "orders_pkey",
	}
	db, mock := mockedDatabase(t)
	mock.ExpectQuery("").WillReturnError(violation)
	mock.ExpectQuery("").WillReturnError(fmt.Errorf("wrapped by the layer under: %w", violation))
	mock.ExpectQuery("").WillReturnError(violation)

	_, existsErr := db.Exists(context.Background(), dalrecord.NewKeyWithID("orders", "1"))
	_, listErr := db.ListSchemas(context.Background())
	rec := unreadRecord()
	getErr := db.Get(context.Background(), rec)

	for name, err := range map[string]error{"Exists": existsErr, "ListSchemas": listErr, "Get": getErr} {
		var serverErr *pgconn.PgError
		if !errors.As(err, &serverErr) || serverErr != violation {
			t.Errorf("%s: err = %v, want the server's own *pgconn.PgError", name, err)
			continue
		}
		if !strings.Contains(err.Error(), violation.Message) {
			t.Errorf("%s: the text %q lost the server's message", name, err)
		}
		if _, isConnection := err.(*ConnectionError); isConnection {
			t.Errorf("%s: a statement error was made a connection error", name)
		}
	}
	if recErr := rec.Error(); recErr == nil || !errors.Is(recErr, getErr) {
		t.Errorf("the record's error = %v, want the error the call returned", recErr)
	}
	if !IsAlreadyExists(getErr) {
		t.Error("IsAlreadyExists no longer recognises a unique violation that passed the adapter")
	}
}

// A record that was not found stays not found, and a failure of the connection is put on the
// record in its place, so that printing the record's error, or the panic of Data on a record that
// has one, holds no configuration.
func TestRecordErrors(t *testing.T) {
	t.Run("a failure of the connection is replaced on every record", func(t *testing.T) {
		db := lazyDatabase(t, refuseEveryDial)
		records := []dalrecord.Record{unreadRecord(), dalrecord.NewRecordWithData(dalrecord.NewKeyWithID("orders", "2"), map[string]any{})}
		err := db.GetMulti(context.Background(), records)
		for i, rec := range records {
			if err == nil && rec.Error() == nil {
				t.Fatalf("record %d has no error, and the call returned none", i)
			}
			if recErr := rec.Error(); recErr != nil {
				assertHoldsNoConfiguration(t, fmt.Sprintf("record %d", i), recErr)
			}
		}
		if err != nil {
			assertHoldsNoConfiguration(t, "GetMulti", err)
		}
	})
	t.Run("what Data says of a record whose read failed holds no configuration", func(t *testing.T) {
		db := lazyDatabase(t, refuseEveryDial)
		rec := unreadRecord()
		_ = db.Get(context.Background(), rec)
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("Data on a record that has an error did not panic")
			}
			assertHoldsNoConfiguration(t, "the panic of Data", fmt.Errorf("%v", recovered))
		}()
		_ = rec.Data()
	})
	t.Run("a record that was not found stays not found", func(t *testing.T) {
		db, mock := mockedDatabase(t)
		mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"status"}))
		rec := unreadRecord()
		err := db.Get(context.Background(), rec)
		if !dalrecord.IsNotFound(err) {
			t.Errorf("err = %v, want not found", err)
		}
		if rec.Exists() {
			t.Error("the record exists")
		}
	})
	t.Run("a record read well has no error", func(t *testing.T) {
		db, mock := mockedDatabase(t)
		mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("new"))
		rec := unreadRecord()
		if err := db.Get(context.Background(), rec); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !rec.Exists() || rec.Error() != nil {
			t.Errorf("exists = %v, error = %v; want a record that exists", rec.Exists(), rec.Error())
		}
	})
}

// errPlainFailure is a failure of a statement that is no failure of the connection.
var errPlainFailure = errors.New("a plain failure")

// What a worker is given keeps what the transaction under it has: its options, its ID, the legacy
// read method and the end of a result.
func TestGuardedTransactionsKeepWhatTheyHad(t *testing.T) {
	ctx := context.Background()
	t.Run("read transaction", func(t *testing.T) {
		db, mock := mockedDatabase(t)
		mock.ExpectBegin()
		mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
		mock.ExpectQuery("").WillReturnError(errPlainFailure)
		mock.ExpectCommit()
		err := db.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
			if tx.Options() == nil || !tx.Options().IsReadonly() {
				t.Errorf("Options = %v, want the read-only options of the transaction", tx.Options())
			}
			reader, selectErr := tx.(interface {
				Select(context.Context, dal.Query) (dal.Reader, error)
			}).Select(ctx, textQuery())
			if selectErr != nil || reader == nil {
				t.Fatalf("Select = %v, %v; want a reader", reader, selectErr)
			}
			_, _ = reader.(dal.RecordsReader).Next()
			if _, nextErr := reader.(dal.RecordsReader).Next(); !errors.Is(nextErr, dal.ErrNoMoreRecords) {
				t.Errorf("the end of a result = %v, want dal.ErrNoMoreRecords", nextErr)
			}
			_ = reader.Close()
			if reader, selectErr = tx.(interface {
				Select(context.Context, dal.Query) (dal.Reader, error)
			}).Select(ctx, textQuery()); reader != nil || !errors.Is(selectErr, errPlainFailure) {
				t.Errorf("a failed Select = %v, %v; want a nil reader and the error of the statement", reader, selectErr)
			}
			return nil
		})
		if err != nil {
			t.Errorf("RunReadonlyTransaction = %v", err)
		}
	})
	t.Run("read-write transaction", func(t *testing.T) {
		db, mock := mockedDatabase(t)
		mock.ExpectBegin()
		mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
		mock.ExpectQuery("").WillReturnError(errPlainFailure)
		mock.ExpectRollback()
		workerErr := errors.New("the worker's own error")
		err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			_ = tx.ID()
			if tx.Options() == nil {
				t.Error("Options is nil")
			}
			selector := tx.(interface {
				Select(context.Context, dal.Query) (dal.Reader, error)
			})
			reader, selectErr := selector.Select(ctx, textQuery())
			if selectErr != nil || reader == nil {
				t.Fatalf("Select = %v, %v; want a reader", reader, selectErr)
			}
			_ = reader.Close()
			reader, selectErr = selector.Select(ctx, textQuery())
			if reader != nil || !errors.Is(selectErr, errPlainFailure) {
				t.Errorf("a failed Select = %v, %v; want a nil reader and the error of the statement", reader, selectErr)
			}
			return workerErr
		})
		if !errors.Is(err, workerErr) {
			t.Errorf("RunReadwriteTransaction = %v, want the worker's own error unchanged", err)
		}
	})
}

// The readers keep the end of a result and the recordset, and a successful close is nil.
func TestGuardedReadersKeepWhatTheyHad(t *testing.T) {
	db, mock := mockedDatabase(t)
	mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow([]byte("1")))
	records, err := db.ExecuteQueryToRecordsReader(context.Background(), textQuery())
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	if rec, err := records.Next(); err != nil || rec == nil {
		t.Errorf("Next = %v, %v; want a record", rec, err)
	}
	if _, err := records.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
		t.Errorf("the end of a result = %v, want dal.ErrNoMoreRecords", err)
	}
	if err := records.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	sets, err := db.ExecuteQueryToRecordsetReader(context.Background(), textQuery())
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
	}
	if sets.Recordset() == nil {
		t.Error("the recordset reader has no recordset")
	}
	if _, _, err := sets.Next(); err != nil {
		t.Errorf("Next = %v, want a row", err)
	}
	if _, _, err := sets.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
		t.Errorf("the end of a result = %v, want dal.ErrNoMoreRecords", err)
	}
	if err := sets.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// The error of a call that takes no connection to fail is the one it had.
func TestGuardedCallsThatNeverReachAConnectionKeepTheirErrors(t *testing.T) {
	ctx := context.Background()
	var zero Database
	if err := zero.Close(); err != nil {
		t.Errorf("Close of a Database that was never opened = %v", err)
	}
	if err := zero.UpdateRecord(ctx, ordersRecord(), nil); !errors.Is(err, dal.ErrNotImplementedYet) {
		t.Errorf("UpdateRecord = %v, want dal.ErrNotImplementedYet", err)
	}
	key := dalrecord.NewKeyWithID("orders", "1")
	// A Database that has no backend to write through says so for every write.
	for name, err := range map[string]error{
		"Set":         zero.Set(ctx, ordersRecord()),
		"SetMulti":    zero.SetMulti(ctx, []dalrecord.Record{ordersRecord()}),
		"Insert":      zero.Insert(ctx, ordersRecord()),
		"Upsert":      zero.Upsert(ctx, ordersRecord()),
		"Delete":      zero.Delete(ctx, key),
		"DeleteMulti": zero.DeleteMulti(ctx, []*dalrecord.Key{key}),
		"Update":      zero.Update(ctx, key, someUpdates()),
		"UpdateMulti": zero.UpdateMulti(ctx, []*dalrecord.Key{key}, someUpdates()),
	} {
		if !errors.Is(err, dal.ErrNotImplementedYet) {
			t.Errorf("%s on a Database with no backend = %v, want dal.ErrNotImplementedYet", name, err)
		}
	}
}
