package modeltable

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Plan 050 C5: makers and the source order — the catalog's rules for them,
// the merge (stamping, clearing), the user's source_order, the offered set's
// maker half, the start rules (the memory, the pin, the paid-source guard)
// over every sign-in state of the first source, with and without keys for
// the later ones. The synthetic catalog has three sources of one maker, the
// ChatGPT plan, a key provider and a router, and the router has a model of
// no maker; the shipped catalog's machines close the file.

const makerCatalog = `version = 1
provider_order = ["chatgpt", "acme", "router"]
source_order = ["chatgpt", "acme", "router"]

[providers.chatgpt]
name = "ChatGPT plan"
driver = "chatgpt"
maker = "openai"

[providers.acme]
name = "Acme"
driver = "openai-compat"
base_url = "https://api.acme.example/v1"
env_keys = ["ACME_API_KEY"]
start = ["acme/sol"]
maker = "openai"

[providers.router]
name = "Router"
driver = "openrouter"
env_keys = ["ROUTER_API_KEY"]
start = ["router/gem"]

[chatgpt_defaults]
models_client_version = "0.160.0"
start = ["gpt-5.6-sol"]

[models."acme/sol"]
provider = "acme"
wire_model = "sol"

[models."acme/old"]
provider = "acme"
wire_model = "old"
deprecated = true
replaced_by = "acme/sol"

[models."router/sol"]
provider = "router"
wire_model = "openai/sol"
maker = "openai"

[models."router/gem"]
provider = "router"
wire_model = "google/gem"
`

// The ChatGPT plan's models the synthetic catalog's list gives
// (planModels).
var planListed = []string{"chatgpt/gpt-5.6-luna", "chatgpt/gpt-5.6-sol", "chatgpt/gpt-6-astra"}

// signIn is a state of the first source, the ChatGPT plan, laid down in dir.
type signIn string

const (
	signedOut    signIn = "no sign-in"
	signedInList signIn = "signed in with a usable list"
	signedInNone signIn = "signed in with no list"
	signedOutOff signIn = "signed out, plan usage declined before"
	otherAccount signIn = "signed in with another account's list"
	planOff      signIn = "signed in with plan usage off"
)

var signIns = []signIn{signedOut, signedInList, signedInNone, otherAccount, planOff, signedOutOff}

// layDown writes dir's sign-in state s.
func (s signIn) layDown(t *testing.T, dir string) {
	t.Helper()
	switch s {
	case signedInList:
		signInAs(t, dir, registration(planSubject, planClient, true), true)
		withPlanModels(t, dir, planModels(planSubject, planClient))
	case signedInNone:
		signInAs(t, dir, registration(planSubject, planClient, true), true)
	case otherAccount:
		signInAs(t, dir, registration("someone-else", planClient, true), true)
		withPlanModels(t, dir, planModels(planSubject, planClient))
	case planOff:
		signInAs(t, dir, registration(planSubject, planClient, false), true)
		withPlanModels(t, dir, planModels(planSubject, planClient))
	case signedOutOff:
		// A sign-out keeps the registration and removes the tokens.
		signInAs(t, dir, registration(planSubject, planClient, false), false)
		withPlanModels(t, dir, planModels(planSubject, planClient))
	}
}

// makerTable loads providers and models (each "" for none) over the
// synthetic catalog in a directory signed in as s.
func makerTable(t *testing.T, s signIn, providers, models string) *Table {
	t.Helper()
	dir := writeFiles(t, providers, models)
	s.layDown(t, dir)
	tbl, err := LoadWith(dir, testCatalog(t, makerCatalog))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	return tbl
}

// keys is an environment funding the synthetic catalog's key providers.
func keys(acme, router bool) func(string) string {
	env := map[string]string{}
	if acme {
		env["ACME_API_KEY"] = "sk-acme-dummy-0001"
	}
	if router {
		env["ROUTER_API_KEY"] = "sk-router-dummy-0001"
	}
	return fakeEnv(env)
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// TestCatalogMakerRules: what a catalog may say of makers and source_order.
// One that names no maker needs no source_order (every other test catalog);
// one that names any lists every provider in it exactly once.
func TestCatalogMakerRules(t *testing.T) {
	testCatalog(t, testCatalogV1) // no maker, no source_order: valid
	for _, tc := range []struct {
		name       string
		mutate     func(*Catalog)
		table, key string
	}{
		{"a maker and no source_order", func(c *Catalog) { c.SourceOrder = nil }, "", "source_order"},
		{"a model's maker and no source_order", func(c *Catalog) {
			c.SourceOrder, c.ProviderMakers = nil, nil
		}, "", "source_order"},
		{"an empty source_order", func(c *Catalog) { c.SourceOrder = []string{} }, "", "source_order"},
		{"a provider twice", func(c *Catalog) { c.SourceOrder = append(c.SourceOrder, "acme") }, "", "source_order"},
		{"an unknown provider", func(c *Catalog) { c.SourceOrder = append(c.SourceOrder, "nope") }, "", "source_order"},
		{"a provider left out", func(c *Catalog) { c.SourceOrder = c.SourceOrder[:2] }, "", "source_order"},
		{"a provider's maker that is no word", func(c *Catalog) { c.ProviderMakers["acme"] = "Open AI" }, "providers.acme", "maker"},
		{"a model's maker that is no word", func(c *Catalog) {
			setEntry(c.Models, "router/gem", func(m *Model) { m.Maker = "Google!" })
		}, `models."router/gem"`, "maker"},
		{"a maker for no provider", func(c *Catalog) { c.ProviderMakers["gone"] = "openai" }, "providers.gone", "maker"},
		{"env_key_by_name_only for no provider", func(c *Catalog) {
			c.EnvKeyByNameOnly = map[string]bool{"gone": true}
		}, "providers.gone", "env_key_by_name_only"},
		{"env_key_by_name_only on a sign-in, which names no variable", func(c *Catalog) {
			c.EnvKeyByNameOnly = map[string]bool{"chatgpt": true}
		}, "providers.chatgpt", "env_key_by_name_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCatalog(t, makerCatalog).Clone()
			tc.mutate(c)
			wantFileError(t, c.validate(CatalogFile), CatalogFile, tc.table, tc.key)
		})
	}
	c := testCatalog(t, makerCatalog)
	if c.ProviderMakers["chatgpt"] != "openai" || c.Models["router/sol"].Maker != "openai" || c.Models["acme/sol"].Maker != "" {
		t.Fatalf("decoded makers: providers %v, router/sol %q, acme/sol %q (stamped at the merge, not here)",
			c.ProviderMakers, c.Models["router/sol"].Maker, c.Models["acme/sol"].Maker)
	}
	if clone := c.Clone(); !slices.Equal(clone.SourceOrder, c.SourceOrder) || clone.ProviderMakers["acme"] != "openai" {
		t.Fatalf("Clone dropped the source rule: %+v", clone)
	}
	marked := testCatalog(t, strings.Replace(makerCatalog, "start = [\"acme/sol\"]\n", "start = [\"acme/sol\"]\nenv_key_by_name_only = true\n", 1))
	if clone := marked.Clone(); !maps.Equal(marked.EnvKeyByNameOnly, map[string]bool{"acme": true}) || !maps.Equal(clone.EnvKeyByNameOnly, marked.EnvKeyByNameOnly) {
		t.Fatalf("env_key_by_name_only decoded as %v, cloned as %v; want acme's", marked.EnvKeyByNameOnly, clone.EnvKeyByNameOnly)
	}
}

