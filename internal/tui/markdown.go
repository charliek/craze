package tui

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// markdown-lite: headings, bullet and numbered lists, fenced and inline code,
// bold, italic, blockquote, rules and pipe tables. Links stay literal, and an
// unmatched marker prints as typed. No glamour.

const (
	// listIndent is one nesting step, and maxListDepth caps how deep it goes.
	listIndent   = 2
	maxListDepth = 3
	// codeBar is the dim rail a fenced block hangs off.
	codeBar = "  │ "
)

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	bulletRe  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	numberRe  = regexp.MustCompile(`^(\s*)(\d{1,9})[.)]\s+(.*)$`)
	fenceRe   = regexp.MustCompile("^\\s*(```+|~~~+)\\s*(\\S*)\\s*$")
	quoteRe   = regexp.MustCompile(`^\s*>\s?(.*)$`)
)

// renderMarkdown turns one assistant message into styled, wrapped rows.
func renderMarkdown(text string, width int, th Theme) []string {
	if width <= 0 {
		return nil
	}
	r := mdRenderer{width: width, th: th}
	r.run(text, 0, false)
	return r.trimTrailingBlank()
}

// mdCheckpoint is a place where a later render of the same growing text can
// pick up from where this one stood (plan 032 §3.3 C6): the text's append
// generation, the source length before the line the render resumes at, the
// rows written before that line — untrimmed, and with no spare capacity, so
// the resumed render's first row copies them rather than writing into an array
// rows already drawn still share — and the width and theme they were drawn at.
//
// # Why resuming is byte-identical
//
// The main loop (run) carries nothing from one block to the next but the rows
// written (out) and the paragraph being gathered (para); and it reads the
// source forwards only. So at the top of the loop, with para empty, the rows
// still to come depend only on the lines from there on and on out — read back
// only by blank(), which looks at the last row, and by trimTrailingBlank at the
// end, which is why out is kept untrimmed. Rendering those lines with out as
// it stood is the same render, provided nothing the loop read to get there
// can change. The text only grows by appends while its generation holds
// (transcript.Transcript.TailGen), so what can change is the last line — the
// unterminated tail — and a checkpoint is taken only where every line the loop
// has read to reach it is complete, followed by a "\n". The furthest the loop
// reads before arriving at line c is c itself, through its look-aheads:
//
//   - a paragraph line i is tested with isTable(lines, i), which reads line
//     i+1 — a line that will not be the table's delimiter yet may become one
//     — so a paragraph that ended at c-1 read line c;
//   - a table reads its rows until the line that ends it, and returns the line
//     before it: a table that ended at c-1 read line c, which may still grow
//     into a row (`| a |`, `| - |`, `long` and then ` |`);
//   - a fence — on its own, or owned by a list item — reads up to its closing
//     marker and returns that line, so one closed at c-1 read nothing past it,
//     and one still open reads every line and leaves no line c at all (its
//     marker, arriving in pieces, is the tail until it is whole);
//   - headings, rules, quotes, blanks and list items read their own line.
//
// So the rule is that line c itself is complete: c is not the source's last
// line when the source is split at every "\n" (c ≤ len(lines) − 2 for those
// lines, whose last element is the unterminated tail). Under it, a resumed
// render equals a full one (TestStreamingMarkdownResumesByteIdentical).
//
// # What this does not make linear (SF-105)
//
// A checkpoint sits only between blocks, at the start of the block before the
// one still growing, so a resumed render reads the last two blocks or so: text
// of many blocks costs about the same per chunk however long it gets. A single
// growing block — one long paragraph, a table, an unterminated fence — has no
// boundary inside it, and is rendered whole on every render: live, on every
// chunk, up to the stream cap's 64 KiB (~13–15 ms a render there); during a
// replay, only on the paints the replay's cadence makes (finish), every 256
// events. Once the run is past the cap its head moves with every chunk, the
// generation with it, and every render is a full one.
type mdCheckpoint struct {
	gen   uint64
	off   int
	out   []string
	width int
	theme string
}

// renderStreamingMarkdown is renderMarkdown for the text of a streaming entry
// at append generation gen (gen > 0): it resumes from *cp when *cp is this
// text's — the generation, width and theme it was taken at, and a text still
// longer than its source length — and otherwise renders from the top; either
// way it leaves in *cp the furthest checkpoint the render passed, nil when it
// passed none. parsed is how many bytes of text the renderer read, the pane's
// work counter.
func renderStreamingMarkdown(text string, width int, th Theme, gen uint64, cp **mdCheckpoint) (rows []string, parsed int) {
	if width <= 0 {
		*cp = nil
		return nil, 0
	}
	r := mdRenderer{width: width, th: th}
	from := 0
	if c := *cp; c != nil && c.gen == gen && c.width == width && c.theme == th.Name && len(text) > c.off {
		from, r.out = c.off, c.out
	}
	off, n := r.run(text, from, true)
	*cp = nil
	if off > 0 {
		*cp = &mdCheckpoint{gen: gen, off: off, out: slices.Clip(r.out[:n]), width: width, theme: th.Name}
	}
	return r.trimTrailingBlank(), len(text) - from
}

