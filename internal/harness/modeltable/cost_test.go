package modeltable

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A model's [models."<alias>".cost] section (plan 028 §3.14): input, output,
// cache_read and cache_write, each optionally set, $ per 1,000,000 tokens, no
// tiers; saved only when set, byte-identical to a models.toml written before
// the section existed otherwise; priced by identity, not by alias, in
// picodollars per token, rounded once. (ptr is secret_test.go's.)

// TestCostKeysRoundTrip: a [cost] table round-trips through Save and Load
// with exactly the keys it set — cache_write = 0.0 included, which is not
// its absence — and saves the same bytes twice; a model with no cost writes
// no [cost] table at all (TestSaveWritesStableReadableFiles pins that body
// byte for byte).
func TestCostKeysRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		cost *Cost
		want []string // lines the file holds
		not  []string // keys it must not
	}{
		{"every key", &Cost{Input: ptr(0.60), Output: ptr(2.50), CacheRead: ptr(0.15), CacheWrite: ptr(0.0)},
			[]string{`[models."fireworks/kimi-k3".cost]` + "\n", "input = 0.6\n", "output = 2.5\n",
				"cache_read = 0.15\n", "cache_write = 0.0\n"}, nil},
		{"one key", &Cost{Input: ptr(0.0375)},
			[]string{`[models."fireworks/kimi-k3".cost]` + "\n", "input = 0.0375\n"},
			[]string{"\noutput = ", "\ncache_read = ", "\ncache_write = "}},
		{"none", nil, nil, []string{".cost]", "\ninput = ", "\noutput = ", "\ncache_read = ", "\ncache_write = "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := validTable()
			setModel(tbl, "fireworks/kimi-k3", func(m *Model) { m.Cost = tc.cost })
			dir := t.TempDir()
			if err := Save(dir, tbl); err != nil {
				t.Fatal(err)
			}
			got, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tbl) {
				t.Fatalf("round trip =\n%+v\nwant\n%+v", got.Models["fireworks/kimi-k3"], tbl.Models["fireworks/kimi-k3"])
			}
			mb, err := os.ReadFile(filepath.Join(dir, ModelsFile))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(mb), want) {
					t.Errorf("%s does not hold %q:\n%s", ModelsFile, want, mb)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(string(mb), not) {
					t.Errorf("%s holds %q:\n%s", ModelsFile, not, mb)
				}
			}
			dir2 := t.TempDir()
			if err := Save(dir2, got); err != nil {
				t.Fatal(err)
			}
			if mb2, err := os.ReadFile(filepath.Join(dir2, ModelsFile)); err != nil || string(mb2) != string(mb) {
				t.Fatalf("a second save wrote other bytes (%v):\n%s\nwant\n%s", err, mb2, mb)
			}
		})
	}
}

// TestCostUnknownKeyIsStrict: a key [cost] does not have fails the load,
// naming the file, the table (models."<alias>".cost) and the key.
func TestCostUnknownKeyIsStrict(t *testing.T) {
	dir := writeFiles(t, validProviders, validModels+"\n[models.\"fireworks/kimi-k3\".cost]\ntier = \"low\"\n")
	_, err := Load(dir)
	fe := wantFileError(t, err, filepath.Join(dir, ModelsFile), `models."fireworks/kimi-k3".cost`, "tier")
	if fe.Reason != "unknown key" {
		t.Fatalf("reason = %q, want unknown key", fe.Reason)
	}
}

// TestValidateCostBounds: each set rate is 0 to MaxCostPerMillion, whether
// the table was loaded — the error then names the file's path — or built in
// memory; both ends of the range load, and a model with no [cost] is always
// valid.
func TestValidateCostBounds(t *testing.T) {
	for _, tc := range []struct {
		key   string
		set   *Cost
		valid bool
	}{
		{"input", &Cost{Input: ptr(-0.01)}, false},
		{"input", &Cost{Input: ptr(MaxCostPerMillion + 1)}, false},
		{"output", &Cost{Output: ptr(-1.0)}, false},
		{"cache_read", &Cost{CacheRead: ptr(-1.0)}, false},
		{"cache_write", &Cost{CacheWrite: ptr(-1.0)}, false},
		{"", &Cost{Input: ptr(0.0)}, true},
		{"", &Cost{Input: ptr(MaxCostPerMillion)}, true},
		{"", &Cost{CacheWrite: ptr(0.0)}, true},
		{"", nil, true},
	} {
		t.Run(tc.key, func(t *testing.T) {
			tbl := validTable()
			setModel(tbl, "fireworks/kimi-k3", func(m *Model) { m.Cost = tc.set })
			err := tbl.Validate()
			if tc.valid {
				if err != nil {
					t.Fatalf("a valid cost: %v", err)
				}
				return
			}
			wantFileError(t, err, ModelsFile, `models."fireworks/kimi-k3".cost`, tc.key)
		})
	}
}

