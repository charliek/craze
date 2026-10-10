package modeltable

import (
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestShippedCatalog is the check a catalog change has to pass (plan 031
// §3.1; RELEASING.md): catalog.toml decodes strictly into its own schema,
// every provider has a display name, a valid driver and base URL and at least
// one env_keys name and no key — the ChatGPT plan's none at all, since it
// signs in (plan 033 §3.11) — every model validates against the catalog's own
// providers, no two aliases share an identity, provider_order names every
// provider exactly once and nothing else, each provider's start is models of
// its own (plan 038 §2), the retired aliases are unique and none is a model,
// and no model's identity is a retired one. Merged over an empty directory it
// is a valid Table, with no default model of its own.
func TestShippedCatalog(t *testing.T) {
	c, err := shippedCatalog()
	if err != nil {
		t.Fatalf("the embedded catalog does not decode: %v", err)
	}
	if err := c.validate(CatalogFile); err != nil {
		t.Fatalf("the shipped catalog breaks a rule the merge relies on: %v", err)
	}
	// The rules restated one by one, so a failure reads as the rule it
	// breaks even if validate itself were wrong.
	for id, p := range c.Providers {
		if p.Driver == DriverChatGPT {
			if id != ChatGPTProvider || p.Name == "" || len(p.EnvKeys) != 0 || p.BaseURL != "" || p.APIKey != "" || p.Source != "" {
				t.Errorf("provider %q = %+v: want the chatgpt id, a name, and no env_keys, base URL, key or source", id, p)
			}
			continue
		}
		if p.Name == "" || len(p.EnvKeys) == 0 || p.APIKey != "" || p.Source != "" {
			t.Errorf("provider %q = %+v: want a name, env_keys, no key, no source", id, p)
		}
	}
	ids := map[identity]string{}
	for alias, m := range c.Models {
		id := identity{m.Provider, m.WireModel}
		if other, ok := ids[id]; ok {
			t.Errorf("%s and %s share the identity %v", alias, other, id)
		}
		ids[id] = alias
	}
	if !slices.Equal(slices.Sorted(slices.Values(c.ProviderOrder)), slices.Sorted(maps.Keys(c.Providers))) {
		t.Errorf("provider_order %q does not name every provider once: %q", c.ProviderOrder, slices.Sorted(maps.Keys(c.Providers)))
	}
	for id, start := range c.Starts {
		if id == ChatGPTProvider || len(start) == 0 {
			t.Errorf("provider %q has start %q: want none for the ChatGPT plan, and a non-empty one otherwise", id, start)
		}
		for _, alias := range start {
			if m, ok := c.Models[alias]; !ok || m.Provider != id {
				t.Errorf("provider %q's start %q is not one of its models (%+v)", id, alias, m)
			}
		}
	}
	retired := map[string]bool{}
	for _, r := range c.Retired {
		if retired[r.Alias] {
			t.Errorf("%s is retired twice", r.Alias)
		}
		retired[r.Alias] = true
		if _, ok := c.Models[r.Alias]; ok {
			t.Errorf("retired %s is still a model", r.Alias)
		}
		for _, w := range r.WireModels {
			if alias, ok := ids[identity{r.Provider, w}]; ok {
				t.Errorf("retired identity %s %s is shipped model %s", r.Provider, w, alias)
			}
		}
	}
	tbl, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("the catalog alone does not load: %v", err)
	}
	if err := tbl.Validate(); err != nil {
		t.Fatalf("the catalog alone is not a valid Table: %v", err)
	}
	if tbl.DefaultModel != "" || tbl.ProviderOrder != nil || !slices.Equal(tbl.StartOrder(), c.ProviderOrder) {
		t.Fatalf("the catalog alone pins %q, orders %q (start order %q); want no pin and the catalog's order", tbl.DefaultModel, tbl.ProviderOrder, tbl.StartOrder())
	}
	// The vision flags (plan 033 X57, from V2's live probe): exactly these
	// models were seen to describe a pasted screenshot, so exactly these are
	// true; glm-5.3 stays false, which is what the composer's paste note and
	// the host's placeholder (D-38, P8) are for.
	// Plan 038 §2.5 adds Ember-1 and GLM 5.3 Flash on Fireworks, each seen
	// taking an image live; GLM 5.3 on Fireworks does not, as on Z.AI. The
	// OpenRouter *-latest routers (2026-10-09) keep their models' flags, and
	// DeepSeek Flash Latest and MiMo V2.6 Pro were seen describing an image
	// read from disk; GLM Latest, GLM 5.3 on OpenRouter, takes text only.
	seesImages := map[string]bool{
		"fireworks/deepseek-v4p1-flash": true, "fireworks/kimi-k3": true,
		"fireworks/ember-1": true, "fireworks/glm-5p3-flash": true,
		"fireworks/qwen3p8-max": true, "glm-5.3-flash": true,
		"muse-spark-1.3": true, "muse-spark-1.3-contributor": true,
		"openrouter/deepseek-flash-latest": true, "openrouter/gemini-flash-latest": true,
		"openrouter/glm-flash-latest": true, "openrouter/gpt-astra-latest": true,
		"openrouter/gpt-luna-latest": true, "openrouter/gpt-sol-latest": true,
		"openrouter/mimo-v2.6-pro": true, "openrouter/minimax-m3": true,
	}
	for alias := range seesImages {
		if md, ok := tbl.Models[alias]; !ok || !md.Vision {
			t.Errorf("%s: vision = %v (present %v), want true", alias, md.Vision, ok)
		}
	}
	for _, alias := range []string{"glm-5.3", "fireworks/glm-5p3", "openrouter/glm-latest"} {
		if md, ok := tbl.Models[alias]; !ok || md.Vision {
			t.Errorf("%s: vision = %v (present %v), want false", alias, md.Vision, ok)
		}
	}
	for alias, md := range tbl.Models {
		if md.Vision && !seesImages[alias] {
			t.Errorf("%s is marked vision but is not on X57's list", alias)
		}
	}
}

// shippedAliasHistory reads testdata/shipped-aliases.txt: one alias a line,
// blank lines and # comments skipped.
func shippedAliasHistory(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/shipped-aliases.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "#") {
			out = append(out, ln)
		}
	}
	return out
}

