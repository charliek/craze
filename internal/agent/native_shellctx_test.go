package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/journal"
)

// shellOutput is a `!env` that printed the session's own key — the shell
// context block the TUI puts in front of the next message — followed by msg.
func shellOutput(msg string) string {
	return ShellContextBlock([]ShellResult{{Command: "env", Output: "PATH=/bin\nNATIVE_TEST_KEY=" + nativeCanary + "\n"}}) + msg
}

// TestNativeShellContextKeyIsRedactedAtAdmission is plan 037 N1. A `!`
// command's output travels in a shell context block in front of the user's
// next message, and native redacts the key values it knows in that block when
// it admits the text — before anything records it — on every way a person's
// text comes in: a prompt that runs, a prompt refused because another holds
// the slot, and an interjection into the running turn. So the key reaches
// neither the requests, nor the events, nor the transcript, nor any line of
// the complete journal; the block itself still goes out, with the variable's
// name and the marker where the value was.
func TestNativeShellContextKeyIsRedactedAtAdmission(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := startContent(t, f, Options{JournalDir: dir}, map[string]string{"a.txt": "alpha\n"}, nil)
	w := journalOf(t, s.log)
	h := newHeld(t)
	m := f.models["test/a"]
	m.push(
		h.step(nativeCallParts("c1", "read", nativeArgs(t, map[string]any{"filePath": "a.txt"})),
			finishParts(fantasy.FinishReasonToolCalls)),
		answer("done"),
	)

	out := startPrompt(s, shellOutput("summarise that"))
	await(t, h.reached, "the held tool step")
	if _, err := s.Prompt(context.Background(), shellOutput("and this")); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("a second prompt during the turn: %v, want ErrPromptInFlight", err)
	}
	if err := s.Interject(context.Background(), shellOutput("and hurry")); err != nil {
		t.Fatalf("Interject: %v", err)
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}

	calls := m.requests()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want two", len(calls))
	}
	// Sanity: the blocks really went out — the prompt's in both requests, the
	// interjection's in the second — so their cleanliness below is the
	// admission's doing, not a block that never arrived.
	var redacted int
	for _, call := range calls {
		for _, text := range userTexts(call) {
			if strings.Contains(text, "NATIVE_TEST_KEY="+redact.Marker) {
				redacted++
			}
		}
	}
	if redacted != 3 {
		t.Fatalf("%d user messages carry the redacted block, want 3: %+v", redacted, calls)
	}
	evs := drained(s)
	for what, v := range map[string]any{"the requests": calls, "the events": evs} {
		if leaks := nativeLeaks(v, nativeCanary); len(leaks) > 0 {
			t.Fatalf("the key leaked into %s at %v", what, leaks)
		}
	}
	for _, line := range transcriptOf(t, f) {
		if strings.Contains(line, nativeCanary) {
			t.Fatalf("the key leaked into the transcript: %q", line)
		}
	}
	attempts, lines := journaledAttemptsOf(t, s, w)
	assertOneJournal(t, dir, w, s.Incarnation())
	if written := journalBytes(t, w); strings.Contains(written, nativeCanary) {
		t.Fatalf("the key leaked into the journal:\n%s", written)
	}
	if len(lines) == 0 {
		t.Fatal("the journal is empty")
	}
	// The three attempts are on the record, each with the redacted block.
	kinds := map[string]int{}
	for _, a := range attempts {
		kinds[jsonString(a.prompt, "kind")]++
		if text := jsonString(a.prompt, "text"); !strings.Contains(text, "NATIVE_TEST_KEY="+redact.Marker) {
			t.Fatalf("a journaled attempt's text %q does not carry the redacted block", text)
		}
	}
	if kinds[string(journal.PromptKindPrompt)] != 2 || kinds[string(journal.PromptKindInterject)] != 1 {
		t.Fatalf("journaled attempts by kind %v, want two prompts (one refused) and an interjection", kinds)
	}
}

// TestNativeShellContextWithoutAKeyIsSentAsIs is N1's negative control: a
// block with nothing the redactor knows is admitted as the very string sent,
// and reaches the model byte for byte; so is text with no block, the key in
// the user's own words included, which native never redacts (nativePrompt).
func TestNativeShellContextWithoutAKeyIsSentAsIs(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("ok"))
	s := f.started(Options{})
	clean := ShellContextBlock([]ShellResult{{Command: "git status --short", Exit: 1, Output: " M a.go\n"}}) + "summarise that"
	for _, text := range []string{clean, "my own words " + nativeCanary, "plain"} {
		if got := s.AdmitPrompt(text); got != text {
			t.Fatalf("AdmitPrompt(%q) = %q, want it unchanged", text, got)
		}
	}
	if _, err := s.Prompt(context.Background(), clean); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if sent := firstUserText(t, f.models["test/a"].requests()[0]); sent != clean {
		t.Fatalf("the model was sent %q, want the text as it was typed: %q", sent, clean)
	}
}

