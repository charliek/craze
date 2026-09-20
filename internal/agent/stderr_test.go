package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/journal"
)

// The agent child's stderr tee (plan 020 §3.5, A17/A25): what the child writes
// reaches whoever asked for it byte for byte and without waiting for the
// journal, and the journal's copy is bounded twice over — one line at 4 KiB
// while it is being scanned, and the session's whole share at the budget,
// after which the rest is one count.

// collectedNotes is the notes a tee wrote, in order.
type collectedNotes struct {
	notes []journal.Note
}

func (c *collectedNotes) note(n journal.Note) { c.notes = append(c.notes, n) }

// diagTexts is the text of every agent_stderr note, and whether each was
// marked truncated.
func (c *collectedNotes) diagTexts(t *testing.T) (texts []string, truncated []bool) {
	t.Helper()
	for _, n := range c.notes {
		d, ok := n.(journal.DiagNote)
		if !ok || d.Kind != journal.DiagAgentStderr {
			continue
		}
		text, _ := d.Fields["text"].(string)
		cut, _ := d.Fields["truncated"].(bool)
		texts = append(texts, text)
		truncated = append(truncated, cut)
	}
	return texts, truncated
}

// newCollectingTee is a tee writing into w and collecting its notes, without a
// journal behind it: what the scan does is this file's subject, and the
// journal's own queue has its own tests.
func newCollectingTee(w io.Writer) (*stderrTee, *collectedNotes) {
	c := &collectedNotes{}
	return &stderrTee{w: w, note: c.note}, c
}