// TestUserFilesCannotNameAMaker: maker is the catalog's alone, in either
// file.
func TestUserFilesCannotNameAMaker(t *testing.T) {
	cat := testCatalog(t, makerCatalog)
	for _, tc := range []struct{ providers, models, file, table string }{
		{"", "version = 1\n\n[models.\"acme/sol\"]\nmaker = \"meta\"\n", ModelsFile, `models."acme/sol"`},
		{"version = 1\n\n[providers.acme]\nmaker = \"meta\"\n", "", ProvidersFile, "providers.acme"},
	} {
		dir := writeFiles(t, tc.providers, tc.models)
		_, err := LoadWith(dir, cat)
		if fe := wantFileError(t, err, filepath.Join(dir, tc.file), tc.table, "maker"); fe.Reason != "unknown key" {
			t.Fatalf("%v, want an unknown key", err)
		}
	}
}

// TestMakersFollowTheShippedAlias: a maker is stamped on every model of the
// catalog's copy — a provider's on each of its models, the ChatGPT plan's
// list's included — and an override that keeps the alias's model keeps it;
// one that re-points the alias, or moves its provider's driver or base_url,
// clears it, the provider's own included; a model of the user's own has none,
// even on a tagged provider.
func TestMakersFollowTheShippedAlias(t *testing.T) {
	tbl := makerTable(t, signedInList, "", "")
	for alias, want := range map[string]string{
		"acme/sol": "openai", "acme/old": "openai", "router/sol": "openai", "router/gem": "",
		"chatgpt/gpt-5.6-sol": "openai", "chatgpt/gpt-6-astra": "openai",
	} {
		if got := tbl.Models[alias].Maker; got != want {
			t.Errorf("%s maker = %q, want %q", alias, got, want)
		}
	}
	renamed := makerTable(t, signedOut, "", "version = 1\n\n[models.\"router/sol\"]\nname = \"Mine\"\n")
	if renamed.Models["router/sol"].Maker != "openai" {
		t.Fatal("an override that keeps the model dropped its maker")
	}
	for _, tc := range []struct{ name, providers, models, alias string }{
		{"a re-pointed wire model", "", "version = 1\n\n[models.\"router/sol\"]\nwire_model = \"other/sol\"\n", "router/sol"},
		{"another provider", "", "version = 1\n\n[models.\"router/sol\"]\nprovider = \"acme\"\nwire_model = \"x\"\n", "router/sol"},
		{"a moved base_url", "version = 1\n\n[providers.acme]\nbase_url = \"https://gateway.example/v1\"\nenv_keys = [\"ACME_API_KEY\"]\n", "", "acme/sol"},
		{"a moved driver", "version = 1\n\n[providers.acme]\ndriver = \"openai-responses\"\nbase_url = \"https://api.acme.example/v1\"\nenv_keys = [\"ACME_API_KEY\"]\n", "", "acme/old"},
		{"a model of the user's own", "", "version = 1\n\n[models.mine]\nprovider = \"acme\"\nwire_model = \"mine\"\n", "mine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := makerTable(t, signedOut, tc.providers, tc.models)
			if m, ok := tbl.Models[tc.alias]; !ok || m.Maker != "" {
				t.Fatalf("%s = %+v (present %v), want no maker", tc.alias, m, ok)
			}
			if strings.Contains(tc.providers, "acme") && tbl.providerMakers["acme"] != "" {
				t.Fatalf("a moved provider kept its maker: %v", tbl.providerMakers)
			}
		})
	}
	// What craze learned of an identity holds no maker: a user's alias on the
	// tagged model's identity is still the user's own, with none.
	twin := makerTable(t, signedOut, "", "version = 1\n\n[models.twin]\nprovider = \"router\"\nwire_model = \"openai/sol-2\"\n")
	if twin.Models["twin"].Maker != "" {
		t.Fatal("a model of the user's own took a maker")
	}
}

// TestUserSourceOrder: the user's source_order against the catalog's — absent
// (the catalog's), empty (the rule off), partial (first, the catalog's rest
// after), a duplicate (a load error), an unknown id (one warning) — and Save
// keeps what the file wrote.
func TestUserSourceOrder(t *testing.T) {
	env := keys(true, true)
	for _, tc := range []struct {
		name, models string
		order        []string
		offered      []string
		warnings     int
	}{
		{"absent", "", []string{"chatgpt", "acme", "router"}, []string{"acme/sol", "router/gem"}, 0},
		{"empty: the rule off", "source_order = []", nil, []string{"acme/sol", "router/gem", "router/sol"}, 0},
		{"partial: first, the rest after", `source_order = ["router"]`, []string{"router", "chatgpt", "acme"}, []string{"router/gem", "router/sol"}, 0},
		{"an unknown id", `source_order = ["nope", "router"]`, []string{"router", "chatgpt", "acme"}, []string{"router/gem", "router/sol"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := ""
			if tc.models != "" {
				models = "version = 1\n" + tc.models + "\n"
			}
			tbl := makerTable(t, signedOut, "", models)
			if len(tbl.Warnings) != tc.warnings {
				t.Fatalf("warnings = %q, want %d", tbl.Warnings, tc.warnings)
			}
			if tc.warnings > 0 && !strings.Contains(tbl.Warnings[0], `source_order: "nope" is not a provider`) {
				t.Fatalf("warning = %q", tbl.Warnings[0])
			}
			if got := tbl.EffectiveSourceOrder(); !slices.Equal(got, tc.order) {
				t.Fatalf("EffectiveSourceOrder = %q, want %q", got, tc.order)
			}
			if got := sortedKeys(tbl.Offered(env)); !slices.Equal(got, tc.offered) {
				t.Fatalf("Offered = %q, want %q", got, tc.offered)
			}
			// Save writes back exactly what the file wrote.
			out := filepath.Join(t.TempDir(), "native")
			if err := Save(out, tbl); err != nil {
				t.Fatal(err)
			}
			again, err := LoadWith(out, testCatalog(t, makerCatalog))
			if err != nil {
				t.Fatal(err)
			}
			if (again.SourceOrder == nil) != (tbl.SourceOrder == nil) || !slices.Equal(again.SourceOrder, tbl.SourceOrder) {
				t.Fatalf("source_order after a round trip = %#v, want %#v", again.SourceOrder, tbl.SourceOrder)
			}
		})
	}
	dir := writeFiles(t, "", "version = 1\nsource_order = [\"acme\", \"acme\"]\n")
	_, err := LoadWith(dir, testCatalog(t, makerCatalog))
	wantFileError(t, err, filepath.Join(dir, ModelsFile), "", "source_order")
}