// TestRedactShellContextTouchesOnlyTheBlock: the redactor runs over the
// block alone — after an attachment envelope when one leads — and never over
// the user's words after it, nor over text whose block is not well-formed
// (which is the user's own text, SplitShellContext's rule).
func TestRedactShellContextTouchesOnlyTheBlock(t *testing.T) {
	red := func() func(string) string {
		return func(s string) string { return strings.ReplaceAll(s, "KEY", "[redacted]") }
	}
	block := ShellContextBlock([]ShellResult{{Command: "env", Output: "A=KEY\n"}})
	for _, tc := range []struct{ in, want string }{
		{block + "the KEY stays", strings.Replace(block, "KEY", "[redacted]", 1) + "the KEY stays"},
		{"<shell_context>\nnot a block KEY\n</shell_context>\n\nKEY", "<shell_context>\nnot a block KEY\n</shell_context>\n\nKEY"},
		{"no block, KEY", "no block, KEY"},
	} {
		if got := redactShellContext(tc.in, red); got != tc.want {
			t.Errorf("redactShellContext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// No redactor yet (an unstarted session): the block is withheld whole.
	if got := redactShellContext(block+"x", func() func(string) string { return nil }); got != "x" {
		t.Errorf("with no redactor: %q, want the block withheld", got)
	}
}

// TestNativeUnstartedAdmissionWithholdsTheBlock: before Start has installed
// the harness, no redactor knows a key, so an unstarted session's admission
// withholds the shell context block rather than let a key through as though
// it had been looked for — and what an unstarted session journals (an
// interjection it refuses, a prompt it refuses) is the user's words alone.
func TestNativeUnstartedAdmissionWithholdsTheBlock(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := f.session(Options{JournalDir: dir})
	w := journalOf(t, s.log)
	if got := s.AdmitPrompt(shellOutput("summarise that")); got != "summarise that" {
		t.Fatalf("an unstarted AdmitPrompt = %q, want the block withheld", got)
	}
	if err := s.Interject(context.Background(), shellOutput("and hurry")); err == nil {
		t.Fatal("an unstarted session took an interjection")
	}
	if _, err := s.Prompt(context.Background(), shellOutput("and this")); err == nil {
		t.Fatal("an unstarted session took a prompt")
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 2 {
		t.Fatalf("%d journaled attempts, want the refused interjection and prompt", len(attempts))
	}
	if written := journalBytes(t, w); strings.Contains(written, nativeCanary) || strings.Contains(written, "<shell_context>") {
		t.Fatalf("an unstarted session journaled the block:\n%s", written)
	}
}

// TestNativeBeginBeforeStartIsRefusedForGood (astra r14): a direct caller's
// Begin made before Start has installed the harness is refused for good —
// its continuation fails with the unstarted session's refusal even when
// Start has succeeded by the time it runs — rather than sending the text
// with its shell context block withheld. Nothing is sent, the slot is not
// held, and a prompt begun after Start runs as ever.
func TestNativeBeginBeforeStartIsRefusedForGood(t *testing.T) {
	f := newNativeFixture(t)
	m := f.models["test/a"]
	m.push(answer("ok"))
	s := f.session(Options{})
	clean := ShellContextBlock([]ShellResult{{Command: "git status --short", Output: " M a.go\n"}}) + "summarise"

	run := s.Begin(clean)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := run(context.Background()); err == nil || err.Error() != "agent: session not started" {
		t.Fatalf("the continuation of a Begin made before Start: %v, want the unstarted refusal", err)
	}
	if n := len(m.requests()); n != 0 {
		t.Fatalf("%d requests sent for it, want none: %+v", n, m.requests())
	}
	if _, err := s.Prompt(context.Background(), clean); err != nil {
		t.Fatalf("a prompt begun after Start: %v", err)
	}
	if sent := firstUserText(t, m.requests()[0]); sent != clean {
		t.Fatalf("the prompt after Start sent %q, want %q", sent, clean)
	}
}
