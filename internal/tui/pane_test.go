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
	"github.com/charliek/craze/internal/transcript"
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

// streamOpen is the old fold's field of that name as the display list holds
// it (C5c): whether the pane's last row shows the shared model's open stream
// entry, which the next chunk of its kind grows in place.
func (t *pane) streamOpen() bool {
	n := len(t.rows)
	return n > 0 && t.rows[n-1].streaming
}

// toolLine is the old fold's tool index as the display list holds it (C5c):
// the tool id of every tool row the pane's id index can find, and the row's
// position in the list.
func (t *pane) toolLine() map[string]int {
	out := map[string]int{}
	for i, e := range t.rows {
		if e.kind == entryTool && e.tool != nil && !e.id.IsZero() && t.ids[e.id] == e {
			out[e.tool.ID] = i
		}
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
	sub.appendLocal(entry{kind: entryNote, text: "in the child"}, cp.now())
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

// TestClientLocalRowsLiveInThePane is §3.8's local-row rule (A12): every row
// this client writes for a message of its own — §2.4's first list — is marked
// local, and every row an event drew is not; and the shared model never holds
// a local row — the act that wrote one appended no entry to it, and the row
// names none — while every row an event drew shows an entry the model holds.
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
			held := modelIDs(m)
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
			// The shared model's side of the same rows.
			now := modelIDs(m)
			for _, e := range p.rows[before:] {
				switch {
				case tc.local && !e.id.IsZero():
					t.Fatalf("local row %q names shared entry %v", e.text, e.id)
				case !tc.local && !now[e.id]:
					t.Fatalf("row %q names entry %v, which the shared model does not hold", e.text, e.id)
				}
			}
			if tc.local {
				for id := range now {
					if !held[id] {
						t.Fatalf("writing a local row appended entry %v to the shared model", id)
					}
				}
			}
		})
	}
}

// modelIDs is every entry the shared model holds, over all its transcripts.
func modelIDs(m Model) map[transcript.EntryID]bool {
	out := map[transcript.EntryID]bool{}
	if m.shared == nil {
		return out
	}
	h := m.shared.History()
	for _, e := range h.Main.Entries {
		out[e.ID] = true
	}
	for _, sub := range h.Subs {
		for _, e := range sub.Entries {
			out[e.ID] = true
		}
	}
	return out
}

// The TUI's tests' names for rows only the shared model writes now (plan 024
// C5c), so no production code reads them.
const (
	// foreignTurnNote heads the stream of a turn the agent started on its own.
	foreignTurnNote = transcript.NoteForeignTurn
	// restoredNote closes a session/load replay in the transcript: everything
	// above it is history the agent handed back, everything below is this
	// session.
	restoredNote = transcript.NoteRestored
	// commandLineMark leads the provenance line under a user entry craze
	// expanded a plugin command or skill into.
	commandLineMark = transcript.CommandLineMark
)

// TestTheNoteWordingsAreTheSharedModels pins the TUI's names for the notes
// the shared model writes to the model's own wording: a test that reads a row
// by the TUI's constant reads the model's row.
func TestTheNoteWordingsAreTheSharedModels(t *testing.T) {
	for _, c := range []struct{ tui, model string }{
		{stopCancelled, transcript.NoteCancelled},
		{restoredNote, transcript.NoteRestored},
		{foreignTurnNote, transcript.NoteForeignTurn},
		{commandLineMark, transcript.CommandLineMark},
		{trimmedNote, transcript.TrimmedNote},
	} {
		if c.tui != c.model {
			t.Fatalf("the TUI says %q where the shared model writes %q", c.tui, c.model)
		}
	}
}

