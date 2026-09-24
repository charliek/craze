package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

func fill(m *Model, n int) {
	for i := 0; i < n; i++ {
		m.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("entry %02d padding so the transcript outgrows the viewport", i)})
	}
	m.refreshViewport()
}

func toolEvent(m Model, t *agent.ToolEvent) Model {
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: t}})
	return tm.(Model)
}

func lastToolRow(t *testing.T, m Model) string {
	t.Helper()
	rows := toolRows(m)
	if len(rows) == 0 {
		t.Fatalf("no tool rows in:\n%s", plainView(m))
	}
	return rows[len(rows)-1]
}

// TestStreamChunkRendersExactlyOneEntry is the render cache: a chunk must not
// restyle the whole transcript.
func TestStreamChunkRendersExactlyOneEntry(t *testing.T) {
	m := sized(t)
	fill(&m, 20)
	before := m.main.renders

	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "chunk"}})
	m = tm.(Model)
	if got := m.main.renders - before; got != 1 {
		t.Fatalf("a new stream entry rendered %d entries, want 1", got)
	}

	before = m.main.renders
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: " more"}})
	m = tm.(Model)
	if got := m.main.renders - before; got != 1 {
		t.Fatalf("a stream chunk rendered %d entries, want 1", got)
	}
	if !strings.Contains(plainView(m), "chunk more") {
		t.Fatalf("chunks did not coalesce:\n%s", plainView(m))
	}
}

func TestResizeAndCtrlOInvalidateEveryEntry(t *testing.T) {
	m := sized(t)
	fill(&m, 5)
	before := m.main.renders
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	m = tm.(Model)
	if got := m.main.renders - before; got != 5 {
		t.Fatalf("resize rendered %d entries, want 5", got)
	}
	before = m.main.renders
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = tm.(Model)
	if !m.expanded {
		t.Fatal("ctrl+o did not toggle")
	}
	if got := m.main.renders - before; got != 5 {
		t.Fatalf("ctrl+o rendered %d entries, want 5", got)
	}
}

func TestStickToBottomAcrossChunkResizeThemeAndCtrlO(t *testing.T) {
	m := sized(t)
	fill(&m, 60)
	if !m.vp.AtBottom() {
		t.Fatal("setup should stick to the bottom")
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "tail chunk"}})
	m = tm.(Model)
	if !m.vp.AtBottom() {
		t.Fatal("a chunk unstuck the viewport")
	}
	tm, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = tm.(Model)
	if !m.vp.AtBottom() {
		t.Fatal("resize unstuck the viewport")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = tm.(Model)
	if !m.vp.AtBottom() {
		t.Fatal("ctrl+o unstuck the viewport")
	}
	m.theme = Preset("dark")
	m.setViewportContent(m.vp.AtBottom())
	if !m.vp.AtBottom() {
		t.Fatal("a theme change unstuck the viewport")
	}
}

func TestScrolledUpSurvivesChunkResizeThemeAndCtrlO(t *testing.T) {
	m := sized(t)
	fill(&m, 60)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = tm.(Model)
	if m.vp.AtBottom() {
		t.Fatal("page up did not scroll")
	}
	offset := m.vp.YOffset

	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "tail chunk"}})
	m = tm.(Model)
	if m.vp.YOffset != offset {
		t.Fatalf("a chunk moved the viewport: %d -> %d", offset, m.vp.YOffset)
	}
	tm, _ = m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	m = tm.(Model)
	if m.vp.AtBottom() {
		t.Fatal("resize jumped to the bottom while scrolled up")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = tm.(Model)
	if m.vp.AtBottom() {
		t.Fatal("ctrl+o jumped to the bottom while scrolled up")
	}
	m.theme = Preset("dark")
	m.setViewportContent(m.vp.AtBottom())
	if m.vp.AtBottom() {
		t.Fatal("a theme change jumped to the bottom while scrolled up")
	}
}

// The tests from here to TestEntryCapTrimsWithANote, and
// TestOnlyTheWiresEndingClosesAStreamRun and TestCancelledTurnLeavesANote
// below, state transcript facts through model_facts_test.go's helpers, so C1
// carries their assertion lines into internal/transcript unchanged (plan 024
// §3.8). What a frame shows of the same events is each one's …Renders
// companion, which stays here.

