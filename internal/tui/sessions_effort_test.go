package tui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Plan 047 D5 (§7 A5): the session list's /effort offers real levels — on
// native `default` and the levels of the model new sessions start on, read
// from its table (the start model's when none was chosen); on an ACP provider
// every level craze's providers name — takes only a value it offers, and an
// effort kept from before a choice of a model that does not offer it is
// shown not offered.

// nativeEffortHome writes a native table under a CRAZE_HOME of the test's
// own, every model funded: wonder, minimal to max; spark, minimal to xhigh,
// the default model; plain, no effort control. It answers its native dir.
func nativeEffortHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	providers := `version = 1

[providers.funded]
driver = "openai-compat"
base_url = "http://127.0.0.1:1/v1"
api_key = "sk-d5-test-0123456789abcdef"
source = "manual"
`
	models := `version = 1
catalog = false
default_model = "spark"

[models.wonder]
provider = "funded"
wire_model = "wonder-wire"
name = "Wonder"
efforts = ["minimal", "low", "medium", "high", "xhigh", "max"]
default_effort = "high"

[models.spark]
provider = "funded"
wire_model = "spark-wire"
name = "Spark"
efforts = ["minimal", "low", "medium", "high", "xhigh"]
default_effort = "high"

[models.plain]
provider = "funded"
wire_model = "plain-wire"
name = "Plain"
`
	if err := os.WriteFile(filepath.Join(dir, modeltable.ProvidersFile), []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, modeltable.ModelsFile), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// sessRead runs the read a choice started — readNativeDefault or
// readNativeLevels, named by part, among cmd — and hands its answer to m.
func sessRead(t *testing.T, m Model, cmd tea.Cmd, part string) Model {
	t.Helper()
	tm, _ := m.Update(runWatched(t, mustCmd(t, cmd, part)))
	return tm.(Model)
}

// effortValues is the list's /effort popup over `/effort `: its values, and
// the one marked current ("" for none).
func effortValues(t *testing.T, m Model) ([]string, string) {
	t.Helper()
	mm, _ := typeList(t, m, "/effort ")
	current := ""
	for _, it := range mm.sessList.in.cmd.ans.Items {
		if it.Note == sessCurrentNote {
			current = it.Name
		}
	}
	return cmdItems(mm), current
}

