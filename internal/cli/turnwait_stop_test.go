package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// A stop told from a crash: what a waiter says when its session ends under it,
// by what it saw of the end. The stops themselves are the engine's and the
// host's (internal/engine's last_turn_test.go, the serve tests below); here is
// the waiter's reading of them, and of a host that said nothing.

// exitedPID is a pid no process has: one that ran, exited and was reaped.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// heldAfter is a turn that emits emit, says so on the channel it returns, and
// then runs until its session closes: a turn a stop finds where the test wants.
func heldAfter(r *waitRig, emit ...agent.Event) (stubTurn, <-chan struct{}) {
	emitted := make(chan struct{})
	turn := endTurn()
	turn.before = func() {
		for _, ev := range emit {
			r.s.emit(ev)
		}
		close(emitted)
		<-r.s.closed
	}
	return turn, emitted
}

// goPrompted is a wait on a prompt of its own, once its answer is in: the
// waiter has its target, and has looked for an open ask.
func goPrompted(r *waitRig, text string) *waitRun {
	r.t.Helper()
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{submit: submitPrompt(conn, text)}, waitOptions{})
	waitFor(r.t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	return w
}

// wantStopped is a result of the session's close, cause what began it.
func wantStopped(t *testing.T, res *turnResult, f *waitFailure, cause string) *turnResult {
	t.Helper()
	res = wantResult(t, res, f, statusCancelled)
	if res.StopReason != stopStopped || res.Error == nil || res.Error.Kind != errkind.Stopped || res.Error.Retryable {
		t.Fatalf("the result: %+v, its error %+v", res, res.Error)
	}
	if statusExit(res.Status) != 3 {
		t.Fatalf("exit %d, want 3", statusExit(res.Status))
	}
	var got string
	if res.Error.Detail != nil {
		got = res.Error.Detail.Cause
	}
	if got != cause {
		t.Fatalf("its cause %q, want %q", got, cause)
	}
	return res
}

// TestAStopMidTurnIsACancelledResultWithItsUsage: whatever began it, the close's
// ending is the turn's result, with the cause and what was folded: the turn's
// last message and tool calls, the usage of each step that finished, and
// usageComplete false exactly when a step had no report.
func TestAStopMidTurnIsACancelledResultWithItsUsage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      string
		emit       []agent.Event
		wantUsage  bool
		incomplete bool
		tools      int
		message    string
	}{
		{name: "during the first step", cause: agent.StopCauseStop, incomplete: true},
		{name: "during the first step's text", cause: agent.StopCauseSignal, emit: []agent.Event{textEv("so far")}, incomplete: true, message: "so far"},
		{name: "after a step that called a tool: the next is in flight", cause: agent.StopCauseStop,
			emit: []agent.Event{toolEv("t1", "edit", "completed", "a.go"), usageEv(7, false)}, wantUsage: true, incomplete: true, tools: 1},
		{name: "during a step after a finished one", cause: agent.StopCauseStop,
			emit: []agent.Event{toolEv("t1", "edit", "completed", "a.go"), usageEv(7, false), textEv("more")}, wantUsage: true, incomplete: true, tools: 1, message: "more"},
		{name: "after a last step, none in flight", cause: agent.StopCauseIdle,
			emit: []agent.Event{textEv("done"), usageEv(7, false)}, wantUsage: true, message: "done"},
		{name: "a sub-agent's text is no step of the parent's", cause: agent.StopCauseLostOwner,
			emit: []agent.Event{textEv("done"), usageEv(7, false), {Type: agent.EventText, Agent: "child", Text: "child's"}}, wantUsage: true, message: "done"},
		{name: "a parent held in a sub-agent's call", cause: agent.StopCauseStop,
			emit: []agent.Event{usageEv(7, false), toolEv("task", "other", "in_progress")}, wantUsage: true, incomplete: true},
		{name: "exit after turn's cause", cause: agent.StopCauseExitAfterTurn, incomplete: true},
		{name: "a close that did not say", incomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitRig(t, 0)
			turn, emitted := heldAfter(r, tc.emit...)
			r.script(turn)
			w := goPrompted(r, "mine")
			<-emitted
			if err := r.eng.CloseFor(tc.cause); err != nil {
				t.Fatal(err)
			}
			res, f := w.result(t)
			res = wantStopped(t, res, f, tc.cause)
			if res.Turn != "turn-1" || res.message() != tc.message || res.toolCallCount() != tc.tools {
				t.Fatalf("what was folded: %+v", res)
			}
			if got := res.Usage != nil; got != tc.wantUsage {
				t.Fatalf("usage %s, want present %v", res.Usage, tc.wantUsage)
			}
			switch {
			case tc.incomplete && (res.UsageComplete == nil || *res.UsageComplete):
				t.Fatalf("usageComplete %v, want false", res.UsageComplete)
			case !tc.incomplete && res.UsageComplete != nil:
				t.Fatalf("usageComplete %v, want absent", *res.UsageComplete)
			}
			b, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(b), `"usageComplete":false`); got != tc.incomplete {
				t.Fatalf("the JSON: %s", b)
			}
		})
	}
}

