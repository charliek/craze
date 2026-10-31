package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
)

// exitJob is a promptJob with --exit-after-turn over conn, its output its own, as
// the command's run builds one.
func exitJob(t *testing.T, sigs <-chan os.Signal) (*promptJob, *bytes.Buffer, *syncBuffer) {
	t.Helper()
	var out bytes.Buffer
	errs := &syncBuffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(errs)
	r := newPromptJob(cmd, sendWhat, "go", true, time.Now(), 0, true, 0, sigs)
	r.exitAfterTurn = true
	t.Cleanup(r.close)
	return r, &out, errs
}

// syncBuffer is a stderr the submit's goroutine may write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// withStopBarrier has stopAfterTurn call hook.
func withStopBarrier(t *testing.T, hook func()) {
	t.Helper()
	prev := stopAfterTurn
	stopAfterTurn = hook
	t.Cleanup(func() { stopAfterTurn = prev })
}

// silentSocketPath is a path for a socket a test binds.
func silentSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "czx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// silentSocket is a socket that takes a connection and never says anything: a host
// whose stop hangs.
func silentSocket(t *testing.T) string {
	t.Helper()
	path := silentSocketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return path
}

// exitRig is a rig whose server serves the fence and session.stop, stops holding what
// it was told.
type exitRig struct {
	*waitRig
	stops chan control.StopRequest
}

func newExitRig(t *testing.T, turns ...stubTurn) *exitRig {
	t.Helper()
	stops := make(chan control.StopRequest, 8)
	opts := control.Options{Workspace: "/w", ExitAfterTurn: true, Stop: func(r control.StopRequest) { stops <- r }}
	return &exitRig{newWaitRigOn(t, opts, 0, turns...), stops}
}

// wantStop is the stop the rig heard, within the watchdog.
func (r *exitRig) wantStop(t *testing.T) {
	t.Helper()
	select {
	case got := <-r.stops:
		if got.Cause != agent.StopCauseExitAfterTurn {
			t.Fatalf("the stop said %+v", got)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("no stop was sent")
	}
}

// liveProcess is a real process for a test to name as its host, and whether it has
// gone: a test's own host is the test, which no kill may reach.
func liveProcess(t *testing.T) (pid int, start uint64, gone func() bool) {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	id, err := rundir.ProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, id.Start, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

// withExitBounds shortens what --exit-after-turn waits for.
func withExitBounds(t *testing.T, send, gone time.Duration) {
	t.Helper()
	prevSend, prevGone := exitStopSendWait, exitAfterTurnWait
	exitStopSendWait, exitAfterTurnWait = send, gone
	t.Cleanup(func() { exitStopSendWait, exitAfterTurnWait = prevSend, prevGone })
}

// TestTheResultIsOutBeforeTheStop: the caller has the whole result on stdout by the
// time the stop is sent, whatever then comes of it, and the stop says its cause. A
// host that cannot be told from another process is not killed, and stderr says it may
// still be running; the result's exit stands.
func TestTheResultIsOutBeforeTheStop(t *testing.T) {
	withExitBounds(t, stubWatchdog, 50*time.Millisecond)
	r := newExitRig(t, endTurn())
	conn, tp := r.dialTapped()
	job, out, errs := exitJob(t, make(chan os.Signal, 1))
	sawResult := false
	withStopBarrier(t, func() {
		var res map[string]any
		sawResult = json.Unmarshal(out.Bytes(), &res) == nil && res["status"] == "completed" && res["stopping"] == true
	})
	if err := job.waitFor(conn); err != nil {
		t.Fatalf("a completed turn: %v", err)
	}
	if !sawResult {
		t.Fatal("the stop was reached with the result not written")
	}
	r.wantStop(t)
	lines := tp.lines(protocol.MethodSessionPrompt)
	if len(lines) != 1 || !strings.Contains(lines[0], `"exitAfterTurn":true`) {
		t.Fatalf("the prompt: %q", lines)
	}
	if s := errs.String(); !strings.Contains(s, "may still be running") || !strings.Contains(s, "cannot be told") {
		t.Fatalf("stderr: %q", s)
	}
}

// TestAHostThatCannotEndWithItsTurnIsRefused: no capability (an older host), or one
// that cannot be stopped: exit 1, kind config, and the prompt never sent.
func TestAHostThatCannotEndWithItsTurnIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts control.Options
	}{
		{"an older host", control.Options{Workspace: "/w", Stop: func(control.StopRequest) {}}},
		{"a host that cannot be stopped", control.Options{Workspace: "/w", ExitAfterTurn: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitRigOn(t, tc.opts, 0, endTurn())
			conn, tp := r.dialTapped()
			job, out, _ := exitJob(t, make(chan os.Signal, 1))
			err := job.waitFor(conn)
			if failureKind(err) != errkind.Config || !strings.Contains(err.Error(), "the prompt was not sent") {
				t.Fatalf("the failure: %v", err)
			}
			if !strings.Contains(out.String(), `"kind":"config"`) {
				t.Fatalf("stdout: %q", out)
			}
			if lines := tp.lines(protocol.MethodSessionPrompt); len(lines) != 0 {
				t.Fatalf("the prompt was sent: %q", lines)
			}
		})
	}
}

