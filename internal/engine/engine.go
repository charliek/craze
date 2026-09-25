package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/transcript"
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

// Options configure an Engine. The zero value is a TUI's chain policy and no
// session index.
type Options struct {
	Chain ChainPolicy
	// Index is the session index and what writing a row needs that the engine
	// cannot know (index.go). A zero IndexOptions — which is what `craze
	// prompt`, every TUI unit test and every golden leave it — writes nothing
	// at all.
	Index IndexOptions
	// CrazeSessionID is the durable craze session id this session already has:
	// the crazeId of the row a --continue or a --resume loaded, carried back in
	// so that one thread of work keeps one identity across every agent session
	// it is loaded into (session control SD-22). Empty mints a fresh UUIDv7,
	// which is what a new session and a row written before crazeId existed both
	// want.
	CrazeSessionID string
	// ReceiptClock is the command-id table's clock (receipts.go): how long a
	// result is answerable and how long a released client waits before it can
	// be retired are measured on it. nil is time.Now, which is what every host
	// runs on. It is the seam a test outside this package moves past the
	// table's age bound with, instead of waiting ten minutes — the socket
	// server's client-lifecycle tests (plan 027 §3.6, A12).
	ReceiptClock func() time.Time
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
	// row is the queued row the turn's text came from, as it stood when it was
	// taken — the head a drain or a settlement's successor popped, the row a
	// Submit named, the row an armed send re-took — and nil for text a client
	// held itself. A refusal by the agent's own turn puts it back as it was
	// (restoreLocked, SF-21).
	row *agent.QueuedPrompt
	// cancelAsked says a cancel was validated against this turn — Cancel, Stop,
	// or the cancel an armed send-now asked for (holdCancelLocked). A row-sourced
	// turn so marked that the agent's own turn refused ends as cancelled and does
	// not put its row back: the user asked for it to stop (cancelledRefusalLocked).
	cancelAsked bool
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
// file I/O — and the engine calls exactly three things on the session while
// holding it, all of which take the session's own mutex briefly and wait on
// nothing: Begin, the leaf accessor ForeignTurn, and — for a session that has
// one — the admission fence's FenceUp and FenceDown (agent.AdmissionFence),
// which record a state and never call back. So the order is e.mu → s.mu, and it
// cannot cycle, because a session never calls the engine: it reports through
// Result, through events, and through the log's observer.
//
// # The admission fence
//
// A session that can start a turn of its own — native's wake — must not start
// one in the gap between the engine reading ForeignTurn and calling Begin, nor
// while a cancel the engine validated is still on its way to the session: the
// cancel promises to land on the turn it was validated for or on none (cancel.go),
// and a turn the session started in that gap would be the one it landed on. The
// engine's own admissions are fenced by e.mu and the hold; the session's are
// fenced by agent.AdmissionFence, which the engine keeps up whenever it is not
// idle (fenceWantLocked).
//
// Every section that reads ForeignTurn, calls Begin or validates a cancel raises
// it first (raiseFenceLocked) and brings it to what the engine now is on the way
// out (syncFenceLocked, deferred after the unlock is, so it runs first and still
// under e.mu: a Begin that panics and every early return leave it balanced).
// The call sites and the sections that reach them are these — a grep for
// "e.sess.ForeignTurn()" and "e.sess.Begin(" in this package lists every site,
// and TestEveryForeignReadAndClaimIsFenced holds the code to the list:
//
//   - ForeignTurn: foreignLocked, the one read, which notes a true answer for the
//     section's sync (the owed-drain latch). It is called from canStartLocked
//     (from submit's canSubmitLocked and nextLocked), submit's send-now gate,
//     retryLocked, holdCancelLocked's validation, and owedDrainLocked, which
//     raises the fence itself before it reads.
//   - Begin: claimLocked (from submit's reserveLocked and nextLocked) and
//     retryLocked.
//   - The sections: submit's, holdCancel's (Cancel and Stop; the send-now arm is
//     inside submit's), and every one that runs passLocked or settleLocked —
//     runTurn's, drive's, releaseHold's, GiveUp's and GiveUpDrain's. Close raises
//     it for good. Started and every queue verb only sync it: they read neither,
//     but change what the fence should be. The same test also holds, by parsing,
//     that every method that changes an input of the fence — the queue, the
//     activity, the current turn, the cancel count, stopped — syncs it or is
//     reached only from a section that does.
//
// Under e.mu the engine also calls EventLog.Enqueue, whose mutex is a leaf.
// That is the point of the outbox: every event the engine authors is enqueued
// in the section that made the change it describes, so the order of the
// engine's events is the order of its state, with no lock held across a send.
// What follows from it is that an engine event trails the state it describes:
// State can already show a turn whose started has not been delivered. The same
// rule holds one layer down, and is the session's to keep: **s.mu → the outbox
// mutex**, every settings delta enqueued in the section that mutates the
// snapshot (plan 021 §3.8). The engine does not author those; it serialises the
// calls that cause them (settings.go).
//
// The observer runs inside the log's publishing boundary and takes leaves
// only, one after another and never one inside another: the transcript model's
// own mutex, in its first statement (e.model.Fold); then e.obsMu, which guards
// the two flags it keeps; then the index writer's (idx.post). So the order is
// **the boundary → model.mu** (and the boundary → each of the other two), and
// the other direction never happens: Snapshot takes model.mu with the boundary
// never held and releases it before it returns — a snapshot is a value — so
// Attach enters Subscribe, which waits for the boundary, holding no lock at all
// (attach.go, and TestAttachWhileAPublisherHoldsTheBoundary). model.mu is
// nested with neither e.obsMu nor the index's, and **e.mu and model.mu are
// never nested, in either order**: nothing that holds e.mu folds or snapshots
// (an event enqueued under e.mu is committed, and folded, by the outbox's
// drainer on a goroutine that holds nothing of the engine's), and the fold
// calls nothing of the engine's.
type Engine struct {
	sess agent.Session
	log  *agent.EventLog
	// model is the engine's instance of the transcript model (plan 024 §3.1):
	// folded from the observer, once per committed event and in Seq order,
	// inside the publishing boundary, so it is always a complete folded prefix
	// of the committed sequence — equal to it once the observer returns. It is
	// what Attach cuts a snapshot from. Built in New with the default bounds, no
	// clock (a zero At stays zero: nothing is called back under the boundary)
	// and the log's incarnation, and never replaced; its mutex is its own.
	model *transcript.Model
	// asks is the session's ask registry: the engine holds it so that a client
	// can list and answer through Control without reaching around to the seam.
	// It has a mutex of its own, and **e.mu and registry.mu are never nested, in
	// either order** (plan 021 §3.6): every method here that touches it does so
	// with e.mu released.
	asks *agent.AskRegistry
	now  func() time.Time
	opts Options
	// craze is the durable craze session id (SD-22), fixed at construction and
	// never written again, so it needs no lock.
	craze string
	// idx is the session index: the bookkeeping, the command-driven writes and
	// the worker the observer feeds (index.go). Its mutex is a LEAF and no
	// write of its ever runs under e.mu.
	idx *indexWriter

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
	queue           agent.PromptQueue
	armed           *armedSend
	cancelsInFlight int
	stopped         bool
	closed          bool
	prompted        bool
	cancelled       bool
	err             string
	startFailed     bool
	// fence is the session's agent.AdmissionFence, nil for a session without
	// one; it is set in newEngine and never written again. fenced is what the
	// engine last told it, so every call it makes is a transition (the type
	// assertion's nil makes both of them inert).
	fence  agent.AdmissionFence
	fenced bool
	// drainOwed is the owed-drain latch and sawForeign the section's note that
	// it read the agent's turn running (owedDrainLocked, X39). Both are only ever
	// set with a fence, and sawForeign lives for one section: its sync consumes it.
	drainOwed  bool
	sawForeign bool
	// recheck says a restoring settlement has put a refused row back and its
	// paced recheck is owed: the driver's tick is armed for it (rearm), and the
	// tick's pass is that recheck, whatever it finds. Only a tick clears it.
	recheck bool

	obsMu     sync.Mutex
	replaying bool

	// receipts is the command-id table (receipts.go): one table for the whole
	// engine, shared by every client, with its own mutex — a LEAF. It is never
	// held while e.mu, s.mu or registry.mu is taken (a command runs with it
	// released), and never taken while one of those is held, so it adds no edge
	// to the order above and cannot cycle. It mints the client ids too
	// (NewClientID), which is why the engine keeps no client counter of its own.
	receipts *receiptTable

	// sets is the settings worker's FIFO: every Control.Set joins it under e.mu
	// and is served one at a time, in arrival order (settings.go). setWake is
	// its kick, one slot, for the same reason the driver's is.
	sets    []*setReq
	setWake chan struct{}
	// beforeDropSet is a test barrier, nil in every build but a test's: it is
	// called on a Set's own goroutine after its context has ended and before it
	// takes itself out of the queue, so a test can force the one schedule the
	// caller and the worker race for (settings.go's Set and takeSet). It is set
	// before the engine is driven and never written again, so it needs no lock.
	beforeDropSet func()

	// kick wakes the driver. One slot is enough because the driver's passes
	// are level-triggered: each re-reads the state under e.mu, so two kicks
	// collapsed into one lose nothing.
	kick chan struct{}
	// rearm asks the driver to arm its tick and make no pass now: a restoring
	// settlement's paced recheck (restoreLocked). One slot, like kick.
	rearm chan struct{}
	done  chan struct{}
	wg    sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	// ready is Ready's channel, closed once, by readyOnce, from Started or Close
	// — whichever comes first.
	ready     chan struct{}
	readyOnce sync.Once

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
	// taken again, and a restored row's recheck is made, when the test sends on
	// it, and at no other time.
	retryTick <-chan time.Time
	// drivePassed runs on the driver's goroutine after each of its passes, once
	// what the pass claimed is launched and before it arms its tick, with e.mu
	// released: the barrier a test waits on to know a kick, or a tick (ticked),
	// has been spent.
	drivePassed func(ticked bool)
	// beforeRunSet runs on the settings worker's own goroutine between taking a
	// request out of the queue (takeSet) and the locked section that decides
	// whether to run it and claims it (runSet) — the one gap in which a request
	// is neither queued nor claimed, and the only place a test can force the
	// schedules of r27 finding 3.
	beforeRunSet func()
	// beforeInlineSeed runs on a Submit's OWN goroutine, after that submit's
	// turn has been launched and before the claimed index write it may still
	// owe (runOwn): the window in which the turn can run, end and give its e.wg
	// count back while its caller has not reached the write, which is the
	// schedule r31 finding 1 is about. It is named the turn so a test can hold
	// one submit and let the others through.
	beforeInlineSeed func(turn string)
	// beforeSessionClose runs on Close's own goroutine, after e.mu is released
	// (and Ready closed) and before Session.Close is called: the window in
	// which the engine has refused every later command and the ask registry is
	// still open, because closing it is Session.Close's own doing (tui.Stub's
	// Close, as the live session's does). It is nil in production and never
	// takes an argument: Close runs at most once (closeOnce).
	beforeSessionClose func()
	// receipts are the command-id table's own seams (receipts.go): its clock,
	// and the barriers a duplicate's schedule turns on. Like the rest of these
	// they are in place before the table exists and never assigned afterwards.
	receipts *receiptHooks
	// attachSnapshotted runs on an Attach's own goroutine each time it has cut
	// a fresh snapshot, with the snapshot's Seq and the attempt (0 for the
	// first), after the model's lock is released and before Subscribe is
	// called: the gap in which the ring can move past the snapshot (attach.go).
	// The event log's own seams are internal/agent's, so this is the barrier an
	// engine test has there; it holds no lock, so a hook may publish.
	attachSnapshotted func(seq uint64, attempt int)
}

// New builds the engine for sess. The session must own an event log
// (agent.LogOwner), because the engine publishes through it and observes it,
// and an ask registry (agent.AskSource), because a client answers asks through
// Control; New installs the log's single observer, so a second engine on one
// session is refused with agent.ErrObserverSet. It is called before sess.Start.
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
	src, ok := sess.(agent.AskSource)
	if !ok || src.Asks() == nil {
		return nil, fmt.Errorf("engine: %T has no ask registry", sess)
	}
	craze := opts.CrazeSessionID
	if craze == "" {
		craze = newCrazeSessionID()
	}
	e := &Engine{
		sess:     sess,
		log:      owner.EventLog(),
		asks:     src.Asks(),
		now:      time.Now,
		opts:     opts,
		craze:    craze,
		activity: ActivityStarting,
		kick:     make(chan struct{}, 1),
		rearm:    make(chan struct{}, 1),
		setWake:  make(chan struct{}, 1),
		done:     make(chan struct{}),
		ready:    make(chan struct{}),
		hooks:    h,
	}
	// Optional, like the Clocked below: a session that never starts a turn of
	// its own has no fence, and every fence call is then a no-op.
	if f, ok := sess.(agent.AdmissionFence); ok {
		e.fence = f
	}
	e.idx = newIndexWriter(opts.Index, craze, sess.Snapshot, e.reportIndexErr)
	// The session's clock, not the wall's: the Stub's is injected, and every
	// golden that shows a time shows one of its. It is read where the session
	// reads it already — on the goroutine that admits a prompt and the one
	// that runs it — so an injected clock needs nothing it did not need before.
	if c, ok := sess.(agent.Clocked); ok {
		e.now = c.Now
	}
	// The receipts table keeps a clock of its OWN (time.Now, or a test's
	// through these hooks) rather than the session's: how long a command id
	// stays answerable is real elapsed time, and the session's clock is an
	// event clock that a Stub freezes and a golden pins (r24 finding 6).
	var rh *receiptHooks
	if h != nil {
		rh = h.receipts
	}
	if opts.ReceiptClock != nil && (rh == nil || rh.now == nil) {
		withClock := receiptHooks{}
		if rh != nil {
			withClock = *rh
		}
		withClock.now = opts.ReceiptClock
		rh = &withClock
	}
	e.receipts = newReceiptTable(rh)
	// Before the observer is installed, which is what folds it: every committed
	// event from the first reaches it.
	e.model = transcript.New(transcript.Options{Incarnation: e.log.Incarnation()})
	if err := e.log.Observe(e.observe); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	// The durable id, once per incarnation and before anything else can use it:
	// one engine wraps one session, which owns one log, which is one
	// incarnation, so this runs exactly once for each. It is a Note, so it is
	// journal-only and never blocks, and it is written with no lock of any kind
	// held — the convention every note in craze keeps (plan 020 §3.5, the
	// session note's own noteSession).
	e.log.Note(journal.DiagNote{Kind: journal.DiagCrazeSession, Fields: map[string]any{
		"crazeSessionId": craze,
		"loaded":         opts.CrazeSessionID != "",
	}})
	e.wg.Add(2)
	go e.drive()
	go e.serveSets()
	go e.idx.serve()
	return e, nil
}

