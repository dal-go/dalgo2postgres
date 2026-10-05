// Package dalgo2postgres is the PostgreSQL-specific DALgo driver.
//
// It composes [github.com/dal-go/dalgo2sql] for the [dal.DB] read/write
// surface (transactions, recordset reader, Get/Set/Insert/Delete) and
// adds PostgreSQL-native implementations of:
//
//   - structured queries, which dalgo2sql compiles with its typed PostgreSQL
//     compiler (every value a bound argument, every name quoted) and which run
//     filtering, ordering, grouping, aggregation and joins on the server; the
//     constructors force the dialect, so the legacy text emitter is never reached,
//     and [*Database] declares what the dialect runs on the server
//     ([dal.QueryCapabilitiesProvider], [dal.NativeJoinProvider] and
//     [dal.JoinFieldsProvider])
//   - [dbschema.SchemaReader] for schema introspection via information_schema
//     and pg_indexes
//   - [ddl.SchemaModifier] for PostgreSQL-flavored CREATE / DROP / ALTER
//   - [dal.ConcurrencyAware] returning true (PostgreSQL supports concurrent
//     connections from multiple goroutines and processes)
//   - [IsAlreadyExists], a [dalgo2sql.DbOptions.IsAlreadyExists] classifier
//     that reports a PostgreSQL unique-key violation (SQLSTATE 23505) so
//     Insert rejects a duplicate key with record.IsAlreadyExists
package dalgo2postgres

import (
	"context"
	"database/sql"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" driver (pure Go, CGO_ENABLED=0)
)

// Database is the dalgo2postgres driver instance. It implements [dal.DB] by
// embedding a [dal.DB] obtained from [dalgo2sql.NewDatabase], and adds
// PostgreSQL-specific dbschema, ddl, and concurrency surfaces.
//
// The embedded dal.DB (rather than a named field) is what lets Database
// satisfy dal.DB itself: dal.DB is sealed by an unexported marker method,
// and embedding is the only way for that method to be promoted onto a
// decorating type — see dal.NewDB's doc comment.
//
// Construct via [NewDatabase]. Database values are safe for concurrent
// use — the underlying PostgreSQL server and pgx connection pool both
// support concurrent connections from multiple goroutines.
type Database struct {
	dal.ConcurrencyAvailable // SupportsConcurrentConnections() = true

	dal.DB         // delegate for the dal.DB surface; its errors are not guarded: call the methods of the Database
	sqlDB  *sql.DB // direct handle for DDL + introspection queries

	schema            string         // schema the reader inspects; "" means DefaultSchema
	identifierMode    IdentifierMode // how names are matched by the reader and written by queries
	identifierModeSet bool           // WithIdentifierMode was given (the zero mode is a real mode)
}

// NewDatabase opens a connection to the PostgreSQL server identified by dsn
// using github.com/jackc/pgx/v5/stdlib (pure Go, CGO_ENABLED=0), pings to
// surface connectivity errors at construction time, wraps the *sql.DB via
// dalgo2sql.NewDatabase for the dal.DB surface, and returns a *Database
// that satisfies dal.DB + dal.ConcurrencyAware.
//
// A failure to open or reach the server is a [*ConnectionError]; its text holds
// nothing but the host, port and database name the driver parsed (each only when
// it passes a strict check), and no other text of dsn or of the driver. A
// connection that fails at a later call is a [*ConnectionError] too, naming no part
// of the configuration.
//
// Use [NewDatabaseWithOptions] when you need to supply per-collection
// primary-key metadata (required for Insert/Get/Delete with map[string]any data).
// The optional [Option] values choose the schema the schema reader inspects and
// how names are matched and written ([IdentifierMode]: lower-cased by default).
func NewDatabase(dsn string, options ...Option) (*Database, error) {
	return NewDatabaseWithOptions(dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, options...)
}