// TestTheListsEffortOffersTheModelsLevels (A5): with native chosen, /effort
// offers `default` and the start model's levels (spark: minimal to xhigh);
// /model wonder offers wonder's, minimal to max, once its table has been read
// — every level until then — and a model with no effort control offers
// `default` alone. A typed value is taken only from what is offered. On an
// ACP provider every level is offered. Negative control: a sessEffortOffered
// that answers sessEffortLevels whatever the provider offers max on spark and
// fails the first check.
func TestTheListsEffortOffersTheModelsLevels(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	if got, cur := effortValues(t, m); !slices.Equal(got, []string{"default", "minimal", "low", "medium", "high", "xhigh", "max"}) || cur != "default" {
		t.Fatalf("on cursor /effort lists %v (current %q)", got, cur)
	}
	m.nativeDir, m.nativeEnv = nativeEffortHome(t), func(string) string { return "" }

	m, _ = typeList(t, m, "/provider native")
	m, cmd := press(m, enter())
	m = sessRead(t, m, cmd, "readNativeDefault")
	spark := []string{"default", "minimal", "low", "medium", "high", "xhigh"}
	if got, _ := effortValues(t, m); !slices.Equal(got, spark) {
		t.Fatalf("on native's start model (spark) /effort lists %v, want %v", got, spark)
	}

	m, _ = typeList(t, m, "/model wonder")
	m, cmd = press(m, enter())
	if got, _ := effortValues(t, m); !slices.Equal(got, sessEffortLevels) {
		t.Fatalf("before wonder's levels are read /effort lists %v; want every level", got)
	}
	m = sessRead(t, m, cmd, "readNativeLevels")
	wonder := []string{"default", "minimal", "low", "medium", "high", "xhigh", "max"}
	if got, _ := effortValues(t, m); !slices.Equal(got, wonder) {
		t.Fatalf("on wonder /effort lists %v, want %v", got, wonder)
	}

	m, _ = typeList(t, m, "/model plain")
	m, cmd = press(m, enter())
	m = sessRead(t, m, cmd, "readNativeLevels")
	if got, _ := effortValues(t, m); !slices.Equal(got, []string{"default"}) {
		t.Fatalf("on plain /effort lists %v; want default alone", got)
	}
	m, _ = typeList(t, m, "/effort high")
	if m.sessList.in.cmd.visible() {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m, _ = press(m, enter())
	if h := sessHint(m); h != "no effort high: one of default" || m.sessPick.effort != "" {
		t.Fatalf("/effort high on plain: the hint %q, the pick %q", h, m.sessPick.effort)
	}

	// /model default on native: the start model's levels again.
	m = clearInput(t, m)
	m, _ = typeList(t, m, "/model default")
	if m.sessList.in.cmd.visible() {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m, cmd = press(m, enter())
	m = sessRead(t, m, cmd, "readNativeLevels")
	if got, _ := effortValues(t, m); !slices.Equal(got, spark) {
		t.Fatalf("on native's default model /effort lists %v, want spark's %v", got, spark)
	}
}

// TestAKeptEffortTheModelDoesNotOfferIsShownSo (A5, P9): /effort max chosen on
// wonder is kept when the model moves to spark, which tops out at xhigh: the
// rule and the hint say `max (not offered)`, the popup marks nothing current,
// and new sessions are still started at max — the host notes it, as it
// always has. Back on wonder it is offered again. Negative control: a
// sessNewSettings that does not ask what is offered draws `· max` alone.
func TestAKeptEffortTheModelDoesNotOfferIsShownSo(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	m.nativeDir, m.nativeEnv = nativeEffortHome(t), func(string) string { return "" }
	pick := func(m Model, line string) Model {
		t.Helper()
		m, _ = typeList(t, m, line)
		if m.sessList.in.cmd.visible() {
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
		}
		m, cmd := press(m, enter())
		if c := findCmd(cmd, "readNativeLevels"); c != nil {
			m = sessRead(t, m, c, "readNativeLevels")
		}
		return m
	}
	m, _ = typeList(t, m, "/provider native")
	m, cmd := press(m, enter())
	m = sessRead(t, m, cmd, "readNativeDefault")
	m = pick(m, "/model wonder")
	m = pick(m, "/effort max")
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · native · wonder · max") {
		t.Fatalf("on wonder at max the rule %q", r)
	}
	m = pick(m, "/model spark")
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · native · spark · max (not offered)") {
		t.Fatalf("on spark the rule %q; want max not offered", r)
	}
	if h := sessHint(m); h != "new sessions use native · spark · max (not offered)" {
		t.Fatalf("on spark the hint %q", h)
	}
	if _, cur := effortValues(t, m); cur != "" {
		t.Fatalf("on spark the popup marks %q current; max is not among its values", cur)
	}
	if s, err := m.sessNewSpec("/x"); err != nil || s.Effort != "max" || s.Model != "spark" {
		t.Fatalf("the spec %+v, %v; want spark, still at max", s, err)
	}
	m = pick(m, "/model wonder")
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · native · wonder · max") {
		t.Fatalf("back on wonder the rule %q", r)
	}
	if _, cur := effortValues(t, m); cur != "max" {
		t.Fatalf("back on wonder the popup marks %q current", cur)
	}
}

