package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// ChainPolicy is what a settled turn does to the queue behind it. It is the
// engine's because the rule has to run inside the settlement, in the section
// that decides the successor: a client that applied it on hearing the ending
// would be too late, the next row already on its way to the agent.
type ChainPolicy struct {
	// StopOnNonEndTurn ends the chain on any clean stop other than end_turn —
	// cancelled, max_turn_requests and the rest: the queue is cleared and
	// nothing succeeds the turn. It is `craze prompt`'s rule, where a chain of
	// follow-ups is one request and a turn that did not finish has ended it.
	// The TUI leaves it false: Esc stops a turn, and the queue carries on.
	StopOnNonEndTurn bool
	// RetryForeignTurn keeps a turn the session refused with
	// agent.ErrForeignTurn current and claims it again once the agent's own
	// turn is over, with no event for the refusal and no second started. The
	// session's foreign-turn flag can lag the client's, so admission can pass
	// and the prompt still be refused; `craze prompt` has always waited that
	// out. The client owns the budget: it calls Stop when it has waited long
	// enough, and the turn then ends as the refusal it was. The TUI leaves it
	// false and shows the refusal.
	RetryForeignTurn bool
}

// Options configure an Engine. The zero value is the TUI's.
type Options struct {
	Chain ChainPolicy
}

// foreignRetryTick is how often a turn held by ChainPolicy.RetryForeignTurn
// is looked at again when nothing has said the foreign turn ended: the
// session's flag can already be clear while the client still refuses, and
// then no event is coming.
const foreignRetryTick = 10 * time.Millisecond

// turn is one turn the engine reserved: from the section that claimed it to
// the settlement that ended it.
type turn struct {
	id     string
	text   string
	origin string
	cause  string
	// returned says the continuation has come back with res and err. A turn
	// settles once it has and no cancel is in flight.
	returned bool
	res      agent.Result
	err      error
	// retry says the claim was refused with ErrForeignTurn under
	// ChainPolicy.RetryForeignTurn and is to be taken again: the turn is
	// current, working, and has no continuation running.
	retry bool
}

// launch is a continuation the engine has claimed under e.mu and has still to
// run, which it does only once the lock is released.
type launch struct {
	t   *turn
	run func(context.Context) (agent.Result, error)
	// barrier says this is the turn's first continuation, which waits for the
	// outbox so that its started is delivered before anything the agent says.
	barrier bool
}

// Engine wraps exactly one agent.Session and is the one driver of its turns:
// admission, the queue's removal-plus-claim, prompt completion with the
// endings the wire never produced, and cancel against a turn id. Clients
// submit commands and observe events (session control SD-24); two of them
// cannot double-drain, and a session with none drains itself.
//
// # Locks
//
// e.mu guards everything below it. It is never held across a call that
// blocks — a provider call, Session.Cancel, a continuation, Publish, Flush,
// file I/O — and the engine calls exactly two things on the session while
// holding it, both of which take the session's own mutex briefly and wait on
// nothing: Begin, and the leaf accessor ForeignTurn. So the order is
// e.mu → s.mu, and it cannot cycle, because a session never calls the engine:
// it reports through Result, through events, and through the log's observer.
//
// Under e.mu the engine also calls EventLog.Enqueue, whose mutex is a leaf.
// That is the point of the outbox: every event the engine authors is enqueued
// in the section that made the change it describes, so the order of the
// engine's events is the order of its state, with no lock held across a send.
// What follows from it is that an engine event trails the state it describes:
// State can already show a turn whose started has not been delivered.
//
// The observer runs inside the log's publishing boundary and takes only
// e.obsMu, a leaf that guards the two flags it keeps.
type Engine struct {
	sess agent.Session
	log  *agent.EventLog
	now  func() time.Time
	opts Options

	mu              sync.Mutex
	activity        Activity
	cur             *turn
	turnSeq         int
	clientSeq       int
	queue           agent.PromptQueue
	cancelsInFlight int
	stopped         bool
	closed          bool
	prompted        bool
	cancelled       bool
	err             string
	startFailed     bool

	obsMu     sync.Mutex
	replaying bool

	// kick wakes the driver. One slot is enough because the driver's passes
	// are level-triggered: each re-reads the state under e.mu, so two kicks
	// collapsed into one lose nothing.
	kick chan struct{}
	done chan struct{}
	wg   sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	// hooks are test seams; nil in production.
	hooks *hooks
}

