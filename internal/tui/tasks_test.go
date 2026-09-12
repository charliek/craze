package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// todoModel is a started 100x30 model whose clock the test owns, so the linger
// can be driven without sleeping.
func todoModel(t *testing.T) (Model, *Stub, *time.Time) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	now, clock := fixedClock(t)
	stub.Clock = clock

	m := New(Config{Session: stub, Workspace: t.TempDir(), Model: "grok", Yolo: true})
	m.clock = clock
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	return tm.(Model), stub, now
}

func sendTodos(t *testing.T, m Model, stub *Stub, todos []agent.Todo) Model {
	t.Helper()
	stub.SetTodos(todos)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}})
	return tm.(Model)
}

// beat delivers one tick of the live chain, which is how a clock-driven linger
// reaches the screen in production.
func beat(t *testing.T, m Model) Model {
	t.Helper()
	tm, _ := m.Update(tickMsg{gen: m.tickGen})
	return tm.(Model)
}

func openTodos() []agent.Todo {
	return []agent.Todo{
		{ID: "1", Content: "Read main.go", Status: "in_progress"},
		{ID: "2", Content: "Edit main.go", Status: "pending"},
		{ID: "3", Content: "Run go vet", Status: "pending"},
	}
}

func closedTodos() []agent.Todo {
	return []agent.Todo{
		{ID: "1", Content: "Read main.go", Status: "completed"},
		{ID: "2", Content: "Edit main.go", Status: "completed"},
		{ID: "3", Content: "Run go vet", Status: "cancelled"},
	}
}

func TestTasksPanelAppearsOnFirstTodoEvent(t *testing.T) {
	m, stub, _ := todoModel(t)
	if m.tasksPanelVisible() {
		t.Fatal("no panel before the first todo event")
	}
	m = sendTodos(t, m, stub, openTodos())
	view := m.View()
	if !strings.Contains(view, "TASKS 0/3") {
		t.Fatalf("missing the compact header:\n%s", view)
	}
	if !strings.Contains(view, "▸ Read main.go") || !strings.Contains(view, "○ Run go vet") {
		t.Fatalf("in-progress first, then pending:\n%s", view)
	}
	if m.lay.Tasks.Height() != 4 {
		t.Fatalf("panel is %d rows, want header + 3", m.lay.Tasks.Height())
	}
	// The panel sits directly above the composer.
	if m.lay.Tasks.Bottom != m.lay.Spinner.Top || m.lay.Spinner.Bottom != m.lay.Composer.Top {
		t.Fatalf("panel is not pinned above the composer: %+v", m.lay)
	}
}

func TestTasksCompactCapsRowsAndFoldsClosed(t *testing.T) {
	m, stub, _ := todoModel(t)
	todos := []agent.Todo{{ID: "0", Content: "done one", Status: "completed"}}
	for i := 1; i <= 6; i++ {
		todos = append(todos, agent.Todo{ID: string(rune('a' + i)), Content: "open " + string(rune('a'+i)), Status: "pending"})
	}
	m = sendTodos(t, m, stub, todos)
	if got := m.lay.TasksRows; got != tasksCompactRows {
		t.Fatalf("compact body is %d rows, want %d", got, tasksCompactRows)
	}
	view := m.View()
	if !strings.Contains(view, "TASKS 1/7") {
		t.Fatalf("closed items fold into the count:\n%s", view)
	}
	if strings.Contains(view, "done one") {
		t.Fatalf("a completed item must not take a compact row:\n%s", view)
	}
}

