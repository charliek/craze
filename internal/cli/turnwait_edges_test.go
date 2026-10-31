package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// The waiter's edges the C2 review (astra, round 1) found, each its event
// sequence placed by the test: the stream's end ahead of a submit's reply,
// another incarnation's lastTurn and turn-1, an unrelated queued row, an
// empty interjection, a withdrawn turn's usage, a restored row behind a
// refused replay, a successor's ask, a read that fails or stalls, the
// session's end, a cleanup that outlives a restoration, and a detach a
// second signal must not wait for.

// TestWaitTurnReplaysTheBufferPastTheStreamsEnd (review 1): the turn's
// started, text and ended, then the session's end and the stream's loss, all
// reach the waiter before the submit's reply: the result is the turn's,
// replayed from the buffer, never the loss.
func TestWaitTurnReplaysTheBufferPastTheStreamsEnd(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("seen before the end")))
	conn := r.dial(nil)
	lost, quit := make(chan struct{}), r.gate()
	var once sync.Once
	prev := waitBuffered
	waitBuffered = func(p pumped) {
		if p.err != nil {
			once.Do(func() { close(lost) })
		}
	}
	t.Cleanup(func() { waitBuffered = prev })
	submit := func(ctx context.Context, ident uint64) (waitTarget, error) {
		target, err := submitPrompt(conn, "go")(ctx, ident)
		if err != nil {
			return target, err
		}
		// The turn is over; the session ends with the reply still held.
		waitFor(t, "the turn's end", func() bool { return r.eng.State().LastTurn != nil })
		_ = r.eng.Close()
		select {
		case <-lost:
		case <-quit.ch:
		}
		return target, nil
	}
	res, f := goWait(conn, waitStart{submit: submit}, waitOptions{}).result(t)
	quit.open()
	wantResult(t, res, f, statusCompleted)
	if res.Turn != "turn-1" || res.message() != "seen before the end" {
		t.Fatalf("the result: %+v", res)
	}
}

