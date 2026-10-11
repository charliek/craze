package modeltable

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Anthropic's provider and driver in the table (plan 044 §3.3, §3.5): the
// driver is the one provider's alone and takes no base URL, that provider's
// entry is locked once the catalog ships it, env_keys = [] still opts an
// exported key out, and Resolve says what thinking a request asks for. They
// run over a small catalog of their own, so a change to the shipped seeds
// changes none of them.

const anthropicCatalog = `version = 1
provider_order = ["acme", "anthropic"]

[providers.acme]
name = "Acme"
driver = "openai-compat"
base_url = "https://api.acme.example/v1"
env_keys = ["ACME_API_KEY"]
start = ["acme/fast"]

[providers.anthropic]
name = "Anthropic"
driver = "anthropic"
env_keys = ["ANTHROPIC_API_KEY"]

[anthropic_defaults]
start_family = "test"
default_effort = "medium"

[models."acme/fast"]
provider = "acme"
wire_model = "acme-fast-1"

[models."anthropic/claude-test-5"]
provider = "anthropic"
wire_model = "claude-test-5"
family = "test"
released = "2026-01-01"
thinking = "adaptive"
efforts = ["low", "medium", "high", "xhigh", "max"]
`

// anthropicKey is a dummy Anthropic key: at least eight bytes, nothing like
// a real one.
const anthropicKey = "sk-ant-canary-not-a-secret-0001"

// TestAnthropicProviderRules (§3.3, P22): over a catalog that ships it, the
// Anthropic provider's entry may set name, env_keys and api_key — kept — and a
// driver or base_url written for it is dropped with a warning naming the key
// and never a value; a provider of the user's on the anthropic driver under
// another id is dropped, as is the driver written for a shipped provider of
// another id. With the catalog off the same entries fail the load, and the
// provider alone loads and resolves (the control).
func TestAnthropicProviderRules(t *testing.T) {
	cat := testCatalog(t, anthropicCatalog)
	const providers = "version = 1\n\n" +
		"[providers.anthropic]\nname = \"My Claude\"\ndriver = \"openai-compat\"\nbase_url = \"https://proxy.example/v1\"\nenv_keys = [\"MY_CLAUDE_KEY\"]\napi_key = \"" + anthropicKey + "\"\n\n" +
		"[providers.mine]\ndriver = \"anthropic\"\n\n" +
		"[providers.acme]\ndriver = \"anthropic\"\n"
	tbl, _ := loadOver(t, cat, providers, "")
	p := tbl.Providers[AnthropicProvider]
	if p.Name != "My Claude" || p.Driver != DriverAnthropic || p.BaseURL != "" ||
		!slices.Equal(p.EnvKeys, []string{"MY_CLAUDE_KEY"}) || p.APIKey.Reveal() != anthropicKey {
		t.Fatalf("anthropic = %+v; want its name, env_keys and api_key overridden, its driver and endpoint craze's", p)
	}
	if _, ok := tbl.Providers["mine"]; ok {
		t.Fatal("a provider of the user's on the anthropic driver was kept")
	}
	if tbl.Providers["acme"].Driver != DriverOpenAICompat {
		t.Fatalf("acme = %+v; want its shipped driver", tbl.Providers["acme"])
	}
	wantWarnings(t, tbl,
		[]string{"providers.anthropic", "driver", "always driver", "only name, env_keys and api_key"},
		[]string{"providers.anthropic", "base_url", "endpoint is fixed", "only name, env_keys and api_key"},
		[]string{"providers.acme", "driver", "alone", "shipped driver"},
		[]string{"providers.mine", "driver", "alone", "ignored"},
	)
	for _, w := range tbl.Warnings {
		if strings.Contains(w, anthropicKey) || strings.Contains(w, "proxy.example") {
			t.Fatalf("a warning quotes a value: %q", w)
		}
	}
	r, err := tbl.Resolve("anthropic/claude-test-5", fakeEnv(map[string]string{"MY_CLAUDE_KEY": "sk-ant-from-the-env-0002"}))
	if err != nil || r.Driver != DriverAnthropic || r.BaseURL != "" || r.APIKey.Reveal() != "sk-ant-from-the-env-0002" {
		t.Fatalf("Resolve = %+v, %v; want the anthropic driver on the user's variable", r, err)
	}

	// The control: an entry that writes only what it may draws no warning.
	only, _ := loadOver(t, cat, "version = 1\n\n[providers.anthropic]\nname = \"Claude\"\napi_key = \""+anthropicKey+"\"\n", "")
	wantWarnings(t, only)

	// A driver and an endpoint the entry writes together that cannot go
	// together fail the load, as openrouter's do.
	dir := writeFiles(t, "version = 1\n\n[providers.acme]\ndriver = \"anthropic\"\nbase_url = \"https://x.example\"\n", "")
	_, err = LoadWith(dir, cat)
	wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.acme", "base_url")

	// Catalog off: the strict rules of a whole table.
	const models = "version = 1\ncatalog = false\ndefault_model = \"m\"\n\n[models.m]\nprovider = \"anthropic\"\nwire_model = \"claude-test-5\"\n"
	for _, tc := range []struct{ name, providers, table, key string }{
		{"an endpoint", "version = 1\n\n[providers.anthropic]\ndriver = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n", "providers.anthropic", "base_url"},
		{"another id", "version = 1\n\n[providers.anthropic]\ndriver = \"anthropic\"\n\n[providers.mine]\ndriver = \"anthropic\"\n", "providers.mine", "driver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.providers, models)
			_, err := Load(dir)
			wantFileError(t, err, filepath.Join(dir, ProvidersFile), tc.table, tc.key)
		})
	}
	alone := writeFiles(t, "version = 1\n\n[providers.anthropic]\ndriver = \"anthropic\"\nenv_keys = [\"ANTHROPIC_API_KEY\"]\n", models)
	tbl, err = Load(alone)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := tbl.Resolve("m", fakeEnv(map[string]string{"ANTHROPIC_API_KEY": anthropicKey})); err != nil || r.Driver != DriverAnthropic || r.Thinking != ThinkingAdaptive {
		t.Fatalf("Resolve = %+v, %v", r, err)
	}
}

