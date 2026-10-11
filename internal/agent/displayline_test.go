package agent

import (
	"strings"
	"testing"
)

// TestDisplayLineFastPathMatchesTheFullOne holds DisplayLine's clean-line fast
// path to the full path it skips: every string displayCleanLine admits comes
// back exactly as stripANSI, displayDropControls and Fields/Join would have
// left it, and every string it refuses ("a  b", " a", a tab, DEL) goes the long
// way. It moved here from internal/transcript with the function (plan 043 C1);
// whether the full path is the TUI's x/ansi answer is
// internal/tui's TestDisplayLineMatchesTheTUIsAnsiStrip.
func TestDisplayLineFastPathMatchesTheFullOne(t *testing.T) {
	full := func(s string) string {
		if s == "" {
			return ""
		}
		return strings.Join(strings.Fields(displayDropControls(stripANSI(s))), " ")
	}
	for _, s := range []string{"a", "p:c", "a b c", "a  b", " a", "a ", "tab\tx", "é", "~!@#$%^&*()_+{}|:\"<>?`-=[]\\;',./", "\x7f"} {
		if got, want := DisplayLine(s), full(s); got != want {
			t.Fatalf("DisplayLine(%q) = %q, the full path %q", s, got, want)
		}
	}
}
