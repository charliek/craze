package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// The launch's session list (plan 030 §3.9), production's pieces end to end:
// the roster over the registry and the index, Open over a host's socket, Stop
// over one of its own — against a host this test binary runs as craze serve
// with the fake agent. Every wait is bounded on its own (serveStep).

// waitList reads r's snapshots until pred holds for one, within a step.
func waitList(t *testing.T, r tui.SessionRoster, what string, pred func(roster.Snapshot) bool) roster.Snapshot {
	t.Helper()
	deadline := time.After(serveStep)
	var last roster.Snapshot
	for {
		select {
		case s, ok := <-r.Updates():
			if !ok {
				t.Fatalf("%s: the roster stopped", what)
			}
			last = s
			if pred(s) {
				return s
			}
		case <-deadline:
			t.Fatalf("%s: never; the last snapshot %+v", what, last)
		}
	}
}

// runningRow is the running row of craze session id in s, nil when none.
func runningRow(s roster.Snapshot, id string) *roster.Row {
	for i := range s.Running {
		if s.Running[i].Host.CrazeSessionID == id {
			return &s.Running[i]
		}
	}
	return nil
}

// TestTheSessionListListsOpensAndStopsAHost: the host a launch spawned is
// listed, reachable, with the row facts — prompted once its turn has run,
// its reply the last reply — and opened as a second client, attached; Stop
// ends it on its host, which leaves the registry, and the session, prompted,
// is saved.
func TestTheSessionListListsOpensAndStopsAHost(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	ran := false
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		ran = true
		if cfg.Sessions == nil {
			t.Fatal("the launch has no session list")
		}
		b, err := cfg.NewBackend(cfg.Provider, false)
		if err != nil {
			t.Fatalf("NewBackend: %v", err)
		}
		started(t, b)
		// P27 (plan 033): a host this launch spawned runs in its CRAZE_HOME,
		// and so reads the TUI's attachments directory.
		if !readsAttachments(b) {
			t.Fatal("the launch's own host says it cannot read the attachments")
		}
		id := b.Info().CrazeSessionID
		if _, err := b.Submit(stepCtx(t), engine.Command{Client: b.ClientID(), ID: "1"}, "hello there", engine.SubmitQueue, ""); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		r := cfg.Sessions.Roster()
		defer r.Close()
		s := waitList(t, r, "the host listed, its turn over", func(s roster.Snapshot) bool {
			row := runningRow(s, id)
			return row != nil && row.Status == roster.Reachable && row.Session != nil &&
				row.Session.Prompted && row.Session.Activity == engine.ActivityIdle && row.Session.LastReply != ""
		})
		row := runningRow(s, id)
		switch se := row.Session; {
		case !se.RowFacts || se.Since.IsZero() || !se.Stop:
			t.Fatalf("a craze serve host's row: %+v", se)
		case row.Version == "" || row.Host.Workspace == "":
			t.Fatalf("the row's host: %+v, version %q", row.Host, row.Version)
		}
		o, err := cfg.Sessions.Open(row.Ref())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got := o.Info().CrazeSessionID; got != id {
			t.Fatalf("Open answered session %q, want %q", got, id)
		}
		if !readsAttachments(o) {
			t.Fatal("a running row of this CRAZE_HOME opened says it cannot read the attachments")
		}
		_ = o.Close()
		_ = b.Close()
		if err := cfg.Sessions.Stop(row.Ref()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		waitReaped(t, cmds.pids()[0])
		s = waitList(t, r, "the session saved once its host is gone", func(s roster.Snapshot) bool {
			return runningRow(s, id) == nil && len(s.Saved) == 1 && s.Saved[0].CrazeID == id
		})
		if err := cfg.Sessions.Stop(roster.SavedRef(s.Saved[0])); !errors.Is(err, errStopSaved) {
			t.Fatalf("Stop of a saved session: %v", err)
		}
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if !ran {
		t.Fatal("the TUI never ran")
	}
	assertNoHosts(t, env)
}

// withinStep runs fn on a goroutine of its own and answers its error,
// failing the test when fn has not returned within a step: a call bounded
// by the code under test is bounded here too, so a regression there fails
// with its name instead of hanging the suite.
func withinStep(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(serveStep):
		t.Fatalf("%s did not return within %v", what, serveStep)
		return nil
	}
}

