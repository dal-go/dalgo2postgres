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
  integer or a float `$n::numeric`, except a float against a `real` column, which is
  `$n::real`; a bool `$n::boolean`, a `time.Time` `$n::timestamptz`,
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
groups of an aggregation (dalgo v0.89.6), and past them returns an error, not a partial
result.

- **A query with a subquery** (`IN`, `EXISTS`, a scalar subquery): DALgo reads each source with
  plain statements and combines them itself. A **correlated** subquery reads the whole inner
  table, every column and no filter, once for each row of the outer table, after one read of the
  outer table: 1 + 7 statements for 7 outer rows, and the server cannot shorten it. Where the
  tables are large, a correlated subquery costs what they hold (the statement count of each
  fixture case is asserted: `TestFixturesIntegration_SubqueriesRunInDALgoOverLeafReads`).
  Issue for dal-go/dalgo (a correlated subquery read with the outer value as a filter): not
  yet filed. A query with a **derived source** is not answered at all: it is refused (see the
  known limits).
- **A join the database declines**: the `ON` types differ (text with an integer, a boolean
  with an integer), or two text keys have different collations of their own, neither the
  database's default (the server cannot choose between them and refuses the comparison,
  `42P22`, so DALgo compares the texts itself). dalgo2sql also declines a key whose type has
  no usable equality
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

