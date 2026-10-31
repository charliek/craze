package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// The waiter (plan 041 §3.1, W1) follows one turn of a craze session over the
// control protocol and reports its result: `craze wait`'s, and the one `craze
// new --wait` and `craze send --wait` build on (C3), which attach first, then
// submit, and hand the submit to the waiter (waitStart.submit).
//
// It is a state machine over the session's stream, from one attachment:
//
//   - queued(row): the row's `sent`, then the next `started` of craze's own,
//     is running(turn); the row's `removed` is dropped.
//   - running(turn): the `ended` of (incarnation, turn) is the result — unless
//     it is synthetic, refused by the agent's own turn, and the target's own
//     row came back `queued` in that same settlement (the event just before
//     it), when the waiter goes back to queued(row) (P3,
//     internal/engine/engine.go's restoreLocked). A turn submitted directly
//     has no row, and its refusal is its ending. A foreign turn (plain wait
//     only) runs from its bracket's start to its end.
//   - Identity is (incarnation, turn id) (P7): turn ids restart at turn-1
//     with every incarnation, so a snapshot of another incarnation — the host
//     restarted, the engine replaced — is never the same turn: the ending was
//     not observed (exit 1, with the handle). Every read and command the
//     waiter makes for its target — the owned forms' prompt among them — is
//     bound to a client identity proven to be its incarnation's (bind,
//     proveIdentity): an answer from, or a prompt or a cancel to, a host the
//     client reconnected to since is never taken or sent.
//   - A non-automatic ask open when the waiter looks (session.snapshot) — once
//     its attachment has caught up (synchronized), and at every opening after
//     — is needs_input, for the target's own turn or, while the target is
//     still queued, for the turn that blocks it (blockedBy), and only while
//     the session still stands at the target (by id: the agent's own turn's
//     bracket too): a successor's ask, its ending still on the stream, is
//     not; and one that found the session past where the stream stood looks
//     again once the target moves on it (relook: its row put back, its row's
//     turn started), an ask already open bringing no opening of its own. An
//     automatic one never is (P9). A look a signal or --timeout cut off
//     decides nothing; one that found another incarnation is the ending not
//     observed; any other failed look is made again.
//   - Events before a submit's reply are buffered from the attach on — the
//     stream's end and loss among them — and replayed once the reply names
//     the target (P7), so a `started` — even an `ended` — that reached the
//     stream first is still the target's.
//   - A reconnect is internal/remote's: it redials and re-attaches with the
//     stream's cursor. A replay the host refuses comes back as a snapshot
//     (reconcile): the target's row back in its queue is waited for again,
//     the target running in it is followed on (partial), a turn that ended is
//     settled from session.state's lastTurn (partial), and anything else is
//     an ending not observed.
//   - The ending a session's close gives the turn it cuts short (stopReason
//     closing, its cause what began the stop) is that turn's result: cancelled,
//     exit 3, stopReason stopped, with what was folded of it (closed). Whichever
//     ending the engine published first is the one folded: nothing is read past
//     a result.
//   - A turn whose ending never reached the waiter, in a stop its host said it
//     began (its stop record, its registry entry marked), is that same result,
//     from what was folded, its usage not called whole (stopUnseen).
//   - A session that ends with no ending for the target and no such turn — a row
//     that never ran, the agent's own turn, a turn the stop found already over —
//     is exit 1, kind stopped, `the session ended`, with the reload hint and the
//     handle.
//   - A stream lost with no word of a stop is told by the host's process (lost):
//     gone, exit 1, host_exited; running, exit 1, connection_lost, the turn runs
//     on in its host; one that cannot be named, the same kind, its state unknown.
//   - A signal or --timeout's end interrupts the waiter's work at once,
//     reads in flight included (run's op). Past --timeout's end nothing read
//     decides the target: it is running, with its handle (timedOut).
//
// Signals act only on the command's own work (P8), and only on the owned
// forms (wait --from, new --wait, send --wait): a queued row is removed, its
// own running turn cancelled by id — never a session.cancel without one — on
// a fresh cleanupWait context, a second signal ending the command at once —
// its detach as it exits included (waitConn.close). Plain wait exits on a
// signal without touching the session.

// cleanupWait bounds a signal's cleanup (§3.1 "Signals"), every part of it on
// one fresh context, and the wait for a submit's reply a signal finds still
// out. A variable only so a test can make it long: then nothing but a second
// signal ends a cleanup that waits on a cancel the host never answers.
var cleanupWait = 2 * time.Second

// waitConn is one connection to a session's host, attached to its session.
type waitConn struct {
	c  *remote.Client
	st *remote.Stream
	// entry is the registry entry the connection was dialled from, and
	// sessionID the craze session it is attached to.
	entry     rundir.Entry
	sessionID string
	// closeBy and abandon are how close ends after a signal, which the
	// waiter sets: the detach within the cleanup's own budget (§3.1: the
	// whole cleanup within cleanupWait), or — after a second signal, which
	// exits at once — none at all.
	closeBy time.Time
	abandon bool
}

// detachBound bounds a waiting command's detach as it exits, as a view
// close's is bounded (internal/remote's closeBound): past it the client's
// close ends the transport, and the subscription with it. A variable only so
// a test can make it long, and see that a signal's exit does not wait on it.
var detachBound = 3 * time.Second

// close detaches the stream — within detachBound, or by closeBy after a
// signal; not at all once abandoned — and closes the client, which ends the
// transport and the subscription with it. A signal sigs delivers meanwhile —
// the command's signals, once its waiter has stopped reading them (nil: none)
// — ends the detach at once: a signal during the exit is one more (§3.1: a
// second signal exits at once), and the detach is only a courtesy to the
// host, the subscription ending with the transport anyway.
func (wc *waitConn) close(sigs <-chan os.Signal) {
	if !wc.abandon {
		by := wc.closeBy
		if by.IsZero() {
			by = time.Now().Add(detachBound)
		}
		ctx, cancel := context.WithDeadline(context.Background(), by)
		stop := make(chan struct{})
		go func() {
			select {
			case <-sigs:
				cancel()
			case <-stop:
			}
		}()
		_ = wc.st.Close(ctx)
		close(stop)
		cancel()
	}
	_ = wc.c.Close()
}

// readBound bounds each read the waiter makes (session.state, session.sync,
// session.snapshot, sessions.list), and askRetryEvery is how soon a look for
// an open ask whose read failed is made again (askCheck). Variables only so a
// test can shorten them.
var (
	readBound     = dialTimeout
	askRetryEvery = time.Second
)

// dialWait dials the host e names, as client what ("craze wait"), and attaches
// to session sessionID from cursor (nil: a snapshot), waiting for the
// session's start (when: ready). The dial is bounded by dialTimeout and the
// attach by ctx (--timeout's, when given). A failure is the command's exit-1
// error, carrying the attach's own refusal (exitError.cause): craze send
// tells a host closing its session (engine.ErrClosing), or one whose session
// has ended, from any other by it, and reloads the session (plan 041 §3.4).
func dialWait(ctx context.Context, e rundir.Entry, sessionID, what string, cursor *protocol.Cursor) (*waitConn, error) {
	c, err := dialCLI(ctx, e, what)
	if err != nil {
		if ctx.Err() != nil {
			return nil, kindf(errkind.TimeoutBeforePrompt, "%s: timed out before session %s answered", what, psShort(sessionID))
		}
		return nil, unreachable(what, e, err)
	}
	if cd := c.Hello().Codecs; cd.Event != agent.EventCodecVersion || cd.Snapshot != transcript.SnapshotVersion {
		// A stream this build cannot fold (remote.DialSession's own check).
		_ = c.Close()
		return nil, exitf(1, "%s: session %s runs in a craze whose stream this one cannot read (event codec %d, snapshot %d)",
			what, psShort(sessionID), cd.Event, cd.Snapshot)
	}
	st, err := c.Attach(ctx, remote.AttachOptions{SessionID: sessionID, Cursor: cursor, When: protocol.WhenReady})
	if err != nil {
		_ = c.Close()
		var re *remote.Error
		switch {
		case ctx.Err() != nil:
			return nil, kindf(errkind.TimeoutBeforePrompt, "%s: timed out before session %s answered", what, psShort(sessionID))
		case errors.As(err, &re) && re.Reason == protocol.ReasonStartFailed:
			return nil, &exitError{code: 1, msg: what + ": session " + psShort(sessionID) + " did not start: " + sanitizeLine(re.Cause), cause: err, kind: startKind(re.Kind)}
		case errors.Is(err, engine.ErrNotAccepting), errors.Is(err, remote.ErrSessionEnded):
			return nil, &exitError{code: 1, msg: what + ": " + endedLine(sessionID), cause: err, kind: errkind.Stopped}
		}
		return nil, unreachable(what, e, err)
	}
	return &waitConn{c: c, st: st, entry: e, sessionID: sessionID}, nil
}

// waitOptions shape one wait.
type waitOptions struct {
	// timeout fires at --timeout's end, counted from the command's start
	// (timeoutAt); nil waits as long as the turn runs. A channel, so a test
	// fires it when it means to.
	timeout <-chan time.Time
	// sigs delivers SIGINT, SIGTERM and SIGHUP: signal.Notify's channel in
	// production, a test's own channel in its tests.
	sigs <-chan os.Signal
}

// waitStart is where one wait starts. At most one of from and submit is set;
// with neither, the wait is plain: the session's current turn, or its last.
type waitStart struct {
	// from is wait --from's handle: the connection is attached at its cursor,
	// and the wait follows exactly its target.
	from *turnHandle
	// submit is the owned forms' prompt (C3): called once the attachment's
	// reply is in, on a goroutine of its own, with ident — the client
	// identity proven to be the attachment's incarnation's (proveIdentity),
	// which the prompt must be bound to (remote.CommandOptions.Identity;
	// submitPrompt): a prompt sent under another identity could run in
	// another incarnation, whose turn-1 is another turn, and the events the
	// stream buffered from the attachment's would then be taken for its. Its
	// answer — the turn the prompt started, or the row it became — is the
	// target, and the events the stream brought meanwhile are replayed
	// against it. An error is the command's exit-1 failure, its message as
	// the error says it.
	submit func(ctx context.Context, ident uint64) (waitTarget, error)
}

// owned says a signal acts on the wait's own target — the handle's, or the
// command's own submit's: wait --from, new --wait, send --wait (§3.1
// "Signals", P8).
func (s waitStart) owned() bool { return s.from != nil || s.submit != nil }

