package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

func (m *Model) ensureSub(id string) *pane {
	if id == "" {
		return m.main
	}
	if m.subs == nil {
		m.subs = make(map[string]*pane)
	}
	if t := m.subs[id]; t != nil {
		return t
	}
	t := newSubPane()
	m.subs[id] = t
	return t
}

func (m *Model) enterView(id string) {
	if id == "" || m.viewing == id {
		return
	}
	if m.viewing == "" {
		m.storeViewport(m.main)
	} else {
		m.storeViewport(m.cur())
	}
	m.viewing = id
	m.agentID = id
	m.agentFocus = false
	m.input.Blur()
	tr := m.ensureSub(id)
	first := tr.transcriptRows == nil
	if !m.showSubagentTranscript() {
		m.rebuildReceiptTranscript(id)
	}
	if info, ok := m.subagentByID(id); ok {
		cp := info
		m.tombstone = &cp
	}
	if first {
		m.setViewportContent(true)
		return
	}
	// setViewportContent stores the viewport back into tr, so the saved
	// position has to be read before the call, not after it.
	stick, off := tr.atBottom, tr.yOffset
	m.setViewportContent(stick)
	if !stick {
		m.vp.SetYOffset(off)
		m.storeViewport(tr)
	}
}

func (m *Model) leaveView() {
	if m.viewing == "" {
		return
	}
	id := m.viewing
	m.storeViewport(m.cur())
	if info, ok := m.subagentByID(id); ok && subagentTerminal(info) {
		if m.agentDone == nil {
			m.agentDone = make(map[string]time.Time)
		}
		m.agentDone[id] = m.now()
	}
	m.viewing = ""
	// The keyboard stays on the rows, marking the row the view came from;
	// typing hands it back to the composer.
	m.agentFocus = true
	m.input.Blur()
	released := m.tombstone != nil && m.tombstone.ID == id
	m.tombstone = nil
	if released {
		if _, live := liveInfo(m.snap.Subagents, id); !live {
			delete(m.subs, id)
			delete(m.agentStart, id)
			delete(m.agentDone, id)
		}
	}
	stick, off := m.main.atBottom, m.main.yOffset
	m.setViewportContent(stick)
	if !stick {
		m.vp.SetYOffset(off)
		m.storeViewport(m.main)
	}
}

func (m *Model) switchView(delta int) {
	items := m.visibleAgents()
	if len(items) == 0 {
		return
	}
	idx := 0
	for i, s := range items {
		if s.ID == m.viewing {
			idx = i
			break
		}
	}
	idx = (idx + delta) % len(items)
	if idx < 0 {
		idx += len(items)
	}
	m.enterView(items[idx].ID)
}

func (m Model) handleViewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if stopped, next := m.stopViewKey(msg); stopped { // subcancel.go
		return next, nil
	}
	switch msg.Type {
	case tea.KeyCtrlY:
		return m.copySelectionOrLastReply()
	case tea.KeyCtrlO:
		return m.toggleExpanded()
	case tea.KeyCtrlT:
		return m.cycleTasks()
	case tea.KeyCtrlG:
		return m.openThemePicker(), nil
	case tea.KeyEsc, tea.KeyLeft:
		m.leaveView()
		return m, nil
	case tea.KeyUp:
		m.vp.ScrollUp(1)
		return m, nil
	case tea.KeyDown:
		m.vp.ScrollDown(1)
		return m, nil
	case tea.KeyPgUp, tea.KeyPgDown:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case tea.KeyTab:
		m.switchView(1)
		return m, nil
	case tea.KeyShiftTab:
		m.switchView(-1)
		return m, nil
	case tea.KeyEnter, tea.KeyRight:
		return m, nil
	}
	return m, nil
}

func (m *Model) applySubagentEvent(ev agent.Event) {
	if ev.Subagent == nil || ev.Subagent.ID == "" {
		return
	}
	info := *ev.Subagent
	id := info.ID
	m.refreshSnap()
	m.ensureSub(id)
	if m.viewing == id {
		cp := info
		if live, ok := liveInfo(m.snap.Subagents, id); ok {
			cp = live
		}
		m.tombstone = &cp
	}
	switch ev.SubagentChange {
	case agent.SubagentChangeSpawned:
		// A new attempt of the same id must count elapsed from this sighting
		// and linger from the new finish, not the previous attempt.
		if m.agentStart == nil {
			m.agentStart = make(map[string]time.Time)
		}
		m.agentStart[id] = m.now()
		delete(m.agentDone, id)
	case agent.SubagentChangeFinished:
		// The child's run is closed by the fold, at the event's At.
		m.noteAgentDone(id)
	default:
		m.noteAgentStart(id)
	}
	if m.viewing == id && !m.showSubagentTranscript() {
		m.rebuildReceiptTranscript(id)
		if m.cur().dirty {
			stick := m.vp.Height == 0 || m.vp.AtBottom()
			m.setViewportContent(stick)
		}
	}
}

