package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

const (
	providerDialogTitle = "provider"
	providerDialogHint  = "↑↓ · tab · enter starts · esc uses default"
)

// pickerRows settles the picker's list, and tui.New is its only caller: the
// rows are the caller's availability-filtered list — or the built-in
// non-optional set when it is empty, which keeps a hand-built Config hermetic —
// unioned with the resolved default.
//
// The union is the invariant, not a convention the caller honours (§3.4). Esc
// starts providerDefault whatever the list says, so a default that is missing
// from it would be started by a picker that never showed it, never tagged it
// and preselected cursor instead. Order is agent.Providers() order and names
// are deduplicated, so neither a caller's ordering nor an inserted default can
// move the rows around.
func pickerRows(list []agent.Provider, def agent.Provider) []agent.Provider {
	if len(list) == 0 {
		list = agent.DefaultProviders()
	}
	want := make(map[string]agent.Provider, len(list)+1)
	arrived := make([]string, 0, len(list)+1)
	remember := func(p agent.Provider) {
		if _, dup := want[p.Name()]; p.Name() == "" || dup {
			return
		}
		want[p.Name()] = p
		arrived = append(arrived, p.Name())
	}
	for _, p := range list {
		remember(p)
	}
	remember(def)
	rows := make([]agent.Provider, 0, len(arrived))
	for _, p := range agent.Providers() {
		if got, ok := want[p.Name()]; ok {
			rows = append(rows, got)
			delete(want, p.Name())
		}
	}
	// A name the registry does not know keeps the order it arrived in. Every
	// provider is in the registry today, so this is belt and braces against a
	// caller that builds one some other way.
	for _, name := range arrived {
		if got, ok := want[name]; ok {
			rows = append(rows, got)
		}
	}
	return rows
}

func (m Model) providerIndex(p agent.Provider) int {
	want := p.Name()
	for i, c := range m.providers {
		if c.Name() == want {
			return i
		}
	}
	return 0
}

func (m Model) confirmProvider(p agent.Provider, explicit bool) (tea.Model, tea.Cmd) {
	if p.Name() == "" {
		p = agent.CursorProvider()
	}
	m.pickingProvider = false
	m.dialog = dialogNone
	m.pickedExplicit = explicit
	m.sessProvider = p.Name()
	if m.newSession != nil {
		if m.sess != nil {
			_ = m.sess.Close()
		}
		m.sess = m.newSession(p)
	}
	if m.sess == nil {
		m.sess = NewStub()
	}
	m.refreshSnap()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	// The same batch Init returns, for the same reason: the session this just
	// built may be a load, and its replay is emitted from the client's read
	// loop while Start is still running (§3.5).
	return m, tea.Batch(m.startCmd(), waitEvent(m.sess))
}

func (m Model) handleProviderDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.providers)
	if n == 0 {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		return m.confirmProvider(m.providers[m.providerCursor], true)
	case tea.KeyEsc:
		return m.confirmProvider(m.providerDefault, false)
	case tea.KeyUp, tea.KeyShiftTab:
		m.providerCursor = (m.providerCursor - 1 + n) % n
		return m, nil
	case tea.KeyDown, tea.KeyTab:
		m.providerCursor = (m.providerCursor + 1) % n
		return m, nil
	}
	return m, nil
}

// providerDialogPlan is the window onto the row list and whether the footer
// survived: title + rows + footer, with the list giving its rows up one at a
// time and the footer going only once even one row no longer fits. The
// renderer and the hit-tester both take it, so the two cannot disagree about
// which rows the frame is showing.
//
// The window follows providerCursor instead of always starting at row 0. A box
// that truncated from the bottom would draw the top of the list while the keys
// — and the Enter that starts a session — were on a row nobody could see, and
// would bound clicks to rows that are not the ones on screen. Three rows and a
// default at the last index is what makes that reachable, but the arithmetic is
// the same one row or ten.
func (m Model) providerDialogPlan(budget int) (top, shown int, footer bool) {
	footer = budget >= 3
	rows := budget - 1
	if footer {
		rows--
	}
	// dialogListWindow scrolls only as far as the cursor forces, so a list that
	// fits still starts at row 0 and the goldens are unmoved.
	top, shown = dialogListWindow(len(m.providers), m.providerCursor, max(rows, 0))
	return top, shown, footer
}

// providerDialogBody draws the window the plan settled. There is no ▲/▼ marker
// the way the theme list has one: the tag column is the "default" tag, and a
// row cannot carry both.
func (m Model) providerDialogBody(inner, budget int) []string {
	top, shown, footer := m.providerDialogPlan(budget)
	rows := []string{m.dialogTitle(providerDialogTitle, inner)}
	for i := top; i < top+shown; i++ {
		p := m.providers[i]
		tag := ""
		if p.Name() == m.providerDefault.Name() {
			tag = "default"
		}
		rows = append(rows, m.dialogRow(p.Name(), tag, i == m.providerCursor, true, inner))
	}
	if footer {
		rows = append(rows, m.dialogFooter(providerDialogHint, inner))
	}
	return rows
}

func (m Model) providerDialogClick(i int) (tea.Model, tea.Cmd) {
	top, shown, _ := m.providerDialogPlan(m.lay.Dialog.H - dialogBorder)
	row := i - 1 // the title row
	if row < 0 || row >= shown {
		return m, nil
	}
	m.providerCursor = top + row
	return m, nil
}
