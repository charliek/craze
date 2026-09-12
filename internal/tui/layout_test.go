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

func fixedClock(t *testing.T) (*time.Time, func() time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	return &now, func() time.Time { return now }
}

func stubTodos() []agent.Todo {
	return []agent.Todo{
		{ID: "1", Content: "Read main.go", Status: "completed"},
		{ID: "2", Content: "Edit main.go", Status: "in_progress"},
		{ID: "3", Content: "Run go vet", Status: "pending"},
		{ID: "4", Content: "Write the note", Status: "pending"},
	}
}

func stubAgentTools(n int) []agent.ToolEvent {
	out := make([]agent.ToolEvent, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, agent.ToolEvent{
			ID:       fmt.Sprintf("task-%d", i),
			Kind:     "other",
			ToolName: "task",
			Title:    fmt.Sprintf("Task: count lines %d", i),
			Status:   "in_progress",
			RawInput: fmt.Sprintf("count the lines in file %d", i),
			Task:     &agent.TaskInfo{Description: fmt.Sprintf("count lines %d", i)},
		})
	}
	return out
}

// loadedModel is the acceptance fixture: tasks panel, spinner, four sub-agent
// rows and a card, all on at once.
func loadedModel(t *testing.T, cols, rows int) Model {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.HangNext()
	_, clock := fixedClock(t)
	stub.Clock = clock

	m := New(Config{Session: stub, Workspace: t.TempDir(), Model: "grok", Yolo: true})
	m.clock = clock
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)

	m.input.SetValue("go")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("fixture is %s, want working", m.status)
	}

	m = applyInFlight(t, m, stubAgentTools(4))

	todos := stubTodos()
	stub.SetTodos(todos)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}})
	m = tm.(Model)

	tm, _ = m.Update(eventMsg{agent.Event{
		Type:       agent.EventPermission,
		Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "bash"},
	}})
	return tm.(Model)
}

func TestFrameHeightContract(t *testing.T) {
	for _, tc := range []struct{ cols, rows int }{
		{80, 24}, {80, 30}, {80, 40}, {40, 12}, {120, 40},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.cols, tc.rows), func(t *testing.T) {
			m := loadedModel(t, tc.cols, tc.rows)
			view := m.View()
			if h := lipgloss.Height(view); h != tc.rows {
				t.Fatalf("frame is %d rows, want %d:\n%s", h, tc.rows, view)
			}
			for _, ln := range strings.Split(view, "\n") {
				if w := lipgloss.Width(ln); w != tc.cols {
					t.Fatalf("line is %d wide, want %d: %q", w, tc.cols, ln)
				}
			}
			if got := m.lay.Transcript.Height(); got < minTranscriptRows {
				t.Fatalf("transcript kept %d rows, want at least %d", got, minTranscriptRows)
			}
			if got, want := m.chromeHeight(), tc.rows-m.lay.Transcript.Height(); got != want {
				t.Fatalf("chromeHeight %d, want %d", got, want)
			}
			if !strings.Contains(view, "TASKS") {
				t.Fatalf("tasks panel missing:\n%s", view)
			}
			if !strings.Contains(view, "Waiting for your answer") {
				t.Fatalf("spinner missing while a card is up:\n%s", view)
			}
			if !strings.Contains(view, "permission bash") {
				t.Fatalf("permission line missing:\n%s", view)
			}
		})
	}
}

func TestShortTerminalDegradesUnconditionally(t *testing.T) {
	m := loadedModel(t, 80, 24)
	m.input.SetValue(strings.Repeat("z", (80-composerPromptW)*5))
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	if m.lay.Degraded < forcedDegrade {
		t.Fatalf("degraded %d steps, want at least %d", m.lay.Degraded, forcedDegrade)
	}
	if m.lay.TasksRows != 0 || m.lay.Tasks.Height() != 1 {
		t.Fatalf("tasks panel should be header-only, got %d rows in %v", m.lay.TasksRows, m.lay.Tasks)
	}
	if m.lay.AgentRows > agentRowsShort {
		t.Fatalf("agent rows capped at %d, got %d", agentRowsShort, m.lay.AgentRows)
	}
	if m.lay.ComposerRows > composerShortRows {
		t.Fatalf("composer is %d rows, want at most %d", m.lay.ComposerRows, composerShortRows)
	}
	view := m.View()
	if strings.Contains(view, "Run go vet") {
		t.Fatalf("a header-only panel must not list rows:\n%s", view)
	}

	tall := loadedModel(t, 80, 30)
	if tall.lay.Degraded != 0 {
		t.Fatalf("30 rows fits, but degraded %d steps", tall.lay.Degraded)
	}
	if tall.lay.TasksRows == 0 {
		t.Fatalf("30 rows should list task rows:\n%s", tall.View())
	}
	if tall.lay.AgentRows != 4 {
		t.Fatalf("30 rows should keep 4 agent rows, got %d", tall.lay.AgentRows)
	}
}

