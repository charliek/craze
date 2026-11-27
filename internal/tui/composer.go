package tui

import (
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
)

const (
	// composerMaxRows / composerShortRows are the autogrow ceiling and what
	// degradation step 3 leaves of it.
	composerMaxRows   = 6
	composerShortRows = 3
	// composerPromptW is the width of "❯ ", reserved on every input row.
	composerPromptW = 2
	// composerTitleShare is the fraction of the width the session title may
	// take on the top rule.
	composerTitleShare = 2
	// composerDefaultTitle is what the top rule reads before a title arrives.
	composerDefaultTitle = "craze"
	// planOfferPlaceholder is the composer's half of the plan-mode exit: the
	// three things Enter, typing and Shift+Tab do while a plan is on offer.
	planOfferPlaceholder = "enter implements this plan  ·  type to refine  ·  shift+tab leaves plan mode"
	// The two mid-turn hints. Ctrl+L is the strong send, and what strong
	// means depends on what the agent can do: grok merges the text into the
	// running turn, cursor can only cancel it and start again.
	queueInterjectHint = "enter queues  ·  ctrl+l interjects"
	queueSendNowHint   = "enter queues  ·  ctrl+l sends now"
	// queueForeignHint is the hint while the agent runs a turn of its own
	// that shows as work (foreignWorking): a message waits for it to end.
	queueForeignHint = "enter queues"
)

func newComposer(th Theme) textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "message  / for commands"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.DynamicHeight = true
	// MaxHeight stays unset on purpose: bubbles refuses to open a new logical
	// line once the buffer reaches MaxHeight, so setting it would turn the
	// visible height into a text limit instead of a window.
	ta.MaxHeight = 0
	// MaxWidth defaults to 500, and SetWidth silently clamps to it: past that
	// the textarea would wrap at 498 cells while composerRows counted at the
	// terminal's real width, and a count that disagrees loses rows.
	ta.MaxWidth = 0
	ta.SetPromptFunc(composerPromptW, composerPrompt)
	ta.SetHeight(1)

	km := textarea.DefaultKeyMap()
	// Craze uses Alt for word motion; Ctrl+arrows leave the draft in place.
	km.WordBackward.SetKeys("alt+left", "alt+b")
	km.WordForward.SetKeys("alt+right", "alt+f")
	km.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	km.DeleteCharacterForward = key.NewBinding(key.WithKeys("delete"))
	// ctrl+t is the tasks panel, not transpose; ctrl+d is left unbound so it
	// quits rather than deleting forward.
	km.TransposeCharacterBackward = key.NewBinding()
	// bubbles' own ctrl+v runs clipboard.ReadAll inside the textarea
	// (textarea.go:1391), which shells out to xclip / wl-paste with no seam a
	// test or `craze frame` could stand in front of. craze keeps the key and
	// handles it itself, through the same kind of seam a copy goes through.
	km.Paste = key.NewBinding()
	km.CopySelection = key.NewBinding()
	ta.KeyMap = km

	styleComposer(&ta, th)
	ta.Focus()
	return ta
}

// styleComposer applies both focus states so a live theme change reaches the
// prompt, text and placeholder even while the draft is covered by a dialog.
func styleComposer(ta *textarea.Model, th Theme) {
	styles := ta.Styles()
	// The composer receives editing events; static cursors keep frames stable
	// without scheduling blink events that the model would discard.
	styles.Cursor.Blink = false
	styles.Cursor.Color = th.FG
	for _, state := range []*textarea.StyleState{&styles.Focused, &styles.Blurred} {
		state.CursorLine = lipgloss.NewStyle()
		state.Prompt = styleFG(th.Accent)
		state.Placeholder = styleFG(th.Dim)
		state.Text = styleFG(th.FG)
	}
	ta.SetStyles(styles)
}

// composerPrompt marks only the first display row, Claude Code style.
func composerPrompt(info textarea.PromptInfo) string {
	if info.LineNumber == 0 {
		return "❯ "
	}
	return "  "
}

// isNewlineKey accepts Shift+Enter from extended keyboards and the
// Alt+Enter and Ctrl+J equivalents available in legacy terminals.
func isNewlineKey(msg keyInput) bool {
	switch shortcut(msg) {
	case "alt+enter", "shift+enter", "ctrl+j":
		return true
	}
	return false
}

