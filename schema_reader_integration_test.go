package dalgo2postgres

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// The tests in this file run against a real PostgreSQL server and skip unless
// DALGO2POSTGRES_TEST_DSN is set (see testDSN); CI sets it. They create their
// own schemas and drop them afterwards.

// exactIdent quotes an identifier without changing its case (quoteIdent folds to
// lower case, which is exactly what these tests must not do).
func exactIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// openSchemaTestDB opens a Database for the named schema in the given mode and
// creates the schema, dropping it (with everything in it) on cleanup.
func openSchemaTestDB(t *testing.T, schemaName string, mode IdentifierMode) *Database {
	t.Helper()
	db, err := NewDatabase(testDSN(t), WithSchema(schemaName), WithIdentifierMode(mode))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	ctx := context.Background()
	if _, err := db.sqlDB.ExecContext(ctx, `CREATE SCHEMA `+exactIdent(schemaName)); err != nil {
		_ = db.Close()
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.sqlDB.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+exactIdent(schemaName)+` CASCADE`)
		_ = db.Close()
	})
	return db
}

func execAll(t *testing.T, db *Database, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.sqlDB.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%v\nstatement: %s", err, statement)
		}
	}
}

func collectionNames(refs []dal.CollectionRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name())
	}
	sort.Strings(names)
	return names
}

