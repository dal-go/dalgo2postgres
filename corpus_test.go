package dalgo2postgres

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The helpers of the query corpus: the DTQL fixtures of dal-go/dalgo that this package
// vendors under testdata/joins and testdata/subqueries, the file that lists where
// PostgreSQL's answer differs from the fixture's, and the comparison of rows. The tests
// that run the corpus against a server are in corpus_integration_test.go; the helpers
// need no server and are tested without one (corpus_unit_test.go).

// dalgoModule is the module the fixtures are vendored from.
const dalgoModule = "github.com/dal-go/dalgo"

// fixtureManifest is manifest.json of a vendored fixture directory: the manifest dalgo ships
// with the fixtures (a schema version and the SHA-256 of every file) plus the module,
// version and commit the directory was taken from.
type fixtureManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	SourceModule  string            `json:"sourceModule"`
	SourceVersion string            `json:"sourceVersion"`
	SourceCommit  string            `json:"sourceCommit"`
	SourcePath    string            `json:"sourcePath"`
	Files         map[string]string `json:"files"`
}

// readFixtureManifest reads dir/manifest.json.
func readFixtureManifest(dir string) (fixtureManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fixtureManifest{}, err
	}
	var manifest fixtureManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fixtureManifest{}, fmt.Errorf("%s: %w", filepath.Join(dir, "manifest.json"), err)
	}
	return manifest, nil
}

func digestOf(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

// vendoredProblems lists what is wrong with the vendored directory dir, as sentences, and
// nothing when it is as the manifest says: a file whose SHA-256 is not the manifest's, a file
// the manifest names that is missing, a file the manifest does not name (the names in own,
// local files that are not vendored, excepted), a manifest of another schema version, and a
// manifest that was taken from another module or version than the one this package
// requires. The last is what makes an upstream change fail here until it is taken on
// purpose: a bump of the module moves the version, and the directory is vendored again.
func vendoredProblems(dir, module, version string, own ...string) []string {
	manifest, err := readFixtureManifest(dir)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	if manifest.SchemaVersion != 1 {
		problems = append(problems, fmt.Sprintf("%s: the manifest has schemaVersion %d, want 1", dir, manifest.SchemaVersion))
	}
	if manifest.SourceModule != module || manifest.SourceVersion != version {
		problems = append(problems, fmt.Sprintf("%s: the fixtures were vendored from %s %s, and this package requires %s %s: vendor them again from the module and take the changes on purpose",
			dir, manifest.SourceModule, manifest.SourceVersion, module, version))
	}
	names := make([]string, 0, len(manifest.Files))
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: the manifest names %s, which cannot be read: %v", dir, name, err))
			continue
		}
		if got := digestOf(data); got != manifest.Files[name] {
			problems = append(problems, fmt.Sprintf("%s: %s has SHA-256 %s, the manifest says %s", dir, name, got, manifest.Files[name]))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return append(problems, err.Error())
	}
	for _, entry := range entries {
		name := entry.Name()
		if _, named := manifest.Files[name]; named || name == "manifest.json" || contains(own, name) {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: %s is in the directory and not in the manifest", dir, name))
	}
	return problems
}

func contains(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// upstreamProblems lists how the vendored directory dir differs from the directory it was
// taken from, upstream (the same fixtures in the module's cache): a file with other bytes, a
// file the module has that is not vendored, and a file vendored that the module does not
// have. manifest.json is compared by the files it lists, since the vendored one adds the
// source it came from.
func upstreamProblems(dir, upstream string) []string {
	vendored, err := readFixtureManifest(dir)
	if err != nil {
		return []string{err.Error()}
	}
	original, err := readFixtureManifest(upstream)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	if !reflect.DeepEqual(vendored.Files, original.Files) {
		problems = append(problems, fmt.Sprintf("%s: the manifest lists other files or digests than %s", dir, upstream))
	}
	entries, err := os.ReadDir(upstream)
	if err != nil {
		return append(problems, err.Error())
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "manifest.json" {
			continue
		}
		want, err := os.ReadFile(filepath.Join(upstream, name))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		got, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %s is in the module and is not vendored", dir, name))
		case !bytes.Equal(got, want):
			problems = append(problems, fmt.Sprintf("%s: %s differs from the module's", dir, name))
		}
	}
	if own, err := os.ReadDir(dir); err == nil {
		for _, entry := range own {
			if _, err := os.Stat(filepath.Join(upstream, entry.Name())); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s is not in the module", dir, entry.Name()))
			}
		}
	}
	return problems
}

