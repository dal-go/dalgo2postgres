package dalgo2postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// The stage queries of one DescribeCollection call, in the order they run.
const (
	qProbe   = `information_schema\.tables`
	qPK      = `tc\.constraint_type = 'PRIMARY KEY'`
	qColumns = `information_schema\.columns`
	qIndexes = `pg_indexes`
	qFKs     = `c\.contype = 'f'`
)

var columnHeader = []string{"column_name", "data_type", "udt_name", "character_maximum_length", "numeric_precision", "numeric_scale", "is_nullable"}

func nilInt() driver.Value { return nil }

// albumColumns is a table with the types a real catalog contains and the old reader refused.
func albumColumns() *sqlmock.Rows {
	return sqlmock.NewRows(columnHeader).
		AddRow("AlbumId", "integer", "int4", nilInt(), int64(32), int64(0), "NO").
		AddRow("Title", "character varying", "varchar", int64(160), nilInt(), nilInt(), "NO").
		AddRow("Rating", "numeric", "numeric", nilInt(), int64(10), int64(2), "YES").
		AddRow("ExternalId", "uuid", "uuid", nilInt(), nilInt(), nilInt(), "YES").
		AddRow("Meta", "jsonb", "jsonb", nilInt(), nilInt(), nilInt(), "YES").
		AddRow("Tags", "ARRAY", "_text", nilInt(), nilInt(), nilInt(), "YES").
		AddRow("Mood", "USER-DEFINED", "mood", nilInt(), nilInt(), nilInt(), "YES").
		AddRow("Length", "interval", "interval", nilInt(), nilInt(), nilInt(), "YES").
		AddRow("Addr", "inet", "inet", nilInt(), nilInt(), nilInt(), "YES").
		AddRow("Price", "money", "money", nilInt(), nilInt(), nilInt(), "YES")
}

func expectProbeFound(mock sqlmock.Sqlmock, schema, name string) {
	mock.ExpectQuery(qProbe).WithArgs(schema, name).
		WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow(name))
}

func expectPK(mock sqlmock.Sqlmock, schema, name string, cols ...string) {
	rows := sqlmock.NewRows([]string{"column_name"})
	for _, c := range cols {
		rows.AddRow(c)
	}
	mock.ExpectQuery(qPK).WithArgs(schema, name).WillReturnRows(rows)
}

func TestListCollections_QueriesTablesAndViewsInSchema(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(`table_type IN \('BASE TABLE', 'VIEW'\)`).WithArgs("sales").
		WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow("Album").AddRow("album_view"))

	got, err := d.ListCollections(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	var names []string
	for _, ref := range got {
		names = append(names, ref.Name())
	}
	if want := []string{"Album", "album_view"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v (case preserved, views included)", names, want)
	}
}

func TestListCollections_DefaultsToPublic(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(`information_schema\.tables`).WithArgs("public").
		WillReturnRows(sqlmock.NewRows([]string{"table_name"}))
	got, err := d.ListCollections(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty and nil", got, err)
	}
}

func TestListCollections_Errors(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`information_schema\.tables`).WillReturnError(errors.New("boom"))
		if _, err := d.ListCollections(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want wrapped boom", err)
		}
	})
	t.Run("scan", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`information_schema\.tables`).
			WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow(nil))
		if _, err := d.ListCollections(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "scan") {
			t.Fatalf("err = %v, want scan error", err)
		}
	})
	t.Run("rows", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`information_schema\.tables`).
			WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow("a").RowError(0, errors.New("row broke")))
		if _, err := d.ListCollections(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "row broke") {
			t.Fatalf("err = %v, want row error", err)
		}
	})
}

// describeAlbum arranges every stage of DescribeCollection for the Album table.
func describeAlbum(mock sqlmock.Sqlmock, schema, name, fkNamespace string) {
	expectProbeFound(mock, schema, name)
	expectPK(mock, schema, name, "AlbumId")
	mock.ExpectQuery(qColumns).WithArgs(schema, name).WillReturnRows(albumColumns())
	mock.ExpectQuery(qIndexes).WithArgs(schema, name).WillReturnRows(
		sqlmock.NewRows([]string{"indexname", "indexdef"}).
			AddRow("Album_Title_idx", `CREATE INDEX "Album_Title_idx" ON public."Album" USING btree ("Title")`).
			AddRow("Album_Ext_uq", `CREATE UNIQUE INDEX "Album_Ext_uq" ON public."Album" USING btree ("ExternalId", "Title" DESC)`))
	mock.ExpectQuery(qFKs).WithArgs(schema, name).WillReturnRows(
		sqlmock.NewRows([]string{"oid", "conname", "from", "ns", "target", "to", "upd", "del"}).
			AddRow(int64(7), "Album_Artist_fk", "ArtistId", fkNamespace, "Artist", "ArtistId", "NO ACTION", "CASCADE").
			AddRow(int64(8), "Album_Pair_fk", "A", "other", "Pair", "A", "NO ACTION", "NO ACTION").
			AddRow(int64(8), "Album_Pair_fk", "B", "other", "Pair", "B", "NO ACTION", "NO ACTION"))
}

