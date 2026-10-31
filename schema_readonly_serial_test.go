package dalgo2postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dbschema"
	"github.com/stretchr/testify/require"
)

var errSchemaRowsBusy = errors.New("native connection already owns an open result")

// database/sql serializes individual driver calls, not the interval between
// opening rows and consuming/closing them. Model a native connection that must
// not receive another query while that interval is still owned by its reader.
type schemaBusyTrace struct {
	schemaRecordingTrace
	active   atomic.Bool
	queries  atomic.Int32
	overlaps atomic.Int32
}

type schemaBusyConnector struct{ trace *schemaBusyTrace }

func (c schemaBusyConnector) Connect(context.Context) (driver.Conn, error) {
	return schemaBusyConn{schemaRecordingConn{&c.trace.schemaRecordingTrace}, c.trace}, nil
}
func (schemaBusyConnector) Driver() driver.Driver { return nil }

type schemaBusyConn struct {
	schemaRecordingConn
	trace *schemaBusyTrace
}

func (c schemaBusyConn) QueryContext(ctx context.Context, text string, args []driver.NamedValue) (driver.Rows, error) {
	c.trace.queries.Add(1)
	if c.trace.active.Swap(true) {
		c.trace.overlaps.Add(1)
		return nil, errSchemaRowsBusy
	}
	rows, err := c.schemaRecordingConn.QueryContext(ctx, text, args)
	if err != nil {
		c.trace.active.Store(false)
		return nil, err
	}
	return &schemaBusyRows{Rows: rows, trace: c.trace}, nil
}

type schemaBusyRows struct {
	driver.Rows
	trace *schemaBusyTrace
}

func (r *schemaBusyRows) Close() error {
	err := r.Rows.Close()
	r.trace.active.Store(false)
	return err
}

// Pause outside the driver lock after the first real sql.Tx query opens its
// rows, before the adapter receives them. A second ungated metadata method can
// now reach that same connection, faithfully exposing the ownership overlap.
type pausedSchemaQueries struct {
	catalogQueryer
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (p *pausedSchemaQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := p.catalogQueryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	p.once.Do(func() { close(p.entered); <-p.release })
	return rows, nil
}

func TestReadonlySchemaTransactionSerializesNativeRowsLifetime(t *testing.T) {
	for _, cancelWaiter := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel-waiter"}[cancelWaiter], func(t *testing.T) {
			trace := &schemaBusyTrace{}
			raw := sql.OpenDB(schemaBusyConnector{trace})
			defer func() { require.NoError(t, raw.Close()) }()
			db := &Database{sqlDB: raw}
			err := db.RunReadonlySchemaTransaction(context.Background(), func(ctx context.Context, reader dbschema.SchemaReader) error {
				view := reader.(*readonlySchemaView)
				paused := &pausedSchemaQueries{catalogQueryer: view.db.catalog, entered: make(chan struct{}), release: make(chan struct{})}
				view.db.catalog = paused
				var release sync.Once
				unblock := func() { release.Do(func() { close(paused.release) }) }
				defer unblock()
				first := make(chan error, 1)
				go func() { _, e := view.ListSchemas(ctx); first <- e }()
				<-paused.entered
				secondCtx, stop := context.WithCancel(ctx)
				defer stop()
				second := make(chan error, 1)
				go func() { _, e := view.ListSchemas(secondCtx); second <- e }()
				select {
				case e := <-second:
					t.Fatalf("second metadata call escaped the owned native rows: %v", e)
				case <-time.After(30 * time.Millisecond):
				}
				if cancelWaiter {
					stop()
					require.ErrorIs(t, <-second, context.Canceled)
					require.EqualValues(t, 2, trace.queries.Load(), "only SHOW and the first metadata query may reach the driver")
				}
				unblock()
				require.NoError(t, <-first)
				if !cancelWaiter {
					require.NoError(t, <-second)
					require.EqualValues(t, 3, trace.queries.Load())
				}
				require.Zero(t, trace.overlaps.Load())
				return nil
			})
			require.NoError(t, err)
		})
	}
}

type schemaErrObserver struct {
	context.Context
	once sync.Once
	act  func()
}

func (c *schemaErrObserver) Err() error {
	c.once.Do(c.act)
	return c.Context.Err()
}

func TestReadonlySchemaViewRechecksClosureBeforeDispatch(t *testing.T) {
	view := &readonlySchemaView{}
	// Close in the valid interval after context's initial closed check, before
	// gate acquisition. Err still honestly returns nil for the live context.
	view.lifetime = &schemaErrObserver{Context: context.Background(), act: func() { view.closed.Store(true) }}
	calls := 0
	value, err := schemaValue(context.Background(), view, func(context.Context, *Database) (int, error) {
		calls++
		return 42, nil
	})
	require.ErrorIs(t, err, errSchemaClosed)
	require.Zero(t, value)
	require.Zero(t, calls)
}

func TestReadonlySchemaViewLifetimeCancelsWaiterWithoutWaitingForCurrentRead(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	view := &readonlySchemaView{lifetime: lifetime, cancel: cancel}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	first := make(chan error, 1)
	go func() {
		_, err := schemaValue(context.Background(), view, func(context.Context, *Database) (int, error) {
			close(entered)
			<-release
			return 42, nil
		})
		first <- err
	}()
	<-entered
	var calls atomic.Int32
	second := make(chan error, 1)
	go func() {
		_, err := schemaValue(context.Background(), view, func(context.Context, *Database) (int, error) {
			calls.Add(1)
			return 42, nil
		})
		second <- err
	}()
	cancel()
	select {
	case err := <-second:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("lifetime cancellation waited for the current read")
	}
	require.Zero(t, calls.Load())
	unblock()
	require.ErrorIs(t, <-first, context.Canceled)
}
