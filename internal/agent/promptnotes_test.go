package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/journal"
)

// The prompt notes (plan 020 §3.5, commit C5b): every way a prompt attempt can
// end leaves a prompt line and exactly one prompt_end, because the notes wrap
// Begin's continuation rather than the prompt's body — a claim that was
// refused, a prompt cancelled in the catalog wait and one withdrawn before the
// wire all return from the continuation without reaching the body at all.
// These tests walk §3.5's path table row by row, on the fake agent and on the
// scripted native model, and read the class back out of the file.

// journaledScript is startScriptOpts with a journal of its own, and the writer
// whose file the test reads back.
func journaledScript(t *testing.T, script string, opts Options) (*session, *journal.Writer) {
	t.Helper()
	opts.JournalDir = filepath.Join(t.TempDir(), "journal")
	s := startScriptOpts(t, script, opts)
	return s, journalOf(t, s.log)
}

// journaledGrok is journaledScript on the grok provider, whose keys are
// cleared the way startGrokScript clears them.
func journaledGrok(t *testing.T, script string) (*session, *journal.Writer) {
	t.Helper()
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	grok := GrokProvider()
	return journaledScript(t, script, Options{Force: true, Provider: &grok})
}

// attemptLines is one attempt's pair: the prompt note and the prompt_end that
// closed it.
type attemptLines struct {
	prompt map[string]any
	end    map[string]any
}

// journalAttempts pairs every prompt note with its prompt_end, in the order
// the prompts were written. It fails on a prompt with no ending, an ending
// with no prompt, and a second ending for one attempt: that every attempt has
// exactly one of each is the invariant every row below rests on.
func journalAttempts(t *testing.T, lines []map[string]any) []attemptLines {
	t.Helper()
	var out []attemptLines
	at := make(map[string]int)
	for _, l := range lines {
		id, _ := l["attempt"].(string)
		switch l["type"] {
		case "prompt":
			if _, dup := at[id]; dup {
				t.Fatalf("two prompt notes for attempt %q", id)
			}
			at[id] = len(out)
			out = append(out, attemptLines{prompt: l})
		case "prompt_end":
			i, ok := at[id]
			if !ok {
				t.Fatalf("a prompt_end for attempt %q with no prompt note: %v", id, l)
			}
			if out[i].end != nil {
				t.Fatalf("two prompt_end notes for attempt %q", id)
			}
			out[i].end = l
		}
	}
	for _, a := range out {
		if a.end == nil {
			t.Fatalf("attempt %q has no prompt_end: %v", a.prompt["attempt"], a.prompt)
		}
	}
	return out
}

// jsonString is a line's field as a string, "" when the key is absent.
func jsonString(l map[string]any, key string) string {
	s, _ := l[key].(string)
	return s
}

// assertAttempt fails unless a records the kind and text given and ended with
// that stop reason and error class ("" for a key the line leaves out).
func assertAttempt(t *testing.T, what string, a attemptLines, kind journal.PromptKind, text, stopReason, errClass string) {
	t.Helper()
	if got := jsonString(a.prompt, "kind"); got != string(kind) {
		t.Errorf("%s: prompt kind %q, want %q", what, got, kind)
	}
	if got := jsonString(a.prompt, "text"); got != text {
		t.Errorf("%s: prompt text %q, want %q", what, got, text)
	}
	if got := jsonString(a.end, "stopReason"); got != stopReason {
		t.Errorf("%s: stopReason %q, want %q", what, got, stopReason)
	}
	if got := jsonString(a.end, "errClass"); got != errClass {
		t.Errorf("%s: errClass %q, want %q (%v)", what, got, errClass, a.end)
	}
	if _, ok := a.end["durationMs"]; !ok {
		t.Errorf("%s: the prompt_end carries no durationMs: %v", what, a.end)
	}
}

// journaledAttemptsOf closes the session, waits for its writer, and returns
// the file's attempts and its lines.
func journaledAttemptsOf(t *testing.T, s Session, w *journal.Writer) ([]attemptLines, []map[string]any) {
	t.Helper()
	closeJournaled(t, s, w)
	lines := fileLines(t, w)
	assertClosingIsLast(t, lines)
	return journalAttempts(t, lines), lines
}