// TestOfferedOverEverySignInState: the offered set over each sign-in state of
// the first source, with and without keys for the later ones. The winner is
// judged by models that resolve, never by a connected provider: a sign-in
// with no usable list, another account's list or plan usage off offers
// nothing, and the next source with a funded model wins. A model of no maker
// is never hidden.
func TestOfferedOverEverySignInState(t *testing.T) {
	type want struct {
		offered []string
		winner  string
	}
	both := keys(true, true)
	for _, tc := range []struct {
		s    signIn
		env  func(string) string
		want want
	}{
		{signedOut, both, want{[]string{"acme/sol", "router/gem"}, "acme"}},
		{signedOut, keys(false, true), want{[]string{"router/gem", "router/sol"}, "router"}},
		{signedOut, keys(false, false), want{nil, ""}},
		{signedInList, both, want{append(slices.Clone(planListed), "router/gem"), "chatgpt"}},
		{signedInList, keys(false, false), want{planListed, "chatgpt"}},
		{signedInNone, both, want{[]string{"acme/sol", "router/gem"}, "acme"}},
		{signedInNone, keys(false, true), want{[]string{"router/gem", "router/sol"}, "router"}},
		{otherAccount, both, want{[]string{"acme/sol", "router/gem"}, "acme"}},
		{planOff, both, want{[]string{"acme/sol", "router/gem"}, "acme"}},
		{planOff, keys(false, true), want{[]string{"router/gem", "router/sol"}, "router"}},
	} {
		t.Run(string(tc.s), func(t *testing.T) {
			tbl := makerTable(t, tc.s, "", "")
			o := tbl.Offers(tc.env)
			if got := sortedKeys(o.Offered); !slices.Equal(got, tc.want.offered) {
				t.Fatalf("Offered = %q, want %q", got, tc.want.offered)
			}
			if got := o.Winners["openai"]; got != tc.want.winner {
				t.Fatalf("OpenAI's source = %q, want %q", got, tc.want.winner)
			}
			for alias := range o.Hidden {
				if !o.Runs[alias] || o.Offered[alias] {
					t.Fatalf("%s is hidden but runs %v, offered %v", alias, o.Runs[alias], o.Offered[alias])
				}
			}
			// The current model is always listed, hidden or not.
			if got := choiceAliases(tbl.Choices(nil, tc.env, "router/sol")); !slices.Contains(got, "router/sol") {
				t.Fatalf("Choices of a session on router/sol = %q, want it listed", got)
			}
		})
	}
	// A source whose models of the maker are all deprecated does not win; the
	// next one does.
	tbl := makerTable(t, signedOut, "", "")
	setModel(tbl, "acme/sol", func(m *Model) { m.Deprecated, m.ReplacedBy = true, "router/sol" })
	if o := tbl.Offers(both); o.Winners["openai"] != "router" || !o.Offered["router/sol"] || o.Offered["acme/sol"] {
		t.Fatalf("with acme's OpenAI models all deprecated: source %q, offered %q", o.Winners["openai"], sortedKeys(o.Offered))
	}
}

// TestStartPickOverEverySignInState: where a new session with nothing
// remembered and no pin starts, over each sign-in state of the first source.
// A sign-in that is set up but offers none of OpenAI's models holds every
// later OpenAI source back (the paid-source guard): another maker's start
// still starts, and with none left StartPick is a *HeldError naming the
// sign-in and the model, not ErrNothingFunded.
func TestStartPickOverEverySignInState(t *testing.T) {
	covered := map[signIn]bool{}
	defer func() {
		for _, s := range signIns {
			if !covered[s] {
				t.Errorf("no case for %s", s)
			}
		}
	}()
	for _, tc := range []struct {
		s        signIn
		env      func(string) string
		want     string
		fallback bool
		held     string // the model a *HeldError names, "" for none
	}{
		{signedOut, keys(true, true), "acme/sol", false, ""},
		{signedOut, keys(true, false), "acme/sol", false, ""},
		{signedOut, keys(false, true), "router/gem", false, ""},
		{signedInList, keys(true, true), "chatgpt/gpt-5.6-sol", false, ""},
		{signedInList, keys(false, false), "chatgpt/gpt-5.6-sol", false, ""},
		{signedInNone, keys(true, true), "router/gem", false, ""},
		{signedInNone, keys(true, false), "", false, "acme/sol"},
		{signedInNone, keys(false, true), "router/gem", false, ""},
		{otherAccount, keys(true, false), "", false, "acme/sol"},
		{planOff, keys(true, false), "", false, "acme/sol"},
		{planOff, keys(true, true), "router/gem", false, ""},
		// Signed out: the registration left behind sets nothing up.
		{signedOutOff, keys(true, false), "acme/sol", false, ""},
	} {
		covered[tc.s] = true
		t.Run(string(tc.s), func(t *testing.T) {
			tbl := makerTable(t, tc.s, "", "")
			got, err := tbl.StartPick(nil, tc.env)
			if tc.held != "" {
				var h *HeldError
				if !errors.As(err, &h) || !errors.Is(err, ErrSourceHeld) || errors.Is(err, ErrNothingFunded) || errors.Is(err, ErrNoAPIKey) {
					t.Fatalf("StartPick = %+v, %v; want a *HeldError only", got, err)
				}
				if h.Alias != tc.held || h.Source != "chatgpt" || h.Maker != "openai" {
					t.Fatalf("HeldError = %+v", h)
				}
				return
			}
			if err != nil || got.Alias != tc.want || got.Fallback != tc.fallback {
				t.Fatalf("StartPick = %+v, %v; want %s, fallback %v", got, err, tc.want, tc.fallback)
			}
		})
	}
}

