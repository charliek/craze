package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

const (
	providerDialogTitle = "provider"
	providerDialogHint  = "↑↓ · tab · enter starts · esc uses default"
)

func providerChoices() []agent.Provider {
	return []agent.Provider{agent.CursorProvider(), agent.GrokProvider()}
}

func providerIndex(p agent.Provider) int {
	want := p.Name()
	for i, c := range providerChoices() {
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
	return m, m.startCmd()
}

func (m Model) handleProviderDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(providerChoices())
	if n == 0 {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		return m.confirmProvider(providerChoices()[m.providerCursor], true)
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

func (m Model) providerDialogPlan(budget int) (shown int, footer bool) {
	n := len(providerChoices())
	footer = budget >= 3
	rows := budget - 1
	if footer {
		rows--
	}
	return min(max(rows, 0), n), footer
}

func (m Model) providerDialogBody(inner, budget int) []string {
	shown, footer := m.providerDialogPlan(budget)
	rows := []string{m.dialogTitle(providerDialogTitle, inner)}
	choices := providerChoices()
	for i := 0; i < shown; i++ {
		p := choices[i]
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
	shown, _ := m.providerDialogPlan(m.lay.Dialog.H - dialogBorder)
	row := i - 1
	if row < 0 || row >= shown {
		return m, nil
	}
	m.providerCursor = row
	return m, nil
}