// TestWaitTurnNeverTakesAnotherIncarnationsLastTurn (review 2): a refused
// replay of incarnation A's turn-1, which failed, is reconciled; its
// session.state read goes out as the engine is replaced by incarnation B,
// whose own turn-1 completed, and its connection drops. The read is made
// again on B's connection — never taken: the turn's ending was not observed.
func TestWaitTurnNeverTakesAnotherIncarnationsLastTurn(t *testing.T) {
	r := newWaitRig(t, 4)
	var chunks []agent.Event
	for _, s := range []string{"a ", "long ", "failing ", "turn ", "in ", "chunks"} {
		chunks = append(chunks, textEv(s))
	}
	r.script(stubTurn{emit: chunks, err: errors.New("boom")})
	r.submit("a")
	waitFor(t, "A's turn-1 to fail", func() bool {
		lt := r.eng.State().LastTurn
		return lt != nil && lt.Outcome == "failed"
	})
	incA := r.eng.State().Incarnation
	_, engB := r.newEngine(0, answerTurn(textEv("B's")))
	if res, err := engB.Submit(engine.Command{}, "b", engine.SubmitQueue, ""); err != nil || res.Turn != "turn-1" {
		t.Fatalf("B's turn: %+v %v", res, err)
	}
	waitFor(t, "B's turn-1 to complete", func() bool {
		lt := engB.State().LastTurn
		return lt != nil && lt.Outcome == "done"
	})
	tp := &tap{}
	tp.on(`"method":"session.state"`, func() bool {
		r.srv.SetEngine(engB)
		tp.drop()
		return false
	})
	h := turnHandle{incarnation: incA, seq: 0, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	conn, _ := r.dialTappedFrom(&protocol.Cursor{Incarnation: incA, Seq: 0}, tp)
	res, f := goWait(conn, waitStart{from: &h}, waitOptions{}).result(t)
	if res != nil || f == nil || f.msg != "the turn's ending was not observed" {
		t.Fatalf("the wait across B's turn-1: %+v, %+v", res, f)
	}
}

// TestWaitTurnNeverTakesAnotherRowForItsRestoration (review 3): a turn
// submitted directly, refused by the agent's own turn just after another
// client's row was queued — the row's `queued` right before its synthetic
// ending — is that refusal, failed: never the other row's wait.
func TestWaitTurnNeverTakesAnotherRowForItsRestoration(t *testing.T) {
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(stubTurn{before: func() { <-g.ch }, err: agent.ErrForeignTurn})
	conn := r.dial(nil)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
	r.waitStarted("turn-1")
	r.submit("someone else's")
	r.waitQueued(1)
	g.open()
	res, f := w.result(t)
	res = wantResult(t, res, f, statusFailed)
	if res.Turn != "turn-1" || res.Row != "" || res.Error == nil || !strings.Contains(res.Error.Message, "running a turn of its own") || res.Error.Kind != errkind.Agent || !res.Error.Retryable {
		t.Fatalf("the refused turn's result: %+v", res)
	}
}

// TestAccumulatorSplitsInterjectionsAsTheTranscript (review 4): an empty,
// or shell-context-only, interjection draws no user row in the transcript
// and so splits no assistant entry; one with text does. The accumulator's
// message is the transcript's last assistant entry in each case.
func TestAccumulatorSplitsInterjectionsAsTheTranscript(t *testing.T) {
	block := agent.ShellContextBlock([]agent.ShellResult{{Command: "ls", Exit: 0, Output: "a.go"}})
	for _, c := range []struct {
		name, text, want string
	}{
		{"empty", "", "before after"},
		{"shell context only", block, "before after"},
		{"with text", block + "and also this", "after"},
	} {
		evs := []agent.Event{
			textEv("before "),
			{Type: agent.EventUser, Text: c.text, Interjection: true},
			textEv("after"),
		}
		a := newTurnAccumulator()
		m := transcript.New(transcript.Options{})
		for i, ev := range evs {
			ev.Seq = uint64(i + 1)
			ev.At = time.Unix(int64(i+1), 0)
			a.fold(ev)
			m.Fold(ev)
		}
		got, _ := a.message()
		snap, err := m.Snapshot(0)
		if err != nil {
			t.Fatal(err)
		}
		var last string
		for _, e := range snap.Main.Entries {
			if e.Kind == transcript.KindAssistant {
				last = e.Text
			}
		}
		if got != c.want || last != c.want {
			t.Errorf("%s: the accumulator's message %q, the transcript's last assistant entry %q; want %q", c.name, got, last, c.want)
		}
	}
}

// TestWaitPlainIdleCarriesNoOtherTurnsUsage (review 5): turn-1 reports usage
// and completes; turn-2 is withdrawn before it opens, reporting none. Plain
// wait on the idle session reports turn-2 — with no usage, never turn-1's.
func TestWaitPlainIdleCarriesNoOtherTurnsUsage(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("one"), usageEv(40_000_000_000, false)), stubTurn{err: agent.ErrPromptCancelled})
	r.submit("one")
	r.waitIdle()
	r.submit("two")
	waitFor(t, "turn-2's withdrawal", func() bool {
		lt := r.eng.State().LastTurn
		return lt != nil && lt.TurnID == "turn-2"
	})
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--json"})
	m := decodeResult(t, stdout)
	if code != 3 || m["status"] != "cancelled" || m["turn"] != "turn-2" || m["usage"] != nil {
		t.Fatalf("plain wait after a withdrawn turn: exit %d, %v, %s", code, m, stderr)
	}
}