// TestStartPickStepsAndTheSourceRule: the memory skips a model the source
// order hides (its entry stays in the file) and still starts a remembered
// deprecated one; the pin is never held to the rule; the guard holds the
// sorted fallback and the last resort back too; `source_order = []` turns
// the hiding off but not the guard.
func TestStartPickStepsAndTheSourceRule(t *testing.T) {
	remember := func(tbl *Table, alias string) []RecentEntry {
		m := tbl.Models[alias]
		return []RecentEntry{{Alias: alias, Provider: m.Provider, WireModel: m.WireModel}}
	}
	t.Run("a remembered model on a hidden source", func(t *testing.T) {
		tbl := makerTable(t, signedOut, "", "")
		if got, err := tbl.StartPick(remember(tbl, "router/sol"), keys(true, true)); err != nil || got.Alias != "acme/sol" {
			t.Fatalf("StartPick = %+v, %v; want acme/sol, the source in force", got, err)
		}
		// The same entry starts it once its source is the one in force again.
		if got, err := tbl.StartPick(remember(tbl, "router/sol"), keys(false, true)); err != nil || got.Alias != "router/sol" {
			t.Fatalf("StartPick = %+v, %v; want router/sol", got, err)
		}
	})
	t.Run("a remembered plan model when the list is gone", func(t *testing.T) {
		listed := makerTable(t, signedInList, "", "")
		recent := remember(listed, "chatgpt/gpt-6-astra")
		tbl := makerTable(t, signedInNone, "", "")
		if _, err := tbl.StartPick(recent, keys(true, false)); !errors.Is(err, ErrSourceHeld) {
			t.Fatalf("StartPick = %v; want the guard's refusal, not the key", err)
		}
	})
	t.Run("a remembered deprecated model", func(t *testing.T) {
		tbl := makerTable(t, signedOut, "", "")
		if got, err := tbl.StartPick(remember(tbl, "acme/old"), keys(true, true)); err != nil || got.Alias != "acme/old" {
			t.Fatalf("StartPick = %+v, %v; want acme/old", got, err)
		}
	})
	t.Run("a pin on a hidden source", func(t *testing.T) {
		tbl := makerTable(t, signedInList, "", "version = 1\ndefault_model = \"router/sol\"\n")
		if got, err := tbl.StartPick(nil, keys(true, true)); err != nil || got.Alias != "router/sol" {
			t.Fatalf("StartPick = %+v, %v; want the pin", got, err)
		}
	})
	t.Run("a pin held by nothing", func(t *testing.T) {
		tbl := makerTable(t, signedInNone, "", "version = 1\ndefault_model = \"acme/sol\"\n")
		if got, err := tbl.StartPick(nil, keys(true, false)); err != nil || got.Alias != "acme/sol" {
			t.Fatalf("StartPick = %+v, %v; want the pin", got, err)
		}
	})
	t.Run("the sorted fallback and the last resort are held too", func(t *testing.T) {
		tbl := makerTable(t, signedInNone, "", `version = 1
provider_order = ["chatgpt"]
`)
		if _, err := tbl.StartPick(nil, keys(true, false)); !errors.Is(err, ErrSourceHeld) {
			t.Fatalf("the sorted fallback: %v, want held", err)
		}
		setModel(tbl, "acme/sol", func(m *Model) { m.Deprecated, m.ReplacedBy = true, "router/sol" })
		_, err := tbl.StartPick(nil, keys(true, false))
		var h *HeldError
		if !errors.As(err, &h) || h.Alias != "acme/old" {
			t.Fatalf("the last resort: %v, want acme/old held", err)
		}
	})
	t.Run("the hiding off, the guard on", func(t *testing.T) {
		tbl := makerTable(t, signedInNone, "", "version = 1\nsource_order = []\n")
		if _, err := tbl.StartPick(nil, keys(true, false)); !errors.Is(err, ErrSourceHeld) {
			t.Fatalf("StartPick = %v; want held", err)
		}
		if got := sortedKeys(tbl.Offered(keys(true, true))); !slices.Equal(got, []string{"acme/sol", "router/gem", "router/sol"}) {
			t.Fatalf("Offered = %q, want every source", got)
		}
	})
	t.Run("the key first", func(t *testing.T) {
		tbl := makerTable(t, signedInNone, "", "version = 1\nsource_order = [\"acme\"]\n")
		if got, err := tbl.StartPick(nil, keys(true, false)); err != nil || got.Alias != "acme/sol" {
			t.Fatalf("StartPick = %+v, %v; want acme/sol: the user put the key first", got, err)
		}
	})
	t.Run("a keyed source that offers none of the maker's models", func(t *testing.T) {
		// A key provider with a usable key holds nothing back: it has left
		// the maker, it is not broken (review F1).
		tbl := makerTable(t, signedOut, "", "version = 1\nprovider_order = [\"router\"]\n")
		delete(tbl.Models, "acme/sol")
		delete(tbl.Models, "acme/old")
		if _, err := tbl.StartPick(nil, keys(true, false)); !errors.Is(err, ErrNothingFunded) {
			t.Fatalf("StartPick = %v; want nothing funded", err)
		}
		got, err := tbl.StartPick(nil, keys(true, true))
		if err != nil || got.Alias != "router/gem" {
			t.Fatalf("StartPick = %+v, %v; want router/gem", got, err)
		}
		if h := tbl.Offers(keys(true, true)).Held["router/sol"]; h != nil {
			t.Fatalf("router/sol held by %+v; a keyed source holds nothing", h)
		}
	})
}

// TestKeyedSourceThatLeftAMakerHoldsNothing (review F1): keyed providers a
// and b, in that order for maker m; a's only model of m is deprecated, its
// replacement b/new active. With both keys and nothing remembered, b wins m,
// which hides a/old — and the guard must not then hold b/new back because a
// funds no active model of m: a has a working key, it has left the maker,
// and the session starts on the replacement the catalog names.
func TestKeyedSourceThatLeftAMakerHoldsNothing(t *testing.T) {
	cat := testCatalog(t, `version = 1
provider_order = ["a", "b"]
source_order = ["a", "b"]

[providers.a]
name = "A"
driver = "openai-compat"
base_url = "https://a.example/v1"
env_keys = ["A_API_KEY"]

[providers.b]
name = "B"
driver = "openai-compat"
base_url = "https://b.example/v1"
env_keys = ["B_API_KEY"]
start = ["b/new"]

[models."a/old"]
provider = "a"
wire_model = "old"
maker = "m"
deprecated = true
replaced_by = "b/new"

[models."b/new"]
provider = "b"
wire_model = "new"
maker = "m"
`)
	tbl, _ := loadOver(t, cat, "", "")
	env := fakeEnv(map[string]string{"A_API_KEY": "sk-a-dummy-0001", "B_API_KEY": "sk-b-dummy-0001"})
	got, err := tbl.StartPick(nil, env)
	if err != nil || got.Alias != "b/new" {
		t.Fatalf("StartPick = %+v, %v; want b/new", got, err)
	}
	if o := tbl.Offers(env); !o.Hidden["a/old"] || o.Held["b/new"] != nil {
		t.Fatalf("hidden %v, held %v", o.Hidden, o.Held)
	}
}

// TestHeldErrorText: the guard's refusal names the sign-in to repair and the
// alias to pass, and is not "no model provider has a key".
func TestHeldErrorText(t *testing.T) {
	h := &HeldError{Alias: "openai/gpt-6.1-sol", Maker: "openai", Source: "chatgpt"}
	if got, want := h.Reason(), "signed in to ChatGPT, but its models are not available"; got != want {
		t.Errorf("Reason = %q, want %q", got, want)
	}
	if got, want := h.Fix(), "craze auth login chatgpt, or start with --model openai/gpt-6.1-sol"; got != want {
		t.Errorf("Fix = %q, want %q", got, want)
	}
	if strings.Contains(h.Error(), "API key") || !strings.Contains(h.Error(), "openai/gpt-6.1-sol") {
		t.Errorf("Error = %q", h.Error())
	}
	// The environment's kind (guard 2, G1): the variable's name, the
	// provider's display name, and the same fix, which stores the key.
	env := &HeldError{Alias: "openai/gpt-6.1-sol", Maker: "openai", Source: "openai", SourceName: "OpenAI", EnvVar: "OPENAI_API_KEY"}
	if got, want := env.Reason(), "OPENAI_API_KEY is set in the environment, but no key is stored for OpenAI"; got != want {
		t.Errorf("Reason = %q, want %q", got, want)
	}
	if got, want := env.Fix(), "craze auth login openai, or start with --model openai/gpt-6.1-sol"; got != want {
		t.Errorf("Fix = %q, want %q", got, want)
	}
	if got, want := env.Error(), "modeltable: "+env.Reason()+", so craze does not choose openai/gpt-6.1-sol on its own"; got != want {
		t.Errorf("Error = %q, want %q", got, want)
	}
	if !errors.Is(env, ErrSourceHeld) {
		t.Error("the environment's kind does not unwrap to ErrSourceHeld")
	}
}

