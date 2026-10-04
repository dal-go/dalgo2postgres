package dalgo2postgres

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
)

// qInexact pins the whole statement text: the table name must travel only as a
// bound argument, so any statement other than the constant fails to match.
var qInexact = "^" + regexp.QuoteMeta(strings.TrimSpace(nonDeterministicTextColumnsSQL)) + "$"

func inexactRef(name string) *dal.CollectionRef {
	r := dal.NewRootCollectionRef(name, "")
	return &r
}

func inexactQualifiedRef(schema, name string) *dal.CollectionRef {
	r := dal.NewQualifiedRootCollectionRef(schema, name, "")
	return &r
}

func TestNonDeterministicTextColumns_ReturnsColumnsInCatalogOrder(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("sales"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qInexact).WithArgs("sales", "Customer").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("Email").AddRow("Name"))
	got, err := d.NonDeterministicTextColumns(context.Background(), inexactRef("Customer"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Email", "Name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNonDeterministicTextColumns_QualifiedReferenceWinsOverConfiguredSchema(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qInexact).WithArgs("sales", "Customer").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("Email"))
	got, err := d.NonDeterministicTextColumns(context.Background(), inexactQualifiedRef("sales", "Customer"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Email"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNonDeterministicTextColumns_QualifiedReferenceWinsOverWithSchema(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithSchema("other"), WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qInexact).WithArgs("sales", "Customer").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow(nil))
	if _, err := d.NonDeterministicTextColumns(context.Background(), inexactQualifiedRef("sales", "Customer")); err != nil {
		t.Fatal(err)
	}
}

func TestNonDeterministicTextColumns_PlainTableYieldsEmptyNonNilSlice(t *testing.T) {
	// An existing table with no such column still produces one row, whose
	// column name is NULL: that row is how the table's existence is known.
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(qInexact).WithArgs("public", "plain").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow(nil))
	got, err := d.NonDeterministicTextColumns(context.Background(), inexactRef("plain"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty non-nil slice", got)
	}
}

func TestNonDeterministicTextColumns_MissingTableIsNotFound(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	mock.ExpectQuery(qInexact).WithArgs("public", "Nope").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}))
	got, err := d.NonDeterministicTextColumns(context.Background(), inexactRef("Nope"))
	if err == nil || got != nil {
		t.Fatalf("got %v, %v; want nil and a not-found error", got, err)
	}
	if want := newCollectionNotFoundError("Nope").Error(); err.Error() != want {
		t.Fatalf("error %q, want %q", err, want)
	}
}

func TestNonDeterministicTextColumns_FoldsTableNameByDefault(t *testing.T) {
	d, mock := newSchemaMockDatabase(t)
	mock.ExpectQuery(qInexact).WithArgs("public", "customer").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("email"))
	if _, err := d.NonDeterministicTextColumns(context.Background(), inexactRef("Customer")); err != nil {
		t.Fatal(err)
	}
}

func TestNonDeterministicTextColumns_TableNameIsBoundNeverSQLText(t *testing.T) {
	d, mock := newSchemaMockDatabase(t, WithIdentifierMode(IdentifierExact))
	hostile := `x'; DROP TABLE users; --`
	// qInexact is anchored to the constant statement, so a name appended to the
	// text would not match; the name is expected only as the bound argument.
	mock.ExpectQuery(qInexact).WithArgs("public", hostile).
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow(nil))
	if _, err := d.NonDeterministicTextColumns(context.Background(), inexactRef(hostile)); err != nil {
		t.Fatal(err)
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
			`non-deterministic text columns of "t": boom`,
		},
		"scan": {
			// Two result columns against one scan destination.
			func(m sqlmock.Sqlmock) {
				m.ExpectQuery(qInexact).WillReturnRows(sqlmock.NewRows([]string{"attname", "extra"}).AddRow("a", "b"))
			},
			`non-deterministic text columns of "t" scan: `,
		},
		"rows": {
			func(m sqlmock.Sqlmock) {
				m.ExpectQuery(qInexact).WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("a").RowError(0, boom))
			},
			`non-deterministic text columns of "t" rows: boom`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, mock := newSchemaMockDatabase(t)
			tc.arrange(mock)
			got, err := d.NonDeterministicTextColumns(context.Background(), inexactRef("t"))
			if err == nil {
				t.Fatalf("got %v, want an error", got)
			}
			if got != nil {
				t.Fatalf("got %v, want nil on error", got)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q lacks %q", err, tc.wantMsg)
			}
			if name != "scan" && !errors.Is(err, boom) {
				t.Fatalf("error %v does not wrap the cause", err)
			}
		})
	}
}
