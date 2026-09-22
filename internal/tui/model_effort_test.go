package tui

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// Plan 025 C2b: `/model <id> <effort>` resolves the model first and judges the
// effort against the catalog of the model it switched to (design 5), with the
// X4 notes for an effort that was not applied; and the model dialog's two
// follow-ups (X10 (f)): only a tab the user moved is sent, and a footer whose
// tab names do not fit says "tab options".

// configWrites records every option change the Stub installs, as id=value, in
// order — what reached the "agent" — and answers with what it recorded so far.
// SetConfig runs on the engine's worker, so the record is guarded.
func configWrites(stub *Stub) func() []string {
	var mu sync.Mutex
	var got []string
	stub.InstallConfigAs(func(id, value string) string {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, id+"="+value)
		return value
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// runSlashModel types line and presses Enter, runs the command /model returned,
// and applies its answer and the session's deltas in the order asked for:
// which of the two reaches the model first is a race in the program.
func runSlashModel(t *testing.T, m Model, stub *Stub, line string, deltasFirst bool) Model {
	t.Helper()
	m.input.SetValue(line)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	msg := runCmd(cmd)
	if msg == nil {
		t.Fatalf("%s came back with no answer; errors %q", line, texts(m, entryError))
	}
	evs := stubDeltas(t, stub)
	if deltasFirst {
		m = feed(t, m, evs...)
		m = deliver(t, m, msg)
	} else {
		m = deliver(t, m, msg)
		m = feed(t, m, evs...)
	}
	return m
}

// answerOrders runs f once per order of a command's answer and the session's
// deltas, as a subtest each.
func answerOrders(t *testing.T, f func(t *testing.T, deltasFirst bool)) {
	for _, deltasFirst := range []bool{false, true} {
		name := "the answer, then the deltas"
		if deltasFirst {
			name = "the deltas, then the answer"
		}
		t.Run(name, func(t *testing.T) { f(t, deltasFirst) })
	}
}

// TestModelEffortIsJudgedOnTheModelItSwitchesTo is design 5's four cases over
// cursor's per-model catalog, plus the model left as it is: the model is read
// off the model list, and the effort is judged against the catalog the session
// installed for the model it landed on — by role, whatever that model calls its
// effort, and in its own spelling — then sent bound to that model. An effort
// that model cannot take is a note, never an error row, and nothing is sent.
func TestModelEffortIsJudgedOnTheModelItSwitchesTo(t *testing.T) {
	for _, tc := range []struct {
		name, from, line string
		wantModel        string
		wantWrites       []string
		wantNotes        []string
		wantLabel        string
		check            func(t *testing.T, snap agent.Snapshot)
	}{
		{
			// Before design 5 this was the unknown model "grok-4.6 high":
			// composer-2.5 has no effort to recognise the suffix by.
			name: "absent to present", from: "composer-2.5", line: "/model grok-4.6 high",
			wantModel: "grok-4.6", wantWrites: []string{"effort=high"}, wantLabel: "Grok 4.6 (high)",
			check: func(t *testing.T, snap agent.Snapshot) {
				if got := optionCurrent(snap.Config, "effort"); got != "high" {
					t.Fatalf("grok-4.6's effort is %q, want high", got)
				}
			},
		},
		{
			name: "present to absent", from: "grok-4.6", line: "/model composer-2.5 high",
			wantModel: "composer-2.5",
			wantNotes: []string{"effort not applied: composer-2.5 has no effort"}, wantLabel: "Composer 2.5",
		},
		{
			// grok-4.6's effort is `effort`, glm-5.2's is `reasoning`: the
			// value goes to the destination's own id.
			name: "the effort has another id", from: "grok-4.6", line: "/model glm-5.2 max",
			wantModel: "glm-5.2", wantWrites: []string{"reasoning=max"}, wantLabel: "GLM 5.2 (max)",
		},
		{
			name: "the effort has another vocabulary", from: "grok-4.6", line: "/model glm-5.2 low",
			wantModel: "glm-5.2",
			wantNotes: []string{"effort not applied: glm-5.2 does not offer low"}, wantLabel: "GLM 5.2 (max)",
			check: func(t *testing.T, snap agent.Snapshot) {
				if got := optionCurrent(snap.Config, "reasoning"); got != "max" {
					t.Fatalf("glm-5.2's reasoning moved to %q", got)
				}
			},
		},
		{
			// The model named is the one the session is on: that is the
			// destination, and the word is sent as the agent spells it.
			name: "the model left as it is", from: "grok-4.6", line: "/model grok-4.6 HIGH",
			wantModel: "grok-4.6", wantWrites: []string{"effort=high"}, wantLabel: "Grok 4.6 (high)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answerOrders(t, func(t *testing.T, deltasFirst bool) {
				m, stub := cursorStub(t, tc.from)
				writes := configWrites(stub)
				m = runSlashModel(t, m, stub, tc.line, deltasFirst)

				if got := writes(); strings.Join(got, "|") != strings.Join(tc.wantWrites, "|") {
					t.Fatalf("the agent was sent %q, want %q", got, tc.wantWrites)
				}
				if got := texts(m, entryNote); strings.Join(got, "|") != strings.Join(tc.wantNotes, "|") {
					t.Fatalf("notes %q, want %q", got, tc.wantNotes)
				}
				if errs := texts(m, entryError); len(errs) != 0 {
					t.Fatalf("an effort that was not applied is a note, never an error row: %q", errs)
				}
				snap := stub.Snapshot()
				if snap.CurrentModel != tc.wantModel {
					t.Fatalf("the session is on %q, want %q", snap.CurrentModel, tc.wantModel)
				}
				if m.snap.CurrentModel != tc.wantModel || m.model != tc.wantModel {
					t.Fatalf("the screen ended on %q / %q", m.snap.CurrentModel, m.model)
				}
				if got := m.modelLabel(); got != tc.wantLabel {
					t.Fatalf("the status row reads %q, want %q", got, tc.wantLabel)
				}
				if tc.check != nil {
					tc.check(t, snap)
				}
			})
		})
	}
}

