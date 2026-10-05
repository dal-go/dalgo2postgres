package dalgo2postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
)

// PostgreSQL keeps 63 bytes of an identifier. The schema reader refuses a schema
// or table name over 63 bytes with the error the key paths return for it, before
// any statement. These tests run on a go-sqlmock handle that has no expectation,
// so a statement fails the test (the error of the call would then not be the
// refusal), and the real server's answers are in
// TestSchemaReaderIntegration_NameOf64BytesAgainstAnObjectOf63.

// schemaReaderEntries calls every entry of the schema reader that takes a name
// from the caller: table is the table name of the entries that take a reference,
// schema the schema name of the entries that take one, and the configured schema
// of d is read by ListCollections and ListViews.
func schemaReaderEntries(d *Database, table, schema string) map[string]func(ctx context.Context) error {
	inConfigured := dal.NewRootCollectionRef(table, "")
	inNamedSchema := dal.NewQualifiedRootCollectionRef(schema, "t", "")
	return map[string]func(ctx context.Context) error{
		"ListCollections": func(ctx context.Context) error { _, err := d.ListCollections(ctx, nil); return err },
		"ListViews":       func(ctx context.Context) error { _, err := d.ListViews(ctx); return err },
		"ListSchemaCollections": func(ctx context.Context) error {
			_, err := d.ListSchemaCollections(ctx, schema)
			return err
		},
		"ListSchemaViews": func(ctx context.Context) error { _, err := d.ListSchemaViews(ctx, schema); return err },
		"DescribeCollection (table)": func(ctx context.Context) error {
			_, err := d.DescribeCollection(ctx, &inConfigured)
			return err
		},
		"DescribeCollection (schema)": func(ctx context.Context) error {
			_, err := d.DescribeCollection(ctx, &inNamedSchema)
			return err
		},
		"ListIndexes (table)":  func(ctx context.Context) error { _, err := d.ListIndexes(ctx, &inConfigured); return err },
		"ListIndexes (schema)": func(ctx context.Context) error { _, err := d.ListIndexes(ctx, &inNamedSchema); return err },
		"ListConstraints (table)": func(ctx context.Context) error {
			_, err := d.ListConstraints(ctx, &inConfigured)
			return err
		},
		"ListConstraints (schema)": func(ctx context.Context) error {
			_, err := d.ListConstraints(ctx, &inNamedSchema)
			return err
		},
		"ListReferrers (table)":  func(ctx context.Context) error { _, err := d.ListReferrers(ctx, &inConfigured); return err },
		"ListReferrers (schema)": func(ctx context.Context) error { _, err := d.ListReferrers(ctx, &inNamedSchema); return err },
		"NonDeterministicTextColumns (table)": func(ctx context.Context) error {
			_, err := d.NonDeterministicTextColumns(ctx, &inConfigured)
			return err
		},
		"NonDeterministicTextColumns (schema)": func(ctx context.Context) error {
			_, err := d.NonDeterministicTextColumns(ctx, &inNamedSchema)
			return err
		},
	}
}

// refusesName reports whether err is the refusal of a name over 63 bytes.
func refusesName(err error) bool {
	return errors.Is(err, dalgo2sql.ErrUnsafeName) && strings.Contains(err.Error(), "it keeps 63 bytes")
}

func TestSchemaReader_RefusesANameOver63BytesBeforeAnyStatement(t *testing.T) {
	long := strings.Repeat("n", 64)
	twoBytes := strings.Repeat("é", 32) // 32 characters, 64 bytes
	for _, tc := range []struct {
		name        string
		table, schm string
		configured  string // the configured schema, "" for the default
		only        string // the entries that take this name, as a substring of their key
	}{
		{"a table of 64 bytes", long, "public", "", "(table)"},
		{"a schema of 64 bytes", "t", long, "", "(schema)"},
		{"a schema of 64 bytes named by the entry", "t", long, "", "ListSchema"},
		{"a table of 32 two-byte characters", twoBytes, "public", "", "(table)"},
		{"a schema of 32 two-byte characters", "t", twoBytes, "", "(schema)"},
		{"a configured schema of 64 bytes", "t", "public", long, "ListCollections"},
		{"a configured schema of 64 bytes, views", "t", "public", long, "ListViews"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var options []Option
			if tc.configured != "" {
				options = append(options, WithSchema(tc.configured))
			}
			d, _ := newSchemaMockDatabase(t, append(options, WithIdentifierMode(IdentifierExact))...)
			ran := 0
			for entry, call := range schemaReaderEntries(d, tc.table, tc.schm) {
				if !strings.Contains(entry, tc.only) {
					continue
				}
				ran++
				if err := call(context.Background()); !refusesName(err) {
					t.Errorf("%s: error = %v, want a refusal that matches dalgo2sql.ErrUnsafeName and says the server keeps 63 bytes", entry, err)
				}
			}
			if ran == 0 {
				t.Fatalf("no entry matched %q", tc.only)
			}
		})
	}
}