// composerInner is the cell width one input row has for text, which is what
// SetWidth leaves the textarea after the prompt (textarea.go:892).
func (m Model) composerInner() int {
	return max(1, m.width-composerPromptW)
}

// composerCovered reports that a layer stands between the user and the draft:
// a card, a dialog, or the sub-agent view. Each takes the keyboard away from
// the composer (handleKey returns above it for all three), so what belongs to
// the draft — the slash band, an asynchronous paste — goes with it. The
// confirm line is deliberately not here: it swallows keys too, but it is one
// armed question rather than a layer, and it is answered in place.
func (m Model) composerCovered() bool {
	return m.cardOpen() || m.dialogOpen() || m.viewing != ""
}

// composerPlain reports that the draft is plain text to the composer: a `!`
// command line (shellMode), or any draft in the side view (plan 040 §3.6),
// whose composer asks a question. Neither has the slash menu, the `@` popup or
// an image chip: a path in either is text.
func (m Model) composerPlain() bool {
	return m.shellMode() || m.btw != nil
}

// composerCursorOffset is the cursor's byte offset into the draft, which is
// what slashToken works in.
//
// The textarea indexes its logical row by runes; completion and image chips
// use byte offsets in the complete draft.
func (m Model) composerCursorOffset() int {
	value := m.input.Value()
	lines := strings.Split(value, "\n")
	row := min(max(m.input.Line(), 0), len(lines)-1)
	off := 0
	for i := 0; i < row; i++ {
		// +1 for the "\n" the split took out.
		off += len(lines[i]) + 1
	}
	rs := []rune(lines[row])
	col := min(m.input.Column(), len(rs))
	return off + len(string(rs[:col]))
}

// setComposerCursor puts the cursor at a byte offset in a value the textarea
// already holds. SetValue leaves it at the very end of the buffer, so an edit
// that inserted in the middle has to walk back: CursorUp steps one display row
// (a wrapped one or a logical one) and always makes progress, and SetCursor
// then takes the rune column inside the logical line — the same coordinates
// LineInfo reports back. The loop is bounded by the draft's display rows as a
// guard only.
func (m *Model) setComposerCursor(value string, off int) {
	row, col := runeRowCol(value, off)
	for guard := m.composerRows(); m.input.Line() > row && guard > 0; guard-- {
		m.input.CursorUp()
	}
	m.input.SetCursorColumn(col)
}

// runeRowCol splits a byte offset into the logical row it falls on and the
// rune column inside that row, which is the pair bubbles works in.
func runeRowCol(value string, off int) (row, col int) {
	off = min(max(off, 0), len(value))
	head := value[:off]
	row = strings.Count(head, "\n")
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[i+1:]
	}
	return row, utf8.RuneCountInString(head)
}

// composerCursorRow uses the widget’s soft-wrapped cursor coordinates. Ask a
// copy for a terminal cursor so the live widget keeps its virtual cursor and
// focus state. Its dynamically sized viewport contains the entire draft.
func (m Model) composerCursorRow() int {
	input := m.input
	input.SetVirtualCursor(false)
	_ = input.Focus()
	return input.Cursor().Y
}

// planPlaceholder is the plan offer's placeholder, or "" when the composer
// keeps its own. The gate is Value()=="" because that, not composerEmpty, is
// the rule the textarea draws a placeholder by at all.
func (m Model) planPlaceholder() string {
	if !m.planOffering() || m.input.Value() != "" {
		return ""
	}
	return clampWidth(planOfferPlaceholder, m.composerInner())
}

// composerHint is the placeholder for this frame. The plan offer keeps
// priority; under a running turn the hint says what Enter and Ctrl+L do,
// which differs by provider because only grok can interject.
func (m Model) composerHint() string {
	if m.btw != nil {
		// The side view's composer asks follow-ups (plan 040 §3.6): neither
		// the plan offer nor the queue's verbs are its.
		if m.cardOpen() || m.input.Value() != "" {
			return ""
		}
		return clampWidth(btwHint, m.composerInner())
	}
	if p := m.planPlaceholder(); p != "" {
		return p
	}
	if u := m.unstarted; u != nil && u.pending == 0 && m.input.Value() == "" {
		// A new session not spawned yet (plan 030 §3.13): its first prompt is
		// what starts it.
		return clampWidth(unstartedHint, m.composerInner())
	}
	// A card owns the keyboard, so neither verb is available under one.
	if m.cardOpen() || m.input.Value() != "" {
		return ""
	}
	if m.status != statusWorking {
		if m.foreignWorking() {
			return clampWidth(queueForeignHint, m.composerInner())
		}
		return ""
	}
	hint := queueSendNowHint
	if m.caps().Interject {
		hint = queueInterjectHint
	}
	return clampWidth(hint, m.composerInner())
}

