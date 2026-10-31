package engine

import (
	"sync"

	"github.com/charliek/craze/internal/agent"
)

// The close fence (plan 030 §3.6, round-2 blocker R2-1): the engine's half of
// a detached host's atomic idle-exit decision. The host's lifecycle goroutine,
// once its idle clock has run out, (1) raises the control server's attach
// fence (control.Server.FenceAttaches), then (2) this one, and (3) with both
// up decides: nobody attached and nothing in flight ends the session — the
// stop sequence, with both fences left up — and anything else lowers both and
// starts the clock again. The decision is atomic because nothing can be
// admitted between the reads it rests on and the stop: an attach is refused
// closing by the first fence, and every admission of the engine's by this one.
//
// # What the fence refuses
//
// Every admission: refusalLocked answers ErrClosing while a fence is up — code
// unavailable, reason closing, never stored — so Submit (a prompt, a queued
// prompt, a send-now), the queue verbs (Queue, EditQueued, Unqueue,
// ClearQueue), Disarm, Interject, Set (queued, and again in the worker's
// claim) and SetTitle are refused, and so is GiveUpDrain's drain, which would
// otherwise abandon the rows for good over a pause. An admission that came
// before the fence is in flight until its work is done: the queue verbs and
// Disarm do theirs inside the section that admitted them; Set is queued, then
// running; a turn is current; and the three whose work runs after e.mu is
// released — SetTitle's rename and index row, Interject's call, Submit's
// first-prompt seed — are counted from their admission until it is over
// (admitLocked, plan 030 C5r). A pass of the driver's —
// a settlement's successor, a drain — starts nothing either (canStartLocked
// reads the same refusal), and one it held back is replayed when the fence
// comes down (passHeld). What stops work goes on: Cancel, Stop, GiveUp,
// CancelSubagent, and Answer, which admits nothing new — an open ask already
// makes the engine busy.
//
// It keeps the session's admission fence up too (fenceWantLocked), so a
// session that can start a turn of its own — native's wake — cannot start one
// while the host decides. An ACP agent has no such fence: a turn the agent
// starts of its own after the verdict is ended by the stop's close exactly as
// by a quit (accepted: plan 030 C5).
//
// # Busy
//
// busy is anything in flight, read with the fence up so nothing new can join
// it: the engine starting, replaying, working or closing; a turn of its own
// current, a cancel on its way, a send-now armed; rows queued; a settings
// command queued or running; a command admitted whose work is not over
// (admitLocked); the agent's own turn (a foreign turn); an ask open (which
// pins a host indefinitely — the owner's rule); a sub-agent or a background
// child running; and a background child's result owed outside any turn — its
// child ended but its result not yet published, or published and waiting for
// the wake the fence holds back (agent.OwedWork, plan 030 C5r). A session whose
// asks do not pin it (Options.AsksDoNotPin, or its exit fence up) is not busy for
// what an open ask holds up: the turn or the agent's own turn waiting on it, the
// rows and the armed send behind that, the sub-agents running, one of which may be
// the asker, and the background work the parent cannot deliver while it waits. A
// start that
// failed replays nothing (retireReplay): its load's open bracket is not a
// replay running for ever. The engine's own facts are read in the fenced
// section; the ask registry, the session's roster and its owed work after it,
// outside e.mu — the registry's mutex is never nested with the engine's —
// which is still a cut: an ask opens, and a child is spawned, only inside a
// turn, a result owed is delivered only by one, and with the fence up none can
// start.

// FenceClose raises a close fence over the engine's admission and reports
// whether anything is in flight (the file's doc comment). release lowers it,
// and is idempotent; the engine admits again once every fence raised has been
// released, and a pass the fence held back is made then. The stop sequence
// never releases it: the engine's Close follows. It waits on nothing: one
// section under e.mu, then the registry's and the session's own reads, each
// brief.
func (e *Engine) FenceClose() (release func(), busy bool) {
	facts := e.closeFenceUp()
	asks := e.asks.Asks()
	busy = facts.busy(e.sess.Snapshot(), asks) || facts.owedCounts(asks) && e.owesWork()
	var once sync.Once
	return func() { once.Do(e.releaseCloseFence) }, busy
}

// Busy reports whether anything is in flight, by FenceClose's rule, with no
// fence raised: a sample for a host's idle clock, which is not a cut — the
// engine's facts are read under e.mu, and the session's foreign turn, its
// roster, its owed work and the asks outside it — and only FenceClose's
// verdict is one. It waits on nothing.
func (e *Engine) Busy() bool {
	snap := e.sess.Snapshot()
	asks := e.asks.Asks()
	e.mu.Lock()
	facts := e.busyFactsLocked()
	e.mu.Unlock()
	facts.waits = facts.waits || snap.ForeignTurn
	return facts.busy(snap, asks) || facts.owedCounts(asks) && e.owesWork()
}

