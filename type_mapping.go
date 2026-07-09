package dalgo2postgres

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dal-go/dalgo/dbschema"
)

// postgresTypeFor returns the PostgreSQL column-type keyword for the given
// dbschema.FieldDef.  Length and Precision annotations are honoured:
//
//   - String   → TEXT (no length) or VARCHAR(n) when Length > 0
//   - Int      → BIGINT
//   - Float    → DOUBLE PRECISION
//   - Bool     → BOOLEAN
//   - Time     → TIMESTAMPTZ
//   - Decimal  → NUMERIC (no precision) or NUMERIC(total, scale) when Precision != nil
//   - Bytes    → BYTEA
func postgresTypeFor(f dbschema.FieldDef) (string, error) {
	switch f.Type {
	case dbschema.String:
		if f.Length != nil && *f.Length > 0 {
			return fmt.Sprintf("VARCHAR(%d)", *f.Length), nil
		}
		return "TEXT", nil
	case dbschema.Int:
		return "BIGINT", nil
	case dbschema.Float:
		return "DOUBLE PRECISION", nil
	case dbschema.Bool:
		return "BOOLEAN", nil
	case dbschema.Time:
		return "TIMESTAMPTZ", nil
	case dbschema.Decimal:
		if f.Precision != nil {
			return fmt.Sprintf("NUMERIC(%d, %d)", f.Precision.Total, f.Precision.Scale), nil
		}
		return "NUMERIC", nil
	case dbschema.Bytes:
		return "BYTEA", nil
	case dbschema.Null:
		return "", fmt.Errorf("dalgo2postgres: dbschema.Null is not a valid column type")
	default:
		return "", fmt.Errorf("dalgo2postgres: unknown dbschema.Type %v", f.Type)
	}
}

// numericRe matches NUMERIC(p,s) or DECIMAL(p,s) from information_schema.
var numericRe = regexp.MustCompile(`^(?:NUMERIC|DECIMAL)\s*\(\s*(\d+)\s*,\s*(\d+)\s*\)$`)

// varcharRe matches CHARACTER VARYING(n) or VARCHAR(n).
var varcharRe = regexp.MustCompile(`^(?:CHARACTER VARYING|VARCHAR)\s*\(\s*(\d+)\s*\)$`)

// dbschemaTypeFromPostgres maps information_schema.columns.data_type (or
// udt_name) to a dbschema.Type.  The mapping handles the most common
// PostgreSQL built-in types.  It is lossy by design: the round-trip is
// sufficient for schema inspection and migration, not for exact DDL replay.
func dbschemaTypeFromPostgres(dataType, udtName string) (dbschema.Type, *dbschema.Precision, *int, bool) {
	upper := strings.ToUpper(strings.TrimSpace(dataType))
	udt := strings.ToLower(strings.TrimSpace(udtName))

	// VARCHAR(n) / CHARACTER VARYING(n)
	if m := varcharRe.FindStringSubmatch(upper); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return dbschema.String, nil, &n, true
		}
	}

	// NUMERIC(p,s) / DECIMAL(p,s)
	if m := numericRe.FindStringSubmatch(upper); m != nil {
		total, errT := strconv.Atoi(m[1])
		scale, errS := strconv.Atoi(m[2])
		if errT == nil && errS == nil {
			return dbschema.Decimal, &dbschema.Precision{Total: total, Scale: scale}, nil, true
		}
	}

	switch upper {
	case "TEXT", "CHARACTER VARYING", "CHARACTER", "CHAR", "NAME":
		return dbschema.String, nil, nil, true
	case "BIGINT", "INTEGER", "SMALLINT", "INT", "INT2", "INT4", "INT8":
		return dbschema.Int, nil, nil, true
	case "DOUBLE PRECISION", "REAL", "FLOAT4", "FLOAT8":
		return dbschema.Float, nil, nil, true
	case "BOOLEAN":
		return dbschema.Bool, nil, nil, true
	case "TIMESTAMP WITH TIME ZONE", "TIMESTAMPTZ",
		"TIMESTAMP WITHOUT TIME ZONE", "TIMESTAMP", "DATE", "TIME WITH TIME ZONE",
		"TIME WITHOUT TIME ZONE", "TIME":
		return dbschema.Time, nil, nil, true
	case "NUMERIC", "DECIMAL":
		return dbschema.Decimal, nil, nil, true
	case "BYTEA":
		return dbschema.Bytes, nil, nil, true
	case "USER-DEFINED":
		// Fall through to udt_name check below.
	}

	// udt_name fallback for types that appear as USER-DEFINED in data_type.
	switch udt {
	case "timestamptz":
		return dbschema.Time, nil, nil, true
	case "numeric":
		return dbschema.Decimal, nil, nil, true
	}

	return dbschema.Null, nil, nil, false
}