// TestStderrTeeReassemblesFragmentedLines: the child's stderr arrives in
// whatever chunks the pipe hands over, so a line is only a line once its
// newline has been seen — and a \r\n one is not two.
func TestStderrTeeReassemblesFragmentedLines(t *testing.T) {
	var out bytes.Buffer
	tee, notes := newCollectingTee(&out)
	for _, chunk := range []string{"he", "llo wor", "ld\nsec", "ond\r\n", "\n", "third\n"} {
		if _, err := tee.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	tee.Flush()
	texts, _ := notes.diagTexts(t)
	want := []string{"hello world", "second", "third"}
	if !equalStrings(texts, want) {
		t.Fatalf("the tee noted %q, want %q (a blank line is not worth a note)", texts, want)
	}
	if got := out.String(); got != "hello world\nsecond\r\n\nthird\n" {
		t.Fatalf("the child's own writer saw %q", got)
	}
}

// TestStderrTeeTruncatesAnOversizedLine: one line is cut at the cap and marked,
// and the bytes past it are gone rather than buffered — the next line still
// starts where the newline says it does.
func TestStderrTeeTruncatesAnOversizedLine(t *testing.T) {
	tee, notes := newCollectingTee(io.Discard)
	long := strings.Repeat("x", stderrLineCap+4096)
	if _, err := tee.Write([]byte(long + "\nafter\n")); err != nil {
		t.Fatal(err)
	}
	texts, truncated := notes.diagTexts(t)
	if len(texts) != 2 {
		t.Fatalf("the tee noted %d lines, want the truncated one and the one after it", len(texts))
	}
	if len(texts[0]) != stderrLineCap || !truncated[0] {
		t.Fatalf("the long line was noted as %d bytes, truncated=%v; want %d and true", len(texts[0]), truncated[0], stderrLineCap)
	}
	if texts[1] != "after" || truncated[1] {
		t.Fatalf("the line after the truncated one is %q (truncated=%v)", texts[1], truncated[1])
	}
}

// TestStderrTeeDoesNotGrowWithoutANewline is the child that never writes one:
// the scan holds at most one capped line however much arrives, and Flush is
// what finally writes that fragment out, marked.
func TestStderrTeeDoesNotGrowWithoutANewline(t *testing.T) {
	tee, notes := newCollectingTee(io.Discard)
	chunk := bytes.Repeat([]byte("y"), 64<<10)
	for range 32 { // two megabytes with no newline in them
		if _, err := tee.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if got := cap(tee.line); got > stderrLineCap {
			t.Fatalf("the scan buffer grew to %d bytes, want no more than the %d-byte cap", got, stderrLineCap)
		}
	}
	if texts, _ := notes.diagTexts(t); len(texts) != 0 {
		t.Fatalf("a stream with no newline in it was noted as %d lines", len(texts))
	}
	tee.Flush()
	texts, truncated := notes.diagTexts(t)
	if len(texts) != 1 || len(texts[0]) != stderrLineCap || !truncated[0] {
		t.Fatalf("Flush wrote %d lines (first %d bytes, truncated=%v), want one capped and marked", len(texts), len(texts[0]), truncated[0])
	}
	// Flushing again writes nothing: Close calls it once, and a second call
	// must not repeat the fragment.
	tee.Flush()
	if again, _ := notes.diagTexts(t); len(again) != 1 {
		t.Fatalf("a second Flush wrote %d lines in all", len(again))
	}
}

// TestStderrTeeDoesNotChargeALineEndingToTheCap is the 4 KiB boundary with
// both line endings: the \r of a \r\n is line punctuation, not what the agent
// said, so it neither counts toward the cap nor marks the line truncated. A
// line of exactly the cap ends the same way whether it ends \n or \r\n, and
// whether the ending arrives in the same write as the line or the one after
// it — the child's stderr is chunked by the pipe, not by lines.
func TestStderrTeeDoesNotChargeALineEndingToTheCap(t *testing.T) {
	for _, n := range []int{stderrLineCap - 1, stderrLineCap, stderrLineCap + 1} {
		for _, ending := range []string{"\n", "\r\n"} {
			for _, split := range []bool{false, true} {
				name := fmt.Sprintf("%d bytes ending %q", n, ending)
				if split {
					name += " in a write of its own"
				}
				t.Run(name, func(t *testing.T) {
					tee, notes := newCollectingTee(io.Discard)
					line := strings.Repeat("x", n)
					writes := []string{line + ending}
					if split {
						writes = []string{line, ending}
					}
					for _, w := range writes {
						if _, err := tee.Write([]byte(w)); err != nil {
							t.Fatal(err)
						}
					}
					texts, truncated := notes.diagTexts(t)
					if len(texts) != 1 {
						t.Fatalf("the tee noted %d lines, want one", len(texts))
					}
					wantLen, wantCut := min(n, stderrLineCap), n > stderrLineCap
					if len(texts[0]) != wantLen || truncated[0] != wantCut {
						t.Fatalf("a %d-byte line ending %q was noted as %d bytes, truncated=%v; want %d and %v",
							n, ending, len(texts[0]), truncated[0], wantLen, wantCut)
					}
					if strings.ContainsRune(texts[0], '\r') {
						t.Fatalf("the noted line carries the \\r of its own line ending: %q", texts[0][len(texts[0])-1:])
					}
				})
			}
		}
	}
}

// TestStderrTeeKeepsAFinalCarriageReturn: a \r at the end of an unterminated
// last fragment is content — the child stopped there, so nothing makes it a
// line ending — and Flush keeps it. A \r that does turn out to be one is still
// dropped, so both spellings are in the same test.
func TestStderrTeeKeepsAFinalCarriageReturn(t *testing.T) {
	tee, notes := newCollectingTee(io.Discard)
	if _, err := tee.Write([]byte("first\r\nerror\r")); err != nil {
		t.Fatal(err)
	}
	if texts, _ := notes.diagTexts(t); !equalStrings(texts, []string{"first"}) {
		t.Fatalf("before the flush the tee noted %q, want only the terminated line", texts)
	}
	tee.Flush()
	texts, truncated := notes.diagTexts(t)
	if !equalStrings(texts, []string{"first", "error\r"}) {
		t.Fatalf("the tee noted %q, want the terminated line and the fragment with its own carriage return", texts)
	}
	if truncated[1] {
		t.Fatalf("the final fragment %q was marked truncated", texts[1])
	}
}

// countingWriter answers a short write and then an error, so the tee's answers
// can be compared with the ones the same writer gives on its own.
type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

var errStderrSink = errors.New("test: the stderr sink failed")

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	switch c.writes {
	case 2:
		// A short write with no error: io.Copy treats it as one, and whatever
		// the tee does it must not turn it into something else.
		n, _ := c.buf.Write(p[:len(p)/2])
		return n, nil
	case 3:
		return 0, errStderrSink
	}
	return c.buf.Write(p)
}