// TestAPriorTurnsSpendIsNotThisTurns is the far side of a session that drops an
// idle report once a turn has begun (internal/agent's
// "a turn begun between the idle read and the publication"): a report from before
// the turn's started is the stream's, not the turn's, whatever the waiter's stage.
func TestAPriorTurnsSpendIsNotThisTurns(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("first"), usageEv(7, false)))
	w := goPrompted(r, "first")
	res, f := w.result(t)
	wantResult(t, res, f, statusCompleted)
	r.waitIdle()
	turn, emitted := heldAfter(r)
	r.script(turn)
	w = goPrompted(r, "second")
	<-emitted
	_ = r.eng.CloseFor(agent.StopCauseStop)
	res, f = w.result(t)
	res = wantStopped(t, res, f, agent.StopCauseStop)
	if res.Turn != "turn-2" || res.Usage != nil || res.UsageComplete == nil || *res.UsageComplete {
		t.Fatalf("turn-2 stopped in its first step: %+v", res)
	}
}

// TestAStopRacingCompletion: whichever ending the engine published first is the
// one the waiter folds, and the stream's end after it is ignored.
func TestAStopRacingCompletion(t *testing.T) {
	t.Run("the turn ends first", func(t *testing.T) {
		r := newWaitRig(t, 0, answerTurn(textEv("all done"), usageEv(7, false)))
		conn := r.dial(nil)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
		waitFor(t, "the turn's end", func() bool { return r.eng.State().LastTurn != nil })
		_ = r.eng.CloseFor(agent.StopCauseStop)
		res, f := w.result(t)
		res = wantResult(t, res, f, statusCompleted)
		if res.StopReason != stopEndTurn || res.Error != nil || res.UsageComplete != nil || res.message() != "all done" {
			t.Fatalf("the ending that came first: %+v", res)
		}
	})
	t.Run("both on the stream before the waiter reads either", func(t *testing.T) {
		// The submit's answer is held until the turn's ending and the stream's end
		// are both buffered: the ending, then the end, handed over in one replay.
		r := newWaitRig(t, 0, answerTurn(textEv("all done"), usageEv(7, false)))
		conn := r.dial(nil)
		end := make(chan struct{})
		var once sync.Once
		prev := waitBuffered
		waitBuffered = func(p pumped) {
			if p.err != nil || p.it.Kind == remote.KindEnd {
				once.Do(func() { close(end) })
			}
		}
		defer func() { waitBuffered = prev }()
		submit := submitPrompt(conn, "mine")
		w := goWait(conn, waitStart{submit: func(ctx context.Context, ident uint64) (waitTarget, error) {
			target, err := submit(ctx, ident)
			if err == nil {
				for r.eng.State().LastTurn == nil {
					time.Sleep(time.Millisecond)
				}
				_ = r.eng.CloseFor(agent.StopCauseStop)
				<-end
			}
			return target, err
		}}, waitOptions{})
		res, f := w.result(t)
		res = wantResult(t, res, f, statusCompleted)
		if res.StopReason != stopEndTurn || res.Error != nil || res.UsageComplete != nil || res.message() != "all done" {
			t.Fatalf("the ending that came first: %+v", res)
		}
	})
	t.Run("the close ends it first", func(t *testing.T) {
		// The turn's own ending would follow, had the engine not cleared it.
		r := newWaitRig(t, 0)
		turn, emitted := heldAfter(r, textEv("nearly"))
		r.script(turn)
		w := goPrompted(r, "mine")
		<-emitted
		_ = r.eng.CloseFor(agent.StopCauseSignal)
		res, f := w.result(t)
		res = wantStopped(t, res, f, agent.StopCauseSignal)
		if res.message() != "nearly" {
			t.Fatalf("what was folded: %+v", res)
		}
	})
}

// TestAWaiterFromAHandleReplaysAClosesEnding: the ending read from history, its
// cause decoded from the wire's bytes, as a wait --from meets it: the same
// result, partial for what it did not see of the turn.
func TestAWaiterFromAHandleReplaysAClosesEnding(t *testing.T) {
	h := &turnHandle{incarnation: "inc", seq: 4, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	w := newTurnWaiter(&waitConn{sessionID: waitRigID}, waitStart{from: h}, waitOptions{})
	w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindAttached, Reply: &protocol.AttachResult{
		After: protocol.Cursor{Incarnation: "inc", Seq: 4}}}})
	for _, raw := range []string{
		`{"type":"text","text":"so far"}`,
		`{"type":"meta","state":{"usage":{"contextTokens":9,"contextWindow":0,"turn":{"input":1,"output":2,"reasoning":0,"cacheRead":0,"cacheCreation":0,"costPicoUsd":0,"unpriced":false},"session":{"input":1,"output":2,"reasoning":0,"cacheRead":0,"cacheCreation":0,"costPicoUsd":0,"unpriced":false}}}}`,
		`{"type":"turn","turn":{"id":"turn-1","phase":"ended","stopReason":"closing","synthetic":true,"cause":"signal"}}`,
	} {
		w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindEvent, Body: []byte(raw)}})
	}
	res := wantStopped(t, w.result, w.fail, agent.StopCauseSignal)
	if !res.Partial || res.Usage == nil || res.message() != "so far" {
		t.Fatalf("the replay: %+v", res)
	}
}