// TestPromptNotesRecordACompletedTurn is the path table's completed row for the
// ACP session, and the text rule beside it: what the note holds is the draft as
// the user typed it, before craze expanded the plugin command into the request,
// because the expansion is an event of its own.
func TestPromptNotesRecordACompletedTurn(t *testing.T) {
	s, w := journaledScript(t, "echo", Options{Force: true, PluginDirs: []string{probeFixtureDir(t)}})
	log := collect(t, s)
	const typed = "/probe-plugin:probe-echo banana"
	if _, err := s.Prompt(t.Context(), typed); err != nil {
		t.Fatal(err)
	}
	// The expansion event is emitted before the request reaches the wire, so
	// it is in the channel by the time Prompt returns — but this log collects
	// on a goroutine of its own, and "emitted means buffered" promises the
	// buffer, not that the collector has drained it yet. Reading the snapshot
	// straight after Prompt therefore found an empty list on a loaded CI
	// runner. The wait is for the collector, not for the session.
	var cmds []Event
	waitUntil(t, "the expansion event to reach the collector", func() bool {
		cmds = commandEvents(log.snapshot())
		return len(cmds) > 0
	})
	if len(cmds) != 1 || !strings.Contains(cmds[0].Command.Text, "PROBE-COMMAND-EXPANDED") {
		t.Fatalf("the prompt expanded %+v, so there is nothing for the note to be shorter than", cmds)
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want one", len(attempts))
	}
	assertAttempt(t, "the completed turn", attempts[0], journal.PromptKindPrompt, typed, acp.StopEndTurn, "")
}

// TestPromptNotesRecordARefusedClaim is the claim-refused row: a Begin while
// the slot is held claims nothing, and its continuation's ErrPromptInFlight is
// what the note records — a second attempt of its own, beside the one that
// holds the slot.
func TestPromptNotesRecordARefusedClaim(t *testing.T) {
	s, w := journaledScript(t, "echo", Options{Force: true})
	holder := s.Begin("holder")
	if _, err := s.Begin("refused")(t.Context()); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("the second claim returned %v", err)
	}
	if _, err := holder(t.Context()); err != nil {
		t.Fatalf("the holder: %v", err)
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want the holder's and the refused one's", len(attempts))
	}
	assertAttempt(t, "the holder", attempts[0], journal.PromptKindPrompt, "holder", acp.StopEndTurn, "")
	assertAttempt(t, "the refused claim", attempts[1], journal.PromptKindPrompt, "refused", "", promptEndPromptInFlight)
}

// TestPromptNotesRecordACancelledCatalogWait is the row for a prompt Esc
// stopped while it was still parked for the agent's first command catalog: no
// turn was opened and nothing was sent, so the note is the only record there
// is that the user ever asked for it.
func TestPromptNotesRecordACancelledCatalogWait(t *testing.T) {
	shortCatalogWait(t, longCatalogWait)
	s, w := journaledScript(t, "nocommands", Options{Force: true, PluginDirs: []string{probeFixtureDir(t)}})
	out := promptOn(s, "/probe-echo banana")
	waitForCatalogWait(t, s)
	if _, err := s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := promptReturn(t, out, "the cancel never reached the wait"); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the cancelled prompt returned %v", err)
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want one", len(attempts))
	}
	assertAttempt(t, "the cancelled wait", attempts[0], journal.PromptKindPrompt, "/probe-echo banana", "", promptEndPromptCancelled)
}

// TestPromptNotesRecordAWithdrawnPrompt is the row for Esc straight after
// Enter: the claim is taken, the cancel lands on it, and the continuation
// withdraws before its turn is open. Nothing reached the agent, and the class
// is the same one the cancelled wait gets, because it is the same answer.
func TestPromptNotesRecordAWithdrawnPrompt(t *testing.T) {
	s, w := journaledScript(t, "echo", Options{Force: true})
	run := s.Begin("withdrawn")
	cancelled := cancelBounded(t, s)
	waitCancelling(t, s)
	if err := promptReturn(t, runOn(run), "the claimed prompt never came back"); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the claimed prompt returned %v", err)
	}
	if err := cancelReturn(t, cancelled, logWatchdog); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want one", len(attempts))
	}
	assertAttempt(t, "the withdrawn prompt", attempts[0], journal.PromptKindPrompt, "withdrawn", "", promptEndPromptCancelled)
}

