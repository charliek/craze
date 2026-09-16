package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// DefaultTheme is the preset craze falls back to when neither --theme nor the
// config file names one.
const DefaultTheme = "craze-dark"

// diffBGMix is how much of the add/delete colour is mixed into the background
// for a diff row: enough to tint it, not enough to fight the foreground.
const diffBGMix = 10

// ruleMix lifts the composer rules one step off Border: the two lines frame
// the thing the user types into, so they read as structure rather than as
// another divider.
const ruleMix = 30

// selectionMix is how much accent is mixed into the background for a selected
// row: enough to read as "this one", not enough to hide the text on it.
const selectionMix = 25

// paletteSpec is the hand-picked part of a theme. Every other slot is derived
// from these eleven colours, so a new preset is one row of this table.
type paletteSpec struct {
	name                                                             string
	bg, fg, dim, bright, border, accent, teal, ok, warn, err, purple string
}

// palettes is the pinned preset table, in the order the picker lists them.
// The first entry is the default.
var palettes = []paletteSpec{
	{"craze-dark", "#0c0c11", "#c9c9d4", "#82829a", "#e2e2ea", "#24242f", "#e8a33d", "#3fc8c8", "#6fbf73", "#e8a33d", "#e06c75", "#bb9af7"},
	{"craze-light", "#f6f6f2", "#2a2a33", "#7a7a8c", "#101018", "#d8d8d0", "#b8760f", "#137f7f", "#3f8a45", "#b8760f", "#b8404a", "#7a4fbf"},
	{"tokyo-night", "#1a1b26", "#a9b1d6", "#565f89", "#c0caf5", "#292e42", "#7aa2f7", "#7dcfff", "#9ece6a", "#e6c248", "#f7768e", "#bb9af7"},
	{"dark", "#1c1c1c", "#d0d0d0", "#808080", "#e4e4e4", "#3a3a3a", "#5f87d7", "#5fafd7", "#87af5f", "#e6c850", "#d75f5f", "#af87d7"},
	{"light", "#fafafa", "#383a42", "#a0a1a7", "#101010", "#d4d4d4", "#4078f2", "#0184bc", "#50a14f", "#c89614", "#e45649", "#a626a4"},
	{"catppuccin", "#1e1e2e", "#cdd6f4", "#6c7086", "#f5f5ff", "#313244", "#89b4fa", "#89dceb", "#a6e3a1", "#f9e2af", "#f38ba8", "#cba6f7"},
	{"gruvbox", "#282828", "#ebdbb2", "#928374", "#fbf1c7", "#3c3836", "#fabd2f", "#83a598", "#b8bb26", "#fabd2f", "#fb4934", "#d3869b"},
}

// themeAliases are the spellings that are not a preset name but mean one.
var themeAliases = map[string]string{
	"tokyonight": "tokyo-night",
	"tokyo":      "tokyo-night",
	"craze":      "craze-dark",
	"crazedark":  "craze-dark",
	"crazelight": "craze-light",
}

// Theme is one palette, expanded into the roles the UI paints with. The first
// block is the preset table verbatim; the rest is derived, so a renderer names
// what it is drawing rather than picking a colour.
//
// craze paints no full-screen background of its own. Instead, when the
// `background` setting is on, it sets the terminal's *own* default colours
// from BG and FG with OSC 11/10 while it runs and resets them on exit
// (internal/tui/terminal.go, §3.3); with `background = false` or
// --no-background it leaves the terminal alone and draws on whatever
// background the terminal already has. BG also derives the tinted backgrounds
// below either way.
type Theme struct {
	Name string

	BG, FG, Dim, Bright, Border         lipgloss.Color
	Accent, Teal, OK, Warn, Err, Purple lipgloss.Color

	// UserMark colours the ❯ / ↳ glyph that opens a user row; User is the bold
	// prompt text beside it. Heading colours a markdown heading (h1-h6 draw
	// identically), kept apart from Accent so a heading and inline code in
	// the same reply are not the same colour.
	User, UserMark, Assistant, Thought, ToolKind, Heading lipgloss.Color
	DiffAdd, DiffDel                                      lipgloss.Color
	DiffAddBG, DiffDelBG                                  lipgloss.Color
	LineNo, TaskRail, Selection, Rule                     lipgloss.Color
	// SelectionBG is the background the dialog cursor row (and V4's mouse
	// selection) paints with; Selection stays a foreground slot.
	SelectionBG                      lipgloss.Color
	ChipBypass, ChipPrompt, Provider lipgloss.Color
	// One colour per mode kind, not per mode id: the chip has to stay
	// readable for an agent that spells plan mode "architect".
	ModeImplement, ModePlan, ModeReadOnly lipgloss.Color
}