// TestALocalRowInsideAStreamDrawsBelowTheGrowingEntry is the recorded change
// of plan 024 §3.8 and §4 (i), as execution amendment X27 pins it: a local row
// that lands while a run is streaming no longer ends the run. The run is the
// shared model's, so the next chunk grows the entry above the local row — for
// an assistant reply as for a thought — where the old fold started a new row
// below it; and a thought's duration freezes at the next event, not at the
// local row.
func TestALocalRowInsideAStreamDrawsBelowTheGrowingEntry(t *testing.T) {
	base := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	t.Run("an assistant reply", func(t *testing.T) {
		m := sized(t)
		theme := otherTheme(t, m)
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "one ", At: base})
		m = runSlash(t, m, "/theme "+theme)
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "two", At: base.Add(time.Second)})
		want := []shownRow{
			{kind: "assistant", text: "one two"},
			{kind: "note", text: "theme → " + theme, local: true},
		}
		if got := shownRows(m.main); !slices.Equal(got, want) {
			t.Fatalf("rows %v, want the reply grown above the local row: %v", got, want)
		}
		if !m.main.rows[0].streaming {
			t.Fatal("the reply above the local row is no longer the open run")
		}
		if view := plainView(m); !inOrder(view, "one two", "theme → "+theme) {
			t.Fatalf("the frame does not draw the grown reply above the local row:\n%s", view)
		}
	})
	t.Run("a thought", func(t *testing.T) {
		m := sized(t)
		m.clock = func() time.Time { return base.Add(3 * time.Second) }
		theme := otherTheme(t, m)
		m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "weighing ", At: base})
		m = runSlash(t, m, "/theme "+theme)
		m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "it", At: base.Add(4 * time.Second)})
		if got := factsOf(m.main, "thought"); len(got) != 1 || !got[0].Open || got[0].Text != "weighing it" {
			t.Fatalf("the thought run did not grow past the local row: %v", facts(m.main))
		}
		if view := plainView(m); !inOrder(view, "+ Thinking…", "theme → "+theme) {
			t.Fatalf("the open run is not drawn above the local row:\n%s", view)
		}
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "answer", At: base.Add(5 * time.Second)})
		want := []shownRow{
			{kind: "thought", text: "weighing it"},
			{kind: "note", text: "theme → " + theme, local: true},
			{kind: "assistant", text: "answer"},
		}
		if got := shownRows(m.main); !slices.Equal(got, want) {
			t.Fatalf("rows %v, want %v", got, want)
		}
		// The local row landed at +3s; the run ended at the reply's +5s.
		if f := factsOf(m.main, "thought")[0]; f.Open || f.End.Sub(f.At) != 5*time.Second {
			t.Fatalf("the thought froze at %v, want at the next event's 5s: %v", f.End.Sub(f.At), f)
		}
		if view := plainView(m); !inOrder(view, "+ Thought for 5s", "theme → "+theme, "answer") {
			t.Fatalf("the frame:\n%s", view)
		}
	})
}

