package modeltable

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The Anthropic seeds and the account's model list in a load (plan 044 §3.5,
// A4): the shipped catalog with a key, and a list file written beside the
// model files, bound to a key by its id.

// The two keys a list can be bound to. Neither is anything like a real one.
const (
	anthropicKeyA = "sk-ant-canary-not-a-secret-000A"
	anthropicKeyB = "sk-ant-canary-not-a-secret-000B"
)

// withAnthropicEnv makes vars the environment a load reads the Anthropic key
// from (anthropicGetenv) for the rest of the test, and returns the same
// getenv for Resolve.
func withAnthropicEnv(t *testing.T, vars map[string]string) func(string) string {
	t.Helper()
	env := fakeEnv(vars)
	old := anthropicGetenv
	anthropicGetenv = env
	t.Cleanup(func() { anthropicGetenv = old })
	return env
}

// writeAnthropicList writes dir's account list, bound to key, as the fetcher
// writes it.
func writeAnthropicList(t *testing.T, dir, key string, models []AnthropicModel) {
	t.Helper()
	doc := AnthropicModels{Version: AnthropicModelsVersion, FetchedAt: time.Now().UTC(), KeyID: AnthropicKeyID(key), Models: models}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AnthropicModelsFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedEntries is the account list that names every shipped seed as the
// research list did (plan 044 research/models-list-2026-10-11.json): the
// seeds' own settings, read back as the list says them.
func seedEntries(t *testing.T) []AnthropicModel {
	t.Helper()
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var out []AnthropicModel
	for _, alias := range slices.Sorted(maps.Keys(c.Models)) {
		m := c.Models[alias]
		if m.Provider != AnthropicProvider {
			continue
		}
		created, err := time.Parse(time.DateOnly, m.Released)
		if err != nil {
			t.Fatal(err)
		}
		adaptive := m.Thinking == ThinkingAdaptive
		out = append(out, AnthropicModel{ID: m.WireModel, DisplayName: m.Name, CreatedAt: created, Line: m.Family,
			MaxInputTokens: int64(m.ContextWindow), MaxTokens: 128000, Efforts: m.Efforts, ImageInput: true,
			AdaptiveThinking: &adaptive, Lifecycle: "active"})
	}
	return out
}

// opus6 is a model the account lists that no seed names: a newer Opus.
func opus6() AnthropicModel {
	adaptive := true
	return AnthropicModel{ID: "claude-opus-6", DisplayName: "Claude Opus 6", CreatedAt: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC),
		Line: "opus", MaxInputTokens: 2000000, MaxTokens: 256000, Efforts: []string{"max", "low", "ultra", "high"},
		ImageInput: true, AdaptiveThinking: &adaptive, Lifecycle: "active"}
}

