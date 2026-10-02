package chatgptauth

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

// thisPackage is the sign-in's import path.
const thisPackage = "github.com/charliek/craze/internal/chatgptauth"

// craze-internal dependencies the sign-in may have (plan 033 §3.10, P31):
// internal/atomicfile alone, so the CLI, the TUI and the agent adapter can
// all build on it, and it on none of them.
var allowedInternal = []string{thisPackage, "github.com/charliek/craze/internal/atomicfile"}

// TestImportsOnlyTheStandardLibraryAndAtomicfile: the package's whole
// dependency closure (tests excluded) is the standard library and
// internal/atomicfile — no Fantasy, no openai-go, no harness. Guards against
// a vacuous pass: the listing reaches the package itself, atomicfile and
// net/http, and the same rule run over package llm, which imports Fantasy,
// fails (the control).
func TestImportsOnlyTheStandardLibraryAndAtomicfile(t *testing.T) {
	deps := goListDeps(t, "./internal/chatgptauth")
	for _, want := range []string{thisPackage, "github.com/charliek/craze/internal/atomicfile", "net/http"} {
		if !slices.Contains(deps, want) {
			t.Fatalf("go list -deps does not list %s; the check is not seeing the package", want)
		}
	}
	if bad := beyondTheRule(t, deps); len(bad) > 0 {
		t.Fatalf("internal/chatgptauth depends on %s; it may import the standard library and internal/atomicfile only (plan 033 §3.10)", strings.Join(bad, ", "))
	}
	if bad := beyondTheRule(t, goListDeps(t, "./internal/harness/llm")); !slices.Contains(bad, "charm.land/fantasy") {
		t.Fatalf("the control found %v beyond the rule in package llm, want Fantasy among them", bad)
	}
}

// beyondTheRule is every package of deps that is neither the standard
// library's nor allowed.
func beyondTheRule(t *testing.T, deps []string) []string {
	t.Helper()
	std := map[string]bool{}
	for _, p := range goListStd(t) {
		std[p] = true
	}
	var out []string
	for _, p := range deps {
		if !std[p] && !slices.Contains(allowedInternal, p) && !strings.HasPrefix(p, "vendor/") {
			out = append(out, p)
		}
	}
	return out
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

func goListDeps(t *testing.T, pkg string) []string {
	return goList(t, "-deps", "-f", "{{.ImportPath}}", pkg)
}

func goListStd(t *testing.T) []string {
	return goList(t, "-f", "{{.ImportPath}}", "std")
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
