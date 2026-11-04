package dalgo2postgres

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// qNativeAccess pins the whole statement: the names travel only as bound
// arguments.
var qNativeAccess = "^" + regexp.QuoteMeta(strings.TrimSpace(nativeAccessSQL)) + "$"

func catalogRow(t *testing.T, cat *pgCatalog) *sqlmock.Rows {
	t.Helper()
	doc, err := json.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	return sqlmock.NewRows([]string{"json_build_object"}).AddRow(string(doc))
}

func TestReadNativeAccess_ReadsOneStatementAndResolves(t *testing.T) {
	t.Parallel()
	d, mock := newSchemaMockDatabase(t, WithSchema("other"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qNativeAccess).WithArgs("billing", "invoices").WillReturnRows(catalogRow(t, appCatalog()))
	got, err := d.ReadNativeAccess(context.Background(), &invoicesRef)
	if err != nil {
		t.Fatal(err)
	}
	if got.Coverage.Level != dbschema.NativeCoveragePartial || len(got.Principals) != 12 || got.AsOf != nativeAsOf {
		t.Fatalf("got %+v", got)
	}
}

func TestReadNativeAccess_ImplementsTheCapability(t *testing.T) {
	t.Parallel()
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(qNativeAccess).WithArgs("public", "invoices").WillReturnRows(catalogRow(t, appCatalog()))
	ref := dal.NewRootCollectionRef("Invoices", "")
	reader, ok := dal.As[dbschema.NativeAccessReader](d)
	if !ok {
		t.Fatal("*Database does not implement dbschema.NativeAccessReader")
	}
	if _, err := reader.ReadNativeAccess(context.Background(), &ref); err != nil {
		t.Fatal(err)
	}
}

// Honesty rule: a table that is not there is an error, never an empty answer.
func TestReadNativeAccess_MissingTableIsNotFoundNeverEmpty(t *testing.T) {
	t.Parallel()
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	cat := appCatalog()
	cat.Table = nil
	mock.ExpectQuery(qNativeAccess).WithArgs("billing", "invoices").WillReturnRows(catalogRow(t, cat))
	got, err := d.ReadNativeAccess(context.Background(), &invoicesRef)
	if got != nil || err == nil || !strings.Contains(err.Error(), `"billing"."invoices" not found`) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// Honesty rule: an unreadable catalog is an error, never "nobody".
func TestReadNativeAccess_UnreadableCatalogIsAnError(t *testing.T) {
	t.Parallel()
	denied := errors.New("permission denied for table pg_auth_members")
	for _, tt := range []struct {
		name   string
		expect func(*sqlmock.ExpectedQuery)
		want   string
	}{
		{"query refused", func(q *sqlmock.ExpectedQuery) { q.WillReturnError(denied) }, "permission denied"},
		{"no row", func(q *sqlmock.ExpectedQuery) { q.WillReturnRows(sqlmock.NewRows([]string{"doc"})) }, "no rows"},
		{"not JSON", func(q *sqlmock.ExpectedQuery) { q.WillReturnRows(sqlmock.NewRows([]string{"doc"}).AddRow("{")) }, "decode"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
			tt.expect(mock.ExpectQuery(qNativeAccess).WithArgs("billing", "invoices"))
			got, err := d.ReadNativeAccess(context.Background(), &invoicesRef)
			if got != nil || err == nil || !strings.Contains(err.Error(), tt.want) ||
				!strings.Contains(err.Error(), `native access of "billing"."invoices"`) {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

func TestReadNativeAccess_ServerOlderThan13IsNotSupported(t *testing.T) {
	t.Parallel()
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	cat := appCatalog()
	cat.Version = 120020
	mock.ExpectQuery(qNativeAccess).WithArgs("billing", "invoices").WillReturnRows(catalogRow(t, cat))
	_, err := d.ReadNativeAccess(context.Background(), &invoicesRef)
	var ns *dbschema.NotSupportedError
	if !errors.As(err, &ns) || !errors.Is(err, dal.ErrNotSupported) || !strings.Contains(ns.Reason, "120020") {
		t.Fatalf("got %v", err)
	}
}

func TestReadNativeAccess_OverlongNameRefusedBeforeAnyStatement(t *testing.T) {
	t.Parallel()
	d, _ := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	ref := dal.NewRootCollectionRef(strings.Repeat("x", 64), "")
	if _, err := d.ReadNativeAccess(context.Background(), &ref); err == nil {
		t.Fatal("want ErrUnsafeName")
	}
}

func TestReadNativeAccess_InTheReadonlySchemaTransaction(t *testing.T) {
	t.Parallel()
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	expectSchemaMode(mock)
	mock.ExpectQuery(qNativeAccess).WithArgs("billing", "invoices").WillReturnRows(catalogRow(t, appCatalog()))
	mock.ExpectRollback()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := d.RunReadonlySchemaTransaction(ctx, func(ctx context.Context, reader dbschema.SchemaReader) error {
		native, ok := reader.(dbschema.NativeAccessReader)
		if !ok {
			t.Fatal("the read-only schema view does not offer NativeAccessReader")
		}
		got, err := native.ReadNativeAccess(ctx, &invoicesRef)
		if err == nil && len(got.Principals) == 0 {
			t.Fatal("no principals")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
