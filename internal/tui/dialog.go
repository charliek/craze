package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	// dialogMaxWidth is the widest a dialog gets; dialogGutter is the columns
	// it leaves either side of it on a narrow terminal.
	//
	// §3.4 pins 52, but its own footer is 51 cells and a 52-cell box has only
	// 50 inside its borders, so the box is two cells wider than the plan's
	// number rather than two cells short of the plan's text.
	dialogMaxWidth = 54
	dialogGutter   = 4
	// dialogBorder is the two rows and two columns the box border costs.
	dialogBorder = 2
	// dialogListMax caps the list so a 38-model catalogue is still a dialog
	// and not a panel; past it the list scrolls and says so.
	dialogListMax = 12
	// dialogCursorMark is the focus gutter: the row the keys are on. The list
	// has always drawn it, and the toggle rows draw the same one, so a stripped
	// frame says which of the three focus targets is live.
	dialogCursorMark = "> "
	// dialogSelMark marks a list selection whose keys have moved to a toggle
	// row: still what Enter would apply, no longer where Tab left the focus.
	dialogSelMark = "· "
	dialogNoMark  = "  "
)

// dialogKind is which modal layer is up. dialogNone is the zero value, so a
// fresh Model has no dialog.
type dialogKind int

const (
	dialogNone dialogKind = iota
	dialogModel
	dialogTheme
	dialogHelp
	dialogProvider
)

// rect is the modal layer's box in screen cells: the outer rectangle, borders
// included. The zero value is "no dialog", which is what makes Empty the one
// test every caller needs.
type rect struct{ X, Y, W, H int }