// TestAnthropicEnvKeysOptOut (P24): an exported ANTHROPIC_API_KEY funds the
// shipped provider, and `env_keys = []` in providers.toml opts out of it —
// the provider is then funded by its inline key alone, or not at all — while
// the variable is still known to hold a credential, so a command the session
// runs never sees it. No warning: env_keys is the entry's to set.
func TestAnthropicEnvKeysOptOut(t *testing.T) {
	cat := testCatalog(t, anthropicCatalog)
	env := fakeEnv(map[string]string{"ANTHROPIC_API_KEY": anthropicKey})

	shipped, _ := loadOver(t, cat, "", "")
	if r, err := shipped.Resolve("anthropic/claude-test-5", env); err != nil || r.APIKey.Reveal() != anthropicKey {
		t.Fatalf("the control: Resolve = %+v, %v; want the exported key", r, err)
	}

	optOut, _ := loadOver(t, cat, "version = 1\n\n[providers.anthropic]\nenv_keys = []\n", "")
	wantWarnings(t, optOut)
	if _, err := optOut.Resolve("anthropic/claude-test-5", env); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("env_keys = []: err = %v, want ErrNoAPIKey: the exported key still funds craze", err)
	}
	if !slices.Contains(optOut.CredentialEnvNames(), "ANTHROPIC_API_KEY") {
		t.Fatal("the opted-out variable is no longer withheld from the commands a session runs")
	}

	const inline = "sk-ant-inline-not-a-secret-0003"
	stored, _ := loadOver(t, cat, "version = 1\n\n[providers.anthropic]\nenv_keys = []\napi_key = \""+inline+"\"\n", "")
	if r, err := stored.Resolve("anthropic/claude-test-5", env); err != nil || r.APIKey.Reveal() != inline {
		t.Fatalf("env_keys = [] with an inline key: Resolve = %+v, %v; want the inline key", r, err)
	}
}

