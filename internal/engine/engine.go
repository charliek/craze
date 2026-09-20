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
	// out. The client owns the budget: it reads State.Waiting and calls GiveUp
	// when it has waited long enough, and the turn then ends as the refusal it
	// was. The TUI leaves it false and shows the refusal.
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
	// current, working, and has no continuation running. retryDue says a tick
	// has passed since that refusal, and only then is the claim taken: a wake-up
	// alone never authorises it, or a session whose flag is already clear while
	// its client still refuses would be claimed again as fast as it could refuse.
	retry    bool
	retryDue bool
	// givenUp says the client that owns the wait has stopped waiting (GiveUp):
	// the turn is to settle as the refusal it was rather than be claimed again.
	// It is what stops the settlement from parking the turn a second time, and it
	// is per turn, because the budget is.
	givenUp bool
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
// admission, the message queue and its verbs, the queue's removal-plus-claim,
// send-now, prompt completion with the endings the wire never produced, and
// cancel against a turn id. Clients submit commands and observe events
// (session control SD-24); two of them cannot double-drain, and a session with
// none drains itself.
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

	mu       sync.Mutex
	activity Activity
	cur      *turn
	// settled remembers what each recently settled turn's continuation returned,
	// for the in-process client that has to hand a caller the failure itself and
	// not a rendering of it (TurnErr). Only a failure is kept — a clean turn's
	// answer is nil either way — and only the last settledCap of them, because a
	// client reads endings with the outbox's lag and may ask about a turn the
	// engine has already succeeded twice over.
	settled         map[string]error
	settledIDs      []string
	turnSeq         int
	clientSeq       int
	queue           agent.PromptQueue
	armed           *armedSend
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
	// return allowed is over, with e.mu released and the goroutine's count
	// given back, so a hook may close the engine.
	turnReturned func(turn string)
	// retryTick, when set, stands in for the driver's timer: a refused claim is
	// taken again when the test sends on it, and at no other time.
	retryTick <-chan time.Time
}

// New builds the engine for sess. The session must own an event log
// (agent.LogOwner), because the engine publishes through it and observes it;
// New installs the log's single observer, so a second engine on one session is
// refused with agent.ErrObserverSet. It is called before sess.Start.
func New(sess agent.Session, opts Options) (*Engine, error) {
	return newEngine(sess, opts, nil)
}

