package modeltable

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The merge of the user's files over a catalog (plan 031 §3.2–§3.3). Most
// cases run over testCatalogV1, a small catalog of their own, through
// LoadWith, so they pin the rules and not the shipped contents; the upgrade
// cases at the end run the owner's own files over the shipped catalog.

const testCatalogV1 = `version = 1
default_model = "acme/fast"

[[retired]]
alias = "acme/old"
provider = "acme"
wire_models = ["acme-old-1", "acme-old-2"]

[providers.acme]
name = "Acme"
driver = "openai-compat"
base_url = "https://api.acme.example/v1"
env_keys = ["ACME_API_KEY"]

[providers.router]
name = "Router"
driver = "openrouter"
env_keys = ["ROUTER_API_KEY"]

[models."acme/fast"]
provider = "acme"
wire_model = "acme-fast-1"
name = "Acme Fast"
context_window = 100000
efforts = ["low", "medium", "high"]
default_effort = "medium"

[models."acme/fast".cost]
input = 1.0
output = 2.0

[models."acme/big"]
provider = "acme"
wire_model = "acme-big-1"
efforts = ["low", "high"]
default_effort = "high"

[models."router/m"]
provider = "router"
wire_model = "vendor/m"
`

// testCatalog decodes and validates a test catalog: every case runs over a
// catalog that passes the rules the shipped one must.
func testCatalog(t *testing.T, doc string) *Catalog {
	t.Helper()
	c, err := parseCatalog(CatalogFile, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.validate(CatalogFile); err != nil {
		t.Fatalf("the test catalog is not valid: %v", err)
	}
	return c
}

// loadOver writes the two files ("" leaves one out) and loads them over cat.
func loadOver(t *testing.T, cat *Catalog, providers, models string) (*Table, string) {
	t.Helper()
	dir := writeFiles(t, providers, models)
	tbl, err := LoadWith(dir, cat)
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	return tbl, dir
}

// wantWarnings asserts tbl warned exactly len(want) times, each warning
// holding every substring of its want entry, in order.
func wantWarnings(t *testing.T, tbl *Table, want ...[]string) {
	t.Helper()
	if len(tbl.Warnings) != len(want) {
		t.Fatalf("Warnings = %q, want %d", tbl.Warnings, len(want))
	}
	for i, subs := range want {
		for _, s := range subs {
			if !strings.Contains(tbl.Warnings[i], s) {
				t.Errorf("warning %d = %q, want it to contain %q", i, tbl.Warnings[i], s)
			}
		}
	}
}

// TestMergeCatalogAlone: no user files at all is the catalog, every entry
// shipped, and LoadWith leaves the catalog it was given untouched.
func TestMergeCatalogAlone(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	before := cat.Clone()
	tbl, _ := loadOver(t, cat, "", "")
	if tbl.DefaultModel != "acme/fast" || !reflect.DeepEqual(tbl.Models, before.Models) || !reflect.DeepEqual(tbl.Providers, before.Providers) {
		t.Fatalf("the catalog alone = %+v", tbl)
	}
	for _, alias := range tbl.Aliases() {
		if o := tbl.ModelOrigin(alias); o != OriginShipped {
			t.Errorf("%s origin = %q, want shipped", alias, o)
		}
	}
	if o := tbl.ProviderOrigin("acme"); o != OriginShipped {
		t.Errorf("acme origin = %q", o)
	}
	if tbl.ModelOrigin("nope") != "" || tbl.ProviderOrigin("nope") != "" {
		t.Error("an absent entry has an origin")
	}
	if len(tbl.Warnings) != 0 || len(tbl.RedundantOverrides()) != 0 || tbl.NoCatalog {
		t.Fatalf("warnings %q, redundant %q, NoCatalog %v", tbl.Warnings, tbl.RedundantOverrides(), tbl.NoCatalog)
	}
	// The merge changed nothing of the catalog it was handed.
	tbl.Models["acme/fast"].Efforts[0] = "changed"
	if !reflect.DeepEqual(cat, before) {
		t.Fatal("LoadWith changed the catalog it was given")
	}
}

// TestMergeFieldOverrides: a user entry for a shipped alias overrides only
// the fields it writes; each cost rate on its own (an inherited one stays); an
// efforts override keeps the shipped default only if the new list has it.
func TestMergeFieldOverrides(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	tbl, _ := loadOver(t, cat, "", `version = 1

[models."acme/fast"]
name = "My Fast"
context_window = 50000
efforts = ["low", "high"]

[models."acme/fast".cost]
output = 3.0

[models."acme/big"]
efforts = ["high", "max"]
vision = true
`)
	wantWarnings(t, tbl)
	fast := tbl.Models["acme/fast"]
	if fast.Name != "My Fast" || fast.ContextWindow != 50000 || fast.WireModel != "acme-fast-1" || fast.Provider != "acme" {
		t.Fatalf("acme/fast = %+v", fast)
	}
	if !slices.Equal(fast.Efforts, []string{"low", "high"}) || fast.DefaultEffort != "" {
		t.Fatalf("acme/fast efforts %q default %q: the shipped default \"medium\" is not in the new list, so it clears", fast.Efforts, fast.DefaultEffort)
	}
	if fast.Cost == nil || *fast.Cost.Input != 1.0 || *fast.Cost.Output != 3.0 || fast.Cost.CacheRead != nil {
		t.Fatalf("acme/fast cost = %+v: want input inherited, output overridden", fast.Cost)
	}
	if fast.Source != SourceManual || tbl.ModelOrigin("acme/fast") != OriginOverridden {
		t.Fatalf("source %q origin %q", fast.Source, tbl.ModelOrigin("acme/fast"))
	}
	big := tbl.Models["acme/big"]
	if !slices.Equal(big.Efforts, []string{"high", "max"}) || big.DefaultEffort != "high" || !big.Vision {
		t.Fatalf("acme/big = %+v: the shipped default \"high\" is in the new list, so it stays", big)
	}
	if tbl.ModelOrigin("router/m") != OriginShipped {
		t.Fatal("an untouched model is not shipped")
	}
}

// TestMergeExplicitEmpties: a key written with its zero value is not a key
// left out — `efforts = []` is no effort control, `env_keys = []` is no
// variables, `name = ""` shows the alias, `default_effort = ""` is none.
func TestMergeExplicitEmpties(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	tbl, _ := loadOver(t, cat, `version = 1

[providers.acme]
env_keys = []
api_key = "acme-inline-key-0001"
`, `version = 1

[models."acme/fast"]
efforts = []
name = ""

[models."acme/big"]
default_effort = ""
`)
	wantWarnings(t, tbl)
	if m := tbl.Models["acme/fast"]; m.Efforts != nil || m.DefaultEffort != "" || m.Name != "" {
		t.Fatalf("acme/fast = %+v", m)
	}
	if m := tbl.Models["acme/big"]; m.DefaultEffort != "" || len(m.Efforts) != 2 {
		t.Fatalf("acme/big = %+v", m)
	}
	p := tbl.Providers["acme"]
	if p.EnvKeys != nil || p.BaseURL != "https://api.acme.example/v1" {
		t.Fatalf("acme = %+v", p)
	}
	// With no variables, an exported ACME_API_KEY does not count.
	r, err := tbl.Resolve("acme/big", fakeEnv(map[string]string{"ACME_API_KEY": "exported-key-0001"}))
	if err != nil || r.APIKey.Reveal() != "acme-inline-key-0001" {
		t.Fatalf("Resolve = %v; want the inline key, not the exported one", err)
	}
}

// TestMergeProviderDependentFields (plan 031 §3.2): a driver override keeps
// the shipped base_url only when the driver stays the shipped one, and an
// entry that changes the endpoint without writing env_keys does not inherit
// the shipped ones — an exported ACME_API_KEY must never reach the user's
// own endpoint.
func TestMergeProviderDependentFields(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	env := fakeEnv(map[string]string{"ACME_API_KEY": "exported-acme-0001", "ROUTER_API_KEY": "exported-router-01", "MY_KEY": "my-own-key-0001"})
	for _, tc := range []struct {
		name, entry string
		want        Provider
		key         string // the key acme/big resolves to, "" for none
	}{
		{"a key only inherits everything", `api_key = "stored-key-0001"`,
			Provider{Name: "Acme", Driver: DriverOpenAICompat, BaseURL: "https://api.acme.example/v1", EnvKeys: []string{"ACME_API_KEY"}, APIKey: "stored-key-0001", Source: SourceManual},
			"exported-acme-0001"},
		{"a name only", `name = "Acme Corp"`,
			Provider{Name: "Acme Corp", Driver: DriverOpenAICompat, BaseURL: "https://api.acme.example/v1", EnvKeys: []string{"ACME_API_KEY"}, Source: SourceManual},
			"exported-acme-0001"},
		{"another base_url drops the shipped variables", `base_url = "https://proxy.example/v1"`,
			Provider{Name: "Acme", Driver: DriverOpenAICompat, BaseURL: "https://proxy.example/v1", Source: SourceManual},
			""},
		{"another base_url with its own variables", "base_url = \"https://proxy.example/v1\"\nenv_keys = [\"MY_KEY\"]",
			Provider{Name: "Acme", Driver: DriverOpenAICompat, BaseURL: "https://proxy.example/v1", EnvKeys: []string{"MY_KEY"}, Source: SourceManual},
			"my-own-key-0001"},
		{"the same base_url written keeps them", `base_url = "https://api.acme.example/v1"`,
			Provider{Name: "Acme", Driver: DriverOpenAICompat, BaseURL: "https://api.acme.example/v1", EnvKeys: []string{"ACME_API_KEY"}, Source: SourceManual},
			"exported-acme-0001"},
		{"driver openrouter alone drops base_url and variables", `driver = "openrouter"`,
			Provider{Name: "Acme", Driver: DriverOpenRouter, Source: SourceManual},
			""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _ := loadOver(t, cat, "version = 1\n\n[providers.acme]\n"+tc.entry+"\n", "")
			wantWarnings(t, tbl)
			if got := tbl.Providers["acme"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("acme = %+v, want %+v", got, tc.want)
			}
			if tbl.ProviderOrigin("acme") != OriginOverridden {
				t.Fatalf("origin = %q", tbl.ProviderOrigin("acme"))
			}
			r, err := tbl.Resolve("acme/big", env)
			switch {
			case tc.key == "" && !errors.Is(err, ErrNoAPIKey):
				t.Fatalf("Resolve = %+v, %v; want no key", r.APIKey, err)
			case tc.key != "" && (err != nil || r.APIKey.Reveal() != tc.key):
				t.Fatalf("Resolve = %v; want the key %s", err, tc.key)
			}
		})
	}
}

