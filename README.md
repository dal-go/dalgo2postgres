# dalgo2postgres

PostgreSQL-specific DALgo driver. Wraps `github.com/dal-go/dalgo2sql` to provide the `dal.DB` surface, and adds PostgreSQL-native implementations of:

- `dbschema.SchemaReader` — schema introspection via `information_schema` and `pg_indexes`
- `ddl.Applier` — PostgreSQL-flavored `CREATE TABLE` / `CREATE INDEX` / `DROP TABLE` / `ALTER TABLE`
- `dal.ConcurrencyAware` — advertises `SupportsConcurrentConnections() = true`
  (PostgreSQL supports concurrent connections from multiple goroutines and processes,
  unlike SQLite which serializes writers)

## Constructor usage

```go
import (
    "github.com/dal-go/dalgo/dal"
    "github.com/dal-go/dalgo2postgres"
    "github.com/dal-go/dalgo2sql"
)

// Minimal — no per-collection primary-key configuration.
db, err := dalgo2postgres.NewDatabase("postgres://user:pass@localhost:5432/mydb?sslmode=disable")

// With per-collection recordset options (required for Insert/Get/Delete with map[string]any data):
db, err := dalgo2postgres.NewDatabaseWithOptions(dsn, dal.NewSchema(nil, nil),
    dalgo2sql.DbOptions{
        Recordsets: map[string]*dalgo2sql.Recordset{
            "widgets": dalgo2sql.NewRecordset("widgets", dalgo2sql.Table,
                []dal.FieldRef{dal.Field("id")}),
        },
    })
if err != nil {
    return err
}
defer db.Close()
```

## Schema reader options

The schema reader (`ListCollections`, `DescribeCollection`, `ListIndexes`,
`ListConstraints`, `ListReferrers`) lists tables and views of one PostgreSQL
schema. Both constructors take optional settings:

```go
db, err := dalgo2postgres.NewDatabase(dsn,
    dalgo2postgres.WithSchema("sales"),                              // default "public"
    dalgo2postgres.WithIdentifierMode(dalgo2postgres.IdentifierExact)) // default IdentifierFoldLower
```

- `IdentifierFoldLower` (default) lower-cases a table name before looking it up,
  matching the lower-cased tables this package's DDL creates.
- `IdentifierExact` looks the table up under exactly the name given, so a table
  created as `"Album"` is found as `Album` and not as `album`.

A column whose type has no `dbschema.Type` (uuid, json, jsonb, arrays, enums,
interval, inet, money, ...) is described as a `String` field; it never fails the
table. Materialized views are not listed (PostgreSQL does not expose them in
`information_schema`).

## Type mapping

| `dbschema.Type` | PostgreSQL column type |
|-----------------|------------------------|
| `String`        | `TEXT` (or `VARCHAR(n)` when `Length` is set) |
| `Int`           | `BIGINT` |
| `Float`         | `DOUBLE PRECISION` |
| `Bool`          | `BOOLEAN` |
| `Time`          | `TIMESTAMPTZ` |
| `Decimal`       | `NUMERIC` (or `NUMERIC(total, scale)` when `Precision` is set) |
| `Bytes`         | `BYTEA` |

## Running the tests

The tests that touch a live database are gated on the `DALGO2POSTGRES_TEST_DSN`
environment variable. When it is not set, those tests are skipped so plain CI
passes without a database.

Start a local PostgreSQL 17 server (Docker):

```sh
docker run -d \
  --name postgres17 \
  -e POSTGRES_USER=ovdb \
  -e POSTGRES_PASSWORD=ovdb \
  -e POSTGRES_DB=ovdb \
  -p 15432:5432 \
  postgres:17
```

Then run the tests:

```sh
DALGO2POSTGRES_TEST_DSN='postgres://ovdb:ovdb@127.0.0.1:15432/ovdb?sslmode=disable' \
    go test ./... -count=1 -v
```

## PostgreSQL driver: `github.com/jackc/pgx/v5/stdlib` (pure Go, `CGO_ENABLED=0`)

This package uses **`github.com/jackc/pgx/v5/stdlib`** — a pure-Go PostgreSQL
driver exposed through the standard `database/sql` interface (driver name `"pgx"`).
No C toolchain is required; the package compiles with `CGO_ENABLED=0`.

### Placeholder dialect

PostgreSQL requires positional parameter markers `$1`, `$2`, … rather than the
`?` style used by SQLite and MySQL. `dalgo2postgres` automatically sets
`dalgo2sql.DbOptions.Placeholder = dalgo2sql.PlaceholderDollar` so that all
SQL emitted through `dalgo2sql` uses the correct form. This field was added to
`dalgo2sql` as a minimal backward-compatible extension; the zero value
(`PlaceholderQuestion`) preserves the existing behavior for all other drivers.

### Duplicate-key classification

`record.ErrRecordExists` / `record.IsAlreadyExists` let callers detect an
`Insert` over an existing key without depending on driver-specific error
types or message text. `dalgo2sql` owns the wrapping (it applies
`fmt.Errorf("%w: %w", record.ErrRecordExists, err)` at its single
`execInsert` choke point, covering `Insert`, `InsertMulti`, and their
transactional variants) but delegates the one question only a driver knows
the answer to — is this particular error a unique-key violation? — to
`dalgo2sql.DbOptions.IsAlreadyExists`.

`dalgo2postgres` supplies that hook: `NewDatabase` and
`NewDatabaseWithOptions` default `DbOptions.IsAlreadyExists` to
`dalgo2postgres.IsAlreadyExists` whenever the caller leaves it nil (a
caller-supplied hook is never overridden). It reports true when the error is
a `*pgconn.PgError` (from `github.com/jackc/pgx/v5/pgconn`, the driver this
package uses) whose `Code` is the PostgreSQL SQLSTATE `23505`
(`unique_violation`) — matched on the SQLSTATE code via `errors.As`, never
the error message. Sibling class-23 codes such as `23503`
(`foreign_key_violation`) and `23514` (`check_violation`) are integrity
violations but not duplicate keys, and are deliberately reported as false.

`dalgo2postgres.IsAlreadyExists` is exported so callers who construct their
own `dalgo2sql.DbOptions` directly (e.g. calling `dalgo2sql.NewDatabase`
themselves) can reuse it instead of rewriting the same `*pgconn.PgError`
check.

### Identifier case folding

PostgreSQL folds unquoted identifiers to lower case. `dalgo2postgres` quotes
identifiers (so reserved words and otherwise-illegal names stay usable) but
**lower-cases them first**, so the case-preserving quoted form agrees with the
unquoted references that `dalgo2sql`'s DML and dalgo's structured-query
rendering emit (which PostgreSQL also folds to lower case). DDL and DML thus
always address the same physical identifier.

Consequence: collection (table) and field (column) names are stored
lower-cased. Typed clients round-trip transparently because `encoding/json`
unmarshalling is case-insensitive; consumers reading raw records observe
lower-cased field names. Fully case-preserving storage would require dalgo's
structured-query `String()` to quote column identifiers, tracked upstream.