// TestAnotherClientsModelChangeBeforeTheEffortIsANote is astra 4's schedule
// over `/model <id> <effort>`: another client's model change lands between the
// model step and the effort step. The effort is then not applied — to the model
// it was chosen for, which the session has left, nor to the one it is on now,
// even where that one would take it — and the note says why.
func TestAnotherClientsModelChangeBeforeTheEffortIsANote(t *testing.T) {
	for _, tc := range []struct {
		name  string
		arm   func(*Stub)
		other string
		label string
	}{
		{
			// The step reads grok-4.6's catalog, which offers low, and the move
			// lands straight after that read: the engine's ForModel check is
			// what refuses the change — claude-opus-5 has an `effort` that
			// offers low, so without the binding it would have taken it.
			name:  "after the step has read the destination, the engine refuses it",
			arm:   func(s *Stub) { s.MoveModelOnRead("grok-4.6", "claude-opus-5") },
			other: "claude-opus-5", label: "Claude Opus 5 (high)",
		},
		{
			// The move lands with the model step's own answer, so the step's
			// read finds composer-2.5, which has no effort. It says the model
			// changed, not that grok-4.6 has no effort, which is untrue.
			name:  "before the step reads the destination, the step sees it",
			arm:   func(s *Stub) { s.MoveModelAfterNextSetModel("composer-2.5") },
			other: "composer-2.5", label: "Composer 2.5",
		},
		{
			// astra r4 item 1: the agent's push lands before the setter reads
			// its outcome, so the model step itself confirms claude-opus-5 —
			// whose `effort` offers low. The effort is bound to grok-4.6, the
			// model the command names, so it is stale and is not sent to a
			// model nobody chose.
			name:  "before the setter reads its outcome, the command sees it",
			arm:   func(s *Stub) { s.MoveModelBeforeNextSetModelAnswers("claude-opus-5") },
			other: "claude-opus-5", label: "Claude Opus 5 (high)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answerOrders(t, func(t *testing.T, deltasFirst bool) {
				m, stub := cursorStub(t, "composer-2.5")
				writes := configWrites(stub)
				tc.arm(stub)
				m = runSlashModel(t, m, stub, "/model grok-4.6 low", deltasFirst)

				want := []string{"effort not applied: the model changed"}
				if got := texts(m, entryNote); strings.Join(got, "|") != strings.Join(want, "|") {
					t.Fatalf("notes %q, want %q", got, want)
				}
				if errs := texts(m, entryError); len(errs) != 0 {
					t.Fatalf("a stale effort is a note, never an error row: %q", errs)
				}
				if got := writes(); len(got) != 0 {
					t.Fatalf("the agent was sent %q", got)
				}
				snap := stub.Snapshot()
				if snap.CurrentModel != tc.other {
					t.Fatalf("the session is on %q, want the other client's %q", snap.CurrentModel, tc.other)
				}
				if tc.other == "claude-opus-5" {
					if got := optionCurrent(snap.Config, "effort"); got != "high" {
						t.Fatalf("the other model's effort moved to %q", got)
					}
				}
				if m.snap.CurrentModel != tc.other {
					t.Fatalf("the screen ended on %q", m.snap.CurrentModel)
				}
				if got := m.modelLabel(); got != tc.label {
					t.Fatalf("the status row reads %q, want %q", got, tc.label)
				}
			})
		})
	}
}

