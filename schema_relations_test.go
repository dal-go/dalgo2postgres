package dalgo2postgres

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// viewLister is the optional interface the DataTug schema provider looks for
// (datatug-cli, pkg/schemers/dalgoschema.ViewLister), copied here so a change to
// the method breaks this build, not a scan.
type viewLister interface {
	ListViews(ctx context.Context) ([]dal.CollectionRef, error)
}

var _ viewLister = (*Database)(nil)

const listSchemasQuery = `FROM pg_catalog\.pg_namespace AS n`

func refNames(refs []dal.CollectionRef) []string {
	var out []string
	for _, ref := range refs {
		out = append(out, ref.Schema()+"|"+ref.Name())
	}
	return out
}

func TestListSchemas(t *testing.T) {
	ctx := context.Background()
	t.Run("in name order, as the server answers", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listSchemasQuery).WillReturnRows(
			sqlmock.NewRows([]string{"nspname"}).AddRow("analytics").AddRow("public").AddRow("Sales"))
		got, err := d.ListSchemas(ctx)
		if err != nil || !reflect.DeepEqual(got, []string{"analytics", "public", "Sales"}) {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("the statement leaves out the system schemas and takes no argument", func(t *testing.T) {
		for _, fragment := range []string{
			`'pg_catalog'`, `'information_schema'`, `'pg_toast'`, `'pg\_temp\_%'`, `'pg\_toast\_temp\_%'`, `has_schema_privilege`, `ORDER BY n.nspname`,
		} {
			if !strings.Contains(listSchemasSQL, fragment) {
				t.Errorf("listSchemasSQL does not contain %s:\n%s", fragment, listSchemasSQL)
			}
		}
		if strings.Contains(listSchemasSQL, "$") {
			t.Errorf("listSchemasSQL has a parameter, and no name goes into it")
		}
	})
	t.Run("errors", func(t *testing.T) {
		boom := errors.New("boom")
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listSchemasQuery).WillReturnError(boom)
		if _, err := d.ListSchemas(ctx); !errors.Is(err, boom) || !strings.Contains(err.Error(), "ListSchemas") {
			t.Errorf("query: err = %v", err)
		}
		d, mock = newSchemaMockDatabase(t)
		mock.ExpectQuery(listSchemasQuery).WillReturnRows(sqlmock.NewRows([]string{"nspname"}).AddRow(nil))
		if _, err := d.ListSchemas(ctx); err == nil || !strings.Contains(err.Error(), "ListSchemas scan") {
			t.Errorf("scan: err = %v", err)
		}
		d, mock = newSchemaMockDatabase(t)
		mock.ExpectQuery(listSchemasQuery).WillReturnRows(sqlmock.NewRows([]string{"nspname"}).AddRow("a").RowError(0, boom))
		if _, err := d.ListSchemas(ctx); !errors.Is(err, boom) || !strings.Contains(err.Error(), "ListSchemas rows") {
			t.Errorf("rows: err = %v", err)
		}
	})
}

