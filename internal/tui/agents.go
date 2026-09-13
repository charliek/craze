package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

const (
	// agentRowsMax is the pinned cap; past it the rows end in "… +n more".
	agentRowsMax = 4
	// agentLinger keeps a finished sub-agent row up after it completed.
	agentLinger = 10 * time.Second
	// agentPeekLines caps the prompt peek.
	agentPeekLines = 8
)

func toolInFlight(status string) bool {
	return status == "pending" || status == "in_progress"
}

// noteToolUpdate keeps the most recently updated tool first; the agent rows
// are ordered by it.
func (m *Model) noteToolUpdate(id string) {
	if id == "" {
		return
	}
	next := make([]string, 0, len(m.toolTouch)+1)
	next = append(next, id)
	for _, x := range m.toolTouch {
		if x != id {
			next = append(next, x)
		}
	}
	m.toolTouch = next
}

// noteAgentTiming stamps when a sub-agent was first seen and when it closed,
// so a row can count up while it runs and linger for ten seconds after.
func (m *Model) noteAgentTiming(t *agent.ToolEvent) {
	if t == nil || t.ID == "" || !t.IsTask() {
		return
	}
	if m.agentStart == nil {
		m.agentStart = make(map[string]time.Time)
		m.agentDone = make(map[string]time.Time)
	}
	if _, ok := m.agentStart[t.ID]; !ok {
		m.agentStart[t.ID] = m.now()
	}
	if toolInFlight(t.Status) {
		delete(m.agentDone, t.ID)
		return
	}
	if _, ok := m.agentDone[t.ID]; !ok {
		m.agentDone[t.ID] = m.now()
	}
}