// anthropicTable is a table built in memory — no catalog, so no snapshot of
// what craze learned (Table.learned) — with models on the anthropic driver
// whose thinking is set and unset, and a model on another driver.
func anthropicTable() *Table {
	return &Table{
		NoCatalog:    true,
		DefaultModel: "anthropic/opus",
		Providers: map[string]Provider{
			AnthropicProvider: {Driver: DriverAnthropic, EnvKeys: []string{"ANTHROPIC_API_KEY"}},
			"fireworks":       {Driver: DriverOpenAICompat, BaseURL: "https://api.fireworks.example/v1", EnvKeys: []string{"FIREWORKS_API_KEY"}},
		},
		Models: map[string]Model{
			"anthropic/opus":    {Provider: AnthropicProvider, WireModel: "claude-opus-5-5", Thinking: ThinkingAdaptive},
			"anthropic/legacy":  {Provider: AnthropicProvider, WireModel: "claude-opus-4-5-20251101", Thinking: ThinkingNone},
			"mine/new":          {Provider: AnthropicProvider, WireModel: "claude-future-9"},
			"fireworks/kimi-k3": {Provider: "fireworks", WireModel: "accounts/fireworks/models/kimi-k3"},
		},
	}
}

// TestResolveThinking (§3.5's tri-state): a model's own thinking resolves as
// it is; one craze knows nothing of asks for adaptive thinking; and a model
// on another driver has none at all (a model looked up by its identity:
// TestThinkingFollowsTheIdentity). Validate refuses a thinking that is none of
// these, and Save never writes it: no file has a key for it (P11).
func TestResolveThinking(t *testing.T) {
	tbl := anthropicTable()
	if err := tbl.Validate(); err != nil {
		t.Fatalf("the test table: %v", err)
	}
	env := fakeEnv(map[string]string{"ANTHROPIC_API_KEY": anthropicKey, "FIREWORKS_API_KEY": "key-fw-0001"})
	for alias, want := range map[string]string{
		"anthropic/opus":    ThinkingAdaptive,
		"anthropic/legacy":  ThinkingNone,
		"mine/new":          ThinkingAdaptive,
		"fireworks/kimi-k3": "",
	} {
		r, err := tbl.Resolve(alias, env)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", alias, err)
		}
		if r.Thinking != want {
			t.Errorf("Resolve(%s).Thinking = %q, want %q", alias, r.Thinking, want)
		}
	}

	bad := anthropicTable()
	setModel(bad, "mine/new", func(m *Model) { m.Thinking = "enabled" })
	wantFileError(t, bad.Validate(), ModelsFile, `models."mine/new"`, "thinking")

	dir := t.TempDir()
	if err := Save(dir, tbl); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "thinking") || strings.Contains(string(raw), ThinkingNone) {
		t.Fatalf("Save wrote a model's thinking:\n%s", raw)
	}
}

// TestAnthropicDriverRules (§3.3): the anthropic driver is a known one, valid
// only under the Anthropic provider's id and with no base URL, each failure
// naming the file, the table and the key.
func TestAnthropicDriverRules(t *testing.T) {
	if r := driverProblem(DriverAnthropic); r != "" {
		t.Fatalf("driverProblem(anthropic) = %q", r)
	}
	for _, tc := range []struct {
		name       string
		mutate     func(*Table)
		table, key string
	}{
		{"under another id", func(t *Table) {
			setProvider(t, "fireworks", func(p *Provider) { p.Driver, p.BaseURL = DriverAnthropic, "" })
		}, "providers.fireworks", "driver"},
		{"with a base URL", func(t *Table) {
			setProvider(t, AnthropicProvider, func(p *Provider) { p.BaseURL = "https://api.anthropic.com" })
		}, "providers.anthropic", "base_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := anthropicTable()
			tc.mutate(tbl)
			wantFileError(t, tbl.Validate(), ProvidersFile, tc.table, tc.key)
		})
	}
}

