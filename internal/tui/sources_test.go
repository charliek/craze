package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Plan 050 C5 in the TUI: the session list's start when the paid-source
// guard holds every model back, and the /connect note that says which source
// a maker's models come from.

// shippedDir is a native directory over the shipped catalog alone, signed in
// to the ChatGPT plan with plan usage and no model list when signedIn.
func shippedDir(t *testing.T, signedIn bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "native")
	if err := os.MkdirAll(filepath.Join(dir, modeltable.ChatGPTAuthDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if signedIn {
		for name, body := range map[string]string{
			modeltable.ChatGPTClientFile: `{"client_id":"app_client-0001","subject":"user-0001","plan_usage":true}`,
			modeltable.ChatGPTTokenFile:  `{"placeholder":"no token here"}`,
		} {
			if err := os.WriteFile(filepath.Join(dir, modeltable.ChatGPTAuthDir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// envOf is an environment holding exactly vars.
func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// TestTheListsStartHeldBySourceGuard: signed in to the ChatGPT plan with no
// model list and an OpenAI key, native's default for the list is the guard's
// own refusal — naming the sign-in and /model — not "no model provider has an
// API key"; its /model still offers the OpenAI key's models. Signed out, the
// key in the environment alone is held too (guard 2, G1), naming the
// variable and the provider to store a key for, and /model still offers its
// models; with the key stored, the default is the key's Sol.
func TestTheListsStartHeldBySourceGuard(t *testing.T) {
	env := envOf(map[string]string{"OPENAI_API_KEY": "sk-openai-tui-dummy-0001"})
	_, err := nativeDefaultModel(shippedDir(t, true), env)
	want := "signed in to ChatGPT, but its models are not available, so craze does not pick openai/gpt-6.1-sol for you — run craze auth login chatgpt, or pick a model with /model"
	if err == nil || err.Error() != want {
		t.Fatalf("nativeDefaultModel = %v;\nwant %s", err, want)
	}
	models, err := nativeModelChoices(shippedDir(t, true), env)
	if err != nil || len(models) != 4 || !strings.HasPrefix(models[0].ID, "openai/") {
		t.Fatalf("nativeModelChoices = %+v, %v; want the four OpenAI models", models, err)
	}
	_, err = nativeDefaultModel(shippedDir(t, false), env)
	want = "OPENAI_API_KEY is set in the environment, but no key is stored for OpenAI, so craze does not pick openai/gpt-6.1-sol for you — run craze auth login openai, or pick a model with /model"
	if err == nil || err.Error() != want || strings.Contains(err.Error(), "sk-openai-tui-dummy-0001") {
		t.Fatalf("signed out, the key in the environment: %v;\nwant %s", err, want)
	}
	if models, err := nativeModelChoices(shippedDir(t, false), env); err != nil || len(models) != 4 || !strings.HasPrefix(models[0].ID, "openai/") {
		t.Fatalf("signed out, the key in the environment: nativeModelChoices = %+v, %v; want the four OpenAI models", models, err)
	}
	stored := shippedDir(t, false)
	if err := modeltable.SetKey(stored, "openai", "sk-openai-tui-dummy-0001"); err != nil {
		t.Fatal(err)
	}
	if md, err := nativeDefaultModel(stored, envOf(nil)); err != nil || md.ID != "openai/gpt-6.1-sol" {
		t.Fatalf("signed out, the key stored: %+v, %v; want openai/gpt-6.1-sol", md, err)
	}
}

// TestConnectSaysWhichSourceAMakersModelsComeFrom (review F4): the note
// after /connect or a sign-in says what the source order makes of the
// provider's models — never that a hidden model is in /model: the ones it
// lists, the makers whose models come from elsewhere and still run with
// --model, none listed at all when every one is hidden, and the makers whose
// models now come from it.
func TestConnectSaysWhichSourceAMakersModelsComeFrom(t *testing.T) {
	applied := &agent.ModelsRefresh{Status: agent.ModelsApplied}
	pending := &agent.ModelsRefresh{Status: agent.ModelsPending}
	env := envOf(map[string]string{"FIREWORKS_API_KEY": "fw-tui-dummy-0001", "OPENROUTER_API_KEY": "or-tui-dummy-0001", "META_API_KEY": "meta-tui-dummy-0001"})
	table, err := modeltable.Load(shippedDir(t, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		r    *agent.ModelsRefresh
		want string
	}{
		{"fireworks", applied, "Connected Fireworks. Its models are in /model now. DeepSeek models come from it now; those of OpenRouter are no longer listed. GLM models come from it now; those of OpenRouter are no longer listed. They still run with --model (source_order in models.toml changes the order)."},
		{"openrouter", applied, "Connected OpenRouter. Its models are in /model now, except its DeepSeek models, which come from Fireworks, and its Meta models, which come from Meta, and its GLM models, which come from Fireworks; they still run with --model (source_order in models.toml changes the order)."},
		{"openrouter", pending, "Connected OpenRouter. Its models will be in /model after this turn, except its DeepSeek models, which come from Fireworks, and its Meta models, which come from Meta, and its GLM models, which come from Fireworks; they still run with --model (source_order in models.toml changes the order)."},
		{"openrouter", nil, "Connected OpenRouter. New sessions offer its models, except its DeepSeek models, which come from Fireworks, and its Meta models, which come from Meta, and its GLM models, which come from Fireworks; they still run with --model (source_order in models.toml changes the order). To use the listed ones in this conversation, /exit and run craze -c."},
		{"anthropic", applied, "Connected Anthropic. Its models are in /model now."},
	} {
		if got := connectedNote(table.ProviderName(tc.id), connectSourcesOf(table, tc.id, env), tc.r); got != tc.want {
			t.Errorf("%s:\n%q\nwant\n%q", tc.id, got, tc.want)
		}
	}
	// Every one of its models hidden — an OpenAI key behind a ChatGPT list —
	// is "none of its models are listed".
	signedIn := shippedDir(t, true)
	if err := os.WriteFile(filepath.Join(signedIn, modeltable.ChatGPTModelsFile), []byte(planList), 0o600); err != nil {
		t.Fatal(err)
	}
	oa := envOf(map[string]string{"OPENAI_API_KEY": "sk-openai-tui-dummy-0001"})
	plan, err := modeltable.Load(signedIn)
	if err != nil {
		t.Fatal(err)
	}
	const noneListed = "Connected OpenAI. None of its models are listed: OpenAI models come from ChatGPT plan; they still run with --model (source_order in models.toml changes the order)."
	if got := connectedNote("OpenAI", connectSourcesOf(plan, "openai", oa), applied); got != noneListed {
		t.Errorf("all hidden:\n%q\nwant\n%q", got, noneListed)
	}
	// The sign-in: with the key first in the source order, the plan's models
	// are hidden, and the note says so instead of "in /model now".
	if err := os.WriteFile(filepath.Join(signedIn, modeltable.ModelsFile), []byte("version = 1\nsource_order = [\"openai\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyFirst, err := modeltable.Load(signedIn)
	if err != nil {
		t.Fatal(err)
	}
	const planHidden = "None of the ChatGPT plan's models are listed: OpenAI models come from OpenAI; they still run with --model (source_order in models.toml changes the order)."
	if got := signedInSessionNote(connectSourcesOf(keyFirst, modeltable.ChatGPTProvider, oa), applied); got != planHidden {
		t.Errorf("a sign-in behind the key:\n%q\nwant\n%q", got, planHidden)
	}
	const planWins = "The plan's models are in /model now. OpenAI models come from it now; those of OpenAI are no longer listed. They still run with --model (source_order in models.toml changes the order)."
	if got := signedInSessionNote(connectSourcesOf(plan, modeltable.ChatGPTProvider, oa), applied); got != planWins {
		t.Errorf("a sign-in ahead of the key:\n%q\nwant\n%q", got, planWins)
	}
	if got := signedInSessionNote(connectSources{}, applied); got != "The plan's models are in /model now." {
		t.Errorf("a sign-in with nothing hidden: %q", got)
	}

	// Both source sentences speak of the state the refresh left (plan 050
	// review): the present only once the session took the change up, the
	// future while a turn runs, and new sessions' with no refresh — or one
	// that changed nothing in this conversation.
	current := &agent.ModelsRefresh{Status: agent.ModelsCurrent}
	for _, tc := range []struct {
		name string
		r    *agent.ModelsRefresh
		none string
		wins string
	}{
		{"applied", applied, noneListed, planWins},
		{"pending", pending,
			"Connected OpenAI. None of its models will be listed after this turn: OpenAI models come from ChatGPT plan; they still run with --model (source_order in models.toml changes the order).",
			"The plan's models will be in /model after this turn. OpenAI models will come from it after this turn; those of OpenAI will no longer be listed. They still run with --model (source_order in models.toml changes the order)."},
		{"no refresh", nil,
			"Connected OpenAI. New sessions list none of its models: OpenAI models come from ChatGPT plan; they still run with --model (source_order in models.toml changes the order).",
			"New sessions offer the ChatGPT plan's models; to use them in this conversation, /exit and run craze -c. In new sessions OpenAI models come from it; those of OpenAI are not listed there. They still run with --model (source_order in models.toml changes the order)."},
		{"nothing changed", current,
			"Connected OpenAI. New sessions list none of its models: OpenAI models come from ChatGPT plan; they still run with --model (source_order in models.toml changes the order).",
			"New sessions offer the ChatGPT plan's models; to use them in this conversation, /exit and run craze -c. In new sessions OpenAI models come from it; those of OpenAI are not listed there. They still run with --model (source_order in models.toml changes the order)."},
	} {
		if got := connectedNote("OpenAI", connectSourcesOf(plan, "openai", oa), tc.r); got != tc.none {
			t.Errorf("%s, all hidden:\n%q\nwant\n%q", tc.name, got, tc.none)
		}
		if got := signedInSessionNote(connectSourcesOf(plan, modeltable.ChatGPTProvider, oa), tc.r); got != tc.wins {
			t.Errorf("%s, a sign-in that takes a maker over:\n%q\nwant\n%q", tc.name, got, tc.wins)
		}
	}
}

// planList is a ChatGPT model list for shippedDir's account.
const planList = `{"version":1,"subject":"user-0001","client_id":"app_client-0001","models":[
{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","context_window":272000,"efforts":["low","medium","high"],"default_effort":"medium","input_modalities":["text"],"priority":0}
]}`

// hintCatalog: the stub's model, grok, is OpenAI's on acme; router has
// OpenAI's router/sol and Gemini's router/gem.
func hintCatalog() *modeltable.Catalog {
	p := func(name, env string) modeltable.Provider {
		return modeltable.Provider{Name: name, Driver: modeltable.DriverOpenAICompat, BaseURL: "https://" + name + ".example/v1", EnvKeys: []string{env}}
	}
	return &modeltable.Catalog{
		ProviderOrder:  []string{"acme", "router"},
		SourceOrder:    []string{"acme", "router"},
		ProviderMakers: map[string]string{"acme": "openai"},
		Providers:      map[string]modeltable.Provider{"acme": p("Acme", "ACME_HINT_KEY"), "router": p("Router", "ROUTER_HINT_KEY")},
		Models: map[string]modeltable.Model{
			"grok":       {Provider: "acme", WireModel: "acme-grok"},
			"router/sol": {Provider: "router", WireModel: "openai/sol", Maker: "openai", Cost: &modeltable.Cost{}},
			"router/gem": {Provider: "router", WireModel: "google/gem"},
		},
		Starts: map[string][]string{"acme": {"grok"}},
	}
}

// TestAQuotaFailureIsFollowedByAHintNote (plan 050 C5, review F3): a native
// turn that fails on a quota or plan limit draws the failure's row as it
// always did, and then — from the TUI's own table, read off the Update — one
// note naming the maker's other funded source and the way to it. It is never
// part of the failure, so no waiting command can read it. Nothing follows
// another kind of failure, a replayed one, an ACP session's, or one with no
// other funded source.
func TestAQuotaFailureIsFollowedByAHintNote(t *testing.T) {
	prev := noteTable
	t.Cleanup(func() { noteTable = prev })
	noteTable = func(dir string) (*modeltable.Table, error) { return modeltable.LoadWith(dir, hintCatalog()) }
	quota := fmt.Errorf("native: ChatGPT plan usage limit reached: %w", chatgptauth.ErrUsageLimited)
	const want = "OpenAI models are also funded by your Router key (billed per token): router/sol. To use one, end this session with /exit, then run craze -c --model router/sol"

	open := func(stub *Stub, env map[string]string) Model {
		t.Cleanup(func() { _ = stub.Close() })
		return connectModelOf(t, Config{Session: stub, NativeDir: t.TempDir(), Getenv: func(k string) string { return env[k] }})
	}
	both := map[string]string{"ACME_HINT_KEY": "sk-acme-hint-0001", "ROUTER_HINT_KEY": "sk-router-hint-0001"}
	m := open(nativeStub(), both)
	cmd := m.reduceEvent(agent.Event{Type: agent.EventError, Err: quota})
	if cmd == nil {
		t.Fatal("a quota failure scheduled no hint")
	}
	msg, ok := cmd().(quotaHintMsg)
	if !ok || msg.text != want {
		t.Fatalf("the hint = %#v, want %q", msg, want)
	}
	tm, _ := m.Update(msg)
	if got := allNotes(tm.(Model)); strings.Count(got, want) != 1 {
		t.Fatalf("notes = %q, want the hint once", got)
	}
	if strings.Contains(quota.Error(), "also funded") {
		t.Fatal("the failure carries the hint")
	}

	if m.reduceEvent(agent.Event{Type: agent.EventError, Err: errors.New("native: provider \"acme\" failed (HTTP 500)")}) != nil {
		t.Error("another kind of failure scheduled a hint")
	}
	if m.reduceEvent(agent.Event{Type: agent.EventError, Err: quota, Replayed: true}) != nil {
		t.Error("a replayed failure scheduled a hint")
	}
	alone := open(nativeStub(), map[string]string{"ACME_HINT_KEY": "sk-acme-hint-0001"})
	if cmd := alone.reduceEvent(agent.Event{Type: agent.EventError, Err: quota}); cmd == nil || cmd() != nil {
		t.Error("with no other funded source a hint was answered")
	}
	acp := NewStub()
	acp.SetProvider(agent.GrokProvider())
	am := open(acp, both)
	if am.reduceEvent(agent.Event{Type: agent.EventError, Err: quota}) != nil {
		t.Error("an ACP session scheduled a hint")
	}
}