// TestALastTurnClosesLikeTheLiveEnding: read afterwards, the same status, reason,
// error and cause: what differs is the fold, which holds whatever the waiter saw
// of the turn before it lost it, and nothing when it saw none; and a usage it
// never saw reported is not claimed whole.
func TestALastTurnClosesLikeTheLiveEnding(t *testing.T) {
	lt := protocol.LastTurn{Outcome: protocol.TurnCancelled, StopReason: stopClosing, Cause: agent.StopCauseIdle, TurnID: "turn-1"}
	t.Run("nothing folded", func(t *testing.T) {
		w := waiterAt(nil)
		w.settleOutcome(context.Background(), &lt)
		res := wantStopped(t, w.result, w.fail, agent.StopCauseIdle)
		if res.Usage != nil || res.UsageComplete == nil || *res.UsageComplete {
			t.Fatalf("no usage, and not whole: %+v", res)
		}
	})
	t.Run("a usage folded before the gap", func(t *testing.T) {
		acc := newTurnAccumulator()
		acc.fold(textEv("done"))
		acc.fold(usageEv(7, false))
		w := waiterAt(acc)
		w.partial = true
		w.settleOutcome(context.Background(), &lt)
		res := wantStopped(t, w.result, w.fail, agent.StopCauseIdle)
		if res.Usage == nil || res.UsageComplete == nil || *res.UsageComplete {
			t.Fatalf("the usage kept, and a gap is not called whole: %+v", res)
		}
	})
	t.Run("an older host says no cause", func(t *testing.T) {
		w := waiterAt(nil)
		w.settleOutcome(context.Background(), &protocol.LastTurn{Outcome: protocol.TurnCancelled, StopReason: stopClosing, TurnID: "turn-1"})
		wantStopped(t, w.result, w.fail, "")
	})
}

// TestAReconnectKeepsTheUsageFolded: a report carries the turn so far, and a
// snapshot of the same turn replaces everything else the waiter folded.
func TestAReconnectKeepsTheUsageFolded(t *testing.T) {
	acc := newTurnAccumulator()
	acc.fold(usageEv(7, false))
	w := waiterAt(acc)
	w.followFrom(&transcript.Snapshot{}, 9, true)
	if w.acc.usage == nil {
		t.Fatal("the usage folded before the reconnect is gone")
	}
	w = waiterAt(acc)
	w.followFrom(&transcript.Snapshot{}, 9, false)
	if w.acc.usage != nil {
		t.Fatal("another start took the usage")
	}
}

