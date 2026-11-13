package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// TestModelSourceOfARepointedAlias (plan 050 review): `source` says whose
// model an alias is. A shipped alias untouched, or overridden on keys other
// than its provider and wire model, is the catalog's; one models.toml points
// at another provider or wire model is the user's, as an alias of the
// user's own is.
func TestModelSourceOfARepointedAlias(t *testing.T) {
	dir := t.TempDir()
	models := "version = 1\n\n" +
		"[models.\"fireworks/kimi-k3p1-pro\"]\nwire_model = \"accounts/me/models/my-kimi\"\n\n" +
		"[models.\"fireworks/glm-5p3\"]\ndefault_effort = \"low\"\n\n" +
		"[models.\"mine\"]\nprovider = \"fireworks\"\nwire_model = \"accounts/me/models/mine\"\n"
	if err := os.WriteFile(filepath.Join(dir, modeltable.ModelsFile), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]string{
		"fireworks/kimi-k3":       sourceCatalog,
		"fireworks/glm-5p3":       sourceCatalog,
		"fireworks/kimi-k3p1-pro": sourceUser,
		"mine":                    sourceUser,
	} {
		if got := modelSource(tbl, alias); got != want {
			t.Errorf("modelSource(%s) = %q, want %q", alias, got, want)
		}
	}
}