// TestPromptNotesRecordAnErroredTurn is the errored-turn row: the agent failed
// the prompt itself, so the class is the table's fallback and the message is
// what the agent said. The turn's own classified error is on the EventError it
// emitted; prompt_end says how the call ended for its caller.
func TestPromptNotesRecordAnErroredTurn(t *testing.T) {
	s, w := journaledScript(t, "turnfail", Options{Force: true})
	if _, err := s.Prompt(t.Context(), "boom"); err == nil {
		t.Fatal("the turnfail script's prompt succeeded")
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want one", len(attempts))
	}
	assertAttempt(t, "the errored turn", attempts[0], journal.PromptKindPrompt, "boom", "", promptEndOther)
	if msg := jsonString(attempts[0].end, "errMessage"); !strings.Contains(msg, "the turn failed") {
		t.Fatalf("errMessage %q, want the agent's own words", msg)
	}
}

// TestPromptNotesRecordAWireRefusalAndAnInterjection is two rows of the table
// in the one run that produces both: grok mints a turn of its own for an
// interjection it could not merge, and while that foreign turn runs the client
// refuses craze's next prompt before the wire. The accepted interjection is
// journaled under its own attempt id, not the craze-N id the wire correlates
// it by — a refused one never spends one of those.
func TestPromptNotesRecordAWireRefusalAndAnInterjection(t *testing.T) {
	s, w := journaledGrok(t, "grok-long-turn-fallback")
	log := collect(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Prompt(t.Context(), "do the steps"); err != nil {
			t.Errorf("prompt: %v", err)
		}
	}()
	waitUntil(t, "the turn to start", s.promptInFlight)
	if err := s.Interject(t.Context(), "BANANA"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	<-done
	waitUntil(t, "the foreign turn's start event", func() bool {
		started, _ := foreignTurnCounts(log.snapshot())
		return started > 0
	})
	if _, err := s.Prompt(t.Context(), "refused"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("the prompt during a foreign turn returned %v", err)
	}
	waitUntil(t, "the foreign turn to end", func() bool { return !s.Snapshot().ForeignTurn })

	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 3 {
		t.Fatalf("%d attempts, want the turn, the interjection and the refused prompt", len(attempts))
	}
	assertAttempt(t, "the turn", attempts[0], journal.PromptKindPrompt, "do the steps", acp.StopEndTurn, "")
	assertAttempt(t, "the accepted interjection", attempts[1], journal.PromptKindInterject, "BANANA", "", "")
	assertAttempt(t, "the refused prompt", attempts[2], journal.PromptKindPrompt, "refused", "", promptEndForeignTurn)
	if id := jsonString(attempts[1].prompt, "attempt"); !strings.HasPrefix(id, string(journal.PromptKindInterject)+"-") {
		t.Fatalf("the interjection's attempt id is %q, want the journal's own", id)
	}
}

// TestInterjectNotesRecordItsRefusals is the interjection half of the table's
// last-but-one row: a provider that cannot interject at all, and one that can
// with no turn to merge into. Both refuse before the wire and return nothing
// the caller can see afterwards, which is exactly why the note is worth having.
func TestInterjectNotesRecordItsRefusals(t *testing.T) {
	t.Run("the provider has no interjections", func(t *testing.T) {
		s, w := journaledScript(t, "echo", Options{Force: true})
		if err := s.Interject(t.Context(), "nope"); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("cursor's Interject returned %v", err)
		}
		attempts, _ := journaledAttemptsOf(t, s, w)
		if len(attempts) != 1 {
			t.Fatalf("%d attempts, want one", len(attempts))
		}
		assertAttempt(t, "the unsupported interjection", attempts[0], journal.PromptKindInterject, "nope", "", promptEndUnsupported)
	})
	t.Run("there is no turn to merge into", func(t *testing.T) {
		s, w := journaledGrok(t, "grok-echo")
		if err := s.Interject(t.Context(), "nope"); !errors.Is(err, ErrNotInTurn) {
			t.Fatalf("an idle grok session's Interject returned %v", err)
		}
		attempts, _ := journaledAttemptsOf(t, s, w)
		if len(attempts) != 1 {
			t.Fatalf("%d attempts, want one", len(attempts))
		}
		assertAttempt(t, "the stranded interjection", attempts[0], journal.PromptKindInterject, "nope", "", promptEndNotInTurn)
	})
}