// TestATurnRestoredMidWayMayHaveAStepInFlight: a report that follows a restoration
// says nothing of a step the waiter never saw begin. The tool that would have said
// another step follows is in the snapshot, or behind the cursor, so the report
// reads as a last step's; the next request is out, silent, when the stop comes.
func TestATurnRestoredMidWayMayHaveAStepInFlight(t *testing.T) {
	ending := func(w *turnWaiter) {
		w.ended(context.Background(), &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, Synthetic: true, StopReason: stopClosing, Cause: agent.StopCauseStop})
	}
	body := func(t *testing.T, ev agent.Event) pumped {
		t.Helper()
		b, err := agent.EncodeEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		return pumped{it: remote.Item{Kind: remote.KindEvent, Body: []byte(b)}}
	}
	t.Run("the snapshot route", func(t *testing.T) {
		r := newWaitRig(t, 0)
		turn, emitted := heldAfter(r, toolEv("t1", "edit", "completed", "a.go"))
		release := make(chan struct{})
		inner := turn.before
		turn.before = func() {
			go func() { <-emitted; <-release; r.s.emit(usageEv(7, false)) }()
			inner()
		}
		r.script(turn)
		r.submit("mine")
		<-emitted
		// A plain wait: the snapshot holds the tool, and the stream only the
		// report after it.
		conn, tp := r.dialTapped()
		w := goWait(conn, waitStart{}, waitOptions{})
		waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
		close(release)
		waitFor(t, "the report on the stream", func() bool { return tp.read(`"usage":`) })
		_ = r.eng.CloseFor(agent.StopCauseStop)
		res, f := w.result(t)
		res = wantStopped(t, res, f, agent.StopCauseStop)
		if res.Usage == nil || res.UsageComplete == nil || *res.UsageComplete {
			t.Fatalf("restored by a snapshot: usage %s, usageComplete %v", res.Usage, res.UsageComplete)
		}
	})
	t.Run("the replay cursor route", func(t *testing.T) {
		h := &turnHandle{incarnation: "inc", seq: 4, target: waitTarget{kind: targetTurn, id: "turn-1"}}
		w := newTurnWaiter(&waitConn{sessionID: waitRigID}, waitStart{from: h}, waitOptions{})
		w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindAttached, Reply: &protocol.AttachResult{
			After: protocol.Cursor{Incarnation: "inc", Seq: 4}}}})
		w.item(context.Background(), body(t, usageEv(7, false)))
		ending(w)
		res := wantStopped(t, w.result, w.fail, agent.StopCauseStop)
		if res.Usage == nil || res.UsageComplete == nil || *res.UsageComplete {
			t.Fatalf("restored by a cursor: usage %s, usageComplete %v", res.Usage, res.UsageComplete)
		}
	})
	t.Run("a cursor just before the turn saw it whole", func(t *testing.T) {
		h := &turnHandle{incarnation: "inc", seq: 4, target: waitTarget{kind: targetTurn, id: "turn-1"}}
		w := newTurnWaiter(&waitConn{sessionID: waitRigID}, waitStart{from: h}, waitOptions{})
		w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindAttached, Reply: &protocol.AttachResult{
			After: protocol.Cursor{Incarnation: "inc", Seq: 4}}}})
		started := body(t, agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted}})
		started.it.Seq = 5
		w.item(context.Background(), started)
		w.item(context.Background(), body(t, textEv("done")))
		w.item(context.Background(), body(t, usageEv(7, false)))
		ending(w)
		res := wantStopped(t, w.result, w.fail, agent.StopCauseStop)
		if res.Partial || res.UsageComplete != nil {
			t.Fatalf("seen whole, its last step reported: %+v", res)
		}
	})
}

// TestAWithdrawnPromptRanNoStep: a turn whose prompt was never sent has no step
// to have been in flight.
func TestAWithdrawnPromptRanNoStep(t *testing.T) {
	w := waiterAt(nil)
	w.ended(context.Background(), &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: stopCancelled, Synthetic: true})
	res := wantResult(t, w.result, w.fail, statusCancelled)
	if res.UsageComplete != nil {
		t.Fatalf("usageComplete %v, want absent", *res.UsageComplete)
	}
	w = waiterAt(nil)
	w.ended(context.Background(), &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: stopCancelled})
	res = wantResult(t, w.result, w.fail, statusCancelled)
	if res.UsageComplete == nil || *res.UsageComplete {
		t.Fatalf("a cancel mid-step: usageComplete %v, want false", res.UsageComplete)
	}

	// Read afterwards, the same: a plain wait, with nothing folded.
	w = waiterAt(nil)
	w.partial = true
	w.settleOutcome(context.Background(), &protocol.LastTurn{Outcome: protocol.TurnCancelled, StopReason: stopCancelled, Withdrawn: true, TurnID: "turn-1"})
	res = wantResult(t, w.result, w.fail, statusCancelled)
	if res.UsageComplete != nil {
		t.Fatalf("withdrawn, read from lastTurn: usageComplete %v, want absent", *res.UsageComplete)
	}
	// An older host does not say, and is not called whole.
	w = waiterAt(nil)
	w.partial = true
	w.settleOutcome(context.Background(), &protocol.LastTurn{Outcome: protocol.TurnCancelled, StopReason: stopCancelled, TurnID: "turn-1"})
	res = wantResult(t, w.result, w.fail, statusCancelled)
	if res.UsageComplete == nil || *res.UsageComplete {
		t.Fatalf("a cancel read afterwards: usageComplete %v, want false", res.UsageComplete)
	}
}