// run is the main loop over text from byte from, which is 0 or a checkpoint's
// source length. With record set it returns the furthest checkpoint it
// passed — the source length at it and how many rows out held there — and an
// off of 0 when it passed none past the start (mdCheckpoint has the rule).
// The paragraph left gathering at the end is flushed; the trailing blank rows
// are the caller's to trim.
func (r *mdRenderer) run(text string, from int, record bool) (off, rows int) {
	lines := strings.Split(strings.TrimRight(text[from:], "\n"), "\n")
	at := from // the source offset of lines[i]
	for i := 0; i < len(lines); i++ {
		if record && len(r.para) == 0 && at > 0 && at+len(lines[i]) < len(text) {
			off, rows = at, len(r.out)
		}
		last := r.block(lines, i)
		for ; i < last; i++ {
			at += len(lines[i]) + 1
		}
		at += len(lines[i]) + 1
	}
	r.flush()
	return off, rows
}

// block renders the block that starts at line i, and returns the index of its
// last line: i itself, or further for a fence, a table, or a list item that
// owns a fence. A paragraph line is gathered, not written (flush).
func (r *mdRenderer) block(lines []string, i int) int {
	ln := strings.TrimRight(lines[i], " \t")
	if f := fenceRe.FindStringSubmatch(ln); f != nil {
		r.flush()
		return r.fence(lines, i, f[1], f[2], "")
	}
	switch {
	case strings.TrimSpace(ln) == "":
		r.flush()
		r.blank()
	case headingRe.MatchString(ln):
		r.flush()
		r.heading(ln)
	case isRule(ln):
		r.flush()
		r.rule()
	case quoteRe.MatchString(ln):
		r.flush()
		r.quote(ln)
	case bulletRe.MatchString(ln) || numberRe.MatchString(ln):
		r.flush()
		return r.list(lines, i)
	case isTable(lines, i):
		r.flush()
		return r.table(lines, i)
	default:
		r.para = append(r.para, strings.TrimSpace(ln))
	}
	return i
}

type mdRenderer struct {
	width int
	th    Theme
	out   []string
	para  []string
}

func (r *mdRenderer) push(prefix string, body string, st lipgloss.Style) {
	r.out = append(r.out, renderSegs(r.width, seg{prefix + body, st}))
}

func (r *mdRenderer) blank() {
	if len(r.out) > 0 && r.out[len(r.out)-1] != "" {
		r.out = append(r.out, "")
	}
}

func (r *mdRenderer) trimTrailingBlank() []string {
	for len(r.out) > 0 && r.out[len(r.out)-1] == "" {
		r.out = r.out[:len(r.out)-1]
	}
	return r.out
}

// flush wraps the pending paragraph. Inline markers are styled before wrapping
// so a span that survives the line break keeps its colour.
func (r *mdRenderer) flush() {
	if len(r.para) == 0 {
		return
	}
	// Inline markers are matched per source line: §3.4 pins a pair to one line,
	// and joining first would let *foo\nbar* become one italic span.
	styled := make([]string, len(r.para))
	for i, ln := range r.para {
		styled[i] = inlineMarkdown(ln, r.th)
	}
	text := strings.Join(styled, " ")
	r.para = nil
	r.out = append(r.out, hangingRows(text, "", "", r.width, styleFG(r.th.Assistant))...)
}

func (r *mdRenderer) heading(ln string) {
	mt := headingRe.FindStringSubmatch(ln)
	st := lipgloss.NewStyle().Foreground(r.th.Heading).Bold(true)
	r.push("", inlineMarkdown(strings.TrimSpace(mt[2]), r.th), st)
}

func (r *mdRenderer) rule() {
	w := proseWidth(r.width)
	r.push("", strings.Repeat("─", w), styleFG(r.th.Dim))
}

func (r *mdRenderer) quote(ln string) {
	body := quoteRe.FindStringSubmatch(ln)[1]
	r.out = append(r.out, hangingRows(inlineMarkdown(body, r.th), "│ ", "│ ", r.width, styleFG(r.th.Dim))...)
}