// waitFailure is a wait that ends in exit 1: why, what it was (kind), and the
// handle that names its target when there is one. follow says the target goes
// on in its host — the connection lost, the command interrupted, a cancel not
// confirmed — so the handle is worth following (`craze wait <id> --from
// <handle>`). No kind is internal, a site TestInternalFailureSitesAreListed
// lists.
type waitFailure struct {
	msg    string
	kind   errkind.Kind
	handle string
	follow bool
	// awaited is where the waiter's stream stood when it gave up on a submit's
	// answer, for a handle should the gate turn out to hold one (waitAfter).
	awaited turnHandle
	// cause is what began the session's close (kind stopped), when it was seen.
	// usage and inFlight are what was folded of a turn whose session ended with
	// no ending seen (sessionGone).
	cause    string
	usage    *agent.UsageState
	inFlight bool
}

// pumped is one stream item as the pump hands it over, or why the stream
// stopped.
type pumped struct {
	it  remote.Item
	err error
}

// waitInterrupt is a signal or --timeout's end, as the waiter's watch hands
// it to its loop (watchInterrupts).
type waitInterrupt struct{ timeout bool }

// submitAnswer is a submit's outcome, and the identity it was bound to.
type submitAnswer struct {
	target waitTarget
	ident  uint64
	err    error
}

// turnWaiter is one wait.
type turnWaiter struct {
	conn  *waitConn
	opts  waitOptions
	start waitStart

	// session is who the result is about; its model is read from
	// session.state and the stream's model deltas.
	session resultSession

	// inc is the incarnation of the stream the waiter folds. live says the
	// current attachment has caught up (synchronized): an ask opening from
	// here on is looked at at once.
	inc  string
	live bool
	// ident is the client identity (remote.Client.Identity) the waiter's
	// reads and commands are bound to, 0 until one is: an identity whose
	// connection is to inc's host incarnation, proven by a sessions.list
	// answered under it (bind). A reconnect that did not resume moves the
	// client to another identity — maybe another incarnation, whose turn-1
	// is another turn (P7) — and nothing the waiter reads or sends is ever
	// taken from or made on it until it is proven again.
	ident uint64

	// base is the wait's own context (waitTurn's): what the submit runs on.
	base context.Context
	// intr is what the watch (watchInterrupts) hands the loop: a signal, or
	// --timeout's end. askDue fires a look for an open ask again, after one
	// whose read failed (askLater); askTimer is its timer.
	intr     chan waitInterrupt
	askDue   <-chan time.Time
	askTimer *time.Timer
	// stale says the last look found the session past the target as the
	// stream then stood (askCheck): the target's next move looks again
	// (relook). expired says --timeout's end has come: the buffer replayed
	// after it moves the target, and nothing read then decides it (timedOut).
	stale   bool
	expired bool

	// The target: what the handle names, and so where the waiter is — none
	// yet, queued(row) while it is a row, running(turn) once it is a turn,
	// craze's own or the agent's. row is the row the target came from (""
	// for a turn submitted directly); rowSent says the row's `sent` has been
	// folded, so the next `started` of craze's own is its turn.
	target  waitTarget
	row     string
	rowSent bool
	// folding says the events folded now are the target turn's: from its
	// `started`, or from wherever the wait met it already running (a
	// snapshot, a handle's cursor mid-turn); before it, the waiter folds
	// nothing of the turn. startSeen says its `started` itself was folded.
	// partial says some of the turn's events were never seen.
	folding   bool
	startSeen bool
	partial   bool
	// anchor is the handle's seq: the cursor just before the target's first
	// event the wait relies on (turnHandle).
	anchor uint64
	acc    *turnAccumulator
	// lastQueued is the last queue `queued` event folded while the target ran
	// (id and seq): the row a restoring settlement put back is the event just
	// before its synthetic ending (P3).
	lastQueuedID  string
	lastQueuedSeq uint64
	// rowLinked says the handle's cursor (anchor) still replays the row's
	// link to its turn — it is before the row's `sent` — so the handle names
	// the row (handle); a turn met running mid-way (a reconcile's snapshot)
	// is named by the turn, whose cursor is past the link.
	rowLinked bool
	// foreign is the id of the agent's own turn running now, for blockedBy.
	foreign string
	// closing says a close's ending has gone by on the stream (any turn's, the
	// target still a row), and closeCause what it said began the stop: the
	// session is ending, whatever its stream does next.
	closing    bool
	closeCause string
	// withdrawn says the target's prompt never reached the agent (ended,
	// settleOutcome).
	withdrawn bool

	// submitted is set while a submit's reply is still out (buffering):
	// every item is kept in buffer, in order, and handed to item once the
	// reply names the target.
	submitted chan submitAnswer
	buffer    []pumped

	// idle is plain wait's look at a session idle at its attach (idleLook),
	// nil when it is not looking.
	idle *idleScan

	// cleaning says a signal's cleanup runs: no needs_input is reported, and
	// the target's ending is what is waited for. removeSent and cancelSent
	// say the row's removal and the turn's cancel have gone.
	cleaning   bool
	removeSent bool
	cancelSent bool

	// unsettled is the target's ending, read already, whose settlement an
	// interrupt cut short — its returned-plan lookup (returnedCancel) — set
	// until it is settled: nothing on the stream after it is the target's,
	// and the interrupt, already on its way, decides (run). An owned wait's
	// cleanup settles it on its own context (signalled), as it would an
	// ending it read itself.
	unsettled func(context.Context)

	// result and fail are the wait's end: one of them is set.
	result *turnResult
	fail   *waitFailure
}

// idleScan is plain wait's look at a session idle at its attach: the stream
// is folded up to until (session.sync's seq), and a turn that starts in it is
// followed; none, and the last turn is reported (finishIdle) from the state
// and the snapshot read at the attach.
type idleScan struct {
	st    *protocol.StateResult
	snap  *transcript.Snapshot
	until uint64
}

// newTurnWaiter is a wait over conn.
func newTurnWaiter(conn *waitConn, start waitStart, opts waitOptions) *turnWaiter {
	return &turnWaiter{
		conn:    conn,
		opts:    opts,
		start:   start,
		session: resultSession{id: conn.sessionID, provider: conn.entry.Provider},
		acc:     newTurnAccumulator(),
	}
}

// waitTurn is the waiter's entry point (the one C3's new --wait and send
// --wait call too): it follows start's target over conn, attached already,
// until the target's result, a failure, --timeout, or a signal, and answers
// either the result or the failure. It closes nothing: conn is the caller's.
func waitTurn(ctx context.Context, conn *waitConn, start waitStart, opts waitOptions) (*turnResult, *waitFailure) {
	return newTurnWaiter(conn, start, opts).run(ctx)
}

// run is waitTurn.
//
// The waiter's own work — every item it folds, every read it makes for its
// target — runs on op, which a signal or --timeout's end cancels at once
// (watchInterrupts): a read in flight then decides nothing, and the loop
// answers the interrupt rather than the read's ten seconds later. The pump
// runs on a context of its own, so a signal's cleanup goes on reading the
// stream after op has ended.
func (w *turnWaiter) run(ctx context.Context) (*turnResult, *waitFailure) {
	w.base = ctx
	pctx, stopPump := context.WithCancel(ctx)
	defer stopPump()
	op, endOp := context.WithCancel(ctx)
	defer endOp()
	w.intr = make(chan waitInterrupt, 4)
	stopWatch, watched := make(chan struct{}), make(chan struct{})
	go func() {
		watchInterrupts(w.opts, endOp, w.intr, stopWatch)
		close(watched)
	}()
	defer func() {
		// A signal the watch handed over that the loop never answered came
		// as the wait ended — a second one, or one with the result: the
		// command exits at once, without the detach. One after the watch has
		// stopped is the close's (waitConn.close).
		close(stopWatch)
		<-watched
		for {
			select {
			case in := <-w.intr:
				if !in.timeout {
					w.conn.abandon = true
				}
				continue
			default:
			}
			return
		}
	}()
	defer func() {
		if w.askTimer != nil {
			w.askTimer.Stop()
		}
	}()
	items := make(chan pumped)
	go pumpStream(pctx, w.conn.st, items)
	take := func(p pumped) {
		if w.buffering() {
			// Kept, the stream's end and loss included, until the reply
			// names the target: an ending already on the stream is then
			// still the target's, and reported before the loss.
			w.buffer = append(w.buffer, p)
			waitBuffered(p)
			return
		}
		w.item(op, p)
		waitTaken(p)
	}
	for !w.done() {
		if w.unsettled != nil {
			// The target's ending is read and its settlement was cut short by
			// the interrupt, which is on its way (the work's context ended
			// first): nothing the stream brings now is the target's.
			select {
			case in := <-w.intr:
				if in.timeout {
					w.timedOut(items)
				} else {
					w.signalled(items)
				}
			case <-ctx.Done():
				w.fail = w.interrupted(ctx.Err().Error())
			}
			continue
		}
		// What the stream has handed over already goes first: an ending that
		// came before the timeout, or before a signal, is the result.
		select {
		case p := <-items:
			take(p)
			continue
		default:
		}
		select {
		case p := <-items:
			take(p)
		case a := <-w.submitted:
			w.submitAnswered(op, a)
		case in := <-w.intr:
			if in.timeout {
				w.timedOut(items)
			} else {
				w.signalled(items)
			}
		case <-w.askDue:
			w.askDue = nil
			w.askCheck(op)
		case <-ctx.Done():
			w.fail = w.interrupted(ctx.Err().Error())
		}
	}
	if w.result != nil && w.session.model == "" && !w.closing {
		// Nothing the wait read named the session's model — a replay from a
		// handle that ended before its attachment caught up reads nothing:
		// one bound read of the session's state names it, on the wait's own
		// work (op), which an interrupt has ended already. Not of a session
		// that is closing: nothing is left to answer it, and it would only
		// hold the result back.
		if _, err := w.state(op); err == nil {
			w.result.Model = w.session.model
		}
	}
	return w.result, w.fail
}

// watchInterrupts hands each signal, and --timeout's end, to the waiter's
// loop (out), ending its work (end) first, until stop is closed.
func watchInterrupts(o waitOptions, end context.CancelFunc, out chan<- waitInterrupt, stop <-chan struct{}) {
	timeout := o.timeout
	hand := func(in waitInterrupt) {
		end()
		select {
		case out <- in:
		default:
			// The loop has four not yet taken: one more says nothing new.
		}
	}
	for {
		select {
		case <-o.sigs:
			hand(waitInterrupt{})
		case <-timeout:
			timeout = nil
			hand(waitInterrupt{timeout: true})
		case <-stop:
			return
		}
	}
}

// done says the wait has its end: a result or a failure.
func (w *turnWaiter) done() bool { return w.result != nil || w.fail != nil }

// buffering says a submit's reply is still out.
func (w *turnWaiter) buffering() bool { return w.submitted != nil }

// waitBuffered sees each item the waiter buffers while a submit's reply is
// out — the stream's loss among them: a no-op, and the barrier a test holds a
// reply on until the events it wants ahead of it have been buffered.
var waitBuffered = func(pumped) {}

// waitTaken sees each item the waiter's loop has folded (item), once it has:
// a no-op, and the barrier a test ends --timeout after (timeoutAt) once the
// waiter has met its target — its attachment's reply folded.
var waitTaken = func(pumped) {}

