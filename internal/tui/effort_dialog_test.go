package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Plan 047 D3 (§7 A3): on a session that states its models' effort facts the
// model dialog's effort row is the highlighted model's — its levels, the
// session's effort on the current model, else the effort a switch would land
// on, else the default, else nothing bracketed — a touched choice is kept as
// the cursor moves, and Enter sends the effort the row showed for the model it
// switches to. Without the facts the row is the current model's, as it always
// was.

// factsModels is the model list a facts Stub states: one, the model the
// session is on, low and high; two, six levels, landing on max (its
// SwitchEffort); dflt, landing on nothing remembered but defaulting to
// medium; none, no effort control at all; bare, three levels with no default
// and no memory.
func factsModels() []agent.ModelInfo {
	return []agent.ModelInfo{
		{ID: "one", Name: "One", Efforts: []string{"low", "high"}, DefaultEffort: "high", SwitchEffort: "high"},
		{ID: "two", Name: "Two", Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}, DefaultEffort: "high", SwitchEffort: "max"},
		{ID: "dflt", Name: "Dflt", Efforts: []string{"low", "medium"}, DefaultEffort: "medium"},
		{ID: "none", Name: "None"},
		{ID: "bare", Name: "Bare", Efforts: []string{"low", "medium", "high"}},
	}
}

// factsCatalogs is each factsModels model's own catalog, as a native session
// advertises it: the effort option alone, for a model that has levels, on the
// effort it is at.
func factsCatalogs() map[string][]agent.ConfigOption {
	effort := func(cur string, levels ...string) []agent.ConfigOption {
		return []agent.ConfigOption{*agent.NativeEffortOption(levels, cur)}
	}
	return map[string][]agent.ConfigOption{
		"one":  effort("low", "low", "high"),
		"two":  effort("high", "minimal", "low", "medium", "high", "xhigh", "max"),
		"dflt": effort("medium", "low", "medium"),
		"none": nil,
		"bare": effort("", "low", "medium", "high"),
	}
}

// factsStub is a 100×30 model over a Stub that states its models' effort facts
// (PublishEffortFacts, set before it starts), on factsModels and their
// catalogs, on model current; facts false is the same Stub saying nothing of
// them — an ACP session or an older host.
func factsStub(t *testing.T, current string, facts bool) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.PublishEffortFacts = facts
	m := startStub(t, stub, t.TempDir(), 100, 30)
	stub.mu.Lock()
	stub.snap.Models = agent.CloneModels(factsModels())
	stub.snap.CurrentModel = current
	stub.mu.Unlock()
	stub.SetModelCatalogs(factsCatalogs())
	m = republish(t, m)
	stubDeltas(t, stub)
	if m.effortFacts() != facts {
		t.Fatalf("the model says effort facts %v, want %v", m.effortFacts(), facts)
	}
	return m, stub
}

// effortRow is the dialog's effort row as drawn, "" when it draws none.
func effortRow(t *testing.T, m Model) string {
	t.Helper()
	for _, ln := range rows(plainView(m)) {
		if i := strings.Index(ln, "effort  "); i >= 0 && strings.Contains(ln, "│") {
			row := strings.TrimRight(strings.TrimSuffix(strings.TrimRight(ln, " "), "│"), " ")
			return row[strings.Index(row, "│")+len("│"):]
		}
	}
	return ""
}

// highlight moves the dialog's cursor onto model id, with ↓.
func highlight(t *testing.T, m Model, id string) Model {
	t.Helper()
	for range len(m.dialogModelList()) + 1 {
		if list := m.dialogModelList(); m.mdlg.sel < len(list) && list[m.mdlg.sel].ID == id {
			return m
		}
		m = pressKey(t, m, tea.KeyDown)
	}
	t.Fatalf("no row %q in the dialog's list", id)
	return m
}

