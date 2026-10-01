package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// rowSpan is one part of a pane's drawn rows: an entry's rendered rows, or the
// trim note's one row (e is nil), and the transcript row it starts at.
type rowSpan struct {
	e     *entry
	rows  []string
	start int
}

// rowIndex is the rows one paint of a pane drew (plan 032 §3.3 C5): each
// entry's own rendered slice, never copied, and the row it starts at; a row is
// one screen line (physicalLines). Nothing joins them: the viewport, the hit
// test and the highlight read a row through the starts, and only the rows they
// ask for.
//
// An index is immutable once a paint has made it. The pane keeps the latest
// (pane.drawn) and the viewport of every Model copy keeps the one it was given,
// so an older copy still draws exactly what it drew — bubbletea copies Model
// by value — while a paint never copies a row, and copies a span only when it
// must:
//
//   - tail holds the spans from the first one this paint changed, in a slice
//     of its own; base holds the spans before it, shared with the previous
//     index. A chunk into the last entry, or into one a local row was written
//     under, makes a tail of one or two spans, and the spans of the previous
//     tail that it keeps are appended to base.
//   - base only ever grows by an append past its end, which no index has seen,
//     because a paint that keeps fewer spans than base holds cuts it with a
//     full slice expression (len == cap): the next append to it copies first
//     rather than writing over a slot an older index still reads.
type rowIndex struct {
	base    []rowSpan
	tail    []rowSpan
	total   int
	key     renderKey
	trimmed bool
}

// spans is how many spans the index holds.
func (x *rowIndex) spans() int { return len(x.base) + len(x.tail) }

// span is the i-th span, base and tail as one list.
func (x *rowIndex) span(i int) *rowSpan {
	if i < len(x.base) {
		return &x.base[i]
	}
	return &x.tail[i-len(x.base)]
}

// find is the span holding row i (0 <= i < total) and its place in the list.
// A span with no rows starts where the next one does, so the search for the
// last span starting at or before i never lands on one.
func (x *rowIndex) find(i int) (*rowSpan, int) {
	if n := len(x.tail); n > 0 && i >= x.tail[0].start {
		k := sort.Search(n, func(k int) bool { return x.tail[k].start > i }) - 1
		return &x.tail[k], len(x.base) + k
	}
	k := sort.Search(len(x.base), func(k int) bool { return x.base[k].start > i }) - 1
	return &x.base[k], k
}

// appendRows appends rows [lo, hi) to dst: the span holding lo is found once
// and the rest are walked in order.
func (x *rowIndex) appendRows(dst []string, lo, hi int) []string {
	if lo >= hi {
		return dst
	}
	s, k := x.find(lo)
	for i := lo; ; {
		for ; i < hi && i-s.start < len(s.rows); i++ {
			dst = append(dst, s.rows[i-s.start])
		}
		if i >= hi {
			return dst
		}
		k++
		s = x.span(k)
	}
}

// drawnLen is how many rows the pane's last paint drew.
func (t *pane) drawnLen() int {
	if t.drawn == nil {
		return 0
	}
	return t.drawn.total
}

// plainRow is drawn row i as the selection reads it: the text without its
// styling or trailing blanks. Only a selection gesture asks, and only for the
// rows it touches, so nothing is stripped on a paint; an entry's plain rows
// are worked out the first time one of them is asked for and kept until the
// entry re-renders (entry.plain).
func (t *pane) plainRow(i int) string {
	s, _ := t.drawn.find(i)
	e := s.e
	if e == nil || !sameRows(e.rendered, s.rows) {
		return plainText(s.rows[i-s.start])
	}
	if e.plain == nil {
		e.plain = make([]string, len(e.rendered))
		for k, ln := range e.rendered {
			e.plain[k] = plainText(ln)
		}
	}
	return e.plain[i-s.start]
}

// plainText is one drawn row as the selection copies it.
func plainText(row string) string { return strings.TrimRight(ansi.Strip(row), " ") }

