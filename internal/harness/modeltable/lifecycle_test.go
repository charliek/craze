package modeltable

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Plan 050 C1: a model's deprecated state, a retired alias's replacement and
// the context default — the catalog's rules for them, the merge (stamping,
// clearing on a re-point, the warnings a reference to one gives), the offered
// set and where it applies, the "<provider id>/<alias>" form, Save, and the
// release rule's history files. The cases run over testCatalogV1 with a
// deprecated model added, through LoadWith: the shipped catalog has none.

// lifecycleCatalog is testCatalogV1 with acme/dep, a deprecated model replaced
// by acme/fast, a context default on acme/fast, and the retired acme/old
// pointing at acme/fast.
var lifecycleCatalog = strings.NewReplacer(
	"wire_models = [\"acme-old-1\", \"acme-old-2\"]\n",
	"wire_models = [\"acme-old-1\", \"acme-old-2\"]\nreplaced_by = \"acme/fast\"\n",
	"name = \"Acme Fast\"\ncontext_window = 100000\n",
	"name = \"Acme Fast\"\ncontext_window = 100000\ndefault_context = 50000\n",
).Replace(testCatalogV1) + `
[models."acme/dep"]
provider = "acme"
wire_model = "acme-dep-1"
name = "Acme Dep"
efforts = ["low", "high"]
default_effort = "high"
deprecated = true
replaced_by = "acme/fast"
`

// lifecycle is lifecycleCatalog decoded and validated.
func lifecycle(t *testing.T) *Catalog {
	t.Helper()
	if !strings.Contains(lifecycleCatalog, "replaced_by = \"acme/fast\"\n\n[providers") ||
		!strings.Contains(lifecycleCatalog, "default_context = 50000") {
		t.Fatal("premise: testCatalogV1 moved under the replacements")
	}
	return testCatalog(t, lifecycleCatalog)
}

// acmeEnv funds the test catalogs' acme provider, and router too.
var acmeEnv = fakeEnv(map[string]string{"ACME_API_KEY": "sk-acme-dummy-0001", "ROUTER_API_KEY": "sk-router-dummy-0001"})

// TestCatalogLifecycleRules: what the catalog may say of a deprecated model,
// a retired alias's replacement and a context default — and that the
// lifecycle catalog itself, the control, passes.
func TestCatalogLifecycleRules(t *testing.T) {
	setM := func(alias string, f func(*Model)) func(*Catalog) {
		return func(c *Catalog) { setEntry(c.Models, alias, f) }
	}
	for _, tc := range []struct {
		name       string
		mutate     func(*Catalog)
		table, key string
	}{
		{"a deprecated model names no replacement", setM("acme/dep", func(m *Model) { m.ReplacedBy = "" }), `models."acme/dep"`, "replaced_by"},
		{"replaced_by on an active model", setM("acme/big", func(m *Model) { m.ReplacedBy = "acme/fast" }), `models."acme/big"`, "replaced_by"},
		{"a replacement that is no model", setM("acme/dep", func(m *Model) { m.ReplacedBy = "acme/gone" }), `models."acme/dep"`, "replaced_by"},
		{"a replacement that is deprecated", setM("acme/dep", func(m *Model) { m.ReplacedBy = "acme/dep" }), `models."acme/dep"`, "replaced_by"},
		{"a context default at the window", setM("acme/fast", func(m *Model) { m.DefaultContext = 100000 }), `models."acme/fast"`, "default_context"},
		{"a context default and no window", setM("acme/big", func(m *Model) { m.DefaultContext = 1000 }), `models."acme/big"`, "default_context"},
		{"a negative context default", setM("acme/fast", func(m *Model) { m.DefaultContext = -1 }), `models."acme/fast"`, "default_context"},
		{"a deprecated start", func(c *Catalog) { c.Starts["acme"] = []string{"acme/dep"} }, "providers.acme", "start"},
		{"a retired row's replacement is no model", func(c *Catalog) { c.Retired[0].ReplacedBy = "acme/gone" }, "retired", "replaced_by"},
		{"a retired row's replacement is deprecated", func(c *Catalog) { c.Retired[0].ReplacedBy = "acme/dep" }, "retired", "replaced_by"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := lifecycle(t).Clone()
			tc.mutate(c)
			wantFileError(t, c.validate(CatalogFile), CatalogFile, tc.table, tc.key)
		})
	}
	c := lifecycle(t)
	if m := c.Models["acme/dep"]; !m.Deprecated || m.ReplacedBy != "acme/fast" || c.Models["acme/fast"].DefaultContext != 50000 {
		t.Fatalf("the catalog decodes acme/dep as %+v", m)
	}
	if c.Retired[0].ReplacedBy != "acme/fast" || c.Clone().Retired[0].ReplacedBy != "acme/fast" {
		t.Fatalf("the retired row = %+v", c.Retired[0])
	}

	// A default_context the catalog writes is above 0: `default_context = 0`
	// is the user file's way to turn a shipped one off, never the catalog's.
	// It is refused as written, which the decoded value alone (0, as for a
	// model that leaves the key out) cannot tell — a copy keeps the mark.
	zero := strings.Replace(lifecycleCatalog, "default_context = 50000", "default_context = 0", 1)
	if zero == lifecycleCatalog {
		t.Fatal("premise: the catalog has no default_context to zero")
	}
	zc, err := parseCatalog(CatalogFile, []byte(zero))
	if err != nil {
		t.Fatal(err)
	}
	wantFileError(t, zc.validate(CatalogFile), CatalogFile, `models."acme/fast"`, "default_context")
	wantFileError(t, zc.Clone().validate(CatalogFile), CatalogFile, `models."acme/fast"`, "default_context")
}