// TestMergeUserOnlyEntries: a provider or model the catalog does not ship is
// the user's own, complete, under today's rules.
func TestMergeUserOnlyEntries(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	tbl, _ := loadOver(t, cat, `version = 1

[providers.local]
name = "Local"
driver = "openai-compat"
base_url = "http://127.0.0.1:8080/v1"
api_key = "local-key-00000001"
`, `version = 1
default_model = "local/qwen"

[models."local/qwen"]
provider = "local"
wire_model = "qwen"
efforts = ["low"]
default_effort = "low"

[models."acme/extra"]
provider = "acme"
wire_model = "acme-extra-1"
`)
	wantWarnings(t, tbl)
	if tbl.DefaultModel != "local/qwen" || tbl.ModelOrigin("local/qwen") != OriginYours || tbl.ProviderOrigin("local") != OriginYours {
		t.Fatalf("default %q, origins %q %q", tbl.DefaultModel, tbl.ModelOrigin("local/qwen"), tbl.ProviderOrigin("local"))
	}
	if m := tbl.Models["acme/extra"]; m.Provider != "acme" || tbl.ModelOrigin("acme/extra") != OriginYours {
		t.Fatalf("acme/extra = %+v", m)
	}
	if len(tbl.Models) != 5 || len(tbl.Providers) != 3 {
		t.Fatalf("%d models, %d providers", len(tbl.Models), len(tbl.Providers))
	}
}

