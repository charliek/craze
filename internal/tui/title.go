package tui

import (
	"io"

	"github.com/charmbracelet/x/ansi"
)

// The four marks §3.10 pins, highest priority first in windowTitle: a card
// waiting on the user outranks an error, which outranks a turn in progress,
// which outranks idle (the default, including the pre-start picker).
const (
	titleMarkNeedsYou = "⚠" // U+26A0 — a permission, question or plan card is open
	titleMarkError    = "✕" // U+2715 — statusError
	titleMarkWorking  = "❖" // U+2756 — statusWorking, replaying, or a foreign turn
	titleMarkIdle     = "✦" // U+2726 — everything else
)

// titleTextCap is how many display cells of the session title the tab title
// keeps before an ellipsis, independent of titleRuneCap (the much longer cap
// the session index applies): a tab is narrow, an index row is not.
const titleTextCap = 40

// titleUntitled is the text half of the title before the session has one:
// the literal word "craze", never the workspace or the provider, so the tab
// says the same thing for every session until it earns a name.
const titleUntitled = "craze"

// windowTitle is the terminal tab title for the model's current state. It is
// pure — no side effect, no channel read — so the Update wrapper can call it
// on every transition and compare the result to what it last emitted (§3.10).
//
// The strong-send confirm line is deliberately not read here: it is the
// user's own keystroke landing on its own dialog, not the agent asking for
// something, so it must not raise the needs-you mark.
func (m Model) windowTitle() string {
	mark := titleMarkIdle
	switch {
	case m.cardOpen():
		mark = titleMarkNeedsYou
	case m.status == statusError:
		mark = titleMarkError
	case m.status == statusWorking || m.replaying || m.snap.ForeignTurn:
		mark = titleMarkWorking
	}

	text := sanitizeLine(m.snap.Title)
	if text == "" {
		text = titleUntitled
	}
	text = ansi.Truncate(text, titleTextCap, "…")

	return mark + " " + text
}

// clearWindowTitle is Run's exit-path clear (§3.10): writing OSC 2 with an
// empty title through the same writer bubbletea rendered into hands the tab
// back to Ghostty's or roost's own derived name instead of leaving a stale
// "✦ craze" once craze itself is gone. It is a no-op when lastTitle is still
// "" — nothing was ever set, whether because the run never got past its first
// frame or because Config.TerminalTitle was off — so a session that never
// touched the tab title does not need the terminal to fall back to anything.
func clearWindowTitle(w io.Writer, m Model) {
	if m.lastTitle == "" {
		return
	}
	_, _ = w.Write([]byte(ansi.SetWindowTitle("")))
}
