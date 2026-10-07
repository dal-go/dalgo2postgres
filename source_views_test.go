package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dbschema"
)

const listSourceViewsQuery = `pg_catalog\.pg_get_viewdef\(c\.oid, true\)`

func TestListSourceViewsReturnsOrderedViewMetadata(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(listSourceViewsQuery).WithArgs("sales").WillReturnRows(
		sqlmock.NewRows([]string{"relname", "attname", "definition"}).
			AddRow("active_orders", "order_id", " SELECT order_id FROM orders;\n").
			AddRow("active_orders", "status", " SELECT order_id FROM orders;\n").
			AddRow("totals", "total", " SELECT sum(amount) AS total FROM orders;\n"),
	)

	got, err := d.ListSourceViews(context.Background())
	if err != nil {
		t.Fatalf("ListSourceViews: %v", err)
	}
	want := []dbschema.SourceViewDef{
		{Name: "active_orders", Columns: []string{"order_id", "status"}, CreateSQL: " SELECT order_id FROM orders;\n"},
		{Name: "totals", Columns: []string{"total"}, CreateSQL: " SELECT sum(amount) AS total FROM orders;\n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListSourceViews = %#v, want %#v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SQL expectations: %v", err)
	}
}

func TestListSourceViewsReturnsEmptyInventory(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(listSourceViewsQuery).WithArgs("public").WillReturnRows(
		sqlmock.NewRows([]string{"relname", "attname", "definition"}),
	)
	got, err := d.ListSourceViews(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("ListSourceViews = %#v, %v; want empty, nil", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SQL expectations: %v", err)
	}
}

func TestListSourceViewsReportsQueryScanAndRowErrors(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listSourceViewsQuery).WithArgs("public").WillReturnError(errors.New("query failed"))
		if _, err := d.ListSourceViews(context.Background()); err == nil || !strings.Contains(err.Error(), "query failed") {
			t.Fatalf("ListSourceViews error = %v, want query failure", err)
		}
	})
	t.Run("scan", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listSourceViewsQuery).WithArgs("public").WillReturnRows(
			sqlmock.NewRows([]string{"relname", "attname", "definition"}).AddRow("v", nil, "SELECT 1"),
		)
		if _, err := d.ListSourceViews(context.Background()); err == nil || !strings.Contains(err.Error(), "scan") {
			t.Fatalf("ListSourceViews error = %v, want scan failure", err)
		}
	})
	t.Run("rows", func(t *testing.T) {
		d, mock := newSchemaMockDatabase(t)
		mock.ExpectQuery(listSourceViewsQuery).WithArgs("public").WillReturnRows(
			sqlmock.NewRows([]string{"relname", "attname", "definition"}).AddRow("v", "id", "SELECT 1").RowError(0, errors.New("row failed")),
		)
		if _, err := d.ListSourceViews(context.Background()); err == nil || !strings.Contains(err.Error(), "row failed") {
			t.Fatalf("ListSourceViews error = %v, want row failure", err)
		}
	})
}
