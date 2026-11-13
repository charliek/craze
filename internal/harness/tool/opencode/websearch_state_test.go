package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// TestFileToolsRefuseTheWebSearchState (plan 051 §C.2): <Home>/websearch.json,
// the web search's remembered choice, is refused as providers.toml is — read,
// write and edit, by name (any case, whether it exists or not), through a
// symlink and through a hard link — and grep and glob pass over it, so a
// model cannot turn its own web search on or read what was chosen. Nothing
// is written. The controls: models.toml beside it, and a websearch.json
// anywhere else, are just files.
func TestFileToolsRefuseTheWebSearchState(t *testing.T) {
	f := newFixture(t)
	state := filepath.Join(f.env.Home, WebSearchStateFile)
	must(t, os.Symlink(state, f.path("sneaky.json")))

	// Before it exists: nothing may create it, under any spelling.
	for _, p := range []string{state, f.path("sneaky.json"), filepath.Join(f.env.Home, "WebSearch.JSON")} {
		failed(t, second(f.call(t, "write", map[string]any{"filePath": p, "content": `{"version":1,"provider":"exa"}`})), tool.ClassToolError, credentialsText)
	}
	if _, err := os.Lstat(state); err == nil {
		t.Fatal("the state file was created")
	}

	const body = `{"version":1,"provider":false,"note":"test-websearch-state"}` + "\n"
	put(t, state, body)
	must(t, os.Link(state, f.path("hard.json")))
	for _, p := range []string{state, f.path("sneaky.json"), f.path("hard.json")} {
		_, res := f.call(t, "read", map[string]any{"filePath": p})
		failed(t, res, tool.ClassToolError, credentialsText)
		if strings.Contains(res.Text+res.Content, "test-websearch-state") {
			t.Fatalf("read %s: the state reached the result", p)
		}
		failed(t, second(f.call(t, "write", map[string]any{"filePath": p, "content": "x"})), tool.ClassToolError, credentialsText)
		failed(t, second(f.edit(t, p, "false", `"exa"`, false)), tool.ClassToolError, credentialsText)
	}
	if load(t, state) != body {
		t.Fatal("the state file changed")
	}

	t.Run("grep and glob", func(t *testing.T) {
		requireRG(t)
		failed(t, second(f.call(t, "grep", map[string]any{"pattern": "test-websearch-state", "path": state})), tool.ClassToolError, credentialsText)
		text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "test-websearch-state", "path": f.env.Home}))
		if strings.Contains(text, "test-websearch-state") || strings.Contains(text, WebSearchStateFile) {
			t.Fatalf("grep showed the state file: %q", text)
		}
		listed := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.json", "path": f.env.Home}))
		if strings.Contains(listed, WebSearchStateFile) {
			t.Fatalf("glob listed the state file: %q", listed)
		}
	})

	// The controls.
	put(t, filepath.Join(f.env.Home, "models.toml"), "version = 1\n")
	ok(t, second(f.call(t, "read", map[string]any{"filePath": filepath.Join(f.env.Home, "models.toml")})))
	ok(t, second(f.call(t, "write", map[string]any{"filePath": f.path(WebSearchStateFile), "content": "{}"})))
	ok(t, second(f.call(t, "read", map[string]any{"filePath": f.path(WebSearchStateFile)})))
}
