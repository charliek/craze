package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/journal"
)

// The native session's wake (plan 026 §3.11). A background sub-agent's result
// that finishes while no turn runs would otherwise wait for the user to type:
// the wake is the session starting a turn of its own to deliver it, bracketed
// as a foreign turn — EventForeignTurn{Running: true} before any of its
// output and {Running: false} after, with no EventDone and no EventError,
// exactly as the live session brackets grok's interjection fallback — so the
// engine above the seam, which holds craze's prompts while ForeignTurn() is
// true and drains its queue when the ending arrives (engine.go's observe),
// needs no new verb for it.
//
// # The worker
//
// One goroutine per session, started in the section that installs the harness
// (native.go's start) and joined by Close. It is kicked through a one-slot
// channel (kickWake) — by the harness when a result becomes pending
// (Options.OnPending), by every release of the prompt claim (prompt's deferred
// release, on every path), by a wake's own ending, and by the engine's
// FenceDown — and never trusts a kick: each recheck (recheckPending) reads the
// state from scratch, level-triggered (panel P23), so a result that became
// pending just after a turn's last step boundary is delivered by that turn's
// release, and one that became pending while the engine fenced the session by
// the fence coming down. Only a pending result wakes it (HasPending); one a
// failed or cancelled wake set aside is suspended, and never does (§3.11: no
// automatic retry loop).
//
// # The transitions (panel P22, astra r14's C9 invariants)
//
// The opening is ONE s.mu section: the session started and not closed,
// nothing claimed (neither a prompt's claim nor a wake's), the fence down and
// something pending — HasPending takes the harness's registry lock, a leaf
// (s.mu → regMu already exists through SetMode), and no other harness lock is
// taken under s.mu — then the claim's bookkeeping exactly as prompt installs
// a turn's (claimed, inPrompt, a fresh released, the turn's cancel func and
// ask token), wake and foreign set, s.snap.ForeignTurn with them, and the
// opening bracket ENQUEUED. Then, with s.mu released, the outbox is flushed,
// so the bracket is in the record before any of the wake's output.
//
// The ending is ONE s.mu section too, after the wake's tool rows are settled
// and its ask token retired outside the lock (as prompt does before its
// terminal event): wake and foreign cleared, the claim released — everything
// prompt's deferred release does — and the ending bracket enqueued. The
// order inside the section is the point: when the engine's observer reacts
// to the ending and runs its driver, ForeignTurn() is already false and the
// claim already free, so the queued row drains. Emitting the ending first and
// releasing after would strand the queue: the driver would look while the
// claim was still held, and nothing would kick it again
// (TestWakeEndingReleasesBeforeObserver). Then the flush, outside the lock,
// and the recheck's kick.
//
// Every way the turn can end goes through that ending: a delivered result, a
// provider failure, a cancel (Cancel and Close find the wake's cancel func
// and released channel where they find a turn's), a clean stop that persisted
// nothing, ErrNothingPending (nothing ran: the results went to a turn that
// claimed first), and a recovered panic. None leaves the claim held.
//
// # Locks
//
// No sink, Publish or Flush is ever called with s.mu held, and the only cross
// order stays e.mu → s.mu: FenceUp and FenceDown record a bool under s.mu and
// return, and the kick never blocks.

// wakeText is the opening bracket's Text: what the agent's turn is about.
const wakeText = "sub-agent result"

// diagSubagentWake is the journal diag each wake's outcome is written as
// (noteWake): a wake publishes no terminal event, so this is the one record
// of a wake that failed.
const diagSubagentWake = "subagent_wake"

// kickWake asks the worker for a recheck, without waiting: one slot, and a
// kick that finds it full is folded into the one already there, which the
// recheck's reading from scratch makes safe. It is Options.OnPending, called
// by the harness with no lock of its held, and it is safe from any goroutine
// at any time, a closed session included.
func (s *nativeSession) kickWake() {
	select {
	case s.wakeKick <- struct{}{}:
	default:
	}
}

// wakeWorker is the worker's loop: a recheck per kick, until Close. done is
// the channel Close joins on.
func (s *nativeSession) wakeWorker(done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-s.done:
			return
		case <-s.wakeKick:
		}
		s.recheckPending()
	}
}

// wakeReadyLocked is whether a wake may claim now: the harness open, the
// session not closed, nothing claimed, the fence down and a result pending.
// s.mu is held; HasPending takes the harness's registry lock, a leaf.
func (s *nativeSession) wakeReadyLocked() bool {
	return !s.closed && s.hs != nil && !s.claimed && !s.inPrompt && !s.fenced && s.hs.HasPending()
}