// hostProxy is a Unix socket in front of a host's, for a client the test
// watches on the wire: every request the client sends is recorded before it
// is passed on to the host, and every answer the host sends is recorded
// before it is passed back — so the record is the order the host was asked
// in, and says whether an answer had reached the client before its next
// request left it.
type hostProxy struct {
	path string
	mu   sync.Mutex
	log  []string
}

// newHostProxy listens in front of the socket at target until the test ends.
func newHostProxy(t *testing.T, target string) *hostProxy {
	t.Helper()
	p := &hostProxy{path: filepath.Join(shortRuntimeDir(t), "proxy.sock")}
	ln, err := net.Listen("unix", p.path)
	if err != nil {
		t.Fatal(err)
	}
	var (
		wg    sync.WaitGroup
		cmu   sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = ln.Close()
		cmu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		cmu.Unlock()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			host, err := net.Dial("unix", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			cmu.Lock()
			conns = append(conns, client, host)
			cmu.Unlock()
			wg.Add(2)
			go func() { defer wg.Done(); p.pipe(client, host, true) }()
			go func() { defer wg.Done(); p.pipe(host, client, false) }()
		}
	}()
	return p
}

// pipe passes from's lines on to to, one at a time, recording each first: a
// client's request as "→ <method> <id>", a host's answer as "← <id>" (the
// host's notifications are not recorded). Either side's end closes both.
func (p *hostProxy) pipe(from, to net.Conn, fromClient bool) {
	defer func() {
		_ = from.Close()
		_ = to.Close()
	}()
	r := bufio.NewReader(from)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(line, &msg) == nil {
				switch {
				case fromClient && msg.Method != "":
					p.record("→ " + msg.Method + " " + string(msg.ID))
				case !fromClient && msg.Method == "" && len(msg.ID) > 0:
					p.record("← " + string(msg.ID))
				}
			}
			if _, err := to.Write(line); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *hostProxy) record(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, s)
}

// seen is the record so far.
func (p *hostProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.log)
}