func TestListSchemaCollectionsAndViews(t *testing.T) {
	ctx := context.Background()
	rows := func() *sqlmock.Rows {
		return relationRows().AddRow("Album", false).AddRow("AlbumTitles", true).AddRow("Sold", true).AddRow("Remote", false)
	}
	t.Run("a named schema is bound and every reference names it", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listRelationsQuery).WithArgs("sales").WillReturnRows(rows())
		got, err := d.ListSchemaCollections(ctx, "sales")
		want := []string{"sales|Album", "sales|AlbumTitles", "sales|Sold", "sales|Remote"}
		if err != nil || !reflect.DeepEqual(refNames(got), want) {
			t.Fatalf("got %v, %v; want %v", refNames(got), err, want)
		}
	})
	t.Run("an empty schema is the configured one", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithSchema("billing"))
		mock.ExpectQuery(listRelationsQuery).WithArgs("billing").WillReturnRows(relationRows().AddRow("Invoice", false))
		got, err := d.ListSchemaCollections(ctx, "")
		if err != nil || !reflect.DeepEqual(refNames(got), []string{"billing|Invoice"}) {
			t.Fatalf("got %v, %v", refNames(got), err)
		}
	})
	t.Run("views only, in a named schema", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listRelationsQuery).WithArgs("sales").WillReturnRows(rows())
		got, err := d.ListSchemaViews(ctx, "sales")
		if err != nil || !reflect.DeepEqual(refNames(got), []string{"sales|AlbumTitles", "sales|Sold"}) {
			t.Fatalf("got %v, %v", refNames(got), err)
		}
	})
	t.Run("views of a named schema, the configured one when empty", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listRelationsQuery).WithArgs("public").WillReturnRows(rows())
		got, err := d.ListSchemaViews(ctx, "")
		if err != nil || !reflect.DeepEqual(refNames(got), []string{"public|AlbumTitles", "public|Sold"}) {
			t.Fatalf("got %v, %v", refNames(got), err)
		}
	})
	t.Run("ListViews names no schema, like ListCollections, and leaves the tables out", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithSchema("sales"))
		mock.ExpectQuery(listRelationsQuery).WithArgs("sales").WillReturnRows(rows())
		got, err := d.ListViews(ctx)
		if err != nil || !reflect.DeepEqual(refNames(got), []string{"|AlbumTitles", "|Sold"}) {
			t.Fatalf("got %v, %v", refNames(got), err)
		}
	})
	t.Run("no view is no references", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listRelationsQuery).WithArgs("public").WillReturnRows(relationRows().AddRow("t", false))
		if got, err := d.ListViews(ctx); err != nil || got != nil {
			t.Fatalf("got %v, %v; want nil and nil", got, err)
		}
	})
	t.Run("errors name the method", func(t *testing.T) {
		boom := errors.New("boom")
		calls := map[string]func(d *Database) error{
			"ListViews":             func(d *Database) error { _, err := d.ListViews(ctx); return err },
			"ListSchemaViews":       func(d *Database) error { _, err := d.ListSchemaViews(ctx, "s"); return err },
			"ListSchemaCollections": func(d *Database) error { _, err := d.ListSchemaCollections(ctx, "s"); return err },
		}
		for name, call := range calls {
			d, mock := newSchemaMockDatabase(t)
			mock.ExpectQuery(listRelationsQuery).WillReturnError(boom)
			if err := call(d); !errors.Is(err, boom) || !strings.Contains(err.Error(), name) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})
}

func TestListRelationsStatementListsTheKindsAndNoName(t *testing.T) {
	// The kinds are the listed ones, the schema is the only parameter, and the
	// privilege test is the one information_schema.tables applies.
	for _, fragment := range []string{
		`relkind IN ('r', 'p', 'f', 'v', 'm')`, `n.nspname = $1`, `ORDER BY c.relname`,
		`pg_has_role(c.relowner, 'USAGE')`, `has_table_privilege(c.oid,`, `has_any_column_privilege(c.oid,`, `pg_is_other_temp_schema`,
	} {
		if !strings.Contains(listRelationsSQL, fragment) {
			t.Errorf("listRelationsSQL does not contain %s", fragment)
		}
	}
	if strings.Contains(listRelationsSQL, "$2") {
		t.Errorf("listRelationsSQL takes a second parameter")
	}
}

func TestDescribeCollection_MaterializedViewIsReadFromPgAttribute(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	ref := dal.NewQualifiedRootCollectionRef("sales", "Sold", "")
	mock.ExpectQuery(qProbe).WithArgs("sales", "Sold").WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow("m"))
	expectPK(mock, "sales", "Sold")
	mock.ExpectQuery(`FROM pg_catalog\.pg_attribute AS a`).WithArgs("sales", "Sold").WillReturnRows(sqlmock.NewRows(columnHeader).
		AddRow("Total", "numeric", "numeric", nilInt(), int64(10), int64(2), "YES", nil, "NO", "NEVER", nil).
		AddRow("Note", "character varying", "varchar", int64(20), nilInt(), nilInt(), "NO", nil, "NO", "NEVER", nil))
	mock.ExpectQuery(qIndexes).WithArgs("sales", "Sold").WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	mock.ExpectQuery(qFKs).WithArgs("sales", "Sold").WillReturnRows(sqlmock.NewRows([]string{"a", "b", "c", "d", "e", "f", "g", "h"}))

	got, err := d.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if len(got.Fields) != 2 || got.Fields[0].Type != dbschema.Decimal || !got.Fields[0].Nullable ||
		got.Fields[0].Precision == nil || *got.Fields[0].Precision != (dbschema.Precision{Total: 10, Scale: 2}) ||
		got.Fields[1].Type != dbschema.String || got.Fields[1].Nullable || *got.Fields[1].Length != 20 {
		t.Errorf("fields = %+v", got.Fields)
	}
	for _, f := range got.Fields {
		if f.Default != nil || f.AutoIncrement {
			t.Errorf("%s: a materialized view column has no default or identity: %+v", f.Name, f)
		}
	}
}