// recheckPending is one recheck (the file's comment). It reads first without
// claiming, so a kick that finds nothing to do — every prompt's release in a
// session with no background children — costs one locked read and no ask
// token; then it mints the wake's token outside s.mu, as prompt mints a
// turn's, and claims in one section, reading the state again there: a Begin
// that took s.mu in between has claimed, and the wake stands down for it —
// that turn takes the pending results at its first step (the harness's
// prepareStep does it).
func (s *nativeSession) recheckPending() {
	s.mu.Lock()
	seam, decided := s.wakeSeam, s.wakeDecided
	ready := s.wakeReadyLocked()
	s.mu.Unlock()
	if !ready {
		if decided != nil {
			decided(false)
		}
		return
	}
	if seam != nil {
		seam()
	}
	tok := s.asks.BeginTurn()

	s.mu.Lock()
	if !s.wakeReadyLocked() {
		s.mu.Unlock()
		s.asks.EndTurn(tok)
		if decided != nil {
			decided(false)
		}
		return
	}
	hs := s.hs
	s.wakeSeq++
	id := "wake-" + strconv.FormatUint(s.wakeSeq, 10)
	rel := make(chan struct{})
	// The session's context, not a caller's: nothing above the seam asked for
	// this turn. Cancel and Close end it through turnCancel, as a turn's.
	turnCtx, cancel := context.WithCancel(context.Background())
	s.claimed, s.inPrompt, s.cancelling, s.doneEmitted = true, true, false, false
	s.released = rel
	s.turnCancel, s.turnToken = cancel, tok
	s.wake, s.foreign, s.snap.ForeignTurn = true, true, true
	s.log.Enqueue(Event{Type: EventForeignTurn, At: s.Now(), ForeignTurn: &ForeignTurnInfo{
		ID: id, Text: wakeText, Reason: ReasonSubagentWake, Running: true,
	}})
	s.mu.Unlock()
	if decided != nil {
		decided(true)
	}
	// The bracket reaches the record before anything the wake says. Bounded
	// by s.done as every barrier here is: a Close that has begun frees it.
	_ = s.log.Flush(context.Background(), s.done)

	res, err := s.wakeTurn(hs, turnCtx)
	cancel()
	s.endWake(id, tok, rel, res, err)
}

// wakeTurn runs the wake's turn and recovers a panic from it as a failure of
// the turn, so the worker — which nothing above recovers for — ends the wake
// and releases the claim whatever happened inside.
func (s *nativeSession) wakeTurn(hs *harness.Session, ctx context.Context) (res harness.Result, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("native: the wake panicked: %v", v)
		}
	}()
	return hs.Wake(ctx, s.sink)
}

// endWake is the ending (the file's comment): the rows settled and the token
// retired outside s.mu; the state cleared, the claim released and the ending
// bracket enqueued in one section — marked queued until the flush that
// follows has committed it, for a prompt claiming in between (native.go's
// prompt); the flush; the journal's note of what the wake came to; and the
// recheck's kick, because a result that finished during the wake's last step
// is pending now and this ending is what delivers it.
func (s *nativeSession) endWake(id string, tok TurnToken, rel chan struct{}, res harness.Result, err error) {
	s.settleTools()
	s.asks.EndTurn(tok)
	s.mu.Lock()
	ended := s.wakeEnded
	s.wake, s.foreign, s.snap.ForeignTurn = false, false, false
	s.claimed, s.inPrompt, s.cancelling = false, false, false
	s.turnCancel, s.turnToken = nil, TurnToken{}
	if s.released == rel {
		s.released = nil
	}
	close(rel)
	s.log.Enqueue(Event{Type: EventForeignTurn, At: s.Now(), ForeignTurn: &ForeignTurnInfo{
		ID: id, Reason: ReasonSubagentWake,
	}})
	s.wakeEndingQueued = true
	s.mu.Unlock()
	if ended != nil {
		ended()
	}
	_ = s.log.Flush(context.Background(), s.done)
	s.mu.Lock()
	s.wakeEndingQueued = false
	s.mu.Unlock()
	s.noteWake(id, res, err)
	s.kickWake()
}

// noteWake journals one wake's outcome: delivered, nothing pending (a turn
// took the results first), cancelled, or failed with its error — redacted,
// since a provider's message can hold anything (plan 018 §3.7). Notes are
// journal-only and never block (EventLog.Note).
func (s *nativeSession) noteWake(id string, res harness.Result, err error) {
	fields := map[string]any{"wake": id}
	switch {
	case errors.Is(err, harness.ErrNothingPending):
		fields["outcome"] = "nothing_pending"
	case err != nil:
		fields["outcome"] = "failed"
		fields["error"] = s.redactor()(sanitizeLine(err.Error()))
	case res.StopReason == harness.StopCancelled:
		fields["outcome"] = "cancelled"
	default:
		fields["outcome"] = "delivered"
		fields["stop_reason"] = string(res.StopReason)
	}
	s.log.Note(journal.DiagNote{Kind: diagSubagentWake, Fields: fields})
}