// newEngine is New with a test's hooks, which have to be in place before the
// driver's goroutine exists to read them.
func newEngine(sess agent.Session, opts Options, h *hooks) (*Engine, error) {
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
		hooks:    h,
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

// Session is the session the engine wraps: the raw provider seam, held by a
// client only for what has not moved behind Control yet. Everything else goes
// through Control, so that one component decides what runs and two clients cannot
// contradict each other.
//
// What is still reached through here, and what takes each away:
//
//   - AnswerPermission, AnswerQuestion, AnswerPlan — until the ask registry gives
//     Control its Answer and Asks (plan 021 C8b);
//   - SetModel, SetMode, SetConfig, SetTitle — until settings become state deltas
//     in order behind Control.Set and Control.SetTitle (C10);
//   - Snapshot, which the TUI reads through Control.State() already, and which a
//     caller that has no engine state to merge may still read here;
//   - Events, which is the log's primary and therefore the very channel
//     Control.Events() hands out: a helper written against a session — headless
//     craze's final sweeps — reads the same stream through either.
//
// The TUI additionally keeps writing the session index itself, from the snapshot
// this hands it, until the index moves into the engine (C12). Nothing else may go
// around Control: no Begin, no Cancel, no queue verb, no Interject.
func (e *Engine) Session() agent.Session { return e.sess }

// Start starts the session. Until it returns the engine refuses every
// command; afterwards it is idle, or failed with StartFailed set.
func (e *Engine) Start(ctx context.Context) error {
	err := e.sess.Start(ctx)
	e.Started(err)
	return err
}

// Started opens the gate: the session is up, and commands are admitted from
// here. err is what starting came to; a failure leaves the engine in its error
// state with StartFailed set, exactly as a failed Start does. It waits on
// nothing.
//
// Start calls it with whatever sess.Start returned, and it is exported because
// starting is not always the engine's own call — a client can learn that the
// session is up another way and has to be able to say so. The TUI is one: its
// start command calls Start on a goroutine of bubbletea's and reports back as a
// message, and the model that handles that message is where the session being up
// becomes true for everything the model then admits. So in an ordinary run BOTH
// reach here, and the two properties that makes necessary are:
//
//   - It is idempotent. The gate only ever opens, and only from
//     ActivityStarting, so a second call is a no-op whatever the engine is doing
//     by then: a working turn is not put back to idle, and a start that failed is
//     not talked out of its error state by a later Started(nil).
//   - It never reopens a gate that has closed. Close is checked here; Stop sets
//     stopped, which refusalLocked reads whatever the activity is; and a client
//     that has moved on to another engine must not reach this one at all, which
//     is the caller's to get right — the TUI names the engine on the message its
//     start command reports with, and ignores one for an engine it no longer
//     holds (app.go's startedMsg and staleFor).
func (e *Engine) Started(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.activity != ActivityStarting {
		return
	}
	if err != nil {
		e.activity = ActivityError
		e.startFailed = true
		e.err = err.Error()
		return
	}
	e.activity = ActivityIdle
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
// still holds — and joins the engine's goroutines, the cancel an armed send-now
// asked for included. It is idempotent.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.activity = ActivityClosing
		// An armed send will never fire now, and the record says so before the
		// log stops taking work: a mandatory completion, like every other
		// ending shutdown authors.
		if ev, ok := e.disarmLocked(agent.SendNowClosing, "", ""); ok {
			e.log.Enqueue(ev)
		}
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

// canSubmitLocked is canStartLocked for a prompt a client submitted directly,
// and differs in one term: under a chain policy that waits a foreign-turn
// refusal out, the session's flag is not a gate.
//
// The gate exists so that a client which would have to show a refusal is not
// handed one it could have avoided (the TUI queues such a prompt instead, plan
// 021 X22). A client whose policy is "keep the turn current and claim it again"
// has asked for the opposite: `craze prompt` has always sent its prompt into a
// session the agent was holding and waited the refusal out, and a queued row in
// its place would be a message that run never sends and a pair of lines its JSON
// never had.
//
// The drain keeps the gate whatever the policy: a row must not leave the queue
// for a turn that cannot start — its sent line would precede the end of the
// foreign turn it is waiting for — and the queue is exactly where it is safe.
func (e *Engine) canSubmitLocked() bool {
	if e.opts.Chain.RetryForeignTurn {
		return e.cur == nil && e.cancelsInFlight == 0 && e.refusalLocked() == nil
	}
	return e.canStartLocked()
}

// Submit admits a prompt: it starts a turn now, queues the text, or — in
// send-now mode behind a running turn — arms the send and cancels that turn.
//
// Starting is one section under e.mu: the gate, the room in the outbox, the row
// the text came from leaving the queue, the turn's id, working, its started
// event, and the session's Begin. The claim is therefore taken on the caller's
// goroutine — the Update that shows the turn working — so an Esc handled by the
// next Update cancels this prompt, which withdraws without reaching the agent,
// rather than finding no turn and writing its cancel ahead of the prompt.
// Reservation and claim being atomic is also why Submit never has to forward a
// cancel itself: there is no moment at which a turn is reserved and not
// claimed. It waits on nothing.
//
// fromRow names a queued row the prompt is: the row is taken (QueueSent), its
// own text is what goes — the text argument is then only what a client believed
// the row held — and its removal is enqueued *before* the started, so the band
// shrinks before the user row appears, as it always has. A row that is not
// there is an error that mutates nothing — the client's view of the queue is
// stale — and never text the engine invented instead.
func (e *Engine) Submit(c Command, text string, mode SubmitMode, fromRow string) (SubmitResult, error) {
	switch mode {
	case SubmitQueue, SubmitSendNow:
	default:
		return SubmitResult{}, fmt.Errorf("%w: submit mode %q", ErrBadRequest, mode)
	}
	var started []launch
	// armedTurn is the turn an arm asked to have cancelled, carried out of the
	// locked section so the cancel itself is made with the lock released.
	armedTurn, armedCause := "", ""
	res, err := func() (SubmitResult, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.refusalLocked(); err != nil {
			return SubmitResult{}, err
		}
		if !e.log.OutboxRoom() {
			return SubmitResult{}, ErrUnavailable
		}
		if mode == SubmitSendNow {
			if e.armed != nil {
				// One at a time: two turns cannot both be the one that
				// replaces this.
				return SubmitResult{}, ErrAlreadyPending
			}
			if e.sess.ForeignTurn() {
				// 05's gate table: a foreign turn overlays every activity and
				// nothing can be sent into it, so there is no turn of craze's
				// own for a send-now to take the place of.
				return SubmitResult{}, agent.ErrForeignTurn
			}
		}
		// A direct submit is what clears an error: the turn that failed said
		// so, and only the next send retires it.
		if e.canSubmitLocked() {
			origin := agent.TurnOriginSubmit
			if mode == SubmitSendNow {
				origin = agent.TurnOriginSendNow
			}
			// The row is read and validated here and taken only after the claim,
			// because Begin can panic in a test double and a caller may recover
			// from it: a row taken first would be gone from a queue whose event
			// stream still shows it waiting.
			var take func() []agent.Event
			if fromRow != "" {
				row, ok := e.rowLocked(fromRow)
				if !ok {
					return SubmitResult{}, fmt.Errorf("%w: %s", ErrUnknownRow, fromRow)
				}
				text = row.Text
				take = func() []agent.Event { return e.takeRowLocked(fromRow, c.Cause()) }
			}
			l := e.reserveLocked(text, origin, c.Cause(), take)
			started = append(started, l)
			return SubmitResult{Turn: l.t.id}, nil
		}
		if mode == SubmitSendNow {
			res, turn, err := e.armLocked(c, text, fromRow)
			armedTurn, armedCause = turn, c.Cause()
			return res, err
		}
		if fromRow != "" {
			// A row that cannot start now is already where it belongs: queue
			// mode is "start now, else queue", and it is queued. Nothing is
			// mutated and nothing is said, and the answer is the row itself,
			// still waiting.
			row, ok := e.rowLocked(fromRow)
			if !ok {
				return SubmitResult{}, fmt.Errorf("%w: %s", ErrUnknownRow, fromRow)
			}
			return SubmitResult{Queued: &row}, nil
		}
		row, qev, err := e.queue.Add(text, e.now())
		if err != nil {
			return SubmitResult{}, err
		}
		e.log.Enqueue(e.stamp(qev.Event(), c.Cause()))
		return SubmitResult{Queued: &row}, nil
	}()
	e.run(started)
	if armedTurn != "" {
		go e.cancelArmed(armedTurn, armedCause)
	}
	return res, err
}

// takeRowLocked takes a row that has already been validated in this same
// section out of the queue, and returns the event that says it has gone. The row
// is there — nothing else can touch the queue under e.mu — so there is nothing
// to report but the event.
func (e *Engine) takeRowLocked(id, cause string) []agent.Event {
	qev, ok := e.queue.Take(id)
	if !ok {
		// Unreachable: the row was read under this same lock.
		return nil
	}
	return []agent.Event{e.stamp(qev.Event(), cause)}
}

// rowLocked is the queued row id names, and whether it is there at all. It
// mutates nothing, which is what a verb that has to validate before it changes
// anything needs — and what lets every path claim its turn before it takes the
// row, so a Begin that panics leaves the row where the stream says it is.
func (e *Engine) rowLocked(id string) (agent.QueuedPrompt, bool) {
	for _, row := range e.queue.List() {
		if row.ID == id {
			return row, true
		}
	}
	return agent.QueuedPrompt{}, false
}

// headLocked is the row at the head of the queue, and whether there is one. Like
// rowLocked it mutates nothing: the drain reads the head, claims its turn, and
// only then pops it, which under this lock can only be the same row.
func (e *Engine) headLocked() (agent.QueuedPrompt, bool) {
	rows := e.queue.List()
	if len(rows) == 0 {
		return agent.QueuedPrompt{}, false
	}
	return rows[0], true
}

// reserveLocked starts a turn: it claims it, then runs take — which frees the
// text from the row it came from, if it came from one — and enqueues that row's
// removal followed by the turn's started, as one batch. The band shrinks before
// the user row appears, as it always has.
//
// take runs after the claim and not before, because claiming calls Begin, which
// a session double can panic in: a row taken first would be gone from a queue
// whose stream still shows it waiting.
func (e *Engine) reserveLocked(text, origin, cause string, take func() []agent.Event) launch {
	l := e.claimLocked(text, origin, cause)
	var before []agent.Event
	if take != nil {
		before = take()
	}
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
	// Deferred first, so it runs last: after the count has been given back.
	defer func() {
		if h := e.hooks; h != nil && h.turnReturned != nil {
			h.turnReturned(l.t.id)
		}
	}()
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
		ticked := false
		select {
		case <-e.kick:
		case <-tick:
			ticked = true
		case <-e.done:
			return
		}
		var next []launch
		retrying := false
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if ticked && e.cur != nil && e.cur.retry {
				// Only a tick makes a refused claim due. A kick says the state
				// may have changed; it does not say time has passed, and a
				// refusal with nothing else to wait for is paced by time alone.
				e.cur.retryDue = true
			}
			next = e.passLocked()
			retrying = e.cur != nil && e.cur.retry
		}()
		e.run(next)
		tick = nil
		if !retrying {
			continue
		}
		if h := e.hooks; h != nil && h.retryTick != nil {
			tick = h.retryTick
			continue
		}
		if timer == nil {
			timer = time.NewTimer(foreignRetryTick)
		} else {
			timer.Reset(foreignRetryTick)
		}
		tick = timer.C
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
// agent was running one of its own. Stop and GiveUp end the wait: the turn then
// settles as the refusal it was.
func (e *Engine) retryLocked(t *turn) []launch {
	if e.stopped {
		t.retry = false
		if e.cancelsInFlight > 0 {
			return nil
		}
		return e.settleLocked(t)
	}
	if !t.retryDue || e.cancelsInFlight > 0 || e.sess.ForeignTurn() {
		return nil
	}
	t.retry, t.retryDue, t.returned = false, false, false
	return []launch{e.launchLocked(t, e.sess.Begin(t.text), false)}
}

// GiveUp ends the engine's wait on a turn whose claim the session keeps refusing
// because the agent is running one of its own: the turn settles NOW as the
// refusal it was, through the ordinary settlement, so the chain policy, the
// batch's shape and the send-now handling are the same code as for any other
// ending.
//
// It is the client's half of the foreign-turn wait. The engine claims again for
// as long as the client lets it and authors no event for a refusal (§3.4); the
// budget is the client's, and this is how it spends it. `craze prompt` is the one
// client that has a budget: the TUI's policy never parks a refusal at all.
//
// It is conditional and atomic, which is the whole point of it being a command of
// the engine's rather than a cancel the client writes:
//
//   - turn must name the current turn, or it is ErrStaleTurn — the client decided
//     to give up on one turn and must not end another;
//   - that turn must still be waiting (State.Waiting), or it is ErrNotAccepting
//     and NOTHING changes. A client decides from an observation, and by the time
//     it acts the claim may have gone through: the prompt is then running, and a
//     give-up that cancelled it would kill the very turn the wait was for.
//
// It makes no session call and waits on nothing — a waiting turn has no
// continuation in flight — so a client may call it from the primary's own reader.
// A cancel in flight holds the settlement exactly as it holds every other one:
// the give-up is committed, and the ending follows when the hold is released.
//
// The Command is not the ending's cause: the ending belongs to the turn and
// carries the cause the turn was submitted with, as every other ending does.
func (e *Engine) GiveUp(_ Command, turn string) error {
	var next []launch
	err := func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.closed {
			return ErrNotAccepting
		}
		if turn == "" || e.cur == nil || e.cur.id != turn {
			return ErrStaleTurn
		}
		t := e.cur
		if !t.retry {
			// Running, or a claim of its own is in flight: either way this turn is
			// not waiting for anything the client can give up on.
			return ErrNotAccepting
		}
		t.givenUp, t.retry, t.retryDue = true, false, false
		if e.cancelsInFlight > 0 {
			return nil
		}
		next = e.settleLocked(t)
		return nil
	}()
	e.run(next)
	return err
}

// GiveUpDrain ends a client's wait for rows held behind a turn of the agent's
// own, one way or the other, in one section: the driver's pass is made here and
// now, and either a turn is current when it returns — the drain's own, or one
// that was already running — or the engine stops admitting, so that nothing can
// start behind a client that is about to leave.
//
// It exists because a client cannot decide this from outside. The drain is the
// driver's, and the driver runs when it is woken: a client whose budget runs out
// in the instant the agent's turn ends reads a state in which the session's flag
// is down, nothing is current and the rows are still queued, and cannot tell a
// queue that is about to drain from one that never will. A grace period does not
// settle it — the scheduler owes the driver no deadline, and the flag a client
// read a moment ago can be stale in either direction. `craze prompt` never had
// the question, because it took the row itself, synchronously, the moment the
// flag was down. This gives it the same thing: the session's flag is read in the
// section that would claim, as it is for every other admission.
//
// turn is the current turn when the call returns, and "" means the drain is
// abandoned: pending says how many rows that leaves queued, which is the
// client's to report. Abandoning clears nothing and cancels nothing — the rows
// stay where they are, as they did for a run that gave up on them — it only sets
// the gate Stop sets, so the abandonment cannot be overtaken by the very drain it
// gave up on. It makes no blocking call: Begin is the one thing it may call on
// the session, under e.mu like every other claim.
func (e *Engine) GiveUpDrain(_ Command) (turn string, pending int, err error) {
	var next []launch
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.closed {
			err = ErrNotAccepting
			return
		}
		if e.cur == nil {
			next = e.passLocked()
		}
		if e.cur != nil {
			turn = e.cur.id
			return
		}
		e.stopped = true
		pending = e.queue.Len()
		// A send armed and waiting for the same drain goes with it, and says so,
		// as it does for Stop: nothing will fire it now.
		if ev, ok := e.disarmLocked(agent.SendNowStopped, "", ""); ok {
			e.log.Enqueue(ev)
		}
	}()
	e.run(next)
	return turn, pending, err
}

