package dalgo2postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/dal-go/dalgo2sql"
	dalrecord "github.com/dal-go/record"
)

// checkName is the one rule for every name a statement that creates, alters or
// drops writes: a plain identifier of at most 63 bytes after lower-casing.

func TestCheckName_AcceptsAPlainIdentifierOfUpTo63Bytes(t *testing.T) {
	for _, name := range []string{
		"a", "_", "_a", "A", "a1", "A1_b", "Orders", "select", "table",
		strings.Repeat("n", 63), strings.Repeat("N", 63), "a" + strings.Repeat("_", 62),
	} {
		got, err := checkName(positionField, name)
		if err != nil {
			t.Errorf("checkName(%q) = %v, want it accepted", name, err)
			continue
		}
		if got.String() != name {
			t.Errorf("checkName(%q).String() = %q, want the name as given", name, got.String())
		}
		if want := `"` + strings.ToLower(name) + `"`; got.quoted() != want {
			t.Errorf("checkName(%q).quoted() = %s, want %s", name, got.quoted(), want)
		}
	}
}

func TestCheckName_RefusesWhatIsNotAPlainIdentifierOrIsOver63Bytes(t *testing.T) {
	for _, tc := range []struct {
		what, name, says string
	}{
		{"an empty name", "", "is not a plain identifier"},
		{"a leading digit", "1a", "is not a plain identifier"},
		{"a space", "a b", "is not a plain identifier"},
		{"a double quote", `a"b`, "is not a plain identifier"},
		{"a single quote", `a'b`, "is not a plain identifier"},
		{"a semicolon", "a;b", "is not a plain identifier"},
		{"a dash", "a-b", "is not a plain identifier"},
		{"a dot", "a.b", "is not a plain identifier"},
		{"a trailing newline", "a\n", "is not a plain identifier"},
		{"a NUL", "a\x00b", "is not a plain identifier"},
		{"an invalid UTF-8 byte", "a\xffb", "is not a plain identifier"},
		{"a non-ASCII letter", "café", "is not a plain identifier"},
		{"64 bytes", strings.Repeat("n", 64), "it keeps 63 bytes"},
		{"64 bytes in capitals", strings.Repeat("N", 64), "it keeps 63 bytes"},
		{"32 two-byte characters", strings.Repeat("é", 32), "it keeps 63 bytes"},
		{"63 bytes that are 94 once lower-cased", strings.Repeat("Ⱥ", 31) + "a", "it keeps 63 bytes"},
		{"64 bytes with a double quote", strings.Repeat("n", 63) + `"`, "it keeps 63 bytes"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := checkName(positionIndex, tc.name)
			if !errors.Is(err, dalgo2sql.ErrUnsafeName) {
				t.Fatalf("checkName(%q) = %v, want a refusal that matches dalgo2sql.ErrUnsafeName", tc.name, err)
			}
			if !strings.Contains(err.Error(), ": index name ") || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %v, want an index name that %s", err, tc.says)
			}
			if got != (checkedName{}) {
				t.Errorf("a refused name gave the checked name %v, want none", got)
			}
		})
	}
}

func TestCheckName_ReadsAsTheErrorOfTheKeyPaths(t *testing.T) {
	// The wording is dalgo2sql's: the refusal of a name is the text a key path gives
	// for the same name as a collection, both for a name that is not a plain
	// identifier and for one that is too long, including the part of a long name it
	// shows.
	for _, name := range []string{
		"a b", `a"b`, "1a", "café", "a;b",
		strings.Repeat("n", 64), strings.Repeat("n", 200), strings.Repeat("é", 32), strings.Repeat("é", 40),
		strings.Repeat(`x"`, 40),
	} {
		sqlDB, _ := newStructuredMock(t)
		keyDB := openMockedDatabase(t, sqlDB, dalgo2sql.DbOptions{})
		_, want := keyDB.Exists(context.Background(), dalrecord.NewKeyWithID(name, "id1"))
		if !errors.Is(want, dalgo2sql.ErrUnsafeName) {
			t.Fatalf("a key path's error for %q = %v, want ErrUnsafeName", name, want)
		}
		if _, got := checkName(positionCollection, name); got == nil || got.Error() != want.Error() {
			t.Errorf("name %q: checkName error = %v\n key path error = %v\nwant the same", name, got, want)
		}
	}
}

func TestCheckName_ShowsLittleOfTheName(t *testing.T) {
	_, err := checkName(positionPrimaryKey, strings.Repeat(`x"`, 50))
	want := `unsafe SQL name: primary key name ` + strconv.Quote(strings.Repeat(`x"`, 16)) + ` (truncated) is too long for PostgreSQL: it keeps 63 bytes`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	_, err = checkName(positionField, strings.Repeat(`"`, 50))
	want = `unsafe SQL name: field name ` + strconv.Quote(strings.Repeat(`"`, 32)) + ` (truncated) is not a plain identifier`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}
