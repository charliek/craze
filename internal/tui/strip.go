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

func (m Model) stripView() string {
	items := m.stripItems()
	if len(items) == 0 {
		return ""
	}
	sel := m.stripSel
	if sel < 0 || sel >= len(items) {
		sel = 0
	}
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
	if m.stripPeek {
		if peek := formatStripPeek(items[sel], m.width); peek != "" {
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Dim).Render(peek))
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatStripRow(t agent.ToolEvent) string {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = t.Kind
	}
	if isSubagentTitle(t.Title) {
		title = "subagent " + title
	}
	status := t.Status
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
		ln = strings.TrimSpace(ln)
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

func clampWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	limit := w - 1
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}

func clampWidthTail(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	rs := []rune(s)
	var kept []rune
	used := 0
	limit := w - 1
	for i := len(rs) - 1; i >= 0; i-- {
		rw := lipgloss.Width(string(rs[i]))
		if used+rw > limit {
			break
		}
		kept = append(kept, rs[i])
		used += rw
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return "…" + string(kept)
}