// loadShipped loads the shipped catalog over dir's files.
func loadShipped(t *testing.T, dir string) *Table {
	t.Helper()
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

// anthropicAliases are tbl's models on the Anthropic provider, sorted.
func anthropicAliases(tbl *Table) []string {
	var out []string
	for _, alias := range tbl.Aliases() {
		if tbl.Models[alias].Provider == AnthropicProvider {
			out = append(out, alias)
		}
	}
	return out
}

// TestAnthropicSeedsAlone (A4): with no account list the seeds stand alone,
// each at medium effort when it offers it (D3; Haiku 4.5 offers none), and a
// key alone starts on Opus 5.5 at medium. The control: with no key the
// provider's models are not offered and a session does not start there.
func TestAnthropicSeedsAlone(t *testing.T) {
	env := withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	tbl := loadShipped(t, t.TempDir())
	wantWarnings(t, tbl)
	if got := anthropicAliases(tbl); len(got) != 13 {
		t.Fatalf("Anthropic models = %q, want the thirteen seeds", got)
	}
	for _, alias := range anthropicAliases(tbl) {
		m := tbl.Models[alias]
		want := "medium"
		if m.Efforts == nil {
			want = ""
		}
		if m.DefaultEffort != want {
			t.Errorf("%s default effort = %q, want %q", alias, m.DefaultEffort, want)
		}
	}
	start, err := tbl.StartPick(nil, env)
	if err != nil || start != (Start{Alias: "anthropic/claude-opus-5-5", Effort: "medium"}) {
		t.Fatalf("start = %+v, %v", start, err)
	}
	if r, err := tbl.Resolve("anthropic/claude-opus-4-5-20251101", env); err != nil || r.Thinking != ThinkingNone {
		t.Fatalf("Opus 4.5 resolves %+v, %v; want no thinking", r, err)
	}

	// The control: no key, nothing offered, no start there.
	none := fakeEnv(nil)
	for _, c := range tbl.Choices(nil, none, "") {
		if c.Provider == AnthropicProvider {
			t.Fatalf("an unfunded Anthropic model is offered: %+v", c)
		}
	}
	if _, err := tbl.StartPick(nil, none); !errors.Is(err, ErrNothingFunded) {
		t.Fatalf("no key: err = %v, want ErrNothingFunded", err)
	}
}

// TestAnthropicListAddsANewModel (A4): a list bound to the key that adds a
// model no seed names makes it a model — its settings from the entry, no
// price — and, being the newest Opus, the start; the seeds the list names keep
// their own settings, whatever the list says of them. The control: the same
// file bound to another key is ignored — the seeds alone, the start Opus 5.5.
func TestAnthropicListAddsANewModel(t *testing.T) {
	env := withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	sonnet6 := opus6()
	sonnet6.ID, sonnet6.Line, sonnet6.Efforts = "claude-sonnet-6", "sonnet", []string{"low", "medium"}
	entries := append(seedEntries(t), opus6(), sonnet6)
	for i := range entries {
		if entries[i].ID == "claude-opus-5-5" {
			entries[i].DisplayName = "Something Else" // a seed's name is craze's
			entries[i].MaxInputTokens = 5
		}
	}
	dir := t.TempDir()
	writeAnthropicList(t, dir, anthropicKeyA, entries)
	tbl := loadShipped(t, dir)
	wantWarnings(t, tbl)

	m, ok := tbl.Models["anthropic/claude-opus-6"]
	want := Model{Provider: AnthropicProvider, WireModel: "claude-opus-6", Name: "Claude Opus 6", ContextWindow: 2000000,
		MaxOutputTokens: 64000, Efforts: []string{"low", "high", "max"}, DefaultEffort: "", Vision: true,
		Thinking: ThinkingAdaptive, Family: "opus", Released: "2026-10-10"}
	if !ok || m.Name != want.Name || m.ContextWindow != want.ContextWindow || m.MaxOutputTokens != want.MaxOutputTokens ||
		!slices.Equal(m.Efforts, want.Efforts) || m.Vision != want.Vision || m.Thinking != want.Thinking ||
		m.Family != want.Family || m.Released != want.Released || m.Cost != nil {
		t.Fatalf("anthropic/claude-opus-6 = %+v (present %v)\nwant %+v", m, ok, want)
	}
	// It offers no medium, so it has no default effort: its own levels only.
	// One that does starts at medium, as every Anthropic model does (D3).
	if m.DefaultEffort != "" {
		t.Fatalf("default effort = %q, want none: the model does not offer medium", m.DefaultEffort)
	}
	if e := tbl.Models["anthropic/claude-sonnet-6"].DefaultEffort; e != "medium" {
		t.Fatalf("a list model offering medium starts at %q, want medium", e)
	}
	if _, priced := tbl.Price(AnthropicProvider, "claude-opus-6"); priced {
		t.Fatal("a model only the list names is priced")
	}
	if _, priced := tbl.Price(AnthropicProvider, "claude-opus-5-5"); !priced {
		t.Fatal("a seed the list names lost its price")
	}
	if o := tbl.ModelOrigin("anthropic/claude-opus-6"); o != OriginShipped {
		t.Fatalf("origin = %q", o)
	}
	if s := tbl.Models["anthropic/claude-opus-5-5"]; s.Name != "Claude Opus 5.5" || s.ContextWindow != 1000000 {
		t.Fatalf("the seed = %+v, want the catalog's own settings", s)
	}
	start, err := tbl.StartPick(nil, env)
	if err != nil || start.Alias != "anthropic/claude-opus-6" {
		t.Fatalf("start = %+v, %v; want the newest Opus the account lists", start, err)
	}
	if alias, ok := tbl.NewestOfFamily(AnthropicProvider, "opus"); !ok || alias != "anthropic/claude-opus-6" {
		t.Fatalf("NewestOfFamily(opus) = %q, %v", alias, ok)
	}

	// The control: the list is another key's.
	other := t.TempDir()
	writeAnthropicList(t, other, anthropicKeyB, entries)
	seeds := loadShipped(t, other)
	wantWarnings(t, seeds)
	if _, ok := seeds.Models["anthropic/claude-opus-6"]; ok {
		t.Fatal("another key's list was used")
	}
	if start, err := seeds.StartPick(nil, env); err != nil || start != (Start{Alias: "anthropic/claude-opus-5-5", Effort: "medium"}) {
		t.Fatalf("another key's list: start = %+v, %v", start, err)
	}
}

// TestAnthropicListKeyBinding (P7, A4): the list is used only when it was
// fetched with the key that funds the provider at load — the first of its
// env_keys set to a usable key (the user's own, when providers.toml writes
// them), else the inline key. A list fetched with key A shows when A is
// exported; after a switch to key B whose fetch failed (the file is still A's)
// the seeds show; an exported A beats a stored B; a stored key binds the list
// when nothing is exported; and a user's own variable is read in place of the
// shipped one.
func TestAnthropicListKeyBinding(t *testing.T) {
	entries := append(seedEntries(t), opus6())
	for _, tc := range []struct {
		name      string
		env       map[string]string
		providers string
		listKey   string
		used      bool
	}{
		{"the exported key's list", map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA}, "", anthropicKeyA, true},
		{"another key's list", map[string]string{"ANTHROPIC_API_KEY": anthropicKeyB}, "", anthropicKeyA, false},
		{"no key", nil, "", anthropicKeyA, false},
		{"the exported key beats the stored one", map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA},
			"version = 1\n\n[providers.anthropic]\napi_key = \"" + anthropicKeyB + "\"\n", anthropicKeyA, true},
		{"the stored key's list, against the exported one", map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA},
			"version = 1\n\n[providers.anthropic]\napi_key = \"" + anthropicKeyB + "\"\n", anthropicKeyB, false},
		{"the stored key alone", nil, "version = 1\n\n[providers.anthropic]\napi_key = \"" + anthropicKeyB + "\"\n", anthropicKeyB, true},
		{"env_keys = [] opts the exported key out", map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA},
			"version = 1\n\n[providers.anthropic]\nenv_keys = []\napi_key = \"" + anthropicKeyB + "\"\n", anthropicKeyB, true},
		{"the user's own variable", map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA, "MY_CLAUDE_KEY": anthropicKeyB},
			"version = 1\n\n[providers.anthropic]\nenv_keys = [\"MY_CLAUDE_KEY\"]\n", anthropicKeyB, true},
		{"a value too short to be a key is no key", map[string]string{"ANTHROPIC_API_KEY": "short"}, "", "short", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAnthropicEnv(t, tc.env)
			dir := writeFiles(t, tc.providers, "")
			writeAnthropicList(t, dir, tc.listKey, entries)
			tbl := loadShipped(t, dir)
			wantWarnings(t, tbl)
			if _, used := tbl.Models["anthropic/claude-opus-6"]; used != tc.used {
				t.Fatalf("the list used = %v, want %v", used, tc.used)
			}
		})
	}
}

