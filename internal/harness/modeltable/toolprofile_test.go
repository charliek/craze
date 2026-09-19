package modeltable

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// TestToolProfilesArePinnedToTheProfiles: the catalog keeps its own copy of
// the tool profile names, so loading models.toml links no tool code; this
// test is what keeps the copy honest. It also pins the file tools' name for
// the key file to this package's. When a profile is added, both lists move.
func TestToolProfilesArePinnedToTheProfiles(t *testing.T) {
	if got := ToolProfiles(); !slices.Equal(got, []string{opencode.Name}) {
		t.Fatalf("ToolProfiles = %q, want the registered profiles, the default first: %q", got, []string{opencode.Name})
	}
	if opencode.CredentialsFile != ProvidersFile {
		t.Fatalf("the file tools refuse %q, but the keys live in %q", opencode.CredentialsFile, ProvidersFile)
	}
	ToolProfiles()[0] = "changed"
	if toolProfiles[0] != opencode.Name {
		t.Fatal("ToolProfiles returned the package's own slice")
	}
}

// TestLoadToolProfile: tool_profile is optional; a known name loads and
// resolves, an absent or empty one is the default (""), an unknown one is a
// load error naming the model, the key and the value. A named profile
// survives Save, and a model without one is written without the key.
func TestLoadToolProfile(t *testing.T) {
	const minimax = `wire_model = "minimax/minimax-m3"`
	withProfile := func(value string) string {
		return strings.Replace(validModels, minimax, minimax+"\ntool_profile = "+value, 1)
	}

	dir := writeFiles(t, validProviders, withProfile(`"opencode"`))
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := tbl.Models["openrouter/minimax-m3"].ToolProfile; got != "opencode" {
		t.Fatalf("ToolProfile = %q", got)
	}
	if got := tbl.Models["fireworks/kimi-k3"].ToolProfile; got != "" {
		t.Fatalf("a model without tool_profile has %q, want the default", got)
	}
	r, err := tbl.Resolve("openrouter/minimax-m3", fakeEnv(nil))
	if err != nil || r.ToolProfile != "opencode" {
		t.Fatalf("Resolve = %+v, %v; want the profile carried", r, err)
	}

	out := filepath.Join(t.TempDir(), "native")
	if err := Save(out, tbl); err != nil {
		t.Fatal(err)
	}
	back, err := Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Models, tbl.Models) {
		t.Fatalf("round trip lost the profile:\n%+v\nwant\n%+v", back.Models, tbl.Models)
	}
	body, err := os.ReadFile(filepath.Join(out, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "tool_profile"); n != 1 || !strings.Contains(string(body), `tool_profile = "opencode"`) {
		t.Fatalf("models.toml holds tool_profile %d times, want once:\n%s", n, body)
	}

	if tbl, err := Load(writeFiles(t, validProviders, withProfile(`""`))); err != nil || tbl.Models["openrouter/minimax-m3"].ToolProfile != "" {
		t.Fatalf("an empty tool_profile: %v", err)
	}

	dir = writeFiles(t, validProviders, withProfile(`"gpt-patch"`))
	_, err = Load(dir)
	wantFileError(t, err, filepath.Join(dir, ModelsFile), `models."openrouter/minimax-m3"`, "tool_profile")
	if msg := err.Error(); !strings.Contains(msg, `"gpt-patch"`) || !strings.Contains(msg, `"opencode"`) {
		t.Fatalf("error %q must name the value and the profiles there are", msg)
	}

	dir = writeFiles(t, validProviders, withProfile(`5`))
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "tool_profile") {
		t.Fatalf("a non-string tool_profile: %v", err)
	}
}