// guardLocal is a providers.toml with a provider of the user's own, funded
// by a stored dummy key, and guardLocalModel a models.toml line set giving
// it an unpriced model whose alias sorts after every shipped one.
const (
	guardLocal = `version = 1

[providers.local]
name = "Local"
driver = "openai-compat"
base_url = "http://127.0.0.1:8080/v1"
api_key = "sk-guard-local-dummy-0001"
`
	guardLocalModel = `[models.zz-local]
provider = "local"
wire_model = "qwen"
`
)

// TestPaidSourceGuardOverTheShippedCatalog (guard 2): where a new session
// with no --model starts on the machines the owner named, over the shipped
// catalog. G2: a ChatGPT sign-in that is set up but offers none of its
// models holds back every model billed per token — it, or any model of its
// provider, has a price — of a provider after it in the start order or left
// out of it, whatever its maker; a provider before it, a provider none of
// whose models has a price, the model memory and the pin are not held. The
// load's Anthropic key is read from the case's own environment
// (withAnthropicEnv), never the real one. G1: a key for openai
// found only in the environment is never chosen; stored, it is. A model held
// for both reasons names the sign-in. `source_order = []` changes none of
// it: the guard is not the hiding.
func TestPaidSourceGuardOverTheShippedCatalog(t *testing.T) {
	const (
		oaKey = "sk-guard-openai-dummy-0001"
		fw    = "FIREWORKS_API_KEY"
		or    = "OPENROUTER_API_KEY"
		zai   = "ZHIPU_API_KEY"
		an    = "ANTHROPIC_API_KEY"
	)
	storedOpenAI := "version = 1\n\n[providers.openai]\napi_key = \"" + oaKey + "\"\n"
	type held struct{ alias, source, envVar string }
	for _, tc := range []struct {
		name         string
		s            signIn
		keys         []string // set to dummy keys of their own
		openai       string   // OPENAI_API_KEY's value, "" unset
		providers    string   // providers.toml, "" none
		models       string   // models.toml's lines after its version
		remember     string   // the model memory's one entry, "" none
		start        string
		fallback     bool
		held         held
		holdsNothing bool // Offers holds no model back at all
		sourceOrder  bool // models writes its own source_order: no `source_order = []` run
		opus6        bool // the Anthropic account's list, bound to the case's key, names the seeds and Opus 6
	}{
		// G2: a broken ChatGPT sign-in.
		{name: "broken ChatGPT and a Fireworks key", s: signedInNone, keys: []string{fw},
			held: held{"fireworks/kimi-k3p1-pro", ChatGPTProvider, ""}},
		{name: "broken ChatGPT and an OpenRouter key", s: signedInNone, keys: []string{or},
			held: held{"openrouter/gemini-flash-latest", ChatGPTProvider, ""}},
		{name: "another account's list and a Fireworks key", s: otherAccount, keys: []string{fw},
			held: held{"fireworks/kimi-k3p1-pro", ChatGPTProvider, ""}},
		{name: "plan usage off and an OpenRouter key", s: planOff, keys: []string{or},
			held: held{"openrouter/gemini-flash-latest", ChatGPTProvider, ""}},
		{name: "broken ChatGPT and a Z.AI key, which comes before it", s: signedInNone, keys: []string{zai}, start: "glm-5.3"},
		{name: "broken ChatGPT and the user's unpriced local model", s: signedInNone, providers: guardLocal, models: guardLocalModel,
			start: "zz-local", fallback: true},
		{name: "broken ChatGPT after Fireworks in the user's provider_order", s: signedInNone, keys: []string{fw},
			models: `provider_order = ["fireworks", "chatgpt"]` + "\n", start: "fireworks/kimi-k3p1-pro"},
		// A model with no price of its own on a provider whose models are
		// priced is billed per token all the same (review F1): an Opus only
		// the account's list names, and an entry of the user's on Fireworks.
		{name: "broken ChatGPT, an Anthropic key and an unseeded Opus the account lists", s: signedInNone, keys: []string{an},
			opus6: true, held: held{"anthropic/claude-opus-6", ChatGPTProvider, ""}},
		{name: "broken ChatGPT, a Fireworks key and the user's unpriced model on Fireworks", s: signedInNone, keys: []string{fw},
			models: "[models.\"fireworks/aaa-mine\"]\nprovider = \"fireworks\"\nwire_model = \"accounts/me/models/mine\"\n",
			held:   held{"fireworks/kimi-k3p1-pro", ChatGPTProvider, ""}},
		// A provider the user's provider_order leaves out is held; a sign-in
		// it leaves out holds nothing (review F4).
		{name: "broken ChatGPT and a Fireworks key the user's provider_order leaves out", s: signedInNone, keys: []string{fw},
			models: `provider_order = ["zai-coding-plan", "chatgpt"]` + "\n", held: held{"fireworks/deepseek-v4p1-flash-1017", ChatGPTProvider, ""}},
		{name: "broken ChatGPT left out of the user's provider_order and a Fireworks key", s: signedInNone, keys: []string{fw},
			models: `provider_order = ["zai-coding-plan", "fireworks"]` + "\n", start: "fireworks/kimi-k3p1-pro"},
		{name: "broken ChatGPT and a remembered Fireworks model", s: signedInNone, keys: []string{fw},
			remember: "fireworks/kimi-k3p1-pro", start: "fireworks/kimi-k3p1-pro"},
		{name: "broken ChatGPT and a pin on OpenRouter", s: signedInNone, keys: []string{or},
			models: `default_model = "openrouter/gemini-flash-latest"` + "\n", start: "openrouter/gemini-flash-latest"},
		{name: "a working ChatGPT sign-in", s: signedInList, keys: []string{fw, or, an},
			start: "chatgpt/gpt-5.6-sol", holdsNothing: true},
		{name: "a registration a sign-out left", s: signedOutOff, keys: []string{fw, or},
			start: "fireworks/kimi-k3p1-pro", holdsNothing: true},
		// G1: OPENAI_API_KEY.
		{name: "OPENAI_API_KEY alone", s: signedOut, openai: oaKey,
			held: held{"openai/gpt-6.1-sol", "openai", "OPENAI_API_KEY"}},
		{name: "the user's local model and OPENAI_API_KEY", s: signedOut, openai: oaKey, providers: guardLocal, models: guardLocalModel,
			start: "zz-local", fallback: true},
		{name: "OPENAI_API_KEY and the same key stored", s: signedOut, openai: oaKey, providers: storedOpenAI,
			start: "openai/gpt-6.1-sol", holdsNothing: true},
		{name: "an unusable OPENAI_API_KEY and a key stored", s: signedOut, openai: "short", providers: storedOpenAI,
			start: "openai/gpt-6.1-sol", holdsNothing: true},
		{name: "a key stored for openai alone", s: signedOut, providers: storedOpenAI, start: "openai/gpt-6.1-sol", holdsNothing: true},
		{name: "OPENAI_API_KEY and a remembered openai/gpt-6-luna", s: signedOut, openai: oaKey,
			remember: "openai/gpt-6-luna", start: "openai/gpt-6-luna"},
		{name: "OPENAI_API_KEY and a pin on openai/gpt-6.1-terra", s: signedOut, openai: oaKey,
			models: `default_model = "openai/gpt-6.1-terra"` + "\n", start: "openai/gpt-6.1-terra"},
		{name: "OPENAI_API_KEY and a Fireworks key", s: signedOut, openai: oaKey, keys: []string{fw}, start: "fireworks/kimi-k3p1-pro"},
		{name: "FIREWORKS_API_KEY alone", s: signedOut, keys: []string{fw}, start: "fireworks/kimi-k3p1-pro", holdsNothing: true},
		{name: "OPENROUTER_API_KEY alone", s: signedOut, keys: []string{or}, start: "openrouter/gemini-flash-latest", holdsNothing: true},
		{name: "ANTHROPIC_API_KEY alone", s: signedOut, keys: []string{an}, start: "anthropic/claude-opus-5-5", holdsNothing: true},
		// Both reasons: the sign-in's wins — the per-maker rule's, and,
		// with the key first in the user's source_order, where that rule
		// holds nothing, the start order's.
		{name: "plan usage off and OPENAI_API_KEY", s: planOff, openai: oaKey,
			held: held{"openai/gpt-6.1-sol", ChatGPTProvider, ""}},
		{name: "plan usage off, OPENAI_API_KEY and the key first in source_order", s: planOff, openai: oaKey,
			models: `source_order = ["openai"]` + "\n", sourceOrder: true, held: held{"openai/gpt-6.1-sol", ChatGPTProvider, ""}},
	} {
		for _, hidingOff := range []bool{false, true} {
			if hidingOff && tc.sourceOrder {
				continue
			}
			name := tc.name
			if hidingOff {
				name += ", source_order = []"
			}
			t.Run(name, func(t *testing.T) {
				models := ""
				if hidingOff {
					models = "source_order = []\n"
				}
				if models += tc.models; models != "" {
					models = "version = 1\n" + models
				}
				vars := map[string]string{}
				for i, k := range tc.keys {
					vars[k] = fmt.Sprintf("sk-guard-dummy-%04d", i+1)
				}
				if tc.openai != "" {
					vars["OPENAI_API_KEY"] = tc.openai
				}
				env := withAnthropicEnv(t, vars)
				dir := writeFiles(t, tc.providers, models)
				tc.s.layDown(t, dir)
				if tc.opus6 {
					writeAnthropicList(t, dir, vars[an], append(seedEntries(t), opus6()))
				}
				tbl, err := Load(dir)
				if err != nil {
					t.Fatal(err)
				}
				if tc.opus6 {
					if m, ok := tbl.Models["anthropic/claude-opus-6"]; !ok || m.Cost != nil {
						t.Fatalf("premise: anthropic/claude-opus-6 = %+v (present %v), want a model with no price", m, ok)
					}
				}
				if strings.Contains(tc.models, "aaa-mine") {
					if m, ok := tbl.Models["fireworks/aaa-mine"]; !ok || m.Cost != nil || !tbl.Offers(env).Offered["fireworks/aaa-mine"] {
						t.Fatalf("premise: fireworks/aaa-mine = %+v (present %v), want an offered model with no price", m, ok)
					}
				}
				var recent []RecentEntry
				if tc.remember != "" {
					m := tbl.Models[tc.remember]
					recent = []RecentEntry{{Alias: tc.remember, Provider: m.Provider, WireModel: m.WireModel}}
				}
				if tc.holdsNothing {
					if h := tbl.Offers(env).Held; len(h) != 0 {
						t.Fatalf("Held = %v, want nothing held", h)
					}
				}
				got, err := tbl.StartPick(recent, env)
				if tc.held.alias == "" {
					if err != nil || got.Alias != tc.start || got.Fallback != tc.fallback {
						t.Fatalf("StartPick = %+v, %v; want %s, fallback %v", got, err, tc.start, tc.fallback)
					}
					return
				}
				var h *HeldError
				if !errors.As(err, &h) || !errors.Is(err, ErrSourceHeld) || errors.Is(err, ErrNothingFunded) || errors.Is(err, ErrNoAPIKey) {
					t.Fatalf("StartPick = %+v, %v; want a *HeldError only", got, err)
				}
				if h.Alias != tc.held.alias || h.Source != tc.held.source || h.EnvVar != tc.held.envVar {
					t.Fatalf("HeldError = %+v, want %+v", h, tc.held)
				}
				if strings.Contains(err.Error(), oaKey) || strings.Contains(err.Error(), "sk-guard-dummy") {
					t.Fatalf("%q carries a key", err)
				}
			})
		}
	}
}