// hooks let a test hold the engine at the points its schedules turn on.
type hooks struct {
	// beforeSessionCancel runs after a cancel has been validated and its hold
	// taken, just before Session.Cancel is called.
	beforeSessionCancel func(turn string)
	// afterSessionCancel runs when Session.Cancel has returned, before the
	// hold is released.
	afterSessionCancel func(turn string)
	// turnReturned runs once a continuation has come back and the pass its
	// return allowed is over, with e.mu released.
	turnReturned func(turn string)
}

// New builds the engine for sess. The session must own an event log
// (agent.LogOwner), because the engine publishes through it and observes it;
// New installs the log's single observer, so a second engine on one session is
// refused with agent.ErrObserverSet. It is called before sess.Start.
func New(sess agent.Session, opts Options) (*Engine, error) {
	owner, ok := sess.(agent.LogOwner)
	if !ok || owner.EventLog() == nil {
		return nil, fmt.Errorf("engine: %T has no event log", sess)
	}
	e := &Engine{
		sess:     sess,
		log:      owner.EventLog(),
		now:      time.Now,
		opts:     opts,
		activity: ActivityStarting,
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	// The session's clock, not the wall's: the Stub's is injected, and every
	// golden that shows a time shows one of its. It is read where the session
	// reads it already — on the goroutine that admits a prompt and the one
	// that runs it — so an injected clock needs nothing it did not need before.
	if c, ok := sess.(agent.Clocked); ok {
		e.now = c.Now
	}
	if err := e.log.Observe(e.observe); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	e.wg.Add(1)
	go e.drive()
	return e, nil
}

// Session is the session the engine wraps: the raw provider seam, for what has
// not moved behind Control yet.
func (e *Engine) Session() agent.Session { return e.sess }

// Start starts the session. Until it returns the engine refuses every
// command; afterwards it is idle, or failed with StartFailed set.
func (e *Engine) Start(ctx context.Context) error {
	err := e.sess.Start(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return err
	}
	if err != nil {
		e.activity = ActivityError
		e.startFailed = true
		e.err = err.Error()
		return err
	}
	e.activity = ActivityIdle
	return nil
}

// Events is the session's primary subscription.
func (e *Engine) Events() <-chan agent.Event { return e.log.Primary() }

// Subscribe opens a budgeted subscription on the session's log.
func (e *Engine) Subscribe(o agent.SubscribeOptions) (*agent.Subscription, error) {
	return e.log.Subscribe(o)
}

// NewClientID mints a client id unique in the incarnation.
func (e *Engine) NewClientID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clientSeq++
	return fmt.Sprintf("c-%d", e.clientSeq)
}

// Sync returns once every event enqueued before the call has been delivered.
func (e *Engine) Sync(ctx context.Context) error { return e.log.Flush(ctx) }

// Interject merges text into the running turn. It is the session's own verb
// and its own refusals: the engine adds nothing but the door.
func (e *Engine) Interject(ctx context.Context, _ Command, text string) error {
	e.mu.Lock()
	refused := e.refusalLocked()
	e.mu.Unlock()
	if refused != nil {
		return refused
	}
	return e.sess.Interject(ctx, text)
}

// Close refuses every later command, closes the session — which ends the turn
// that is running and closes the log last, committing whatever the outbox
// still holds — and joins the engine's goroutines. It is idempotent.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.activity = ActivityClosing
		e.mu.Unlock()
		e.closeErr = e.sess.Close()
		close(e.done)
		e.wg.Wait()
	})
	return e.closeErr
}

// observe is the log's observer. It runs inside the publishing boundary, once
// per committed event, so it does nothing but note what the driver has to look
// at again and wake it: it takes no lock but its own leaf and calls neither the
// log nor the session. It is the seed of S1c's fold.
func (e *Engine) observe(ev agent.Event) {
	switch ev.Type {
	case agent.EventReplay:
		if ev.Replay == nil {
			return
		}
		e.obsMu.Lock()
		e.replaying = ev.Replay.Phase == agent.ReplayStart
		e.obsMu.Unlock()
		if ev.Replay.Phase == agent.ReplayEnd {
			e.wake()
		}
	case agent.EventForeignTurn:
		if ev.ForeignTurn != nil && !ev.ForeignTurn.Running {
			e.wake()
		}
	}
}

