package cli

import (
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The waiter's edges the C2 review's third round (astra) found, each its
// sequence placed by the test: a second signal as an owned cleanup ends, a
// restoration behind an ask already open, a timeout waiting for a submit's
// reply, a timeout before the attachment's first item, and a replay that
// ends before the attachment catches up. (Its N2 item extends
// TestWaitTurnHandleAfterAReconcileKeepsTheRow, and the handle's grammar
// TestHandleRoundTrips.)

// awaited waits for one send on ch, within stubWatchdog.
func awaited(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(stubWatchdog):
		t.Fatalf("still waiting for %s after %s", what, stubWatchdog)
	}
}

// TestWaitTurnSecondSignalAsTheCleanupEnds (item 12, round 3): an owned
// wait's signal removes its row (dropped); the second signal comes just as
// the cleanup ends, and its watch takes it there (waitCleanupEnding, the
// handoff placed). It is the second signal all the same: the detach is
// abandoned, and the close does not wait on a host that never answers it.
func TestWaitTurnSecondSignalAsTheCleanupEnds(t *testing.T) {
	shorten(t, &cleanupWait, time.Hour)
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(heldTurn(g))
	r.submit("first")
	conn, tp := r.dialTapped()
	tp.swallow(`"method":"session.detach"`)
	sigs := make(chan os.Signal, 2)
	prev := waitCleanupEnding
	waitCleanupEnding = func(taken <-chan struct{}) {
		sigs <- syscall.SIGINT
		select {
		case <-taken:
		case <-time.After(stubWatchdog):
		}
	}
	t.Cleanup(func() { waitCleanupEnding = prev })
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
	r.waitQueued(1)
	waitFor(t, "the prompt's reply", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
	sigs <- syscall.SIGTERM
	res, f := w.result(t)
	wantResult(t, res, f, statusDropped)
	if !conn.abandon {
		// The close would wait out the cleanup's hour on the detach.
		conn.abandon = true
		t.Fatal("the second signal, taken as the cleanup ended, was lost: the detach is not abandoned")
	}
	shorten(t, &detachBound, time.Hour)
	closed := make(chan struct{})
	go func() { conn.close(sigs); close(closed) }()
	awaited(t, closed, "the close")
}

// TestWaitTurnRestorationLooksAgainForAnOpenAsk (round 3 A): the waiter
// follows q-1's turn-2; the agent's own turn takes the session and asks. As
// the waiter's look at that ask goes out, turn-2 is refused and q-1 put back:
// the look finds the session past turn-2, so it reports nothing — and the
// restoration, folded after it, looks again. The ask holds q-1 up: exit 4,
// q-1's handle, blocked by the agent's turn — never a wait for an event that
// does not come.
func TestWaitTurnRestorationLooksAgainForAnOpenAsk(t *testing.T) {
	r := newWaitRig(t, 0)
	first, refuse := r.gate(), r.gate()
	r.script(heldTurn(first, textEv("first")), stubTurn{before: func() { <-refuse.ch }, err: agent.ErrForeignTurn},
		answerTurn(textEv("never")))
	r.submit("first")
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
	r.waitQueued(1)
	waitFor(t, "the waiter's first look answered", func() bool { return tp.answered(protocol.MethodSessionSnapshot, 1) })
	first.open()
	r.waitStarted("turn-2")
	tp.on(`"method":"session.snapshot"`, func() bool {
		refuse.open()
		waitFor(t, "the row put back", func() bool {
			st := r.eng.State()
			return len(st.Queue) == 1 && st.LastTurn != nil && st.LastTurn.TurnID == "turn-2"
		})
		return true
	})
	r.s.mu.Lock()
	r.s.foreign = true
	r.s.mu.Unlock()
	r.s.emit(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "wake-1", Running: true}})
	r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-9", Tool: "Shell"}})
	res, f := w.result(t)
	res = wantResult(t, res, f, statusNeedsInput)
	h, err := parseHandle(res.Handle)
	if res.Row != "q-1" || res.BlockedBy != "wake-1" || res.Ask == nil || res.Ask.ID != "perm-9" ||
		err != nil || h.target != (waitTarget{kind: targetRow, id: "q-1"}) {
		t.Fatalf("the restored row behind the open ask: %+v (ask %+v), handle %+v %v", res, res.Ask, h, err)
	}
	if n := len(tp.lines(protocol.MethodSessionSnapshot)); n < 3 {
		t.Fatalf("the waiter looked %d times, want the look again after the restoration", n)
	}
}