func TestTasksCycleCompactExpandedHidden(t *testing.T) {
	m, stub, _ := todoModel(t)
	todos := append(openTodos(), agent.Todo{ID: "4", Content: "Write it up", Status: "completed"})
	m = sendTodos(t, m, stub, todos)
	if m.tasksState != tasksCompact {
		t.Fatalf("default state %v", m.tasksState)
	}

	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = tm.(Model)
	if m.tasksState != tasksExpanded {
		t.Fatalf("ctrl+t once: %v", m.tasksState)
	}
	if view := m.View(); !strings.Contains(view, "Write it up") {
		t.Fatalf("expanded lists completed items:\n%s", view)
	}

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = tm.(Model)
	if m.tasksState != tasksHidden || m.tasksPanelVisible() {
		t.Fatalf("ctrl+t twice: %v", m.tasksState)
	}
	if view := m.View(); strings.Contains(view, "TASKS") {
		t.Fatalf("hidden panel still drawn:\n%s", view)
	}

	// /tasks is the same cycle.
	m.input.SetValue("/tasks")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.tasksState != tasksCompact {
		t.Fatalf("/tasks: %v", m.tasksState)
	}
	if m.input.Value() != "" {
		t.Fatalf("the builtin should clear the composer, got %q", m.input.Value())
	}
}

func TestTasksHiddenPersistsThroughNewLists(t *testing.T) {
	m, stub, _ := todoModel(t)
	m = sendTodos(t, m, stub, openTodos())
	for i := 0; i < 2; i++ {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
		m = tm.(Model)
	}
	if m.tasksState != tasksHidden {
		t.Fatalf("state %v", m.tasksState)
	}

	next := []agent.Todo{
		{ID: "9", Content: "A whole new plan", Status: "in_progress"},
		{ID: "10", Content: "And another", Status: "pending"},
	}
	m = sendTodos(t, m, stub, next)
	if m.tasksPanelVisible() {
		t.Fatal("a new list must not unhide the panel")
	}
	if len(m.snap.Todos) != 2 || m.snap.Todos[0].Content != "A whole new plan" {
		t.Fatalf("hidden panel should still take the data: %+v", m.snap.Todos)
	}
	if view := m.View(); strings.Contains(view, "A whole new plan") {
		t.Fatalf("hidden panel drew a row:\n%s", view)
	}
}

func TestTasksLingerHidesAfterTenSeconds(t *testing.T) {
	m, stub, now := todoModel(t)
	m = sendTodos(t, m, stub, openTodos())
	m = sendTodos(t, m, stub, closedTodos())

	view := m.View()
	if !strings.Contains(view, "TASKS 3/3 ✓") {
		t.Fatalf("a finished list shows the tick:\n%s", view)
	}
	if !m.tasksLingering() {
		t.Fatal("the linger should be running")
	}
	if !m.wantFastTick() {
		t.Fatal("a linger keeps the fast chain")
	}

	*now = now.Add(tasksLinger - time.Second)
	m = beat(t, m)
	if !m.tasksPanelVisible() {
		t.Fatalf("hid after %s, want %s", tasksLinger-time.Second, tasksLinger)
	}

	*now = now.Add(2 * time.Second)
	m = beat(t, m)
	if m.tasksPanelVisible() {
		t.Fatal("the panel should be gone once the linger expired")
	}
	if view := m.View(); strings.Contains(view, "TASKS") {
		t.Fatalf("expired panel still drawn:\n%s", view)
	}
	if m.tasksLingering() {
		t.Fatal("an expired linger must not hold the fast chain open")
	}

	// The next list brings it back.
	m = sendTodos(t, m, stub, openTodos())
	if !m.tasksPanelVisible() {
		t.Fatal("a new todo event should show the panel again")
	}
}

func TestTasksReopenCancelsTheLinger(t *testing.T) {
	m, stub, now := todoModel(t)
	m = sendTodos(t, m, stub, openTodos())
	m = sendTodos(t, m, stub, closedTodos())
	if m.todosClosedAt.IsZero() {
		t.Fatal("the linger should have started")
	}

	*now = now.Add(5 * time.Second)
	reopened := closedTodos()
	reopened[2].Status = "in_progress"
	m = sendTodos(t, m, stub, reopened)
	if !m.todosClosedAt.IsZero() {
		t.Fatal("reopening an item must cancel the linger")
	}

	*now = now.Add(2 * tasksLinger)
	m = beat(t, m)
	if !m.tasksPanelVisible() {
		t.Fatalf("the panel should stay while work is open:\n%s", m.View())
	}
}
