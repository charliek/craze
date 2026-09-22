package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

// Plan 025 C2a: the model dialog's tabs come from the current model's catalog
// (design 4), and its apply chain binds every option step to the model the
// chain ends on, re-resolving the steps on the model it switched to (X3) and
// writing a note for each one it did not apply (X4).

// cursorCatalogs is cursor's per-model catalog in the Stub (plan 025 §1.1, the
// shapes F1's permodel fixtures carry): mode and model, then grok-4.6's effort
// and fast, composer-2.5's fast alone, claude-opus-5's thinking, context,
// effort and fast in the order cursor answers them, and glm-5.2's reasoning
// select, whose vocabulary is not grok's. The current values are chosen so a
// preselected value is never simply the first one, and each model's fast
// starts off so a step that turns it on can be seen to land.
func cursorCatalogs() map[string][]agent.ConfigOption {
	models := []string{"grok-4.6", "composer-2.5", "claude-opus-5", "glm-5.2"}
	values := func(vs ...string) []agent.SelectValue {
		out := make([]agent.SelectValue, len(vs))
		for i, v := range vs {
			out[i] = agent.SelectValue{Value: v, Name: strings.ToUpper(v[:1]) + v[1:]}
		}
		return out
	}
	offOn := func(on string) []agent.SelectValue {
		return []agent.SelectValue{{Value: "false", Name: "Off"}, {Value: "true", Name: on}}
	}
	head := func(model string) []agent.ConfigOption {
		return []agent.ConfigOption{
			{ID: "mode", Name: "Mode", Category: "mode", Type: "select", Current: "agent", SelectValues: values("agent", "plan", "ask")},
			{ID: "model", Name: "Model", Category: "model", Type: "select", Current: model, SelectValues: values(models...)},
		}
	}
	effort := func(cur string, vs ...string) agent.ConfigOption {
		return agent.ConfigOption{ID: "effort", Name: "Effort", Category: "thought_level", Type: "select", Current: cur, SelectValues: values(vs...)}
	}
	fast := agent.ConfigOption{ID: "fast", Name: "Fast", Category: "model_config", Type: "select", Current: "false", SelectValues: offOn("Fast")}
	return map[string][]agent.ConfigOption{
		"grok-4.6":     append(head("grok-4.6"), effort("medium", "low", "medium", "high", "xhigh"), fast),
		"composer-2.5": append(head("composer-2.5"), fast),
		"claude-opus-5": append(head("claude-opus-5"),
			agent.ConfigOption{ID: "thinking", Name: "Thinking", Category: "thought_level", Type: "select", Current: "true", SelectValues: offOn("On")},
			agent.ConfigOption{ID: "context", Name: "Context", Category: "model_config", Type: "select", Current: "1m", SelectValues: values("300k", "1m")},
			effort("high", "low", "medium", "high", "xhigh", "max"),
			fast,
		),
		"glm-5.2": append(head("glm-5.2"), agent.ConfigOption{
			ID: "reasoning", Name: "Reasoning", Category: "thought_level", Type: "select", Current: "max", SelectValues: values("high", "max"),
		}),
	}
}

// cursorStub is a sized model over a Stub carrying cursorCatalogs, on model
// current, with whatever the set-up published drained.
func cursorStub(t *testing.T, current string) (Model, *Stub) {
	t.Helper()
	m := sized(t)
	stub := m.sess.(*Stub)
	stub.mu.Lock()
	stub.snap.Models = []agent.ModelInfo{
		{ID: "grok-4.6", Name: "Grok 4.6"},
		{ID: "composer-2.5", Name: "Composer 2.5"},
		{ID: "claude-opus-5", Name: "Claude Opus 5"},
		{ID: "glm-5.2", Name: "GLM 5.2"},
	}
	stub.snap.CurrentModel = current
	stub.mu.Unlock()
	stub.SetModelCatalogs(cursorCatalogs())
	m.refreshSnap()
	stubDeltas(t, stub)
	return m, stub
}

func tabLabels(tabs []modelTab) string {
	out := make([]string, len(tabs))
	for i, t := range tabs {
		out[i] = t.label
	}
	return strings.Join(out, " ")
}

