package modeltable

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Plan 047 D1, D2 and P6 from the table's side: the effort levels a model
// may list (at most MaxEfforts, each at most MaxEffortLen bytes and one
// printable line), the effort a native session's switch lands on
// (SwitchEffort: the session's own memory view, else recent.json, else the
// default — never the effort it is on), and the effort facts every Choice
// carries.

// TestLoadRefusesEffortLevelsPastTheBounds (P6, A2): Load refuses a model
// whose efforts list more than MaxEfforts levels, or a level longer than
// MaxEffortLen bytes or not one printable line — on the FileError path an
// empty or repeated level takes, naming the file, the model's table and the
// key — whether the model is the user's own over the catalog, a shipped one's
// override, or in a directory with the catalog off; a table built in memory
// does not validate either. The controls: eight levels, and a level of
// exactly 32 bytes, load. Negative control: an effortsProblem with no count
// bound loads the nine levels and fails the first refusal.
func TestLoadRefusesEffortLevelsPastTheBounds(t *testing.T) {
	levels := func(n int) string {
		q := make([]string, n)
		for i := range q {
			q[i] = strconv.Quote(fmt.Sprintf("l%d", i))
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	at32, over32 := strings.Repeat("e", MaxEffortLen), strings.Repeat("e", MaxEffortLen+1)
	own := func(efforts string) string {
		return "\n[models.\"fireworks/mine\"]\nprovider = \"fireworks\"\nwire_model = \"accounts/x/mine\"\nefforts = " + efforts + "\n"
	}
	for _, c := range []struct{ name, models, table string }{
		{"the user's own model over the catalog", "version = 1\n", modelTable("fireworks/mine")},
		{"the user's own model, catalog off", validModels, modelTable("fireworks/mine")},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, efforts := range []string{levels(MaxEfforts), `["` + at32 + `"]`} {
				if _, err := Load(writeFiles(t, validProviders, c.models+own(efforts))); err != nil {
					t.Fatalf("control: efforts = %s: %v", efforts, err)
				}
			}
			for _, bad := range []struct{ efforts, reason string }{
				{levels(MaxEfforts + 1), fmt.Sprintf("lists %d levels; a model lists at most %d", MaxEfforts+1, MaxEfforts)},
				{`["low", "` + over32 + `"]`, fmt.Sprintf("entry 2 is %d bytes; an effort level is at most %d", MaxEffortLen+1, MaxEffortLen)},
				{`["low", "hi\ngh"]`, "entry 2 is not one printable line"},
				{`["low", "hi\u202egh"]`, "entry 2 is not one printable line"},
				{`["low", "hi\u2028gh"]`, "entry 2 is not one printable line"},
				{`["", "low"]`, "entry 1 is empty"},
			} {
				dir := writeFiles(t, validProviders, c.models+own(bad.efforts))
				_, err := Load(dir)
				fe := wantFileError(t, err, filepath.Join(dir, ModelsFile), c.table, "efforts")
				if fe.Reason != bad.reason {
					t.Fatalf("efforts = %q: reason %q, want %q", bad.efforts, fe.Reason, bad.reason)
				}
			}
		})
	}
	t.Run("a shipped model's override", func(t *testing.T) {
		dir := writeFiles(t, "", "version = 1\n\n[models.\"glm-5.3\"]\nefforts = "+levels(MaxEfforts+1)+"\n")
		_, err := Load(dir)
		wantFileError(t, err, filepath.Join(dir, ModelsFile), modelTable("glm-5.3"), "efforts")
	})
	t.Run("a table built in memory", func(t *testing.T) {
		tbl := validTable()
		m := tbl.Models["fireworks/kimi-k3"]
		m.Efforts = append(slices.Clone(m.Efforts), over32)
		tbl.Models["fireworks/kimi-k3"] = m
		wantFileError(t, tbl.Validate(), ModelsFile, modelTable("fireworks/kimi-k3"), "efforts")
	})
}

// TestTheShippedCatalogIsWithinTheEffortBounds (P6): no shipped model comes
// near the bounds — the most levels any lists is seven.
func TestTheShippedCatalogIsWithinTheEffortBounds(t *testing.T) {
	tbl, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	most := 0
	for alias, m := range tbl.Models {
		if r := effortsProblem(m.Efforts); r != "" {
			t.Errorf("%s: %s", alias, r)
		}
		most = max(most, len(m.Efforts))
	}
	if most > 7 {
		t.Fatalf("a shipped model lists %d levels; the plan's premise is at most 7", most)
	}
}

// TestSwitchEffortIsRememberedElseTheDefault (D1, O2): the effort a switch
// lands on is the session's own memory view first — even over a newer
// recent.json entry — while the model offers it, and its default when it
// does not; else what recent.json remembers, while offered; else the
// default; and none for a model with no effort control. It never depends on
// the effort a session is on: there is no such argument. Negative control: a
// SwitchEffort that reads the file before the memory view fails the second
// case.
func TestSwitchEffortIsRememberedElseTheDefault(t *testing.T) {
	table := recentTable()
	file := []RecentEntry{entry(t, "p/effort", "xhigh"), entry(t, "p/default", "high")}
	for _, tc := range []struct {
		name   string
		memory map[string]string
		recent []RecentEntry
		alias  string
		want   string
	}{
		{"nothing remembered: the default", nil, nil, "p/effort", "medium"},
		{"the memory view over the file", map[string]string{"p/effort": "low"}, file, "p/effort", "low"},
		{"the memory view, no longer offered: the default", map[string]string{"p/effort": "max"}, file, "p/effort", "medium"},
		{"the memory view of another model: the file", map[string]string{"p/default": "low"}, file, "p/effort", "xhigh"},
		{"the file", nil, file, "p/default", "high"},
		{"the file, no longer offered: the default", nil, []RecentEntry{entry(t, "p/effort", "turbo")}, "p/effort", "medium"},
		{"no effort control", map[string]string{"q/plain": ""}, nil, "q/plain", ""},
		{"no effort control, the file", nil, []RecentEntry{entry(t, "q/plain", "high")}, "q/plain", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := table.SwitchEffort(tc.recent, tc.memory, tc.alias); got != tc.want {
				t.Fatalf("SwitchEffort(%s) = %q, want %q", tc.alias, got, tc.want)
			}
		})
	}
}

