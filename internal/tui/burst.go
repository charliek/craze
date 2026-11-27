package tui

import (
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/rivo/uniseg"
)

// Terminals and frame drivers can send several typed characters together.
// Dispatch each grapheme independently so a literal word such as "up" never
// matches a navigation binding. A composed character remains a single key.
func burstKeys(k tea.KeyPressMsg) []tea.KeyPressMsg {
	if k.Mod&^tea.ModShift != 0 || len(k.Text) < 2 {
		return nil
	}
	g := uniseg.NewGraphemes(k.Text)
	if !g.Next() || len(g.Str()) == len(k.Text) {
		return nil
	}
	keys := []tea.KeyPressMsg{textKey(g.Str())}
	for g.Next() {
		keys = append(keys, textKey(g.Str()))
	}
	return keys
}

func textKey(text string) tea.KeyPressMsg {
	code, n := utf8.DecodeRuneInString(text)
	if n < len(text) {
		code = tea.KeyExtended
	}
	return tea.KeyPressMsg{Code: code, Text: text}
}

func runeKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

// keyInput carries either a key press or a bracketed paste through the same
// focus and command-gate rules. Widgets receive the original Bubble Tea event.
type keyInput interface{ String() string }

// shortcut excludes pasted text even when its entire content names a binding.
func shortcut(msg keyInput) string {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		return k.String()
	}
	return ""
}

// burst applies keys, a burst's (burstKeys), in order, each through the
// command gate (gated) exactly as a key typed on its own arrives, with their
// commands batched. So a burst is the same keys typed one at a time
// everywhere: the composer and its chips, the app's own bindings, every input
// box with its own limit and cursor. A key that opens a gate holds every
// later key of the burst, drained in order once the gate closes, as slow keys
// are; and a burst arriving while keys are held already is held behind them.
func (m Model) burst(keys []tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var next tea.Model = m
	cmds := make([]tea.Cmd, 0, len(keys))
	for _, k := range keys {
		cur, ok := next.(Model)
		if !ok {
			// Nothing the handler answers is ever not a Model; if it were,
			// there would be no model to type the rest into.
			break
		}
		var cmd tea.Cmd
		next, cmd = cur.gated(k, Model.update)
		cmds = append(cmds, cmd)
	}
	return next, tea.Batch(cmds...)
}