func TestLayoutRegionsTileTheScreen(t *testing.T) {
	m := loadedModel(t, 100, 30)
	regions := []yRange{
		m.lay.Transcript, m.lay.Overlay, m.lay.Tasks, m.lay.Spinner,
		m.lay.Composer, m.lay.Agents, m.lay.Peek, m.lay.Modal, m.lay.Status,
	}
	y := 0
	for i, r := range regions {
		if r.Top != y {
			t.Fatalf("region %d starts at %d, want %d (%+v)", i, r.Top, y, m.lay)
		}
		y = r.Bottom
	}
	if y != m.lay.Height {
		t.Fatalf("regions cover %d rows, want %d", y, m.lay.Height)
	}
	if m.lay.Status.Row(m.lay.Status.Top) != 0 || m.lay.Status.Row(m.lay.Status.Top-1) != -1 {
		t.Fatal("Row should be region-relative and -1 outside")
	}
}

func TestTooSmallTerminal(t *testing.T) {
	isolateSkillsHome(t)
	m := New(Config{Session: NewStub(), Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)

	view := m.View()
	if !m.lay.TooSmall {
		t.Fatal("30x8 should be below the minimum")
	}
	if h := lipgloss.Height(view); h != 8 {
		t.Fatalf("too-small frame is %d rows, want 8:\n%s", h, view)
	}
	for _, ln := range strings.Split(view, "\n") {
		if w := lipgloss.Width(ln); w != 30 {
			t.Fatalf("line is %d wide, want 30: %q", w, ln)
		}
	}
	if !strings.Contains(view, "terminal too small") {
		t.Fatalf("missing the minimum-size message:\n%s", view)
	}

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !tm.(Model).quitting || cmd == nil {
		t.Fatal("ctrl+d must still quit from the too-small screen")
	}
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !tm.(Model).quitting || cmd == nil {
		t.Fatal("ctrl+c must still quit from the too-small screen")
	}

	// One row and one column more each way is a real frame again.
	tm, _ = m.Update(tea.WindowSizeMsg{Width: minFrameCols, Height: minFrameRows})
	m = tm.(Model)
	if m.lay.TooSmall {
		t.Fatalf("%dx%d is the minimum craze draws in", minFrameCols, minFrameRows)
	}
	if h := lipgloss.Height(m.View()); h != minFrameRows {
		t.Fatalf("minimum frame is %d rows, want %d", h, minFrameRows)
	}
}

func TestComposerAutogrowsAndClamps(t *testing.T) {
	m := sized(t)
	// A tall terminal, so the ceiling under test is the autogrow cap and not
	// the short-terminal degradation step.
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = tm.(Model)
	if m.lay.ComposerRows != 1 {
		t.Fatalf("an empty composer is %d rows, want 1", m.lay.ComposerRows)
	}
	if m.input.MaxHeight != 0 {
		t.Fatalf("MaxHeight is %d; setting it makes bubbles refuse new lines", m.input.MaxHeight)
	}

	inner := 80 - composerPromptW
	m.input.SetValue(strings.Repeat("x", inner*3))
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = tm.(Model)
	if m.lay.ComposerRows != 4 {
		t.Fatalf("a pasted paragraph is %d rows, want 4", m.lay.ComposerRows)
	}
	if m.input.Height() != m.lay.ComposerRows {
		t.Fatalf("SetHeight was not called: textarea is %d, layout says %d", m.input.Height(), m.lay.ComposerRows)
	}
	if h := lipgloss.Height(m.View()); h != 40 {
		t.Fatalf("a grown composer broke the height contract: %d rows", h)
	}

	m.input.SetValue(strings.Repeat("y", inner*12))
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = tm.(Model)
	if m.lay.ComposerRows != composerMaxRows {
		t.Fatalf("composer grew to %d rows, want the %d cap", m.lay.ComposerRows, composerMaxRows)
	}

	// Past the visible cap the buffer must still take new logical lines.
	m.input.SetValue("")
	for i := 0; i < 9; i++ {
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
		m = tm.(Model)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
		m = tm.(Model)
	}
	if got := m.input.LineCount(); got != 10 {
		t.Fatalf("composer holds %d logical lines, want 10", got)
	}
	if m.lay.ComposerRows != composerMaxRows {
		t.Fatalf("composer shows %d rows, want the %d cap", m.lay.ComposerRows, composerMaxRows)
	}
}

func TestEscKeepsTheDraft(t *testing.T) {
	m := sized(t)
	m.input.SetValue("half a thought")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.input.Value() != "half a thought" {
		t.Fatalf("esc cleared the composer: %q", m.input.Value())
	}
}

func TestCtrlTIsNotTranspose(t *testing.T) {
	m := sized(t)
	m.input.SetValue("ab")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = tm.(Model)
	if m.input.Value() != "ab" {
		t.Fatalf("ctrl+t reached the textarea: %q", m.input.Value())
	}
	if m.tasksState != tasksExpanded {
		t.Fatalf("ctrl+t should cycle the panel, state %v", m.tasksState)
	}
}