// TestAnthropicListHidesUnnamedSeeds (A4): a seed the list does not name is
// not offered, and a user's entry left over for it is ignored without a word
// — a complete entry of the user's under its alias is their own model all
// the same. The control: with the seed named, the same entry overrides it.
func TestAnthropicListHidesUnnamedSeeds(t *testing.T) {
	env := withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	const hidden = "anthropic/claude-opus-4-5-20251101"
	var without []AnthropicModel
	for _, e := range seedEntries(t) {
		if e.ID != "claude-opus-4-5-20251101" {
			without = append(without, e)
		}
	}
	const override = "version = 1\n\n[models.\"" + hidden + "\"]\nname = \"Mine\"\n"
	dir := writeFiles(t, "", override)
	writeAnthropicList(t, dir, anthropicKeyA, without)
	tbl := loadShipped(t, dir)
	wantWarnings(t, tbl)
	if _, ok := tbl.Models[hidden]; ok {
		t.Fatalf("%s is offered though the account does not list it", hidden)
	}
	if got := len(anthropicAliases(tbl)); got != 12 {
		t.Fatalf("%d Anthropic models, want 12", got)
	}
	// Still a model craze knows: a user's own alias of it takes no thinking.
	mine := writeFiles(t, "", "version = 1\n\n[models.mine]\nprovider = \"anthropic\"\nwire_model = \"claude-opus-4-5-20251101\"\n")
	writeAnthropicList(t, mine, anthropicKeyA, without)
	own := loadShipped(t, mine)
	if r, err := own.Resolve("mine", env); err != nil || r.Thinking != ThinkingNone {
		t.Fatalf("the user's alias of a hidden seed resolves %+v, %v; want no thinking", r, err)
	}

	// The control: the seed listed, the entry overrides it.
	listed := writeFiles(t, "", override)
	writeAnthropicList(t, listed, anthropicKeyA, seedEntries(t))
	tbl = loadShipped(t, listed)
	if m := tbl.Models[hidden]; m.Name != "Mine" || tbl.ModelOrigin(hidden) != OriginOverridden {
		t.Fatalf("%s = %+v (%s), want the user's override", hidden, m, tbl.ModelOrigin(hidden))
	}
}

