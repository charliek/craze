package protocol_test

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/craze/internal/protocol"
)

// updatePublished re-copies docs/reference/protocol/schema/ from the
// embedded schema (plan 027 C10, SD-14): "go test ./internal/protocol/...
// -run TestPublishedSchemaIsTheEmbedded -update".
var updatePublished = flag.Bool("update", false, "rewrite docs/reference/protocol/schema/*.json from the embedded schema")

// moduleRoot finds go.mod upward from the test's working directory, which go
// test sets to this package's directory (internal/paths/paths_repo_test.go's
// pattern, duplicated here rather than shared: it is four lines, and this
// package imports nothing of craze's but the standard library and
// internal/protocol itself, doc.go's import-boundary rule).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// TestPublishedSchemaIsTheEmbedded is plan 027 C10 / A10: the copy under
// docs/reference/protocol/schema/ — what Zensical serves at SchemaBaseURI —
// must be byte for byte the embedded schema (protocol.SchemaFS), so the
// published spec can never drift from what the code actually checks fixtures
// and instances against. -update rewrites the published copy from the
// embedded one instead of failing.
func TestPublishedSchemaIsTheEmbedded(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "docs", "reference", "protocol", "schema")
	names := protocol.SchemaNames()

	if *updatePublished {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			b, err := protocol.SchemaFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	published, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v (run with -update to create it)", dir, err)
	}
	var publishedNames []string
	for _, e := range published {
		if !e.IsDir() {
			publishedNames = append(publishedNames, e.Name())
		}
	}
	if diff := diffStrings(names, publishedNames); diff != "" {
		t.Fatalf("docs/reference/protocol/schema/ does not hold exactly the embedded schema's files (run with -update):\n%s", diff)
	}

	for _, name := range names {
		embedded, err := protocol.SchemaFile(name)
		if err != nil {
			t.Fatal(err)
		}
		onDisk, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading published %s: %v (run with -update)", name, err)
		}
		if string(onDisk) != string(embedded) {
			t.Errorf("docs/reference/protocol/schema/%s differs from the embedded schema (run with -update)", name)
		}
	}
}

// diffStrings is "" when got and want hold the same set of names, else a
// two-line report of what is missing and what is extra.
func diffStrings(want, got []string) string {
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	var missing, extra []string
	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !wantSet[g] {
			extra = append(extra, g)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	s := ""
	if len(missing) > 0 {
		s += "missing: " + join(missing) + "\n"
	}
	if len(extra) > 0 {
		s += "extra: " + join(extra) + "\n"
	}
	return s
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// TestPublishedSchemaFSHasNoSubdirectories guards the assumption
// TestPublishedSchemaIsTheEmbedded makes: every embedded schema file sits
// directly under schema/, none nested.
func TestPublishedSchemaFSHasNoSubdirectories(t *testing.T) {
	err := fs.WalkDir(protocol.SchemaFS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." {
			t.Fatalf("unexpected subdirectory in the embedded schema: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
