package tui

import (
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// doubleClickWindow is how close two presses on the same cell have to be to
// count as a double-click.
const doubleClickWindow = 400 * time.Millisecond

// cellPos is one cell in transcript-row coordinates: line indexes the drawn
// transcript's rows (not the screen), col is a display column from the left
// edge. Absolute rows are the point — a selection whose anchor has scrolled off
// the top still knows which text it holds.
type cellPos struct{ line, col int }

// before is reading order: earlier line, or the same line further left.
func (p cellPos) before(q cellPos) bool {
	return p.line < q.line || (p.line == q.line && p.col < q.col)
}

// selection is the drag in progress or the highlight it left behind. The range
// is inclusive of both endpoints, so a press and a release on the same cell
// select that one cell — which is why a plain click, where they are equal, is
// deliberately treated as no selection at all.
type selection struct {
	on     bool // a press has happened; anchor and head are meaningful
	drag   bool // the button is still down
	exact  bool // the endpoints were chosen, not dragged: one cell is enough
	anchor cellPos
	head   cellPos
}

// empty is "nothing to highlight or copy". A drag that never left its cell is
// an ordinary click and selects nothing, but a gesture that picked its own
// endpoints — a double-clicked one-letter word — means the single cell it
// named, because the range is inclusive.
func (s selection) empty() bool {
	if !s.on {
		return true
	}
	return s.anchor == s.head && !s.exact
}

// bounds normalises a reverse drag: the range is always stated in reading
// order, whichever way the pointer travelled.
func (s selection) bounds() (from, to cellPos) {
	if s.head.before(s.anchor) {
		return s.head, s.anchor
	}
	return s.anchor, s.head
}

// selSpan is the inclusive column range the selection covers on one transcript
// line: the first line starts at the anchor, the last ends at the head, and
// everything between is the whole row. ok is false for a line outside it.
func selSpan(line int, from, to cellPos, width int) (lo, hi int, ok bool) {
	if line < from.line || line > to.line || width <= 0 {
		return 0, 0, false
	}
	lo, hi = 0, width-1
	if line == from.line {
		lo = from.col
	}
	if line == to.line {
		hi = to.col
	}
	if hi < lo {
		return 0, 0, false
	}
	return lo, hi, true
}

// transcriptCell is the cell under the pointer. y is clamped into the band and
// the line into the content, so a drag that runs past the top or the bottom
// still has a cell to extend to rather than losing the selection.
func (m Model) transcriptCell(x, y int) (cellPos, bool) {
	rows := m.cur().drawnLen()
	tr := m.lay.Region(regionTranscript)
	if tr.Empty() || m.width <= 0 || rows == 0 {
		return cellPos{}, false
	}
	y = clampInt(y, tr.Top, tr.Bottom-1)
	line := clampInt(m.vp.YOffset+(y-tr.Top), 0, rows-1)
	return cellPos{line: line, col: clampInt(x, 0, m.width-1)}, true
}

// selectable says whether a press at (x, y) starts a selection. Only the
// transcript is selectable: every other band has a click meaning of its own,
// and the modal layer owns the whole mouse while it is up.
func (m Model) selectable(x, y int) bool {
	if !m.mouseEnabled || m.lay.TooSmall || !m.lay.Dialog.Empty() {
		return false
	}
	tr := m.lay.Region(regionTranscript)
	rows := m.cur().drawnLen()
	if !tr.Contains(y) || rows == 0 {
		return false
	}
	// The band is taller than the content until the transcript fills it, and
	// the blank cells below the last row hold no text: a press there starts
	// nothing. transcriptCell clamps onto the last row on purpose — that is for
	// a drag already under way running off the end, not for a press.
	if m.vp.YOffset+(y-tr.Top) >= rows {
		return false
	}
	return x >= 0 && x < m.width
}

// selectionText is what the selection copies: the plain rows, cut to the
// selected cells; the rows it touches are the only ones made plain. A span
// that runs past the end of its row takes the line break with it, which is
// what makes a multi-row selection paste as multiple lines.
func (m Model) selectionText() string {
	if m.sel.empty() {
		return ""
	}
	from, to := m.sel.bounds()
	tr := m.cur()
	rows := tr.drawnLen()
	var b strings.Builder
	for line := from.line; line <= to.line; line++ {
		if line < 0 || line >= rows {
			continue
		}
		lo, hi, ok := selSpan(line, from, to, m.width)
		if !ok {
			continue
		}
		plain := tr.plainRow(line)
		w := ansi.StringWidth(plain)
		b.WriteString(cutCells(plain, lo, min(hi+1, w)))
		// Every row but the last one of the selection is followed by the row
		// under it, so it keeps its line break whatever its width — a row that
		// happens to fill the terminal exactly would otherwise be glued to the
		// next one. On the last row the break is the span's own: it is there
		// only when the selection reached past the end of the text.
		if line < to.line || hi+1 > w {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// cutCells returns the graphemes of s covering display cells [lo, hi). A
// grapheme straddling either edge is taken whole: half a wide character is not
// a character, so the selection snaps outward rather than cutting one in two.
func cutCells(s string, lo, hi int) string {
	if hi <= lo || s == "" {
		return ""
	}
	var b strings.Builder
	x := 0
	for cluster, w := range displayCells(s) {
		if x < hi && x+w > lo {
			b.WriteString(cluster)
		}
		x += w
		if x >= hi {
			break
		}
	}
	return b.String()
}

// wordAt is the run of non-space cells around col, as an inclusive cell range.
// A press on whitespace selects nothing: there is no word under it.
func wordAt(s string, col int) (lo, hi int, ok bool) {
	type cell struct {
		x, w  int
		space bool
	}
	var cells []cell
	x := 0
	for cluster, w := range displayCells(s) {
		cells = append(cells, cell{x: x, w: w, space: strings.TrimSpace(cluster) == ""})
		x += w
	}
	at := -1
	for i, c := range cells {
		if col >= c.x && col < c.x+c.w {
			at = i
			break
		}
	}
	if at < 0 || cells[at].space {
		return 0, 0, false
	}
	first, last := at, at
	for first > 0 && !cells[first-1].space {
		first--
	}
	for last < len(cells)-1 && !cells[last+1].space {
		last++
	}
	return cells[first].x, cells[last].x + cells[last].w - 1, true
}

// transcriptView is the transcript band. Without a selection it is the
// viewport's own view; with one the rows come from the drawn transcript — the
// exact styled lines the viewport was given — so the highlight is painted over
// the cached render rather than through it, and selecting re-renders nothing.
func (m Model) transcriptView() string {
	if m.sel.empty() || m.vp.Height <= 0 || m.width <= 0 {
		return m.inspectedViewport()
	}
	from, to := m.sel.bounds()
	bg := selectionSeq(m.theme.profile, m.theme.SelectionBG)
	top := m.vp.YOffset
	var rows []string
	if drawn := m.cur().drawn; drawn != nil {
		rows = drawn.appendRows(make([]string, 0, m.vp.Height), top, min(top+m.vp.Height, drawn.total))
	}
	for k, row := range rows {
		row = padRow(row, m.width)
		if lo, hi, ok := selSpan(top+k, from, to, m.width); ok {
			row = highlightSpan(row, lo, hi, bg)
		}
		rows[k] = row
	}
	return strings.Join(rows, "\n")
}

// selectionSeq is the raw SGR that turns the selection band on under profile
// p. It is raw rather than a lipgloss style because the band has to survive
// every sequence already inside the row, and a style would close itself with a
// reset that takes the row's own colours with it. An empty string means the
// profile has no colour to give, and the highlight is simply not drawn.
//
// The band is c's own colour for the depth: the hex at true colour, and at 256
// colours c's own index, never termenv's conversion of the hex (SF-142, see
// tone). At 16 colours it is reverse video, because no index of the user's
// own palette keeps FG, Dim and Bright all legible on it; reverse keeps each
// slot's own contrast against the background, whatever the palette is.
// highlightSpan's closing reset turns it off after the span.
func selectionSeq(p colorprofile.Profile, c color.Color) string {
	switch p {
	case colorprofile.ANSI:
		return ansi.NewStyle().Reverse(true).String()
	case colorprofile.ANSI256, colorprofile.TrueColor:
		return ansi.NewStyle().BackgroundColor(c).String()
	default:
		return ""
	}
}

// selectionBand is st on the selection band under profile p: the band's own
// colour, or reverse video at 16 colours (selectionSeq says why). It is the
// style half of selectionSeq, for a row craze styles itself: the dialog
// cursor row and the /model value row.
func selectionBand(p colorprofile.Profile, st lipgloss.Style, c color.Color) lipgloss.Style {
	if p == colorprofile.ANSI {
		return st.Reverse(true)
	}
	return st.Background(c)
}

// highlightSpan paints cells [from, to] of an already-rendered row with the
// selection background, leaving the row exactly as wide as it was.
//
// The row is split at cell boundaries with the same truncation pair spliceRow
// uses — truncateCells stops short of a straddling grapheme, dropCells replays
// every sequence it skipped — so the head and the tail keep their own styles.
// Inside the span the background is re-emitted after every escape sequence,
// reset or not, which is what makes a diff row's own background, a bold
// markdown run and an OSC-8 link all keep the highlight.
//
// Both cuts snap outward, the way cutCells does, so the highlight covers
// exactly the graphemes the copy takes. The head falls short of a grapheme
// straddling `from`, which leaves it in the middle; the right edge needs a
// nudge, because truncation would leave a straddling grapheme in the tail —
// copied but unpainted.
func highlightSpan(line string, from, to int, bg string) string {
	w := ansi.StringWidth(line)
	if bg == "" || w == 0 || to < from || from > w-1 {
		return line
	}
	from = max(from, 0)
	to = min(to, w-1)

	head := truncateCells(line, from, "")
	hw := ansi.StringWidth(head)
	rest := dropCells(line, hw, "")
	span := to + 1 - hw
	mid := truncateCells(rest, span, "")
	mw := ansi.StringWidth(mid)
	// Ask for one more cell until the cut reaches the end of the span. mw only
	// grows, and hw+mw == w is the whole row, so this terminates.
	for n := span + 1; mw < span && hw+mw < w; n++ {
		mid = truncateCells(rest, n, "")
		mw = ansi.StringWidth(mid)
	}
	tail := ""
	if hw+mw < w {
		tail = dropCells(rest, mw, "")
	}

	var b strings.Builder
	b.WriteString(head)
	pending := true
	walkANSI(mid, func(chunk string, esc bool) {
		if esc {
			b.WriteString(chunk)
			pending = true
			return
		}
		if pending {
			b.WriteString(bg)
			pending = false
		}
		b.WriteString(chunk)
	})
	// The background must not run past the span, and must not run past the row
	// either: a dangling SGR would colour the next line the renderer writes.
	b.WriteString(ansi.ResetStyle)
	b.WriteString(tail)
	return b.String()
}

// walkANSI hands fn each escape sequence and each printable grapheme of s in
// order, so a caller can rewrite the printable cells without disturbing the
// sequences between them.
func walkANSI(s string, fn func(chunk string, esc bool)) {
	for chunk, width := range displayCells(s) {
		fn(chunk, width < 0)
	}
}

// escLen is the length of the escape sequence starting at s[0]: a CSI runs to
// its final byte, an OSC to BEL or ST, anything else is two bytes.
func escLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']':
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == ansi.ESC && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	}
	return 2
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
