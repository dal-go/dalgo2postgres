package dalgo2postgres

import "fmt"

// quotedCollection is a collection as an error message writes it: the quoted
// name, preceded by the quoted schema when the collection reference named one.
// Only [collectionLabel] makes one. It is a type of its own, not a string, so
// that a bare table name cannot be passed where a label is expected and end up
// in a message unquoted: the compiler refuses it.
type quotedCollection string

// newCollectionNotFoundError formats the standard "collection not found" error
// for a bare collection name, which it quotes. The contract is content-based per
// the Feature spec: the message MUST contain the substring "not found" and the
// collection name. Code that knows the schema the reference named passes a
// [collectionLabel] to [newQualifiedCollectionNotFoundError] instead.
func newCollectionNotFoundError(name string) error {
	return newQualifiedCollectionNotFoundError(collectionLabel("", name))
}

// newQualifiedCollectionNotFoundError formats the same error for a collection
// as [collectionLabel] writes it.
func newQualifiedCollectionNotFoundError(label quotedCollection) error {
	return fmt.Errorf("dalgo2postgres: collection %s not found", label)
}

// collectionLabel writes a collection for an error message: the quoted name,
// preceded by the quoted schema when the collection reference named one
// (`"sales"."Album"`). A schema that only came from the Database's
// configuration is not repeated: the caller chose it once, for every table.
func collectionLabel(schema, name string) quotedCollection {
	if schema == "" {
		return quotedCollection(fmt.Sprintf("%q", name))
	}
	return quotedCollection(fmt.Sprintf("%q.%q", schema, name))
}