func TestDescribeCollection_ReadsDefaultsIdentityAndGeneration(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	ref := dal.NewRootCollectionRef("t", "")
	expectProbeFound(mock, "public", "t")
	expectPK(mock, "public", "t", "id")
	mock.ExpectQuery(qColumns).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows(columnHeader).
		AddRow("id", "bigint", "int8", nilInt(), nilInt(), nilInt(), "NO", nil, "YES", "NEVER", nil).
		AddRow("status", "text", "text", nilInt(), nilInt(), nilInt(), "NO", "'new'::text", "NO", "NEVER", nil).
		AddRow("made", "timestamp with time zone", "timestamptz", nilInt(), nilInt(), nilInt(), "NO", "now()", "NO", "NEVER", nil).
		AddRow("seq", "bigint", "int8", nilInt(), nilInt(), nilInt(), "NO", "nextval('t_seq'::regclass)", "NO", "NEVER", nil).
		AddRow("double", "integer", "int4", nilInt(), nilInt(), nilInt(), "YES", nil, "NO", "ALWAYS", "(n * 2)").
		AddRow("plain", "text", "text", nilInt(), nilInt(), nilInt(), "YES", nil, "NO", "NEVER", nil))
	mock.ExpectQuery(qIndexes).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	mock.ExpectQuery(qFKs).WillReturnRows(sqlmock.NewRows([]string{"a", "b", "c", "d", "e", "f", "g", "h"}))

	got, err := d.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	want := []struct {
		name     string
		def      dbschema.DefaultExpr
		autoIncr bool
	}{
		{"id", nil, true},
		{"status", dbschema.DefaultLiteral{Value: "'new'::text"}, false},
		{"made", dbschema.DefaultLiteral{Value: "now()"}, false},
		{"seq", dbschema.DefaultLiteral{Value: "nextval('t_seq'::regclass)"}, false},
		{"double", dbschema.DefaultLiteral{Value: "GENERATED ALWAYS AS ((n * 2))"}, false},
		{"plain", nil, false},
	}
	if len(got.Fields) != len(want) {
		t.Fatalf("fields = %+v", got.Fields)
	}
	for i, w := range want {
		f := got.Fields[i]
		if string(f.Name) != w.name || !reflect.DeepEqual(f.Default, w.def) || f.AutoIncrement != w.autoIncr {
			t.Errorf("field %d = {%s default=%#v autoIncrement=%v}, want {%s default=%#v autoIncrement=%v}", i, f.Name, f.Default, f.AutoIncrement, w.name, w.def, w.autoIncr)
		}
	}
}

func TestColumnDefaultOf(t *testing.T) {
	text := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	none := sql.NullString{}
	tests := []struct {
		name       string
		def        sql.NullString
		generated  string
		generation sql.NullString
		want       dbschema.DefaultExpr
	}{
		{"no default", none, "NEVER", none, nil},
		{"constant", text("'x'::text"), "NEVER", none, dbschema.DefaultLiteral{Value: "'x'::text"}},
		{"generated", none, "ALWAYS", text("(a + b)"), dbschema.DefaultLiteral{Value: "GENERATED ALWAYS AS ((a + b))"}},
		{"generated without its expression falls back to the default", text("1"), "ALWAYS", none, dbschema.DefaultLiteral{Value: "1"}},
		{"generated beats a default", text("1"), "ALWAYS", text("2"), dbschema.DefaultLiteral{Value: "GENERATED ALWAYS AS (2)"}},
		{"an empty default is a default", text(""), "NEVER", none, dbschema.DefaultLiteral{Value: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := columnDefaultOf(tt.def, tt.generated, tt.generation); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMaterializedViewColumnsStatementAnswersTheShapeOfInformationSchema(t *testing.T) {
	// Eleven columns in the order informationSchemaColumnsSQL answers them; the
	// last four are the constants of a column with no default, identity or
	// generation. Schema and name are the only parameters, sent as text.
	for _, fragment := range []string{
		`pg_catalog.format_type(t.oid, NULL)`, `'ARRAY'`, `'USER-DEFINED'`, `NULL::text, 'NO', 'NEVER', NULL::text`,
		`n.nspname = $1::text AND c.relname = $2::text`, `ORDER BY a.attnum`, `NOT a.attisdropped`,
	} {
		if !strings.Contains(materializedViewColumnsSQL, fragment) {
			t.Errorf("materializedViewColumnsSQL does not contain %s", fragment)
		}
	}
	for _, fragment := range []string{`column_default`, `is_identity`, `is_generated`, `generation_expression`, `table_schema = $1::text AND table_name = $2::text`} {
		if !strings.Contains(informationSchemaColumnsSQL, fragment) {
			t.Errorf("informationSchemaColumnsSQL does not contain %s", fragment)
		}
	}
}
