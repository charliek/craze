package modeltable

import (
	"reflect"
	"strconv"
	"testing"
)

// Which models a picker offers, and in what order (plan 031 §3.6, Q4, P7):
// Choices over recentTable, whose "nk" provider never has a key unless a case
// gives it one.

// choiceAliases is each Choice's alias and rank, "alias#rank" for a recent one.
func choiceAliases(cs []Choice) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		s := c.Alias
		if c.Recent > 0 {
			s += "#" + strconv.Itoa(c.Recent)
		}
		out = append(out, s)
	}
	return out
}

// TestChoicesOffersConnectedProvidersOnly (Q4): with no memory and no current
// model, the list is every model whose provider has a key and nothing of the
// one that has none, by display name — here the alias, as none has a name —
// with no rank; each Choice carries its provider and a display name.
func TestChoicesOffersConnectedProvidersOnly(t *testing.T) {
	got := recentTable().Choices(nil, fundedEnv("p", "q"), "")
	want := []Choice{
		{Alias: "p/default", Name: "p/default", Provider: "p"},
		{Alias: "p/effort", Name: "p/effort", Provider: "p"},
		{Alias: "q/plain", Name: "q/plain", Provider: "q"},
		{Alias: "q/twin-a", Name: "q/twin-a", Provider: "q"},
		{Alias: "q/twin-b", Name: "q/twin-b", Provider: "q"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Choices =\n%+v\nwant\n%+v", got, want)
	}
	if got := recentTable().Choices(nil, fundedEnv(), ""); len(got) != 0 {
		t.Fatalf("with no key at all Choices = %+v, want nothing", got)
	}
}

// TestChoicesOrder (§3.6): the remembered models first, newest first, each
// with its rank; then the rest by display name — which is not alias order
// here — and by alias between two models of one name. A remembered model is
// found through Table.Recent: a renamed one by its identity, at its place; a
// re-pointed alias is not the model that was picked, so that model sorts with
// the rest.
func TestChoicesOrder(t *testing.T) {
	table := recentTable()
	named := func(alias, name string) {
		m := table.Models[alias]
		m.Name = name
		table.Models[alias] = m
	}
	named("p/default", "Zeta")
	named("p/effort", "Alpha")
	named("q/twin-a", "Twin")
	named("q/twin-b", "Twin")
	recent := []RecentEntry{
		entry(t, "q/twin-b", ""),
		{Alias: "q/plain-2025", Provider: "q", WireModel: "w-plain"}, // renamed: q/plain now
		{Alias: "p/default", Provider: "p", WireModel: "w-retired"},  // re-pointed: not recent
		{Alias: "gone/model", Provider: "gone", WireModel: "w-gone"}, // nothing: skipped
	}
	got := choiceAliases(table.Choices(recent, fundedEnv("p", "q"), ""))
	want := []string{"q/twin-b#1", "q/plain#2", "p/effort", "q/twin-a", "p/default"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Choices = %v, want %v", got, want)
	}
}

// TestChoicesAlwaysListsTheRunningModel (P7): the model a session runs on is in
// its own picker even when its provider has no key — at its rank when it is
// remembered, with the rest when not — and a current model the table does not
// have, or none, adds nothing.
func TestChoicesAlwaysListsTheRunningModel(t *testing.T) {
	table := recentTable()
	for _, tc := range []struct {
		name    string
		recent  []RecentEntry
		current string
		want    []string
	}{
		{name: "unfunded, not remembered", current: "nk/dry",
			want: []string{"nk/dry", "p/default", "p/effort"}},
		{name: "unfunded and remembered keeps its rank",
			recent: []RecentEntry{entry(t, "p/effort", ""), entry(t, "nk/dry", "high")}, current: "nk/dry",
			want: []string{"p/effort#1", "nk/dry#2", "p/default"}},
		{name: "funded changes nothing", current: "p/effort",
			want: []string{"p/default", "p/effort"}},
		{name: "one the table does not have adds nothing", current: "gone/model",
			want: []string{"p/default", "p/effort"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := choiceAliases(table.Choices(tc.recent, fundedEnv("p"), tc.current)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Choices = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestChoicesRanksCountOnlyWhatIsListed: a remembered model the list does not
// offer — its provider has no key — is passed over without leaving a gap, so a
// client reading the ranks sees 1, 2, … for exactly the rows it has.
func TestChoicesRanksCountOnlyWhatIsListed(t *testing.T) {
	recent := []RecentEntry{entry(t, "nk/dry", "high"), entry(t, "q/plain", ""), entry(t, "p/effort", "xhigh")}
	got := choiceAliases(recentTable().Choices(recent, fundedEnv("p", "q"), ""))
	want := []string{"q/plain#1", "p/effort#2", "p/default", "q/twin-a", "q/twin-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Choices = %v, want %v", got, want)
	}
}

// TestChoicesJudgeKeysAsResolveDoes (§3.2: "funded" is one rule for Resolve,
// Keys, Choices and StartModel): a provider whose only key is a variable set
// to a value that cannot be a key — shorter than MinKeyLen — has no usable
// key, so Resolve refuses its models and Choices does not offer them; an
// inline key funds its provider as it funds Resolve.
func TestChoicesJudgeKeysAsResolveDoes(t *testing.T) {
	table := recentTable()
	env := fakeEnv(map[string]string{"RECENT_P_KEY": "sk-recent-dummy-p-0001", "RECENT_Q_KEY": "short"})
	if _, err := table.Resolve("q/plain", env); err == nil {
		t.Fatal("control: Resolve took a key shorter than MinKeyLen")
	}
	if got, want := choiceAliases(table.Choices(nil, env, "")), []string{"p/default", "p/effort"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with q's variable too short Choices = %v, want %v", got, want)
	}
	nk := table.Providers["nk"]
	nk.APIKey = Secret("sk-recent-inline-nk-0001")
	table.Providers["nk"] = nk
	if got, want := choiceAliases(table.Choices(nil, env, "")), []string{"nk/dry", "p/default", "p/effort"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with nk's inline key Choices = %v, want %v", got, want)
	}
}
