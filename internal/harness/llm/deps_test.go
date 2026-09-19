package llm

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

// sdkFreePackages are the package patterns, relative to the module root,
// whose dependencies must not include a provider SDK (owner decision 2:
// Fantasy is a plain import, but only its OpenAI-compatible provider is
// linked). The binary joins the list when it first links the harness.
var sdkFreePackages = []string{
	"./internal/harness/...",
}

// forbiddenSDKs are import-path fragments of the SDKs Fantasy's other
// providers pull in. Importing Fantasy's openrouter package alone brings all
// three, through its anthropic and google packages.
var forbiddenSDKs = []string{
	"aws-sdk-go-v2",
	"google.golang.org/genai",
	"anthropic-sdk-go",
}

func TestNoProviderSDKsLinked(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command on PATH to list dependencies with: %v", err)
	}
	cmd := exec.Command(gobin, append([]string{"list", "-deps", "-f", "{{.ImportPath}}"}, sdkFreePackages...)...)
	cmd.Dir = moduleRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", strings.Join(sdkFreePackages, " "), err, stderr.String())
	}
	deps := strings.Fields(string(out))
	// Guard against a vacuous pass: the listing must reach the provider this
	// package does link.
	if !slices.Contains(deps, "charm.land/fantasy/providers/openaicompat") {
		t.Fatalf("go list -deps %s does not include Fantasy's openaicompat provider; the check is not seeing the harness",
			strings.Join(sdkFreePackages, " "))
	}
	var linked []string
	for _, pkg := range deps {
		for _, sdk := range forbiddenSDKs {
			if strings.Contains(pkg, sdk) {
				linked = append(linked, pkg)
			}
		}
	}
	if len(linked) > 0 {
		t.Fatalf("%d provider SDK packages are dependencies of %s, e.g. %s; import only Fantasy's openaicompat provider",
			len(linked), strings.Join(sdkFreePackages, " "), strings.Join(linked[:min(5, len(linked))], ", "))
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