// checkAliasHistory is the retirement procedure as a rule (plan 031 §3.1;
// RELEASING.md): every alias craze has shipped is still a model or a retired
// alias, and every model is in the history. It returns each break.
func checkAliasHistory(c *Catalog, history []string) []string {
	var bad []string
	retired := map[string]bool{}
	for _, r := range c.Retired {
		retired[r.Alias] = true
	}
	inHistory := map[string]bool{}
	for _, a := range history {
		inHistory[a] = true
		if _, ok := c.Models[a]; !ok && !retired[a] {
			bad = append(bad, a+" was shipped and is neither a model nor a [[retired]] alias")
		}
	}
	for a := range c.Models {
		if !inHistory[a] {
			bad = append(bad, a+" is a model missing from testdata/shipped-aliases.txt")
		}
	}
	slices.Sort(bad)
	return bad
}

// TestShippedCatalogHistory makes retirement a test failure, not a memory
// (plan 031 review r5): removing a model from catalog.toml without a
// [[retired]] row, or adding one without recording it in the history, breaks
// `make test`.
func TestShippedCatalogHistory(t *testing.T) {
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	history := shippedAliasHistory(t)
	for _, b := range checkAliasHistory(c, history) {
		t.Error(b)
	}
	// Negative controls: a model deleted from a copy without retiring it, and
	// a model added without a history line, are each a break.
	dropped := *c
	dropped.Models = maps.Clone(c.Models)
	delete(dropped.Models, "glm-5.3")
	if bad := checkAliasHistory(&dropped, history); len(bad) != 1 || !strings.HasPrefix(bad[0], "glm-5.3 was shipped") {
		t.Errorf("a model removed without retiring it reports %q", bad)
	}
	added := *c
	added.Models = maps.Clone(c.Models)
	added.Models["brand-new"] = c.Models["glm-5.3"]
	if bad := checkAliasHistory(&added, history); len(bad) != 1 || !strings.HasPrefix(bad[0], "brand-new is a model missing") {
		t.Errorf("a model added without a history line reports %q", bad)
	}
	// Retiring it instead is fine.
	dropped.Retired = append(slices.Clone(c.Retired), Retired{Alias: "glm-5.3", Provider: "zai-coding-plan"})
	if bad := checkAliasHistory(&dropped, history); len(bad) != 0 {
		t.Errorf("a retired model reports %q", bad)
	}
}