// moduleVersionIn is the version of module that a go.mod requires, and whether it requires
// it. It reads the lines of a require block and the one-line form.
func moduleVersionIn(goMod []byte, module string) (string, bool) {
	for _, line := range strings.Split(string(goMod), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "require" {
			fields = fields[1:]
		}
		if len(fields) >= 2 && fields[0] == module {
			return fields[1], true
		}
	}
	return "", false
}

// requiredDalgoVersion is the version of dalgo this package's go.mod requires.
func requiredDalgoVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	version, ok := moduleVersionIn(data, dalgoModule)
	if !ok {
		t.Fatalf("go.mod requires no %s", dalgoModule)
	}
	return version
}

// dalgoModuleDir is where the module cache holds the dalgo this package is built with.
func dalgoModuleDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", dalgoModule).Output()
	if err != nil {
		t.Fatalf("go list -m %s: %v", dalgoModule, err)
	}
	return strings.TrimSpace(string(out))
}

// normalizeRows returns rows as JSON makes of them, so that the integers, the floats and the
// counts a reader returns meet the numbers of a fixture's JSON as float64, a NULL stays nil
// and a time is its RFC 3339 text.
func normalizeRows(rows []map[string]any) ([]map[string]any, error) {
	encoded, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// numberTolerance is how far two numbers of a fixture may be apart and still be equal: the
// server computes in double precision and a fixture is written by hand.
const numberTolerance = 1e-9

// sameCell says whether two cells, as JSON holds them, are equal: numbers within
// numberTolerance (relative to their size once they are above 1), everything else exactly.
func sameCell(got, want any) bool {
	switch w := want.(type) {
	case float64:
		g, ok := got.(float64)
		if !ok {
			return false
		}
		if g == w {
			return true
		}
		scale := math.Max(1, math.Max(math.Abs(g), math.Abs(w)))
		return math.Abs(g-w) <= numberTolerance*scale
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !sameCell(g[i], w[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for key, value := range w {
			if other, present := g[key]; !present || !sameCell(other, value) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

// rowDifference says how the rows got differ from the rows want, both as JSON holds them, or
// "" when they are equal: the number of rows when it differs, else the first cell that does.
func rowDifference(got, want []map[string]any) string {
	if len(got) != len(want) {
		return fmt.Sprintf("%d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if sameCell(any(got[i]), any(want[i])) {
			continue
		}
		return fmt.Sprintf("row %d is %s, want %s", i, compact(got[i]), compact(want[i]))
	}
	return ""
}

func compact(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// The kinds of difference a case can have from its fixture. A case that differs in none is
// not listed; a case that differs is listed with the kinds it differs in, and the test fails
// for a difference that is not listed, for a listed case that no longer differs, and for a
// case whose kinds changed.
const (
	differsInRows       = "rows"       // the rows are not the fixture's
	differsInStatements = "statements" // not the statements the fixture's corpus expects of the server
	differsInError      = "error"      // the read failed, or failed with another diagnostic than the fixture's
)

var differenceKinds = []string{differsInRows, differsInStatements, differsInError}

// divergenceSuites are the suites of the corpus an entry can name: the two the tests run.
var divergenceSuites = []string{"joins", "subqueries"}

// divergence is one entry of testdata/postgres-divergences.json. An entry says which kinds of
// difference the case has and also what the case does, so that the file holds a case to
// its difference and not only to its kind: Error is the text the error of the case contains
// (required with the kind "error"; the error must not be the server's), and Statements is
// the number of statements the case sends, the catalog lookup left out (required with the
// kind "statements", where every one of them must also be a plain read of one source).
type divergence struct {
	Suite      string   `json:"suite"`
	Case       string   `json:"case"`
	Differs    []string `json:"differs"`
	Reason     string   `json:"reason"`
	Error      string   `json:"error,omitempty"`
	Statements *int     `json:"statements,omitempty"`
}

func (d divergence) key() string { return d.Suite + "/" + d.Case }

// divergenceFile is testdata/postgres-divergences.json.
type divergenceFile struct {
	SchemaVersion int          `json:"schemaVersion"`
	Divergences   []divergence `json:"divergences"`
}

// parseDivergences reads the divergence file: every entry names a suite of the corpus, a
// case, one or more kinds of difference from differenceKinds, a reason, and what the kinds
// need (an error text for "error", a statement count for "statements"); no case is listed
// twice.
func parseDivergences(data []byte) (map[string]divergence, error) {
	var file divergenceFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.SchemaVersion != 1 {
		return nil, fmt.Errorf("schemaVersion %d, want 1", file.SchemaVersion)
	}
	listed := map[string]divergence{}
	for _, entry := range file.Divergences {
		switch {
		case entry.Suite == "" || entry.Case == "":
			return nil, fmt.Errorf("an entry names no suite or no case: %+v", entry)
		case !contains(divergenceSuites, entry.Suite):
			return nil, fmt.Errorf("%s: unknown suite %q, want one of %v: an entry of another suite would never be checked", entry.key(), entry.Suite, divergenceSuites)
		case strings.TrimSpace(entry.Reason) == "":
			return nil, fmt.Errorf("%s: no reason", entry.key())
		case len(entry.Differs) == 0:
			return nil, fmt.Errorf("%s: no kind of difference", entry.key())
		}
		for _, kind := range entry.Differs {
			if !contains(differenceKinds, kind) {
				return nil, fmt.Errorf("%s: unknown kind of difference %q, want one of %v", entry.key(), kind, differenceKinds)
			}
		}
		switch {
		case contains(entry.Differs, differsInError) && strings.TrimSpace(entry.Error) == "":
			return nil, fmt.Errorf("%s: it differs in %q and says no error text: any error would satisfy it", entry.key(), differsInError)
		case !contains(entry.Differs, differsInError) && entry.Error != "":
			return nil, fmt.Errorf("%s: it gives an error text and does not differ in %q", entry.key(), differsInError)
		case contains(entry.Differs, differsInStatements) && entry.Statements == nil:
			return nil, fmt.Errorf("%s: it differs in %q and says no statement count: any shape would satisfy it", entry.key(), differsInStatements)
		case entry.Statements != nil && *entry.Statements < 0:
			return nil, fmt.Errorf("%s: a statement count of %d", entry.key(), *entry.Statements)
		}
		if _, twice := listed[entry.key()]; twice {
			return nil, fmt.Errorf("%s is listed twice", entry.key())
		}
		listed[entry.key()] = entry
	}
	return listed, nil
}

// loadDivergences reads testdata/postgres-divergences.json.
func loadDivergences(t *testing.T) map[string]divergence {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "postgres-divergences.json"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := parseDivergences(data)
	if err != nil {
		t.Fatalf("testdata/postgres-divergences.json: %v", err)
	}
	return listed
}

// divergenceVerdict is what a case that differs from its fixture in the kinds observed
// must be told, given the entry the file has for it (nil when it has none): the failure, or
// "" when the case is as the file says.
func divergenceVerdict(key string, observed []string, listed *divergence) string {
	observed = append([]string(nil), observed...)
	sort.Strings(observed)
	switch {
	case len(observed) == 0 && listed == nil:
		return ""
	case len(observed) > 0 && listed == nil:
		return fmt.Sprintf("%s differs from its fixture (%s) and is not in testdata/postgres-divergences.json: fix the cause, or list the case there with the reason", key, strings.Join(observed, ", "))
	case len(observed) == 0:
		return fmt.Sprintf("%s is listed in testdata/postgres-divergences.json (%s: %s) and no longer differs from its fixture: delete its entry", key, strings.Join(listed.Differs, ", "), listed.Reason)
	}
	want := append([]string(nil), listed.Differs...)
	sort.Strings(want)
	if !reflect.DeepEqual(observed, want) {
		return fmt.Sprintf("%s differs from its fixture in %s, and testdata/postgres-divergences.json says %s: update its entry", key, strings.Join(observed, ", "), strings.Join(want, ", "))
	}
	return ""
}

// notALeafRead says whether a statement is more than a plain read of one source: it holds a
// join, a subquery or an EXISTS, which DALgo's own engine never sends.
func notALeafRead(statement string) bool {
	upper := strings.ToUpper(statement)
	return strings.Contains(upper, " JOIN ") || strings.Contains(upper, "(SELECT") || strings.Contains(upper, "EXISTS")
}

// divergenceDetails holds a case to what its entry says it does, beyond the kinds: for a
// difference in "error", the read must have failed with an error that contains the entry's
// text and is not the server's (a refusal by the adapter or DALgo, not a server error that
// would hide a different fault); for a statement count, the case must have sent that many
// statements; and for a difference in "statements", each of them must be a plain read. It
// returns the problems as sentences, and none when the case is as its entry says.
func divergenceDetails(entry divergence, err error, statements []string) []string {
	var problems []string
	if contains(entry.Differs, differsInError) {
		var server *pgconn.PgError
		switch {
		case err == nil:
			problems = append(problems, fmt.Sprintf("%s says the read fails with %q, and it did not fail", entry.key(), entry.Error))
		case errors.As(err, &server):
			problems = append(problems, fmt.Sprintf("%s says the read is refused with %q, and the error is the server's (SQLSTATE %s): %v", entry.key(), entry.Error, server.Code, err))
		case !strings.Contains(err.Error(), entry.Error):
			problems = append(problems, fmt.Sprintf("%s says the read fails with %q, and the error is %q", entry.key(), entry.Error, err.Error()))
		}
	}
	if entry.Statements != nil && len(statements) != *entry.Statements {
		problems = append(problems, fmt.Sprintf("%s says %d statements are sent, and %d were: %q", entry.key(), *entry.Statements, len(statements), statements))
	}
	if contains(entry.Differs, differsInStatements) {
		for _, statement := range statements {
			if notALeafRead(statement) {
				problems = append(problems, fmt.Sprintf("%s says every statement is a plain read, and one is not: %s", entry.key(), statement))
			}
		}
	}
	return problems
}

// unrunFixtures lists the problems of a directory of fixtures against the names the tests
// run: a DTQL document in dir (name.dtql.yaml or name.dtql.json) that no test runs, which
// would pass unnoticed, and a name that is run and has no document.
func unrunFixtures(dir string, ran []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var problems []string
	present := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		for _, suffix := range []string{".dtql.yaml", ".dtql.json"} {
			if base, found := strings.CutSuffix(name, suffix); found {
				present[base] = true
				if !contains(ran, base) {
					problems = append(problems, fmt.Sprintf("%s: %s is a fixture that no test runs: add a case for it", dir, name))
				}
			}
		}
	}
	for _, name := range ran {
		if !present[name] {
			problems = append(problems, fmt.Sprintf("%s: a test runs %s and there is no such fixture", dir, name))
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// inputlessCases are the cases of the subquery suite that hold no query of their own and are
// not run, by name: the null truth table of IN and NOT IN, which the runnable in-correlated and
// not-in-correlated cases carry. Any other case without an input is an error.
var inputlessCases = []string{"in-not-in-null-table"}

// unexpectedInputless lists the cases of the suite that hold no input and are not among the
// ones that may.
func unexpectedInputless(cases []suiteCase) []string {
	var unexpected []string
	for _, tc := range cases {
		if tc.Input == "" && !contains(inputlessCases, tc.Name) {
			unexpected = append(unexpected, tc.Name)
		}
	}
	return unexpected
}

// unusedDivergences are the keys of listed entries of the suite that no case of the suite
// has, which are entries that name a fixture that is gone or renamed.
func unusedDivergences(listed map[string]divergence, suite string, seen map[string]bool) []string {
	var unused []string
	for key, entry := range listed {
		if entry.Suite == suite && !seen[entry.Case] {
			unused = append(unused, key)
		}
	}
	sort.Strings(unused)
	return unused
}
