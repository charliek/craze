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
)

func toolInFlight(status string) bool {
	return status == "pending" || status == "in_progress"
}

func subagentRunning(s agent.SubagentInfo) bool {
	return s.Status == "" || s.Status == agent.SubagentRunning
}

func subagentTerminal(s agent.SubagentInfo) bool {
	return s.Status == agent.SubagentCompleted || s.Status == agent.SubagentFailed || s.Status == agent.SubagentCancelled
}

func (m *Model) noteAgentStart(id string) {
	if id == "" {
		return
	}
	if m.agentStart == nil {
		m.agentStart = make(map[string]time.Time)
	}
	if _, ok := m.agentStart[id]; !ok {
		m.agentStart[id] = m.now()
	}
}

func (m *Model) noteAgentDone(id string) {
	if id == "" {
		return
	}
	if m.agentDone == nil {
		m.agentDone = make(map[string]time.Time)
	}
	if _, ok := m.agentDone[id]; !ok {
		m.agentDone[id] = m.now()
	}
}

func (m Model) subagentByID(id string) (agent.SubagentInfo, bool) {
	if id == "" {
		return agent.SubagentInfo{}, false
	}
	for i := range m.snap.Subagents {
		if m.snap.Subagents[i].ID == id {
			return m.snap.Subagents[i], true
		}
	}
	if m.tombstone != nil && m.tombstone.ID == id {
		return *m.tombstone, true
	}
	return agent.SubagentInfo{}, false
}

func (m Model) liveSubIDs() map[string]struct{} {
	live := make(map[string]struct{}, len(m.snap.Subagents))
	for i := range m.snap.Subagents {
		if id := m.snap.Subagents[i].ID; id != "" {
			live[id] = struct{}{}
		}
	}
	return live
}

func (m Model) anySubagentRunning() bool {
	for i := range m.snap.Subagents {
		if subagentRunning(m.snap.Subagents[i]) {
			return true
		}
	}
	if m.tombstone != nil && subagentRunning(*m.tombstone) {
		return true
	}
	return false
}

func (m Model) viewedInfo() (agent.SubagentInfo, bool) {
	return m.subagentByID(m.viewing)
}

func (m Model) viewedRunning() bool {
	info, ok := m.viewedInfo()
	return ok && subagentRunning(info)
}

func agentLabel(s agent.SubagentInfo) string {
	t := strings.TrimSpace(s.SubagentType)
	if t != "" && !strings.EqualFold(t, "unspecified") {
		return sanitizeLine(t)
	}
	return "task"
}