// TestTheSessionListCancelsATurnAndClearsItsQueue is the list's ctrl+x on a
// working row (plan 030 §3.10), production's Cancel end to end: a host whose
// agent never finishes a prompt until it is cancelled (the fake's hang), one
// turn running and one prompt queued behind it. Cancel clears the queue and
// then cancels, over a connection of its own — watched on the wire (a proxy
// in front of the host's socket), where the host is asked session.queue.clear
// first and session.cancel only once the clear's answer has reached the
// client (sol r19-c10 4): the row then settles idle, its last turn cancelled,
// and nothing is left queued. A second Cancel, with nothing left to cancel,
// is not an error; a saved session's is. Every Cancel is bounded by a step.
func TestTheSessionListCancelsATurnAndClearsItsQueue(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
	ran := false
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		ran = true
		b, err := cfg.NewBackend(cfg.Provider, false)
		if err != nil {
			t.Fatalf("NewBackend: %v", err)
		}
		started(t, b)
		id := b.Info().CrazeSessionID
		cmd := func(n string) engine.Command { return engine.Command{Client: b.ClientID(), ID: n} }
		first, err := b.Submit(stepCtx(t), cmd("1"), "never ends", engine.SubmitQueue, "")
		if err != nil || first.Turn == "" {
			t.Fatalf("the first prompt: %+v, %v", first, err)
		}
		second, err := b.Submit(stepCtx(t), cmd("2"), "queued behind it", engine.SubmitQueue, "")
		if err != nil || second.Queued == nil {
			t.Fatalf("the second prompt was not queued: %+v, %v", second, err)
		}
		r := cfg.Sessions.Roster()
		defer r.Close()
		s := waitList(t, r, "the host listed, its turn working", func(s roster.Snapshot) bool {
			row := runningRow(s, id)
			return row != nil && row.Status == roster.Reachable && row.Session != nil &&
				row.Session.Activity == engine.ActivityWorking
		})
		ref := runningRow(s, id).Ref()
		proxy := newHostProxy(t, ref.Host.Socket)
		proxied := ref
		proxied.Host.Socket = proxy.path
		if err := withinStep(t, "Cancel", func() error { return cfg.Sessions.Cancel(proxied) }); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		var methods []string
		clearID, answered := "", false
		for _, line := range proxy.seen() {
			if m, ok := strings.CutPrefix(line, "→ "); ok {
				method, reqID, _ := strings.Cut(m, " ")
				methods = append(methods, method)
				switch method {
				case protocol.MethodQueueClear:
					clearID = reqID
				case protocol.MethodSessionCancel:
					if !answered {
						t.Fatalf("session.cancel left before session.queue.clear was answered: %q", proxy.seen())
					}
				}
			} else if clearID != "" && line == "← "+clearID {
				answered = true
			}
		}
		if want := []string{protocol.MethodHello, protocol.MethodQueueClear, protocol.MethodSessionCancel}; !slices.Equal(methods, want) {
			t.Fatalf("the host was asked %q, want %q (the record: %q)", methods, want, proxy.seen())
		}
		waitList(t, r, "the turn cancelled and nothing started behind it", func(s roster.Snapshot) bool {
			row := runningRow(s, id)
			return row != nil && row.Session != nil && row.Session.Activity == engine.ActivityIdle &&
				row.Session.LastTurn != nil && row.Session.LastTurn.Outcome == engine.TurnCancelled
		})
		removed, err := b.ClearQueue(stepCtx(t), cmd("3"))
		if err != nil || len(removed) != 0 {
			t.Fatalf("the queue after Cancel: %d rows left, %v", len(removed), err)
		}
		if err := withinStep(t, "a second Cancel", func() error { return cfg.Sessions.Cancel(ref) }); err != nil {
			t.Fatalf("a second Cancel, with nothing to cancel: %v", err)
		}
		saved := roster.SavedRef(sessions.Row{SessionID: "x", Provider: "cursor"})
		if err := withinStep(t, "a saved session's Cancel", func() error { return cfg.Sessions.Cancel(saved) }); !errors.Is(err, errStopSaved) {
			t.Fatalf("Cancel of a saved session: %v", err)
		}
		_ = b.Close()
		if err := cfg.Sessions.Stop(ref); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		waitReaped(t, cmds.pids()[0])
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if !ran {
		t.Fatal("the TUI never ran")
	}
	assertNoHosts(t, env)
}