// assertSynthesizedEnding fails unless the file ends with exactly the prompt_end
// lines Close synthesized and then closing: an attempt Close ended has the
// closed class, no message of its own — nothing failed, the session went — and
// nothing at all is written after closing.
func assertSynthesizedEnding(t *testing.T, lines []map[string]any, attempts int) {
	t.Helper()
	assertClosingIsLast(t, lines)
	tail := lines[len(lines)-1-attempts:]
	for i, l := range tail[:attempts] {
		if l["type"] != "prompt_end" {
			t.Fatalf("the line %d before closing is %v, want a synthesized prompt_end", attempts-i, l)
		}
		if got := jsonString(l, "errClass"); got != promptEndClosed {
			t.Fatalf("the synthesized prompt_end's errClass is %q, want %q", got, promptEndClosed)
		}
		if msg := jsonString(l, "errMessage"); msg != "" {
			t.Fatalf("the synthesized prompt_end carries a message (%q), so it is the continuation's, not Close's", msg)
		}
	}
}

// TestCloseSynthesizesAnEndingForAnOpenPrompt is the table's last row for the
// ACP session, in both of its shapes: a prompt whose turn is open when Close
// runs — live Close does not join the prompt goroutine — and a continuation
// the caller never ran at all. Each ends with a prompt_end Close wrote, and
// the continuation's own ending, whenever it arrives, is refused by the cutoff
// rather than landing after closing.
func TestCloseSynthesizesAnEndingForAnOpenPrompt(t *testing.T) {
	t.Run("the turn is open", func(t *testing.T) {
		// A barrier of the test's own, not holdBeforeWire's: that one releases
		// on s.done, which Close closes first, and the prompt would then be
		// free to return before the cutoff. This one is released by the test
		// alone, so the turn is provably still open when Close writes.
		release := make(chan struct{})
		reached := make(chan struct{})
		testBeforeWire = func(*session) {
			close(reached)
			<-release
		}
		t.Cleanup(func() { testBeforeWire = nil })
		closeRelease := func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		t.Cleanup(closeRelease)

		s, w := journaledScript(t, "echo", Options{Force: true})
		out := promptOn(s, "held")
		select {
		case <-reached:
		case <-time.After(logWatchdog):
			t.Fatal("the prompt never reached the wire seam")
		}
		closeJournaled(t, s, w)
		lines := fileLines(t, w)
		assertSynthesizedEnding(t, lines, 1)
		attempts := journalAttempts(t, lines)
		if len(attempts) != 1 {
			t.Fatalf("%d attempts, want one", len(attempts))
		}
		assertAttempt(t, "the open prompt", attempts[0], journal.PromptKindPrompt, "held", "", promptEndClosed)
		closeRelease()
		<-out
	})

	t.Run("the continuation never ran", func(t *testing.T) {
		s, w := journaledScript(t, "echo", Options{Force: true})
		_ = s.Begin("never run")
		closeJournaled(t, s, w)
		lines := fileLines(t, w)
		assertSynthesizedEnding(t, lines, 1)
		attempts := journalAttempts(t, lines)
		if len(attempts) != 1 {
			t.Fatalf("%d attempts, want one", len(attempts))
		}
		assertAttempt(t, "the unrun continuation", attempts[0], journal.PromptKindPrompt, "never run", "", promptEndClosed)
	})
}

// TestStartFailedIsNotedBeforeClosing is A16: a Start that fails after the
// agent is spawned — the load scripts refuse session/new, so this one fails
// where a real agent's would — leaves a start_failed diag with the failure's
// class and message, and it comes before closing, because the note is written
// before Start tears the session down.
func TestStartFailedIsNotedBeforeClosing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	s := newTestSession(t, Options{
		Binary:     fakeAgentPath(t),
		ExtraArgs:  []string{"-script=load"},
		Workspace:  t.TempDir(),
		Force:      true,
		Stderr:     io.Discard,
		JournalDir: dir,
	})
	w := journalOf(t, s.log)
	err := s.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded against an agent that refuses session/new")
	}
	closeJournaled(t, s, w)

	lines := fileLines(t, w)
	assertClosingIsLast(t, lines)
	failures := diags(lines, journal.DiagStartFailed)
	if len(failures) != 1 {
		t.Fatalf("%d start_failed diags, want one (%v)", len(failures), lines)
	}
	if got, want := failures[0]["errClass"], promptEndOther; got != want {
		t.Errorf("start_failed errClass %v, want %q", got, want)
	}
	msg, _ := failures[0]["errMessage"].(string)
	if !strings.Contains(msg, "session/new must not be called") {
		t.Errorf("start_failed errMessage %q, want the agent's own refusal", msg)
	}
	// Before closing, and before the log stopped taking notes: a note written
	// after the internal Close would have been counted and dropped instead.
	if kindsBefore := lineKinds(lines); len(kindsBefore) == 0 ||
		indexOfKind(kindsBefore, journal.DiagStartFailed) > indexOfKind(kindsBefore, journal.DiagClosing) {
		t.Fatalf("start_failed does not precede closing: %v", kindsBefore)
	}
}