// agentItems lists the rows in spawn order (the snapshot's order), which
// never changes while a sub-agent is alive, so ↑/↓ selection and the reserved
// viewed slot stay put while children stream. The viewed sub-agent is appended
// only when its record has already left the snapshot (tombstone).
func (m Model) agentItems() []agent.SubagentInfo {
	if !m.showSubagents() {
		return nil
	}
	out := make([]agent.SubagentInfo, 0, len(m.snap.Subagents)+1)
	seen := false
	for _, s := range m.snap.Subagents {
		if !m.agentVisible(s) {
			continue
		}
		out = append(out, s)
		if s.ID != "" && s.ID == m.viewing {
			seen = true
		}
	}
	if m.viewing != "" && !seen {
		if info, ok := m.subagentByID(m.viewing); ok {
			out = append(out, info)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m Model) agentRowCap() int {
	if m.lay.Height == 0 {
		return agentRowsMax
	}
	return max(0, m.lay.AgentRows)
}

func (m Model) visibleAgents() []agent.SubagentInfo {
	items := m.agentItems()
	capN := m.agentRowCap()
	if capN <= 0 {
		return nil
	}
	if len(items) > capN {
		items = items[:capN]
	}
	if m.viewing == "" {
		return items
	}
	for _, s := range items {
		if s.ID == m.viewing {
			return items
		}
	}
	info, ok := m.subagentByID(m.viewing)
	if !ok {
		return items
	}
	if len(items) == capN {
		items[capN-1] = info
		return items
	}
	return append(items, info)
}

func (m Model) agentVisible(s agent.SubagentInfo) bool {
	if subagentRunning(s) {
		return true
	}
	if m.viewing != "" && s.ID == m.viewing {
		return true
	}
	done, ok := m.agentDone[s.ID]
	if !ok {
		return false
	}
	return m.now().Sub(done) < agentLinger
}

func (m Model) agentLingering() bool {
	if !m.showSubagents() {
		return false
	}
	for _, s := range m.snap.Subagents {
		if subagentRunning(s) || (m.viewing != "" && s.ID == m.viewing) {
			continue
		}
		if done, ok := m.agentDone[s.ID]; ok && m.now().Sub(done) < agentLinger {
			return true
		}
	}
	return false
}

func (m *Model) pruneSubs() {
	live := m.liveSubIDs()
	if m.viewing != "" {
		if info, ok := liveInfo(m.snap.Subagents, m.viewing); ok {
			cp := info
			m.tombstone = &cp
		}
	}
	for id := range m.subs {
		if _, ok := live[id]; ok {
			continue
		}
		if m.viewing != "" && id == m.viewing {
			continue
		}
		delete(m.subs, id)
	}
	for id := range m.agentStart {
		if _, ok := live[id]; ok {
			continue
		}
		if m.viewing != "" && id == m.viewing {
			continue
		}
		delete(m.agentStart, id)
		delete(m.agentDone, id)
	}
	// A record whose only sighting was its finish has a done stamp and no
	// start stamp; it has to leave too, or a later record with the same id
	// would measure its linger from this stale clock.
	for id := range m.agentDone {
		if _, ok := live[id]; ok {
			continue
		}
		if m.viewing != "" && id == m.viewing {
			continue
		}
		delete(m.agentDone, id)
	}
}

func liveInfo(subs []agent.SubagentInfo, id string) (agent.SubagentInfo, bool) {
	for i := range subs {
		if subs[i].ID == id {
			return subs[i], true
		}
	}
	return agent.SubagentInfo{}, false
}

func (m *Model) syncAgents() {
	if m.viewing != "" {
		m.agentID = m.viewing
	}
	items := m.visibleAgents()
	if len(items) == 0 {
		m.agentSel = 0
		if m.viewing == "" {
			m.agentID = ""
			if m.agentFocus {
				m.focusComposer()
			}
		}
		return
	}
	if m.agentID != "" {
		for i, s := range items {
			if s.ID == m.agentID {
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
		if m.viewing == "" {
			m.agentID = ""
		}
		return
	}
	m.agentSel = min(max(m.agentSel+delta, 0), len(items)-1)
	m.agentID = items[m.agentSel].ID
}

// rowsFocused says whether a row carries the selection mark: while the
// keyboard is on the rows, or while a sub-agent is being viewed (its row is
// the one the view came from).
func (m Model) rowsFocused() bool {
	return m.agentFocus || m.viewing != ""
}

func (m *Model) selectAgent(i int) {
	items := m.visibleAgents()
	if i < 0 || i >= len(items) {
		return
	}
	m.agentSel = i
	m.agentID = items[i].ID
	m.enterView(items[i].ID)
}

func (m Model) agentSelection(n int) int {
	if m.agentSel < 0 || m.agentSel >= n {
		return 0
	}
	return m.agentSel
}

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

func (m Model) agentRowsView() string {
	items := m.visibleAgents()
	if len(items) == 0 {
		return ""
	}
	more := len(m.agentItems()) - len(items)
	sel := -1
	if m.rowsFocused() {
		sel = m.agentSelection(len(items))
	}
	rows := make([]string, 0, len(items)+1)
	for i, s := range items {
		glyph, gst := m.agentGlyph(s)
		st := styleFG(m.theme.Dim)
		if i == sel {
			st = styleFG(m.theme.Accent)
		}
		label := agentLabel(s)
		suffix := m.agentSuffix(s)
		rows = append(rows, m.agentRow(i == sel, glyph, gst, label, sanitizeLine(s.Description), suffix, st))
	}
	if more > 0 {
		rows = append(rows, renderSegs(m.width,
			seg{agentGutterBlank + fmt.Sprintf("… +%d more", more), styleFG(m.theme.Dim)}))
	}
	return strings.Join(rows, "\n")
}

// The gutter marks the selected row the way the composer marks its prompt,
// so ↑/↓ have a target that reads even where the accent colour is faint.
const (
	agentGutterMark  = "❯ "
	agentGutterBlank = "  "
)

func (m Model) agentRow(selected bool, glyph string, gst lipgloss.Style, label, desc, suffix string, descSt lipgloss.Style) string {
	fixed := len(agentGutterBlank) + 2 + lipgloss.Width(label)
	if suffix != "" {
		fixed += 2 + lipgloss.Width(suffix)
	}
	if desc != "" {
		if avail := m.width - fixed - 2; avail >= 1 {
			desc = clampWidth(desc, avail)
		} else {
			desc = ""
		}
	}
	gutter := seg{agentGutterBlank, styleFG(m.theme.Dim)}
	if selected {
		gutter = seg{agentGutterMark, styleFG(m.theme.Accent)}
	}
	segs := []seg{gutter, {glyph + " ", gst}, {label, styleFG(m.theme.ToolKind)}}
	if desc != "" {
		segs = append(segs, seg{"  " + desc, descSt})
	}
	if suffix != "" {
		segs = append(segs, seg{"  " + suffix, styleFG(m.theme.Dim)})
	}
	return renderSegs(m.width, segs...)
}

func (m Model) agentGlyph(s agent.SubagentInfo) (string, lipgloss.Style) {
	if m.viewing != "" && s.ID == m.viewing {
		return "●", styleFG(m.theme.Accent)
	}
	if subagentRunning(s) {
		return "○", styleFG(m.theme.Dim)
	}
	switch s.Status {
	case agent.SubagentFailed:
		return "✗", styleFG(m.theme.Err)
	case agent.SubagentCancelled:
		return "–", styleFG(m.theme.Dim)
	default:
		return "✓", styleFG(m.theme.OK)
	}
}

func (m Model) agentSuffix(s agent.SubagentInfo) string {
	if subagentRunning(s) {
		start, ok := m.agentStart[s.ID]
		if !ok {
			return ""
		}
		out := formatElapsed(0)
		if !m.frozen {
			out = formatElapsed(m.now().Sub(start))
		}
		if tok := formatTokens(s.TokensUsed); tok != "" {
			out += " · " + tok
		}
		return out
	}
	out := formatMillis(s.DurationMs)
	if name := shortModelName(s.Model); name != "" {
		if out != "" {
			out += " · "
		}
		out += name
	}
	return out
}

func formatTokens(n int) string {
	if n <= 0 {
		return ""
	}
	if n < 1000 {
		return fmt.Sprintf("%d tok", n)
	}
	v := float64(n) / 1000
	if v >= 10 {
		return fmt.Sprintf("%.0fk tok", v)
	}
	return fmt.Sprintf("%.1fk tok", v)
}
