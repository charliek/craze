package modeltable

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// liveTable is a table built in memory for the reload helpers (live.go): two
// key-funded providers and a model on each, the first with a cost.
func liveTable() *Table {
	in := 1.5
	return &Table{
		DefaultModel: "a/one",
		Providers: map[string]Provider{
			"a": {Driver: DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"LIVE_A_KEY"}},
			"b": {Driver: DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"LIVE_B_KEY"}},
		},
		Models: map[string]Model{
			"a/one": {Provider: "a", WireModel: "wire-one", Name: "One", ContextWindow: 64000,
				Efforts: []string{"low", "high"}, DefaultEffort: "high", Cost: &Cost{Input: &in}},
			"b/two": {Provider: "b", WireModel: "wire-two"},
		},
	}
}

// TestCarryKeepsTheRunningEntry (plan 034 §3.4, Q16, A20): the model a
// session runs on goes into the next table as it was — whether the next
// table dropped its alias and its provider, or points the alias at another
// model with other efforts — so it resolves, prices and lists as before, on
// the provider it had when the next table has none and on the next table's
// own when it has one. The control: a model the next table changed and that
// is not the running one keeps the next table's entry. Negative control: a
// Carry that keeps an entry the next table already has fails the re-pointed
// case.
func TestCarryKeepsTheRunningEntry(t *testing.T) {
	env := fakeEnv(map[string]string{"LIVE_A_KEY": "sk-live-a-key-0001", "LIVE_B_KEY": "sk-live-b-key-0002"})
	t.Run("its alias and its provider gone", func(t *testing.T) {
		from := liveTable()
		next := liveTable()
		delete(next.Models, "a/one")
		delete(next.Providers, "a")
		if !next.Carry(from, "a/one") {
			t.Fatal("Carry reported nothing carried")
		}
		if !reflect.DeepEqual(next.Models["a/one"], from.Models["a/one"]) {
			t.Fatalf("carried entry = %+v, want %+v", next.Models["a/one"], from.Models["a/one"])
		}
		if !reflect.DeepEqual(next.Providers["a"], from.Providers["a"]) {
			t.Fatalf("carried provider = %+v, want the running model's", next.Providers["a"])
		}
		r, err := next.Resolve("a/one", env)
		if err != nil || r.ContextWindow != 64000 || !slices.Equal(r.Efforts, []string{"low", "high"}) {
			t.Fatalf("Resolve(a/one) = %+v, %v; want the carried entry", r, err)
		}
		if _, ok := next.Price("a", "wire-one"); !ok {
			t.Fatal("the carried model is not priced")
		}
		if !slices.Contains(next.CredentialEnvNames(), "LIVE_A_KEY") {
			t.Fatal("the carried provider's variable is not a credential of the next table")
		}
	})
	t.Run("its alias re-pointed", func(t *testing.T) {
		from := liveTable()
		next := liveTable()
		next.Models["a/one"] = Model{Provider: "b", WireModel: "wire-other", Efforts: []string{"max"}, DefaultEffort: "max"}
		next.Models["b/two"] = Model{Provider: "b", WireModel: "wire-two", Name: "Two, renamed"}
		next.Providers["a"] = Provider{Driver: DriverOpenAICompat, BaseURL: "http://127.0.0.1:10/v1", EnvKeys: []string{"LIVE_A_KEY"}}
		next.Carry(from, "a/one")
		if !reflect.DeepEqual(next.Models["a/one"], from.Models["a/one"]) {
			t.Fatalf("carried entry = %+v, want the running model's %+v", next.Models["a/one"], from.Models["a/one"])
		}
		if next.Providers["a"].BaseURL != "http://127.0.0.1:10/v1" {
			t.Fatal("Carry replaced a provider the next table has: every other model of it goes by the next table's")
		}
		if next.Models["b/two"].Name != "Two, renamed" {
			t.Fatal("control: a model that is not the running one did not keep the next table's entry")
		}
	})
	t.Run("nothing to carry", func(t *testing.T) {
		next := liveTable()
		if next.Carry(liveTable(), "c/three") {
			t.Fatal("Carry reported an alias the source does not have as carried")
		}
		if _, ok := next.Models["c/three"]; ok {
			t.Fatal("Carry added an alias the source does not have")
		}
	})
}

