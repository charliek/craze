// Package textdiff is a line-oriented Myers diff with a work cap, shared by
// internal/agent (change counts) and internal/tui (hunk rendering).
package textdiff

import (
	"errors"
	"strings"
)

// ErrTooLarge is returned instead of a diff when the inputs exceed maxLines or
// the edit-graph exploration exceeds maxSteps. Callers show "diff too large".
var ErrTooLarge = errors.New("textdiff: input too large")

const (
	// maxLines caps len(old)+len(new) in lines.
	maxLines = 10000
	// maxSteps caps the edit-graph exploration.
	maxSteps = 2000000
	// context is how many unchanged lines surround each change in a hunk.
	context = 2
)

// Line kinds.
const (
	Add     = '+'
	Del     = '-'
	Context = ' '
)

// Line is one rendered diff line. OldNo is 0 on an added line, NewNo is 0 on a
// removed line; both are 1-based.
type Line struct {
	Kind  byte
	Text  string
	OldNo int
	NewNo int
}

// Hunk is a run of changed lines plus up to `context` unchanged lines on each
// side. OldStart/NewStart are the 1-based line numbers of the first line.
type Hunk struct {
	OldStart int
	NewStart int
	Lines    []Line
}

// Lines diffs old against new by line. CRLF is normalised and a missing final
// newline is a normal last line. Identical inputs give 0/0 and no hunks.
func Lines(old, new string) (added, removed int, hunks []Hunk, err error) {
	a := splitLines(old)
	b := splitLines(new)
	if len(a)+len(b) > maxLines {
		return 0, 0, nil, ErrTooLarge
	}
	if len(a) == len(b) && equalLines(a, b) {
		return 0, 0, nil, nil
	}
	ops, err := script(a, b)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, o := range ops {
		switch o.kind {
		case Add:
			added++
		case Del:
			removed++
		}
	}
	return added, removed, group(ops), nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func equalLines(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type op struct {
	kind byte
	text string
	old  int // 1-based old line number, 0 on an add
	new  int // 1-based new line number, 0 on a delete
}

// script runs Myers' greedy algorithm, keeping one trimmed V snapshot per edit
// distance so the path can be walked back into an ordered edit script. The
// snapshot for distance d only spans k in [-d, d], which is all backtrack
// reads.
func script(a, b []string) ([]op, error) {
	n, m := len(a), len(b)
	max := n + m
	off := max
	v := make([]int, 2*max+1)
	var trace [][]int
	steps := 0

	for d := 0; d <= max; d++ {
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
		for k := -d; k <= d; k += 2 {
			steps++
			if steps > maxSteps {
				return nil, ErrTooLarge
			}
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
				steps++
			}
			if steps > maxSteps {
				return nil, ErrTooLarge
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace), nil
			}
		}
	}
	return nil, ErrTooLarge
}

func backtrack(a, b []string, trace [][]int) []op {
	x, y := len(a), len(b)
	var rev []op
	for d := len(trace) - 1; d >= 0; d-- {
		// At d == 0 the rest of the path is one diagonal back to the origin.
		prevX, prevY := 0, 0
		if d > 0 {
			v := trace[d]
			k := x - y
			prevK := k - 1
			if k == -d || (k != d && v[k-1+d] < v[k+1+d]) {
				prevK = k + 1
			}
			prevX = v[prevK+d]
			prevY = prevX - prevK
		}
		for x > prevX && y > prevY {
			rev = append(rev, op{kind: Context, text: a[x-1], old: x, new: y})
			x--
			y--
		}
		if d == 0 {
			break
		}
		if x == prevX {
			rev = append(rev, op{kind: Add, text: b[y-1], new: y})
		} else {
			rev = append(rev, op{kind: Del, text: a[x-1], old: x})
		}
		x, y = prevX, prevY
	}
	ops := make([]op, len(rev))
	for i, o := range rev {
		ops[len(rev)-1-i] = o
	}
	return ops
}

// group turns the edit script into hunks: every changed line pulls in the
// `context` lines around it, and overlapping windows merge into one hunk.
func group(ops []op) []Hunk {
	keep := make([]bool, len(ops))
	changed := false
	for i, o := range ops {
		if o.kind == Context {
			continue
		}
		changed = true
		lo := i - context
		if lo < 0 {
			lo = 0
		}
		hi := i + context
		if hi >= len(ops) {
			hi = len(ops) - 1
		}
		for j := lo; j <= hi; j++ {
			keep[j] = true
		}
	}
	if !changed {
		return nil
	}
	var hunks []Hunk
	oldNo, newNo := 0, 0
	for i := 0; i < len(ops); {
		if !keep[i] {
			oldNo, newNo = advance(ops[i], oldNo, newNo)
			i++
			continue
		}
		h := Hunk{OldStart: oldNo + 1, NewStart: newNo + 1}
		for i < len(ops) && keep[i] {
			o := ops[i]
			h.Lines = append(h.Lines, Line{Kind: o.kind, Text: o.text, OldNo: o.old, NewNo: o.new})
			oldNo, newNo = advance(o, oldNo, newNo)
			i++
		}
		hunks = append(hunks, h)
	}
	return hunks
}

func advance(o op, oldNo, newNo int) (int, int) {
	if o.old > 0 {
		oldNo = o.old
	}
	if o.new > 0 {
		newNo = o.new
	}
	return oldNo, newNo
}