// TestShippedCatalogOwnerDecisions pins the decisions about the catalog's
// contents that are not rules of its shape (plan 031 P5, §3.1): the
// providers' display names, the ChatGPT plan's among them (plan 033 §3.11),
// and no generic MODEL_API_KEY — a variable that name could be set for
// anything, and would fund (and be redacted as) Meta. And the ChatGPT plan's
// defaults: gpt-6.1-sol then gpt-5.6-sol to start on, gpt-6-astra at low
// effort, and none of it a model of the catalog's. And plan 038 §2: the
// provider order, each provider's start, the three Fireworks models it adds
// and the prices it fills. And OpenRouter's models (2026-10-09): each family
// with a ~<family>-latest router is on it, so a new release needs no catalog
// change, and the numbered aliases it replaced retire the alias only.
func TestShippedCatalogOwnerDecisions(t *testing.T) {
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for id, p := range c.Providers {
		names[id] = p.Name
		if slices.Contains(p.EnvKeys, "MODEL_API_KEY") {
			t.Errorf("provider %q ships MODEL_API_KEY", id)
		}
	}
	want := map[string]string{"chatgpt": "ChatGPT plan", "fireworks": "Fireworks", "meta": "Meta", "openrouter": "OpenRouter", "zai-coding-plan": "Z.AI Coding Plan"}
	if !maps.Equal(names, want) {
		t.Fatalf("providers = %v, want %v", names, want)
	}
	if c.Providers["chatgpt"].Driver != DriverChatGPT {
		t.Fatalf("chatgpt's driver = %q", c.Providers["chatgpt"].Driver)
	}
	wantDefaults := ChatGPTDefaults{Start: []string{"gpt-6.1-sol", "gpt-5.6-sol"}, startSet: true, ModelsClientVersion: "0.160.0", Models: map[string]ChatGPTModelDefaults{"gpt-6-astra": {DefaultEffort: "low"}}}
	if !reflect.DeepEqual(c.ChatGPT, wantDefaults) {
		t.Fatalf("[chatgpt_defaults] = %+v, want %+v", c.ChatGPT, wantDefaults)
	}
	for alias, m := range c.Models {
		if m.Provider == ChatGPTProvider || strings.HasPrefix(alias, ChatGPTAliasPrefix) {
			t.Errorf("%s is a shipped model of the ChatGPT plan; its models are the account's own", alias)
		}
	}
	if want := []string{"zai-coding-plan", "chatgpt", "fireworks", "openrouter", "meta"}; !slices.Equal(c.ProviderOrder, want) {
		t.Fatalf("provider_order = %q, want %q", c.ProviderOrder, want)
	}
	wantStarts := map[string][]string{
		"zai-coding-plan": {"glm-5.3"},
		"fireworks":       {"fireworks/ember-1"},
		"openrouter":      {"openrouter/gemini-flash-latest"},
		"meta":            {"muse-spark-1.3-contributor"},
	}
	if !reflect.DeepEqual(c.Starts, wantStarts) {
		t.Fatalf("starts = %q, want %q", c.Starts, wantStarts)
	}
	// OpenRouter's models and their wire ids: a family's router where
	// OpenRouter has one, the numbered id where it does not (MiMo, MiniMax).
	wantOpenRouter := map[string]string{
		"openrouter/deepseek-flash-latest": "~deepseek/deepseek-flash-latest",
		"openrouter/gemini-flash-latest":   "~google/gemini-flash-latest",
		"openrouter/glm-flash-latest":      "~z-ai/glm-flash-latest",
		"openrouter/glm-latest":            "~z-ai/glm-latest",
		"openrouter/gpt-astra-latest":      "~openai/gpt-astra-latest",
		"openrouter/gpt-luna-latest":       "~openai/gpt-luna-latest",
		"openrouter/gpt-sol-latest":        "~openai/gpt-sol-latest",
		"openrouter/mimo-v2.6-pro":         "xiaomi/mimo-v2.6-pro",
		"openrouter/minimax-m3":            "minimax/minimax-m3",
	}
	gotOpenRouter := map[string]string{}
	for alias, m := range c.Models {
		if m.Provider == "openrouter" {
			gotOpenRouter[alias] = m.WireModel
		}
	}
	if !maps.Equal(gotOpenRouter, wantOpenRouter) {
		t.Fatalf("OpenRouter's models = %q, want %q", gotOpenRouter, wantOpenRouter)
	}
	// OpenRouter takes only low, high and max for GLM and DeepSeek (its model
	// list's supported_efforts), so those routers offer no others.
	for _, alias := range []string{"openrouter/deepseek-flash-latest", "openrouter/glm-flash-latest", "openrouter/glm-latest"} {
		if got := c.Models[alias].Efforts; !slices.Equal(got, []string{"low", "high", "max"}) {
			t.Errorf("%s efforts = %q, want low, high, max", alias, got)
		}
	}
	// The three Fireworks models (§2.5), as verified live.
	for alias, want := range map[string]struct {
		wire, name, effort string
		vision             bool
	}{
		"fireworks/ember-1":       {"accounts/fireworks/models/ember-1", "Ember-1 (Fireworks)", "high", true},
		"fireworks/glm-5p3":       {"accounts/fireworks/models/glm-5p3", "GLM 5.3 (Fireworks)", "max", false},
		"fireworks/glm-5p3-flash": {"accounts/fireworks/models/glm-5p3-flash", "GLM 5.3 Flash (Fireworks)", "high", true},
	} {
		m := c.Models[alias]
		if m.Provider != "fireworks" || m.WireModel != want.wire || m.Name != want.name || m.ContextWindow != 1048576 ||
			!slices.Equal(m.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) || m.DefaultEffort != want.effort || m.Vision != want.vision {
			t.Errorf("%s = %+v", alias, m)
		}
	}
	// The prices (§2.5, §2.6): input, output, cache read, per 1M tokens.
	for alias, want := range map[string][3]float64{
		"fireworks/ember-1":                {3.0, 15.0, 0.3},
		"fireworks/glm-5p3":                {1.4, 4.4, 0.26},
		"fireworks/glm-5p3-flash":          {0.15, 0.5, 0.03},
		"fireworks/qwen3p8-max":            {2.0, 6.0, 0.25},
		"openrouter/deepseek-flash-latest": {0.016, 0.6, 0.005},
		"openrouter/gemini-flash-latest":   {0.75, 3.75, 0.075},
		"openrouter/glm-flash-latest":      {0.032, 0.72, 0.01},
		"openrouter/glm-latest":            {0.039, 4.8, 0.038},
		"openrouter/mimo-v2.6-pro":         {0.435, 0.87, 0.0036},
		"openrouter/minimax-m3":            {0.3, 1.2, 0.06},
		"muse-spark-1.3-contributor":       {0.1, 0.2, 0.002},
	} {
		cost := c.Models[alias].Cost
		if cost == nil || cost.Input == nil || cost.Output == nil || cost.CacheRead == nil ||
			[3]float64{*cost.Input, *cost.Output, *cost.CacheRead} != want {
			t.Errorf("%s cost = %+v, want %v", alias, cost, want)
		}
	}
	// The Z.AI coding plan is flat-rate, like ChatGPT's: unpriced.
	for _, alias := range []string{"glm-5.3", "glm-5.3-flash"} {
		if cost := c.Models[alias].Cost; cost != nil {
			t.Errorf("%s is priced (%+v): the coding plan is flat-rate", alias, cost)
		}
	}
	// Aliases the owner dropped while their wire ids still answer retire the
	// alias only (plan 031 X6): a hand entry pointing at the live id stays the
	// user's own model (§3.3), so these rows name no identity.
	for _, r := range c.Retired {
		switch r.Alias {
		case "fireworks/glm-5p3-fast", "fireworks/kimi-k3-fast",
			"openrouter/gpt-5.6-luna", "openrouter/gpt-5.6-sol", "openrouter/gpt-5.6-terra",
			"openrouter/gpt-6-sol", "openrouter/gemini-3.8-flash", "openrouter/glm-5.3-flash",
			"openrouter/gpt-6-astra", "openrouter/gpt-6-luna", "openrouter/gpt-6.1-sol":
			if len(r.WireModels) != 0 {
				t.Errorf("retired %q names live wire ids %v", r.Alias, r.WireModels)
			}
		}
	}
}

