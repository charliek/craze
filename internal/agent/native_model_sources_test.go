package agent

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// Plan 050 C1 in the native adapter: --model as "<provider id>/<alias>", and
// the start error for a retired alias, which leads with its replacement.

// sourcesTable is a table over a small catalog of its own: an unprefixed
// alias on each of two providers, two models one display name shares, and
// two retired aliases, one with a replacement.
func sourcesTable(t *testing.T) *modeltable.Table {
	t.Helper()
	p := func(env string) modeltable.Provider {
		return modeltable.Provider{Name: env, Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", EnvKeys: []string{env}}
	}
	cat := &modeltable.Catalog{
		ProviderOrder: []string{"zai-coding-plan", "meta"},
		Providers:     map[string]modeltable.Provider{"zai-coding-plan": p("ZAI_KEY"), "meta": p("META_KEY")},
		Models: map[string]modeltable.Model{
			"glm-5.3":        {Provider: "zai-coding-plan", WireModel: "glm-5.3", Name: "GLM 5.3"},
			"muse-spark-1.3": {Provider: "meta", WireModel: "muse-spark-1.3", Name: "Muse Spark"},
			"twin-a":         {Provider: "meta", WireModel: "twin-a", Name: "meta/muse-spark-1.3"},
			"twin-b":         {Provider: "meta", WireModel: "twin-b", Name: "meta/muse-spark-1.3"},
		},
		Retired: []modeltable.Retired{
			{Alias: "meta/muse-old", Provider: "meta", ReplacedBy: "muse-spark-1.3"},
			{Alias: "meta/muse-older", Provider: "meta"},
		},
	}
	table, err := modeltable.LoadWith(t.TempDir(), cat)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// TestMatchNativeModelByProvider: a --model the table's aliases and names do
// not match is tried as "<provider id>/<alias>", under --model's own
// normalisation; one they match several times stays ambiguous, never
// read as the qualified form. The sub-agent matcher reads the same.
func TestMatchNativeModelByProvider(t *testing.T) {
	table := sourcesTable(t)
	match := nativeModelMatcher(table)
	for raw, want := range map[string]string{
		"zai-coding-plan/glm-5.3": "glm-5.3",
		"ZAI-Coding-Plan/GLM-5.3": "glm-5.3",
		"meta/muse-spark-1.3":     "", // two display names normalise to it: ambiguous
		"meta/twin_a":             "twin-a",
		"meta/glm-5.3":            "",
		"glm 5.3":                 "glm-5.3",
	} {
		got, err := MatchNativeModel(table, raw)
		if want == "" {
			if err == nil {
				t.Errorf("MatchNativeModel(%q) = %q; want a refusal", raw, got)
			}
		} else if err != nil || got != want {
			t.Errorf("MatchNativeModel(%q) = %q, %v; want %q", raw, got, err, want)
		}
		if alias, ok := match(raw); alias != got || ok != (err == nil) {
			t.Errorf("the sub-agent matcher reads %q as %q, %v; --model as %q, %v", raw, alias, ok, got, err)
		}
	}
	if _, err := MatchNativeModel(table, "meta/muse-spark-1.3"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("an ambiguous name = %v; want it reported ambiguous", err)
	}
}

// TestRetiredModelText: a --model naming a retired alias is refused with the
// replacement first and no alias list after it; one retired with no
// replacement says it was retired, then lists the table; any other unknown
// model is refused as it always was.
func TestRetiredModelText(t *testing.T) {
	table := sourcesTable(t)
	text := func(raw string) string {
		_, err := MatchNativeModel(table, raw)
		if err == nil {
			t.Fatalf("%q matched", raw)
		}
		return unknownNativeModelText(table, raw, err)
	}
	if got, want := text("meta/muse-old"), "native: meta/muse-old was retired; use muse-spark-1.3"; got != want {
		t.Errorf("a retired alias with a replacement = %q, want %q", got, want)
	}
	if got := text(" Meta/Muse-Old "); got != "native: meta/muse-old was retired; use muse-spark-1.3" {
		t.Errorf("a retired alias as --model normalises it = %q", got)
	}
	if got := text("meta/muse-older"); !strings.HasPrefix(got, "native: meta/muse-older was retired (the model table has glm-5.3, ") {
		t.Errorf("a retired alias with no replacement = %q", got)
	}
	if got := text("nope"); !strings.HasPrefix(got, `native: unknown model "nope" (the model table has glm-5.3, `) {
		t.Errorf("an unknown model = %q", got)
	}
}
