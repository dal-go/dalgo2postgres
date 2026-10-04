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

func TestListReferrers_ReportsTheReferencingTable(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	// Artist is referenced by Album (two columns) and Tour.
	mock.ExpectQuery(`source\.relname AS referrer_table`).WithArgs("sales", "Artist").
		WillReturnRows(sqlmock.NewRows([]string{"referrer_table", "referrer_col"}).
			AddRow("Album", "ArtistId").
			AddRow("Album", "FeaturedArtistId").
			AddRow("Tour", "ArtistId"))

	ref := dal.NewRootCollectionRef("Artist", "")
	got, err := d.ListReferrers(context.Background(), &ref)
	if err != nil {
		t.Fatalf("ListReferrers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("referrers = %+v, want 2", got)
	}
	if got[0].Collection.Name() != "Album" || !reflect.DeepEqual(got[0].Fields, []dal.FieldName{"ArtistId", "FeaturedArtistId"}) {
		t.Errorf("first referrer = %s %v, want Album [ArtistId FeaturedArtistId]", got[0].Collection.Name(), got[0].Fields)
	}
	if got[1].Collection.Name() != "Tour" {
		t.Errorf("second referrer = %s, want Tour", got[1].Collection.Name())
	}
}

func TestListReferrers_QuerySelectsSourceNotTarget(t *testing.T) {
	// The old query selected the referenced table's name as the referrer.
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(`SELECT DISTINCT source\.relname AS referrer_table, source_column\.attname AS referrer_col`).
		WithArgs("public", "artist").
		WillReturnRows(sqlmock.NewRows([]string{"referrer_table", "referrer_col"}))
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
			WillReturnRows(sqlmock.NewRows([]string{"referrer_table", "referrer_col"}).AddRow(nil, "c"))
		if _, err := d.ListReferrers(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "scan") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rows", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(`referrer_table`).
			WillReturnRows(sqlmock.NewRows([]string{"referrer_table", "referrer_col"}).AddRow("a", "b").RowError(0, errors.New("broke")))
		if _, err := d.ListReferrers(context.Background(), &ref); err == nil || !strings.Contains(err.Error(), "broke") {
			t.Fatalf("err = %v", err)
		}
	})
}