// TestCatalogValidateRefuses: each rule TestShippedCatalog relies on is one
// validate enforces — a catalog breaking it is refused, naming the place. The
// shipped catalog, changed one way at a time, is each case.
func TestCatalogValidateRefuses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*Catalog)
		table, key string
	}{
		{"a provider without a name", func(c *Catalog) { setEntry(c.Providers, "meta", func(p *Provider) { p.Name = "" }) }, "providers.meta", "name"},
		{"a provider without env_keys", func(c *Catalog) { setEntry(c.Providers, "meta", func(p *Provider) { p.EnvKeys = nil }) }, "providers.meta", "env_keys"},
		{"a provider with a key", func(c *Catalog) { setEntry(c.Providers, "meta", func(p *Provider) { p.APIKey = canary }) }, "providers.meta", "api_key"},
		{"a bad base URL", func(c *Catalog) { setEntry(c.Providers, "meta", func(p *Provider) { p.BaseURL = "nope" }) }, "providers.meta", "base_url"},
		{"a model on no provider", func(c *Catalog) {
			m := c.Models["glm-5.3"]
			m.Provider = "gone"
			c.Models["glm-5.3"] = m
		}, `models."glm-5.3"`, "provider"},
		{"two aliases of one identity", func(c *Catalog) { c.Models["glm-5.3-again"] = c.Models["glm-5.3"] }, `models."glm-5.3-again"`, "wire_model"},
		// Plan 038 §2.1–§2.2: the order, and each provider's start.
		{"no provider_order", func(c *Catalog) { c.ProviderOrder = nil }, "", "provider_order"},
		{"a provider ordered twice", func(c *Catalog) { c.ProviderOrder = append(c.ProviderOrder, "meta") }, "", "provider_order"},
		{"an unknown provider ordered", func(c *Catalog) { c.ProviderOrder = append(c.ProviderOrder, "nope") }, "", "provider_order"},
		{"a provider left out of the order", func(c *Catalog) {
			c.ProviderOrder = slices.DeleteFunc(c.ProviderOrder, func(id string) bool { return id == "meta" })
		}, "", "provider_order"},
		{"a start alias of another provider", func(c *Catalog) { c.Starts["fireworks"] = []string{"glm-5.3"} }, "providers.fireworks", "start"},
		{"a start alias that is not a model", func(c *Catalog) { c.Starts["fireworks"] = []string{"fireworks/nope"} }, "providers.fireworks", "start"},
		{"a start alias twice", func(c *Catalog) {
			c.Starts["fireworks"] = []string{"fireworks/ember-1", "fireworks/ember-1"}
		}, "providers.fireworks", "start"},
		{"an empty start", func(c *Catalog) { c.Starts["fireworks"] = []string{} }, "providers.fireworks", "start"},
		{"a start for the ChatGPT plan", func(c *Catalog) { c.Starts["chatgpt"] = []string{"glm-5.3"} }, "providers.chatgpt", "start"},
		{"a retired alias twice", func(c *Catalog) { c.Retired = append(c.Retired, c.Retired[0]) }, "retired", "alias"},
		{"a retired alias that is a model", func(c *Catalog) {
			c.Retired = append(c.Retired, Retired{Alias: "glm-5.3", Provider: "zai-coding-plan"})
		}, "retired", "alias"},
		{"a retired identity that is shipped", func(c *Catalog) {
			c.Retired = append(c.Retired, Retired{Alias: "old", Provider: "zai-coding-plan", WireModels: []string{"glm-5.3"}})
		}, "retired", "wire_models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ShippedCatalog()
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(c)
			wantFileError(t, c.validate(CatalogFile), CatalogFile, tc.table, tc.key)
		})
	}
}