// openDialog is /model typed and entered.
func openDialog(t *testing.T, m Model) Model {
	t.Helper()
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogModel {
		t.Fatal("expected the model dialog")
	}
	return m
}

// applyDialog is Enter, with the chain run and its answer delivered.
func applyDialog(t *testing.T, m Model) Model {
	t.Helper()
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogNone {
		t.Fatal("enter closes the dialog optimistically")
	}
	return flushCmd(t, m, cmd)
}

// stubConfigCalls is how many SetConfig calls reached the Stub.
func stubConfigCalls(s *Stub) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configCalls
}

// optionCurrent is option id's value in a catalog, "" when it is not there.
func optionCurrent(cfg []agent.ConfigOption, id string) string {
	for _, o := range cfg {
		if o.ID == id {
			return o.Current
		}
	}
	return ""
}

// dialogRowIndex is the frame row that carries text inside the box, or -1.
func dialogRowIndex(m Model, text string) int {
	r := m.lay.Dialog
	lines := rows(plainView(m))
	for y := r.Y + 1; y < r.Y+r.H-1 && y < len(lines); y++ {
		if strings.Contains(lines[y], text) {
			return y
		}
	}
	return -1
}

// TestModelDialogHintsAreTodays: the footer is built from the tabs now, and
// over the catalogs craze has always drawn it is the four strings it always
// was — the two tabs, either one, and none.
func TestModelDialogHintsAreTodays(t *testing.T) {
	stub := NewStub().Snapshot().Config
	effort, fast := stub[:1], stub[1:]
	for _, tc := range []struct {
		name string
		cfg  []agent.ConfigOption
		want string
	}{
		{"effort and fast", stub, "type to filter · ↑↓ · tab effort/fast · enter · esc"},
		{"effort", effort, "type to filter · ↑↓ · tab effort · enter · esc"},
		{"fast", fast, "type to filter · ↑↓ · tab fast · enter · esc"},
		{"nothing", nil, "type to filter · ↑↓ · enter · esc"},
	} {
		if got := modelDialogHintText(catalogTabs(agent.Snapshot{Config: tc.cfg})); got != tc.want {
			t.Errorf("%s: the footer is %q, want %q", tc.name, got, tc.want)
		}
	}
	if modelDialogHint != "type to filter · ↑↓ · tab effort/fast · enter · esc" {
		t.Fatalf("modelDialogHint is %q", modelDialogHint)
	}
}

// TestModelDialogTabsComeFromTheCatalog is the tab list's rule: every select
// with a value to pick, in catalog order, each id once, and never the mode or
// the model — by category first, by id second. Effort and fast are named by
// role, whatever the agent calls them; every other tab is its advertised name,
// lowercased and folded onto one line.
func TestModelDialogTabsComeFromTheCatalog(t *testing.T) {
	sel := func(id, name, category string) agent.ConfigOption {
		return agent.ConfigOption{
			ID: id, Name: name, Category: category, Type: "select", Current: "a",
			SelectValues: []agent.SelectValue{{Value: "a"}, {Value: "b"}},
		}
	}
	cfg := []agent.ConfigOption{
		sel("mode", "Mode", "mode"),
		sel("picker", "Model", "model"), // the model by its category
		sel("model", "Model", ""),       // and by its id
		sel("session_mode", "Session", "mode"),
		sel("thinking", "Thinking", "thought_level"),
		sel("reasoning_effort", "Reasoning Effort", "thought_level"),
		sel("speed", "Fast Mode", "model_config"),
		sel("context", "Context\nWindow", "model_config"),
		sel("thinking", "Thinking again", "thought_level"), // an id twice
		{ID: "empty", Name: "Empty", Type: "select"},       // nothing to pick
		{ID: "flag", Name: "Flag", Type: "boolean", SelectValues: []agent.SelectValue{{Value: "x"}}},
		{ID: "", Name: "Nameless", Type: "select", SelectValues: []agent.SelectValue{{Value: "x"}}},
		{ID: "Untitled", Type: "select", SelectValues: []agent.SelectValue{{Value: "x"}}},
	}
	tabs := catalogTabs(agent.Snapshot{Config: cfg})
	if got, want := tabLabels(tabs), "thinking effort fast context window untitled"; got != want {
		t.Fatalf("the tabs are %q, want %q", got, want)
	}
	for i, id := range []string{"thinking", "reasoning_effort", "speed", "context", "Untitled"} {
		if tabs[i].opt.ID != id {
			t.Fatalf("tab %d sets %q, want %q", i, tabs[i].opt.ID, id)
		}
	}
	if tabs[1].role != roleEffort || tabs[2].role != roleFast || tabs[0].role != roleOther {
		t.Fatalf("the roles are %v %v %v", tabs[0].role, tabs[1].role, tabs[2].role)
	}
}