// timeoutAt is --timeout's channel for a deadline (zero: none) and what
// stops its timer. A variable only so a test can end --timeout when it means
// to, not when a wall clock does.
var timeoutAt = func(deadline time.Time) (<-chan time.Time, func()) {
	if deadline.IsZero() {
		return nil, func() {}
	}
	t := time.NewTimer(time.Until(deadline))
	return t.C, func() { t.Stop() }
}

// submitPrompt is the owned forms' submit (waitStart.submit; C3's new --wait
// and send --wait): session.prompt, mode queue, over the wait's own
// connection, under a command id its client mints, bound to the identity
// ident the waiter proved (remote.CommandOptions.Identity). A prompt that
// started is its turn; one that queued, its row. An answer lost with its
// connection (resume_lost), or a prompt the client left its identity before
// it was sent, is never sent again (P7): the prompt may not have reached the
// session.
func submitPrompt(conn *waitConn, text string) func(context.Context, uint64) (waitTarget, error) {
	return func(ctx context.Context, ident uint64) (waitTarget, error) {
		var res protocol.PromptResult
		_, err := conn.c.Command(ctx, protocol.MethodSessionPrompt,
			protocol.PromptParams{SessionID: conn.sessionID, Text: text, Mode: protocol.PromptQueue}, &res, remote.CommandOptions{Identity: ident})
		follow := "; craze wait " + psShort(conn.sessionID) + " follows the session"
		switch {
		case errors.Is(err, remote.ErrOutcomeUnknown):
			return waitTarget{}, &submitError{msg: "the prompt may not have reached it" + follow, err: err, kind: errkind.Unobserved}
		case refusedByHost(err):
			return waitTarget{}, &submitError{msg: "the prompt was refused: " + sanitizeLine(err.Error()), err: err, kind: refusalKind(err)}
		case err != nil:
			// Not the host's refusal: a reply this client cannot read, or a
			// failure after the request may have gone — the prompt may have
			// run (C3 review r1, 5).
			return waitTarget{}, &submitError{msg: "the prompt's answer cannot be read (" + sanitizeLine(err.Error()) +
				"); it may have reached the session" + follow, err: err, kind: errkind.Unobserved}
		case res.Turn != "":
			return waitTarget{kind: targetTurn, id: res.Turn}, nil
		case res.Queued != nil:
			row, err := agent.DecodeQueuedPrompt(res.Queued)
			if err != nil || row.ID == "" {
				return waitTarget{}, &submitError{msg: "the prompt's queued row cannot be read; it may have reached the session" + follow, kind: errkind.Unobserved}
			}
			return waitTarget{kind: targetRow, id: row.ID}, nil
		}
		return waitTarget{}, &submitError{msg: "the prompt's answer names no turn and no queued row; it may have reached the session" + follow, kind: errkind.Unobserved}
	}
}

// submitError is submitPrompt's failure: its words, as the command says them,
// what it was, and the client's error behind them, so a caller tells an answer
// lost with its connection (remote.ErrOutcomeUnknown: the prompt may have run)
// from a prompt that never ran (submitGate.close).
type submitError struct {
	msg  string
	err  error
	kind errkind.Kind
}

func (e *submitError) Error() string { return e.msg }
func (e *submitError) Unwrap() error { return e.err }

// ErrorInfo is what the submit's failure was.
func (e *submitError) ErrorInfo() errkind.Info { return errkind.New(e.kind) }

// refusalKind is what the host's refusal of a prompt was: a session closing or
// closed is stopped, one that cannot start its start's, a prompt the host's own
// bound refuses the prompt's, a session it does not serve config's, and a full
// queue or the agent's own turn holding the session the agent's. Anything else
// is craze's own.
func refusalKind(err error) errkind.Kind {
	var re *remote.Error
	if !errors.As(err, &re) {
		return errkind.Internal
	}
	switch re.Reason {
	case protocol.ReasonNotAccepting, protocol.ReasonClosing, protocol.ReasonNotResumable:
		return errkind.Stopped
	case protocol.ReasonStartFailed:
		return startKind(re.Kind)
	case protocol.ReasonTextTooLong:
		return errkind.Prompt
	case protocol.ReasonUnknownSession:
		return errkind.Config
	case protocol.ReasonQueueFull, protocol.ReasonForeignTurn, protocol.ReasonPromptInFlight:
		return errkind.Agent
	}
	return errkind.Internal
}

// startKind is what a failed start said of itself, or start_failed from a host
// that does not say.
func startKind(kind string) errkind.Kind {
	if kind == "" {
		return errkind.StartFailed
	}
	return errkind.Info{Kind: errkind.Kind(kind)}.Known().Kind
}