// TestCatalogSchemaRefusesUserKeys: the catalog's own schema has no api_key,
// source, catalog, default_model, [subagents] or [compaction], so a strict
// decode refuses each — a key can never be shipped by mistake, nor a setting
// that belongs to the user (plan 031 §3.1). default_model is the user's pin
// alone since plan 038 §2.3: a catalog that still has one does not load.
func TestCatalogSchemaRefusesUserKeys(t *testing.T) {
	const head = "version = 1\nprovider_order = [\"p\"]\n"
	const prov = "\n[providers.p]\nname = \"P\"\ndriver = \"openrouter\"\nenv_keys = [\"P_KEY\"]\nstart = [\"m\"]\n"
	const model = "\n[models.m]\nprovider = \"p\"\nwire_model = \"w\"\n"
	if _, err := parseCatalog(CatalogFile, []byte(head+prov+model)); err != nil {
		t.Fatalf("control: %v", err)
	}
	for _, tc := range []struct{ name, doc, table, key string }{
		{"api_key", head + prov + "api_key = \"" + canary + "\"\n" + model, "providers.p", "api_key"},
		{"provider source", head + prov + "source = \"gx\"\n" + model, "providers.p", "source"},
		{"model source", head + prov + model + "source = \"gx\"\n", "models.m", "source"},
		{"catalog", "catalog = false\n" + head + prov + model, "", "catalog"},
		{"default_model", head + "default_model = \"m\"\n" + prov + model, "", "default_model"},
		{"subagents", head + prov + model + "\n[subagents]\nmodel = \"m\"\n", "", "subagents"},
		{"compaction", head + prov + model + "\n[compaction]\nauto = false\n", "", "compaction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCatalog(CatalogFile, []byte(tc.doc))
			fe := wantFileError(t, err, CatalogFile, tc.table, tc.key)
			if fe.Reason != "unknown key" || strings.Contains(err.Error(), canary) {
				t.Fatalf("err = %v, want an unknown key that does not quote the value", err)
			}
		})
	}
}