// TestMergeReleaseNeverBreaksALoad (plan 031 §3.2): a problem a user entry has
// only against what the catalog holds drops that key or entry with one
// warning naming the file, the table and the key, and the load carries on.
func TestMergeReleaseNeverBreaksALoad(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	tbl, dir := loadOver(t, cat, `version = 1

[providers.router]
base_url = "https://router.example/v1"

[providers.half]
driver = "openai-compat"
`, `version = 1
default_model = "gone/model"

[models."acme/fast"]
default_effort = "max"

[models."acme/big"]
provider = "nowhere"

[models."acme/dropped"]
context_window = 1000

[models."half/m"]
provider = "half"
wire_model = "m"

[models."local/ok"]
provider = "acme"
wire_model = "ok-1"
default_effort = "low"

[subagents]
model = "gone/model"
effort = "high"

[subagents.tiers]
opus = "acme/big"
haiku = "gone/too"
`)
	pp, mp := filepath.Join(dir, ProvidersFile), filepath.Join(dir, ModelsFile)
	wantWarnings(t, tbl,
		[]string{pp, "[providers.half] base_url", "missing", "craze does not ship this provider, so the entry is ignored"},
		[]string{pp, "[providers.router] base_url", "must be absent", "craze's shipped driver and base_url are used"},
		[]string{mp, `[models."acme/big"] provider`, `"nowhere"`, "craze's shipped entry is used"},
		[]string{mp, `[models."acme/dropped"]`, "needs provider and wire_model", "the entry is ignored"},
		[]string{mp, `[models."acme/fast"] default_effort`, `"max"`, "the key is ignored"},
		[]string{mp, `[models."half/m"] provider`, `"half"`, "the entry is ignored"},
		[]string{mp, `[models."local/ok"] default_effort`, `"low"`, "the key is ignored"},
		[]string{mp, "default_model", `"gone/model"`, `craze's default "acme/fast" is used`},
		[]string{mp, "[subagents] model", `"gone/model"`, "parent's model"},
		[]string{mp, "[subagents.tiers] haiku", `"gone/too"`, "parent's model"},
	)
	if tbl.DefaultModel != "acme/fast" {
		t.Fatalf("default = %q", tbl.DefaultModel)
	}
	if m := tbl.Models["acme/fast"]; m.DefaultEffort != "medium" {
		t.Fatalf("acme/fast default effort = %q, want the shipped one", m.DefaultEffort)
	}
	if m := tbl.Models["acme/big"]; m.Provider != "acme" || tbl.ModelOrigin("acme/big") != OriginShipped {
		t.Fatalf("acme/big = %+v (%s), want the shipped entry restored", m, tbl.ModelOrigin("acme/big"))
	}
	if m := tbl.Models["local/ok"]; m.DefaultEffort != "" {
		t.Fatalf("local/ok = %+v", m)
	}
	for _, gone := range []string{"acme/dropped", "half/m"} {
		if _, ok := tbl.Models[gone]; ok {
			t.Errorf("%s survived", gone)
		}
	}
	if _, ok := tbl.Providers["half"]; ok {
		t.Error("the incomplete provider survived")
	}
	if p := tbl.Providers["router"]; p.BaseURL != "" || !slices.Equal(p.EnvKeys, []string{"ROUTER_API_KEY"}) {
		t.Fatalf("router = %+v, want the shipped endpoint and its variables", p)
	}
	want := Subagents{Effort: "high", Tiers: map[string]string{"opus": "acme/big"}}
	if !reflect.DeepEqual(tbl.Subagents, want) {
		t.Fatalf("subagents = %+v, want %+v", tbl.Subagents, want)
	}
	if err := tbl.Validate(); err != nil {
		t.Fatalf("the merged table: %v", err)
	}
}

