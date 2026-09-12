package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const composerInnerH = 3

func newComposer() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "message  / for commands"
	ta.Prompt = " "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(composerInnerH)
	ta.MaxHeight = composerInnerH
	km := textarea.DefaultKeyMap
	km.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	km.DeleteCharacterForward = key.NewBinding(key.WithKeys("delete"))
	ta.KeyMap = km
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()
	return ta
}

func composerEmpty(ta textarea.Model) bool {
	return strings.TrimSpace(ta.Value()) == ""
}

func isNewlineKey(msg tea.KeyMsg) bool {
	s := msg.String()
	return s == "shift+enter" || s == "alt+enter" || msg.Type == tea.KeyCtrlJ
}

func composerBoxHeight() int {
	return composerInnerH + 2
}