// TestUserFilesCannotDeprecate: deprecated and replaced_by are the catalog's
// alone, so a user file writing either is an unknown key; default_context is
// a key of models.toml, never negative.
func TestUserFilesCannotDeprecate(t *testing.T) {
	for _, key := range []string{"deprecated = true", "replaced_by = \"acme/fast\""} {
		dir := writeFiles(t, "", "version = 1\n\n[models.\"acme/fast\"]\n"+key+"\n")
		_, err := LoadWith(dir, lifecycle(t))
		if fe := wantFileError(t, err, filepath.Join(dir, ModelsFile), `models."acme/fast"`, strings.Fields(key)[0]); fe.Reason != "unknown key" {
			t.Fatalf("%s: %v, want an unknown key", key, err)
		}
	}
	dir := writeFiles(t, "", "version = 1\n\n[models.\"acme/fast\"]\ndefault_context = -1\n")
	_, err := LoadWith(dir, lifecycle(t))
	wantFileError(t, err, filepath.Join(dir, ModelsFile), `models."acme/fast"`, "default_context")
}

// TestMergeKeepsDeprecationOnTheShippedAlias: deprecated and replaced_by are
// stamped on the shipped alias and resolve with it; an override that keeps
// the alias's model keeps them, and warns; one that re-points the alias at
// another provider or wire model clears them, without a word; and a model of
// the user's own is never deprecated, even on the deprecated model's
// identity.
func TestMergeKeepsDeprecationOnTheShippedAlias(t *testing.T) {
	tbl, _ := loadOver(t, lifecycle(t), "", "")
	r, err := tbl.Resolve("acme/dep", acmeEnv)
	if err != nil || !r.Deprecated || r.ReplacedBy != "acme/fast" {
		t.Fatalf("Resolve(acme/dep) = %+v, %v; want it to run, deprecated, replaced by acme/fast", r, err)
	}
	wantWarnings(t, tbl)

	renamed, _ := loadOver(t, lifecycle(t), "", "version = 1\n\n[models.\"acme/dep\"]\nname = \"Mine\"\n")
	if m := renamed.Models["acme/dep"]; !m.Deprecated || m.ReplacedBy != "acme/fast" || m.Name != "Mine" {
		t.Fatalf("a renamed acme/dep = %+v; want it still deprecated", m)
	}
	wantWarnings(t, renamed, []string{`[models."acme/dep"]`, `"acme/dep" is deprecated; use "acme/fast"`, "retires it"})

	for _, tc := range []struct{ name, alias, entry string }{
		{"another wire model", "acme/dep", "wire_model = \"acme-dep-2\"\n"},
		{"another provider", "acme/dep", "provider = \"router\"\nwire_model = \"vendor/dep\"\n"},
		{"a model of the user's own on its identity", "mine", "provider = \"acme\"\nwire_model = \"acme-dep-1\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _ := loadOver(t, lifecycle(t), "", "version = 1\n\n[models.\""+tc.alias+"\"]\n"+tc.entry)
			if m := tbl.Models[tc.alias]; m.Deprecated || m.ReplacedBy != "" {
				t.Fatalf("%s = %+v; want it not deprecated", tc.alias, m)
			}
			wantWarnings(t, tbl)
		})
	}
}