// TestARefusedModelWithAnEffortIsTodaysRevert: a model the session refuses is
// revertModelMsg exactly as it always was — the effort is never judged, never
// sent, and never noted, and the screen goes back to the model it left.
func TestARefusedModelWithAnEffortIsTodaysRevert(t *testing.T) {
	m, stub := perModelStub(t)
	stub.FailNextSetModel()
	prevRev := m.modelRev
	m.input.SetValue("/model fast high")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.snap.CurrentModel != "fast" {
		t.Fatalf("the optimistic value is %q", m.snap.CurrentModel)
	}
	refusal, ok := runCmd(cmd).(revertModelMsg)
	if !ok || refusal.prev != "grok" || refusal.at != prevRev || refusal.err == nil {
		t.Fatalf("the refused /model answered %+v", refusal)
	}
	m = deliver(t, m, refusal)
	if n := stubConfigCalls(stub); n != 0 {
		t.Fatalf("%d option changes reached the agent", n)
	}
	if m.snap.CurrentModel != "grok" || m.model != "grok" {
		t.Fatalf("the screen is on %q / %q, want the model it left", m.snap.CurrentModel, m.model)
	}
	if len(texts(m, entryError)) != 1 || len(texts(m, entryNote)) != 0 {
		t.Fatalf("errors %q, notes %q: want the refusal alone", texts(m, entryError), texts(m, entryNote))
	}
}

// TestTheShorthandIsTheCapabilityBit is design 4's gate on the shorthand, as
// design 5 leaves it: with the provider's Effort bit the model is resolved
// first and the last word is a candidate; without it the whole argument is the
// model's name, never split — whatever the current catalog advertises, since
// that is the model being left. Every provider craze ships has the bit.
func TestTheShorthandIsTheCapabilityBit(t *testing.T) {
	m, _ := cursorStub(t, "composer-2.5")
	if !m.effortShorthand() {
		t.Fatal("cursor has efforts, so the shorthand is on")
	}
	for _, p := range []agent.Provider{agent.GrokProvider(), agent.GxProvider(), agent.NativeProvider()} {
		if !p.Capabilities().Effort {
			t.Fatalf("%s has no Effort bit", p.Name())
		}
	}
	if agent.EffortOption(m.snap) != nil {
		t.Fatal("composer-2.5 has no effort: the gate must not need one")
	}
	id, effort, err := resolveModelArgs(m.snap, "grok-4.6 high", true)
	if err != nil || id != "grok-4.6" || effort != "high" {
		t.Fatalf("on: %q %q %v", id, effort, err)
	}
	id, effort, err = resolveModelArgs(m.snap, " grok-4.6 ", false)
	if err != nil || id != "grok-4.6" || effort != "" {
		t.Fatalf("off, a model alone: %q %q %v", id, effort, err)
	}
	if _, _, err := resolveModelArgs(m.snap, "grok-4.6 high", false); err == nil ||
		err.Error() != `unknown model "grok-4.6 high"` {
		t.Fatalf("off, the whole argument is the model's name: %v", err)
	}
}

