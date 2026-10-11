package agent

import (
	"strings"
	"unicode"
)

// DisplayLine folds a string onto one line the way every client draws one:
// escape sequences and control characters go, and runs of whitespace collapse.
// It is the TUI's sanitizeLine (internal/tui/transcript.go) rule for rule —
// ansi.Strip, then every control dropped (newline, carriage return and tab
// first turned into a space), then strings.Fields joined by single spaces —
// with ansi.Strip transcribed (strip.go) because nothing below the clients may
// import the terminal library it comes from.
//
// It is distinct from this package's own sanitizeLine on purpose. That one is
// sanitizeText folded onto a line — ingestion's scrub, which also drops the
// zero-width and bidi runes and replaces invalid UTF-8 — and is what agent text
// has been through before it is stored; this one is what a client does to a
// stored string at the moment it draws it, and the slash catalog's builder
// (slash.go) applies it to every description because the TUI's menu always has
// (plan 043 P18). One implementation serves three callers: the TUI's
// sanitizeLine, the transcript model's (internal/transcript's notes.go, and
// through it engine.IndexTitleLine) and the builder.
//
// A string that is already one clean line — printable ASCII words separated by
// single spaces, which is every plugin name and nearly every description craze
// sees — comes back as it was, without allocating; that fast path returns
// exactly what the full path would (TestDisplayLineFastPathMatchesTheFullOne).
func DisplayLine(s string) string {
	if s == "" {
		return ""
	}
	if displayCleanLine(s) {
		return s
	}
	return strings.Join(strings.Fields(displayDropControls(stripANSI(s))), " ")
}

// displayCleanLine reports whether s is printable ASCII with single inner
// spaces and none at either end: a string DisplayLine's full path returns
// unchanged, since stripANSI prints every byte 0x20-0x7E in the ground state,
// nothing in it is a control character, and Fields/Join rebuild exactly the
// same words and spaces.
func displayCleanLine(s string) bool {
	if s[0] == ' ' || s[len(s)-1] == ' ' {
		return false
	}
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			if prevSpace {
				return false
			}
			prevSpace = true
		case c > ' ' && c < 0x7f:
			prevSpace = false
		default:
			return false
		}
	}
	return true
}

// displayDropControls is the TUI's dropControls: newlines, carriage returns and
// tabs become a space, and every other control code point goes —
// unicode.IsControl, the whole Cc category, so a C1 CSI in UTF-8 cannot pass as
// text.
func displayDropControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
