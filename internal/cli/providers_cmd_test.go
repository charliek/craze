package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// craze providers (plan 036 §3.2, A2). Every case runs over a scratch PATH,
// HOME and CRAZE_HOME (t.Setenv, so none is parallel), with no
// CRAZE_PROVIDER or CRAZE_AGENT_BIN, and TestMain's login session (the
// GUI's): what the command says is the scratch machine's, never the owner's.

// providersMachine is the scratch machine the cases run on: grok on PATH and
// nothing else, an empty craze directory (no native key), and its
// config.toml's path, which cursor's fix names.
func providersMachine(t *testing.T) (config string) {
	t.Helper()
	availPath(t, "grok")
	isolateHome(t)
	home := crazeHome(t)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv(envAgentBin, "")
	return filepath.Join(home, "config.toml")
}

// TestProvidersTable (A2): the human table — a header, then one row per
// listed provider in the registry's order, its id, its state in words, and
// its reason and fix, "-" for a ready one's — and exit 0 although cursor is
// unavailable. gx, not installed, is not listed; nothing goes to stderr.
func TestProvidersTable(t *testing.T) {
	config := providersMachine(t)
	stdout, stderr, code := executeErr([]string{"providers"})
	if code != 0 {
		t.Fatalf("craze providers: exit %d %q", code, stderr)
	}
	row := func(cells ...string) string {
		return fmt.Sprintf("%-10s%-13s%-32s%s\n", cells[0], cells[1], cells[2], cells[3])
	}
	want := row("PROVIDER", "STATE", "REASON", "FIX") +
		row("cursor", "unavailable", "cursor-agent not found on PATH", "install cursor-agent, or set [agents].cursor in "+config) +
		row("grok", "ready", "-", "-") +
		row("native", "needs setup", "no model provider has a key", nativeNoKeyFix)
	if stdout != want {
		t.Fatalf("craze providers:\n%s\nwant:\n%s", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr: %q", stderr)
	}
}

// TestProvidersJSON (A2): --json is one line, {"providers":[…]}, each entry
// the wire's providerOption — id, label, state in the wire's words, and a
// reason and a fix that are absent, not empty, when it is ready — and exit 0.
func TestProvidersJSON(t *testing.T) {
	config := providersMachine(t)
	stdout, stderr, code := executeErr([]string{"providers", "--json"})
	if code != 0 {
		t.Fatalf("craze providers --json: exit %d %q", code, stderr)
	}
	q := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	want := `{"providers":[` +
		`{"id":"cursor","label":"cursor","state":"unavailable","reason":"cursor-agent not found on PATH","fix":` +
		q("install cursor-agent, or set [agents].cursor in "+config) + `},` +
		`{"id":"grok","label":"grok","state":"ready"},` +
		`{"id":"native","label":"native","state":"needs_setup","reason":"no model provider has a key","fix":` +
		q(nativeNoKeyFix) + `}]}` + "\n"
	if stdout != want {
		t.Fatalf("craze providers --json:\n%s\nwant:\n%s", stdout, want)
	}
}

// TestProvidersUsageErrors (A2): an argument, or a flag it does not take, is
// a usage error, exit 2, and prints nothing.
func TestProvidersUsageErrors(t *testing.T) {
	providersMachine(t)
	for _, argv := range [][]string{{"providers", "cursor"}, {"providers", "--hub-of-nothing"}} {
		stdout, stderr, code := executeErr(argv)
		if code != 2 {
			t.Fatalf("craze %q: exit %d %q, want 2", argv, code, stderr)
		}
		if stdout != "" {
			t.Fatalf("craze %q printed %q", argv, stdout)
		}
	}
}