// list renders one item and returns the index of its last source line: an item
// whose body opens a fence owns the block that follows it.
func (r *mdRenderer) list(lines []string, i int) int {
	ln := strings.TrimRight(lines[i], " \t")
	var lead, marker, body string
	if mt := numberRe.FindStringSubmatch(ln); mt != nil {
		lead, marker, body = mt[1], mt[2]+".", mt[3]
	} else {
		mt := bulletRe.FindStringSubmatch(ln)
		lead, marker, body = mt[1], "•", mt[2]
	}
	depth := len(strings.ReplaceAll(lead, "\t", "  ")) / listIndent
	if depth > maxListDepth {
		depth = maxListDepth
	}
	pad := strings.Repeat(" ", depth*listIndent)
	if f := fenceRe.FindStringSubmatch(body); f != nil {
		r.push("", pad+marker, styleFG(r.th.Assistant))
		return r.fence(lines, i, f[1], f[2], pad)
	}
	first := pad + marker + " "
	cont := strings.Repeat(" ", lipgloss.Width(first))
	r.out = append(r.out, hangingRows(inlineMarkdown(body, r.th), first, cont, r.width, styleFG(r.th.Assistant))...)
	return i
}

// fence renders a code block: never wrapped, clipped to the terminal instead,
// behind a dim rail. An unterminated fence still renders as one.
func (r *mdRenderer) fence(lines []string, start int, marker, lang, pad string) int {
	bar := styleFG(r.th.Dim)
	if lang != "" {
		r.push(pad+codeBar, lang, bar)
	}
	i := start + 1
	for ; i < len(lines); i++ {
		if f := fenceRe.FindStringSubmatch(strings.TrimRight(lines[i], " \t")); f != nil && f[1][0] == marker[0] {
			return i
		}
		r.out = append(r.out, renderSegs(r.width,
			seg{pad + codeBar, bar},
			seg{plainLine(lines[i]), styleFG(r.th.FG)},
		))
	}
	return i - 1
}

// isRule matches a thematic break: three or more of the same marker, spaces
// aside. A list bullet never matches because it carries other text.
func isRule(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// tableGap is the column gutter. tableJunction is the same three cells drawn
// under it, so the rule meets the bar instead of slipping a column sideways.
const (
	tableGap      = " │ "
	tableJunction = "─┼─"
)

type mdAlign int

const (
	alignLeft mdAlign = iota
	alignCenter
	alignRight
)

// A delimiter cell is optional colons around one or more dashes: :---, ---:,
// :---:, ---. One dash is enough; the border pipes are not part of the cell.
var delimCellRe = regexp.MustCompile(`^:?-+:?$`)

// isTable reports whether line i opens a pipe table: a header row and, on the
// next line, a delimiter with one dash-cell per header cell. A pipe with no
// delimiter stays prose, which is what keeps "a | b" a sentence.
func isTable(lines []string, i int) bool {
	_, _, ok := tableHeader(lines, i)
	return ok
}

func tableHeader(lines []string, i int) (header []string, aligns []mdAlign, ok bool) {
	if i+1 >= len(lines) {
		return nil, nil, false
	}
	header = splitTableRow(lines[i])
	aligns = delimiterAligns(lines[i+1])
	if header == nil || aligns == nil || len(header) != len(aligns) {
		return nil, nil, false
	}
	return header, aligns, true
}

// table renders one pipe table and returns the index of its last line. The
// line after it — a blank, a heading, a list — is left for the main loop.
// Cells wrap inside their column, so a wide row stays columns instead of
// joining the next row into the paragraph.
func (r *mdRenderer) table(lines []string, i int) int {
	header, aligns, ok := tableHeader(lines, i)
	if !ok {
		return i
	}
	rows := [][]string{header}
	j := i + 2
	for ; j < len(lines); j++ {
		ln := strings.TrimRight(lines[j], " \t")
		cells := splitTableRow(ln)
		if tableRowBreak(ln) || cells == nil {
			break
		}
		rows = append(rows, fitRow(cells, len(aligns)))
	}
	r.drawTable(rows, aligns)
	return j - 1
}

// tableRowBreak is a line a table must not swallow. A cell may itself start
// with a dash ("- item" lives after the pipe); the line-level check is what
// ends the table, so a following list or heading stays that block.
func tableRowBreak(ln string) bool {
	if strings.TrimSpace(ln) == "" {
		return true
	}
	return fenceRe.MatchString(ln) || headingRe.MatchString(ln) || isRule(ln) ||
		quoteRe.MatchString(ln) || bulletRe.MatchString(ln) || numberRe.MatchString(ln)
}

// splitTableRow splits one markdown table row into cells. A leading or
// trailing pipe is the border, not a cell. A pipe inside `code`, or written
// as \|, stays in the cell. A line with no separator pipe is not a row.
func splitTableRow(line string) []string {
	rs := []rune(strings.TrimSpace(line))
	if len(rs) == 0 {
		return nil
	}
	var (
		cells []string
		b     strings.Builder
		saw   bool
	)
	for i := 0; i < len(rs); {
		if rs[i] == '`' {
			if j := indexRuneFrom(rs, '`', i+1); j > i+1 {
				b.WriteString(string(rs[i : j+1]))
				i = j + 1
				continue
			}
		}
		if rs[i] == '\\' && i+1 < len(rs) && rs[i+1] == '|' {
			b.WriteRune('|')
			i += 2
			continue
		}
		if rs[i] == '|' {
			saw = true
			cells = append(cells, cleanCell(b.String()))
			b.Reset()
			i++
			continue
		}
		b.WriteRune(rs[i])
		i++
	}
	if !saw {
		return nil
	}
	cells = append(cells, cleanCell(b.String()))
	if rs[0] == '|' && len(cells) > 0 && cells[0] == "" {
		cells = cells[1:]
	}
	if bareTrailingPipe(rs) && len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	if len(cells) == 0 {
		return nil
	}
	return cells
}

func cleanCell(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\t", " "))
}

// bareTrailingPipe reports whether the line ends in a separator pipe. An odd
// run of backslashes escapes it, so "\|" is a cell that happens to end in |.
func bareTrailingPipe(rs []rune) bool {
	if len(rs) == 0 || rs[len(rs)-1] != '|' {
		return false
	}
	n := 0
	for i := len(rs) - 2; i >= 0 && rs[i] == '\\'; i-- {
		n++
	}
	return n%2 == 0
}

func delimiterAligns(line string) []mdAlign {
	cells := splitTableRow(line)
	if len(cells) == 0 {
		return nil
	}
	aligns := make([]mdAlign, len(cells))
	for i, c := range cells {
		if !delimCellRe.MatchString(c) {
			return nil
		}
		switch {
		case strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":"):
			aligns[i] = alignCenter
		case strings.HasSuffix(c, ":"):
			aligns[i] = alignRight
		default:
			aligns[i] = alignLeft
		}
	}
	return aligns
}