// pumpStream hands the stream's items over, in order, until ctx ends or the
// stream stops.
func pumpStream(ctx context.Context, st *remote.Stream, out chan<- pumped) {
	for {
		it, err := st.Next(ctx)
		select {
		case out <- pumped{it: it, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// --------------------------------------------------------------- the stream

// item is one stream item.
func (w *turnWaiter) item(ctx context.Context, p pumped) {
	if p.err != nil {
		w.lost(p.err)
		return
	}
	it := p.it
	switch it.Kind {
	case remote.KindAttached:
		w.attached(ctx, it.Reply)
	case remote.KindRestore:
		w.restored(ctx, it.Reply)
	case remote.KindEvent:
		ev, err := agent.DecodeEvent(string(it.Body))
		if err != nil {
			w.fail = &waitFailure{msg: "event " + strconv.FormatUint(it.Seq, 10) + " cannot be read: " + sanitizeLine(err.Error()), kind: errkind.Internal, handle: w.handle()}
			return
		}
		ev.Seq = it.Seq
		w.event(ctx, ev)
	case remote.KindSynchronized:
		w.live = true
		w.synchronized(ctx)
	case remote.KindReady:
		if r := it.Ready; r != nil && r.StartFailed {
			w.fail = &waitFailure{msg: "the session did not start: " + sanitizeLine(r.Err), kind: startKind(r.Kind)}
		}
	case remote.KindEnd:
		// The handle still names the target (§3.1: every exit 1 after one
		// has a target carries it), whose ending the stream never brought.
		// The host itself said the session has closed, so its process is not
		// asked: it may yet be on its way out. Its record says of the target
		// what the stream did not.
		if rec, ok := rundir.ReadStopRecord(rundir.ProcessEnv(), w.conn.entry.HostID); ok && !w.closing {
			w.stopUnseen(&rec)
			return
		}
		w.sessionGone(endedLine(w.session.id), errkind.Stopped)
	case remote.KindError:
		w.lost(it.Err)
	}
}

// lost is the stream given up: its redials spent, or the stream broken. Only what
// says a stop began makes it one: a close seen on the stream, the host's stop
// record, its registry entry marked stopping. A registry entry that is merely
// missing says nothing — a host that is killed has its entry swept, by this very
// read or anyone's — so with no such word the host's process decides, read after
// the registry: one gone exited with no ending seen, and what was folded of the
// turn is all there will be; one running has a connection lost and a turn that
// runs on (the handle follows it); one that cannot be named is never host_exited,
// and nothing is claimed of its turn.
func (w *turnWaiter) lost(err error) {
	if err == nil || errors.Is(err, remote.ErrStreamClosed) {
		err = errors.New("the stream closed")
	}
	e := w.conn.entry
	if w.closing {
		w.sessionGone(endedLine(w.session.id), errkind.Stopped)
		return
	}
	if rec, ok := rundir.ReadStopRecord(rundir.ProcessEnv(), e.HostID); ok {
		w.stopUnseen(&rec)
		return
	}
	if hostMarkedStopping(e) {
		w.stopUnseen(nil)
		return
	}
	lost := "lost the connection to session " + psShort(w.session.id) + ": " + sanitizeLine(err.Error())
	switch readHost(e) {
	case hostExited:
		w.sessionGone("the session ended: its host exited with no ending for the turn"+reloadHint(matchCrazeID(indexCrazeIDs(), w.session.id)), errkind.HostExited)
	case hostRunning:
		w.fail = &waitFailure{msg: lost + "; the turn runs on in its host", kind: errkind.ConnectionLost, handle: w.handle(), follow: true}
	default:
		w.fail = &waitFailure{msg: lost + "; its host's state is unknown, and the turn may have ended", kind: errkind.ConnectionLost, handle: w.handle(), follow: true}
	}
}

// stopUnseen is a stop whose ending never reached the waiter: a backlog the host's
// flush gave up on, a subscription dropped for overflow, a reconnect its attach
// fence refused. rec is what the host wrote as it began, nil when only its
// registry entry says. The target turn, the one the stop found running as far as
// it can be told, has the result its ending would have given, from what was
// folded; anything else is the session's end, the host's word on the target being
// only that it is over.
func (w *turnWaiter) stopUnseen(rec *rundir.StopRecord) {
	w.closing = true
	at := time.Now()
	if rec != nil {
		w.closeCause, at = rec.Cause, rec.At
	}
	if w.target.kind != targetTurn || (rec != nil && rec.Turn != w.target.id) {
		w.sessionGone(endedLine(w.session.id), errkind.Stopped)
		return
	}
	w.partial = true
	w.acc.end(at)
	w.closed(w.closeCause)
}

// sessionGone is the session's end with no ending for the target: exit 1 as kind,
// with the handle, what began its close if that was seen, and — a turn's — what
// was folded of its usage, and whether a step was in flight when the stream ended.
func (w *turnWaiter) sessionGone(msg string, kind errkind.Kind) {
	f := &waitFailure{msg: msg, kind: kind, handle: w.handle(), cause: w.closeCause}
	if k := w.target.kind; k == targetTurn || k == targetForeign {
		f.usage, f.inFlight = w.acc.usage, w.stepInFlight()
	}
	w.fail = f
}

// attached is the first attachment's reply: where the stream starts, and —
// per the start — the target.
func (w *turnWaiter) attached(ctx context.Context, r *protocol.AttachResult) {
	w.inc, w.anchor = r.After.Incarnation, r.After.Seq
	w.adoptInfo(r.Session)
	switch {
	case w.start.from != nil:
		h := w.start.from
		w.setTarget(h.target)
		if h.row != "" {
			// A turn that came from a queued row, its handle's place past
			// the row's link to it: the row is the target's still, should the
			// agent's own turn refuse the turn and put it back (restoring).
			w.row = h.row
		}
		if r.Snapshot == nil {
			// The cursor was honoured: the stream goes on from the handle.
			// A turn's handle is just before its started, or — met running
			// — anywhere in it: every event from the cursor to its ending is
			// the turn's. Whole only if the started is the first of them
			// (runningEvent).
			w.anchor = h.seq
			if h.target.kind != targetRow {
				w.folding, w.partial = true, true
			}
			return
		}
		// A replay the host refused: what the snapshot says of the target.
		w.inc, w.anchor = h.incarnation, h.seq
		w.reconcile(ctx, r)
	case w.start.submit != nil:
		// The channel alone, not the waiter, goes to the submit's goroutine:
		// a wait that ended first keeps nothing of its own alive. The submit
		// runs on the wait's own context, never the work's (op): a signal or
		// --timeout waits for its reply (§3.1 "Signals") rather than cut it
		// off, which would leave the prompt's fate unknown. It is bound to an
		// identity proven to be the attachment's incarnation's first: one the
		// client has left by the time it is written is never sent at all
		// (remote's BOUND TO AN IDENTITY), so a prompt never runs in an
		// incarnation other than the one whose events the waiter buffers.
		ch, submit, base := make(chan submitAnswer, 1), w.start.submit, w.base
		c, sid, inc := w.conn.c, w.session.id, w.inc
		w.submitted = ch
		go func() {
			pctx, cancel := context.WithTimeout(base, readBound)
			ident, err := proveIdentity(pctx, c, sid, inc)
			cancel()
			switch {
			case errors.Is(err, errNotThisHost):
				ch <- submitAnswer{err: &submitError{msg: "the session's host changed before the prompt was sent; it was not sent", err: errNotThisHost, kind: errkind.ConnectionLost}}
				return
			case err != nil:
				ch <- submitAnswer{err: &submitError{msg: "the session did not answer before the prompt was sent; it was not sent: " + sanitizeLine(err.Error()), err: err, kind: errkind.ConnectionLost}}
				return
			}
			t, err := submit(base, ident)
			ch <- submitAnswer{target: t, ident: ident, err: err}
		}()
	default:
		w.plainStart(ctx, r)
	}
}

// setTarget makes t the target: a row's id is the row it came from too.
func (w *turnWaiter) setTarget(t waitTarget) {
	w.target = t
	if t.kind == targetRow {
		w.row, w.rowLinked = t.id, true
	}
}

// requeue is the target's row waiting again, the stream from seq: its next
// run is the target, folded afresh — and, in a signal's cleanup, a row to
// remove again and a turn to cancel again (the removal of the row as it was
// sent, and the cancel of the turn the agent refused, have found nothing).
func (w *turnWaiter) requeue(row string, seq uint64) {
	w.setTarget(waitTarget{kind: targetRow, id: row})
	w.rowSent, w.folding, w.startSeen, w.anchor = false, false, false, seq
	w.removeSent, w.cancelSent = false, false
	w.acc = newTurnAccumulator()
}

// adoptInfo takes the session's facts from an info document.
func (w *turnWaiter) adoptInfo(info protocol.SessionInfo) {
	if info.SessionID != "" {
		w.session.id = info.SessionID
	}
	if info.Provider.Name != "" {
		w.session.provider = info.Provider.Name
	}
}

// restored is a re-attach's snapshot (internal/remote's Restore): the stream
// goes on from it, and whatever it skipped is reconciled.
func (w *turnWaiter) restored(ctx context.Context, r *protocol.AttachResult) {
	w.live = false
	// Whatever identity read for the waiter before, the one after a restore
	// is proven again before anything is read or sent on it (bind).
	w.ident = 0
	w.adoptInfo(r.Session)
	w.reconcile(ctx, r)
}

// reconcile is the target as a snapshot shows it (P7): a snapshot of another
// incarnation never shows the same turn — the ending was not observed;
// otherwise the target's row back in the queue — restored after the agent's
// own turn refused it (P3), whatever the target had become — is waited for
// again, the target still running in it is followed on from it (partial),
// and a turn that ended is settled from session.state's lastTurn (partial),
// read under an identity proven to be this incarnation's (read). Anything
// else is an ending not observed; a read that could not be made under such
// an identity decides nothing — the stream's next restore (the identity
// moved) or the interrupt that ended it does.
func (w *turnWaiter) reconcile(ctx context.Context, r *protocol.AttachResult) {
	if w.target.kind == targetNone {
		// Nothing followed yet (plain wait's look, before a target): start
		// again from this snapshot.
		w.inc, w.anchor = r.After.Incarnation, r.After.Seq
		w.idle = nil
		w.plainStart(ctx, r)
		return
	}
	if r.After.Incarnation != w.inc {
		w.notObserved()
		return
	}
	snap, ok := w.snapshot(r)
	if !ok {
		return
	}
	w.partial = true
	if w.row != "" && slices.ContainsFunc(snap.Queue, func(q agent.QueuedPrompt) bool { return q.ID == w.row }) {
		w.requeue(w.row, r.After.Seq)
		return
	}
	// The same incarnation (above), and the same turn: what the waiter
	// folded of it that no snapshot carries is kept (followFrom).
	switch w.target.kind {
	case targetTurn:
		if snap.Turn.ID == w.target.id {
			w.followFrom(snap, r.After.Seq, true)
			return
		}
	case targetForeign:
		if f := snap.Turn.Foreign; f != nil && f.Running && f.ID == w.target.id {
			w.followFrom(snap, r.After.Seq, true)
			return
		}
	}
	if w.target.kind == targetRow {
		w.notObserved()
		return
	}
	if w.expired {
		// Past --timeout's end nothing read decides the target (timedOut):
		// it is running, with the handle, which a later wait reconciles.
		return
	}
	st, err := w.state(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		// The interrupt's: the loop answers it.
		return
	case errors.Is(err, errNotThisHost):
		// The host now is another incarnation: what became of the turn
		// can no longer be learnt.
		w.notObserved()
		return
	case err != nil:
		w.fail = &waitFailure{msg: "the turn's ending was not observed: " + sanitizeLine(err.Error()), kind: errkind.Unobserved, handle: w.handle()}
		return
	}
	lt := st.LastTurn
	if lt == nil || lt.TurnID != w.target.id {
		w.notObserved()
		return
	}
	w.settleLastTurn(ctx, lt, snap)
}

// undecided says a read's failure decides nothing for plain wait's look at
// an idle session (idleLook), which has no target yet: the waiter's work was
// interrupted (a signal, --timeout), which the loop answers, or the client
// has left the identity the read had to be made under (errNotThisHost), and
// the stream's next restore starts the look again.
func undecided(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, errNotThisHost))
}

// interrupted is the command's signal ending the wait with the target going on
// in its host, so the handle is worth following.
func (w *turnWaiter) interrupted(msg string) *waitFailure {
	return &waitFailure{msg: msg, kind: errkind.Interrupted, handle: w.handle(), follow: true}
}

// notObserved is a target whose ending the waiter cannot learn: exit 1 with
// the handle.
func (w *turnWaiter) notObserved() {
	w.fail = &waitFailure{msg: "the turn's ending was not observed", kind: errkind.Unobserved, handle: w.handle()}
}

// snapshot decodes an attach reply's snapshot; a reply that cannot be read
// fails the wait.
func (w *turnWaiter) snapshot(r *protocol.AttachResult) (*transcript.Snapshot, bool) {
	if r.Snapshot == nil {
		w.fail = &waitFailure{msg: "the host sent no snapshot to start from", kind: errkind.Internal, handle: w.handle()}
		return nil, false
	}
	snap, err := transcript.DecodeSnapshot(r.Snapshot)
	if err != nil {
		w.fail = &waitFailure{msg: "the session's snapshot cannot be read: " + sanitizeLine(err.Error()), kind: errkind.Internal, handle: w.handle()}
		return nil, false
	}
	if snap.Settings.Model != "" {
		w.session.model = snap.Settings.Model
	}
	return snap, true
}

// plainStart is plain wait's start (§3.1): the turn the snapshot shows running
// — craze's own or the agent's — followed from it (partial); with none, the
// session is looked at once more (idleLook).
func (w *turnWaiter) plainStart(ctx context.Context, r *protocol.AttachResult) {
	snap, ok := w.snapshot(r)
	if !ok {
		return
	}
	switch {
	case snap.Turn.ID != "":
		w.target = waitTarget{kind: targetTurn, id: snap.Turn.ID}
		w.partial = true
		w.followFrom(snap, r.After.Seq, false)
	case snap.Turn.Foreign != nil && snap.Turn.Foreign.Running:
		w.target = waitTarget{kind: targetForeign, id: snap.Turn.Foreign.ID}
		w.foreign = snap.Turn.Foreign.ID
		w.partial = true
		w.followFrom(snap, r.After.Seq, false)
	default:
		w.idleLook(ctx, snap, r.After.Seq)
	}
}

// idleLook is plain wait at a session that was idle at its attach, at seq:
// its state (the last turn), then session.sync — every event committed by
// then is on this stream before its reply — and the stream folded through
// that seq. A turn that started meanwhile is followed; with none, the last
// turn is the result (finishIdle).
func (w *turnWaiter) idleLook(ctx context.Context, snap *transcript.Snapshot, seq uint64) {
	st, err := w.state(ctx)
	switch {
	case undecided(ctx, err):
		return
	case err != nil:
		w.fail = &waitFailure{msg: "session " + psShort(w.session.id) + " did not answer: " + sanitizeLine(err.Error()), kind: errkind.ConnectionLost}
		return
	}
	if st.StartFailed {
		w.fail = &waitFailure{msg: "the session did not start: " + sanitizeLine(st.Err), kind: errkind.StartFailed}
		return
	}
	var sync protocol.SyncResult
	err = w.read(ctx, protocol.MethodSessionSync, protocol.SyncParams{SessionID: w.session.id}, &sync)
	switch {
	case undecided(ctx, err):
		return
	case err != nil:
		w.fail = &waitFailure{msg: "session " + psShort(w.session.id) + " did not answer: " + sanitizeLine(err.Error()), kind: errkind.ConnectionLost}
		return
	}
	look := &idleScan{st: st, snap: snap, until: sync.Seq}
	if sync.Seq <= seq {
		w.finishIdle(ctx, look)
		return
	}
	w.idle = look
}

// finishIdle is plain wait's answer for a session nothing runs in (§3.1):
// idle when it has had no turn (or its host does not say how its last one
// ended), and otherwise the last turn, settled from lastTurn and the
// snapshot (partial: an idle wait has no stop reason).
func (w *turnWaiter) finishIdle(ctx context.Context, look *idleScan) {
	w.idle = nil
	st, snap := look.st, look.snap
	if st.LastTurn == nil {
		w.result = newResult(w.session, statusIdle)
		return
	}
	lt := st.LastTurn
	w.target = waitTarget{kind: targetTurn, id: lt.TurnID}
	if f := snap.Turn.Foreign; f != nil && f.ID == lt.TurnID {
		w.target.kind = targetForeign
	} else if !snap.Turn.At.IsZero() {
		w.acc.start(snap.Turn.At)
	}
	w.partial = true
	w.settleLastTurn(ctx, lt, snap)
}

// settleLastTurn is the target's result from how the session says its last
// turn ended, and what the snapshot holds of it. The snapshot's usage is never
// the result's: it is the last report before the snapshot, which need not be
// this turn's at all — a turn withdrawn before it opened, or one that failed
// before its first step, reports none, and the one before it stands (P6). A
// usage the waiter folded from the turn's own events, before a gap, stays.
func (w *turnWaiter) settleLastTurn(ctx context.Context, lt *protocol.LastTurn, snap *transcript.Snapshot) {
	seedFromSnapshot(w.acc, snap)
	w.acc.end(lt.EndedAt)
	w.partial = true
	w.settleOutcome(ctx, lt)
}

// settleOutcome is settleLastTurn's result from lastTurn's outcome. A
// cancelled turn is settled by the one rule for a cancelled ending
// (returnedCancel): completed when its plan was returned; one whose lookup an
// interrupt cut short is left unsettled for the interrupt to decide. A turn that
// finished may have finished at its approved plan (lookApproved).
func (w *turnWaiter) settleOutcome(ctx context.Context, lt *protocol.LastTurn) {
	if lt.StopReason == stopClosing {
		w.closed(lt.Cause)
		return
	}
	w.withdrawn = lt.Withdrawn
	status := statusCompleted
	switch lt.Outcome {
	case protocol.TurnDone:
		w.lookApproved(ctx, lt.TurnID, lt.StopReason)
	case protocol.TurnFailed:
		status = statusFailed
	case protocol.TurnCancelled:
		returned, decided := w.returnedCancel(ctx, lt.TurnID)
		if !decided {
			w.unsettled = func(ctx context.Context) { w.settleOutcome(ctx, lt) }
			return
		}
		status = statusCancelled
		if returned {
			status = statusCompleted
		}
	}
	res := w.newResult(status)
	res.StopReason = lt.StopReason
	if lt.Err != "" {
		res.Error = newResultError(lt.Err, remote.LastTurnInfo(lt))
	}
	w.finish(res)
}

// returnedCancel is the one rule for a cancelled ending of the target turn
// (X28), wherever it is settled — live (ended), or from lastTurn after a
// restore, a reconcile or an idle look (settleLastTurn): whether the host
// returned the turn's plan and cancelled the turn itself, so that the plan is
// the turn's result and the ending is completed. The waiter's own mark says
// so: the plan it folded, kept across a snapshot of the same turn
// (followFrom). Without it, a waiter that has not seen the whole turn
// (partial: it met the turn running, or a gap came) looks for the plan in the
// turn's events as the host still holds them (lookReturned) — a snapshot
// never carries it; one that folded every event of the turn has read
// everything the lookup could, and has its answer. A turn whose events are
// gone is what can be proven: cancelled.
//
// decided is false when the lookup was interrupted — a signal, --timeout's
// end — or would be a read past --timeout's end (expired): interrupted is
// not "not found", and the interrupt decides, exactly as if it had come a
// step earlier (unsettled).
func (w *turnWaiter) returnedCancel(ctx context.Context, turn string) (returned, decided bool) {
	switch {
	case w.target.kind != targetTurn:
		return false, true
	case w.acc.returned:
		return true, true
	case !w.partial:
		return false, true
	case w.expired:
		return false, false
	}
	found, err := w.lookReturned(ctx, turn)
	if err != nil {
		return false, false
	}
	return found, true
}

// lookReturned looks for turn's returned plan in its events (readTurnBack) and,
// found, folds it into the accumulator as the live wait would have: the plan,
// its automatic answer, and the mark. err is the lookup's interruption (its
// context's error), never a plan not found.
func (w *turnWaiter) lookReturned(ctx context.Context, turn string) (bool, error) {
	back, err := readTurnBack(ctx, w.conn.entry, w.session.id, w.inc, turn)
	p := back.returned
	if p == nil {
		return false, err
	}
	w.acc.plan = p.Plan
	w.acc.auto = append(w.acc.auto, autoPlan(p))
	w.acc.returned = true
	return true, nil
}

// lookApproved is a turn that finished with end_turn, and that the waiter has
// not seen whole, asked whether it finished at its approved plan: read from the
// turn's events as the host still holds them (readTurnBack), since neither
// lastTurn nor a snapshot says, and an automatic plan is not in a snapshot at all.
// Only when a snapshot does not rule it out by naming no plan's ask as ended,
// so that a wait on an ordinary turn dials nothing more. Nothing here decides the
// turn's status: a lookup that is cut short, past --timeout's end or with the
// events gone, leaves a turn that finished finished, and planEnded unsaid.
func (w *turnWaiter) lookApproved(ctx context.Context, turn, stop string) {
	if w.target.kind != targetTurn || !w.partial || w.expired || w.acc.noPlanEnded || w.acc.planApproved || w.acc.returned ||
		(stop != "" && stop != stopEndTurn) {
		return
	}
	back, err := readTurnBack(ctx, w.conn.entry, w.session.id, w.inc, turn)
	if whole := back.whole; err == nil && whole != nil && whole.planApproved {
		w.acc.planApproved, w.acc.plan, w.acc.auto = true, whole.plan, whole.auto
	}
}

// waitLookingBack sees the turn's lookup (readTurnBack) about to make its own
// attach, on the lookup's context: a no-op, and the barrier a test stalls that
// attach at. waitLookedBack sees it read a returned plan, before it answers: a
// no-op, and the barrier a test ends the waiter's work at.
var (
	waitLookingBack = func(context.Context) {}
	waitLookedBack  = func(context.Context) {}
)

// turnBack is what a turn's events said, read back: the plan an unattended host
// returned in it, or, read through to its ending, the whole turn folded as a
// live wait would have folded it. Either is nil for not found.
type turnBack struct {
	returned *agent.PlanEvent
	whole    *turnAccumulator
}

// readTurnBack reads turn of incarnation inc from the session's events as its
// host still holds them, on a connection of its own, attached at the very start
// of inc's stream (cursor seq 0) and read until it has caught up, within
// readBound and ctx. A plan the host returned (X28) — an automatic plan answer
// that did not accept, between the turn's started and its ended — ends the
// reading: found while ctx stands, it is the answer. Otherwise the turn's ending
// does, and the folded turn is the answer. With neither, what comes back is
// empty and the error says which: nil is not found — a replay the host refuses
// (the events evicted), another incarnation's answer, none such in the turn, or
// no answer within readBound — and ctx's own error is interrupted: the waiter's
// work ended under the lookup (a signal, --timeout's end), which decides nothing
// about the plan, found or not. No protocol change: an attach with a cursor is
// what wait --from makes.
func readTurnBack(ctx context.Context, e rundir.Entry, sessionID, inc, turn string) (turnBack, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, readBound)
	defer cancel()
	miss := func() (turnBack, error) { return turnBack{}, parent.Err() }
	c, err := dialCLI(ctx, e, waitWhat)
	if err != nil {
		return miss()
	}
	defer func() { _ = c.Close() }()
	waitLookingBack(ctx)
	st, err := c.Attach(ctx, remote.AttachOptions{SessionID: sessionID, Cursor: &protocol.Cursor{Incarnation: inc}, When: protocol.WhenNow})
	if err != nil {
		return miss()
	}
	in, whole := false, newTurnAccumulator()
	for {
		it, err := st.Next(ctx)
		if err != nil {
			return miss()
		}
		switch it.Kind {
		case remote.KindAttached:
			if r := it.Reply; r == nil || r.Snapshot != nil || r.After.Incarnation != inc {
				return miss()
			}
		case remote.KindEvent:
			ev, err := agent.DecodeEvent(string(it.Body))
			if err != nil {
				return miss()
			}
			ev.Seq = it.Seq
			switch tu := ev.Turn; {
			case ev.Type == agent.EventTurn && tu != nil && tu.ID == turn && tu.Phase == agent.TurnStarted:
				in = true
			case ev.Type == agent.EventTurn && tu != nil && tu.ID == turn && tu.Phase == agent.TurnEnded:
				if !in {
					return miss()
				}
				return turnBack{whole: whole}, parent.Err()
			case in && ev.Type == agent.EventPlan && ev.Plan != nil && ev.Plan.Auto && !ev.Plan.Accepted && ev.Agent == "":
				waitLookedBack(ctx)
				if err := parent.Err(); err != nil {
					// The work ended under the lookup before it answered.
					// remote's Attach and Next hand over what has arrived
					// whatever their context, so a lookup the interrupt
					// cut short — even at its attach — can still read the
					// plan: interrupted all the same, as a miss then is.
					return turnBack{}, err
				}
				return turnBack{returned: ev.Plan}, nil
			case in:
				whole.fold(ev)
			}
		case remote.KindReady, remote.KindPresence:
		default:
			// Caught up (synchronized), restored, ended or lost: not found.
			return miss()
		}
	}
}

// followFrom follows the target from a snapshot that shows it running: its
// turn so far as the snapshot holds it (seedFromSnapshot), the stream from
// seq. same says the snapshot is of the turn the waiter was folding already,
// in the same incarnation (reconcile): what it folded that no snapshot
// carries is kept — the turn's automatic answers (P9); when the host returned
// its plan (X28), the plan and the mark that its cancelled ending is the plan's
// delivery (returnedCancel); and when it saw the plan approved, the plan and the
// approval, unless the snapshot shows work the approval did not end.
func (w *turnWaiter) followFrom(snap *transcript.Snapshot, seq uint64, same bool) {
	prev := w.acc
	w.folding = true
	w.acc = newTurnAccumulator()
	if w.target.kind == targetTurn && !snap.Turn.At.IsZero() {
		w.acc.start(snap.Turn.At)
	}
	seedFromSnapshot(w.acc, snap)
	if same {
		w.acc.auto = append(prev.auto, w.acc.auto...)
		// A report carries the whole turn's spend so far, and no snapshot is
		// read for it (settleLastTurn).
		w.acc.usage = prev.usage
		w.acc.retries = max(w.acc.retries, prev.retries)
		if prev.returned {
			w.acc.returned, w.acc.plan = true, prev.plan
		}
		if prev.planApproved && !w.acc.workSince(prev) {
			w.acc.planApproved, w.acc.plan = true, prev.plan
		}
	}
	// The cursor is past the row's `sent` and the turn's `started` now: the
	// handle names the turn (rowLinked).
	w.anchor, w.rowLinked = seq, false
}

// seedFromSnapshot folds into a what a snapshot's main transcript holds of its
// last turn: every entry after the turn's boundary — its user row, or the
// note a foreign turn opens with — the last assistant entry its message (the
// open run when the snapshot ends inside it), its tool rows and its plan; and
// whether any plan's ask has ended at all (lookApproved). A row a retried step's
// retraction marked is none of it, and an attempt the snapshot was cut in the
// middle of is retracted when its event arrives (retract).
func seedFromSnapshot(a *turnAccumulator, snap *transcript.Snapshot) {
	a.noPlanEnded = !slices.ContainsFunc(snap.Ended, func(e transcript.AskEnding) bool { return e.Kind == agent.AskPlan })
	a.retries = max(a.retries, snap.Turn.Retries)
	entries := snap.Main.Entries
	from := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if turnBoundary(entries[i]) {
			from = i + 1
			break
		}
	}
	seg := entries[from:]
	for i, e := range seg {
		if e.Retracted {
			continue
		}
		a.seq = e.ID.Seq
		switch {
		case e.Kind == transcript.KindAssistant:
			open := e.Streaming && snap.Main.StreamOpen && i == len(seg)-1
			a.seedRun(e.ID.Seq, e.Text, open, e.Cut || (e.Streaming && snap.Main.TailCut))
		case e.Kind == transcript.KindTool && e.Tool != nil:
			a.tool(*e.Tool)
		case e.Kind == transcript.KindPlan && e.Plan != nil:
			a.plan = e.Plan.Plan
		}
	}
}