// TestModelEffortOverTheLiveSession is design 5 over a real session and F1's
// permodel script, cursor's wire: every switch is set_config_option(model, X),
// whose reply installs X's catalog, and the effort is judged against it. The
// session starts on grok-4.6 (effort high, fast on); the four cases run in a
// row, each from where the last one left it.
func TestModelEffortOverTheLiveSession(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := t.TempDir()
	sess := agent.New(agent.Options{
		Binary:    bin,
		ExtraArgs: []string{"-script=permodel"},
		Workspace: ws,
		Force:     true,
		Stderr:    io.Discard,
	})
	if err := sess.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "grok-4.6"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	if got := m.modelLabel(); got != "Grok 4.6 (high · fast)" {
		t.Fatalf("the session started as %q", got)
	}

	var notes []string
	for _, step := range []struct {
		line, model, effortID, effort, label, note string
	}{
		// present → absent: nothing is sent to composer-2.5 — cursor would
		// refuse an effort it lacks with "Unknown model config option", which
		// would be an error row.
		{"/model composer-2.5 high", "composer-2.5", "", "", "Composer 2.5", "effort not applied: composer-2.5 has no effort"},
		// absent → present: composer-2.5 has no effort to read the word by.
		{"/model grok-4.6 xhigh", "grok-4.6", "effort", "xhigh", "Grok 4.6 (xhigh · fast)", ""},
		// the effort under another id: glm-5.2's is `reasoning`, on high.
		{"/model glm-5.2 max", "glm-5.2", "reasoning", "max", "GLM 5.2 (max)", ""},
		// another vocabulary, on the model the session is already on.
		{"/model glm-5.2 low", "glm-5.2", "reasoning", "max", "GLM 5.2 (max)", "effort not applied: glm-5.2 does not offer low"},
	} {
		m = pumpEnter(t, m, step.line)
		m = pumpSettled(t, m)
		if step.note != "" {
			notes = append(notes, step.note)
		}
		if got := texts(m, entryNote); strings.Join(got, "|") != strings.Join(notes, "|") {
			t.Fatalf("%s: notes %q, want %q", step.line, got, notes)
		}
		if errs := texts(m, entryError); len(errs) != 0 {
			t.Fatalf("%s: %q", step.line, errs)
		}
		snap := sess.Snapshot()
		if snap.CurrentModel != step.model || m.snap.CurrentModel != step.model {
			t.Fatalf("%s: the session is on %q, the screen on %q, want %q", step.line, snap.CurrentModel, m.snap.CurrentModel, step.model)
		}
		opt := agent.EffortOption(snap)
		switch {
		case step.effortID == "" && opt != nil:
			t.Fatalf("%s: %s offers %+v", step.line, step.model, opt)
		case step.effortID != "" && (opt == nil || opt.ID != step.effortID || opt.Current != step.effort):
			t.Fatalf("%s: the effort is %+v, want %s=%s", step.line, opt, step.effortID, step.effort)
		}
		if got := m.modelLabel(); got != step.label {
			t.Fatalf("%s: the status row reads %q, want %q", step.line, got, step.label)
		}
	}
}

// TestAnUntouchedTabFollowsTheLiveValueAndIsNotSent is X10 (f)'s first
// follow-up: a tab's value is seeded when the box opens, and a delta can move
// that option while the box is open. A tab the user never moved follows the
// live value on screen and Enter sends nothing for it; a tab the user did move
// is their choice, stays where they put it whatever the delta says, and is
// sent.
func TestAnUntouchedTabFollowsTheLiveValueAndIsNotSent(t *testing.T) {
	m, stub := cursorStub(t, "claude-opus-5")
	m = openDialog(t, m)
	for range 3 {
		m = pressKey(t, m, tea.KeyTab) // thinking, context, effort
	}
	m = pressKey(t, m, tea.KeyLeft) // effort → medium
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight) // fast → on

	// Another client moves the two options the user left alone, and the one
	// the user chose.
	for id, value := range map[string]string{"thinking": "false", "context": "300k", "effort": "xhigh"} {
		if _, err := stub.SetConfig(context.Background(), stubOtherClient, id, value, ""); err != nil {
			t.Fatal(err)
		}
	}
	m = feed(t, m, stubDeltas(t, stub)...)
	view := plainView(m)
	for _, want := range []string{
		"  thinking  [false]  true",
		"  context  [300k]  1m",
		"  effort  low  [medium]  high  xhigh  max",
		"> fast  off  [on]",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("the box is missing %q:\n%s", want, view)
		}
	}

	writes := configWrites(stub)
	m = applyDialog(t, m)
	if got, want := writes(), []string{"effort=medium", "fast=true"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the agent was sent %q, want the touched tabs alone, %q", got, want)
	}
	snap := stub.Snapshot()
	if optionCurrent(snap.Config, "thinking") != "false" || optionCurrent(snap.Config, "context") != "300k" {
		t.Fatalf("an untouched tab undid the other client's change: %+v", snap.Config)
	}
	if got := texts(m, entryNote); strings.Join(got, "|") != "effort → medium|fast → on" {
		t.Fatalf("notes %q", got)
	}
}