// TestMergeStructuralProblemsStillFail (plan 031 §3.2): what a user file gets
// wrong on its own, whatever the catalog holds — syntax, an unknown key (the
// catalog's own `retired` among them), a value no entry could hold, an invalid
// inline key — fails the load naming the file, the table and the key.
func TestMergeStructuralProblemsStillFail(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	for _, tc := range []struct {
		name, providers, models, file, table, key string
	}{
		{"retired is not a user key", "", "version = 1\n\n[[retired]]\nalias = \"x\"\n", ModelsFile, "", "retired"},
		{"unknown model key", "", "version = 1\n\n[models.\"acme/fast\"]\ncontxt_window = 1\n", ModelsFile, `models."acme/fast"`, "contxt_window"},
		{"unknown driver", "version = 1\n\n[providers.acme]\ndriver = \"anthropic\"\n", "", ProvidersFile, "providers.acme", "driver"},
		// Defined for the Responses driver, and refused until its sign-in
		// lands (plan 033 C12, C14).
		{"the chatgpt driver, not yet enabled", "version = 1\n\n[providers.acme]\ndriver = \"chatgpt\"\n", "", ProvidersFile, "providers.acme", "driver"},
		{"not a URL", "version = 1\n\n[providers.acme]\nbase_url = \"api.example\"\n", "", ProvidersFile, "providers.acme", "base_url"},
		{"a pair that cannot go together", "version = 1\n\n[providers.acme]\ndriver = \"openrouter\"\nbase_url = \"https://x.example\"\n", "", ProvidersFile, "providers.acme", "base_url"},
		{"a blank env_keys name", "version = 1\n\n[providers.acme]\nenv_keys = [\" \"]\n", "", ProvidersFile, "providers.acme", "env_keys"},
		{"a short inline key", "version = 1\n\n[providers.acme]\napi_key = \"zq-1234\"\n", "", ProvidersFile, "providers.acme", "api_key"},
		{"a negative window", "", "version = 1\n\n[models.\"acme/fast\"]\ncontext_window = -1\n", ModelsFile, `models."acme/fast"`, "context_window"},
		{"an effort twice", "", "version = 1\n\n[models.\"acme/fast\"]\nefforts = [\"low\", \"low\"]\n", ModelsFile, `models."acme/fast"`, "efforts"},
		{"a default outside its own efforts", "", "version = 1\n\n[models.\"acme/fast\"]\nefforts = [\"low\"]\ndefault_effort = \"high\"\n", ModelsFile, `models."acme/fast"`, "default_effort"},
		{"an empty provider", "", "version = 1\n\n[models.\"acme/fast\"]\nprovider = \"\"\n", ModelsFile, `models."acme/fast"`, "provider"},
		{"an unknown tool profile", "", "version = 1\n\n[models.\"acme/fast\"]\ntool_profile = \"gpt\"\n", ModelsFile, `models."acme/fast"`, "tool_profile"},
		{"a cost out of range", "", "version = 1\n\n[models.\"acme/fast\".cost]\ninput = -1.0\n", ModelsFile, `models."acme/fast".cost`, "input"},
		{"a tier named inherit", "", "version = 1\n\n[subagents.tiers]\ninherit = \"acme/fast\"\n", ModelsFile, "subagents.tiers", "inherit"},
		{"a compaction out of range", "", "version = 1\n\n[compaction]\nthreshold_percent = 100\n", ModelsFile, "compaction", "threshold_percent"},
		{"a newer version", "version = 2\n", "", ProvidersFile, "", "version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.providers, tc.models)
			_, err := LoadWith(dir, cat)
			wantFileError(t, err, filepath.Join(dir, tc.file), tc.table, tc.key)
			if strings.Contains(err.Error(), "zq-1234") {
				t.Fatal("the error quotes the key")
			}
		})
	}
}

// TestMergeLegacyRule (plan 031 §3.3): an entry the former gx importer wrote
// (source = "gx") yields to the catalog. A model entry is ignored when its
// alias is shipped or retired, and kept as the user's own otherwise; a
// provider entry for a shipped id gives only its key and any variable names
// the catalog lacks, after the catalog's; for another id it is the user's
// own.
func TestMergeLegacyRule(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	tbl, _ := loadOver(t, cat, `version = 1

[providers.acme]
driver = "openai-compat"
base_url = "https://old.acme.example/v1"
env_keys = ["ACME_API_KEY", "ACME_OLD_KEY"]
api_key = "legacy-acme-key-01"
source = "gx"

[providers.other]
driver = "openrouter"
api_key = "legacy-other-key-1"
source = "gx"
`, `version = 1
default_model = "acme/old"

[models."acme/fast"]
provider = "acme"
wire_model = "acme-fast-0"
name = "Stale"
source = "gx"

[models."acme/old"]
provider = "acme"
wire_model = "acme-old-9"
source = "gx"

[models."other/x"]
provider = "other"
wire_model = "x"
source = "gx"
`)
	wantWarnings(t, tbl)
	if m, s := tbl.Models["acme/fast"], cat.Models["acme/fast"]; !reflect.DeepEqual(m, s) || tbl.ModelOrigin("acme/fast") != OriginShipped {
		t.Fatalf("acme/fast = %+v, want the shipped entry", m)
	}
	if _, ok := tbl.Models["acme/old"]; ok {
		t.Fatal("an import's entry for a retired alias survived")
	}
	if tbl.DefaultModel != "acme/fast" {
		t.Fatalf("a retired default fell back to %q", tbl.DefaultModel)
	}
	if m := tbl.Models["other/x"]; m.Source != legacySource || tbl.ModelOrigin("other/x") != OriginYours {
		t.Fatalf("other/x = %+v", m)
	}
	p := tbl.Providers["acme"]
	want := Provider{Name: "Acme", Driver: DriverOpenAICompat, BaseURL: "https://api.acme.example/v1",
		EnvKeys: []string{"ACME_API_KEY", "ACME_OLD_KEY"}, APIKey: "legacy-acme-key-01"}
	if !reflect.DeepEqual(p, want) || tbl.ProviderOrigin("acme") != OriginShipped {
		t.Fatalf("acme = %+v (%s), want the shipped endpoint with the import's key and extra variable", p, tbl.ProviderOrigin("acme"))
	}
	if r, err := tbl.Resolve("acme/big", fakeEnv(map[string]string{"ACME_OLD_KEY": "old-var-key-0001"})); err != nil || r.APIKey.Reveal() != "old-var-key-0001" {
		t.Fatalf("a gx-era variable no longer funds acme: %v", err)
	}
	if tbl.ProviderOrigin("other") != OriginYours {
		t.Fatal("the import's own provider is not the user's")
	}
	if len(tbl.RedundantOverrides()) != 0 {
		t.Fatalf("legacy entries flagged redundant: %q", tbl.RedundantOverrides())
	}
}