// drainLocked starts what is waiting when nothing is current and a turn may
// start: an armed send-now first, then the head of the queue. It is the pass
// that runs when a settlement could not start anything — a foreign turn held it
// — and the end of that foreign turn releases it.
//
// It does not ask the outbox for room. A drain that declined would have nobody
// to try it again — no wake-up is owed for room coming back — and it is bounded
// by state as a completion is: two events a row, and the queue has a cap.
func (e *Engine) drainLocked() []launch {
	l, before, disarm := e.nextLocked(true)
	// The disarm comes first: a send whose row had already gone is why the
	// head of the queue is running in its place.
	batch := append(disarm, before...)
	if l != nil {
		batch = append(batch, e.startedEvent(l.t))
	}
	e.log.Enqueue(batch...)
	if l == nil {
		return nil
	}
	return []launch{*l}
}

// nextLocked decides what runs next, from a settlement or from the drain, and
// claims it: the armed send-now first, then — if queueMayRun — the head of the
// queue. The armed send goes ahead of everything queued because it is what a
// client cancelled a running turn for (A10).
//
// queueMayRun is false where a settlement's chain policy is about to clear the
// follow-ups behind the turn: the head is then not a candidate, but the armed
// send still is, and its row is taken here, before that clear, so the policy
// cannot delete the very text the send is about. An armed send is not a
// follow-up — it is what a client asked for *instead of* the turn — which is why
// one survives a policy the other does not.
//
// It returns the continuation it claimed, the queue events that freed its text,
// and the disarm delta for an armed send whose row had already left the queue:
// that send falls through with a reason and the head takes its place, as the
// TUI's own drain always did.
//
// Nothing runs from an error state — the turn that failed said so, and only the
// next direct submit retires it — nor while the agent is running a turn of its
// own, nor while a cancel is on its way to the session.
func (e *Engine) nextLocked(queueMayRun bool) (*launch, []agent.Event, []agent.Event) {
	if e.activity != ActivityIdle || !e.canStartLocked() {
		return nil, nil, nil
	}
	var before, disarm []agent.Event
	if a := e.armed; a != nil {
		text, from := a.text, agent.QueuedPrompt{}
		ok := true
		if a.from != "" {
			// Read now, taken after the claim, and never before: the row's text
			// as it stands — an edit while the send was armed is what goes — and
			// the queue is left alone until Begin has been through.
			from, ok = e.rowLocked(a.from)
			text = from.Text
		}
		if ok {
			// Firing consumes the arm with no delta of its own: the started it
			// claims here, with its send_now origin, is what says the send has
			// gone. A delta is what the paths that lose one owe.
			e.armed = nil
			l := e.claimLocked(text, agent.TurnOriginSendNow, a.cause)
			if a.from != "" {
				// Here and nowhere earlier, so the row is taken exactly once and
				// a send that never fired lost nothing.
				before = append(before, e.takeRowLocked(a.from, a.cause)...)
			}
			return &l, before, nil
		}
		if ev, ok := e.disarmLocked(agent.SendNowRowGone, "", ""); ok {
			disarm = append(disarm, ev)
		}
	}
	if !queueMayRun {
		return nil, before, disarm
	}
	if head, ok := e.headLocked(); ok {
		l := e.claimLocked(head.Text, agent.TurnOriginDrain, "")
		if qev, popped := e.queue.Pop(); popped {
			before = append(before, e.stamp(qev.Event(), ""))
		}
		return &l, before, disarm
	}
	return nil, before, disarm
}