// turnBoundary reports whether e opens a turn in a transcript: a user row the
// turn's started drew (an interjection's is mid-turn), or the note a foreign
// turn's start draws.
func turnBoundary(e transcript.Entry) bool {
	switch e.Kind {
	case transcript.KindUser:
		return !e.Interject
	case transcript.KindNote:
		switch e.Text {
		case transcript.NoteForeignTurn, transcript.NoteSubagentWake, transcript.NoteJobWake:
			return true
		}
	}
	return false
}

// synchronized is the attachment caught up: the session's model, and — for a
// target not yet settled — an ask the stream's replay left open (askCheck).
func (w *turnWaiter) synchronized(ctx context.Context) {
	if w.target.kind == targetNone || w.cleaning {
		return
	}
	w.askCheck(ctx)
}

// ----------------------------------------------------------------- events

// event is one event of the stream, in order.
func (w *turnWaiter) event(ctx context.Context, ev agent.Event) {
	if ft := ev.ForeignTurn; ev.Type == agent.EventForeignTurn && ft != nil {
		if ft.Running {
			w.foreign = ft.ID
		} else if w.foreign == ft.ID {
			w.foreign = ""
		}
	}
	if st := ev.State; ev.Type == agent.EventMeta && st != nil && st.Model != nil {
		w.session.model = *st.Model
	}
	if tu := ev.Turn; ev.Type == agent.EventTurn && tu != nil && tu.Phase == agent.TurnEnded && closingEnding(tu) {
		w.closing, w.closeCause = true, tu.Cause
	}
	switch w.target.kind {
	case targetNone:
		if w.idle != nil {
			w.scan(ctx, ev)
		}
	case targetRow:
		w.queuedEvent(ctx, ev)
	default:
		w.runningEvent(ctx, ev)
	}
}