// TestAnthropicLockDropsBeforeTheChecks (C2 review 5, P22): a key the lock
// drops is dropped before the user's file is checked, so it warns and never
// fails the load — a gateway's base_url beside the anthropic driver, a driver
// craze does not know — and the provider stays craze's own.
func TestAnthropicLockDropsBeforeTheChecks(t *testing.T) {
	cat := testCatalog(t, anthropicCatalog)
	for _, tc := range []struct{ name, entry, key string }{
		{"a base URL beside the driver", "driver = \"anthropic\"\nbase_url = \"https://gateway.example/v1\"\n", "base_url"},
		{"an unknown driver", "driver = \"bedrock\"\n", "driver"},
		{"a base URL that is no URL", "base_url = \"gateway.example\"\n", "base_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, "version = 1\n\n[providers.anthropic]\n"+tc.entry, "")
			tbl, err := LoadWith(dir, cat)
			if err != nil {
				t.Fatalf("the load failed on a key the lock drops: %v", err)
			}
			wantWarnings(t, tbl, []string{"providers.anthropic", tc.key, "only name, env_keys and api_key"})
			if p := tbl.Providers[AnthropicProvider]; p.Driver != DriverAnthropic || p.BaseURL != "" {
				t.Fatalf("anthropic = %+v, want craze's own", p)
			}
		})
	}
}

// TestAnthropicLockDropsSource (C2 review 3, P24): the lock drops a source
// too, so an entry that says it is an old import's cannot take the import's
// path, which would put the shipped variables back: `env_keys = []` keeps an
// exported ANTHROPIC_API_KEY from funding craze whatever source it writes.
func TestAnthropicLockDropsSource(t *testing.T) {
	cat := testCatalog(t, anthropicCatalog)
	env := fakeEnv(map[string]string{"ANTHROPIC_API_KEY": anthropicKey})
	tbl, _ := loadOver(t, cat, "version = 1\n\n[providers.anthropic]\nsource = \"gx\"\nenv_keys = []\n", "")
	wantWarnings(t, tbl, []string{"providers.anthropic", "source", "only name, env_keys and api_key"})
	if _, err := tbl.Resolve("anthropic/claude-test-5", env); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("source = \"gx\", env_keys = []: err = %v, want ErrNoAPIKey: the exported key funds craze", err)
	}
	const inline = "sk-ant-inline-not-a-secret-0003"
	tbl, _ = loadOver(t, cat, "version = 1\n\n[providers.anthropic]\nsource = \"gx\"\nenv_keys = []\napi_key = \""+inline+"\"\n", "")
	if r, err := tbl.Resolve("anthropic/claude-test-5", env); err != nil || r.APIKey.Reveal() != inline {
		t.Fatalf("with an inline key: Resolve = %+v, %v; want the inline key over the exported one", r, err)
	}
}

// TestAnthropicIDIsNotReserved (plan 044 X1): the anthropic driver is the
// anthropic id's alone, but not the other way round — a provider of the
// user's own named anthropic, on another driver, is theirs and loads, with the
// catalog off and over a catalog that does not ship Anthropic's provider, and
// its model resolves on the driver and endpoint the user configured.
func TestAnthropicIDIsNotReserved(t *testing.T) {
	const gateway = "version = 1\n\n[providers.anthropic]\ndriver = \"openai-compat\"\nbase_url = \"https://gateway.example/v1\"\nenv_keys = [\"MY_GATEWAY_KEY\"]\n"
	env := fakeEnv(map[string]string{"MY_GATEWAY_KEY": "gw-key-not-a-secret-0001"})
	off, err := Load(writeFiles(t, gateway, "version = 1\ncatalog = false\ndefault_model = \"claude\"\n\n[models.claude]\nprovider = \"anthropic\"\nwire_model = \"claude-opus-4-5-20251101\"\n"))
	if err != nil {
		t.Fatalf("catalog off: %v", err)
	}
	over, _ := loadOver(t, testCatalog(t, testCatalogV1), gateway, "version = 1\n\n[models.claude]\nprovider = \"anthropic\"\nwire_model = \"claude-opus-4-5-20251101\"\n")
	wantWarnings(t, over)
	for name, tbl := range map[string]*Table{"catalog off": off, "a catalog without the provider": over} {
		r, err := tbl.Resolve("claude", env)
		if err != nil || r.Driver != DriverOpenAICompat || r.BaseURL != "https://gateway.example/v1" || r.Thinking != "" {
			t.Errorf("%s: Resolve = %+v, %v; want the user's gateway", name, r, err)
		}
	}
	if o := over.ProviderOrigin(AnthropicProvider); o != OriginYours {
		t.Errorf("origin = %q, want the user's", o)
	}
}

