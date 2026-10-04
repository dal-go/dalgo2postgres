// Package dalgo2postgres is the PostgreSQL-specific DALgo driver.
//
// It composes [github.com/dal-go/dalgo2sql] for the [dal.DB] read/write
// surface (transactions, recordset reader, Get/Set/Insert/Delete) and
// adds PostgreSQL-native implementations of:
//
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

	dal.DB         // delegate for the dal.DB surface
	sqlDB  *sql.DB // direct handle for DDL + introspection queries
	dsn    string  // remembered for diagnostics

	schema         string         // schema the reader inspects; "" means DefaultSchema
	identifierMode IdentifierMode // how reader table names are matched
}

// NewDatabase opens a connection to the PostgreSQL server identified by dsn
// using github.com/jackc/pgx/v5/stdlib (pure Go, CGO_ENABLED=0), pings to
// surface connectivity errors at construction time, wraps the *sql.DB via
// dalgo2sql.NewDatabase for the dal.DB surface, and returns a *Database
// that satisfies dal.DB + dal.ConcurrencyAware.
//
// Use [NewDatabaseWithOptions] when you need to supply per-collection
// primary-key metadata (required for Insert/Get/Delete with map[string]any data).
// The optional [Option] values choose the schema and table-name matching the
// schema reader uses.
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
// inspects ([WithSchema]) and whether it matches table names exactly
// ([WithIdentifierMode]). A nil option is ignored; an unknown IdentifierMode is
// an error, returned before any connection is attempted.
func NewDatabaseWithOptions(dsn string, schema dal.Schema, opts dalgo2sql.DbOptions, options ...Option) (*Database, error) {
	if err := checkOptions(options); err != nil {
		return nil, err
	}
	applyPostgresDbOptionDefaults(&opts)

	sqlDB, err := openVerified(dsn, sql.Open)
	if err != nil {
		return nil, err
	}
	return newDatabaseFromSQL(sqlDB, dsn, schema, opts, options), nil
}

// sqlOpener is [sql.Open]; it is a seam so tests can make the driver fail.
type sqlOpener func(driverName, dataSourceName string) (*sql.DB, error)

// openVerified opens dsn through open and pings it. No part of the connection
// string other than the host and the database name reaches an error's text: a
// DSN that cannot be parsed is named not at all, and the driver's own error
// text, which may echo the string, is redacted (see [connectionError]).
func openVerified(dsn string, open sqlOpener) (*sql.DB, error) {
	details := inspectDSN(dsn)
	if !details.parsed {
		return nil, details.unparsable()
	}
	sqlDB, err := open("pgx", dsn)
	if err != nil {
		return nil, details.wrap("sql.Open", err)
	}
	if pingErr := sqlDB.PingContext(context.Background()); pingErr != nil {
		_ = sqlDB.Close()
		return nil, details.wrap("PingContext", pingErr)
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
	return d.sqlDB.Close()
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