// TestAnAdvertisedTabIsShownWhateverTheCapabilityBit is the rule written down
// in catalogTabs (plan 025 design 4, CodeRabbit 11): grok's provider has no
// FastToggle, and a fast option its catalog advertises is still a tab the
// dialog draws and applies — while the chip, which the bit does gate, still
// says nothing about it.
func TestAnAdvertisedTabIsShownWhateverTheCapabilityBit(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	stub.SetProvider(agent.GrokProvider())
	m.refreshSnap()
	if m.caps().FastToggle {
		t.Fatal("grok's provider has no fast toggle")
	}
	m = openDialog(t, m)
	view := plainView(m)
	if !strings.Contains(view, "  fast  [off]  on") || !strings.Contains(view, modelDialogHint) {
		t.Fatalf("the advertised fast option should be a tab:\n%s", view)
	}
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight)
	m = applyDialog(t, m)
	if !agent.FastOn(stub.Snapshot()) {
		t.Fatalf("fast was not applied: %+v", stub.Snapshot().Config)
	}
	if got := texts(m, entryNote); len(got) != 1 || got[0] != "fast → on" {
		t.Fatalf("notes %v", got)
	}
	if got := m.modelLabel(); got != "Grok (medium)" {
		t.Fatalf("the chip is gated by the bit and reads %q", got)
	}
}

// TestFitModelDialogDropsRowsInThePinnedOrder: the list shrinks first, then
// the footer, then the filter, and the tabs last, from the last to the first.
func TestFitModelDialogDropsRowsInThePinnedOrder(t *testing.T) {
	// Four tabs under a two-model list: 1 title + 1 filter + 2 + 4 + 1 footer.
	for _, tc := range []struct {
		budget int
		want   modelDialogRows
	}{
		{9, modelDialogRows{list: 2, footer: true, filter: true, tabs: 4}},
		{8, modelDialogRows{list: 1, footer: true, filter: true, tabs: 4}},
		{7, modelDialogRows{list: 0, footer: true, filter: true, tabs: 4}},
		{6, modelDialogRows{list: 0, footer: false, filter: true, tabs: 4}},
		{5, modelDialogRows{list: 0, footer: false, filter: false, tabs: 4}},
		{4, modelDialogRows{tabs: 3}},
		{3, modelDialogRows{tabs: 2}},
		{2, modelDialogRows{tabs: 1}},
		{1, modelDialogRows{tabs: 0}},
		{0, modelDialogRows{tabs: 0}},
	} {
		got := fitModelDialog(tc.budget, 2, 4)
		if got != tc.want {
			t.Errorf("budget %d: %+v, want %+v", tc.budget, got, tc.want)
		}
		if tc.budget > 0 && got.total() > tc.budget {
			t.Errorf("budget %d: %d rows drawn", tc.budget, got.total())
		}
	}
}