// TestAClearMidStreamContinuesInANewRow is execution amendment X30: /clear
// empties the pane but not the session, so the run it interrupted goes on in
// the shared model — one entry holding everything streamed — and the pane
// shows what came after the clear as a row of its own, dated at the first
// chunk after it and closed with the run: exactly the row the old fold drew.
// Once the run is longer than the stream cap, the row shows the whole tail
// (the recorded approximation).
func TestAClearMidStreamContinuesInANewRow(t *testing.T) {
	base := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	t.Run("a reply", func(t *testing.T) {
		m := sized(t)
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "before the clear ", At: base})
		m = runSlash(t, m, "/clear")
		if n := len(m.main.rows); n != 0 {
			t.Fatalf("/clear left %d rows", n)
		}
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "after", At: base.Add(2 * time.Second)})
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: " it", At: base.Add(3 * time.Second)})
		if got, want := shownRows(m.main), []shownRow{{kind: "assistant", text: "after it"}}; !slices.Equal(got, want) {
			t.Fatalf("rows %v, want the text after the clear alone: %v", got, want)
		}
		if f := facts(m.main)[0]; !f.At.Equal(base.Add(2 * time.Second)) {
			t.Fatalf("the continuation is dated %v, want its first chunk's %v", f.At, base.Add(2*time.Second))
		}
		if view := plainView(m); strings.Contains(view, "before the clear") || !strings.Contains(view, "after it") {
			t.Fatalf("the frame after the clear:\n%s", view)
		}
		// The session's own record is one entry, uncleared.
		if es := m.shared.Main.Entries(); len(es) != 1 || m.shared.Main.Tail() != "before the clear after it" {
			t.Fatalf("the shared model holds %d entries, tail %q", len(es), m.shared.Main.Tail())
		}
		m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn", At: base.Add(4 * time.Second)})
		if got, want := shownRows(m.main), []shownRow{{kind: "assistant", text: "after it"}}; !slices.Equal(got, want) || m.main.streamOpen() {
			t.Fatalf("the run closed into %v (open=%v), want %v", got, m.main.streamOpen(), want)
		}
	})
	t.Run("a thought", func(t *testing.T) {
		m := sized(t)
		m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "early ", At: base})
		m = runSlash(t, m, "/clear")
		m = feed(t, m,
			agent.Event{Type: agent.EventThought, Text: "late", At: base.Add(10 * time.Second)},
			agent.Event{Type: agent.EventText, Text: "answer", At: base.Add(13 * time.Second)},
		)
		thoughts := factsOf(m.main, "thought")
		if len(thoughts) != 1 || thoughts[0].Text != "late" || thoughts[0].Open {
			t.Fatalf("the continuation: %v", facts(m.main))
		}
		// Dated at the first chunk after the clear, closed where the run closed.
		if f := thoughts[0]; !f.At.Equal(base.Add(10*time.Second)) || f.End.Sub(f.At) != 3*time.Second {
			t.Fatalf("the continuation spans %v from %v, want 3s from the chunk after the clear", f.End.Sub(f.At), f.At)
		}
		if view := plainView(m); !inOrder(view, "+ Thought for 3s", "answer") {
			t.Fatalf("the frame:\n%s", view)
		}
	})
	t.Run("a run past the stream cap", func(t *testing.T) {
		m := sized(t)
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: "head ", At: base})
		m = runSlash(t, m, "/clear")
		long := strings.Repeat("z", entryTextCap+10)
		m = feed(t, m, agent.Event{Type: agent.EventText, Text: long, At: base.Add(time.Second)})
		rows := m.main.entries()
		if len(rows) != 1 || rows[0].text != m.shared.Main.Tail() || !strings.HasPrefix(rows[0].text, "…") {
			t.Fatalf("a cut run's continuation shows %d bytes, want the model's whole tail", len(rows[0].text))
		}
	})
	t.Run("a run the clear saw close", func(t *testing.T) {
		m := sized(t)
		m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "weighing", At: base})
		m = runSlash(t, m, "/clear")
		m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn", At: base.Add(time.Second)})
		if n := len(m.main.rows); n != 0 {
			t.Fatalf("closing a cleared run drew %v", shownRows(m.main))
		}
		m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "next", At: base.Add(2 * time.Second)})
		if got, want := shownRows(m.main), []shownRow{{kind: "thought", text: "next"}}; !slices.Equal(got, want) {
			t.Fatalf("rows %v, want the next run alone: %v", got, want)
		}
	})
}

// todayTodoNotes is the TUI's todo-note dedupe as it was before plan 024 —
// noteTodos over todoPlanned and todoDone, which /clear reset — kept here as
// the reference a pane is held to: the notes a pane that was shown these lists,
// in this order, since its last clear draws.
type todayTodoNotes struct {
	planned int
	done    bool
	notes   []string
}