// lineKinds is every diag line's kind, in file order.
func lineKinds(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		if l["type"] == "diag" {
			out = append(out, jsonString(l, "kind"))
		}
	}
	return out
}

func indexOfKind(kinds []string, want string) int {
	for i, k := range kinds {
		if k == want {
			return i
		}
	}
	return -1
}

// The native adapter's half of the path table. Its rows are the live
// session's, minus the two the harness has no wire for (a refusal at the wire,
// an accepted interjection) and plus one of its own: a continuation run twice,
// which the adapter refuses and the journal reports once — as the run that
// happened, not as the refusal.

// journaledNative is a started native session on a scripted model, with a
// journal of its own and the writer whose file the test reads back.
func journaledNative(t *testing.T, f *nativeFixture, opts Options) (*nativeSession, *journal.Writer) {
	t.Helper()
	opts.JournalDir = filepath.Join(t.TempDir(), "journal")
	s := f.started(opts)
	return s, journalOf(t, s.log)
}

// waitNativeCancelling is the barrier a withdraw needs: the continuation reads
// the cancel mark in the section that would otherwise open its turn, so the
// test waits for Cancel to have set it before it runs the continuation.
func waitNativeCancelling(t *testing.T, s *nativeSession) {
	t.Helper()
	waitFor(t, "Cancel to mark the native claim cancelling", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cancelling
	})
}

// TestNativePromptNotesRecordACompletedTurnAndARefusedClaim is the native
// session's completed and claim-refused rows, which are the live session's
// answers given by the adapter's own claim.
func TestNativePromptNotesRecordACompletedTurnAndARefusedClaim(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("hello"))
	s, w := journaledNative(t, f, Options{})
	holder := s.Begin("holder")
	if _, err := s.Begin("refused")(t.Context()); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("the second claim returned %v", err)
	}
	if _, err := holder(t.Context()); err != nil {
		t.Fatalf("the holder: %v", err)
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want the holder's and the refused one's", len(attempts))
	}
	assertAttempt(t, "the holder", attempts[0], journal.PromptKindPrompt, "holder", "end_turn", "")
	assertAttempt(t, "the refused claim", attempts[1], journal.PromptKindPrompt, "refused", "", promptEndPromptInFlight)
}

// TestNativePromptNotesRecordADuplicateContinuationOnce is the adapter's own
// row. Running one continuation twice is a caller's mistake, refused with
// ErrPromptInFlight; the attempt is already closed by the run that happened,
// so the journal keeps that ending rather than overwriting it with the
// refusal's.
func TestNativePromptNotesRecordADuplicateContinuationOnce(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("hello"))
	s, w := journaledNative(t, f, Options{})
	run := s.Begin("once")
	if _, err := run(t.Context()); err != nil {
		t.Fatalf("the first call: %v", err)
	}
	if _, err := run(t.Context()); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("the second call returned %v", err)
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want the one Begin opened", len(attempts))
	}
	assertAttempt(t, "the run that happened", attempts[0], journal.PromptKindPrompt, "once", "end_turn", "")
}

// TestNativePromptNotesRecordAWithdrawnPromptAndAFailedTurn is the withdraw
// row and the errored-turn row: a cancel between the claim and the
// continuation, and a model that fails the turn.
func TestNativePromptNotesRecordAWithdrawnPromptAndAFailedTurn(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(reply(errorParts(errors.New("scripted failure"))))
	s, w := journaledNative(t, f, Options{})

	run := s.Begin("withdrawn")
	cancelled := make(chan error, 1)
	go func() { _, err := s.Cancel(context.Background()); cancelled <- err }()
	waitNativeCancelling(t, s)
	if _, err := run(t.Context()); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the claimed prompt returned %v", err)
	}
	if err := await(t, cancelled, "Cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.Prompt(t.Context(), "boom"); err == nil {
		t.Fatal("the failing model's turn succeeded")
	}

	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want the withdrawn one and the failed turn", len(attempts))
	}
	assertAttempt(t, "the withdrawn prompt", attempts[0], journal.PromptKindPrompt, "withdrawn", "", promptEndPromptCancelled)
	assertAttempt(t, "the failed turn", attempts[1], journal.PromptKindPrompt, "boom", "", promptEndOther)
	if msg := jsonString(attempts[1].end, "errMessage"); !strings.Contains(msg, "scripted failure") {
		t.Fatalf("errMessage %q, want the model's own failure", msg)
	}
}

