package dalgo2postgres

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// DefaultSchema is the PostgreSQL schema the schema reader inspects when
// [WithSchema] is not used.
const DefaultSchema = "public"

// IdentifierMode says how the schema reader treats the table names it is given.
type IdentifierMode int

const (
	// IdentifierFoldLower lower-cases a table name before looking it up. It
	// matches the DDL this package writes (see quoteIdent), which stores every
	// name lower-cased. It is the default, so callers that pass no option and
	// create their tables through this package keep their behaviour. A caller
	// that reads a database it did not create with this package (mixed-case
	// table names) must choose [IdentifierExact] explicitly: with the default,
	// ListCollections reports "Album" but DescribeCollection("Album") looks for
	// "album" and answers not found.
	IdentifierFoldLower IdentifierMode = iota

	// IdentifierExact looks a table up under exactly the name it is given.
	// PostgreSQL names are case-sensitive once quoted, so "Album" and "album"
	// are different tables; choose this mode to read a database whose tables
	// were created with mixed-case names.
	//
	// The mode affects only the schema reader (ListCollections,
	// DescribeCollection, ListIndexes, ListConstraints, ListReferrers). The DDL
	// this package writes still lower-cases every name, and record operations
	// address tables as dalgo2sql renders them, resolved by the connection's
	// search_path.
	IdentifierExact
)

// valid reports whether m is one of the defined modes.
func (m IdentifierMode) valid() bool {
	return m == IdentifierFoldLower || m == IdentifierExact
}

// Option customises how a [Database] reads its schema.
//
// An option must be free of side effects and must only set fields of the
// Database it is given: the constructors apply every option twice, first to a
// zero Database (to reject an invalid set before any connection is attempted,
// when the embedded dal.DB is still nil), then to the real one. An option that
// touches anything else, or that does something observable when it runs, would
// run twice and, the first time, against a Database that has no connection.
type Option func(*Database)

// WithSchema makes the schema reader inspect the named PostgreSQL schema
// instead of [DefaultSchema]. An empty name keeps the default. The name is
// always sent to the server as a bound parameter, never inside SQL text.
//
// A collection reference that names its own schema
// ([dal.NewQualifiedRootCollectionRef]) is read from that schema instead.
//
// The option affects only the schema reader (ListCollections,
// DescribeCollection, ListIndexes, ListConstraints, ListReferrers). The DDL this
// package writes and record operations still follow the connection's
// search_path.
func WithSchema(name string) Option {
	return func(d *Database) { d.schema = name }
}

// WithIdentifierMode selects how table names given to the schema reader are
// matched. See [IdentifierMode].
func WithIdentifierMode(mode IdentifierMode) Option {
	return func(d *Database) { d.identifierMode = mode }
}

// schemaFor returns the schema a collection reference addresses: the schema the
// reference carries, else the configured one. A reference is never answered
// from a schema it does not name.
func (d *Database) schemaFor(ref *dal.CollectionRef) string {
	if schema := ref.Schema(); schema != "" {
		return schema
	}
	return d.schemaName()
}

// schemaName returns the PostgreSQL schema the reader inspects by default.
func (d *Database) schemaName() string {
	if d.schema == "" {
		return DefaultSchema
	}
	return d.schema
}

// resolveName turns a name given by the caller into the table name to look up.
func (d *Database) resolveName(name string) string {
	if d.identifierMode == IdentifierExact {
		return name
	}
	return strings.ToLower(name)
}

// newDatabaseFromSQL wraps an already open, already verified *sql.DB. It is
// the part of construction that needs no server, so unit tests reach it with a
// mocked handle.
func newDatabaseFromSQL(sqlDB *sql.DB, dsn string, schema dal.Schema, opts dalgo2sql.DbOptions, options []Option) *Database {
	d := &Database{
		DB:    dalgo2sql.NewDatabase(sqlDB, schema, opts),
		sqlDB: sqlDB,
		dsn:   dsn,
	}
	applyOptions(d, options)
	return d
}

// applyOptions applies every non-nil option to d.
func applyOptions(d *Database, options []Option) {
	for _, option := range options {
		if option != nil {
			option(d)
		}
	}
}

// checkOptions rejects an option set that cannot be honoured. It needs no
// server, so constructors call it before opening a connection.
func checkOptions(options []Option) error {
	var probe Database
	applyOptions(&probe, options)
	if !probe.identifierMode.valid() {
		return fmt.Errorf("dalgo2postgres: unknown IdentifierMode %d", int(probe.identifierMode))
	}
	return nil
}
