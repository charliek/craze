package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// agentModel is a started model on a clock the test owns, tall enough that no
// degradation step caps the agent rows.
func agentModel(t *testing.T, now *time.Time) Model {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.Clock = func() time.Time { return *now }
	m := New(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
	})
	m.clock = func() time.Time { return *now }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	return tm.(Model)
}

// taskTool is one sub-agent call with the fields the rows read.
func taskTool(id, desc, status string) agent.ToolEvent {
	return agent.ToolEvent{
		ID:       id,
		Kind:     "other",
		ToolName: "task",
		Title:    "Task: " + desc,
		Status:   status,
		Task:     &agent.TaskInfo{Description: desc, Prompt: "peek prompt for " + id},
	}
}

func finishedTaskTool(id, desc string) agent.ToolEvent {
	t := taskTool(id, desc, "completed")
	t.Task = &agent.TaskInfo{
		Description: desc,
		Prompt:      "peek prompt for " + id,
		DurationMs:  8010,
		Model:       "cursor-grok-4.6-high-fast",
		Receipt:     true,
	}
	return t
}

func TestAgentRowsSitBelowTheStatusRows(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{
		taskTool("task-1", "count main.go lines", "in_progress"),
		{ID: "sh-1", Kind: "execute", Title: "Shell", Status: "in_progress"},
	})

	view := plainView(m)
	below, ok := belowStatus(view)
	if !ok {
		t.Fatalf("status rows missing:\n%s", view)
	}
	if !strings.Contains(below, "count main.go lines") {
		t.Fatalf("the sub-agent row belongs under the status rows:\n%s", view)
	}
	// §3.9: only Task tools get a row, and there is no row for craze itself.
	if strings.Contains(below, "Shell") {
		t.Fatalf("a shell tool must never be an agent row:\n%s", below)
	}
	if strings.Contains(below, "main ") {
		t.Fatalf("there is no main row:\n%s", below)
	}
	if before, _ := belowComposer(view); strings.Contains(before, "count main.go lines") {
		t.Fatalf("the rows moved above the status rows:\n%s", view)
	}
	if !strings.Contains(view, "← 1 agent") {
		t.Fatalf("status row 2 should count the sub-agent:\n%s", view)
	}
}

func TestAgentRowsCapWithMoreRow(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	tools := make([]agent.ToolEvent, 0, 6)
	for i := 0; i < 6; i++ {
		tools = append(tools, taskTool(fmt.Sprintf("task-%d", i), fmt.Sprintf("job %d", i), "in_progress"))
	}
	m = applyInFlight(t, m, tools)

	if m.lay.AgentRows != agentRowsMax {
		t.Fatalf("drew %d rows, want the %d cap", m.lay.AgentRows, agentRowsMax)
	}
	if got := m.lay.Region(regionAgents).Height(); got != agentRowsMax+1 {
		t.Fatalf("the region is %d rows, want the cap plus the overflow row", got)
	}
	view := plainView(m)
	if !strings.Contains(view, "… +2 more") {
		t.Fatalf("missing the overflow row:\n%s", view)
	}
	if strings.Count(view, "○ task") != agentRowsMax {
		t.Fatalf("want %d listed rows:\n%s", agentRowsMax, view)
	}
}

func TestAgentRowLingersThenGoes(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})

	now = now.Add(4 * time.Second)
	m = poke(t, m)
	if got := rowsOf(t, m); !strings.Contains(got, "○ task  count lines  4s") {
		t.Fatalf("a running row counts up:\n%s", got)
	}

	m = applyInFlight(t, m, []agent.ToolEvent{finishedTaskTool("task-1", "count lines")})
	if got := rowsOf(t, m); !strings.Contains(got, "✓ task  count lines  8.0s · grok-4.6-high-fast") {
		t.Fatalf("a finished row shows duration and model:\n%s", got)
	}

	now = now.Add(agentLinger - time.Second)
	m = poke(t, m)
	if got := rowsOf(t, m); !strings.Contains(got, "✓ task  count lines") {
		t.Fatalf("the row should linger for %s:\n%s", agentLinger, got)
	}

	now = now.Add(2 * time.Second)
	m = poke(t, m)
	if got := rowsOf(t, m); strings.Contains(got, "count lines") {
		t.Fatalf("the row should be gone after %s:\n%s", agentLinger, got)
	}
	if !m.lay.Region(regionAgents).Empty() {
		t.Fatalf("the region should be empty, got %v", m.lay.Region(regionAgents))
	}
}

// poke re-runs Update so the layout is recomputed against the moved clock.
func poke(t *testing.T, m Model) Model {
	t.Helper()
	tm, _ := m.Update(refreshSnapMsg{})
	return tm.(Model)
}

