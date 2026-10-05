# dalgo2postgres

PostgreSQL-specific DALgo driver. Wraps `github.com/dal-go/dalgo2sql` to provide the `dal.DB` surface, and adds PostgreSQL-native implementations of:

- structured queries — compiled by dalgo2sql's typed PostgreSQL compiler with every value
  bound and every name quoted, with filtering, ordering, grouping, aggregation and joins run
  on the server (see "Structured queries")
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

## Structured queries

A structured DALgo query (`dal.From(...).NewQuery()...`) reaches PostgreSQL through
dalgo2sql's typed PostgreSQL compiler. `NewDatabase` and `NewDatabaseWithOptions` force
`dalgo2sql.DbOptions.StructuredQueryDialect` to `"postgres"` whatever the caller passes
(the empty string, `"sqlite"` and `"mysql"` are replaced), so no structured query can reach
dalgo2sql's legacy text emitter, which writes values into the statement. The three fields
of `DbOptions` that replace the compiler (`NativeStructuredQueryCompiler`,
`NativeJoinEligibility` and `NativeJoinHintTranslator`) are refused: a constructor that is
given one returns an error before it connects, because the database would declare the
dialect's capabilities and accept joins by its check and then send them to a compiler that
promised neither.

`*Database` declares what the dialect runs on the server (`dal.QueryCapabilitiesProvider`,
`dal.NativeJoinProvider` and `dal.JoinFieldsProvider`), so `dal.As[...]` on a `*Database`
answers as it does on the `dal.DB` under it.

A read takes two statements on one connection: one catalog query that lists the columns of
every table the query names (type, NOT NULL, collation), then the one `SELECT` compiled from
the answer. **No value is written into a statement**: every constant is a bound argument,
and every name is double-quoted. An injection string is compared as text.

### What runs on the server

- **Filters**: constants of each Go type (a whole number is `$n::bigint`, an unsigned
  integer or a float `$n::numeric`, a bool `$n::boolean`, a `time.Time` `$n::timestamptz`,
  bytes `$n::bytea`, a string is untyped so the server reads it as a date, a UUID or an
  enum where the column says so); `== nil`, `isNull` and `isNotNull`; `IN` and `NOT IN`,
  an empty list included. A constant that does not match its column (a number against
  text) is a server error, not an empty result.
- **ORDER BY, LIMIT and OFFSET**: NULLs sort first ascending and last descending, as in
  DALgo, and the `NULLS` clause is left out for a NOT NULL column. `LIMIT` and `OFFSET` are
  bound.
- **GROUP BY and HAVING with `COUNT`, `SUM`, `AVG`, `MIN` and `MAX`**, and `COUNT DISTINCT`
  and `SUM DISTINCT`, as one statement. `SUM` and `AVG` come back as `float64`, as in
  DALgo's own engine. dalgo2sql declares `AVG DISTINCT` too; this repository's tests do not
  run it.
- **Arithmetic** (`+`, `-`, `*`, `/`) on `double precision`: `3 / 2` is `1.5`, and a division
  by zero is NULL.
- **Select-all with exclusions**: the columns of the table, in table order, minus the
  excluded ones in any case (in the mode that folds names).
- **Joins**, when each `ON` pair has the same type category (numbers with numbers, text with
  text) or the same type: one statement with the `JOIN`.
- Tables, views, materialized views and partitioned tables, and columns of a domain over a
  domain.

### What falls back to DALgo's engine

DALgo's own engine is bounded, in memory: it stops at 10,000 rows of a join and 100,000
groups of an aggregation (dalgo v0.89.4), and past them returns an error, not a partial
result.

- **A query with a subquery** (`EXISTS`, a scalar subquery, a derived source): DALgo reads
  each source with plain statements and combines them itself.
- **A join the database declines**: the `ON` types differ (text with an integer, a boolean
  with an integer). dalgo2sql also declines a key whose type has no usable equality
  (`json`, `xml`, geometric types, `oid`, the `reg*` types), and a query its compiler cannot
  write; this repository's tests do not exercise those two.
- **`FIRST` and `LAST`** are refused with an error, not run anywhere: PostgreSQL promises
  no row order for them, and this database declares no stable row order.