func TestDescribeCollection_ExactNameWithUnknownTypes(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	describeAlbum(mock, "public", "Album", "public")

	ref := dal.NewRootCollectionRef("Album", "")
	got, err := d.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if got.Name != "Album" {
		t.Errorf("Name = %q, want Album (case preserved)", got.Name)
	}

	wantTypes := []struct {
		name     string
		typ      dbschema.Type
		nullable bool
	}{
		{"AlbumId", dbschema.Int, false}, {"Title", dbschema.String, false}, {"Rating", dbschema.Decimal, true},
		{"ExternalId", dbschema.String, true}, {"Meta", dbschema.String, true}, {"Tags", dbschema.String, true},
		{"Mood", dbschema.String, true}, {"Length", dbschema.String, true}, {"Addr", dbschema.String, true},
		{"Price", dbschema.String, true},
	}
	if len(got.Fields) != len(wantTypes) {
		t.Fatalf("Fields = %d, want %d (every column, none dropped): %+v", len(got.Fields), len(wantTypes), got.Fields)
	}
	for i, w := range wantTypes {
		f := got.Fields[i]
		if string(f.Name) != w.name || f.Type != w.typ || f.Nullable != w.nullable {
			t.Errorf("field %d = {%s %v nullable=%v}, want {%s %v nullable=%v}", i, f.Name, f.Type, f.Nullable, w.name, w.typ, w.nullable)
		}
	}
	if title := got.Fields[1]; title.Length == nil || *title.Length != 160 {
		t.Errorf("Title length = %v, want 160", title.Length)
	}
	if rating := got.Fields[2]; rating.Precision == nil || *rating.Precision != (dbschema.Precision{Total: 10, Scale: 2}) {
		t.Errorf("Rating precision = %+v, want (10,2)", rating.Precision)
	}
	if !reflect.DeepEqual(got.PrimaryKey, []dal.FieldName{"AlbumId"}) {
		t.Errorf("PrimaryKey = %v, want [AlbumId]", got.PrimaryKey)
	}

	if len(got.Indexes) != 2 || got.Indexes[0].Name != "Album_Title_idx" || got.Indexes[0].Collection != "Album" || got.Indexes[0].Unique ||
		!reflect.DeepEqual(got.Indexes[0].Fields, []dal.FieldName{"Title"}) {
		t.Errorf("first index = %+v", got.Indexes)
	}
	if !got.Indexes[1].Unique || !reflect.DeepEqual(got.Indexes[1].Fields, []dal.FieldName{"ExternalId", "Title"}) {
		t.Errorf("second index = %+v, want unique over ExternalId, Title", got.Indexes[1])
	}

	if len(got.ForeignKeys) != 2 {
		t.Fatalf("ForeignKeys = %+v, want 2", got.ForeignKeys)
	}
	artist := got.ForeignKeys[0]
	if artist.ReferencedCollection != "Artist" || artist.ReferencedNamespace != "" || artist.OnDelete != "CASCADE" {
		t.Errorf("artist FK = %+v (target in the configured schema has no namespace)", artist)
	}
	pair := got.ForeignKeys[1]
	if pair.ReferencedNamespace != "other" || len(pair.Fields) != 2 || len(pair.ReferencedFields) != 2 {
		t.Errorf("pair FK = %+v (cross-schema target keeps its namespace, composite key grouped)", pair)
	}
}

func TestDescribeCollection_FoldLowerIsTheDefault(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	describeAlbum(mock, "public", "album", "public")

	ref := dal.NewRootCollectionRef("Album", "")
	got, err := d.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if got.Name != "album" {
		t.Errorf("Name = %q, want album", got.Name)
	}
}

func TestDescribeCollection_SecondSchema(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	// In schema "sales" a target that is also in "sales" has no namespace.
	describeAlbum(mock, "sales", "Album", "sales")

	ref := dal.NewRootCollectionRef("Album", "")
	got, err := d.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if got.ForeignKeys[0].ReferencedNamespace != "" {
		t.Errorf("same-schema target namespace = %q, want empty", got.ForeignKeys[0].ReferencedNamespace)
	}
}

