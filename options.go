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

// IdentifierMode says how a [Database] treats the names it is given: the schema
// reader matches a table by it, and a structured query writes the names it spells
// by it (dalgo2sql's DbOptions.IdentifierCase, which this package sets from it).
type IdentifierMode int

const (
	// IdentifierFoldLower lower-cases a name before looking it up or writing it,
	// inside quotes. It matches the DDL this package writes (see quoteIdent), which
	// stores every name lower-cased, so a database this package created is read
	// back with any spelling of a name: Album, album and ALBUM are one table. It is
	// the default, so callers that pass no option and create their tables through
	// this package keep their behaviour. A caller that reads a database it did not
	// create with this package (mixed-case table names) must choose
	// [IdentifierExact] explicitly: with the default, ListCollections reports
	// "Album" but DescribeCollection("Album") looks for "album" and answers not
	// found, and so does a structured query.
	IdentifierFoldLower IdentifierMode = iota

	// IdentifierExact looks a table up, and writes a name, exactly as it is given.
	// PostgreSQL names are case-sensitive once quoted, so "Album" and "album" are
	// different tables; choose this mode to read a database whose tables were
	// created with mixed-case names.
	//
	// The mode affects the schema reader (ListCollections, DescribeCollection,
	// ListIndexes, ListConstraints, ListReferrers, NonDeterministicTextColumns) and
	// structured queries. It does not affect the DDL this package writes, which
	// still lower-cases every name, nor key reads and writes (Get, Set, Insert,
	// Update, Delete and their multi forms), which accept plain names only and
	// write them unquoted, so PostgreSQL folds them to lower case.
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
// DescribeCollection, ListIndexes, ListConstraints, ListReferrers,
// NonDeterministicTextColumns). The DDL this package writes, key reads and
// writes and structured queries still follow the connection's search_path: a
// structured query reads another schema when its collection reference names it
// ([dal.NewQualifiedRootCollectionRef]).
func WithSchema(name string) Option {
	return func(d *Database) { d.schema = name }
}

// WithIdentifierMode selects how names are matched by the schema reader and
// written by structured queries. See [IdentifierMode]. Without it the mode is
// [IdentifierFoldLower], or the one dalgo2sql.DbOptions.IdentifierCase names when
// the caller passed [NewDatabaseWithOptions] one; the two must not disagree, and
// a constructor refuses the pair that does.
func WithIdentifierMode(mode IdentifierMode) Option {
	return func(d *Database) { d.identifierMode, d.identifierModeSet = mode, true }
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

// settings is what the constructors need once the caller's options have been
// checked: the dalgo2sql options this package hands to dalgo2sql, with the dialect
// forced and the identifier case resolved, and the identifier mode the reader uses.
// resolveSettings is the only producer, and newDatabaseFromSQL, the only caller of
// dalgo2sql.NewDatabase outside tests, takes nothing else, so no constructor of this
// package can open a database whose structured queries reach dalgo2sql's legacy
// text emitter.
type settings struct {
	db   dalgo2sql.DbOptions
	mode IdentifierMode
}

// resolveSettings checks options against opts, with no server involved, and
// returns the settings a Database is built from. Constructors call it before they
// open a connection, so an option set that cannot be honoured is refused first.
//
// The identifier mode is one setting with two spellings: the caller's
// [WithIdentifierMode] and dalgo2sql's DbOptions.IdentifierCase. Either one decides
// it; both set and different is an error; neither is [IdentifierFoldLower]. An
// IdentifierCase or an IdentifierMode this package does not define is an error: a
// caller that meant to fold names must not silently get the exact mode.
func resolveSettings(opts dalgo2sql.DbOptions, options []Option) (settings, error) {
	var probe Database
	applyOptions(&probe, options)
	mode, err := probe.chooseIdentifierMode(opts.IdentifierCase)
	if err != nil {
		return settings{}, err
	}
	opts.IdentifierCase = dalgo2sql.IdentifierCaseFoldLower
	if mode == IdentifierExact {
		opts.IdentifierCase = dalgo2sql.IdentifierCaseExact
	}
	applyPostgresDbOptionDefaults(&opts)
	return settings{db: opts, mode: mode}, nil
}

// chooseIdentifierMode is the identifier mode the options of d and the caller's
// DbOptions.IdentifierCase ask for together. See resolveSettings.
func (d *Database) chooseIdentifierMode(identifierCase dalgo2sql.IdentifierCase) (IdentifierMode, error) {
	var fromCase IdentifierMode
	caseSet := true
	switch identifierCase {
	case "":
		caseSet = false
	case dalgo2sql.IdentifierCaseExact:
		fromCase = IdentifierExact
	case dalgo2sql.IdentifierCaseFoldLower:
		fromCase = IdentifierFoldLower
	default:
		return 0, fmt.Errorf("dalgo2postgres: unknown IdentifierCase %q: use %q or %q",
			string(identifierCase), string(dalgo2sql.IdentifierCaseExact), string(dalgo2sql.IdentifierCaseFoldLower))
	}
	switch {
	case d.identifierModeSet:
		if !d.identifierMode.valid() {
			return 0, fmt.Errorf("dalgo2postgres: unknown IdentifierMode %d", int(d.identifierMode))
		}
		if caseSet && fromCase != d.identifierMode {
			return 0, fmt.Errorf("dalgo2postgres: WithIdentifierMode and DbOptions.IdentifierCase %q disagree: give one of them, or the same mode in both",
				string(identifierCase))
		}
		return d.identifierMode, nil
	case caseSet:
		return fromCase, nil
	}
	return IdentifierFoldLower, nil
}

// newDatabaseFromSQL wraps an already open, already verified *sql.DB. It is
// the part of construction that needs no server, so unit tests reach it with a
// mocked handle, and the only place a dalgo2sql database is built.
func newDatabaseFromSQL(sqlDB *sql.DB, schema dal.Schema, s settings, options []Option) *Database {
	d := &Database{
		DB:    dalgo2sql.NewDatabase(sqlDB, schema, s.db),
		sqlDB: sqlDB,
	}
	applyOptions(d, options)
	d.identifierMode = s.mode
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
