package tui

import (
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/charliek/craze/internal/presentation/theme"
)

const DefaultTheme = theme.Default

// Theme holds the semantic colors resolved for this client’s output.
type Theme struct {
	theme.Theme[color.Color]
	profile                colorprofile.Profile
	terminalFG, terminalBG color.Color
}

// Preset returns the full-color palette for headless rendering.
func Preset(name string) Theme {
	return presetFor(name, colorprofile.TrueColor)
}

func presetFor(name string, profile colorprofile.Profile) Theme {
	complete := lipgloss.Complete(profile)
	slots := theme.Preset(name, func(t theme.Tone) color.Color {
		return complete(lipgloss.Color(t.ANSI), lipgloss.Color(t.ANSI256), lipgloss.Color(t.TrueColor))
	})
	fg, bg := theme.TerminalColors(slots.Name)
	return Theme{Theme: slots, profile: profile, terminalFG: lipgloss.Color(fg), terminalBG: lipgloss.Color(bg)}
}

func KnownPreset(name string) bool { return theme.KnownPreset(name) }
func ThemeNames() []string         { return theme.ThemeNames() }

// applyTheme repaints the whole screen. The textarea keeps its own copies of
// the prompt and placeholder styles, so swapping m.theme alone would leave the
// `❯` in the previous palette, and every transcript entry was rendered against
// the old one (the render cache is keyed on the theme name).
func (m *Model) applyTheme(th Theme) {
	if m.theme.Name == th.Name {
		return
	}
	stick := m.vp.Height == 0 || m.vp.AtBottom()
	m.theme = th
	styleComposer(&m.input, th)
	m.setViewportContent(stick)
}

// openThemePicker freezes the list it will show: the live preview changes the
// current theme on every move, and "current first" would otherwise reorder the
// rows under the cursor.
func (m Model) openThemePicker() Model {
	m = m.closeDialog(true)
	m.dialog = dialogTheme
	m.themePrev = m.theme
	m.themeNames = themeOrder(m.theme.Name)
	m.themeSel = 0
	return m
}

// themeOrder is the picker's list: the active theme first, then the rest in
// table order.
func themeOrder(current string) []string {
	names := ThemeNames()
	out := make([]string, 0, len(names))
	if KnownPreset(current) {
		out = append(out, Preset(current).Name)
	}
	for _, n := range names {
		if len(out) > 0 && n == out[0] {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (m Model) handleThemeDialogKey(msg keyInput) (tea.Model, tea.Cmd) {
	n := len(m.themeNames)
	if n == 0 {
		return m.closeDialog(true), nil
	}
	if m.themeSel < 0 || m.themeSel >= n {
		m.themeSel = 0
	}
	switch shortcut(msg) {
	case "esc":
		return m.closeDialog(true), nil
	case "enter":
		return m.keepTheme(), nil
	case "down":
		m.themeSel = (m.themeSel + 1) % n
	case "up":
		m.themeSel = (m.themeSel - 1 + n) % n
	default:
		s := shortcut(msg)
		switch {
		case s == "j":
			m.themeSel = (m.themeSel + 1) % n
		case s == "k":
			m.themeSel = (m.themeSel - 1 + n) % n
		case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
			i := int(s[0] - '1')
			if i >= n {
				return m, nil
			}
			m.themeSel = i
			m.applyTheme(presetFor(m.themeNames[i], m.theme.profile))
			return m.keepTheme(), nil
		default:
			return m, nil
		}
	}
	// Moving the cursor re-themes the live screen, which is the whole point of
	// the picker; nothing is written until Enter.
	m.applyTheme(presetFor(m.themeNames[m.themeSel], m.theme.profile))
	return m, nil
}

// keepTheme closes the picker on the previewed theme and persists it.
func (m Model) keepTheme() Model {
	name := m.theme.Name
	m.dialog = dialogNone
	m.themeNames = nil
	m.themeSel = 0
	if name == m.themePrev.Name {
		return m
	}
	return m.noteAndSaveTheme(name)
}

// setThemeNamed is /theme <name>: an unknown name is an error row and the
// screen keeps the theme it had.
func (m Model) setThemeNamed(name string) Model {
	if !KnownPreset(name) {
		m.addError("theme " + sanitizeLine(name) + " is not a preset (" + strings.Join(ThemeNames(), ", ") + ")")
		return m
	}
	th := presetFor(name, m.theme.profile)
	if th.Name == m.theme.Name {
		return m
	}
	m.applyTheme(th)
	return m.noteAndSaveTheme(th.Name)
}

// noteAndSaveTheme records the change in the transcript and persists it. A
// config craze cannot parse is left alone and says so, rather than costing the
// user the keys it could not read.
func (m Model) noteAndSaveTheme(name string) Model {
	m.addNote("theme → " + name)
	if err := SaveTheme(name); err != nil {
		m.addError(err.Error())
	}
	return m
}

// themeDialogBody previews the selected roles below the complete list when it fits.
func (m Model) themeDialogBody(inner, budget int) []string {
	top, shown, footer, sample := m.themeDialogPlan(inner, budget)
	rows := []string{m.dialogTitle(themeDialogTitle, inner)}
	for i := 0; i < shown; i++ {
		name := m.themeNames[top+i]
		// The theme dialog has one focus target, so its list always has it.
		rows = append(rows, m.dialogRow(name, dialogScrollTag(i, top, shown, len(m.themeNames)), top+i == m.themeSel, true, inner))
	}
	if sample {
		glyph, failed := m.statusGlyph("failed")
		rows = append(rows,
			renderSegs(inner, seg{"❯ ", styleFG(m.theme.UserMark)}, seg{"Review the change", styleFG(m.theme.User).Bold(true)}),
			renderSegs(inner, seg{"▸ Explored · 3 reads", styleFG(m.theme.Dim)}),
			m.toolRow(glyph, failed, "bash", "test", "exit 1", failed, inner),
			renderSegs(inner, seg{"+ added  ", styleFG(m.theme.DiffAdd)}, seg{"− removed", styleFG(m.theme.DiffDel)}),
		)
	}
	if footer {
		rows = append(rows, m.dialogFooter(themeDialogHint, inner))
	}
	return rows
}

// themeDialogPlan is the window onto the name list and whether the footer
// survived (dialogListPlan). The renderer and the hit-tester both take it.
func (m Model) themeDialogPlan(inner, budget int) (top, shown int, footer, sample bool) {
	const sampleRows = 4
	sample = inner >= 20 && budget >= len(m.themeNames)+2+sampleRows
	if sample {
		budget -= sampleRows
	}
	top, shown, footer = dialogListPlan(len(m.themeNames), m.themeSel, budget)
	return
}

// themeDialogClick picks the name under the pointer and keeps it, which is
// Enter under the pointer.
func (m Model) themeDialogClick(i int) (tea.Model, tea.Cmd) {
	top, shown, _, _ := m.themeDialogPlan(m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder)
	row := i - 1 // the title row
	if row < 0 || row >= shown {
		return m, nil
	}
	m.themeSel = top + row
	m.applyTheme(presetFor(m.themeNames[m.themeSel], m.theme.profile))
	return m.keepTheme(), nil
}