func TestDescribeCollection_ViewHasNoKey(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	expectProbeFound(mock, "public", "v")
	expectPK(mock, "public", "v")
	mock.ExpectQuery(qColumns).WillReturnRows(sqlmock.NewRows(columnHeader).
		AddRow("n", "bigint", "int8", nilInt(), nilInt(), nilInt(), "YES").
		AddRow("code", "VARCHAR(40)", "varchar", nilInt(), nilInt(), nilInt(), "YES"))
	mock.ExpectQuery(qIndexes).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	mock.ExpectQuery(qFKs).WillReturnRows(sqlmock.NewRows([]string{"a", "b", "c", "d", "e", "f", "g", "h"}))

	ref := dal.NewRootCollectionRef("v", "")
	got, err := d.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if code := got.Fields[1]; code.Length == nil || *code.Length != 40 {
		t.Errorf("length taken from the type spelling = %v, want 40", code.Length)
	}
	if len(got.Fields) != 2 || len(got.PrimaryKey) != 0 || len(got.Indexes) != 0 || len(got.ForeignKeys) != 0 {
		t.Errorf("view def = %+v", got)
	}
}

func TestDescribeCollection_MockedMissingTable(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qProbe).WithArgs("public", "Nope").WillReturnError(sql.ErrNoRows)

	ref := dal.NewRootCollectionRef("Nope", "")
	got, err := d.DescribeCollection(context.Background(), &ref)
	if got != nil || err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("got %v, %v; want nil and a not-found error naming Nope", got, err)
	}
	// A schema that only came from configuration is not repeated.
	if want := `dalgo2postgres: collection "Nope" not found`; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}

func TestDescribeCollection_FailureAtEveryStage(t *testing.T) {
	boom := errors.New("boom")
	badColumn := sqlmock.NewRows(columnHeader).AddRow(nil, "text", "text", nilInt(), nilInt(), nilInt(), "NO")
	type stage struct {
		name    string
		arrange func(mock sqlmock.Sqlmock)
		wantErr string
	}
	stages := []stage{
		{"probe", func(m sqlmock.Sqlmock) { m.ExpectQuery(qProbe).WillReturnError(boom) }, "probe"},
		{"primary key query", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			m.ExpectQuery(qPK).WillReturnError(boom)
		}, "listPrimaryKeyColumns"},
		{"primary key scan", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			m.ExpectQuery(qPK).WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow(nil))
		}, "listPrimaryKeyColumns scan"},
		{"primary key rows", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			m.ExpectQuery(qPK).WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id").RowError(0, boom))
		}, "boom"},
		{"columns query", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			expectPK(m, "public", "t", "id")
			m.ExpectQuery(qColumns).WillReturnError(boom)
		}, "columns"},
		{"column scan", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			expectPK(m, "public", "t", "id")
			m.ExpectQuery(qColumns).WillReturnRows(badColumn)
		}, "column scan"},
		{"column rows", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			expectPK(m, "public", "t", "id")
			m.ExpectQuery(qColumns).WillReturnRows(sqlmock.NewRows(columnHeader).
				AddRow("id", "text", "text", nilInt(), nilInt(), nilInt(), "NO").RowError(0, boom))
		}, "rows"},
		{"indexes", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			expectPK(m, "public", "t", "id")
			m.ExpectQuery(qColumns).WillReturnRows(sqlmock.NewRows(columnHeader))
			m.ExpectQuery(qIndexes).WillReturnError(boom)
		}, "listIndexesImpl"},
		{"foreign key scan", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			expectPK(m, "public", "t", "id")
			m.ExpectQuery(qColumns).WillReturnRows(sqlmock.NewRows(columnHeader))
			m.ExpectQuery(qIndexes).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
			m.ExpectQuery(qFKs).WillReturnRows(sqlmock.NewRows([]string{"oid", "conname", "from", "ns", "target", "to", "upd", "del"}).
				AddRow("not-a-number", "n", "f", "public", "t", "id", "NO ACTION", "NO ACTION"))
		}, "foreign key scan"},
		{"foreign key rows", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			expectPK(m, "public", "t", "id")
			m.ExpectQuery(qColumns).WillReturnRows(sqlmock.NewRows(columnHeader))
			m.ExpectQuery(qIndexes).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
			m.ExpectQuery(qFKs).WillReturnRows(sqlmock.NewRows([]string{"oid", "conname", "from", "ns", "target", "to", "upd", "del"}).
				AddRow(int64(1), "n", "f", "public", "t", "id", "NO ACTION", "NO ACTION").RowError(0, boom))
		}, "foreign key rows"},
		{"foreign keys", func(m sqlmock.Sqlmock) {
			expectProbeFound(m, "public", "t")
			expectPK(m, "public", "t", "id")
			m.ExpectQuery(qColumns).WillReturnRows(sqlmock.NewRows(columnHeader))
			m.ExpectQuery(qIndexes).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
			m.ExpectQuery(qFKs).WillReturnError(boom)
		}, "foreign keys"},
	}
	for _, s := range stages {
		t.Run(s.name, func(t *testing.T) {
			d, mock := newSchemaMockDatabase(t)
			s.arrange(mock)
			ref := dal.NewRootCollectionRef("t", "")
			got, err := d.DescribeCollection(context.Background(), &ref)
			if got != nil || err == nil || !strings.Contains(err.Error(), s.wantErr) {
				t.Fatalf("got %v, %v; want nil and an error containing %q", got, err, s.wantErr)
			}
		})
	}
}

