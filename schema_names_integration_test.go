package dalgo2postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// Runs against a real PostgreSQL server and skips unless DALGO2POSTGRES_TEST_DSN
// is set (see testDSN); CI sets it, on PostgreSQL 17 and 18.
//
// PostgreSQL keeps 63 bytes of an identifier. This test calls every entry of the
// schema reader that takes a schema or a table name from the caller, with the
// name of an object that exists (63 bytes) and with the same name plus one byte
// (64 bytes). The entries are ListCollections and
// ListViews (the configured schema), ListSchemaCollections and ListSchemaViews
// (a schema name), DescribeCollection, ListIndexes, ListConstraints,
// ListReferrers and NonDeterministicTextColumns (a table name and, when the
// reference names one, a schema name) and JoinFields (a source). ListSchemas
// takes no name, and no entry takes a column name.

// nameOf63 is a name of exactly 63 bytes that starts with prefix and is padded.
func nameOf63(prefix string) string {
	return prefix + strings.Repeat("z", 63-len(prefix))
}

// answer is what one call of a schema-reader entry came to: how many items it
// returned, and its error.
type answer struct {
	items int
	err   error
}

// schemaNameCall is one entry of the schema reader called with a name.
type schemaNameCall struct {
	entry string
	call  func(ctx context.Context) answer
}

// schemaNameFixture holds the objects the calls are made against: a schema of 63
// bytes with a table "t", and, in the configured schema, a table of 63 bytes with
// a primary key, an index, a view over it and a referring table.
type schemaNameFixture struct {
	configured string // the schema the Database reads, short
	longSchema string // a schema of 63 bytes
	table      string // a table of 63 bytes in the configured schema
	db         *Database
}

func newSchemaNameFixture(t *testing.T) schemaNameFixture {
	t.Helper()
	configured := uniqueTable(t, "pg08")
	db := openSchemaTestDB(t, configured, IdentifierExact)
	longSchema := nameOf63("dalgo_pg08_long_schema_")
	table := nameOf63("t63_")
	s, ls := exactIdent(configured), exactIdent(longSchema)
	ctx := context.Background()
	_, _ = db.sqlDB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+ls+` CASCADE`)
	t.Cleanup(func() { _, _ = db.sqlDB.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+ls+` CASCADE`) })
	execAll(t, db,
		`CREATE SCHEMA `+ls,
		`CREATE TABLE `+ls+`.t (id integer PRIMARY KEY, label text)`,
		`CREATE VIEW `+ls+`.v AS SELECT id FROM `+ls+`.t`,
		`CREATE TABLE `+s+`.`+exactIdent(table)+` (id integer PRIMARY KEY, label text)`,
		`CREATE INDEX ix_pg08 ON `+s+`.`+exactIdent(table)+` (label)`,
		`CREATE VIEW `+s+`.`+exactIdent(nameOf63("v63_"))+` AS SELECT id FROM `+s+`.`+exactIdent(table),
		`CREATE TABLE `+s+`.referrer (id integer PRIMARY KEY, t_id integer REFERENCES `+s+`.`+exactIdent(table)+` (id))`,
	)
	return schemaNameFixture{configured: configured, longSchema: longSchema, table: table, db: db}
}