// wake kicks the driver, without waiting.
func (e *Engine) wake() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) isReplaying() bool {
	e.obsMu.Lock()
	defer e.obsMu.Unlock()
	return e.replaying
}

// refusalLocked is why the engine admits no command at all right now, or nil.
func (e *Engine) refusalLocked() error {
	switch {
	case e.closed, e.stopped:
		return ErrNotAccepting
	case e.activity == ActivityStarting, e.startFailed:
		return ErrNotAccepting
	case e.isReplaying():
		return ErrNotAccepting
	}
	return nil
}

// canStartLocked reports whether a turn may be started now, from any path:
// Submit, the drain, or a claim taken again. Nothing is current, no cancel is
// in flight — a cancel validated against the last turn and still on its way to
// the session would otherwise land on the one started here — and the agent is
// not running a turn of its own, as far as the session knows.
func (e *Engine) canStartLocked() bool {
	return e.cur == nil && e.cancelsInFlight == 0 && e.refusalLocked() == nil && !e.sess.ForeignTurn()
}

// Submit admits a prompt: it starts a turn now, or queues the text.
//
// Starting is one section under e.mu: the gate, the room in the outbox, the
// turn's id, working, its started event, and the session's Begin. The claim is
// therefore taken on the caller's goroutine — the Update that shows the turn
// working — so an Esc handled by the next Update cancels this prompt, which
// withdraws without reaching the agent, rather than finding no turn and writing
// its cancel ahead of the prompt. Reservation and claim being atomic is also
// why Submit never has to forward a cancel itself: there is no moment at which
// a turn is reserved and not claimed. It waits on nothing.
func (e *Engine) Submit(c Command, text string, mode SubmitMode, fromRow string) (SubmitResult, error) {
	if mode != SubmitQueue || fromRow != "" {
		// Send-now and row-sourced sends arrive with the queue verbs.
		return SubmitResult{}, fmt.Errorf("%w: submit mode %q", ErrBadRequest, mode)
	}
	var started []launch
	res, err := func() (SubmitResult, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.refusalLocked(); err != nil {
			return SubmitResult{}, err
		}
		if !e.log.OutboxRoom() {
			return SubmitResult{}, ErrUnavailable
		}
		// A direct submit is what clears an error: the turn that failed said
		// so, and only the next send retires it.
		if e.canStartLocked() {
			l := e.reserveLocked(text, agent.TurnOriginSubmit, c.Cause(), nil)
			started = append(started, l)
			return SubmitResult{Turn: l.t.id}, nil
		}
		row, qev, err := e.queue.Add(text, e.now())
		if err != nil {
			return SubmitResult{}, err
		}
		e.log.Enqueue(e.stamp(qev.Event(), c.Cause()))
		return SubmitResult{Queued: &row}, nil
	}()
	e.run(started)
	return res, err
}

// reserveLocked starts a turn: it claims it, and enqueues before — the queue
// events that freed the text, if it came from a row — followed by the turn's
// started, as one batch. The band shrinks before the user row appears, as it
// always has.
func (e *Engine) reserveLocked(text, origin, cause string, before []agent.Event) launch {
	l := e.claimLocked(text, origin, cause)
	e.log.Enqueue(append(before, e.startedEvent(l.t))...)
	return l
}

// claimLocked claims the prompt slot and makes the turn current, and enqueues
// nothing: a settlement places its successor's started in its own batch.
//
// Begin comes first, so a session double that panics in it leaves the engine as
// it found it; callers hold e.mu through a deferred unlock for the same reason.
func (e *Engine) claimLocked(text, origin, cause string) launch {
	run := e.sess.Begin(text)
	e.turnSeq++
	t := &turn{id: fmt.Sprintf("turn-%d", e.turnSeq), text: text, origin: origin, cause: cause}
	e.cur = t
	e.activity = ActivityWorking
	e.err = ""
	e.cancelled = false
	e.prompted = true
	return e.launchLocked(t, run, true)
}