// TestReadHost: a process told by its identity, never by its pid alone.
func TestReadHost(t *testing.T) {
	self, err := rundir.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("no process identity here: %v", err)
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Not reaped: a zombie, until the test's Wait below.
	zombie := cmd.Process.Pid
	waitFor(t, "the child to exit", func() bool {
		id, err := rundir.ProcessIdentity(zombie)
		return err == nil && id.Zombie
	})
	zombieIs := readHost(rundir.Entry{PID: zombie})
	_ = cmd.Wait()
	for _, tc := range []struct {
		name string
		got  hostState
		want hostState
	}{
		{"the very process", readHost(rundir.Entry{PID: os.Getpid(), PIDStart: self.Start}), hostRunning},
		{"its pid another's since", readHost(rundir.Entry{PID: os.Getpid(), PIDStart: self.Start + 1}), hostExited},
		{"no such process", readHost(rundir.Entry{PID: exitedPID(t), PIDStart: self.Start}), hostExited},
		{"a zombie", zombieIs, hostExited},
		{"a record with no start time", readHost(rundir.Entry{PID: os.Getpid()}), hostUnnamed},
		{"a record with no pid", readHost(rundir.Entry{PIDStart: self.Start}), hostUnnamed},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestALostStreamIsToldByTheHost: the stream ends with nothing said. What the
// host's process and its registry entry say decides, with what was folded beside
// a host gone, and never host_exited for a host that may be running.
func TestALostStreamIsToldByTheHost(t *testing.T) {
	self, err := rundir.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("no process identity here: %v", err)
	}
	folded := func() *turnAccumulator {
		acc := newTurnAccumulator()
		acc.fold(toolEv("t1", "edit", "completed", "a.go"))
		acc.fold(usageEv(7, false))
		return acc
	}
	t.Run("SIGKILLed: gone, its usage beside the failure", func(t *testing.T) {
		serveHome(t)
		w := waiterAt(folded())
		w.conn.entry = rundir.Entry{HostID: "0000000000a1", PID: exitedPID(t), PIDStart: self.Start}
		w.lost(context.Background(), errors.New("broken pipe"))
		f := w.fail
		if f == nil || f.kind != errkind.HostExited || f.usage == nil || !f.inFlight || f.follow || f.handle == "" {
			t.Fatalf("the failure: %+v", f)
		}
	})
	t.Run("a socket closed under a live host", func(t *testing.T) {
		r := newWaitRig(t, 0)
		turn, emitted := heldAfter(r)
		r.script(turn)
		conn, tp := r.dialTapped()
		conn.entry.PIDStart = self.Start
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
		waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
		<-emitted
		ctx, cancel := context.WithTimeout(context.Background(), serveStep)
		_ = r.srv.Close(ctx)
		cancel()
		res, f := w.result(t)
		if res != nil || f == nil || f.kind != errkind.ConnectionLost || !f.follow || f.handle == "" || f.usage != nil {
			t.Fatalf("the wait: %+v, %+v", res, f)
		}
	})
	t.Run("marked stopping, no record: a stop whose ending did not come", func(t *testing.T) {
		r := newWaitRig(t, 0)
		e := r.entry()
		if err := r.reg.MarkStopping(); err != nil {
			t.Fatal(err)
		}
		w := waiterAt(folded())
		w.conn.entry = e
		w.lost(context.Background(), errors.New("broken pipe"))
		res := wantStopped(t, w.result, w.fail, "")
		if res.Usage == nil || res.UsageComplete == nil || *res.UsageComplete || !res.Partial {
			t.Fatalf("what was folded: %+v", res)
		}
	})
	t.Run("a stop record names the turn: its result, though the host has gone", func(t *testing.T) {
		env, _ := serveHome(t)
		id := rundir.NewHostID()
		if err := rundir.WriteStopRecord(env, id, rundir.StopRecord{Cause: agent.StopCauseStop, Turn: "turn-1", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
		w := waiterAt(folded())
		w.conn.entry = rundir.Entry{HostID: id, PID: exitedPID(t)}
		w.lost(context.Background(), errors.New("broken pipe"))
		res := wantStopped(t, w.result, w.fail, agent.StopCauseStop)
		if res.Usage == nil || res.UsageComplete == nil || *res.UsageComplete || !res.Partial || statusExit(res.Status) != 3 {
			t.Fatalf("what was folded: %+v", res)
		}
	})
	t.Run("a stop record that names another turn says nothing of this one", func(t *testing.T) {
		env, _ := serveHome(t)
		for _, turn := range []string{"", "turn-2"} {
			id := rundir.NewHostID()
			if err := rundir.WriteStopRecord(env, id, rundir.StopRecord{Cause: agent.StopCauseSignal, Turn: turn, At: time.Now()}); err != nil {
				t.Fatal(err)
			}
			w := waiterAt(folded())
			w.conn.entry = rundir.Entry{HostID: id, PID: exitedPID(t)}
			w.lost(context.Background(), errors.New("broken pipe"))
			if f := w.fail; w.result != nil || f == nil || f.kind != errkind.Stopped || f.cause != agent.StopCauseSignal || f.usage == nil {
				t.Fatalf("recorded %q: %+v, %+v", turn, w.result, f)
			}
		}
	})
	t.Run("the session's end from its host, its record naming the turn", func(t *testing.T) {
		env, _ := serveHome(t)
		id := rundir.NewHostID()
		if err := rundir.WriteStopRecord(env, id, rundir.StopRecord{Cause: agent.StopCauseStop, Turn: "turn-1", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
		w := waiterAt(folded())
		w.conn.entry = rundir.Entry{HostID: id, PID: os.Getpid()}
		w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindEnd}})
		wantStopped(t, w.result, w.fail, agent.StopCauseStop)
	})
	t.Run("alive, its registry entry removed: no evidence of a stop", func(t *testing.T) {
		serveHome(t)
		w := waiterAt(nil)
		w.conn.entry = rundir.Entry{HostID: "0000000000a1", PID: os.Getpid(), PIDStart: self.Start}
		w.lost(context.Background(), errors.New("broken pipe"))
		if f := w.fail; f == nil || f.kind != errkind.ConnectionLost || !strings.Contains(f.msg, "the turn runs on in its host") {
			t.Fatalf("the failure: %+v", f)
		}
	})
	t.Run("SIGKILLed as the registry is read: the scan sweeps its entry, and its process says", func(t *testing.T) {
		serveHome(t)
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		id, err := rundir.ProcessIdentity(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		prev := hostScanned
		hostScanned = func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		defer func() { hostScanned = prev }()
		w := waiterAt(folded())
		w.conn.entry = rundir.Entry{HostID: "0000000000a1", PID: cmd.Process.Pid, PIDStart: id.Start}
		w.lost(context.Background(), errors.New("broken pipe"))
		if f := w.fail; f == nil || f.kind != errkind.HostExited {
			t.Fatalf("the failure: %+v", f)
		}
	})
	t.Run("alive, its registry entry unreadable: not a stop", func(t *testing.T) {
		r := newWaitRig(t, 0)
		e := r.entry()
		if err := os.WriteFile(entryPath(r.env, e.HostID), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.PIDStart = self.Start
		w := waiterAt(nil)
		w.conn.entry = e
		w.lost(context.Background(), errors.New("broken pipe"))
		if f := w.fail; f == nil || f.kind != errkind.ConnectionLost {
			t.Fatalf("the failure: %+v", f)
		}
	})
	t.Run("an identity that cannot be read is not a host gone", func(t *testing.T) {
		serveHome(t)
		for name, e := range map[string]rundir.Entry{
			"no pid":        {HostID: "0000000000a1"},
			"no start time": {HostID: "0000000000a1", PID: os.Getpid()},
		} {
			w := waiterAt(folded())
			w.conn.entry = e
			w.lost(context.Background(), errors.New("broken pipe"))
			f := w.fail
			if f == nil || f.kind != errkind.ConnectionLost || !strings.Contains(f.msg, "state is unknown") || strings.Contains(f.msg, "runs on") {
				t.Fatalf("%s: the failure: %+v", name, f)
			}
		}
	})
	t.Run("a close already seen: the host's word stands", func(t *testing.T) {
		serveHome(t)
		w := waiterAt(nil)
		w.target = waitTarget{kind: targetRow, id: "q-1"}
		w.closing, w.closeCause = true, agent.StopCauseSignal
		w.conn.entry = rundir.Entry{PID: exitedPID(t)}
		w.lost(context.Background(), errors.New("broken pipe"))
		if f := w.fail; f == nil || f.kind != errkind.Stopped || f.cause != agent.StopCauseSignal || f.usage != nil || f.inFlight {
			t.Fatalf("the failure: %+v", f)
		}
	})
}

// TestAQueuedRowUnderAStop: the close's ending is the running turn's, not the
// row's, which never ran. The row's wait is the session's end: exit 1, stopped,
// with the handle and the cause it saw go by, and no usage of a turn it never had.
func TestAQueuedRowUnderAStop(t *testing.T) {
	r := newWaitRig(t, 0)
	turn, emitted := heldAfter(r, usageEv(7, false))
	r.script(turn)
	r.submit("ahead")
	<-emitted
	w := goPrompted(r, "behind")
	r.waitQueued(1)
	_ = r.eng.CloseFor(agent.StopCauseStop)
	res, f := w.result(t)
	if res != nil || f == nil || f.kind != errkind.Stopped || f.cause != agent.StopCauseStop || f.usage != nil || f.inFlight || f.handle == "" {
		t.Fatalf("the row's wait: %+v, %+v", res, f)
	}
	var out bytes.Buffer
	_ = writeFailure(&out, resultOutput{what: waitWhat, json: true}, waitRigShort, f)
	var body map[string]any
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v", out.String(), err)
	}
	e, _ := body["error"].(map[string]any)
	if d, _ := e["detail"].(map[string]any); d["cause"] != agent.StopCauseStop || body["usage"] != nil || body["usageComplete"] != nil {
		t.Fatalf("the object: %s", out.String())
	}
}

// TestAHostExitedFailureCarriesItsUsage: the one object, the folded usage under
// usage beside the error, with usageComplete false for the step it cut; a usage
// not folded is no key at all.
func TestAHostExitedFailureCarriesItsUsage(t *testing.T) {
	var out bytes.Buffer
	u := &agent.UsageState{ContextTokens: 9, Turn: agent.Spend{Input: 1}}
	_ = writeFailure(&out, resultOutput{what: waitWhat, json: true}, waitRigShort, &waitFailure{msg: "gone", kind: errkind.HostExited, handle: "h", usage: u, inFlight: true})
	var body struct {
		Error         resultError     `json:"error"`
		Handle        string          `json:"handle"`
		Usage         json.RawMessage `json:"usage"`
		UsageComplete *bool           `json:"usageComplete"`
	}
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v", out.String(), err)
	}
	if body.Error.Kind != errkind.HostExited || !body.Error.Retryable || body.Handle != "h" || body.Usage == nil || body.UsageComplete == nil || *body.UsageComplete {
		t.Fatalf("the object: %s", out.String())
	}
	out.Reset()
	_ = writeFailure(&out, resultOutput{what: waitWhat, json: true}, waitRigShort, &waitFailure{msg: "lost", kind: errkind.ConnectionLost})
	if s := out.String(); strings.Contains(s, "usage") || strings.Contains(s, "detail") {
		t.Fatalf("the object: %s", s)
	}
}

// TestTheFooterSaysAUsageLeftAStepOut: only where it has a usage to qualify.
func TestTheFooterSaysAUsageLeftAStepOut(t *testing.T) {
	no := false
	res := &turnResult{Status: statusCancelled, UsageComplete: &no}
	if strings.Contains(res.footer(), "in flight") {
		t.Fatalf("no usage, yet: %s", res.footer())
	}
	res.usage = &agent.UsageState{}
	if !strings.Contains(res.footer(), "usage: a step was in flight") {
		t.Fatalf("the footer: %s", res.footer())
	}
	res.UsageComplete = nil
	if strings.Contains(res.footer(), "in flight") {
		t.Fatalf("whole, yet: %s", res.footer())
	}
}

// TestTheFirstStopRequestsCauseStands: a signal during a stop, or anything else
// that asks again, joins it and says nothing new.
func TestTheFirstStopRequestsCauseStands(t *testing.T) {
	l := newHostLifecycle()
	l.request(agent.StopCauseStop, "session.stop from client c-1")
	l.request(agent.StopCauseSignal, "SIGTERM")
	if cause, why := l.cause(); cause != agent.StopCauseStop || why != "session.stop from client c-1" {
		t.Fatalf("the stop's cause: %q, %q", cause, why)
	}
}

// TestTheHostsStopsSayTheirCauses is a real craze serve over the fake agent, a
// turn it never finishes, and a waiter on it: each way a stop reaches the host
// gives the waiter its own cause, and the journal the same.
func TestTheHostsStopsSayTheirCauses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		how   func(t *testing.T, r *serveRun, e rundir.Entry)
		cause string
	}{
		{"session.stop", sendStop, agent.StopCauseStop},
		{"session.stop, cause exit_after_turn", func(t *testing.T, _ *serveRun, e rundir.Entry) {
			if err := askStop(stepCtx(t), e, "test", agent.StopCauseExitAfterTurn); err != nil {
				t.Fatal(err)
			}
		}, agent.StopCauseExitAfterTurn},
		{"SIGTERM", func(_ *testing.T, r *serveRun, _ rundir.Entry) { r.sigs <- syscall.SIGTERM }, agent.StopCauseSignal},
		{"SIGINT", func(_ *testing.T, r *serveRun, _ rundir.Entry) { r.sigs <- syscall.SIGINT }, agent.StopCauseSignal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_JOURNAL", "1")
			t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
			e := r.waitServing(t, env, true)
			conn, err := dialWait(stepCtx(t), e, e.CrazeSessionID, waitWhat, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.close(nil)
			w := goWait(conn, waitStart{submit: submitPrompt(conn, "hold on")}, waitOptions{})
			waitFor(t, "the turn to run", func() bool { return serveTurnRunning(t, e) })
			tc.how(t, r, e)
			res, f := w.result(t)
			wantStopped(t, res, f, tc.cause)
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve: %v", err)
			}
			files, err := filepath.Glob(filepath.Join(os.Getenv("CRAZE_HOME"), "journal", "*", "*.jsonl"))
			if err != nil || len(files) != 1 {
				t.Fatalf("the host's journal: %v, %v", files, err)
			}
			b, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), `"kind":"host_stop"`) || !strings.Contains(string(b), `"stopCause":"`+tc.cause+`"`) {
				t.Fatalf("the journal's host_stop: %s", b)
			}
		})
	}
}