// TestTheListsEffortOffersTheLevelsOfTheSessionItCameFrom (A5): with no model
// chosen, new sessions start on the model of the session the list came from,
// and /effort offers that model's levels as the session states them (effort
// facts) — one's two, then, once the session is on dflt, dflt's two.
// Negative control: a sessEffortOffered that reads only the pick's levels
// offers every level here and fails the first check.
func TestTheListsEffortOffersTheLevelsOfTheSessionItCameFrom(t *testing.T) {
	isolateSkillsHome(t)
	home := os.Getenv("HOME")
	for _, d := range newSessDirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fs := &startSessions{fakeSessions: &fakeSessions{}}
	stub := NewStub()
	stub.PublishEffortFacts = true
	stub.SetProvider(agent.NativeProvider())
	tm, _ := New(Config{Session: stub, Theme: "tokyo-night", Workspace: filepath.Join(home, "projects", "craze"), Yolo: true, Sessions: fs}).
		Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := startedLikeInit(t, tm.(Model))
	m.clock = func() time.Time { return sessNow }
	stub.mu.Lock()
	stub.snap.Models = agent.CloneModels(factsModels())
	stub.snap.CurrentModel = "one"
	stub.mu.Unlock()
	stub.SetModelCatalogs(factsCatalogs())
	m = newList(t, republish(t, m), fs)
	if got, _ := effortValues(t, m); !slices.Equal(got, []string{"default", "low", "high"}) {
		t.Fatalf("from a native session on one /effort lists %v", got)
	}
	if _, err := stub.SetModel(context.Background(), "", "dflt"); err != nil {
		t.Fatal(err)
	}
	m = applyPending(t, m)
	if got, _ := effortValues(t, m); !slices.Equal(got, []string{"default", "low", "medium"}) {
		t.Fatalf("from a native session on dflt /effort lists %v", got)
	}
}

