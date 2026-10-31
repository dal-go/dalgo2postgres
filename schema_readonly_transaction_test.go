package dalgo2postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func expectSchemaMode(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"transaction_read_only"}).AddRow("on"))
}
func TestReadonlySchemaTransactionPreservesAllNarrowCapabilities(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	d.sqlDB.SetMaxOpenConns(1)
	expectSchemaMode(mock)
	mock.ExpectQuery(listRelationsQuery).WithArgs("sales").WillReturnRows(relationRows().AddRow("Album", false))
	describeAlbum(mock, "sales", "Album", "sales")
	mock.ExpectQuery(qIndexes).WithArgs("sales", "Album").WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	mock.ExpectQuery(`SELECT con.conname`).WithArgs("sales", "Album").WillReturnRows(sqlmock.NewRows([]string{"name", "type"}))
	mock.ExpectQuery(`source.relname AS referrer_table`).WithArgs("sales", "Album").WillReturnRows(sqlmock.NewRows([]string{"oid", "table", "from", "to"}))
	mock.ExpectQuery(listSchemasQuery).WillReturnRows(sqlmock.NewRows([]string{"nspname"}).AddRow("sales"))
	mock.ExpectQuery(listRelationsQuery).WithArgs("other").WillReturnRows(relationRows().AddRow("Other", false))
	mock.ExpectQuery(listRelationsQuery).WithArgs("sales").WillReturnRows(relationRows().AddRow("view", true))
	mock.ExpectQuery(listRelationsQuery).WithArgs("other").WillReturnRows(relationRows().AddRow("view", true))
	mock.ExpectQuery(`pg_get_viewdef`).WithArgs("sales").WillReturnRows(sqlmock.NewRows([]string{"name", "column", "sql"}).AddRow("view", "id", "SELECT 1"))
	mock.ExpectQuery(`collisdeterministic`).WithArgs("sales", "Album").WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow(nil))
	mock.ExpectRollback()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var retained *readonlySchemaView
	err := d.RunReadonlySchemaTransaction(ctx, func(ctx context.Context, reader dbschema.SchemaReader) error {
		view, ok := reader.(*readonlySchemaView)
		require.True(t, ok)
		retained = view
		_, raw := any(reader).(dal.DB)
		require.False(t, raw)
		_, closer := any(reader).(io.Closer)
		require.False(t, closer)
		allowed := map[string]bool{"ListCollections": true, "DescribeCollection": true, "ListIndexes": true, "ListConstraints": true, "ListReferrers": true, "ListSchemas": true, "ListSchemaCollections": true, "ListViews": true, "ListSchemaViews": true, "ListSourceViews": true, "NonDeterministicTextColumns": true}
		surface := reflect.TypeOf(reader)
		require.Equal(t, len(allowed), surface.NumMethod())
		for i := 0; i < surface.NumMethod(); i++ {
			require.True(t, allowed[surface.Method(i).Name], "unexpected exposed capability")
		}
		_, transactional := view.db.catalog.(*sql.Tx)
		require.True(t, transactional)
		require.Nil(t, d.catalog)
		require.Same(t, d.sqlDB, d.catalogQueries())
		collections, err := view.ListCollections(ctx, nil)
		require.NoError(t, err)
		require.Equal(t, "Album", collections[0].Name())
		ref := dal.NewRootCollectionRef("Album", "")
		def, err := view.DescribeCollection(ctx, &ref)
		require.NoError(t, err)
		require.Equal(t, "Album", def.Name)
		_, err = view.ListIndexes(ctx, &ref)
		require.NoError(t, err)
		_, err = view.ListConstraints(ctx, &ref)
		require.NoError(t, err)
		_, err = view.ListReferrers(ctx, &ref)
		require.NoError(t, err)
		schemas, err := view.ListSchemas(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"sales"}, schemas)
		_, err = view.ListSchemaCollections(ctx, "other")
		require.NoError(t, err)
		_, err = view.ListViews(ctx)
		require.NoError(t, err)
		_, err = view.ListSchemaViews(ctx, "other")
		require.NoError(t, err)
		views, err := view.ListSourceViews(ctx)
		require.NoError(t, err)
		require.Equal(t, "view", views[0].Name)
		_, err = view.NonDeterministicTextColumns(ctx, &ref)
		require.NoError(t, err)
		return nil
	})
	require.NoError(t, err)
	require.True(t, retained.closed.Load())
	require.ErrorIs(t, retained.lifetime.Err(), context.Canceled)
	_, err = retained.ListCollections(context.Background(), nil)
	require.ErrorIs(t, err, errSchemaClosed)
}