// TestAHostThatWillNotGoIsKilled: the stop refused, the socket silent, or the host
// still there at the bound, each by a real process: it is killed, stderr says so, and
// the result's exit stands.
func TestAHostThatWillNotGoIsKilled(t *testing.T) {
	refusing := func(t *testing.T, r *exitRig) string {
		_, eng := r.newEngine(0)
		srv := control.New(control.Options{Workspace: "/w"})
		srv.SetEngine(eng)
		path := silentSocketPath(t)
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), serveStep)
			defer cancel()
			_ = srv.Close(ctx)
		})
		return path
	}
	for _, tc := range []struct {
		name   string
		socket func(*testing.T, *exitRig) string
		want   string
	}{
		{"its socket silent", func(t *testing.T, _ *exitRig) string { return silentSocket(t) }, "its socket did not answer"},
		{"its stop refused", refusing, "was not stopped"},
		{"still there at the bound", nil, "was still there"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withExitBounds(t, 300*time.Millisecond, 100*time.Millisecond)
			r := newExitRig(t, stubTurn{err: errors.New("boom")})
			conn := r.dial(nil)
			pid, start, gone := liveProcess(t)
			conn.entry.PID, conn.entry.PIDStart = pid, start
			withStopBarrier(t, func() {
				if tc.socket != nil {
					conn.entry.Socket = tc.socket(t, r)
				}
			})
			job, out, errs := exitJob(t, make(chan os.Signal, 1))
			err := job.waitFor(conn)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 3 || !strings.Contains(out.String(), `"status":"failed"`) {
				t.Fatalf("the exit %v, stdout %q", err, out)
			}
			waitFor(t, "the host to be killed", gone)
			if s := errs.String(); !strings.Contains(s, tc.want) || !strings.Contains(s, "its host was killed") {
				t.Fatalf("stderr: %q", s)
			}
		})
	}
}

// TestAHostThatCannotBeKilledIsSaidToBeRunning: a process whose identity cannot be
// read, and one that is not this user's: nothing signalled that should not be, and
// the only line a caller can act on.
func TestAHostThatCannotBeKilledIsSaidToBeRunning(t *testing.T) {
	t.Run("no identity", func(t *testing.T) {
		withExitBounds(t, 50*time.Millisecond, 50*time.Millisecond)
		r := newExitRig(t, endTurn())
		conn := r.dial(nil)
		pid, _, gone := liveProcess(t)
		conn.entry.PID, conn.entry.PIDStart, conn.entry.Socket = pid, 0, silentSocket(t)
		job, _, errs := exitJob(t, make(chan os.Signal, 1))
		if err := job.waitFor(conn); err != nil {
			t.Fatalf("the exit: %v", err)
		}
		if gone() || !strings.Contains(errs.String(), "may still be running") {
			t.Fatalf("gone %v, stderr %q", gone(), errs)
		}
	})
	t.Run("another user's", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root may signal anything")
		}
		id, err := rundir.ProcessIdentity(1)
		if err != nil {
			t.Skip("pid 1's identity cannot be read here")
		}
		withExitBounds(t, 50*time.Millisecond, 50*time.Millisecond)
		r := newExitRig(t, endTurn())
		conn := r.dial(nil)
		conn.entry.PID, conn.entry.PIDStart, conn.entry.Socket = 1, id.Start, silentSocket(t)
		job, _, errs := exitJob(t, make(chan os.Signal, 1))
		if err := job.waitFor(conn); err != nil {
			t.Fatalf("the exit: %v", err)
		}
		if s := errs.String(); !strings.Contains(s, "may still be running") || !strings.Contains(s, "not permitted") {
			t.Fatalf("stderr: %q", s)
		}
	})
}

// TestASignalBeforeTheStopStillSendsIt: a signal arriving before the stop is sent skips
// the wait for the host to go, and nothing else: the stop arrives, the host is not
// killed, and the command returns well inside the wait it skipped.
func TestASignalBeforeTheStopStillSendsIt(t *testing.T) {
	r := newExitRig(t, endTurn())
	conn := r.dial(nil)
	pid, start, gone := liveProcess(t)
	conn.entry.PID, conn.entry.PIDStart = pid, start
	sigs := make(chan os.Signal, 1)
	job, _, errs := exitJob(t, sigs)
	// Relayed, and so there to be read, before anything of the stop has happened.
	withStopBarrier(t, func() {
		sigs <- syscall.SIGTERM
		waitFor(t, "the signal to be relayed", func() bool { return len(job.relayed) == 1 })
	})
	done := make(chan error, 1)
	go func() { done <- job.waitFor(conn) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the exit: %v", err)
		}
	case <-time.After(exitAfterTurnWait / 2):
		t.Fatal("the signal did not end the wait for the host")
	}
	r.wantStop(t)
	if gone() {
		t.Fatal("a signal killed the host")
	}
	if s := errs.String(); !strings.Contains(s, "stop sent") {
		t.Fatalf("stderr: %q", s)
	}
}