func (d *todayTodoNotes) see(todos []agent.Todo) {
	if len(todos) == 0 {
		return
	}
	closed := 0
	for _, td := range todos {
		if td.Status == "completed" || td.Status == "cancelled" {
			closed++
		}
	}
	if closed == len(todos) {
		if !d.done {
			d.done = true
			d.notes = append(d.notes, fmt.Sprintf("tasks: %d/%d done", closed, len(todos)))
		}
		return
	}
	d.done = false
	if len(todos) > d.planned {
		d.planned = len(todos)
		d.notes = append(d.notes, fmt.Sprintf("tasks: %d planned", len(todos)))
	}
}

func (d *todayTodoNotes) clear() { *d = todayTodoNotes{} }

// noteTexts is the text of every note row a pane shows, oldest first.
func noteTexts(p *pane) []string {
	var out []string
	for _, r := range shownRows(p) {
		if r.kind == "note" {
			out = append(out, r.text)
		}
	}
	return out
}

// TestTodoNotesAfterClearAreThePanes is execution amendment X31 as revised at
// r17: the shared model's todo-note dedupe is the session's and never resets,
// and the pane's — which /clear resets — decides what the pane shows, in both
// directions. The pane shows exactly the notes today's dedupe would, for the
// list todosOf chose: a note the fold wrote is the display when the pane owes
// it, a note the pane owes that the fold did not write is written by the pane,
// locally, at the event's At, and a note the fold wrote that the pane does not
// owe gets no row.
func TestTodoNotesAfterClearAreThePanes(t *testing.T) {
	base := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	m := sized(t)
	todos := func(done bool) []agent.Todo {
		st := "pending"
		if done {
			st = "completed"
		}
		return []agent.Todo{{ID: "1", Content: "Read", Status: st}, {ID: "2", Content: "Edit", Status: st}}
	}
	notes := func() []shownRow {
		var out []shownRow
		for _, r := range shownRows(m.main) {
			if r.kind == "note" {
				out = append(out, r)
			}
		}
		return out
	}
	var today todayTodoNotes
	step := 0
	send := func(list []agent.Todo) {
		t.Helper()
		step++
		m = feed(t, m, agent.Event{Type: agent.EventTodos, Todos: list, At: base.Add(time.Duration(step) * time.Second)})
		// Every list here is the event's own, so it is the one todosOf picks.
		today.see(list)
		if got := noteTexts(m.main); !slices.Equal(got, today.notes) {
			t.Fatalf("step %d: the pane shows the notes %q, and today's dedupe %q", step, got, today.notes)
		}
	}
	clearPane := func() {
		t.Helper()
		m = runSlash(t, m, "/clear")
		today.clear()
	}

	send(todos(false))
	if got, want := notes(), []shownRow{{kind: "note", text: "tasks: 2 planned"}}; !slices.Equal(got, want) {
		t.Fatalf("notes %v, want %v", got, want)
	}
	send(todos(false))
	if got := notes(); len(got) != 1 {
		t.Fatalf("a repeated list noted again: %v", got)
	}
	clearPane()
	send(todos(false))
	// The pane owes the note again; the session's model has it already.
	if got, want := notes(), []shownRow{{kind: "note", text: "tasks: 2 planned", local: true}}; !slices.Equal(got, want) {
		t.Fatalf("after /clear the notes are %v, want %v", got, want)
	}
	if f := lastFact(t, m.main, "note", "tasks: 2 planned"); !f.At.Equal(base.Add(3 * time.Second)) {
		t.Fatalf("the pane's note is stamped %v, want the event's %v", f.At, base.Add(3*time.Second))
	}
	planned := 0
	for _, e := range m.shared.Main.Entries() {
		if e.Kind == transcript.KindNote && e.Text == "tasks: 2 planned" {
			planned++
		}
	}
	if planned != 1 {
		t.Fatalf("the shared model holds %d planned notes, want its one", planned)
	}
	// Both owe the done note, so the fold's is the display.
	send(todos(true))
	if got, want := notes(), []shownRow{{kind: "note", text: "tasks: 2 planned", local: true}, {kind: "note", text: "tasks: 2/2 done"}}; !slices.Equal(got, want) {
		t.Fatalf("notes %v, want %v", got, want)
	}
	// And after another clear, the pane owes it again and the model does not.
	clearPane()
	send(todos(true))
	if got, want := notes(), []shownRow{{kind: "note", text: "tasks: 2/2 done", local: true}}; !slices.Equal(got, want) {
		t.Fatalf("notes %v, want %v", got, want)
	}
}

