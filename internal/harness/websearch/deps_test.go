package websearch

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

// thisPackage is the web search service's import path.
const thisPackage = "github.com/charliek/craze/internal/harness/websearch"

// allowedInternal are the craze packages the service may import (plan 051
// §B, S8, R2): the tool framework whose seam (tool.Web) it implements, the
// Messages API client for its key pattern (messagesapi.KeyPattern, whose own
// closure messagesapi/deps_test.go pins), the redactor for its marker, and
// the atomic write and lock its remembered choice will be written with.
// Nothing else of craze's — not the parent harness, whose sessions call it,
// not package llm and its Fantasy, nor the model table, the agent, the CLI
// or the TUI.
var allowedInternal = []string{
	"github.com/charliek/craze/internal/harness/tool",
	"github.com/charliek/craze/internal/harness/llm/messagesapi",
	"github.com/charliek/craze/internal/harness/redact",
	"github.com/charliek/craze/internal/atomicfile",
}

// imported are the allowed packages the service already imports: what the
// listing must see for the check not to pass vacuously. atomicfile joins
// them with the remembered choice (plan 051 §C).
var imported = []string{
	"github.com/charliek/craze/internal/harness/tool",
	"github.com/charliek/craze/internal/harness/llm/messagesapi",
	"github.com/charliek/craze/internal/harness/redact",
}

// forbiddenDeps are what nothing it depends on, directly or not, may be:
// Fantasy, the SDKs, package llm itself, and the harness above it.
var forbiddenDeps = []string{
	"charm.land/fantasy",
	"github.com/openai/openai-go",
	// Charm's fork of the OpenAI SDK, which Fantasy v0.45 and later build on.
	"github.com/charmbracelet/openai-go",
	"github.com/anthropics/anthropic-sdk-go",
	llmPackage,
	harnessPackage,
}

// llmPackage is package llm's import path, and harnessPackage the harness's:
// each forbidden itself, while a subpackage — messagesapi, tool, redact — may
// be allowed (allowedInternal).
const (
	llmPackage     = "github.com/charliek/craze/internal/harness/llm"
	harnessPackage = "github.com/charliek/craze/internal/harness"
)

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
		if forbiddenDep(p) {
			out = append(out, p)
		}
	}
	return out
}

// forbiddenDep says p is one of forbiddenDeps or a subpackage of one (an
// SDK's option package, say), except under package llm and the harness,
// whose allowed subpackages are matched exactly.
func forbiddenDep(p string) bool {
	for _, f := range forbiddenDeps {
		if p == f || (f != llmPackage && f != harnessPackage && strings.HasPrefix(p, f+"/")) {
			return true
		}
	}
	return false
}

// TestImportsOnlyTheSeamTheKeyPatternAndTheMarker (plan 051 §B, S8): the
// service imports, of craze, only allowedInternal, and depends on no Fantasy,
// SDK, package llm or the harness itself. Guards against a vacuous pass: the
// listing sees the package's own imports, and the same rule run over
// internal/agent, which imports the harness and Fantasy, fails (the control).
func TestImportsOnlyTheSeamTheKeyPatternAndTheMarker(t *testing.T) {
	imports := goList(t, "-f", `{{join .Imports "\n"}}`, "./internal/harness/websearch")
	for _, want := range imported {
		if !slices.Contains(imports, want) {
			t.Fatalf("go list does not list %s among the package's imports %q; the check is not seeing them", want, imports)
		}
	}
	if bad := beyondTheRule(t, "./internal/harness/websearch"); len(bad) > 0 {
		t.Fatalf("%s depends on %s; it may import, of craze, %s only (plan 051 §B)", thisPackage, strings.Join(bad, ", "), strings.Join(allowedInternal, ", "))
	}
	if bad := beyondTheRule(t, "./internal/agent"); !slices.Contains(bad, harnessPackage) || !slices.Contains(bad, "charm.land/fantasy") {
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