func (r rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

func (r rect) Contains(x, y int) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

func (m Model) dialogOpen() bool { return m.dialog != dialogNone }

// dialogRect centres the box inside the transcript region and nowhere else:
// the base bands stay the one ordered list, and a dialog is a layer over the
// only band that can afford to be covered.
func (m Model) dialogRect(lay frameLayout) rect {
	if m.dialog == dialogNone || lay.TooSmall {
		return rect{}
	}
	tr := lay.Region(regionTranscript)
	if tr.Height() <= 0 {
		return rect{}
	}
	w := min(m.dialogMaxWidth(), m.width-dialogGutter)
	if w < dialogBorder+1 {
		return rect{}
	}
	h := min(m.dialogRows(w), tr.Height())
	if h < dialogBorder+1 {
		return rect{}
	}
	return rect{
		X: (m.width - w) / 2,
		Y: tr.Top + (tr.Height()-h)/2,
		W: w,
		H: h,
	}
}

// dialogMaxWidth is how wide this dialog is allowed to get. Help is the one
// that carries a two-column key table, and 54 cells would truncate half the
// descriptions it exists to show.
func (m Model) dialogMaxWidth() int {
	if m.dialog == dialogHelp {
		return helpDialogWidth
	}
	return dialogMaxWidth
}

// dialogRows is the box's natural height at this width, borders included.
func (m Model) dialogRows(w int) int {
	return dialogBorder + len(m.dialogBody(w-dialogBorder, 1<<16))
}

// dialogBody is the box's content rows at an inner width and an inner height
// budget. Each dialog builds its own rows and drops them in its own order when
// the budget is short, so the box shrinks instead of covering another band.
func (m Model) dialogBody(inner, budget int) []string {
	switch m.dialog {
	case dialogModel:
		return m.modelDialogBody(inner, budget)
	case dialogTheme:
		return m.themeDialogBody(inner, budget)
	case dialogHelp:
		return m.helpDialogBody(inner, budget)
	case dialogProvider:
		return m.providerDialogBody(inner, budget)
	}
	return nil
}

// dialogTitle is the box's first row, which is the one row it never gives up.
func (m Model) dialogTitle(title string, inner int) string {
	return styleFG(m.theme.Accent).Bold(true).Render(clampWidth(title, inner))
}

// dialogFooter is the dim key hint under the body, the second thing a short
// box drops.
func (m Model) dialogFooter(hint string, inner int) string {
	return styleFG(m.theme.Dim).Render(clampWidth(hint, inner))
}

// dialogMark is the two-cell gutter every dialog row opens with. It is a
// character and not a colour on purpose: the frame goldens are ANSI-stripped,
// so a colour-only focus cue is invisible to them — and to any terminal that
// drops the colour.
func dialogMark(cursor, selected bool) string {
	switch {
	case cursor:
		return dialogCursorMark
	case selected:
		return dialogSelMark
	}
	return dialogNoMark
}

// dialogRow is one list row: the focus gutter, the text, and a right-aligned
// tag. focused is whether the list is the dialog's focus target, so a selection
// the keys have left keeps its "·" but gives up the cursor mark and the
// SelectionBG band — exactly one thing in the box looks active at a time.
func (m Model) dialogRow(text, tag string, selected, focused bool, inner int) string {
	cursor := selected && focused
	// One row is one line. An agent-supplied name with a newline in it would
	// otherwise draw two, and then the box would be taller than the rectangle
	// the layout measured and the hit test would answer for the wrong row.
	body := dialogMark(cursor, selected) + sanitizeLine(text)
	// The tag is right-aligned, and dropped rather than crowded when the row
	// is not wide enough to keep a space between the two.
	if pad := inner - lipgloss.Width(body) - lipgloss.Width(tag); tag != "" && pad >= 1 {
		body += strings.Repeat(" ", pad) + tag
	}
	body = padRow(clampWidth(body, inner), inner)
	st := styleFG(m.theme.FG)
	if cursor {
		st = lipgloss.NewStyle().Foreground(m.theme.Bright).Background(m.theme.SelectionBG)
	}
	return st.Render(body)
}

// dialogTagSeg right-aligns a scroll marker after a row whose text is already
// used cells wide, and drops it rather than crowd a row that has no room for a
// space before it.
func (m Model) dialogTagSeg(used int, tag string, inner int) seg {
	pad := inner - used - lipgloss.Width(tag)
	if tag == "" || pad < 1 {
		return seg{}
	}
	return seg{strings.Repeat(" ", pad) + tag, styleFG(m.theme.Dim)}
}

// dialogListWindow is the slice of a list that fits, scrolled only as far as
// the cursor forces. It is derived rather than stored, so the window cannot
// disagree with the selection a key just moved.
func dialogListWindow(n, sel, rows int) (top, shown int) {
	if rows <= 0 || n <= 0 {
		return 0, 0
	}
	shown = min(n, rows)
	if sel >= shown {
		top = sel - shown + 1
	}
	if top > n-shown {
		top = n - shown
	}
	return max(top, 0), shown
}

// dialogScrollTag is the ▲/▼ marker for a clipped list.
func dialogScrollTag(i, top, shown, n int) string {
	switch {
	case i == 0 && top > 0:
		return "▲"
	case i == shown-1 && top+shown < n:
		return "▼"
	}
	return ""
}

// dialogView draws the whole box: the border, and the body rows fitted into
// it. It always returns exactly r.H rows of exactly r.W cells, which is what
// overlay splices in.
func (m Model) dialogView(r rect) string {
	if r.Empty() {
		return ""
	}
	inner := r.W - dialogBorder
	body := m.dialogBody(inner, r.H-dialogBorder)
	st := styleFG(m.theme.Accent)
	rows := make([]string, 0, r.H)
	rows = append(rows, st.Render("╭"+strings.Repeat("─", inner)+"╮"))
	for i := 0; i < r.H-dialogBorder; i++ {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		rows = append(rows, st.Render("│")+padRow(clampWidth(line, inner), inner)+st.Render("│"))
	}
	rows = append(rows, st.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(rows, "\n")
}

// overlay splices box into base at r. Both are measured in display cells, and
// the result is the same size as base: the layer is drawn over the frame, it
// never resizes it.
func overlay(base, box string, r rect) string {
	if r.Empty() || box == "" {
		return base
	}
	lines := strings.Split(base, "\n")
	for i, bl := range strings.Split(box, "\n") {
		y := r.Y + i
		if i >= r.H || y < 0 || y >= len(lines) {
			break
		}
		lines[y] = spliceRow(lines[y], bl, r.X, r.W)
	}
	return strings.Join(lines, "\n")
}

// spliceRow replaces [x, x+w) of one base line with box.
//
// The suffix is cut with ansi.TruncateLeft, which replays every escape
// sequence it skipped: the base line's SGR state is re-established after the
// box rather than the box's own style running on to the end of the row. A wide
// grapheme straddling either edge becomes a blank cell, because half a
// character is not a character and the row still owes the frame its width.
func spliceRow(line, box string, x, w int) string {
	full := ansi.StringWidth(line)
	if x >= full || w <= 0 {
		return line
	}
	// The box is forced to exactly the cells it was given, so a box that ran
	// off the right edge (or came up short) cannot change the row's width.
	w = min(w, full-x)
	box = ansi.Truncate(box, w, "")
	if pad := w - ansi.StringWidth(box); pad > 0 {
		box += strings.Repeat(" ", pad)
	}
	left := ansi.Truncate(line, x, "")
	if pad := x - ansi.StringWidth(left); pad > 0 {
		// A wide grapheme straddles the left edge. The blanks that replace it
		// are handed to Truncate as its tail, which writes them at the cut and
		// therefore inside the SGR state the base line had there, so the cells
		// keep the base's background instead of falling back to the default.
		left = ansi.Truncate(line, x, strings.Repeat(" ", pad))
	}
	right := ""
	if end := x + w; end < full {
		right = ansi.TruncateLeft(line, end, "")
		if ansi.StringWidth(right) > full-end {
			// Same at the right edge, and the prefix is the same trick:
			// TruncateLeft writes it after the escape sequences it replays.
			right = ansi.TruncateLeft(line, end+1, " ")
		}
		if pad := full - end - ansi.StringWidth(right); pad > 0 {
			// The straddler was the last thing on the line, so the cut took
			// everything and there was no prefix for it to write.
			right = strings.Repeat(" ", pad) + right
		}
	}
	var b strings.Builder
	b.WriteString(left)
	if strings.ContainsRune(left, ansi.ESC) {
		b.WriteString(ansi.ResetStyle)
	}
	b.WriteString(box)
	// The box's styling is closed whether or not there is a suffix to close it:
	// a box flush against the right edge would otherwise paint the row after it.
	if right != "" || strings.ContainsRune(box, ansi.ESC) {
		b.WriteString(ansi.ResetStyle)
	}
	b.WriteString(right)
	return b.String()
}
