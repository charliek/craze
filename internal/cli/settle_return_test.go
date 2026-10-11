package cli

import (
	"context"
	"encoding/json"
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

// A returned plan's turn settled from a snapshot (C3 review r2, 4): wherever
// the waiter settles a cancelled turn, the X28 rule applies when the evidence
// is there — the plan the waiter folded itself, or the turn's events as the
// host still holds them — and what can be proven (cancelled) when it is not.

// returnedTurn is a stub turn whose plan the host returned: the automatic
// plan answer that did not accept, then the cancelled ending its cancel gave.
func returnedTurn() stubTurn {
	return stubTurn{
		emit: []agent.Event{
			{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "the plan", Plan: "## Steps", Auto: true}},
			{Type: agent.EventDone, StopReason: "cancelled"},
		},
		res: agent.Result{StopReason: "cancelled"},
	}
}

// TestAPlainWaitFindsAReturnedPlan: plain craze wait at a session idle after
// a turn whose plan was returned settles it from lastTurn (cancelled) — and
// finds the returned plan in the turn's events, which the host still holds:
// `completed`, with the plan and its automatic answer, partial. Once those
// events are gone, what can be proven is said: `cancelled`.
func TestAPlainWaitFindsAReturnedPlan(t *testing.T) {
	r := newWaitRig(t, 0, returnedTurn())
	r.submit("plan it")
	r.waitIdle()
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--json"})
	m := decodeResult(t, stdout)
	auto, _ := json.Marshal(m["autoAnswered"])
	if code != 0 || m["status"] != "completed" || m["plan"] != "## Steps" || m["partial"] != true ||
		!strings.Contains(string(auto), `"choice":"returned"`) {
		t.Fatalf("plain wait after a returned plan: exit %d, %v, %q", code, m, stderr)
	}
}

// TestAPlainWaitWithTheTurnsEventsGoneSaysCancelled: the same turn, its events
// evicted from the host's ring before the wait: the returned plan cannot be
// found, and the turn is what lastTurn proves — `cancelled`, with no plan.
func TestAPlainWaitWithTheTurnsEventsGoneSaysCancelled(t *testing.T) {
	r := newWaitRig(t, 4, returnedTurn())
	r.submit("plan it")
	r.waitIdle()
	for range 8 {
		r.s.emit(agent.Event{Type: agent.EventThought, Text: "evicting the turn"})
	}
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--json"})
	m := decodeResult(t, stdout)
	if code != 3 || m["status"] != "cancelled" || m["plan"] != nil {
		t.Fatalf("plain wait once the turn's events are gone: exit %d, %v, %q", code, m, stderr)
	}
}

// TestAReconciledReturnedPlanIsCompleted: the waiter folds its own turn's
// returned plan, loses its connection, and the turn ends cancelled while it
// is away; the host can no longer replay that far, so the turn is settled
// from lastTurn — and the plan the waiter already folded makes it
// `completed`, as the live ending would have.
func TestAReconciledReturnedPlanIsCompleted(t *testing.T) {
	r := newWaitRig(t, 4)
	g := r.gate()
	r.script(stubTurn{before: func() { <-g.ch }, emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}},
		res: agent.Result{StopReason: "cancelled"}})
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "plan it")}, waitOptions{})
	r.waitStartedPublished("turn-1")
	r.s.emit(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "the plan", Plan: "## Steps", Auto: true}})
	waitFor(t, "the waiter's client to read the plan", func() bool { return tp.read(`"auto":true`) })
	redial := r.gate()
	tp.holdRedials(redial)
	tp.drop()
	g.open()
	r.waitIdle()
	for range 8 {
		r.s.emit(agent.Event{Type: agent.EventThought, Text: "evicting the replay"})
	}
	redial.open()
	res, f := w.result(t)
	res = wantResult(t, res, f, statusCompleted)
	if res.Plan != "## Steps" || !res.Partial || len(res.AutoAnswered) != 1 || res.AutoAnswered[0].Choice != "returned" {
		t.Fatalf("the reconciled result %+v", res)
	}
}

