package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
)

// The stopping mark (plan 046 B4, #93) as craze's hosts write it: synchronous,
// kept by the registry's writer, written before a session.stop's receipt,
// first thing in a signal's stop, and before a TUI-hosted quit closes its
// engine — so no roster round that reads the registry after it lists the host
// (B9 (viii) for the TUI-hosted quit; (i) and (ix) are the hub's,
// internal/hub's stopping_test.go).

// TestTheMarkIsWrittenAtOnceAndTheWriterKeepsIt: the mark is in the entry
// file when markStopping returns, even while the registry's writer is held in
// the middle of a rewrite queued before it — the mark never waits in the
// writer's queue — and that rewrite, landing after, keeps it, as does every
// later one (the engine's ready rewrite). A second mark rewrites nothing.
func TestTheMarkIsWrittenAtOnceAndTheWriterKeepsIt(t *testing.T) {
	env := serveEnv(t)
	var diag lockedBuffer
	rh, hostID := servingHost(t, env, &diag)
	path := entryPath(env, hostID)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	rh.ctl.mu.Lock()
	update := rh.ctl.update
	rh.ctl.update = func(fn func(*rundir.Entry)) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return update(fn)
	}
	rh.ctl.mu.Unlock()
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)

	eng := grokStubEngine(t)
	rh.onEngine(eng) // a rewrite queued, its writer held in it
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer never took the rewrite")
	}
	rh.markStopping()
	if got, ok := readEntryFile(t, path); !ok || !got.Stopping || got.CrazeSessionID != "" {
		t.Fatalf("at markStopping's return, with the writer held: %+v (exists %v), want stopping and the bind's entry", got, ok)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	rh.markStopping()
	if again, err := os.Lstat(path); err != nil || !os.SameFile(fi, again) {
		t.Fatalf("a second mark rewrote the entry (%v)", err)
	}

	open()
	st := eng.State()
	got := waitEntry(t, path, func(e rundir.Entry) bool { return e.CrazeSessionID != "" })
	if !got.Stopping || got.CrazeSessionID != st.CrazeSessionID {
		t.Fatalf("the writer's rewrite after the mark: %+v, want the engine's identity and the mark", got)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ready := waitEntry(t, path, func(e rundir.Entry) bool { return e.Ready }); !ready.Stopping {
		t.Fatalf("the ready rewrite dropped the mark: %+v", ready)
	}
	if s := diag.String(); s != "" {
		t.Fatalf("a healthy mark said %q", s)
	}
}

// TestALostHostsQueuedRewriteBringsNothingBack (review c3 2): the schedule
// of a host stopping because it was lost — a rewrite queued and its writer
// held in it, the registry entry gone from under the host, the stop's mark
// (which finds nothing to mark), then the held rewrite landing — leaves no
// entry: nothing brings a stopping host back into the registry, marked or
// not. The control, the same schedule with no mark, has the rewrite write
// the entry again, unmarked — the resurrection the mark rules out.
func TestALostHostsQueuedRewriteBringsNothingBack(t *testing.T) {
	for _, mark := range []bool{true, false} {
		name := "marked"
		if !mark {
			name = "no mark (the control)"
		}
		t.Run(name, func(t *testing.T) {
			env := serveEnv(t)
			var diag lockedBuffer
			rh, hostID := servingHost(t, env, &diag)
			path := entryPath(env, hostID)
			gate := make(chan struct{})
			entered := make(chan struct{}, 1)
			rh.ctl.mu.Lock()
			update := rh.ctl.update
			rh.ctl.update = func(fn func(*rundir.Entry)) error {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-gate
				return update(fn)
			}
			rh.ctl.mu.Unlock()
			var once sync.Once
			open := func() { once.Do(func() { close(gate) }) }
			t.Cleanup(open)

			landed := rh.publish(grokStubEngine(t))
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the writer never took the rewrite")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if mark {
				rh.markStopping()
			}
			open()
			select {
			case <-landed:
			case <-time.After(10 * time.Second):
				t.Fatal("the held rewrite never returned")
			}
			got, ok := readEntryFile(t, path)
			switch {
			case mark && ok:
				t.Fatalf("the rewrite after the mark brought the entry back: %+v", got)
			case !mark && (!ok || got.Stopping):
				t.Fatalf("the control: the rewrite wrote %+v (exists %v), want the entry back, unmarked", got, ok)
			}
			if s := diag.String(); s != "" {
				t.Fatalf("said %q", s)
			}
		})
	}
}