// TestThinkingFollowsTheIdentity (C2 reviews 2 and r2 b1, P11): an entry
// that points a model at another wire model forgets what craze knew of the
// old one, and the model resolves by its new identity in what craze learned
// before the user's files were laid over the catalog — a known id's thinking,
// none or adaptive, even when the alias that carried it is pointed elsewhere
// too, or adaptive for an id craze does not know — as does the user's own
// alias of a known id; an entry that writes the model's own identity again
// keeps it; and a model carried into a table that learned nothing of its id
// resolves as it did.
func TestThinkingFollowsTheIdentity(t *testing.T) {
	cat := testCatalog(t, anthropicCatalog+`
[models."anthropic/legacy"]
provider = "anthropic"
wire_model = "claude-legacy-4"
thinking = "none"

[models."anthropic/new"]
provider = "anthropic"
wire_model = "claude-new-6"
thinking = "adaptive"

[models."anthropic/legacy-b"]
provider = "anthropic"
wire_model = "claude-legacy-3"
thinking = "none"

[models."anthropic/legacy-c"]
provider = "anthropic"
wire_model = "claude-legacy-1"
thinking = "none"

[models."anthropic/kept"]
provider = "anthropic"
wire_model = "claude-legacy-2"
thinking = "none"
`)
	tbl, _ := loadOver(t, cat, "", `version = 1

[models."anthropic/claude-test-5"]
wire_model = "claude-legacy-4"

[models."anthropic/legacy"]
wire_model = "claude-future-8"

[models."mine/old"]
provider = "anthropic"
wire_model = "claude-legacy-4"

[models."anthropic/legacy-b"]
wire_model = "claude-new-6"

[models."anthropic/legacy-c"]
wire_model = "claude-future-9"

[models."anthropic/kept"]
wire_model = "claude-legacy-2"
name = "Kept"
`)
	env := fakeEnv(map[string]string{"ANTHROPIC_API_KEY": anthropicKey})
	for alias, want := range map[string]string{
		"anthropic/claude-test-5": ThinkingNone,     // adaptive, pointed at an id that takes none, though its alias points elsewhere now
		"anthropic/legacy":        ThinkingAdaptive, // none, pointed at an id craze does not know
		"mine/old":                ThinkingNone,     // the user's own alias of an id craze knows
		"anthropic/legacy-b":      ThinkingAdaptive, // none, pointed at a model that is adaptive
		"anthropic/legacy-c":      ThinkingAdaptive, // none, pointed at an id craze does not know
		"anthropic/kept":          ThinkingNone,     // its own identity written again
	} {
		r, err := tbl.Resolve(alias, env)
		if err != nil {
			t.Fatal(err)
		}
		if r.Thinking != want {
			t.Errorf("%s (now %s) thinking = %q, want %q", alias, r.WireModel, r.Thinking, want)
		}
	}

	bare := anthropicTable()
	if !bare.Carry(tbl, "mine/old") {
		t.Fatal("Carry refused")
	}
	if r, err := bare.Resolve("mine/old", env); err != nil || r.Thinking != ThinkingNone {
		t.Fatalf("carried: Resolve = %+v, %v; want none, as it resolved where it came from", r, err)
	}
}