// TestStderrTeeIsTransparentToItsWriter: with the tee and without it, the
// session's own writer sees the same bytes in the same chunks and the caller
// gets the same (n, err) each time — a short write and a failure included.
func TestStderrTeeIsTransparentToItsWriter(t *testing.T) {
	chunks := [][]byte{[]byte("one\n"), []byte("two\nthree"), []byte("\nfour\n"), []byte("five\n")}
	run := func(wrap bool) (string, []string) {
		sink := &countingWriter{}
		var w io.Writer = sink
		if wrap {
			tee, _ := newCollectingTee(sink)
			w = tee
		}
		var answers []string
		for _, c := range chunks {
			n, err := w.Write(c)
			answers = append(answers, fmt.Sprintf("%d/%v", n, err))
		}
		return sink.buf.String(), answers
	}
	bare, bareAnswers := run(false)
	teed, teeAnswers := run(true)
	if bare != teed {
		t.Fatalf("with the tee the writer saw %q, without it %q", teed, bare)
	}
	if !equalStrings(bareAnswers, teeAnswers) {
		t.Fatalf("with the tee Write answered %v, without it %v", teeAnswers, bareAnswers)
	}
}

// TestStderrTeeDoesNotWaitOnTheJournal is A17's stall: the child's own writer
// is served before the journal is asked for anything, so a journal that never
// answered could still not delay it. The real Note never blocks; this holds
// one still to prove the order the tee does it in.
func TestStderrTeeDoesNotWaitOnTheJournal(t *testing.T) {
	var sink bytes.Buffer
	seen := make(chan struct{})
	release := make(chan struct{})
	tee := &stderrTee{
		w: writerFunc(func(p []byte) (int, error) {
			n, err := sink.Write(p)
			close(seen)
			return n, err
		}),
		note: func(journal.Note) { <-release },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tee.Write([]byte("the agent's last words\n"))
	}()
	select {
	case <-seen:
	case <-time.After(logWatchdog):
		t.Fatal("the child's own writer was not served while the journal was held")
	}
	if got := sink.String(); got != "the agent's last words\n" {
		t.Fatalf("the writer saw %q before the journal was released", got)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(logWatchdog):
		t.Fatal("the tee never returned after the journal was released")
	}
}