// NewDatabaseWithOptions is like [NewDatabase] but accepts a dal.Schema and
// dalgo2sql.DbOptions so callers can configure per-collection primary-key
// mappings required by Insert/Get/Delete operations.
//
// Example — open a DB whose "widgets" table has "id" as its primary key:
//
//	db, err := dalgo2postgres.NewDatabaseWithOptions(dsn, dal.NewSchema(nil, nil),
//	    dalgo2sql.DbOptions{
//	        Recordsets: map[string]*dalgo2sql.Recordset{
//	            "widgets": dalgo2sql.NewRecordset("widgets", dalgo2sql.Table,
//	                []dal.FieldRef{dal.Field("id")}),
//	        },
//	    })
//
// The optional [Option] values choose the PostgreSQL schema the schema reader
// inspects ([WithSchema]) and whether names are matched by the reader and written
// by structured queries exactly or lower-cased ([WithIdentifierMode]); the mode
// may also be given as opts.IdentifierCase, but not as two different values. A nil
// option is ignored; an unknown IdentifierMode or IdentifierCase, and two that
// disagree, are an error, returned before any connection is attempted.
//
// Three fields of opts are replaced whatever the caller set: Placeholder (always
// dollar markers), StructuredQueryDialect (always "postgres", so no structured
// query reaches dalgo2sql's legacy text emitter) and IdentifierCase (resolved from
// the mode above). IsAlreadyExists is defaulted only when nil. A caller's own
// compiler is not accepted beside the dialect: NativeStructuredQueryCompiler,
// NativeJoinEligibility and NativeJoinHintTranslator are an error too, returned before
// any connection is attempted.
//
// A failure to open or reach the server is a [*ConnectionError], as for
// [NewDatabase].
func NewDatabaseWithOptions(dsn string, schema dal.Schema, opts dalgo2sql.DbOptions, options ...Option) (*Database, error) {
	return newDatabase(dsn, schema, opts, options, sql.Open)
}

// newDatabase is the body of [NewDatabaseWithOptions], taking the function that
// opens the *sql.DB so tests can make the driver fail in every way a driver does.
func newDatabase(dsn string, schema dal.Schema, opts dalgo2sql.DbOptions, options []Option, open sqlOpener) (*Database, error) {
	resolved, err := resolveSettings(opts, options)
	if err != nil {
		return nil, err
	}
	sqlDB, err := openVerified(dsn, open)
	if err != nil {
		return nil, err
	}
	return newDatabaseFromSQL(sqlDB, schema, resolved, options), nil
}

// sqlOpener is [sql.Open]; it is a seam so tests can make the driver fail.
type sqlOpener func(driverName, dataSourceName string) (*sql.DB, error)

// openVerified opens dsn through open and pings it. A failure is a
// [*ConnectionError]: its text is a fixed sentence chosen by the kind of failure
// (told from the types in the driver's error, never from its text), plus the
// host, port and database name of the driver's own parsed configuration, each only
// when it passes a strict check and repeats neither the user name nor the
// password. No other text of dsn and no message of the driver or the server is
// copied into it, and the driver's error is not reachable from it: both hold the
// credentials, whatever separator a string uses and however long a name is.
//
// A string the driver would misread (a quoted URL, a leading space, another
// scheme, a key=value string whose host, user or database holds an equals sign)
// is refused before the driver is asked, with a [FailureMisread] error: the text
// the driver takes for a setting name holds the credentials, and the server it
// reaches would receive it.
//
// The driver is asked even when pgx cannot parse dsn: the string may be a name
// registered with stdlib.RegisterConnConfig, which only the driver knows.
func openVerified(dsn string, open sqlOpener) (*sql.DB, error) {
	info := inspectDSN(dsn)
	if info.misread {
		return nil, &ConnectionError{Kind: FailureMisread}
	}
	sqlDB, err := open("pgx", dsn)
	if err != nil {
		return nil, info.describe("sql.Open", err)
	}
	if pingErr := sqlDB.PingContext(context.Background()); pingErr != nil {
		_ = sqlDB.Close()
		return nil, info.describe("PingContext", pingErr)
	}
	return sqlDB, nil
}

// Close closes the underlying *sql.DB. After Close the Database value
// is unusable; further method calls will fail with an error from
// database/sql.
func (d *Database) Close() error {
	if d.sqlDB == nil {
		return nil
	}
	return leaveAdapter(d.sqlDB.Close())
}

// SupportsConcurrentConnections reports PostgreSQL's own concurrency
// behaviour (always true — see dal.ConcurrencyAvailable), not dalgo2sql's.
// An explicit method is required here: dal.ConcurrencyAvailable and the
// embedded dal.DB (whose Backend requirement embeds dal.ConcurrencyAware)
// both declare this method at the same promotion depth, which Go otherwise
// treats as an ambiguous selector.
func (d *Database) SupportsConcurrentConnections() bool {
	return d.ConcurrencyAvailable.SupportsConcurrentConnections()
}

// ID returns the driver-issued database ID (delegated to dalgo2sql).
func (d *Database) ID() string { return d.DB.ID() }

// Adapter returns the driver/version identifier.
func (d *Database) Adapter() dal.Adapter {
	return dal.NewAdapter("dalgo2postgres", Version)
}

// Schema returns the dal-level Schema (delegated to dalgo2sql).
func (d *Database) Schema() dal.Schema { return d.DB.Schema() }