// applyChildEvent is a sub-agent's own event. Its rows are the fold's, in the
// child's transcript: a reply, a thought and the prompt the parent handed it
// stream; a tool is kept one row per call; and a command line — nothing emits
// one against a child today, but a child's expansion belongs to the child's
// transcript for the same reason its user block does — is a note. What is left
// here is the pane every child event has always made, and what a child's tool
// says about the roster.
func (m *Model) applyChildEvent(ev agent.Event) {
	id := ev.Agent
	m.ensureSub(id)
	if ev.Type == agent.EventTool {
		m.refreshSnap()
		m.noteAgentStart(id)
	}
}

func (m *Model) rebuildReceiptTranscript(id string) {
	info, ok := m.subagentByID(id)
	if !ok {
		return
	}
	tr := m.ensureSub(id)
	stick, off := tr.atBottom, tr.yOffset
	had := tr.transcriptRows != nil
	tr.reset()
	now := m.now()
	label := m.snap.Provider.Label()
	if label == "" {
		label = m.snap.Provider.Name
	}
	// Every row is this client's own, rebuilt from the roster for a provider
	// whose sub-agents stream nothing — the child's shared transcript is empty
	// — so they are local rows (plan 024 §3.8).
	tr.appendLocal(entry{kind: entryNote, text: label + " streams no sub-agent transcript; this is what its receipt carried"}, now)
	if strings.TrimSpace(info.Prompt) != "" {
		tr.appendLocal(entry{kind: entryUser, text: info.Prompt}, now)
	}
	if note := receiptNote(info, m.receiptAgentID(info)); note != "" {
		tr.appendLocal(entry{kind: entryNote, text: note}, now)
	}
	if strings.TrimSpace(info.Output) != "" {
		// The whole reply, capped the way a streamed one is, in one row.
		tr.appendLocal(entry{kind: entryAssistant, text: capEntryText(info.Output)}, now)
	}
	tr.atBottom = stick
	tr.yOffset = off
	if !had {
		tr.atBottom = true
	}
	tr.dirty = true
}

func (m Model) receiptAgentID(info agent.SubagentInfo) string {
	for i := range m.snap.Tools {
		t := &m.snap.Tools[i]
		if t.ID == info.ID && t.Task != nil && t.Task.AgentID != "" {
			return t.Task.AgentID
		}
		if t.Task != nil && t.Task.AgentID != "" && t.ID == info.ToolCallID {
			return t.Task.AgentID
		}
	}
	return info.ID
}

func receiptLanded(info agent.SubagentInfo) bool {
	return info.Model != "" || info.DurationMs > 0
}

func receiptNote(info agent.SubagentInfo, agentID string) string {
	if !receiptLanded(info) {
		return ""
	}
	var bits []string
	if name := shortModelName(info.Model); name != "" {
		bits = append(bits, name)
	}
	if d := formatMillis(info.DurationMs); d != "" {
		bits = append(bits, d)
	}
	if agentID != "" {
		bits = append(bits, agentID)
	}
	return strings.Join(bits, " · ")
}

func (m Model) composerRows() int {
	if m.viewing != "" {
		return 1
	}
	if m.confirm != nil {
		// The confirm replaces the input rows with its one line; the draft
		// and its cursor are still underneath and come back on decline.
		return 1
	}
	inner := m.composerInner()
	rows := 0
	for _, ln := range strings.Split(m.input.Value(), "\n") {
		rows += wrapRows(ln, inner)
	}
	return max(rows, 1)
}

func (m Model) subagentComposerView() string {
	return m.subagentTopRule() + "\n" + m.subagentBanner() + "\n" + m.composerRule(false)
}

func (m Model) subagentChip() string {
	info, ok := m.viewedInfo()
	if !ok {
		return ""
	}
	desc := sanitizeLine(info.Description)
	model := shortModelName(info.Model)
	maxW := max(1, m.width/composerTitleShare)
	prefix := ""
	if model != "" {
		prefix = "(" + model + ")"
		if desc != "" {
			prefix += " "
		}
	}
	if lipgloss.Width(prefix+desc) <= maxW {
		return prefix + desc
	}
	if prefix != "" {
		rest := maxW - lipgloss.Width(prefix)
		if rest < 1 {
			return clampWidth(prefix, maxW)
		}
		return prefix + clampWidth(desc, rest)
	}
	return clampWidth(desc, maxW)
}