// launchLocked counts a continuation the engine now owes a run. It is counted
// here, under e.mu, and not where its goroutine starts: Close sets closed under
// the same lock before it waits, so a claim is either counted before that wait
// begins or never made, and the count is never raised beside a Wait.
func (e *Engine) launchLocked(t *turn, run func(context.Context) (agent.Result, error), barrier bool) launch {
	e.wg.Add(1)
	return launch{t: t, run: run, barrier: barrier}
}

func (e *Engine) startedEvent(t *turn) agent.Event {
	return e.stamp(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
		ID: t.id, Phase: agent.TurnStarted, Text: t.text, Origin: t.origin,
	}}, t.cause)
}

// stamp gives an engine-authored event its time, from the session's clock, and
// its cause. Such an event is never Replayed: the engine refuses every command
// while a replay runs.
func (e *Engine) stamp(ev agent.Event, cause string) agent.Event {
	ev.At = e.now()
	ev.Cause = cause
	return ev
}

// run starts the continuations a locked section claimed, with the lock
// released. Each was counted when it was claimed (launchLocked).
func (e *Engine) run(ls []launch) {
	for _, l := range ls {
		go e.runTurn(l)
	}
}

// runTurn runs one continuation and hands its result to the settlement.
//
// The Flush is an ordering nicety, not a condition: it puts the turn's started
// in the primary's buffer before the agent can say anything, and a continuation
// is run exactly once whatever it returns — Begin's contract — so on a closing
// log the turn simply goes ahead and comes straight back.
func (e *Engine) runTurn(l launch) {
	defer e.wg.Done()
	if l.barrier {
		_ = e.log.Flush(context.Background())
	}
	res, err := l.run(context.Background())
	var next []launch
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		l.t.res, l.t.err, l.t.returned = res, err, true
		next = e.passLocked()
	}()
	e.run(next)
	if h := e.hooks; h != nil && h.turnReturned != nil {
		h.turnReturned(l.t.id)
	}
}

// drive is the driver's loop: on every wake-up, try to settle, else try to
// drain. Its passes are level-triggered — each re-reads the state under e.mu,
// even when the last one found nothing to do — because a wake-up says only that
// something may have changed: the observer saw a foreign turn end, a
// continuation came back, a cancel released its hold. A missed re-check here is
// a queue that never drains with no error anywhere.
func (e *Engine) drive() {
	defer e.wg.Done()
	var tick <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-e.kick:
		case <-tick:
		case <-e.done:
			return
		}
		var next []launch
		retrying := false
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			next = e.passLocked()
			retrying = e.cur != nil && e.cur.retry
		}()
		e.run(next)
		tick = nil
		if retrying {
			if timer == nil {
				timer = time.NewTimer(foreignRetryTick)
			} else {
				timer.Reset(foreignRetryTick)
			}
			tick = timer.C
		}
	}
}

// passLocked is one pass of the driver: settle the current turn if it can be
// settled, take a refused claim again if it can be, else drain. It returns the
// continuations it claimed, for the caller to run once e.mu is released.
func (e *Engine) passLocked() []launch {
	if e.closed {
		return nil
	}
	if t := e.cur; t != nil {
		switch {
		case t.retry:
			return e.retryLocked(t)
		case t.returned && e.cancelsInFlight == 0:
			return e.settleLocked(t)
		}
		return nil
	}
	return e.drainLocked()
}

// retryLocked takes again the claim of a turn the session refused because the
// agent was running one of its own. Stop ends the wait: the turn then settles
// as the refusal it was.
func (e *Engine) retryLocked(t *turn) []launch {
	if e.stopped {
		t.retry = false
		if e.cancelsInFlight > 0 {
			return nil
		}
		return e.settleLocked(t)
	}
	if e.cancelsInFlight > 0 || e.sess.ForeignTurn() {
		return nil
	}
	t.retry, t.returned = false, false
	return []launch{e.launchLocked(t, e.sess.Begin(t.text), false)}
}