// The rest is the review's round 3 (C3 r3, 2 and 3): a snapshot of the turn
// while it still runs keeps, or finds, the returned plan, and a lookup an
// interrupt cuts short is the interrupt's.

// heldReturn scripts a turn that waits for g, then ends cancelled — the
// cancel the host's return wrote — and answers g; the test emits the plan.
func (r *waitRig) heldReturn() *gate {
	g := r.gate()
	r.script(stubTurn{before: func() { <-g.ch }, emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}},
		res: agent.Result{StopReason: "cancelled"}})
	return g
}

// waitStartedPublished waits for turn's started on the session's stream, read
// from the start of the incarnation on a connection of the test's own: an
// event emitted after it is the turn's. The engine runs the turn — State's
// Turn, which waitStarted reads — before its started is published.
func (r *waitRig) waitStartedPublished(turn string) {
	r.t.Helper()
	r.waitStarted(turn)
	probe := r.dial(&protocol.Cursor{Incarnation: r.eng.State().Incarnation})
	for {
		it, err := probe.st.Next(stepCtx(r.t))
		if err != nil {
			r.t.Fatalf("reading for %s's started: %v", turn, err)
		}
		if it.Kind != remote.KindEvent {
			continue
		}
		ev, err := agent.DecodeEvent(string(it.Body))
		if err == nil && ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.ID == turn && ev.Turn.Phase == agent.TurnStarted {
			return
		}
	}
}

// returnedPlanEv is the automatic plan answer that did not accept: the
// return policy's record of a returned plan.
func returnedPlanEv() agent.Event {
	return agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "the plan", Plan: "## Steps", Auto: true}}
}

// wantReturned is a completed result carrying the returned plan.
func wantReturned(t *testing.T, res *turnResult, f *waitFailure) {
	t.Helper()
	res = wantResult(t, res, f, statusCompleted)
	if res.Plan != "## Steps" || !res.Partial || len(res.AutoAnswered) != 1 || res.AutoAnswered[0].Choice != "returned" {
		t.Fatalf("the returned plan's result %+v", res)
	}
}

// TestAPlainWaitMeetingTheWindDownFindsTheReturnedPlan (r3, 2): the host has
// returned the plan, and plain craze wait attaches while the agent is still
// winding down — its snapshot shows the turn running, without the plan,
// which no snapshot carries. The turn's cancelled ending is settled by the
// replay lookup: `completed`, with the plan. The waiter's own stream never
// carried the plan.
func TestAPlainWaitMeetingTheWindDownFindsTheReturnedPlan(t *testing.T) {
	r := newWaitRig(t, 0)
	g := r.heldReturn()
	r.submit("plan it")
	r.waitStartedPublished("turn-1")
	r.s.emit(returnedPlanEv())
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{}, waitOptions{})
	g.open()
	res, f := w.result(t)
	wantReturned(t, res, f)
	if tp.read(`"auto":true`) {
		t.Fatal("the waiter's stream carried the plan: the snapshot path was not taken")
	}
}

// TestAReturnedPlanSurvivesASnapshotOfItsRunningTurn (r3, 2): the waiter
// folds its own turn's returned plan, loses its connection, and is restored
// from a snapshot while the turn still runs — the host can no longer replay
// that far, so the turn's events are gone for the lookup too. What the waiter
// folded of the same turn is kept, and the cancelled ending is `completed`.
func TestAReturnedPlanSurvivesASnapshotOfItsRunningTurn(t *testing.T) {
	r := newWaitRig(t, 4)
	g := r.heldReturn()
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "plan it")}, waitOptions{})
	r.waitStartedPublished("turn-1")
	r.s.emit(returnedPlanEv())
	waitFor(t, "the waiter's client to read the plan", func() bool { return tp.read(`"auto":true`) })
	redial := r.gate()
	tp.holdRedials(redial)
	tp.drop()
	for range 8 {
		r.s.emit(agent.Event{Type: agent.EventThought, Text: "evicting the replay"})
	}
	redial.open()
	// The re-attach's reply read — its snapshot taken while the turn runs —
	// before the turn may end.
	waitFor(t, "the restore", func() bool { return tp.answered(protocol.MethodSessionAttach, 2) })
	g.open()
	res, f := w.result(t)
	wantReturned(t, res, f)
}