// stepHolds holds the stop sequence at each of its steps named, in turn:
// the sequence signals arrival and waits for the test's release.
type stepHolds struct {
	mu   sync.Mutex
	at   map[string]chan struct{}
	rel  map[string]chan struct{}
	once map[string]*sync.Once
}

// holdSteps installs teardownStep holds at steps; each is let go by release,
// and all of them when the test ends — registered after the serve run, so
// before its own cleanup waits for it to return.
func holdSteps(t *testing.T, steps ...string) *stepHolds {
	t.Helper()
	h := &stepHolds{at: map[string]chan struct{}{}, rel: map[string]chan struct{}{}, once: map[string]*sync.Once{}}
	for _, s := range steps {
		h.at[s], h.rel[s], h.once[s] = make(chan struct{}), make(chan struct{}), &sync.Once{}
	}
	prev := teardownStep
	teardownStep = func(s string) {
		h.mu.Lock()
		at, rel := h.at[s], h.rel[s]
		h.mu.Unlock()
		if at == nil {
			return
		}
		close(at)
		<-rel
	}
	t.Cleanup(func() { teardownStep = prev })
	return h
}

func (h *stepHolds) await(t *testing.T, step string) {
	t.Helper()
	select {
	case <-h.at[step]:
	case <-time.After(serveStep):
		t.Fatalf("the stop sequence never reached %q", step)
	}
}

func (h *stepHolds) release(step string) { h.once[step].Do(func() { close(h.rel[step]) }) }

func (h *stepHolds) releaseAll() {
	for s := range h.rel {
		h.release(s)
	}
}

// TestServeMarksStoppingBeforeAnythingOfItsStop: a session.stop's mark is in
// the registry when its receipt has been read — before the stop sequence has
// begun, the sequence held at its first step — written by the control
// server's seam (control.Options.StopAccepted), not by the sequence. A
// signal's stop has no receipt: nothing is marked at the sequence's first step
// (the control: the hold is ahead of the sequence's own mark), and the
// sequence marks before its fence and its engine's close. Either way the host
// then stops as before, and its entry goes.
func TestServeMarksStoppingBeforeAnythingOfItsStop(t *testing.T) {
	for _, by := range []string{"session.stop", "SIGTERM"} {
		t.Run(by, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
			holds := holdSteps(t, "stopping", "fenced")
			t.Cleanup(holds.releaseAll)
			e := r.waitServing(t, env, true)
			path := entryPath(env, e.HostID)
			if got, _ := readEntryFile(t, path); got.Stopping {
				t.Fatalf("a serving host's entry is marked: %+v", got)
			}

			if by == "session.stop" {
				stopOver(t, dialServe(t, e), "1")
			} else {
				r.sigs <- syscall.SIGTERM
			}
			holds.await(t, "stopping")
			got, ok := readEntryFile(t, path)
			if !ok || got.Stopping != (by == "session.stop") {
				t.Fatalf("at the stop sequence's first step the entry is %+v (exists %v), want stopping %v",
					got, ok, by == "session.stop")
			}
			holds.release("stopping")
			holds.await(t, "fenced")
			if got, ok := readEntryFile(t, path); !ok || !got.Stopping {
				t.Fatalf("at the fence the entry is %+v (exists %v), want stopping", got, ok)
			}
			holds.release("fenced")
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve: %v", err)
			}
			assertHostGone(t, env, e)
		})
	}
}