- A NULL is `nil` in the rows of both readers, the records reader and the recordset reader
  (`ExecuteQueryToRecordsetReader`, the path DataTug reads), so a caller can tell it from a
  stored `0`, `false`, `""` or zero time. (Up to dalgo2sql v0.26.4 the recordset reader put
  the zero value of the column's type there.)
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
- A **run-time error in the middle of the rows** (a text that is no number, under the cast of
  arithmetic, at row 4 of 5) reaches the caller as the server's error from both readers, after
  the three rows before it: row by row, through the read-all helpers, and out of a read
  transaction (`TestServerPinsIntegration_RuntimeErrorAtRowN`). With dalgo2sql v0.26.0 the
  recordset reader ended the rows quietly instead; since v0.26.1 it does not.
- **How records are keyed.** A record carries the collection of the query's base source,
  spelled as the query spells it, and an ID taken from the first of these that applies:
  the one-field primary key of the recordset declared for the source
  (`DbOptions.Recordsets`); else `DbOptions.PrimaryKey`, when it is one field; else the primary
  key the catalog reports for the source, when it is exactly one column (a key with an
  `INCLUDE` list counts: only its key column is the key); else the ordinal of the row in
  the result (`"0"`, `"1"`, ...), which is no identity of the row. The ordinal is what a
  table with no primary key, a composite key, a view, a materialized view and a grouped
  query get. A query that does not select the key gets it through a hidden column the
  statement adds (`"id" AS "__dalgo_record_id"`), which is not in the record's data;
  `__dalgo_record_id` is only the name of that column, never an ID. A query that reads into a
  record of its own (`SelectIntoRecord`) fills that record.
  `TestStructuredQueryIntegration_RecordKeysFromTheCatalog` runs every case on the server.
- **How a recordset is found** follows two rules. For a **query**, the recordset is looked up
  under the source name as the query spells it and, in `IdentifierFoldLower`, under that name in
  lower case, so a recordset registered as `album` is found for `album`, `Album` and `ALBUM` (the
  record's collection is the spelling the query used); a recordset registered under a name with
  capitals is found only by a query that spells it exactly so (see the known limits). For a
  **key read or write** (`Get`, `Exists`, `Set` and the rest) the recordset is found only under
  the collection of the key exactly as the key spells it, with no folding: a key spelled `Album`
  does not find a recordset registered as `album`, and `Exists` and `Get` answer an error that
  matches `ErrRecordNotFound` (the primary key is not defined for the recordset), with no
  statement sent, for a row that exists
  (`TestStructuredQueryIntegration_KeyReadFindsItsRecordsetOnlyUnderTheCollectionAsSpelled`).
  So register each recordset under the one spelling the keys use, and in `IdentifierFoldLower`
  spell the collection of every key in lower case when queries may spell it in another case.
- The rows of a grouped query (one with `GroupBy` or an aggregate) are keyed by their
  ordinal in the result, whether a primary key is configured or not: a group is not a row of
  the table, so no key is read and no hidden column is added.

### Constants and column types

A constant is typed by its Go type, never by its value, and the server compares it with the
column. `TestTypeMatrixIntegration_FilterByColumnTypeAndConstantType` runs every Go type
against a column of each of `smallint`, `integer`, `bigint`, `numeric`, `real`,
`double precision`, `boolean`, `date`, `timestamp`, `timestamptz`, `uuid`, `jsonb` and `text`
on PostgreSQL 17 and 18 (the Conformance job runs both, and nothing differs), and prints the
statement, the argument and the answer of each pair (`go test -v`).

| Go type of the constant | The statement writes | The driver is handed |
|---|---|---|
| signed integer (`int`, `int8` ... `int64`) | `$n::bigint` | `int64` |
| unsigned integer (`uint` ... `uint64`) | `$n::numeric` | its decimal text |
| `float32`, `float64` (a whole number too) | `$n::numeric`; against a `real` column `$n::real` | its shortest decimal text |
| `string` | `$n`, untyped | the string |
| `bool` | `$n::boolean` | `bool` |
| `time.Time` | `$n::timestamptz` | `time.Time` |
| `[]byte` | `$n::bytea` | `[]byte` |
| `nil` with `==` | `IS NULL` | nothing |
| `nil` with another operator | `$n`, untyped | `nil`; a comparison with NULL is unknown, so no row |

What the server answers, as observed:

- **A number against a number** compares as numbers, whatever the types: an integer column
  against `1.5` is a miss, not an error, and `2.0` as a `float64` finds the integer 2.
- **A constant the column's type has no comparison with is a server error, never a wrong
  match.** The pairs the server refuses with `42883` (no such operator) are: a number or bytes
  against `boolean`, `date`, `timestamp`, `timestamptz`, `uuid`, `jsonb` or `text`; a bool
  against any column but `boolean`; a `time.Time` against any column but `date`, `timestamp` and
  `timestamptz`; and bytes against any column but `bytea`. A bool against `boolean` and a
  `time.Time` against `date`, `timestamp` and `timestamptz` are compared and match (the
  session's time zone decides the last: see below).
- **A string** is read by the server as the column's own type, so a date, a UUID or a JSON
  document is matched by its text; a string that is no value of the type is `22P02` (`22007`
  for a date or a time). Any string is a `text`.
- **`time.Time` against `date` or `timestamp`** compares in the session's time zone: in UTC,
  `2021-06-15T12:30:00Z` finds the timestamp `2021-06-15 12:30:00`; in `Asia/Tokyo` it does
  not, and `03:30Z` does. Know the zone of your sessions.
- **A bigint above 2^53** is matched exactly by an `int64`, a `uint64` or a string, and read
  back exactly into an integer field (`NUMERIC` too, through a struct); a `float64` cannot
  carry it (2^53+1 is 2^53), so it finds nothing. In a map, a `NUMERIC` is a `float64`, which
  holds about 15 digits.
- **`'Infinity'::numeric`** (PostgreSQL 14 and later) is matched by `math.Inf(1)`, `-Infinity`
  by `math.Inf(-1)` and `NaN` by `math.NaN()`, and read back as those `float64` values.
- **Arithmetic is on `double precision`**: `i4 * 2` on 2147483647 is `4294967294` (a
  `float64`), where the server alone would refuse it with `22003`; a bigint beyond 2^53 loses
  its last digits, and `/` is not an integer division.
- **A `NUMERIC` with a fraction cannot be read into an integer field**: the read fails with
  `column "num": the value is not an int64: it has a fractional part`; a whole `NUMERIC`
  can.
- **ORDER BY** follows the column's collation, not Go's string order: with the database's
  `en_US.utf8`, `São Paulo` sorts before `Shanghai`. This holds for the `ORDER BY` a keys-only
  read adds on the primary key too (see the known limits). A table whose order a caller
  compares with Go's declares its text `COLLATE "C"`. NULLs sort first ascending and last descending;
  a `NOT NULL` column has no `NULLS` clause, except on the nullable side of a `LEFT JOIN`.
- **A float against a `real` column** is compared as a real. A `real` that holds `0.1` is
  `0.10000000149011612` when widened, and a `numeric` `0.1` is not that number, so a numeric
  binding finds nothing; the dialect writes `$n::real` from the float's own decimal text, and
  `float32(0.1)`, `0.1`, the string `"0.1"` and the `float64` the reader returned for the row
  all find it. Against `double precision` and `numeric` the binding stays `numeric`. A float
  is **rounded to a real before it is compared**, so one that differs from the stored real only
  past the seven digits a real holds is the same real: `0.1000000001` finds the row that stores
  `0.1`.
- **Joins**: a text column with a collation of its own against one with the database's
  default joins on the server, in the collation of the first. Two columns with different
  collations of their own do not join on the server (`42P22`: the server cannot choose
  between them); the compiler declines such a join, and DALgo's engine answers it.
- **A select-all over a join** is one `SELECT *`, and the server lists the base source's
  columns first, then each joined source's, in `FROM` order.
- **A column named like its source's alias** (`SELECT "x" FROM "t" AS "x"`) is the column; a
  name that is no column of the source, bare or qualified, is refused before the server,
  because the server would read it as the whole row or as a function of it
  (`f.to_jsonb`).
- **Two aliases that fold to one name** (`Total` and `TOTAL`) are refused before the server in
  `IdentifierFoldLower`; in `IdentifierExact` they are two columns.

Known limits. Each is pinned by a test that prints the statement and the server's answer.
The ones that follow are **defects or limits of dal-go/dalgo2sql**. None has an issue or a pull
request yet (each says "not yet filed" until it has, and then names it), and no fix of any of
them is under way. Each test FAILS when the fix is released and says what to assert instead, so
the line here is then deleted. They are observations, not rules to rely on.

- **A column with a `NOT VALID` not-null constraint** (PostgreSQL 18; `ALTER TABLE ... ADD
  CONSTRAINT ... NOT NULL col NOT VALID`) that holds NULL rows is **sorted wrongly**, which is a
  wrong result and not a refusal. The catalog calls the column NOT NULL, the compiler writes
  no `NULLS` clause for it, and the server's default puts the NULLs last ascending and first
  descending, the opposite of DALgo's rule: `ORDER BY v LIMIT 3` over the values 3, NULL, 1, NULL,
  2 returns 1, 2, 3 where DALgo's rule gives the two NULL rows first
  (`TestServerPinsIntegration_NotValidNotNullColumnWithNullsUnderOrderByAndLimit` pins the
  catalog's two facts, the statement and the rows). Do not rely on the order of such a column,
  or of a `LIMIT` or `OFFSET` page over it, until it is fixed, or validate the constraint.
  Issue or pull request: not yet filed.
- **A derived source** (a query in `FROM` or in a join, DTQL's `from: {query: ...}`) is refused:
  DALgo's engine asks the adapter for the columns of each source, and dalgo2sql answers for a
  table and refuses for a derived source with `not supported: source dal.QuerySource`. Four
  cases of the subquery fixtures are listed in `testdata/postgres-divergences.json` for it, the
  simplest one (`FROM (query) AS recent`, no join) included. Issue: not yet filed.
- **A wildcard between two explicit columns of a join** is not compiled for the server (the
  compiler refuses a wildcard that does not come first), so the join is declined to DALgo's
  engine, which reads each table with one statement and joins in memory, within its bounds. The
  fixture `chinook-wildcard` is listed in the divergence file for it. Issue: not yet filed.

- **A recordset registered under a name with capitals** (`Ck_Mixed`) is found only by a query
  that spells the source exactly so; dalgo2sql folds the name the query spells, never the
  registered names. For `ck_mixed` or `CK_MIXED` the recordset is ignored with no error and the
  records are keyed by the catalog's key instead of the declared one. In
  `IdentifierFoldLower` register recordsets under lower case names, and spell the collection of
  every key the same way (see "How a recordset is found"). Issue: not yet filed.
- **A key read does not fold the collection of its key** (`Ck_Decl` for a recordset registered as
  `ck_decl`): the recordset is not found, and `Exists` and `Get` answer an error that matches
  `ErrRecordNotFound` for a row that exists (see "How a recordset is found"). A query finds it.
  Issue: not yet filed.
- **A keys-only read selects every column.** A read of keys only is sent as
  `SELECT * FROM "t" ORDER BY "id" ASC` for a table whose key the catalog gives, so the server
  reads and sends every column of every row to return a key. Issue: not yet filed
  (`TestStructuredQueryIntegration_RecordKeysFromTheCatalog` pins the statement).

Limits of this combination that are not defects of one library:

- **A keys-only read follows the database's collation.** It is sent as a read of the table
  with `ORDER BY` on the primary key, and for a text key the server orders by the key's
  collation: with `en_US.utf8`, `São Paulo_São Paulo` comes before `Shanghai_Shanghai`, where
  `sort.Strings` puts it last. The shared suite of dal-go/dalgo asserts the keys-only read of
  its cities in Go's order, so it fails here for a key in the database's collation; its fixture
  here declares `COLLATE "C"` (the control, which equals Go's order) and the divergence is
  pinned by `TestServerPinsIntegration_KeysOnlyReadFollowsTheDatabasesCollation`. Issues for
  dal-go/dalgo (is the order part of the contract?) and dal-go/dalgo2sql: not yet filed.
- In `IdentifierFoldLower`, a **row-level access condition** that spells a field in another
  case than the table stores it (`Country` for `country`) is not satisfied on `Exists` or
  `Update`, which read the row into a map keyed by the stored names: the access layer compares
  names exactly and the stored names are lower case, so it denies a row the policy allows.
  `TestEndToEndIntegration_RowConditionsFailClosedOnStoredNames` (Exists) and
  `TestEndToEndIntegration_UpdateUnderRowConditionsFailsClosedOnStoredNames` (Update) pin it.
  It fails closed **when the conditional rule is the only allow for the row**. A conditional
  rule whose condition does not hold falls through to an unconditional allow when the policy has
  one, so a policy that narrows with a condition and then allows without one is decided by the
  wider rule. Spell the names as stored, and give a row condition no wider allow behind it;
  `TestAccessIntegration_UnconditionalAllowBehindAConditionDecidesTheRow` asserts the
  fall-through case.

### What the integration tests prove about queries

Every one runs in the Conformance job on PostgreSQL 17 and 18, and the job fails when one is
skipped, missing or not passing.

- **The corpus.** The DTQL fixtures of dal-go/dalgo under `testdata/joins` and
  `testdata/subqueries` are vendored with their SHA-256 digests and the module version they come
  from, and a test fails when a file or a digest changes, when the files are not those of the
  dalgo the module requires, and when a bump of dalgo has not been followed by vendoring them
  again. The join fixtures run on hand-written PostgreSQL DDL (quoted mixed-case names, `text
  COLLATE "C"`, the exact identifier mode), each as one statement in the server, and the
  subquery fixtures run in DALgo's engine over plain reads. Each case is compared with its
  fixture (numbers within 1e-9); a case that differs is listed in
  `testdata/postgres-divergences.json` with the reason, and the test fails for a difference that
  is not listed and for a listed case that no longer differs. An entry also says what the case
  does: an `error` entry holds the text the refusal contains (and the error must not be the
  server's), a `statements` entry the number of statements and that each is a plain read, and an
  entry of a suite the tests do not run is refused. A fixture document that no test runs fails,
  and so does a case with no input but the one named for it. The number of statements of every
  subquery case is asserted, and a correlated subquery is shown to read the inner table once
  per outer row (`TestFixturesIntegration_JoinsRunAsOneStatementOnTheServer`, which runs each
  join through both readers, `TestFixturesEveryJoinFixtureIsRunByATest`,
  `TestFixturesIntegration_SubqueriesRunInDALgoOverLeafReads`).
- **Pushdown.** A `GROUP BY` with `HAVING` over 200,000 rows and 150,000 groups, and a join of
  50,000 orders to their customers (20,000,000 bytes of text), are each one statement in the
  server, with an answer past the bounds of DALgo's engine (100,000 groups; 10,000 joined rows and
  16 MiB). The negative control runs the same queries over the same tables without the native
  route (a database with no dialect for the grouped query, DALgo's engine over the adapter's plain
  reads for the join), and the engine refuses them with its own limit; the same shapes over a few
  rows are answered by both with the same rows (`TestPushdownIntegration_`). The two proofs run
  through both public reads, the records reader and the recordset reader that DataTug reads, and
  are one statement through either; the negative control reads through the records reader and
  DALgo's engine.
- **Access.** Through `access.SecureReadSession`: a row condition whose value holds a quote
  returns exactly the permitted rows, the value is a bound argument, and the table is whole
  afterwards; the alias of a hidden field and a projection with no permitted column are denied; a
  hidden field in a join's `ON` condition at any depth, in a filter, in an order and in a scan
  order is denied with no statement sent to the server; a session with no policy is denied; and a
  wider allow behind a conditional rule decides the row (`TestAccessIntegration_`, each in both
  identifier modes). A field list of the listed (base) source is applied to every field in a join's
  select list, `WHERE`, `GROUP BY`, `HAVING` and `ORDER BY`, whatever source its qualifier
  names: only a join's `ON` condition and a scan order are attributed to the source they name. So
  a column of a joined source is read only when the list also holds its name, and `ORDER BY
  i.InvoiceId`, `SELECT i.Total`, a filter, a `GROUP BY` and a `HAVING` on a field of the joined
  `Invoice` are all denied by the listed `Person`'s list, with the code, the slot, the column and
  no statement. It fails closed and does not leak; it is a limit of dal-go/dalgo (issue: not yet
  filed), pinned by `TestAccessIntegration_HiddenFieldInAJoinOrAScanOrderIsDeniedWithNoStatement`.
  The shared access controls of dal-go/dalgo run beside them in `TestEndToEnd`.
- **Probes.** A canary table, and a quote, a double quote, doubled quotes, a semicolon with a
  second statement, comment markers, a NUL byte, a name of 64 bytes and a non-ASCII name written
  where a value (in a filter, an `IN` list and a `HAVING`), a field (in a filter, the select
  list, an aggregate, `ORDER BY` and `GROUP BY`), the qualifier of a field, a collection, the
  schema of a qualified collection, an alias (of a column, of the source and of a joined
  source) and a join key go, in both identifier modes. Each answer is rows, an empty result or an
  error built before any statement (a NUL byte in a value is the server's own refusal); outside
  its quoted identifiers no statement holds a marker, every value is a bound argument, and the
  canary is unchanged (`TestProbesIntegration_NothingACallerWritesBecomesSQL`). The key paths,
  which are another emitter of SQL (plain names written unquoted), are probed in the same two
  modes with the same texts as the collection of a key, as its ID, and as the field names and
  the values of a write (`Exists`, `Get`, `Insert`, `Delete`): a text that is not a plain
  identifier is refused with `ErrUnsafeName` and no statement, an ID and a value are arguments
  read back as written, and nothing is left behind
  (`TestProbesIntegration_NothingACallerWritesBecomesSQLInAKeyPath`). A join's `ON` condition
  takes only an equality of two fields, so a value is no position of it.
- **PostgreSQL 18.** A column with a `NOT VALID` not-null constraint and NULL rows under
  `ORDER BY` with `LIMIT` (`TestServerPinsIntegration_NotValidNotNullColumnWithNullsUnderOrderByAndLimit`;
  on 17 it asserts that the syntax is refused).

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

Then run the tests. Two sub-tests of the shared end2end suite (`TestEndToEnd`, which runs
`end2end.TestDalgoDB` of dalgo in `IdentifierFoldLower`) cannot run in that mode, because the
access layer compares names exactly and a key read returns the stored lower-case names
(`TestEndToEndIntegration_RowConditionsFailClosedOnStoredNames` asserts why). The Conformance
job excludes them by name, and so must you:

```sh
DALGO2POSTGRES_TEST_DSN='postgres://ovdb:ovdb@127.0.0.1:15432/ovdb?sslmode=disable' \
    go test ./... -count=1 -v \
    -skip '^TestEndToEnd$/^query$/^access_conditions$/^(point_reads_follow_the_condition|writes_follow_the_condition)$'
```

`TestEndToEnd` then reports those two as skips that name the reason; the Conformance job
fails on any other skip, and on a test of the families `TestEndToEnd`, `TestTypeMatrixIntegration_`,
`TestServerPinsIntegration_`, `TestStructuredQueryIntegration_`, `TestFixturesIntegration_`,
`TestPushdownIntegration_`, `TestAccessIntegration_` and `TestProbesIntegration_` that does not run
to a pass.

**The DSN must name a throwaway database.** The tests create and drop whole schemas with
`CASCADE`: the ones named `test_*`, the schema `main` that the join fixtures name (it is dropped
and created again whatever it holds), the schemas the probes name (the texts of the probes), and
the conformance suite creates and drops its own tables in the connection's default schema. Never
point `DALGO2POSTGRES_TEST_DSN` at a database that holds data. The example above is a container
that exists for the tests.

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