// TestTheDialogsTabsFollowTheModel is design 4's acceptance
// (TestTheDialogsTabsFollowTheModel in plan 025 §4): the tabs are the current
// model's, so a model switched to in the dialog is reopened onto exactly its
// own — fast alone on composer-2.5, and on claude-opus-5 its four in the order
// cursor answers them, each on the value the model holds.
func TestTheDialogsTabsFollowTheModel(t *testing.T) {
	m, _ := cursorStub(t, "grok-4.6")
	m = openDialog(t, m)
	if got := tabLabels(m.modelDialogTabs()); got != "effort fast" {
		t.Fatalf("on grok-4.6 the tabs are %q", got)
	}
	view := plainView(m)
	for _, want := range []string{"  effort  low  [medium]  high  xhigh", "  fast  [off]  on", modelDialogHint} {
		if !strings.Contains(view, want) {
			t.Fatalf("on grok-4.6 the box is missing %q:\n%s", want, view)
		}
	}
	// A mode or model tab would draw its current value bracketed.
	for _, no := range []string{"[agent]", "[grok-4.6]"} {
		if strings.Contains(view, no) {
			t.Fatalf("the mode and model options are never tabs, found %q:\n%s", no, view)
		}
	}

	m = typeInto(t, m, "composer")
	m = applyDialog(t, m)
	if m.snap.CurrentModel != "composer-2.5" {
		t.Fatalf("the switch left the screen on %q", m.snap.CurrentModel)
	}
	m = openDialog(t, m)
	if got := tabLabels(m.modelDialogTabs()); got != "fast" {
		t.Fatalf("on composer-2.5 the tabs are %q", got)
	}
	view = plainView(m)
	if strings.Contains(view, "effort") || !strings.Contains(view, "  fast  [off]  on") ||
		!strings.Contains(view, "tab fast · enter") {
		t.Fatalf("composer-2.5 offers fast alone:\n%s", view)
	}

	m = typeInto(t, m, "claude")
	m = applyDialog(t, m)
	m = openDialog(t, m)
	if got := tabLabels(m.modelDialogTabs()); got != "thinking context effort fast" {
		t.Fatalf("on claude-opus-5 the tabs are %q", got)
	}
	prev := -1
	for _, want := range []string{
		"  thinking  false  [true]",
		"  context  300k  [1m]",
		"  effort  low  medium  [high]  xhigh  max",
		"  fast  [off]  on",
	} {
		y := dialogRowIndex(m, want)
		if y <= prev {
			t.Fatalf("row %q is at %d, after %d:\n%s", want, y, prev, plainView(m))
		}
		prev = y
	}
	// Tab reaches them in that order.
	for _, id := range []string{"thinking", "context", "effort", "fast", ""} {
		m = pressKey(t, m, tea.KeyTab)
		if m.mdlg.focus != dialogFocus(id) {
			t.Fatalf("tab reached %q, want %q", m.mdlg.focus, id)
		}
	}
}

