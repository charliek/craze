package tui

import (
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rivo/uniseg"
)

// Input bursts (SF-40, plan 037 §3.5; X23). bubbletea groups the runes that
// reach it together up to a space into one KeyRunes message — typing over a
// laggy link, tmux send-keys, keys buffered while an Update was slow — and a
// key that is not a paste says its bare text (Key.String), so a burst that
// spells a key name — "up", "end", "home", "delete" — matched that key's
// binding in the composer and every input box, and the word vanished into a
// cursor move. Update splits a burst before anything else (burstKeys, burst):
// it is the same keys typed one at a time, each through the command gate.
//
// A key is one extended grapheme cluster, not one rune: a keycap emoji
// ('1' U+FE0F U+20E3), a letter and its combining accent, a ZWJ sequence are
// each one key, as a terminal delivers them, so a key never matches a
// shortcut by its first rune — `1️⃣` on a permission card is not "1". A
// message that is one cluster is no burst and goes through as it came.
// A paste (Paste: a bracketed paste) is text and keeps its own path; so does
// Alt+….

// burstKeys is k as the keys it was typed as — one KeyRunes message per
// extended grapheme cluster, in order — when it is a burst: more than one
// cluster in a KeyRunes message that is neither a paste nor Alt+…. It is nil
// for anything else, a single cluster of several runes included.
func burstKeys(k tea.KeyMsg) []tea.KeyMsg {
	if k.Type != tea.KeyRunes || k.Alt || k.Paste || len(k.Runes) < 2 {
		return nil
	}
	var keys []tea.KeyMsg
	rest, state, at := string(k.Runes), -1, 0
	for rest != "" {
		var cluster string
		cluster, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		// The cluster's own runes, taken from the message: a rune the string
		// could not hold became one U+FFFD there, so the counts agree and
		// nothing of the message is changed.
		n := utf8.RuneCountInString(cluster)
		keys = append(keys, tea.KeyMsg{Type: tea.KeyRunes, Runes: k.Runes[at : at+n : at+n]})
		at += n
	}
	if len(keys) < 2 {
		return nil
	}
	return keys
}

// burst applies keys, a burst's (burstKeys), in order, each through the
// command gate (gated) exactly as a key typed on its own arrives, with their
// commands batched. So a burst is the same keys typed one at a time
// everywhere: the composer and its chips, the app's own bindings, every input
// box with its own limit and cursor. A key that opens a gate holds every
// later key of the burst, drained in order once the gate closes, as slow keys
// are; and a burst arriving while keys are held already is held behind them.
func (m Model) burst(keys []tea.KeyMsg) (tea.Model, tea.Cmd) {
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