func TestTodoToolIsNeverAdded(t *testing.T) {
	m := sized(t)
	tr := m.main
	m = toolEvent(m, &agent.ToolEvent{
		ID: "todo-1", Kind: "other", Status: "completed",
		Title: "Update TODOs: read, edit, vet", ToolName: "updateTodos",
	})
	if got := factsOf(tr, "tool"); len(got) != 0 {
		t.Fatalf("the todo writer must not reach the transcript: %v", got)
	}
}

func TestTodoToolIsNeverAddedRenders(t *testing.T) {
	m := sized(t)
	m = toolEvent(m, &agent.ToolEvent{
		ID: "todo-1", Kind: "other", Status: "completed",
		Title: "Update TODOs: read, edit, vet", ToolName: "updateTodos",
	})
	if strings.Contains(plainView(m), "Update TODOs") {
		t.Fatalf("Update TODOs is visible:\n%s", plainView(m))
	}
}

func TestTodoStreamNotes(t *testing.T) {
	m := sized(t)
	tr := m.main
	todos := []agent.Todo{
		{ID: "1", Content: "Read main.go", Status: "in_progress"},
		{ID: "2", Content: "Edit main.go", Status: "pending"},
		{ID: "3", Content: "Run go vet", Status: "pending"},
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}})
	m = tm.(Model)
	if got := factTexts(tr, "note"); len(got) != 1 || got[0] != "tasks: 3 planned" {
		t.Fatalf("planned note %q", got)
	}
	for i := range todos {
		todos[i].Status = "completed"
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}})
	m = tm.(Model)
	if got := factTexts(tr, "note"); len(got) != 2 || got[1] != "tasks: 3/3 done" {
		t.Fatalf("done note %q", got)
	}
}

func TestThoughtRunCollapsesToOneRow(t *testing.T) {
	m := sized(t)
	tr := m.main
	// These chunks carry no timestamps, so each entry is stamped from the clock.
	// A clock that stands still makes "nothing was measured" exact rather than
	// "less than the row rounds away"; the …Renders companion keeps the real one.
	base := time.Now()
	m.clock = func() time.Time { return base }
	for _, chunk := range []string{"weighing ", "the options"} {
		tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: chunk}})
		m = tm.(Model)
	}
	if got := facts(tr); len(got) != 1 || got[0].Kind != "thought" || !got[0].Open || got[0].Text != "weighing the options" {
		t.Fatalf("a thought run should be one open entry holding every chunk: %v", got)
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "answer"}})
	m = tm.(Model)
	if f := facts(tr)[0]; f.Kind != "thought" || f.Open {
		t.Fatalf("the next entry should close the thought run: %v", f)
	}
	if f := facts(tr)[0]; f.End.Sub(f.At) != 0 {
		t.Fatalf("a thought run that took no measurable time must not hold a duration: %v", f)
	}
}

func TestThoughtRunCollapsesToOneRowRenders(t *testing.T) {
	m := sized(t)
	for _, chunk := range []string{"weighing ", "the options"} {
		tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: chunk}})
		m = tm.(Model)
	}
	view := plainView(m)
	if !strings.Contains(view, "+ Thinking…") {
		t.Fatalf("an open thought run should say Thinking:\n%s", view)
	}
	if strings.Contains(view, "weighing") {
		t.Fatalf("a collapsed thought must not show its text:\n%s", view)
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "answer"}})
	m = tm.(Model)
	view = plainView(m)
	if !strings.Contains(view, "+ Thought") {
		t.Fatalf("a closed thought run should say Thought:\n%s", view)
	}
	// These chunks carry no timestamps, so nothing was measured: the row says
	// so by leaving the duration off rather than claiming "for 0s".
	if strings.Contains(view, "0s") {
		t.Fatalf("a thought run that took no measurable time must not print a duration:\n%s", view)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = tm.(Model)
	if !strings.Contains(plainView(m), "weighing the options") {
		t.Fatalf("ctrl+o did not expand the thought:\n%s", plainView(m))
	}
}

