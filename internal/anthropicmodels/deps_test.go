package anthropicmodels

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// thisPackage is the model-list cache's import path.
const thisPackage = "github.com/charliek/craze/internal/anthropicmodels"

// allowedInternal are the craze packages the cache may import (plan 044
// §3.6): the Messages API client it fetches with, the model table whose file
// it writes, and the atomic write and lock. Nothing else of craze's — not
// the harness, whose files it writes from outside, not package llm and its
// Fantasy, nor the agent, the CLI or the TUI that call it.
var allowedInternal = []string{
	"github.com/charliek/craze/internal/harness/llm/messagesapi",
	"github.com/charliek/craze/internal/harness/modeltable",
	"github.com/charliek/craze/internal/atomicfile",
}

// forbiddenDeps are what nothing it depends on, directly or not, may be:
// Fantasy, the SDKs (owner decision 2), and package llm itself.
var forbiddenDeps = []string{
	"charm.land/fantasy",
	"github.com/openai/openai-go",
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/charliek/craze/internal/harness/llm",
}

// beyondTheRule is every craze package pkg imports directly that the rule
// does not allow, and every package of its dependency closure the rule
// forbids.
func beyondTheRule(t *testing.T, pkg string) []string {
	t.Helper()
	var out []string
	for _, p := range goList(t, "-f", `{{join .Imports "\n"}}`, pkg) {
		if strings.HasPrefix(p, "github.com/charliek/craze/") && !slices.Contains(allowedInternal, p) {
			out = append(out, p)
		}
	}
	for _, p := range goList(t, "-deps", "-f", "{{.ImportPath}}", pkg) {
		if slices.Contains(forbiddenDeps, p) {
			out = append(out, p)
		}
	}
	return out
}

// TestImportsOnlyTheClientTheTableAndAtomicfile: the cache imports, of
// craze, the Messages API client, the model table and internal/atomicfile
// alone, and depends on no Fantasy, SDK or package llm. Guards against a
// vacuous pass: the listing sees the package's own imports, all three, and
// the same rule run over internal/agent, which imports the harness and llm,
// fails (the control).
func TestImportsOnlyTheClientTheTableAndAtomicfile(t *testing.T) {
	imports := goList(t, "-f", `{{join .Imports "\n"}}`, "./internal/anthropicmodels")
	for _, want := range allowedInternal {
		if !slices.Contains(imports, want) {
			t.Fatalf("go list does not list %s among the package's imports %q; the check is not seeing them", want, imports)
		}
	}
	if bad := beyondTheRule(t, "./internal/anthropicmodels"); len(bad) > 0 {
		t.Fatalf("%s depends on %s; it may import, of craze, %s only (plan 044 §3.6)", thisPackage, strings.Join(bad, ", "), strings.Join(allowedInternal, ", "))
	}
	if bad := beyondTheRule(t, "./internal/agent"); !slices.Contains(bad, "github.com/charliek/craze/internal/harness") || !slices.Contains(bad, "charm.land/fantasy") {
		t.Fatalf("the control found %q beyond the rule in internal/agent, want the harness and Fantasy among them", bad)
	}
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command on PATH to list dependencies with: %v", err)
	}
	cmd := exec.Command(gobin, append([]string{"list"}, args...)...)
	cmd.Dir = moduleRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v\n%s", args, err, stderr.String())
	}
	return strings.Fields(string(out))
}

// moduleRoot walks up from the test's directory to the one holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}
