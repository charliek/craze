package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// entries is the pane's display list as values, oldest first, in the []entry
// shape the transcript held before the pane (plan 024 §3.8, the compatibility
// accessor): render cache included, so a test can read what a row drew. It is
// a copy, so writing to it changes nothing. C5c retires it wherever a test can
// read the shared model instead.
func (t *pane) entries() []entry {
	out := make([]entry, len(t.rows))
	for i, e := range t.rows {
		out[i] = *e
	}
	return out
}

// TestModelCopiesShareTheirPanes pins pane's aliasing rule (plan 024 §3.8):
// bubbletea copies Model by value, and every copy holds the same panes, so a
// row written through one copy is a row of the other — main's, the rows it was
// painted into, and a sub-agent's pane created after the copy was taken.
func TestModelCopiesShareTheirPanes(t *testing.T) {
	m := sized(t)
	notes := len(texts(m, entryNote))
	cp := m
	cp.addNote("written through the copy")
	if got := texts(m, entryNote); len(got) != notes+1 || got[notes] != "written through the copy" {
		t.Fatalf("the original holds the notes %q, want the copy's row last", got)
	}
	cp.refreshViewport()
	if plain := m.main.transcriptPlain; len(plain) == 0 || plain[len(plain)-1] != "written through the copy" {
		t.Fatalf("the original's painted rows are not the copy's: %q", plain)
	}

	sub := cp.ensureSub("task-1")
	sub.addNote("in the child", cp.now())
	if m.subs["task-1"] != sub {
		t.Fatal("a sub-agent's pane made through the copy is not the original's")
	}
	if got := m.subs["task-1"].entries(); len(got) != 1 || got[0].text != "in the child" {
		t.Fatalf("the original's sub-agent pane holds %+v, want the child's row", got)
	}
}

// shownRow is what the pane tests read of one displayed row: its kind, what it
// says, and whether it is this client's own.
type shownRow struct {
	kind  string
	text  string
	local bool
}

func (r shownRow) String() string {
	if r.local {
		return fmt.Sprintf("{%s %q local}", r.kind, r.text)
	}
	return fmt.Sprintf("{%s %q}", r.kind, r.text)
}

// shownRows is a pane's display list, oldest first. A row with no text of its
// own is named by what it shows: a tool by its id, a plan by its name, a `!`
// row by its command.
func shownRows(p *pane) []shownRow {
	var out []shownRow
	for _, e := range p.entries() {
		text := e.text
		switch {
		case e.tool != nil:
			text = e.tool.ID
		case e.plan != nil:
			text = e.plan.Name
		case e.shell != nil:
			text = "! " + e.shell.cmd
		}
		out = append(out, shownRow{kind: factKind(e.kind), text: text, local: e.local})
	}
	return out
}

// inOrder reports whether every want is in view, each below the one before it.
func inOrder(view string, want ...string) bool {
	for _, w := range want {
		i := strings.Index(view, w)
		if i < 0 {
			return false
		}
		view = view[i+len(w):]
	}
	return true
}

// otherTheme is a preset the model is not already drawn in, so /theme with it
// changes something and writes its note.
func otherTheme(t *testing.T, m Model) string {
	t.Helper()
	for _, name := range ThemeNames() {
		if name != m.theme.Name {
			return name
		}
	}
	t.Fatal("fixture: there is only one theme")
	return ""
}