// TestCarryKeepsTheAccountItWasBoundTo (plan 034 Q16, plan 033 X147): a plan
// model the running session's table learned from account A's list stays bound
// to A in the next table — which account B's sign-in loaded, B's list naming
// the same slug — so it resolves only while A is signed in, as its client's
// requests are held to A. The next table's own plan models are B's. The
// control: before the carry the slug resolves bound to B. Negative control:
// Resolve ignoring the carried binding resolves it on B's sign-in.
func TestCarryKeepsTheAccountItWasBoundTo(t *testing.T) {
	const alias = "chatgpt/gpt-5.6-sol"
	dir := signedInDir(t)
	from, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Account{Subject: planSubject, ClientID: planClient}
	b := Account{Subject: "user-subject-0002", ClientID: "app_client-0002"}
	signInAs(t, dir, registration(b.Subject, b.ClientID, true), true)
	withPlanModels(t, dir, planModels(b.Subject, b.ClientID))
	next, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := next.Resolve(alias, fakeEnv(nil)); err != nil || r.Account != b {
		t.Fatalf("control: before the carry %s resolved %+v, %v; want bound to B", alias, r.Account, err)
	}
	next.Carry(from, alias)
	if _, err := next.Resolve(alias, fakeEnv(nil)); !errors.Is(err, ErrOtherAccount) {
		t.Fatalf("the carried %s resolved on B's sign-in (%v); want ErrOtherAccount", alias, err)
	}
	if r, err := next.Resolve("chatgpt/gpt-6-astra", fakeEnv(nil)); err != nil || r.Account != b {
		t.Fatalf("the next table's own plan model resolved %+v, %v; want bound to B", r.Account, err)
	}
	signInAs(t, dir, registration(a.Subject, a.ClientID, true), true)
	if r, err := next.Resolve(alias, fakeEnv(nil)); err != nil || r.Account != a {
		t.Fatalf("with A signed in again the carried %s resolved %+v, %v; want bound to A", alias, r.Account, err)
	}
}

// TestWithholdFrozen (plan 034 §3.4, A22): a provider any of whose keys the
// session cannot take — an env value, or an inline key, inside what it has
// already sent — is dropped with its models; its variable reads as unset to
// Resolve and Keys, and is still a credential (CredentialEnvNames), so no
// command is started with it. The running model and its provider are kept
// whatever the provider holds. The control: a table with no frozen value is
// left alone. Negative control: a WithholdFrozen that only marks the variable
// leaves the provider's model listed and resolvable.
func TestWithholdFrozen(t *testing.T) {
	const frozenVal = "sk-inside-the-prompt-0001"
	env := fakeEnv(map[string]string{"LIVE_A_KEY": "sk-live-a-key-0001", "LIVE_B_KEY": frozenVal})
	frozen := func(v string) bool { return v == frozenVal }

	clean := liveTable()
	if got := clean.WithholdFrozen(fakeEnv(map[string]string{"LIVE_A_KEY": "sk-live-a-key-0001"}), frozen, "a/one"); got != nil || len(clean.Models) != 2 {
		t.Fatalf("control: a table with no frozen key lost %v", got)
	}

	tbl := liveTable()
	tbl.Providers["c"] = Provider{Driver: DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", APIKey: Secret(frozenVal)}
	tbl.Models["c/three"] = Model{Provider: "c", WireModel: "wire-three"}
	got := tbl.WithholdFrozen(env, frozen, "a/one")
	if !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("withheld %v, want [b c]", got)
	}
	for _, alias := range []string{"b/two", "c/three"} {
		if _, ok := tbl.Models[alias]; ok {
			t.Fatalf("%s is still in the table", alias)
		}
		if _, err := tbl.Resolve(alias, env); !errors.Is(err, ErrUnknownModel) {
			t.Fatalf("Resolve(%s) = %v, want unknown", alias, err)
		}
	}
	if !slices.Contains(tbl.CredentialEnvNames(), "LIVE_B_KEY") {
		t.Fatal("the withheld provider's variable is no longer a credential: a command would be started with it")
	}
	keys, err := tbl.Keys(env)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(keys, func(k Secret) bool { return k.Reveal() == frozenVal }) {
		t.Fatal("Keys returned the frozen value")
	}
	if c := tbl.Choices(nil, env, "a/one"); len(c) != 1 || c[0].Alias != "a/one" {
		t.Fatalf("Choices = %+v, want the running model alone", c)
	}

	kept := liveTable()
	if got := kept.WithholdFrozen(env, frozen, "b/two"); got != nil {
		t.Fatalf("withheld %v: the running model's provider must stay", got)
	}
	if _, ok := kept.Models["b/two"]; !ok {
		t.Fatal("the running model was dropped")
	}
	if _, err := kept.Resolve("b/two", env); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("Resolve(b/two) = %v; want its frozen variable read as unset", err)
	}
	if c := kept.Choices(nil, env, "b/two"); !slices.ContainsFunc(c, func(c Choice) bool { return c.Alias == "b/two" }) {
		t.Fatal("the running model is not listed (P7)")
	}
}