// TestADelayedTodoListIsNotedOnce is the schedule review r17 found against
// X31 as first pinned: an empty EventTodos and then a one-item list are both
// published before the TUI consumes either. Applying the empty one,
// refreshSnap already sees the newer list and todosOf falls back to it, so the
// pane notes "tasks: 1 planned" from the snapshot, one event early; the next
// event's fold writes the same note in the shared model. The pane shows it
// once — today's dedupe over the lists todosOf chose — and the fold's note,
// which the pane does not owe, gets no row; the events are still folded as
// they came.
func TestADelayedTodoListIsNotedOnce(t *testing.T) {
	base := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	m := sized(t)
	stub := m.sess.(*Stub)
	one := []agent.Todo{{ID: "1", Content: "Read", Status: "pending"}}
	// The session's state has moved on to the list the second event carries.
	stub.SetTodos(one)
	m = feed(t, m,
		agent.Event{Type: agent.EventTodos, At: base},
		agent.Event{Type: agent.EventTodos, Todos: one, At: base.Add(time.Second)},
	)
	var today todayTodoNotes
	today.see(one) // the first event's list, as todosOf chose it
	today.see(one) // the second's
	if got := noteTexts(m.main); !slices.Equal(got, today.notes) || len(got) != 1 {
		t.Fatalf("the pane shows the notes %q, want today's %q", got, today.notes)
	}
	if got := shownRows(m.main); !got[0].local {
		t.Fatalf("the note is the pane's own, written from the snapshot: %v", got)
	}
	if f := lastFact(t, m.main, "note", "tasks: 1 planned"); !f.At.Equal(base) {
		t.Fatalf("the pane's note is stamped %v, want the first event's %v", f.At, base)
	}
	// The shared model folded both events as they came: its one note is the
	// second event's, which no row shows.
	var notes []*transcript.Entry
	for _, e := range m.shared.Main.Entries() {
		if e.Kind == transcript.KindNote {
			notes = append(notes, e)
		}
	}
	if len(notes) != 1 || notes[0].Text != "tasks: 1 planned" || !notes[0].At.Equal(base.Add(time.Second)) {
		t.Fatalf("the shared model's notes: %+v", notes)
	}
	if m.main.ids[notes[0].ID] != nil {
		t.Fatal("the fold's note, which the pane does not owe, has a row")
	}
}

// driftingErr is an error whose text changes every time it is read: Error is
// foreign code, and nothing promises it answers the same twice.
type driftingErr struct{ reads *int }

func (e driftingErr) Error() string {
	*e.reads++
	if *e.reads == 1 {
		return "the first failure"
	}
	return fmt.Sprintf("failure, read %d times", *e.reads)
}

// emptyErr is an error with no text, counting its reads.
type emptyErr struct{ reads *int }

func (e emptyErr) Error() string { *e.reads++; return "" }