// rowsOf is the agent-row band alone; the transcript has its own row per tool
// call and would otherwise answer for it.
func rowsOf(t *testing.T, m Model) string {
	t.Helper()
	below, ok := belowStatus(plainView(m))
	if !ok {
		t.Fatalf("status rows missing:\n%s", plainView(m))
	}
	return below
}

func TestAgentPeekEnterEsc(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m.status = statusWorking
	m.input.SetValue("")
	// Settle the tick chain first, so a returned command means the handler's
	// own command and not the batched tick.
	m = poke(t, m)

	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("enter on an empty composer peeks, it does not send")
	}
	if !m.agentPeek {
		t.Fatal("expected a peek")
	}
	below, ok := belowComposer(plainView(m))
	if !ok {
		t.Fatalf("composer/status missing:\n%s", plainView(m))
	}
	if !strings.Contains(below, "peek prompt for task-1") {
		t.Fatalf("the peek shows the sub-agent's prompt, between composer and status:\n%s", below)
	}

	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.agentPeek {
		t.Fatal("esc should close the peek")
	}
	if cmd != nil {
		t.Fatal("esc on a peek must not cancel the turn")
	}
	if m.status != statusWorking {
		t.Fatalf("status %s, want working", m.status)
	}
	if below, _ := belowComposer(plainView(m)); strings.Contains(below, "peek prompt") {
		t.Fatalf("the peek is still drawn:\n%s", below)
	}
}

func TestAgentNavArrowsOnlyAndTypingUnaffected(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{
		taskTool("task-a", "job a", "in_progress"),
		taskTool("task-b", "job b", "in_progress"),
	})
	// The most recently updated sub-agent leads the list, and the selection
	// stays on the first row craze ever saw: task-a, now the second row.
	if m.agentID != "task-a" || m.agentSel != 1 {
		t.Fatalf("selection starts at %q (row %d)", m.agentID, m.agentSel)
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.agentID != "task-b" {
		t.Fatalf("down selected %q", m.agentID)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if m.agentID != "task-a" {
		t.Fatalf("up selected %q", m.agentID)
	}

	// ctrl+p / ctrl+n belong to the textarea, not the rows.
	sel := m.agentID
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	m = tm.(Model)
	if m.agentID != sel {
		t.Fatalf("ctrl+n/ctrl+p moved the selection to %q", m.agentID)
	}

	m.input.SetValue("hey")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.agentID != sel {
		t.Fatal("down with composer text must not move the selection")
	}
}

func TestAgentKeepsSelectionByIDAcrossAReorder(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	tools := []agent.ToolEvent{
		taskTool("task-a", "job a", "in_progress"),
		taskTool("task-b", "job b", "in_progress"),
	}
	m = applyInFlight(t, m, tools)
	if m.agentID != "task-a" || m.agentSel != 1 {
		t.Fatalf("start: %q at row %d", m.agentID, m.agentSel)
	}

	// task-a updates, so it moves to the front and the list reorders under the
	// selection. Selection is by id, so the index has to follow it up a row.
	stub := m.sess.(*Stub)
	stub.SetTools(tools)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &tools[0]}})
	m = tm.(Model)

	items := m.agentItems()
	if len(items) != 2 || items[0].ID != "task-a" {
		t.Fatalf("expected task-a first after its update, got %+v", items)
	}
	if m.agentID != "task-a" {
		t.Fatalf("selection jumped to %q", m.agentID)
	}
	if m.agentSel != 0 || items[m.agentSel].ID != "task-a" {
		t.Fatalf("index %d points at %q", m.agentSel, items[m.agentSel].ID)
	}
}

func TestAgentRowStaysOneCleanRow(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	tool := taskTool("task-1", "x", "in_progress")
	tool.Title = "Task: Count\x1b]0;x\x07 main.go\nlines"
	tool.Task = &agent.TaskInfo{Description: "Count\x1b]0;x\x07 main.go\nlines"}
	m = applyInFlight(t, m, []agent.ToolEvent{tool})

	row := plain(m.agentRowsView())
	if strings.Count(row, "\n") != 0 {
		t.Fatalf("one sub-agent is one row: %q", row)
	}
	if strings.ContainsAny(row, "\n\x1b\x07") {
		t.Fatalf("agent row still has control characters: %q", row)
	}
	if strings.Contains(row, "]0;x") || strings.Contains(row, "task-1") {
		t.Fatalf("escape sequence or tool id reached the row: %q", row)
	}
}

