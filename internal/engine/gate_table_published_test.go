package engine_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updatePublishedGateTable rewrites docs/reference/protocol.md's gate table
// section from gateTableMarkdown() instead of failing (plan 027 C10, SF-13):
// "go test ./internal/engine/... -run TestPublishedGateTableIsTheTested -update".
var updatePublishedGateTable = flag.Bool("update", false, "rewrite docs/reference/protocol.md's gate table from the tested one")

const (
	gateTableBeginMarker = "<!-- gate-table:begin -->"
	gateTableEndMarker   = "<!-- gate-table:end -->"
)

// gateTableModuleRoot finds go.mod upward from the test's working directory
// (internal/paths/paths_repo_test.go's pattern, duplicated rather than
// shared: engine's own depguard rule already keeps this package narrow, and
// four lines is not worth a new import).
func gateTableModuleRoot(t *testing.T) string {
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

// betweenGateTableMarkers is the text strictly between the begin and end
// marker lines of doc (each marker on its own line), trimmed of its
// surrounding blank lines, or an error naming what is wrong with the
// markers.
func betweenGateTableMarkers(doc string) (string, error) {
	begin := strings.Index(doc, gateTableBeginMarker)
	if begin < 0 {
		return "", errBeginMarkerMissing
	}
	begin += len(gateTableBeginMarker)
	end := strings.Index(doc[begin:], gateTableEndMarker)
	if end < 0 {
		return "", errEndMarkerMissing
	}
	return strings.Trim(doc[begin:begin+end], "\n"), nil
}

var (
	errBeginMarkerMissing = errMarker(gateTableBeginMarker + " not found")
	errEndMarkerMissing   = errMarker(gateTableEndMarker + " not found")
)

type errMarker string

func (e errMarker) Error() string { return string(e) }

// replaceBetweenGateTableMarkers rewrites doc's section between the markers
// to table (with one blank-line-free newline on each side, matching what
// betweenGateTableMarkers trims).
func replaceBetweenGateTableMarkers(doc, table string) (string, error) {
	begin := strings.Index(doc, gateTableBeginMarker)
	if begin < 0 {
		return "", errBeginMarkerMissing
	}
	begin += len(gateTableBeginMarker)
	rest := doc[begin:]
	end := strings.Index(rest, gateTableEndMarker)
	if end < 0 {
		return "", errEndMarkerMissing
	}
	return doc[:begin] + "\n" + strings.TrimSuffix(table, "\n") + "\n" + rest[end:], nil
}

// TestPublishedGateTableIsTheTested is plan 027 C10 / A13: the markdown
// table published in docs/reference/protocol.md, between its gate-table
// markers, must be exactly gateTableMarkdown()'s render of the SAME table
// TestTheGateTableIsTheEngines drives through the real engine — so the
// published table can never drift from what the engine actually does. On
// failure it names the fix (-update) rather than a diff, since the whole
// published section is meant to be replaced wholesale, never hand-edited.
func TestPublishedGateTableIsTheTested(t *testing.T) {
	path := filepath.Join(gateTableModuleRoot(t), "docs", "reference", "protocol.md")
	want := strings.TrimSuffix(gateTableMarkdown(), "\n")

	if *updatePublishedGateTable {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		updated, err := replaceBetweenGateTableMarkers(string(b), want)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	got, err := betweenGateTableMarkers(string(b))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if got != want {
		t.Fatalf("docs/reference/protocol.md's gate table has drifted from the engine's own "+
			"(TestTheGateTableIsTheEngines); regenerate it:\n"+
			"  go test ./internal/engine/... -run TestPublishedGateTableIsTheTested -update\n\n"+
			"got:\n%s\n\nwant:\n%s", got, want)
	}
}