// TestShippedCatalogLoadsShareNothing: the embedded catalog is decoded once
// and deep-copied into every load (plan 031 §3.1), so two tables — or two
// ShippedCatalog copies — share no map, slice or pointer, and changing one
// changes neither the other nor the next load.
func TestShippedCatalogLoadsShareNothing(t *testing.T) {
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// b and fresh read one directory: a table that funds the ChatGPT plan
	// keeps the directory its sign-in is in (plan 033 §3.11), so two loads of
	// two directories differ by that alone.
	bDir := t.TempDir()
	b, err := Load(bDir)
	if err != nil {
		t.Fatal(err)
	}
	if shared := sharedRefs(a, b); len(shared) > 0 {
		t.Fatalf("two loads share %d references, e.g. at %s", len(shared), shared[0])
	}
	internal, err := shippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := ShippedCatalog()
	for name, other := range map[string]any{"a load": a, "a ShippedCatalog copy": c1} {
		if shared := sharedRefs(internal, other); len(shared) > 0 {
			t.Fatalf("%s shares %d references with the decoded catalog, e.g. at %s", name, len(shared), shared[0])
		}
	}

	// Changing everything reachable in one load leaves the next untouched.
	m := a.Models["fireworks/kimi-k3"]
	m.Efforts[0] = "changed"
	*m.Cost.Input = 999
	a.Models["fireworks/kimi-k3"] = m
	a.Providers["fireworks"].EnvKeys[0] = "CHANGED"
	a.Models["new"] = Model{}
	fresh, err := Load(bDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh, b) {
		t.Fatal("a change to one load reached the next one")
	}
}