// fitRow pads a short body row and folds extra cells into the last column so
// a row with one pipe too many does not drop the tail.
func fitRow(cells []string, n int) []string {
	if n < 1 || len(cells) == n {
		return cells
	}
	out := make([]string, n)
	if len(cells) < n {
		copy(out, cells)
		return out
	}
	copy(out, cells[:n-1])
	out[n-1] = strings.Join(cells[n-1:], " | ")
	return out
}

// fitColumns shrinks the widest column until the row fits budget. A short
// label column keeps its width while the long one wraps; equal columns give
// up the rightmost first, which is where the prose usually is.
func fitColumns(natural []int, budget int) []int {
	widths := append([]int(nil), natural...)
	sum := 0
	for _, w := range widths {
		sum += w
	}
	over := sum - budget
	if budget < 0 {
		over = sum
	}
	for over > 0 {
		maxI := -1
		for i, w := range widths {
			if w > 1 && (maxI < 0 || w >= widths[maxI]) {
				maxI = i
			}
		}
		if maxI < 0 {
			break
		}
		widths[maxI]--
		over--
	}
	return widths
}

func (r *mdRenderer) drawTable(rows [][]string, aligns []mdAlign) {
	n := len(aligns)
	if n == 0 || len(rows) == 0 {
		return
	}
	styled := make([][]string, len(rows))
	natural := make([]int, n)
	for ri, row := range rows {
		styled[ri] = make([]string, n)
		for i := 0; i < n; i++ {
			if i < len(row) && row[i] != "" {
				styled[ri][i] = inlineMarkdown(row[i], r.th)
			}
			if w := lipgloss.Width(styled[ri][i]); w > natural[i] {
				natural[i] = w
			}
		}
	}
	for i := range natural {
		if natural[i] < 1 {
			natural[i] = 1
		}
	}
	gaps := 0
	if n > 1 {
		gaps = (n - 1) * lipgloss.Width(tableGap)
	}
	widths := fitColumns(natural, proseWidth(r.width)-gaps)
	head := lipgloss.NewStyle().Foreground(r.th.Bright).Bold(true)
	body := styleFG(r.th.Assistant)
	dim := styleFG(r.th.Dim)
	r.drawTableRow(styled[0], widths, aligns, head, dim)
	r.drawTableRule(widths, dim)
	for _, row := range styled[1:] {
		r.drawTableRow(row, widths, aligns, body, dim)
	}
}