// TestMergeRetiredIdentities (plan 031 §3.3): any user model entry, whoever
// wrote it, whose (provider, wire model) a catalog retired is dropped
// silently — for a shipped alias the shipped entry stays; a hand entry for a
// retired alias on a live wire id is the user's own model; a leftover partial
// override of a retired alias goes silently.
func TestMergeRetiredIdentities(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	tbl, _ := loadOver(t, cat, "", `version = 1

[models."mine/dead"]
provider = "acme"
wire_model = "acme-old-2"

[models."acme/big"]
wire_model = "acme-old-1"

[models."acme/old"]
provider = "acme"
wire_model = "acme-new-live"
source = "manual"
`)
	wantWarnings(t, tbl)
	if _, ok := tbl.Models["mine/dead"]; ok {
		t.Fatal("an entry on a retired identity survived")
	}
	if m := tbl.Models["acme/big"]; m.WireModel != "acme-big-1" || tbl.ModelOrigin("acme/big") != OriginShipped {
		t.Fatalf("acme/big = %+v, want the shipped entry", m)
	}
	if m := tbl.Models["acme/old"]; m.WireModel != "acme-new-live" || tbl.ModelOrigin("acme/old") != OriginYours {
		t.Fatalf("acme/old = %+v, want the user's own model on a live wire id", m)
	}

	tbl, _ = loadOver(t, cat, "", "version = 1\n\n[models.\"acme/old\"]\ndefault_effort = \"high\"\n")
	wantWarnings(t, tbl)
	if _, ok := tbl.Models["acme/old"]; ok {
		t.Fatal("a partial override of a retired alias survived")
	}
}

// TestMergeSubagentsDegrade (plan 031 §3.3): [subagents] never fails a load
// for what the catalog changed — a target that is a retired alias falls back
// silently, any other missing one with a warning, and an effort the kept
// model does not offer is dropped with a warning. No file is touched.
func TestMergeSubagentsDegrade(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	models := `version = 1

[subagents]
model = "acme/old"
effort = "max"

[subagents.tiers]
opus = "acme/old"
sonnet = "acme/fast"
`
	tbl, dir := loadOver(t, cat, "", models)
	wantWarnings(t, tbl)
	if want := (Subagents{Effort: "max", Tiers: map[string]string{"sonnet": "acme/fast"}}); !reflect.DeepEqual(tbl.Subagents, want) {
		t.Fatalf("subagents = %+v, want %+v", tbl.Subagents, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ModelsFile)); string(b) != models {
		t.Fatal("the load touched models.toml")
	}

	tbl, _ = loadOver(t, cat, "", "version = 1\n\n[subagents]\nmodel = \"acme/big\"\neffort = \"medium\"\n")
	wantWarnings(t, tbl, []string{"[subagents] effort", `"medium" is not offered by "acme/big"`, "default effort is used"})
	if want := (Subagents{Model: "acme/big"}); !reflect.DeepEqual(tbl.Subagents, want) {
		t.Fatalf("subagents = %+v", tbl.Subagents)
	}
}

// TestMergeDefaultModel (plan 031 §3.2): the user's default_model when it
// names a merged model, the catalog's otherwise — silently for a retired
// alias.
func TestMergeDefaultModel(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	for _, tc := range []struct {
		name, line, want string
		warns            int
	}{
		{"a shipped alias", `default_model = "router/m"`, "router/m", 0},
		{"none", ``, "acme/fast", 0},
		{"a retired alias", `default_model = "acme/old"`, "acme/fast", 0},
		{"an unknown alias", `default_model = "who/knows"`, "acme/fast", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _ := loadOver(t, cat, "", "version = 1\n"+tc.line+"\n")
			if tbl.DefaultModel != tc.want || len(tbl.Warnings) != tc.warns {
				t.Fatalf("default %q warnings %q; want %q and %d", tbl.DefaultModel, tbl.Warnings, tc.want, tc.warns)
			}
		})
	}
}

// TestRedundantOverrides (plan 031 §3.3): a hand entry for a shipped entry
// whose every written field equals the shipped value, and a default_model
// equal to the catalog's, is legal and named; one that differs anywhere, or a
// provider entry that only stores a key, is not.
func TestRedundantOverrides(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	tbl, dir := loadOver(t, cat, `version = 1

[providers.acme]
driver = "openai-compat"
env_keys = ["ACME_API_KEY"]
api_key = "stored-key-0001"
source = "manual"

[providers.router]
api_key = "stored-key-0002"
`, `version = 1
default_model = "acme/fast"

[models."acme/fast"]
provider = "acme"
wire_model = "acme-fast-1"
name = "Acme Fast"
efforts = ["low", "medium", "high"]
default_effort = "medium"
source = "manual"

[models."acme/fast".cost]
input = 1.0

[models."acme/big"]
name = "Different"

[models."router/m"]
source = "manual"
`)
	wantWarnings(t, tbl)
	pp, mp := filepath.Join(dir, ProvidersFile), filepath.Join(dir, ModelsFile)
	want := []string{
		"modeltable: " + pp + ": [providers.acme]: repeats craze's shipped settings — keep only api_key to follow craze's updates",
		"modeltable: " + mp + `: [models."acme/fast"]: repeats craze's shipped entry — delete it to follow craze's updates`,
		"modeltable: " + mp + `: [models."router/m"]: repeats craze's shipped entry — delete it to follow craze's updates`,
		"modeltable: " + mp + ": default_model: repeats craze's shipped default — delete it to follow craze's updates",
	}
	if got := tbl.RedundantOverrides(); !slices.Equal(got, want) {
		t.Fatalf("RedundantOverrides =\n%q\nwant\n%q", got, want)
	}
	for _, line := range tbl.RedundantOverrides() {
		if strings.Contains(line, "stored-key") {
			t.Fatal("a note carries a key")
		}
	}
}

