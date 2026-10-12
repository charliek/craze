package agent

import (
	"fmt"
	"os"
	"testing"

	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// TestMain unsets every variable the shipped model catalog takes a key from
// (plan 031 §3.13). The catalog brings those names into every native table
// Start loads, and a native session reads the process environment wherever a
// test leaves the harness's Getenv unset: a key exported in the developer's
// shell would fund, change the model list of, and be redacted in a session
// that should see none. A test that wants one sets it itself (t.Setenv).
func TestMain(m *testing.M) {
	names := modeltable.CatalogEnvNames()
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "FAIL: the shipped model catalog names no variables to unset")
		os.Exit(1)
	}
	for _, name := range names {
		_ = os.Unsetenv(name)
	}
	// Anthropic's API (plan 044 P30) is aimed at a loopback port nothing
	// serves, for the package: a native session fetches the account's model
	// list as it opens whenever a key funds the Anthropic provider, and a
	// test that funds it without starting a fake of its own (t.Setenv over
	// this) gets a refused connection on this machine instead of reaching
	// Anthropic.
	_ = os.Setenv(messagesapi.TestAPIEnv, "http://127.0.0.1:9")
	os.Exit(m.Run())
}
