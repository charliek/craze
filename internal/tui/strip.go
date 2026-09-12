package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

const (
	stripMaxRows      = 4
	stripActivityCols = 60
	stripPeekLines    = 6
)

var subagentTitleRe = regexp.MustCompile(`(?i)subagent|\btask\b`)

func isSubagentTitle(title string) bool {
	return title != "" && subagentTitleRe.MatchString(title)
}

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

func (m *Model) syncStrip() {
	items := m.stripItems()
	if len(items) == 0 {
		m.stripSel = 0
		m.stripID = ""
		m.stripPeek = false
		return
	}
	if m.stripID != "" {
		for i, t := range items {
			if t.ID == m.stripID {
				m.stripSel = i
				return
			}
		}
	}
	if m.stripSel >= len(items) {
		m.stripSel = len(items) - 1
	}
	if m.stripSel < 0 {
		m.stripSel = 0
	}
	m.stripID = items[m.stripSel].ID
}

func (m *Model) moveStrip(delta int) {
	items := m.stripItems()
	if len(items) == 0 {
		m.stripSel = 0
		m.stripID = ""
		m.stripPeek = false
		return
	}
	m.stripSel = (m.stripSel + delta) % len(items)
	if m.stripSel < 0 {
		m.stripSel += len(items)
	}
	m.stripID = items[m.stripSel].ID
}

func (m Model) stripItems() []agent.ToolEvent {
	byID := make(map[string]agent.ToolEvent, len(m.snap.Tools))
	inflight := make([]agent.ToolEvent, 0, len(m.snap.Tools))
	for _, t := range m.snap.Tools {
		if t.Status != "pending" && t.Status != "in_progress" {
			continue
		}
		inflight = append(inflight, t)
		if t.ID != "" {
			byID[t.ID] = t
		}
	}
	if len(inflight) == 0 {
		return nil
	}
	out := make([]agent.ToolEvent, 0, min(stripMaxRows, len(inflight)))
	seen := make(map[string]struct{}, len(inflight))
	for _, id := range m.toolTouch {
		t, ok := byID[id]
		if !ok {
			continue
		}
		out = append(out, t)
		seen[id] = struct{}{}
		if len(out) == stripMaxRows {
			return out
		}
	}
	for _, t := range inflight {
		if t.ID != "" {
			if _, ok := seen[t.ID]; ok {
				continue
			}
		}
		out = append(out, t)
		if len(out) == stripMaxRows {
			break
		}
	}
	return out
}

// stripRowsView draws at most limit rows; the cap comes from the layout, which
// is where degradation decided how many fit.
func (m Model) stripRowsView(limit int) string {
	items := m.stripItems()
	if limit <= 0 || len(items) == 0 {
		return ""
	}
	if len(items) > limit {
		items = items[:limit]
	}
	sel := m.stripSelection(len(items))
	var b strings.Builder
	for i, t := range items {
		line := formatStripRow(t)
		if m.width > 0 {
			line = clampWidth(line, m.width)
		}
		st := lipgloss.NewStyle().Foreground(m.theme.Dim)
		if i == sel {
			st = lipgloss.NewStyle().Foreground(m.theme.Title)
		}
		b.WriteString(st.Render(line))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) stripPeekView() string {
	items := m.stripItems()
	if !m.stripPeek || len(items) == 0 {
		return ""
	}
	peek := formatStripPeek(items[m.stripSelection(len(items))], m.width)
	if peek == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(m.theme.Dim).Render(peek)
}

func (m Model) peekRows() int {
	v := m.stripPeekView()
	if v == "" {
		return 0
	}
	return lipgloss.Height(v)
}

func (m Model) stripSelection(n int) int {
	if m.stripSel < 0 || m.stripSel >= n {
		return 0
	}
	return m.stripSel
}

func formatStripRow(t agent.ToolEvent) string {
	title := sanitizeLine(t.Title)
	if title == "" {
		title = sanitizeLine(t.Kind)
	}
	if isSubagentTitle(t.Title) {
		title = "subagent " + title
	}
	status := sanitizeLine(t.Status)
	if status == "" {
		status = "pending"
	}
	parts := []string{status, title}
	if act := compactActivity(t.ContentText); act != "" {
		parts = append(parts, act)
	}
	return strings.Join(parts, "  ")
}

func compactActivity(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	return clampWidth(s, stripActivityCols)
}

func formatStripPeek(t agent.ToolEvent, width int) string {
	var src []string
	if t.ContentText != "" {
		src = append(src, strings.Split(t.ContentText, "\n")...)
	}
	if t.RawInput != "" {
		src = append(src, t.RawInput)
	}
	lines := make([]string, 0, stripPeekLines)
	for _, ln := range src {
		ln = sanitizeLine(ln)
		if ln == "" {
			continue
		}
		if width > 0 {
			ln = clampWidth(ln, width)
		}
		lines = append(lines, ln)
		if len(lines) >= stripPeekLines {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