// scan is plain wait's look at the events after an idle attach: a turn that
// starts is followed from its start, whole; reaching the sync's seq with none
// is the last turn's answer.
func (w *turnWaiter) scan(ctx context.Context, ev agent.Event) {
	switch {
	case ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnStarted:
		w.idle = nil
		w.target = waitTarget{kind: targetTurn, id: ev.Turn.ID}
		w.runningEvent(ctx, ev)
		return
	case ev.Type == agent.EventForeignTurn && ev.ForeignTurn != nil && ev.ForeignTurn.Running:
		w.idle = nil
		w.target = waitTarget{kind: targetForeign, id: ev.ForeignTurn.ID}
		w.runningEvent(ctx, ev)
		return
	}
	if ev.Seq >= w.idle.until {
		w.finishIdle(ctx, w.idle)
	}
}

// queuedEvent is one event while the target is a queued row.
func (w *turnWaiter) queuedEvent(ctx context.Context, ev agent.Event) {
	switch {
	case ev.Type == agent.EventQueue && ev.Queue != nil && ev.Queue.ID == w.row:
		switch ev.QueueChange {
		case agent.QueueSent:
			// The next started of craze's own is this row's turn; the handle
			// stays before the sent, so a replay from it sees the link.
			w.rowSent = true
			return
		case agent.QueueRemoved:
			w.finish(w.newResult(statusDropped))
			return
		}
	case ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnStarted && w.rowSent:
		w.target = waitTarget{kind: targetTurn, id: ev.Turn.ID}
		w.runningEvent(ctx, ev)
		w.relook(ctx)
		return
	}
	if !w.rowSent {
		w.anchor = ev.Seq
	}
	w.opening(ctx, ev)
}

// runningEvent is one event while the target is a running turn.
func (w *turnWaiter) runningEvent(ctx context.Context, ev agent.Event) {
	if w.target.kind == targetForeign {
		if ft := ev.ForeignTurn; ev.Type == agent.EventForeignTurn && ft != nil && ft.ID == w.target.id {
			if ft.Running {
				w.turnStarted(ev)
				return
			}
			// A foreign turn's ending is always done: its bracket carries no
			// outcome (protocol.TurnDone).
			w.acc.end(ev.At)
			w.finish(w.newResult(statusCompleted))
			return
		}
	} else if tu := ev.Turn; ev.Type == agent.EventTurn && tu != nil && tu.ID == w.target.id {
		switch tu.Phase {
		case agent.TurnStarted:
			w.turnStarted(ev)
		case agent.TurnEnded:
			if row, ok := w.restoring(ev); ok {
				// Refused by the agent's own turn and put back (P3): the row
				// waits again, and its next run is the target.
				w.requeue(row, ev.Seq)
				w.relook(ctx)
				return
			}
			w.acc.end(ev.At)
			w.ended(ctx, tu)
		}
		return
	}
	if q := ev.Queue; ev.Type == agent.EventQueue && q != nil && ev.QueueChange == agent.QueueQueued {
		w.lastQueuedID, w.lastQueuedSeq = q.ID, ev.Seq
	}
	if w.folding {
		w.acc.fold(ev)
	}
	w.opening(ctx, ev)
}

// turnStarted is the target turn's own start: what its duration counts
// from, and — for a turn submitted directly — the handle's place, just
// before it, so a replay from the handle folds the whole turn (a turn that
// came from a row keeps the row's place, before its `sent`: handle). A wait
// whose handle's cursor is just before it was folding from the cursor
// (attached): the turn is whole after all.
func (w *turnWaiter) turnStarted(ev agent.Event) {
	if w.startSeen {
		return
	}
	if w.folding && ev.Seq == w.anchor+1 {
		w.partial = false
	}
	w.startSeen, w.folding = true, true
	w.acc.start(ev.At)
	if !w.rowLinked {
		w.anchor = ev.Seq - 1
	}
}

// restoring reports whether ev, the target turn's ending, is a restoring
// settlement's (P3; engine.go's restoreLocked): synthetic, refused by the
// agent's own turn, and right behind the target's own row's `queued` — the
// settlement's batch is [the row queued, the ending], one seq apart, which
// the log never interleaves (EventLog.publishBatch) — and that row's id. Only
// the target's own row: a turn submitted directly has none, and its refusal
// is its ending, however close another client's queued row came before it.
func (w *turnWaiter) restoring(ev agent.Event) (string, bool) {
	tu := ev.Turn
	if w.row == "" || !tu.Synthetic || tu.ErrClass != agent.EventErrForeignTurn {
		return "", false
	}
	if w.lastQueuedID != w.row || w.lastQueuedSeq+1 != ev.Seq {
		return "", false
	}
	return w.row, true
}

// ended is the target turn's ending as its result (§3.1): completed is
// end_turn with no error, an error is failed, a cancel is cancelled — and any
// other stop the turn ended without completing, failed, its stop reason said.
// A cancel is the one rule's (returnedCancel): when the unattended host
// returned the turn's plan and cancelled the turn itself (X28), the plan is
// the turn's result, completed. Only that policy writes an automatic plan
// answer that does not accept, so a person's cancel of a turn with no
// returned plan is still cancelled. A cancel whose lookup an interrupt cut
// short is left unsettled, for the interrupt to decide.
func (w *turnWaiter) ended(ctx context.Context, tu *agent.TurnInfo) {
	if !w.folding {
		// Its ending, and nothing of the turn before it.
		w.partial = true
	}
	if closingEnding(tu) {
		w.closed(tu.Cause)
		return
	}
	status := statusFailed
	switch {
	case tu.StopReason == stopCancelled:
		returned, decided := w.returnedCancel(ctx, tu.ID)
		if !decided {
			w.unsettled = func(ctx context.Context) { w.ended(ctx, tu) }
			return
		}
		status = statusCancelled
		if returned {
			status = statusCompleted
		}
		w.withdrawn = tu.Synthetic
	case tu.Err != "":
	case tu.StopReason == stopEndTurn:
		status = statusCompleted
		w.lookApproved(ctx, tu.ID, tu.StopReason)
	}
	res := w.newResult(status)
	res.StopReason = tu.StopReason
	switch {
	case tu.Err != "":
		res.Error = newResultError(tu.Err, tu.Info)
	case status == statusFailed:
		// The turn stopped short with no error of its own: its reason says
		// why (max_tokens, refusal, max_turn_requests), and a failed result
		// always has an error for a program to read.
		res.Error = kindError("the turn stopped short: "+tu.StopReason, errkind.Agent)
	}
	w.finish(res)
}

// closingEnding says tu is the ending a session's close gives the turn it cuts
// short (engine.CloseFor).
func closingEnding(tu *agent.TurnInfo) bool { return tu.Synthetic && tu.StopReason == stopClosing }

// closed is the target turn cut short by its session's close, cause what began
// it: cancelled, stopReason stopped, with what was folded of the turn: its usage
// the last report's, not the turn's own ending's (the harness counts a cancelled
// turn as nothing), and whether a step was in flight. Live or read back from
// lastTurn, the result is this; read back, what is folded may be nothing.
func (w *turnWaiter) closed(cause string) {
	res := w.newResult(statusCancelled)
	res.StopReason = stopStopped
	res.Error = stoppedError(endedLine(w.session.id), cause)
	w.finish(res)
}

// The stop reasons the waiter tells apart or says itself: a cancelled turn's,
// the one the engine's close gives the turn it ends (engine.Close), and the two
// a result says when the engine sent none (turnResult.fill).
const (
	stopCancelled = "cancelled"
	stopClosing   = "closing"
	stopError     = "error"
	stopStopped   = "stopped"
)

// opening is a non-automatic ask's opening: once the attachment has caught
// up, the waiter looks at once (askCheck).
func (w *turnWaiter) opening(ctx context.Context, ev agent.Event) {
	if askOpening(ev) && w.live && !w.cleaning {
		w.askCheck(ctx)
	}
}

// askOpening reports whether ev opens a non-automatic ask.
func askOpening(ev agent.Event) bool {
	switch {
	case ev.Type == agent.EventPermission && ev.Permission != nil:
		return true
	case ev.Type == agent.EventQuestion && ev.Question != nil && !ev.Question.Auto:
		return true
	case ev.Type == agent.EventPlan && ev.Plan != nil && !ev.Plan.Auto:
		return true
	}
	return false
}