// TestTheDialogReResolvesItsStepsOnTheModelItSwitchedTo is X3: model, effort
// and fast in one dialog, where each option step is judged, once the model
// step has landed, against the catalog the session installed for the model it
// switched to — effort and fast by role, the value only if that model offers
// it — and a step it cannot take is a note, never an error, and never sent.
func TestTheDialogReResolvesItsStepsOnTheModelItSwitchedTo(t *testing.T) {
	for _, tc := range []struct {
		name, filter string
		effort, fast int // presses of → (or ← when negative) on each tab
		installAs    func(id, value string) string
		wantNotes    []string
		check        func(t *testing.T, m Model, snap agent.Snapshot)
	}{
		{
			name: "the destination lacks effort", filter: "composer", effort: 1, fast: 1,
			wantNotes: []string{"model → composer-2.5", "effort not applied: composer-2.5 has no effort", "fast → on"},
			check: func(t *testing.T, m Model, snap agent.Snapshot) {
				if !agent.FastOn(snap) {
					t.Fatalf("fast should have landed on composer-2.5: %+v", snap.Config)
				}
			},
		},
		{
			name: "the destination has another vocabulary", filter: "glm", effort: -1, fast: 1,
			wantNotes: []string{
				"model → glm-5.2",
				"effort not applied: glm-5.2 does not offer low",
				"fast not applied: glm-5.2 has no fast",
			},
			check: func(t *testing.T, m Model, snap agent.Snapshot) {
				if got := optionCurrent(snap.Config, "reasoning"); got != "max" {
					t.Fatalf("glm-5.2's reasoning moved to %q", got)
				}
			},
		},
		{
			// Effort by role: grok's option is `effort`, glm's is `reasoning`,
			// and the value both offer is sent to glm's own id.
			name: "the destination's effort has another id", filter: "glm", effort: 1,
			wantNotes: []string{"model → glm-5.2", "effort → high"},
			check: func(t *testing.T, m Model, snap agent.Snapshot) {
				if got := optionCurrent(snap.Config, "reasoning"); got != "high" {
					t.Fatalf("glm-5.2's reasoning is %q, want high", got)
				}
			},
		},
		{
			// Both applied, and what the rows and the notes show is what the
			// agent installed: this agent answers low with medium.
			name: "the destination has both", filter: "claude", effort: -1, fast: 1,
			installAs: func(id, value string) string {
				if id == "effort" && value == "low" {
					return "medium"
				}
				return value
			},
			wantNotes: []string{"model → claude-opus-5", "effort → medium", "fast → on"},
			check: func(t *testing.T, m Model, snap agent.Snapshot) {
				if got := optionCurrent(snap.Config, "effort"); got != "medium" {
					t.Fatalf("the session installed %q", got)
				}
				if got := agent.EffortOption(m.snap); got == nil || got.Current != "medium" {
					t.Fatalf("the rows show %+v, want the installed medium", got)
				}
				if got := m.modelLabel(); got != "Claude Opus 5 (medium · fast)" {
					t.Fatalf("the status row reads %q", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := cursorStub(t, "grok-4.6")
			stub.InstallConfigAs(tc.installAs)
			m = openDialog(t, m)
			m = typeInto(t, m, tc.filter)
			for _, presses := range []int{tc.effort, tc.fast} {
				m = pressKey(t, m, tea.KeyTab)
				key, n := tea.KeyRight, presses
				if n < 0 {
					key, n = tea.KeyLeft, -n
				}
				for range n {
					m = pressKey(t, m, key)
				}
			}
			m = applyDialog(t, m)
			if got := texts(m, entryNote); strings.Join(got, "|") != strings.Join(tc.wantNotes, "|") {
				t.Fatalf("notes %q, want %q", got, tc.wantNotes)
			}
			if errs := texts(m, entryError); len(errs) != 0 {
				t.Fatalf("a step that was not applied is a note, never an error row: %q", errs)
			}
			tc.check(t, m, stub.Snapshot())
		})
	}
}

// TestAnotherClientsModelChangeMidChainIsANote is astra 4's schedule over the
// dialog: another client's model change lands after this dialog's steps were
// chosen and before its option steps run. Every option step then says it was
// not applied because the model changed — a note, not an error row — and none
// reaches the agent, not even fast, which the other model has too under the
// same id.
func TestAnotherClientsModelChangeMidChainIsANote(t *testing.T) {
	for _, tc := range []struct {
		name, filter string
		arm          func(*Stub)
		wantNotes    []string
	}{
		{
			// No model step: the option steps are bound to grok-4.6, and the
			// engine's worker is the only thing that can see the change.
			name: "with no model step, the engine refuses it",
			arm: func(s *Stub) {
				if _, err := s.SetModel(context.Background(), stubOtherClient, "composer-2.5"); err != nil {
					t.Fatal(err)
				}
			},
			wantNotes: []string{"effort not applied: the model changed", "fast not applied: the model changed"},
		},
		{
			// The chain reads claude-opus-5's catalog and re-resolves both
			// steps against it; the move lands straight after that read, so
			// the worker's ForModel check is what refuses the first of them.
			name: "after the chain has read the destination, the engine refuses it", filter: "claude",
			arm:       func(s *Stub) { s.MoveModelOnRead("claude-opus-5", "composer-2.5") },
			wantNotes: []string{"model → claude-opus-5", "effort not applied: the model changed", "fast not applied: the model changed"},
		},
		{
			// The move lands with the model step's own answer, so the chain's
			// read of the destination already finds the other model and sends
			// nothing.
			name: "before the chain reads the destination, the chain sees it", filter: "claude",
			arm:       func(s *Stub) { s.MoveModelAfterNextSetModel("composer-2.5") },
			wantNotes: []string{"model → claude-opus-5", "effort not applied: the model changed", "fast not applied: the model changed"},
		},
	} {
		// Whichever of the chain's answer and the session's deltas reaches
		// the model first, the screen ends on the other client's model.
		for _, deltasFirst := range []bool{false, true} {
			order := "the answer, then the deltas"
			if deltasFirst {
				order = "the deltas, then the answer"
			}
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				m, stub := cursorStub(t, "grok-4.6")
				m = openDialog(t, m)
				m = typeInto(t, m, tc.filter)
				m = pressKey(t, m, tea.KeyTab)
				m = pressKey(t, m, tea.KeyLeft) // effort → low
				m = pressKey(t, m, tea.KeyTab)
				m = pressKey(t, m, tea.KeyRight) // fast → on
				tc.arm(stub)
				tm, cmd := m.Update(enter())
				m = tm.(Model)
				applied, ok := runCmd(cmd).(modelApplyMsg)
				if !ok || applied.err != nil {
					t.Fatalf("the chain came back %+v", applied)
				}
				evs := stubDeltas(t, stub)
				if deltasFirst {
					m = feed(t, m, evs...)
					m = deliver(t, m, applied)
				} else {
					m = deliver(t, m, applied)
					m = feed(t, m, evs...)
				}

				if got := texts(m, entryNote); strings.Join(got, "|") != strings.Join(tc.wantNotes, "|") {
					t.Fatalf("notes %q, want %q", got, tc.wantNotes)
				}
				if errs := texts(m, entryError); len(errs) != 0 {
					t.Fatalf("a stale step is a note, never an error row: %q", errs)
				}
				if n := stubConfigCalls(stub); n != 0 {
					t.Fatalf("%d option changes reached the agent", n)
				}
				snap := stub.Snapshot()
				if snap.CurrentModel != "composer-2.5" || agent.FastOn(snap) {
					t.Fatalf("the other client's model is %q with %+v, want composer-2.5 untouched", snap.CurrentModel, snap.Config)
				}
				if m.snap.CurrentModel != "composer-2.5" || agent.FastOn(m.snap) || agent.EffortOption(m.snap) != nil {
					t.Fatalf("the rows show %q %+v, want composer-2.5's own", m.snap.CurrentModel, m.snap.Config)
				}
				if got := m.modelLabel(); got != "Composer 2.5" {
					t.Fatalf("the status row reads %q", got)
				}
			})
		}
	}
}