// writerFunc is an io.Writer from a function.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestStderrBudgetKeepsOneCountAndLosesNoEvents is A25: an agent that writes
// past the session's budget has its first lines journaled and the rest counted
// once, and the events published while it was shouting are all in the file —
// the budget exists precisely so stderr can never push events into a gap.
func TestStderrBudgetKeepsOneCountAndLosesNoEvents(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	keepDrained(t, l)
	tee := newStderrTee(io.Discard, l)
	if tee == nil {
		t.Fatal("a journaled log built no tee")
	}
	const over = 250
	const events = 40
	for i := range stderrBudgetLines + over {
		if _, err := fmt.Fprintf(tee, "line %d\n", i+1); err != nil {
			t.Fatal(err)
		}
		if i%((stderrBudgetLines+over)/events) == 0 {
			publishWithin(t, l, textEvent(fmt.Sprintf("event %d", i)))
		}
	}
	tee.Flush()
	closeLog(t, l, w)

	lines := fileLines(t, w)
	notes := diags(lines, journal.DiagAgentStderr)
	if len(notes) != stderrBudgetLines {
		t.Fatalf("%d agent_stderr notes, want exactly the budget's %d", len(notes), stderrBudgetLines)
	}
	if first := notes[0]["text"]; first != "line 1" {
		t.Fatalf("the first note kept is %v, want the first line the agent wrote", first)
	}
	drops := diags(lines, journal.DiagAgentStderrDropped)
	if len(drops) != 1 {
		t.Fatalf("%d agent_stderr_dropped notes, want one count", len(drops))
	}
	if got := drops[0]["lines"]; got != float64(over) {
		t.Fatalf("the dropped count is %v, want the %d lines past the budget", got, over)
	}
	// The events are the point: not one of them was lost to the shouting.
	var seqs []uint64
	for _, l := range lines {
		if l["type"] == "event" {
			seqs = append(seqs, uint64(l["seq"].(float64)))
		}
	}
	if p := runSeqs(seqs, 1); p != "" {
		t.Fatalf("the events are not whole: %s", p)
	}
	if len(seqs) == 0 {
		t.Fatal("no events were journaled beside the stderr")
	}
	if h := l.Health().Journal; h.State != journal.StateOK || len(h.Gaps) != 0 {
		t.Fatalf("the journal is %q with gaps %v: the stderr budget did not hold", h.State, h.Gaps)
	}
}

// TestSessionJournalsTheAgentsStderr is the wiring: a real session's child
// writes on its own stderr, and what it said is in the journal as
// agent_stderr — while Options.Stderr still sees every byte, because that is
// what plan 017's exit tail reads.
func TestSessionJournalsTheAgentsStderr(t *testing.T) {
	const said = "craze-fake-agent: SPEAKING-UP"
	t.Setenv("CRAZE_FAKE_STDERR", said)
	var own bytes.Buffer
	dir := filepath.Join(t.TempDir(), "journal")
	s := newTestSession(t, Options{
		Binary:     fakeAgentPath(t),
		ExtraArgs:  []string{"-script=echo"},
		Workspace:  t.TempDir(),
		Force:      true,
		Stderr:     &own,
		Diag:       io.Discard,
		JournalDir: dir,
	})
	w := journalOf(t, s.log)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	closeJournaled(t, s, w)

	if got := strings.Count(own.String(), said); got == 0 {
		t.Fatalf("Options.Stderr saw %q, want the agent's own line", own.String())
	}
	lines := fileLines(t, w)
	notes := diags(lines, journal.DiagAgentStderr)
	if len(notes) == 0 {
		t.Fatalf("the journal holds no agent_stderr note: %v", lines)
	}
	for _, n := range notes {
		if text, _ := n["text"].(string); text != said {
			t.Fatalf("an agent_stderr note holds %q, want %q", text, said)
		}
	}
	assertClosingIsLast(t, lines)
}

// TestCrazesOwnNotesAreNotTheAgents: Options.Diag falls back to Options.Stderr
// when a caller sets only one, and craze's own notes must not become the
// agent's words in the journal because of it. The tee is the child's lane
// alone.
func TestCrazesOwnNotesAreNotTheAgents(t *testing.T) {
	var own bytes.Buffer
	dir := filepath.Join(t.TempDir(), "journal")
	s := newTestSession(t, Options{
		Binary:     fakeAgentPath(t),
		ExtraArgs:  []string{"-script=echo"},
		Workspace:  t.TempDir(),
		Force:      true,
		Stderr:     &own,
		PluginDirs: []string{filepath.Join(t.TempDir(), "not-there")},
		JournalDir: dir,
	})
	w := journalOf(t, s.log)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	closeJournaled(t, s, w)
	if !strings.Contains(own.String(), "plugin dir") {
		t.Fatalf("craze's own note never reached Options.Stderr: %q", own.String())
	}
	for _, n := range diags(fileLines(t, w), journal.DiagAgentStderr) {
		if text, _ := n["text"].(string); strings.Contains(text, "plugin dir") {
			t.Fatalf("craze's own note was journaled as the agent's stderr: %q", text)
		}
	}
}