// TestWaitTurnTimeoutAwaitingTheReply (round 3 B): --timeout's end comes
// while the submit's reply is still out, the turn already asking (its
// opening in the buffer). The reply comes within the grace: the result is
// running, exit 5, with the turn's handle — no look made past the timeout
// decides it. A signal while the reply is awaited is answered: the owned
// cleanup cancels the turn, and a second signal exits at once.
func TestWaitTurnTimeoutAwaitingTheReply(t *testing.T) {
	// rig is a session whose turn asks (perm-1) and runs until cancelled,
	// a wait whose submit's reply is held until reply opens, and the seams
	// placed: opened once the ask's opening is in the buffer, awaiting at
	// each wait for the reply (the timeout's, then a signal's).
	type rig struct {
		r        *waitRig
		conn     *waitConn
		tp       *tap
		reply    *gate
		opened   chan struct{}
		awaiting chan struct{}
		fire     chan time.Time
		sigs     chan os.Signal
		w        *waitRun
	}
	start := func(t *testing.T) *rig {
		r := newWaitRig(t, 0)
		cancelled := r.gate()
		r.s.mu.Lock()
		r.s.onCancel = cancelled.open
		r.s.mu.Unlock()
		r.script(stubTurn{before: func() {
			r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell"}})
			<-cancelled.ch
		}, emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}}, res: agent.Result{StopReason: "cancelled"}})
		x := &rig{r: r, reply: r.gate(), opened: make(chan struct{}), awaiting: make(chan struct{}, 4),
			fire: make(chan time.Time), sigs: make(chan os.Signal, 2)}
		x.conn, x.tp = r.dialTapped()
		var once sync.Once
		prevBuffered, prevAwaiting := waitBuffered, waitAwaitingReply
		waitBuffered = func(p pumped) {
			if p.it.Kind == remote.KindEvent && strings.Contains(string(p.it.Body), `"perm-1"`) {
				once.Do(func() { close(x.opened) })
			}
		}
		waitAwaitingReply = func() { x.awaiting <- struct{}{} }
		t.Cleanup(func() { waitBuffered, waitAwaitingReply = prevBuffered, prevAwaiting })
		submit := func(ctx context.Context, ident uint64) (waitTarget, error) {
			target, err := submitPrompt(x.conn, "mine")(ctx, ident)
			<-x.reply.ch
			return target, err
		}
		x.w = goWait(x.conn, waitStart{submit: submit}, waitOptions{timeout: x.fire, sigs: x.sigs})
		awaited(t, x.opened, "the ask's opening in the buffer")
		close(x.fire)
		awaited(t, x.awaiting, "the timeout's wait for the reply")
		return x
	}
	t.Run("an open ask is not the answer", func(t *testing.T) {
		x := start(t)
		x.reply.open()
		res, f := x.w.result(t)
		res = wantResult(t, res, f, statusRunning)
		h, err := parseHandle(res.Handle)
		if res.Ask != nil || res.Turn != "turn-1" || err != nil || h.target != (waitTarget{kind: targetTurn, id: "turn-1"}) {
			t.Fatalf("the timed-out result: %+v, handle %+v %v", res, h, err)
		}
	})
	t.Run("a signal is answered", func(t *testing.T) {
		x := start(t)
		x.sigs <- syscall.SIGINT
		awaited(t, x.awaiting, "the signal's wait for the reply")
		x.reply.open()
		res, f := x.w.result(t)
		wantResult(t, res, f, statusCancelled)
		if cancels := x.tp.lines(protocol.MethodSessionCancel); len(cancels) != 1 || !strings.Contains(cancels[0], `"turnId":"turn-1"`) {
			t.Fatalf("the cancels sent: %q", cancels)
		}
	})
	t.Run("a second signal exits at once", func(t *testing.T) {
		shorten(t, &cleanupWait, time.Hour)
		x := start(t)
		x.sigs <- syscall.SIGINT
		awaited(t, x.awaiting, "the signal's wait for the reply")
		x.sigs <- syscall.SIGINT
		res, f := x.w.result(t)
		if res != nil || f == nil || f.msg != "interrupted again before the prompt's answer; it may have reached the session" || !x.conn.abandon {
			t.Fatalf("the second signal: %+v, %+v, abandon %v", res, f, x.conn.abandon)
		}
	})
}

// TestWaitTurnTimeoutBeforeTheAttachKeepsTheHandle (round 3 C): --timeout's
// end wins before the attachment's first item reaches the waiter — no
// target yet, the handle it started from (wait --from) unread. The failure
// names that handle, and the command that follows it.
func TestWaitTurnTimeoutBeforeTheAttachKeepsTheHandle(t *testing.T) {
	h := turnHandle{incarnation: "0199c0de-2222-7000-8000-00000000b002", seq: 7,
		target: waitTarget{kind: targetTurn, id: "turn-2"}, row: "q-1"}
	w := newTurnWaiter(&waitConn{sessionID: waitRigID}, waitStart{from: &h}, waitOptions{})
	w.timedOut(nil)
	if w.result != nil || w.fail == nil || w.fail.msg != "timed out before the turn could be followed" ||
		w.fail.handle != h.String() || !w.fail.follow {
		t.Fatalf("the timeout before the attach: %+v, %+v", w.result, w.fail)
	}
}

// TestWaitTurnReplayNamesTheModel (round 3 D): a wait from a handle just
// before a turn that has since completed folds the turn and ends before its
// attachment catches up — nothing it read named the model. The result names
// the session's model all the same.
func TestWaitTurnReplayNamesTheModel(t *testing.T) {
	r := newWaitRig(t, 0)
	r.s.mu.Lock()
	r.s.model = "stub-model-1"
	r.s.mu.Unlock()
	g := r.gate()
	r.script(heldTurn(g, textEv("done")))
	conn := r.dial(nil)
	fire := make(chan time.Time)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{timeout: fire})
	r.waitStarted("turn-1")
	close(fire)
	res, f := w.result(t)
	res = wantResult(t, res, f, statusRunning)
	h, err := parseHandle(res.Handle)
	if err != nil {
		t.Fatal(err)
	}
	g.open()
	r.waitIdle()
	from := r.dial(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq})
	res, f = goWait(from, waitStart{from: &h}, waitOptions{}).result(t)
	res = wantResult(t, res, f, statusCompleted)
	if res.Model != "stub-model-1" || res.message() != "done" || res.Partial {
		t.Fatalf("the replayed turn's result: %+v", res)
	}
}