// Session is the session the engine wraps: the raw provider seam, held by a
// client only for what has not moved behind Control yet. Everything else goes
// through Control, so that one component decides what runs and two clients cannot
// contradict each other.
//
// What is still reached through here, and nothing else is:
//
//   - Snapshot, which the TUI reads through Control.State() already, and which a
//     caller that has no engine state to merge may still read here;
//   - Events, which is the log's primary and therefore the very channel
//     Control.Events() hands out: a helper written against a session — headless
//     craze's final sweeps — reads the same stream through either.
//
// That is the whole list. The settings left with C10 — SetModel, SetMode and
// SetConfig are Control.Set, serialised by one worker and answered with a
// revision, and SetTitle is Control.SetTitle — and the session index left with
// C12, so there is no verb here a client may still reach for: no Begin, no
// Cancel, no queue verb, no Interject, no setter, no Upsert.
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
//
// Every call closes Ready, whatever the start came to and whichever way this
// returns — the second call and a call on a closed engine included — after the
// state is written and e.mu released, so a waiter that wakes on Ready reads
// the start's outcome in State.
func (e *Engine) Started(err error) {
	defer e.markReady()
	e.mu.Lock()
	defer e.mu.Unlock()
	// Starting → idle is an activity the fence reads (a queue can only be owed a
	// drain from idle); it is inert today, since nothing queues while starting,
	// and kept so that no section changing an input of the fence skips it.
	defer e.syncFenceLocked()
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

// NewClientID mints a client id unique in the incarnation. The receipts table
// mints it, because the table is what has to recognise it afterwards: a
// command naming a client this engine never minted is ErrBadRequest, which is
// what keeps a per-client high-water mark meaningful (receipts.go's
// "Identity"). It waits on nothing — the table's mutex is a leaf.
func (e *Engine) NewClientID() string { return e.receipts.newClient() }

// ReleaseClient says a minted client's connection has gone (plan 027 §3.6): the
// client stays answerable — its receipts, its mark, a resume's ClaimClient — and
// becomes retirable once it has been released for the table's age bound and
// holds no entry, open or completed (receipts.go's "Clients: released, claimed
// and retired"). It is idempotent, an id the table does not know is a no-op, and
// it waits on nothing.
func (e *Engine) ReleaseClient(id string) { e.receipts.release(id) }

// ClaimClient takes a released client back, for a connection that resumed it
// (plan 027 §3.6): it is live again, and never retired while it stays so. It is
// ErrUnknownClient for an id this engine never minted or has retired, and a
// no-op for a client that was never released. It waits on nothing.
func (e *Engine) ClaimClient(id string) error { return e.receipts.claim(id) }

// Sync returns once every event enqueued before the call has been delivered.
func (e *Engine) Sync(ctx context.Context) error { return e.log.Flush(ctx, nil) }

// SyncSeq is Sync with the seq it reached: the log's committed head once
// everything enqueued before the call is committed (agent.EventLog.FlushSeq),
// and so ≥ every event any command that returned before it caused. It is the
// socket server's reply barrier (plan 027 §3.6): a handler that ran a command
// waits for its client's subscription to deliver up to this seq before it
// queues the reply. It blocks as Sync does, under the same rule.
func (e *Engine) SyncSeq(ctx context.Context) (uint64, error) { return e.log.FlushSeq(ctx, nil) }

// Ready is closed once the session's start has run — Started, whatever it came
// to — or once the engine has closed, whichever is first, and never again. It
// is what an attach that waits for readiness (`when: "ready"`) and the `ready`
// notification wait on (plan 027 §3.4, §3.6); State then says what the start
// came to, and a closed engine is ready at once, so no waiter waits on one. It
// waits on nothing.
func (e *Engine) Ready() <-chan struct{} { return e.ready }

// markReady closes Ready, once.
func (e *Engine) markReady() { e.readyOnce.Do(func() { close(e.ready) }) }

// Done is closed once Close has closed the session — and with it the log, so
// every subscription has ended — and never before: the engine has ended. It is
// the socket server's signal to close the connections of a session that is
// over (plan 027 §3.7). It waits on nothing.
func (e *Engine) Done() <-chan struct{} { return e.done }

// Note writes a journal-only note into the session's journal — the socket
// server's per-connection diag notes (plan 027 §3.7) — through the log, so it
// is refused after the log's Close exactly as the log's own notes are
// (agent.EventLog.Note). It waits on nothing, takes no lock of the engine's, and
// writes nothing without a journal.
func (e *Engine) Note(n journal.Note) { e.log.Note(n) }

// TranscriptSnapshot is one bounded snapshot of the engine's transcript model,
// with no subscription: session.snapshot (plan 027 §3.4, bounded history). With
// agentID "" it is the snapshot an attach cuts (transcript.Model.Snapshot);
// with a child's id, that child's window is filled right after the main
// transcript's newest entry (transcript.Model.SnapshotFor), and an id the model
// names no child or roster row by is an error wrapping agent.ErrNoSuchSubagent
// (unknown_subagent). budget <= 0 is transcript.DefaultSnapshotBytes; a budget
// the mandatory sections do not fit is transcript.ErrSnapshotTooLarge, wrapped.
//
// It is a read and changes nothing. It takes the model's mutex for the cut
// alone — held for at most one fold or one cut, never a context wait — and
// builds the snapshot after releasing it, so it waits on nothing a client could
// be holding; it may not be called from inside the log's publishing boundary
// (the observer), which nothing but the engine's own fold runs in.
func (e *Engine) TranscriptSnapshot(agentID string, budget int) (*transcript.Snapshot, error) {
	return e.model.SnapshotFor(agentID, budget)
}

// Asks is every ask the session is holding, in the order they were opened. It
// waits on nothing and takes no lock of the engine's: the registry is its own
// authority, and e.mu is never held across a call into it.
func (e *Engine) Asks() []agent.AskRecord { return e.asks.Asks() }

// Ask is one ask by id, open or resolved, for as long as the registry keeps its
// record. It is Asks for a client that wants one ask and not the list — asking
// whether an id is still open costs a map lookup here, where listing costs a
// copy of every open request. It waits on nothing, for the same reason Asks
// does.
func (e *Engine) Ask(id string) (agent.AskRecord, bool) { return e.asks.Record(id) }

// Answer answers one ask, and is the whole of what a client does with a card.
// It validates and claims in one step: an answer that does not fit is
// agent.ErrBadAnswer and **leaves the ask open**, so a client that
// mis-addressed one call has not left the agent waiting for an Esc; a second
// valid answer is agent.ErrAlreadyResolved; an id this incarnation never issued
// is agent.ErrUnknownAsk.
//
// The engine adds no gate of its own. An ask may be answered while the agent is
// running a turn of its own, while a cancel is in flight, while the engine is
// replaying — whenever the agent is waiting on one, which is the only condition
// that matters — and the command's cause travels onto the ending, so the client
// that answered can skip its own echo.
//
// It waits on nothing: one registry section and an enqueue, both of which are
// bounded, so a client may call it from the primary's own reader.
//
// Its receipt is recorded without ever taking e.mu: Answer does not touch it
// today (X40) and must not start now, so its entry hook (withSyncReceipt)
// takes the table's mutex twice, briefly, with the registry section between
// them and no lock of the engine's anywhere — and nothing else ever takes the
// table's mutex while holding registry.mu either (receipts.go's package doc).
func (e *Engine) Answer(c Command, id string, a agent.AskAnswer) error {
	hash := receiptHash("Answer", id, answerSpelling(a))
	return withSyncReceiptErr(e.receipts, c, hash, func() error {
		_, err := e.asks.Answer(c.Cause(), id, a)
		return err
	})
}

// Interject merges text into the running turn. It is the session's own verb
// and its own refusals: the engine adds nothing but the door.
func (e *Engine) Interject(ctx context.Context, c Command, text string) error {
	hash := receiptHash("Interject", text)
	return withBlockingReceiptErr(ctx, e.receipts, c, hash, func() error {
		e.mu.Lock()
		refused := e.refusalLocked()
		e.mu.Unlock()
		if refused != nil {
			return refused
		}
		return e.sess.Interject(ctx, text)
	})
}

// Close refuses every later command, closes the session — which ends the turn
// that is running and closes the log last, committing whatever the outbox
// still holds — and joins the engine's goroutines, the cancel an armed send-now
// asked for included. It is idempotent.
//
// # The turn that is running when it is called
//
// It gets its ending here, in the section that refuses admission, and that is
// the ONE ending it gets. Without it a quit mid-turn left a turn with a started
// and no ended in the record — the live smoke's finding A1, on every provider —
// and a client folding the stream saw a turn that never closes. The stream is a
// complete record (SD-30): a turn that has ended says so, whatever ended it,
// and shutdown already authors the endings nothing else will (an armed send's
// disarm below, every parked ask's `closing`).
//
// It is authored HERE and not left to the settlement because the settlement is
// not coming: closed is set in this same section, and the driver's pass returns
// at once for a closed engine (passLocked), so the continuation that comes back
// afterwards runs no pass, emits nothing and touches nothing a client can see.
// Clearing e.cur is what makes that final: the turn is no longer current for
// State, for a cancel's release, or for anything that looks. One mutex, two
// outcomes — a settlement that got there first found e.cur and left none behind,
// and this one finds nothing to end — so there is exactly one ending either way.
//
// The ending is synthetic (the wire produced nothing), stopped `closing` — the
// word the ask registry and the send-now's own disarm already use for this —
// with no successor and the queue's own length as Pending: Close is not Stop, so
// the rows stay where they are and nothing is said about them. It is enqueued
// before the session's close, so the log's own close phases commit it.
//
// The index worker is joined too, but its join is bounded where the others are
// not: a row is written through a file lock that nothing can interrupt, so the
// worker abandons a write still in flight rather than make a quit wait for
// another process to let go of the lock (indexWriter.serve). That write lands
// on its own goroutine; what it records is state the engine had already
// decided.
//
// What the observer posted and the worker never picked up is NOT dropped with
// it. The worker's exit drains the slot one last time and hands a pending
// seed, agent title or loaded-row touch to one final write (indexWriter.finish)
// — this is the last moment any of them can be recorded, and a first prompt the
// drain started vanishing from --continue on a quit is not something the TUI's
// synchronous writes ever did. That final write is bounded, at 500 ms, and
// abandoned when the bound runs out. A plain touch on its own is still dropped:
// its only effect is an UpdatedAt the next run's first write bumps anyway.
//
// The same 500 ms bound is also owed to a lone first-prompt seed attempt in
// flight on ANY goroutine — a server handler's own Submit, or the worker's
// (plan 027 §3.7, SF-15) — with nothing else pending behind it: without this a
// quit mid-seed left a session missing from --continue entirely, where the
// slot alone could not show the attempt was still in flight.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.activity = ActivityClosing
		// Up for good: nothing is admitted again, and a session that could start a
		// turn of its own must not start one under a closing engine either.
		e.raiseFenceLocked()
		var batch []agent.Event
		if t := e.cur; t != nil {
			// The turn that was running has ended, and this is the only place
			// left that can say so (the doc comment above). It carries the
			// turn's own cause, as every other ending does.
			e.cur = nil
			batch = append(batch, e.stamp(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
				ID: t.id, Phase: agent.TurnEnded, Synthetic: true,
				StopReason: stopClosing, Pending: e.queue.Len(),
			}}, t.cause))
		}
		// An armed send will never fire now, and the record says so before the
		// log stops taking work: a mandatory completion, like every other
		// ending shutdown authors. It follows the ending, where a settlement
		// puts it too (settleLocked's batch).
		if ev, ok := e.disarmLocked(agent.SendNowClosing, "", ""); ok {
			batch = append(batch, ev)
		}
		e.log.Enqueue(batch...)
		e.mu.Unlock()
		// Closed admits nothing from here, so a start this engine never had
		// can no longer come: nobody waits for one (Ready).
		e.markReady()
		if h := e.hooks; h != nil && h.beforeSessionClose != nil {
			h.beforeSessionClose()
		}
		e.closeErr = e.sess.Close()
		close(e.done)
		e.wg.Wait()
		e.idx.close()
	})
	return e.closeErr
}