// TestAMarkThatFailsIsSaidAndTheStopGoesOn: the mark is best effort — a
// rewrite that fails is one line on the host's log, and the stop goes on to
// its end, the entry unlinked as before.
func TestAMarkThatFailsIsSaidAndTheStopGoesOn(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	prev := hostMarkStopping
	hostMarkStopping = func(*rundir.Host) error { return errors.New("injected: the registry is read-only") }
	t.Cleanup(func() { hostMarkStopping = prev })
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	stopOver(t, dialServe(t, e), "1")
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	assertHostGone(t, env, e)
	if s := r.stderr.String(); !strings.Contains(s, "craze: control socket registry entry not marked stopping: injected: the registry is read-only") {
		t.Fatalf("the log did not say the mark failed: %s", s)
	}
}

// TestATUIHostedQuitIsNeverListedAfterItsMark is B9 (viii): a TUI-hosted
// session — its TUI quits with Ctrl+D — and a roster polling it. Once the
// session's registry entry is ready and the roster has read that, a round's
// registry read lists the session, serving; its attempt is held until the
// quit has marked the entry and closed the engine (the teardown's first step,
// the server still up), and then let go: its result moves the row, the entry
// read again says stopping, and the host is gone in that round. No Snapshot
// published after the mark lists it. The negative control runs the same
// schedule with the quit's mark unwired (Config.OnQuit cleared on its way to
// tui.Run): the entry is unmarked at the teardown and the host is listed.
func TestATUIHostedQuitIsNeverListedAfterItsMark(t *testing.T) {
	for _, wired := range []bool{true, false} {
		name := "the quit marks"
		if !wired {
			name = "the quit's mark unwired (the control)"
		}
		t.Run(name, func(t *testing.T) { tuiHostedQuit(t, wired) })
	}
}

