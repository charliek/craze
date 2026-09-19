package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

const (
	resumeDialogTitle = "resume"
	resumeDialogHint  = "↑↓ · tab · enter loads · esc quits"
	// resumeDialogMax is how many rows the picker offers. internal/cli asks
	// the index for ten, and the cap is repeated here so a caller that asks
	// for more still gets a dialog rather than a panel (§3.7).
	resumeDialogMax = 10
	// resumeRowSep separates the three fields of a row. One space either side
	// of the dot and not the two §3.7 draws: dialogRow folds every row onto a
	// single line with sanitizeLine, which collapses runs of whitespace, so a
	// wider gap is not something a dialog row can carry.
	resumeRowSep = " · "
)

// resumeRows settles the picker's list, and tui.New is its only caller: the
// caller's rows, newest first, with any provider this build no longer knows
// dropped and the list capped.
//
// A provider id the registry does not know is not offered — the row stays in
// the index (a newer craze may know it again), but craze cannot start it, so
// there is nothing Enter could do with it. A provider craze knows whose binary
// does not resolve *is* offered: the spawn error is the right message, which
// is what plan 012 settled for `--provider gx` on a machine without gx.
func resumeRows(rows []sessions.Row) []sessions.Row {
	out := make([]sessions.Row, 0, len(rows))
	for _, row := range rows {
		if len(out) >= resumeDialogMax {
			break
		}
		if _, err := agent.ProviderByName(row.Provider); err != nil {
			continue
		}
		out = append(out, row)
	}
	return out
}

// resumeAge is how long ago a row was last used, in one unit: minutes under an
// hour, hours under a day, days after that. A picker row is a choice between
// sessions, not a timestamp, so the coarsest unit that still separates them is
// the one worth the cells.
func resumeAge(now, at time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := now.Sub(at)
	if d < 0 {
		// A row written by a machine whose clock is ahead is "just now"
		// rather than a negative age.
		d = 0
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// resumeRowText is one row: title · provider · age, with the title clamped
// first so the two short fields survive a title of any length. They are what
// tells two sessions of the same name apart, and a row that gave them up to a
// 200-character first prompt would be no choice at all.
func (m Model) resumeRowText(row sessions.Row, inner int) string {
	tail := resumeRowSep + row.Provider
	if age := resumeAge(m.now(), row.UpdatedAt); age != "" {
		tail += resumeRowSep + age
	}
	title := sanitizeLine(row.Title)
	if strings.TrimSpace(title) == "" {
		// Every row the index writes has a title (§3.2); a row from a torn
		// write is still worth offering, and its id is the only name it has.
		title = row.SessionID
	}
	budget := inner - lipgloss.Width(dialogNoMark) - lipgloss.Width(tail)
	return clampWidth(title, max(budget, 0)) + tail
}

// confirmResume loads the chosen row. It is confirmProvider's twin, and ends
// the same way: the session the row describes, and the batch Init returns.
//
// The row's provider is locked, whatever --provider or the config file
// resolved to: the index says which agent wrote this session, and only that
// agent can be trusted to load it (a grok row is started by grok even though
// gx would load it — §3.1). persistProvider is left alone: internal/cli has
// already decided whether this run may write a default, and a load never does
// unless --provider was explicit.
func (m Model) confirmResume(row sessions.Row) (tea.Model, tea.Cmd) {
	p, err := agent.ProviderByName(row.Provider)
	if err != nil {
		// Unreachable: resumeRows drops a row the registry does not know.
		p = m.providerDefault
	}
	m.pickingResume = false
	m.dialog = dialogNone
	m.providerLocked = true
	m.providerDefault = p
	m.sessProvider = p.Name()
	// The session about to be built is a load, so the model is replaying
	// before its first event, exactly as Config.Loading makes it for
	// --continue: tea.Batch promises no ordering between startCmd and the
	// first waitEvent, so EventReplay{start} cannot be what learns it (§3.5).
	m.replaying = true
	m.loading = true
	if m.loadSession != nil {
		if m.sess != nil {
			_ = m.sess.Close()
		}
		m.setSession(m.loadSession(p, row))
	}
	if m.sess == nil {
		m.setSession(NewStub())
	}
	m.refreshSnap()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	return m, tea.Batch(m.startCmd(), waitEvent(m.sess))
}

func (m Model) handleResumeDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.resume)
	if n == 0 {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		return m.confirmResume(m.resume[m.resumeCursor])
	case tea.KeyEsc:
		// Esc quits, and quits clean: no session was ever started, so
		// startErr is nil and Run returns nil (§3.7). There is no "default
		// row" the way the provider picker has a default provider — starting
		// somebody's last session because they pressed Esc would be the
		// opposite of what Esc means.
		return m.requestQuit()
	case tea.KeyUp, tea.KeyShiftTab:
		m.resumeCursor = (m.resumeCursor - 1 + n) % n
		return m, nil
	case tea.KeyDown, tea.KeyTab:
		m.resumeCursor = (m.resumeCursor + 1) % n
		return m, nil
	}
	return m, nil
}

// resumeDialogPlan is providerDialogPlan for the resume list: the window onto
// the rows and whether the footer survived, with the window following the
// cursor so a short box never hides the row Enter would load.
func (m Model) resumeDialogPlan(budget int) (top, shown int, footer bool) {
	footer = budget >= 3
	rows := budget - 1
	if footer {
		rows--
	}
	top, shown = dialogListWindow(len(m.resume), m.resumeCursor, max(rows, 0))
	return top, shown, footer
}

func (m Model) resumeDialogBody(inner, budget int) []string {
	top, shown, footer := m.resumeDialogPlan(budget)
	rows := []string{m.dialogTitle(resumeDialogTitle, inner)}
	for i := top; i < top+shown; i++ {
		rows = append(rows, m.dialogRow(m.resumeRowText(m.resume[i], inner), "", i == m.resumeCursor, true, inner))
	}
	if footer {
		rows = append(rows, m.dialogFooter(resumeDialogHint, inner))
	}
	return rows
}

// resumeDialogClick loads the row that was clicked, rather than only moving
// the highlight the way the provider picker does: there is no second gesture
// to make here, and the box is the whole of the session's start.
func (m Model) resumeDialogClick(i int) (tea.Model, tea.Cmd) {
	top, shown, _ := m.resumeDialogPlan(m.lay.Dialog.H - dialogBorder)
	row := i - 1 // the title row
	if row < 0 || row >= shown {
		return m, nil
	}
	return m.confirmResume(m.resume[top+row])
}
