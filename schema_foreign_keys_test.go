package dalgo2postgres

import (
	"context"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

func TestDescribeCollection_ForeignKeys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	parent := uniqueTable(t, "fk_parent")
	child := uniqueTable(t, "fk_child")
	other := uniqueTable(t, "fk_other")
	defer dropTable(t, db, parent)
	defer dropTable(t, db, child)
	defer dropTable(t, db, other)
	for _, statement := range []string{
		`CREATE TABLE ` + quoteIdent(parent) + ` (a INTEGER, b INTEGER, PRIMARY KEY (a,b))`,
		`CREATE TABLE ` + quoteIdent(child) + ` (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER,
		 CONSTRAINT fk_shared FOREIGN KEY (a,b) REFERENCES ` + quoteIdent(parent) + `(a,b)
		 ON DELETE CASCADE ON UPDATE RESTRICT)`,
		`CREATE TABLE ` + quoteIdent(other) + ` (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER,
		 CONSTRAINT fk_shared FOREIGN KEY (a,b) REFERENCES ` + quoteIdent(parent) + `(a,b))`,
	} {
		if _, err := db.sqlDB.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	ref := dal.NewRootCollectionRef(child, "")
	def, err := db.DescribeCollection(ctx, &ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.ForeignKeys) != 1 {
		t.Fatalf("foreign keys: %+v", def.ForeignKeys)
	}
	fk := def.ForeignKeys[0]
	if fk.Name != "fk_shared" || fk.ReferencedCollection != parent || fk.ReferencedNamespace != "" ||
		len(fk.Fields) != 2 || fk.Fields[0] != "a" || fk.Fields[1] != "b" ||
		len(fk.ReferencedFields) != 2 || fk.ReferencedFields[0] != "a" || fk.ReferencedFields[1] != "b" ||
		fk.OnDelete != "CASCADE" || fk.OnUpdate != "RESTRICT" ||
		fk.Enforcement != dbschema.ForeignKeyEnforcementUnknown {
		t.Fatalf("foreign key metadata: %+v", fk)
	}
}