// TestThoughtRunShowsAMeasuredDuration is the other half: cursor bursts a run
// inside a second often enough that "+ Thought for 0s" read like a bug, but a
// run that really did take time still says how long.
func TestThoughtRunShowsAMeasuredDuration(t *testing.T) {
	m := sized(t)
	tr := m.main
	base := time.Now()
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "weighing", At: base}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{
		Type: agent.EventText,
		Text: "answer",
		At:   base.Add(5 * time.Second),
	}})
	m = tm.(Model)
	if f := facts(tr)[0]; f.Kind != "thought" || f.Open || f.End.Sub(f.At) != 5*time.Second {
		t.Fatalf("a measured thought run should hold its duration: %v", f)
	}
}

func TestThoughtRunShowsAMeasuredDurationRenders(t *testing.T) {
	m := sized(t)
	base := time.Now()
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "weighing", At: base}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{
		Type: agent.EventText,
		Text: "answer",
		At:   base.Add(5 * time.Second),
	}})
	m = tm.(Model)
	if view := plainView(m); !strings.Contains(view, "+ Thought for 5s") {
		t.Fatalf("a measured thought run should print its duration:\n%s", view)
	}
}

// TestThoughtRunClosesWhenANoteLandsAfterIt pins that an open run is ended by
// whatever is appended next: a note between the run and EventDone used to leave
// the row saying "Thinking…" for the rest of the session.
func TestThoughtRunClosesWhenANoteLandsAfterIt(t *testing.T) {
	m := sized(t)
	tr := m.main
	base := time.Now()
	m.clock = func() time.Time { return base.Add(5 * time.Second) }

	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "weighing", At: base}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{
		Type:  agent.EventTodos,
		Todos: []agent.Todo{{ID: "1", Content: "Read main.go", Status: "pending"}},
		At:    base.Add(5 * time.Second),
	}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn", At: base.Add(9 * time.Second)}})
	m = tm.(Model)

	if f := facts(tr)[0]; f.Kind != "thought" || f.End.Sub(f.At) != 5*time.Second {
		t.Fatalf("elapsed should freeze where the note landed: %v", f)
	}
	if facts(tr)[0].Open || streamOpen(tr) {
		t.Fatal("the run should be closed in the model too")
	}
}

func TestThoughtRunClosesWhenANoteLandsAfterItRenders(t *testing.T) {
	m := sized(t)
	base := time.Now()
	m.clock = func() time.Time { return base.Add(5 * time.Second) }

	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "weighing", At: base}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{
		Type:  agent.EventTodos,
		Todos: []agent.Todo{{ID: "1", Content: "Read main.go", Status: "pending"}},
		At:    base.Add(5 * time.Second),
	}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn", At: base.Add(9 * time.Second)}})
	m = tm.(Model)

	view := plainView(m)
	if strings.Contains(view, "+ Thinking…") {
		t.Fatalf("the run is still open after the turn ended:\n%s", view)
	}
	if !strings.Contains(view, "+ Thought for 5s") {
		t.Fatalf("elapsed should freeze where the note landed:\n%s", view)
	}
}

// TestToolRowEndsTheThoughtRunAboveIt covers the in-place path: a tool update
// that lands in an existing row still ends the run.
func TestToolRowEndsTheThoughtRunAboveIt(t *testing.T) {
	m := sized(t)
	tr := m.main
	tool := &agent.ToolEvent{ID: "b1", Kind: "execute", Status: "pending", Title: "Shell", RawInput: "echo hi"}
	m = toolEvent(m, tool)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "first"}})
	m = tm.(Model)
	m = toolEvent(m, &agent.ToolEvent{ID: "b1", Kind: "execute", Status: "completed", Title: "Shell", RawInput: "echo hi"})
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "second"}})
	m = tm.(Model)

	if got := factTexts(tr, "thought"); len(got) != 2 {
		t.Fatalf("a tool update between chunks must end the run: %q", got)
	}
	if got := factsOf(tr, "tool"); len(got) != 1 {
		t.Fatalf("the tool must still be one row: %v", got)
	}
}

