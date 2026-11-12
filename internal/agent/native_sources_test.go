package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Plan 050 C5 in the native adapter, over the shipped catalog: the model
// list one source per maker offers, the start the paid-source guard refuses
// (kind config, naming the sign-in and --model), a reload that changes a
// maker's source while the session runs, and the quota hint a person is
// given and an unattended session is not.

// shippedFixture is newNativeFixture over the shipped catalog alone (the
// fixture's own files removed), funded by keys, its scripted models every
// alias the directory's table has — after layDown, which writes the sign-in.
func shippedFixture(t *testing.T, keys map[string]string, layDown func(dir string)) *nativeFixture {
	t.Helper()
	noOpenAI(t) // a sign-in's list refresh reaches a fake, never OpenAI
	f := newNativeFixture(t)
	for _, name := range []string{modeltable.ProvidersFile, modeltable.ModelsFile} {
		if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if layDown != nil {
		layDown(f.dir)
	}
	f.env = keys
	f.loadModels()
	return f
}

// loadModels gives every alias of the directory's table a scripted model.
func (f *nativeFixture) loadModels() {
	f.t.Helper()
	table, err := modeltable.Load(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	for alias, m := range table.Models {
		if f.models[alias] == nil {
			f.models[alias] = &scriptedModel{provider: m.Provider, wire: m.WireModel}
		}
	}
}

// signedInNoList signs dir in to the ChatGPT plan with plan usage and no
// model list: the registration and a token record.
func signedInNoList(t *testing.T) func(dir string) {
	return func(dir string) {
		if err := os.MkdirAll(chatgptauth.AuthDir(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := signInOf(dir, planSubject, planClient, "test-plan-access-0001", "test-plan-refresh-0001", "inc-1"); err != nil {
			t.Fatal(err)
		}
	}
}

// signedInWithList is signedInNoList with the account's list, which names
// gpt-5.6-sol (writePlanAccount).
func signedInWithList(t *testing.T) func(dir string) {
	return func(dir string) {
		signedInNoList(t)(dir)
		writePlanAccount(t, dir)
	}
}

const (
	dummyOpenAI     = "sk-openai-dummy-0000000001"
	dummyOpenRouter = "sk-or-dummy-000000000001"
)

// TestNativeStartHeldBySourceGuard (plan 050 acceptance 6): signed in to the
// ChatGPT plan with no model list and an OpenAI key, nothing remembered and
// no pin, a new session does not start on an openai/* model; the refusal is
// kind config, names the sign-in and --model, and is not "no model provider
// has a key". --model openai/gpt-6.1-sol starts it.
func TestNativeStartHeldBySourceGuard(t *testing.T) {
	f := shippedFixture(t, map[string]string{"OPENAI_API_KEY": dummyOpenAI}, signedInNoList(t))
	s := f.session(Options{})
	err := s.Start(context.Background())
	want := "native: signed in to ChatGPT, but its models are not available, and craze does not start a new session on openai/gpt-6.1-sol (OpenAI) on its own; sign in again with craze auth login chatgpt, or start with --model openai/gpt-6.1-sol"
	if err == nil || err.Error() != want {
		t.Fatalf("Start = %v;\nwant %s", err, want)
	}
	if !errors.Is(err, modeltable.ErrSourceHeld) || errors.Is(err, harness.ErrNoAPIKey) {
		t.Fatalf("Start's error %v: want ErrSourceHeld, not a missing key", err)
	}
	if got := StartErrorKind(err); got != errkind.Config {
		t.Fatalf("StartErrorKind = %s, want config", got)
	}
	explicit := f.started(Options{Model: "openai/gpt-6.1-sol"})
	if got := explicit.Snapshot().CurrentModel; got != "openai/gpt-6.1-sol" {
		t.Fatalf("--model openai/gpt-6.1-sol started on %q", got)
	}
}

// TestNativeListOneSourcePerMaker (plan 050 acceptance 5): signed in to the
// ChatGPT plan with its list, an OpenAI key and an OpenRouter key, the list
// has the plan's models and no openai/* or openrouter/gpt-* row; --model
// openai/gpt-6.1-sol still runs, listed as the session's own; with
// source_order = [] every row is back. /model cannot switch to a hidden
// model: it matches the advertised list.
func TestNativeListOneSourcePerMaker(t *testing.T) {
	keys := map[string]string{"OPENAI_API_KEY": dummyOpenAI, "OPENROUTER_API_KEY": dummyOpenRouter}
	f := shippedFixture(t, keys, signedInWithList(t))
	ids := offeredIDs(f.started(Options{}).Snapshot().Models)
	if !slices.Contains(ids, "chatgpt/gpt-5.6-sol") {
		t.Fatalf("the list %q lacks the plan's model", ids)
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "openai/") || strings.HasPrefix(id, "openrouter/gpt-") {
			t.Fatalf("the list offers %s: %q", id, ids)
		}
	}
	if !slices.Contains(ids, "openrouter/gemini-flash-latest") || !slices.Contains(ids, "openrouter/muse-wonder") {
		t.Fatalf("the list %q lacks OpenRouter's models of other makers", ids)
	}
	explicit := f.started(Options{Model: "openai/gpt-6.1-sol"})
	listed := offeredIDs(explicit.Snapshot().Models)
	if explicit.Snapshot().CurrentModel != "openai/gpt-6.1-sol" || !slices.Contains(listed, "openai/gpt-6.1-sol") || slices.Contains(listed, "openai/gpt-6-luna") {
		t.Fatalf("a session on openai/gpt-6.1-sol lists %q", listed)
	}
	if _, err := explicit.SetModel(context.Background(), "", "openai/gpt-6-luna"); err == nil {
		t.Fatal("SetModel switched to a hidden model")
	}

	if err := os.WriteFile(filepath.Join(f.dir, modeltable.ModelsFile), []byte("version = 1\nsource_order = []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	all := offeredIDs(f.started(Options{}).Snapshot().Models)
	for _, id := range []string{"chatgpt/gpt-5.6-sol", "openai/gpt-6.1-sol", "openai/gpt-6-luna", "openrouter/gpt-sol-latest"} {
		if !slices.Contains(all, id) {
			t.Fatalf("with source_order = [] the list %q lacks %s", all, id)
		}
	}
}

// TestNativeReloadMovesAMakersSource: a key stored while a session runs on
// openrouter/gpt-sol-latest makes the OpenAI key OpenAI's source: the
// session keeps running on its model, which stays listed, and the list's
// other OpenAI rows now come from the key; a later new session with no
// --model follows the new source on its own, through the model memory.
func TestNativeReloadMovesAMakersSource(t *testing.T) {
	f := shippedFixture(t, map[string]string{"OPENROUTER_API_KEY": dummyOpenRouter}, nil)
	table, err := modeltable.Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, alias := range []string{"openai/gpt-6-luna", "openrouter/gpt-sol-latest"} { // oldest first
		m := table.Models[alias]
		if err := modeltable.Remember(f.dir, modeltable.RecentEntry{Alias: alias, Provider: m.Provider, WireModel: m.WireModel}, time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.started(Options{}).Snapshot().CurrentModel; got != "openrouter/gpt-sol-latest" {
		t.Fatalf("before the key a new session started on %s, want the remembered openrouter/gpt-sol-latest", got)
	}
	s := f.started(Options{Model: "openrouter/gpt-sol-latest"})
	if ids := offeredIDs(s.Snapshot().Models); !slices.Contains(ids, "openrouter/gpt-luna-latest") || slices.Contains(ids, "openai/gpt-6-luna") {
		t.Fatalf("before: %q", ids)
	}
	if err := modeltable.SetKey(f.dir, "openai", dummyOpenAI); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, f.dir)
	snap := s.Snapshot()
	ids := offeredIDs(snap.Models)
	if snap.CurrentModel != "openrouter/gpt-sol-latest" || !slices.Contains(ids, "openrouter/gpt-sol-latest") {
		t.Fatalf("after: on %s, list %q; want the current model kept and listed", snap.CurrentModel, ids)
	}
	if slices.Contains(ids, "openrouter/gpt-luna-latest") || !slices.Contains(ids, "openai/gpt-6-luna") {
		t.Fatalf("after: %q; want OpenAI's other rows from the key", ids)
	}
	f.models["openrouter/gpt-sol-latest"].push(answer("ok"))
	if res, err := s.Prompt(context.Background(), "hi"); err != nil || res.StopReason != "end_turn" {
		t.Fatalf("a turn on the kept model = %+v, %v", res, err)
	}
	// A new session with no --model chooses by the new source on its own:
	// the memory's newest model, openrouter/gpt-sol-latest, is now hidden and
	// passed over (its entry stays), and the older openai/gpt-6-luna, which
	// did not run before the key, starts.
	if got := f.started(Options{}).Snapshot().CurrentModel; got != "openai/gpt-6-luna" {
		t.Fatalf("a new session after the key started on %s, want openai/gpt-6-luna", got)
	}
}

// TestNativeQuotaHint (review F3): a quota failure's message is the failure
// alone in every session — attended, unattended, whoever started the host —
// so no failure object a waiting command reads carries a route to another
// payer; QuotaHint, which the TUI notes beside the row, names the other
// funded sources of the model's maker, who pays, and the route — /exit, then
// craze -c --model — the closest model first, and nothing for a model of no
// maker.
func TestNativeQuotaHint(t *testing.T) {
	keys := map[string]string{"OPENAI_API_KEY": dummyOpenAI, "OPENROUTER_API_KEY": dummyOpenRouter}
	quota := &harness.ProviderError{Provider: "openai", Model: "openai/gpt-6.1-sol", Driver: modeltable.DriverOpenAIResponses,
		StatusCode: 429, Code: "insufficient_quota", Message: "You exceeded your current quota", Final: true}
	if ErrorInfo(quota).Kind != errkind.Quota {
		t.Fatalf("premise: %v is kind %s", quota, ErrorInfo(quota).Kind)
	}
	f := shippedFixture(t, keys, nil)
	plain := phraseTurnError(quota).Error()
	for name, interactive := range map[string]bool{"attended": true, "unattended": false} {
		s := f.started(Options{Model: "openai/gpt-6.1-sol", Interactive: interactive})
		got := s.phraseTurn(quota)
		if got.Error() != plain || strings.Contains(got.Error(), "also funded") || !errors.Is(got, quota) {
			t.Fatalf("%s: the quota failure = %q; want the failure alone, %q", name, got, plain)
		}
	}
	table, err := modeltable.Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) string { return keys[k] }
	const hint = "OpenAI models are also funded by your OpenRouter key (billed per token): openrouter/gpt-astra-latest, openrouter/gpt-luna-latest, openrouter/gpt-sol-latest, openrouter/gpt-terra-latest. To use one, end this session with /exit, then run craze -c --model openrouter/gpt-sol-latest"
	if got := QuotaHint(table, "openai/gpt-6.1-sol", env); got != hint {
		t.Fatalf("QuotaHint =\n%q\nwant\n%q", got, hint)
	}
	if got := QuotaHint(table, "openrouter/gemini-flash-latest", env); got != "" {
		t.Fatalf("a model of no maker has a hint: %q", got)
	}
	if got := QuotaHint(table, "openai/gpt-6.1-sol", func(k string) string {
		if k == "OPENROUTER_API_KEY" {
			return ""
		}
		return keys[k]
	}); got != "" {
		t.Fatalf("no other funded source, and a hint: %q", got)
	}

	// The plan's model with the key and OpenRouter behind it: both routes,
	// the plan model's twin first.
	g := shippedFixture(t, keys, signedInWithList(t))
	planTable, err := modeltable.Load(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	msg := QuotaHint(planTable, "chatgpt/gpt-5.6-sol", env)
	if !strings.HasPrefix(msg, "OpenAI models are also funded by your OpenAI key (billed per token): openai/gpt-6-astra, ") ||
		!strings.Contains(msg, "; and by your OpenRouter key (billed per token): ") ||
		!strings.HasSuffix(msg, "craze -c --model openai/gpt-6.1-sol") {
		t.Fatalf("the plan model's hint = %q", msg)
	}
}

// TestNativeSwitchAwayDropsAModelNoLongerOffered (plan 050 C1, C5): a session
// started on a model that runs but is not offered lists it while it runs on
// it; a /model switch away drops its row, and the model cannot be switched
// back to. Both kinds: one the source order hides (openai/gpt-6.1-sol behind
// the ChatGPT plan's list) and a deprecated one (fireworks/kimi-k3). The
// control: an offered model switched away from keeps its row.
func TestNativeSwitchAwayDropsAModelNoLongerOffered(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		keys     map[string]string
		layDown  func(dir string)
		from, to string
	}{
		{"hidden by the source order", map[string]string{"OPENAI_API_KEY": dummyOpenAI, "OPENROUTER_API_KEY": dummyOpenRouter},
			signedInWithList(t), "openai/gpt-6.1-sol", "chatgpt/gpt-5.6-sol"},
		{"deprecated", map[string]string{"FIREWORKS_API_KEY": "fw-dummy-000000000001"}, nil, "fireworks/kimi-k3", "fireworks/kimi-k3p1-pro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shippedFixture(t, tc.keys, tc.layDown)
			s := f.started(Options{Model: tc.from})
			if ids := offeredIDs(s.Snapshot().Models); s.Snapshot().CurrentModel != tc.from || !slices.Contains(ids, tc.from) {
				t.Fatalf("premise: a session on %s (current %q) lists %q", tc.from, s.Snapshot().CurrentModel, ids)
			}
			if _, err := s.SetModel(ctx, "", tc.to); err != nil {
				t.Fatalf("SetModel(%s): %v", tc.to, err)
			}
			if ids := offeredIDs(s.Snapshot().Models); slices.Contains(ids, tc.from) || !slices.Contains(ids, tc.to) {
				t.Fatalf("after switching to %s the list %q still has %s", tc.to, ids, tc.from)
			}
			if _, err := s.SetModel(ctx, "", tc.from); err == nil {
				t.Fatalf("SetModel switched back to %s, which is no longer offered", tc.from)
			}
		})
	}
	t.Run("control: an offered model keeps its row", func(t *testing.T) {
		f := shippedFixture(t, map[string]string{"FIREWORKS_API_KEY": "fw-dummy-000000000001"}, nil)
		s := f.started(Options{Model: "fireworks/kimi-k3p1-pro"})
		if _, err := s.SetModel(ctx, "", "fireworks/glm-5p3"); err != nil {
			t.Fatal(err)
		}
		if ids := offeredIDs(s.Snapshot().Models); !slices.Contains(ids, "fireworks/kimi-k3p1-pro") {
			t.Fatalf("after switching away from an offered model the list %q lacks it", ids)
		}
	})
}