// TestAStopWhoseEndingNeverArrivedIsStillItsResult is a real craze serve, and a
// waiter whose connection is lost as the close's ending reaches it: the flush that
// gave up, the subscription dropped for overflow. The attach fence is up, so no
// re-attach recovers it; the host's stop record says what happened.
func TestAStopWhoseEndingNeverArrivedIsStillItsResult(t *testing.T) {
	for _, tc := range []struct {
		name  string
		how   func(t *testing.T, r *serveRun, e rundir.Entry)
		cause string
	}{
		{"session.stop", sendStop, agent.StopCauseStop},
		{"SIGTERM", func(_ *testing.T, r *serveRun, _ rundir.Entry) { r.sigs <- syscall.SIGTERM }, agent.StopCauseSignal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
			e := r.waitServing(t, env, true)
			tp := &tap{}
			lost := tp.loseReply(`"stopReason":"closing"`)
			c, err := remote.Dial(stepCtx(t), e.Socket, remote.Options{Dial: tp.dial})
			if err != nil {
				t.Fatal(err)
			}
			st, err := c.Attach(stepCtx(t), remote.AttachOptions{SessionID: e.CrazeSessionID, When: protocol.WhenReady})
			if err != nil {
				t.Fatal(err)
			}
			conn := &waitConn{c: c, st: st, entry: e, sessionID: e.CrazeSessionID}
			defer conn.close(nil)
			w := goWait(conn, waitStart{submit: submitPrompt(conn, "hold on")}, waitOptions{})
			waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
			tc.how(t, r, e)
			select {
			case <-lost:
			case <-time.After(serveStep):
				t.Fatal("the close's ending never reached the transport")
			}
			res, f := w.result(t)
			res = wantStopped(t, res, f, tc.cause)
			if res.Turn != "turn-1" || !res.Partial || res.UsageComplete == nil || *res.UsageComplete {
				t.Fatalf("the result: %+v", res)
			}
			if tp.read(`"stopReason":"closing"`) {
				t.Fatal("the premise: the ending reached the client")
			}
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve: %v", err)
			}
		})
	}
}

