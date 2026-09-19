package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reflectMethodLookup matches the symbols that look a method up at run time.
// While any of them is linked, the Go linker cannot tell which methods are
// called, so it keeps every exported method of every linked type. With the
// native harness linked that is the difference between a 19 MB and a 32 MB
// binary, most of it openai-go's generated API surface.
var reflectMethodLookup = regexp.MustCompile(`^reflect\.(\(\*?\w+\)|\w+)\.(Method|MethodByName)$`)

// TestNoReflectiveMethodLookupLinked links the real binary and fails when a
// run-time method lookup is reachable from it. The usual way in is
// text/template: cobra reaches its executor only through SetUsageTemplate,
// SetHelpTemplate, SetVersionTemplate and AddTemplateFunc, which is why craze
// prints --version itself (internal/cli/root.go).
func TestNoReflectiveMethodLookupLinked(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go command on PATH to link the binary with: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "craze")
	if out, err := exec.Command(gobin, "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	var stderr bytes.Buffer
	nm := exec.Command(gobin, "tool", "nm", bin)
	nm.Stderr = &stderr
	out, err := nm.Output()
	if err != nil {
		t.Fatalf("go tool nm: %v\n%s", err, stderr.String())
	}

	var found []string
	sawHarness := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sym := fields[len(fields)-1]
		if strings.HasPrefix(sym, "charm.land/fantasy/providers/openaicompat.") {
			sawHarness = true
		}
		if reflectMethodLookup.MatchString(sym) {
			found = append(found, sym)
		}
	}
	// Guard against a vacuous pass: the symbol table must be readable and must
	// be the binary that links the harness.
	if !sawHarness {
		t.Fatal("no Fantasy openaicompat symbol in the linked binary; the check is not seeing the harness")
	}
	if len(found) > 0 {
		t.Fatalf("run-time method lookup is linked (%s), so the linker keeps every exported method of every type; "+
			"look for a cobra Set*Template or AddTemplateFunc call, or another text/template or html/template user",
			strings.Join(found, ", "))
	}
}
