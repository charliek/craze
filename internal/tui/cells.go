package tui

import (
	"iter"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// displayCells walks a styled row using the same grapheme widths as the
// renderer. Escape sequences have width -1, so zero-width text is distinct.
func displayCells(s string) iter.Seq2[string, int] {
	return func(yield func(string, int) bool) {
		for len(s) > 0 {
			var chunk string
			var width int
			if s[0] == ansi.ESC {
				chunk, width = s[:escLen(s)], -1
			} else {
				chunk, width = ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
			}
			if !yield(chunk, width) {
				return
			}
			s = s[len(chunk):]
		}
	}
}

// truncateCells keeps a prefix of a styled row, including the tail in the
// cell budget. All escape sequences survive, so a style closes even if its
// last characters were clipped. x/ansi v0.12's truncation splits a cluster
// beginning with ASCII (e.g. a keycap) although its StringWidth measures the
// whole cluster. Walk complete clusters for both decisions.
func truncateCells(s string, limit int, tail string) string {
	if ansi.StringWidth(s) <= limit {
		return s
	}
	room := limit - ansi.StringWidth(tail)
	if room < 0 {
		return ""
	}
	var b strings.Builder
	cut := false
	for chunk, width := range displayCells(s) {
		switch {
		case width < 0:
			b.WriteString(chunk)
		case cut:
		case width <= room:
			b.WriteString(chunk)
			room -= width
		default:
			b.WriteString(tail)
			cut = true
		}
	}
	return b.String()
}

// dropCells skips the first n cells, replaying their style before the suffix.
// A grapheme that straddles the cut belongs to the suffix.
func dropCells(s string, n int, prefix string) string {
	if n <= 0 {
		return s
	}
	var b strings.Builder
	x := 0
	for chunk, width := range displayCells(s) {
		if width < 0 {
			b.WriteString(chunk)
			continue
		}
		x += width
		if x > n {
			b.WriteString(prefix)
			prefix = ""
			b.WriteString(chunk)
		}
	}
	return b.String()
}