// TestNoCatalogIsTodaysTable (plan 031 P10): `catalog = false` makes the
// directory's files the whole table under the rules before plan 031 — no
// shipped entry, both files needed, default_model required — exactly as
// LoadWith with no catalog reads them; Save writes the key back.
func TestNoCatalogIsTodaysTable(t *testing.T) {
	dir := writeFiles(t, validProviders, validModels)
	merged, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	alone, err := LoadWith(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged, alone) || !reflect.DeepEqual(merged, validTable()) {
		t.Fatalf("catalog = false loads\n%+v\nwant\n%+v", merged, validTable())
	}
	if merged.ModelOrigin("fireworks/kimi-k3") != OriginYours || len(merged.RedundantOverrides()) != 0 {
		t.Fatal("a table without a catalog has shipped entries")
	}
	noDefault := strings.Replace(validModels, "default_model = \"fireworks/kimi-k3\"\n", "", 1)
	dir = writeFiles(t, validProviders, noDefault)
	_, err = Load(dir)
	wantFileError(t, err, filepath.Join(dir, ModelsFile), "", "default_model")
	// An entry missing its provider is an error, not a warning, here.
	partial := validModels + "\n[models.\"extra\"]\ncontext_window = 1\n"
	dir = writeFiles(t, validProviders, partial)
	_, err = Load(dir)
	wantFileError(t, err, filepath.Join(dir, ModelsFile), "models.extra", "provider")

	// Without the key, the same two files merge over the catalog.
	over, err := Load(writeFiles(t, validProviders, strings.Replace(validModels, "catalog = false\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if over.NoCatalog || len(over.Models) <= 2 {
		t.Fatalf("without catalog = false: %d models, NoCatalog %v", len(over.Models), over.NoCatalog)
	}
}

// TestOverlayKeysMatchTheSavedShape: Load reads through the pointer-typed
// overlays and Save writes through providerEntry and modelEntry; the two
// shapes must name exactly the same keys, or Save would write one Load
// refuses (models.toml's `catalog` is the top-level key both docs carry). A
// field tagged "-" is never written (modelEntry.ParallelToolCalls, plan 033
// §3.11), so it names no key.
func TestOverlayKeysMatchTheSavedShape(t *testing.T) {
	keys := func(v any) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			if key := strings.Split(rt.Field(i).Tag.Get("toml"), ",")[0]; key != "-" {
				out = append(out, key)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, pair := range [][2]any{
		{providerOverlay{}, providerEntry{}},
		{modelOverlay{}, modelEntry{}},
		{modelsOverlay{}, modelsDoc{}},
		{providersOverlay{}, providersDoc{}},
	} {
		if a, b := keys(pair[0]), keys(pair[1]); !slices.Equal(a, b) {
			t.Errorf("%T keys %q, %T keys %q", pair[0], a, pair[1], b)
		}
	}
}

// TestEnvValueSkippedOverTheCatalog (plan 031 §3.2, §3.13): a short value in a
// shipped provider's variable is skipped with a warning naming the variable —
// an unrelated short META_API_KEY no longer fails every session — and the
// provider is unfunded unless another key funds it.
func TestEnvValueSkippedOverTheCatalog(t *testing.T) {
	tbl, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(map[string]string{"META_API_KEY": "zq-12", "FIREWORKS_API_KEY": "fw-exported-0001"})
	if _, err := tbl.Keys(env); err != nil {
		t.Fatalf("Keys = %v", err)
	}
	if _, err := tbl.Resolve("muse-spark-1.3", env); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("Resolve on the short key = %v, want ErrNoAPIKey", err)
	}
	if _, err := tbl.Resolve("fireworks/kimi-k3", env); err != nil {
		t.Fatalf("another provider is unaffected: %v", err)
	}
	w := tbl.EnvWarnings(env)
	if len(w) != 1 || !strings.Contains(w[0], "META_API_KEY") || strings.Contains(w[0], "zq-12") {
		t.Fatalf("EnvWarnings = %q", w)
	}
}

// TestCredentialEnvOutlivesFunding (plan 031 §3.2, C2r2): an override that
// moves a shipped provider's endpoint drops the shipped variable as a funding
// source, and one that writes its own env_keys replaces the shipped list —
// but the shipped variables still hold credentials, and so does the variable
// of a user entry the merge dropped. The table keeps every name it knows holds
// a key, apart from funding: CredentialEnvNames lists them for the tool
// environment's filter, and Keys hands the redactor each usable value. Funding
// is untouched: Resolve, Choices, StartModel and Providers still take a key
// from the merged providers' env_keys alone. With `catalog = false` the
// catalog's names are not the table's.
func TestCredentialEnvOutlivesFunding(t *testing.T) {
	const (
		stored   = "stored-proxy-key-01"
		exported = "exported-acme-0001" // the shipped variable of the endpoint the user moved
		replaced = "exported-router-01" // the shipped variable the user's env_keys replaced
		dropped  = "exported-broken-01" // the variable of an entry the merge dropped
	)
	cat := testCatalog(t, testCatalogV1)
	tbl, dir := loadOver(t, cat, `version = 1

[providers.acme]
base_url = "https://proxy.example/v1"
api_key = "`+stored+`"

[providers.router]
env_keys = ["MY_ROUTER_KEY"]

[providers.broken]
name = "Broken"
env_keys = ["BROKEN_API_KEY"]
`, "")
	wantWarnings(t, tbl, []string{"providers.broken", "ignored"})

	want := []string{"ACME_API_KEY", "BROKEN_API_KEY", "MY_ROUTER_KEY", "ROUTER_API_KEY"}
	if got := tbl.CredentialEnvNames(); !slices.Equal(got, want) {
		t.Errorf("CredentialEnvNames = %q, want %q", got, want)
	}
	env := map[string]string{"ACME_API_KEY": exported, "ROUTER_API_KEY": replaced, "BROKEN_API_KEY": dropped}
	keys, err := tbl.Keys(fakeEnv(env))
	if err != nil {
		t.Fatalf("Keys = %v", err)
	}
	var got []string
	for _, k := range keys {
		got = append(got, k.Reveal())
	}
	for label, v := range map[string]string{"the stored key": stored, "the moved endpoint's shipped variable": exported,
		"the replaced shipped variable": replaced, "the dropped entry's variable": dropped} {
		if !slices.Contains(got, v) {
			t.Errorf("Keys lacks %s's value", label) // never the value: a failure names what is missing
		}
	}

	// Funding is the merged providers' alone (§3.2): the shipped variable
	// funds neither the moved endpoint nor the one whose list was replaced.
	if p := tbl.Providers["acme"]; len(p.EnvKeys) != 0 {
		t.Fatalf("acme takes a key from %q, want no variable", p.EnvKeys)
	}
	if r, err := tbl.Resolve("acme/big", fakeEnv(env)); err != nil || r.APIKey.Reveal() != stored {
		t.Fatalf("Resolve(acme/big) = %v; want the stored key, not the exported one", err)
	}
	if _, err := tbl.Resolve("router/m", fakeEnv(env)); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("Resolve(router/m) = %v; want ErrNoAPIKey: ROUTER_API_KEY no longer funds router", err)
	}
	var offered []string
	for _, c := range tbl.Choices(nil, fakeEnv(env), "") {
		offered = append(offered, c.Alias)
	}
	if want := []string{"acme/big", "acme/fast"}; !slices.Equal(slices.Sorted(slices.Values(offered)), want) {
		t.Fatalf("Choices = %q, want %q", offered, want)
	}
	if alias, _, err := tbl.StartModel(nil, fakeEnv(env)); err != nil || alias != "acme/fast" {
		t.Fatalf("StartModel = %q, %v", alias, err)
	}
	infos, err := providersWith(dir, fakeEnv(env), cat)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		switch info.ID {
		case "acme":
			if info.Via != KeyStored || info.EnvVar != "" {
				t.Errorf("Providers: acme via %v %q, want the stored key", info.Via, info.EnvVar)
			}
		case "router":
			if info.Via != KeyNone {
				t.Errorf("Providers: router via %v %q, want not connected", info.Via, info.EnvVar)
			}
		}
	}
	// A value that cannot be a key is skipped as Resolve skips one, and the
	// variable funds nothing, so there is nothing to warn about: EnvWarnings
	// names only variables a provider takes its key from.
	env["ACME_API_KEY"] = "zq-12"
	keys, err = tbl.Keys(fakeEnv(env))
	if err != nil || len(keys) != 3 {
		t.Fatalf("Keys with a short value in ACME_API_KEY = %d keys, %v; want the other three", len(keys), err)
	}
	if w := tbl.EnvWarnings(fakeEnv(env)); len(w) != 0 {
		t.Fatalf("EnvWarnings = %q, want none for a variable no provider takes its key from", w)
	}

	// `catalog = false`: the directory's files are the whole table, and the
	// catalog's names are not among its credentials.
	alone, _ := loadOver(t, cat, `version = 1

[providers.acme]
driver = "openai-compat"
base_url = "https://proxy.example/v1"
api_key = "`+stored+`"
`, `version = 1
catalog = false
default_model = "acme/x"

[models."acme/x"]
provider = "acme"
wire_model = "x"
`)
	if got := alone.CredentialEnvNames(); len(got) != 0 {
		t.Fatalf("catalog = false: CredentialEnvNames = %q, want none", got)
	}
	// A table built in memory knows its providers' names, as they are now.
	mem := validTable()
	setProvider(mem, "fireworks", func(p *Provider) { p.EnvKeys = append(p.EnvKeys, "FW_EXTRA_KEY") })
	if got, want := mem.CredentialEnvNames(), envKeyNames(mem.Providers); !slices.Equal(got, want) || !slices.Contains(got, "FW_EXTRA_KEY") {
		t.Fatalf("in memory: CredentialEnvNames = %q, want %q", got, want)
	}
	if got := tbl.CredentialEnvNames(); len(got) > 0 {
		got[0] = "changed"
		if tbl.CredentialEnvNames()[0] == "changed" {
			t.Fatal("CredentialEnvNames returned the table's own storage")
		}
	}
}