DALgo's engine reads each source as the catalog names its columns and matches the names a
query spells against them exactly. So in `IdentifierFoldLower` a query that runs there must
spell its names as the catalog stores them (lower case, for a database this package
created): a subquery that reads `Title` from a column stored as `title` is answered with
`field "Title" is unavailable in "a"`. The native path accepts any case. A join that DALgo
runs itself is read by the same engine.

### Identifier modes

`IdentifierFoldLower`, the default, lower-cases every name before it quotes it, which is
what this package's DDL stores: a database this package created is read back with any
spelling (`Album`, `album` and `ALBUM` are one table). `IdentifierExact` writes every name
as the query spells it, inside quotes, so `Album` and `album` are two tables; choose it to
read a database whose tables were created with mixed-case names (DataTug does). Either mode
keeps the names the query asked for in the result (`Title` comes back as `Title`); a
select-all returns the names the catalog has.

```go
db, err := dalgo2postgres.NewDatabase(dsn, dalgo2postgres.WithIdentifierMode(dalgo2postgres.IdentifierExact))
```

The mode is one setting with two spellings: `WithIdentifierMode`, and dalgo2sql's
`DbOptions.IdentifierCase` (`IdentifierCaseExact`, `IdentifierCaseFoldLower`) for
`NewDatabaseWithOptions`. Either one decides it, and it decides how the schema reader
matches names too. Giving both with different modes is an error, and so is a value this
package does not define: the constructor returns it before it connects, so a mount that
meant to fold names never runs in the exact mode. Do not put a field mask or an access check
on a database that folds names unless the names it compares are folded first, or `Total`
passes a mask that names `total`.

A structured query reads the schema its collection reference names
(`dal.NewQualifiedRootCollectionRef("sales", "Album", "")`), else the schemas the
connection's `search_path` resolves. `WithSchema` affects only the schema reader.

### Results and errors

- A `NUMERIC` column is read as `float64` (pgx delivers it as text), `NaN` included. A
  `float64` holds about 15 significant digits exactly, so a longer `NUMERIC` is
  rounded. Dates and times are `time.Time`.
- A table the database does not have is a `*dalgo2sql.TableNotFoundError` (it matches
  `dalgo2sql.ErrTableNotFound`) that names the table and the nearest one that exists:
  `failed to get SQL reader: table "album" not found; did you mean "Album"? Table names
  are case-sensitive.` (the last sentence is there in `IdentifierExact` only). No statement
  is sent for it.
- A read inside a transaction (`RunReadonlyTransaction`) compiles the same way, and its
  catalog query runs on the transaction.
- A record carries the collection of the query's base source and, when
  `DbOptions.Recordsets` gives that source a one-field primary key, the key's value as its
  ID. A query that does not select the key gets it through a hidden column the statement
  adds, which is not in the record's data. A query that reads into a record of its own
  (`SelectIntoRecord`) fills that record. With no primary key configured, the ID is the
  placeholder `__dalgo_record_id`, not an identity of the row.
- A recordset is found by the source name **as the query spells it**, also in
  `IdentifierFoldLower`, which folds the name only in the statement. A recordset registered as
  `album` is not found for a query that spells `Album`: the statement reads the right table,
  yet every record's ID is the placeholder `__dalgo_record_id`, with no error. Register the
  recordset under the spelling your queries use.
- The rows of a grouped query (one with `GroupBy` or an aggregate) are keyed by their
  ordinal in the result (`"0"`, `"1"`, ...), whether a primary key is configured or not: a
  group is not a row of the table, so no key is read and no hidden column is added.

### The connection a read holds

The catalog query and the statement must see one session (each connection has its own
`search_path`), so a structured read takes one connection from the pool for both and keeps
it until the reader is done: it is given back when the reader is closed, when its rows are
read to the end, when a statement fails, and when the context of the read ends, as the pool
does for a read of its own. A caller that stops reading without closing the reader and
without ending its context keeps the connection. When the pool is exhausted, the next read
waits for a connection and ends with its own context if none is free.