func TestReadonlySchemaTransactionRefusesBeforeWorker(t *testing.T) {
	cases := map[string]func(sqlmock.Sqlmock){
		"begin": func(m sqlmock.Sqlmock) {
			m.ExpectBegin().WillReturnError(errors.New("sql: driver does not support read-only transactions"))
		},
		"query": func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery("SHOW transaction_read_only").WillReturnError(errors.New("SHOW failure"))
			m.ExpectRollback()
		},
		"off": func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("off"))
			m.ExpectRollback()
		},
		"empty": func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"mode"}))
			m.ExpectRollback()
		},
		"extra": func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("on").AddRow("on"))
			m.ExpectRollback()
		},
		"scan": func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(nil))
			m.ExpectRollback()
		},
		"read": func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("on").RowError(0, errors.New("read failure")))
			m.ExpectRollback()
		},
		"close": func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("on").CloseError(errors.New("close failure")))
			m.ExpectRollback()
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			d, mock := newSchemaMockDatabase(t)
			setup(mock)
			err := d.RunReadonlySchemaTransaction(context.Background(), func(context.Context, dbschema.SchemaReader) error { t.Fatal("unverified callback ran"); return nil })
			require.Error(t, err)
		})
	}
	var nilCtx context.Context
	var nilDB *Database
	for _, d := range []*Database{nilDB, {}} {
		require.ErrorIs(t, d.RunReadonlySchemaTransaction(context.Background(), func(context.Context, dbschema.SchemaReader) error { return nil }), errSchemaReadonly)
	}
	d, _ := newSchemaMockDatabase(t)
	require.ErrorIs(t, d.RunReadonlySchemaTransaction(nilCtx, func(context.Context, dbschema.SchemaReader) error { return nil }), errSchemaReadonly)
	require.ErrorIs(t, d.RunReadonlySchemaTransaction(context.Background(), nil), errSchemaReadonly)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, d.RunReadonlySchemaTransaction(ctx, func(context.Context, dbschema.SchemaReader) error { t.Fatal("cancelled callback"); return nil }), context.Canceled)
}

func TestReadonlySchemaTransactionWorkerAndRollbackErrors(t *testing.T) {
	own := errors.New("caller-owned marker")
	for _, withWorker := range []bool{false, true} {
		for _, rollback := range []error{nil, errors.New("rollback failure"), serverError("28P01")} {
			t.Run(fmt.Sprintf("%t/%v", withWorker, reflect.TypeOf(rollback)), func(t *testing.T) {
				d, mock := newSchemaMockDatabase(t)
				expectSchemaMode(mock)
				mock.ExpectRollback().WillReturnError(rollback)
				err := d.RunReadonlySchemaTransaction(context.Background(), func(context.Context, dbschema.SchemaReader) error {
					if withWorker {
						return own
					}
					return nil
				})
				if rollback == nil {
					if withWorker {
						require.Same(t, own, err)
					} else {
						require.NoError(t, err)
					}
					return
				}
				if _, native := rollback.(*pgconn.PgError); native {
					assertAClassifiedConnectionError(t, err)
					assertHoldsNoConfiguration(t, "schema rollback", err)
					return
				}
				if withWorker {
					ended, ok := err.(endedTransaction)
					require.True(t, ok)
					require.Same(t, own, ended.OriginalError())
					require.Same(t, rollback, ended.RollbackError())
				} else {
					require.Same(t, rollback, err)
				}
			})
		}
	}
	for _, phase := range []string{"begin", "show", "catalog"} {
		t.Run(phase, func(t *testing.T) {
			d, mock := newSchemaMockDatabase(t)
			if phase == "begin" {
				mock.ExpectBegin().WillReturnError(serverError("28P01"))
			} else {
				mock.ExpectBegin()
				if phase == "show" {
					mock.ExpectQuery("SHOW transaction_read_only").WillReturnError(serverError("28P01"))
				} else {
					mock.ExpectQuery("SHOW transaction_read_only").WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow("on"))
					mock.ExpectQuery(listRelationsQuery).WillReturnError(serverError("28P01"))
				}
				mock.ExpectRollback()
			}
			err := d.RunReadonlySchemaTransaction(context.Background(), func(ctx context.Context, reader dbschema.SchemaReader) error {
				_, err := reader.ListCollections(ctx, nil)
				return err
			})
			assertAClassifiedConnectionError(t, err)
			assertHoldsNoConfiguration(t, "schema native error", err)
		})
	}
}

