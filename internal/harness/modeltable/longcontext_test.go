package modeltable

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The long-context price tier (plan 047 D10): long_context_above and
// [models."<alias>".cost.long_context], merged field by field, turned off by
// long_context_above = 0, validated on the merged model, cloned deep, part of
// the same-as-shipped check, the conflict warning and the rates an identity
// is priced at.

// tieredCatalog is testCatalogV1 with a tier on acme/fast: above 50,000
// prompt tokens, input 4, output 8, cache read 0.4 and cache write 5.
const tieredCatalog = testCatalogV1 + `
[models."acme/fast".cost.long_context]
input = 4.0
output = 8.0
cache_read = 0.4
cache_write = 5.0
`

// tiered is tieredCatalog decoded and validated, acme/fast's [cost] table
// given its threshold, 50,000 tokens.
func tiered(t *testing.T) *Catalog {
	t.Helper()
	doc := strings.Replace(tieredCatalog, "[models.\"acme/fast\".cost]\ninput = 1.0\noutput = 2.0\n",
		"[models.\"acme/fast\".cost]\ninput = 1.0\noutput = 2.0\nlong_context_above = 50000\n", 1)
	if doc == tieredCatalog {
		t.Fatal("premise: the catalog's acme/fast cost table moved")
	}
	return testCatalog(t, doc)
}

