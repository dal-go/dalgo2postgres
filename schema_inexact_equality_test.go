package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const qInexact = `collisdeterministic`

func TestNonDeterministicTextColumns_ReturnsColumnsInCatalogOrder(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qInexact).WithArgs("sales", "Customer").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("Email").AddRow("Name"))
	got, err := d.NonDeterministicTextColumns(context.Background(), "Customer")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Email", "Name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNonDeterministicTextColumns_PlainTableYieldsEmptyNonNilSlice(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(qInexact).WithArgs("public", "plain").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}))
	got, err := d.NonDeterministicTextColumns(context.Background(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty non-nil slice", got)
	}
}

func TestNonDeterministicTextColumns_FoldsTableNameByDefault(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(qInexact).WithArgs("public", "customer").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("email"))
	if _, err := d.NonDeterministicTextColumns(context.Background(), "Customer"); err != nil {
		t.Fatal(err)
	}
}

func TestNonDeterministicTextColumns_TableNameIsBoundNeverSQLText(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	hostile := `x'; DROP TABLE users; --`
	mock.ExpectQuery(qInexact).WithArgs("public", hostile).
		WillReturnRows(sqlmock.NewRows([]string{"attname"}))
	if _, err := d.NonDeterministicTextColumns(context.Background(), hostile); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(nonDeterministicTextColumnsSQL, "DROP TABLE") {
		t.Fatal("the statement text must not carry the table name")
	}
}

func TestNonDeterministicTextColumns_Errors(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]struct {
		arrange func(sqlmock.Sqlmock)
		wantMsg string
	}{
		"query": {
			func(m sqlmock.Sqlmock) { m.ExpectQuery(qInexact).WillReturnError(boom) },
			`non-deterministic text columns of "t"`,
		},
		"scan": {
			func(m sqlmock.Sqlmock) {
				m.ExpectQuery(qInexact).WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow(nil))
			},
			`scan`,
		},
		"rows": {
			func(m sqlmock.Sqlmock) {
				m.ExpectQuery(qInexact).WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("a").RowError(0, boom))
			},
			`rows`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, mock := newSchemaMockDatabase(t)
			tc.arrange(mock)
			got, err := d.NonDeterministicTextColumns(context.Background(), "t")
			if err == nil {
				t.Fatalf("got %v, want an error", got)
			}
			if got != nil {
				t.Fatalf("got %v, want nil on error", got)
			}
			if name != "scan" && !errors.Is(err, boom) {
				t.Fatalf("error %v does not wrap the cause", err)
			}
		})
	}
}