func TestReadonlySchemaTransactionInvalidatesOnPanicAndGoexit(t *testing.T) {
	for _, mode := range []string{"panic", "goexit"} {
		t.Run(mode, func(t *testing.T) {
			d, mock := newSchemaMockDatabase(t)
			expectSchemaMode(mock)
			mock.ExpectRollback()
			var retained *readonlySchemaView
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if mode == "panic" {
						require.Equal(t, "caller panic", recover())
					}
				}()
				_ = d.RunReadonlySchemaTransaction(context.Background(), func(_ context.Context, reader dbschema.SchemaReader) error {
					retained = reader.(*readonlySchemaView)
					if mode == "panic" {
						panic("caller panic")
					}
					runtime.Goexit()
					return nil
				})
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("callback exit leaked transaction")
			}
			require.True(t, retained.closed.Load())
			require.ErrorIs(t, retained.lifetime.Err(), context.Canceled)
		})
	}
}

type schemaRecordingConnector struct{ trace *schemaRecordingTrace }
type schemaRecordingTrace struct {
	mu        sync.Mutex
	begins    []driver.TxOptions
	rollbacks int
	entered   chan context.Context
	hold      bool
	modeClose func()
}

func (c schemaRecordingConnector) Connect(context.Context) (driver.Conn, error) {
	return schemaRecordingConn{c.trace}, nil
}
func (schemaRecordingConnector) Driver() driver.Driver { return nil }

type schemaRecordingConn struct{ trace *schemaRecordingTrace }