// sharedRefs lists the paths at which x and y hold the same map, the same
// slice backing array or the same pointer.
func sharedRefs(x, y any) []string {
	seen := map[uintptr]string{}
	var walk func(v reflect.Value, path string, record bool, out *[]string)
	walk = func(v reflect.Value, path string, record bool, out *[]string) {
		mark := func(p uintptr) {
			if p == 0 {
				return
			}
			if record {
				seen[p] = path
			} else if first, ok := seen[p]; ok {
				*out = append(*out, path+" (also "+first+")")
			}
		}
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			mark(v.Pointer())
			walk(v.Elem(), path, record, out)
		case reflect.Map:
			if v.IsNil() {
				return
			}
			mark(v.Pointer())
			for _, k := range v.MapKeys() {
				walk(v.MapIndex(k), path+"["+k.String()+"]", record, out)
			}
		case reflect.Slice:
			if v.Cap() == 0 {
				return
			}
			mark(v.Pointer())
			for i := range v.Len() {
				walk(v.Index(i), path, record, out)
			}
		case reflect.Struct:
			for i := range v.NumField() {
				walk(v.Field(i), path+"."+v.Type().Field(i).Name, record, out)
			}
		case reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem(), path, record, out)
			}
		}
	}
	var out []string
	walk(reflect.ValueOf(x), "x", true, &out)
	walk(reflect.ValueOf(y), "y", false, &out)
	return out
}

// TestCatalogEnvNames: every variable a shipped provider takes a key from,
// sorted, once each — what the tests unset (plan 031 §3.13).
func TestCatalogEnvNames(t *testing.T) {
	got := CatalogEnvNames()
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, p := range c.Providers {
		for _, n := range p.EnvKeys {
			if !slices.Contains(want, n) {
				want = append(want, n)
			}
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) || len(got) == 0 {
		t.Fatalf("CatalogEnvNames = %q, want %q", got, want)
	}
	for _, n := range []string{"FIREWORKS_API_KEY", "META_API_KEY", "OPENROUTER_API_KEY", "ZHIPU_API_KEY"} {
		if !slices.Contains(got, n) {
			t.Errorf("CatalogEnvNames lacks %s", n)
		}
	}
	got[0] = "changed"
	if CatalogEnvNames()[0] == "changed" {
		t.Fatal("CatalogEnvNames returned shared storage")
	}
}

// TestLoadReportsABrokenCatalog: a catalog that does not validate fails the
// merged table's validation, naming the entry, rather than loading a table
// the harness cannot use. TestShippedCatalog keeps this from ever shipping;
// this proves the load would not paper over it.
func TestLoadReportsABrokenCatalog(t *testing.T) {
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	setEntry(c.Providers, "meta", func(p *Provider) { p.Driver = "nope" })
	_, err = LoadWith(t.TempDir(), c)
	var fe *FileError
	if !errors.As(err, &fe) || fe.Table != "providers.meta" || fe.Key != "driver" {
		t.Fatalf("err = %v, want the broken provider named", err)
	}
}
