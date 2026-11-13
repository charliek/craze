package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestConfigWebSearch (plan 051 §C.1): `websearch` has two valid forms,
// `websearch = false` (off) and a [websearch] table whose only key is a
// provider craze has (on); no key is absent; everything else — and a
// config.toml that cannot be read or parsed — is invalid, which is off, with
// a line naming the two forms that quotes nothing from the file.
func TestConfigWebSearch(t *testing.T) {
	const forms = "(websearch takes one of two forms: `websearch = false`, or a [websearch] table with provider = \"exa\")"
	notForm := "config.toml websearch is not one of its two forms, so web search is off " + forms
	for _, tc := range []struct {
		name, body string
		want       agent.WebConfig
	}{
		{"no file", "", agent.WebConfig{}},
		{"no key", "theme = \"dark\"\n", agent.WebConfig{}},
		{"false", "websearch = false\n", agent.WebConfig{Setting: agent.WebSettingOff}},
		{"a table", "[websearch]\nprovider = \"exa\"\n", agent.WebConfig{Setting: agent.WebSettingOn, Provider: "exa"}},
		{"an inline table", "websearch = { provider = \"exa\" }\n", agent.WebConfig{Setting: agent.WebSettingOn, Provider: "exa"}},
		{"true", "websearch = true\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"a string", "websearch = \"false\"\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"a number", "websearch = 0\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"an empty table", "[websearch]\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"an unknown provider", "[websearch]\nprovider = \"sk-not-a-provider-0123\"\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"a provider in capitals", "[websearch]\nprovider = \"EXA\"\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"a provider that is no string", "[websearch]\nprovider = true\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"another key beside it", "[websearch]\nprovider = \"exa\"\nkey = \"sk-not-here-0123\"\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"another key alone", "[websearch]\nenabled = true\n", agent.WebConfig{Setting: agent.WebSettingInvalid, Why: notForm}},
		{"a malformed file", "websearch = false\nthis is not toml [[[\n", agent.WebConfig{Setting: agent.WebSettingInvalid,
			Why: "config.toml could not be parsed, so web search is off " + forms}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.body == "" {
				t.Setenv("CRAZE_HOME", t.TempDir())
			} else {
				writeConfigFile(t, tc.body)
			}
			got := ConfigWebSearch()
			if got != tc.want {
				t.Fatalf("ConfigWebSearch() = %+v\nwant %+v", got, tc.want)
			}
			if strings.Contains(got.Why, "sk-not") {
				t.Fatalf("the line quotes the file: %q", got.Why)
			}
		})
	}

	t.Run("an unreadable file", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CRAZE_HOME", dir)
		if err := os.Mkdir(filepath.Join(dir, configName), 0o700); err != nil {
			t.Fatal(err)
		}
		want := agent.WebConfig{Setting: agent.WebSettingInvalid, Why: "config.toml could not be read, so web search is off " + forms}
		if got := ConfigWebSearch(); got != want {
			t.Fatalf("ConfigWebSearch() = %+v\nwant %+v", got, want)
		}
	})
}