// TestATypedModelIsResolvedForItsLevels (C2 review F4): `/model SPARK`, typed
// with the completion dismissed, is the model a session started from the
// list runs — spark, as --model resolves it — so /effort offers spark's
// levels and refuses its missing max. Negative control: a nativeModelLevels
// that looks the typed name up as it stands finds no SPARK and offers every
// level.
func TestATypedModelIsResolvedForItsLevels(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	m.nativeDir, m.nativeEnv = nativeEffortHome(t), func(string) string { return "" }
	m, _ = typeList(t, m, "/provider native")
	m, cmd := press(m, enter())
	m = sessRead(t, m, cmd, "readNativeDefault")
	m, _ = typeList(t, m, "/model wonder")
	m, cmd = press(m, enter())
	m = sessRead(t, m, cmd, "readNativeLevels")

	m, _ = typeList(t, m, "/model SPARK")
	if m.sessList.in.cmd.visible() {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m, cmd = press(m, enter())
	m = sessRead(t, m, cmd, "readNativeLevels")
	if got, _ := effortValues(t, m); !slices.Equal(got, []string{"default", "minimal", "low", "medium", "high", "xhigh"}) {
		t.Fatalf("on SPARK /effort lists %v; want spark's levels", got)
	}
	m, _ = typeList(t, m, "/effort max")
	if m.sessList.in.cmd.visible() {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m, _ = press(m, enter())
	if h := sessHint(m); h != "no effort max: one of default, minimal, low, medium, high, xhigh" || m.sessPick.effort != "" {
		t.Fatalf("/effort max on SPARK: the hint %q, the pick %q", h, m.sessPick.effort)
	}
}

// TestAnOriginWithoutFactsHasItsLevelsRead (C2 review F5): a list opened over
// a native session that states no effort facts — an older host — on plain,
// a model with no effort control, with no model chosen: new sessions start
// on plain, so the list reads plain's levels from the table as it opens, and
// /effort offers `default` alone and refuses high. Negative control: an
// askNativeLevels that asks only for a picked model offers every level and
// takes high.
func TestAnOriginWithoutFactsHasItsLevelsRead(t *testing.T) {
	isolateSkillsHome(t)
	home := os.Getenv("HOME")
	for _, d := range newSessDirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fs := &startSessions{fakeSessions: &fakeSessions{}}
	stub := NewStub()
	stub.SetProvider(agent.NativeProvider())
	dir := nativeEffortHome(t)
	tm, _ := New(Config{Session: stub, Theme: "tokyo-night", Workspace: filepath.Join(home, "projects", "craze"), Yolo: true,
		Sessions: fs, NativeDir: dir, Getenv: func(string) string { return "" }}).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := startedLikeInit(t, tm.(Model))
	m.clock = func() time.Time { return sessNow }
	stub.mu.Lock()
	stub.snap.Models = []agent.ModelInfo{{ID: "plain", Name: "Plain"}, {ID: "wonder", Name: "Wonder"}}
	stub.snap.CurrentModel = "plain"
	stub.mu.Unlock()
	stub.SetModelCatalogs(map[string][]agent.ConfigOption{"plain": nil})
	m = republish(t, m)
	if m.effortFacts() {
		t.Fatal("fixture: the origin states effort facts")
	}
	m, open := press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if !m.sessList.open {
		t.Fatal("← did not open the list")
	}
	m = sessRead(t, m, open, "readNativeLevels")
	if got, _ := effortValues(t, m); !slices.Equal(got, []string{"default"}) {
		t.Fatalf("from an origin on plain without facts /effort lists %v; want default alone", got)
	}
	m, _ = typeList(t, m, "/effort high")
	if m.sessList.in.cmd.visible() {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m, _ = press(m, enter())
	if h := sessHint(m); h != "no effort high: one of default" || m.sessPick.effort != "" {
		t.Fatalf("/effort high on plain: the hint %q, the pick %q", h, m.sessPick.effort)
	}
}

// TestTheDefaultModelsLevelsAreReadAgain (C2 fix review 3): `/model default`
// leaves new sessions on native's start model, which follows the model
// memory, so what "" names can change while the list is shut. Chosen while
// the memory says wonder, /effort offers wonder's levels; once the memory
// says plain (no effort control), the list's next opening reads the levels
// again — /effort high is then refused, and the levels' provenance is plain
// — and so does choosing `/model default` again. Negative controls: a cache
// asked once per model, whatever the opening, schedules no read at the
// reopening; a choice that keeps the cache schedules none at the second
// `/model default`.
func TestTheDefaultModelsLevelsAreReadAgain(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	dir := nativeEffortHome(t)
	m.nativeDir, m.nativeEnv = dir, func(string) string { return "" }
	remember := func(alias string, at time.Time) {
		t.Helper()
		if err := modeltable.Remember(dir, modeltable.RecentEntry{Alias: alias, Provider: "funded", WireModel: alias + "-wire"}, at); err != nil {
			t.Fatal(err)
		}
	}
	effortHigh := func(m Model) Model {
		t.Helper()
		m, _ = typeList(t, m, "/effort high")
		if m.sessList.in.cmd.visible() {
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
		}
		m, _ = press(m, enter())
		return m
	}
	remember("wonder", sessNow.Add(-time.Hour))
	m, _ = typeList(t, m, "/provider native")
	m, cmd := press(m, enter())
	m = sessRead(t, m, cmd, "readNativeDefault")
	m, _ = typeList(t, m, "/model default")
	if m.sessList.in.cmd.visible() {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m, cmd = press(m, enter())
	m = sessRead(t, m, cmd, "readNativeLevels")
	if got, _ := effortValues(t, m); !slices.Contains(got, "max") || m.sessPick.levels.alias != "wonder" {
		t.Fatalf("on the default while the memory says wonder /effort lists %v (from %q)", got, m.sessPick.levels.alias)
	}

	remember("plain", sessNow)
	m = clearInput(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open {
		t.Fatal("esc on an empty input did not leave the list")
	}
	m, open := press(m, tea.KeyMsg{Type: tea.KeyLeft})
	m = sessRead(t, m, open, "readNativeLevels")
	if got, _ := effortValues(t, m); !slices.Equal(got, []string{"default"}) || m.sessPick.levels.alias != "plain" {
		t.Fatalf("reopened on the default, the memory now plain: /effort lists %v (from %q)", got, m.sessPick.levels.alias)
	}
	m = effortHigh(m)
	if h := sessHint(m); h != "no effort high: one of default" || m.sessPick.effort != "" {
		t.Fatalf("/effort high on plain: the hint %q, the pick %q", h, m.sessPick.effort)
	}

	// Choosing the default again reads again: the memory back on wonder.
	remember("wonder", sessNow.Add(time.Hour))
	m = clearInput(t, m)
	m, _ = typeList(t, m, "/model default")
	if m.sessList.in.cmd.visible() {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m, cmd = press(m, enter())
	m = sessRead(t, m, cmd, "readNativeLevels")
	if got, _ := effortValues(t, m); !slices.Contains(got, "max") || m.sessPick.levels.alias != "wonder" {
		t.Fatalf("/model default again, the memory wonder: /effort lists %v (from %q)", got, m.sessPick.levels.alias)
	}
}