// TestNativeInterjectNoteRecordsTheRefusal: the text the user sent is
// journaled with the refusal it met — it is the one place it is kept. Plan
// 019 gave the native session a real Interject, so a session with no turn
// running refuses with ErrNotInTurn where it once answered ErrUnsupported;
// what this pins is the note, which is the same either way.
func TestNativeInterjectNoteRecordsTheRefusal(t *testing.T) {
	f := newNativeFixture(t)
	s, w := journaledNative(t, f, Options{})
	if err := s.Interject(t.Context(), "nope"); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("the native Interject returned %v", err)
	}
	attempts, _ := journaledAttemptsOf(t, s, w)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want one", len(attempts))
	}
	assertAttempt(t, "the refused interjection", attempts[0], journal.PromptKindInterject, "nope", "", promptEndNotInTurn)
}

// TestNativeCloseSynthesizesAnEndingForAnUnrunContinuation is the native
// session's last row. Its Close waits for a turn in flight — so a running
// prompt writes its own ending — but deliberately not for a claimed
// continuation that may still be due on the caller's goroutine, and that is
// the one Close has to end itself.
func TestNativeCloseSynthesizesAnEndingForAnUnrunContinuation(t *testing.T) {
	f := newNativeFixture(t)
	s, w := journaledNative(t, f, Options{})
	_ = s.Begin("never run")
	closeJournaled(t, s, w)
	lines := fileLines(t, w)
	assertSynthesizedEnding(t, lines, 1)
	attempts := journalAttempts(t, lines)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want one", len(attempts))
	}
	assertAttempt(t, "the unrun continuation", attempts[0], journal.PromptKindPrompt, "never run", "", promptEndClosed)
}

// TestNativeStartFailedIsNoted is A16 for the adapter: a Start that never gets
// as far as opening the harness still leaves the failure in the journal.
func TestNativeStartFailedIsNoted(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := f.session(Options{Mode: "architecting", JournalDir: dir})
	w := journalOf(t, s.log)
	if err := s.Start(t.Context()); err == nil {
		t.Fatal("the native session started in a mode it has none of")
	}
	closeJournaled(t, s, w)
	lines := fileLines(t, w)
	assertClosingIsLast(t, lines)
	failures := diags(lines, journal.DiagStartFailed)
	if len(failures) != 1 {
		t.Fatalf("%d start_failed diags, want one (%v)", len(failures), lines)
	}
	if msg, _ := failures[0]["errMessage"].(string); !strings.Contains(msg, `mode "architecting"`) {
		t.Fatalf("start_failed errMessage %q, want the adapter's refusal", msg)
	}
}

// TestPromptErrClassNamesEverySentinel pins the second class table itself
// (plan 020 §3.3): every sentinel a prompt attempt can come back with, each
// wrapped the way the session that returns it wraps it, and the fallback for
// what is not one of them.
func TestPromptErrClassNamesEverySentinel(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, ""},
		{"a claim refused", ErrPromptInFlight, promptEndPromptInFlight},
		{"a wrapped refusal", fmt.Errorf("prompting: %w", ErrPromptInFlight), promptEndPromptInFlight},
		{"a withdrawn prompt", ErrPromptCancelled, promptEndPromptCancelled},
		{"the agent's own turn", ErrForeignTurn, promptEndForeignTurn},
		{"a provider without it", ErrUnsupported, promptEndUnsupported},
		{"no turn to merge into", ErrNotInTurn, promptEndNotInTurn},
		{"the connection closed", acp.ErrClosed, promptEndClosed},
		{"the harness closed", phraseTurnError(harness.ErrClosed), promptEndClosed},
		{"the caller's context", &nativeError{msg: "native: prompt cancelled", cause: context.Canceled}, promptEndCallerEnded},
		{"a deadline", context.DeadlineExceeded, promptEndCallerEnded},
		{"a full queue", ErrQueueFull, promptEndQueueFull},
		{"a message too long", ErrQueueTextTooLong, promptEndQueueTextTooLong},
		{"a turn the agent failed", &acp.RPCError{Code: -32000, Message: "the turn failed"}, promptEndOther},
		{"anything else", errors.New("agent: session not started"), promptEndOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := promptErrClass(tc.err); got != tc.want {
				t.Fatalf("promptErrClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