// owesWork is the session's own work outside any turn (agent.OwedWork):
// native's background child still running, or its result pending for the wake
// (plan 030 C5r, astra r8-c5 1). The roster cannot say it — a background
// child's row is finished before its result is published — and the agent's
// turn cannot either: a pending result is no turn until the wake claims it,
// and under a close fence the wake cannot claim. So a verdict that read only
// those would stop a host with a result it was about to deliver, and the
// fence would be what kept it from being delivered. Read with e.mu released:
// the session takes its own leaves.
func (e *Engine) owesWork() bool {
	w, ok := e.sess.(agent.OwedWork)
	return ok && w.OwesWork()
}

// closeFenceUp is FenceClose's section: the session's admission fence first,
// as every section that reads the agent's turn raises it (the Engine doc's
// "The admission fence"), then one close fence more, and the engine's half of
// the busy verdict — its own facts and the agent's turn, read with both up.
// The deferred sync keeps the session's fence up for the close fence.
func (e *Engine) closeFenceUp() busyFacts {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.syncFenceLocked()
	e.raiseFenceLocked()
	e.closeFences++
	facts := e.busyFactsLocked()
	facts.waits = facts.waits || e.foreignLocked()
	return facts
}

// releaseCloseFence lowers one close fence. The last one down replays a pass
// the fences held back (passHeld): the kick that asked for it was spent while
// nothing could start, and nothing else may ever send another.
func (e *Engine) releaseCloseFence() {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.syncFenceLocked()
	e.closeFences--
	if e.closeFences == 0 && e.passHeld {
		e.passHeld = false
		e.wake()
	}
}

// busyFacts is the engine's half of the busy verdict: work, what is in flight
// whatever else is open; waits, what an open ask may be holding up; and whether
// an open ask pins this session at all.
type busyFacts struct {
	work, waits  bool
	asksDoNotPin bool
	// nothingStarts says the exit fence's turn has ended: a wake owed could never
	// claim, and a host waiting for it would wait for ever.
	nothingStarts bool
}

// busy is the verdict with the facts from outside the engine read (the file's
// doc comment), but for the session's owed work.
func (f busyFacts) busy(snap agent.Snapshot, asks []agent.AskRecord) bool {
	switch {
	case f.work:
		return true
	case f.asksDoNotPin && len(asks) > 0:
		return false
	}
	return f.waits || busyElsewhere(snap, asks)
}

// owedCounts says the session's owed work is any part of the verdict. Not where
// nothing can start again, and not where an open ask does not pin the session: a
// parent blocked on an ask nobody will answer delivers no background result either,
// so the limit cuts that work off with the ask.
func (f busyFacts) owedCounts(asks []agent.AskRecord) bool {
	return !f.nothingStarts && (!f.asksDoNotPin || len(asks) == 0)
}

// busyFactsLocked is the engine's own half of the busy verdict (the file's doc
// comment): everything but the agent's turn, the asks and the roster. e.mu is
// held.
func (e *Engine) busyFactsLocked() busyFacts {
	f := busyFacts{asksDoNotPin: e.opts.AsksDoNotPin || e.exitFence != nil, nothingStarts: e.exitFenceEndedLocked()}
	switch {
	case e.closed:
		f.work = true
	case e.activity == ActivityStarting, e.activity == ActivityClosing:
		f.work = true
	case e.isReplaying():
		f.work = true
	case e.cancelsInFlight > 0:
		f.work = true
	case len(e.sets) > 0, e.setRunning:
		f.work = true
	case e.admitting > 0:
		f.work = true
	}
	f.waits = e.activity == ActivityWorking || e.cur != nil || e.armed != nil || e.queue.Len() > 0
	return f
}

// admitLocked is refusalLocked for a command whose work runs after e.mu is
// released (plan 030 C5r, astra r8-c5 2): nil admits it and counts it in
// flight (e.admitting, busyLocked) in the very section that judged it, and
// done — which the caller calls once that work is over, the persistence
// included, on every way out — takes it out again. So a close fence raised
// after the admission reads it busy until it is over: without the count a
// rename admitted before the fence ran after the fence had reported idle, and
// renamed the session and wrote the index under the stop. Set needs none of
// it: its request is queued, then running, in the same sections (e.sets,
// setRunning). e.mu is held; done takes it.
func (e *Engine) admitLocked() (done func(), err error) {
	if err := e.refusalLocked(); err != nil {
		return nil, err
	}
	e.admitting++
	var once sync.Once
	return func() { once.Do(e.admitted) }, nil
}

// admitted is one admission's work over (admitLocked).
func (e *Engine) admitted() {
	e.mu.Lock()
	e.admitting--
	e.mu.Unlock()
}

// busyElsewhere is the rest of the verdict, from outside the engine: an ask
// open, or a sub-agent — a turn's child or a background one — still running.
func busyElsewhere(snap agent.Snapshot, asks []agent.AskRecord) bool {
	if len(asks) > 0 {
		return true
	}
	for _, s := range snap.Subagents {
		if s.Status == agent.SubagentRunning {
			return true
		}
	}
	return false
}