// TestPicodollarRatesAreExact: a rate is rounded to picodollars per token
// exactly once (PD22) — $1/M is 1,000,000 p$/token — and 0.15 and 0.0375,
// which are not exact in float64, still round to the exact integers the plan
// names.
func TestPicodollarRatesAreExact(t *testing.T) {
	for _, tc := range []struct {
		dollarsPerMillion float64
		want              int64
	}{
		{0.15, 150000},
		{0.0375, 37500},
		{1, 1000000},
		{0, 0},
		{10000, 10000000000},
	} {
		if got := picodollarsPerToken(&tc.dollarsPerMillion); got != tc.want {
			t.Errorf("picodollarsPerToken(%v) = %d, want %d", tc.dollarsPerMillion, got, tc.want)
		}
	}
	if got := picodollarsPerToken(nil); got != 0 {
		t.Errorf("picodollarsPerToken(nil) = %d, want 0", got)
	}
}

// TestPriceLooksUpByIdentity: Price answers by (provider, wire model), not
// by alias, so a second alias of the same identity with no cost of its own
// still prices the same way, and an identity no priced alias names is
// unpriced.
func TestPriceLooksUpByIdentity(t *testing.T) {
	tbl := validTable()
	setModel(tbl, "fireworks/kimi-k3", func(m *Model) {
		m.Cost = &Cost{Input: ptr(0.60), Output: ptr(2.50), CacheRead: ptr(0.15), CacheWrite: ptr(0.0)}
	})
	// A second alias of the SAME identity, with no cost of its own.
	mirror := tbl.Models["fireworks/kimi-k3"]
	mirror.Cost = nil
	tbl.Models["fireworks/kimi-k3-mirror"] = mirror
	if err := tbl.Validate(); err != nil {
		t.Fatalf("control: %v", err)
	}

	r, ok := tbl.Price("fireworks", "accounts/fireworks/models/kimi-k3")
	if !ok {
		t.Fatal("Price = not found, want the priced identity")
	}
	want := Rates{Input: 600000, Output: 2500000, CacheRead: 150000, CacheWrite: 0}
	if r != want {
		t.Fatalf("Price = %+v, want %+v", r, want)
	}

	if _, ok := tbl.Price("openrouter", "minimax/minimax-m3"); ok {
		t.Fatal("an identity with no priced alias reported ok")
	}
	if _, ok := tbl.Price("nope", "nope"); ok {
		t.Fatal("an unknown identity reported ok")
	}
}

// TestConflictingPricesWarn: two aliases of one identity with different cost
// load with a table warning naming both, and Price still answers — the
// first alias in sorted order's rate.
func TestConflictingPricesWarn(t *testing.T) {
	const models = `version = 1
default_model = "fireworks/kimi-k3"

[models."fireworks/kimi-k3"]
provider = "fireworks"
wire_model = "accounts/fireworks/models/kimi-k3"

[models."fireworks/kimi-k3".cost]
input = 0.60

[models."fireworks/kimi-k3-alt"]
provider = "fireworks"
wire_model = "accounts/fireworks/models/kimi-k3"

[models."fireworks/kimi-k3-alt".cost]
input = 0.80
`
	dir := writeFiles(t, validProviders, models)
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got.Warnings, func(w string) bool {
		return strings.Contains(w, "fireworks/kimi-k3") && strings.Contains(w, "fireworks/kimi-k3-alt")
	}) {
		t.Fatalf("Warnings = %q, want one naming both aliases", got.Warnings)
	}
	r, ok := got.Price("fireworks", "accounts/fireworks/models/kimi-k3")
	if !ok || r.Input != 600000 {
		t.Fatalf("Price = %+v, %v; want the sorted-first alias's 600000", r, ok)
	}

	// Equal cost, no conflict: no warning.
	const agree = `version = 1
default_model = "fireworks/kimi-k3"

[models."fireworks/kimi-k3"]
provider = "fireworks"
wire_model = "accounts/fireworks/models/kimi-k3"

[models."fireworks/kimi-k3".cost]
input = 0.60

[models."fireworks/kimi-k3-alt"]
provider = "fireworks"
wire_model = "accounts/fireworks/models/kimi-k3"

[models."fireworks/kimi-k3-alt".cost]
input = 0.60
`
	dir2 := writeFiles(t, validProviders, agree)
	got2, err := Load(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Warnings) != 0 {
		t.Fatalf("Warnings = %q, want none for agreeing prices", got2.Warnings)
	}
}
