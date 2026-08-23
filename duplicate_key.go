package dalgo2postgres

import (
	"errors"

	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5/pgconn"
)

// sqlStateUniqueViolation is the PostgreSQL SQLSTATE code for a unique-
// constraint violation ("unique_violation", class 23 — integrity constraint
// violation). See:
// https://www.postgresql.org/docs/current/errcodes-appendix.html
//
// Class 23 also contains "23503" (foreign_key_violation) and "23514"
// (check_violation) — neither is a duplicate key, so [IsAlreadyExists]
// matches only this one code, not the whole class.
const sqlStateUniqueViolation = "23505"

// IsAlreadyExists is a [dalgo2sql.DbOptions.IsAlreadyExists] classifier for
// PostgreSQL via github.com/jackc/pgx/v5 (the driver this package uses —
// see the "pgx" import in database.go). It reports whether err represents a
// unique-key violation: the driver reports these as a *[pgconn.PgError]
// whose Code is the SQLSTATE "23505" (unique_violation).
//
// Detection matches the SQLSTATE code via [errors.As], never the error
// message — messages are not a stable contract and can vary by locale or
// server version. A sibling class-23 code such as "23503"
// (foreign_key_violation) or "23514" (check_violation) is a different kind
// of constraint failure, not a duplicate key, and is deliberately reported
// as false here.
//
// [NewDatabaseWithOptions] (and therefore [NewDatabase]) wires this in
// automatically whenever the caller's [dalgo2sql.DbOptions.IsAlreadyExists]
// is nil. It is exported so callers who build a [dalgo2sql.DbOptions]
// directly — e.g. calling dalgo2sql.NewDatabase themselves rather than
// going through this package's constructors — can reuse it instead of
// rewriting the same *pgconn.PgError check.
func IsAlreadyExists(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlStateUniqueViolation
}

// applyPostgresDbOptionDefaults mutates opts in place, filling in the
// PostgreSQL-specific defaults [NewDatabaseWithOptions] always applies
// regardless of what the caller passed in:
//
//   - Placeholder is forced to [dalgo2sql.PlaceholderDollar] ($1/$2/…)
//     unconditionally, since this package only ever talks to PostgreSQL.
//   - IsAlreadyExists is defaulted to [IsAlreadyExists] only when the
//     caller left it nil, so Insert/InsertMulti reject a unique-key
//     violation with record.IsAlreadyExists (see dalgotest's unconditional
//     conformance check) without every caller having to wire this hook up
//     by hand — while a caller-supplied hook is never overridden.
//
// Split out from NewDatabaseWithOptions so this defaulting logic can be
// unit-tested directly, without needing a live PostgreSQL connection.
func applyPostgresDbOptionDefaults(opts *dalgo2sql.DbOptions) {
	opts.Placeholder = dalgo2sql.PlaceholderDollar
	if opts.IsAlreadyExists == nil {
		opts.IsAlreadyExists = IsAlreadyExists
	}
}