// TestAgentSelectionNeverLeavesTheVisibleRows is the §3.9 cap read strictly:
// a sub-agent hidden behind "… +n more" is not on screen, so the arrows must
// not walk onto it and Enter must not peek at it.
func TestAgentSelectionNeverLeavesTheVisibleRows(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	tools := make([]agent.ToolEvent, 0, 6)
	for i := 0; i < 6; i++ {
		tools = append(tools, taskTool(fmt.Sprintf("task-%d", i), fmt.Sprintf("job %d", i), "in_progress"))
	}
	m = applyInFlight(t, m, tools)
	if m.lay.AgentRows != agentRowsMax {
		t.Fatalf("fixture draws %d rows, want the %d cap", m.lay.AgentRows, agentRowsMax)
	}

	// The first sub-agent craze saw is now row 5, behind the overflow row, so
	// the selection has to have fallen back onto a visible one.
	visible := map[string]bool{}
	for _, t := range m.visibleAgents() {
		visible[t.ID] = true
	}
	if len(visible) != agentRowsMax {
		t.Fatalf("visible rows: %d", len(visible))
	}
	if !visible[m.agentID] {
		t.Fatalf("selection %q is not on screen (visible %v)", m.agentID, visible)
	}

	// Walking all the way round only ever lands on drawn rows, and lands on
	// each of them.
	seen := map[string]bool{}
	for i := 0; i < agentRowsMax*2; i++ {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = tm.(Model)
		if !visible[m.agentID] {
			t.Fatalf("step %d selected the hidden %q", i, m.agentID)
		}
		if m.agentSel >= m.lay.AgentRows {
			t.Fatalf("step %d selected row %d of %d drawn", i, m.agentSel, m.lay.AgentRows)
		}
		seen[m.agentID] = true
	}
	if len(seen) != agentRowsMax {
		t.Fatalf("the arrows reached %d of %d drawn rows", len(seen), agentRowsMax)
	}

	// And the peek shows the row the frame highlights.
	m.input.SetValue("")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	below, ok := belowComposer(plainView(m))
	if !ok {
		t.Fatalf("composer/status missing:\n%s", plainView(m))
	}
	if !strings.Contains(below, "peek prompt for "+m.agentID) {
		t.Fatalf("peek is not the highlighted row %q:\n%s", m.agentID, below)
	}
	rows := rowsOf(t, m)
	if !strings.Contains(rows, "… +2 more") {
		t.Fatalf("the overflow row went missing:\n%s", rows)
	}
}

func TestDegradationShrinksTheSelectableRows(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	tools := make([]agent.ToolEvent, 0, 4)
	for i := 0; i < 4; i++ {
		tools = append(tools, taskTool(fmt.Sprintf("task-%d", i), fmt.Sprintf("job %d", i), "in_progress"))
	}
	m = applyInFlight(t, m, tools)
	if len(m.visibleAgents()) != 4 {
		t.Fatalf("30 rows should draw all four: %d", len(m.visibleAgents()))
	}

	// A short terminal caps the rows at two; the selection follows.
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	if m.lay.AgentRows != agentRowsShort {
		t.Fatalf("degraded to %d rows, want %d", m.lay.AgentRows, agentRowsShort)
	}
	if got := len(m.visibleAgents()); got != agentRowsShort {
		t.Fatalf("%d selectable rows, want %d", got, agentRowsShort)
	}
	if m.agentSel >= agentRowsShort {
		t.Fatalf("selection sits at row %d of %d drawn", m.agentSel, agentRowsShort)
	}
}

// TestPeekNeverShowsAHiddenSubAgent pins the peek on its own: even handed an
// out-of-window index it draws a row that is on screen, so the box and the
// highlight can never name different sub-agents.
func TestPeekNeverShowsAHiddenSubAgent(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	tools := make([]agent.ToolEvent, 0, 6)
	for i := 0; i < 6; i++ {
		tools = append(tools, taskTool(fmt.Sprintf("task-%d", i), fmt.Sprintf("job %d", i), "in_progress"))
	}
	m = applyInFlight(t, m, tools)

	all := m.agentItems()
	hidden := all[len(all)-1]
	m.agentSel = len(all) - 1
	m.agentID = hidden.ID
	m.agentPeek = true

	peek := plain(m.agentPeekView())
	if strings.Contains(peek, "peek prompt for "+hidden.ID) {
		t.Fatalf("the peek shows %q, which is behind the overflow row: %q", hidden.ID, peek)
	}
	if !strings.Contains(peek, "peek prompt for "+m.visibleAgents()[0].ID) {
		t.Fatalf("the peek should fall back to the highlighted row: %q", peek)
	}
}