// TestWaitTurnReconcilesARestoredRow (review 6): the waiter has followed its
// row into its turn; its connection drops; the turn is refused by the agent's
// own turn and the row put back; the replay is gone (the ring evicted it).
// The snapshot the re-attach brings shows the row queued: the waiter waits
// for it again — never the refused turn's failure — and reports its later
// run.
func TestWaitTurnReconcilesARestoredRow(t *testing.T) {
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
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
	r.waitQueued(1)
	first.open()
	r.waitStarted("turn-2")
	// The row's sent and its turn's started are on the waiter's stream.
	var sync protocol.SyncResult
	if err := conn.c.Call(stepCtx(t), protocol.MethodSessionSync, protocol.SyncParams{SessionID: waitRigID}, &sync); err != nil {
		t.Fatal(err)
	}
	redial := r.gate()
	tp.holdRedials(redial)
	tp.drop()
	refuse.open()
	waitFor(t, "the row put back", func() bool {
		st := r.eng.State()
		return len(st.Queue) == 1 && st.LastTurn != nil && st.LastTurn.TurnID == "turn-2"
	})
	for range 6 {
		r.s.emit(agent.Event{Type: agent.EventThought, Text: "evicting the replay"})
	}
	redial.open()
	// The re-attach's reply — the cursor refused, the snapshot cut with the
	// row queued — has reached the client; only then may the row run again.
	waitFor(t, "the refused replay's snapshot", func() bool { return tp.read(`"reset":`) })
	r.s.setForeign(false)
	res, f := w.result(t)
	res = wantResult(t, res, f, statusCompleted)
	if res.Row != "q-1" || res.Turn != "turn-3" || res.message() != "mine, at last" || !res.Partial {
		t.Fatalf("the restored row's result: %+v", res)
	}
}

// TestWaitTurnNeverReportsASuccessorsAsk (review 7, rounds 1 and 2): the
// waiter's turn ends, a successor starts and opens an ask — all before the
// waiter has folded its turn's ending. The open ask is the successor's: the
// result is the turn's own, completed. The successor is the turn queued
// behind it (the reply that names the target held until the ask is open), or
// — for the agent's own turn — the agent's next one, wake-2 after wake-1,
// which the look finds running in wake-1's place (placed as the waiter's look
// goes out).
func TestWaitTurnNeverReportsASuccessorsAsk(t *testing.T) {
	t.Run("craze's own successor", func(t *testing.T) {
		r := newWaitRig(t, 0)
		one, two, reply := r.gate(), r.gate(), r.gate()
		r.script(heldTurn(one, textEv("one's answer")),
			stubTurn{before: func() {
				r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell"}})
				<-two.ch
			}, emit: []agent.Event{{Type: agent.EventDone, StopReason: "end_turn"}}, res: agent.Result{StopReason: "end_turn"}})
		conn := r.dial(nil)
		submit := func(ctx context.Context, ident uint64) (waitTarget, error) {
			target, err := submitPrompt(conn, "one")(ctx, ident)
			<-reply.ch
			return target, err
		}
		w := goWait(conn, waitStart{submit: submit}, waitOptions{})
		r.waitStarted("turn-1")
		r.submit("two")
		r.waitQueued(1)
		one.open()
		waitFor(t, "the successor's ask", func() bool { return r.eng.State().PendingAsks == 1 })
		reply.open()
		res, f := w.result(t)
		res = wantResult(t, res, f, statusCompleted)
		if res.Turn != "turn-1" || res.message() != "one's answer" || res.Ask != nil {
			t.Fatalf("the result: %+v", res)
		}
	})
	t.Run("the agent's own successor", func(t *testing.T) {
		r := newWaitRig(t, 0)
		wake := func(id string, running bool) {
			r.s.mu.Lock()
			r.s.foreign = running
			r.s.mu.Unlock()
			r.s.emit(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: id, Running: running}})
		}
		wake("wake-1", true)
		r.s.emit(textEv("wake-1's answer"))
		tp := &tap{}
		// The waiter met wake-1 running and caught up; as its look goes out,
		// wake-1 ends, wake-2 starts and asks.
		tp.on(`"method":"session.snapshot"`, func() bool {
			wake("wake-1", false)
			wake("wake-2", true)
			r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-2", Tool: "Shell"}})
			waitFor(t, "wake-2's ask", func() bool { return r.eng.State().PendingAsks == 1 })
			return true
		})
		conn, _ := r.dialTappedFrom(nil, tp)
		res, f := goWait(conn, waitStart{}, waitOptions{}).result(t)
		res = wantResult(t, res, f, statusCompleted)
		if res.Turn != "wake-1" || res.Ask != nil || res.message() != "wake-1's answer" {
			t.Fatalf("the result: %+v", res)
		}
		if len(tp.lines(protocol.MethodSessionSnapshot)) == 0 {
			t.Fatal("the waiter never looked")
		}
	})
}

