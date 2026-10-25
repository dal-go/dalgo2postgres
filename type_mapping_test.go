package dalgo2postgres

import (
	"testing"

	"github.com/dal-go/dalgo/dbschema"
)

func TestDbschemaTypeFromPostgres(t *testing.T) {
	t.Parallel()
	intPtr := func(n int) *int { return &n }
	tests := []struct {
		name          string
		dataType, udt string
		want          dbschema.Type
		wantPrecision *dbschema.Precision
		wantLength    *int
	}{
		// Mapped types keep their meaning.
		{name: "text", dataType: "text", udt: "text", want: dbschema.String},
		{name: "character varying", dataType: "character varying", udt: "varchar", want: dbschema.String},
		{name: "varchar(n) spelling", dataType: "VARCHAR(40)", udt: "varchar", want: dbschema.String, wantLength: intPtr(40)},
		{name: "character", dataType: "character", udt: "bpchar", want: dbschema.String},
		{name: "name", dataType: "name", udt: "name", want: dbschema.String},
		{name: "integer", dataType: "integer", udt: "int4", want: dbschema.Int},
		{name: "bigint", dataType: "bigint", udt: "int8", want: dbschema.Int},
		{name: "smallint", dataType: "smallint", udt: "int2", want: dbschema.Int},
		{name: "double precision", dataType: "double precision", udt: "float8", want: dbschema.Float},
		{name: "real", dataType: "real", udt: "float4", want: dbschema.Float},
		{name: "boolean", dataType: "boolean", udt: "bool", want: dbschema.Bool},
		{name: "timestamptz", dataType: "timestamp with time zone", udt: "timestamptz", want: dbschema.Time},
		{name: "timestamp", dataType: "timestamp without time zone", udt: "timestamp", want: dbschema.Time},
		{name: "date", dataType: "date", udt: "date", want: dbschema.Time},
		{name: "time", dataType: "time without time zone", udt: "time", want: dbschema.Time},
		{name: "numeric", dataType: "numeric", udt: "numeric", want: dbschema.Decimal},
		{name: "numeric(p,s) spelling", dataType: "NUMERIC(10, 2)", udt: "numeric", want: dbschema.Decimal, wantPrecision: &dbschema.Precision{Total: 10, Scale: 2}},
		{name: "bytea", dataType: "bytea", udt: "bytea", want: dbschema.Bytes},
		{name: "user-defined timestamptz", dataType: "USER-DEFINED", udt: "timestamptz", want: dbschema.Time},
		{name: "user-defined numeric", dataType: "USER-DEFINED", udt: "numeric", want: dbschema.Decimal},

		// Types without a dbschema counterpart read as String instead of failing the table.
		{name: "uuid", dataType: "uuid", udt: "uuid", want: dbschema.String},
		{name: "json", dataType: "json", udt: "json", want: dbschema.String},
		{name: "jsonb", dataType: "jsonb", udt: "jsonb", want: dbschema.String},
		{name: "array", dataType: "ARRAY", udt: "_text", want: dbschema.String},
		{name: "enum", dataType: "USER-DEFINED", udt: "mood", want: dbschema.String},
		{name: "interval", dataType: "interval", udt: "interval", want: dbschema.String},
		{name: "inet", dataType: "inet", udt: "inet", want: dbschema.String},
		{name: "money", dataType: "money", udt: "money", want: dbschema.String},
		{name: "empty", dataType: "", udt: "", want: dbschema.String},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, precision, length := dbschemaTypeFromPostgres(tt.dataType, tt.udt)
			if got != tt.want {
				t.Errorf("type = %v, want %v", got, tt.want)
			}
			if (precision == nil) != (tt.wantPrecision == nil) || (precision != nil && *precision != *tt.wantPrecision) {
				t.Errorf("precision = %+v, want %+v", precision, tt.wantPrecision)
			}
			if (length == nil) != (tt.wantLength == nil) || (length != nil && *length != *tt.wantLength) {
				t.Errorf("length = %v, want %v", length, tt.wantLength)
			}
		})
	}
}

func TestDbschemaTypeFromPostgres_OverflowingSizesFallBack(t *testing.T) {
	t.Parallel()
	// A length or precision too large for int cannot be parsed; the column still reads.
	huge := "99999999999999999999999999"
	if got, _, length := dbschemaTypeFromPostgres("VARCHAR("+huge+")", "varchar"); got != dbschema.String || length != nil {
		t.Errorf("huge varchar length: type=%v length=%v, want String and no length", got, length)
	}
	// The udt_name still says numeric, so the column stays a Decimal; its precision comes
	// from the numeric_precision column instead.
	if got, precision, _ := dbschemaTypeFromPostgres("NUMERIC("+huge+", 2)", "numeric"); got != dbschema.Decimal || precision != nil {
		t.Errorf("huge numeric precision: type=%v precision=%v, want Decimal and no precision", got, precision)
	}
}

func TestPostgresTypeFor(t *testing.T) {
	t.Parallel()
	intPtr := func(n int) *int { return &n }
	tests := []struct {
		name    string
		field   dbschema.FieldDef
		want    string
		wantErr bool
	}{
		{name: "string default", field: dbschema.FieldDef{Type: dbschema.String}, want: "TEXT"},
		{name: "string zero length", field: dbschema.FieldDef{Type: dbschema.String, Length: intPtr(0)}, want: "TEXT"},
		{name: "string with length", field: dbschema.FieldDef{Type: dbschema.String, Length: intPtr(40)}, want: "VARCHAR(40)"},
		{name: "int", field: dbschema.FieldDef{Type: dbschema.Int}, want: "BIGINT"},
		{name: "float", field: dbschema.FieldDef{Type: dbschema.Float}, want: "DOUBLE PRECISION"},
		{name: "bool", field: dbschema.FieldDef{Type: dbschema.Bool}, want: "BOOLEAN"},
		{name: "time", field: dbschema.FieldDef{Type: dbschema.Time}, want: "TIMESTAMPTZ"},
		{name: "decimal default", field: dbschema.FieldDef{Type: dbschema.Decimal}, want: "NUMERIC"},
		{name: "decimal with precision", field: dbschema.FieldDef{Type: dbschema.Decimal, Precision: &dbschema.Precision{Total: 10, Scale: 2}}, want: "NUMERIC(10, 2)"},
		{name: "bytes", field: dbschema.FieldDef{Type: dbschema.Bytes}, want: "BYTEA"},
		{name: "null", field: dbschema.FieldDef{Type: dbschema.Null}, wantErr: true},
		{name: "unknown", field: dbschema.FieldDef{Type: dbschema.Type(99)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := postgresTypeFor(tt.field)
			if (err != nil) != tt.wantErr {
				t.Fatalf("postgresTypeFor(%+v) err = %v, wantErr %v", tt.field, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("postgresTypeFor(%+v) = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}
