package transcript

import (
	"slices"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The tests in this file are internal/tui/transcript_test.go's model facts,
// moved under their names (plan 024 §3.8, A13): T1a split each mixed test
// there into its render assertion, which stays in the TUI, and its model
// assertion, which is here, driven by Fold. The assertion lines are T1a's,
// byte for byte; the setup lines build a Model instead of a TUI. Three are
// re-expressed rather than moved (A13): EntryCapTrimsWithANote is built through
// Fold, and TaskRowRunningThenReceipt and RepeatedBasenameFallsBackToDir are
// fresh model halves of tests that stay in the TUI whole.

// maxEntries is the main transcript's entry bound, the TUI's constant of the
// same name (DefaultBounds().MainEntries; TestDefaultBoundsAreTheTUIs pins the
// two together).
const maxEntries = 5000

func TestTodoToolIsNeverAdded(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	toolEvent(m, &agent.ToolEvent{
		ID: "todo-1", Kind: "other", Status: "completed",
		Title: "Update TODOs: read, edit, vet", ToolName: "updateTodos",
	})
	if got := factsOf(tr, "tool"); len(got) != 0 {
		t.Fatalf("the todo writer must not reach the transcript: %v", got)
	}
}

func TestTodoStreamNotes(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	todos := []agent.Todo{
		{ID: "1", Content: "Read main.go", Status: "in_progress"},
		{ID: "2", Content: "Edit main.go", Status: "pending"},
		{ID: "3", Content: "Run go vet", Status: "pending"},
	}
	// The model keeps the list it was handed (a payload is immutable once
	// emitted, SD-19), so each event carries a copy of its own.
	m.Fold(agent.Event{Type: agent.EventTodos, Todos: slices.Clone(todos)})
	m.Fold(agent.Event{Type: agent.EventTodos, Todos: slices.Clone(todos)})
	if got := factTexts(tr, "note"); len(got) != 1 || got[0] != "tasks: 3 planned" {
		t.Fatalf("planned note %q", got)
	}
	for i := range todos {
		todos[i].Status = "completed"
	}
	m.Fold(agent.Event{Type: agent.EventTodos, Todos: slices.Clone(todos)})
	if got := factTexts(tr, "note"); len(got) != 2 || got[1] != "tasks: 3/3 done" {
		t.Fatalf("done note %q", got)
	}
}

func TestThoughtRunCollapsesToOneRow(t *testing.T) {
	// These chunks carry no timestamps, so each entry is stamped from the clock.
	// A clock that stands still makes "nothing was measured" exact rather than
	// "less than the row rounds away"; the …Renders companion keeps the real one.
	base := time.Now()
	m := New(Options{Clock: func() time.Time { return base }})
	tr := m.Main
	for _, chunk := range []string{"weighing ", "the options"} {
		m.Fold(agent.Event{Type: agent.EventThought, Text: chunk})
	}
	if got := facts(tr); len(got) != 1 || got[0].Kind != "thought" || !got[0].Open || got[0].Text != "weighing the options" {
		t.Fatalf("a thought run should be one open entry holding every chunk: %v", got)
	}
	m.Fold(agent.Event{Type: agent.EventText, Text: "answer"})
	if f := facts(tr)[0]; f.Kind != "thought" || f.Open {
		t.Fatalf("the next entry should close the thought run: %v", f)
	}
	if f := facts(tr)[0]; f.End.Sub(f.At) != 0 {
		t.Fatalf("a thought run that took no measurable time must not hold a duration: %v", f)
	}
}

// TestThoughtRunCollapsesToOneRowWithNoClock is the engine's configuration of
// the same run: with no clock an unstamped chunk stays unstamped, and the span
// is zero just the same.
func TestThoughtRunCollapsesToOneRowWithNoClock(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	for _, chunk := range []string{"weighing ", "the options"} {
		m.Fold(agent.Event{Type: agent.EventThought, Text: chunk})
	}
	m.Fold(agent.Event{Type: agent.EventText, Text: "answer"})
	if f := facts(tr)[0]; f.Kind != "thought" || f.Open || !f.At.IsZero() || !f.End.IsZero() {
		t.Fatalf("with no clock an unstamped run stays unstamped: %v (at %v, end %v)", f, f.At, f.End)
	}
}

func TestThoughtRunShowsAMeasuredDuration(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	base := time.Now()
	m.Fold(agent.Event{Type: agent.EventThought, Text: "weighing", At: base})
	m.Fold(agent.Event{
		Type: agent.EventText,
		Text: "answer",
		At:   base.Add(5 * time.Second),
	})
	if f := facts(tr)[0]; f.Kind != "thought" || f.Open || f.End.Sub(f.At) != 5*time.Second {
		t.Fatalf("a measured thought run should hold its duration: %v", f)
	}
}

// TestThoughtRunClosesWhenANoteLandsAfterIt pins that an open run is ended by
// whatever is appended next: a note between the run and EventDone used to leave
// the row saying "Thinking…" for the rest of the session.
func TestThoughtRunClosesWhenANoteLandsAfterIt(t *testing.T) {
	base := time.Now()
	m := New(Options{Clock: func() time.Time { return base.Add(5 * time.Second) }})
	tr := m.Main

	m.Fold(agent.Event{Type: agent.EventThought, Text: "weighing", At: base})
	m.Fold(agent.Event{
		Type:  agent.EventTodos,
		Todos: []agent.Todo{{ID: "1", Content: "Read main.go", Status: "pending"}},
		At:    base.Add(5 * time.Second),
	})
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "end_turn", At: base.Add(9 * time.Second)})

	if f := facts(tr)[0]; f.Kind != "thought" || f.End.Sub(f.At) != 5*time.Second {
		t.Fatalf("elapsed should freeze where the note landed: %v", f)
	}
	if facts(tr)[0].Open || streamOpen(tr) {
		t.Fatal("the run should be closed in the model too")
	}
}