// TestASpawnTheTUINeverTookIsStoppedAtQuit (sol r17-c9 1): a host the
// list's Spawn started is this launch's to stop, so a TUI that quits first
// leaves none behind. Never opened, or opened and never come up, it is
// stopped before runTUI returns (finish), as a launch's own spawn nobody
// adopted is; one Open cannot reach is stopped before Open answers — nobody
// else would stop it. One on which a session came up in the TUI (its start
// acknowledged) goes on, even when an earlier Open's session never came up
// (sol r20-c9r 1: decided by host, not by whichever Open took it first); so
// does one the list left running (PR 3's background dispatch), opened or not.
func TestASpawnTheTUINeverTookIsStoppedAtQuit(t *testing.T) {
	for _, tc := range []struct {
		name string
		// dialFails makes every dial of the launch's fail: Open's.
		dialFails bool
		take      func(t *testing.T, cfg tui.Config, ref roster.Ref, pid int)
		kept      bool
	}{
		{name: "never opened", take: func(*testing.T, tui.Config, roster.Ref, int) {}},
		{name: "opened, never come up", take: func(t *testing.T, cfg tui.Config, ref roster.Ref, _ int) {
			if _, err := cfg.Sessions.Open(ref); err != nil {
				t.Fatalf("Open: %v", err)
			}
		}},
		{name: "opened, unreachable", dialFails: true, take: func(t *testing.T, cfg tui.Config, ref roster.Ref, pid int) {
			if _, err := cfg.Sessions.Open(ref); err == nil {
				t.Fatal("Open answered with every dial failing")
			}
			// No wait: Open stopped it (hostRef.abandon) before it answered.
			if processAlive(pid) {
				t.Fatalf("Open failed with the host %d it could not reach still there (%s)", pid, procState(pid))
			}
		}},
		{name: "opened and come up", kept: true, take: func(t *testing.T, cfg tui.Config, ref roster.Ref, _ int) {
			b, err := cfg.Sessions.Open(ref)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			started(t, b)
			_ = b.Close()
		}},
		{name: "opened twice, the second come up", kept: true, take: func(t *testing.T, cfg tui.Config, ref roster.Ref, _ int) {
			// The TUI moved on before the first came up (C11 closes a dial
			// it no longer wants), and opened the session again.
			first, err := cfg.Sessions.Open(ref)
			if err != nil {
				t.Fatalf("the first Open: %v", err)
			}
			_ = first.Close()
			b, err := cfg.Sessions.Open(ref)
			if err != nil {
				t.Fatalf("the second Open: %v", err)
			}
			started(t, b)
			_ = b.Close()
		}},
		{name: "left running", kept: true, take: func(t *testing.T, cfg tui.Config, ref roster.Ref, _ int) {
			if err := cfg.Sessions.LeaveRunning(ref); err != nil {
				t.Fatalf("LeaveRunning: %v", err)
			}
		}},
		{name: "opened, never come up, left running", kept: true, take: func(t *testing.T, cfg tui.Config, ref roster.Ref, _ int) {
			if _, err := cfg.Sessions.Open(ref); err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := cfg.Sessions.LeaveRunning(ref); err != nil {
				t.Fatalf("LeaveRunning: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			if tc.dialFails {
				prev := spawnDial
				spawnDial = func(context.Context, string, remote.SessionOptions) (*remote.Session, error) {
					return nil, errors.New("the test's dial fails")
				}
				t.Cleanup(func() { spawnDial = prev })
			}
			var spawned roster.Ref
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				ref, err := cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: ws, Provider: cfg.Provider})
				if err != nil {
					t.Fatalf("Spawn: %v", err)
				}
				if ref.Host.ID == "" || ref.Host.Socket == "" || ref.Saved != nil {
					t.Fatalf("Spawn's ref %+v", ref)
				}
				spawned = ref
				tc.take(t, cfg, ref, cmds.pids()[0])
				return tui.Result{}, nil
			})
			if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if n := cmds.count(); n != 1 {
				t.Fatalf("%d hosts spawned, want Spawn's one", n)
			}
			pid := cmds.pids()[0]
			if !tc.kept {
				// No wait: finish stopped it before runTUI returned.
				if processAlive(pid) {
					t.Fatalf("runTUI returned with the host %d the TUI never took still there (%s)", pid, procState(pid))
				}
				assertNoHosts(t, env)
				return
			}
			e := onlyHost(t, env)
			if e.HostID != spawned.Host.ID || e.CrazeSessionID == "" {
				t.Fatalf("after the quit %+v, want Spawn's host %s still serving", e, spawned.Host.ID)
			}
			stopEntry(t, e, pid)
		})
	}
}