func TestSchemaReaderIntegration_MixedCaseUnknownTypesViewsAndReferrers(t *testing.T) {
	schemaName := uniqueTable(t, "pg01")
	db := openSchemaTestDB(t, schemaName, IdentifierExact)
	ctx := context.Background()
	s := exactIdent(schemaName)

	execAll(t, db,
		`CREATE TYPE `+s+`."Mood" AS ENUM ('happy', 'sad')`,
		`CREATE TABLE `+s+`."TimeKinds" (
			"date_value" date,
			"time_value" time without time zone,
			"time_tz_value" time with time zone,
			"timestamp_value" timestamp without time zone,
			"timestamptz_value" timestamp with time zone
		)`,
		`CREATE TABLE `+s+`."Artist" ("ArtistId" integer PRIMARY KEY, "Name" text)`,
		`CREATE TABLE `+s+`."Album" (
			"AlbumId" integer PRIMARY KEY,
			"ArtistId" integer NOT NULL CONSTRAINT "fk_artist" REFERENCES `+s+`."Artist" ("ArtistId"),
			"Title" varchar(160) NOT NULL,
			"Rating" numeric(10,2),
			"ExternalId" uuid,
			"Meta" jsonb,
			"Tags" text[],
			"Mood" `+s+`."Mood",
			"Length" interval,
			"Addr" inet,
			"Price" money
		)`,
		`CREATE UNIQUE INDEX "Album_Title_uq" ON `+s+`."Album" ("Title")`,
		`CREATE VIEW `+s+`."AlbumTitles" AS SELECT "AlbumId", "Title" FROM `+s+`."Album"`,
	)

	// Tables and views are listed under the names PostgreSQL reports.
	refs, err := db.ListCollections(ctx, nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if got, want := collectionNames(refs), []string{"Album", "AlbumTitles", "Artist", "TimeKinds"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListCollections = %v, want %v", got, want)
	}

	// The table with the unknown types is described with every column.
	album := dal.NewRootCollectionRef("Album", "")
	def, err := db.DescribeCollection(ctx, &album)
	if err != nil {
		t.Fatalf("DescribeCollection(Album): %v", err)
	}
	wantTypes := map[string]dbschema.Type{
		"AlbumId": dbschema.Int, "ArtistId": dbschema.Int, "Title": dbschema.String, "Rating": dbschema.Decimal,
		"ExternalId": dbschema.String, "Meta": dbschema.String, "Tags": dbschema.String, "Mood": dbschema.String,
		"Length": dbschema.String, "Addr": dbschema.String, "Price": dbschema.String,
	}
	if def.Name != "Album" || len(def.Fields) != len(wantTypes) {
		t.Fatalf("Album def: name=%q fields=%d, want Album with %d fields: %+v", def.Name, len(def.Fields), len(wantTypes), def.Fields)
	}
	for _, f := range def.Fields {
		if want, ok := wantTypes[string(f.Name)]; !ok || f.Type != want {
			t.Errorf("field %q type = %v, want %v (known=%v)", f.Name, f.Type, want, ok)
		}
	}
	if !reflect.DeepEqual(def.PrimaryKey, []dal.FieldName{"AlbumId"}) {
		t.Errorf("PrimaryKey = %v", def.PrimaryKey)
	}
	if def.SourceDefinition == nil || def.SourceDefinition.Dialect != "postgres" {
		t.Fatalf("Album SourceDefinition = %+v, want PostgreSQL metadata", def.SourceDefinition)
	}
	sourceColumns := make(map[string]dbschema.SourceColumnDef, len(def.SourceDefinition.Columns))
	for _, column := range def.SourceDefinition.Columns {
		sourceColumns[column.Name] = column
	}
	if got := sourceColumns["Rating"].DeclaredType; got != "numeric" {
		t.Errorf("Rating declared type = %q, want numeric", got)
	}
	if got := sourceColumns["ArtistId"].PrimaryKeyPosition; got != 0 {
		t.Errorf("ArtistId primary-key position = %d, want 0", got)
	}

	timeKinds := dal.NewRootCollectionRef("TimeKinds", "")
	timeDef, err := db.DescribeCollection(ctx, &timeKinds)
	if err != nil {
		t.Fatalf("DescribeCollection(TimeKinds): %v", err)
	}
	wantDeclaredTypes := map[string]string{
		"date_value": "date", "time_value": "time without time zone",
		"time_tz_value": "time with time zone", "timestamp_value": "timestamp without time zone",
		"timestamptz_value": "timestamp with time zone",
	}
	if timeDef.SourceDefinition == nil {
		t.Fatal("TimeKinds SourceDefinition is nil")
	}
	if len(timeDef.SourceDefinition.Columns) != len(wantDeclaredTypes) {
		t.Fatalf("TimeKinds source columns = %+v, want %d columns", timeDef.SourceDefinition.Columns, len(wantDeclaredTypes))
	}
	seenTemporalTypes := make(map[string]bool, len(wantDeclaredTypes))
	for _, column := range timeDef.SourceDefinition.Columns {
		if want, ok := wantDeclaredTypes[column.Name]; !ok || column.DeclaredType != want {
			t.Errorf("column %q declared type = %q, want %q (known=%v)", column.Name, column.DeclaredType, want, ok)
		} else {
			seenTemporalTypes[column.Name] = true
		}
	}
	for name := range wantDeclaredTypes {
		if !seenTemporalTypes[name] {
			t.Errorf("TimeKinds is missing native type metadata for %q", name)
		}
	}
	if len(def.ForeignKeys) != 1 || def.ForeignKeys[0].Name != "fk_artist" || def.ForeignKeys[0].ReferencedCollection != "Artist" ||
		def.ForeignKeys[0].ReferencedNamespace != "" {
		t.Errorf("ForeignKeys = %+v", def.ForeignKeys)
	}
	if len(def.Indexes) != 1 || def.Indexes[0].Name != "Album_Title_uq" || !def.Indexes[0].Unique {
		t.Errorf("Indexes = %+v, want the one unique index (primary key excluded)", def.Indexes)
	}

	// A view is described too.
	view := dal.NewRootCollectionRef("AlbumTitles", "")
	viewDef, err := db.DescribeCollection(ctx, &view)
	if err != nil {
		t.Fatalf("DescribeCollection(view): %v", err)
	}
	if len(viewDef.Fields) != 2 {
		t.Errorf("view fields = %+v, want 2", viewDef.Fields)
	}

	// Referrers: Album references Artist, and Artist is not reported as its own referrer.
	artist := dal.NewRootCollectionRef("Artist", "")
	referrers, err := db.ListReferrers(ctx, &artist)
	if err != nil {
		t.Fatalf("ListReferrers: %v", err)
	}
	if len(referrers) != 1 || referrers[0].Collection.Name() != "Album" ||
		!reflect.DeepEqual(referrers[0].Fields, []dal.FieldName{"ArtistId"}) {
		t.Errorf("ListReferrers(Artist) = %+v, want Album[ArtistId]", referrers)
	}
	if none, err := db.ListReferrers(ctx, &album); err != nil || len(none) != 0 {
		t.Errorf("ListReferrers(Album) = %+v, %v; want none", none, err)
	}

	// Constraints use the exact name.
	constraints, err := db.ListConstraints(ctx, &album)
	if err != nil {
		t.Fatalf("ListConstraints: %v", err)
	}
	types := map[string]int{}
	for _, c := range constraints {
		types[c.Type]++
	}
	if types["primary-key"] != 1 || types["foreign-key"] != 1 {
		t.Errorf("constraints = %+v, want one primary-key and one foreign-key", constraints)
	}

	// Exact means exact: the wrong case does not match.
	wrongCase := dal.NewRootCollectionRef("album", "")
	if got, err := db.DescribeCollection(ctx, &wrongCase); err == nil || got != nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("DescribeCollection(album) = %v, %v; want a not-found error", got, err)
	}
}

