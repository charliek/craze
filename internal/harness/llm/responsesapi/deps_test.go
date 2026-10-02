package responsesapi

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

// thisPackage is the driver core's import path.
const thisPackage = "github.com/charliek/craze/internal/harness/llm/responsesapi"

// forbiddenDeps are what the core must not depend on, directly or through
// anything it imports (owner decision 14, plan 033 §3.9): Fantasy, the
// OpenAI SDK, and package llm, which brings both — so leaving Fantasy later
// rewrites the adapter (llm/responses_adapter.go) and nothing here.
var forbiddenDeps = []string{
	"charm.land/fantasy",
	"github.com/openai/openai-go",
	"github.com/charliek/craze/internal/harness/llm",
}

// forbidden is every package in deps that forbiddenDeps names: itself, or
// one of its subpackages (the core is a subpackage of llm, and is no
// violation of it).
func forbidden(deps []string) []string {
	var out []string
	for _, pkg := range deps {
		if pkg == thisPackage {
			continue
		}
		for _, f := range forbiddenDeps {
			if pkg == f || strings.HasPrefix(pkg, f+"/") {
				out = append(out, pkg)
			}
		}
	}
	return out
}

func listDeps(t *testing.T, pkg string) []string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command on PATH to list dependencies with: %v", err)
	}
	cmd := exec.Command(gobin, "list", "-deps", "-f", "{{.ImportPath}}", pkg)
	cmd.Dir = moduleRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, stderr.String())
	}
	return strings.Fields(string(out))
}

// TestCoreImportsNeitherFantasyNorTheSDK: the core's whole dependency
// closure holds no Fantasy, no openai-go and no package llm. Guards against
// a vacuous pass: the listing reaches the core itself and net/http, and the
// same check run over package llm — the adapter's side, which imports
// Fantasy — does find them (the control).
func TestCoreImportsNeitherFantasyNorTheSDK(t *testing.T) {
	deps := listDeps(t, "./internal/harness/llm/responsesapi")
	if !slices.Contains(deps, thisPackage) || !slices.Contains(deps, "net/http") {
		t.Fatalf("go list -deps of the core lists neither it nor net/http; the check is not seeing it")
	}
	if bad := forbidden(deps); len(bad) > 0 {
		t.Fatalf("the driver core depends on %s; it must import neither Fantasy nor openai-go (owner decision 14)", strings.Join(bad, ", "))
	}
	if bad := forbidden(listDeps(t, "./internal/harness/llm")); !slices.Contains(bad, "charm.land/fantasy") {
		t.Fatalf("the control found %v in package llm's dependencies, want Fantasy among them", bad)
	}
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
