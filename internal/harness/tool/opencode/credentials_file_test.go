package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// TestFileToolsRefuseTheWebCredentialsFile (plan 051 §F.5):
// <Home>/credentials.toml, the web search's stored key, is refused as
// providers.toml is — read, write and edit, by name (any case, whether it
// exists or not), through a symlink and through a hard link — and grep and
// glob pass over it, so a model can neither read the key nor plant one.
// Nothing is written. The controls: models.toml beside it, and a
// credentials.toml anywhere else, are just files.
func TestFileToolsRefuseTheWebCredentialsFile(t *testing.T) {
	const key = "test-web-credential-key-0001"
	f := newFixture(t)
	cred := filepath.Join(f.env.Home, WebCredentialsFile)
	must(t, os.Symlink(cred, f.path("sneaky.toml")))

	// Before it exists: nothing may create it, under any spelling.
	for _, p := range []string{cred, f.path("sneaky.toml"), filepath.Join(f.env.Home, "Credentials.TOML")} {
		failed(t, second(f.call(t, "write", map[string]any{"filePath": p, "content": "[exa]\napi_key = \"" + key + "\"\n"})), tool.ClassToolError, credentialsText)
	}
	if _, err := os.Lstat(cred); err == nil {
		t.Fatal("the credentials file was created")
	}

	body := "[exa]\napi_key = \"" + key + "\"\n"
	put(t, cred, body)
	must(t, os.Link(cred, f.path("hard.toml")))
	for _, p := range []string{cred, f.path("sneaky.toml"), f.path("hard.toml")} {
		_, res := f.call(t, "read", map[string]any{"filePath": p})
		failed(t, res, tool.ClassToolError, credentialsText)
		if strings.Contains(res.Text+res.Content, key) {
			t.Fatalf("read %s: the key reached the result", p)
		}
		failed(t, second(f.call(t, "write", map[string]any{"filePath": p, "content": "x"})), tool.ClassToolError, credentialsText)
		failed(t, second(f.edit(t, p, key, "planted-key-0002", false)), tool.ClassToolError, credentialsText)
	}
	if load(t, cred) != body {
		t.Fatal("the credentials file changed")
	}

	t.Run("grep and glob", func(t *testing.T) {
		requireRG(t)
		failed(t, second(f.call(t, "grep", map[string]any{"pattern": key, "path": cred})), tool.ClassToolError, credentialsText)
		text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": key, "path": f.env.Home}))
		if strings.Contains(text, key) || strings.Contains(text, WebCredentialsFile) {
			t.Fatalf("grep showed the credentials file: %q", text)
		}
		listed := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.toml", "path": f.env.Home}))
		if strings.Contains(listed, WebCredentialsFile) {
			t.Fatalf("glob listed the credentials file: %q", listed)
		}
	})

	// The controls.
	put(t, filepath.Join(f.env.Home, "models.toml"), "version = 1\n")
	ok(t, second(f.call(t, "read", map[string]any{"filePath": filepath.Join(f.env.Home, "models.toml")})))
	ok(t, second(f.call(t, "write", map[string]any{"filePath": f.path(WebCredentialsFile), "content": "x = 1\n"})))
	ok(t, second(f.call(t, "read", map[string]any{"filePath": f.path(WebCredentialsFile)})))
}