// reportIndexErr publishes a failed index write, for every write but the
// rename whose caller is still there to be told (Control.SetTitle). The event
// is a StateDelta carrying nothing but IndexErr: a session craze could not
// remember is a note to the user and never a reason to stop running it, and
// §2.4 keeps that note a client-local row.
//
// It is a mandatory completion — enqueued whether or not the outbox reports
// room — because it is a report of something that has already happened and
// there is nobody to refuse it to.
func (e *Engine) reportIndexErr(cause, msg string) {
	e.log.Enqueue(e.stamp(agent.Event{
		Type: agent.EventMeta, State: &agent.StateDelta{IndexErr: msg},
	}, cause))
}

// observe is the log's observer. It runs inside the publishing boundary, once
// per committed event, so it does nothing but fold the event into the
// transcript model and note what the driver and the index worker have to look
// at again and wake them: it takes no lock but its own leaves (the model's
// mutex and e.obsMu), does no I/O, and calls neither the log nor the session.
//
// The fold is the first statement, ahead of the sub-agent guard (plan 024
// §3.1): every child event and every roster event reaches the model, which
// routes them to the child's transcript and the roster itself. The guard is
// right for what follows it and wrong for the fold — a sub-agent's event is
// never the main session's news for the engine's flags: the TUI has always
// routed those away before any of this ran (applyEvent), and a sub-agent
// finishing is not this session being used.
func (e *Engine) observe(ev agent.Event) {
	e.model.Fold(ev)
	if ev.Agent != "" || ev.Type == agent.EventSubagent {
		return
	}
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
			// Only a load replays, so this is exactly "a loaded session came
			// up": its row is the row its id came from, and touching it is what
			// sorts a resumed session to the front of the picker before it has
			// done anything (index.go's load).
			e.idx.post(indexWork{load: true})
		}
	case agent.EventForeignTurn:
		if ev.ForeignTurn != nil && !ev.ForeignTurn.Running {
			e.wake()
		}
	case agent.EventDone:
		// A turn ended, so this session is the newest thing in the workspace.
		// It is the session's own ending and not the engine's, because a turn
		// the AGENT ran on its own ends here too and is just as much use of
		// this session — which is what the TUI keyed on. A touch is a no-op
		// until a row exists, so one before craze has ever prompted conjures
		// nothing.
		e.idx.post(indexWork{touch: true})
	case agent.EventMeta:
		// session_info_update: the agent named the session. Only an
		// agent-initiated update fills Event.Text — a /rename and a load's own
		// title seed carry theirs in State alone (plan 021 correction 20) — so
		// this is the one title the index takes as the agent's. Replayed is
		// deliberately NOT consulted: the TUI never consulted it either, and no
		// agent replays a session_info_update, so the two rules cannot differ
		// in practice and the one that was shipped is the one kept.
		if ev.Text != "" {
			e.idx.post(indexWork{title: ev.Text})
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
	return e.cur == nil && e.cancelsInFlight == 0 && e.refusalLocked() == nil && !e.foreignLocked()
}