// agentItems are the sub-agent rows: every Task tool in flight plus the ones
// that closed inside the linger window, most recently updated first. There is
// no row for craze's own turn — the pinned "no main row".
func (m Model) agentItems() []agent.ToolEvent {
	if !m.showSubagents() {
		return nil
	}
	byID := make(map[string]agent.ToolEvent, len(m.snap.Tools))
	live := make([]agent.ToolEvent, 0, len(m.snap.Tools))
	for _, t := range m.snap.Tools {
		if !t.IsTask() || !m.agentVisible(t) {
			continue
		}
		live = append(live, t)
		if t.ID != "" {
			byID[t.ID] = t
		}
	}
	if len(live) == 0 {
		return nil
	}
	out := make([]agent.ToolEvent, 0, len(live))
	seen := make(map[string]struct{}, len(live))
	for _, id := range m.toolTouch {
		if t, ok := byID[id]; ok {
			out = append(out, t)
			seen[id] = struct{}{}
		}
	}
	for _, t := range live {
		if t.ID != "" {
			if _, ok := seen[t.ID]; ok {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// agentRowCap is how many sub-agent rows the frame draws: the cap degradation
// left. Before the first layout it is the full cap, so a selection made while
// the first events land is not stranded.
func (m Model) agentRowCap() int {
	if m.lay.Height == 0 {
		return agentRowsMax
	}
	return max(0, m.lay.AgentRows)
}

// visibleAgents are the rows actually on screen. Selection, the peek and
// clicks all work on these and never on a sub-agent hidden behind the
// "… +n more" row: what the arrow keys walk is what the frame highlights.
func (m Model) visibleAgents() []agent.ToolEvent {
	items := m.agentItems()
	if n := m.agentRowCap(); len(items) > n {
		items = items[:n]
	}
	return items
}

// agentVisible keeps a finished sub-agent for the linger window. One that
// closed before craze ever saw it running has no timing and is not shown.
func (m Model) agentVisible(t agent.ToolEvent) bool {
	if toolInFlight(t.Status) {
		return true
	}
	done, ok := m.agentDone[t.ID]
	if !ok {
		return false
	}
	return m.now().Sub(done) < agentLinger
}

// agentLingering reports whether a finished row is still counting down, so the
// tick chain keeps redrawing until it disappears.
func (m Model) agentLingering() bool {
	if !m.showSubagents() {
		return false
	}
	for _, t := range m.snap.Tools {
		if !t.IsTask() || toolInFlight(t.Status) {
			continue
		}
		if done, ok := m.agentDone[t.ID]; ok && m.now().Sub(done) < agentLinger {
			return true
		}
	}
	return false
}

// syncAgents re-finds the selection after the list changed. Selection is by
// tool id, never by index: the in-flight list reorders on every update. A
// selected sub-agent that has been pushed off the visible rows falls back to
// the first row, so the highlight, the peek and Enter always agree.
func (m *Model) syncAgents() {
	items := m.visibleAgents()
	if len(items) == 0 {
		m.agentSel = 0
		m.agentID = ""
		m.agentPeek = false
		return
	}
	if m.agentID != "" {
		for i, t := range items {
			if t.ID == m.agentID {
				m.agentSel = i
				return
			}
		}
	}
	if m.agentSel >= len(items) {
		m.agentSel = len(items) - 1
	}
	if m.agentSel < 0 {
		m.agentSel = 0
	}
	m.agentID = items[m.agentSel].ID
}

func (m *Model) moveAgent(delta int) {
	items := m.visibleAgents()
	if len(items) == 0 {
		m.agentSel = 0
		m.agentID = ""
		m.agentPeek = false
		return
	}
	m.agentSel = (m.agentSel + delta) % len(items)
	if m.agentSel < 0 {
		m.agentSel += len(items)
	}
	m.agentID = items[m.agentSel].ID
}

// selectAgent points the selection at one drawn row and peeks at it, which is
// what a click on the row does. A click on the "… +n more" row selects
// nothing: it is not a sub-agent.
func (m *Model) selectAgent(i int) {
	items := m.visibleAgents()
	if i < 0 || i >= len(items) {
		return
	}
	m.agentSel = i
	m.agentID = items[i].ID
	m.agentPeek = true
}

func (m Model) agentSelection(n int) int {
	if m.agentSel < 0 || m.agentSel >= n {
		return 0
	}
	return m.agentSel
}

// agentRegionRows is how many screen rows n sub-agents take under a cap: the
// rows that fit, plus the "… +n more" row when the cap bites.
func agentRegionRows(n, limit int) int {
	switch {
	case n <= 0 || limit <= 0:
		return 0
	case n <= limit:
		return n
	default:
		return limit + 1
	}
}

// agentRowsView draws the rows degradation left, and counts the sub-agents it
// could not fit into the overflow row.
func (m Model) agentRowsView() string {
	items := m.visibleAgents()
	if len(items) == 0 {
		return ""
	}
	more := len(m.agentItems()) - len(items)
	sel := m.agentSelection(len(items))
	rows := make([]string, 0, len(items)+1)
	for i, t := range items {
		glyph, gst := m.agentGlyph(t)
		st := styleFG(m.theme.Dim)
		if i == sel {
			st = styleFG(m.theme.Accent)
		}
		rows = append(rows, renderSegs(m.width,
			seg{glyph + " ", gst},
			seg{"task  ", styleFG(m.theme.ToolKind)},
			seg{agentDesc(t), st},
			seg{"  " + m.agentSuffix(t), styleFG(m.theme.Dim)},
		))
	}
	if more > 0 {
		rows = append(rows, renderSegs(m.width,
			seg{fmt.Sprintf("… +%d more", more), styleFG(m.theme.Dim)}))
	}
	return strings.Join(rows, "\n")
}

func (m Model) agentGlyph(t agent.ToolEvent) (string, lipgloss.Style) {
	if toolInFlight(t.Status) {
		return "○", styleFG(m.theme.Dim)
	}
	if t.Status == "failed" {
		return "✗", styleFG(m.theme.Err)
	}
	return "✓", styleFG(m.theme.OK)
}

func agentDesc(t agent.ToolEvent) string { return taskDesc(&t) }

// agentSuffix counts up while the sub-agent runs, and becomes its duration and
// model once cursor's receipt has landed.
func (m Model) agentSuffix(t agent.ToolEvent) string {
	if toolInFlight(t.Status) {
		start, ok := m.agentStart[t.ID]
		if !ok {
			return ""
		}
		return formatElapsed(m.now().Sub(start))
	}
	if t.Task == nil {
		return ""
	}
	out := formatMillis(t.Task.DurationMs)
	if t.Task.Receipt && t.Task.Model != "" {
		if out != "" {
			out += " · "
		}
		out += shortModelName(t.Task.Model)
	}
	return out
}

// agentPeekView is the selected sub-agent's own prompt, wrapped, drawn between
// the composer and the status rows.
func (m Model) agentPeekView() string {
	items := m.visibleAgents()
	if !m.agentPeek || len(items) == 0 {
		return ""
	}
	t := items[m.agentSelection(len(items))]
	text := ""
	if t.Task != nil {
		text = t.Task.Prompt
	}
	if strings.TrimSpace(text) == "" {
		text = t.RawInput
	}
	text = sanitizeLine(text)
	if text == "" {
		return ""
	}
	rows := hangingRows(text, "  ", "  ", m.width, styleFG(m.theme.Dim))
	if len(rows) > agentPeekLines {
		rows = rows[:agentPeekLines]
	}
	return strings.Join(rows, "\n")
}

func (m Model) peekRows() int {
	v := m.agentPeekView()
	if v == "" {
		return 0
	}
	return lipgloss.Height(v)
}