// TestEnvOnlyKeyIsHeldNotHidden (guard 2, G1): an openai key found only in
// the environment changes what craze chooses, never what runs or what is
// offered: each of openai's models still resolves and is listed, and only
// the guard holds it — the deprecated ones too — with the environment's
// reason. A key stored for it, alone or beside the variable, holds nothing.
func TestEnvOnlyKeyIsHeldNotHidden(t *testing.T) {
	const key = "sk-guard-openai-dummy-0002"
	env := withAnthropicEnv(t, map[string]string{"OPENAI_API_KEY": key})
	tbl, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := tbl.Offers(env)
	n := 0
	for _, alias := range tbl.Aliases() {
		if tbl.Models[alias].Provider != "openai" {
			continue
		}
		n++
		h := o.Held[alias]
		if !o.Runs[alias] || h == nil || h.EnvVar != "OPENAI_API_KEY" || h.Source != "openai" || h.SourceName != "OpenAI" || h.Maker != "openai" {
			t.Errorf("%s: runs %v, held %+v; want it to run, held by the environment", alias, o.Runs[alias], h)
		}
		if !tbl.Models[alias].Deprecated && !o.Offered[alias] {
			t.Errorf("%s is not offered", alias)
		}
		if _, err := tbl.Resolve(alias, env); err != nil {
			t.Errorf("%s does not resolve: %v", alias, err)
		}
	}
	if n == 0 {
		t.Fatal("the shipped catalog has no openai model; the case proves nothing")
	}
	if len(o.Held) != n {
		t.Fatalf("Held = %v; want openai's %d models alone", o.Held, n)
	}
	stored, err := Load(writeFiles(t, "version = 1\n\n[providers.openai]\napi_key = \""+key+"\"\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []func(string) string{fakeEnv(nil), env} {
		if h := stored.Offers(e).Held; len(h) != 0 {
			t.Fatalf("with a key stored, Held = %v", h)
		}
	}
}

// TestEnvKeyByNameOnlyIsTheCatalogs (guard 2, G1): env_key_by_name_only is
// treated as a provider's maker is — read from the catalog only, refused in
// a user's providers.toml as an unknown key, never written by Save, kept by
// an entry that leaves the endpoint as shipped (spelled otherwise
// included), and dropped by one that moves the driver or base_url, whose
// endpoint, and key, are then the user's own.
func TestEnvKeyByNameOnlyIsTheCatalogs(t *testing.T) {
	const key = "sk-guard-openai-dummy-0003"
	env := withAnthropicEnv(t, map[string]string{"OPENAI_API_KEY": key})
	dir := writeFiles(t, "version = 1\n\n[providers.openai]\nenv_key_by_name_only = false\n", "")
	_, err := Load(dir)
	if fe := wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.openai", "env_key_by_name_only"); fe.Reason != "unknown key" {
		t.Fatalf("%v, want an unknown key", err)
	}
	for _, tc := range []struct {
		name, providers string
		marked          bool
	}{
		{"no entry", "", true},
		{"a new name", "version = 1\n\n[providers.openai]\nname = \"Mine\"\n", true},
		{"the endpoint spelled otherwise", "version = 1\n\n[providers.openai]\nbase_url = \"HTTPS://API.OpenAI.com/v1/\"\n", true},
		{"a moved base_url", "version = 1\n\n[providers.openai]\nbase_url = \"https://gateway.example/v1\"\nenv_keys = [\"OPENAI_API_KEY\"]\n", false},
		{"a moved driver", "version = 1\n\n[providers.openai]\ndriver = \"openai-compat\"\nbase_url = \"https://api.openai.com/v1\"\nenv_keys = [\"OPENAI_API_KEY\"]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, err := Load(writeFiles(t, tc.providers, ""))
			if err != nil {
				t.Fatal(err)
			}
			if tbl.envKeyByNameOnly["openai"] != tc.marked {
				t.Fatalf("openai marked %v, want %v", tbl.envKeyByNameOnly["openai"], tc.marked)
			}
			_, err = tbl.StartPick(nil, env)
			if held := errors.Is(err, ErrSourceHeld); held != tc.marked {
				t.Fatalf("StartPick: %v; want held %v", err, tc.marked)
			}
			out := filepath.Join(t.TempDir(), "native")
			if err := Save(out, tbl); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{ProvidersFile, ModelsFile} {
				b, err := os.ReadFile(filepath.Join(out, f))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if strings.Contains(string(b), "env_key_by_name_only") {
					t.Fatalf("Save wrote env_key_by_name_only into %s:\n%s", f, b)
				}
			}
			again, err := Load(out)
			if err != nil {
				t.Fatal(err)
			}
			if again.envKeyByNameOnly["openai"] != tc.marked {
				t.Fatalf("after Save, openai marked %v, want %v", again.envKeyByNameOnly["openai"], tc.marked)
			}
		})
	}
}