// TestReleaseTransition (plan 031 A4, r2-6): a directory nobody edits, loaded
// over catalog v1 and then over v2 — which adds a model, renames one (the old
// alias retired, its wire id carried by the new one), retires another with
// its wire id, and drops an effort —
// loads both times with no error, and the v2 table reflects every change: the
// legacy entries of the retired and renamed aliases vanish, a hand entry on a
// retired wire id is dropped, a default_effort v2 no longer offers is dropped
// with a warning, [subagents] naming the retired alias falls back, and a hand
// override of the shipped default pointed at a wire id v2 retires restores the
// shipped default, which survives.
func TestReleaseTransition(t *testing.T) {
	v1 := testCatalog(t, testCatalogV1)
	v2 := testCatalog(t, `version = 1
default_model = "acme/fast"

[[retired]]
alias = "acme/old"
provider = "acme"
wire_models = ["acme-old-1", "acme-old-2"]

# acme/big is renamed acme/huge: the wire id lives on under the new alias,
# so only the old alias is retired.
[[retired]]
alias = "acme/big"
provider = "acme"

[[retired]]
alias = "router/m"
provider = "router"
wire_models = ["vendor/m"]

[[retired]]
alias = "acme/fast-legacy"
provider = "acme"
wire_models = ["acme-fast-0"]

[providers.acme]
name = "Acme"
driver = "openai-compat"
base_url = "https://api.acme.example/v1"
env_keys = ["ACME_API_KEY"]

[providers.router]
name = "Router"
driver = "openrouter"
env_keys = ["ROUTER_API_KEY"]

[models."acme/fast"]
provider = "acme"
wire_model = "acme-fast-1"
name = "Acme Fast"
context_window = 100000
efforts = ["low", "high"]
default_effort = "high"

[models."acme/huge"]
provider = "acme"
wire_model = "acme-big-1"
efforts = ["low", "high"]
default_effort = "high"

[models."router/m2"]
provider = "router"
wire_model = "vendor/m2"
`)
	providers := `version = 1

[providers.acme]
driver = "openai-compat"
base_url = "https://api.acme.example/v1"
env_keys = ["ACME_API_KEY"]
api_key = "legacy-acme-key-01"
source = "gx"
`
	models := `version = 1
default_model = "acme/big"

[models."acme/big"]
provider = "acme"
wire_model = "acme-big-1"
source = "gx"

[models."router/m"]
provider = "router"
wire_model = "vendor/m"
source = "gx"

[models."acme/fast"]
default_effort = "medium"

[models."my/fast-legacy"]
provider = "acme"
wire_model = "acme-fast-0"

[subagents]
model = "acme/big"
`
	dir := writeFiles(t, providers, models)
	before, err := LoadWith(dir, v1)
	if err != nil {
		t.Fatalf("over v1: %v", err)
	}
	wantWarnings(t, before)
	if before.DefaultModel != "acme/big" || before.Subagents.Model != "acme/big" || before.Models["acme/fast"].DefaultEffort != "medium" {
		t.Fatalf("over v1 = default %q, subagents %+v, fast %+v", before.DefaultModel, before.Subagents, before.Models["acme/fast"])
	}

	after, err := LoadWith(dir, v2)
	if err != nil {
		t.Fatalf("over v2: %v", err)
	}
	wantWarnings(t, after, []string{`[models."acme/fast"] default_effort`, `"medium"`, "the key is ignored"})
	if got, want := after.Aliases(), []string{"acme/fast", "acme/huge", "router/m2"}; !slices.Equal(got, want) {
		t.Fatalf("over v2 aliases = %q, want %q", got, want)
	}
	if after.DefaultModel != "acme/fast" || !reflect.DeepEqual(after.Subagents, Subagents{}) || after.Models["acme/fast"].DefaultEffort != "high" {
		t.Fatalf("over v2 = default %q, subagents %+v, fast %+v", after.DefaultModel, after.Subagents, after.Models["acme/fast"])
	}
	if r, err := after.Resolve("acme/huge", fakeEnv(nil)); err != nil || r.APIKey.Reveal() != "legacy-acme-key-01" {
		t.Fatalf("the new model is not funded by the old key: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ModelsFile)); string(b) != models {
		t.Fatal("a load rewrote models.toml")
	}

	// r2-6: a hand override of the shipped default whose wire id the next
	// catalog retires restores the shipped default, so the default survives.
	v3 := v2.Clone()
	v3.Retired = append(v3.Retired, Retired{Alias: "acme/fast-rc", Provider: "acme", WireModels: []string{"acme-fast-rc"}})
	pinned, err := LoadWith(writeFiles(t, "", "version = 1\n\n[models.\"acme/fast\"]\nwire_model = \"acme-fast-rc\"\n"), v2)
	if err != nil || pinned.Models["acme/fast"].WireModel != "acme-fast-rc" {
		t.Fatalf("over v2 the override stands: %v", err)
	}
	dir = writeFiles(t, "", "version = 1\n\n[models.\"acme/fast\"]\nwire_model = \"acme-fast-rc\"\n")
	restored, err := LoadWith(dir, v3)
	if err != nil {
		t.Fatal(err)
	}
	wantWarnings(t, restored)
	if m := restored.Models["acme/fast"]; m.WireModel != "acme-fast-1" || restored.DefaultModel != "acme/fast" {
		t.Fatalf("over v3 acme/fast = %+v, default %q", m, restored.DefaultModel)
	}
}

// TestOwnerFilesUpgrade is plan 031 §3.3's worked example, on key-free
// replicas of the owner's own files (testdata/upgrade; the providers file's
// keys are dummies): the 2026-09-29 models.toml over the gx-era
// providers.toml loads to exactly the shipped catalog's models, all four
// providers funded by their inline keys, the default unchanged — the twelve gx
// entries ignored, the manual deepseek entry a no-op override named as
// redundant, with default_model — and no warning. The import-era models.toml
// of an older machine loads to the same models: its retired aliases vanish
// silently and its retired default falls back to the catalog's.
func TestOwnerFilesUpgrade(t *testing.T) {
	cat, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", "upgrade", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	providers := read("providers-gx.toml")
	for _, tc := range []struct {
		models    string
		redundant int
	}{
		{"models-2026-09-29.toml", 2},
		{"models-import-era.toml", 0},
	} {
		t.Run(tc.models, func(t *testing.T) {
			dir := writeFiles(t, providers, read(tc.models))
			tbl, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			wantWarnings(t, tbl)
			if tbl.DefaultModel != cat.DefaultModel {
				t.Fatalf("default = %q", tbl.DefaultModel)
			}
			if !slices.Equal(tbl.Aliases(), slices.Sorted(maps.Keys(cat.Models))) {
				t.Fatalf("aliases = %q", tbl.Aliases())
			}
			for alias, m := range tbl.Models {
				want := cat.Models[alias]
				want.Source = m.Source // the manual entry's source is its own
				if !reflect.DeepEqual(m, want) {
					t.Errorf("%s = %+v\nwant %+v", alias, m, want)
				}
			}
			for id := range cat.Providers {
				if tbl.Providers[id].APIKey == "" {
					t.Errorf("provider %s lost its inline key", id)
				}
			}
			for _, alias := range tbl.Aliases() {
				if _, err := tbl.Resolve(alias, fakeEnv(nil)); err != nil {
					t.Errorf("%s is not funded: %v", alias, err)
				}
			}
			if !slices.Contains(tbl.Providers["meta"].EnvKeys, "MODEL_API_KEY") {
				t.Error("meta lost the gx-era MODEL_API_KEY the old file named")
			}
			if n := len(tbl.RedundantOverrides()); n != tc.redundant {
				t.Fatalf("RedundantOverrides = %q, want %d", tbl.RedundantOverrides(), tc.redundant)
			}
		})
	}
}