// TestAnAcknowledgedStopSaysStop: a session.stop is acknowledged before the
// coordinator hears of it, and a signal comes in that window. The receipt said
// stop, and so do the ending, the record and the journal: the signal joins it.
func TestAnAcknowledgedStopSaysStop(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_JOURNAL", "1")
	t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
	reached, release := make(chan struct{}), make(chan struct{})
	prev := serveStopHanded
	serveStopHanded = func() {
		close(reached)
		<-release
	}
	defer func() { serveStopHanded = prev }()
	let := sync.OnceFunc(func() { close(release) })
	defer let()
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	conn, err := dialWait(stepCtx(t), e, e.CrazeSessionID, waitWhat, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.close(nil)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "hold on")}, waitOptions{})
	waitFor(t, "the turn to run", func() bool { return serveTurnRunning(t, e) })
	sendStop(t, r, e)
	select {
	case <-reached:
	case <-time.After(serveStep):
		t.Fatal("the stop never reached its hand-over")
	}
	r.sigs <- syscall.SIGTERM
	res, f := w.result(t)
	wantStopped(t, res, f, agent.StopCauseStop)
	let()
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	if rec, ok := rundir.ReadStopRecord(env, e.HostID); !ok || rec.Cause != agent.StopCauseStop || rec.Turn != "turn-1" {
		t.Fatalf("the record: %+v, %v", rec, ok)
	}
	files, err := filepath.Glob(filepath.Join(os.Getenv("CRAZE_HOME"), "journal", "*", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the host's journal: %v, %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"stopCause":"stop"`) {
		t.Fatalf("the journal's host_stop: %s", b)
	}
}