func TestListIndexes_ExactNameInSchema(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(`pg_indexes`).WithArgs("sales", "Album").WillReturnRows(
		sqlmock.NewRows([]string{"indexname", "indexdef"}).
			AddRow("ix", `CREATE INDEX ix ON sales."Album" USING btree (a, b desc)`))
	ref := dal.NewRootCollectionRef("Album", "")
	got, err := d.ListIndexes(context.Background(), &ref)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	if len(got) != 1 || got[0].Collection != "Album" || !reflect.DeepEqual(got[0].Fields, []dal.FieldName{"a", "b"}) {
		t.Errorf("indexes = %+v", got)
	}
}

func TestListIndexes_QueryScopesPrimaryKeyExclusionToSchema(t *testing.T) {
	// A same-named primary key in another schema must not hide an index.
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(`n\.nspname = i\.schemaname`).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	ref := dal.NewRootCollectionRef("t", "")
	if _, err := d.ListIndexes(context.Background(), &ref); err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
}

func TestListIndexes_Errors(t *testing.T) {
	ref := dal.NewRootCollectionRef("t", "")
	t.Run("scan", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`pg_indexes`).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}).AddRow(nil, "x"))
		if _, err := d.ListIndexes(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "scan") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rows", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`pg_indexes`).WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}).AddRow("a", "b").RowError(0, errors.New("broke")))
		if _, err := d.ListIndexes(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "broke") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestParseIndexDefFields(t *testing.T) {
	tests := []struct {
		def  string
		want []dal.FieldName
	}{
		{`CREATE INDEX i ON t USING btree (a)`, []dal.FieldName{"a"}},
		{`CREATE INDEX i ON t USING btree ("A", "b" DESC, c asc)`, []dal.FieldName{"A", "b", "c"}},
		{`CREATE INDEX i ON t USING btree ()`, []dal.FieldName{}},
		{`no parentheses`, nil},
		{`) reversed (`, nil},
	}
	for _, tt := range tests {
		if got := parseIndexDefFields(tt.def); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseIndexDefFields(%q) = %#v, want %#v", tt.def, got, tt.want)
		}
	}
}

func TestIsUniqueIndexDef(t *testing.T) {
	for def, want := range map[string]bool{
		"CREATE UNIQUE INDEX i ON t (a)": true,
		"create unique index i on t (a)": true,
		"CREATE INDEX i ON t (a)":        false,
		"U":                              false,
		"UNIQU":                          false,
	} {
		if got := isUniqueIndexDef(def); got != want {
			t.Errorf("isUniqueIndexDef(%q) = %v, want %v", def, got, want)
		}
	}
}

func TestListIndexes_QueryNamesTheCatalogSchema(t *testing.T) {
	// Every catalog relation is qualified, so a same-named object earlier on the
	// search_path cannot stand in for the catalog.
	d, mock := newSchemaMockDatabase(t)
	for _, relation := range []string{"pg_indexes", "pg_constraint", "pg_class", "pg_namespace"} {
		mock.ExpectQuery(`(FROM|JOIN) pg_catalog\.` + relation + `\b`).
			WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
		ref := dal.NewRootCollectionRef("t", "")
		if _, err := d.ListIndexes(context.Background(), &ref); err != nil {
			t.Fatalf("ListIndexes (%s): %v", relation, err)
		}
	}
}