// blockedWriter is a stdout nobody reads.
type blockedWriter struct{ entered, release chan struct{} }

func (w *blockedWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}

// TestAStdoutNobodyReadsPinsNothing: the attachment is dropped before the write, so a
// write that blocks holds nothing up on the host. A signal gives up on it, goes on to
// the stop, and exits with the result's code.
func TestAStdoutNobodyReadsPinsNothing(t *testing.T) {
	withExitBounds(t, stubWatchdog, 50*time.Millisecond)
	r := newExitRig(t, stubTurn{err: errors.New("boom")})
	conn := r.dial(nil)
	r.waitAttached(1)
	sigs := make(chan os.Signal, 1)
	job, _, errs := exitJob(t, sigs)
	w := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(w.release)
	job.cmd.SetOut(w)
	done := make(chan error, 1)
	go func() { done <- job.waitFor(conn) }()
	select {
	case <-w.entered:
	case <-time.After(stubWatchdog):
		t.Fatal("the result was never written")
	}
	waitFor(t, "the attachment to go with the write blocked", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.attached == 0
	})
	select {
	case err := <-done:
		t.Fatalf("returned with its write blocked and no signal: %v", err)
	case <-r.stops:
		t.Fatal("the stop went with the result unwritten and no signal")
	case <-time.After(50 * time.Millisecond):
	}
	sigs <- syscall.SIGTERM
	var err error
	select {
	case err = <-done:
	case <-time.After(stubWatchdog):
		t.Fatal("a signal did not end the blocked write")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("the exit: %v", err)
	}
	r.wantStop(t)
	if !strings.Contains(errs.String(), "not written whole") {
		t.Fatalf("stderr: %q", errs)
	}
}

// TestARemovedFencedRowLeavesTheSessionRunning: another client takes the waited row
// away and goes on using the session. The waiter reads dropped, exit 3, with no
// stopping, sends no stop, and says so; what that client started is not cancelled.
func TestARemovedFencedRowLeavesTheSessionRunning(t *testing.T) {
	r := newExitRig(t)
	first, after := r.gate(), r.gate()
	r.script(heldTurn(first, textEv("first")), heldTurn(after, textEv("theirs")))
	defer first.open()
	defer after.open()
	r.submit("running")
	r.waitStarted("turn-1")
	conn := r.dial(nil)
	job, out, errs := exitJob(t, make(chan os.Signal, 1))
	done := make(chan error, 1)
	go func() { done <- job.waitFor(conn) }()
	var row string
	waitFor(t, "the fenced row", func() bool {
		st := r.eng.State()
		if len(st.Queue) == 1 && st.ExitFenced {
			row = st.Queue[0].ID
			return true
		}
		return false
	})
	if _, err := r.eng.Unqueue(engine.Command{}, row); err != nil {
		t.Fatal(err)
	}
	if res, err := r.eng.Submit(engine.Command{}, "theirs", engine.SubmitQueue, ""); err != nil || res.Queued == nil {
		t.Fatalf("a prompt after the removal: %+v, %v", res, err)
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(stubWatchdog):
		t.Fatal("the waiter never read its row's removal")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("the exit: %v", err)
	}
	if s := out.String(); !strings.Contains(s, `"status":"dropped"`) || strings.Contains(s, "stopping") {
		t.Fatalf("stdout: %q", s)
	}
	if !strings.Contains(errs.String(), "was left running") {
		t.Fatalf("stderr: %q", errs)
	}
	select {
	case got := <-r.stops:
		t.Fatalf("a stop was sent: %+v", got)
	default:
	}
	first.open()
	r.waitStarted("turn-2")
	if r.eng.State().Turn != "turn-2" {
		t.Fatalf("the other client's turn: %+v", r.eng.State())
	}
}

// planAnswered is what the approval policy leaves when it answers a plan's ask.
func planAnswered() agent.Event {
	return agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: "plan-1", Kind: agent.AskPlan, Outcome: agent.AskAutomatic, By: agent.AskByPolicy}}
}