func (r *mdRenderer) drawTableRow(cells []string, widths []int, aligns []mdAlign, textSt, barSt lipgloss.Style) {
	wrapped := make([][]string, len(widths))
	height := 1
	for i, w := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		lines := wrapCell(cell, w)
		wrapped[i] = lines
		if len(lines) > height {
			height = len(lines)
		}
	}
	for line := 0; line < height; line++ {
		segs := make([]seg, 0, len(widths)*2)
		for i, w := range widths {
			if i > 0 {
				segs = append(segs, seg{tableGap, barSt})
			}
			text := ""
			if line < len(wrapped[i]) {
				text = wrapped[i][line]
			}
			align := alignLeft
			if i < len(aligns) {
				align = aligns[i]
			}
			segs = append(segs, seg{padCell(text, w, align), textSt})
		}
		r.out = append(r.out, renderSegs(r.width, segs...))
	}
}

func (r *mdRenderer) drawTableRule(widths []int, st lipgloss.Style) {
	var b strings.Builder
	for i, w := range widths {
		if i > 0 {
			b.WriteString(tableJunction)
		}
		if w > 0 {
			b.WriteString(strings.Repeat("─", w))
		}
	}
	r.out = append(r.out, renderSegs(r.width, seg{b.String(), st}))
}

// wrapCell wraps already-styled cell text to width. Markers are resolved
// before this, so a span broken across the wrap keeps the colour it opened
// with, the same way a paragraph does.
func wrapCell(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if s == "" {
		return []string{""}
	}
	wrapped := ansi.Hardwrap(ansi.Wordwrap(s, width, ""), width, true)
	lines := strings.Split(wrapped, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, ln := range lines {
		if lipgloss.Width(ln) > width {
			lines[i] = clampWidth(ln, width)
		}
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func padCell(s string, width int, align mdAlign) string {
	gap := width - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	switch align {
	case alignRight:
		return strings.Repeat(" ", gap) + s
	case alignCenter:
		left := gap / 2
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
	default:
		return s + strings.Repeat(" ", gap)
	}
}

// inlineMarkdown styles `code`, **bold** and *italic* / _italic_ spans. A
// marker without its partner on the same line is left as typed.
func inlineMarkdown(s string, th Theme) string {
	code := styleFG(th.ToolKind)
	bold := lipgloss.NewStyle().Bold(true)
	italic := lipgloss.NewStyle().Italic(true)
	boldItalic := lipgloss.NewStyle().Bold(true).Italic(true)

	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		switch {
		case rs[i] == '`':
			if j := indexRuneFrom(rs, '`', i+1); j > i+1 {
				b.WriteString(code.Render(string(rs[i+1 : j])))
				i = j + 1
				continue
			}
		case runAt(rs, i, '*', 3):
			if j := indexRunFrom(rs, '*', 3, i+3); j > i+3 {
				b.WriteString(boldItalic.Render(string(rs[i+3 : j])))
				i = j + 3
				continue
			}
		case rs[i] == '*' && i+1 < len(rs) && rs[i+1] == '*':
			if j := indexRunFrom(rs, '*', 2, i+2); j > i+2 {
				b.WriteString(bold.Render(string(rs[i+2 : j])))
				i = j + 2
				continue
			}
		case rs[i] == '*' || rs[i] == '_':
			if j := indexRuneFrom(rs, rs[i], i+1); j > i+1 && rs[i+1] != ' ' && rs[j-1] != ' ' && !intraword(rs, i, j) {
				b.WriteString(italic.Render(string(rs[i+1 : j])))
				i = j + 1
				continue
			}
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String()
}

// intraword keeps snake_case_names literal: an underscore only opens emphasis
// when it sits on a word boundary.
func intraword(rs []rune, open, close int) bool {
	if rs[open] != '_' {
		return false
	}
	if open > 0 && wordRune(rs[open-1]) {
		return true
	}
	return close+1 < len(rs) && wordRune(rs[close+1])
}

func wordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func indexRuneFrom(rs []rune, r rune, from int) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// runAt reports whether n copies of r start at i.
func runAt(rs []rune, i int, r rune, n int) bool {
	if i+n > len(rs) {
		return false
	}
	for k := 0; k < n; k++ {
		if rs[i+k] != r {
			return false
		}
	}
	return true
}

func indexRunFrom(rs []rune, r rune, n, from int) int {
	for i := from; i+n <= len(rs); i++ {
		if runAt(rs, i, r, n) {
			return i
		}
	}
	return -1
}