// TestEchoHidingLeavesTheFrameUnchanged is §3.8's two echo schedules (A12):
// the echo of something this client already drew adds nothing, and the rows
// written around it stay where they were written.
//
//   - A,U,L: a reply, the user's own Enter — the optimistic row — and then a
//     local row, before the started the engine publishes for that Enter
//     arrives. The started is this client's echo, so it draws nothing: the
//     optimistic row stays the display, above the local row that followed it.
//   - An answer, a chunk, then the answer's own ending: the card answered
//     here writes its notes at once, the reply carries on under them, and the
//     ending the answer caused — this client's echo — adds nothing.
func TestEchoHidingLeavesTheFrameUnchanged(t *testing.T) {
	t.Run("a reply, the user's Enter, a local row, then the started", func(t *testing.T) {
		m := sized(t)
		theme := otherTheme(t, m)
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "an earlier reply"})
		m = startTurn(t, m, "my question")
		own := m.ownTurn
		if own == "" {
			t.Fatal("fixture: Enter started no turn of this client's own")
		}
		m = runSlash(t, m, "/theme "+theme)
		want := []shownRow{
			{kind: "assistant", text: "an earlier reply"},
			{kind: "user", text: "my question", local: true},
			{kind: "note", text: "theme → " + theme, local: true},
		}
		if got := shownRows(m.main); !slices.Equal(got, want) {
			t.Fatalf("before the echo the rows are %v, want %v", got, want)
		}
		frame := slices.Clone(m.main.transcriptPlain)

		m = feed(t, m, agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
			ID: own, Phase: agent.TurnStarted, Text: "my question",
		}})
		if got := shownRows(m.main); !slices.Equal(got, want) {
			t.Fatalf("the started echo changed the rows to %v, want %v", got, want)
		}
		if got := m.main.transcriptPlain; !slices.Equal(got, frame) {
			t.Fatalf("the started echo changed the frame:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(frame, "\n"))
		}
		if view := plainView(m); !inOrder(view, "an earlier reply", "❯ my question", "theme → "+theme) {
			t.Fatalf("the frame does not show the three rows in the order they were written:\n%s", view)
		}
	})

	t.Run("an answer, a chunk, then the answer's own ending", func(t *testing.T) {
		m, _ := questionCard(t)
		m, _ = press(m, runeKey('1'))
		m, _ = press(m, runeKey('1'))
		m, _ = press(m, enter())
		if len(m.askEchoes) != 1 {
			t.Fatalf("fixture: the answer is not awaiting its echo: %v", m.askEchoes)
		}
		cause := m.askEchoes[0]
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "carrying on"})
		want := []shownRow{
			{kind: "note", text: "? Pick one → A", local: true},
			{kind: "note", text: "? Pick any → X", local: true},
			{kind: "assistant", text: "carrying on"},
		}
		if got := shownRows(m.main); !slices.Equal(got, want) {
			t.Fatalf("before the echo the rows are %v, want %v", got, want)
		}
		frame := slices.Clone(m.main.transcriptPlain)

		m = feed(t, m, agent.Event{Type: agent.EventAsk, Cause: cause, Ask: &agent.AskUpdate{
			ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered,
			Answers: map[string][]string{"q1": {"opt-a"}, "q2": {"opt-x"}},
		}})
		if got := shownRows(m.main); !slices.Equal(got, want) {
			t.Fatalf("the ending echo changed the rows to %v, want %v", got, want)
		}
		if got := m.main.transcriptPlain; !slices.Equal(got, frame) {
			t.Fatalf("the ending echo changed the frame:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(frame, "\n"))
		}
		if view := plainView(m); !inOrder(view, "? Pick one → A", "? Pick any → X", "carrying on") {
			t.Fatalf("the frame does not show the three rows in the order they were written:\n%s", view)
		}
	})
}

// TestMixedRowsAtTheCapTrimFromTheFront is the display cap over both kinds of
// row (§3.8): at maxEntries the oldest rows go, whichever kind they are, the
// trim note leads the list, and the newest row of each kind is still there.
func TestMixedRowsAtTheCapTrimFromTheFront(t *testing.T) {
	m := sized(t)
	if n := len(m.main.entries()); n != 0 {
		t.Fatalf("fixture: the pane starts with %d rows", n)
	}
	// Even rows are local failures, odd ones the command lines events draw.
	row := func(i int) shownRow {
		if i%2 == 0 {
			return shownRow{kind: "error", text: fmt.Sprintf("local %d", i), local: true}
		}
		return shownRow{kind: "note", text: fmt.Sprintf("⤷ p:c%d", i)}
	}
	const over = 101
	total := maxEntries + over
	for i := 0; i < total; i++ {
		if i%2 == 0 {
			m.addError(row(i).text)
			continue
		}
		m.applyEvent(agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{
			PluginCommand: agent.PluginCommand{Qualified: fmt.Sprintf("p:c%d", i)},
		}})
	}

	got := shownRows(m.main)
	if len(got) != maxEntries {
		t.Fatalf("the list holds %d rows, want the cap's %d", len(got), maxEntries)
	}
	// The front went, whatever its kind: what is left is exactly the newest
	// maxEntries rows, in the order they were written.
	for j, r := range got {
		if want := row(over + j); r != want {
			t.Fatalf("row %d is %v, want %v", j, r, want)
		}
	}
	if last := got[len(got)-2:]; last[0] != row(total-2) || last[1] != row(total-1) {
		t.Fatalf("the newest rows are %v, want the newest of each kind", last)
	}
	m.refreshViewport()
	m.vp.GotoTop()
	if view := plainView(m); !inOrder(view, trimmedNote, fmt.Sprintf("local %d", over+1)) {
		t.Fatalf("the trim note does not lead the list:\n%s", view)
	}
}