// TestSourceRuleOverTheShippedCatalog: the shipped order and makers on the
// machines plan 050 names — where a new session with nothing remembered and
// no pin starts, and which of the makers' models a picker lists.
func TestSourceRuleOverTheShippedCatalog(t *testing.T) {
	const (
		oa  = "OPENAI_API_KEY"
		or  = "OPENROUTER_API_KEY"
		fw  = "FIREWORKS_API_KEY"
		zai = "ZHIPU_API_KEY"
		mt  = "META_API_KEY"
	)
	openaiKey := []string{"openai/gpt-6-astra", "openai/gpt-6-luna", "openai/gpt-6.1-sol", "openai/gpt-6.1-terra"}
	gptRouters := []string{"openrouter/gpt-astra-latest", "openrouter/gpt-luna-latest", "openrouter/gpt-sol-latest", "openrouter/gpt-terra-latest"}
	museRouters := []string{"openrouter/muse-spark-1.3", "openrouter/muse-wonder"}
	glmRouters := []string{"openrouter/glm-flash-latest", "openrouter/glm-latest"}
	for _, tc := range []struct {
		name           string
		s              signIn
		keys           []string
		start          string
		held           string
		offered, hides []string
	}{
		{"ChatGPT with its list, an OpenAI key and an OpenRouter key", signedInList, []string{oa, or}, "chatgpt/gpt-5.6-sol", "",
			append(slices.Clone(planListed), museRouters...), append(slices.Clone(openaiKey), gptRouters...)},
		// The sign-in with no list holds OpenRouter's start back too: it is
		// priced per token and after chatgpt in the start order (guard 2).
		{"ChatGPT with no list, an OpenAI key and an OpenRouter key", signedInNone, []string{oa, or}, "", "openrouter/gemini-flash-latest",
			append(slices.Clone(openaiKey), museRouters...), gptRouters},
		{"plan usage off and an OpenAI key", planOff, []string{oa}, "", "openai/gpt-6.1-sol", openaiKey, nil},
		{"an OpenRouter key alone", signedOut, []string{or}, "openrouter/gemini-flash-latest", "",
			append(slices.Clone(gptRouters), append(museRouters, glmRouters...)...), nil},
		{"Z.AI and Fireworks", signedOut, []string{zai, fw}, "glm-5.3", "",
			[]string{"glm-5.3", "glm-5.3-flash", "fireworks/deepseek-v4p1-flash-1017"}, []string{"fireworks/glm-5p3", "fireworks/glm-5p3-flash"}},
		{"Meta and OpenRouter", signedOut, []string{mt, or}, "openrouter/gemini-flash-latest", "",
			[]string{"muse-spark-1.3", "muse-wonder-1.0", "openrouter/gpt-sol-latest"}, museRouters},
		{"Fireworks and OpenRouter", signedOut, []string{fw, or}, "fireworks/kimi-k3p1-pro", "",
			[]string{"fireworks/glm-5p3", "fireworks/deepseek-v4p1-flash-1017", "openrouter/gpt-sol-latest", "openrouter/muse-wonder"},
			append(slices.Clone(glmRouters), "openrouter/deepseek-flash-latest")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.s.layDown(t, dir)
			tbl, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, k := range tc.keys {
				env[k] = "sk-source-dummy-0001"
			}
			o := tbl.Offers(fakeEnv(env))
			for _, alias := range tc.offered {
				if !o.Offered[alias] {
					t.Errorf("%s is not offered", alias)
				}
			}
			for _, alias := range tc.hides {
				if !o.Hidden[alias] || o.Offered[alias] {
					t.Errorf("%s is not hidden (runs %v)", alias, o.Runs[alias])
				}
			}
			got, err := tbl.StartPick(nil, fakeEnv(env))
			if tc.held != "" {
				var h *HeldError
				if !errors.As(err, &h) || h.Alias != tc.held || h.Source != ChatGPTProvider {
					t.Fatalf("StartPick = %+v, %v; want %s held by the sign-in", got, err, tc.held)
				}
				return
			}
			if err != nil || got.Alias != tc.start {
				t.Fatalf("StartPick = %+v, %v; want %s", got, err, tc.start)
			}
		})
	}
}

