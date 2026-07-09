package dalgo2postgres

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
)

// safeIdentRe matches identifier names that are safe to embed in SQL without
// further escaping: ASCII letters, digits, and underscores only.
// Postgres keywords, spaces, and special characters are intentionally excluded
// because they require double-quoting; all identifiers are always double-quoted
// by this package, so only the character-set check is enforced here.
var safeIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateIdent returns an error if name is empty or contains characters
// outside the safe ASCII identifier set.
func validateIdent(name string) error {
	if name == "" {
		return fmt.Errorf("dalgo2postgres: identifier cannot be empty")
	}
	if !safeIdentRe.MatchString(name) {
		return fmt.Errorf("dalgo2postgres: identifier %q contains characters not matching [A-Za-z_][A-Za-z0-9_]*", name)
	}
	return nil
}

// quoteIdent folds name to lower case and wraps it in double-quotes for
// Postgres. Lower-casing makes the case-preserving quoted form agree with the
// unquoted references that dalgo2sql's DML and dal's structured-query
// rendering emit (Postgres folds those to lower case), so DDL and DML always
// address the same physical identifier. Quoting is retained so reserved words
// and otherwise-illegal names remain usable. Caller must have validated name
// first via validateIdent to prevent injection.
//
// Consequence: collection and column names are stored lower-cased. Typed
// clients round-trip transparently (JSON unmarshalling is case-insensitive);
// consumers reading raw records observe lower-cased field names. Case-
// preserving storage would require dal's structured-query String() to quote
// column identifiers — tracked upstream.
func quoteIdent(name string) string {
	return `"` + strings.ToLower(name) + `"`
}

func buildCreateTableSQL(c dbschema.CollectionDef, opts ddl.Options) (string, error) {
	if err := validateIdent(c.Name); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("CREATE TABLE ")
	if opts.IfNotExists {
		sb.WriteString("IF NOT EXISTS ")
	}
	sb.WriteString(quoteIdent(c.Name))
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
			pkNames[i] = quoteIdent(string(n))
		}
		parts = append(parts, "PRIMARY KEY ("+strings.Join(pkNames, ", ")+")")
	}
	sb.WriteString(strings.Join(parts, ", "))
	sb.WriteString(")")
	return sb.String(), nil
}

// buildColumnDecl renders one column declaration with double-quoted identifier
// and PostgreSQL-specific type mapping.
// inPK: this field is a member of the primary key (always NOT NULL in Postgres).
func buildColumnDecl(f dbschema.FieldDef, inPK bool) (string, error) {
	if err := validateIdent(string(f.Name)); err != nil {
		return "", err
	}
	sqlType, err := postgresTypeFor(f)
	if err != nil {
		return "", fmt.Errorf("dalgo2postgres: field %q: %w", f.Name, err)
	}
	parts := []string{quoteIdent(string(f.Name)), sqlType}
	if !f.Nullable || inPK {
		parts = append(parts, "NOT NULL")
	}
	return strings.Join(parts, " "), nil
}

func buildCreateIndexSQL(idx dbschema.IndexDef, opts ddl.Options) (string, error) {
	if idx.Name == "" {
		return "", fmt.Errorf("dalgo2postgres: index name cannot be empty")
	}
	if idx.Collection == "" {
		return "", fmt.Errorf("dalgo2postgres: index %q: collection cannot be empty", idx.Name)
	}
	if len(idx.Fields) == 0 {
		return "", fmt.Errorf("dalgo2postgres: index %q: must have at least one field", idx.Name)
	}
	if err := validateIdent(idx.Name); err != nil {
		return "", err
	}
	if err := validateIdent(idx.Collection); err != nil {
		return "", err
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
	sb.WriteString(quoteIdent(idx.Name))
	sb.WriteString(" ON ")
	sb.WriteString(quoteIdent(idx.Collection))
	sb.WriteString(" (")
	cols := make([]string, len(idx.Fields))
	for i, n := range idx.Fields {
		cols[i] = quoteIdent(string(n))
	}
	sb.WriteString(strings.Join(cols, ", "))
	sb.WriteString(")")
	return sb.String(), nil
}

func buildDropTableSQL(name string, opts ddl.Options) string {
	if opts.IfExists {
		return "DROP TABLE IF EXISTS " + quoteIdent(name)
	}
	return "DROP TABLE " + quoteIdent(name)
}

func buildDropIndexSQL(name string, opts ddl.Options) string {
	if opts.IfExists {
		return "DROP INDEX IF EXISTS " + quoteIdent(name)
	}
	return "DROP INDEX " + quoteIdent(name)
}

func buildAlterTableAddColumnSQL(table string, f dbschema.FieldDef) (string, error) {
	colDecl, err := buildColumnDecl(f, false)
	if err != nil {
		return "", err
	}
	return "ALTER TABLE " + quoteIdent(table) + " ADD COLUMN " + colDecl, nil
}

func buildAlterTableDropColumnSQL(table string, col dal.FieldName) string {
	return "ALTER TABLE " + quoteIdent(table) + " DROP COLUMN " + quoteIdent(string(col))
}

func buildAlterTableRenameColumnSQL(table string, oldName, newName dal.FieldName) string {
	return "ALTER TABLE " + quoteIdent(table) +
		" RENAME COLUMN " + quoteIdent(string(oldName)) +
		" TO " + quoteIdent(string(newName))
}