func TestSchemaReader_RefusalIsTheErrorOfTheKeyPaths(t *testing.T) {
	// The wording is dalgo2sql's: the schema reader's refusal of a table is the
	// text a key path gives for the same name as a collection, including the part
	// of a long name it shows.
	for _, name := range []string{strings.Repeat("n", 64), strings.Repeat("n", 200), strings.Repeat("é", 32), strings.Repeat("é", 40)} {
		sqlDB, _ := newStructuredMock(t)
		keyDB := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		_, want := keyDB.Exists(context.Background(), dalrecord.NewKeyWithID(name, "id1"))
		if !errors.Is(want, dalgo2sql.ErrUnsafeName) {
			t.Fatalf("a key path's error for a name of %d bytes = %v, want ErrUnsafeName", len(name), want)
		}

		d, _ := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		ref := dal.NewRootCollectionRef(name, "")
		_, got := d.DescribeCollection(context.Background(), &ref)
		if got == nil || got.Error() != want.Error() {
			t.Errorf("name of %d bytes: schema reader error = %v\n key path error = %v\nwant the same", len(name), got, want)
		}
	}
}

func TestSchemaReader_RefusalNamesTheSchemaAndShowsLittleOfTheName(t *testing.T) {
	d, _ := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	_, err := d.ListSchemaCollections(context.Background(), strings.Repeat("s", 100))
	want := `unsafe SQL name: schema name "` + strings.Repeat("s", 32) + `" (truncated) is too long for PostgreSQL: it keeps 63 bytes`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestSchemaReader_AcceptsANameOfExactly63Bytes(t *testing.T) {
	name := strings.Repeat("n", 63)
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(listRelationsQuery).WithArgs(name).WillReturnRows(relationRows().AddRow("t", false))
	refs, err := d.ListSchemaCollections(context.Background(), name)
	if err != nil || len(refs) != 1 {
		t.Fatalf("ListSchemaCollections = %v, %v; want the one collection", refs, err)
	}

	mock.ExpectQuery(qProbe).WithArgs("public", name).WillReturnError(errors.New("boom"))
	ref := dal.NewRootCollectionRef(name, "")
	if _, err := d.DescribeCollection(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("DescribeCollection of a table of 63 bytes = %v, want the statement to have been sent", err)
	}

	// 31 two-byte characters and an ASCII one are 63 bytes.
	mixed := strings.Repeat("é", 31) + "e"
	mock.ExpectQuery(listRelationsQuery).WithArgs(mixed).WillReturnRows(relationRows())
	if _, err := d.ListSchemaViews(context.Background(), mixed); err != nil {
		t.Errorf("ListSchemaViews of 63 bytes in 32 characters = %v, want no refusal", err)
	}
}

func TestSchemaReader_CountsTheNameThatIsLookedUp(t *testing.T) {
	// Fold-lower mode looks up the lower-cased table name, so that is the name the
	// server would cut: 31 capital A with a stroke (2 bytes) and an "a" are 63
	// bytes as given and 94 once lower-cased (U+2C65 is 3 bytes).
	name := strings.Repeat("Ⱥ", 31) + "a"
	if len(name) != 63 {
		t.Fatalf("len = %d, want 63", len(name))
	}
	ref := dal.NewRootCollectionRef(name, "")

	folding, _ := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierFoldLower))
	if _, err := folding.DescribeCollection(context.Background(), &ref); !refusesName(err) {
		t.Errorf("fold-lower: error = %v, want a refusal of the lower-cased name", err)
	}

	exact, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qProbe).WithArgs("public", name).WillReturnError(sqlmock.ErrCancelled)
	if _, err := exact.DescribeCollection(context.Background(), &ref); refusesName(err) {
		t.Errorf("exact: error = %v, want the name sent as given", err)
	}
}