// TestClientLocalRowsLiveInThePane is the half of §3.8's local-row rule that
// holds under the old fold (A12): every row this client writes for a message
// of its own — §2.4's first list — is marked local, and every row an event
// drew is not. (C5c adds that the shared model never holds a local row.)
func TestClientLocalRowsLiveInThePane(t *testing.T) {
	boom := errors.New("boom")
	update := func(msg tea.Msg) func(*testing.T, Model) Model {
		return func(_ *testing.T, m Model) Model {
			tm, _ := m.Update(msg)
			return tm.(Model)
		}
	}
	events := func(evs ...agent.Event) func(*testing.T, Model) Model {
		return func(t *testing.T, m Model) Model { return feed(t, m, evs...) }
	}
	keys := func(ks ...tea.KeyMsg) func(*testing.T, Model) Model {
		return func(_ *testing.T, m Model) Model {
			for _, k := range ks {
				m, _ = press(m, k)
			}
			return m
		}
	}
	slash := func(line string) func(*testing.T, Model) Model {
		return func(t *testing.T, m Model) Model { return runSlash(t, m, line) }
	}
	withQuestion := func(t *testing.T) Model { m, _ := questionCard(t); return m }
	withPlan := func(t *testing.T) Model { m, _ := planCard(t); return m }
	withChild := func(t *testing.T) Model {
		now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
		m := agentModel(t, &now)
		return applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	}
	withReplay := func(t *testing.T) Model {
		m, _ := loadedStub(t, nil)
		return feed(t, m, replayEvent(agent.ReplayStart))
	}
	turn := func(ti agent.TurnInfo) agent.Event { return agent.Event{Type: agent.EventTurn, Turn: &ti} }

	for _, tc := range []struct {
		name  string
		local bool
		build func(*testing.T) Model // sized when nil
		act   func(*testing.T, Model) Model
		on    string // the sub-agent whose pane the rows land in; main when ""
	}{
		// §2.4's first list: a message of the client's own.
		{name: "the start failure", local: true, act: update(errMsg{err: boom})},
		{name: "an action's failure", local: true, act: update(actionErrMsg{err: boom})},
		{name: "a refused mode", local: true, act: func(t *testing.T, m Model) Model {
			return update(revertModeMsg{gen: m.modeGen, err: boom})(t, m)
		}},
		{name: "a refused model", local: true, act: update(revertModelMsg{err: boom})},
		{name: "a model it could not read back", local: true, act: update(modelUnreadMsg{})},
		{name: "the model dialog's apply notes", local: true,
			act: update(modelApplyMsg{gen: -1, done: []applyStep{{note: "model → other"}}})},
		{name: "an effort the model step could not keep", local: true, act: update(effortNotAppliedMsg{note: "effort stays"})},
		{name: "its own failed cancel", local: true, act: update(cancelFailedMsg{err: boom})},
		{name: "the plan-implement mode note", local: true, act: func(t *testing.T, m Model) Model {
			return update(planImplementMsg{seq: m.turnSeq - 1, gen: m.modeGen, mode: "agent"})(t, m)
		}},
		{name: "the optimistic user row", local: true, act: func(t *testing.T, m Model) Model {
			return startTurn(t, m, "typed here")
		}},
		{name: "an answer's notes", local: true, build: withQuestion,
			act: keys(runeKey('1'), runeKey('1'), enter())},
		{name: "a skip's note", local: true, build: withQuestion, act: keys(tea.KeyMsg{Type: tea.KeyEsc})},
		{name: "a plan's verb", local: true, build: withPlan, act: keys(runeKey('a'))},
		{name: "another client's answer to its card", local: true, build: withQuestion,
			act: events(agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
				ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered,
				Answers: map[string][]string{"q1": {"opt-b"}},
			}})},
		{name: "a theme note", local: true, act: func(t *testing.T, m Model) Model {
			return runSlash(t, m, "/theme "+otherTheme(t, m))
		}},
		{name: "an unknown theme", local: true, act: slash("/theme no-such-theme")},
		{name: "/rename's usage", local: true, act: slash("/rename")},
		{name: "/rename's note", local: true, act: slash("/rename a better title")},
		{name: "/model's refusal", local: true, act: slash("/model no-such-model")},
		{name: "a mode slash command", local: true, act: slash("/plan")},
		{name: "a `!` row", local: true, act: func(t *testing.T, m Model) Model {
			return runShellThrough(t, m, "!echo hi")
		}},
		{name: "a `!` result whose row /clear took", local: true, act: func(t *testing.T, m Model) Model {
			m.input.SetValue("!echo hi")
			tm, cmd := m.Update(enter())
			done, ok := runCmd(cmd).(shellDoneMsg)
			if !ok {
				t.Fatal("fixture: the command did not run")
			}
			m = runSlash(t, tm.(Model), "/clear")
			tm, _ = m.Update(done)
			return tm.(Model)
		}},
		{name: "a receipt-mode sub-agent's rebuilt rows", local: true, build: withChild, on: "task-1",
			act: func(t *testing.T, m Model) Model { return openView(t, m) }},

		// §2.4's second list: what an event drew.
		{name: "a reply", act: events(agent.Event{Type: agent.EventText, Text: "a reply"})},
		{name: "a thought", act: events(agent.Event{Type: agent.EventThought, Text: "weighing"})},
		{name: "a tool", act: events(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
			ID: "t1", Kind: "read", Status: "completed", Title: "Read main.go",
		}})},
		{name: "the todos note", act: events(agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{
			{ID: "1", Content: "Read main.go", Status: "pending"},
		}})},
		{name: "a plan block", act: events(agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})},
		{name: "a command line", act: events(agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{
			PluginCommand: agent.PluginCommand{Qualified: "p:c", Kind: agent.PluginKindCommand},
		}})},
		{name: "a cancelled turn's note", act: events(agent.Event{Type: agent.EventDone, StopReason: stopCancelled})},
		{name: "the session's failure", act: events(agent.Event{Type: agent.EventError, Err: boom})},
		{name: "the engine's cancel failure", act: events(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
			Detail: "cancel failed",
		}})},
		{name: "an index write that failed", act: events(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
			IndexErr: "index write failed",
		}})},
		{name: "a foreign turn's note", act: events(agent.Event{Type: agent.EventForeignTurn,
			ForeignTurn: &agent.ForeignTurnInfo{Running: true}})},
		{name: "the restored note", build: withReplay, act: events(replayEvent(agent.ReplayEnd))},
		{name: "an interjection", act: events(agent.Event{Type: agent.EventUser, Interjection: true, Text: "and also"})},
		{name: "a replayed prompt", act: events(agent.Event{Type: agent.EventUser, Replayed: true, Text: "an old prompt"})},
		{name: "another client's turn", act: events(turn(agent.TurnInfo{
			ID: "turn-elsewhere", Phase: agent.TurnStarted, Text: "from elsewhere",
		}))},
		{name: "a synthetic cancelled ending", act: events(turn(agent.TurnInfo{
			ID: "turn-x", Phase: agent.TurnEnded, Synthetic: true, StopReason: stopCancelled,
		}))},
		{name: "a synthetic failed ending", act: events(turn(agent.TurnInfo{
			ID: "turn-x", Phase: agent.TurnEnded, Synthetic: true, Err: "refused",
		}))},
		{name: "a sub-agent's reply", build: withChild, on: "task-1",
			act: events(agent.Event{Type: agent.EventText, Agent: "task-1", Text: "partial"})},
		{name: "a sub-agent's prompt", build: withChild, on: "task-1",
			act: events(agent.Event{Type: agent.EventUser, Agent: "task-1", Text: "count them"})},
		{name: "a sub-agent's tool", build: withChild, on: "task-1",
			act: events(agent.Event{Type: agent.EventTool, Agent: "task-1", Tool: &agent.ToolEvent{
				ID: "c1", Kind: "read", Status: "completed", Title: "Read a.go",
			}})},
		{name: "a sub-agent's command line", build: withChild, on: "task-1",
			act: events(agent.Event{Type: agent.EventCommand, Agent: "task-1", Command: &agent.ExpandedCommand{
				PluginCommand: agent.PluginCommand{Qualified: "p:c"},
			}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := tc.build
			if build == nil {
				build = sized
			}
			m := build(t)
			paneOf := func(m Model) *pane {
				if tc.on == "" {
					return m.main
				}
				return m.subs[tc.on]
			}
			before := 0
			if p := paneOf(m); p != nil {
				before = len(p.rows)
			}
			m = tc.act(t, m)
			p := paneOf(m)
			if p == nil {
				t.Fatal("no pane for the rows")
			}
			got := shownRows(p)
			if len(got) <= before {
				t.Fatalf("wrote no row: %v", got)
			}
			for _, r := range got[before:] {
				if r.local != tc.local {
					t.Fatalf("row %v is marked local=%v, want %v (all: %v)", r, r.local, tc.local, got)
				}
			}
		})
	}
}
