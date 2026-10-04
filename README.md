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
  matching the lower-cased tables this package's DDL creates. It is the default
  so that callers that pass no option and create their tables through this
  package keep working. A caller that reads a database it did not create with
  this package must pass `IdentifierExact` explicitly: with the default,
  `ListCollections` reports `Album` but `DescribeCollection("Album")` looks for
  `album` and answers not found.
- `IdentifierExact` looks the table up under exactly the name given, so a table
  created as `"Album"` is found as `Album` and not as `album`.
- A nil option is ignored; an unknown `IdentifierMode` makes the constructor
  return an error before it connects.

A collection reference that names its own schema
(`dal.NewQualifiedRootCollectionRef("sales", "Album", "")`) is read from that
schema, whatever `WithSchema` says; a reference without a schema is read from
the configured one. The reader never answers a reference from a schema it does
not name. `ListReferrers` reports only referrers in the same schema as the
queried table; a foreign key into another schema shows up in `DescribeCollection`
as `ReferencedNamespace`, which is empty when the target is in the table's own
schema.

`WithSchema` and `IdentifierExact` affect **only the schema reader**. The DDL
this package writes still lower-cases every name (see "Identifier case folding"
below), and record operations (`Get`, `Insert`, queries) address tables as
`dalgo2sql` renders them and resolve them through the connection's
`search_path`.

`ListReferrers` returns one `Referrer` per foreign key, with the referencing
columns in key order. A table with two foreign keys into the queried table is
therefore two entries with the same `Collection` (a `Referrer` carries no
constraint name); callers that want one entry per table group by
`Collection.Name()`. When the queried reference names its schema, each
referrer's `Collection` names it too, so passing it back to `DescribeCollection`
reads the table that was found.

Error messages name the collection (`"sales"."Album"` when the reference named
its schema) and never contain a connection string.

Connection strings and errors. At construction, a failure to connect names at
most the host and the database name; the user, password, port and any other part
of the DSN are removed from the driver's error text. Nothing is named when the
string cannot be read with confidence: a string that cannot be parsed, or a URL
whose password was not percent-encoded and so splits at a `/`, `?` or `#` (write
`p%2Fss`, not `p/ss`). A configuration registered with
`stdlib.RegisterConnConfig` is passed to the driver as before. `errors.Is` and
`errors.As` still reach the driver's own error through `Unwrap` (except for a
string that cannot be parsed, whose error carries no cause), so do not print the
unwrapped error. This covers construction only: a later failure to connect (the
pool opening a new connection, a dropped connection) comes from the driver
without redaction and may name the database user, never the password.

A column whose type has no `dbschema.Type` (uuid, json, jsonb, arrays, enums,
interval, inet, money, ...) is described as a `String` field; it never fails the
table. Base tables and views are listed; `dbschema` has no collection kind, so a
view cannot be told apart from a table in the result. Materialized views and
foreign tables are not listed (PostgreSQL does not expose materialized views in
`information_schema.tables`, and foreign tables are filtered out).

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