// TestAnOptionGoneIsANoteAndTheChainGoesOn: the agent takes a step, and the
// catalog its answer carries no longer lists the option (agent.ErrOptionGone).
// That step is a note naming the model, and the steps after it still run.
func TestAnOptionGoneIsANoteAndTheChainGoesOn(t *testing.T) {
	m, stub := cursorStub(t, "claude-opus-5")
	m = openDialog(t, m)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyLeft) // context → 300k
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight) // fast → on
	stub.DropOptionOnSet("context")
	m = applyDialog(t, m)
	want := []string{"context not applied: claude-opus-5 has no context", "fast → on"}
	if got := texts(m, entryNote); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("notes %q, want %q", got, want)
	}
	if errs := texts(m, entryError); len(errs) != 0 {
		t.Fatalf("errors %q", errs)
	}
	if !agent.FastOn(stub.Snapshot()) || !agent.FastOn(m.snap) {
		t.Fatalf("the step after the gone one did not land: %+v", stub.Snapshot().Config)
	}
	if optionCurrent(m.snap.Config, "context") != "" {
		t.Fatalf("the rows still show the option the agent dropped: %+v", m.snap.Config)
	}
}

// TestTheFocusedTabsOptionGoingHandsTheFocusBack is the focus repair: a delta
// arrives while the box is open and takes the focused tab's option away, so
// the focus goes back to the list and stays there; a chosen value the new
// catalog does not offer falls back to its option's current one; and a tab the
// delta brings is seeded from its option's value.
func TestTheFocusedTabsOptionGoingHandsTheFocusBack(t *testing.T) {
	m, stub := cursorStub(t, "claude-opus-5")
	m = openDialog(t, m)
	for range 3 {
		m = pressKey(t, m, tea.KeyTab) // thinking, context, effort
	}
	m = pressKey(t, m, tea.KeyRight)
	m = pressKey(t, m, tea.KeyRight) // effort → max, which grok-4.6 does not offer
	m = pressKey(t, m, tea.KeyShiftTab)
	if m.mdlg.focus != "context" || m.mdlg.chosen["effort"] != "max" {
		t.Fatalf("focus %q, effort %q", m.mdlg.focus, m.mdlg.chosen["effort"])
	}

	// Another client moves the session to grok-4.6: no context there.
	if _, err := stub.SetModel(context.Background(), stubOtherClient, "grok-4.6"); err != nil {
		t.Fatal(err)
	}
	m = feed(t, m, stubDeltas(t, stub)...)
	if m.dialog != dialogModel {
		t.Fatal("a delta does not close the box")
	}
	if m.mdlg.focus != focusList {
		t.Fatalf("the focus stayed on %q, whose option has gone", m.mdlg.focus)
	}
	if got := m.mdlg.chosen["effort"]; got != "medium" {
		t.Fatalf("effort's choice is %q, want grok-4.6's current medium", got)
	}
	if marked := markedDialogRows(m); len(marked) != 1 || !strings.HasPrefix(marked[0], "Grok 4.6") {
		t.Fatalf("the list should carry the one cursor mark: %q\n%s", marked, plainView(m))
	}
	view := plainView(m)
	if !strings.Contains(view, "  effort  low  [medium]  high  xhigh") || !strings.Contains(view, modelDialogHint) {
		t.Fatalf("the box should be grok-4.6's:\n%s", view)
	}
	m = pressKey(t, m, tea.KeyTab)
	if m.mdlg.focus != "effort" {
		t.Fatalf("tab from the list reached %q", m.mdlg.focus)
	}

	// Back to claude-opus-5 by another client: the focus that was repaired
	// stays where the user now has it, and every tab is drawn on its option's
	// value — effort too, whose max the repair above took back, so it is no
	// choice of the user's any more and follows the model's own high.
	m = pressKey(t, m, tea.KeyShiftTab)
	if _, err := stub.SetModel(context.Background(), stubOtherClient, "claude-opus-5"); err != nil {
		t.Fatal(err)
	}
	m = feed(t, m, stubDeltas(t, stub)...)
	if m.mdlg.focus != focusList {
		t.Fatalf("focus %q", m.mdlg.focus)
	}
	view = plainView(m)
	for _, want := range []string{"  thinking  false  [true]", "  context  300k  [1m]", "  effort  low  medium  [high]  xhigh  max"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the box is missing %q:\n%s", want, view)
		}
	}
}

