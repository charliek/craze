package tui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	runewidth "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

const (
	// composerMaxRows / composerShortRows are the autogrow ceiling and what
	// degradation step 3 leaves of it.
	composerMaxRows   = 6
	composerShortRows = 3
	// composerPromptW is the width of "❯ ", reserved on every input row.
	composerPromptW = 2
	// composerHeadroom is the height the textarea is lent for the duration of
	// one key. See updateComposer. It has to exceed the display rows of any
	// draft: a draft that reached this many rows would be gigabytes of text,
	// which is past what the composer could hold anyway.
	composerHeadroom = 1 << 30
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
)

func newComposer(th Theme) textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "message  / for commands"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
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

	km := textarea.DefaultKeyMap
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
	ta.KeyMap = km

	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	styleComposer(&ta, th)
	ta.Focus()
	return ta
}

// styleComposer applies the theme colours a live re-theme has to re-apply.
//
// bubbles keeps an unexported pointer at whichever of the two style sets is
// active, and Update copies the whole Model, so that pointer still aims at the
// styles of an older copy. Writing new colours into this copy's fields would
// change nothing on screen; Focus/Blur re-seat the pointer, which is the only
// way in. The blink command they return is dropped: the cursor keeps the phase
// it is in and the next keystroke restarts the chain (textarea does that
// itself whenever the cursor moves).
func styleComposer(ta *textarea.Model, th Theme) {
	prompt := lipgloss.NewStyle().Foreground(th.Accent)
	placeholder := lipgloss.NewStyle().Foreground(th.Dim)
	ta.FocusedStyle.Prompt = prompt
	ta.BlurredStyle.Prompt = prompt
	ta.FocusedStyle.Placeholder = placeholder
	ta.BlurredStyle.Placeholder = placeholder
	if ta.Focused() {
		_ = ta.Focus()
	} else {
		ta.Blur()
	}
}

// composerPrompt marks only the first display row, Claude Code style.
func composerPrompt(line int) string {
	if line == 0 {
		return "❯ "
	}
	return "  "
}

func isNewlineKey(msg tea.KeyMsg) bool {
	s := msg.String()
	return s == "shift+enter" || s == "alt+enter" || msg.Type == tea.KeyCtrlJ
}

// composerInner is the cell width one input row has for text, which is what
// SetWidth leaves the textarea after the prompt (textarea.go:892).
func (m Model) composerInner() int {
	return max(1, m.width-composerPromptW)
}

// composerCursorRow is the cursor's row in the same coordinates: the rows the
// logical lines above it take, plus its offset inside its own line.
func (m Model) composerCursorRow() int {
	inner := m.composerInner()
	lines := strings.Split(m.input.Value(), "\n")
	row := 0
	for i := 0; i < m.input.Line() && i < len(lines); i++ {
		row += wrapRows(lines[i], inner)
	}
	return row + m.input.LineInfo().RowOffset
}

// wrapRows is how many display rows one logical line takes at this width.
//
// It reproduces bubbles v0.21's unexported textarea.wrap (textarea.go:1398)
// step for step, because a count that disagrees with what the textarea drew is
// how the composer loses rows: ceil(width/inner) under-reports an exactly-full
// line, which the textarea's ">= width" tail rule spills onto a second row.
func wrapRows(line string, width int) int {
	width = max(width, 1)
	var (
		rows   = [][]rune{{}}
		word   []rune
		row    int
		spaces int
	)
	for _, r := range line {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			if uniseg.StringWidth(string(rows[row]))+uniseg.StringWidth(string(word))+spaces > width {
				row++
				rows = append(rows, []rune{})
			}
			rows[row] = append(rows[row], word...)
			rows[row] = append(rows[row], []rune(strings.Repeat(" ", spaces))...)
			spaces, word = 0, nil
			continue
		}
		// A word that fills the line on its own breaks mid-word, and the last
		// rune may be double-width, so the check is per rune and not per word.
		if uniseg.StringWidth(string(word))+runewidth.RuneWidth(word[len(word)-1]) > width {
			if len(rows[row]) > 0 {
				row++
				rows = append(rows, []rune{})
			}
			rows[row] = append(rows[row], word...)
			word = nil
		}
	}
	// bubbles gives the tail a trailing space, so a line that already reaches
	// the width spills one more row.
	if uniseg.StringWidth(string(rows[row]))+uniseg.StringWidth(string(word))+spaces >= width {
		row++
	}
	return row + 1
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
	if p := m.planPlaceholder(); p != "" {
		return p
	}
	// A card owns the keyboard, so neither verb is available under one.
	if m.status != statusWorking || m.cardOpen() || m.input.Value() != "" {
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
	if chip := m.queueEditChip(); top && chip != "" {
		// The rule drops a title it cannot fit beside its margins, and at
		// the 40-column minimum the whole chip is wider than the rule. A
		// truncated "editing #1 · enter saves…" still says the composer is
		// not the composer right now; nothing at all does not.
		title = clampWidth(chip, max(0, width-4))
	}
	// "─"*n + " " + title + " ─": the two spaces and the closing dash are the
	// three cells the title needs beside itself.
	lead := width - lipgloss.Width(title) - 3
	if !top || lead < 1 {
		return style.Render(strings.Repeat("─", width))
	}
	return style.Render(strings.Repeat("─", lead) + " " + title + " ─")
}
