package dalgo2postgres

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// The schema reader compares whole names in any database encoding: each of its
// statements sends the schema and the table name as parameters of type text, so
// the server compares the whole text with the stored name. The Go check of 63
// bytes stays as the early refusal, with no statement (see
// TestSchemaReader_RefusesANameOver63BytesBeforeAnyStatement). The encoding side
// is in TestSchemaReaderIntegration_NamesAreComparedWholeInAnyDatabaseEncoding.

// boundParameter matches a numbered placeholder and the cast written right after it.
var boundParameter = regexp.MustCompile(`\$\d+(::[a-z]+)?`)

// textCastMatcher is the matcher of a handle on which every parameter of every
// statement must be written $N::text. The expected text is a regular expression
// that identifies the statement, as sqlmock's own matcher takes it.
func textCastMatcher(expected, actual string) error {
	if err := sqlmock.QueryMatcherRegexp.Match(expected, actual); err != nil {
		return err
	}
	for _, parameter := range boundParameter.FindAllString(actual, -1) {
		if !strings.HasSuffix(parameter, "::text") {
			return fmt.Errorf("parameter %s is not cast to text in the statement:\n%s", parameter, actual)
		}
	}
	return nil
}

func TestSchemaReader_EveryNameParameterIsCastToText(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(textCastMatcher)))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = sqlDB.Close()
	})
	resolved, err := resolveSettings(dalgo2sql.DbOptions{}, []Option{WithIdentifierMode(IdentifierExact)})
	if err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	d := newDatabaseFromSQL(sqlDB, dal.NewSchema(nil, nil), resolved, []Option{WithIdentifierMode(IdentifierExact)})
	ctx := context.Background()
	ref := dal.NewRootCollectionRef("t", "")

	// The collections of a schema (the statement of every list entry).
	mock.ExpectQuery(listRelationsQuery).WithArgs("public").WillReturnRows(relationRows())
	if _, err := d.ListCollections(ctx, nil); err != nil {
		t.Errorf("ListCollections: %v", err)
	}

	// DescribeCollection of a table: the probe, the primary key, the columns, the
	// indexes and the foreign keys.
	expectProbeFound(mock, "public", "t")
	expectPK(mock, "public", "t")
	mock.ExpectQuery(qColumns).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows(columnHeader))
	mock.ExpectQuery(qIndexes).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	mock.ExpectQuery(qFKs).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"a", "b", "c", "d", "e", "f", "g", "h"}))
	if _, err := d.DescribeCollection(ctx, &ref); err != nil {
		t.Errorf("DescribeCollection of a table: %v", err)
	}

	// DescribeCollection of a materialized view reads its columns by another statement.
	mock.ExpectQuery(qProbe).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow("m"))
	expectPK(mock, "public", "t")
	mock.ExpectQuery(`a\.attnum > 0`).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows(columnHeader))
	mock.ExpectQuery(qIndexes).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	mock.ExpectQuery(qFKs).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"a", "b", "c", "d", "e", "f", "g", "h"}))
	if _, err := d.DescribeCollection(ctx, &ref); err != nil {
		t.Errorf("DescribeCollection of a materialized view: %v", err)
	}

	// ListIndexes.
	mock.ExpectQuery(qIndexes).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"indexname", "indexdef"}))
	if _, err := d.ListIndexes(ctx, &ref); err != nil {
		t.Errorf("ListIndexes: %v", err)
	}

	// ListConstraints.
	mock.ExpectQuery(`pg_catalog\.pg_constraint`).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"name", "type"}))
	if _, err := d.ListConstraints(ctx, &ref); err != nil {
		t.Errorf("ListConstraints: %v", err)
	}

	// ListReferrers.
	mock.ExpectQuery(`referrer_table`).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"oid", "t", "c"}))
	if _, err := d.ListReferrers(ctx, &ref); err != nil {
		t.Errorf("ListReferrers: %v", err)
	}

	// NonDeterministicTextColumns.
	mock.ExpectQuery(`collisdeterministic`).WithArgs("public", "t").WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow(nil))
	if _, err := d.NonDeterministicTextColumns(ctx, &ref); err != nil {
		t.Errorf("NonDeterministicTextColumns: %v", err)
	}
}
