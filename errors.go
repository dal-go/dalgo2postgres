package dalgo2postgres

import "fmt"

// newCollectionNotFoundError formats the standard "collection not
// found" error. The contract is content-based per the Feature spec:
// the message MUST contain the substring "not found" and the collection name.
func newCollectionNotFoundError(name string) error {
	return fmt.Errorf("dalgo2postgres: collection %q not found", name)
}