// TestTheEffortRowFollowsTheHighlight (A3): the row is the current model's
// option on the session's own effort, and as the cursor moves it is each
// highlighted model's levels — on the effort a switch would land on (two:
// max), else its default (dflt: medium), else nothing bracketed (bare) — and
// a model with no levels has no row at all, nor a tab in the footer. A
// session whose current model has no effort option shows the row as soon as
// a model with levels is highlighted, under native's effort id (P7).
// Negative control: a modelDialogTabs that ignores the facts (catalogTabs
// alone) shows one's row on two and fails the first highlight.
func TestTheEffortRowFollowsTheHighlight(t *testing.T) {
	m, _ := factsStub(t, "one", true)
	m = openDialog(t, m)
	for _, step := range []struct{ model, row string }{
		{"one", "  effort  [low]  high"},
		{"two", "  effort  minimal  low  medium  high  xhigh  [max]"},
		{"dflt", "  effort  low  [medium]"},
		{"none", ""},
		{"bare", "  effort  low  medium  high"},
	} {
		m = highlight(t, m, step.model)
		if got := effortRow(t, m); got != step.row {
			t.Fatalf("on %s the effort row is %q, want %q:\n%s", step.model, got, step.row, plainView(m))
		}
		hasTab := strings.Contains(plainView(m), "tab effort")
		if hasTab != (step.row != "") {
			t.Fatalf("on %s the footer names the effort tab: %v:\n%s", step.model, hasTab, plainView(m))
		}
	}

	m, _ = factsStub(t, "none", true)
	m = openDialog(t, m)
	if got := effortRow(t, m); got != "" {
		t.Fatalf("on none, current, the effort row is %q", got)
	}
	m = highlight(t, m, "two")
	if got := effortRow(t, m); got != "  effort  minimal  low  medium  high  xhigh  [max]" {
		t.Fatalf("highlighting two from none: the effort row is %q", got)
	}
	tabs := m.modelDialogTabs()
	if len(tabs) != 1 || tabs[0].opt.ID != agent.NativeEffortID || tabs[0].role != roleEffort {
		t.Fatalf("the tabs on two from none are %+v; want the effort under native's id", tabs)
	}
}

// TestATouchedEffortIsKeptAsTheCursorMoves (A3): an effort chosen on one
// model is the user's choice for every model that offers it — shown again on
// the way back to it — and a model that does not offer it shows its own
// value meanwhile; a model with no levels takes the effort tab away, and the
// focus with it, back to the list. Negative control: a repaired that untouches
// a choice the highlighted model does not offer (today's rule for every tab)
// shows two's own max on the way back, not the xhigh chosen.
func TestATouchedEffortIsKeptAsTheCursorMoves(t *testing.T) {
	m, _ := factsStub(t, "one", true)
	m = openDialog(t, m)
	m = highlight(t, m, "two")
	m = pressKey(t, m, tea.KeyTab)
	if m.mdlg.focus != dialogFocus(agent.NativeEffortID) {
		t.Fatalf("tab focused %q, want the effort row", m.mdlg.focus)
	}
	m = pressKey(t, m, tea.KeyLeft)
	if got := effortRow(t, m); got != "> effort  minimal  low  medium  high  [xhigh]  max" {
		t.Fatalf("after ← on two the row is %q", got)
	}
	m = highlight(t, m, "none")
	if m.mdlg.focus != focusList || effortRow(t, m) != "" {
		t.Fatalf("on none the focus is %q and the row %q; want the list, and no row", m.mdlg.focus, effortRow(t, m))
	}
	m = highlight(t, m, "bare")
	if got := effortRow(t, m); got != "  effort  low  medium  high" {
		t.Fatalf("on bare, which has no xhigh, the row is %q", got)
	}
	// Round the list again, through one — low and high, no xhigh — to two.
	m = highlight(t, m, "one")
	if got := effortRow(t, m); got != "  effort  [low]  high" {
		t.Fatalf("on one the row is %q; want the session's own low", got)
	}
	m = highlight(t, m, "two")
	if got := effortRow(t, m); got != "  effort  minimal  low  medium  high  [xhigh]  max" {
		t.Fatalf("back on two the row is %q; want the xhigh chosen there", got)
	}
	if m.mdlg.focus != focusList {
		t.Fatalf("back on two the focus is %q; the effort row lost it on none", m.mdlg.focus)
	}
}

