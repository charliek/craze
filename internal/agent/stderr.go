package agent

import (
	"bytes"
	"io"
	"sync"

	"github.com/charliek/craze/internal/journal"
)

// The agent child's stderr is where an ACP agent says what went wrong when the
// wire says nothing at all — a crash, a login prompt, a stack trace. A
// journaled session tees it (plan 020 §3.5): the child's bytes go on to
// whoever asked for them, unchanged and without waiting for anything, and a
// copy is split into lines beside them and noted.
//
// Two bounds keep a talkative agent from being the thing that fills a journal.
// One line is cut at stderrLineCap while it is being scanned, so a child that
// never writes a newline costs a fixed 4 KiB and no more; and the session as a
// whole journals at most stderrBudgetBytes or stderrBudgetLines, after which
// the rest is counted and one agent_stderr_dropped note is written at close.
// Without the second bound an agent looping on its own stderr would push the
// journal's queue into the gap state and take the session's events with it.

const (
	// stderrLineCap is the hard cap on one journaled line. Bytes past it are
	// discarded as they are scanned, never buffered, and the note says it was
	// truncated.
	stderrLineCap = 4 << 10
	// stderrBudgetBytes and stderrBudgetLines are one session's whole stderr
	// budget. The byte bound is the TUI's deferredStderrMax, which exists for
	// the same looping agent; the line bound catches one that writes many
	// short lines instead of many bytes.
	stderrBudgetBytes = 256 << 10
	stderrBudgetLines = 2000
)

// stderrTee is Options.Stderr with the journal beside it. Its Write forwards
// the original chunk first and returns the original writer's own (n, err), so
// nothing about the child's stderr changes: what reads it sees the same bytes,
// the same answers, and no delay from the journal, which is why the copy is
// taken after the forward and never waits.
type stderrTee struct {
	w io.Writer // the session's own sink; nil when it has none
	// note is where a completed line goes: the log's Note, which queues it
	// and returns. It is a field rather than a call so that a test can hold
	// one still and see that the child's own writer was served all the same
	// (plan 020 A17).
	note func(journal.Note)

	// mu guards the scan. acp copies the child's stderr from one goroutine,
	// so it is uncontended in practice; it is here because Flush runs on the
	// closing goroutine.
	mu sync.Mutex
	// line is the current unterminated line, never longer than stderrLineCap,
	// and truncated says bytes of it were discarded past that cap.
	line      []byte
	truncated bool
	// bytes and lines are what this session has journaled, against the budget;
	// dropped and droppedBytes are what it refused once the budget was gone.
	bytes        int
	lines        int
	dropped      int
	droppedBytes int
}

// newStderrTee wraps w for a journaled session. It returns nil when the log
// has no journal: there is then nothing to tee into, and the session keeps
// handing the child's stderr straight to w.
func newStderrTee(w io.Writer, log *EventLog) *stderrTee {
	if log == nil || log.journal == nil {
		return nil
	}
	return &stderrTee{w: w, note: log.Note}
}

// Write forwards p to the session's own sink and returns exactly what that
// sink returned, then takes the journal's copy. A session with no sink of its
// own reports the whole chunk written, as io.Discard does.
func (t *stderrTee) Write(p []byte) (int, error) {
	n, err := len(p), error(nil)
	if t.w != nil {
		n, err = t.w.Write(p)
	}
	t.absorb(p)
	return n, err
}

// absorb splits p into lines, noting each as it completes. What is left over
// waits for the next write, or for Flush.
func (t *stderrTee) absorb(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.appendLocked(p)
			return
		}
		t.appendLocked(p[:i])
		t.emitLocked()
		p = p[i+1:]
	}
}

// appendLocked adds b to the line being scanned, keeping what fits under the
// cap and discarding the rest: the line is bounded whatever the child writes.
func (t *stderrTee) appendLocked(b []byte) {
	room := stderrLineCap - len(t.line)
	if len(b) <= room {
		t.line = append(t.line, b...)
		return
	}
	if room > 0 {
		t.line = append(t.line, b[:room]...)
	}
	t.truncated = true
}

// emitLocked notes the line just completed, unless it is empty or the
// session's budget is gone, and starts the next one. It keeps the line's
// storage, so a steady stream allocates nothing beyond the note's own text.
func (t *stderrTee) emitLocked() {
	// A \r\n line ends with a carriage return that is not part of what the
	// agent said.
	line := bytes.TrimSuffix(t.line, []byte("\r"))
	truncated := t.truncated
	if len(line) == 0 {
		t.line, t.truncated = t.line[:0], false
		return
	}
	text := string(line)
	t.line, t.truncated = t.line[:0], false
	if t.lines >= stderrBudgetLines || t.bytes+len(text) > stderrBudgetBytes {
		t.dropped++
		t.droppedBytes += len(text)
		return
	}
	t.lines++
	t.bytes += len(text)
	fields := map[string]any{"text": text}
	if truncated {
		fields["truncated"] = true
	}
	t.note(journal.DiagNote{Kind: journal.DiagAgentStderr, Fields: fields})
}

// Flush notes the last line, which a child that exits without a final newline
// leaves unterminated, and the count of everything the budget refused. It is
// called once the child's stderr copy has finished — acp.Child.Wait, which
// Client.Close waits for, is what says so, since io.Copy never closes its
// destination — and before the log's own Close, which is the note cutoff
// (plan 020 §3.5). Calling it twice writes nothing the second time, and a nil
// tee (an unjournaled session) does nothing at all.
func (t *stderrTee) Flush() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emitLocked()
	if t.dropped == 0 {
		return
	}
	t.note(journal.DiagNote{Kind: journal.DiagAgentStderrDropped, Fields: map[string]any{
		"lines": t.dropped, "bytes": t.droppedBytes,
	}})
	t.dropped, t.droppedBytes = 0, 0
}