func tuiHostedQuit(t *testing.T, wired bool) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Skipf("pty resize: %v", err)
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv("CRAZE_JOURNAL", "0")
	crazeHome(t)
	t.Setenv("CRAZE_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	env := rundir.ProcessEnv()
	if !wired {
		prev := tuiRun
		tuiRun = func(cfg tui.Config) (tui.Result, error) {
			cfg.OnQuit = nil
			return prev(cfg)
		}
		t.Cleanup(func() { tuiRun = prev })
	}

	// The roster: the poll's own, over the real registry; its ticks the
	// test's, each attempt's budget a step's (a starved scheduler fails no
	// attempt the test did not mean to), and the next attempt held once
	// armed.
	ticks := make(chan time.Time)
	var (
		mu       sync.Mutex
		snaps    []roster.Snapshot
		armed    bool
		arrived  = make(chan struct{})
		released = make(chan struct{})
		relOnce  sync.Once
	)
	note := make(chan struct{}, 1)
	release := func() { relOnce.Do(func() { close(released) }) }
	r := roster.OpenHub(roster.HubOptions{
		Hosts: func() ([]rundir.Entry, error) { return rundir.Hosts(env) },
		Entry: func(id string) (rundir.Entry, bool, error) { return rundir.HostEntry(env, id) },
		Publish: func(s roster.Snapshot) {
			mu.Lock()
			snaps = append(snaps, s)
			mu.Unlock()
			select {
			case note <- struct{}{}:
			default:
			}
		},
		Client: protocol.ClientInfo{Kind: "test", Name: "the quit's roster"},
		Ticks:  ticks,
		Budget: serveStep,
		Attempting: func(string) {
			mu.Lock()
			hold := armed
			armed = false
			mu.Unlock()
			if hold {
				close(arrived)
				<-released
			}
		},
	})
	defer r.Close()
	defer release() // before the roster's Close: a held attempt holds it
	published := func() []roster.Snapshot {
		mu.Lock()
		defer mu.Unlock()
		return append([]roster.Snapshot(nil), snaps...)
	}
	waitSnap := func(what string, from int, pred func(roster.Snapshot) bool) {
		t.Helper()
		deadline := time.After(serveStep)
		for {
			for _, s := range published()[from:] {
				if pred(s) {
					return
				}
			}
			select {
			case <-note:
			case <-deadline:
				t.Fatalf("%s: no such Snapshot within %v", what, serveStep)
			}
		}
	}

	// The teardown's first step (controlHost.close's "flushing"): the quit
	// has marked — when wired — and closed the engine, and the server is
	// still up. The held attempt goes there, and the teardown waits for its
	// result.
	var markedAtTeardown, entryAtTeardown bool
	var afterMark int
	teardownStep = func(step string) {
		if step != "flushing" {
			return
		}
		paths, _ := filepath.Glob(filepath.Join(home, ".cache", "craze", "hosts", "*.json"))
		if len(paths) == 1 {
			e, ok := readEntryFile(t, paths[0])
			entryAtTeardown, markedAtTeardown = ok, e.Stopping
		}
		afterMark = len(published())
		release()
		// The attempt's result is published once taken: the host gone, or —
		// without the mark — listed with its moved row.
		deadline := time.After(serveStep)
		for len(published()) == afterMark {
			select {
			case <-note:
			case <-deadline:
				return
			}
		}
	}
	t.Cleanup(func() { teardownStep = func(string) {} })

	tail := newPTYTail(ptmx)
	prevIn, prevOut, prevErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = tty, tty, tty
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = prevIn, prevOut, prevErr })
	done := make(chan error, 1)
	go func() {
		done <- runTUI(nil, &tuiFlags{agentBin: fakeAgentPath(t), workspace: t.TempDir(), force: true}, hostEnv{})
	}()
	if !tail.wait(" craze ─", 10*time.Second) {
		t.Fatalf("craze never drew a frame; got %q", tail.text())
	}
	waitSessionStarted(t, ptmx, tail)
	// The session's entry as it will stay until the quit: its identity and
	// ready rewrites landed (the writer's, asynchronous), so no later round
	// reads a changed entry before the mark.
	var hostID string
	deadline := time.Now().Add(serveStep)
	for hostID == "" {
		if entries, err := rundir.Hosts(env); err == nil && len(entries) == 1 && entries[0].CrazeSessionID != "" && entries[0].Ready {
			hostID = entries[0].HostID
		} else if time.Now().After(deadline) {
			t.Fatalf("the TUI-hosted session never registered ready: %+v, %v", entries, err)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Run 1: the host listed ready, and answered.
	r.Resume(1)
	waitSnap("the host reachable and ready", 0, func(s roster.Snapshot) bool {
		for _, row := range s.Running {
			if row.Host.ID == hostID && row.Status == roster.Reachable && row.Host.Ready {
				return true
			}
		}
		return false
	})
	// The next round: its registry read lists the host serving — as the
	// last did, so the round publishes nothing before its attempt — and its
	// attempt is held.
	mu.Lock()
	armed = true
	mu.Unlock()
	select {
	case ticks <- time.Now():
	case <-time.After(serveStep):
		t.Fatal("the roster took no tick")
	}
	select {
	case <-arrived:
	case <-time.After(serveStep):
		t.Fatal("the round's attempt never came")
	}

	if _, err := ptmx.Write([]byte{0x04}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runTUI = %v, want a clean exit", err)
		}
	case <-time.After(serveStep):
		t.Fatal("craze did not quit after ctrl+d")
	}

	if !entryAtTeardown || markedAtTeardown != wired {
		t.Errorf("at the teardown the entry existed %v, marked stopping %v; want it there, marked %v",
			entryAtTeardown, markedAtTeardown, wired)
	}
	after := published()[afterMark:]
	if len(after) == 0 {
		t.Fatal("the held attempt's result was never published")
	}
	listed := false
	for _, s := range after {
		for _, row := range s.Running {
			if row.Host.ID == hostID {
				listed = true
				if wired {
					t.Errorf("a Snapshot published after the mark lists the host (%s)", row.Status)
				}
			}
		}
	}
	if !wired && !listed {
		t.Error("the control: with the quit's mark unwired, no Snapshot after the quit lists the host")
	}
}
