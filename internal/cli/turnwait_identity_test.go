package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The waiter's edges the C2 review's second round (astra) found, each its
// sequence placed by the test: a prompt sent across an engine's replacement,
// a handle taken after a reconcile, and an engine closed under the waiter's
// own turn. (Its other items extend the round-1 tests: a foreign successor's
// ask, a stalled ask look, a signal during the detach; and --from's handle on
// a failed resolution, in TestSessionIdsNoLiveHostServes.)

// TestWaitTurnSubmitIsBoundToTheAttachmentsIncarnation (N1): another client's
// turn-1 runs to its end in incarnation A and reaches the waiter's buffer, its
// own prompt not yet sent; the engine is replaced by B and the client's
// identity moves. The prompt, bound to the identity proven to be A's, is
// never sent to B — whose turn-1 would be matched to A's buffered ending: the
// wait fails with the prompt's fate unknown, and B runs nothing.
func TestWaitTurnSubmitIsBoundToTheAttachmentsIncarnation(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("A's answer")))
	sB, engB := r.newEngine(0, answerTurn(textEv("B's answer")))
	ended := make(chan struct{})
	var once sync.Once
	prev := waitBuffered
	waitBuffered = func(p pumped) {
		if p.it.Kind == remote.KindEvent && strings.Contains(string(p.it.Body), `"phase":"ended"`) {
			once.Do(func() { close(ended) })
		}
	}
	t.Cleanup(func() { waitBuffered = prev })
	conn, tp := r.dialTapped()
	submit := func(ctx context.Context, ident uint64) (waitTarget, error) {
		// A's turn-1, another client's, ends on the waiter's stream first.
		r.submit("theirs")
		select {
		case <-ended:
		case <-time.After(stubWatchdog):
			return waitTarget{}, errors.New("A's turn-1 never reached the buffer")
		}
		r.srv.SetEngine(engB)
		tp.drop()
		waitFor(t, "the client's identity to move", func() bool { return conn.c.Identity() != ident })
		return submitPrompt(conn, "mine")(ctx, ident)
	}
	res, f := goWait(conn, waitStart{submit: submit}, waitOptions{}).result(t)
	// B never had the prompt: sent there, it would run a turn-1 the wait
	// takes for A's (or, should a later read find B, one nobody follows).
	if got := sB.sent(); len(got) != 0 || engB.State().LastTurn != nil {
		t.Fatalf("B ran %q, its last turn %+v; the wait: %+v, %+v", got, engB.State().LastTurn, res, f)
	}
	if res != nil || f == nil || !strings.Contains(f.msg, "may not have reached it") {
		t.Fatalf("the prompt across B: %+v, %+v", res, f)
	}
}

// TestWaitTurnHandleAfterAReconcileKeepsTheRow (round 2 N2, round 3): the
// waiter has followed its row q-1 into turn-2; its connection drops and the
// replay is evicted, so the re-attach's snapshot shows turn-2 running, its
// cursor past the row's link to it. --timeout's handle then names turn-2 —
// what that cursor can follow — with q-1 beside it, never the row alone. A
// wait from that handle owns the row too: turn-2 is refused by the agent's
// own turn and q-1 put back, and the wait follows q-1 into its next run.
func TestWaitTurnHandleAfterAReconcileKeepsTheRow(t *testing.T) {
	r := newWaitRig(t, 4)
	first, refuse := r.gate(), r.gate()
	r.script(heldTurn(first, textEv("first")),
		stubTurn{before: func() {
			<-refuse.ch
			r.s.mu.Lock()
			r.s.foreign = true
			r.s.mu.Unlock()
		}, err: agent.ErrForeignTurn},
		answerTurn(textEv("mine, at last")))
	r.submit("first")
	conn, tp := r.dialTapped()
	fire := make(chan time.Time)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{timeout: fire})
	r.waitQueued(1)
	// The waiter's first look is answered with q-1 still queued: none is
	// out, nor due, when the connection drops.
	waitFor(t, "the waiter's first look answered", func() bool { return tp.answered(protocol.MethodSessionSnapshot, 1) })
	first.open()
	r.waitStarted("turn-2")
	// The row's sent and its turn's started are on the waiter's stream.
	var synced protocol.SyncResult
	if err := conn.c.Call(stepCtx(t), protocol.MethodSessionSync, protocol.SyncParams{SessionID: waitRigID}, &synced); err != nil {
		t.Fatal(err)
	}
	redial := r.gate()
	tp.holdRedials(redial)
	tp.drop()
	for range 6 {
		r.s.emit(agent.Event{Type: agent.EventThought, Text: "evicting the replay"})
	}
	redial.open()
	// The restore reconciled: the look the re-attach's synchronized brings
	// has gone out after it.
	waitFor(t, "the waiter's look after the restore", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 2 })
	close(fire)
	res, f := w.result(t)
	res = wantResult(t, res, f, statusRunning)
	h, err := parseHandle(res.Handle)
	if err != nil || h.target != (waitTarget{kind: targetTurn, id: "turn-2"}) || h.row != "q-1" || !res.Partial {
		t.Fatalf("the running result %+v: its handle %+v, %v", res, h, err)
	}
	from := r.dial(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq})
	w2 := goWait(from, waitStart{from: &h}, waitOptions{})
	refuse.open()
	waitFor(t, "the row put back", func() bool {
		st := r.eng.State()
		return len(st.Queue) == 1 && st.LastTurn != nil && st.LastTurn.TurnID == "turn-2"
	})
	r.s.setForeign(false)
	res, f = w2.result(t)
	res = wantResult(t, res, f, statusCompleted)
	if res.Row != "q-1" || res.Turn != "turn-3" || res.message() != "mine, at last" {
		t.Fatalf("q-1 followed from turn-2's handle: %+v", res)
	}
}

// TestWaitTurnEngineCloseIsTheSessionsEnd (N3): the engine closes while the
// waiter's own turn runs. The turn's ending the close authors (synthetic,
// stopReason closing) is the session's end — exit 1, `the session ended`,
// with the handle — never the turn failed.
func TestWaitTurnEngineCloseIsTheSessionsEnd(t *testing.T) {
	r := newWaitRig(t, 0)
	turn := endTurn()
	turn.before = func() { <-r.s.closed }
	r.script(turn)
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
	r.waitStarted("turn-1")
	waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	_ = r.eng.Close()
	res, f := w.result(t)
	if res != nil || f == nil || !strings.HasPrefix(f.msg, "the session ended") {
		t.Fatalf("the engine's close: %+v, %+v", res, f)
	}
	if h, err := parseHandle(f.handle); err != nil || h.target != (waitTarget{kind: targetTurn, id: "turn-1"}) {
		t.Fatalf("the handle %q: %+v %v", f.handle, h, err)
	}
}
