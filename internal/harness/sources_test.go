package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
)

// Plan 050 C5 in the harness: the agent tool's call held to the sub-agent
// choice set when the source order hides a model, and the resume fallback's
// paid-source guard. The table is the fixture's providers under a catalog of
// this file's own: test (no maker) with test/a, other with other/c of maker
// "acme" and other/g of none, and the ChatGPT plan, acme's first source,
// whose sign-in the cases lay down in the fixture's home.

// sourcesCatalog is the catalog over the fixture's providers, at baseURL.
func sourcesCatalog(baseURL string) *modeltable.Catalog {
	keyed := func(name, env string) modeltable.Provider {
		return modeltable.Provider{Name: name, Driver: modeltable.DriverOpenAICompat, BaseURL: baseURL, EnvKeys: []string{env}}
	}
	return &modeltable.Catalog{
		ProviderOrder:  []string{"chatgpt", "test", "other"},
		SourceOrder:    []string{"chatgpt", "test", "other"},
		ProviderMakers: map[string]string{"chatgpt": "acme", "test": "acme"},
		Providers: map[string]modeltable.Provider{
			"chatgpt": {Name: "ChatGPT plan", Driver: modeltable.DriverChatGPT},
			"test":    keyed("Test", "TEST_API_KEY"),
			"other":   keyed("Other", "OTHER_API_KEY"),
		},
		Models: map[string]modeltable.Model{
			"test/a":  {Provider: "test", WireModel: "wire-a", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
			"other/c": {Provider: "other", WireModel: "wire-c", Maker: "acme"},
			"other/g": {Provider: "other", WireModel: "wire-g"},
		},
		Starts:  map[string][]string{"other": {"other/c", "other/g"}},
		ChatGPT: modeltable.ChatGPTDefaults{ModelsClientVersion: "0.160.0"},
	}
}

// sourcesFixture is newFixture with its table loaded over sourcesCatalog,
// signed in to the ChatGPT plan with no model list when signedIn.
func sourcesFixture(t *testing.T, signedIn bool) *fixture {
	t.Helper()
	f := newFixture(t, "http://127.0.0.1:1/v1")
	for _, name := range []string{modeltable.ProvidersFile, modeltable.ModelsFile} {
		if err := os.Remove(filepath.Join(f.home, name)); err != nil {
			t.Fatal(err)
		}
	}
	if signedIn {
		signInFixture(t, f.home)
	}
	table, err := modeltable.LoadWith(f.home, sourcesCatalog("http://127.0.0.1:1/v1"))
	if err != nil {
		t.Fatal(err)
	}
	f.table = table
	for alias, m := range table.Models {
		f.models[alias] = &scripted{provider: m.Provider, wire: m.WireModel}
	}
	return f
}

// TestSubagentCallHeldToTheSourceOrder: test/a is acme's model on test, the
// first funded source, so other/c, acme's on other, is hidden: the agent
// tool's menu leaves it out, a call naming it is refused with the choice set,
// and a persona's model or the [subagents] model that names it still runs —
// the owner wrote those. A model of no maker on the same provider is offered.
func TestSubagentCallHeldToTheSourceOrder(t *testing.T) {
	f := sourcesFixture(t, false)
	if _, err := f.table.Resolve("other/c", f.getenv); err != nil {
		t.Fatalf("premise: other/c runs: %v", err)
	}
	m := f.table.Models["test/a"]
	if m.Maker != "acme" {
		t.Fatalf("premise: test/a's maker = %q", m.Maker)
	}
	red := redact.New()
	menu := renderSubagentModels(f.table, f.getenv, []string{"test/a"}, red)
	if strings.Contains(menu, "other/c") || !strings.Contains(menu, "other/g") || !strings.Contains(menu, "test/a") {
		t.Fatalf("the menu is\n%s\nwant test/a and other/g, not the hidden other/c", menu)
	}
	ts := &toolset{}
	ts.red.Store(redact.New())
	s := &Session{table: f.table, getenv: f.getenv, tools: ts}
	_, err := s.resolveChildModel(s.view(), childModelInput{Call: "other/c", ParentAlias: "test/a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Model not offered to sub-agents `other/c`. Models: other/g, test/a;") {
		t.Fatalf("a call naming the hidden other/c = %v; want it refused with the choice set", err)
	}
	if got, err := s.resolveChildModel(s.view(), childModelInput{Call: "other/g", ParentAlias: "test/a"}, nil); err != nil || got != "other/g" {
		t.Fatalf("a call naming other/g = %q, %v; want it to run", got, err)
	}
	if got, err := s.resolveChildModel(s.view(), childModelInput{Persona: "other/c", ParentAlias: "test/a"}, nil); err != nil || got != "other/c" {
		t.Fatalf("a persona naming other/c = %q, %v; want full resolution", got, err)
	}
	f.table.Subagents = modeltable.Subagents{Model: "other/c"}
	if got, err := s.resolveChildModel(s.view(), childModelInput{ParentAlias: "test/a"}, nil); err != nil || got != "other/c" {
		t.Fatalf("the [subagents] model other/c = %q, %v; want full resolution", got, err)
	}
	if got, err := s.resolveChildModel(s.view(), childModelInput{Call: "other/c", ParentAlias: "test/a"}, nil); err != nil || got != "other/c" {
		t.Fatalf("a call naming the configured other/c = %q, %v; want it to run", got, err)
	}
}

// TestResumeFallbackHeldBySourceGuard: a stored session whose model no longer
// runs (its key gone) resumes on a start model only when craze may choose it
// for the owner. Signed in to the ChatGPT plan with no model list, acme's
// other/c is held back — the plan is acme's first source — and the next
// start, other/g of no maker, is taken; with no other start the refusal names
// the sign-in and the alias. Signed out, other/c is chosen as before.
func TestResumeFallbackHeldBySourceGuard(t *testing.T) {
	noTest := func(name string) string {
		if name == "TEST_API_KEY" {
			return ""
		}
		return testEnv[name]
	}
	for _, tc := range []struct {
		name     string
		signedIn bool
		starts   []string
		want     string // the model resumed on; "" for the refusal
		pin      string // the default_model pin, set after the load
	}{
		{"signed out: the first start", false, []string{"other/c", "other/g"}, "other/c", ""},
		{"signed in, no list: the start of no maker", true, []string{"other/c", "other/g"}, "other/g", ""},
		{"signed in, no list, no other start: refused", true, []string{"other/c"}, "", ""},
		// The pin is never held to the source order (plan 050): pinned on
		// the held other/c, the resume continues there, neither on other/g
		// nor refused.
		{"signed in, no list: a held pin still continues", true, []string{"other/c", "other/g"}, "other/c", "other/c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sourcesFixture(t, tc.signedIn)
			opts := f.options()
			opts.Model = "test/a"
			id := storedTurn(t, f, opts, nil)

			cat := sourcesCatalog("http://127.0.0.1:1/v1")
			cat.Starts["other"] = tc.starts
			table, err := modeltable.LoadWith(f.home, cat)
			if err != nil {
				t.Fatal(err)
			}
			table.DefaultModel = tc.pin // the catalog itself sets none
			ropts := resumeOptions(f.options(), id)
			ropts.Model, ropts.Table, ropts.Getenv = "", table, noTest
			var warned []string
			ropts.Warn = func(line string) { warned = append(warned, line) }
			s, err := Open(ropts)
			if tc.want == "" {
				if !errors.Is(err, ErrResumeModel) || !strings.Contains(err.Error(), "other/c: not chosen for you: signed in to ChatGPT, but its models are not available; craze auth login chatgpt, or start with --model other/c") {
					t.Fatalf("resume = %v; want ErrResumeModel naming the guard", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if got, _ := s.Current(); got != tc.want {
				t.Fatalf("resumed on %s, want %s", got, tc.want)
			}
			if len(warned) == 0 || !strings.Contains(warned[0], "continuing on "+tc.want) {
				t.Fatalf("warned %q; want the move named", warned)
			}
		})
	}
}

// TestResumeFallbackHeldByAnEnvOnlyKey (guard 2, G1): with other marked
// env_key_by_name_only and its key in the environment alone, a stored
// session whose model no longer runs is not moved to other's start on the
// owner's behalf: the refusal names the variable, the provider to store a
// key for and the alias, in the sign-in's sentence form. A stored session
// on other's own model continues on it by identity: that is the owner's
// conversation, not craze's choice.
func TestResumeFallbackHeldByAnEnvOnlyKey(t *testing.T) {
	noTest := func(name string) string {
		if name == "TEST_API_KEY" {
			return ""
		}
		return testEnv[name]
	}
	marked := func() *modeltable.Catalog {
		cat := sourcesCatalog("http://127.0.0.1:1/v1")
		cat.Starts["other"] = []string{"other/c"}
		cat.EnvKeyByNameOnly = map[string]bool{"other": true}
		return cat
	}
	f := sourcesFixture(t, false)
	opts := f.options()
	opts.Model = "test/a"
	id := storedTurn(t, f, opts, nil)
	table, err := modeltable.LoadWith(f.home, marked())
	if err != nil {
		t.Fatal(err)
	}
	ropts := resumeOptions(f.options(), id)
	ropts.Model, ropts.Table, ropts.Getenv = "", table, noTest
	_, err = Open(ropts)
	const want = "other/c: not chosen for you: OTHER_API_KEY is set in the environment, but no key is stored for Other; craze auth login other, or start with --model other/c"
	if !errors.Is(err, ErrResumeModel) || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), canaryOther) {
		t.Fatalf("resume = %v; want ErrResumeModel naming %q", err, want)
	}

	opts.Model = "other/c"
	onOther := storedTurn(t, f, opts, nil)
	ropts = resumeOptions(f.options(), onOther)
	ropts.Model, ropts.Table, ropts.Getenv = "", table, noTest
	if table.Offers(noTest).Held["other/c"] == nil {
		t.Fatal("premise: other/c is not held")
	}
	s, err := Open(ropts)
	if err != nil {
		t.Fatalf("resume on other/c by identity: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got, _ := s.Current(); got != "other/c" {
		t.Fatalf("resumed on %s, want other/c", got)
	}
}
