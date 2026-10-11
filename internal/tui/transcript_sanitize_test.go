package tui

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	// Aliased: this package already has its own type named transcript, which
	// would collide with the import's package identifier in file scope.
	trmodel "github.com/charliek/craze/internal/transcript"
)

// ansiSanitizeLine is the rule the TUI's sanitizeLine drew by before plan 043
// C1 made it agent.DisplayLine: x/ansi's own ansi.Strip, then this package's
// dropControls, then Fields joined by single spaces. It is the reference the
// two drift guards below hold the transcription (agent/strip.go) to, because
// after C1 nothing in the TUI calls ansi.Strip for a line any more — comparing
// sanitizeLine with the transcript model's would be comparing
// agent.DisplayLine with itself.
func ansiSanitizeLine(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(dropControls(ansi.Strip(s))), " ")
}

// TestDisplayLineMatchesTheTUIsAnsiStrip is the drift guard for the one
// sanitizer (plan 024 r1 X12, plan 043 C1): agent.DisplayLine — the TUI's
// sanitizeLine, the transcript model's and the slash catalog builder's —
// gives x/ansi's answer on every seed case and on the same seeded random
// strings as below. So a bump of x/ansi that changes ansi.Strip, or an edit of
// the transcription that changes it, fails the gate; it cannot silently move
// the TUI's lines and the host's catalog apart from what they were.
func TestDisplayLineMatchesTheTUIsAnsiStrip(t *testing.T) {
	check := func(t *testing.T, s string) {
		t.Helper()
		if got, want := sanitizeLine(s), ansiSanitizeLine(s); got != want {
			t.Fatalf("sanitizeLine(%q) = %q, x/ansi's rule %q", s, got, want)
		}
	}
	for _, s := range seedSanitizeStrings() {
		check(t, s)
	}
	r := rand.New(rand.NewPCG(0x043, 0x0c1)) // plan 043, C1
	for i := 0; i < 3000; i++ {
		check(t, randSanitizeString(r))
	}
}

// TestTranscriptModelsCommandLineMatchesTheTUIs is a drift guard for r1's
// execution amendment X12: the transcription of ansi.Strip (agent/strip.go
// since plan 043 C1, internal/transcript/strip.go before) lets the model's
// command-line note match the TUI's without importing a terminal library.
//
// It folds the same EventCommand into a transcript.New(...) model — the
// package's own commandLine, built from its sanitizeLine — and holds the
// resulting note's text against the rule the TUI's own addCommandLine drew by
// before the shared model took the row over in C5c: commandLineMark +
// the line, plus " (" + the kind + ")" when the kind sanitises to something,
// and no note at all when the name sanitises to "" — each line folded by
// x/ansi's rule (ansiSanitizeLine), never by the function under test. So a
// bump of x/ansi that changes the TUI's old answer — and not the transcribed
// copy's — fails the gate.
func TestTranscriptModelsCommandLineMatchesTheTUIs(t *testing.T) {
	check := func(t *testing.T, q, k string) {
		t.Helper()

		want := ""
		if name := ansiSanitizeLine(q); name != "" {
			want = commandLineMark + name
			if kind := ansiSanitizeLine(k); kind != "" {
				want += " (" + kind + ")"
			}
		}

		m := trmodel.New(trmodel.Options{})
		m.Fold(agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{
			PluginCommand: agent.PluginCommand{Qualified: q, Kind: k},
		}})

		var (
			got   string
			found bool
		)
		for _, e := range m.Main.Entries() {
			if e.Kind == trmodel.KindNote {
				got, found = e.Text, true
			}
		}

		if want == "" {
			if found {
				t.Fatalf("q=%q k=%q: the TUI draws no note, the model drew %q", q, k, got)
			}
			return
		}
		if !found {
			t.Fatalf("q=%q k=%q: the TUI draws %q, the model drew none", q, k, want)
		}
		if got != want {
			t.Fatalf("q=%q k=%q: the model's note %q, want the TUI's %q", q, k, got, want)
		}
	}

	// seedQualified is "p:c" fixed, so a seed case under test as the kind is
	// read against a name that always sanitises to something.
	const fixedQ = "p:c"
	// seedKind is "command" fixed, so a seed case under test as the name is
	// read with a kind that always sanitises to something.
	const fixedK = "command"

	for _, s := range seedSanitizeStrings() {
		t.Run(fmt.Sprintf("qualified/%q", s), func(t *testing.T) { check(t, s, fixedK) })
		t.Run(fmt.Sprintf("kind/%q", s), func(t *testing.T) { check(t, fixedQ, s) })
	}

	t.Run("random", func(t *testing.T) {
		r := rand.New(rand.NewPCG(0x024, 0x012c)) // plan 024, execution amendment X12
		const n = 3000
		for i := 0; i < n; i++ {
			q, k := randSanitizeString(r), randSanitizeString(r)
			check(t, q, k)
		}
	})
}

