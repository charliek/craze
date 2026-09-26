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
//
// A hidden provider's row is dropped the same way, although the registry
// resolves the id: its sessions have no loader yet (plan 018 §3.4).
// internal/cli's knownProvider already keeps such a row out of the list it
// hands over; this is the same rule for a Config built any other way.
func resumeRows(rows []sessions.Row) []sessions.Row {
	out := make([]sessions.Row, 0, len(rows))
	for _, row := range rows {
		if len(out) >= resumeDialogMax {
			break
		}
		if p, err := agent.ProviderByName(row.Provider); err != nil || p.Hidden() {
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

// resumeClaimMsg is Config.ClaimSession's answer for the row the picker chose
// (plan 027 §3.9, SQ16), carried back from the tea.Cmd that asked. attempt is
// the stamp chooseResume gave it: the picker acts on the answer only while it
// is still waiting for exactly that attempt.
type resumeClaimMsg struct {
	attempt int
	row     sessions.Row
	crazeID string
	release func()
	err     error
}

// chooseResume is Enter (or a click) on a picker row. With no ClaimSession it
// loads the row at once, as the picker always has. With one, the claim — the
// session index's lock, bounded, and the session's own lock — runs in a
// tea.Cmd, never in this Update, and the row is built only when its answer
// lands (resumeClaimed). While an attempt is in flight another Enter does
// nothing; Esc still quits.
func (m Model) chooseResume(row sessions.Row) (tea.Model, tea.Cmd) {
	if m.claimSession == nil {
		return m.confirmResume(row)
	}
	if m.resumeWaiting != 0 {
		return m, nil
	}
	m.resumeAttempt++
	m.resumeWaiting = m.resumeAttempt
	m.resumeErr = ""
	attempt, claim := m.resumeAttempt, m.claimSession
	return m, func() tea.Msg {
		id, release, err := claim(row)
		return resumeClaimMsg{attempt: attempt, row: row, crazeID: id, release: release, err: err}
	}
}

// resumeClaimed is a claim's answer. It builds the row's session only while
// the picker is still up, not quitting, and waiting for this very attempt;
// otherwise the claim it carries belongs to nobody and is released here and
// now. A refusal keeps the picker open with the refusal's text as its error
// row, and builds nothing.
func (m Model) resumeClaimed(msg resumeClaimMsg) (tea.Model, tea.Cmd) {
	if !m.pickingResume || m.quitting || msg.attempt != m.resumeWaiting {
		if msg.release != nil {
			msg.release()
		}
		return m, nil
	}
	m.resumeWaiting = 0
	if msg.err != nil {
		if msg.release != nil {
			msg.release()
		}
		m.resumeErr = msg.err.Error()
		return m, nil
	}
	// The id the claim is for — a legacy row's freshly minted one included —
	// is the id the engine is built with, so the two agree.
	row := msg.row
	row.CrazeID = msg.crazeID
	return m.confirmResume(row)
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
	if m.loadSession != nil {
		// The engine, not the session: see confirmProvider.
		if m.eng != nil {
			_ = m.eng.Close()
		}
		// The row's own durable id travels with it: this is the same thread of
		// work, loaded into another agent session (session control SD-22).
		// With a ClaimSession it is the id the claim was taken for, a legacy
		// row's freshly minted one included; without one, a row written before
		// crazeId existed carries none, and the engine mints one that the next
		// write puts in the file.
		m.setSession(m.loadSession(p, row), row.CrazeID)
	}
	if m.eng == nil && m.engErr == nil {
		m.setSession(NewStub(), "")
	}
	m.refreshSnap()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	return m, tea.Batch(m.startCmd(), waitEvent(m.eng))
}

func (m Model) handleResumeDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.resume)
	if n == 0 {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		return m.chooseResume(m.resume[m.resumeCursor])
	case tea.KeyEsc:
		// Esc quits, and quits clean: no session was ever started, so
		// startErr is nil and Run returns nil (§3.7). There is no "default
		// row" the way the provider picker has a default provider — starting
		// somebody's last session because they pressed Esc would be the
		// opposite of what Esc means.
		return m.requestQuit()
	case tea.KeyUp, tea.KeyShiftTab:
		m.resumeCursor = (m.resumeCursor - 1 + n) % n
		// A refusal is about the row it was for: once the cursor leaves it,
		// the error row goes.
		m.resumeErr = ""
		return m, nil
	case tea.KeyDown, tea.KeyTab:
		m.resumeCursor = (m.resumeCursor + 1) % n
		m.resumeErr = ""
		return m, nil
	}
	return m, nil
}

// resumeDialogPlan is providerDialogPlan for the resume list: the window onto
// the rows and whether the footer survived, with the window following the
// cursor so a short box never hides the row Enter would load.
//
// A refusal's error row goes under the list and above the footer, and a short
// box keeps it over the footer but never over the last list row: it is about
// the row the cursor is on. With no refusal the plan is exactly what it was.
func (m Model) resumeDialogPlan(budget int) (top, shown int, footer bool) {
	rows := budget - 1
	if m.resumeErrShown(budget) {
		rows--
	}
	footer = rows >= 2
	if footer {
		rows--
	}
	top, shown = dialogListWindow(len(m.resume), m.resumeCursor, max(rows, 0))
	return top, shown, footer
}

// resumeErrShown is whether the error row fits the budget: the title, one list
// row and itself.
func (m Model) resumeErrShown(budget int) bool {
	return m.resumeErr != "" && budget >= 3
}

func (m Model) resumeDialogBody(inner, budget int) []string {
	top, shown, footer := m.resumeDialogPlan(budget)
	rows := []string{m.dialogTitle(resumeDialogTitle, inner)}
	for i := top; i < top+shown; i++ {
		rows = append(rows, m.dialogRow(m.resumeRowText(m.resume[i], inner), "", i == m.resumeCursor, true, inner))
	}
	if m.resumeErrShown(budget) {
		rows = append(rows, styleFG(m.theme.Err).Render(clampWidth(sanitizeLine(m.resumeErr), inner)))
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
	return m.chooseResume(m.resume[top+row])
}
