// Package redact keeps craze's own provider keys out of what the native
// harness shows a model, an event, the transcript or a spill file: it
// replaces every occurrence of a set of secret values with Marker (plan 019
// §3.8).
//
// It matches exact values only. There is no URL rule, unlike the llm
// package's error scrubber: a tool's output is file content and command
// output, and rewriting a query string there would corrupt text the edit
// tool could then write back to disk. It is also no security boundary: a
// model with a shell can encode a key before printing it. What it stops is
// the accidental, verbatim disclosure — `env`, `cat`, a grep hit, a diff.
//
// A Replacer is immutable and safe for concurrent use. A Writer, which
// redacts a stream, belongs to one goroutine at a time, like a bufio.Writer.
//
// The package imports only the standard library, so it can sit under the
// tool framework, which must not link Fantasy or the rest of craze.
package redact

import (
	"cmp"
	"errors"
	"io"
	"slices"
	"strings"
)

// Marker is what every redacted occurrence becomes. It holds no quote or
// backslash, so a key redacted inside a JSON string leaves valid JSON.
const Marker = "[craze:redacted-credential]"

// MarkerOverlaps reports whether the marker itself could print key back: key
// is a substring of Marker ("redacted", "credential"), contains it, starts
// with one of its non-empty suffixes, or ends with one of its non-empty
// prefixes. Such a key would reappear, verbatim, in the very text that
// redacted it — inside a marker, or across a marker and the text beside it.
//
// For any set of keys none of which overlaps the marker, String's output
// holds no key at all: an occurrence of a key in the output either lies in
// text copied from the input, where it would have been redacted, or crosses
// a marker, which this rules out. Redacting output a second time is
// therefore a no-op. The caller refuses an overlapping key, as it refuses a
// short one (modeltable).
func MarkerOverlaps(key string) bool {
	if key == "" {
		return false
	}
	if strings.Contains(Marker, key) || strings.Contains(key, Marker) {
		return true
	}
	for i := 1; i < len(Marker); i++ {
		if strings.HasPrefix(key, Marker[i:]) || strings.HasSuffix(key, Marker[:i]) {
			return true
		}
	}
	return false
}

// Replacer redacts a fixed set of keys. The zero value and a nil *Replacer
// redact nothing.
//
// Every byte that belongs to an occurrence of any key is covered, and each
// maximal run of overlapping occurrences becomes one Marker. So when two keys
// overlap in the text neither leaks a fragment, and a key contained in a
// longer one never splits it: the longer key wins. Occurrences that only
// touch (one ends where the next begins) are two runs and two markers.
type Replacer struct {
	// byFirst holds the keys by first byte, longest first, so matchAt tries
	// only the keys that can start at a position and stops at the longest.
	byFirst [256][]string
	keys    []string // deduplicated, longest first
	longest int
}

// New returns a Replacer over keys. Empty keys are ignored and duplicates
// collapse. It enforces no minimum length and accepts a key that overlaps
// the marker: a one-byte key would shred every text it touches and an
// overlapping one would be printed back (MarkerOverlaps), so the caller
// refuses both before they get here (modeltable).
func New(keys ...string) *Replacer {
	r := &Replacer{keys: slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == "" })}
	// Longest first, then bytewise, so the order and therefore every
	// decision is the same whatever order the keys came in.
	slices.SortFunc(r.keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	r.keys = slices.Compact(r.keys)
	for _, k := range r.keys {
		r.byFirst[k[0]] = append(r.byFirst[k[0]], k)
	}
	if len(r.keys) > 0 {
		r.longest = len(r.keys[0])
	}
	return r
}

// empty reports whether r redacts nothing, which makes every method a
// pass-through.
func (r *Replacer) empty() bool { return r == nil || len(r.keys) == 0 }

// String returns s with every key replaced. When s holds no key it returns s
// itself, without allocating.
func (r *Replacer) String(s string) string {
	if r.empty() || !slices.ContainsFunc(r.keys, func(k string) bool { return strings.Contains(s, k) }) {
		return s
	}
	out, _ := r.scan(nil, []byte(s), len(s), 0)
	return string(out)
}