// TestAReadBackRefusedByAStopIsNotAPlanNotFound: a waiter that joined late, its lookup
// held at its attach while the stop's fence goes up. The snapshot named the plan's ask
// the policy answered in this turn: completed, planEnded, exit 0, the plan's text gone
// with its event. A snapshot that names none stays what can be proven: cancelled.
func TestAReadBackRefusedByAStopIsNotAPlanNotFound(t *testing.T) {
	plan := func(kind agent.AskKind, outcome agent.AskOutcome) agent.Event {
		ev := planAnswered()
		ev.Ask.Kind, ev.Ask.Outcome = kind, outcome
		return ev
	}
	for _, tc := range []struct {
		name   string
		emit   []agent.Event
		ago    time.Duration
		status string
		ended  bool
	}{
		{name: "a returned plan", emit: []agent.Event{returnedPlanEv(), planAnswered()}, status: statusCompleted, ended: true},
		{name: "no plan", status: statusCancelled},
		{name: "a plan a person answered", emit: []agent.Event{plan(agent.AskPlan, agent.AskAnswered)}, status: statusCancelled},
		{name: "a permission the policy answered", emit: []agent.Event{plan(agent.AskPermission, agent.AskAutomatic)}, status: statusCancelled},
		{name: "a plan of an earlier turn", emit: []agent.Event{planAnswered()}, ago: time.Hour, status: statusCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitRig(t, 0)
			g := r.heldReturn()
			r.submit("plan it")
			r.waitStartedPublished("turn-1")
			for _, ev := range tc.emit {
				ev.At = time.Now().Add(-tc.ago)
				r.s.emit(ev)
			}
			r.waitPublished(tc.emit)
			prev := waitLookingBack
			waitLookingBack = func(context.Context) { _, _ = r.srv.FenceAttaches() }
			t.Cleanup(func() { waitLookingBack = prev })
			conn := r.dial(nil)
			w := goWait(conn, waitStart{}, waitOptions{})
			r.waitAttached(1)
			g.open()
			res, f := w.result(t)
			res = wantResult(t, res, f, tc.status)
			if res.PlanEnded != tc.ended || res.Plan != "" || !res.Partial {
				t.Fatalf("the result: %+v", res)
			}
		})
	}
	if !inProcessProvider("native") || inProcessProvider("grok") || inProcessProvider("no such") {
		t.Fatal("which provider's automatic plan answer accepts")
	}
}

// TestNoStopIsSentWithoutAResult: running at --timeout goes on; a failure leaves the
// session alone. Only a result that ends the wait reaches the stop.
func TestNoStopIsSentWithoutAResult(t *testing.T) {
	for status, want := range map[string]bool{statusCompleted: true, statusFailed: true, statusCancelled: true, statusDropped: false,
		statusNeedsInput: true, statusRunning: false, statusIdle: false} {
		if got := endsWithResult(status); got != want {
			t.Errorf("%s: ends with its result %v, want %v", status, got, want)
		}
	}
	r := newWaitRig(t, 0, endTurn())
	conn := r.dial(nil)
	job, _, _ := exitJob(t, make(chan os.Signal, 1))
	withStopBarrier(t, func() { t.Error("a failed wait reached the stop") })
	err := job.waitAfter(&submitGate{sent: true, answered: true}, conn, nil, &waitFailure{msg: "lost", kind: errkind.ConnectionLost})
	if failureKind(err) != errkind.ConnectionLost {
		t.Fatalf("the failure: %v", err)
	}
}