// theme expands a palette into every slot the UI can ask for.
func (p paletteSpec) theme() Theme {
	th := Theme{
		Name:   p.name,
		BG:     lipgloss.Color(p.bg),
		FG:     lipgloss.Color(p.fg),
		Dim:    lipgloss.Color(p.dim),
		Bright: lipgloss.Color(p.bright),
		Border: lipgloss.Color(p.border),
		Accent: lipgloss.Color(p.accent),
		Teal:   lipgloss.Color(p.teal),
		OK:     lipgloss.Color(p.ok),
		Warn:   lipgloss.Color(p.warn),
		Err:    lipgloss.Color(p.err),
		Purple: lipgloss.Color(p.purple),
	}
	// User was Bright, which vanished into body text. Teal makes it read as
	// "you" without competing for the loudest colour on screen. UserMark on
	// the ❯ derives from Accent, which is what the composer's own prompt
	// glyph and the row gutter mark paint with, so today one colour means
	// "you, here" across all three.
	th.User = th.Teal
	th.UserMark = th.Accent
	th.Assistant = th.FG
	th.Thought = th.Dim
	th.ToolKind = th.Accent
	// Heading was Accent, which is also inline code, the spinner and chips,
	// so a heading and a code word in the same reply were the same amber. OK
	// is not otherwise used in running prose, so it reads as its own thing.
	th.Heading = th.OK
	th.DiffAdd = th.OK
	th.DiffDel = th.Err
	th.DiffAddBG = blend(p.bg, p.ok, diffBGMix)
	th.DiffDelBG = blend(p.bg, p.err, diffBGMix)
	th.LineNo = th.Dim
	th.TaskRail = th.Accent
	th.Selection = th.Accent
	th.SelectionBG = blend(p.bg, p.accent, selectionMix)
	th.Rule = blend(p.border, p.fg, ruleMix)
	th.ChipBypass = th.Err
	th.ChipPrompt = th.Warn
	th.Provider = th.Teal
	th.ModeImplement = th.Accent
	th.ModePlan = th.Purple
	th.ModeReadOnly = th.Teal
	return th
}

// Preset is the theme by name, falling back to the default for anything craze
// does not know.
func Preset(name string) Theme {
	want := normalizeThemeName(name)
	for _, p := range palettes {
		if p.name == want {
			return p.theme()
		}
	}
	// palettes[0] is the default, so an unknown name cannot recurse.
	return palettes[0].theme()
}

func KnownPreset(name string) bool {
	want := normalizeThemeName(name)
	for _, p := range palettes {
		if p.name == want {
			return true
		}
	}
	return false
}

// ThemeNames lists the presets in table order.
func ThemeNames() []string {
	out := make([]string, 0, len(palettes))
	for _, p := range palettes {
		out = append(out, p.name)
	}
	return out
}

func normalizeThemeName(name string) string {
	n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	if alias, ok := themeAliases[strings.ReplaceAll(n, "-", "")]; ok {
		return alias
	}
	return n
}

func rgb(r, g, b uint8) lipgloss.Color {
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r, g, b))
}