// composerTitle is the right end of the top rule: the session title cursor
// sent, or the program's name until one arrives.
func (m Model) composerTitle() string {
	if t := clampWidth(sanitizeLine(m.snap.Title), m.width/composerTitleShare); t != "" {
		return t
	}
	return composerDefaultTitle
}

// composerView is the pinned shape: a titled rule, the visible input rows, a
// plain rule.
//
// The textarea holds every row of the draft, so a draft taller than the band
// is windowed here rather than by the textarea's own viewport: the first row
// of the band is the first row of the draft until the cursor pushes past the
// bottom, which is what keeps the `❯` on screen.
func (m Model) composerView(lay frameLayout) string {
	if m.viewing != "" {
		return m.subagentComposerView()
	}
	if m.confirm != nil {
		return m.composerRule(true) + "\n" +
			renderSegs(m.width, seg{clampWidth(confirmLine, m.width), styleFG(m.theme.Accent)}) + "\n" +
			m.composerRule(false)
	}
	if p := m.composerHint(); p != "" {
		// m is a copy, so this swaps the placeholder for this frame only.
		m.input.Placeholder = p
	}
	if m.inspector.active {
		m.input.Blur() // Render the saved draft without a second focus indicator.
	}
	rows := m.composerRows()
	shown := max(1, lay.ComposerRows)
	view := strings.Split(m.input.View(), "\n")
	top := 0
	if rows > shown {
		top = min(max(m.composerCursorRow()-(shown-1), 0), rows-shown)
	}
	top = min(top, max(0, len(view)-shown))
	band := view[top:min(len(view), top+shown)]
	// The band owes the layout exactly shown rows. The textarea returns fewer
	// only if its height and the layout disagree, but the bottom rule has to
	// stay at the bottom either way.
	for len(band) < shown {
		band = append(band, "")
	}
	return m.composerRule(true) + "\n" +
		strings.Join(band, "\n") + "\n" +
		m.composerRule(false)
}

// composerRule draws one of the two lines around the input. The top one ends
// with the session title; both are exactly m.width cells wide.
func (m Model) composerRule(top bool) string {
	width := max(1, m.width)
	style := styleFG(m.theme.Rule)
	title := m.composerTitle()
	if m.shellMode() && m.viewing == "" {
		// The whole visible change, and it costs the layout nothing: both rules
		// take the shell colour and the top one says what Enter and Esc do
		// instead of naming the session. The two are mutually exclusive with
		// the queue-edit chip below — a row being edited is never shell mode —
		// so the order of these two branches decides nothing.
		//
		// The sub-agent view borrows this rule for the bottom of its own band
		// (subagentComposerView), where the draft is not on screen and Enter
		// belongs to the view: the mode is still the mode, but its colour has
		// nothing to say about a band that is not the composer.
		style = styleFG(m.theme.Shell)
		title = clampWidth(shellRuleTitle, max(0, width-4))
	}
	if chip := m.queueEditChip(); top && chip != "" {
		// The rule drops a title it cannot fit beside its margins, and at
		// the 40-column minimum the whole chip is wider than the rule. A
		// truncated "editing #1 · enter saves…" still says the composer is
		// not the composer right now; nothing at all does not.
		title = clampWidth(chip, max(0, width-4))
	}
	if top && m.btw != nil {
		// The side view's composer (plan 040 §3.6) carries the view's chip
		// where the title stands, as a sub-agent view's top rule does.
		return m.chipRule(btwRuleChip)
	}
	// "─"*n + " " + title + " ─": the two spaces and the closing dash are the
	// three cells the title needs beside itself.
	lead := width - lipgloss.Width(title) - 3
	if !top || lead < 1 {
		return style.Render(strings.Repeat("─", width))
	}
	return style.Render(strings.Repeat("─", lead) + " " + title + " ─")
}