func TestSchemaReaderIntegration_FoldLowerFindsLowerCaseTables(t *testing.T) {
	schemaName := uniqueTable(t, "pg01fold")
	db := openSchemaTestDB(t, schemaName, IdentifierFoldLower)
	s := exactIdent(schemaName)
	execAll(t, db,
		`CREATE TABLE `+s+`."album" ("id" integer PRIMARY KEY, "meta" jsonb)`,
	)
	ref := dal.NewRootCollectionRef("Album", "")
	def, err := db.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection(Album) in fold-lower mode: %v", err)
	}
	if def.Name != "album" || len(def.Fields) != 2 {
		t.Errorf("def = %+v, want album with 2 fields", def)
	}
}

func TestSchemaReaderIntegration_SecondSchemaIsKeptApart(t *testing.T) {
	first := uniqueTable(t, "pg01a")
	second := uniqueTable(t, "pg01b")
	db1 := openSchemaTestDB(t, first, IdentifierExact)
	db2 := openSchemaTestDB(t, second, IdentifierExact)
	s1, s2 := exactIdent(first), exactIdent(second)
	ctx := context.Background()

	// Both schemas hold Artist and Album, and both foreign keys carry the same constraint name.
	execAll(t, db1,
		`CREATE TABLE `+s1+`."Artist" ("ArtistId" integer PRIMARY KEY)`,
		`CREATE TABLE `+s1+`."Album" ("AlbumId" integer PRIMARY KEY, "ArtistId" integer CONSTRAINT "fk_artist" REFERENCES `+s1+`."Artist" ("ArtistId"))`,
	)
	execAll(t, db2,
		`CREATE TABLE `+s2+`."Artist" ("ArtistId" integer PRIMARY KEY)`,
		`CREATE TABLE `+s2+`."Tour" ("TourId" integer PRIMARY KEY, "LeadArtist" integer CONSTRAINT "fk_artist" REFERENCES `+s2+`."Artist" ("ArtistId"))`,
		`CREATE TABLE `+s2+`."OnlyInSecond" ("Id" integer PRIMARY KEY)`,
	)

	refs1, err := db1.ListCollections(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs2, err := db2.ListCollections(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := collectionNames(refs1), []string{"Album", "Artist"}; !reflect.DeepEqual(got, want) {
		t.Errorf("first schema lists %v, want %v", got, want)
	}
	if got, want := collectionNames(refs2), []string{"Artist", "OnlyInSecond", "Tour"}; !reflect.DeepEqual(got, want) {
		t.Errorf("second schema lists %v, want %v", got, want)
	}

	artist := dal.NewRootCollectionRef("Artist", "")
	r1, err := db1.ListReferrers(ctx, &artist)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := db2.ListReferrers(ctx, &artist)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) != 1 || r1[0].Collection.Name() != "Album" {
		t.Errorf("first schema referrers = %+v, want Album", r1)
	}
	if len(r2) != 1 || r2[0].Collection.Name() != "Tour" || !reflect.DeepEqual(r2[0].Fields, []dal.FieldName{"LeadArtist"}) {
		t.Errorf("second schema referrers = %+v, want Tour[LeadArtist]", r2)
	}

	// A table that exists only in the second schema is not found through the first.
	only := dal.NewRootCollectionRef("OnlyInSecond", "")
	if _, err := db1.DescribeCollection(ctx, &only); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("DescribeCollection through the first schema: err = %v, want not found", err)
	}
	if def, err := db2.DescribeCollection(ctx, &only); err != nil || len(def.Fields) != 1 {
		t.Errorf("DescribeCollection through the second schema: %+v, %v", def, err)
	}

	// A reference that names its schema is read from that schema, whichever
	// schema the database is configured for.
	qualifiedOnly := dal.NewQualifiedRootCollectionRef(second, "OnlyInSecond", "")
	if def, err := db1.DescribeCollection(ctx, &qualifiedOnly); err != nil || def == nil || len(def.Fields) != 1 {
		t.Errorf("qualified DescribeCollection through the first schema: %+v, %v", def, err)
	}
	qualifiedAlbum := dal.NewQualifiedRootCollectionRef(second, "Album", "")
	if def, err := db1.DescribeCollection(ctx, &qualifiedAlbum); err == nil || def != nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Album exists only in the first schema, so asking for it in the second must not find it: %+v, %v", def, err)
	}
	qualifiedArtist := dal.NewQualifiedRootCollectionRef(second, "Artist", "")
	viaFirst, err := db1.ListReferrers(ctx, &qualifiedArtist)
	if err != nil {
		t.Fatal(err)
	}
	if len(viaFirst) != 1 || viaFirst[0].Collection.Name() != "Tour" || viaFirst[0].Collection.Schema() != second {
		t.Errorf("qualified ListReferrers through the first schema = %+v, want Tour in schema %q", viaFirst, second)
	}
	qualifiedTour := dal.NewQualifiedRootCollectionRef(second, "Tour", "")
	if constraints, err := db1.ListConstraints(ctx, &qualifiedTour); err != nil || len(constraints) != 2 {
		t.Errorf("qualified ListConstraints through the first schema = %+v, %v; want Tour's primary key and foreign key", constraints, err)
	}
	if indexes, err := db1.ListIndexes(ctx, &qualifiedTour); err != nil || len(indexes) != 0 {
		t.Errorf("qualified ListIndexes through the first schema = %+v, %v; want none (primary key excluded)", indexes, err)
	}
}