// autoAsk reports whether an ask's body is an automatic one's.
func autoAsk(b agent.AskBody) bool {
	return (b.Question != nil && b.Question.Auto) || (b.Plan != nil && b.Plan.Auto)
}

// askSummary is what an ask is about, on one line: a permission's tool, a
// question's first question (else its title), a plan's name.
func askSummary(kind agent.AskKind, b agent.AskBody) string {
	switch kind {
	case agent.AskPermission:
		if b.Permission != nil {
			return psLine(b.Permission.Tool)
		}
	case agent.AskQuestion:
		if q := b.Question; q != nil {
			if len(q.Questions) > 0 && q.Questions[0].Prompt != "" {
				return psLine(q.Questions[0].Prompt)
			}
			return psLine(q.Title)
		}
	case agent.AskPlan:
		if p := b.Plan; p != nil {
			return psLine(p.Name)
		}
	}
	return ""
}

// askCheck is the look §3.1 asks for after the event batch: where the
// session stands now (session.snapshot: its running turn — the agent's own
// by its bracket's id — its queue and its open asks, each with its opening).
// An open, non-automatic ask is needs_input — the target's own, or, while the
// target is still queued, the one blocking the turn ahead of it (blockedBy) —
// and only while the session still stands at the target: its own turn
// running (a foreign target: that very bracket, by id), or its row still
// queued. The look is newer than the stream: a target whose ending is still
// on its way down it, a successor already running and asking, is not the
// asking turn, and the stream's next events — its ending — say what it is.
//
// A look the interrupt ended decides nothing: the loop answers the signal or
// the timeout (an owned cleanup; running). One no identity of the target's
// incarnation could make is the target's ending not observed. Any other
// failure is looked again (askLater): a parked ask brings no event that would.
func (w *turnWaiter) askCheck(ctx context.Context) {
	if w.cleaning || w.expired {
		// A signal's cleanup reports no needs_input, and nothing looked at
		// past --timeout's end decides the target (timedOut).
		return
	}
	w.stale = false
	snap, err := w.look(ctx)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return
	case errors.Is(err, errNotThisHost):
		w.notObserved()
		return
	default:
		w.askLater()
		return
	}
	if !w.standsAtTarget(snap) {
		// The look is ahead of the stream: the target's next move on it —
		// its row put back, its row's turn started — looks again (relook),
		// for an ask already open then brings no opening of its own.
		w.stale = true
		return
	}
	i := slices.IndexFunc(snap.Asks, func(a transcript.Ask) bool { return !autoAsk(a.Body) })
	if i < 0 {
		return
	}
	a := snap.Asks[i]
	summary := askSummary(a.Kind, a.Body)
	if summary == "" {
		summary = psLine(agent.AskLabel(a.Kind, a.Body))
	}
	res := w.newResult(statusNeedsInput)
	res.Ask = &resultAsk{ID: a.ID, Kind: string(a.Kind), Summary: summary}
	if w.target.kind == targetRow {
		switch f := snap.Turn.Foreign; {
		case snap.Turn.ID != "":
			res.BlockedBy = snap.Turn.ID
		case f != nil && f.Running:
			res.BlockedBy = f.ID
		}
	}
	w.finish(res)
}

// standsAtTarget says a look finds the session still at the target
// (askCheck): its own turn running; for a foreign target, that very bracket
// running (by id — a later one is another turn) and no turn of craze's; for
// a row, the row still queued.
func (w *turnWaiter) standsAtTarget(snap *transcript.Snapshot) bool {
	switch w.target.kind {
	case targetTurn:
		return snap.Turn.ID == w.target.id
	case targetForeign:
		f := snap.Turn.Foreign
		return f != nil && f.Running && f.ID == w.target.id && snap.Turn.ID == ""
	case targetRow:
		return slices.ContainsFunc(snap.Queue, func(q agent.QueuedPrompt) bool { return q.ID == w.row })
	}
	return false
}

// look is where the session stands now: one snapshot (session.snapshot),
// read under the target's proven identity (read).
func (w *turnWaiter) look(ctx context.Context) (*transcript.Snapshot, error) {
	var res protocol.SnapshotResult
	if err := w.read(ctx, protocol.MethodSessionSnapshot, protocol.SnapshotParams{SessionID: w.session.id}, &res); err != nil {
		return nil, err
	}
	snap, err := transcript.DecodeSnapshot(res.Snapshot)
	if err != nil {
		return nil, err
	}
	if snap.Settings.Model != "" {
		w.session.model = snap.Settings.Model
	}
	return snap, nil
}

// relook is the look again after the target moved on the stream — its row
// put back, its row's turn started — when the last look found the session
// already past where the stream then was (stale): the ask it saw open may be
// the target's now, and nothing else on the stream need come to say so. Only
// while the attachment is live; one catching up looks at its synchronized.
func (w *turnWaiter) relook(ctx context.Context) {
	if w.stale && w.live && !w.done() {
		w.askCheck(ctx)
	}
}

// askLater makes the look for an open ask again after askRetryEvery (the
// loop's askDue), unless one is already due.
func (w *turnWaiter) askLater() {
	if w.askDue != nil {
		return
	}
	w.askTimer = time.NewTimer(askRetryEvery)
	w.askDue = w.askTimer.C
}

// ---------------------------------------------------------------- reads

// state is session.state.
func (w *turnWaiter) state(ctx context.Context) (*protocol.StateResult, error) {
	var st protocol.StateResult
	if err := w.read(ctx, protocol.MethodSessionState, protocol.StateParams{SessionID: w.session.id}, &st); err != nil {
		return nil, err
	}
	if st.Settings.Model != "" {
		w.session.model = st.Settings.Model
	}
	return &st, nil
}

// errNotThisHost is a read or a command the waiter could not make under an
// identity proven to be its incarnation's (bind): the client has reconnected
// to a host that is not — or not yet proven to be — the one the target ran
// in. Where the waiter has no target yet (idleLook) it decides nothing
// (undecided): the restore the reconnect brings starts the look again. Where
// it has one (reconcile, the ask look, a command) the turn's ending can no
// longer be learnt, and the waiter says so with the handle.
var errNotThisHost = errors.New("the session's host is not the one the turn ran in")

// read is one read for the target, bounded by readBound and ctx, and made
// only under the identity bind proved to be the waiter's incarnation's: the
// identity is read before and after, and an answer from a connection of
// another — a reconnect that did not resume, maybe to another incarnation,
// whose turn-1 is another turn (P7) — is never taken. remote.Client.Call is
// bound to no identity, and a read whose connection went is sent again on
// whichever comes next: a connection's identity is fixed for its life and
// picked under the lock the identity is read under (remote's wireAs), and
// identities only move on, so the same identity before and after is a read
// answered on a connection of that identity. A read whose connection went
// before its reply, or whose identity moved, is made again under a proven
// one, while ctx allows; one no identity of the waiter's incarnation can make
// is errNotThisHost.
func (w *turnWaiter) read(ctx context.Context, method string, params, result any) error {
	rctx, cancel := context.WithTimeout(ctx, readBound)
	defer cancel()
	for {
		if err := w.bind(rctx); err != nil {
			return err
		}
		ident := w.ident
		err := w.conn.c.Call(rctx, method, params, result)
		if w.conn.c.Identity() != ident {
			w.ident = 0
			if rctx.Err() == nil {
				continue
			}
			return errNotThisHost
		}
		if errors.Is(err, remote.ErrConnectionLost) && rctx.Err() == nil {
			continue
		}
		return err
	}
}

// bind proves the client's identity to be the waiter's incarnation's, unless
// it is the one proved already: a sessions.list answered under one identity
// — the same before the call and after — whose row for the session names the
// waiter's incarnation (the info document's). A list that names another is
// errNotThisHost.
func (w *turnWaiter) bind(ctx context.Context) error {
	if w.ident != 0 && w.conn.c.Identity() == w.ident {
		return nil
	}
	w.ident = 0
	ident, err := proveIdentity(ctx, w.conn.c, w.session.id, w.inc)
	if err != nil {
		return err
	}
	w.ident = ident
	return nil
}

// proveIdentity is the identity c holds now, proven to be incarnation inc's
// for session sessionID: a sessions.list answered under one identity — the
// same before the call and after — whose row for the session names inc. A
// list that names another, or none, is errNotThisHost. bind's, and the
// submit's before its prompt is sent (attached).
func proveIdentity(ctx context.Context, c *remote.Client, sessionID, inc string) (uint64, error) {
	for {
		ident := c.Identity()
		var list protocol.SessionsListResult
		err := c.Call(ctx, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list)
		switch {
		case c.Identity() != ident && ctx.Err() == nil:
			continue
		case c.Identity() != ident:
			return 0, errNotThisHost
		case errors.Is(err, remote.ErrConnectionLost) && ctx.Err() == nil:
			continue
		case err != nil:
			return 0, err
		}
		i := slices.IndexFunc(list.Sessions, func(r protocol.SessionRow) bool { return r.SessionID == sessionID })
		if i < 0 || list.Sessions[i].Incarnation != inc {
			return 0, errNotThisHost
		}
		return ident, nil
	}
}

// ---------------------------------------------------------------- the end

// newResult is a result about the target with status: its turn and row, the
// handle a non-terminal one carries, and — for a terminal one — its facts.
func (w *turnWaiter) newResult(status string) *turnResult {
	res := newResult(w.session, status)
	res.inc = w.inc
	switch w.target.kind {
	case targetTurn, targetForeign:
		res.Turn = w.target.id
	}
	res.Row = w.row
	if !terminalStatus(status) {
		res.Handle = w.handle()
	}
	return res
}

// finish ends the wait with res, its facts filled from the accumulator. A turn
// that was cancelled says so when its usage leaves a step out; one that failed
// does not: a failing step has no spend it can report, and none is expected.
func (w *turnWaiter) finish(res *turnResult) {
	res.fill(w.acc)
	res.Partial = w.partial
	if res.Status == statusCancelled {
		res.UsageComplete = usageIncomplete(w.stepInFlight())
	}
	w.result = res
}

// stepInFlight says a step of the target may have been under way as it was cut
// short: the accumulator's reading of the events it folded, and whenever some of
// the turn's events were never seen (a snapshot, a cursor mid-way, a look
// afterwards), when that reading has nothing to stand on. Only a prompt withdrawn
// before it was sent is known to have had none.
func (w *turnWaiter) stepInFlight() bool {
	return !w.withdrawn && (w.partial || w.acc.stepOpen)
}

