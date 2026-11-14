package agent

import (
	"fmt"
	"os"
	"testing"

	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/websearch"
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
	// The web search (plan 051 §B): its key is not a catalog name, so it is
	// unset by name, and its endpoint is aimed at the same dead port, so a
	// session offered the web tools whose test reads the process environment
	// gets a refused connection instead of reaching Exa.
	_ = os.Unsetenv(modeltable.ExaKeyEnv)
	_ = os.Setenv(websearch.TestEndpointEnv, "http://127.0.0.1:9/mcp")
	os.Exit(m.Run())
}
