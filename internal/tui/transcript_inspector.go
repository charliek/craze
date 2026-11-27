package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The cursor remembers the selected pane entry across paints.
type transcriptInspector struct {
	active bool
	entry  *entry
}

func (x *rowIndex) target(i int) bool {
	if x == nil || i < 0 || i >= x.spans() {
		return false
	}
	s := x.span(i)
	return s.disclosable && len(s.rows) != 0
}

// visibleTarget resolves focus against the current viewport after a page move,
// wheel scroll or streaming update. Only headers on screen can receive Enter.
func (m Model) visibleTarget() int {
	x := m.vp.rows
	if x == nil || x.total == 0 || m.vp.Height <= 0 || m.vp.YOffset >= x.total {
		return -1
	}
	_, first := x.find(m.vp.YOffset)
	bottom := m.vp.YOffset + m.vp.Height
	best := -1
	for i := first; i < x.spans(); i++ {
		s := x.span(i)
		if s.start >= bottom {
			break
		}
		if s.start < m.vp.YOffset || !x.target(i) {
			continue
		}
		if s.e == m.inspector.entry {
			return i
		}
		if best < 0 {
			best = i
		}
	}
	return best
}

func (m *Model) focusTarget(from, step int) bool {
	x := m.vp.rows
	if x == nil {
		return false
	}
	for i := from; i >= 0 && i < x.spans(); i += step {
		if x.target(i) {
			s := x.span(i)
			m.inspector = transcriptInspector{active: true, entry: s.e}
			m.revealHeader(s.start)
			return true
		}
	}
	return false
}

func (m *Model) revealHeader(row int) {
	switch {
	case row < m.vp.YOffset:
		m.vp.SetYOffset(row)
	case row >= m.vp.YOffset+m.vp.Height:
		m.vp.SetYOffset(row - m.vp.Height + 1)
	}
	m.storeViewport(m.cur())
}

func (m *Model) focusNearTop() {
	m.inspector = transcriptInspector{active: true}
	x := m.vp.rows
	if x == nil || x.total == 0 {
		return
	}
	_, first := x.find(min(m.vp.YOffset, x.total-1))
	if !m.focusTarget(first, 1) {
		m.focusTarget(first-1, -1)
	}
}

// transcriptKey runs after modal, confirmation, and side-view dispatch. It
// handles both the main and subagent pane without taking the composer's draft.
func (m *Model) transcriptKey(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyF2:
		m.inspector = transcriptInspector{}
		switch m.transcriptLayout {
		case layoutGrouped:
			m.transcriptLayout = layoutCompact
		case layoutCompact:
			m.transcriptLayout = layoutClassic
		default:
			m.transcriptLayout = layoutGrouped
		}
		m.showTranscriptLayout = true
		m.refreshViewport()
		return true
	case tea.KeyF4:
		if m.inspector.active {
			m.inspector = transcriptInspector{}
		} else {
			m.catchUp()
			m.focusNearTop()
		}
		return true
	}
	if !m.inspector.active {
		return false
	}
	if msg.Type == tea.KeyEsc {
		m.inspector = transcriptInspector{}
		return true
	}
	m.catchUp()
	x := m.vp.rows
	i := m.visibleTarget()
	switch msg.Type {
	case tea.KeyCtrlO, tea.KeyCtrlG, tea.KeyCtrlY, tea.KeyCtrlT:
		return false // existing pane shortcuts
	case tea.KeyPgUp, tea.KeyPgDown:
		m.vp, _ = m.vp.Update(msg)
		m.storeViewport(m.cur())
	case tea.KeyUp, tea.KeyDown, tea.KeyEnter:
		if i < 0 {
			if x != nil && x.total > 0 && msg.Type != tea.KeyEnter {
				_, at := x.find(min(m.vp.YOffset, x.total-1))
				if msg.Type == tea.KeyUp {
					m.focusTarget(at, -1)
				} else {
					m.focusTarget(at+1, 1)
				}
			}
			break
		}
		m.inspector.entry = x.span(i).e
		switch msg.Type {
		case tea.KeyUp:
			m.focusTarget(i-1, -1)
		case tea.KeyDown:
			m.focusTarget(i+1, 1)
		case tea.KeyEnter:
			if !m.expanded {
				e := x.span(i).e
				e.detailOpen = !e.detailOpen
				e.dirty = true
				m.setViewportContent(false)
				if next := m.visibleTarget(); next >= 0 {
					m.revealHeader(m.vp.rows.span(next).start)
				}
			}
		}
	}
	return true
}

// The focus marker is drawn over the right edge of the visible header. The
// transcript index retains the original line for copying and hit testing.
func (m Model) inspectedViewport() string {
	view := m.vp.View()
	if !m.inspector.active || m.vp.Width < 2 {
		return view
	}
	i := m.visibleTarget()
	if i < 0 {
		return view
	}
	line := m.vp.rows.span(i).start - m.vp.YOffset
	if line < 0 || line >= m.vp.Height {
		return view
	}
	rows := strings.Split(view, "\n")
	if line < len(rows) {
		header := padRow(ansi.Truncate(rows[line], m.vp.Width-2, ""), m.vp.Width-2) + " ◀"
		rows[line] = highlightSpan(header, 0, m.vp.Width-1, selectionSeq(lipgloss.ColorProfile(), m.theme.SelectionBG))
	}
	return strings.Join(rows, "\n")
}
