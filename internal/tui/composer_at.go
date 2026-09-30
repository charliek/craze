package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// The composer's `@` popup (plan 030 §3.16, owner decision 10): a file or a
// directory of the shown session's workspace, mentioned in the prompt as
// `@relative/path` — completed from the workspace's files (at_files.go) by
// the completion component the session list's input uses (complete.go).
//
//   - It opens on the `@` token under the cursor — any `@` that starts a
//     whitespace-delimited token, anywhere in the draft, not only a leading
//     one as the list's (atGrammar: the cursor inside the token, X126) — and
//     only that token is completed.
//   - It is a composer feature, not the session list's: it is there in every
//     mode, the opt-out (`CRAZE_DETACH=0`, in process) and `craze attach`
//     included, wherever the composer has the keyboard (composerAtOn) — not
//     in shell mode, not under a card, a dialog, the confirm line or the
//     sub-agent view, not while the list is up or a band has the keyboard,
//     and not without a workspace to search.
//   - Its keys (X127) are the component's: ↑/↓ and ctrl+p/ctrl+n choose, tab
//     descends into a directory or accepts a file and never submits, enter
//     accepts only with a candidate to accept — else it is the composer's
//     enter — and esc hides it until the token changes. They shadow the
//     composer's own only while it is up: ↑/↓ moving the keyboard to the
//     queue or the sub-agent rows, ctrl+p/ctrl+n moving between the draft's
//     lines. Every other key is the composer's as ever: alt+enter is a
//     newline (which ends the token, and so closes the popup), ctrl+l the
//     strong send, PgUp/PgDn the transcript's. With no popup up nothing
//     changes.
//   - A pick writes `@relative/path ` — `@"relative/path with spaces" ` when
//     the path needs quoting (X126) — and a directory ends in `/`; tab on a
//     directory writes `@dir/`, with no space, and the popup stays open on
//     what is inside. The text sent is the composer's text: nothing is
//     expanded or attached (attaching contents is a follow-up, §3.16).
//   - It is drawn where the slash menu is, in the band between the
//     transcript and the composer (regionOverlay), the popup's own shape
//     (complete.go's view): the title rule `files in <workspace>`, up to
//     eight rows and `↓ N more`. It adds no line to /help (§3.16): the help
//     goldens stay as they are; docs/reference/tui.md documents it.
//
// The popup is the TUI's, as the composer's textarea is (withSession carries
// it): it follows the draft, and the Update wrapper syncs it with the draft
// after every message it applies (finish, syncComposerAt) — the one place
// every change to the draft passes, a key's, a paste's, a queue edit's, a
// switch's draft put back — so the band, the keys and the draft never
// disagree. Its environment is the shown session's workspace and the shown
// session (completeEnv): a switch to another session closes it and cancels
// its search (switchBackend, openUnstarted), and a session whose workspace
// moves closes it on the next sync (X125).

// composerAtRows is the most rows the popup ever asks the layout for: its
// title rule, completeMaxRows candidates and the count line.
const composerAtRows = completeMaxRows + 2

// newComposerAt is the composer's popup over src, its loads recorded in
// loads (Model.completeLoads) so that every exit cancels one still running.
func newComposerAt(src completeSource, loads *completeLoadSet) completePopup {
	p := newCompletePopup(src, atGrammar)
	p.trackIn(loads)
	return p
}

// composerAtOn says the composer may have its `@` popup now: the composer has
// the keyboard — no list over it, no card, dialog or sub-agent view covering
// it (composerCovered), no confirm line in its place, no band holding the
// keyboard — the draft is not a shell command (a `!ls @x` is the shell's to
// read), and there is a workspace to search. A popup with no source is a
// model a test built by hand, which has none.
func (m Model) composerAtOn() bool {
	switch {
	case m.composerAt.src == nil:
		return false
	case m.sessList.open, m.composerCovered(), m.confirm != nil:
		return false
	case m.agentFocus, m.queueFocus:
		return false
	case m.shellMode():
		return false
	}
	return m.sessHereDir() != ""
}

// composerAtShown says the popup is up: open, and the composer is where it
// may be.
func (m Model) composerAtShown() bool { return m.composerAtOn() && m.composerAt.visible() }

// composerAtActive says the popup has the keys it names: up, and given rows
// to be drawn in. A popup the layout has no room for this frame is nothing
// on screen, so a key means what it means with no popup at all — the slash
// menu's rule (slashActive). OverlayCap is the room the band would have
// whether or not it is open (layout.go), so the key that opened the popup is
// judged by the frame it opened in.
func (m Model) composerAtActive() bool { return m.composerAtShown() && m.lay.OverlayCap > 0 }

// syncComposerAt follows the draft (finish, before the layout that draws the
// popup): the popup is closed while the composer may not have one — its
// search cancelled, an esc's hide kept for when it may — and otherwise synced
// with the draft and the cursor in the shown session's workspace. A popup
// that is up over the same draft, cursor and environment as its last sync is
// left as it is: a tick or a stream event changes none of them, and asking
// the source again would match every path in the workspace for nothing. The
// command is the popup's search, when its opening starts one.
func (m *Model) syncComposerAt() tea.Cmd {
	p := &m.composerAt
	if !m.composerAtOn() {
		if p.visible() {
			p.close()
		}
		return nil
	}
	v, cur := m.input.Value(), m.composerCursorOffset()
	env := completeEnv{Workspace: m.sessHereDir(), Shown: m.shownGen}
	if p.visible() && p.value == v && p.cursor == cur && p.env == env {
		return nil
	}
	return p.sync(v, cur, env)
}

// composerAtKey is msg for the popup, while it has the keys (composerAtActive):
// handled says it was the popup's, and the key does nothing more. A choice
// writes the draft the popup made — the token replaced, the cursor where the
// choice left it — as acceptSlash writes one; the Update wrapper syncs the
// popup with it (closed past an accept's space, open on a descended
// directory). A key the popup does not name, and enter with no candidate to
// accept, is not handled: it is the composer's.
func (m Model) composerAtKey(msg tea.KeyMsg) (Model, bool) {
	if !m.composerAtActive() {
		return m, false
	}
	choice, handled := m.composerAt.key(msg)
	if !handled {
		return m, false
	}
	if choice.verb != 0 {
		m.input.SetValue(choice.value)
		m.setComposerCursor(choice.value, choice.cursor)
	}
	return m, true
}

// composerAtLoaded is the popup's search answering: taken only if the popup
// still awaits it (completePopup.loaded) — not once it has closed, been
// hidden, moved to another token, or been opened again, nor for another
// session shown (which the command gate drops before this: X121). The
// command is a further load the answer needs.
func (m Model) composerAtLoaded(msg completeLoadedMsg) (Model, tea.Cmd) {
	cmd, _ := m.composerAt.loaded(msg)
	return m, cmd
}

// overlayBandRows is the band between the transcript and the composer
// (regionOverlay), as tall as it asks to be: the `@` popup's rows while it is
// up, the slash menu's otherwise. The two never share it — a token is an `@`
// token or a slash token — and the popup outranks the menu for the one draft
// that could open both (a quoted `@"a /b"` with the cursor after `/b`), as
// its keys do.
func (m Model) overlayBandRows() int {
	if m.composerAtShown() {
		return m.composerAt.height(composerAtRows)
	}
	return m.overlayRows()
}

// overlayBandView draws the band in the rows the layout granted it.
func (m Model) overlayBandView(lay frameLayout) string {
	if m.composerAtShown() {
		return m.composerAt.view(m.theme, m.width, lay.Region(regionOverlay).Height())
	}
	return m.overlayView(lay)
}