func (m Model) subagentTopRule() string {
	width := max(1, m.width)
	style := styleFG(m.theme.Rule)
	chip := m.subagentChip()
	if chip == "" {
		return style.Render(strings.Repeat("─", width))
	}
	chipSt := lipgloss.NewStyle().Foreground(m.theme.BG).Background(m.theme.Accent)
	lead := width - lipgloss.Width(chip) - 3
	if lead < 1 {
		return chipSt.Render(clampWidth(chip, width))
	}
	return style.Render(strings.Repeat("─", lead)+" ") + chipSt.Render(chip) + style.Render(" ─")
}

func (m Model) subagentBanner() string {
	info, ok := m.viewedInfo()
	if !ok {
		return renderSegs(m.width, seg{"esc to return", styleFG(m.theme.Dim)})
	}
	typ := agentLabel(info)
	esc := "esc to return"
	st := styleFG(m.theme.Dim)
	var head, tail string
	if subagentRunning(info) {
		if m.showSubagentTranscript() {
			head = "○ @" + typ + " · read-only"
		} else {
			head = "○ @" + typ + " · receipt only"
		}
		if len(m.visibleAgents()) > 1 {
			tail = " · tab next agent"
		}
		tail += m.stopBannerHint()
	} else {
		// A finished sub-agent gets the Warn banner on both providers; the
		// receipt note stays so the cursor view still says what it holds.
		st = styleFG(m.theme.Warn)
		glyph := "✓"
		status := "completed"
		switch info.Status {
		case agent.SubagentFailed:
			glyph, status = "✗", "failed"
		case agent.SubagentCancelled:
			glyph, status = "–", "cancelled"
		}
		head = glyph + " @" + typ + " · " + status
		if !m.showSubagentTranscript() {
			head += " · receipt only"
		}
		if err := sanitizeLine(info.Error); err != "" {
			head += " · " + err
		}
	}
	return renderSubBanner(m.width, head, esc, tail, st)
}

func renderSubBanner(width int, head, esc, tail string, st lipgloss.Style) string {
	escBit := " · " + esc
	full := head + escBit + tail
	if lipgloss.Width(full) <= width {
		return renderSegs(width, seg{full, st})
	}
	if lipgloss.Width(head+escBit) <= width {
		return renderSegs(width, seg{head + escBit, st})
	}
	room := width - lipgloss.Width(escBit)
	if room < lipgloss.Width(esc) {
		return renderSegs(width, seg{clampWidth(esc, width), st})
	}
	if room < 1 {
		return renderSegs(width, seg{clampWidth(esc, width), st})
	}
	return renderSegs(width, seg{clampWidth(head, room) + escBit, st})
}

func (m Model) subagentSpinnerView() string {
	info, ok := m.viewedInfo()
	if !ok || !subagentRunning(info) {
		return ""
	}
	activity := sanitizeLine(info.Activity)
	if activity == "" {
		activity = "Working"
	}
	text := activity + " · " + m.subElapsed(info)
	if tok := formatTokens(info.TokensUsed); tok != "" {
		text += " · " + tok
	}
	return renderSegs(m.width,
		seg{m.spinnerGlyph() + " ", styleFG(m.theme.Accent)},
		seg{text, styleFG(m.theme.Dim)},
	)
}

func (m Model) subElapsed(info agent.SubagentInfo) string {
	start, ok := m.agentStart[info.ID]
	if !ok || m.frozen {
		return formatElapsed(0)
	}
	return formatElapsed(m.now().Sub(start))
}

func (m Model) mergedSpinnerText() string {
	if m.viewing != "" {
		if info, ok := m.viewedInfo(); ok {
			glyph, _ := m.agentGlyph(info)
			if subagentRunning(info) || info.DurationMs <= 0 {
				return glyph + " " + m.subElapsed(info)
			}
			return glyph + " " + formatMillis(info.DurationMs)
		}
	}
	return m.spinnerGlyph() + " " + m.spinnerElapsed()
}

// spinnerElapsed is the clock the main spinner shows: the turn's while it
// runs, else the longest-running sub-agent's — after end_turn the turn's
// clock would keep growing for a turn that is over.
func (m Model) spinnerElapsed() string {
	if m.status == statusWorking {
		return m.turnElapsed()
	}
	for i := range m.snap.Subagents {
		if subagentRunning(m.snap.Subagents[i]) {
			return m.subElapsed(m.snap.Subagents[i])
		}
	}
	return m.turnElapsed()
}