// TestAFailedOpenStopsNoSessionAnotherOpened (sol r20-c9r 1), a forced
// schedule: two Opens of the host Spawn started, the first's dial held until
// the second has attached, and then failed. The first's failure stops
// nothing — the host is the second Open's session, which then comes up — and
// the quit leaves it running. A failed open stops the host only when nothing
// else holds it (the "opened, unreachable" case above).
func TestAFailedOpenStopsNoSessionAnotherOpened(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var dials atomic.Int32
	prev := spawnDial
	spawnDial = func(ctx context.Context, socket string, opts remote.SessionOptions) (*remote.Session, error) {
		if dials.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("the test's first dial fails")
		}
		return prev(ctx, socket, opts)
	}
	t.Cleanup(func() { spawnDial = prev })
	var spawned roster.Ref
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		ref, err := cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: ws, Provider: cfg.Provider})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		spawned = ref
		pid := cmds.pids()[0]
		first := make(chan error, 1)
		go func() {
			_, err := cfg.Sessions.Open(ref)
			first <- err
		}()
		select {
		case <-entered:
		case <-time.After(serveStep):
			t.Fatal("the first Open never dialled")
		}
		var b backend.Backend
		if err := withinStep(t, "the second Open", func() (err error) {
			b, err = cfg.Sessions.Open(ref)
			return err
		}); err != nil {
			t.Fatalf("the second Open, the first's dial held: %v", err)
		}
		once.Do(func() { close(release) })
		select {
		case err := <-first:
			if err == nil {
				t.Fatal("the first Open answered with its dial failing")
			}
		case <-time.After(serveStep):
			t.Fatal("the first Open never answered")
		}
		// The first Open has answered: had its failure stopped the host, it
		// would have done so before answering.
		if !processAlive(pid) {
			t.Fatalf("the first Open's failure stopped the host %d the second Open attached to", pid)
		}
		started(t, b)
		_ = b.Close()
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	e := onlyHost(t, env)
	if e.HostID != spawned.Host.ID {
		t.Fatalf("after the quit %+v, want Spawn's host %s still serving", e, spawned.Host.ID)
	}
	stopEntry(t, e, cmds.pids()[0])
}

// TestAFailedOpenStopsAHostItsOpenersLetGo (sol r21-c10r 1), a forced
// schedule: the host the list's Spawn started is opened, and that backend is
// closed — the TUI let it go: a switch away from it, a dial it no longer
// wanted (plan 030 §3.11) — and then an Open of it fails (every dial after
// the first does). Closed before its session came up in the TUI, nothing
// holds the host any more, and the failed Open stops it before answering, as
// one with no opener ever does. Closed after it came up, the host is the
// user's — a session came up on it — and the failed Open leaves it, and so
// does the quit. Every call is bounded by a step.
func TestAFailedOpenStopsAHostItsOpenersLetGo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cameUp  bool
		stopped bool
	}{
		{name: "closed before it came up", stopped: true},
		{name: "closed after it came up", cameUp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			var dials atomic.Int32
			prev := spawnDial
			spawnDial = func(ctx context.Context, socket string, opts remote.SessionOptions) (*remote.Session, error) {
				if dials.Add(1) > 1 {
					return nil, errors.New("the test's later dials fail")
				}
				return prev(ctx, socket, opts)
			}
			t.Cleanup(func() { spawnDial = prev })
			var spawned roster.Ref
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				ref, err := cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: ws, Provider: cfg.Provider})
				if err != nil {
					t.Fatalf("Spawn: %v", err)
				}
				spawned = ref
				pid := cmds.pids()[0]
				var b backend.Backend
				if err := withinStep(t, "the first Open", func() (err error) {
					b, err = cfg.Sessions.Open(ref)
					return err
				}); err != nil {
					t.Fatalf("the first Open: %v", err)
				}
				if tc.cameUp {
					started(t, b)
				}
				if err := withinStep(t, "the close", b.Close); err != nil {
					t.Fatalf("the close: %v", err)
				}
				if err := withinStep(t, "the failing Open", func() error {
					_, err := cfg.Sessions.Open(ref)
					return err
				}); err == nil {
					t.Fatal("the second Open answered with its dial failing")
				}
				// No wait: an Open that stops the host does so before it answers.
				if alive := processAlive(pid); alive == tc.stopped {
					t.Fatalf("after the failed Open the host %d is alive %v, want %v (%s)", pid, alive, !tc.stopped, procState(pid))
				}
				return tui.Result{}, nil
			})
			if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if tc.stopped {
				assertNoHosts(t, env)
				return
			}
			e := onlyHost(t, env)
			if e.HostID != spawned.Host.ID {
				t.Fatalf("after the quit %+v, want Spawn's host %s still serving", e, spawned.Host.ID)
			}
			stopEntry(t, e, cmds.pids()[0])
		})
	}
}

