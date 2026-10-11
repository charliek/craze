package engine

import (
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

// titleCases is plan 043 §5 C4's normalization table: the titles every place
// a title enters craze — the hub's session.create, craze serve --title, craze
// new --title and --wait — must make the same thing of, because each runs
// NormalizeTitle. internal/hub's TestCreateTitleNormalization and
// internal/cli's TestTitleFlagsNormalizeAsTheHubDoes run these very inputs
// through their own paths and want these very outputs; "" is a title each
// refuses.
var titleCases = []struct {
	name, in, want string
}{
	{"plain", "Fix the login bug", "Fix the login bug"},
	{"empty", "", ""},
	{"whitespace only", " \t\n  ", ""},
	{"control characters only", "\x00\x07\x1b", ""},
	{"padded", "  padded title  ", "padded title"},
	{"inner runs collapse", "a  b\t\tc\n\nd", "a b c d"},
	{"control characters dropped", "bell\x07 and \x00nul", "bell and nul"},
	{"an escape sequence dropped", "\x1b[31mred\x1b[0m title", "red title"},
	{"an OSC title sequence dropped", "\x1b]0;evil\x07kept", "kept"},
	{"unicode kept", "日本語のタイトル — café ✓", "日本語のタイトル — café ✓"},
	{"120 runes kept", strings.Repeat("é", 120), strings.Repeat("é", 120)},
	{"121 runes cut to 120", strings.Repeat("日", 121), strings.Repeat("日", 120)},
	{"a cut after a space trimmed", strings.Repeat("a b ", 31), strings.TrimSpace(strings.Repeat("a b ", 30))},
	// What the session's ingestion scrub strips (review C7): the title is
	// what the session will store, so these go before anything is hashed.
	{"a zero-width space only", "\u200b", ""},
	{"a zero-width space inside", "A\u200bB", "AB"},
	{"a BOM and a word joiner", "\ufeffTi\u2060tle", "Title"},
	{"bidi overrides and isolates", "\u202eevil\u202c and \u2066isolate\u2069", "evil and isolate"},
	{"invalid UTF-8 replaced", "bad\xffbyte", "bad\ufffdbyte"},
	{"only stripped runes", "\u200b\u202e\ufeff\u2066\u2069\u2060", ""},
	{"a carriage return dropped", "a\rb", "ab"},
	{"a C1 CSI dropped with its sequence", "\u009b31mred", "red"},
	// ZWJ and ZWNJ are letters and emoji glue, which ingestion keeps.
	{"ZWJ and ZWNJ kept", "a\u200db\u200cc", "a\u200db\u200cc"},
}

// TestNormalizeTitle is the table, and the two properties the hub and craze
// serve lean on: a title normalized again is the same title (the hub's
// --title, normalized once more by the host it spawns), and the index row's
// fold changes nothing of it (the row says what the session does).
func TestNormalizeTitle(t *testing.T) {
	for _, tc := range titleCases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeTitle(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := NormalizeTitle(got); again != got {
				t.Fatalf("NormalizeTitle is not idempotent: %q then %q", got, again)
			}
			if row := IndexTitleLine(got); row != got {
				t.Fatalf("the index row folds %q to %q", got, row)
			}
		})
	}
	// The negative control for the trailing trim: the index's own fold, which
	// NormalizeTitle wraps, does leave the space a cut lands after — the case
	// above is not passing by accident.
	if raw := IndexTitleLine(strings.Repeat("a b ", 31)); !strings.HasSuffix(raw, " ") {
		t.Fatalf("IndexTitleLine left no space at the cut: %q", raw)
	}
	r := rand.New(rand.NewPCG(0x043, 0xc4))
	for i := 0; i < 4000; i++ {
		in := randTitle(r)
		got := NormalizeTitle(in)
		if NormalizeTitle(got) != got || IndexTitleLine(got) != got || agent.SanitizeText(got) != got ||
			utf8.RuneCountInString(got) > IndexTitleRunes || got != strings.TrimSpace(got) || !utf8.ValidString(got) {
			t.Fatalf("NormalizeTitle(%q) = %q: not a fixed point of itself, the session's scrub and the index's fold", in, got)
		}
	}
}

// randTitle is a random title from what a client might send and the session's
// scrub or the index's fold changes: escape and C1 sequences, controls,
// zero-width and bidi runes, ZWJ, invalid bytes, wide and combining runes.
func randTitle(r *rand.Rand) string {
	pieces := []string{"a", "b", " ", "  ", "\t", "\n", "\r", "\x00", "\x07", "\x7f", "\x1b", "\x1b[31m", "\x1b]0;t\x07",
		"\u009b", "\u009b1m", "\u009d0;x\u009c", "\u200b", "\u200c", "\u200d", "\u2060", "\ufeff", "\u202e", "\u2066",
		"\u2069", "\u200f", "\xff", "\xc3", "é", "日", "✓", "e\u0301", "👩\u200d💻", "\u2028", "\u00a0", "[", ";"}
	var b strings.Builder
	for j := 0; j < r.IntN(200); j++ {
		b.WriteString(pieces[r.IntN(len(pieces))])
	}
	return b.String()
}

// TestANormalizedTitleIsWhatTheSessionStores (review C5, C7): a title
// NormalizeTitle let through is exactly what a session stores of it — a new
// session's seed (agent.Options' Title) and SetTitle, live and native alike —
// and what the index row holds (IndexTitleLine), so the hub's hash, the index
// and the first state and roster row agree byte for byte; and the session's
// scrub, applied to it, is the identity. The negative control is the fold
// alone, without the scrub — the normalization this replaced: it keeps a
// zero-width space the session then drops, so the two disagree.
func TestANormalizedTitleIsWhatTheSessionStores(t *testing.T) {
	native := agent.NativeProvider()
	stored := func(t *testing.T, title string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for name, provider := range map[string]*agent.Provider{"live": nil, "native": &native} {
			seeded := agent.New(agent.Options{Provider: provider, Title: title, TitlePinned: true, Stderr: io.Discard})
			out[name+" seed"] = seeded.Snapshot().Title
			_ = seeded.Close()
			renamed := agent.New(agent.Options{Provider: provider, Stderr: io.Discard})
			if err := renamed.SetTitle("", title); err != nil {
				t.Fatalf("%s SetTitle: %v", name, err)
			}
			out[name+" rename"] = renamed.Snapshot().Title
			_ = renamed.Close()
		}
		return out
	}
	r := rand.New(rand.NewPCG(0x043, 0xc7))
	inputs := []string{}
	for _, tc := range titleCases {
		inputs = append(inputs, tc.in)
	}
	for i := 0; i < 200; i++ {
		inputs = append(inputs, randTitle(r))
	}
	for _, in := range inputs {
		n := NormalizeTitle(in)
		if n == "" {
			continue
		}
		if agent.SanitizeText(n) != n {
			t.Fatalf("the session's scrub changes the normalized %q to %q", n, agent.SanitizeText(n))
		}
		if row := IndexTitleLine(n); row != n {
			t.Fatalf("the index row of the normalized %q is %q", n, row)
		}
		for path, got := range stored(t, n) {
			if got != n {
				t.Fatalf("the %s stores %q of the normalized %q (from %q)", path, got, n, in)
			}
		}
	}
	// The negative control: the fold without the scrub.
	foldOnly := strings.TrimSpace(IndexTitleLine("A\u200bB"))
	if got := stored(t, foldOnly)["live seed"]; got == foldOnly {
		t.Fatalf("the session stores the fold-only %q as it is: the control shows nothing", foldOnly)
	}
}