// TestAnthropicListNeverOffersARetiredSeed (P12, A4): a seed the catalog
// retired — its alias in [[retired]] with its wire id — is never offered
// again, though the account still lists it. The control: the same list over
// the catalog that ships the seed offers it.
func TestAnthropicListNeverOffersARetiredSeed(t *testing.T) {
	withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	const alias, wire = "anthropic/claude-opus-4-6", "claude-opus-4-6"
	for _, tc := range []struct {
		name    string
		retired []string
		offered bool
	}{
		{"retired with its wire id", []string{wire}, false},
		{"its alias alone retired", nil, false},
		{"the control: shipped", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ShippedCatalog()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.offered {
				delete(c.Models, alias)
				c.Retired = append(c.Retired, Retired{Alias: alias, Provider: AnthropicProvider, WireModels: tc.retired})
				if err := c.validate(CatalogFile); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			writeAnthropicList(t, dir, anthropicKeyA, seedEntries(t))
			tbl, err := LoadWith(dir, c)
			if err != nil {
				t.Fatal(err)
			}
			wantWarnings(t, tbl)
			if _, ok := tbl.Models[alias]; ok != tc.offered {
				t.Fatalf("%s offered = %v, want %v", alias, ok, tc.offered)
			}
		})
	}
	// And a seed removed without a [[retired]] row breaks the alias history
	// (TestShippedCatalogHistory's rule, on an Anthropic seed).
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	delete(c.Models, alias)
	if bad := checkAliasHistory(c, shippedAliasHistory(t)); len(bad) != 1 || !strings.HasPrefix(bad[0], alias+" was shipped") {
		t.Fatalf("a seed removed without retiring it reports %q", bad)
	}
}

// TestAnthropicListNewest (O3's trim knob, A4): list = "newest" keeps only
// each family's newest model, the seeds alone or with the account's list; a
// session running on a model it drops keeps it through Carry, resolving as it
// did. The control: list = "all" keeps every one.
func TestAnthropicListNewest(t *testing.T) {
	env := withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	newest := func(t *testing.T) *Catalog {
		c, err := ShippedCatalog()
		if err != nil {
			t.Fatal(err)
		}
		c.Anthropic.List = AnthropicListNewest
		if err := c.validate(CatalogFile); err != nil {
			t.Fatal(err)
		}
		return c
	}
	seedsOnly, err := LoadWith(t.TempDir(), newest(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"anthropic/claude-fable-5-1", "anthropic/claude-haiku-5-5", "anthropic/claude-opus-5-5", "anthropic/claude-sonnet-5-5"}
	if got := anthropicAliases(seedsOnly); !slices.Equal(got, want) {
		t.Fatalf("newest, the seeds alone: %q, want %q", got, want)
	}
	dir := t.TempDir()
	writeAnthropicList(t, dir, anthropicKeyA, append(seedEntries(t), opus6()))
	listed, err := LoadWith(dir, newest(t))
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"anthropic/claude-fable-5-1", "anthropic/claude-haiku-5-5", "anthropic/claude-opus-6", "anthropic/claude-sonnet-5-5"}
	if got := anthropicAliases(listed); !slices.Equal(got, want) {
		t.Fatalf("newest, with the list: %q, want %q", got, want)
	}

	// A session on Opus 4.5 under list = "all" takes the trimmed table.
	all := loadShipped(t, dir)
	const running = "anthropic/claude-opus-4-5-20251101"
	if _, ok := listed.Models[running]; ok {
		t.Fatalf("%s survived the trim", running)
	}
	if !listed.Carry(all, running) {
		t.Fatal("Carry refused")
	}
	if r, err := listed.Resolve(running, env); err != nil || r.Thinking != ThinkingNone || r.DefaultEffort != "medium" {
		t.Fatalf("carried: Resolve = %+v, %v", r, err)
	}
	if !slices.ContainsFunc(listed.Choices(nil, env, running), func(c Choice) bool { return c.Alias == running }) {
		t.Fatal("the running model is not offered")
	}

	// The control: "all".
	if got := len(anthropicAliases(all)); got != 14 {
		t.Fatalf("all: %d Anthropic models, want the thirteen seeds and Opus 6", got)
	}
}