// stallLookup stalls the returned-plan lookup's attach (waitLookingBack)
// until its context ends — the first time only, or every time — and answers
// what is closed once it has stalled.
func stallLookup(t *testing.T, every bool) <-chan struct{} {
	t.Helper()
	reached := make(chan struct{})
	var mu sync.Mutex
	stalled := false
	prev := waitLookingBack
	waitLookingBack = func(ctx context.Context) {
		mu.Lock()
		first := !stalled
		stalled = true
		mu.Unlock()
		if !first && !every {
			return
		}
		if first {
			close(reached)
		}
		<-ctx.Done()
	}
	t.Cleanup(func() { waitLookingBack = prev })
	return reached
}

func waitReached(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(stubWatchdog):
		t.Fatalf("never reached %s", what)
	}
}

// TestAnInterruptedLookupIsTheInterrupts (r3, 3): plain craze wait reads a
// returned plan's cancelled lastTurn, and the lookup's attach stalls; a
// signal, or --timeout's end, then cuts it short. Interrupted is not "not
// found": the signal is plain wait's interrupt (exit 1, the turn goes on,
// with the handle), the timeout `running` (exit 5, with the handle) — never
// `cancelled`.
func TestAnInterruptedLookupIsTheInterrupts(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "a signal"
		if timeout {
			name = "--timeout"
		}
		t.Run(name, func(t *testing.T) {
			r := newWaitRig(t, 0, returnedTurn())
			r.submit("plan it")
			r.waitIdle()
			reached := stallLookup(t, true)
			sigs, fire := make(chan os.Signal, 1), make(chan time.Time)
			w := goWait(r.dial(nil), waitStart{}, waitOptions{sigs: sigs, timeout: fire})
			waitReached(t, reached, "the lookup's attach")
			if timeout {
				close(fire)
			} else {
				sigs <- syscall.SIGINT
			}
			wantInterrupted(t, timeout, w)
		})
	}
}

// wantInterrupted is the answer to an interrupt that cut the target's
// returned-plan lookup short (r3, 3): a signal is plain wait's interrupt
// (exit 1, the turn goes on, with the handle), --timeout's end `running`
// (exit 5, with the handle) — never `cancelled`, nor the plan the lookup may
// have read.
func wantInterrupted(t *testing.T, timeout bool, w *waitRun) {
	t.Helper()
	res, f := w.result(t)
	if timeout {
		res = wantResult(t, res, f, statusRunning)
		if res.Turn != "turn-1" || res.Handle == "" || statusExit(res.Status) != 5 {
			t.Fatalf("the timed-out lookup: %+v", res)
		}
		return
	}
	if res != nil || f == nil || f.msg != "interrupted; the turn goes on" || f.handle == "" || !f.follow {
		t.Fatalf("the signalled lookup: %+v, %+v", res, f)
	}
}

// pastReturnedPlan is a handle past the returned plan of turn-1, which runs
// on until the gate it answers opens, then ends cancelled — the cancel the
// host's return wrote.
func (r *waitRig) pastReturnedPlan() (*gate, turnHandle) {
	r.t.Helper()
	g := r.heldReturn()
	r.submit("plan it")
	r.waitStartedPublished("turn-1")
	r.s.emit(returnedPlanEv())
	// The handle's cursor: past the plan, the turn running.
	probe := r.dial(nil)
	it, err := probe.st.Next(stepCtx(r.t))
	if err != nil || it.Kind != remote.KindAttached || it.Reply == nil {
		r.t.Fatalf("the probe's attach: %+v, %v", it, err)
	}
	after := it.Reply.After
	return g, turnHandle{incarnation: after.Incarnation, seq: after.Seq, target: waitTarget{kind: targetTurn, id: "turn-1"}}
}