// TestAnErrorsTextIsReadOnce is review r17's first finding: an EventError's
// text is read once, on the Update goroutine, before the fold — the row the
// shared model draws, m.err and so the host status all say what that one
// reading said, and Error runs exactly once per event (as it always has), under
// no lock. An empty text draws no row, and a child's error, which nothing
// reads, is not read at all.
func TestAnErrorsTextIsReadOnce(t *testing.T) {
	t.Run("a failure", func(t *testing.T) {
		m := sized(t)
		reads := 0
		m = feed(t, m, agent.Event{Type: agent.EventError, Err: driftingErr{&reads}})
		if reads != 1 {
			t.Fatalf("Error was called %d times for one event", reads)
		}
		if got := texts(m, entryError); len(got) != 1 || got[0] != "the first failure" {
			t.Fatalf("the error rows %q, want the first reading", got)
		}
		if m.err != "the first failure" {
			t.Fatalf("m.err %q, want the first reading", m.err)
		}
		if got := m.hostInput().Err; got != "the first failure" {
			t.Fatalf("the host status says %q, want the first reading", got)
		}
		if e := m.shared.Main.Entries(); e[len(e)-1].Text != "the first failure" {
			t.Fatalf("the shared model holds %q", e[len(e)-1].Text)
		}
	})
	t.Run("an empty failure", func(t *testing.T) {
		m := sized(t)
		reads := 0
		m = feed(t, m, agent.Event{Type: agent.EventError, Err: emptyErr{&reads}})
		if reads != 1 || m.err != "" || len(texts(m, entryError)) != 0 || len(m.shared.Main.Entries()) != 0 {
			t.Fatalf("reads %d, m.err %q, rows %q, entries %d: want one read and nothing drawn",
				reads, m.err, texts(m, entryError), len(m.shared.Main.Entries()))
		}
	})
	t.Run("a child's failure", func(t *testing.T) {
		m := withChild(t, sized(t), "task-1")
		reads := 0
		feed(t, m, agent.Event{Type: agent.EventError, Agent: "task-1", Err: driftingErr{&reads}})
		if reads != 0 {
			t.Fatalf("a child's error, which nothing draws, was read %d times", reads)
		}
	})
}

// TestAViewedChildTheModelEvictedKeepsItsRows is review r17's third finding:
// the shared model evicts a finished child past the roster's bound (32
// finished) and its transcript with it, but the TUI keeps the pane of the
// child it is viewing (pruneSubs). That pane's rows name entries nothing holds
// any more, so the roster fold that evicted it lets them go: they stay on
// screen as this client's own. (The parity watch, which checks every row of
// every pane, is what failed on this schedule.)
func TestAViewedChildTheModelEvictedKeepsItsRows(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	stub := m.sess.(*Stub)
	// A provider whose children stream their own transcript.
	stub.SetProvider(agent.GrokProvider())
	var roster []agent.SubagentInfo
	emit := func(info agent.SubagentInfo, change string) {
		t.Helper()
		if i := slices.IndexFunc(roster, func(s agent.SubagentInfo) bool { return s.ID == info.ID }); i >= 0 {
			roster[i] = info
		} else {
			roster = append(roster, info)
		}
		stub.SetSubagents(roster)
		m = feed(t, m, agent.Event{Type: agent.EventSubagent, Subagent: &info, SubagentChange: change})
	}
	child := func(id string, n int, done bool) agent.SubagentInfo {
		info := agent.SubagentInfo{ID: id, ToolCallID: id, Description: "child " + id, Status: agent.SubagentRunning, Transcript: true}
		if done {
			info.Status = agent.SubagentCompleted
			info.EndedAt = now.Add(time.Duration(n) * time.Second)
		}
		return info
	}

	emit(child("oldest", 0, false), agent.SubagentChangeSpawned)
	m = feed(t, m, agent.Event{Type: agent.EventText, Agent: "oldest", Text: "the oldest child's work"})
	m = openView(t, m)
	if m.viewing != "oldest" {
		t.Fatalf("fixture: viewing %q", m.viewing)
	}
	emit(child("oldest", 0, true), agent.SubagentChangeFinished)
	for i := 1; i <= 32; i++ {
		id := fmt.Sprintf("child-%02d", i)
		emit(child(id, i, false), agent.SubagentChangeSpawned)
		emit(child(id, i, true), agent.SubagentChangeFinished)
	}
	if m.shared.Sub("oldest") != nil {
		t.Fatal("fixture: the shared model kept the oldest of 33 finished children")
	}
	p := m.subs["oldest"]
	if m.viewing != "oldest" || p == nil {
		t.Fatalf("the view left the evicted child: viewing %q, pane %v", m.viewing, p != nil)
	}
	if got, want := shownRows(p), []shownRow{{kind: "assistant", text: "the oldest child's work", local: true}}; !slices.Equal(got, want) {
		t.Fatalf("the evicted child's pane holds %v, want its row, now this client's own: %v", got, want)
	}
	m.setViewportContent(true)
	if view := plainView(m); !strings.Contains(view, "the oldest child's work") {
		t.Fatalf("the view no longer shows the evicted child's rows:\n%s", view)
	}
}

