package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

func clickAt(t *testing.T, m Model, y int) Model {
	t.Helper()
	tm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      1,
		Y:      y,
	})
	return tm.(Model)
}

func wheel(t *testing.T, m Model, button tea.MouseButton) Model {
	t.Helper()
	tm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: button, X: 1, Y: 1})
	return tm.(Model)
}

// scrollable fills the transcript so the viewport has somewhere to go.
func scrollable(t *testing.T) Model {
	t.Helper()
	m := sized(t)
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "line %02d\n\n", i)
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: b.String()}})
	return tm.(Model)
}

func TestWheelScrollsTheTranscript(t *testing.T) {
	m := scrollable(t)
	if !m.vp.AtBottom() {
		t.Fatal("a fresh transcript sticks to the bottom")
	}
	bottom := m.vp.YOffset
	if bottom < 2*wheelLines {
		t.Fatalf("not enough scrollback to test with: offset %d", bottom)
	}

	m = wheel(t, m, tea.MouseButtonWheelUp)
	if got, want := m.vp.YOffset, bottom-wheelLines; got != want {
		t.Fatalf("wheel up moved to %d, want %d (%d lines per notch)", got, want, wheelLines)
	}
	m = wheel(t, m, tea.MouseButtonWheelDown)
	if got := m.vp.YOffset; got != bottom {
		t.Fatalf("wheel down moved to %d, want %d", got, bottom)
	}

	// A drag is not a scroll and not a click.
	tm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, Y: m.lay.Region(regionStatus).Top})
	if got := tm.(Model).vp.YOffset; got != bottom {
		t.Fatalf("a drag moved the viewport to %d", got)
	}
}

func TestClickHitTestsAgentRowsAfterAResize(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	m := agentModel(t, &now)
	m = applyInFlight(t, m, []agent.ToolEvent{
		taskTool("task-a", "job a", "in_progress"),
		taskTool("task-b", "job b", "in_progress"),
	})
	// The most recent update leads, so row 0 is task-b and row 1 is task-a.
	rowsTop := m.lay.Region(regionAgents).Top
	if got := clickAt(t, m, rowsTop+1); got.agentID != "task-a" || !got.agentPeek {
		t.Fatalf("click on row 1 selected %q (peek %v)", got.agentID, got.agentPeek)
	}

	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = tm.(Model)
	if m.lay.Region(regionAgents).Top == rowsTop {
		t.Fatal("the resize should have moved the agent rows")
	}
	// The old coordinate is transcript now, and hit-testing follows the layout.
	if got := clickAt(t, m, rowsTop); got.agentPeek {
		t.Fatalf("a stale row coordinate selected %q", got.agentID)
	}
	if got := clickAt(t, m, m.lay.Region(regionAgents).Top+1); got.agentID != "task-a" || !got.agentPeek {
		t.Fatalf("after the resize row 1 selected %q (peek %v)", got.agentID, got.agentPeek)
	}
	// The overflow row is not a sub-agent and selects nothing.
	if got := clickAt(t, m, m.lay.Region(regionAgents).Bottom); got.agentPeek {
		t.Fatal("a click below the rows selected one")
	}
}

func TestClickOnTheTasksHeaderCycles(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	todos := stubTodos()
	m.sess.(*Stub).SetTodos(todos)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTodos, Todos: todos}})
	m = tm.(Model)
	if m.lay.Region(regionTasks).Empty() {
		t.Fatalf("expected a tasks panel:\n%s", plainView(m))
	}

	next := clickAt(t, m, m.lay.Region(regionTasks).Top)
	if next.tasksState != tasksExpanded {
		t.Fatalf("clicking the header should cycle, state %v", next.tasksState)
	}
	// Rows are not clickable.
	same := clickAt(t, m, m.lay.Region(regionTasks).Top+1)
	if same.tasksState != m.tasksState {
		t.Fatalf("a task row cycled the panel to %v", same.tasksState)
	}
}

func TestClickPicksAModelRow(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.picking {
		t.Fatal("expected the model picker")
	}
	next := clickAt(t, m, m.lay.Region(regionOverlay).Top+pickerHeaderRows+1)
	if next.snap.CurrentModel != "fast" {
		t.Fatalf("clicking option 2 chose %q", next.snap.CurrentModel)
	}
	// The border and the title row are not options.
	same := clickAt(t, m, m.lay.Region(regionOverlay).Top)
	if same.snap.CurrentModel != "grok" || same.effortStep {
		t.Fatalf("the picker border is not an option: %q", same.snap.CurrentModel)
	}
}

func TestClicksAreIgnoredWhileACardIsUp(t *testing.T) {
	m := loadedModel(t, 100, 30)
	if !m.cardOpen() {
		t.Fatal("the fixture should have a card up")
	}
	next := clickAt(t, m, m.lay.Region(regionTasks).Top)
	if next.tasksState != m.tasksState {
		t.Fatal("a card owns the keyboard and the mouse")
	}
}

// TestWheelIsIgnoredWhileACardIsUp: the card owns the whole mouse, not only
// the clicks, so the transcript does not scroll away underneath it.
func TestWheelIsIgnoredWhileACardIsUp(t *testing.T) {
	m, stub := sizedCards(t)
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "line %02d\n\n", i)
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: b.String()}})
	m = cardEvent(t, tm.(Model), stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	bottom := m.vp.YOffset
	if bottom < 2*wheelLines {
		t.Fatalf("not enough scrollback to test with: offset %d", bottom)
	}
	if got := wheel(t, m, tea.MouseButtonWheelUp).vp.YOffset; got != bottom {
		t.Fatalf("the wheel scrolled to %d behind the card, want %d", got, bottom)
	}
	if got := wheel(t, m, tea.MouseButtonWheelDown).vp.YOffset; got != bottom {
		t.Fatalf("the wheel scrolled to %d behind the card, want %d", got, bottom)
	}
}