// TestWaitTurnRetriesAFailedAskLook (review 7): the target's turn holds an
// open ask; the waiter's first look at where the session stands
// (session.snapshot) goes unanswered (its read fails at readBound). It looks
// again — a parked ask brings no event that would — and reports needs_input.
func TestWaitTurnRetriesAFailedAskLook(t *testing.T) {
	shorten(t, &readBound, 200*time.Millisecond)
	shorten(t, &askRetryEvery, 10*time.Millisecond)
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(stubTurn{before: func() {
		r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell"}})
		<-g.ch
	}, emit: []agent.Event{{Type: agent.EventDone, StopReason: "end_turn"}}, res: agent.Result{StopReason: "end_turn"}})
	r.submit("someone else's")
	waitFor(t, "the ask", func() bool { return r.eng.State().PendingAsks == 1 })
	tp := &tap{}
	tp.on(`"method":"session.snapshot"`, func() bool { return false })
	conn, _ := r.dialTappedFrom(nil, tp)
	res, f := goWait(conn, waitStart{}, waitOptions{}).result(t)
	res = wantResult(t, res, f, statusNeedsInput)
	if res.Ask == nil || res.Ask.ID != "perm-1" || res.Turn != "turn-1" {
		t.Fatalf("the result: %+v", res)
	}
}

// TestWaitTurnNeverCancelsAnotherIncarnationsTurn (review 8): an owned wait
// follows A's turn-1; its connection drops and the redial is held; the
// engine is replaced by B, which runs a turn-1 of its own; a signal comes.
// The cancel, bound to A's identity, is never sent under B's: B's turn-1 is
// untouched, and the cleanup ends with the target's ending not observed.
func TestWaitTurnNeverCancelsAnotherIncarnationsTurn(t *testing.T) {
	r := newWaitRig(t, 0)
	a := r.gate()
	r.script(heldTurn(a))
	b := &gate{ch: make(chan struct{})}
	sB, engB := r.newEngine(0, heldTurn(b))
	t.Cleanup(b.open)
	if res, err := engB.Submit(engine.Command{}, "B's own", engine.SubmitQueue, ""); err != nil || res.Turn != "turn-1" {
		t.Fatalf("B's turn: %+v %v", res, err)
	}
	conn, tp := r.dialTapped()
	commanding := make(chan string, 4)
	prev := waitCommanding
	waitCommanding = func(m string) { commanding <- m }
	t.Cleanup(func() { waitCommanding = prev })
	sigs := make(chan os.Signal, 2)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "A's")}, waitOptions{sigs: sigs})
	r.waitStarted("turn-1")
	// The waiter's reads are bound to A: it has looked for an ask.
	waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	redial := r.gate()
	tp.holdRedials(redial)
	tp.drop()
	r.srv.SetEngine(engB)
	// The signal waits until the client has seen the drop (its redial is
	// held): a cancel written before that went out on the dead connection,
	// which the tap records though nothing received it (seen on CI).
	tp.awaitRedial(t)
	sigs <- syscall.SIGINT
	// The cancel is on its way, its connection gone: the redial lands on B.
	select {
	case m := <-commanding:
		if m != protocol.MethodSessionCancel {
			t.Fatalf("the cleanup sends %s", m)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("the cleanup sent no cancel")
	}
	redial.open()
	res, f := w.result(t)
	if len(tp.lines(protocol.MethodSessionCancel)) != 0 || sB.cancelsReturned() != 0 || engB.State().Turn != "turn-1" {
		t.Fatalf("B's turn-1 was touched: cancels %q, B's state %+v", tp.lines(protocol.MethodSessionCancel), engB.State())
	}
	if res != nil || f == nil || !strings.Contains(f.msg, "host changed") {
		t.Fatalf("the cleanup across B: %+v, %+v", res, f)
	}
}

