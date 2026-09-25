package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// Stopping one sub-agent (plan 026 §3.10). Backspace and Delete — both,
// because a Mac keyboard's "delete" key sends Backspace — stop the running
// child the keyboard is on: the focused row, the one carrying the gutter mark,
// or the child whose view is open. It is the queue band's own remove key
// (handleQueueKey), a single press: the row has to be focused on purpose
// first, which is the confirm a second press would be.
//
// The key is the stop only where it can be one: the session says it has the
// verb (Capabilities.SubagentCancel, native alone) and the child is running.
// Anywhere else it falls through exactly as it did before the stop existed —
// on the rows to the composer, which gives the draft its key; in a view to
// nothing, as the view has always swallowed it — so grok's and cursor's rows,
// and every finished row, behave as they always have.
//
// After a stop the keyboard stays where it was: the rows keep it, and a view
// stays open, its banner turning to cancelled when the child's finished event
// arrives. Nothing is drawn for the stop itself — its outcome is that event.
// The engine call waits on nothing (Control.CancelSubagent), so it is made
// here, in Update; SF-17 moves it onto a tea.Cmd with the rest when the TUI
// becomes a socket client.

// stopHint is the banner's hint on a running child's view, where the key
// stops it (subagentBanner): at the tail's end, so a narrow terminal drops it
// with the tail (renderSubBanner).
const stopHint = " · del to stop"

// stopHelpKey and stopHelpDesc are the help dialog's line for the key.
const (
	stopHelpKey  = "del, backspace"
	stopHelpDesc = "stop the selected running sub-agent, or the one in view"
)

// isStopKey reports whether msg is one of the two stop keys.
func isStopKey(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyBackspace || msg.Type == tea.KeyDelete
}

// canStopSubagent is the key's gate: the session has the verb and the child is
// running.
func (m Model) canStopSubagent(info agent.SubagentInfo) bool {
	return m.caps().SubagentCancel && info.ID != "" && subagentRunning(info)
}

// stopRowKey is handleRowsKey's hook: a stop key on the focused row — the
// gutter mark's, items[agentSelection] — when that child can be stopped.
// handled false leaves the key to the rows' own handling, unchanged.
func (m Model) stopRowKey(msg tea.KeyMsg, items []agent.SubagentInfo) (handled bool, next Model) {
	if !isStopKey(msg) || len(items) == 0 {
		return false, m
	}
	return m.stopSubagent(items[m.agentSelection(len(items))])
}

// stopViewKey is handleViewKey's hook: a stop key in the view of a child that
// can be stopped. handled false leaves the key to the view's own handling,
// unchanged.
func (m Model) stopViewKey(msg tea.KeyMsg) (handled bool, next Model) {
	if !isStopKey(msg) {
		return false, m
	}
	info, ok := m.viewedInfo()
	if !ok {
		return false, m
	}
	return m.stopSubagent(info)
}

// stopSubagent asks the engine to stop info's child, when the key's gate
// allows, and reports whether the key was the stop. The error is dropped, as
// the queue band drops Unqueue's: agent.ErrNoSuchSubagent is the race with
// the child's own end — it finished between the frame the user acted on and
// the key, and its finished row says how — so there is nothing to show, and a
// row the engine refuses outright (closed) is one whose session is going away.
// A model with no engine has nothing to call and takes the key all the same.
func (m Model) stopSubagent(info agent.SubagentInfo) (bool, Model) {
	if !m.canStopSubagent(info) {
		return false, m
	}
	if m.eng != nil {
		_ = m.eng.CancelSubagent(m.nextCmd(), info.ID)
	}
	return true, m
}

// stopBannerHint is the banner's hint for a running child's view: the key's
// name, only where the session has the verb. The caller draws it only for a
// running child.
func (m Model) stopBannerHint() string {
	if !m.caps().SubagentCancel {
		return ""
	}
	return stopHint
}

// stopHelpLines is the help dialog's line for the key, only where the session
// has the verb.
func (m Model) stopHelpLines() []helpLine {
	if !m.caps().SubagentCancel {
		return nil
	}
	return []helpLine{{key: stopHelpKey, desc: stopHelpDesc}}
}