// withSchema opens a second Database over the same server that reads schema.
func (f schemaNameFixture) withSchema(t *testing.T, schema string) *Database {
	t.Helper()
	db, err := NewDatabase(testDSN(t), WithSchema(schema), WithIdentifierMode(IdentifierExact))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// calls lists every entry with the table and schema names given.
func (f schemaNameFixture) calls(t *testing.T, table, schema string) []schemaNameCall {
	t.Helper()
	inConfigured := dal.NewRootCollectionRef(table, "")
	inLongSchema := dal.NewQualifiedRootCollectionRef(schema, "t", "")
	onLongSchema := f.withSchema(t, schema)
	return []schemaNameCall{
		{"ListCollections", func(ctx context.Context) answer {
			refs, err := onLongSchema.ListCollections(ctx, nil)
			return answer{len(refs), err}
		}},
		{"ListViews", func(ctx context.Context) answer {
			refs, err := onLongSchema.ListViews(ctx)
			return answer{len(refs), err}
		}},
		{"ListSchemaCollections", func(ctx context.Context) answer {
			refs, err := f.db.ListSchemaCollections(ctx, schema)
			return answer{len(refs), err}
		}},
		{"ListSchemaViews", func(ctx context.Context) answer {
			refs, err := f.db.ListSchemaViews(ctx, schema)
			return answer{len(refs), err}
		}},
		{"DescribeCollection (table)", func(ctx context.Context) answer {
			def, err := f.db.DescribeCollection(ctx, &inConfigured)
			if def == nil {
				return answer{0, err}
			}
			return answer{len(def.Fields), err}
		}},
		{"DescribeCollection (schema)", func(ctx context.Context) answer {
			def, err := f.db.DescribeCollection(ctx, &inLongSchema)
			if def == nil {
				return answer{0, err}
			}
			return answer{len(def.Fields), err}
		}},
		{"ListIndexes", func(ctx context.Context) answer {
			indexes, err := f.db.ListIndexes(ctx, &inConfigured)
			return answer{len(indexes), err}
		}},
		{"ListConstraints", func(ctx context.Context) answer {
			constraints, err := f.db.ListConstraints(ctx, &inConfigured)
			return answer{len(constraints), err}
		}},
		{"ListReferrers", func(ctx context.Context) answer {
			referrers, err := f.db.ListReferrers(ctx, &inConfigured)
			return answer{len(referrers), err}
		}},
		{"NonDeterministicTextColumns (table)", func(ctx context.Context) answer {
			columns, err := f.db.NonDeterministicTextColumns(ctx, &inConfigured)
			if err != nil {
				return answer{0, err}
			}
			return answer{1 + len(columns), nil} // 1: the table was found
		}},
		{"NonDeterministicTextColumns (schema)", func(ctx context.Context) answer {
			columns, err := f.db.NonDeterministicTextColumns(ctx, &inLongSchema)
			if err != nil {
				return answer{0, err}
			}
			return answer{1 + len(columns), nil}
		}},
		{"JoinFields", func(ctx context.Context) answer {
			fields, err := f.db.JoinFields(ctx, dal.NewQualifiedRootCollectionRef(f.configured, table, ""))
			return answer{len(fields), err}
		}},
	}
}

func TestSchemaReaderIntegration_NameOf64BytesAgainstAnObjectOf63(t *testing.T) {
	f := newSchemaNameFixture(t)
	ctx := context.Background()

	// The control: with the names of the objects, each entry finds them. A failure
	// here is a fault of the fixture, not of the behaviour under test.
	for _, c := range f.calls(t, f.table, f.longSchema) {
		if got := c.call(ctx); got.err != nil || got.items == 0 {
			t.Errorf("%s with a name of 63 bytes: %d items, err %v; want its answer", c.entry, got.items, got.err)
		}
	}

	// A name of 64 bytes, the 63 bytes of an object that exists and one more, is
	// refused by every entry with the error of the key paths.
	for _, c := range f.calls(t, f.table+"x", f.longSchema+"x") {
		got := c.call(ctx)
		t.Logf("%s with 64 bytes: %d items, err %v", c.entry, got.items, got.err)
		if c.entry == "JoinFields" {
			// dalgo2sql refuses it in its own words, before the catalog is read: it is
			// not the schema reader's rule, and is held here as it is observed.
			if got.items != 0 || got.err == nil || !strings.Contains(got.err.Error(), "over the engine limit of 63") {
				t.Errorf("JoinFields with a name of 64 bytes: %d items, err %v; want dalgo2sql's refusal", got.items, got.err)
			}
			continue
		}
		if got.items != 0 || !errors.Is(got.err, dalgo2sql.ErrUnsafeName) || !strings.Contains(got.err.Error(), "it keeps 63 bytes") {
			t.Errorf("%s with a name of 64 bytes: %d items, err %v; want a refusal that matches dalgo2sql.ErrUnsafeName and says the server keeps 63 bytes", c.entry, got.items, got.err)
		}
	}

	// The rule is in bytes, as the server's: a table of 31 two-byte characters (62
	// bytes) is found by its name and refused by one of 32 two-byte characters (64
	// bytes).
	twoByte := strings.Repeat("é", 31)
	execAll(t, f.db, `CREATE TABLE `+exactIdent(f.configured)+`.`+exactIdent(twoByte)+` (id integer PRIMARY KEY)`)
	found := dal.NewRootCollectionRef(twoByte, "")
	if def, err := f.db.DescribeCollection(ctx, &found); err != nil || len(def.Fields) != 1 {
		t.Fatalf("DescribeCollection of 62 bytes in 31 characters = %v, %v; want its one column", def, err)
	}
	refused := dal.NewRootCollectionRef(twoByte+"é", "")
	if def, err := f.db.DescribeCollection(ctx, &refused); def != nil || !errors.Is(err, dalgo2sql.ErrUnsafeName) {
		t.Errorf("DescribeCollection of 64 bytes in 32 characters = %v, %v; want a refusal that matches dalgo2sql.ErrUnsafeName", def, err)
	}

	// Nothing was changed by the refusals: the objects are still there.
	if refs, err := f.db.ListCollections(ctx, nil); err != nil || len(refs) != 4 {
		t.Errorf("ListCollections afterwards = %v, %v; want the table, its view, the referrer and the table of two-byte characters", refs, err)
	}
}