// TestWaitSignalEndsTheAttach (review 8): a session whose start never comes
// keeps craze wait's attach waiting; a signal ends it at once — exit 1, the
// session untouched — and so does --timeout's end.
func TestWaitSignalEndsTheAttach(t *testing.T) {
	env, _ := serveHome(t)
	start := make(chan struct{})
	t.Cleanup(func() { close(start) })
	serveFakeHost(t, env, fakehost.Options{HostID: "0000000000c3", CrazeSessionID: "0199c0de-3333-7000-8000-00000000c003",
		Start: func(ctx context.Context) error {
			select {
			case <-start:
			case <-ctx.Done():
			}
			return nil
		}})
	sigs := withWaitSignals(t)
	run := goCraze("wait", "0000c003")
	sigs <- syscall.SIGINT
	stdout, stderr, code := run.wait(t)
	if code != 1 || stdout != "" || stderr != "craze wait: interrupted before session 0000c003 answered\n" {
		t.Fatalf("a signal during the attach: exit %d, %q %q", code, stdout, stderr)
	}
	stdout, stderr, code = executeErr([]string{"wait", "0000c003", "--timeout", "200ms"})
	if code != 1 || stdout != "" || stderr != "craze wait: timed out before session 0000c003 answered\n" {
		t.Fatalf("--timeout during the attach: exit %d, %q %q", code, stdout, stderr)
	}
}

// TestWaitTurnInterruptsAStalledAskLook (review 9, rounds 1 and 2): the
// target's turn holds an open permission, which the waiter's look
// (session.snapshot) would report; the look goes unanswered for good.
// --timeout's end is running, with the handle; plain wait's signal is
// interrupted; an owned wait's signal cancels its turn by id — never
// needs_input from a look that never came back. A look the engine's
// replacement cut off — the client's identity moved to another incarnation —
// is the turn's ending not observed, exit 1 with the handle.
func TestWaitTurnInterruptsAStalledAskLook(t *testing.T) {
	shorten(t, &readBound, time.Hour)
	// asking is a rig whose turn opens a permission and runs until it is
	// cancelled (signalRig's, asking).
	asking := func(t *testing.T) *waitRig {
		r := newWaitRig(t, 0)
		cancelled := r.gate()
		r.s.mu.Lock()
		r.s.onCancel = cancelled.open
		r.s.mu.Unlock()
		r.script(stubTurn{before: func() {
			r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell"}})
			<-cancelled.ch
		}, emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}}, res: agent.Result{StopReason: "cancelled"}})
		return r
	}
	// stall makes every look go unanswered, closing the channel it answers
	// as the first goes out, the ask open by then.
	stall := func(t *testing.T, r *waitRig, tp *tap) <-chan struct{} {
		stalled := make(chan struct{})
		tp.on(`"method":"session.snapshot"`, func() bool {
			waitFor(t, "the ask", func() bool { return r.eng.State().PendingAsks == 1 })
			close(stalled)
			return false
		})
		tp.swallow(`"method":"session.snapshot"`)
		return stalled
	}
	stalledBy := func(t *testing.T, stalled <-chan struct{}) {
		t.Helper()
		select {
		case <-stalled:
		case <-time.After(stubWatchdog):
			t.Fatal("the waiter never looked")
		}
	}
	t.Run("timeout", func(t *testing.T) {
		r := asking(t)
		r.submit("someone else's")
		tp := &tap{}
		stalled := stall(t, r, tp)
		conn, _ := r.dialTappedFrom(nil, tp)
		fire := make(chan time.Time)
		w := goWait(conn, waitStart{}, waitOptions{timeout: fire})
		stalledBy(t, stalled)
		close(fire)
		res, f := w.result(t)
		res = wantResult(t, res, f, statusRunning)
		if res.Turn != "turn-1" || res.Handle == "" || res.Ask != nil {
			t.Fatalf("the timed-out result: %+v", res)
		}
	})
	t.Run("plain wait's signal", func(t *testing.T) {
		r := asking(t)
		r.submit("someone else's")
		tp := &tap{}
		stalled := stall(t, r, tp)
		conn, _ := r.dialTappedFrom(nil, tp)
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{}, waitOptions{sigs: sigs})
		stalledBy(t, stalled)
		sigs <- syscall.SIGTERM
		res, f := w.result(t)
		if res != nil || f == nil || f.msg != "interrupted; the turn goes on" || f.handle == "" {
			t.Fatalf("the interrupted wait: %+v, %+v", res, f)
		}
	})
	t.Run("an owned wait's signal", func(t *testing.T) {
		r := asking(t)
		tp := &tap{}
		stalled := stall(t, r, tp)
		conn, _ := r.dialTappedFrom(nil, tp)
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
		stalledBy(t, stalled)
		sigs <- syscall.SIGINT
		res, f := w.result(t)
		res = wantResult(t, res, f, statusCancelled)
		if res.Turn != "turn-1" || res.Ask != nil || statusExit(res.Status) != 3 {
			t.Fatalf("the owned wait's signal: %+v", res)
		}
		if cancels := tp.lines(protocol.MethodSessionCancel); len(cancels) != 1 || !strings.Contains(cancels[0], `"turnId":"turn-1"`) {
			t.Fatalf("the cancels sent: %q", cancels)
		}
	})
	t.Run("another incarnation", func(t *testing.T) {
		r := asking(t)
		_, engB := r.newEngine(0)
		r.submit("someone else's")
		tp := &tap{}
		tp.on(`"method":"session.snapshot"`, func() bool {
			waitFor(t, "the ask", func() bool { return r.eng.State().PendingAsks == 1 })
			r.srv.SetEngine(engB)
			tp.drop()
			return false
		})
		conn, _ := r.dialTappedFrom(nil, tp)
		res, f := goWait(conn, waitStart{}, waitOptions{}).result(t)
		if res != nil || f == nil || f.msg != "the turn's ending was not observed" {
			t.Fatalf("the look across B: %+v, %+v", res, f)
		}
		if h, err := parseHandle(f.handle); err != nil || h.target != (waitTarget{kind: targetTurn, id: "turn-1"}) {
			t.Fatalf("the handle %q: %+v %v", f.handle, h, err)
		}
	})
}

