package dalgo2postgres

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
)

// safeIdentRe matches the names this package writes into the text of a statement:
// ASCII letters, digits and underscores, not starting with a digit. Every name is
// always double-quoted by this package, so a reserved word is usable, but a name
// that needs quoting for another reason is not accepted.
var safeIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkedName is a name that passed [checkName]. Only checkName makes one, and
// every builder of a statement takes the names it writes as checkedName, so a
// statement cannot be built from a name that was not checked.
type checkedName struct{ name string }

// checkName is the one rule for every name that a statement creating, altering or
// dropping writes into its text, whatever the position (table, column, index,
// primary-key name, field of an index). The name must be a plain identifier (see
// safeIdentRe) of at most 63 bytes (see [maxIdentifierBytes]) after the
// lower-casing [quoteIdent] applies. The refusal is an error that matches
// [dalgo2sql.ErrUnsafeName] and reads as the schema reader's does; position says
// what the name is (see the position constants).
func checkName(position, name string) (checkedName, error) {
	if err := checkIdentifierLength(position, strings.ToLower(name)); err != nil {
		return checkedName{}, err
	}
	if !safeIdentRe.MatchString(name) {
		return checkedName{}, newUnsafeNameError(position, name, reasonNotPlain)
	}
	return checkedName{name: name}, nil
}

// String is the name as it was given.
func (n checkedName) String() string { return n.name }

// quoted is the name as quoteIdent writes it.
func (n checkedName) quoted() string { return quoteIdent(n.name) }

// quoteIdent folds name to lower case and wraps it in double-quotes for
// Postgres, doubling a double quote inside it, so the result is one quoted
// identifier whatever the name holds: it is safe on its own, without its caller's
// check. Lower-casing makes the case-preserving quoted form agree with the
// unquoted references that dalgo2sql's DML and dal's structured-query
// rendering emit (Postgres folds those to lower case), so DDL and DML always
// address the same physical identifier. Quoting is retained so reserved words
// remain usable. The statements of this package take their names as
// [checkedName] and write them with [checkedName.quoted].
//
// Consequence: collection and column names are stored lower-cased. Typed
// clients round-trip transparently (JSON unmarshalling is case-insensitive);
// consumers reading raw records observe lower-cased field names. Case-
// preserving storage would require dal's structured-query String() to quote
// column identifiers — tracked upstream.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(strings.ToLower(name), `"`, `""`) + `"`
}

func buildCreateTableSQL(c dbschema.CollectionDef, opts ddl.Options) (string, error) {
	table, err := checkName(positionCollection, c.Name)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("CREATE TABLE ")
	if opts.IfNotExists {
		sb.WriteString("IF NOT EXISTS ")
	}
	sb.WriteString(table.quoted())
	sb.WriteString(" (")

	// Build a set of PK field names for quick lookup.
	pkSet := make(map[dal.FieldName]bool, len(c.PrimaryKey))
	for _, n := range c.PrimaryKey {
		pkSet[n] = true
	}

	parts := make([]string, 0, len(c.Fields)+1)
	for _, f := range c.Fields {
		colSQL, err := buildColumnDecl(f, pkSet[f.Name])
		if err != nil {
			return "", err
		}
		parts = append(parts, colSQL)
	}
	if len(c.PrimaryKey) > 0 {
		pkNames := make([]string, len(c.PrimaryKey))
		for i, n := range c.PrimaryKey {
			pk, err := checkName(positionPrimaryKey, string(n))
			if err != nil {
				return "", err
			}
			pkNames[i] = pk.quoted()
		}
		parts = append(parts, "PRIMARY KEY ("+strings.Join(pkNames, ", ")+")")
	}
	sb.WriteString(strings.Join(parts, ", "))
	sb.WriteString(")")
	return sb.String(), nil
}

// buildColumnDecl renders one column declaration with double-quoted identifier
// and PostgreSQL-specific type mapping. The type is one of the closed list of
// postgresTypeFor, or the field is refused. Nothing else of f is written: not its
// default, and not AutoIncrement.
// inPK: this field is a member of the primary key (always NOT NULL in Postgres).
func buildColumnDecl(f dbschema.FieldDef, inPK bool) (string, error) {
	name, err := checkName(positionField, string(f.Name))
	if err != nil {
		return "", err
	}
	sqlType, err := postgresTypeFor(f)
	if err != nil {
		return "", fmt.Errorf("dalgo2postgres: field %q: %w", f.Name, err)
	}
	parts := []string{name.quoted(), sqlType}
	if !f.Nullable || inPK {
		parts = append(parts, "NOT NULL")
	}
	return strings.Join(parts, " "), nil
}

func buildCreateIndexSQL(idx dbschema.IndexDef, opts ddl.Options) (string, error) {
	name, err := checkName(positionIndex, idx.Name)
	if err != nil {
		return "", err
	}
	table, err := checkName(positionCollection, idx.Collection)
	if err != nil {
		return "", err
	}
	if len(idx.Fields) == 0 {
		return "", fmt.Errorf("dalgo2postgres: index %q: must have at least one field", name)
	}
	cols := make([]string, len(idx.Fields))
	for i, n := range idx.Fields {
		col, err := checkName(positionField, string(n))
		if err != nil {
			return "", err
		}
		cols[i] = col.quoted()
	}
	var sb strings.Builder
	sb.WriteString("CREATE ")
	if idx.Unique {
		sb.WriteString("UNIQUE ")
	}
	sb.WriteString("INDEX ")
	if opts.IfNotExists {
		sb.WriteString("IF NOT EXISTS ")
	}
	sb.WriteString(name.quoted())
	sb.WriteString(" ON ")
	sb.WriteString(table.quoted())
	sb.WriteString(" (")
	sb.WriteString(strings.Join(cols, ", "))
	sb.WriteString(")")
	return sb.String(), nil
}

func buildDropTableSQL(name checkedName, opts ddl.Options) string {
	if opts.IfExists {
		return "DROP TABLE IF EXISTS " + name.quoted()
	}
	return "DROP TABLE " + name.quoted()
}

func buildDropIndexSQL(name checkedName, opts ddl.Options) string {
	if opts.IfExists {
		return "DROP INDEX IF EXISTS " + name.quoted()
	}
	return "DROP INDEX " + name.quoted()
}

func buildAlterTableAddColumnSQL(table checkedName, f dbschema.FieldDef) (string, error) {
	colDecl, err := buildColumnDecl(f, false)
	if err != nil {
		return "", err
	}
	return "ALTER TABLE " + table.quoted() + " ADD COLUMN " + colDecl, nil
}

func buildAlterTableDropColumnSQL(table, col checkedName) string {
	return "ALTER TABLE " + table.quoted() + " DROP COLUMN " + col.quoted()
}

func buildAlterTableRenameColumnSQL(table, oldName, newName checkedName) string {
	return "ALTER TABLE " + table.quoted() +
		" RENAME COLUMN " + oldName.quoted() +
		" TO " + newName.quoted()
}
