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
// providers, default_model is a model, no two aliases share an identity, the
// retired aliases are unique and none is a model, and no model's identity is
// a retired one. Merged over an empty directory it is a valid Table.
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
	if _, ok := c.Models[c.DefaultModel]; !ok {
		t.Errorf("default_model %q is not a model", c.DefaultModel)
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
	// The vision flags (plan 033 X57, from V2's live probe): exactly these
	// models were seen to describe a pasted screenshot, so exactly these are
	// true; glm-5.3 stays false, which is what the composer's paste note and
	// the host's placeholder (D-38, P8) are for.
	seesImages := map[string]bool{
		"fireworks/deepseek-v4p1-flash": true, "fireworks/kimi-k3": true,
		"fireworks/qwen3p8-max": true, "glm-5.3-flash": true,
		"muse-spark-1.3": true, "muse-spark-1.3-contributor": true,
		"openrouter/gemini-3.8-flash": true, "openrouter/glm-5.3-flash": true,
		"openrouter/gpt-6-astra": true, "openrouter/gpt-6-luna": true,
		"openrouter/gpt-6.1-sol": true, "openrouter/minimax-m3": true,
	}
	for alias := range seesImages {
		if md, ok := tbl.Models[alias]; !ok || !md.Vision {
			t.Errorf("%s: vision = %v (present %v), want true", alias, md.Vision, ok)
		}
	}
	if md, ok := tbl.Models["glm-5.3"]; !ok || md.Vision {
		t.Errorf("glm-5.3: vision = %v (present %v), want false", md.Vision, ok)
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
// defaults: gpt-5.6-sol to start on, gpt-6-astra at low effort, and none of
// it a model of the catalog's.
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
	if c.DefaultModel != "fireworks/deepseek-v4p1-flash" {
		t.Fatalf("default_model = %q", c.DefaultModel)
	}
	// Aliases the owner dropped while their wire ids still answer retire the
	// alias only (plan 031 X6): a hand entry pointing at the live id stays the
	// user's own model (§3.3), so these rows name no identity.
	for _, r := range c.Retired {
		switch r.Alias {
		case "fireworks/glm-5p3-fast", "fireworks/kimi-k3-fast",
			"openrouter/gpt-5.6-luna", "openrouter/gpt-5.6-sol", "openrouter/gpt-5.6-terra",
			"openrouter/gpt-6-sol":
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
		{"a default that is not a model", func(c *Catalog) { c.DefaultModel = "nope" }, "", "default_model"},
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
// source, catalog, [subagents] or [compaction], so a strict decode refuses
// each — a key can never be shipped by mistake, nor a setting that belongs
// to the user (plan 031 §3.1).
func TestCatalogSchemaRefusesUserKeys(t *testing.T) {
	const head = "version = 1\ndefault_model = \"m\"\n"
	const prov = "\n[providers.p]\nname = \"P\"\ndriver = \"openrouter\"\nenv_keys = [\"P_KEY\"]\n"
	const model = "\n[models.m]\nprovider = \"p\"\nwire_model = \"w\"\n"
	if _, err := parseCatalog(CatalogFile, []byte(head+prov+model)); err != nil {
		t.Fatalf("control: %v", err)
	}
	for _, tc := range []struct{ name, doc, table, key string }{
		{"api_key", head + prov + "api_key = \"" + canary + "\"\n" + model, "providers.p", "api_key"},
		{"provider source", head + prov + "source = \"gx\"\n" + model, "providers.p", "source"},
		{"model source", head + prov + model + "source = \"gx\"\n", "models.m", "source"},
		{"catalog", "catalog = false\n" + head + prov + model, "", "catalog"},
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