// TestSessionChoicesCarryTheSwitchEffort (D2): every choice carries its
// levels in the table's order, its default and the effort a switch would land
// on, judged with the memory view and the file — so a picker shows a model
// it has not been switched to at the effort a switch would give it. A model
// with no effort control carries none. Neither memory nor the table is
// written to: a caller's change to a choice's levels leaves the table's.
// Negative control: a choice built without SwitchEffort fails the first
// check.
func TestSessionChoicesCarryTheSwitchEffort(t *testing.T) {
	table := recentTable()
	memory := map[string]string{"p/effort": "xhigh"}
	recent := []RecentEntry{entry(t, "p/default", "high")}
	facts := map[string]string{}
	for _, c := range table.SessionChoices(recent, memory, fundedEnv("p", "q"), "") {
		facts[c.Alias] = strings.Join(c.Efforts, ",") + "|" + c.DefaultEffort + "|" + c.SwitchEffort
	}
	want := map[string]string{
		"p/default": "low,high|low|high",
		"p/effort":  "low,medium,xhigh|medium|xhigh",
		"q/plain":   "||", "q/twin-a": "||", "q/twin-b": "||",
	}
	if !reflect.DeepEqual(facts, want) {
		t.Fatalf("the choices' facts are %v, want %v", facts, want)
	}
	if !reflect.DeepEqual(memory, map[string]string{"p/effort": "xhigh"}) {
		t.Fatalf("SessionChoices wrote to the memory view: %v", memory)
	}
	cs := table.Choices(nil, fundedEnv("p"), "")
	cs[0].Efforts[0] = "changed"
	if got := table.Models[cs[0].Alias].Efforts[0]; got == "changed" {
		t.Fatal("a choice's levels are the table's own slice")
	}
}

// TestRelistKeepsItsModelsAndRanksThemAgain (D2): Relist is the list a
// session already offers, ranked again by the memory a switch leaves and
// with its facts judged again — the same models whatever is funded (it
// resolves nothing: an unfunded running model stays, and nothing newly
// funded joins), the remembered first by rank, then the rest in Choices'
// order; an alias the table does not have is left out. Negative control: a
// Relist that resolves its aliases drops nk/dry and fails the first check.
func TestRelistKeepsItsModelsAndRanksThemAgain(t *testing.T) {
	table := recentTable()
	offered := []string{"nk/dry", "p/default", "p/effort", "gone/model"}
	recent := WithNewest([]RecentEntry{entry(t, "p/effort", "xhigh")}, entry(t, "nk/dry", "high"))
	got := table.Relist(offered, recent, map[string]string{"nk/dry": "high"})
	if want := []string{"nk/dry#1", "p/effort#2", "p/default"}; !reflect.DeepEqual(choiceAliases(got), want) {
		t.Fatalf("Relist = %v, want %v", choiceAliases(got), want)
	}
	if got[1].SwitchEffort != "xhigh" || got[2].SwitchEffort != "low" {
		t.Fatalf("Relist's switch efforts: %+v", got)
	}
	// The same members Choices would rank, in the same order, when every one
	// of them is funded: Relist is Choices without the resolving.
	funded := []string{"p/default", "p/effort"}
	if a, b := choiceAliases(table.Relist(funded, recent, nil)), choiceAliases(table.Choices(recent, fundedEnv("p"), "")); !reflect.DeepEqual(a, b) {
		t.Fatalf("Relist %v and Choices %v disagree over the same models", a, b)
	}
}

// TestWithNewestIsWhatRememberWrites (D2): WithNewest is the list Remember
// leaves — the entry first, its alias's older entry gone, at most RecentCap —
// so a session can rank by its own write without reading the file back, and
// the list it is handed is not written to.
func TestWithNewestIsWhatRememberWrites(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	var list []RecentEntry
	for i := range RecentCap + 2 {
		e := RecentEntry{Alias: fmt.Sprintf("p/m%02d", i), Provider: "p", WireModel: fmt.Sprintf("w%02d", i), Effort: "low"}
		if i == RecentCap+1 {
			e = RecentEntry{Alias: "p/m03", Provider: "p", WireModel: "w03", Effort: "high"}
		}
		list = WithNewest(list, e)
		if err := Remember(dir, e, now); err != nil {
			t.Fatal(err)
		}
		read := ReadRecent(dir)
		for j := range read {
			read[j].At = time.Time{}
		}
		if !reflect.DeepEqual(read, list) {
			t.Fatalf("after %d writes the file holds %+v; WithNewest says %+v", i+1, read, list)
		}
	}
	if len(list) != RecentCap || list[0].Alias != "p/m03" || list[0].Effort != "high" {
		t.Fatalf("the list is %+v; want %d entries, p/m03 at high first", list, RecentCap)
	}
	handed := []RecentEntry{{Alias: "a"}, {Alias: "b"}}
	_ = WithNewest(handed, RecentEntry{Alias: "b"})
	if handed[0].Alias != "a" || handed[1].Alias != "b" {
		t.Fatalf("WithNewest wrote to the list it was handed: %+v", handed)
	}
}
