package tool

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The tool framework's boundary (plan 019 §3.1): no Fantasy, even
// transitively, and nothing of craze but internal/atomicfile, the redactor
// and the framework's own packages. depguard checks direct imports only;
// this checks the whole closure. It also keeps text/template out (D-37).
const module = "github.com/charliek/craze/"

// toolAllowed are the craze packages the tool framework's closure may hold;
// a pattern ending in "/..." also covers everything under it.
var toolAllowed = []string{
	module + "internal/atomicfile",
	module + "internal/harness/redact",
	module + "internal/harness/tool/...",
}

// forbidden reports the packages in deps that break the boundary: any
// Fantasy package, text/template or html/template, and any craze package
// not matched by allowed.
func forbidden(deps, allowed []string) []string {
	var bad []string
	for _, pkg := range deps {
		switch {
		case pkg == "charm.land/fantasy" || strings.HasPrefix(pkg, "charm.land/fantasy/"):
			bad = append(bad, pkg)
		case pkg == "text/template" || pkg == "html/template":
			bad = append(bad, pkg)
		case strings.HasPrefix(pkg, module) && !slices.ContainsFunc(allowed, func(a string) bool {
			if root, ok := strings.CutSuffix(a, "/..."); ok {
				return pkg == root || strings.HasPrefix(pkg, root+"/")
			}
			return pkg == a
		}):
			bad = append(bad, pkg)
		}
	}
	return bad
}

// TestForbiddenSeesViolations is the negative control for the tests below:
// the checker reports each kind of violation, and only violations.
func TestForbiddenSeesViolations(t *testing.T) {
	deps := []string{
		"context", "encoding/json",
		module + "internal/atomicfile",
		module + "internal/harness/redact",
		module + "internal/harness/tool",
		module + "internal/harness/tool/opencode",
		"charm.land/fantasy",
		"charm.land/fantasy/schema",
		"text/template",
		module + "internal/harness/modeltable",
		module + "internal/harness/toolkit", // a prefix of tool, not under it
		module + "internal/agent",
	}
	want := []string{
		"charm.land/fantasy", "charm.land/fantasy/schema", "text/template",
		module + "internal/harness/modeltable", module + "internal/harness/toolkit", module + "internal/agent",
	}
	if got := forbidden(deps, toolAllowed); !slices.Equal(got, want) {
		t.Fatalf("forbidden =\n%q\nwant\n%q", got, want)
	}
	if got := forbidden([]string{module + "internal/harness/redact"}, nil); len(got) != 1 {
		t.Fatalf("with nothing allowed, redact itself is not reported: %q", got)
	}
}

func TestToolFrameworkLinksNoFantasyAndLittleCraze(t *testing.T) {
	deps := listDeps(t, module+"internal/harness/tool/...", module+"internal/harness/redact")
	// Guard against a vacuous pass: the listing must see both packages.
	for _, pkg := range []string{module + "internal/harness/tool", module + "internal/harness/redact"} {
		if !slices.Contains(deps, pkg) {
			t.Fatalf("go list -deps does not include %s; the check is not seeing the framework", pkg)
		}
	}
	if bad := forbidden(deps, toolAllowed); len(bad) > 0 {
		t.Fatalf("the tool framework's dependencies break its boundary: %s", strings.Join(bad, ", "))
	}
}

// TestRedactIsALeaf: the redactor sits under everything and imports
// nothing of craze at all.
func TestRedactIsALeaf(t *testing.T) {
	deps := listDeps(t, module+"internal/harness/redact")
	if !slices.Contains(deps, module+"internal/harness/redact") {
		t.Fatal("go list -deps does not include the redact package")
	}
	var others []string
	for _, pkg := range deps {
		if pkg != module+"internal/harness/redact" {
			others = append(others, pkg)
		}
	}
	if bad := forbidden(others, nil); len(bad) > 0 {
		t.Fatalf("redact depends on %s; it must import only the standard library", strings.Join(bad, ", "))
	}
	for _, pkg := range others {
		if strings.Contains(strings.SplitN(pkg, "/", 2)[0], ".") {
			t.Fatalf("redact depends on %s, outside the standard library", pkg)
		}
	}
}

// listDeps runs go list -deps on import-path patterns, which resolve from the
// test's own directory inside the module, so no module root is needed.
func listDeps(t *testing.T, patterns ...string) []string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command on PATH to list dependencies with: %v", err)
	}
	cmd := exec.Command(gobin, append([]string{"list", "-deps", "-f", "{{.ImportPath}}"}, patterns...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", strings.Join(patterns, " "), err, stderr.String())
	}
	return strings.Fields(string(out))
}
