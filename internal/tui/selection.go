package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"
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
	rows := m.cur().transcriptRows
	tr := m.lay.Region(regionTranscript)
	if tr.Empty() || m.width <= 0 || len(rows) == 0 {
		return cellPos{}, false
	}
	y = clampInt(y, tr.Top, tr.Bottom-1)
	line := clampInt(m.vp.YOffset+(y-tr.Top), 0, len(rows)-1)
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
	rows := m.cur().transcriptRows
	if !tr.Contains(y) || len(rows) == 0 {
		return false
	}
	// The band is taller than the content until the transcript fills it, and
	// the blank cells below the last row hold no text: a press there starts
	// nothing. transcriptCell clamps onto the last row on purpose — that is for
	// a drag already under way running off the end, not for a press.
	if m.vp.YOffset+(y-tr.Top) >= len(rows) {
		return false
	}
	return x >= 0 && x < m.width
}

// selectionText is what the selection copies: the plain rows, cut to the
// selected cells. A span that runs past the end of its row takes the line break
// with it, which is what makes a multi-row selection paste as multiple lines.
func (m Model) selectionText() string {
	if m.sel.empty() {
		return ""
	}
	from, to := m.sel.bounds()
	plainRows := m.cur().transcriptPlain
	var b strings.Builder
	for line := from.line; line <= to.line; line++ {
		if line < 0 || line >= len(plainRows) {
			continue
		}
		lo, hi, ok := selSpan(line, from, to, m.width)
		if !ok {
			continue
		}
		plain := plainRows[line]
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
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		w := max(g.Width(), 1)
		if x < hi && x+w > lo {
			b.WriteString(g.Str())
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
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		w := max(g.Width(), 1)
		cells = append(cells, cell{x: x, w: w, space: strings.TrimSpace(g.Str()) == ""})
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
		return m.vp.View()
	}
	from, to := m.sel.bounds()
	bg := selectionSeq(m.theme.SelectionBG)
	drawn := m.cur().transcriptRows
	rows := make([]string, 0, m.vp.Height)
	for i := m.vp.YOffset; i < m.vp.YOffset+m.vp.Height && i < len(drawn); i++ {
		row := padRow(drawn[i], m.width)
		if lo, hi, ok := selSpan(i, from, to, m.width); ok {
			row = highlightSpan(row, lo, hi, bg)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

// selectionSeq is the raw SGR that turns the selection background on. It is
// raw rather than a lipgloss style because the background has to survive every
// sequence already inside the row, and a style would close itself with a reset
// that takes the row's own colours with it. An empty string means the profile
// has no colour to give, and the highlight is simply not drawn.
func selectionSeq(c lipgloss.Color) string {
	col := lipgloss.ColorProfile().Color(string(c))
	if col == nil {
		return ""
	}
	seq := col.Sequence(true)
	if seq == "" {
		return ""
	}
	return termenv.CSI + seq + "m"
}

// highlightSpan paints cells [from, to] of an already-rendered row with the
// selection background, leaving the row exactly as wide as it was.
//
// The row is split at cell boundaries with the same truncation pair spliceRow
// uses — Truncate stops short of a straddling grapheme, TruncateLeft replays
// every sequence it skipped — so the head and the tail keep their own styles.
// Inside the span the background is re-emitted after every escape sequence,
// reset or not, which is what makes a diff row's own background, a bold
// markdown run and an OSC-8 link all keep the highlight.
//
// Both cuts snap outward, the way cutCells does, so the highlight covers
// exactly the graphemes the copy takes. The head falls short of a grapheme
// straddling `from`, which leaves it in the middle; the right edge needs a
// nudge, because Truncate would leave a straddling grapheme in the tail —
// copied but unpainted.
func highlightSpan(line string, from, to int, bg string) string {
	w := ansi.StringWidth(line)
	if bg == "" || w == 0 || to < from || from > w-1 {
		return line
	}
	from = max(from, 0)
	to = min(to, w-1)

	head := ansi.Truncate(line, from, "")
	hw := ansi.StringWidth(head)
	rest := ansi.TruncateLeft(line, hw, "")
	span := to + 1 - hw
	mid := ansi.Truncate(rest, span, "")
	mw := ansi.StringWidth(mid)
	// Ask for one more cell until the cut reaches the end of the span. mw only
	// grows, and hw+mw == w is the whole row, so this terminates.
	for n := span + 1; mw < span && hw+mw < w; n++ {
		mid = ansi.Truncate(rest, n, "")
		mw = ansi.StringWidth(mid)
	}
	tail := ""
	if hw+mw < w {
		tail = ansi.TruncateLeft(rest, mw, "")
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
	state := -1
	for len(s) > 0 {
		if s[0] == ansi.ESC {
			n := escLen(s)
			fn(s[:n], true)
			s = s[n:]
			state = -1
			continue
		}
		cluster, rest, _, ns := uniseg.FirstGraphemeClusterInString(s, state)
		if cluster == "" {
			fn(s, false)
			return
		}
		fn(cluster, false)
		s, state = rest, ns
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