// TestReferencesToDeprecatedAndRetiredAliasesWarn: every reference the user's
// models.toml makes to a deprecated model — an override, the pin, the
// [subagents] model and a tier — stands, with one warning each naming the
// replacement; every reference to a retired alias whose row names a
// replacement is dropped with one, where one with no replacement is still
// dropped silently.
func TestReferencesToDeprecatedAndRetiredAliasesWarn(t *testing.T) {
	refs := func(alias string) string {
		return "version = 1\ndefault_model = \"" + alias + "\"\n\n[subagents]\nmodel = \"" + alias +
			"\"\n\n[subagents.tiers]\nsonnet = \"" + alias + "\"\n\n[models.\"" + alias + "\"]\nname = \"Mine\"\n"
	}
	dep, _ := loadOver(t, lifecycle(t), "", refs("acme/dep"))
	use := `"acme/dep" is deprecated; use "acme/fast"`
	wantWarnings(t, dep,
		[]string{`[models."acme/dep"]`, use},
		[]string{"default_model", use},
		[]string{"[subagents] model", use},
		[]string{"[subagents.tiers] sonnet", use})
	if dep.DefaultModel != "acme/dep" || dep.Subagents.Model != "acme/dep" || dep.Subagents.Tiers["sonnet"] != "acme/dep" {
		t.Fatalf("the references to a deprecated model = %q, %+v; want each to stand", dep.DefaultModel, dep.Subagents)
	}

	ret, _ := loadOver(t, lifecycle(t), "", refs("acme/old"))
	was := `"acme/old" was retired; use "acme/fast"`
	wantWarnings(t, ret,
		[]string{`[models."acme/old"]`, was, "the entry is ignored"},
		[]string{"default_model", was, "provider order"},
		[]string{"[subagents] model", was, "parent's model"},
		[]string{"[subagents.tiers] sonnet", was, "parent's model"})
	if ret.DefaultModel != "" || ret.Subagents.Model != "" || ret.Subagents.Tiers != nil || hasModel(ret.Models, "acme/old") {
		t.Fatalf("the references to a retired alias = %q, %+v; want each dropped", ret.DefaultModel, ret.Subagents)
	}

	silent, _ := loadOver(t, testCatalog(t, testCatalogV1), "", refs("acme/old"))
	wantWarnings(t, silent)

	// A complete entry of the user's on a dead wire id under the retired
	// alias is dropped too, and says so; on a live one it is the user's own
	// model, as it always was.
	dead, _ := loadOver(t, lifecycle(t), "", "version = 1\n\n[models.\"acme/old\"]\nprovider = \"acme\"\nwire_model = \"acme-old-1\"\n")
	wantWarnings(t, dead, []string{`[models."acme/old"]`, was})
	live, _ := loadOver(t, lifecycle(t), "", "version = 1\n\n[models.\"acme/old\"]\nprovider = \"acme\"\nwire_model = \"acme-old-3\"\n")
	if m := live.Models["acme/old"]; m.Deprecated || live.ModelOrigin("acme/old") != OriginYours {
		t.Fatalf("a live entry under a retired alias = %+v, %s; want the user's own model", m, live.ModelOrigin("acme/old"))
	}
	wantWarnings(t, live)
}