// TestAModelTrimLeavesThePaneFromTheFront is execution amendment X28: when the
// shared model trims — here its 8 MiB main budget (owner decision 3), filled by
// tool payloads — the rows of the entries it let go leave the pane: from the
// front, with the local rows older than them, and a row re-appended out of the
// model's order on its own. The trim note leads what is left.
func TestAModelTrimLeavesThePaneFromTheFront(t *testing.T) {
	m := sized(t)
	// Eight of these fit the model's 8 MiB main budget and a ninth does not.
	big := strings.Repeat("x", 1<<20-4<<10)
	tool := func(id string) agent.Event {
		return agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
			ID: id, Kind: "execute", Status: "completed", Title: "Shell", RawInput: "echo " + id, ContentText: big,
		}}
	}
	m.addError("local 0")
	m = feed(t, m, tool("t1"))
	m.addError("local 1")
	m = feed(t, m, tool("t2"), tool("t3"))
	m.addError("local 3")
	for i := 4; i <= 8; i++ {
		m = feed(t, m, tool(fmt.Sprintf("t%d", i)))
	}
	if m.shared.Main.Trimmed() || m.main.trimmed {
		t.Fatal("fixture: 8 MiB of payloads already trimmed")
	}
	// The ninth pushes the model past its budget: t1 goes, and with it the
	// local row older than it; the local row after it stays.
	m = feed(t, m, tool("t9"))
	if !m.shared.Main.Trimmed() {
		t.Fatal("fixture: the model did not trim")
	}
	got := shownRows(m.main)
	if got[0] != (shownRow{kind: "error", text: "local 1", local: true}) || got[1].text != "t2" {
		t.Fatalf("after the model's trim the pane starts %v", got[:3])
	}
	for _, r := range got {
		if r.text == "t1" || r.text == "local 0" {
			t.Fatalf("row %v outlived the entry in front of it: %v", r, got)
		}
	}
	if !m.main.trimmed {
		t.Fatal("the pane does not say it lost rows")
	}
	m.refreshViewport()
	m.vp.GotoTop()
	if view := plainView(m); !inOrder(view, trimmedNote, "local 1") {
		t.Fatalf("the trim note does not lead the pane:\n%s", view)
	}

	// A re-appended row: /clear, then an update to t3 shows it again at the
	// tail, behind rows the model holds as newer. When the model lets t3 go,
	// that row leaves on its own.
	m = runSlash(t, m, "/clear")
	m = feed(t, m, tool("t10"))
	upd := tool("t3")
	upd.Tool.Status = "failed"
	m = feed(t, m, upd)
	if got := shownRows(m.main); len(got) != 2 || got[0].text != "t10" || got[1].text != "t3" {
		t.Fatalf("after /clear the pane shows %v, want t10 then the re-appended t3", got)
	}
	t3 := m.main.rows[1].id
	for id := 11; ; id++ {
		if id > 30 {
			t.Fatal("fixture: the model never let t3 go")
		}
		m = feed(t, m, tool(fmt.Sprintf("t%d", id)))
		if _, held := m.shared.Main.Entry(t3); !held {
			break
		}
	}
	got = shownRows(m.main)
	if got[0].text != "t10" {
		t.Fatalf("the model let t3 go before t10, yet t10's row left: %v", got)
	}
	for _, r := range got {
		if r.text == "t3" {
			t.Fatalf("the re-appended row outlived its entry: %v", got)
		}
	}
}
