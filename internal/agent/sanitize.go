package agent

import (
	"strings"
	"unicode/utf8"
)

// isZeroWidth reports whether r is one of the invisible runes craze strips,
// and that list is the whole of it: a zero-width space, a word joiner and a
// BOM. Cursor pads its "Fast" label with two U+200B, which would otherwise
// widen every label craze measures.
//
// ZWNJ (U+200C) and ZWJ (U+200D) are deliberately not here. They are letters
// in Persian and the glue that holds an emoji sequence together, so dropping
// them would corrupt text rather than tidy it.
func isZeroWidth(r rune) bool {
	return r == 0x200b || r == 0x2060 || r == 0xfeff
}

// isC1 reports whether r is one of the C1 controls, U+0080–U+009F. Each is a
// single code point, two bytes in UTF-8, and a terminal reading C1 takes
// several of them as the 8-bit form of the sequences ESC spells in two bytes:
// U+009B is CSI, U+009D is OSC, U+0090 is DCS, U+0098 SOS, U+009E PM, U+009F
// APC, U+009C the string terminator. So `\u009b31m` colours a line and
// `\u009d0;x\u009c` retitles a window exactly as their ESC forms do, and
// dropping the introducer alone would leave its parameters as text
// (plan 023 X14).
func isC1(r rune) bool { return r >= 0x80 && r <= 0x9f }

// isBidi reports whether r is one of Unicode's bidirectional formatting
// controls: the embeddings and overrides (U+202A–U+202E), the isolates
// (U+2066–U+2069) and the two marks (U+200E, U+200F). They take no cells and
// reorder what is drawn after them, so a label, a path or a todo carrying one
// can be read as something other than what it says (plan 023 X14).
func isBidi(r rune) bool {
	return r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// isDropped reports whether r is a rune sanitizeText removes. A C1 control
// takes whatever it introduces with it (skipC1Escape); the rest go on their
// own.
func isDropped(r rune) bool { return isZeroWidth(r) || isC1(r) || isBidi(r) }

// sanitizeText scrubs a string that came from the agent before it reaches the
// UI or the JSON event stream: C0 controls except \n and \t (plus DEL) are
// dropped, CSI/OSC/DCS/SOS/PM/APC escape sequences are removed whole in both
// their 7-bit (ESC) and 8-bit (C1) spellings, the C1 controls, the zero-width
// runes and the bidi formatting controls above are stripped, and invalid
// UTF-8 is replaced by U+FFFD. Tool ids are exempt: they are map keys only
// and are never rendered.
func sanitizeText(s string) string {
	if s == "" || clean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscape(s, i)
		case c < 0x20:
			if c == '\n' || c == '\t' {
				b.WriteByte(c)
			}
			i++
		case c == 0x7f:
			i++
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				b.WriteRune(utf8.RuneError)
				i += size
			case isC1(r):
				// The control and, for the six that introduce one, its whole
				// sequence.
				i = skipC1Escape(s, i, r)
			case isZeroWidth(r) || isBidi(r):
				// dropped: neither takes a cell, and both change how what is
				// around them measures or reads
				i += size
			default:
				b.WriteString(s[i : i+size])
				i += size
			}
		}
	}
	return b.String()
}

// sanitizeLine is sanitizeText folded onto one line: every run of whitespace,
// newlines and tabs included, collapses to a single space and the ends are
// trimmed. It is what a value has to survive before it can go inside a tag
// craze writes — an argument or a path carrying a newline would otherwise put
// the rest of that tag on a line of its own.
func sanitizeLine(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(sanitizeText(s)), " ")
}

// clean reports whether s needs no scrubbing at all, so the common case does
// not allocate.
//
// The non-ASCII answer is not "valid UTF-8 is fine": a zero-width, bidi or C1
// rune is perfectly valid and still has to go, so a string with one falls
// through to the slow loop that drops it. The fast path is part of the
// sanitiser, not an optimisation beside it — a rune it fails to notice is a
// rune that is never removed.
func clean(s string) bool {
	ascii := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x7f || (c < 0x20 && c != '\n' && c != '\t') {
			return false
		}
		if c >= utf8.RuneSelf {
			ascii = false
		}
	}
	if ascii {
		return true
	}
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if isDropped(r) {
			return false
		}
	}
	return true
}

// skipEscape returns the index just past the escape sequence starting at i.
func skipEscape(s string, i int) int {
	i++ // ESC
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[': // CSI: parameters then a final byte in 0x40..0x7e
		i++
		for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
			i++
		}
		if i < len(s) {
			i++
		}
		return i
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: run to BEL or ST
		return skipToST(s, i+1)
	default: // ESC + optional intermediates (0x20..0x2f) + one final byte
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) {
			i++
		}
		return i
	}
}

// skipC1Escape returns the index just past the C1 control r at i — two bytes
// in UTF-8 — and the sequence it introduces, when it introduces one. It is
// skipEscape's 8-bit twin: the same three shapes, with one code point in
// place of ESC and the two-byte ST (U+009C) accepted as a terminator beside
// the ESC \ form, because a sequence opened in the 8-bit spelling is closed
// in it too.
func skipC1Escape(s string, i int, r rune) int {
	i += 2 // the control itself: every C1 rune is two bytes in UTF-8
	switch r {
	case 0x9b: // CSI: parameters then a final byte in 0x40..0x7e
		for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
			i++
		}
		if i < len(s) {
			i++
		}
		return i
	case 0x90, 0x98, 0x9d, 0x9e, 0x9f: // DCS, SOS, OSC, PM, APC: run to BEL or ST
		return skipToST(s, i)
	default:
		// Every other C1 control, the string terminator included, says nothing
		// on its own and is simply gone.
		return i
	}
}

// skipToST returns the index just past the string-sequence body starting at
// i: it runs to BEL, to ESC \ or to U+009C, whichever comes first, and to the
// end of s when the sequence was never closed.
func skipToST(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
			return i + 2
		case s[i] == 0xc2 && i+1 < len(s) && s[i+1] == 0x9c: // U+009C, ST
			return i + 2
		}
		i++
	}
	return i
}
