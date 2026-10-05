package dalgo2postgres

import (
	"fmt"
	"strconv"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
)

// maxIdentifierBytes is how many bytes of an identifier PostgreSQL keeps. A name
// over it is refused before any statement, wherever this package would send it
// (as a bound parameter of the schema reader, or as the text of a statement that
// creates, alters or drops), as dalgo2sql's key paths refuse it under the postgres
// dialect.
const maxIdentifierBytes = 63

// maxNameInError is how many characters of a refused name its error shows, the
// same as dalgo2sql's.
const maxNameInError = 32

// The positions a refused name can have, as dalgo2sql's errors name them (index
// is the one position dalgo2sql has no key path for).
const (
	positionCollection = "collection"
	positionSchema     = "schema"
	positionField      = "field"
	positionPrimaryKey = "primary key"
	positionIndex      = "index"
)

// The reasons a name is refused, as dalgo2sql words them for the same name.
const reasonNotPlain = "is not a plain identifier"

var reasonTooLong = fmt.Sprintf("is too long for PostgreSQL: it keeps %d bytes", maxIdentifierBytes)

// unsafeNameError is the refusal of a name. It is dalgo2sql's error for the same
// name in a key path: it matches [dalgo2sql.ErrUnsafeName] and reads the same,
// character for character. dalgo2sql does not export its constructor, so the text
// is written here, once, for every refusal of a name of this package; a unit test
// holds the texts equal.
type unsafeNameError struct {
	position  string
	name      string // at most maxNameInError characters of the refused name
	truncated bool
	reason    string
}

func newUnsafeNameError(position, name, reason string) *unsafeNameError {
	err := &unsafeNameError{position: position, name: name, reason: reason}
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

func (e *unsafeNameError) Error() string {
	truncated := ""
	if e.truncated {
		truncated = " (truncated)"
	}
	return fmt.Sprintf("%v: %s name %s%s %s", dalgo2sql.ErrUnsafeName, e.position, strconv.Quote(e.name), truncated, e.reason)
}

func (e *unsafeNameError) Unwrap() error { return dalgo2sql.ErrUnsafeName }

// checkIdentifierLength refuses name when it is longer than PostgreSQL keeps of
// an identifier. The rule is in bytes, as the server's.
func checkIdentifierLength(position, name string) error {
	if len(name) > maxIdentifierBytes {
		return newUnsafeNameError(position, name, reasonTooLong)
	}
	return nil
}

// lookupNames returns the schema and the table name the schema reader looks up
// for ref: the schema the reference names, else the configured one, and the name
// as [IdentifierMode] resolves it. It refuses either one over 63 bytes, so no
// statement is sent for it; the length is that of the name the server would be
// given, after folding. Both are then sent as parameters of type text ($1::text,
// $2::text in every statement of the reader), so that the server compares the
// whole name with the stored one, in any database encoding; the check above is the
// early refusal, with no statement.
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
