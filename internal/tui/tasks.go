package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

const (
	// tasksCompactRows is how many open items the compact panel lists.
	tasksCompactRows = 4
	// tasksLinger keeps "TASKS n/n ✓" on screen after the last item closes.
	tasksLinger = 10 * time.Second
)

// tasksPanelState is the pinned cycle Ctrl+T and /tasks step through.
type tasksPanelState int

const (
	tasksCompact tasksPanelState = iota
	tasksExpanded
	tasksHidden
)

func (s tasksPanelState) next() tasksPanelState {
	switch s {
	case tasksCompact:
		return tasksExpanded
	case tasksExpanded:
		return tasksHidden
	default:
		return tasksCompact
	}
}

// cycleTasks is Ctrl+T and /tasks. Hidden persists: a later list updates the
// data without bringing the panel back.
func (m Model) cycleTasks() (tea.Model, tea.Cmd) {
	m.tasksState = m.tasksState.next()
	return m, nil
}

// noteTodoLifecycle drives the linger. A list where everything is closed
// starts the ten seconds; an update that reopens an item cancels it.
func (m *Model) noteTodoLifecycle(todos []agent.Todo) {
	if len(todos) == 0 {
		return
	}
	m.todosSeen = true
	if todosAllClosed(todos) {
		m.todosClosedAt = m.now()
		return
	}
	m.todosClosedAt = time.Time{}
}

func todosAllClosed(todos []agent.Todo) bool {
	if len(todos) == 0 {
		return false
	}
	for _, td := range todos {
		if !todoClosed(td.Status) {
			return false
		}
	}
	return true
}

func todoClosed(status string) bool {
	return status == "completed" || status == "cancelled"
}

// tasksLingering reports whether the closed panel is still counting down, so
// the tick chain knows to keep it moving until it disappears.
func (m Model) tasksLingering() bool {
	if m.todosClosedAt.IsZero() || !m.todosSeen || m.tasksState == tasksHidden {
		return false
	}
	return m.now().Sub(m.todosClosedAt) < tasksLinger
}

func (m Model) tasksPanelVisible() bool {
	if !m.todosSeen || m.tasksState == tasksHidden || len(m.snap.Todos) == 0 {
		return false
	}
	if !m.todosClosedAt.IsZero() && m.now().Sub(m.todosClosedAt) >= tasksLinger {
		return false
	}
	return true
}

func (m Model) tasksBodyRows() int {
	if !m.tasksPanelVisible() {
		return 0
	}
	return len(m.tasksPanelItems())
}

// tasksPanelItems is the panel body: compact lists the open items only,
// in-progress first, and folds everything closed into the header count.
func (m Model) tasksPanelItems() []agent.Todo {
	if m.tasksState == tasksExpanded {
		return m.snap.Todos
	}
	out := make([]agent.Todo, 0, len(m.snap.Todos))
	for _, td := range m.snap.Todos {
		if td.Status == "in_progress" {
			out = append(out, td)
		}
	}
	for _, td := range m.snap.Todos {
		if !todoClosed(td.Status) && td.Status != "in_progress" {
			out = append(out, td)
		}
	}
	if len(out) > tasksCompactRows {
		out = out[:tasksCompactRows]
	}
	return out
}

func (m Model) tasksHeader() string {
	closed := 0
	for _, td := range m.snap.Todos {
		if todoClosed(td.Status) {
			closed++
		}
	}
	head := fmt.Sprintf("TASKS %d/%d", closed, len(m.snap.Todos))
	if closed > 0 && closed == len(m.snap.Todos) {
		head += " ✓"
	}
	return head
}

// tasksView draws the panel into the rows the layout gave it; TasksRows is 0
// when degradation left only the header.
func (m Model) tasksView(lay frameLayout) string {
	if lay.Region(regionTasks).Empty() {
		return ""
	}
	rows := []string{renderSegs(m.width, seg{m.tasksHeader(), styleFG(m.theme.Title)})}
	items := m.tasksPanelItems()
	if len(items) > lay.TasksRows {
		items = items[:max(0, lay.TasksRows)]
	}
	rail := seg{"┃ ", styleFG(m.theme.Warn)}
	for _, td := range items {
		glyph, gst := m.todoGlyph(td.Status)
		rows = append(rows, renderSegs(m.width,
			rail,
			seg{glyph + " ", gst},
			seg{sanitizeLine(td.Content), m.todoTextStyle(td.Status)},
		))
	}
	return strings.Join(rows, "\n")
}

func (m Model) todoGlyph(status string) (string, lipgloss.Style) {
	switch status {
	case "in_progress":
		return "▸", styleFG(m.theme.Title)
	case "completed":
		return "✓", styleFG(m.theme.OK)
	case "cancelled":
		return "–", styleFG(m.theme.Dim)
	default:
		return "○", styleFG(m.theme.Dim)
	}
}

func (m Model) todoTextStyle(status string) lipgloss.Style {
	switch status {
	case "completed":
		return styleFG(m.theme.Dim).Strikethrough(true)
	case "cancelled":
		return styleFG(m.theme.Dim)
	default:
		return styleFG(m.theme.FG)
	}
}