// TestOpenAIsOwnEndpointSpelledOtherwiseKeepsItsMaker (plan 050 review): a
// [providers.openai] entry whose base_url is OpenAI's own endpoint spelled
// otherwise — a trailing slash, the host in upper case — has not moved the
// provider (sameEndpoint): its models keep their maker, so the source order
// still hides them behind the ChatGPT plan's list, and with a ChatGPT
// sign-in that has no list the paid-source guard still refuses to start on
// the key's model. Its key funds them, and the shipped variable still does.
func TestOpenAIsOwnEndpointSpelledOtherwiseKeepsItsMaker(t *testing.T) {
	const key = "sk-openai-dummy-00000009"
	for _, base := range []string{"https://api.openai.com/v1/", "HTTPS://API.OpenAI.com/v1"} {
		t.Run(base, func(t *testing.T) {
			load := func(s signIn) *Table {
				t.Helper()
				dir := writeFiles(t, "version = 1\n\n[providers.openai]\nbase_url = \""+base+"\"\napi_key = \""+key+"\"\n", "")
				s.layDown(t, dir)
				tbl, err := Load(dir)
				if err != nil {
					t.Fatal(err)
				}
				return tbl
			}
			listed := load(signedInList)
			if m := listed.Models["openai/gpt-6.1-sol"]; m.Maker != "openai" {
				t.Fatalf("openai/gpt-6.1-sol's maker = %q, want openai", m.Maker)
			}
			if p := listed.Providers["openai"]; !slices.Equal(p.EnvKeys, []string{"OPENAI_API_KEY"}) {
				t.Fatalf("openai = %+v; want the shipped variable still its own", p)
			}
			if o := listed.Offers(fakeEnv(nil)); !o.Hidden["openai/gpt-6.1-sol"] || o.Offered["openai/gpt-6.1-sol"] {
				t.Fatalf("openai/gpt-6.1-sol: hidden %v, offered %v; want hidden behind the plan's list", o.Hidden["openai/gpt-6.1-sol"], o.Offered["openai/gpt-6.1-sol"])
			}
			got, err := load(signedInNone).StartPick(nil, fakeEnv(nil))
			var h *HeldError
			if !errors.As(err, &h) || h.Alias != "openai/gpt-6.1-sol" || h.Source != ChatGPTProvider {
				t.Fatalf("StartPick = %+v, %v; want openai/gpt-6.1-sol held by the sign-in", got, err)
			}
		})
	}
}

// TestSourceRuleReadsTheSignInNow: the sets follow the sign-in as it is at
// each call — a list written mid-process moves OpenAI's source to the plan
// at the next call, with no new load (the list is the load's, so a load
// that read none still has none: the guard holds the key back until then).
func TestSourceRuleReadsTheSignInNow(t *testing.T) {
	dir := t.TempDir()
	signedInNone.layDown(t, dir)
	tbl, err := LoadWith(dir, testCatalog(t, makerCatalog))
	if err != nil {
		t.Fatal(err)
	}
	env := keys(true, false)
	if o := tbl.Offers(env); !o.Offered["acme/sol"] || o.Held["acme/sol"] == nil {
		t.Fatalf("before: offered %q, held %v", sortedKeys(o.Offered), o.Held)
	}
	if err := os.Remove(filepath.Join(dir, ChatGPTAuthDir, ChatGPTTokenFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ChatGPTAuthDir, ChatGPTClientFile)); err != nil {
		t.Fatal(err)
	}
	if got, err := tbl.StartPick(nil, env); err != nil || got.Alias != "acme/sol" {
		t.Fatalf("after signing out: StartPick = %+v, %v; want acme/sol", got, err)
	}
}

// TestOtherSourcesAndHiddenSources: the routes a person on a model could
// continue on — every other source of its maker that funds an active model
// of it, in the source order, the same wire id named first — and what the
// source order hides, by maker, for the notes.
func TestOtherSourcesAndHiddenSources(t *testing.T) {
	tbl := makerTable(t, signedInList, "", "")
	routes := tbl.OtherSources("chatgpt/gpt-6-astra", keys(true, true))
	if len(routes) != 2 || routes[0].Provider != "acme" || routes[1].Provider != "router" ||
		routes[0].First != "acme/sol" || routes[0].SignIn || !slices.Equal(routes[1].Aliases, []string{"router/sol"}) {
		t.Fatalf("OtherSources = %+v", routes)
	}
	// Two of the router's models now offer a route: the one whose alias
	// shares the most words is named first, until the other carries the
	// model's own wire id, which wins over any wording.
	tbl.Models["router/gpt-6-astra-x"] = Model{Provider: "router", WireModel: "openai/x", Maker: "openai"}
	if r := tbl.OtherSources("chatgpt/gpt-6-astra", keys(true, true)); len(r[1].Aliases) != 2 || r[1].First != "router/gpt-6-astra-x" {
		t.Fatalf("the closest alias is not named first: %+v", r)
	}
	setModel(tbl, "router/sol", func(m *Model) { m.WireModel = "gpt-6-astra" })
	if r := tbl.OtherSources("chatgpt/gpt-6-astra", keys(true, true)); r[1].First != "router/sol" {
		t.Fatalf("the same wire id is not named first: %+v", r)
	}
	delete(tbl.Models, "router/gpt-6-astra-x")
	setModel(tbl, "router/sol", func(m *Model) { m.WireModel = "openai/sol" })
	if r := tbl.OtherSources("acme/sol", keys(false, false)); len(r) != 1 || r[0].Provider != "chatgpt" || !r[0].SignIn {
		t.Fatalf("OtherSources from acme with only the plan = %+v", r)
	}
	if r := tbl.OtherSources("router/gem", keys(true, true)); r != nil {
		t.Fatalf("a model of no maker has routes: %+v", r)
	}
	hs := tbl.HiddenSources(keys(true, true))
	if len(hs) != 1 || hs[0].Maker != "openai" || hs[0].Winner != "chatgpt" ||
		!slices.Equal(hs[0].Hidden, []HiddenSource{{"acme", 1}, {"router", 1}}) {
		t.Fatalf("HiddenSources = %+v", hs)
	}
	if hs := makerTable(t, signedOut, "", "version = 1\nsource_order = []\n").HiddenSources(keys(true, true)); hs != nil {
		t.Fatalf("with the rule off, HiddenSources = %+v", hs)
	}
}