func (schemaRecordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (schemaRecordingConn) Close() error              { return nil }
func (schemaRecordingConn) Begin() (driver.Tx, error) { return nil, errors.New("BeginTx required") }
func (c schemaRecordingConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.trace.mu.Lock()
	c.trace.begins = append(c.trace.begins, options)
	c.trace.mu.Unlock()
	return schemaRecordingTx{c.trace}, nil
}
func (c schemaRecordingConn) QueryContext(ctx context.Context, text string, _ []driver.NamedValue) (driver.Rows, error) {
	if text == "SHOW transaction_read_only" {
		return &schemaRecordingRows{columns: []string{"transaction_read_only"}, values: [][]driver.Value{{"on"}}, closeHook: c.trace.modeClose}, nil
	}
	if c.trace.entered != nil {
		c.trace.entered <- ctx
	}
	if c.trace.hold {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &schemaRecordingRows{columns: []string{"nspname"}, values: [][]driver.Value{{"sales"}}}, nil
}

type schemaRecordingTx struct{ trace *schemaRecordingTrace }

func (schemaRecordingTx) Commit() error { return errors.New("schema transaction must never commit") }
func (tx schemaRecordingTx) Rollback() error {
	tx.trace.mu.Lock()
	tx.trace.rollbacks++
	tx.trace.mu.Unlock()
	return nil
}

type schemaRecordingRows struct {
	columns   []string
	values    [][]driver.Value
	closeHook func()
}

func (r *schemaRecordingRows) Columns() []string { return r.columns }
func (r *schemaRecordingRows) Close() error {
	if r.closeHook != nil {
		r.closeHook()
	}
	return nil
}
func (r *schemaRecordingRows) Next(destination []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(destination, r.values[0])
	r.values = r.values[1:]
	return nil
}
func recordingSchemaDB(t *testing.T, trace *schemaRecordingTrace) *Database {
	t.Helper()
	raw := sql.OpenDB(schemaRecordingConnector{trace})
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	return &Database{sqlDB: raw}
}

func TestReadonlySchemaTransactionRecordsNativeReadonlyFlagAndDeadline(t *testing.T) {
	trace := &schemaRecordingTrace{entered: make(chan context.Context, 1)}
	d := recordingSchemaDB(t, trace)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	original, _ := ctx.Deadline()
	require.NoError(t, d.RunReadonlySchemaTransaction(ctx, func(_ context.Context, reader dbschema.SchemaReader) error {
		schemas, err := reader.(*readonlySchemaView).ListSchemas(context.Background())
		require.NoError(t, err)
		require.Equal(t, []string{"sales"}, schemas)
		supplied := <-trace.entered
		deadline, ok := supplied.Deadline()
		require.True(t, ok)
		require.Equal(t, original, deadline)
		return nil
	}))
	trace.mu.Lock()
	defer trace.mu.Unlock()
	require.Len(t, trace.begins, 1)
	require.True(t, trace.begins[0].ReadOnly)
	require.Equal(t, 1, trace.rollbacks)
}
func TestReadonlySchemaTransactionCancelsBackgroundReadsBeforeRollback(t *testing.T) {
	trace := &schemaRecordingTrace{entered: make(chan context.Context, 1), hold: true}
	d := recordingSchemaDB(t, trace)
	readDone := make(chan error, 1)
	require.NoError(t, d.RunReadonlySchemaTransaction(context.Background(), func(_ context.Context, reader dbschema.SchemaReader) error {
		go func() { _, err := reader.(*readonlySchemaView).ListSchemas(context.Background()); readDone <- err }()
		<-trace.entered
		return nil
	}))
	select {
	case err := <-readDone:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Background read detached from callback lifetime")
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	require.Equal(t, 1, trace.rollbacks)
}
func TestReadonlySchemaTransactionOriginalCancellationCannotDetach(t *testing.T) {
	trace := &schemaRecordingTrace{entered: make(chan context.Context, 1), hold: true}
	d := recordingSchemaDB(t, trace)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := d.RunReadonlySchemaTransaction(ctx, func(_ context.Context, reader dbschema.SchemaReader) error {
		done := make(chan error, 1)
		go func() { _, err := reader.(*readonlySchemaView).ListSchemas(context.Background()); done <- err }()
		<-trace.entered
		cancel()
		return <-done
	})
	require.ErrorIs(t, err, context.Canceled)
}
func TestReadonlySchemaViewRefusesMissingCancelledAndClosedContexts(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &readonlySchemaView{lifetime: life, cancel: cancel}
	var nilCtx context.Context
	_, _, err := v.context(nilCtx)
	require.ErrorIs(t, err, errSchemaClosed)
	supplied, stop := context.WithCancel(context.Background())
	stop()
	_, _, err = v.context(supplied)
	require.ErrorIs(t, err, context.Canceled)
	cancel()
	_, _, err = v.context(context.Background())
	require.ErrorIs(t, err, context.Canceled)
	// A value that becomes cancelled while a provider returns must be withheld.
	life, cancel = context.WithCancel(context.Background())
	v = &readonlySchemaView{lifetime: life, cancel: cancel}
	value, err := schemaValue(context.Background(), v, func(context.Context, *Database) (string, error) { cancel(); return "withheld", nil })
	require.Empty(t, value)
	require.ErrorIs(t, err, context.Canceled)
	life, cancel = context.WithCancel(context.Background())
	defer cancel()
	v = &readonlySchemaView{lifetime: life, cancel: cancel}
	supplied, stop = context.WithCancel(context.Background())
	value, err = schemaValue(supplied, v, func(context.Context, *Database) (string, error) { stop(); return "withheld", nil })
	require.Empty(t, value)
	require.ErrorIs(t, err, context.Canceled)
}
func TestReadonlySchemaTransactionsUseIndependentViewsConcurrently(t *testing.T) {
	trace := &schemaRecordingTrace{}
	d := recordingSchemaDB(t, trace)
	entered := make(chan *readonlySchemaView, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			done <- d.RunReadonlySchemaTransaction(context.Background(), func(_ context.Context, reader dbschema.SchemaReader) error {
				entered <- reader.(*readonlySchemaView)
				<-release
				return nil
			})
		}()
	}
	first, second := <-entered, <-entered
	require.NotSame(t, first, second)
	require.NotSame(t, first.db.catalog, second.db.catalog)
	require.Nil(t, d.catalog)
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
}

type schemaCancelOnCheck struct {
	context.Context
	cancel context.CancelFunc
	armed  atomic.Bool
}

func (c *schemaCancelOnCheck) Err() error {
	if c.armed.Swap(false) {
		c.cancel()
	}
	return c.Context.Err()
}
func TestReadonlySchemaTransactionCancellationBeforeWorkerAndOnSuccess(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlled := &schemaCancelOnCheck{Context: base, cancel: cancel}
	trace := &schemaRecordingTrace{modeClose: func() { controlled.armed.Store(true) }}
	d := recordingSchemaDB(t, trace)
	err := d.RunReadonlySchemaTransaction(controlled, func(context.Context, dbschema.SchemaReader) error {
		t.Fatal("callback started after mode verification cancelled its context")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	d = recordingSchemaDB(t, &schemaRecordingTrace{})
	base, cancel = context.WithCancel(context.Background())
	defer cancel()
	err = d.RunReadonlySchemaTransaction(base, func(context.Context, dbschema.SchemaReader) error { cancel(); return nil })
	require.ErrorIs(t, err, context.Canceled)
}