// sameRows reports whether a and b are the same slice: a rendered slice is
// never written to, so the same backing array and length are the same rows.
func sameRows(a, b []string) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// physicalLines is an entry's rendered rows as the screen lines they draw: a
// row holding a line break is the lines it breaks into. It is bubbles'
// SetContent (v0.21.0) — "\r\n" made "\n", then split at every "\n" — applied
// row by row, which is what that split of the joined transcript came to, so
// the viewport, the hit test, the highlight and the copy all count the lines
// bubbles' viewport counted. Nothing a renderer draws should hold a line
// break, but a value an agent sent can carry one through to a row: a tool's
// location "/tmp/a\nb" survives sanitizeText and toolRow draws it inside one
// row, which bubbles showed as two lines.
//
// The selection now reads those same lines. Before C5 it read the logical
// rows while the viewport showed the physical ones, so in this case the line a
// press landed on and the line it copied could differ; reading the physical
// lines is a deliberate fix of that, and changes no drawn byte.
//
// What the per-row split does not reproduce is a row ending in a lone "\r":
// joined to the next row it made "\r\n", which bubbles cut back to "\n". No row
// holds one — sanitizeText and sanitizeShellOutput drop "\r", and the tests'
// paint watch refuses any "\r" left in a drawn row.
//
// rows comes back untouched when no row holds a "\n" (one byte scan per row,
// no allocation); otherwise the lines are a new slice, so a rendered slice is
// never written to (rowIndex).
func physicalLines(rows []string) []string {
	first := -1
	for i, r := range rows {
		if strings.IndexByte(r, '\n') >= 0 {
			first = i
			break
		}
	}
	if first < 0 {
		return rows
	}
	out := make([]string, first, len(rows)+1)
	copy(out, rows[:first])
	for _, r := range rows[first:] {
		if strings.IndexByte(r, '\n') < 0 {
			out = append(out, r)
			continue
		}
		out = append(out, strings.Split(strings.ReplaceAll(r, "\r\n", "\n"), "\n")...)
	}
	return out
}

// needsRender says whether e has to be rendered again before it is drawn under
// key: its content changed, the key did, or it is a `!` row still running —
// that one draws the spinner, and the cache is keyed on things that do not
// move while it spins, so it is rendered on every paint until it settles. The
// tick is what asks for those paints (handleTick).
func (e *entry) needsRender(key renderKey) bool {
	return e.dirty || e.renderedFor != key || (e.shell != nil && !e.shell.done)
}

// paint brings tr's drawn rows up to date for m's key and returns them: dirty
// entries are rendered again, and the index is assembled from the first span
// that changed, keeping every span before it.
//
// The full path — every span made again, from none — is taken when there is no
// earlier index, when the list changed shape under it (pane.reshaped: the cap's
// dropFirst, removeRow, /clear), when the trim note came or went or draws
// differently, and when the key changed (a resize, a theme, Ctrl+O), which
// renders every entry again anyway. Otherwise the first span that changed is
// the first entry that has to be rendered, or that is not the entry the old
// index drew in its place, or the first entry the old index did not draw.
func (m *Model) paint(tr *pane) *rowIndex {
	key := m.renderKey()
	prev := tr.drawn
	var note string
	if tr.trimmed {
		// One physical line as it stands: the note is a constant with no line
		// break, so it needs no physicalLines.
		note = renderSegs(m.width, seg{trimmedNote, styleFG(m.theme.Dim)})
	}
	full := prev == nil || tr.reshaped || prev.key != key || prev.trimmed != tr.trimmed ||
		(tr.trimmed && prev.span(0).rows[0] != note)
	tr.reshaped = false
	off := 0
	if tr.trimmed {
		off = 1
	}
	from := 0
	if !full {
		painted := prev.spans() - off
		from = len(tr.rows)
		for i, e := range tr.rows {
			if i >= painted || prev.span(off+i).e != e || e.needsRender(key) {
				from = i
				break
			}
		}
		if from == len(tr.rows) && from == painted {
			return prev
		}
	}

	// The spans kept from prev, and the row the first new one starts at.
	keep, start := 0, 0
	var base []rowSpan
	if !full {
		keep = off + from
		if keep > 0 {
			last := prev.span(keep - 1)
			start = last.start + len(last.rows)
		}
		if keep >= len(prev.base) {
			base = append(prev.base, prev.tail[:keep-len(prev.base)]...)
		} else {
			base = prev.base[:keep:keep]
		}
	}
	tail := make([]rowSpan, 0, off+len(tr.rows)-keep)
	if full && tr.trimmed {
		tail = append(tail, rowSpan{rows: []string{note}})
		start = 1
	}
	for _, e := range tr.rows[from:] {
		if e.needsRender(key) {
			// Split here and nowhere earlier: a streaming reply's markdown
			// checkpoint keeps the renderer's own rows (mdCheckpoint.out), and
			// a resumed render goes on from those, not from these.
			e.rendered = physicalLines(m.renderEntry(tr, e, key))
			e.plain = nil
			e.renderedFor = key
			e.dirty = false
			tr.renders++
		}
		tail = append(tail, rowSpan{e: e, rows: e.rendered, start: start})
		start += len(e.rendered)
	}
	tr.work.spans += len(tail)
	return &rowIndex{base: base, tail: tail, total: start, key: key, trimmed: tr.trimmed}
}