// TestTheDialogSendsTheEffortItShows (A3): Enter on another model sends the
// effort its row showed — the effort a switch would land on, untouched — so
// the session lands on what the box showed; on a model whose row brackets
// nothing it sends no effort unless one is chosen; and on the current model
// it sends one only when the user moved it, as every tab. Negative control:
// an apply that sends an untouched effort tab only when touched (today's
// rule) leaves two on its catalog's high and fails the first case.
func TestTheDialogSendsTheEffortItShows(t *testing.T) {
	m, stub := factsStub(t, "one", true)
	writes := configWrites(stub)

	m = openDialog(t, m)
	m = highlight(t, m, "two")
	m = applyDialog(t, m)
	if got := writes(); !slices.Equal(got, []string{"effort=max"}) {
		t.Fatalf("the switch to two sent %v, want the max it showed", got)
	}
	if got := optionCurrent(stub.Snapshot().Config, agent.NativeEffortID); stub.Snapshot().CurrentModel != "two" || got != "max" {
		t.Fatalf("the session is on %s at %q", stub.Snapshot().CurrentModel, got)
	}
	if got := texts(m, entryNote); !slices.Equal(got, []string{"model → two", "effort → max"}) {
		t.Fatalf("notes %q", got)
	}

	m = openDialog(t, m)
	m = highlight(t, m, "bare")
	m = applyDialog(t, m)
	if got := writes(); !slices.Equal(got, []string{"effort=max"}) || stub.Snapshot().CurrentModel != "bare" {
		t.Fatalf("the switch to bare, nothing bracketed, sent %v (on %s)", got, stub.Snapshot().CurrentModel)
	}

	// On the current model: untouched is nothing; moved is sent.
	m = openDialog(t, m)
	m = applyDialog(t, m)
	if got := writes(); len(got) != 1 {
		t.Fatalf("an untouched current model sent %v", got)
	}
	m = openDialog(t, m)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight)
	if got := effortRow(t, m); got != "> effort  [low]  medium  high" {
		t.Fatalf("→ on a row with nothing bracketed: %q; want its first level", got)
	}
	_ = applyDialog(t, m)
	if got := writes(); !slices.Equal(got, []string{"effort=max", "effort=low"}) {
		t.Fatalf("the moved effort on bare sent %v", got)
	}
}

// TestACatalogDeltaReseedsTheUntouchedEffort (A3): while the box is open a
// catalog delta that moves the highlighted model's switch effort moves an
// untouched row with it, and leaves a touched one where the user put it.
// Negative control: a repaired that keeps an untouched effort row's seeded
// value keeps showing two's old max after the delta.
func TestACatalogDeltaReseedsTheUntouchedEffort(t *testing.T) {
	m, _ := factsStub(t, "one", true)
	m = openDialog(t, m)
	m = highlight(t, m, "two")
	if got := effortRow(t, m); got != "  effort  minimal  low  medium  high  xhigh  [max]" {
		t.Fatalf("before the delta the row is %q", got)
	}
	moved := factsModels()
	moved[1].SwitchEffort = "medium"
	m = feed(t, m, catalogDelta(900, 50, moved))
	if got := effortRow(t, m); got != "  effort  minimal  low  [medium]  high  xhigh  max" {
		t.Fatalf("after the delta the untouched row is %q", got)
	}
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight)
	moved[1].SwitchEffort = "xhigh"
	m = feed(t, m, catalogDelta(901, 51, moved))
	if got := effortRow(t, m); got != "> effort  minimal  low  medium  [high]  xhigh  max" {
		t.Fatalf("after the second delta the touched row is %q", got)
	}
}

// TestWithoutEffortFactsTheRowIsTheCurrentModels (A3): the same Stub saying
// nothing of its facts — an ACP session, an older host — draws the current
// model's effort row whatever is highlighted, as the dialog always has, and
// sends nothing for it untouched. Negative control: a modelDialogTabs that
// reads the facts without asking effortFacts fails the first highlight.
func TestWithoutEffortFactsTheRowIsTheCurrentModels(t *testing.T) {
	m, stub := factsStub(t, "one", false)
	writes := configWrites(stub)
	m = openDialog(t, m)
	for _, id := range []string{"two", "none", "bare"} {
		m = highlight(t, m, id)
		if got := effortRow(t, m); got != "  effort  [low]  high" {
			t.Fatalf("without facts, on %s the row is %q; want one's", id, got)
		}
	}
	m = highlight(t, m, "two")
	_ = applyDialog(t, m)
	if got := writes(); len(got) != 0 || stub.Snapshot().CurrentModel != "two" {
		t.Fatalf("without facts the switch sent %v (on %s)", got, stub.Snapshot().CurrentModel)
	}
}