// TestAStoppingResultSaysSo: stopping on the object, the plain footer's line, and an
// open ask's handle and next: gone with what they promised.
func TestAStoppingResultSaysSo(t *testing.T) {
	res := newResult(resultSession{id: "0199c0de-1111-7000-8000-00000000a001"}, statusNeedsInput)
	res.Handle = "h"
	res.Ask = &resultAsk{ID: "a-1", Kind: "permission", Summary: "bash"}
	if !strings.Contains(res.footer(), "next: answer it") {
		t.Fatalf("before: %s", res.footer())
	}
	res.markStopping()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"stopping":true`) || strings.Contains(string(b), `"handle"`) {
		t.Fatalf("the object: %s", b)
	}
	foot := res.footer()
	if !strings.Contains(foot, "the ask is abandoned") || strings.Contains(foot, "next:") {
		t.Fatalf("the footer: %s", foot)
	}
	done := newResult(resultSession{id: "0199c0de-1111-7000-8000-00000000a001"}, statusCompleted)
	done.markStopping()
	if foot := done.footer(); !strings.Contains(foot, "session stopping") || strings.Contains(foot, "abandoned") {
		t.Fatalf("a finished turn's footer: %s", foot)
	}
}

// TestExitAfterTurnIsTheFirstCause: a signal at the moment the stop is acknowledged
// leaves exit_after_turn on a waiter's result, the record and the journal; a signal
// first leaves signal, the later stop answered all the same.
func TestExitAfterTurnIsTheFirstCause(t *testing.T) {
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
	if err := askStop(stepCtx(t), e, "test", agent.StopCauseExitAfterTurn); err != nil {
		t.Fatalf("session.stop: %v", err)
	}
	select {
	case <-reached:
	case <-time.After(serveStep):
		t.Fatal("the stop never reached its hand-over")
	}
	r.sigs <- syscall.SIGTERM
	res, f := w.result(t)
	wantStopped(t, res, f, agent.StopCauseExitAfterTurn)
	let()
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	if rec, ok := rundir.ReadStopRecord(env, e.HostID); !ok || rec.Cause != agent.StopCauseExitAfterTurn || rec.Turn != "turn-1" {
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
	if !strings.Contains(string(b), `"stopCause":"exit_after_turn"`) {
		t.Fatalf("the journal: %s", b)
	}
}

// TestASignalFirstKeepsItsCause is the other order, at the lifecycle: whichever
// began the stop says, and session.stop's cause is what the request carries.
func TestASignalFirstKeepsItsCause(t *testing.T) {
	l := newHostLifecycle()
	l.request(agent.StopCauseSignal, "SIGTERM")
	l.stopBeginning(controlStop(agent.StopCauseExitAfterTurn))
	if cause, _ := l.cause(); cause != agent.StopCauseSignal {
		t.Fatalf("the cause: %q", cause)
	}
	l = newHostLifecycle()
	l.stopBeginning(controlStop(agent.StopCauseExitAfterTurn))
	l.request(agent.StopCauseSignal, "SIGTERM")
	if cause, _ := l.cause(); cause != agent.StopCauseExitAfterTurn {
		t.Fatalf("the cause: %q", cause)
	}
	l = newHostLifecycle()
	l.stopBeginning(controlStop(""))
	if cause, _ := l.cause(); cause != agent.StopCauseStop {
		t.Fatalf("a stop that says nothing: %q", cause)
	}
}

// controlStop is a session.stop as the server hands it over, saying cause.
func controlStop(cause string) control.StopRequest {
	return control.StopRequest{Client: "c-1", Cause: cause}
}

// TestARealHostsStopSaysExitAfterTurn: over a real craze serve, its capability passes
// the check, and the waiter's own stop says its cause. This host is the test process,
// which does not exit and is never signalled, so the wait for it runs to a short bound.
func TestARealHostsStopSaysExitAfterTurn(t *testing.T) {
	prev := exitAfterTurnWait
	exitAfterTurnWait = 300 * time.Millisecond
	t.Cleanup(func() { exitAfterTurnWait = prev })
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	conn, err := dialWait(stepCtx(t), e, e.CrazeSessionID, waitWhat, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, out, errs := exitJob(t, make(chan os.Signal, 1))
	if err := job.waitFor(conn); err != nil {
		t.Fatalf("the waiter: %v, stderr %q", err, errs)
	}
	if !strings.Contains(out.String(), `"stopping":true`) {
		t.Fatalf("stdout %q, stderr %q", out, errs)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	if rec, ok := rundir.ReadStopRecord(env, e.HostID); !ok || rec.Cause != agent.StopCauseExitAfterTurn {
		t.Fatalf("the record: %+v, %v", rec, ok)
	}
}

// TestAFenceRefusalReadsAsStopped: the host's refusal, over the wire, is kind stopped
// and a prompt refused, which is what a second craze send reports.
func TestAFenceRefusalReadsAsStopped(t *testing.T) {
	err := &remote.Error{Code: protocol.CodeNotAccepting, Reason: protocol.ReasonExitFenced, Message: engine.ErrExitFenced.Error()}
	if refusalKind(err) != errkind.Stopped || !refusedByHost(err) || !errors.Is(err, engine.ErrExitFenced) {
		t.Fatalf("kind %s, refused %v", refusalKind(err), refusedByHost(err))
	}
	if reloadable(err) {
		t.Fatal("a fence's refusal reads as a session to load again, with its host still up")
	}
}

// TestAStopRacingTheHostsExit: the host gone between the lookup and the dial, or
// between the dial and the command, is the session ended, exit 0, for stop and cancel
// with --json, and never a failure. Gone before anything was sent is already_ended;
// under a stop that was sent, stopped.
func TestAStopRacingTheHostsExit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		barrier *func(rundir.Entry)
		verb    string
		want    string
	}{
		{"stop, before the dial", &controlDialling, "stop", resultAlreadyEnded},
		{"cancel, before the dial", &controlDialling, "cancel", resultAlreadyEnded},
		{"stop, before the command", &controlDialled, "stop", resultStopped},
		{"cancel, before the command", &controlDialled, "cancel", resultAlreadyEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, _ := unattendedHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			stdout, stderr, code := executeErr([]string{"new", "--wait", "--json", "--provider", "cursor", "-C", ws, "go"})
			if code != 0 {
				t.Fatalf("craze new --wait: exit %d, %s, %s", code, stdout, stderr)
			}
			short := decodeResult(t, stdout)["short"].(string)
			entries, err := rundir.Hosts(env)
			if err != nil || len(entries) != 1 {
				t.Fatalf("the host: %+v, %v", entries, err)
			}
			prev := *tc.barrier
			t.Cleanup(func() { *tc.barrier = prev })
			*tc.barrier = func(rundir.Entry) {
				if err := syscall.Kill(entries[0].PID, syscall.SIGKILL); err != nil {
					t.Error(err)
				}
				waitReaped(t, entries[0].PID)
			}
			stdout, stderr, code = executeErr([]string{tc.verb, short, "--json"})
			if m := decodeResult(t, stdout); code != 0 || m["result"] != tc.want || stderr != "" {
				t.Fatalf("exit %d, %v, %q", code, m, stderr)
			}
		})
	}
}

// TestAHostMarkedStoppingHasGone: its process still there and its entry too, a host
// that has begun to stop reads as going, for a command it did not answer.
func TestAHostMarkedStoppingHasGone(t *testing.T) {
	r := newWaitRig(t, 0)
	if hostWent(r.entry(), waitRigID) {
		t.Fatal("a running host reads as gone")
	}
	if err := r.reg.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	if !hostWent(r.entry(), waitRigID) {
		t.Fatal("a host marked stopping does not read as going")
	}
}

// TestASignalBeforeThePromptTakesTheFileWithIt: a session the command stopped, its
// prompt never sent, leaves no index row: the file it wrote goes with it, so a caller
// finds no session that never existed.
func TestASignalBeforeThePromptTakesTheFileWithIt(t *testing.T) {
	_, ws, _ := unattendedHome(t)
	file := filepath.Join(t.TempDir(), "session.json")
	prompts := filepath.Join(t.TempDir(), "prompts")
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv("CRAZE_FAKE_DUMP_PROMPTS", prompts)
	sigs := withWaitSignals(t)
	var host rundir.Entry
	prev, prevKnown := promptSpawned, newWaitHostKnown
	promptSpawned = func(ref hostRef) { host = ref.entry }
	wrote := false
	// The signal relayed, and so the command's work ended, before anything is dialled:
	// the prompt cannot be sent.
	newWaitHostKnown = func(r *promptJob) {
		_, err := os.Stat(file)
		wrote = err == nil
		sigs <- syscall.SIGINT
		waitFor(t, "the signal to be relayed", func() bool { return len(r.relayed) == 1 })
	}
	t.Cleanup(func() { promptSpawned, newWaitHostKnown = prev, prevKnown })
	stdout, _, code := goCraze("new", "--wait", "--json", "--session-file", file, "--provider", "cursor", "-C", ws, "never sent").wait(t)
	if m := decodeResult(t, stdout); code != 3 || m["status"] != "cancelled" {
		t.Fatalf("exit %d, %v", code, m)
	}
	if !wrote {
		t.Fatal("the file was not there before the signal")
	}
	waitReaped(t, host.PID)
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file of a session that never took a prompt: %v", err)
	}
	if b, err := os.ReadFile(prompts); err == nil && len(b) != 0 {
		t.Fatalf("the agent was prompted: %s", b)
	}
}

// TestTheSessionFilesSteps: its two writes, a rewrite that fails leaving the first,
// removal only of what this run wrote, and a directory left alone.
func TestTheSessionFilesSteps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d", "session.json")
	e := rundir.Entry{HostID: "0000000000a1", PID: 42, CrazeSessionID: "0199c0de-1111-7000-8000-00000000a001"}
	read := func() sessionFileRecord {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var rec sessionFileRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	var none *sessionFile
	none.clear()
	none.remove()
	if err := none.host(sendWhat, e); err != nil || none.submitted("h") != nil {
		t.Fatal("no flag wrote something")
	}
	f := newSessionFile(path)
	if err := f.submitted("too soon"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a handle with no first write: %v", err)
	}
	if err := f.host(sendWhat, e); err != nil {
		t.Fatal(err)
	}
	want := sessionFileRecord{Session: e.CrazeSessionID, Short: "0000a001", HostPID: 42, HostID: e.HostID}
	if got := read(); got != want {
		t.Fatalf("the first write: %+v", got)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("its mode: %v, %v", st, err)
	}
	if err := f.host(sendWhat, rundir.Entry{HostID: "another"}); err != nil || read() != want {
		t.Fatalf("a second host call: %v, %+v", err, read())
	}
	if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
	if err := f.submitted("h"); err == nil {
		t.Fatal("a rewrite into a read-only directory said nothing")
	}
	if got := read(); got != want {
		t.Fatalf("a failed rewrite left: %+v", got)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.submitted("h"); err != nil {
		t.Fatal(err)
	}
	want.Handle = "h"
	if got := read(); got != want {
		t.Fatalf("the rewrite: %+v", got)
	}
	f.remove()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed: %v", err)
	}
	// What this run did not write stays until clear, and a directory always.
	if err := os.WriteFile(path, []byte("earlier"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := newSessionFile(path)
	other.remove()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("remove took what it did not write: %v", err)
	}
	other.clear()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clear left an earlier run's file: %v", err)
	}
	empty := t.TempDir()
	asDir := newSessionFile(empty)
	asDir.clear()
	if _, err := os.Stat(empty); err != nil {
		t.Fatalf("clear took a directory: %v", err)
	}
	err := asDir.host(sendWhat, e)
	if err == nil || failureKind(err) != errkind.Config {
		t.Fatalf("a first write onto a directory: %v", err)
	}
}

// TestPsBriefLeavesOnlyTheCatalogsOut: members this build does not know pass
// through, and a row not read yet is no error.
func TestPsBriefLeavesOnlyTheCatalogsOut(t *testing.T) {
	in := `{"epoch":"e","cursor":3,"later":{"a":1},"sessions":[` +
		`{"hostId":"h1","row":{"sessionId":"s1","catalogs":{"models":[{"id":"x"}]},"later":[1,2],"attached":2}},` +
		`{"hostId":"h2","status":"connecting"}]}`
	b, err := psBrief(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cursor":3,"epoch":"e","later":{"a":1},"sessions":[` +
		`{"hostId":"h1","row":{"attached":2,"later":[1,2],"sessionId":"s1"}},` +
		`{"hostId":"h2","status":"connecting"}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	if _, err := psBrief(json.RawMessage(`{"sessions":[{"row":3}]}`)); err == nil {
		t.Fatal("a row that is no object said nothing")
	}
}

// ------------------------------------------------------------- the idle clock

// asksFreeRig is a watcher over a stub on an unattended host: its asks do not pin it.
func asksFreeRig(t *testing.T, config string) *idleRig {
	t.Helper()
	stub := tui.NewStubNoPrimary()
	r := newIdleRigOn(t, config, true, stub, engine.Options{AsksDoNotPin: true})
	r.stub = stub
	r.asUnattended()
	return r
}

// openAsk opens a permission ask on the rig's stub.
func (r *idleRig) openAsk(id string) {
	r.t.Helper()
	r.stub.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: id, Tool: "Shell",
		Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
	if n := r.eng.State().PendingAsks; n == 0 {
		r.t.Fatal("the ask did not open")
	}
}

// TestAnAskGetsTheWholeLimit: an ask that opens between two looks, with the clock
// nearly out, gets the whole limit from the first look that sees it; another ask
// restarts it again. Otherwise the turn that asked would be stopped at once.
func TestAnAskGetsTheWholeLimit(t *testing.T) {
	r := asksFreeRig(t, "host_idle_exit_unattended = \"10s\"\n")
	r.prompted()
	r.advance(hostStartupGrace)
	r.wantStay("armed")
	r.advance(9999 * time.Millisecond)
	r.openAsk("perm-1")
	r.advance(time.Millisecond)
	if l := r.wantStay("the first look at the ask"); l.Idle != 0 {
		t.Fatalf("the clock did not restart: %+v", l)
	}
	r.advance(9999 * time.Millisecond)
	r.wantStay("just under the limit since")
	r.advance(time.Millisecond)
	r.wantStop("the limit since", "an ask nobody answered")
}

// TestAnAskOpeningAtTheVerdictKeepsTheHost: one that opens in the instant between the
// look's sample and its close fence, which reads it as not pinning: the verdict is not
// taken over it, and the clock starts again.
func TestAnAskOpeningAtTheVerdictKeepsTheHost(t *testing.T) {
	r := asksFreeRig(t, "host_idle_exit_unattended = \"10s\"\n")
	r.prompted()
	r.advance(hostStartupGrace)
	r.wantStay("armed")
	r.advance(10 * time.Second)
	r.beforeFence = func() { r.openAsk("perm-1") }
	if l := r.wantStay("an ask opening at the verdict"); !l.Fenced {
		t.Fatalf("%+v", l)
	}
	if _, err := r.eng.Submit(engine.Command{}, "still admits", engine.SubmitQueue, ""); errors.Is(err, engine.ErrClosing) {
		t.Fatal("the fences stayed up")
	}
}

// TestOwedBackgroundWorkDoesNotOutliveTheAsk: a real native session with a real
// background job running, its turn then blocked on a question nobody answers. The job
// is owed work, and would pin the host for good: a parent that cannot go on cannot
// deliver it. Past the limit the host stops, the job with it. Interactive rules keep it.
func TestOwedBackgroundWorkDoesNotOutliveTheAsk(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts engine.Options
		stop bool
	}{
		{"asks do not pin", engine.Options{AsksDoNotPin: true}, true},
		{"an interactive host", engine.Options{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := syscall.Mkfifo(ws+"/g", 0o600); err != nil {
				t.Fatal(err)
			}
			question := `{"questions":[{"question":"Which?","options":[{"label":"a","description":"a"},{"label":"b","description":"b"}]}]}`
			job := nativeJSONCall("c1", "bash", `{"command":"read line < g","run_in_background":true}`)
			ask := nativeJSONCall("c2", "ask_user_question", question)
			model := &lockedSteps{steps: [][]fantasy.StreamPart{job, ask}}
			sess := interactiveNative(t, model, ws)
			r := newIdleRigOn(t, "host_idle_exit = \"10s\"\nhost_idle_exit_unattended = \"10s\"\n", true, sess, tc.opts)
			if tc.stop {
				r.asUnattended()
			}
			if _, err := r.eng.Submit(engine.Command{}, "go", engine.SubmitQueue, ""); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the question, the job running", func() bool {
				owed, _ := sess.(agent.OwedWork)
				return r.eng.State().PendingAsks == 1 && owed.OwesWork()
			})
			r.advance(hostStartupGrace + time.Second)
			r.wantStay("armed")
			r.advance(2 * time.Second)
			r.wantStay("the first look at the ask")
			r.advance(time.Hour)
			if tc.stop {
				r.wantStop("past the limit", "an ask nobody answered")
			} else {
				r.wantStay("an ask pins an interactive host")
			}
		})
	}
}

// TestAnUnreadableEntryEstablishesNoEnd: hostWent's scan: an entry it cannot read may
// be this session's host. A running host's process and entry say otherwise anyway.
func TestAnUnreadableEntryEstablishesNoEnd(t *testing.T) {
	r := newWaitRig(t, 0)
	e := r.entry()
	path := filepath.Join(os.Getenv("HOME"), ".cache", "craze", "hosts", e.HostID+".json")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !sessionLive(waitRigID) {
		t.Fatal("a scan that missed a host says no host serves the session")
	}
	if hostWent(e, waitRigID) {
		t.Fatal("a host whose entry cannot be read reads as gone")
	}
}

// asUnattended replaces the rig's watcher with an unattended host's.
func (r *idleRig) asUnattended() {
	r.w = newIdleWatcher(idleServerHook{Server: r.srv, r: r}, r.eng, r.lost, r.now, true, r.t.Output())
}

// TestAnUnattendedHostHasItsOwnLimit: host_idle_exit_unattended, not host_idle_exit,
// from a look that names it; and a client attached restarts its clock on leaving, as
// it does on any host.
func TestAnUnattendedHostHasItsOwnLimit(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"1h\"\nhost_idle_exit_unattended = \"3s\"\n", true)
	r.asUnattended()
	r.prompted()
	kill := r.attach()
	r.waitAttached(1)
	r.wantStay("armed by the attach")
	r.advance(time.Hour)
	if l := r.wantStay("an hour with a client attached"); !l.Armed {
		t.Fatalf("%+v", l)
	}
	kill()
	r.waitAttached(0)
	r.wantStay("just left")
	r.advance(2999 * time.Millisecond)
	if l := r.wantStay("just under the limit since it left"); l.Limit != 3*time.Second {
		t.Fatalf("the limit: %+v", l)
	}
	r.advance(time.Millisecond)
	r.wantStop("the limit since it left", "idle for 3s")
}

// TestAnUnattendedHostDefaultsToTenMinutes: and a never-prompted one keeps its cap.
func TestAnUnattendedHostDefaultsToTenMinutes(t *testing.T) {
	r := newIdleRig(t, "host_idle_exit = \"1h\"\n", true)
	r.asUnattended()
	r.advance(hostStartupGrace)
	r.wantStay("armed")
	r.advance(time.Second)
	if l := r.wantStay("never prompted"); l.Limit != hostNeverPromptedIdle {
		t.Fatalf("never prompted: %+v", l)
	}
	r.prompted()
	r.advance(time.Second)
	if l := r.wantStay("prompted"); l.Limit != 10*time.Minute {
		t.Fatalf("the default: %+v", l)
	}
}

// TestAFenceShortensAnInteractiveHostsLimit: a session under its exit fence on a host
// a person started has at most the unattended limit: what ends it if its waiter dies.
// Without one the host keeps its own, and "never" there is a limit under a fence.
func TestAFenceShortensAnInteractiveHostsLimit(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		limit        time.Duration
		timed        bool
	}{
		{"an hour's host", "host_idle_exit = \"1h\"\nhost_idle_exit_unattended = \"3s\"\n", 3 * time.Second, true},
		{"a shorter host's own", "host_idle_exit = \"1s\"\nhost_idle_exit_unattended = \"3s\"\n", time.Second, true},
		{"never's host", "host_idle_exit = \"never\"\nhost_idle_exit_unattended = \"3s\"\n", 3 * time.Second, true},
		{"never under the fence too", "host_idle_exit = \"never\"\nhost_idle_exit_unattended = \"never\"\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newIdleRig(t, tc.config, true)
			if _, err := r.eng.SubmitExitAfterTurn(engine.Command{}, "the last"); err != nil {
				t.Fatal(err)
			}
			r.waitIdle()
			r.advance(hostStartupGrace)
			r.wantStay("armed")
			if l := r.wantStay("the limit it has"); l.Limit != tc.limit || l.Timed != tc.timed {
				t.Fatalf("%+v", l)
			}
		})
	}
}