// TestEntryCapTrimsWithANote is re-expressed rather than moved (plan 024 A13).
// It used to pin the tool index's rebase — the surviving row's index moving
// down by one — which the package's EntryID addressing retires; what outlives
// the rebase is that the surviving tool is still the one its id finds. The two
// indexed entries are tool entries here, the only kind the index can name once
// the fold builds it. The trim note's visibility is the …Renders companion.
func TestEntryCapTrimsWithANote(t *testing.T) {
	m := sized(t)
	tr := m.main
	m.main.rows = make([]*entry, maxEntries)
	for i := range m.main.rows {
		m.main.rows[i] = &entry{kind: entryNote, text: fmt.Sprintf("old %d", i)}
	}
	m.main.rows[0] = &entry{kind: entryTool, tool: &agent.ToolEvent{ID: "first"}}
	m.main.rows[maxEntries-1] = &entry{kind: entryTool, tool: &agent.ToolEvent{ID: "last"}}
	m.main.toolLine = map[string]int{"first": 0, "last": maxEntries - 1}
	m.appendEntry(entry{kind: entryNote, text: "newest"})

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

func TestEntryCapTrimsWithANoteRenders(t *testing.T) {
	m := sized(t)
	m.main.rows = make([]*entry, maxEntries)
	for i := range m.main.rows {
		m.main.rows[i] = &entry{kind: entryNote, text: fmt.Sprintf("old %d", i)}
	}
	m.main.toolLine = map[string]int{"first": 0, "last": maxEntries - 1}
	m.appendEntry(entry{kind: entryNote, text: "newest"})

	m.refreshViewport()
	m.vp.GotoTop()
	if !strings.Contains(plainView(m), trimmedNote) {
		t.Fatalf("missing the trim note:\n%s", plainView(m))
	}
}

func TestReadAndEditRows(t *testing.T) {
	m := sized(t)
	m = toolEvent(m, &agent.ToolEvent{
		ID: "r1", Kind: "read", Status: "completed", Title: "Read main.go",
		Locations: []string{"/tmp/ws/main.go"},
		Output:    &agent.ToolOutput{Content: "package main\nfunc main() {}\n"},
	})
	if got := lastToolRow(t, m); got != "✓ read  main.go" {
		t.Fatalf("read row %q", got)
	}

	oldText := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	newText := strings.Replace(oldText, `"hello"`, `"hello, world"`, 1)
	m = toolEvent(m, &agent.ToolEvent{
		ID: "e1", Kind: "edit", Status: "completed", Title: "Edit `/tmp/ws/main.go`",
		Locations: []string{"/tmp/ws/main.go"},
		Diffs: []agent.ToolDiff{{
			Path: "/tmp/ws/main.go", OldText: oldText, NewText: newText, Added: 1, Removed: 1,
		}},
	})
	row := lastToolRow(t, m)
	head, rest, _ := strings.Cut(row, "\n")
	if head != "✓ edit  main.go  +1 −1" {
		t.Fatalf("edit head %q", head)
	}
	if !strings.Contains(rest, "- ") || !strings.Contains(rest, "+ ") {
		t.Fatalf("collapsed edit should show the hunk, got:\n%s", rest)
	}
	if n := strings.Count(rest, "\n") + 1; n > editCollapsedLines+1 {
		t.Fatalf("collapsed edit is %d rows:\n%s", n, rest)
	}
	if strings.Contains(rest, "\t") {
		t.Fatalf("diff rows must expand tabs: %q", rest)
	}
}

func TestEditRowTooLargeNeverDiffs(t *testing.T) {
	m := sized(t)
	m = toolEvent(m, &agent.ToolEvent{
		ID: "e1", Kind: "edit", Status: "completed", Title: "Edit `/tmp/ws/big.txt`",
		Locations: []string{"/tmp/ws/big.txt"},
		Diffs: []agent.ToolDiff{{
			Path:      "/tmp/ws/big.txt",
			OldText:   strings.Repeat("x", 64*1024),
			NewText:   strings.Repeat("y", 64*1024),
			Added:     1,
			Removed:   1,
			Truncated: true,
		}},
	})
	got := lastToolRow(t, m)
	if got != "✓ edit  big.txt  diff too large (128 KiB)" {
		t.Fatalf("truncated edit row %q", got)
	}
}

func TestExecRowShowsExitCodeAndStderrPreview(t *testing.T) {
	m := sized(t)
	code := 127
	m = toolEvent(m, &agent.ToolEvent{
		ID: "b1", Kind: "execute", Status: "completed", Title: "`go vet ./...`",
		RawInput: "go vet ./...",
		Output: &agent.ToolOutput{
			ExitCode:   &code,
			Stderr:     "Command 'go' not found\nsudo apt install golang-go\n",
			StderrHead: "Command 'go' not found\nsudo apt install golang-go\n",
		},
	})
	row := lastToolRow(t, m)
	head, preview, _ := strings.Cut(row, "\n")
	if head != "✓ bash  go vet ./...  exit 127" {
		t.Fatalf("bash head %q", head)
	}
	if strings.TrimSpace(preview) != "Command 'go' not found" {
		t.Fatalf("stderr preview %q", preview)
	}
	if strings.Contains(row, "sudo apt") {
		t.Fatalf("collapsed bash shows one preview row only:\n%s", row)
	}

	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = tm.(Model)
	if !strings.Contains(lastToolRow(t, m), "sudo apt install golang-go") {
		t.Fatalf("ctrl+o should show the whole tail:\n%s", lastToolRow(t, m))
	}
}

func TestTaskRowRunningThenReceipt(t *testing.T) {
	m := sized(t)
	task := &agent.ToolEvent{
		ID: "t1", Kind: "other", Status: "in_progress", Title: "Task: Count main.go lines",
		ToolName: "task",
		Task:     &agent.TaskInfo{Description: "Count main.go lines"},
	}
	m = toolEvent(m, task)
	if got := lastToolRow(t, m); got != "● agent  Count main.go lines  running" {
		t.Fatalf("running task row %q", got)
	}
	m = toolEvent(m, &agent.ToolEvent{
		ID: "t1", Kind: "other", Status: "completed", Title: "Task: Count main.go lines",
		ToolName: "task",
		Task: &agent.TaskInfo{
			Description: "Count main.go lines",
			Model:       "cursor-grok-4.6-high-fast",
			DurationMs:  8010,
			Receipt:     true,
		},
	})
	if got := lastToolRow(t, m); got != "✓ agent  Count main.go lines  8.0s · grok-4.6-high-fast" {
		t.Fatalf("completed task row %q", got)
	}
	if len(toolRows(m)) != 1 {
		t.Fatalf("a task must stay one row: %q", toolRows(m))
	}
}

func TestRepeatedBasenameFallsBackToDir(t *testing.T) {
	m := sized(t)
	m = toolEvent(m, &agent.ToolEvent{
		ID: "r1", Kind: "read", Status: "completed", Title: "Read main.go",
		Locations: []string{"/tmp/ws/main.go"},
	})
	m = toolEvent(m, &agent.ToolEvent{
		ID: "r2", Kind: "read", Status: "completed", Title: "Read main.go",
		Locations: []string{"/tmp/ws/cmd/main.go"},
	})
	rows := toolRows(m)
	if len(rows) != 2 {
		t.Fatalf("rows %q", rows)
	}
	if rows[0] != "✓ read  ws/main.go" || rows[1] != "✓ read  cmd/main.go" {
		t.Fatalf("an ambiguous basename should show dir/file: %q", rows)
	}
}

func TestToolRowsClampToWidth(t *testing.T) {
	m := sized(t)
	code := 3
	m = toolEvent(m, &agent.ToolEvent{
		ID: "b1", Kind: "execute", Status: "completed",
		RawInput: strings.Repeat("long-command ", 40),
		Output:   &agent.ToolOutput{ExitCode: &code, StderrHead: strings.Repeat("e", 300)},
	})
	for _, ln := range strings.Split(lastToolRow(t, m), "\n") {
		if w := lipgloss.Width(ln); w > m.width {
			t.Fatalf("row is %d wide, terminal is %d: %q", w, m.width, ln)
		}
	}
	if !strings.Contains(lastToolRow(t, m), "exit 3") {
		t.Fatalf("the exit code must survive clamping: %q", lastToolRow(t, m))
	}
}

// TestPromptDoneDoesNotSplitAStreamRun is retired with the message it was about
// (plan 021 C4). A prompt's return is no longer a message at all: the engine runs
// the continuation and publishes one ordered ending into the same log as the
// chunks, so nothing can overtake them and there is no overtaking to pin. That
// EventDone alone closes a run is asserted just below
// (TestOnlyTheWiresEndingClosesAStreamRun); the property both were for — a turn's
// chunks coalesce into one run and the next turn's are a run of their own — is
// driven by TestCoalesceStreamChunks and by TestEnterSendsAndFollowUp.

// TestOnlyTheWiresEndingClosesAStreamRun: the engine's own ending for a turn does
// not break a run, because it is not what orders the transcript — the wire's
// EventDone is, and it closes the run as it always did.
func TestOnlyTheWiresEndingClosesAStreamRun(t *testing.T) {
	m := sized(t)
	tr := m.main
	m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "a"})
	m = feed(t, m, agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
		ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn",
	}})
	m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "b"})
	if got := factTexts(tr, "thought"); len(got) != 1 || got[0] != "ab" {
		t.Fatalf("the turn's ending split the run: %q", got)
	}
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	m = feed(t, m, agent.Event{Type: agent.EventThought, Text: "c"})
	if got := factTexts(tr, "thought"); len(got) != 2 {
		t.Fatalf("EventDone must close the run: %q", got)
	}
}

