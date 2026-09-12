package agent

import (
	"strings"
	"unicode/utf8"
)

// zeroWidth are the invisible runes craze strips, and the whole of that list:
// a zero-width space, a word joiner and a BOM. Cursor pads its "Fast" label
// with two U+200B, which would otherwise widen every label craze measures.
//
// ZWNJ (U+200C) and ZWJ (U+200D) are deliberately not here. They are letters
// in Persian and the glue that holds an emoji sequence together, so dropping
// them would corrupt text rather than tidy it.
const zeroWidth = "\u200b\u2060\ufeff"

func isZeroWidth(r rune) bool {
	return r == '\u200b' || r == '\u2060' || r == '\ufeff'
}

// sanitizeText scrubs a string that came from the agent before it reaches the
// UI or the JSON event stream: C0 controls except \n and \t (plus DEL) are
// dropped, CSI/OSC/DCS/SOS/PM/APC escape sequences are removed whole, the
// zero-width runes above are stripped, and invalid UTF-8 is replaced by
// U+FFFD. Tool ids are exempt: they are map keys only and are never rendered.
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
			case isZeroWidth(r):
				// dropped: it takes no cells but does widen every len()
			default:
				b.WriteString(s[i : i+size])
			}
			i += size
		}
	}
	return b.String()
}

// clean reports whether s needs no scrubbing at all, so the common case does
// not allocate.
//
// The non-ASCII answer is not "valid UTF-8 is fine": a zero-width rune is
// perfectly valid and still has to go, so a string with one falls through to
// the slow loop that drops it.
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
	return utf8.ValidString(s) && !strings.ContainsAny(s, zeroWidth)
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
		i++
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		return i
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
