package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	// composerMaxRows / composerShortRows are the autogrow ceiling and what
	// degradation step 3 leaves of it.
	composerMaxRows   = 6
	composerShortRows = 3
	// composerPromptW is the width of "❯ ", reserved on every input row.
	composerPromptW = 2
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
	ta.SetPromptFunc(composerPromptW, composerPrompt)
	ta.SetHeight(1)

	km := textarea.DefaultKeyMap
	km.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	km.DeleteCharacterForward = key.NewBinding(key.WithKeys("delete"))
	// ctrl+t is the tasks panel, not transpose; ctrl+d is left unbound so it
	// quits rather than deleting forward.
	km.TransposeCharacterBackward = key.NewBinding()
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

func composerEmpty(ta textarea.Model) bool {
	return strings.TrimSpace(ta.Value()) == ""
}

func isNewlineKey(msg tea.KeyMsg) bool {
	s := msg.String()
	return s == "shift+enter" || s == "alt+enter" || msg.Type == tea.KeyCtrlJ
}

// composerRows is the composer's content height: one row per soft-wrapped
// display line of the draft, clamped. bubbles does not autogrow, so this is
// what the layout pushes back into SetHeight after every key.
func (m Model) composerRows() int {
	inner := max(1, m.width-composerPromptW)
	rows := 0
	for _, ln := range strings.Split(m.input.Value(), "\n") {
		rows += max(1, (lipgloss.Width(ln)+inner-1)/inner)
	}
	return min(max(rows, 1), composerMaxRows)
}

// composerView is the pinned shape: a rule, the input rows, a rule.
func (m Model) composerView() string {
	rule := styleFG(m.theme.Border).Render(strings.Repeat("─", max(1, m.width)))
	return rule + "\n" + m.input.View() + "\n" + rule
}