// TestAnthropicLearnedFollowTheIdentity (P11, A4): an entry that points a
// seed at another wire model forgets its family, release and thinking and
// takes the new identity's — a seed's or a list model's — so the start, the
// newest of a family and the thinking all follow it; an identity craze does
// not know is in no family and asks for adaptive thinking. The control: the
// seed left alone keeps its own.
func TestAnthropicLearnedFollowTheIdentity(t *testing.T) {
	env := withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	dir := writeFiles(t, "", `version = 1

[models."anthropic/claude-opus-5-5"]
wire_model = "claude-haiku-4-5-20251001"

[models."anthropic/claude-sonnet-5-5"]
wire_model = "claude-sonnet-9-unknown"

[models.mine]
provider = "anthropic"
wire_model = "claude-opus-6"
`)
	writeAnthropicList(t, dir, anthropicKeyA, append(seedEntries(t), opus6()))
	tbl := loadShipped(t, dir)
	// Opus 5.5's price, now on Haiku 4.5's identity, disagrees with Haiku's.
	wantWarnings(t, tbl, []string{"anthropic/claude-haiku-4-5-20251001 and anthropic/claude-opus-5-5", "different cost"})
	for alias, want := range map[string]learnedFacts{
		"anthropic/claude-opus-5-5":   {Thinking: ThinkingNone, Family: "haiku", Released: "2025-10-15"},
		"anthropic/claude-sonnet-5-5": {},
		"mine":                        {Thinking: ThinkingAdaptive, Family: "opus", Released: "2026-10-10"},
		"anthropic/claude-opus-5":     {Thinking: ThinkingAdaptive, Family: "opus", Released: "2026-07-24"}, // the control
	} {
		if got := tbl.factsOf(tbl.Models[alias]); got != want {
			t.Errorf("%s facts = %+v, want %+v", alias, got, want)
		}
	}
	if r, err := tbl.Resolve("anthropic/claude-sonnet-5-5", env); err != nil || r.Thinking != ThinkingAdaptive {
		t.Fatalf("an unknown id resolves %+v, %v; want adaptive", r, err)
	}
	if alias, _ := tbl.NewestOfFamily(AnthropicProvider, "haiku"); alias != "anthropic/claude-haiku-5-5" {
		t.Fatalf("newest haiku = %q", alias)
	}
	if alias, _ := tbl.NewestOfFamily(AnthropicProvider, "sonnet"); alias != "anthropic/claude-sonnet-5" {
		t.Fatalf("newest sonnet = %q: the repointed Sonnet 5.5 is no sonnet now", alias)
	}
	// The user's alias of Opus 6 and the list's own share the newest
	// release; the first alias wins.
	if alias, _ := tbl.NewestOfFamily(AnthropicProvider, "opus"); alias != "anthropic/claude-opus-6" {
		t.Fatalf("newest opus = %q", alias)
	}
	for _, tc := range []struct{ provider, family string }{{AnthropicProvider, "nope"}, {"fireworks", "opus"}, {AnthropicProvider, ""}} {
		if alias, ok := tbl.NewestOfFamily(tc.provider, tc.family); ok {
			t.Errorf("NewestOfFamily(%q, %q) = %q", tc.provider, tc.family, alias)
		}
	}
}

