package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	helpDialogTitle = "help"
	helpDialogHint  = "↑↓ · pgup/pgdn · esc closes"
	// helpDialogWidth is wider than the other dialogs because help is a
	// two-column table: at 54 cells half the descriptions would be truncated
	// away, and the descriptions are the thing help is for.
	helpDialogWidth = 72
	// helpKeyCol is the fixed gutter a key gets, so every description on the
	// screen starts in the same column. helpIndent sets the rows in under their
	// section heading.
	helpKeyCol = 18
	helpIndent = "  "
)

// helpLine is one row of the help box: a section heading when key is empty,
// otherwise a key and what it does.
type helpLine struct{ key, desc string }

func (l helpLine) heading() bool { return l.key == "" }

// helpKeyLines is the keyboard and mouse half of the box, grouped so the eye
// can find a key by what it is for instead of reading four keys to a line.
func (m Model) helpKeyLines() []helpLine {
	out := []helpLine{
		{desc: "sending and editing"},
		{"enter", "send the draft"},
		{"alt+enter, ctrl+j", "newline (shift+enter where the terminal sends it)"},
		{"ctrl+v", "paste"},
		{"esc", "cancel the turn, or close what is open"},
		{"ctrl+c", "cancel the turn, then quit"},
		{"ctrl+d", "quit"},
	}
	if m.showModes() {
		out = append(out,
			helpLine{desc: "mode"},
			helpLine{key: "shift+tab", desc: "cycle the mode: agent, plan, ask"},
			helpLine{key: "click ◆ chip", desc: "cycle the mode from status row 2"},
		)
	}
	arrows := "a list inside a dialog"
	if m.showSubagents() {
		arrows = "sub-agent rows, or a list inside a dialog"
	}
	out = append(out,
		helpLine{desc: "moving and scrolling"},
		helpLine{key: "↑ ↓", desc: arrows},
		helpLine{key: "pgup pgdn, wheel", desc: "scroll the transcript"},
		helpLine{key: "tab", desc: "complete the slash command being typed"},
		helpLine{desc: "panels and views"},
	)
	if m.showTodos() {
		out = append(out, helpLine{key: "ctrl+t", desc: "tasks panel: compact, expanded, hidden"})
	}
	out = append(out,
		helpLine{key: "ctrl+g", desc: "theme dialog"},
		helpLine{key: "ctrl+o", desc: "expand or collapse transcript detail"},
	)
	if m.showSubagents() {
		out = append(out, helpLine{key: "enter on a row", desc: "peek at a selected sub-agent's prompt"})
	}
	out = append(out,
		helpLine{key: "click the model", desc: "model dialog, from status row 1"},
		helpLine{desc: "selection and clipboard"},
		helpLine{key: "drag", desc: "select transcript rows and copy them"},
		helpLine{key: "double-click", desc: "select the word under the pointer"},
		helpLine{key: "ctrl+y", desc: "copy the selection, or the last reply"},
	)
	return out
}

// helpCommandLines is the slash catalog: craze's own builtins first, then
// whatever this session advertises under its own heading. Agent commands and
// skills vary by session, so they must not read as craze's own.
func (m Model) helpCommandLines() []helpLine {
	var builtin, session []helpLine
	for _, it := range m.slashCatalog() {
		l := helpLine{key: "/" + it.Name, desc: it.labeledDesc()}
		if it.Builtin {
			builtin = append(builtin, l)
			continue
		}
		session = append(session, l)
	}
	out := append([]helpLine{{desc: "commands"}}, builtin...)
	if len(session) > 0 {
		out = append(out, helpLine{desc: "this session's commands"})
		out = append(out, session...)
	}
	return out
}

func (m Model) helpLines() []helpLine {
	return append(m.helpKeyLines(), m.helpCommandLines()...)
}

func (m Model) openHelp() Model {
	m = m.closeDialog(true)
	m.dialog = dialogHelp
	m.helpTop = 0
	return m
}

func (m Model) handleHelpDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m.closeDialog(true), nil
	case tea.KeyUp:
		return m.scrollHelp(-1), nil
	case tea.KeyDown:
		return m.scrollHelp(1), nil
	case tea.KeyPgUp:
		return m.scrollHelp(-m.helpPage()), nil
	case tea.KeyPgDown:
		return m.scrollHelp(m.helpPage()), nil
	}
	// Everything else is swallowed, the way the box has always swallowed it: a
	// bare `q` or `?` is a message, not a binding.
	return m, nil
}

// helpShown is how many content rows the box is drawing this frame, which is
// both the scroll page and the clamp the top is held to.
func (m Model) helpShown() int {
	_, shown, _ := m.helpDialogPlan(m.lay.Dialog.H - dialogBorder)
	return shown
}

func (m Model) helpPage() int { return max(1, m.helpShown()-1) }

func (m Model) scrollHelp(delta int) Model {
	top := m.helpTop + delta
	limit := max(0, len(m.helpLines())-m.helpShown())
	m.helpTop = min(max(top, 0), limit)
	return m
}

// helpDialogPlan is the window onto the help rows and whether the footer
// survived. The title is the one row the box never gives up, the footer is the
// second thing to go, and the content scrolls rather than growing the box past
// the transcript region. The renderer and the scroll clamp both take it.
func (m Model) helpDialogPlan(budget int) (top, shown int, footer bool) {
	n := len(m.helpLines())
	footer = budget >= 3
	rows := budget - 1
	if footer {
		rows--
	}
	shown = min(max(rows, 0), n)
	return min(max(m.helpTop, 0), max(0, n-shown)), shown, footer
}

func (m Model) helpDialogBody(inner, budget int) []string {
	lines := m.helpLines()
	top, shown, footer := m.helpDialogPlan(budget)
	rows := []string{m.dialogTitle(helpDialogTitle, inner)}
	for i := 0; i < shown; i++ {
		rows = append(rows, m.helpRow(lines[top+i], dialogScrollTag(i, top, shown, len(lines)), inner))
	}
	if footer {
		rows = append(rows, m.dialogFooter(helpDialogHint, inner))
	}
	return rows
}

// helpRow draws one line: a heading in the accent, or a key padded to the
// shared gutter with its description beside it. The ▲/▼ marker rides on the
// same right edge the dialog lists use, and the text gives up the cells for it
// rather than a narrow box losing the only sign that it scrolls.
func (m Model) helpRow(l helpLine, tag string, inner int) string {
	room := inner
	if tag != "" {
		room = max(0, inner-lipgloss.Width(tag)-1)
	}
	if l.heading() {
		head := clampWidth(l.desc, room)
		return renderSegs(inner,
			seg{head, styleFG(m.theme.Accent).Bold(true)},
			m.dialogTagSeg(lipgloss.Width(head), tag, inner))
	}
	key := helpIndent + l.key
	if pad := helpKeyCol + lipgloss.Width(helpIndent) - lipgloss.Width(key); pad > 0 {
		key += strings.Repeat(" ", pad)
	}
	key = clampWidth(key, room)
	desc := clampWidth(l.desc, max(0, room-lipgloss.Width(key)))
	return renderSegs(inner,
		seg{key, styleFG(m.theme.Bright)},
		seg{desc, styleFG(m.theme.Dim)},
		m.dialogTagSeg(lipgloss.Width(key)+lipgloss.Width(desc), tag, inner))
}