// seedSanitizeStrings is the deterministic edge-case set for the drift guards
// above: every C0 and C1 control byte, CSI/OSC/DCS/APC sequences both
// terminated (ST and BEL alike) and left open, the C1 8-bit introducers for
// each of those, invalid UTF-8, multi-byte runes, and tabs/newlines/runs of
// spaces — every shape stripANSI's transition table branches on.
func seedSanitizeStrings() []string {
	var out []string

	// Every C0 control byte (0x00-0x1F) and DEL (0x7F), alone and framed by
	// plain text.
	for b := 0; b <= 0x1F; b++ {
		out = append(out, string([]byte{byte(b)}), "a"+string([]byte{byte(b)})+"b")
	}
	out = append(out, "\x7f", "a\x7fb")

	// Every C1 control byte (0x80-0x9F) as a raw byte, the way a terminal
	// that reads C1 outside UTF-8 sends them (dropControls's comment).
	for b := 0x80; b <= 0x9F; b++ {
		out = append(out, string([]byte{byte(b)}), "a"+string([]byte{byte(b)})+"b")
	}

	seeds := []string{
		// CSI: terminated, unterminated (no final byte), with intermediates.
		"a\x1b[31mred\x1b[0mb",
		"a\x1b[31",
		"a\x1b[?25h\x1b[2Kb",
		// OSC: ST (both 2-byte ESC \\ and the 8-bit 0x9C), BEL, unterminated.
		"a\x1b]0;title\x1b\\b",
		"a\x1b]0;title\x9cb",
		"a\x1b]0;title\x07b",
		"a\x1b]0;title",
		// DCS: terminated, unterminated.
		"a\x1bPqdata\x1b\\b",
		"a\x1bPqdata",
		// APC: terminated, unterminated.
		"a\x1b_data\x1b\\b",
		"a\x1b_data",
		// SOS/PM: terminated, unterminated.
		"a\x1bXdata\x1b\\b",
		"a\x1b^data\x1b\\b",
		"a\x1bXdata",
		// The C1 8-bit introducers themselves: CSI 0x9B, DCS 0x90, OSC 0x9D,
		// SOS 0x98, PM 0x9E, APC 0x9F, each closed with the 8-bit ST 0x9C.
		"a\x9b31mred\x9c b",
		"a\x90qdata\x9cb",
		"a\x9d0;title\x9cb",
		"a\x98data\x9cb",
		"a\x9edata\x9cb",
		"a\x9fdata\x9cb",
		// Invalid UTF-8: a lead byte with no (or a bad) continuation, a bare
		// continuation byte, an overlong/surrogate encoding.
		"a\xc3b",
		"a\xc3\x28b",
		"a\xffb",
		"a\xfeb",
		"a\x80b",
		"a\xed\xa0\x80b",
		"a\xf0\x28\x8c\x28b",
		// Multi-byte runes: accented Latin, CJK, an emoji outside the BMP.
		"héllo wörld",
		"日本語のテスト",
		"café \U0001F600 done",
		// Tabs, newlines, carriage returns and runs of whitespace.
		"a\tb\nc\rd",
		"  leading and trailing  ",
		"a    b\t\tc",
		"\t\n\r",
		// Already clean, so the fast path is exercised too.
		"",
		" ",
		"p:c",
		"a b c",
	}
	out = append(out, seeds...)
	return out
}

// sanitizeAlphabet is a set of tokens randSanitizeString draws from, weighted
// towards the escape-related bytes stripANSI's transition table branches on:
// most of the slots below are C0/C1 controls, introducers and terminators: a
// small remainder is ordinary text and multi-byte runes, so the generated
// strings are mostly escape noise with plain characters mixed through it.
var sanitizeAlphabet = func() []string {
	var a []string
	escapes := []string{
		"\x1b[", "\x1b]", "\x1bP", "\x1b_", "\x1bX", "\x1b^", "\x1b\\", "\x1b",
		"\x07", "\x9b", "\x9c", "\x9d", "\x90", "\x9e", "\x9f", "\x98",
		";", ":", "?", "<", "=", ">", "0", "1", "9", "31", "38;5;9",
		"m", "H", "J", "K", "h", "l", "q", "title=x",
	}
	for i := 0; i < 6; i++ { // repeated to bias selection towards these
		a = append(a, escapes...)
	}
	for b := 0; b <= 0x1F; b++ {
		a = append(a, string([]byte{byte(b)}))
	}
	for b := 0x80; b <= 0x9F; b++ {
		a = append(a, string([]byte{byte(b)}))
	}
	a = append(a,
		"a", "bb", "word", " ", "\t", "\n", "\r",
		"é", "日", "本", "😀", "\xff", "\xfe", "\xc3\x28", "\xed\xa0\x80", "\xf0\x28\x8c\x28",
	)
	return a
}()

// randSanitizeString builds one string from sanitizeAlphabet's tokens, r's
// draws reproducible from the fixed seed the caller built r with.
func randSanitizeString(r *rand.Rand) string {
	n := r.IntN(14)
	var b []byte
	for i := 0; i < n; i++ {
		b = append(b, sanitizeAlphabet[r.IntN(len(sanitizeAlphabet))]...)
	}
	return string(b)
}
