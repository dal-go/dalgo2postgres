package dalgo2postgres

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests need no server: they test the helpers of the query corpus (corpus_test.go),
// so that a check that never fails is noticed here and not only on a server.

// writeFixtureDir writes files into a new directory with a manifest of their digests,
// from the module and version given, and returns the directory.
func writeFixtureDir(t *testing.T, module, version string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	digests := map[string]string{}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		digests[name] = digestOf([]byte(content))
	}
	writeManifest(t, dir, fixtureManifest{SchemaVersion: 1, SourceModule: module, SourceVersion: version, Files: digests})
	return dir
}

func writeManifest(t *testing.T, dir string, manifest fixtureManifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func wantProblem(t *testing.T, problems []string, fragment string) {
	t.Helper()
	for _, problem := range problems {
		if strings.Contains(problem, fragment) {
			return
		}
	}
	t.Errorf("problems = %q, want one that says %q", problems, fragment)
}

func TestVendoredProblems(t *testing.T) {
	files := map[string]string{"a.yaml": "a: 1\n", "b.json": "{}\n"}
	t.Run("a directory as its manifest says has no problem", func(t *testing.T) {
		dir := writeFixtureDir(t, "m", "v1", files)
		if problems := vendoredProblems(dir, "m", "v1"); len(problems) != 0 {
			t.Errorf("problems = %q, want none", problems)
		}
	})
	t.Run("a changed file is reported", func(t *testing.T) {
		dir := writeFixtureDir(t, "m", "v1", files)
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("a: 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		wantProblem(t, vendoredProblems(dir, "m", "v1"), "a.yaml has SHA-256")
	})
	t.Run("a missing file is reported", func(t *testing.T) {
		dir := writeFixtureDir(t, "m", "v1", files)
		if err := os.Remove(filepath.Join(dir, "b.json")); err != nil {
			t.Fatal(err)
		}
		wantProblem(t, vendoredProblems(dir, "m", "v1"), "the manifest names b.json, which cannot be read")
	})
	t.Run("a file the manifest does not name is reported, unless it is a local file", func(t *testing.T) {
		dir := writeFixtureDir(t, "m", "v1", files)
		if err := os.WriteFile(filepath.Join(dir, "extra.sql"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		wantProblem(t, vendoredProblems(dir, "m", "v1"), "extra.sql is in the directory and not in the manifest")
		if problems := vendoredProblems(dir, "m", "v1", "extra.sql"); len(problems) != 0 {
			t.Errorf("problems = %q, want none for a local file", problems)
		}
	})
	t.Run("a manifest of another version or module than the one required is reported", func(t *testing.T) {
		dir := writeFixtureDir(t, "m", "v1", files)
		wantProblem(t, vendoredProblems(dir, "m", "v2"), "vendored from m v1, and this package requires m v2")
		wantProblem(t, vendoredProblems(dir, "other", "v1"), "vendored from m v1, and this package requires other v1")
	})
	t.Run("a manifest of another schema version is reported", func(t *testing.T) {
		dir := writeFixtureDir(t, "m", "v1", files)
		writeManifest(t, dir, fixtureManifest{SchemaVersion: 2, SourceModule: "m", SourceVersion: "v1", Files: map[string]string{}})
		wantProblem(t, vendoredProblems(dir, "m", "v1"), "schemaVersion 2, want 1")
	})
	t.Run("no manifest, an unreadable manifest and no directory are reported", func(t *testing.T) {
		empty := t.TempDir()
		if problems := vendoredProblems(empty, "m", "v1"); len(problems) != 1 {
			t.Errorf("problems = %q, want the missing manifest", problems)
		}
		if err := os.WriteFile(filepath.Join(empty, "manifest.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		wantProblem(t, vendoredProblems(empty, "m", "v1"), "manifest.json")
		if problems := vendoredProblems(filepath.Join(empty, "nope"), "m", "v1"); len(problems) != 1 {
			t.Errorf("problems = %q, want the missing directory", problems)
		}
	})
}

func TestUpstreamProblems(t *testing.T) {
	files := map[string]string{"a.yaml": "a: 1\n", "b.json": "{}\n"}
	t.Run("the same files have no problem, whatever the vendored manifest adds", func(t *testing.T) {
		upstream := writeFixtureDir(t, "", "", files)
		vendored := writeFixtureDir(t, "m", "v1", files)
		if problems := upstreamProblems(vendored, upstream); len(problems) != 0 {
			t.Errorf("problems = %q, want none", problems)
		}
	})
	t.Run("a file with other bytes is reported", func(t *testing.T) {
		upstream := writeFixtureDir(t, "", "", files)
		vendored := writeFixtureDir(t, "m", "v1", map[string]string{"a.yaml": "a: 2\n", "b.json": "{}\n"})
		problems := upstreamProblems(vendored, upstream)
		wantProblem(t, problems, "a.yaml differs from the module's")
		wantProblem(t, problems, "the manifest lists other files or digests")
	})
	t.Run("a file the module has and the directory has not is reported", func(t *testing.T) {
		upstream := writeFixtureDir(t, "", "", files)
		vendored := writeFixtureDir(t, "m", "v1", map[string]string{"a.yaml": "a: 1\n"})
		wantProblem(t, upstreamProblems(vendored, upstream), "b.json is in the module and is not vendored")
	})
	t.Run("a file the directory has and the module has not is reported", func(t *testing.T) {
		upstream := writeFixtureDir(t, "", "", files)
		vendored := writeFixtureDir(t, "m", "v1", files)
		if err := os.WriteFile(filepath.Join(vendored, "extra.sql"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		wantProblem(t, upstreamProblems(vendored, upstream), "extra.sql is not in the module")
	})
	t.Run("a missing manifest or directory on either side is reported", func(t *testing.T) {
		good := writeFixtureDir(t, "m", "v1", files)
		empty := t.TempDir()
		if problems := upstreamProblems(empty, good); len(problems) != 1 {
			t.Errorf("problems = %q, want the vendored manifest missing", problems)
		}
		if problems := upstreamProblems(good, empty); len(problems) != 1 {
			t.Errorf("problems = %q, want the module's manifest missing", problems)
		}
	})
	t.Run("a module file that cannot be read is reported", func(t *testing.T) {
		upstream := writeFixtureDir(t, "", "", files)
		vendored := writeFixtureDir(t, "m", "v1", files)
		if err := os.Remove(filepath.Join(upstream, "b.json")); err != nil {
			t.Fatal(err)
		}
		// A directory where a file is expected lists in the module and cannot be read as one.
		if err := os.Mkdir(filepath.Join(upstream, "b.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		wantProblem(t, upstreamProblems(vendored, upstream), "b.json")
	})
}

func TestModuleVersionIn(t *testing.T) {
	goMod := []byte("module x\n\nrequire github.com/dal-go/dalgo v0.89.6\n\nrequire (\n\tgithub.com/dal-go/dalgo2sql v0.26.5\n\tgithub.com/dal-go/dalgo v0.90.0\n)\n")
	if got, ok := moduleVersionIn(goMod, "github.com/dal-go/dalgo"); !ok || got != "v0.89.6" {
		t.Errorf("dalgo = %q, %v; want v0.89.6 (the first requirement)", got, ok)
	}
	if got, ok := moduleVersionIn(goMod, "github.com/dal-go/dalgo2sql"); !ok || got != "v0.26.5" {
		t.Errorf("dalgo2sql = %q, %v; want v0.26.5 from the block", got, ok)
	}
	if got, ok := moduleVersionIn(goMod, "github.com/dal-go/record"); ok {
		t.Errorf("record = %q, want no requirement", got)
	}
	if got, ok := moduleVersionIn([]byte("github.com/dal-go/dalgo\n"), "github.com/dal-go/dalgo"); ok {
		t.Errorf("a line with no version = %q, want no requirement", got)
	}
}

func TestVendoredFixturesAreTheDalgoModulesAtTheRequiredVersion(t *testing.T) {
	// Not a server test, and it runs wherever the module cache holds dalgo: the vendored
	// fixtures are those of the dalgo this package is built with, and they are as their
	// manifests say.
	version := requiredDalgoVersion(t)
	upstream := dalgoModuleDir(t)
	for _, name := range []string{"joins", "subqueries"} {
		dir := filepath.Join("testdata", name)
		for _, problem := range vendoredProblems(dir, dalgoModule, version) {
			t.Error(problem)
		}
		manifest, err := readFixtureManifest(dir)
		if err != nil {
			t.Fatal(err)
		}
		if want := "dtql/testdata/" + name; manifest.SourcePath != want || manifest.SourceCommit == "" {
			t.Errorf("%s: sourcePath %q and sourceCommit %q, want %q and a commit", dir, manifest.SourcePath, manifest.SourceCommit, want)
		}
		for _, problem := range upstreamProblems(dir, filepath.Join(upstream, filepath.FromSlash(manifest.SourcePath))) {
			t.Error(problem)
		}
	}
}

func TestNormalizeRowsAndSameCell(t *testing.T) {
	rows, err := normalizeRows([]map[string]any{{"n": int64(2), "x": 1.5, "s": "a", "none": nil, "ok": true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"n": 2.0, "x": 1.5, "s": "a", "none": nil, "ok": true}}
	if d := rowDifference(rows, want); d != "" {
		t.Errorf("normalized rows differ: %s", d)
	}
	if _, err := normalizeRows([]map[string]any{{"bad": math.NaN()}}); err == nil {
		t.Error("a NaN was normalized, want the JSON error")
	}
	if _, err := normalizeRows([]map[string]any{{"bad": make(chan int)}}); err == nil {
		t.Error("a channel was normalized, want the JSON error")
	}
	for _, tc := range []struct {
		name      string
		got, want any
		same      bool
	}{
		{"equal numbers", 1.0, 1.0, true},
		{"numbers within the tolerance", 100000000.00000001, 100000000.0, true},
		{"small numbers within the tolerance", 0.1 + 0.2, 0.3, true},
		{"numbers beyond the tolerance", 1.0, 1.000001, false},
		{"a number against text", "1", 1.0, false},
		{"equal texts", "a", "a", true},
		{"different texts", "a", "b", false},
		{"a NULL against zero", nil, 0.0, false},
		{"a NULL against a NULL", nil, nil, true},
		{"equal lists", []any{1.0, "a"}, []any{1.0, "a"}, true},
		{"lists of another length", []any{1.0}, []any{1.0, 2.0}, false},
		{"lists with another element", []any{1.0, "a"}, []any{1.0, "b"}, false},
		{"a list against a number", 1.0, []any{1.0}, false},
		{"equal maps", map[string]any{"a": 1.0}, map[string]any{"a": 1.0}, true},
		{"maps with another key", map[string]any{"b": 1.0}, map[string]any{"a": 1.0}, false},
		{"maps of another size", map[string]any{"a": 1.0, "b": 2.0}, map[string]any{"a": 1.0}, false},
		{"maps with another value", map[string]any{"a": 2.0}, map[string]any{"a": 1.0}, false},
		{"a map against a number", 1.0, map[string]any{"a": 1.0}, false},
	} {
		if got := sameCell(tc.got, tc.want); got != tc.same {
			t.Errorf("%s: sameCell(%v, %v) = %v, want %v", tc.name, tc.got, tc.want, got, tc.same)
		}
	}
}

func TestRowDifference(t *testing.T) {
	a := []map[string]any{{"id": 1.0, "name": "x"}, {"id": 2.0, "name": nil}}
	if d := rowDifference(a, a); d != "" {
		t.Errorf("the same rows differ: %s", d)
	}
	if d := rowDifference(a[:1], a); d != "1 rows, want 2" {
		t.Errorf("difference = %q, want the row counts", d)
	}
	other := []map[string]any{{"id": 1.0, "name": "x"}, {"id": 2.0, "name": "y"}}
	if d := rowDifference(other, a); !strings.Contains(d, `row 1 is {"id":2,"name":"y"}, want {"id":2,"name":null}`) {
		t.Errorf("difference = %q, want the row that differs", d)
	}
	// A value JSON cannot hold is still written, for a message.
	if got := compact(math.NaN()); got != "NaN" {
		t.Errorf("compact(NaN) = %q, want NaN", got)
	}
}

func TestParseDivergences(t *testing.T) {
	valid := `{"schemaVersion":1,"divergences":[{"suite":"joins","case":"a","differs":["rows"],"reason":"because"}]}`
	listed, err := parseDivergences([]byte(valid))
	if err != nil || len(listed) != 1 || listed["joins/a"].Reason != "because" {
		t.Fatalf("parse = %v, %v; want the one entry", listed, err)
	}
	for name, doc := range map[string]string{
		"not JSON":        `{`,
		"another version": `{"schemaVersion":2,"divergences":[]}`,
		"no suite":        `{"schemaVersion":1,"divergences":[{"case":"a","differs":["rows"],"reason":"r"}]}`,
		"no case":         `{"schemaVersion":1,"divergences":[{"suite":"s","differs":["rows"],"reason":"r"}]}`,
		"no reason":       `{"schemaVersion":1,"divergences":[{"suite":"s","case":"a","differs":["rows"],"reason":" "}]}`,
		"no kind":         `{"schemaVersion":1,"divergences":[{"suite":"s","case":"a","differs":[],"reason":"r"}]}`,
		"an unknown kind": `{"schemaVersion":1,"divergences":[{"suite":"s","case":"a","differs":["speed"],"reason":"r"}]}`,
		"a case twice": `{"schemaVersion":1,"divergences":[` +
			`{"suite":"s","case":"a","differs":["rows"],"reason":"r"},{"suite":"s","case":"a","differs":["error"],"reason":"r"}]}`,
	} {
		if _, err := parseDivergences([]byte(doc)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

func TestDivergenceFileOfThisRepositoryParses(t *testing.T) {
	listed := loadDivergences(t)
	for key, entry := range listed {
		if len(entry.Reason) < 20 {
			t.Errorf("%s: reason %q is too short to explain a divergence", key, entry.Reason)
		}
	}
}

func TestDivergenceVerdict(t *testing.T) {
	listed := &divergence{Suite: "joins", Case: "a", Differs: []string{"rows", "statements"}, Reason: "the reason"}
	for _, tc := range []struct {
		name     string
		observed []string
		listed   *divergence
		want     string // a fragment of the failure, "" for none
	}{
		{"equal and not listed", nil, nil, ""},
		{"different and not listed", []string{"rows"}, nil, "is not in testdata/postgres-divergences.json"},
		{"equal and listed", nil, listed, "no longer differs from its fixture: delete its entry"},
		{"different as listed, in any order", []string{"statements", "rows"}, listed, ""},
		{"different in other kinds than listed", []string{"rows"}, listed, "update its entry"},
	} {
		got := divergenceVerdict("joins/a", tc.observed, tc.listed)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: verdict = %q, want one that says %q", tc.name, got, tc.want)
		}
	}
	// The caller's slice is not reordered.
	observed := []string{"statements", "rows"}
	divergenceVerdict("joins/a", observed, listed)
	if observed[0] != "statements" {
		t.Errorf("observed = %v, want the caller's order kept", observed)
	}
}

func TestUnusedDivergences(t *testing.T) {
	listed := map[string]divergence{
		"joins/a": {Suite: "joins", Case: "a"},
		"joins/b": {Suite: "joins", Case: "b"},
		"subs/c":  {Suite: "subs", Case: "c"},
	}
	got := unusedDivergences(listed, "joins", map[string]bool{"a": true})
	if len(got) != 1 || got[0] != "joins/b" {
		t.Errorf("unused = %v, want [joins/b]: the other suite's entry is not this suite's", got)
	}
}
