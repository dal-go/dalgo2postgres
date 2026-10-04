package dalgo2postgres

import (
	"database/sql"
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
	// name lower-cased. It is the default, so existing callers keep their
	// behaviour.
	IdentifierFoldLower IdentifierMode = iota

	// IdentifierExact looks a table up under exactly the name it is given.
	// PostgreSQL names are case-sensitive once quoted, so "Album" and "album"
	// are different tables; choose this mode to read a database whose tables
	// were created with mixed-case names.
	IdentifierExact
)

// Option customises how a [Database] reads its schema.
type Option func(*Database)

// WithSchema makes the schema reader inspect the named PostgreSQL schema
// instead of [DefaultSchema]. An empty name keeps the default. The name is
// always sent to the server as a bound parameter, never inside SQL text.
func WithSchema(name string) Option {
	return func(d *Database) { d.schema = name }
}

// WithIdentifierMode selects how table names given to the schema reader are
// matched. See [IdentifierMode].
func WithIdentifierMode(mode IdentifierMode) Option {
	return func(d *Database) { d.identifierMode = mode }
}

// schemaName returns the PostgreSQL schema the reader inspects.
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
	for _, option := range options {
		option(d)
	}
	return d
}