// settleLocked ends the current turn. It runs when the turn's continuation has
// returned and no cancel is in flight, whichever happens last, so a cancel
// never overlaps the decision about what runs next, and a client never sees an
// ended with an empty Next that a moment later turns out to have had a
// successor.
//
// It is one transaction. In order: the steers the turn accepted and could not
// answer go back to the head of the queue; the armed send-now is decided, and
// disarmed here if this turn is not one it can fire after; the activity is set
// and the chain policy's verdict on the queue is taken; the successor, if there
// is one — the armed send first, else the head of the queue — is claimed and its
// row taken; *then* the policy's clear runs, so it can never take the row the
// armed send just claimed with it; and one batch is enqueued — the queue's
// events, then the turn's ended with Next and Pending, then the disarm delta if
// there is one, then the successor's started. So an ended is always preceded by
// everything its settlement produced and followed only by its successor, and
// ended{Next: "", Pending: 0} is the last event a chain produces. The batch is a
// mandatory completion: it is enqueued whether or not the outbox reports room,
// because a turn that has ended has ended.
func (e *Engine) settleLocked(t *turn) []launch {
	if e.opts.Chain.RetryForeignTurn && !e.stopped && !t.givenUp && errors.Is(t.err, agent.ErrForeignTurn) {
		// Not an ending: the agent has the session for a turn of its own, and
		// this client waits that out. The turn stays current and working, and
		// the driver is woken so that it arms its tick: nothing else may ever
		// say the foreign turn is over.
		t.retry = true
		e.wake()
		return nil
	}
	var batch []agent.Event
	// The steers the turn accepted and could not answer go back first: ahead of
	// everything already queued, and ahead of the successor decision, so
	// interjected text runs before whatever was waiting behind the turn that
	// took it. Last first, so the rows end up in the order they were typed and
	// a consumer replaying the events — each an insert at position 0 — lands on
	// the same order. PushFront is bounded by neither cap: the text is already
	// the user's, accepted by a turn, and a cap must not be the reason it
	// vanishes when it has nowhere else to go (§3.5).
	for i := len(t.res.Unanswered) - 1; i >= 0; i-- {
		batch = append(batch, e.stamp(e.queue.PushFront(t.res.Unanswered[i], e.now()).Event(), ""))
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
	e.rememberErrLocked(t)

	clear := func() {
		for _, qev := range e.queue.Clear() {
			batch = append(batch, e.stamp(qev.Event(), ""))
		}
	}
	e.cur = nil

	// The armed send-now, decided before the queue: this settlement either
	// fires it below or takes it with the turn.
	var disarm []agent.Event
	if a := e.armed; a != nil {
		reason := ""
		switch {
		case a.turn != t.id:
			// It was armed against a turn that is no longer the one that just
			// ended — a direct submit started another one in between — so it is
			// not this turn's business.
			reason = agent.SendNowOtherTurn
		case failed:
			// Nothing runs from an error state, and the queue goes with it, so a
			// send waiting on this turn is waiting for nothing.
			reason = agent.SendNowTurnFailed
		}
		if reason != "" {
			if ev, ok := e.disarmLocked(reason, "", ""); ok {
				disarm = append(disarm, ev)
			}
		}
	}

	// What the chain policy makes of the queue behind the turn, and whether the
	// queue may supply a successor at all.
	chainClears := false
	switch {
	case failed:
		e.activity = ActivityError
		e.err = info.Err
		// Nothing drains from an error state, and a queue that outlived one
		// would run behind whatever is sent next. A refusal reached nothing,
		// and leaves the queue as it was. A send armed against this turn was
		// disarmed above, and the row it named is cleared with the rest: that
		// is the one path where an armed send's text does not survive, and it
		// is deliberate — the error took the whole queue with it.
		chainClears = !refused
	case e.stopped, e.opts.Chain.StopOnNonEndTurn && info.StopReason != stopEndTurn:
		e.activity = ActivityIdle
		chainClears = true
	default:
		e.activity = ActivityIdle
	}
	// The successor, from the same decision the drain makes: an armed send-now
	// first, then the head of the queue. It runs *before* the clear and is told
	// whether the queue may supply a successor, so that a policy which ends the
	// chain still cannot delete the row an armed send is about: the send takes
	// its row here, and the ordinary follow-ups are cleared below. nextLocked
	// starts nothing from an error state, so the failed branch has already ruled
	// everything out there.
	var next []launch
	l, before, rowGone := e.nextLocked(!chainClears)
	batch = append(batch, before...)
	disarm = append(disarm, rowGone...)
	if l != nil {
		info.Next = l.t.id
		next = append(next, *l)
	}
	if chainClears {
		clear()
	}

	info.Pending = e.queue.Len()
	batch = append(batch, e.stamp(agent.Event{Type: agent.EventTurn, Turn: info}, t.cause))
	batch = append(batch, disarm...)
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

// settledCap is how many settled turns' failures TurnErr remembers.
const settledCap = 16

// rememberErrLocked keeps what t's continuation returned, for TurnErr. A clean
// turn is not kept: nil is the answer either way, and the table stays the size of
// the failures it is for.
func (e *Engine) rememberErrLocked(t *turn) {
	if t.err == nil {
		return
	}
	if e.settled == nil {
		e.settled = map[string]error{}
	}
	e.settled[t.id] = t.err
	e.settledIDs = append(e.settledIDs, t.id)
	if len(e.settledIDs) > settledCap {
		delete(e.settled, e.settledIDs[0])
		e.settledIDs = e.settledIDs[1:]
	}
}

// TurnErr is the error the named turn's continuation returned: the value, not
// TurnInfo.Err's rendering of it. It is nil for a turn that ended cleanly, for
// one that has not settled, and for one the engine no longer remembers
// (settledCap).
//
// It exists because an in-process client has callers of its own to answer:
// `craze prompt` returns the failure of a turn to whatever ran it, and a caller
// that matches on a sentinel — errors.Is, a wrapped wire error, the
// ErrPromptCancelled a withdrawn prompt returns — needs the error and not a
// rendering. It is deliberately NOT on the event: an event enqueued through the
// log's outbox may not carry an Err at all (eventlog.go's Enqueue replaces one,
// because encoding it would call arbitrary code under a caller's lock), and a
// synthetic ending has no live error value to carry in the first place.
//
// So this is an in-process convenience that a socket client will not have: over a
// socket, TurnInfo's ErrClass and Err text are the whole of what a failure is.
func (e *Engine) TurnErr(turn string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settled[turn]
}
