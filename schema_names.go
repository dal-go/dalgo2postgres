package dalgo2postgres

import (
	"fmt"
	"strconv"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// maxIdentifierBytes is how many bytes of an identifier PostgreSQL keeps. The
// schema reader sends a schema and a table name as bound parameters, and the
// server compares them as identifiers, so a name longer than this is not a name
// of this server: the reader refuses it, as the key paths of dalgo2sql do under
// the postgres dialect, before any statement.
const maxIdentifierBytes = 63

// maxNameInError is how many characters of a refused name its error shows, the
// same as dalgo2sql's.
const maxNameInError = 32

// The positions a refused name can have, as dalgo2sql's errors name them.
const (
	positionCollection = "collection"
	positionSchema     = "schema"
)

// tooLongNameError is the refusal of a name over [maxIdentifierBytes]. It is
// dalgo2sql's error for the same name in a key path: it matches
// [dalgo2sql.ErrUnsafeName] and reads the same, character for character.
// dalgo2sql does not export its constructor, so the text is written here; a unit
// test holds the two texts equal.
type tooLongNameError struct {
	position  string
	name      string // at most maxNameInError characters of the refused name
	truncated bool
}

func newTooLongNameError(position, name string) *tooLongNameError {
	err := &tooLongNameError{position: position, name: name}
	characters := 0
	for i := range name {
		if characters == maxNameInError {
			err.name, err.truncated = name[:i], true
			break
		}
		characters++
	}
	return err
}

func (e *tooLongNameError) Error() string {
	truncated := ""
	if e.truncated {
		truncated = " (truncated)"
	}
	return fmt.Sprintf("%v: %s name %s%s is too long for PostgreSQL: it keeps %d bytes",
		dalgo2sql.ErrUnsafeName, e.position, strconv.Quote(e.name), truncated, maxIdentifierBytes)
}

func (e *tooLongNameError) Unwrap() error { return dalgo2sql.ErrUnsafeName }

// checkIdentifierLength refuses name when it is longer than PostgreSQL keeps of
// an identifier. The rule is in bytes, as the server's.
func checkIdentifierLength(position, name string) error {
	if len(name) > maxIdentifierBytes {
		return newTooLongNameError(position, name)
	}
	return nil
}

// lookupNames returns the schema and the table name the schema reader looks up
// for ref: the schema the reference names, else the configured one, and the name
// as [IdentifierMode] resolves it. It refuses either one over 63 bytes, so no
// statement is sent for it; the length is that of the name the server would be
// given, after folding.
func (d *Database) lookupNames(ref *dal.CollectionRef) (schema, name string, err error) {
	schema = d.schemaFor(ref)
	if err = checkIdentifierLength(positionSchema, schema); err != nil {
		return "", "", err
	}
	name = d.resolveName(ref.Name())
	if err = checkIdentifierLength(positionCollection, name); err != nil {
		return "", "", err
	}
	return schema, name, nil
}