// TestAnthropicListKey (plan 044 §3.6, A5): the key the account's model list
// is fetched with is the key craze's own Anthropic provider is funded by, as
// a load binds the list to it — an exported one alone (env-only), the stored
// one, the exported one over the stored one, the stored one alone under
// `env_keys = []` — and there is none with nothing funding it. A provider of
// the user's own named anthropic (plan 044 X1) fetches nothing, whatever
// funds it, and neither does one WithholdFrozen took out. The guard on the
// driver, which the lock makes unreachable, is pinned on a table whose
// shipped provider is set to another driver; the control is the same table
// on its own driver, which fetches.
func TestAnthropicListKey(t *testing.T) {
	cat := testCatalog(t, anthropicCatalog)
	const stored = "sk-ant-stored-not-a-secret-0004"
	exported := fakeEnv(map[string]string{"ANTHROPIC_API_KEY": anthropicKey})
	none := fakeEnv(nil)
	storedEntry := "version = 1\n\n[providers.anthropic]\napi_key = \"" + stored + "\"\n"
	for _, tc := range []struct {
		name, providers string
		env             func(string) string
		want            string // "" for none
	}{
		{"an exported key alone", "", exported, anthropicKey},
		{"a stored key alone", storedEntry, none, stored},
		{"the exported key over the stored one", storedEntry, exported, anthropicKey},
		{"env_keys = [] with an exported key and none stored", "version = 1\n\n[providers.anthropic]\nenv_keys = []\n", exported, ""},
		{"env_keys = [] with a stored key", "version = 1\n\n[providers.anthropic]\nenv_keys = []\napi_key = \"" + stored + "\"\n", exported, stored},
		{"nothing funds it", "", none, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _ := loadOver(t, cat, tc.providers, "")
			k, ok := tbl.AnthropicListKey(tc.env)
			if ok != (tc.want != "") || k.Reveal() != tc.want {
				t.Fatalf("AnthropicListKey = %t (the key matches: %t); want %t", ok, k.Reveal() == tc.want, tc.want != "")
			}
		})
	}

	t.Run("a provider of the user's own", func(t *testing.T) {
		const gateway = "version = 1\n\n[providers.anthropic]\ndriver = \"openai-compat\"\nbase_url = \"https://gateway.example/v1\"\napi_key = \"" + stored + "\"\n"
		off, err := Load(writeFiles(t, gateway, "version = 1\ncatalog = false\ndefault_model = \"claude\"\n\n[models.claude]\nprovider = \"anthropic\"\nwire_model = \"claude-opus-4-5-20251101\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		over, _ := loadOver(t, testCatalog(t, testCatalogV1), gateway, "")
		mine := anthropicTable() // built in memory, on the anthropic driver
		for name, tbl := range map[string]*Table{"catalog off": off, "a catalog without the provider": over, "built in memory": mine} {
			if _, ok := tbl.AnthropicListKey(exported); ok {
				t.Errorf("%s: a list would be fetched for a provider craze does not ship", name)
			}
		}
	})

	t.Run("withheld", func(t *testing.T) {
		tbl, _ := loadOver(t, cat, "", "")
		if _, ok := tbl.AnthropicListKey(exported); !ok {
			t.Fatal("the control: the exported key fetches nothing")
		}
		tbl.WithholdFrozen(exported, func(v string) bool { return v == anthropicKey }, "acme/fast")
		if _, ok := tbl.AnthropicListKey(exported); ok {
			t.Fatal("a list would be fetched with a key the session withholds")
		}
	})

	t.Run("the driver guard", func(t *testing.T) {
		tbl, _ := loadOver(t, cat, storedEntry, "")
		if _, ok := tbl.AnthropicListKey(none); !ok || tbl.ProviderOrigin(AnthropicProvider) != OriginOverridden {
			t.Fatalf("the control: no key on the shipped provider (origin %q)", tbl.ProviderOrigin(AnthropicProvider))
		}
		setProvider(tbl, AnthropicProvider, func(p *Provider) { p.Driver = DriverOpenAICompat })
		if _, ok := tbl.AnthropicListKey(none); ok {
			t.Fatal("a list would be fetched for the shipped provider on another driver")
		}
	})
}
