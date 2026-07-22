package dalgo2postgres

import (
	"context"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
)

// buildOpts constructs DbOptions with the given table's primary key configured.
func buildOpts(table, pkCol string) dalgo2sql.DbOptions {
	return dalgo2sql.DbOptions{
		Recordsets: map[string]*dalgo2sql.Recordset{
			table: dalgo2sql.NewRecordset(table, dalgo2sql.Table,
				[]dal.FieldRef{dal.Field(pkCol)}),
		},
	}
}

func TestRecord_MapRoundTrip(t *testing.T) {
	ctx := context.Background()
	tbl := uniqueTable(t, "rr")

	// We need a base DB first to create the table, then one with recordset opts.
	base := openTestDB(t)
	defer dropTable(t, base, tbl)

	// Create the table.
	if _, err := base.sqlDB.ExecContext(ctx, `CREATE TABLE `+quoteIdent(tbl)+` (
		"id"    TEXT NOT NULL PRIMARY KEY,
		"name"  TEXT,
		"score" BIGINT
	)`); err != nil {
		t.Fatal(err)
	}

	db := openTestDBWithOpts(t, buildOpts(tbl, "id"))

	// Insert via dalrecord.Record with map[string]any data.
	key1 := dalrecord.NewKeyWithID(tbl, "r1")
	rec1 := dalrecord.NewRecordWithData(key1, map[string]any{
		"name":  "Alice",
		"score": int64(42),
	})
	if err := db.Insert(ctx, rec1); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Get back.
	getKey := dalrecord.NewKeyWithID(tbl, "r1")
	getRec := dalrecord.NewRecordWithData(getKey, make(map[string]any))
	if err := db.Get(ctx, getRec); err != nil {
		t.Fatalf("Get: %v", err)
	}
	data := getRec.Data().(map[string]any)
	if data["name"] != "Alice" {
		t.Errorf("name = %v, want Alice", data["name"])
	}

	// dalrecord.IsNotFound on missing.
	missingKey := dalrecord.NewKeyWithID(tbl, "missing_xyz")
	missingRec := dalrecord.NewRecordWithData(missingKey, make(map[string]any))
	if err := db.Get(ctx, missingRec); !dalrecord.IsNotFound(err) {
		t.Errorf("expected IsNotFound for missing key, got %v", err)
	}

	// Set-upsert: update name.
	upsertRec := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "r1"), map[string]any{
		"name":  "Alice Updated",
		"score": int64(99),
	})
	if err := db.Set(ctx, upsertRec); err != nil {
		t.Fatalf("Set (upsert): %v", err)
	}

	getKey2 := dalrecord.NewKeyWithID(tbl, "r1")
	getRec2 := dalrecord.NewRecordWithData(getKey2, make(map[string]any))
	if err := db.Get(ctx, getRec2); err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	data2 := getRec2.Data().(map[string]any)
	if data2["name"] != "Alice Updated" {
		t.Errorf("after Set: name = %v, want Alice Updated", data2["name"])
	}

	// Delete.
	if err := db.Delete(ctx, dalrecord.NewKeyWithID(tbl, "r1")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	delRec := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "r1"), make(map[string]any))
	if err := db.Get(ctx, delRec); !dalrecord.IsNotFound(err) {
		t.Errorf("expected IsNotFound after Delete, got %v", err)
	}
}

func TestRecord_WhereFieldQuery(t *testing.T) {
	ctx := context.Background()
	tbl := uniqueTable(t, "wfq")

	base := openTestDB(t)
	defer dropTable(t, base, tbl)

	if _, err := base.sqlDB.ExecContext(ctx, `CREATE TABLE `+quoteIdent(tbl)+` (
		"id"   TEXT NOT NULL PRIMARY KEY,
		"name" TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, name string }{
		{"a", "Alice"}, {"b", "Bob"}, {"c", "Alice"},
	} {
		if _, err := base.sqlDB.ExecContext(ctx,
			`INSERT INTO `+quoteIdent(tbl)+` ("id","name") VALUES ($1,$2)`,
			row.id, row.name); err != nil {
			t.Fatal(err)
		}
	}

	db := openTestDBWithOpts(t, buildOpts(tbl, "id"))

	collRef := dal.NewRootCollectionRef(tbl, "")
	q := dal.From(&collRef).NewQuery().WhereField("name", dal.Equal, "Alice").SelectIntoRecord(func() dalrecord.Record {
		return dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, ""), make(map[string]any))
	})

	reader, err := db.ExecuteQueryToRecordsReader(ctx, q)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var count int
	for {
		_, err := reader.Next()
		if err != nil {
			if err == dal.ErrNoMoreRecords {
				break
			}
			t.Fatalf("Next: %v", err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("WhereField Alice: got %d records, want 2", count)
	}
}

func TestRunReadwriteTransaction(t *testing.T) {
	ctx := context.Background()
	tbl := uniqueTable(t, "rwtx")

	base := openTestDB(t)
	defer dropTable(t, base, tbl)

	if _, err := base.sqlDB.ExecContext(ctx, `CREATE TABLE `+quoteIdent(tbl)+` (
		"id"   TEXT NOT NULL PRIMARY KEY,
		"name" TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	// Pre-insert a record to delete in the transaction.
	if _, err := base.sqlDB.ExecContext(ctx,
		`INSERT INTO `+quoteIdent(tbl)+` ("id","name") VALUES ('del_me','ToDelete')`); err != nil {
		t.Fatal(err)
	}

	db := openTestDBWithOpts(t, buildOpts(tbl, "id"))

	err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		// Set a new record.
		setRec := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "tx1"), map[string]any{
			"name": "TransactionUser",
		})
		if err := tx.Set(ctx, setRec); err != nil {
			return err
		}
		// Insert another.
		insRec := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "tx2"), map[string]any{
			"name": "Inserted",
		})
		if err := tx.Insert(ctx, insRec); err != nil {
			return err
		}
		// Delete the pre-inserted record.
		if err := tx.Delete(ctx, dalrecord.NewKeyWithID(tbl, "del_me")); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunReadwriteTransaction: %v", err)
	}

	// Verify post-transaction state.
	for _, id := range []string{"tx1", "tx2"} {
		rec := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, id), make(map[string]any))
		if err := db.Get(ctx, rec); err != nil {
			t.Errorf("Get %q after tx: %v", id, err)
		}
	}
	delRec := dalrecord.NewRecordWithData(dalrecord.NewKeyWithID(tbl, "del_me"), make(map[string]any))
	if err := db.Get(ctx, delRec); !dalrecord.IsNotFound(err) {
		t.Errorf("expected 'del_me' to be deleted, got %v", err)
	}
}