// TestAnthropicListEntries: the list is read entry by entry — one that does
// not decode, has an id craze cannot name, is listed twice, or is no model
// costs only itself, with a warning naming the file and the entry — and a
// model the list says is deprecated, a seed's or its own, says so in its
// name. A file this craze cannot read as a list (not JSON, another version,
// a symlink) is one warning, and the seeds stand. The control: the clean
// entries alone draw no warning.
func TestAnthropicListEntries(t *testing.T) {
	withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	clean := append(seedEntries(t), opus6())
	for i := range clean {
		if clean[i].ID == "claude-opus-4-6" {
			clean[i].Lifecycle = "deprecated"
		}
	}
	old := AnthropicModel{ID: "claude-old-1", Lifecycle: "deprecated", MaxTokens: 4096}
	raw := func(m any) json.RawMessage {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var models []json.RawMessage
	for _, e := range clean {
		models = append(models, raw(e))
	}
	models = append(models,
		json.RawMessage(`"not an entry"`),                         // models[14]
		raw(AnthropicModel{ID: "bad id/x"}),                       // models[15]
		raw(opus6()),                                              // models[16], twice
		raw(AnthropicModel{ID: "claude-neg", MaxInputTokens: -1}), // models[17]
		raw(old), // models[18]
	)
	write := func(dir string, doc any) {
		if err := os.WriteFile(filepath.Join(dir, AnthropicModelsFile), raw(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	write(dir, map[string]any{"version": 1, "fetched_at": time.Now().UTC(), "key_id": AnthropicKeyID(anthropicKeyA), "models": models})
	tbl := loadShipped(t, dir)
	path := filepath.Join(dir, AnthropicModelsFile)
	wantWarnings(t, tbl,
		[]string{path, "models[14]", "not a model entry"},
		[]string{path, "models[15]", "not one craze can name"},
		[]string{path, "models[16]", `"claude-opus-6"`, "listed twice"},
		[]string{path, "models[17]", `"claude-neg"`, "context_window"},
	)
	if m := tbl.Models["anthropic/claude-opus-4-6"]; m.Name != "Claude Opus 4.6 (deprecated)" || m.Cost == nil {
		t.Fatalf("the deprecated seed = %+v", m)
	}
	if m := tbl.Models["anthropic/claude-old-1"]; m.Name != "anthropic/claude-old-1 (deprecated)" || m.MaxOutputTokens != 4096 || m.Thinking != "" || m.Family != "" {
		t.Fatalf("the deprecated list model = %+v", m)
	}
	if !slices.Contains(tbl.Aliases(), "anthropic/claude-opus-6") || slices.Contains(tbl.Aliases(), "anthropic/claude-neg") {
		t.Fatalf("aliases = %q", tbl.Aliases())
	}

	// The control: the clean entries alone.
	control := t.TempDir()
	writeAnthropicList(t, control, anthropicKeyA, clean)
	wantWarnings(t, loadShipped(t, control))

	for name, setup := range map[string]func(dir string){
		"not JSON":        func(dir string) { _ = os.WriteFile(filepath.Join(dir, AnthropicModelsFile), []byte("{"), 0o600) },
		"another version": func(dir string) { write(dir, map[string]any{"version": 2, "key_id": AnthropicKeyID(anthropicKeyA)}) },
		"a symlink": func(dir string) {
			target := filepath.Join(t.TempDir(), "list.json")
			writeAnthropicList(t, filepath.Dir(target), anthropicKeyA, clean)
			if err := os.Symlink(filepath.Join(filepath.Dir(target), AnthropicModelsFile), filepath.Join(dir, AnthropicModelsFile)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setup(dir)
			tbl := loadShipped(t, dir)
			wantWarnings(t, tbl, []string{filepath.Join(dir, AnthropicModelsFile), "not an Anthropic model list"})
			if got := len(anthropicAliases(tbl)); got != 13 {
				t.Fatalf("%d Anthropic models, want the thirteen seeds", got)
			}
		})
	}
}

// TestAnthropicListWithNothingStandingIsIgnored (C4 review 1): a list bound
// to the key that has no entry that stands — its models missing, null or
// empty, every entry broken or with an id craze cannot name, or every one
// retired — is never authoritative: it is the one warning of a file craze
// cannot read, and the seeds stand, so a damaged file never takes every
// Anthropic model away. The control: the same envelope with one entry that
// stands is the list, and hides every seed it does not name.
func TestAnthropicListWithNothingStandingIsIgnored(t *testing.T) {
	withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	envelope := func(models string) string {
		at, _ := json.Marshal(time.Now().UTC())
		doc := `{"version":1,"fetched_at":` + string(at) + `,"key_id":"` + AnthropicKeyID(anthropicKeyA) + `"`
		if models != "" {
			doc += `,"models":` + models
		}
		return doc + "}"
	}
	retired := func(t *testing.T) *Catalog {
		c, err := ShippedCatalog()
		if err != nil {
			t.Fatal(err)
		}
		delete(c.Models, "anthropic/claude-opus-4-6")
		c.Retired = append(c.Retired, Retired{Alias: "anthropic/claude-opus-4-6", Provider: AnthropicProvider, WireModels: []string{"claude-opus-4-6"}})
		return c
	}
	for _, tc := range []struct {
		name, models string
		cat          func(*testing.T) *Catalog
		seeds        int
	}{
		{"models missing", "", nil, 13},
		{"models null", "null", nil, 13},
		{"models empty", "[]", nil, 13},
		{"every entry broken", `["not an entry", 7, {"id": 5}]`, nil, 13},
		{"every id one craze cannot name", `[{"id": "bad id"}, {"id": "../x"}]`, nil, 13},
		{"every entry retired", `[{"id": "claude-opus-4-6"}]`, retired, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, AnthropicModelsFile)
			if err := os.WriteFile(path, []byte(envelope(tc.models)), 0o600); err != nil {
				t.Fatal(err)
			}
			cat, err := ShippedCatalog()
			if err != nil {
				t.Fatal(err)
			}
			if tc.cat != nil {
				cat = tc.cat(t)
			}
			tbl, err := LoadWith(dir, cat)
			if err != nil {
				t.Fatal(err)
			}
			wantWarnings(t, tbl, []string{path, "not an Anthropic model list"})
			if got := len(anthropicAliases(tbl)); got != tc.seeds {
				t.Fatalf("%d Anthropic models, want the %d seeds: %q", got, tc.seeds, anthropicAliases(tbl))
			}
		})
	}

	// The control: one entry that stands makes the envelope the list.
	dir := t.TempDir()
	entry, err := json.Marshal(opus6())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AnthropicModelsFile), []byte(envelope(`[{"id": "bad id"}, `+string(entry)+`]`)), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl := loadShipped(t, dir)
	wantWarnings(t, tbl, []string{"models[0]", "not one craze can name"})
	if got := anthropicAliases(tbl); !slices.Equal(got, []string{"anthropic/claude-opus-6"}) {
		t.Fatalf("the control: %q, want the list's one model alone", got)
	}
}

// TestAnthropicChoicesBlock (P28, §3.5): the picker lists Anthropic's
// anthropic/<id> models as one block placed by the provider's display name —
// "Anthropic", before every other provider's models today — each newest
// first, then by alias, a model whose release is not known last; a model of
// the user's under another alias on the provider sorts by its own name, as
// agent.OrderModels keeps the block by the prefix. The control: renamed past
// every other model, the block moves to the end, in the same order.
func TestAnthropicChoicesBlock(t *testing.T) {
	env := withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA, "FIREWORKS_API_KEY": "fw-key-not-a-secret-01"})
	const models = "version = 1\n\n" +
		"[models.\"anthropic/claude-unknown-1\"]\nprovider = \"anthropic\"\nwire_model = \"claude-unknown-1\"\n\n" +
		"[models.mine]\nprovider = \"anthropic\"\nwire_model = \"claude-opus-5-5\"\nname = \"A Mine\"\n"
	wantBlock := []string{
		"anthropic/claude-haiku-5-5", "anthropic/claude-sonnet-5-5", "anthropic/claude-opus-5-5", "anthropic/claude-fable-5-1",
		"anthropic/claude-opus-5", "anthropic/claude-sonnet-5", "anthropic/claude-fable-5", "anthropic/claude-opus-4-8",
		"anthropic/claude-opus-4-7", "anthropic/claude-sonnet-4-6", "anthropic/claude-opus-4-6",
		"anthropic/claude-opus-4-5-20251101", "anthropic/claude-haiku-4-5-20251001", "anthropic/claude-unknown-1",
	}
	for _, tc := range []struct {
		name, providers string
		last            bool
	}{
		{"Anthropic", "", false},
		{"renamed", "version = 1\n\n[providers.anthropic]\nname = \"Zz Claude\"\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := loadShipped(t, writeFiles(t, tc.providers, models))
			var got []string
			for _, c := range tbl.Choices(nil, env, "") {
				got = append(got, c.Alias)
			}
			if got[0] != "mine" {
				t.Fatalf("the user's own alias, A Mine, is not first by name: %q", got)
			}
			at := slices.Index(got, wantBlock[0])
			if at < 0 || at+len(wantBlock) > len(got) || !slices.Equal(got[at:at+len(wantBlock)], wantBlock) {
				t.Fatalf("Choices = %q\nwant the block %q in it", got, wantBlock)
			}
			if wantAt := map[bool]int{false: 1, true: len(got) - len(wantBlock)}[tc.last]; at != wantAt {
				t.Fatalf("the block starts at %d of %d, want %d: %q", at, len(got), wantAt, got)
			}
		})
	}
}