// TestAnUntouchedTabOnAnUnofferedValueIsNotSent: an agent can name a current
// value its option does not offer, and the tab then shows the first one it
// does. Nobody chose that value, so Enter with nothing moved sends nothing —
// before the tabs knew which of them the user had moved, it sent the first
// value as a change.
func TestAnUntouchedTabOnAnUnofferedValueIsNotSent(t *testing.T) {
	m, stub := cursorStub(t, "grok-4.6")
	cfg := cursorCatalogs()
	for i := range cfg["grok-4.6"] {
		if cfg["grok-4.6"][i].ID == "effort" {
			cfg["grok-4.6"][i].Current = "turbo"
		}
	}
	stub.SetModelCatalogs(cfg)
	m.refreshSnap()
	m = openDialog(t, m)
	if view := plainView(m); !strings.Contains(view, "  effort  [low]  medium") {
		t.Fatalf("the tab should show the first offered value:\n%s", view)
	}
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd != nil || stubConfigCalls(stub) != 0 {
		t.Fatal("nothing was moved, so nothing is sent")
	}
	if got := texts(m, entryNote); len(got) != 0 {
		t.Fatalf("notes %q", got)
	}
}

// TestTheFooterSaysTabOptionsWhenTheLabelsDoNotFit is X10 (f)'s second
// follow-up: the tabs are named in the footer when that fits the box, and
// "tab options" stands in for them when it does not, so the footer still ends
// on enter · esc. The footers over effort and fast alone are today's at every
// width, clamped where the box is narrower, as the 40x12 frame draws them.
func TestTheFooterSaysTabOptionsWhenTheLabelsDoNotFit(t *testing.T) {
	four := catalogTabs(agent.Snapshot{Config: stubFourSelectCatalog()})
	stub := NewStub().Snapshot().Config
	var ctxThinking []agent.ConfigOption
	for _, o := range stubFourSelectCatalog() {
		if o.ID == "context" || o.ID == "thinking" {
			ctxThinking = append(ctxThinking, o)
		}
	}
	const boxInner = dialogMaxWidth - dialogBorder // 52, every frame 58 columns wide or more
	for _, tc := range []struct {
		name  string
		tabs  []modelTab
		inner int
		want  string
	}{
		{"four tabs", four, boxInner, "type to filter · ↑↓ · tab options · enter · esc"},
		{"four tabs, room for their names", four, 200, "type to filter · ↑↓ · tab effort/fast/context/thinking · enter · esc"},
		{"two other tabs", catalogTabs(agent.Snapshot{Config: ctxThinking}), boxInner, "type to filter · ↑↓ · tab options · enter · esc"},
		{"effort and fast", catalogTabs(agent.Snapshot{Config: stub}), boxInner, modelDialogHint},
		// The 40x12 frame's box: today's footer, to be clamped as it always was.
		{"effort and fast, narrow", catalogTabs(agent.Snapshot{Config: stub}), 34, modelDialogHint},
		{"effort, narrow", catalogTabs(agent.Snapshot{Config: stub[:1]}), 30, "type to filter · ↑↓ · tab effort · enter · esc"},
		{"fast", catalogTabs(agent.Snapshot{Config: stub[1:]}), boxInner, "type to filter · ↑↓ · tab fast · enter · esc"},
		{"nothing", nil, 20, modelDialogHintPlain},
	} {
		if got := modelDialogHintAt(tc.tabs, tc.inner); got != tc.want {
			t.Errorf("%s at %d: %q, want %q", tc.name, tc.inner, got, tc.want)
		}
	}
}