func TestModeChangeLeavesANote(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	notes := texts(m, entryNote)
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "mode → ") {
		t.Fatalf("mode note %q", notes)
	}
}

// TestCancelledTurnLeavesANote pins the acknowledgement Esc used to lack. The
// spinner going away is the only other sign the cancel landed, and that is
// indistinguishable from the turn having finished on its own.
func TestCancelledTurnLeavesANote(t *testing.T) {
	m := sized(t)
	tr := m.main
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "cancelled"}})
	m = tm.(Model)
	if notes := factTexts(tr, "note"); len(notes) != 1 || notes[0] != "cancelled" {
		t.Fatalf("cancel note %q", notes)
	}

	// A turn that ended on its own says nothing.
	m = sized(t)
	tr = m.main
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	if notes := factTexts(tr, "note"); len(notes) != 0 {
		t.Fatalf("a normal turn end should be silent, got %q", notes)
	}
}

// TestCancelledTurnLeavesANoteRenders holds the one assertion of the test above
// that is neither a frame nor a transcript fact: the TUI's own status after a
// cancel, which stays with the client.
func TestCancelledTurnLeavesANoteRenders(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "cancelled"}})
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %v after a cancel", m.status)
	}
}

// TestSearchRowDoesNotRepeatTheQuery: cursor titles a search with the pattern
// it is searching for and sends the same pattern as rawInput, so the row used
// to print it twice, the second time as raw JSON.
func TestSearchRowDoesNotRepeatTheQuery(t *testing.T) {
	m := sized(t)
	cases := []struct {
		name     string
		title    string
		rawInput string
		want     string
		notWant  string
	}{
		{
			name:     "title already shows the pattern",
			title:    "Find `**/main.go`",
			rawInput: `{"pattern":"**/main.go"}`,
			want:     "Find `**/main.go`",
			notWant:  `{"pattern"`,
		},
		{
			name:     "title hides the query, so show it",
			title:    "Search",
			rawInput: `{"pattern":"needle"}`,
			want:     "needle",
		},
		{
			name:     "unparsable rawInput still shows",
			title:    "Search",
			rawInput: "needle",
			want:     "needle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &agent.ToolEvent{ID: "s1", Kind: "search", Status: "completed", Title: tc.title, RawInput: tc.rawInput}
			rows := m.otherRows(ev, 80)
			got := strings.Join(rows, "\n")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("row %q does not contain %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Fatalf("row %q still repeats the query (%q)", got, tc.notWant)
			}
		})
	}
}

// hangingRows renders each row as one seg. A two-seg split would clamp
// differently when the prefix alone fills the width — renderSegSpans consumes
// the prefix, finds no room left and breaks before the text seg, so it
// truncates without the ellipsis a single seg would have produced. craze never
// draws a transcript narrower than minFrameCols, so no caller reaches this,
// but hangingRows' seven call sites predate the styled variant and must keep
// rendering exactly as they did.
func TestHangingRowsClampsAcrossThePrefixBoundary(t *testing.T) {
	st := styleFG(Preset("tokyo-night").Err)
	rows := hangingRows("x", "error: ", "  ", 7, st)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %q", len(rows), rows)
	}
	if got := plain(rows[0]); got != "error:…" {
		t.Fatalf("hangingRows clamped to %q, want %q", got, "error:…")
	}
}