// TestContextDefaultMerges: default_context merges as context_window does —
// a shipped value stands, a user's value replaces it, 0 turns it off — and a
// value at or above the merged window is the whole window, never an error,
// so a user may lower a shipped window under a shipped default.
func TestContextDefaultMerges(t *testing.T) {
	for _, tc := range []struct {
		name         string
		models       string
		def, inForce int
		window       int
	}{
		{"shipped", "", 50000, 50000, 100000},
		{"overridden", "default_context = 80000\n", 80000, 80000, 100000},
		{"turned off", "default_context = 0\n", 0, 100000, 100000},
		{"above the window", "default_context = 200000\n", 200000, 100000, 100000},
		{"under a lowered window", "context_window = 40000\n", 50000, 40000, 40000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := ""
			if tc.models != "" {
				models = "version = 1\n\n[models.\"acme/fast\"]\n" + tc.models
			}
			tbl, _ := loadOver(t, lifecycle(t), "", models)
			r, err := tbl.Resolve("acme/fast", acmeEnv)
			if err != nil || r.DefaultContext != tc.def || r.ContextInForce() != tc.inForce || r.ContextWindow != tc.window {
				t.Fatalf("Resolve = default %d, in force %d, window %d, %v; want %d, %d, %d",
					r.DefaultContext, r.ContextInForce(), r.ContextWindow, err, tc.def, tc.inForce, tc.window)
			}
			if err := tbl.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
	// A user's own model may set one too.
	tbl, _ := loadOver(t, lifecycle(t), "", "version = 1\n\n[models.\"mine\"]\nprovider = \"acme\"\nwire_model = \"m\"\ncontext_window = 1000\ndefault_context = 500\n")
	if r, err := tbl.Resolve("mine", acmeEnv); err != nil || r.ContextInForce() != 500 {
		t.Fatalf("a user's own model's context = %d, %v; want 500", r.ContextInForce(), err)
	}
}

// withShipped replaces the shipped catalog with change applied to a copy of
// it, for the rest of t: Save marks an explicit empty against the shipped
// catalog alone, and C1 ships no model with a context default. No test of
// the package runs in parallel.
func withShipped(t *testing.T, change func(*Catalog)) {
	t.Helper()
	orig, err := shippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	c := orig.Clone()
	change(c)
	shipped.cat = c
	t.Cleanup(func() { shipped.cat = orig })
}

// TestSaveKeepsAContextDefaultTurnedOff: `default_context = 0` over a shipped
// value survives a Save and a reload, as `efforts = []` does, and is the one
// `default_context = 0` the file writes (a shipped value saves as itself).
func TestSaveKeepsAContextDefaultTurnedOff(t *testing.T) {
	withShipped(t, func(c *Catalog) {
		setEntry(c.Models, "glm-5.3", func(m *Model) { m.DefaultContext = m.ContextWindow / 2 })
	})
	cat, _ := shippedCatalog()
	if cat.Models["glm-5.3"].DefaultContext <= 0 {
		t.Fatal("premise: glm-5.3 has no context window to halve")
	}
	first, err := Load(writeFiles(t, "", "version = 1\n\n[models.\"glm-5.3\"]\ndefault_context = 0\n"))
	if err != nil || first.Models["glm-5.3"].DefaultContext != 0 {
		t.Fatalf("the override loads as %d, %v", first.Models["glm-5.3"].DefaultContext, err)
	}
	out := filepath.Join(t.TempDir(), "native")
	if err := Save(out, first); err != nil {
		t.Fatal(err)
	}
	second, err := Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Models["glm-5.3"].DefaultContext; got != 0 {
		t.Fatalf("default_context after a round trip = %d, want 0", got)
	}
	b, err := os.ReadFile(filepath.Join(out, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "default_context = 0\n"); n != 1 {
		t.Fatalf("the saved models.toml writes default_context = 0 %d times, want once (the override):\n%s", n, b)
	}
}

// TestOfferedLeavesOutDeprecated: the offered set is what resolves and is
// not deprecated; the picker lists it, plus the current model even when it
// is deprecated; Relist keeps a list's members whatever their state.
func TestOfferedLeavesOutDeprecated(t *testing.T) {
	tbl, _ := loadOver(t, lifecycle(t), "", "")
	offered := tbl.Offered(acmeEnv)
	if offered["acme/dep"] || !offered["acme/fast"] || !offered["acme/big"] || !offered["router/m"] || len(offered) != 3 {
		t.Fatalf("Offered = %v", offered)
	}
	if got := tbl.Offered(fakeEnv(nil)); len(got) != 0 {
		t.Fatalf("Offered with no key = %v, want none", got)
	}
	if got := choiceAliases(tbl.Choices(nil, acmeEnv, "")); slices.Contains(got, "acme/dep") || len(got) != 3 {
		t.Fatalf("Choices = %q, want no deprecated model", got)
	}
	if got := choiceAliases(tbl.Choices(nil, acmeEnv, "acme/dep")); !slices.Contains(got, "acme/dep") || len(got) != 4 {
		t.Fatalf("Choices of a session on acme/dep = %q, want its own model listed", got)
	}
	if got := choiceAliases(tbl.Relist([]string{"acme/dep", "acme/fast"}, nil, nil)); len(got) != 2 || !slices.Contains(got, "acme/dep") {
		t.Fatalf("Relist = %q, want both members kept", got)
	}
}

// TestStartPickPassesOverDeprecated: craze never chooses a deprecated model
// for the owner while it has another — a start (step 3) or the sorted
// fallback (step 4) — but a remembered or pinned one still starts, and a
// machine whose only funded models are deprecated starts on the first of
// them, with Fallback set, rather than report nothing funded.
func TestStartPickPassesOverDeprecated(t *testing.T) {
	table := func() *Table {
		tbl := recentTable()
		tbl.DefaultModel = ""
		setModel(tbl, "p/default", func(m *Model) { m.Deprecated, m.ReplacedBy = true, "p/effort" })
		setModel(tbl, "q/plain", func(m *Model) { m.Deprecated, m.ReplacedBy = true, "q/twin-a" })
		return tbl
	}
	for _, tc := range []struct {
		name     string
		set      func(*Table)
		recent   []RecentEntry
		env      func(string) string
		want     string
		fallback bool
	}{
		{"a deprecated start is passed over", func(tbl *Table) {
			tbl.catalogOrder, tbl.starts = []string{"p"}, map[string][]string{"p": {"p/default", "p/effort"}}
		}, nil, fundedEnv("p"), "p/effort", false},
		{"the sorted fallback passes over a deprecated model", nil, nil, fundedEnv("p"), "p/effort", true},
		{"the sorted fallback takes an active model before a deprecated one", nil, nil, fundedEnv("q"), "q/twin-a", true},
		{"only deprecated models funded: the last resort", func(tbl *Table) {
			delete(tbl.Models, "q/twin-a")
			delete(tbl.Models, "q/twin-b")
		}, nil, fundedEnv("q"), "q/plain", true},
		{"a remembered deprecated model starts", nil, []RecentEntry{entry(t, "p/default", "high")}, fundedEnv("p"), "p/default", false},
		{"a pinned deprecated model starts", func(tbl *Table) { tbl.DefaultModel = "p/default" }, nil, fundedEnv("p"), "p/default", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := table()
			if tc.set != nil {
				tc.set(tbl)
			}
			got, err := tbl.StartPick(tc.recent, tc.env)
			if err != nil || got.Alias != tc.want || got.Fallback != tc.fallback {
				t.Fatalf("StartPick = %+v, %v; want %s, fallback %v", got, err, tc.want, tc.fallback)
			}
		})
	}
	if _, err := table().StartPick(nil, fundedEnv()); err == nil {
		t.Fatal("StartPick with nothing funded found a model")
	}
}

// TestQualifiedAlias: "<provider id>/<alias>" names the alias on that
// provider, under the normalisation the caller passes, and nothing else.
func TestQualifiedAlias(t *testing.T) {
	tbl := &Table{
		Providers: map[string]Provider{"zai-coding-plan": {}, "meta": {}, "fireworks": {}},
		Models: map[string]Model{
			"glm-5.3":           {Provider: "zai-coding-plan"},
			"muse-spark-1.3":    {Provider: "meta"},
			"fireworks/kimi-k3": {Provider: "fireworks"},
		},
	}
	for raw, want := range map[string]string{
		"zai-coding-plan/glm-5.3":     "glm-5.3",
		"meta/muse-spark-1.3":         "muse-spark-1.3",
		"fireworks/fireworks/kimi-k3": "fireworks/kimi-k3",
		"meta/glm-5.3":                "",
		"nope/glm-5.3":                "",
		"glm-5.3":                     "",
		"/glm-5.3":                    "",
		"meta/":                       "",
		"Meta/MUSE-spark-1.3":         "",
	} {
		got, ok := tbl.QualifiedAlias(raw, nil)
		if got != want || ok != (want != "") {
			t.Errorf("QualifiedAlias(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	if got, ok := tbl.QualifiedAlias("Meta/MUSE-spark-1.3", strings.ToLower); !ok || got != "muse-spark-1.3" {
		t.Errorf("QualifiedAlias under a normalisation = %q, %v", got, ok)
	}
}

// TestTableRetired: a merged table knows its catalog's retired rows, the
// replacement with them; a table with no catalog retires nothing.
func TestTableRetired(t *testing.T) {
	tbl, _ := loadOver(t, lifecycle(t), "", "")
	r, ok := tbl.Retired("acme/old")
	if !ok || r.ReplacedBy != "acme/fast" || r.Provider != "acme" || len(r.WireModels) != 2 {
		t.Fatalf("Retired(acme/old) = %+v, %v", r, ok)
	}
	r.WireModels[0] = "changed"
	if again, _ := tbl.Retired("acme/old"); again.WireModels[0] != "acme-old-1" {
		t.Fatal("a change to the returned row reached the table")
	}
	if _, ok := tbl.Retired("acme/fast"); ok {
		t.Fatal("a model is retired")
	}
	if _, ok := validTable().Retired("acme/old"); ok {
		t.Fatal("a table with no catalog retires something")
	}
}

// historyLines reads a testdata history file: its lines, blank lines and #
// comments skipped.
func historyLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
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

// releasePattern is the versions scripts/release/update-version.sh accepts,
// and so the ones internal/version.Version can hold: MAJOR.MINOR.PATCH and an
// optional prerelease after a hyphen (0.3.0-rc.1).
var releasePattern = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(?:-([a-zA-Z0-9.-]+))?$`)

// release is a version releasePattern accepts: its three numbers and its
// prerelease's dot-separated identifiers (none for a release).
type release struct {
	core, pre []string
}

// releaseVersion is v as a release, false when the release script would
// refuse it.
func releaseVersion(v string) (release, bool) {
	m := releasePattern.FindStringSubmatch(v)
	if m == nil {
		return release{}, false
	}
	r := release{core: strings.Split(m[1], ".")}
	if m[2] != "" {
		r.pre = strings.Split(m[2], ".")
	}
	return r, true
}

// compareRelease orders two releases as semantic versioning does: by their
// numbers, then a prerelease before its release (0.3.0-rc.1 < 0.3.0), then
// the prereleases identifier by identifier — numeric ones as numbers and
// before any other, the rest as text — and a shorter run of equal
// identifiers first.
func compareRelease(a, b release) int {
	for i := range a.core {
		if c := compareNumeric(a.core[i], b.core[i]); c != 0 {
			return c
		}
	}
	if len(a.pre) == 0 || len(b.pre) == 0 {
		return cmp.Compare(len(b.pre), len(a.pre)) // none (a release) is the greater
	}
	for i := range min(len(a.pre), len(b.pre)) {
		x, y := a.pre[i], b.pre[i]
		xn, yn := isNumeric(x), isNumeric(y)
		var c int
		switch {
		case xn && yn:
			c = compareNumeric(x, y)
		case xn || yn:
			c = cmp.Compare(boolRank(yn), boolRank(xn)) // a numeric identifier is the lesser
		default:
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.pre), len(b.pre))
}

// isNumeric reports whether s is all digits (and not empty).
func isNumeric(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// compareNumeric compares two runs of digits as numbers, however long.
func compareNumeric(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// boolRank is 1 for true, 0 for false.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkDeprecationHistory is the release rule's mechanical half (plan 050 C1;
// RELEASING.md): every deprecated model of c has a line in deprecated, and
// every [[retired]] row not in grandfathered has one whose version is below
// current — internal/version.Version, the version the last release carried — so the deprecation
// shipped in a release before this one retires it. It returns each break.
func checkDeprecationHistory(c *Catalog, deprecated, grandfathered []string, current string) []string {
	var bad []string
	cur, ok := releaseVersion(current)
	if !ok {
		return []string{"internal/version.Version " + strconv.Quote(current) + " is not a release version"}
	}
	since := map[string]release{}
	for _, ln := range deprecated {
		alias, v, found := strings.Cut(ln, " ")
		parts, ok := releaseVersion(strings.TrimSpace(v))
		if !found || !ok || alias == "" {
			bad = append(bad, "deprecated-aliases.txt line "+strconv.Quote(ln)+" is not \"<alias> <version>\"")
			continue
		}
		since[alias] = parts
	}
	for _, alias := range slices.Sorted(maps.Keys(c.Models)) {
		if _, ok := since[alias]; c.Models[alias].Deprecated && !ok {
			bad = append(bad, alias+" is deprecated but has no line in testdata/deprecated-aliases.txt")
		}
	}
	for _, r := range c.Retired {
		if slices.Contains(grandfathered, r.Alias) {
			continue
		}
		v, ok := since[r.Alias]
		switch {
		case !ok:
			bad = append(bad, r.Alias+" is retired but no release shipped it deprecated: it has no line in testdata/deprecated-aliases.txt")
		case compareRelease(v, cur) >= 0:
			bad = append(bad, r.Alias+" is retired in the release that deprecated it: deprecate it in one release, retire it in a later one")
		}
	}
	return bad
}

// releasedVersion is internal/version.Version's literal, the last release,
// read from its source: the harness may import nothing of craze's but
// atomicfile and errkind (depguard, D-02), and a build's ldflags would
// override the compiled-in value anyway.
func releasedVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "version", "version.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^var Version = "([^"]*)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("internal/version/version.go has no `var Version = \"…\"` line")
	}
	return string(m[1])
}

// TestDeprecationHistory holds the shipped catalog to the release rule: a
// deprecation and its retirement never ship in the same release. It reads
// only checked-in files, so it runs in a `git archive` export with no .git
// (scripts/verify/gate.sh) and on a shallow clone.
func TestDeprecationHistory(t *testing.T) {
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	deprecated, grandfathered := historyLines(t, "deprecated-aliases.txt"), historyLines(t, "retired-grandfathered.txt")
	for _, b := range checkDeprecationHistory(c, deprecated, grandfathered, releasedVersion(t)) {
		t.Error(b)
	}
	// The grandfathered rows are rows of the catalog, and no row is added.
	for _, alias := range grandfathered {
		if !slices.ContainsFunc(c.Retired, func(r Retired) bool { return r.Alias == alias }) {
			t.Errorf("retired-grandfathered.txt names %s, which is no [[retired]] row", alias)
		}
	}
	if len(grandfathered) != 14 {
		t.Errorf("retired-grandfathered.txt names %d rows; it is the 14 of commit 26181d1, and never grows", len(grandfathered))
	}

	// Negative controls, over the lifecycle catalog.
	lc := lifecycle(t)
	lc.Retired = append(lc.Retired, Retired{Alias: "acme/older", Provider: "acme"})
	gf := []string{"acme/old"}
	for name, tc := range map[string]struct {
		history []string
		want    string
	}{
		"a deprecated model with no line": {[]string{"acme/older 0.1.0"}, "acme/dep is deprecated"},
		"a retired row with no line":      {[]string{"acme/dep 0.2.0"}, "acme/older is retired but no release"},
		"retired in the same release":     {[]string{"acme/dep 0.2.0", "acme/older 0.2.0"}, "acme/older is retired in the release"},
		"a malformed line":                {[]string{"acme/dep 0.2.0", "acme/older 0.1.0", "acme/x"}, "is not \"<alias> <version>\""},
	} {
		bad := checkDeprecationHistory(lc, tc.history, gf, "0.2.0")
		if len(bad) != 1 || !strings.Contains(bad[0], tc.want) {
			t.Errorf("%s reports %q; want one break containing %q", name, bad, tc.want)
		}
	}
	if bad := checkDeprecationHistory(lc, []string{"acme/dep 0.2.0", "acme/older 0.1.9"}, gf, "0.2.0"); len(bad) != 0 {
		t.Errorf("a retirement a release after its deprecation reports %q", bad)
	}
	if bad := checkDeprecationHistory(lc, []string{"acme/dep 0.2.0", "acme/older 0.2.0"}, gf, "0.10.0"); len(bad) != 0 {
		t.Errorf("versions compare as numbers, not text: %q", bad)
	}

	// Prerelease versions, which scripts/release/update-version.sh accepts
	// (0.3.0-rc.1): one orders before its release, and its identifiers
	// compare as numbers when numeric. The shipped catalog with an empty
	// ledger passes on a prerelease's release commit.
	if bad := checkDeprecationHistory(c, deprecated, grandfathered, "0.3.0-rc.1"); len(bad) != 0 {
		t.Errorf("the shipped catalog at a prerelease version reports %q", bad)
	}
	for _, tc := range []struct {
		since, current string
		ok             bool
	}{
		{"0.3.0-rc.1", "0.3.0", true},
		{"0.3.0-rc.1", "0.3.0-rc.2", true},
		{"0.3.0-rc.2", "0.3.0-rc.10", true},
		{"0.3.0-alpha", "0.3.0-beta", true},
		{"0.3.0-rc.1", "0.3.0-rc.1.1", true},
		{"0.2.0", "0.3.0-rc.1", true},
		{"0.3.0-rc.1", "0.3.0-rc.1", false},
		{"0.3.0", "0.3.0-rc.1", false},
		{"0.3.0-rc.10", "0.3.0-rc.2", false},
		{"0.3.0-beta", "0.3.0-alpha", false},
	} {
		bad := checkDeprecationHistory(lc, []string{"acme/dep 0.1.0", "acme/older " + tc.since}, gf, tc.current)
		if (len(bad) == 0) != tc.ok {
			t.Errorf("deprecated in %s, retired at %s, reports %q; want ok %v", tc.since, tc.current, bad, tc.ok)
		}
	}
	for _, v := range []string{"0.3", "v0.3.0", "0.3.0-", "0.3.0+build", "dev"} {
		if bad := checkDeprecationHistory(lc, []string{"acme/dep 0.1.0", "acme/older 0.1.0"}, gf, v); len(bad) != 1 {
			t.Errorf("the current version %q reports %q; want it refused", v, bad)
		}
	}
}