// drainLocked starts the head of the queue when nothing is current and a turn
// may start. Nothing drains from an error: the turn that failed said so, and
// only the next send clears it.
//
// It does not ask the outbox for room. A drain that declined would have nobody
// to try it again — no wake-up is owed for room coming back — and it is bounded
// by state as a completion is: two events a row, and the queue has a cap.
func (e *Engine) drainLocked() []launch {
	if e.activity != ActivityIdle || e.queue.Len() == 0 || !e.canStartLocked() {
		return nil
	}
	qev, ok := e.queue.Pop()
	if !ok {
		return nil
	}
	return []launch{e.reserveLocked(qev.Prompt.Text, agent.TurnOriginDrain, "", []agent.Event{e.stamp(qev.Event(), "")})}
}

// settleLocked ends the current turn. It runs when the turn's continuation has
// returned and no cancel is in flight, whichever happens last, so a cancel
// never overlaps the decision about what runs next, and a client never sees an
// ended with an empty Next that a moment later turns out to have had a
// successor.
//
// It is one transaction. In order: the chain policy decides what becomes of the
// queue; the successor, if there is one, is taken off the queue and claimed;
// the activity is set; and one batch is enqueued — the queue's events, then the
// turn's ended with Next and Pending, then the successor's started. So an ended
// is always preceded by everything its settlement produced and followed only by
// its successor, and ended{Next: "", Pending: 0} is the last event a chain
// produces. The batch is a mandatory completion: it is enqueued whether or not
// the outbox reports room, because a turn that has ended has ended.
func (e *Engine) settleLocked(t *turn) []launch {
	if e.opts.Chain.RetryForeignTurn && !e.stopped && errors.Is(t.err, agent.ErrForeignTurn) {
		// Not an ending: the agent has the session for a turn of its own, and
		// this client waits that out. The turn stays current and working, and
		// the driver is woken so that it arms its tick: nothing else may ever
		// say the foreign turn is over.
		t.retry = true
		e.wake()
		return nil
	}
	info := &agent.TurnInfo{ID: t.id, Phase: agent.TurnEnded}
	refused := errors.Is(t.err, agent.ErrPromptInFlight) || errors.Is(t.err, agent.ErrForeignTurn)
	failed := false
	switch {
	case t.err == nil:
		info.StopReason = t.res.StopReason
	case errors.Is(t.err, agent.ErrPromptCancelled):
		// Cancelled before its turn opened: nothing ran, nothing failed, and
		// no event of any kind is coming from the session. It is the ending a
		// cancelled turn has.
		info.Synthetic = true
		info.StopReason = stopCancelled
	default:
		// A refusal emits nothing either, so its ending is authored here; any
		// other failure was published by the session as its EventError, which
		// this follows.
		failed = true
		info.Synthetic = refused
		info.Err = t.err.Error()
		info.ErrClass = agent.ClassifyEventErr(t.err)
	}
	e.cancelled = info.StopReason == stopCancelled

	var batch []agent.Event
	clear := func() {
		for _, qev := range e.queue.Clear() {
			batch = append(batch, e.stamp(qev.Event(), ""))
		}
	}
	e.cur = nil
	var next []launch
	switch {
	case failed:
		e.activity = ActivityError
		e.err = info.Err
		if !refused {
			// Nothing drains from an error state, and a queue that outlived
			// one would run behind whatever is sent next. A refusal reached
			// nothing, and leaves the queue as it was.
			clear()
		}
	case e.stopped, e.opts.Chain.StopOnNonEndTurn && info.StopReason != stopEndTurn:
		e.activity = ActivityIdle
		clear()
	default:
		e.activity = ActivityIdle
		if e.queue.Len() > 0 && e.canStartLocked() {
			if qev, ok := e.queue.Pop(); ok {
				batch = append(batch, e.stamp(qev.Event(), ""))
				// The claim, not the enqueue: the successor's started goes out
				// in this batch, after the ending.
				l := e.claimLocked(qev.Prompt.Text, agent.TurnOriginDrain, "")
				info.Next = l.t.id
				next = append(next, l)
			}
		}
	}
	info.Pending = e.queue.Len()
	batch = append(batch, e.stamp(agent.Event{Type: agent.EventTurn, Turn: info}, t.cause))
	for _, l := range next {
		batch = append(batch, e.startedEvent(l.t))
	}
	e.log.Enqueue(batch...)
	return next
}

const (
	stopEndTurn   = "end_turn"
	stopCancelled = "cancelled"
)
