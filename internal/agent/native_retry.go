package agent

import (
	"slices"
	"sync"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/journal"
)

// A retried step's retraction (D-32, as amended, at llm.MidStreamError). The
// harness says a step's attempt failed and will be sent again (harness.Retrying),
// after everything the attempt streamed; EventStepRetry says so on the stream,
// and says from where: the Seq the attempt began at. A consumer folds events in
// log order, so every one of the attempt's is before it, whatever consumer and
// whichever way it got them, and From means the same to a client that folded the
// attempt, one restored from a snapshot cut halfway through it, and a line
// reader of `craze prompt --json`.

// attemptMarks is where each owner's — "" the session's own, else a child's —
// current attempt at a step began. A leaf lock, as every lock in the sink is
// (sink's audit).
type attemptMarks struct {
	mu   sync.Mutex
	from map[string]attemptMark
}

type attemptMark struct {
	from uint64
	// shown is textShown as the attempt found it, the session's own only.
	shown bool
}

// markAttempt notes that owner's next event opens an attempt: at a step's start
// and after a retry's event, on the goroutine that publishes the attempt's
// events, so all of them come after it. Another owner's events may too. From
// is a line in the one stream and a child's rows are in a transcript of their
// own; a main-session event that is not its turn's (a state delta's error row)
// is not a text, thought or tool, which are what From retracts.
func (s *nativeSession) markAttempt(owner string) {
	s.attempts.mu.Lock()
	defer s.attempts.mu.Unlock()
	if s.attempts.from == nil {
		s.attempts.from = map[string]attemptMark{}
	}
	s.attempts.from[owner] = attemptMark{from: s.log.LastSeq() + 1, shown: owner == "" && s.textShown.Load()}
}

// stepRetried is owner's step failing and going again: the attempt's rows leave
// its tool set — they were settled, and a snapshot's tools are of calls that
// count — its record goes to the journal, and the stream says what it retracts.
func (s *nativeSession) stepRetried(owner string, e harness.Retrying) {
	s.attempts.mu.Lock()
	mark := s.attempts.from[owner]
	s.attempts.mu.Unlock()
	reason := nativeSafe{red: s.redactor()}.line(e.Reason)
	s.dropToolRows(owner, e.Abandoned)
	if owner == "" && e.Discarded {
		s.textShown.Store(mark.shown)
	}
	s.log.Note(journal.DiagNote{Kind: journal.DiagStepRetry, Fields: map[string]any{
		"agent": owner, "turn": e.Turn, "step": e.Step, "attempt": e.Attempt, "total": e.Total,
		"reason": reason, "delayMs": e.Delay.Milliseconds(), "discarded": e.Discarded, "from": mark.from,
	}})
	s.emit(Event{Type: EventStepRetry, Agent: owner, StepRetry: &StepRetryInfo{
		Turn: e.Turn, Step: e.Step, Attempt: e.Attempt, Total: e.Total, Reason: reason,
		DelayMs: e.Delay.Milliseconds(), From: mark.from, Discarded: e.Discarded,
	}})
	s.markAttempt(owner)
}

// dropToolRows takes ids out of owner's tool set.
func (s *nativeSession) dropToolRows(owner string, ids []string) {
	if len(ids) == 0 {
		return
	}
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	set := s.toolSetLocked(owner)
	if set == nil {
		return
	}
	for _, id := range ids {
		delete(set.rows, id)
	}
	set.order = slices.DeleteFunc(set.order, func(id string) bool { return slices.Contains(ids, id) })
}