When the context of a read ends while its statement waits on the server, the caller gets the
context's own error (`context.DeadlineExceeded` or `context.Canceled`). When it ends before
the statement is sent (before the catalog query, or between the two statements), the caller
gets `driver: bad connection` (pgx returns it for a statement it did not send because the
context was already done, and database/sql does not retry on the read's own connection) or
`sql: connection is already closed` (the lease gave the connection back first), not the
context's error. Which of the two depends on timing. Check the context's own `Err()` instead
of matching the read's error against it. Nothing is sent to the server after the context
ends. In every case the read lets go of its connection, and the pool serves the next caller
(a connection that failed with `driver: bad connection` is discarded and replaced).

### Key reads and writes

`Get`, `Exists`, `Set`, `Insert`, `Update`, `Delete` and their multi forms take a collection,
field or primary-key name that is a plain identifier (ASCII letters, digits and underscores,
not starting with a digit, at most 255 bytes), written as given and unquoted, so PostgreSQL
folds it to lower case. Any other name is refused with an error that matches
`dalgo2sql.ErrUnsafeName` before a statement is sent; a table whose name needs quoting cannot
be addressed by key, whatever the identifier mode. A nested key addresses one table whose
name joins the collections of the key and of its parents with an underscore, the key's own
first (`pets_owners`), and the primary key is looked up in the recordset of that name.

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
- The same mode decides how structured queries write names (see "Identifier
  modes"), so the reader and the queries never disagree about a name.
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

`NonDeterministicTextColumns(ctx, ref)` lists the columns of a table or view whose
equality is not exact (type `citext`, or a non-deterministic collation such as a
case-insensitive ICU collation), in column order. It finds the table the way the
schema reader does (reference schema, else `WithSchema`; name per
`IdentifierMode`), except that it also answers for materialized views and foreign
tables, which `DescribeCollection` reports as not found. It returns a not-found
error when the table does not exist, and an empty list when the table exists and
none of its columns is `citext` or has a non-deterministic collation. A domain
over `citext` and an array of `citext` are not reported (treat such columns as
inexact yourself), and `char(n)` equality, which ignores trailing spaces, is not
covered.

`WithSchema` affects **only the schema reader**. The DDL this package writes
still lower-cases every name (see "Identifier case folding" below), and key
reads and writes and structured queries resolve tables through the connection's
`search_path`, unless a collection reference names its schema. `IdentifierExact`
does not change the DDL or key reads and writes either: they keep their rules
(see "Key reads and writes").

`ListReferrers` returns one `Referrer` per foreign key, with the referencing
columns in key order. A table with two foreign keys into the queried table is
therefore two entries with the same `Collection` (a `Referrer` carries no
constraint name); callers that want one entry per table group by
`Collection.Name()`. When the queried reference names its schema, each
referrer's `Collection` names it too, so passing it back to `DescribeCollection`
reads the table that was found.

Error messages name the collection (`"sales"."Album"` when the reference named
its schema).

A column whose type has no `dbschema.Type` (uuid, json, jsonb, arrays, enums,
interval, inet, money, ...) is described as a `String` field; it never fails the
table. Base tables and views are listed; `dbschema` has no collection kind, so a
view cannot be told apart from a table in the result. Materialized views and
foreign tables are not listed (PostgreSQL does not expose materialized views in
`information_schema.tables`, and foreign tables are filtered out).

## Connection errors

`NewDatabase` and `NewDatabaseWithOptions` return a `*dalgo2postgres.ConnectionError`
when they cannot open or reach the server. Its text is built, never filtered:

- a **fixed sentence** chosen by the kind of failure, decided from the types in
  the driver's error chain (`errors.As` and `errors.Is`), never from its text;
- the **host, port and database name** the driver itself parsed, each only when it
  passes a strict check and is not repeated from a secret (below).

Nothing of the connection string but the host, port and database name the driver
parsed, and no message of the driver or the server (not a setting name, not a
server's "role ... does not exist"), is copied into it: those hold the user name
and the password. For the same reason the driver's error is not reachable:
`errors.Unwrap` returns nil. Callers branch on the fields:

```go
var connErr *dalgo2postgres.ConnectionError
if errors.As(err, &connErr) {
    switch connErr.Kind {
    case dalgo2postgres.FailureServer: // connErr.SQLState, for example "28P01"
    case dalgo2postgres.FailureNetwork, dalgo2postgres.FailureTimeout, dalgo2postgres.FailureTLS:
    case dalgo2postgres.FailureInvalidDSN, dalgo2postgres.FailureMisread:
    }
    // connErr.Host, connErr.Port, connErr.Database: empty when not nameable.
}
```

| `Kind`              | When                                                                  |
|---------------------|-----------------------------------------------------------------------|
| `FailureInvalidDSN` | the driver cannot parse the string, or a file or service it names cannot be read; nothing is named |
| `FailureMisread`    | the string would be read as something else than was meant; refused before any connection (below) |
| `FailureNetwork`    | name resolution failed, or the connection was refused or reset        |
| `FailureTLS`        | the TLS handshake failed                                              |
| `FailureTimeout`    | the attempt timed out or was canceled                                 |
| `FailureServer`     | the server answered with an error; `SQLState` holds its five-character code |
| `FailureOther`      | anything else (also the zero value)                                   |

A part is named only when:

- **host** is 1 to 253 letters, digits, dots, hyphens, colons and brackets (a name
  with an underscore, or a socket directory, is left out), and the string names
  exactly one server (with several, the failure cannot be pinned to one);
- **port** is the driver's port, under the same single-server condition;
- **database** is 1 to 63 letters, digits, underscores and hyphens;
- and none of them is equal to the user name or the password, or shares a run of
  four bytes with either or with a runtime setting the driver kept (for example
  `application_name`): `postgres:postgres@host/postgres` names no database.

What this means for a string you write:

- Separate `key=value` pairs with **white space**, and put a value with a space in
  single quotes. A keyword string whose host, user or database value holds an
  equals sign (the sign of a pair that a semicolon, comma or ampersand failed to
  separate, as in `host=h;user=u;password=p`) is refused before any connection is
  attempted (`FailureMisread`); the error names nothing of it. A name that really
  holds an equals sign must be given in the URL form, percent-encoded
  (`postgres://cn%3Dapp@h/d`).
- Write a URL that **starts exactly** with `postgres://` or `postgresql://` and
  percent-encode a password with a special character (`p%2Fss`, not `p/ss`). A URL
  in literal quotes (as `docker --env-file` keeps them), with a leading space, in
  capitals or with another scheme (`postgresql+psycopg2://`), or with an unencoded
  `?` in the password, is read by the driver as `key=value` pairs whose first
  setting name holds the credentials, and a server would receive it. Such a
  string is refused before any connection is attempted (`FailureMisread`). A URL
  whose password holds an unencoded `/` or `#` is still passed to the driver,
  because the driver reads it, but its parts are not named.
- A configuration registered with `stdlib.RegisterConnConfig` is passed to the
  driver as before; its host, port and database come from the driver's own error.

Limits: an option value the driver does not keep (`sslpassword`, a path) cannot be
compared with the host and the database, so one that equals them would show the
host or database, which is only what it is. And this covers construction only: a
later failure to connect (the pool opening a new connection, a dropped
connection) comes from the driver without this treatment and may name the
database user, never the password.

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

### Placeholder dialect and structured query dialect

PostgreSQL requires positional parameter markers `$1`, `$2`, … rather than the
`?` style used by SQLite and MySQL. `dalgo2postgres` automatically sets
`dalgo2sql.DbOptions.Placeholder = dalgo2sql.PlaceholderDollar` so that all
SQL emitted through `dalgo2sql` uses the correct form, and
`dalgo2sql.DbOptions.StructuredQueryDialect = "postgres"` so that structured
queries are compiled by the typed PostgreSQL compiler (see "Structured
queries"). Both are forced: a caller's value is replaced.

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
identifiers in its DDL (so reserved words and otherwise-illegal names stay usable)
but **lower-cases them first**, so the case-preserving quoted form agrees with the
unquoted references that key reads and writes emit (which PostgreSQL also folds to
lower case). DDL and key reads and writes thus always address the same physical
identifier. Structured queries quote every name too, and in the default mode
(`IdentifierFoldLower`) lower-case it first, which is the same identifier; in
`IdentifierExact` they write the name as the query spells it (see "Identifier
modes").

Consequence: collection (table) and field (column) names created through this
package are stored lower-cased. Typed clients round-trip transparently because
`encoding/json` unmarshalling is case-insensitive; consumers reading raw records
observe lower-cased field names.