// blend mixes pct percent of fg into bg. It is how the tinted diff backgrounds
// stay on the right side of a light or a dark palette without a second table.
func blend(bg, fg string, pct int) lipgloss.Color {
	br, bgr, bb := hexRGB(bg)
	fr, fg2, fb := hexRGB(fg)
	mix := func(a, b uint8) uint8 {
		return uint8((int(a)*(100-pct) + int(b)*pct + 50) / 100)
	}
	return rgb(mix(br, fr), mix(bgr, fg2), mix(bb, fb))
}

// hexRGB parses "#rrggbb"; anything else is black, which only a malformed
// table could produce.
func hexRGB(s string) (r, g, b uint8) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return 0, 0, 0
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0
	}
	return uint8(n >> 16), uint8(n >> 8), uint8(n)
}

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
	// The terminal's own default colours are part of the theme, so the live
	// preview has to move them too. A no-op unless Run attached a terminal.
	m.term.apply(th)
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
		out = append(out, normalizeThemeName(current))
	}
	for _, n := range names {
		if len(out) > 0 && n == out[0] {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (m Model) handleThemeDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.themeNames)
	if n == 0 {
		return m.closeDialog(true), nil
	}
	if m.themeSel < 0 || m.themeSel >= n {
		m.themeSel = 0
	}
	switch msg.Type {
	case tea.KeyEsc:
		return m.closeDialog(true), nil
	case tea.KeyEnter:
		return m.keepTheme(), nil
	case tea.KeyDown:
		m.themeSel = (m.themeSel + 1) % n
	case tea.KeyUp:
		m.themeSel = (m.themeSel - 1 + n) % n
	default:
		s := msg.String()
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
			m.applyTheme(Preset(m.themeNames[i]))
			return m.keepTheme(), nil
		default:
			return m, nil
		}
	}
	// Moving the cursor re-themes the live screen, which is the whole point of
	// the picker; nothing is written until Enter.
	m.applyTheme(Preset(m.themeNames[m.themeSel]))
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
	th := Preset(name)
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

// themeDialogBody is the names-only list in the shared dialog frame: no
// swatches, because the live preview is the swatch, and no filter, because
// seven names need none. It drops the list first and then the footer, the same
// order the model dialog uses.
func (m Model) themeDialogBody(inner, budget int) []string {
	top, shown, footer := m.themeDialogPlan(budget)
	rows := []string{m.dialogTitle(themeDialogTitle, inner)}
	for i := 0; i < shown; i++ {
		name := m.themeNames[top+i]
		// The theme dialog has one focus target, so its list always has it.
		rows = append(rows, m.dialogRow(name, dialogScrollTag(i, top, shown, len(m.themeNames)), top+i == m.themeSel, true, inner))
	}
	if footer {
		rows = append(rows, m.dialogFooter(themeDialogHint, inner))
	}
	return rows
}

// themeDialogPlan is the window onto the name list and whether the footer
// survived: title + list + footer, with the list giving its rows up one at a
// time and the footer only once even one list row no longer fits. The renderer
// and the hit-tester both take it.
func (m Model) themeDialogPlan(budget int) (top, shown int, footer bool) {
	list := min(max(budget-2, 0), len(m.themeNames))
	top, shown = dialogListWindow(len(m.themeNames), m.themeSel, list)
	return top, shown, budget >= 2
}

// themeDialogClick picks the name under the pointer and keeps it, which is
// Enter under the pointer.
func (m Model) themeDialogClick(i int) (tea.Model, tea.Cmd) {
	top, shown, _ := m.themeDialogPlan(m.lay.Dialog.H - dialogBorder)
	row := i - 1 // the title row
	if row < 0 || row >= shown {
		return m, nil
	}
	m.themeSel = top + row
	m.applyTheme(Preset(m.themeNames[m.themeSel]))
	return m.keepTheme(), nil
}