// TestToolRowEndsTheThoughtRunAboveIt covers the in-place path: a tool update
// that lands in an existing row still ends the run.
func TestToolRowEndsTheThoughtRunAboveIt(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	tool := &agent.ToolEvent{ID: "b1", Kind: "execute", Status: "pending", Title: "Shell", RawInput: "echo hi"}
	toolEvent(m, tool)
	m.Fold(agent.Event{Type: agent.EventThought, Text: "first"})
	toolEvent(m, &agent.ToolEvent{ID: "b1", Kind: "execute", Status: "completed", Title: "Shell", RawInput: "echo hi"})
	m.Fold(agent.Event{Type: agent.EventThought, Text: "second"})

	if got := factTexts(tr, "thought"); len(got) != 2 {
		t.Fatalf("a tool update between chunks must end the run: %q", got)
	}
	if got := factsOf(tr, "tool"); len(got) != 1 {
		t.Fatalf("the tool must still be one row: %v", got)
	}
}

// TestEntryCapTrimsWithANote is re-expressed (plan 024 A13): the TUI's
// version planted entries by hand; here every entry arrives through Fold — a
// tool, maxEntries-2 notes, a second tool, and one more note, which is one
// entry past the cap — and the head is trimmed. What the TUI's rebase
// assertion became, "the surviving tool is still the one its id finds", holds
// by EntryID with nothing rebased. The trim note itself is the client's.
func TestEntryCapTrimsWithANote(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "first"}})
	for i := 0; i < maxEntries-2; i++ {
		m.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})
	}
	m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "last"}})
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})

	if got := len(facts(tr)); got != maxEntries {
		t.Fatalf("cap not enforced: %d entries", got)
	}
	if !trimmed(tr) {
		t.Fatal("trimming should be recorded")
	}
	if _, ok := toolIndexed(tr, "first"); ok {
		t.Fatal("a trimmed tool row must be forgotten")
	}
	if f, ok := toolIndexed(tr, "last"); !ok || f.Kind != "tool" || f.ToolID != "last" {
		t.Fatalf("the surviving tool row is not the entry its id finds: %v (indexed %v)", f, ok)
	}
}