// TestATabThatAppearsWhileOpenIsSeeded: a box opened on composer-2.5 has one
// tab; another client's switch to claude-opus-5 brings four, each drawn and
// reached on its option's own value, and Enter with nothing moved sends none.
func TestATabThatAppearsWhileOpenIsSeeded(t *testing.T) {
	m, stub := cursorStub(t, "composer-2.5")
	m = openDialog(t, m)
	if got := tabLabels(m.modelDialogTabs()); got != "fast" {
		t.Fatalf("tabs %q", got)
	}
	if _, err := stub.SetModel(context.Background(), stubOtherClient, "claude-opus-5"); err != nil {
		t.Fatal(err)
	}
	m = feed(t, m, stubDeltas(t, stub)...)
	for id, want := range map[string]string{"thinking": "true", "context": "1m", "effort": "high", "fast": "false"} {
		if got := m.mdlg.chosen[id]; got != want {
			t.Fatalf("%s is seeded with %q, want %q", id, got, want)
		}
	}
	// The list's selection is the current model now, so Enter changes nothing.
	m = typeInto(t, m, "claude")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd != nil || stubConfigCalls(stub) != 0 {
		t.Fatal("nothing was moved, so nothing is sent")
	}
	if got := texts(m, entryNote); len(got) != 0 {
		t.Fatalf("notes %q", got)
	}
}