// foreignLocked is the session's ForeignTurn, the one place the engine reads it.
// A true answer is noted for the section's fence sync (sawForeign): a row that
// ends the section queued behind the agent's turn the section saw is a drain
// owed, even if that turn ends before the sync reads the flag again (X39).
func (e *Engine) foreignLocked() bool {
	f := e.sess.ForeignTurn()
	if f && e.fence != nil {
		e.sawForeign = true
	}
	return f
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

// raiseFenceLocked puts the session's admission fence up, if it is not up
// already. It is the first thing every section that reads ForeignTurn, calls
// Begin or validates a cancel does (the Engine doc's list), so a turn the session
// could start of its own is either already running when the engine looks — and
// the engine queues behind it — or cannot start until the engine is idle again.
func (e *Engine) raiseFenceLocked() {
	if e.fence == nil || e.fenced {
		return
	}
	e.fence.FenceUp()
	e.fenced = true
}

// syncFenceLocked brings the fence to what the engine now is, calling the
// session only on a transition. Every section that raised it syncs it on the
// way out, deferred after the unlock is deferred so it runs first and under
// e.mu, and so does every section that changes what fenceWantLocked reads: a
// queue verb that removes the last row lowers it at once. Nothing lowers it
// between a settled turn and its successor, which are one section.
func (e *Engine) syncFenceLocked() {
	if e.fence == nil {
		return
	}
	switch want := e.fenceWantLocked(); {
	case want && !e.fenced:
		e.fence.FenceUp()
		e.fenced = true
	case !want && e.fenced:
		e.fence.FenceDown()
		e.fenced = false
	}
}

// fenceWantLocked is whether the fence should be up: whenever the engine is not
// idle. A turn of its own is current, a cancel it validated is still on its way
// to the session, or a drain is owed; and a stopped or closed engine keeps it up
// for good — nothing will be admitted again, GiveUpDrain's abandonment included,
// and nothing may start behind a client that has been told so. An error state,
// starting and replaying keep it up for none of these reasons, and lower it: a
// failed turn owes nothing (astra's round-4 pin).
//
// The owed drain is settled first and on every sync, whatever the other terms
// say, because it is a latch: it has to be cleared by the sync that sees it
// withdrawn, not merely outvoted while a turn is current.
func (e *Engine) fenceWantLocked() bool {
	owed := e.owedDrainLocked()
	return e.closed || e.stopped || e.cur != nil || e.cancelsInFlight > 0 || owed
}

// owedDrainLocked settles the owed-drain latch (drainOwed) and reports it. A
// drain is owed at the end of the turn the agent is running: the engine is idle
// with rows queued, nothing of its own is current, it admits, and the agent holds
// the session — so the observer's kick at that turn's end (Running:false) is what
// drains the head. The fence stays up across it, so that the end of one turn the
// session started itself is not followed by another before the user's queued row
// has run (plan 026's F3).
//
// It is a latch (X39, astra r16), because the agent's turn can end while a
// section holds e.mu: a Submit that queued its row behind that turn, or an edit
// made after it, would otherwise read the flag clear at its sync and lower the
// fence with the row still queued, and a second turn the session had pending
// would take the session before the driver drained the row. So:
//
//   - It is set when the section saw the agent's turn (sawForeign, any read of
//     the flag in the section — Submit's admission check among them), or, with
//     no cancel in flight, when the flag reads true now.
//   - It holds, without reading the flag again, while the engine stays idle with
//     rows, nothing current and nothing refusing: until the drain it is owed is
//     claimed (a turn is then current, and holds the fence up itself).
//   - It is CLEARED — not merely outvoted — by every sync that finds the queue
//     empty, the engine failed, starting, replaying, stopped or closed, or a turn
//     current. A latch left standing over an emptied queue would come back up
//     over the next row the Queue verb adds on an idle engine, which nothing
//     would ever drain (astra r14's deadlock).
//
// The foreign-turn term is what tells an owed drain from an idle queue: the
// Queue verb adds rows while idle and never starts a turn (queue.go), so rows
// added with the agent's session free leave the fence down.
//
// The flag is read with the fence up, as every read of it is: the section's own
// fence is raised first. In a section that had not raised it — a queue verb, on
// an idle engine with rows — that is one FenceUp and one FenceDown in the same
// section, which a session can only take as "look again".
func (e *Engine) owedDrainLocked() bool {
	saw := e.sawForeign
	e.sawForeign = false
	if e.cur != nil || e.activity != ActivityIdle || e.queue.Len() == 0 || e.refusalLocked() != nil {
		e.drainOwed = false
		return false
	}
	if !e.drainOwed {
		if saw {
			e.drainOwed = true
		} else if e.cancelsInFlight == 0 {
			e.raiseFenceLocked()
			e.drainOwed = e.foreignLocked()
			e.sawForeign = false
		}
	}
	return e.drainOwed
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
	hash := receiptHash("Submit", text, string(mode), fromRow)
	return withSyncReceipt(e.receipts, c, hash, func() (SubmitResult, error) {
		return e.submit(c, text, mode, fromRow)
	})
}

// submit is Submit's own work, run at most once per command id: see
// withSyncReceipt.
//
// The index seed is the one thing here that is not instant: it is file I/O, on
// this caller's goroutine, outside e.mu — §3.2's one documented exception, and
// exactly where the TUI did it. It runs after the locked section has admitted
// the turn and before this returns, so it is inside the command's receipt and a
// resend replays the answer rather than writing a second row. Its failure is
// NOT this call's answer: the turn has been admitted and is the answer, so the
// failure goes out as a StateDelta{IndexErr} naming this command (index.go).
func (e *Engine) submit(c Command, text string, mode SubmitMode, fromRow string) (SubmitResult, error) {
	var started []launch
	// armedTurn is the turn an arm asked to have cancelled, carried out of the
	// locked section so the cancel itself is made with the lock released.
	armedTurn, armedCause := "", ""
	res, err := func() (SubmitResult, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		// Before the foreign check and the claim below (the Engine doc's
		// admission fence), and back to what the engine is on every way out.
		defer e.syncFenceLocked()
		e.raiseFenceLocked()
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
			if e.foreignLocked() {
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
			var from *agent.QueuedPrompt
			if fromRow != "" {
				row, ok := e.rowLocked(fromRow)
				if !ok {
					return SubmitResult{}, fmt.Errorf("%w: %s", ErrUnknownRow, fromRow)
				}
				text = row.Text
				take = func() []agent.Event { return e.takeRowLocked(fromRow, c.Cause()) }
				from = &row
			}
			l := e.reserveLocked(text, origin, c.Cause(), take)
			// The row the turn's text came from, as it stood when it was taken: a
			// refusal by the agent's own turn puts it back (restoreLocked).
			l.t.row = from
			started = append(started, l)
			// The text the turn started with, read in this section: a row's
			// own text when the prompt came from one, which another client may
			// have edited since this one looked (SubmitResult.Text).
			return SubmitResult{Turn: l.t.id, Text: l.t.text}, nil
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
	e.runOwn(started, c.Cause())
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
//
// It also posts each turn's first-prompt index seed to the worker. Every
// caller of run but one is a turn the ENGINE started — a drain, a settlement's
// successor, an armed send firing, a claim taken again — on the driver's
// goroutine or on whichever one a continuation came back on, and none of those
// may do file I/O. The one exception is a client's own Submit, which runs its
// seed inline (runOwn). A seed is claimed once and once only, so a turn that
// posts one after another path has already written it does nothing.
//
// The post comes BEFORE the launch, for the reason runOwn's admission does
// (r31 finding 1): a turn gives its e.wg count back when it ends, so an
// opportunity that only arrives after the launch can arrive after Close has
// passed e.wg.Wait() and the worker's exit has looked at the slot — and then
// nothing is left to write it. Posting is a merge and a non-blocking kick and
// waits on nothing, so the turn loses nothing by it.
//
// The turn's CAUSE goes with the text. A turn the engine started still has one
// where a client's command asked for it — the send-now that was armed, the
// submit whose claim is being taken again — and a seed of that turn that fails
// is a StateDelta{IndexErr} that must name it, exactly as a Submit's own does
// (r29 finding 4). Only a drain's is empty: nobody asked for that turn now.
func (e *Engine) run(ls []launch) {
	for _, l := range ls {
		e.idx.post(indexWork{seed: true, seedText: l.t.text, seedCause: l.t.cause})
	}
	for _, l := range ls {
		go e.runTurn(l)
	}
}

// runOwn is run for the turn a client's own Submit started: its seed is file
// I/O on THIS goroutine — §3.2's one documented exception to "waits on
// nothing", and exactly where the TUI wrote it — rather than the worker's, so
// it sits inside the command's receipt and its failure names the command.
//
// The three steps are in this order, and the order is the whole of r31 finding
// 1: the opportunity is ADMITTED (and claimed, when no other attempt is in
// flight), then the turn is launched, then the claimed write is made. Every
// turn e.wg counts has therefore made its seed opportunity visible to the
// worker's exit before it can end, so Close can no longer pass e.wg.Wait() with
// a seed that is about to be retained behind a failing one — which left a
// session that had run two prompts with no index row at all, its retry kicking
// a worker that had already gone.
//
// A claim this caller holds is never written by anyone else in the meantime:
// the slot is empty while the attempt is in flight, so the worker's exit finds
// nothing to claim and waits for this attempt through seedDone instead
// (indexWriter.finish), and one engine still runs one Upsert at a time.
func (e *Engine) runOwn(ls []launch, cause string) {
	claimed := make([]bool, len(ls))
	for i, l := range ls {
		claimed[i] = e.idx.admitSeed(cause, l.t.text)
	}
	for _, l := range ls {
		go e.runTurn(l)
	}
	for i, l := range ls {
		if h := e.hooks; h != nil && h.beforeInlineSeed != nil {
			h.beforeInlineSeed(l.t.id)
		}
		if claimed[i] {
			e.idx.writeSeed(cause, l.t.text)
		}
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
		// No done of the engine's own: the engine's workers are joined by
		// Close only *after* the session has closed, and closing the session
		// closes the log, which is what frees this barrier (agent.EventLog.Flush).
		_ = e.log.Flush(context.Background(), nil)
	}
	res, err := l.run(context.Background())
	var next []launch
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		defer e.syncFenceLocked()
		e.raiseFenceLocked()
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
//
// Its tick is armed for two things, both paced by time rather than by events: a
// claim refused under ChainPolicy.RetryForeignTurn (retryDue), and a restored
// row's recheck (recheck). A restoring settlement asks for the second through
// rearm, which arms the tick and makes no pass: a pass there and then would take
// the row it just put back while the session's flag may still lag its client's
// refusal, and be refused again, as fast as the session could refuse.
func (e *Engine) drive() {
	defer e.wg.Done()
	var tick <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	arm := func() {
		if h := e.hooks; h != nil && h.retryTick != nil {
			tick = h.retryTick
			return
		}
		if timer == nil {
			timer = time.NewTimer(foreignRetryTick)
		} else {
			timer.Reset(foreignRetryTick)
		}
		tick = timer.C
	}
	for {
		ticked := false
		select {
		case <-e.kick:
		case <-e.rearm:
			arm()
			continue
		case <-tick:
			ticked = true
		case <-e.done:
			return
		}
		var next []launch
		again := false
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			defer e.syncFenceLocked()
			e.raiseFenceLocked()
			if ticked {
				if e.cur != nil && e.cur.retry {
					// Only a tick makes a refused claim due. A kick says the state
					// may have changed; it does not say time has passed, and a
					// refusal with nothing else to wait for is paced by time alone.
					e.cur.retryDue = true
				}
				// This pass is a restored row's recheck, whatever else the tick was
				// armed for: it drains the row if a turn may start, and one that
				// finds the agent's turn still running arms nothing further — that
				// turn's end, or any later pass, drains it. A restoration during
				// this very pass asks again.
				e.recheck = false
			}
			next = e.passLocked()
			again = e.cur != nil && e.cur.retry || e.recheck
		}()
		e.run(next)
		if h := e.hooks; h != nil && h.drivePassed != nil {
			h.drivePassed(ticked)
		}
		tick = nil
		if again {
			arm()
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
	if !t.retryDue || e.cancelsInFlight > 0 || e.foreignLocked() {
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
func (e *Engine) GiveUp(c Command, turn string) error {
	hash := receiptHash("GiveUp", turn)
	return withSyncReceiptErr(e.receipts, c, hash, func() error {
		var next []launch
		err := func() error {
			e.mu.Lock()
			defer e.mu.Unlock()
			// The settlement below decides a successor, which reads the flag and
			// claims.
			defer e.syncFenceLocked()
			e.raiseFenceLocked()
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
	})
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
// giveUpDrainResult bundles GiveUpDrain's two values so its receipt has one T
// to store: a resend must get back exactly what the first call returned, the
// pair included.
type giveUpDrainResult struct {
	turn    string
	pending int
}

func (e *Engine) GiveUpDrain(c Command) (turn string, pending int, err error) {
	hash := receiptHash("GiveUpDrain")
	res, err := withSyncReceipt(e.receipts, c, hash, func() (giveUpDrainResult, error) {
		var out giveUpDrainResult
		var next []launch
		var ferr error
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			// The pass reads the flag and may claim; an abandonment sets stopped,
			// which keeps the fence up for good.
			defer e.syncFenceLocked()
			e.raiseFenceLocked()
			if e.closed {
				ferr = ErrNotAccepting
				return
			}
			if e.cur == nil {
				next = e.passLocked()
			}
			if e.cur != nil {
				out.turn = e.cur.id
				return
			}
			e.stopped = true
			out.pending = e.queue.Len()
			// A send armed and waiting for the same drain goes with it, and says so,
			// as it does for Stop: nothing will fire it now.
			if ev, ok := e.disarmLocked(agent.SendNowStopped, "", ""); ok {
				e.log.Enqueue(ev)
			}
		}()
		e.run(next)
		return out, ferr
	})
	return res.turn, res.pending, err
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
				// a send that never fired lost nothing. The turn keeps the row as
				// it was read, for a refusal to put back (restoreLocked).
				l.t.row = &from
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
		// Captured before the pop, whole — id, text, version, queued time — so a
		// refusal puts back the row it took and not a new one (restoreLocked).
		l.t.row = &head
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
// answer go back to the head of the queue; a turn whose queued row the agent's
// own turn refused ends there, with its row put back (restoreLocked, SF-21); the
// armed send-now is decided, and disarmed here if this turn is not one it can
// fire after; the activity is set and the chain policy's verdict on the queue is
// taken; the successor, if there is one — the armed send first, else the head of
// the queue — is claimed and its row taken; *then* the policy's clear runs, so it
// can never take the row the armed send just claimed with it; and one batch is
// enqueued — the queue's
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
	if e.restoresLocked(t) {
		return e.restoreLocked(t, batch)
	}

	info := &agent.TurnInfo{ID: t.id, Phase: agent.TurnEnded}
	refused := errors.Is(t.err, agent.ErrPromptInFlight) || errors.Is(t.err, agent.ErrForeignTurn)
	failed := false
	switch {
	case t.err == nil:
		info.StopReason = t.res.StopReason
	case errors.Is(t.err, agent.ErrPromptCancelled), e.cancelledRefusalLocked(t):
		// Cancelled before its turn opened: nothing ran, nothing failed, and
		// no event of any kind is coming from the session. It is the ending a
		// cancelled turn has — and the ending a row-sourced turn has that the
		// user cancelled and the agent's own turn refused before the withdrawal
		// could say so.
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

// restoresLocked says t's ending is the restoring one (SF-21): its text came
// from a queued row, and the session refused it because the agent was running a
// turn of its own. Nothing reached the agent, so the row is still the user's
// message, waiting — a refusal must not be the reason it is lost, nor the reason
// the queue behind it stops.
//
// Only under the TUI's policy and on an engine that still admits. Under
// ChainPolicy.RetryForeignTurn a refusal reaches here only when the client gave
// up waiting (GiveUp) or stopped, and the turn then ends as the refusal it was;
// a stopped engine runs nothing again, so there is nothing to put a row back
// for. A refusal of text the client held itself, and ErrPromptInFlight, keep the
// ordinary settlement: an error, with the queue left as it was.
//
// And never for a turn a cancel was validated against: the user asked for that
// turn to stop, and a stop wins over a restore (cancelledRefusalLocked).
func (e *Engine) restoresLocked(t *turn) bool {
	return e.rowRefusedLocked(t) && !t.cancelAsked && !e.stopped
}

// cancelledRefusalLocked says t's ending is the cancel-before-open one although
// its continuation came back refused (the session-control R1): a row-sourced turn
// that a cancel was validated against — Cancel, Stop, or an armed send-now's —
// and that the agent's own turn refused. The cancel was for this turn, and it
// would have withdrawn it had the refusal not come first, so the turn ends as
// ErrPromptCancelled's does: synthetic, stopped `cancelled`, the engine idle; the
// row is not put back and never runs; a send armed against it fires, as a
// send-now does after the turn it cancelled; and the queue behind it follows the
// ordinary rule after a cancel (the TUI's carries on, a stop's is cleared).
//
// Text the client held itself has no row, and keeps the refusal's settlement
// even after a cancel: a residual, recorded with R1. So does a turn under
// ChainPolicy.RetryForeignTurn, whose client reads a foreign-turn refusal as one.
func (e *Engine) cancelledRefusalLocked(t *turn) bool {
	return e.rowRefusedLocked(t) && t.cancelAsked
}

// rowRefusedLocked is what both of the above start from: a turn whose text came
// from a queued row, refused because the agent was running a turn of its own,
// under the TUI's policy.
func (e *Engine) rowRefusedLocked(t *turn) bool {
	return t.row != nil && errors.Is(t.err, agent.ErrForeignTurn) && !e.opts.Chain.RetryForeignTurn
}

// restoreLocked is the restoring settlement: t's row goes back to the head of
// the queue as the row it was — its own id, version and queued time
// (agent.PromptQueue.Restore) — and the engine is idle, not failed. batch holds
// what the settlement has already produced (the turn's unanswered steers).
//
// The ending keeps the wire shape every refusal has: synthetic, the refusal's
// class and text, no successor, and Pending counting the row it put back — so a
// client's fold draws the same error row it always has (SF-47 is the softer
// note). The row's `queued`, at position 0 and carrying the turn's own cause,
// goes in the same batch ahead of the ending, where the steers go.
//
// It claims no successor, neither the head nor an armed send: the session's
// flag can lag its client's refusal (grok's client knows of the agent's turn
// before the session does), so a claim now would be refused again, and put back
// again, as fast as the two could go. No send can be armed against this turn:
// arming validates a cancel against it, and a cancelled turn is not restored
// (cancelledRefusalLocked). One armed against another turn — a send left standing
// behind the agent's turn when a direct submit took a row — goes, as it does from
// any settlement.
//
// And it asks for a paced recheck of its own (rearm → the driver's tick): the
// agent's turn can have ended, and the kick its end gave the driver been spent on
// a pass that found this turn's continuation still out, before this settlement
// ran. Then nothing else would ever drain the row. The tick's pass drains it if a
// turn may start; if the agent's turn is still running, that turn's end does.
func (e *Engine) restoreLocked(t *turn, batch []agent.Event) []launch {
	batch = append(batch, e.stamp(e.queue.Restore(*t.row).Event(), t.cause))
	e.cur = nil
	e.activity = ActivityIdle
	e.cancelled = false
	e.rememberErrLocked(t)
	info := &agent.TurnInfo{
		ID: t.id, Phase: agent.TurnEnded, Synthetic: true,
		Err: t.err.Error(), ErrClass: agent.ClassifyEventErr(t.err),
		Pending: e.queue.Len(),
	}
	batch = append(batch, e.stamp(agent.Event{Type: agent.EventTurn, Turn: info}, t.cause))
	if a := e.armed; a != nil && a.turn != t.id {
		if ev, ok := e.disarmLocked(agent.SendNowOtherTurn, "", ""); ok {
			batch = append(batch, ev)
		}
	}
	e.log.Enqueue(batch...)
	e.recheck = true
	select {
	case e.rearm <- struct{}{}:
	default:
	}
	return nil
}

const (
	stopEndTurn   = "end_turn"
	stopCancelled = "cancelled"
	// stopClosing is the ending Close authors for the turn that was running
	// when it was called. It is the word the rest of shutdown already uses for
	// the same fact — agent.AskClosing for a parked ask, agent.SendNowClosing
	// for an armed send — so a client that folds the stream reads one vocabulary
	// for "the session went away under this".
	stopClosing = "closing"
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