// TestAnthropicDefaultsRefused: [anthropic_defaults] and the catalog-only
// keys are checked as strictly as the rest of the catalog (§3.5), each
// failure naming the place; and no user file can set a catalog-only key.
func TestAnthropicDefaultsRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*Catalog)
		table, key string
	}{
		{"a start family that is no family", func(c *Catalog) { c.Anthropic.StartFamily = "Opus!" }, "anthropic_defaults", "start_family"},
		{"a start family no model has", func(c *Catalog) { c.Anthropic.StartFamily = "nope" }, "anthropic_defaults", "start_family"},
		{"an unknown effort", func(c *Catalog) { c.Anthropic.DefaultEffort = "ultra" }, "anthropic_defaults", "default_effort"},
		{"an unknown list", func(c *Catalog) { c.Anthropic.List = "some" }, "anthropic_defaults", "list"},
		{"defaults with no Anthropic provider", func(c *Catalog) {
			delete(c.Providers, AnthropicProvider)
			c.ProviderOrder = slices.DeleteFunc(c.ProviderOrder, func(id string) bool { return id == AnthropicProvider })
			for alias, m := range c.Models {
				if m.Provider == AnthropicProvider {
					delete(c.Models, alias)
				}
			}
		}, "anthropic_defaults", ""},
		{"a start key on the provider", func(c *Catalog) {
			c.Starts[AnthropicProvider] = []string{"anthropic/claude-opus-5-5"}
		}, "providers.anthropic", "start"},
		{"a family on another provider's model", func(c *Catalog) {
			setEntry(c.Models, "glm-5.3", func(m *Model) { m.Family = "glm" })
		}, `models."glm-5.3"`, "family"},
		{"a thinking on another provider's model", func(c *Catalog) {
			setEntry(c.Models, "glm-5.3", func(m *Model) { m.Thinking = ThinkingNone })
		}, `models."glm-5.3"`, "thinking"},
		{"a release that is no date", func(c *Catalog) {
			setEntry(c.Models, "anthropic/claude-opus-5-5", func(m *Model) { m.Released = "2026-09-21T16:24:00Z" })
		}, `models."anthropic/claude-opus-5-5"`, "released"},
		{"a family that is no family", func(c *Catalog) {
			setEntry(c.Models, "anthropic/claude-opus-5-5", func(m *Model) { m.Family = "Opus" })
		}, `models."anthropic/claude-opus-5-5"`, "family"},
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
	// The control: the shipped catalog, unchanged, passes.
	if c, err := ShippedCatalog(); err != nil || c.validate(CatalogFile) != nil {
		t.Fatalf("the shipped catalog: %v", err)
	}

	// No user file has the catalog-only keys.
	for _, key := range []string{"family = \"opus\"", "released = \"2026-01-01\"", "thinking = \"none\""} {
		dir := writeFiles(t, "", "version = 1\n\n[models.\"anthropic/claude-opus-5-5\"]\n"+key+"\n")
		_, err := Load(dir)
		fe := wantFileError(t, err, filepath.Join(dir, ModelsFile), `models."anthropic/claude-opus-5-5"`, strings.Fields(key)[0])
		if fe.Reason != "unknown key" {
			t.Fatalf("%s: %v, want an unknown key", key, err)
		}
	}
	// And [anthropic_defaults] takes its three keys and nothing else.
	extra := strings.Replace(anthropicCatalog, "default_effort = \"medium\"\n", "default_effort = \"medium\"\nfallback = true\n", 1)
	if extra == anthropicCatalog {
		t.Fatal("the test catalog has no default_effort to add a key beside")
	}
	_, err := parseCatalog(CatalogFile, []byte(extra))
	wantFileError(t, err, CatalogFile, "anthropic_defaults", "fallback")
}

// TestAnthropicKeyID: the list's binding names the key without being it: 12
// hex digits of its SHA-256, the same for the key with surrounding
// whitespace, another for another key.
func TestAnthropicKeyID(t *testing.T) {
	id := AnthropicKeyID(anthropicKeyA)
	if len(id) != 12 || strings.Trim(id, "0123456789abcdef") != "" || strings.Contains(anthropicKeyA, id) {
		t.Fatalf("id = %q", id)
	}
	if AnthropicKeyID(" "+anthropicKeyA+"\n") != id || AnthropicKeyID(anthropicKeyB) == id {
		t.Fatal("the id is not the trimmed key's alone")
	}
}