// A primary key in one schema must not hide a same-named index of a same-named
// table in another schema.
func TestSchemaReaderIntegration_PrimaryKeyExclusionStaysInItsSchema(t *testing.T) {
	first := uniqueTable(t, "pg01pka")
	second := uniqueTable(t, "pg01pkb")
	db1 := openSchemaTestDB(t, first, IdentifierExact)
	db2 := openSchemaTestDB(t, second, IdentifierExact)
	s1, s2 := exactIdent(first), exactIdent(second)
	ctx := context.Background()

	// "T_pkey" is the primary key of T in the second schema, and an ordinary
	// unique index of T in the first.
	execAll(t, db2, `CREATE TABLE `+s2+`."T" ("Id" integer, CONSTRAINT "T_pkey" PRIMARY KEY ("Id"))`)
	execAll(t, db1,
		`CREATE TABLE `+s1+`."T" ("Id" integer, "Code" integer)`,
		`CREATE UNIQUE INDEX "T_pkey" ON `+s1+`."T" ("Code")`,
	)

	ref := dal.NewRootCollectionRef("T", "")
	inFirst, err := db1.ListIndexes(ctx, &ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(inFirst) != 1 || inFirst[0].Name != "T_pkey" || !inFirst[0].Unique || !reflect.DeepEqual(inFirst[0].Fields, []dal.FieldName{"Code"}) {
		t.Errorf("first schema indexes = %+v, want the ordinary unique index T_pkey over Code", inFirst)
	}
	inSecond, err := db2.ListIndexes(ctx, &ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(inSecond) != 0 {
		t.Errorf("second schema indexes = %+v, want none (T_pkey backs the primary key)", inSecond)
	}
}

// Each foreign key of a table is one Referrer, and a composite key lists its
// columns in key order, not alphabetically.
func TestSchemaReaderIntegration_ReferrersPerForeignKeyInKeyOrder(t *testing.T) {
	schemaName := uniqueTable(t, "pg01ref")
	db := openSchemaTestDB(t, schemaName, IdentifierExact)
	s := exactIdent(schemaName)
	execAll(t, db,
		`CREATE TABLE `+s+`."Parent" ("B" integer, "A" integer, PRIMARY KEY ("B", "A"))`,
		`CREATE TABLE `+s+`."Child" (
			"Id" integer PRIMARY KEY,
			"Zed" integer, "Alpha" integer, "Solo" integer,
			CONSTRAINT "fk_pair" FOREIGN KEY ("Zed", "Alpha") REFERENCES `+s+`."Parent" ("B", "A"),
			CONSTRAINT "fk_solo" FOREIGN KEY ("Solo", "Alpha") REFERENCES `+s+`."Parent" ("B", "A")
		)`,
	)
	parent := dal.NewRootCollectionRef("Parent", "")
	referrers, err := db.ListReferrers(context.Background(), &parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(referrers) != 2 ||
		!reflect.DeepEqual(referrers[0].Fields, []dal.FieldName{"Zed", "Alpha"}) ||
		!reflect.DeepEqual(referrers[1].Fields, []dal.FieldName{"Solo", "Alpha"}) {
		t.Errorf("referrers = %+v, want fk_pair [Zed Alpha] then fk_solo [Solo Alpha], one Referrer per foreign key", referrers)
	}
}
