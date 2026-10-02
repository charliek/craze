package opencode

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
)

// The cuts grep and read make in a line they read are redacted first, with
// the session's keys as they are at the cut (tool.Env.CurrentRedactor; plan
// 033 C14r2, review r13 a): the dispatcher redacts the result again when the
// call returns, knowing every key by then, but a key a cut halved is no
// longer whole, and its first half is matched by no redactor.

// planToken is a dummy ChatGPT plan token: over modeltable's 8-byte floor,
// sharing nothing with the marker, and spelled so that no path or word in a
// result can hold its first bytes by chance.
const planToken = "zq-plan-token-cut-0021"

// learnAtCut makes the n-th cut a call makes (from 1) learn token just
// before it: beforeCut runs after the line was read and before it is cut,
// and here teaches the fixture's dispatcher the token as Session.AddSecrets
// teaches a session's (SetRedactor). The call's own Env.Redactor, fixed when
// it began, never knows it. A test that uses it cannot run in parallel.
func learnAtCut(t *testing.T, f *fixture, n int, token string) {
	t.Helper()
	cuts := 0
	beforeCut = func() {
		if cuts++; cuts == n {
			f.d.SetRedactor(redact.New(keyA, token))
		}
	}
	t.Cleanup(func() { beforeCut = nil })
}

// TestCutsRedactWithTheKeysOfTheCut (r13 a): grep and read each read two
// lines that hold the token across the 2000-rune cut, and the session learns
// the token while the call runs. Learned before the first cut, neither line
// shows any of it. Learned between the two cuts — the control — the first
// line keeps the token's first ten bytes and the second shows none: the cut
// is what the redaction must come before, and the dispatcher's redaction of
// the result, which knows the token by then, cannot take the half back.
func TestCutsRedactWithTheKeysOfTheCut(t *testing.T) {
	requireRG(t)
	line := strings.Repeat("x", 1990) + planToken + strings.Repeat("y", 30)
	clean := strings.Repeat("x", 1990) + redact.Marker[:10]
	half := strings.Repeat("x", 1990) + planToken[:10]
	for _, tc := range []struct {
		tool       string
		in         map[string]any
		head, tail string // a line's row in the result, around its text
	}{
		{"grep", map[string]any{"pattern": "x{1990}"}, "  Line %d: ", "..."},
		{"read", map[string]any{"filePath": "log.txt"}, "%d: ", maxLineSuffix},
	} {
		row := func(n int, text string) string {
			return strings.Replace(tc.head, "%d", string(rune('0'+n)), 1) + text + tc.tail
		}
		t.Run(tc.tool+", learned before the first cut", func(t *testing.T) {
			f := newFixture(t)
			put(t, f.path("log.txt"), line+"\n"+line+"\n")
			learnAtCut(t, f, 1, planToken)
			text := ok(t, callOK(t, f, tc.tool, tc.in))
			if strings.Contains(text, "zq-plan") || !strings.Contains(text, row(1, clean)) || !strings.Contains(text, row(2, clean)) {
				t.Fatalf("a token learned before the cut reached the result:\n%s", text)
			}
		})
		t.Run(tc.tool+", learned between the cuts", func(t *testing.T) {
			f := newFixture(t)
			put(t, f.path("log.txt"), line+"\n"+line+"\n")
			learnAtCut(t, f, 2, planToken)
			text := ok(t, callOK(t, f, tc.tool, tc.in))
			if !strings.Contains(text, row(1, half)) || !strings.Contains(text, row(2, clean)) {
				t.Fatalf("control: the line cut before the token was learned does not keep its half:\n%s", text)
			}
		})
	}
}

// TestReadRedactsTheLinesItCuts: read redacts a line before it cuts it, as
// grep does — a key the 2000-rune cut would halve leaves no half behind —
// and a line longer than the part of it read holds (keepBytes) is redacted as
// the start of a longer text: a key that part's end halves, here after 1999
// four-byte runes, leaves not even the one byte the cut would keep. A line the
// redaction brings within the limit is shown whole. The control is a session
// with no redactor, whose read shows both halves.
func TestReadRedactsTheLinesItCuts(t *testing.T) {
	t.Parallel()
	emoji := strings.Repeat("\U0001F600", 1999)     // 7996 bytes: the key starts 4 bytes before keepBytes
	longKey := "sk-long-" + strings.Repeat("q", 92) // 100 bytes, the marker 27
	content := strings.Repeat("x", 1990) + keyA + strings.Repeat("y", 30) + "\n" +
		emoji + keyA + strings.Repeat("z", 100) + "\n" +
		strings.Repeat("w", 1950) + longKey + "\n" // 2050 runes, 1977 once redacted
	run := func(red *redact.Replacer) string {
		f := newFixtureWith(t, red)
		put(t, f.path("long.txt"), content)
		return ok(t, callOK(t, f, "read", map[string]any{"filePath": "long.txt"}))
	}
	text := run(redact.New(keyA, longKey))
	for _, want := range []string{
		"1: " + strings.Repeat("x", 1990) + redact.Marker[:10] + maxLineSuffix + "\n",
		"2: " + emoji + maxLineSuffix + "\n",
		"3: " + strings.Repeat("w", 1950) + redact.Marker + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("read did not redact a line before cutting it; want %.40q… in:\n%.300s", want, text)
		}
	}
	if strings.Contains(text, "sk-") {
		t.Fatalf("a piece of the key reached the result: %.300s", text[strings.Index(text, "sk-")-20:])
	}
	text = run(nil)
	for _, want := range []string{
		"1: " + strings.Repeat("x", 1990) + keyA[:10] + maxLineSuffix,
		"2: " + emoji + keyA[:1] + maxLineSuffix,
		"3: " + strings.Repeat("w", 1950) + longKey[:50] + maxLineSuffix,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("control: with no redactor, the cut half is not in the result; want %.40q…", want)
		}
	}
}