// TestTheDialogLandsOnTheEffortItShowedOverARealSession (A3, D2's freshness
// note): a real native session's catalog says Alpha Two lands on max, from
// the model memory it read; another craze process then remembers Alpha Two
// at low, which the session does not watch — a typed /model would read it
// fresh and land on low. The dialog still shows max, and Enter on Alpha Two
// lands on the max it showed. Negative control: an apply that sends an
// untouched effort tab only when touched leaves the session on low.
func TestTheDialogLandsOnTheEffortItShowedOverARealSession(t *testing.T) {
	isolateSkillsHome(t)
	cfg := nativeEffortConfig(t)
	sess := cfg.Session
	t.Cleanup(func() { _ = sess.Close() })
	tm, _ := New(cfg).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := applyPending(t, startedLikeInit(t, tm.(Model)))
	if !m.effortFacts() || m.snap.CurrentModel != "alpha/one" {
		t.Fatalf("the session: facts %v, on %q", m.effortFacts(), m.snap.CurrentModel)
	}
	two := nativeEffortTable().Models["alpha/two"]
	if err := modeltable.Remember(cfg.NativeDir, modeltable.RecentEntry{Alias: "alpha/two", Provider: two.Provider, WireModel: two.WireModel, Effort: "low"},
		time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	m = openDialog(t, m)
	m = highlight(t, m, "alpha/two")
	if got := effortRow(t, m); got != "  effort  minimal  low  medium  high  xhigh  [max]" {
		t.Fatalf("Alpha Two's row is %q", got)
	}
	tm, cmd := m.Update(enter())
	m = applyPending(t, flushCmd(t, tm.(Model), cmd))
	snap := sess.Snapshot()
	if opt := agent.EffortOption(snap); snap.CurrentModel != "alpha/two" || opt == nil || opt.Current != "max" {
		t.Fatalf("the session is on %s at %+v; want Alpha Two at the max the dialog showed", snap.CurrentModel, opt)
	}
	if got := texts(m, entryNote); !slices.Equal(got, []string{"model → alpha/two", "effort → max"}) {
		t.Fatalf("notes %q", got)
	}
}

// TestASwitchBackFromAPendingSwitchLandsOnTheShownEffort (branch review 1): a
// real native session started on Alpha One with --effort low, which the model
// memory remembers at high. /model alpha/two is issued and held; the dialog,
// reopened, still has Alpha One's catalog, so Alpha One's row shows the low it
// is at. Picked untouched, the apply switches back to it — the switch to Alpha
// Two lands first, and the one back reads the memory's high — and sends the
// low it showed after it, so Alpha One ends at low. Negative control: the
// old condition, which sends an untouched effort only for a model other than
// the catalog's, leaves Alpha One at high.
func TestASwitchBackFromAPendingSwitchLandsOnTheShownEffort(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	dir, getenv := connectFixture(t, nil)
	table := nativeEffortTable()
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	one := table.Models["alpha/one"]
	if err := modeltable.Remember(dir, modeltable.RecentEntry{Alias: "alpha/one", Provider: one.Provider, WireModel: one.WireModel, Effort: "high"}, at); err != nil {
		t.Fatal(err)
	}
	tweak := nativeSessionTweak(dir, table, &nativeScriptedModel{provider: "alpha", wire: "wire-one"})
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), Model: "alpha/one", Effort: "low", Interactive: true},
		func(o *harness.Options) {
			tweak(o)
			o.Getenv = nativePickerEnv
			o.Now = func() time.Time { return at }
		})
	t.Cleanup(func() { _ = sess.Close() })
	tm, _ := New(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true, NativeDir: dir, Getenv: getenv}).
		Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := applyPending(t, startedLikeInit(t, tm.(Model)))
	if opt := agent.EffortOption(m.snap); m.snap.CurrentModel != "alpha/one" || opt == nil || opt.Current != "low" {
		t.Fatalf("fixture: the session is on %s at %+v; want Alpha One at low", m.snap.CurrentModel, opt)
	}

	m, held := typeCommand(t, m, "/model alpha/two")
	if !m.modelPending() {
		t.Fatal("fixture: the switch to Alpha Two is not pending")
	}
	m = openDialog(t, m)
	m = highlight(t, m, "alpha/one")
	if got := effortRow(t, m); got != "  effort  [low]  high" {
		t.Fatalf("Alpha One's row, its switch away pending: %q", got)
	}
	m, applied := press(m, enter())
	// The dialog's chain runs behind the held switch, so running it runs the
	// switch first (chainLock); the switch's own command then answers.
	m = drainCmd(t, m, applied)
	m = applyPending(t, drainCmd(t, m, held))
	snap := sess.Snapshot()
	if opt := agent.EffortOption(snap); snap.CurrentModel != "alpha/one" || opt == nil || opt.Current != "low" {
		t.Fatalf("the session is on %s at %+v; want Alpha One at the low the dialog showed", snap.CurrentModel, opt)
	}
	if got := texts(m, entryNote); !slices.Equal(got, []string{"model → alpha/one", "effort → low"}) {
		t.Fatalf("notes %q", got)
	}
}