// TestAnOwnedWaitSettlesAnInterruptedLookup (r3, 3): wait --from a handle
// past the returned plan (owned, partial) reads the turn's cancelled ending,
// and a signal cuts its lookup short. As if the signal had come a step
// earlier, the cleanup settles that ending on its own context — the lookup
// made again — `completed` with the plan, and sends no cancel: the turn has
// ended.
func TestAnOwnedWaitSettlesAnInterruptedLookup(t *testing.T) {
	r := newWaitRig(t, 0)
	g, h := r.pastReturnedPlan()
	reached := stallLookup(t, false)
	conn, tp := r.dialTappedFrom(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq}, &tap{})
	sigs := make(chan os.Signal, 1)
	w := goWait(conn, waitStart{from: &h}, waitOptions{sigs: sigs})
	g.open()
	waitReached(t, reached, "the lookup's attach")
	sigs <- syscall.SIGINT
	res, f := w.result(t)
	wantReturned(t, res, f)
	if n := len(tp.lines(protocol.MethodSessionCancel)); n != 0 {
		t.Fatalf("the cleanup sent %d cancels for a turn that had ended", n)
	}
}

// interruptAtAnswer ends the waiter's work as the first returned-plan lookup
// has read the plan, before it answers (waitLookedBack): end — a signal, or
// --timeout's end — then the lookup's context's end, which the work's
// brings. A later lookup, a cleanup's own, answers.
func interruptAtAnswer(t *testing.T, end func()) <-chan struct{} {
	t.Helper()
	reached := make(chan struct{})
	var once sync.Once
	prev := waitLookedBack
	waitLookedBack = func(ctx context.Context) {
		once.Do(func() {
			close(reached)
			end()
			<-ctx.Done()
		})
	}
	t.Cleanup(func() { waitLookedBack = prev })
	return reached
}

// TestALookupInterruptedAsItAnswersIsTheInterrupts (r3, 3; the CI flake of
// TestAnInterruptedLookupIsTheInterrupts/a_signal): the interrupt comes once
// the lookup has read the returned plan, before it answers — as it does when
// the lookup's attach, made on the context the interrupt ended, is answered
// all the same (remote's Attach and Next hand over what has arrived, whatever
// their context). The work ended under the lookup, so the plan it read
// decides nothing: plain wait's answers are wantInterrupted's, and so is an
// owned wait's at --timeout's end; an owned wait's signal is its cleanup's —
// the lookup made again on the cleanup's context, `completed` with the plan,
// no cancel, and the detach made — not one that came with a result.
func TestALookupInterruptedAsItAnswersIsTheInterrupts(t *testing.T) {
	for _, c := range []struct {
		name           string
		owned, timeout bool
	}{
		{"plain, a signal", false, false},
		{"plain, --timeout", false, true},
		{"owned, a signal", true, false},
		{"owned, --timeout", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			sigs, fire := make(chan os.Signal, 1), make(chan time.Time)
			reached := interruptAtAnswer(t, func() {
				if c.timeout {
					close(fire)
				} else {
					sigs <- syscall.SIGINT
				}
			})
			opts := waitOptions{sigs: sigs, timeout: fire}
			if !c.owned {
				r := newWaitRig(t, 0, returnedTurn())
				r.submit("plan it")
				r.waitIdle()
				w := goWait(r.dial(nil), waitStart{}, opts)
				waitReached(t, reached, "the lookup's answer")
				wantInterrupted(t, c.timeout, w)
				return
			}
			r := newWaitRig(t, 0)
			g, h := r.pastReturnedPlan()
			conn, tp := r.dialTappedFrom(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq}, &tap{})
			w := goWait(conn, waitStart{from: &h}, opts)
			g.open()
			waitReached(t, reached, "the lookup's answer")
			if c.timeout {
				wantInterrupted(t, true, w)
			} else {
				res, f := w.result(t)
				wantReturned(t, res, f)
				if conn.abandon {
					t.Fatal("the signal was taken for one that came with the result: the detach was abandoned")
				}
			}
			if n := len(tp.lines(protocol.MethodSessionCancel)); n != 0 {
				t.Fatalf("the wait sent %d cancels for a turn that had ended", n)
			}
		})
	}
}