// TestClicksLandOnTheFourTabs is the hit test over dynamic tabs: each tab row
// takes the focus when clicked, the footer is no option, and a list row still
// picks its model — every row counted off the plan the renderer drew.
func TestClicksLandOnTheFourTabs(t *testing.T) {
	m, _ := cursorStub(t, "claude-opus-5")
	m = openDialog(t, m)
	r := m.lay.Dialog
	for _, tc := range []struct{ row, id string }{
		{"  thinking  false", "thinking"},
		{"  context  300k", "context"},
		{"  effort  low", "effort"},
		{"  fast  [off]", "fast"},
	} {
		y := dialogRowIndex(m, tc.row)
		if y < 0 {
			t.Fatalf("no %q row:\n%s", tc.row, plainView(m))
		}
		got := clickXY(t, m, r.X+4, y)
		if got.dialog != dialogModel || got.mdlg.focus != dialogFocus(tc.id) {
			t.Fatalf("clicking %q left dialog=%v focus=%q", tc.row, got.dialog, got.mdlg.focus)
		}
		if marked := markedDialogRows(got); len(marked) != 1 || !strings.HasPrefix(marked[0], strings.TrimSpace(tc.row)) {
			t.Fatalf("clicking %q marked %q", tc.row, marked)
		}
	}
	if got := clickXY(t, m, r.X+4, r.Y+r.H-2); got.dialog != dialogModel || got.mdlg.focus != focusList {
		t.Fatalf("the footer is no option: dialog=%v focus=%q", got.dialog, got.mdlg.focus)
	}
	y := dialogRowIndex(m, "Composer 2.5")
	got := clickXY(t, m, r.X+4, y)
	if got.dialog != dialogNone || got.snap.CurrentModel != "composer-2.5" {
		t.Fatalf("a list row picks its model: dialog=%v model=%q", got.dialog, got.snap.CurrentModel)
	}
}

// TestTheFourTabDialogAt40x12 is the bounded height with four tabs: at the
// smallest frame craze draws the box drops the list, the footer and the
// filter, in that order, and keeps the four tabs; every row fits its width;
// and a click lands only on a row that was drawn.
func TestTheFourTabDialogAt40x12(t *testing.T) {
	m, _ := cursorStub(t, "claude-opus-5")
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = tm.(Model)
	m = openDialog(t, m)
	view := plainView(m)
	lines := rows(view)
	if len(lines) != 12 {
		t.Fatalf("the frame is %d rows:\n%s", len(lines), view)
	}
	for _, ln := range lines {
		if w := lipgloss.Width(ln); w > 40 {
			t.Fatalf("a row is %d wide:\n%s", w, view)
		}
	}
	r := m.lay.Dialog
	if h := lipgloss.Height(m.dialogView(r)); h != r.H {
		t.Fatalf("the box drew %d rows into a %d-row rectangle", h, r.H)
	}
	p := m.modelDialogPlan(r.H - dialogBorder)
	if p.rows.list != 0 || p.rows.footer || p.rows.filter || p.rows.tabs != 4 {
		t.Fatalf("at 40x12 the plan is %+v, want the title and the four tabs", p.rows)
	}
	for i, want := range []string{"model", "  thinking", "  context", "  effort", "  fast"} {
		if !strings.Contains(lines[r.Y+1+i], want) {
			t.Fatalf("box row %d should be %q:\n%s", i, want, view)
		}
	}
	for i, id := range []string{"thinking", "context", "effort", "fast"} {
		got := clickXY(t, m, r.X+4, r.Y+2+i)
		if got.dialog != dialogModel || got.mdlg.focus != dialogFocus(id) {
			t.Fatalf("clicking tab row %d left dialog=%v focus=%q, want %q", i, got.dialog, got.mdlg.focus, id)
		}
	}
	if got := clickXY(t, m, r.X+4, r.Y+1); got.dialog != dialogModel || got.mdlg.focus != focusList ||
		got.snap.CurrentModel != "claude-opus-5" {
		t.Fatalf("the title is no option: dialog=%v focus=%q model=%q", got.dialog, got.mdlg.focus, got.snap.CurrentModel)
	}
}