// TestLongContextRates: a tier's threshold and rates are part of the rates an
// identity is priced at, rounded to picodollars as the base rates are; a
// prompt of exactly the threshold is base and one token more is long; and a
// cost with no tier — or a threshold of 0 — prices every prompt at base, its
// Long rates being the base ones.
func TestLongContextRates(t *testing.T) {
	tbl, _ := loadOver(t, tiered(t), "", "")
	r, ok := tbl.Price("acme", "acme-fast-1")
	want := Rates{Input: 1_000_000, Output: 2_000_000, LongContextAbove: 50_000,
		LongContext: TierRates{Input: 4_000_000, Output: 8_000_000, CacheRead: 400_000, CacheWrite: 5_000_000}}
	if !ok || r != want {
		t.Fatalf("Price = %+v, %v; want %+v", r, ok, want)
	}
	if r.LongPrompt(50_000) || !r.LongPrompt(50_001) {
		t.Fatalf("LongPrompt(50,000) = %v, LongPrompt(50,001) = %v; want base at the threshold, long one above", r.LongPrompt(50_000), r.LongPrompt(50_001))
	}
	if r.Long() != want.LongContext || r.Base() != (TierRates{Input: 1_000_000, Output: 2_000_000}) {
		t.Fatalf("Long = %+v, Base = %+v", r.Long(), r.Base())
	}
	none := Rates{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	if none.LongPrompt(1 << 40) {
		t.Fatal("a price with no tier priced a prompt as long")
	}
	if none.Long() != none.Base() {
		t.Fatalf("with no tier Long = %+v, want the base rates %+v", none.Long(), none.Base())
	}
	// A threshold without a table (which Validate refuses) is no tier.
	if r := ratesFromCost(&Cost{Input: ptr(1.0), LongContextAbove: ptr(int64(10))}); r.LongContextAbove != 0 || r.LongPrompt(11) {
		t.Fatalf("a threshold with no table rates %+v; want no tier", r)
	}
}

// TestCostCloneCopiesTheTier: Cost.Clone copies the long-context table and the
// threshold, so a change through the clone's pointers never reaches the
// original. Negative control: a Clone that shares LongContext (or the
// threshold) fails here.
func TestCostCloneCopiesTheTier(t *testing.T) {
	c := &Cost{Input: ptr(1.0), Output: ptr(2.0), LongContextAbove: ptr(int64(100_000)),
		LongContext: &LongContextCost{Input: ptr(5.0), Output: ptr(10.0), CacheRead: ptr(0.5), CacheWrite: ptr(6.25)}}
	d := c.Clone()
	if !reflect.DeepEqual(c, d) {
		t.Fatalf("Clone = %+v, want %+v", d, c)
	}
	*d.LongContextAbove = 1
	*d.LongContext.Input, *d.LongContext.Output, *d.LongContext.CacheRead, *d.LongContext.CacheWrite = 9, 9, 9, 9
	d.LongContext.Input = nil
	if *c.LongContextAbove != 100_000 || c.LongContext.Input == nil || *c.LongContext.Input != 5 ||
		*c.LongContext.Output != 10 || *c.LongContext.CacheRead != 0.5 || *c.LongContext.CacheWrite != 6.25 {
		t.Fatalf("a change to the clone reached the original: %+v, long %+v", c, c.LongContext)
	}
	if (*Cost)(nil).Clone() != nil || (*LongContextCost)(nil).Clone() != nil {
		t.Fatal("a nil cost cloned to something")
	}
}

// TestLongContextRoundTrip: a tier saves as long_context_above in the model's
// [cost] table and its rates in their own [models."<alias>".cost.long_context]
// table — never an unqualified [long_context] — loads back the same, saves the
// same bytes twice, and leaves the table it saved from untouched. A cost with
// no tier writes neither key (TestCostKeysRoundTrip).
func TestLongContextRoundTrip(t *testing.T) {
	tbl := validTable()
	setModel(tbl, "fireworks/kimi-k3", func(m *Model) {
		m.Cost = &Cost{Input: ptr(0.6), Output: ptr(2.5), LongContextAbove: ptr(int64(128_000)),
			LongContext: &LongContextCost{Input: ptr(1.2), Output: ptr(5.0)}}
	})
	before := tbl.Models["fireworks/kimi-k3"].Cost.Clone()
	dir := t.TempDir()
	if err := Save(dir, tbl); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tbl.Models["fireworks/kimi-k3"].Cost, before) {
		t.Fatal("Save changed the table's cost")
	}
	mb, err := os.ReadFile(filepath.Join(dir, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[models.\"fireworks/kimi-k3\".cost]\ninput = 0.6\noutput = 2.5\nlong_context_above = 128000\n",
		"[models.\"fireworks/kimi-k3\".cost.long_context]\ninput = 1.2\noutput = 5.0\n",
	} {
		if !strings.Contains(string(mb), want) {
			t.Errorf("%s does not hold %q:\n%s", ModelsFile, want, mb)
		}
	}
	if strings.Contains(string(mb), "\n[long_context]") || strings.Contains(string(mb), "\n[cost") {
		t.Fatalf("%s holds an unqualified table:\n%s", ModelsFile, mb)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, tbl) {
		t.Fatalf("round trip = %+v, want %+v", got.Models["fireworks/kimi-k3"].Cost, tbl.Models["fireworks/kimi-k3"].Cost)
	}
	dir2 := t.TempDir()
	if err := Save(dir2, got); err != nil {
		t.Fatal(err)
	}
	if mb2, err := os.ReadFile(filepath.Join(dir2, ModelsFile)); err != nil || string(mb2) != string(mb) {
		t.Fatalf("a second save wrote other bytes (%v):\n%s\nwant\n%s", err, mb2, mb)
	}
}

// TestLongContextMerge: the tier's keys merge field by field over a shipped
// tier — a long-context rate the user writes replaces that rate alone, a
// threshold the user writes replaces the threshold alone — a tier the user
// adds to a shipped model with none stands, and long_context_above = 0 turns
// a shipped tier off, its table with it: the model then prices every prompt
// at base. None of them warns. Negative controls: a merge that ignores the
// long-context table keeps 4.0 as the input rate; one that keeps the shipped
// table under a threshold of 0 fails validation, the entry warned away.
func TestLongContextMerge(t *testing.T) {
	cat := tiered(t)

	t.Run("a partial override", func(t *testing.T) {
		tbl, _ := loadOver(t, cat, "", `version = 1

[models."acme/fast".cost.long_context]
input = 6.0

[models."acme/big".cost]
input = 1.0
output = 1.0
long_context_above = 1000

[models."acme/big".cost.long_context]
input = 2.0
output = 2.0
`)
		wantWarnings(t, tbl)
		c := tbl.Models["acme/fast"].Cost
		if c == nil || c.LongContext == nil || *c.LongContext.Input != 6 || *c.LongContext.Output != 8 ||
			*c.LongContext.CacheRead != 0.4 || *c.LongContext.CacheWrite != 5 || *c.LongContextAbove != 50_000 ||
			*c.Input != 1 || *c.Output != 2 {
			t.Fatalf("acme/fast cost = %+v, long %+v: want input 6 over the shipped tier, the rest inherited", c, c.LongContext)
		}
		r, _ := tbl.Price("acme", "acme-fast-1")
		if r.LongContextAbove != 50_000 || r.LongContext.Input != 6_000_000 || r.LongContext.Output != 8_000_000 {
			t.Fatalf("acme/fast rates %+v", r)
		}
		if r, ok := tbl.Price("acme", "acme-big-1"); !ok || r.LongContextAbove != 1000 || r.LongContext.Input != 2_000_000 {
			t.Fatalf("acme/big rates %+v, %v: want the tier the file added", r, ok)
		}
		// The catalog the merge was handed is untouched.
		if *cat.Models["acme/fast"].Cost.LongContext.Input != 4 {
			t.Fatal("the merge wrote through to the catalog's tier")
		}
	})

	t.Run("a new threshold", func(t *testing.T) {
		tbl, _ := loadOver(t, cat, "", "version = 1\n\n[models.\"acme/fast\".cost]\nlong_context_above = 90000\n")
		wantWarnings(t, tbl)
		r, _ := tbl.Price("acme", "acme-fast-1")
		if r.LongContextAbove != 90_000 || r.LongContext.Input != 4_000_000 || r.Input != 1_000_000 {
			t.Fatalf("acme/fast rates %+v: want the threshold replaced, every rate kept", r)
		}
	})

	t.Run("a disabled tier", func(t *testing.T) {
		tbl, dir := loadOver(t, cat, "", "version = 1\n\n[models.\"acme/fast\".cost]\nlong_context_above = 0\n")
		wantWarnings(t, tbl)
		c := tbl.Models["acme/fast"].Cost
		if c.LongContext != nil || c.LongContextAbove == nil || *c.LongContextAbove != 0 || *c.Input != 1 {
			t.Fatalf("acme/fast cost = %+v, long %+v: want the tier off and the base rates kept", c, c.LongContext)
		}
		r, _ := tbl.Price("acme", "acme-fast-1")
		if r.LongContextAbove != 0 || r.LongPrompt(1<<40) || r.Long() != r.Base() {
			t.Fatalf("acme/fast rates %+v: want no tier", r)
		}
		if len(tbl.RedundantOverrides()) != 0 {
			t.Fatalf("RedundantOverrides = %q: turning a tier off is not the shipped entry", tbl.RedundantOverrides())
		}
		// It saves as the threshold 0 alone, and loads back off.
		if err := Save(dir, tbl); err != nil {
			t.Fatal(err)
		}
		again, err := LoadWith(dir, cat)
		if err != nil {
			t.Fatal(err)
		}
		if r, _ := again.Price("acme", "acme-fast-1"); r.LongContextAbove != 0 || again.Models["acme/fast"].Cost.LongContext != nil {
			t.Fatalf("the saved disabled tier loads back as %+v", r)
		}
	})

	t.Run("a threshold of 0 beside a table of its own", func(t *testing.T) {
		tbl, _ := loadOver(t, cat, "", `version = 1

[models."acme/fast"]
name = "Mine"

[models."acme/fast".cost]
long_context_above = 0

[models."acme/fast".cost.long_context]
input = 3.0
`)
		// The entry cannot stand, so the shipped one is used, the tier on.
		wantWarnings(t, tbl, []string{`models."acme/fast".cost`, "long_context_above", "craze's shipped entry is used"})
		if m := tbl.Models["acme/fast"]; m.Name != "Acme Fast" || *m.Cost.LongContextAbove != 50_000 || *m.Cost.LongContext.Input != 4 {
			t.Fatalf("acme/fast = %+v, cost %+v: want the shipped entry", m, m.Cost)
		}
	})
}

// TestLongContextSameAsShipped: an entry that writes the shipped threshold
// and long-context rates is noted as repeating the shipped entry, as one
// writing the shipped base rates is; one whose threshold or a long-context
// rate differs is not, nor is one adding a table to a model that ships none.
// Negative control: a check that ignores the tier notes the differing ones.
func TestLongContextSameAsShipped(t *testing.T) {
	cat := tiered(t)
	for _, tc := range []struct {
		name, models string
		redundant    bool
	}{
		{"the shipped tier", "[models.\"acme/fast\".cost]\nlong_context_above = 50000\n\n[models.\"acme/fast\".cost.long_context]\ninput = 4.0\ncache_write = 5.0\n", true},
		{"another threshold", "[models.\"acme/fast\".cost]\nlong_context_above = 60000\n", false},
		{"another long rate", "[models.\"acme/fast\".cost.long_context]\noutput = 9.0\n", false},
		{"a tier where none ships", "[models.\"acme/big\".cost]\nlong_context_above = 10\n\n[models.\"acme/big\".cost.long_context]\ninput = 1.0\noutput = 1.0\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, _ := loadOver(t, cat, "", "version = 1\n\n"+tc.models)
			wantWarnings(t, tbl)
			if got := len(tbl.RedundantOverrides()) == 1; got != tc.redundant {
				t.Fatalf("RedundantOverrides = %q; want redundant %v", tbl.RedundantOverrides(), tc.redundant)
			}
		})
	}
}

// TestLongContextConflictsWarn: two aliases of one identity whose costs
// differ only in the tier — the threshold, a long-context rate, or a table
// one has and the other does not — load with the warning naming both, as a
// base rate's difference does; equal tiers do not warn. Negative control:
// sameAs without the tier fields stays silent.
func TestLongContextConflictsWarn(t *testing.T) {
	const tier = "long_context_above = 1000\n"
	model := func(alias, costExtra, longOut string) string {
		s := fmt.Sprintf("\n[models.%[1]q]\nprovider = \"fireworks\"\nwire_model = \"accounts/fireworks/models/kimi-k3\"\n\n[models.%[1]q.cost]\ninput = 0.6\n%[2]s",
			alias, costExtra)
		if longOut != "" {
			s += fmt.Sprintf("\n[models.%q.cost.long_context]\ninput = 2.0\noutput = %s\n", alias, longOut)
		}
		return s
	}
	head := "version = 1\ncatalog = false\ndefault_model = \"fireworks/kimi-k3\"\n"
	for _, tc := range []struct {
		name string
		a, b string
		warn bool
	}{
		{"the same tier", model("fireworks/kimi-k3", tier, "4.0"), model("fireworks/kimi-k3-alt", tier, "4.0"), false},
		{"another threshold", model("fireworks/kimi-k3", tier, "4.0"), model("fireworks/kimi-k3-alt", "long_context_above = 2000\n", "4.0"), true},
		{"another long rate", model("fireworks/kimi-k3", tier, "4.0"), model("fireworks/kimi-k3-alt", tier, "4.5"), true},
		{"a tier and none", model("fireworks/kimi-k3", tier, "4.0"), model("fireworks/kimi-k3-alt", "", ""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Load(writeFiles(t, validProviders, head+tc.a+tc.b))
			if err != nil {
				t.Fatal(err)
			}
			warned := slices.ContainsFunc(got.Warnings, func(w string) bool {
				return strings.Contains(w, "fireworks/kimi-k3 and fireworks/kimi-k3-alt")
			})
			if warned != tc.warn {
				t.Fatalf("Warnings = %q; want a conflict warning %v", got.Warnings, tc.warn)
			}
		})
	}
}

// TestLongContextValidation: the tier's rules, on a table built in memory and
// on a merged one: a threshold above 0 needs the long-context table, with
// input and output; the table needs a threshold above 0; the threshold is at
// most the model's context_window when it declares one (equal to it loads);
// it is not negative; and each long-context rate is in the base rates' range.
// Every refusal names the model's table and the key. A file's own negative
// threshold or out-of-range long rate fails the load before any merge, and a
// user entry whose merged tier breaks a rule is dropped with a warning naming
// it, the shipped entry standing. Negative control: dropping a rule from
// validateTier lets its case load.
func TestLongContextValidation(t *testing.T) {
	const alias = "fireworks/kimi-k3"
	costTbl, longTbl := `models."fireworks/kimi-k3".cost`, `models."fireworks/kimi-k3".cost.long_context`
	long := func(in, out *float64) *LongContextCost { return &LongContextCost{Input: in, Output: out} }
	for _, tc := range []struct {
		name       string
		cost       *Cost
		table, key string // "" table: valid
	}{
		{"a threshold with no table", &Cost{LongContextAbove: ptr(int64(1000))}, costTbl, "long_context_above"},
		{"a table with no input", &Cost{LongContextAbove: ptr(int64(1000)), LongContext: long(nil, ptr(1.0))}, longTbl, "input"},
		{"a table with no output", &Cost{LongContextAbove: ptr(int64(1000)), LongContext: long(ptr(1.0), nil)}, longTbl, "output"},
		{"a table with no threshold", &Cost{LongContext: long(ptr(1.0), ptr(1.0))}, costTbl, "long_context_above"},
		{"a table under a threshold of 0", &Cost{LongContextAbove: ptr(int64(0)), LongContext: long(ptr(1.0), ptr(1.0))}, costTbl, "long_context_above"},
		{"a threshold above the window", &Cost{LongContextAbove: ptr(int64(262_145)), LongContext: long(ptr(1.0), ptr(1.0))}, costTbl, "long_context_above"},
		{"a negative threshold", &Cost{LongContextAbove: ptr(int64(-1))}, costTbl, "long_context_above"},
		{"a long rate out of range", &Cost{LongContextAbove: ptr(int64(1000)), LongContext: &LongContextCost{Input: ptr(1.0), Output: ptr(1.0), CacheRead: ptr(-0.5)}}, longTbl, "cache_read"},
		{"a long rate above the most", &Cost{LongContextAbove: ptr(int64(1000)), LongContext: long(ptr(MaxCostPerMillion+1), ptr(1.0))}, longTbl, "input"},
		{"a threshold equal to the window", &Cost{LongContextAbove: ptr(int64(262_144)), LongContext: long(ptr(1.0), ptr(1.0))}, "", ""},
		{"a threshold of 0 alone", &Cost{LongContextAbove: ptr(int64(0))}, "", ""},
		{"input and output alone", &Cost{LongContextAbove: ptr(int64(1)), LongContext: long(ptr(0.0), ptr(0.0))}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := validTable()
			setModel(tbl, alias, func(m *Model) { m.Cost = tc.cost })
			err := tbl.Validate()
			if tc.table == "" {
				if err != nil {
					t.Fatalf("a valid tier: %v", err)
				}
				return
			}
			wantFileError(t, err, ModelsFile, tc.table, tc.key)
		})
	}
	// A model with no window takes any threshold.
	tbl := validTable()
	setModel(tbl, "openrouter/minimax-m3", func(m *Model) {
		m.Cost = &Cost{LongContextAbove: ptr(int64(1 << 40)), LongContext: long(ptr(1.0), ptr(1.0))}
	})
	if err := tbl.Validate(); err != nil {
		t.Fatalf("a model with no window: %v", err)
	}

	// A file's own problems fail the load, naming the file.
	for _, tc := range []struct{ name, extra, table, key string }{
		{"a negative threshold", "[models.\"fireworks/kimi-k3\".cost]\nlong_context_above = -5\n", costTbl, "long_context_above"},
		{"a long rate out of range", "[models.\"fireworks/kimi-k3\".cost]\nlong_context_above = 5\n\n[models.\"fireworks/kimi-k3\".cost.long_context]\ninput = 1.0\noutput = nan\n", longTbl, "output"},
		{"an unknown long-context key", "[models.\"fireworks/kimi-k3\".cost.long_context]\ntier = 1\n", longTbl, "tier"},
		{"a threshold with no table", "[models.\"fireworks/kimi-k3\".cost]\nlong_context_above = 5\n", costTbl, "long_context_above"},
	} {
		t.Run("load: "+tc.name, func(t *testing.T) {
			dir := writeFiles(t, validProviders, validModels+"\n"+tc.extra)
			_, err := Load(dir)
			wantFileError(t, err, filepath.Join(dir, ModelsFile), tc.table, tc.key)
		})
	}

	// Over a catalog, an entry whose merged tier breaks a rule is dropped with
	// a warning naming the model, and the shipped entry stands.
	cat := tiered(t)
	for _, tc := range []struct{ name, models, key string }{
		{"a threshold above the window", "[models.\"acme/fast\".cost]\nlong_context_above = 100001\n", "long_context_above"},
		{"a threshold over a model with no table", "[models.\"acme/big\".cost]\nlong_context_above = 10\n", "long_context_above"},
	} {
		t.Run("merge: "+tc.name, func(t *testing.T) {
			tbl, _ := loadOver(t, cat, "", "version = 1\n\n"+tc.models)
			wantWarnings(t, tbl, []string{"models.", ".cost", tc.key, "craze's shipped entry is used"})
			if r, _ := tbl.Price("acme", "acme-fast-1"); r.LongContextAbove != 50_000 {
				t.Fatalf("acme/fast rates %+v: want the shipped tier", r)
			}
		})
	}
	// A file's own negative threshold fails a merged load too, before the
	// merge (checkOverlays).
	dir := writeFiles(t, "", "version = 1\n\n[models.\"acme/fast\".cost]\nlong_context_above = -1\n")
	_, err := LoadWith(dir, cat)
	wantFileError(t, err, filepath.Join(dir, ModelsFile), `models."acme/fast".cost`, "long_context_above")
}

