package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// viewport is the transcript band's window onto the drawn rows (plan 032 §3.3
// C5). It replaces bubbles' viewport (v0.21.0), whose SetContent was handed
// every row joined into one string and then split it apart again and measured
// the width of every line, on every paint. This one is handed the paint's
// rowIndex and reads, on View, the rows it shows and no others. Its rows are
// already the lines that split made — an entry's rows are split where it is
// rendered (physicalLines) — so a row is one line here as it was there.
//
// It has every call craze made of bubbles' viewport, with bubbles' clamps, and
// View is bubbles' own lipgloss call; TestViewportMatchesBubbles drives the two
// through the same operations and compares them. What bubbles has and craze
// never reached is left out: horizontal scrolling (nothing set a step or an
// offset), the mouse wheel and the half-page and line keys inside Update
// (craze scrolls on the wheel itself, and the keymap it gave bubbles bound
// PgUp and PgDn only), a Style (craze never set one: its frame sizes were 0),
// the lines the scroll calls returned (craze discarded them), and
// high-performance rendering.
type viewport struct {
	Width   int
	Height  int
	YOffset int
	// rows is what setRows was last handed; nil until then.
	rows *rowIndex
}

// noRows is the content of a paint with no width: no rows, which bubbles held
// as the one empty line SetContent("") makes.
var noRows = &rowIndex{}

// setRows is bubbles' SetContent: the rows to show, and the same pull back to
// the bottom when the offset is past the last line.
func (v *viewport) setRows(rows *rowIndex) {
	v.rows = rows
	if v.YOffset > v.lines()-1 {
		v.GotoBottom()
	}
}

// lines is how many lines the viewport holds: none before setRows, and at
// least one after, because bubbles split even empty content into one line.
func (v viewport) lines() int {
	if v.rows == nil {
		return 0
	}
	return max(1, v.rows.total)
}

func (v viewport) maxYOffset() int { return max(0, v.lines()-v.Height) }

// AtTop reports whether the viewport is at the top.
func (v viewport) AtTop() bool { return v.YOffset <= 0 }

// AtBottom reports whether the viewport is at, or past, the bottom.
func (v viewport) AtBottom() bool { return v.YOffset >= v.maxYOffset() }

// SetYOffset scrolls to line n, clamped into the content.
func (v *viewport) SetYOffset(n int) { v.YOffset = clampInt(n, 0, v.maxYOffset()) }

// ScrollDown moves the view down n lines, stopping at the bottom.
func (v *viewport) ScrollDown(n int) {
	if v.AtBottom() || n == 0 || v.lines() == 0 {
		return
	}
	v.SetYOffset(v.YOffset + n)
}

// ScrollUp moves the view up n lines, stopping at the top.
func (v *viewport) ScrollUp(n int) {
	if v.AtTop() || n == 0 || v.lines() == 0 {
		return
	}
	v.SetYOffset(v.YOffset - n)
}

// PageDown moves the view down one height.
func (v *viewport) PageDown() {
	if v.AtBottom() {
		return
	}
	v.ScrollDown(v.Height)
}

// PageUp moves the view up one height.
func (v *viewport) PageUp() {
	if v.AtTop() {
		return
	}
	v.ScrollUp(v.Height)
}

// GotoTop scrolls to the top.
func (v *viewport) GotoTop() {
	if v.AtTop() {
		return
	}
	v.SetYOffset(0)
}

// GotoBottom scrolls to the bottom.
func (v *viewport) GotoBottom() { v.SetYOffset(v.maxYOffset()) }

// Update is the page keymap craze gave bubbles' viewport: PgDn and PgUp, with
// no modifier.
func (v viewport) Update(msg tea.Msg) (viewport, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "pgdown":
			v.PageDown()
		case "pgup":
			v.PageUp()
		}
	}
	return v, nil
}

// View is the rows on screen, padded and cut to the viewport's size by
// bubbles' own lipgloss call.
func (v viewport) View() string {
	w, h := v.Width, v.Height
	contents := lipgloss.NewStyle().
		Width(w).     // pad to width.
		Height(h).    // pad to height.
		MaxHeight(h). // truncate height if taller.
		MaxWidth(w).  // truncate width if wider.
		Render(strings.Join(v.visibleLines(), "\n"))
	// bubbles renders the result once more through its Style, which craze
	// left at the zero value.
	var style lipgloss.Style
	return style.UnsetWidth().UnsetHeight().Render(contents)
}

// visibleLines is the rows from YOffset, one height of them, each cut to the
// width.
//
// A row is not always as narrow as the width it was rendered for: lipgloss
// expands a tab to four spaces when it styles a row, after the row was
// measured with the tab as no cells at all. bubbles cut every line on screen
// to the width once its scan found a wider one anywhere; cutting each line
// here comes to the same thing without the scan, because the cut leaves a
// line that fits exactly as it was (ansi.Truncate returns it whole). A line no
// longer in bytes than the width fits without being measured: no grapheme is
// wider in cells than it is long in bytes.
func (v viewport) visibleLines() []string {
	var lines []string
	if n := v.lines(); n > 0 {
		top := min(max(0, v.YOffset), n)
		bottom := clampInt(v.YOffset+v.Height, top, n)
		if v.rows.total == 0 {
			lines = make([]string, bottom-top)
		} else {
			lines = v.rows.appendRows(make([]string, 0, bottom-top), top, bottom)
		}
	}
	if v.Width == 0 {
		return lines
	}
	for i, ln := range lines {
		if len(ln) > v.Width {
			lines[i] = ansi.Cut(ln, 0, v.Width)
		}
	}
	return lines
}
