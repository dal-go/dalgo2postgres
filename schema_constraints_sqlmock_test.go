package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

func TestListConstraints_ExactNameInSchema(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(`constraint_type IN \('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY'\)`).
		WithArgs("sales", "Album").
		WillReturnRows(sqlmock.NewRows([]string{"constraint_name", "constraint_type"}).
			AddRow("Album_fk", "FOREIGN KEY").
			AddRow("Album_pk", "PRIMARY KEY").
			AddRow("Album_uq", "UNIQUE").
			AddRow("Album_other", "CHECK"))

	ref := dal.NewRootCollectionRef("Album", "")
	got, err := d.ListConstraints(context.Background(), &ref)
	if err != nil {
		t.Fatalf("ListConstraints: %v", err)
	}
	want := []dbschema.ConstraintDef{
		{Name: "Album_fk", Type: "foreign-key"},
		{Name: "Album_pk", Type: "primary-key"},
		{Name: "Album_uq", Type: "unique"},
		{Name: "Album_other", Type: "CHECK"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("constraints = %+v, want %+v", got, want)
	}
}

func TestListConstraints_FoldLowerDefault(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(`table_constraints`).WithArgs("public", "album").
		WillReturnRows(sqlmock.NewRows([]string{"constraint_name", "constraint_type"}))
	ref := dal.NewRootCollectionRef("Album", "")
	if _, err := d.ListConstraints(context.Background(), &ref); err != nil {
		t.Fatalf("ListConstraints: %v", err)
	}
}

func TestListConstraints_Errors(t *testing.T) {
	ref := dal.NewRootCollectionRef("t", "")
	t.Run("query", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`table_constraints`).WillReturnError(errors.New("boom"))
		if _, err := d.ListConstraints(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("scan", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`table_constraints`).
			WillReturnRows(sqlmock.NewRows([]string{"constraint_name", "constraint_type"}).AddRow(nil, "UNIQUE"))
		if _, err := d.ListConstraints(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "scan") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestListReferrers_OneReferrerPerConstraintInKeyOrder(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	// Artist is referenced by two separate foreign keys of Album and by a
	// composite one of Tour whose columns are not in alphabetical order.
	mock.ExpectQuery(`source\.relname AS referrer_table`).WithArgs("sales", "Artist").
		WillReturnRows(sqlmock.NewRows([]string{"oid", "referrer_table", "referrer_col"}).
			AddRow(int64(5), "Album", "ArtistId").
			AddRow(int64(6), "Album", "FeaturedArtistId").
			AddRow(int64(7), "Tour", "Zone").
			AddRow(int64(7), "Tour", "Area"))

	ref := dal.NewRootCollectionRef("Artist", "")
	got, err := d.ListReferrers(context.Background(), &ref)
	if err != nil {
		t.Fatalf("ListReferrers: %v", err)
	}
	type shape struct {
		name   string
		fields []dal.FieldName
	}
	var gotShapes []shape
	for _, r := range got {
		if r.Collection.Schema() != "" {
			t.Errorf("referrer %s names schema %q, but the queried reference named none", r.Collection.Name(), r.Collection.Schema())
		}
		gotShapes = append(gotShapes, shape{r.Collection.Name(), r.Fields})
	}
	want := []shape{
		{"Album", []dal.FieldName{"ArtistId"}},
		{"Album", []dal.FieldName{"FeaturedArtistId"}},
		{"Tour", []dal.FieldName{"Zone", "Area"}},
	}
	if !reflect.DeepEqual(gotShapes, want) {
		t.Errorf("referrers = %+v, want %+v (one per foreign key, columns in key order)", gotShapes, want)
	}
}

func TestListReferrers_QuerySelectsSourceNotTarget(t *testing.T) {
	// The old query selected the referenced table's name as the referrer, and the
	// current one must order the key columns by their position in the key.
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(`(?s)SELECT c\.oid, source\.relname AS referrer_table, source_column\.attname AS referrer_col.*WITH ORDINALITY.*ORDER BY source\.relname, c\.conname, c\.oid, source_key\.position`).
		WithArgs("public", "artist").
		WillReturnRows(sqlmock.NewRows([]string{"oid", "referrer_table", "referrer_col"}))
	ref := dal.NewRootCollectionRef("Artist", "")
	got, err := d.ListReferrers(context.Background(), &ref)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no referrers and nil", got, err)
	}
}

func TestListReferrers_Errors(t *testing.T) {
	ref := dal.NewRootCollectionRef("t", "")
	t.Run("query", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`referrer_table`).WillReturnError(errors.New("boom"))
		if _, err := d.ListReferrers(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("scan", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`referrer_table`).
			WillReturnRows(sqlmock.NewRows([]string{"oid", "referrer_table", "referrer_col"}).AddRow(int64(1), nil, "c"))
		if _, err := d.ListReferrers(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "scan") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rows", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`referrer_table`).
			WillReturnRows(sqlmock.NewRows([]string{"oid", "referrer_table", "referrer_col"}).AddRow(int64(1), "a", "b").RowError(0, errors.New("broke")))
		if _, err := d.ListReferrers(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "broke") {
			t.Fatalf("err = %v", err)
		}
	})
}