// handle is the wait's handle (turnHandle), "" before it has a target —
// unless the wait started from one (wait --from), whose target it is still.
// A target that came from a queued row is named by its row, from before the
// row's `sent` (queuedEvent's anchor), even once its turn runs: a replay from
// there follows the row into its turn — and back, should the agent's own turn
// refuse it and the row be put back (P3; restoring knows only the target's
// own row). A turn met running — at the attach, or by a reconcile's snapshot,
// whose cursor is past the row's link to it (rowLinked) — is named by its
// turn: a handle names only what its cursor can follow; and, when it came
// from a row, by the row beside it (turnHandle.row), which a wait from it
// takes for its own, so the row's restoration is still the target's.
func (w *turnWaiter) handle() string {
	if w.target.kind == targetNone || w.inc == "" {
		if h := w.start.from; h != nil {
			return h.String()
		}
		return ""
	}
	h := turnHandle{incarnation: w.inc, seq: w.anchor, target: w.target}
	switch {
	case w.row != "" && w.rowLinked:
		h.target = waitTarget{kind: targetRow, id: w.row}
	case w.row != "" && w.target.kind == targetTurn:
		h.row = w.row
	}
	return h.String()
}

// timedOut is --timeout's end (§3.1): a target still queued or running is
// `running`, exit 5, with its handle — the turn goes on; with no target yet,
// the command could not even start the wait (the handle it started from, if
// any, still names its target). A submit still out is waited for first,
// within cleanupWait, as a signal waits for it: its answer is what the handle
// must name. The events buffered meanwhile — all of them handed over before
// the timeout — are replayed on a context of their own, the waiter's having
// ended with the timeout, and only move the target: an ending among them is
// the result, as one the stream handed over before the timeout is in the
// loop, but nothing read past the timeout — a look for an ask, a reconcile's
// state, a returned plan's lookup — decides it (expired). A signal while the reply is awaited is
// answered as any signal is (signalled): an owned wait's cleanup, a second
// signal exiting at once.
func (w *turnWaiter) timedOut(items <-chan pumped) {
	if w.buffering() {
		waitAwaitingReply()
		ctx, cancel := context.WithTimeout(context.Background(), cleanupWait)
		defer cancel()
		select {
		case a := <-w.submitted:
			w.expired = true
			w.submitAnswered(ctx, a)
		case <-w.intr:
			// --timeout's end comes once: this is a signal.
			cancel()
			w.signalled(items)
			return
		case <-ctx.Done():
		}
		if w.done() {
			return
		}
	}
	if w.target.kind == targetNone {
		w.fail = &waitFailure{msg: "timed out before the turn could be followed", kind: errkind.TimeoutBeforePrompt, handle: w.handle(), follow: w.start.from != nil,
			awaited: turnHandle{incarnation: w.inc, seq: w.anchor}}
		return
	}
	w.finish(w.newResult(statusRunning))
}

// waitAwaitingReply sees a timeout's end, or a signal, start to wait for a
// submit's reply still out: a no-op, and the barrier a test releases the
// reply, or sends a signal, after.
var waitAwaitingReply = func() {}

// submitAnswered is the submit's reply (C3): its target, and the events
// buffered meanwhile replayed against it; a failed submit is the command's
// failure.
func (w *turnWaiter) submitAnswered(ctx context.Context, a submitAnswer) {
	w.submitted = nil
	if a.err != nil {
		w.fail = &waitFailure{msg: a.err.Error(), kind: failureKind(a.err)}
		return
	}
	if w.ident == 0 && w.conn.c.Identity() == a.ident {
		// Proven already: the waiter's reads and commands start under it.
		w.ident = a.ident
	}
	if k := a.target.kind; k != targetRow && k != targetTurn {
		w.fail = &waitFailure{msg: "the prompt started no turn and was not queued", kind: errkind.Internal}
		return
	}
	w.setTarget(a.target)
	buffered := w.buffer
	w.buffer = nil
	for _, p := range buffered {
		if w.done() || w.unsettled != nil {
			// Settled; or the target's ending read, and nothing after it is
			// the target's (unsettled).
			return
		}
		w.item(ctx, p)
	}
}

// signalled is a signal (§3.1 "Signals", P8). Plain wait, and a target the
// command does not own — the agent's own turn — exit at once, the session
// untouched. An owned wait acts on its own work only, on a fresh cleanupWait
// context: a queued row is removed (dropped), its own running turn cancelled
// by id and its ending awaited (cancelled, or what it ended as); before a
// submit's reply, the reply is awaited first. A second signal ends the
// command at once, whatever the cleanup waits on — a command in flight
// included, whose context it cancels — and the command's detach is then
// abandoned too (waitConn.abandon), whether it came during the cleanup or as
// the cleanup ended: the watch for it is stopped and joined before it is
// looked for, so one the watch took is never lost. The context's end with
// nothing confirmed is exit 1, `cancel sent, not confirmed`. The whole
// cleanup, the detach as the command exits included (waitConn.closeBy),
// keeps to cleanupWait.
func (w *turnWaiter) signalled(items <-chan pumped) {
	deadline := time.Now().Add(cleanupWait)
	w.conn.closeBy = deadline
	if !w.start.owned() || w.target.kind == targetForeign && !w.buffering() {
		w.fail = w.interrupted("interrupted; the turn goes on")
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	again, stop, watched := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case in := <-w.intr:
				if in.timeout {
					// The cleanup has its own bound.
					continue
				}
				close(again)
				cancel()
				return
			case <-stop:
				return
			}
		}
	}()
	w.cleaning = true
	defer func() {
		// The watch stopped, and joined, first: a second signal it took as
		// the cleanup ended is one all the same. A step that failed as the
		// second signal cancelled it failed for that signal: say so, and
		// exit without the detach.
		waitCleanupEnding(again)
		close(stop)
		<-watched
		select {
		case <-again:
			w.conn.abandon = true
			switch {
			case w.result != nil:
			case w.buffering():
				w.fail = &waitFailure{msg: "interrupted again before the prompt's answer; it may have reached the session", kind: errkind.Interrupted, handle: w.handle()}
			case w.unsettled != nil:
				w.fail = w.interrupted(errUnsettled + " again")
			default:
				w.fail = w.interrupted("interrupted again before the cancel was confirmed")
			}
		default:
		}
	}()
	if w.buffering() {
		waitAwaitingReply()
		select {
		case a := <-w.submitted:
			w.submitAnswered(ctx, a)
		case <-again:
			return
		case <-ctx.Done():
			w.fail = &waitFailure{msg: "interrupted before the prompt's answer; it may have reached the session", kind: errkind.Interrupted}
			return
		}
		if w.done() {
			return
		}
	}
	if settle := w.unsettled; settle != nil {
		// The target's ending was read before the signal, and only whether
		// its plan was returned was not (an interrupted lookup): settled now,
		// on the cleanup's own context, as the cleanup settles an ending it
		// reads itself. A turn that has ended has nothing to cancel.
		w.unsettled = nil
		settle(ctx)
		if !w.done() {
			w.fail = w.interrupted(errUnsettled)
		}
		return
	}
	w.act(ctx)
	for !w.done() {
		select {
		case p := <-items:
			w.item(ctx, p)
			if !w.done() && w.unsettled != nil {
				// An ending the cleanup read, its lookup cut short by the
				// cleanup's own end or a second signal.
				w.fail = w.interrupted(errUnsettled)
				continue
			}
			if !w.done() {
				w.act(ctx)
			}
		case <-again:
			return
		case <-ctx.Done():
			w.fail = w.interrupted("cancel sent, not confirmed")
		}
	}
}

// errUnsettled is a cleanup that ended before the target's ending, read
// already, could be settled (unsettled).
const errUnsettled = "interrupted before the turn's ending was settled"

// waitCleanupEnding sees an owned wait's cleanup end, before it stops its
// watch for a second signal; taken is closed once the watch has taken one.
// A no-op, and the barrier a test sends a signal in that handoff at.
var waitCleanupEnding = func(taken <-chan struct{}) {}

// act is the cleanup's next step for the target as it stands: a row not yet
// sent is removed, once — dropped on the host's receipt; a row the host no
// longer holds has been sent (or removed) already, and the stream, read on,
// says which: its turn, once it starts, is cancelled. A running turn of
// craze's own is cancelled by its id, once. A stale_turn answer is a turn that
// ended first: its ending, on the stream, is the answer.
//
// Both are bound to the identity proven to be the target's incarnation's
// (bind; remote's CommandOptions.Identity): a command is never sent under
// another — a reconnect that did not resume, maybe to an incarnation whose
// turn-1 is another turn (P7) — and one the client could not send under it,
// or that found it proven to be another incarnation's, ends the cleanup with
// the target's ending not observed (hostChanged).
func (w *turnWaiter) act(ctx context.Context) {
	var method string
	var params any
	switch {
	case w.target.kind == targetRow && !w.rowSent && !w.removeSent:
		w.removeSent = true
		method, params = protocol.MethodQueueRemove, protocol.QueueRemoveParams{SessionID: w.session.id, RowID: w.row}
	case w.target.kind == targetTurn && !w.cancelSent:
		w.cancelSent = true
		method, params = protocol.MethodSessionCancel, protocol.CancelParams{SessionID: w.session.id, TurnID: w.target.id}
	default:
		return
	}
	err := w.bind(ctx)
	if err == nil {
		waitCommanding(method)
		_, err = w.conn.c.Command(ctx, method, params, nil, remote.CommandOptions{Identity: w.ident})
	}
	switch {
	case err == nil && method == protocol.MethodQueueRemove:
		w.finish(w.newResult(statusDropped))
	case err == nil, errors.Is(err, engine.ErrUnknownRow), errors.Is(err, engine.ErrStaleTurn):
		// Taken; or the row gone from the queue already, the turn ended
		// already: the stream says where, and how.
	case errors.Is(err, errNotThisHost), errors.Is(err, backend.ErrStaleEpoch):
		w.hostChanged()
	case ctx.Err() != nil:
		// The cleanup's end, or the second signal: the loop says which.
	case method == protocol.MethodQueueRemove:
		w.fail = &waitFailure{msg: "the row's removal failed: " + sanitizeLine(err.Error()), kind: errkind.Interrupted, handle: w.handle()}
	default:
		w.fail = &waitFailure{msg: "the cancel failed: " + sanitizeLine(err.Error()), kind: errkind.Interrupted, handle: w.handle()}
	}
}

// waitCommanding sees each command a signal's cleanup is about to send, by
// its method: a no-op, and the barrier a test changes the host behind.
var waitCommanding = func(string) {}

// hostChanged is a cleanup the session's host changed under (act): nothing
// was cancelled, and the target's ending is not observed.
func (w *turnWaiter) hostChanged() {
	w.fail = &waitFailure{msg: "the session's host changed before the cancel reached it; the turn's ending was not observed",
		kind: errkind.Unobserved, handle: w.handle()}
}

// --------------------------------------------------------------- helpers

// endedLine is the session's end as a waiting command says it: with the
// reload hint when the session index has the session's row (§3.1, §3.4).
func endedLine(id string) string {
	return "the session ended" + reloadHint(matchCrazeID(indexCrazeIDs(), id))
}
