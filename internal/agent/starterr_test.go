package agent

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/journal"
)

// A failed start in the agent's own words (plan 035 C7, SF-125).

// TestStderrTailLineFoldsTheAgentsLastLines: the raw end of an agent's
// stderr, folded onto one line — each shape the plan names, the 512-byte
// budget taken from the end, and a last line longer than it cut on a rune
// boundary.
func TestStderrTailLineFoldsTheAgentsLastLines(t *testing.T) {
	line := func(c byte, n int) string { return strings.Repeat(string(c), n) }
	hundreds := func(n, width int) (string, []string) {
		var raw strings.Builder
		var each []string
		for i := range n {
			l := line('a'+byte(i), width)
			each = append(each, l)
			raw.WriteString(l + "\n")
		}
		return raw.String(), each
	}
	five, fiveEach := hundreds(6, 100) // the last five: 5×100 + 4×3 = 512 bytes, exactly the cap
	over, overEach := hundreds(6, 101) // the last five would be 517: four fit
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"nothing", "", ""},
		{"one line", "Error: one\n", "Error: one"},
		{"two lines", "Error: KEYCHAIN LOCKED\nRun unlock and retry.\n", "Error: KEYCHAIN LOCKED / Run unlock and retry."},
		{"CRLF", "first\r\nsecond\r\n", "first / second"},
		{"ANSI", "\x1b[31mred\x1b[0m alert\n", "red alert"},
		{"a fragment with no newline", "done\npartial", "done / partial"},
		{"blank lines", "\n\n   \nonly this\n\n\t\n", "only this"},
		{"nothing but escapes", "\x1b[2J\x1b[H\n", ""},
		{"invalid UTF-8", "bad \xff byte\n", "bad � byte"},
		{"whitespace inside a line", "a\t b  c\n", "a b c"},
		{"exactly the cap", five, strings.Join(fiveEach[1:], " / ")},
		{"one byte a line over", over, strings.Join(overEach[2:], " / ")},
		{"a long line stops the choice", "early\n" + line('z', 600) + "\nlate\n", "late"},
		{"one line over the cap", line('q', 600) + "\n", line('q', 512)},
		// 512 is two bytes into the 171st three-byte rune: 170 whole ones.
		{"a cut inside a rune", strings.Repeat("日", 200) + "\n", strings.Repeat("日", 170)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stderrTailLine([]byte(tc.raw))
			if got != tc.want {
				t.Fatalf("stderrTailLine = %d bytes %q, want %d bytes %q", len(got), got, len(tc.want), tc.want)
			}
			if len(got) > startErrTailMax || strings.ContainsAny(got, "\n\r") {
				t.Fatalf("stderrTailLine = %d bytes, %q: over the cap or not one line", len(got), got)
			}
		})
	}
}

// TestWithAgentWordsWrapsTheError: the folded tail after ": ", then the hint
// as it is; a tail the error already says is not said twice; the error is
// wrapped, never replaced.
func TestWithAgentWordsWrapsTheError(t *testing.T) {
	exit := &acp.ExitError{}
	base := errors.New("acp: agent exited: exit status 1")
	for _, tc := range []struct {
		name, tail, hint string
		err              error
		want             string
	}{
		{"nothing to add", "", "", base, "acp: agent exited: exit status 1"},
		{"a tail", "Error: one", "", base, "acp: agent exited: exit status 1: Error: one"},
		{"a tail and a hint", "Error: one", "; a hint", base, "acp: agent exited: exit status 1: Error: one; a hint"},
		{"a hint alone", "", "; a hint", base, "acp: agent exited: exit status 1; a hint"},
		{"a tail already said", "exit status 1", "", base, "acp: agent exited: exit status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := withAgentWords(tc.err, tc.tail, tc.hint)
			if wrapped.Error() != tc.want {
				t.Fatalf("withAgentWords = %q, want %q", wrapped, tc.want)
			}
			if !errors.Is(wrapped, base) {
				t.Fatal("the error is replaced, not wrapped")
			}
		})
	}
	var got *acp.ExitError
	if !errors.As(withAgentWords(exit, "said", "; hint"), &got) || got != exit {
		t.Fatal("errors.As no longer finds the exit through the words")
	}
}

// TestAFailedStartSaysTheAgentsLastLines: the fake agent's exit-two-lines
// dies at its start with two lines on its stderr. A session that opted in
// (StartErrAgentStderr) fails its start with those lines after craze's own
// error, then its hint, on one line, the exit still in the chain — and
// journals that same text, because the words are added before the note. One
// that did not opt in fails it with craze's error alone, byte for byte as
// before, whatever hint it was given (the V2 guard). A start that fails with
// the agent alive (authfail) is never decorated, its stderr notwithstanding.
func TestAFailedStartSaysTheAgentsLastLines(t *testing.T) {
	const (
		exited = "acp: agent exited: exit status 1"
		said   = exited + ": Error: KEYCHAIN LOCKED / Run unlock and retry."
		hint   = "; a hint the host built"
		alive  = "json-rpc error -32000: authentication failed (run `agent login`)"
	)
	for _, tc := range []struct {
		name, script, stderr string
		optIn                bool
		hint                 string
		want                 string
		exit                 bool
	}{
		{name: "opted in", script: "exit-two-lines", optIn: true, want: said, exit: true},
		{name: "opted in, with a hint", script: "exit-two-lines", optIn: true, hint: hint, want: said + hint, exit: true},
		{name: "not opted in", script: "exit-two-lines", want: exited, exit: true},
		{name: "not opted in, a hint given", script: "exit-two-lines", hint: hint, want: exited, exit: true},
		{name: "opted in, the agent alive", script: "authfail", stderr: "fake: SAID AT START", optIn: true, hint: hint, want: alive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRAZE_FAKE_STDERR", tc.stderr)
			dir := filepath.Join(t.TempDir(), "journal")
			s := newTestSession(t, Options{
				Binary:              fakeAgentPath(t),
				ExtraArgs:           []string{"-script=" + tc.script},
				Workspace:           t.TempDir(),
				Force:               true,
				Stderr:              io.Discard,
				JournalDir:          dir,
				StartErrAgentStderr: tc.optIn,
				StartErrExitHint:    tc.hint,
			})
			w := journalOf(t, s.log)
			err := s.Start(t.Context())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Start = %v, want %q", err, tc.want)
			}
			var exit *acp.ExitError
			if errors.As(err, &exit) != tc.exit {
				t.Fatalf("errors.As(%v, *acp.ExitError) = %v, want %v", err, !tc.exit, tc.exit)
			}
			closeJournaled(t, s, w)
			failures := diags(fileLines(t, w), journal.DiagStartFailed)
			if len(failures) != 1 || failures[0]["errMessage"] != tc.want {
				t.Fatalf("the journal's start_failed: %v, want errMessage %q", failures, tc.want)
			}
		})
	}
}