// sendStop is a session.stop from a client of its own, its receipt read.
func sendStop(t *testing.T, _ *serveRun, e rundir.Entry) {
	t.Helper()
	c, err := dialCLI(stepCtx(t), e, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Command(stepCtx(t), protocol.MethodSessionStop, protocol.StopParams{SessionID: e.CrazeSessionID}, nil, remote.CommandOptions{}); err != nil {
		t.Fatalf("session.stop: %v", err)
	}
}

// serveTurnRunning says e's session has a turn running.
func serveTurnRunning(t *testing.T, e rundir.Entry) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := dialCLI(ctx, e, "test")
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	var st protocol.StateResult
	if err := c.Call(ctx, protocol.MethodSessionState, protocol.StateParams{SessionID: e.CrazeSessionID}, &st); err != nil {
		return false
	}
	return st.Turn != ""
}

// TestStopCausesAreDocumented keeps the reference pages and the event schema
// naming every cause a host can send.
func TestStopCausesAreDocumented(t *testing.T) {
	root := docsModuleRoot(t)
	for _, path := range []string{
		filepath.Join("docs", "reference", "cli.md"),
		filepath.Join("docs", "reference", "protocol.md"),
		filepath.Join("docs", "reference", "protocol", "schema", "event.json"),
		filepath.Join("docs", "reference", "protocol", "schema", "info.json"),
	} {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		for _, cause := range agent.StopCauses() {
			if !strings.Contains(string(b), cause) {
				t.Errorf("%s does not name the stop cause %q", path, cause)
			}
		}
	}
}