func TestQualifiedReference_SchemaOnTheReferenceIsHonoured(t *testing.T) {
	// The database is configured for "public"; the reference names "sales". The
	// reader must ask the server about "sales", never answer from "public".
	ref := dal.NewQualifiedRootCollectionRef("sales", "Album", "")

	t.Run("DescribeCollection", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		// A target in the reference's schema carries no namespace; one in the
		// configured schema ("public") is another schema as far as "sales" goes.
		describeAlbum(mock, "sales", "Album", "public")
		got, err := d.DescribeCollection(context.Background(), &ref)
		if err != nil {
			t.Fatalf("DescribeCollection: %v", err)
		}
		if ns := got.ForeignKeys[0].ReferencedNamespace; ns != "public" {
			t.Errorf("ReferencedNamespace = %q, want %q (relative to the reference's schema)", ns, "public")
		}
		if ns := got.ForeignKeys[1].ReferencedNamespace; ns != "other" {
			t.Errorf("ReferencedNamespace = %q, want other", ns)
		}
	})
	t.Run("DescribeCollection same schema as the target", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		describeAlbum(mock, "sales", "Album", "sales")
		got, err := d.DescribeCollection(context.Background(), &ref)
		if err != nil {
			t.Fatalf("DescribeCollection: %v", err)
		}
		if ns := got.ForeignKeys[0].ReferencedNamespace; ns != "" {
			t.Errorf("ReferencedNamespace = %q, want empty", ns)
		}
	})
	t.Run("DescribeCollection not found", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(qProbe).WithArgs("sales", "Album").WillReturnError(sql.ErrNoRows)
		if got, err := d.DescribeCollection(context.Background(), &ref); got != nil || err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("got %v, %v; want not found", got, err)
		} else if !strings.Contains(err.Error(), `collection "sales"."Album" not found`) {
			t.Errorf("err = %q, want it to name the schema that was searched", err)
		}
	})
	t.Run("errors name the schema of the reference", func(t *testing.T) {
		boom := errors.New("boom")
		want := `"sales"."Album"`
		d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(qProbe).WillReturnError(boom)
		_, err := d.DescribeCollection(context.Background(), &ref)
		if err == nil || !strings.Contains(err.Error(), "probe "+want) {
			t.Errorf("DescribeCollection probe: err = %v, want %s", err, want)
		}
		d, mock = newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(`pg_indexes`).WillReturnError(boom)
		if _, err = d.ListIndexes(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "listIndexesImpl "+want) {
			t.Errorf("ListIndexes: err = %v, want %s", err, want)
		}
		d, mock = newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(`table_constraints`).WillReturnError(boom)
		if _, err = d.ListConstraints(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "ListConstraints "+want) {
			t.Errorf("ListConstraints: err = %v, want %s", err, want)
		}
		d, mock = newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(`referrer_table`).WillReturnError(boom)
		if _, err = d.ListReferrers(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "ListReferrers "+want) {
			t.Errorf("ListReferrers: err = %v, want %s", err, want)
		}
	})
	t.Run("ListIndexes", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(`pg_indexes`).WithArgs("sales", "Album").
			WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
		if _, err := d.ListIndexes(context.Background(), &ref); err != nil {
			t.Fatalf("ListIndexes: %v", err)
		}
	})
	t.Run("ListConstraints", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(`table_constraints`).WithArgs("sales", "Album").
			WillReturnRows(sqlmock.NewRows([]string{"constraint_name", "constraint_type"}))
		if _, err := d.ListConstraints(context.Background(), &ref); err != nil {
			t.Fatalf("ListConstraints: %v", err)
		}
	})
	t.Run("ListReferrers", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
		mock.ExpectQuery(`referrer_table`).WithArgs("sales", "Album").
			WillReturnRows(sqlmock.NewRows([]string{"oid", "referrer_table", "referrer_col"}).AddRow(int64(1), "Track", "AlbumId"))
		got, err := d.ListReferrers(context.Background(), &ref)
		if err != nil {
			t.Fatalf("ListReferrers: %v", err)
		}
		// The referrer lives in the schema of the queried reference, and says so:
		// handed back to the reader it must address that table, not a same-named
		// one in the configured schema.
		if len(got) != 1 || got[0].Collection.Name() != "Track" || got[0].Collection.Schema() != "sales" {
			t.Errorf("referrers = %+v, want Track in schema sales", got)
		}
	})
}

func TestUnqualifiedReference_UsesTheConfiguredSchema(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(`pg_indexes`).WithArgs("sales", "Album").
		WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	ref := dal.NewRootCollectionRef("Album", "")
	if _, err := d.ListIndexes(context.Background(), &ref); err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
}