// matchAt returns the length of the longest key that s starts with, or 0.
func (r *Replacer) matchAt(s []byte) int {
	for _, k := range r.byFirst[s[0]] {
		if len(s) >= len(k) && string(s[:len(k)]) == k {
			return len(k)
		}
	}
	return 0
}

// scan appends to dst the redacted form of src[:end] and returns it with how
// far past end the last run reaches (0 when it ends by end). run says how far
// into src a run begun before it reaches: those bytes are covered, and the
// Marker for them has already been written.
//
// Whether an occurrence starts at position i depends only on src[i:], so the
// caller must hand in every byte an occurrence starting before end could
// need: either src is the whole remaining text, or it holds at least
// longest-1 bytes past end. That is the whole of the streaming rule, and why
// Writer's output equals String's for every way of splitting the text.
func (r *Replacer) scan(dst, src []byte, end, run int) ([]byte, int) {
	emitted := min(run, end) // src[:emitted] is written or covered
	for i := range end {
		n := r.matchAt(src[i:])
		if n == 0 {
			continue
		}
		if i >= run {
			// A new run: settle the text before it, then its one marker.
			dst = append(dst, src[emitted:i]...)
			dst = append(dst, Marker...)
		}
		run = max(run, i+n)
		emitted = min(run, end)
	}
	dst = append(dst, src[emitted:end]...)
	return dst, max(run-end, 0)
}

// ErrClosed is Writer.Write's error after Close.
var ErrClosed = errors.New("redact: write after close")

// Writer redacts a stream on its way to an underlying writer. It holds back
// the last len(longest key)-1 bytes it has been given, because they could be
// the start of a key the next Write completes; Close writes them out. So a
// key split across any number of writes is still caught, and the bytes that
// reach the underlying writer are exactly String of everything written.
//
// There is no Flush: writing the held-back bytes before the stream ends
// could emit the first half of a key. A bash tool's progress snapshots
// therefore lag the command's output by at most that many bytes.
type Writer struct {
	r      *Replacer
	w      io.Writer
	buf    []byte // held back: not yet decided
	out    []byte // reused for each write's output
	run    int    // how far into buf the last run reaches
	err    error
	closed bool
}

// NewWriter returns a Writer that writes the redacted stream to w. A Writer
// over a Replacer with no keys holds nothing back and writes straight
// through.
func (r *Replacer) NewWriter(w io.Writer) *Writer {
	return &Writer{r: r, w: w}
}

// Write takes p and writes to the underlying writer whatever of the stream
// can now be decided. It reports len(p) once p is taken, although up to
// len(longest key)-1 bytes of it may still be held back. An error from the
// underlying writer is returned, with 0, and every later Write and Close
// returns it again.
func (w *Writer) Write(p []byte) (int, error) {
	switch {
	case w.err != nil:
		return 0, w.err
	case w.closed:
		return 0, ErrClosed
	case w.r.empty():
		if err := w.put(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	end := len(w.buf) - (w.r.longest - 1)
	if end <= 0 {
		return len(p), nil
	}
	if err := w.emit(end); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close writes out the held-back bytes, the stream having ended: a key's
// prefix left there is no key, and is written as it is. It does not close
// the underlying writer. A second Close returns what the first did.
func (w *Writer) Close() error {
	if w.closed || w.err != nil {
		return w.err
	}
	w.closed = true
	if len(w.buf) == 0 {
		return nil
	}
	return w.emit(len(w.buf))
}

// emit decides buf[:end], writes the result, and keeps buf[end:].
func (w *Writer) emit(end int) error {
	w.out, w.run = w.r.scan(w.out[:0], w.buf, end, w.run)
	w.buf = w.buf[:copy(w.buf, w.buf[end:])]
	if len(w.out) == 0 {
		return nil
	}
	return w.put(w.out)
}

// put writes b to the underlying writer and keeps its error for every later
// call.
func (w *Writer) put(b []byte) error {
	if _, err := w.w.Write(b); err != nil {
		w.err = err
		return err
	}
	return nil
}
