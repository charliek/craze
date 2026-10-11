package tui

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// TestTheEnginesIndexTitleIsTheTUIs is the drift guard for plan 030 C2's move
// of indexTitleLine into internal/engine (engine.IndexTitleLine), where every
// host — this TUI, and craze serve, which has no terminal library to fold a
// title with — writes a session-index title through it: the engine's fold is
// internal/transcript's transcription of ansi.Strip, so it is held here, byte
// for byte, against what this package's own sanitizeLine and cap make of the
// same string — the rule /rename normalised a title with before it asked
// (slash.go), and the one every row in the file was written with before the
// move — and so is engine.NormalizeTitle (plan 043 P9), the session's scrub
// and then the same rule, trimmed after the cut, which /rename and every
// create's title now go through. The
// inputs are the sanitize drift guard's (seedSanitizeStrings,
// randSanitizeString), plus titles long enough to be capped, in runes that
// are wider than a byte.
func TestTheEnginesIndexTitleIsTheTUIs(t *testing.T) {
	if titleRuneCap != engine.IndexTitleRunes {
		t.Fatalf("/rename caps at %d runes, the index at %d", titleRuneCap, engine.IndexTitleRunes)
	}
	check := func(t *testing.T, s string) {
		t.Helper()
		want := capRunes(sanitizeLine(s), titleRuneCap)
		if got := engine.IndexTitleLine(s); got != want {
			t.Fatalf("engine.IndexTitleLine(%q) = %q, the TUI's rule %q", s, got, want)
		}
		// engine.NormalizeTitle (plan 043 P9), which /rename now folds its
		// argument through and every create's title goes through: the
		// session's own ingestion scrub first (review C7: the title is what
		// the session will store), then this same rule, the ends trimmed
		// after the cut — and nothing else.
		norm := strings.TrimSpace(capRunes(sanitizeLine(agent.SanitizeText(s)), titleRuneCap))
		if got := engine.NormalizeTitle(s); got != norm {
			t.Fatalf("engine.NormalizeTitle(%q) = %q, the TUI's rule trimmed %q", s, got, norm)
		}
	}
	for _, s := range seedSanitizeStrings() {
		t.Run(fmt.Sprintf("seed/%q", s), func(t *testing.T) { check(t, s) })
	}
	for _, s := range []string{
		strings.Repeat("é", 200),
		strings.Repeat("a b ", 60),
		strings.Repeat("日本語", 50) + "\x1b[31m tail",
		"\x1b]0;" + strings.Repeat("x", 300) + "\x07after",
	} {
		t.Run(fmt.Sprintf("long/%d", len(s)), func(t *testing.T) { check(t, s) })
	}
	t.Run("random", func(t *testing.T) {
		r := rand.New(rand.NewPCG(0x030, 0x0c2)) // plan 030, C2
		for i := 0; i < 3000; i++ {
			// Several tokens' worth, so the cap is reached as often as not.
			var b strings.Builder
			for j := 0; j < 1+r.IntN(20); j++ {
				b.WriteString(randSanitizeString(r))
			}
			check(t, b.String())
		}
	})
}