// TestTaskRowRunningThenReceipt is the model half of the TUI's test of the
// same name, written fresh (A13): the task's receipt lands in the row its
// running report drew — one entry, replaced in place, under the same EntryID,
// now holding the receipt's payload. The row's wording is the TUI's.
func TestTaskRowRunningThenReceipt(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	task := &agent.ToolEvent{
		ID: "t1", Kind: "other", Status: "in_progress", Title: "Task: Count main.go lines",
		ToolName: "task",
		Task:     &agent.TaskInfo{Description: "Count main.go lines"},
	}
	toolEvent(m, task)
	running := tr.Entries()
	if len(running) != 1 {
		t.Fatalf("the running task is not one entry: %v", facts(tr))
	}
	receipt := &agent.ToolEvent{
		ID: "t1", Kind: "other", Status: "completed", Title: "Task: Count main.go lines",
		ToolName: "task",
		Task: &agent.TaskInfo{
			Description: "Count main.go lines",
			Model:       "cursor-grok-4.6-high-fast",
			DurationMs:  8010,
			Receipt:     true,
		},
	}
	toolEvent(m, receipt)
	if got := factsOf(tr, "tool"); len(got) != 1 {
		t.Fatalf("a task must stay one row: %v", got)
	}
	done := tr.Entries()[0]
	if done.ID != running[0].ID {
		t.Fatalf("the receipt moved the row: %v, was %v", done.ID, running[0].ID)
	}
	if done == running[0] {
		t.Fatal("the receipt wrote the running entry in place; an update must be a new *Entry")
	}
	if done.Tool != receipt || running[0].Tool != task {
		t.Fatal("the row must hold the receipt's payload, and the running entry its own")
	}
	if f, ok := toolIndexed(tr, "t1"); !ok || f.ToolID != "t1" {
		t.Fatalf("the task's id no longer finds its row: %v (indexed %v)", f, ok)
	}
}

// TestRepeatedBasenameFallsBackToDir is the model half of the TUI's test of the
// same name, written fresh (A13): both reads are held, each with its own
// location, which is everything the pane needs to find the basename ambiguous
// and draw dir/file. The ambiguity map itself (pathDirs) is the pane's, derived
// at render time, and is not in the model (§3.2).
func TestRepeatedBasenameFallsBackToDir(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	toolEvent(m, &agent.ToolEvent{
		ID: "r1", Kind: "read", Status: "completed", Title: "Read main.go",
		Locations: []string{"/tmp/ws/main.go"},
	})
	toolEvent(m, &agent.ToolEvent{
		ID: "r2", Kind: "read", Status: "completed", Title: "Read main.go",
		Locations: []string{"/tmp/ws/cmd/main.go"},
	})
	if got := factsOf(tr, "tool"); len(got) != 2 || got[0].ToolID != "r1" || got[1].ToolID != "r2" {
		t.Fatalf("both reads should be held, in order: %v", got)
	}
	es := tr.Entries()
	if got := es[0].Tool.Locations; !slices.Equal(got, []string{"/tmp/ws/main.go"}) {
		t.Fatalf("the first read's location %q", got)
	}
	if got := es[1].Tool.Locations; !slices.Equal(got, []string{"/tmp/ws/cmd/main.go"}) {
		t.Fatalf("the second read's location %q", got)
	}
}

// TestOnlyTheWiresEndingClosesAStreamRun: the engine's own ending for a turn does
// not break a run, because it is not what orders the transcript — the wire's
// EventDone is, and it closes the run as it always did.
func TestOnlyTheWiresEndingClosesAStreamRun(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	feed(t, m, agent.Event{Type: agent.EventThought, Text: "a"})
	feed(t, m, agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
		ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn",
	}})
	feed(t, m, agent.Event{Type: agent.EventThought, Text: "b"})
	if got := factTexts(tr, "thought"); len(got) != 1 || got[0] != "ab" {
		t.Fatalf("the turn's ending split the run: %q", got)
	}
	feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	feed(t, m, agent.Event{Type: agent.EventThought, Text: "c"})
	if got := factTexts(tr, "thought"); len(got) != 2 {
		t.Fatalf("EventDone must close the run: %q", got)
	}
}

// TestCancelledTurnLeavesANote pins the acknowledgement Esc used to lack. The
// spinner going away is the only other sign the cancel landed, and that is
// indistinguishable from the turn having finished on its own.
func TestCancelledTurnLeavesANote(t *testing.T) {
	m := New(Options{})
	tr := m.Main
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})
	if notes := factTexts(tr, "note"); len(notes) != 1 || notes[0] != "cancelled" {
		t.Fatalf("cancel note %q", notes)
	}

	// A turn that ended on its own says nothing.
	m = New(Options{})
	tr = m.Main
	m.Fold(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	if notes := factTexts(tr, "note"); len(notes) != 0 {
		t.Fatalf("a normal turn end should be silent, got %q", notes)
	}
}