// TestWaitTurnSessionEndKeepsTheHandle (review 11): a session that ends
// while the waiter follows a target — reset{session_closed}, the stop
// sequence placed exactly (a fake host's run_stop), or its host gone and its
// registry entry with it — is exit 1 with the handle.
func TestWaitTurnSessionEndKeepsTheHandle(t *testing.T) {
	wantEnded := func(t *testing.T, w *waitRun, foreign string) {
		t.Helper()
		res, f := w.result(t)
		if res != nil || f == nil || !strings.HasPrefix(f.msg, "the session ended") || f.handle == "" {
			t.Fatalf("the session's end: %+v, %+v", res, f)
		}
		if h, err := parseHandle(f.handle); err != nil || h.target != (waitTarget{kind: targetForeign, id: foreign}) {
			t.Fatalf("the handle %q: %+v %v", f.handle, h, err)
		}
	}
	t.Run("the session's end", func(t *testing.T) {
		env, _ := serveHome(t)
		h := serveFakeHost(t, env, fakehost.Options{HostID: "0000000000d4", CrazeSessionID: "0199c0de-4444-7000-8000-00000000d004", Stop: true})
		h.ForeignTurn("wake-1", "", "", true)
		entries, err := rundir.Hosts(env)
		if err != nil || len(entries) != 1 {
			t.Fatalf("the registry: %+v %v", entries, err)
		}
		tp := &tap{}
		rc, err := remote.Dial(stepCtx(t), entries[0].Socket, remote.Options{Dial: tp.dial})
		if err != nil {
			t.Fatal(err)
		}
		st, err := rc.Attach(stepCtx(t), remote.AttachOptions{SessionID: h.SessionID(), When: protocol.WhenReady})
		if err != nil {
			t.Fatal(err)
		}
		conn := &waitConn{c: rc, st: st, entry: entries[0], sessionID: h.SessionID()}
		t.Cleanup(func() { conn.close(nil) })
		w := goWait(conn, waitStart{}, waitOptions{})
		// The attachment synchronized — its look for an open ask made — before
		// the session ends: an end before that is the transport's, not a
		// reset{session_closed} (remote's TestASessionClosedEndsTheStream).
		waitFor(t, "the waiter caught up", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
		c, err := dialCLI(stepCtx(t), entries[0], "test")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Command(stepCtx(t), protocol.MethodSessionStop, protocol.StopParams{SessionID: h.SessionID()}, nil, remote.CommandOptions{}); err != nil {
			t.Fatalf("session.stop: %v", err)
		}
		if err := h.RunStop(); err != nil {
			t.Fatal(err)
		}
		wantEnded(t, w, "wake-1")
	})
	t.Run("the host gone", func(t *testing.T) {
		r := newWaitRig(t, 0)
		r.s.setForeign(true)
		waitFor(t, "the agent's own turn", func() bool { return r.s.ForeignTurn() })
		conn := r.dial(nil)
		// The host is this test's process: named by a pid nothing has, it has
		// gone with its registry entry.
		conn.entry.PID = exitedPID(t)
		w := goWait(conn, waitStart{}, waitOptions{})
		_ = r.reg.Close()
		ctx, cancel := context.WithTimeout(context.Background(), serveStep)
		_ = r.srv.Close(ctx)
		cancel()
		wantEnded(t, w, "interject-fallback-1")
	})
}

// TestWaitTurnCleanupOutlivesARestoration (review, extra a): a signal comes
// while the waiter's row waits; its removal finds the row sent already
// (unknown_row), its turn's cancel finds the turn refused and the row put
// back already (stale_turn). Folding the restoration, the cleanup removes
// the row again: dropped, never `cancel sent, not confirmed`.
func TestWaitTurnCleanupOutlivesARestoration(t *testing.T) {
	r := newWaitRig(t, 0)
	first, refuse := r.gate(), r.gate()
	r.script(heldTurn(first, textEv("first")),
		stubTurn{before: func() {
			<-refuse.ch
			r.s.mu.Lock()
			r.s.foreign = true
			r.s.mu.Unlock()
		}, err: agent.ErrForeignTurn},
		answerTurn(textEv("never")))
	r.submit("first")
	conn, tp := r.dialTapped()
	// The removal lands once the row is sent and its turn runs.
	tp.on(`"method":"session.queue.remove"`, func() bool {
		first.open()
		r.waitStarted("turn-2")
		return true
	})
	// The cancel lands once the turn was refused and the row put back.
	tp.on(`"method":"session.cancel"`, func() bool {
		refuse.open()
		waitFor(t, "the row put back", func() bool {
			st := r.eng.State()
			return len(st.Queue) == 1 && st.LastTurn != nil && st.LastTurn.TurnID == "turn-2"
		})
		return true
	})
	sigs := make(chan os.Signal, 2)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
	r.waitQueued(1)
	waitFor(t, "the prompt's reply", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
	sigs <- syscall.SIGTERM
	res, f := w.result(t)
	res = wantResult(t, res, f, statusDropped)
	if res.Row != "q-1" || len(r.eng.State().Queue) != 0 || len(tp.lines(protocol.MethodQueueRemove)) != 2 {
		t.Fatalf("the cleanup: %+v, queue %+v, removals %q", res, r.eng.State().Queue, tp.lines(protocol.MethodQueueRemove))
	}
}

// TestWaitTurnSignalExitKeepsToItsBudget (review, extra b): the host never
// answers a detach. After a second signal the command's close abandons it at
// once; after one signal's cleanup, the detach takes no more than what is
// left of the cleanup's budget — never detachBound.
func TestWaitTurnSignalExitKeepsToItsBudget(t *testing.T) {
	// The close, with a detach bound longer than the test: only what the
	// signal left it (the cleanup's deadline, or nothing) ends it. Set here,
	// at the end, and not for the whole test: a test that failed earlier
	// closes its connection at the usual bound.
	closeWithin := func(t *testing.T, conn *waitConn) {
		t.Helper()
		shorten(t, &detachBound, time.Hour)
		closed := make(chan struct{})
		go func() { conn.close(nil); close(closed) }()
		select {
		case <-closed:
		case <-time.After(stubWatchdog):
			t.Fatal("the close waited on the detach")
		}
	}
	t.Run("a second signal", func(t *testing.T) {
		shorten(t, &cleanupWait, time.Hour)
		r := signalRig(t)
		hold := r.gate()
		r.s.mu.Lock()
		r.s.cancelHold = hold.ch
		r.s.mu.Unlock()
		conn, tp := r.dialTapped()
		tp.swallow(`"method":"session.detach"`)
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{sigs: sigs})
		r.waitStarted("turn-1")
		waitFor(t, "the waiter's target", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		sigs <- syscall.SIGINT
		waitFor(t, "the cancel sent", func() bool { return len(tp.lines(protocol.MethodSessionCancel)) == 1 })
		sigs <- syscall.SIGINT
		if _, f := w.result(t); f == nil || !conn.abandon {
			t.Fatalf("the second signal: %+v, abandon %v", f, conn.abandon)
		}
		closeWithin(t, conn)
	})
	t.Run("one signal", func(t *testing.T) {
		shorten(t, &cleanupWait, 500*time.Millisecond)
		r := newWaitRig(t, 0)
		g := r.gate()
		r.script(heldTurn(g))
		r.submit("first")
		conn, tp := r.dialTapped()
		tp.swallow(`"method":"session.detach"`)
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
		r.waitQueued(1)
		waitFor(t, "the prompt's reply", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		sigs <- syscall.SIGTERM
		res, f := w.result(t)
		wantResult(t, res, f, statusDropped)
		if conn.closeBy.IsZero() || conn.abandon {
			t.Fatalf("the close's bound: %v, abandon %v", conn.closeBy, conn.abandon)
		}
		closeWithin(t, conn)
	})
	t.Run("a signal during the detach", func(t *testing.T) {
		// The cleanup's budget, and the detach's bound, are longer than the
		// test: the detach the host never answers waits for good — until
		// the next signal, which comes once the waiter has stopped reading
		// them (review round 2, item 12).
		shorten(t, &cleanupWait, time.Hour)
		r := newWaitRig(t, 0)
		g := r.gate()
		r.script(heldTurn(g))
		r.submit("first")
		conn, tp := r.dialTapped()
		tp.swallow(`"method":"session.detach"`)
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
		r.waitQueued(1)
		waitFor(t, "the prompt's reply", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		sigs <- syscall.SIGTERM
		res, f := w.result(t)
		wantResult(t, res, f, statusDropped)
		shorten(t, &detachBound, time.Hour)
		closed := make(chan struct{})
		go func() { conn.close(sigs); close(closed) }()
		waitFor(t, "the detach sent", func() bool { return len(tp.lines(protocol.MethodSessionDetach)) == 1 })
		sigs <- syscall.SIGINT
		select {
		case <-closed:
		case <-time.After(stubWatchdog):
			// Ends the transport, and the detach waiting on it with it.
			_ = conn.c.Close()
			t.Fatal("the close waited on the detach past the second signal")
		}
	})
}

// TestWaitTurnRowHandleFollowsTheRowThroughItsRestoration (review 3, its
// other half): restoring knows only the target's own row, so a handle taken
// while a row's turn runs names the row — from before its `sent` — not the
// turn. Followed from it after the turn was refused and the row put back,
// the wait follows the row into its later run: completed, whole.
func TestWaitTurnRowHandleFollowsTheRowThroughItsRestoration(t *testing.T) {
	r := newWaitRig(t, 0)
	first, refuse := r.gate(), r.gate()
	r.script(heldTurn(first, textEv("first")), stubTurn{before: func() { <-refuse.ch }, err: agent.ErrForeignTurn},
		answerTurn(textEv("mine, at last")))
	r.submit("first")
	conn := r.dial(nil)
	fire := make(chan time.Time)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{timeout: fire})
	r.waitQueued(1)
	first.open()
	r.waitStarted("turn-2")
	// The row's turn is on the waiter's stream before the timeout.
	var sync protocol.SyncResult
	if err := conn.c.Call(stepCtx(t), protocol.MethodSessionSync, protocol.SyncParams{SessionID: waitRigID}, &sync); err != nil {
		t.Fatal(err)
	}
	close(fire)
	res, f := w.result(t)
	res = wantResult(t, res, f, statusRunning)
	h, err := parseHandle(res.Handle)
	if err != nil || h.target != (waitTarget{kind: targetRow, id: "q-1"}) {
		t.Fatalf("the running result %+v: its handle %+v, %v", res, h, err)
	}
	refuse.open()
	r.waitIdle()
	from := r.dial(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq})
	res, f = goWait(from, waitStart{from: &h}, waitOptions{}).result(t)
	res = wantResult(t, res, f, statusCompleted)
	if res.Row != "q-1" || res.Turn != "turn-3" || res.message() != "mine, at last" || res.Partial {
		t.Fatalf("the row followed from its handle: %+v", res)
	}
}