// TestLeaveRunningAfterTheQuitSaysSo (sol r20-c9r 2): keep-or-stop is one
// decision under the launcher's lock. A LeaveRunning that comes once the
// quit has decided — finish has stopped the host Spawn started, which
// nobody took — reports that it came too late, never a nil answer for a host
// that is gone; one before it keeps the host (the "left running" cases
// above).
func TestLeaveRunningAfterTheQuitSaysSo(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	var (
		list    tui.Sessions
		spawned roster.Ref
	)
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		ref, err := cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: ws, Provider: cfg.Provider})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		list, spawned = cfg.Sessions, ref
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if err := list.LeaveRunning(spawned); !errors.Is(err, errLaunchOver) {
		t.Fatalf("LeaveRunning after the quit: %v, want errLaunchOver", err)
	}
	if pid := cmds.pids()[0]; processAlive(pid) {
		t.Fatalf("the host %d the TUI never took is still there (%s)", pid, procState(pid))
	}
	assertNoHosts(t, env)
}

// TestSIGTERMWithTheSessionListOpenClosesItsRoster (sol r19-c10 3): the real
// TUI in a pty, its session list open — the roster polling the launch's own
// host, which it has answered — when SIGTERM quits the program. The list
// never saw the quit, so finishRun closes its roster: once runTUI has
// returned no goroutine of the roster is left, and the host's journal closes
// every connection it opened (the roster's among them) while the host goes
// on serving, as a view close leaves it.
func TestSIGTERMWithTheSessionListOpenClosesItsRoster(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_JOURNAL", "1")
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runInPTY(t, launchFlags(t, ws))
	r.see(t, " craze ─")
	e := waitServingEntry(t, env, true, func() error { return nil }, &lockedBuffer{})
	r.see(t, statusElapsed)
	// ← on the empty composer: the list opens, and the host's answer — its
	// idle row — is on it.
	if _, err := r.ptmx.Write([]byte("\x1b[D")); err != nil {
		t.Fatal(err)
	}
	r.see(t, " sessions  1 running")
	r.see(t, "waiting for a prompt")
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("runTUI after SIGTERM: %v", err)
		}
	case <-time.After(serveStep):
		t.Fatal("craze did not quit after SIGTERM")
	}
	waitNoRosterGoroutines(t)
	waitJournalConnsClosed(t)
	if got := onlyHost(t, env); got.HostID != e.HostID {
		t.Fatalf("after SIGTERM to the TUI: %+v, want the host %s still serving", got, e.HostID)
	}
	stopEntry(t, e, cmds.pids()[0])
}

// waitNoRosterGoroutines waits a step for no goroutine of this process to be
// running the roster's code: Close joins them all, but the last of them is
// still returning when Close does.
func waitNoRosterGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		var left []string
		for _, g := range strings.Split(allStacks(), "\n\n") {
			if strings.Contains(g, "/internal/roster.") {
				left = append(left, g)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d roster goroutines left after %v:\n%s", len(left), serveStep, strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// allStacks is every goroutine's stack, however long.
func allStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitJournalConnsClosed waits a step for the host's journal — the one
// session's, under CRAZE_HOME — to have closed every control connection it
// opened, at least two of them (the TUI's own and the roster's).
func waitJournalConnsClosed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		opened, open := journalConns(t)
		if opened >= 2 && len(open) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %v the host's journal has %d connections opened, still open: %v", serveStep, opened, open)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// journalConns reads the host's control_conn notes: how many connections it
// opened, and the ids of those it has not closed.
func journalConns(t *testing.T) (opened int, open []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.Getenv("CRAZE_HOME"), "journal", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			var n struct {
				Type   string         `json:"type"`
				Kind   string         `json:"kind"`
				Fields map[string]any `json:"fields"`
			}
			if json.Unmarshal([]byte(line), &n) != nil || n.Type != "diag" || n.Kind != journal.DiagControlConn {
				continue
			}
			id := fmt.Sprint(n.Fields["conn"])
			switch n.Fields["event"] {
			case "open":
				opened++
				live[id] = true
			case "close":
				delete(live, id)
			}
		}
	}
	for id := range live {
		open = append(open, id)
	}
	return opened, open
}