// TestALeftOutTierRateIsTheBaseRate: a tier rate the merged table leaves out
// is priced at the model's base rate for that field, never at 0 — on a cost
// built in memory, and on a user's tier added to a model that ships none —
// while one a shipped tier sets is inherited first, as merging goes. The Cost
// keeps what was written: the fallback is in the rates alone, so the
// same-as-shipped check and the conflict warning compare what each file
// wrote. Negative control: a tier rate left out priced at 0 fails each case.
func TestALeftOutTierRateIsTheBaseRate(t *testing.T) {
	c := &Cost{Input: ptr(1.0), Output: ptr(2.0), CacheRead: ptr(0.5), CacheWrite: ptr(4.0),
		LongContextAbove: ptr(int64(1000)), LongContext: &LongContextCost{Input: ptr(10.0), Output: ptr(20.0)}}
	if r := ratesFromCost(c); r.LongContext != (TierRates{Input: 10_000_000, Output: 20_000_000, CacheRead: 500_000, CacheWrite: 4_000_000}) {
		t.Errorf("in memory: the tier rates %+v; want cache reads and writes at the base rates", r.LongContext)
	}
	if c.LongContext.CacheRead != nil || c.LongContext.CacheWrite != nil {
		t.Fatal("pricing filled in the written cost")
	}

	cat := tiered(t)
	tbl, _ := loadOver(t, cat, "", `version = 1

[models."acme/big".cost]
input = 1.0
output = 2.0
cache_read = 0.3
cache_write = 3.75
long_context_above = 500

[models."acme/big".cost.long_context]
input = 6.0
output = 12.0

[models."acme/fast".cost.long_context]
input = 6.0
`)
	wantWarnings(t, tbl)
	if r, _ := tbl.Price("acme", "acme-big-1"); r.LongContext != (TierRates{Input: 6_000_000, Output: 12_000_000, CacheRead: 300_000, CacheWrite: 3_750_000}) {
		t.Errorf("a user's tier: rates %+v; want its cache reads and writes at the base rates", r.LongContext)
	}
	if l := tbl.Models["acme/big"].Cost.LongContext; l.CacheRead != nil || l.CacheWrite != nil {
		t.Fatalf("the merged cost holds filled-in rates %+v", l)
	}
	// Over a shipped tier the shipped rate is inherited first: 0.4, not the
	// base cache_read, which acme/fast leaves out.
	if r, _ := tbl.Price("acme", "acme-fast-1"); r.LongContext != (TierRates{Input: 6_000_000, Output: 8_000_000, CacheRead: 400_000, CacheWrite: 5_000_000}) {
		t.Fatalf("over the shipped tier: rates %+v; want the shipped tier's own cache rates", r.LongContext)
	}
}
