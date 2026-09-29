package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/version"
)

// Spawning a detached host (plan 030 C3, §3.4, §3.18 PR 1): spawnHost starting
// real hosts — this test binary re-executed as craze serve (serve_child_test.go)
// with the fake agent — and every way the handshake can end: ok, held (with the
// rendezvous: a holder slow to publish, a holder that crashes), an early exit,
// a malformed or oversized line, a timeout, a cancelled caller, and a dial that
// fails after ok; the reaping of every child; the recorded agent groups killed
// with an agent that survives its host; the ready descriptor kept from the
// agent; a start failure reaching the client; legacy loads; --no-host-status.
//
// Every wait is bounded on its own (serveStep), and the schedules are forced:
// a timeout fires when the test says (spawnReadyTimer), a holder is held
// where it is claimed and not yet in the registry (cliChildGate), and the
// rendezvous is acted on while it is known to be polling (spawnPolled).

// childCmds is every host command a test built (spawnAsChild).
type childCmds struct {
	n    atomic.Int32
	mu   sync.Mutex
	cmds []*exec.Cmd
}

// count is how many hosts were spawned.
func (c *childCmds) count() int { return int(c.n.Load()) }

// pids is every spawned host's pid. Read only once every spawn that started
// them has returned to the test (a channel receive orders it after Start).
func (c *childCmds) pids() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []int
	for _, cmd := range c.cmds {
		if cmd.Process != nil {
			out = append(out, cmd.Process.Pid)
		}
	}
	return out
}

// spawnAsChild makes every host spawned in this test this test binary run as
// craze (serve_child_test.go's init), in the test's environment plus extra(n)
// for the n-th spawn, counting from 1.
func spawnAsChild(t *testing.T, extra func(n int) []string) *childCmds {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &childCmds{}
	prev := hostCommand
	hostCommand = func(argv []string) (*exec.Cmd, error) {
		i := int(c.n.Add(1))
		b, err := json.Marshal(argv)
		if err != nil {
			return nil, err
		}
		// -test.run matches nothing, so a child whose init fell through runs
		// no test.
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), cliChildEnv+"="+string(b))
		if extra != nil {
			cmd.Env = append(cmd.Env, extra(i)...)
		}
		c.mu.Lock()
		c.cmds = append(c.cmds, cmd)
		c.mu.Unlock()
		return cmd, nil
	}
	t.Cleanup(func() { hostCommand = prev })
	return c
}

// spawnBounds sets the handshake's bounds for one test: the ready wait, the
// rendezvous and the termination grace. The spawn tests run starved too, so
// a test that is not about a bound sets them all to serveStep.
func spawnBounds(t *testing.T, ready, rendezvous, grace time.Duration) {
	t.Helper()
	r, v, g := spawnReadyWait, spawnRendezvousWait, spawnTermGrace
	spawnReadyWait, spawnRendezvousWait, spawnTermGrace = ready, rendezvous, grace
	t.Cleanup(func() { spawnReadyWait, spawnRendezvousWait, spawnTermGrace = r, v, g })
}

// spawnFor is a new session's spawn in ws with the fake agent.
func spawnFor(t *testing.T, env rundir.Env, ws string) spawnOptions {
	t.Helper()
	return spawnOptions{env: env, flags: tuiFlags{force: true, agentBin: fakeAgentPath(t), workspace: ws}}
}

type spawnResult struct {
	ref hostRef
	err error
}

// goSpawn runs spawnHost on a goroutine of its own. The host it answers, if
// it started one, is ended when the test ends, once the spawn has returned.
func goSpawn(t *testing.T, ctx context.Context, opts spawnOptions) <-chan spawnResult {
	t.Helper()
	out, kept := make(chan spawnResult, 1), make(chan spawnResult, 1)
	go func() {
		ref, err := spawnHost(ctx, opts)
		out <- spawnResult{ref, err}
		kept <- spawnResult{ref, err}
	}()
	t.Cleanup(func() {
		select {
		case r := <-kept:
			endHost(r.ref)
		case <-time.After(serveStep):
			t.Errorf("a spawn had not returned when the test ended")
		}
	})
	return out
}

// spawned is goSpawn's answer, within serveStep.
func spawned(t *testing.T, ch <-chan spawnResult) spawnResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(serveStep):
		t.Fatalf("spawnHost has not returned after %v", serveStep)
		return spawnResult{}
	}
}

// spawnNow is spawnHost, bounded, its host ended when the test ends.
func spawnNow(t *testing.T, opts spawnOptions) (hostRef, error) {
	t.Helper()
	r := spawned(t, goSpawn(t, context.Background(), opts))
	return r.ref, r.err
}

// endHost ends a host a spawn started, if it is still there.
func endHost(ref hostRef) {
	if ref.child != nil {
		ref.child.terminate()
	}
}

// failure is err's spawnError kind, failing the test for any other error.
func failure(t *testing.T, err error) *spawnError {
	t.Helper()
	var se *spawnError
	if !errors.As(err, &se) {
		t.Fatalf("spawnHost: %v (%T), want a *spawnError", err, err)
	}
	return se
}

// waitReaped: pid no longer names a process — exited and waited for, so not a
// zombie either (kill(pid, 0) still finds a zombie) — within serveStep.
func waitReaped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still there after %v (%s)", pid, serveStep, procState(pid))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGroupGone: nothing is left in process group pgid, within serveStep.
func waitGroupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still has members after %v", pgid, serveStep)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// procState is pid's state letter from /proc on Linux (Z for a zombie), for a
// failure's message; "?" elsewhere or when it cannot be read.
func procState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "?"
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return "?"
	}
	return s[i+2 : i+3]
}

// waitNoZombies: no child of this process is a zombie, within serveStep — a
// child that has just exited is one until its reaper runs, so only one that
// stays one fails.
func waitNoZombies(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		z := zombieChildren()
		if len(z) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("zombie children after %v: %v", serveStep, z)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// zombieChildren is every child of this process that is a zombie, from /proc
// (Linux; nil elsewhere, where waitReaped's kill(pid, 0) is the check).
func zombieChildren() []int {
	if runtime.GOOS != "linux" {
		return nil
	}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	var out []int
	for _, p := range stats {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 || f[0] != "Z" {
			continue
		}
		if ppid, _ := strconv.Atoi(f[1]); ppid == os.Getpid() {
			pid, _ := strconv.Atoi(strings.Fields(s)[0])
			out = append(out, pid)
		}
	}
	return out
}

// waitLog waits for path to hold want, within serveStep.
func waitLog(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the host's log never said %q: %s", want, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordedGroups is the agent process groups a host recorded, once it has
// recorded one, within serveStep.
func recordedGroups(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		b, _ := os.ReadFile(path)
		var pgids []int
		for _, f := range strings.Fields(string(b)) {
			if n, err := strconv.Atoi(f); err == nil {
				pgids = append(pgids, n)
			}
		}
		if len(pgids) > 0 {
			return pgids
		}
		if time.Now().After(deadline) {
			t.Fatalf("no agent process group recorded in %s after %v", path, serveStep)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dialRef dials the host ref names as craze attach would, through dialHost.
func dialRef(t *testing.T, ref hostRef) *remote.Session {
	t.Helper()
	s, err := dialHost(stepCtx(t), ref, remote.SessionOptions{
		Client:    remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())},
		SessionID: ref.entry.CrazeSessionID,
		Provider:  ref.entry.Provider,
		Workspace: ref.entry.Workspace,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", ref.entry.Socket, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// onlyHost is the one live host in env once there is one with a session,
// within serveStep.
func onlyHost(t *testing.T, env rundir.Env) rundir.Entry {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		entries, err := rundir.Hosts(env)
		if err == nil && len(entries) == 1 && entries[0].CrazeSessionID != "" {
			return entries[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no one serving host after %v: %+v, %v", serveStep, entries, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSpawnHostServesASession is the handshake's ok: a host in a session and
// process group of its own (setsid), its log named for its id, answering its
// id, socket, session and craze version — and already in the registry under
// that session when the line is read, provider and workspace included. Its
// agent's process group is recorded beside its log. A client dials it and
// attaches; session.stop then ends it: exit 0, reaped, its agent's group
// gone, its record removed, nothing in the registry, its log kept.
func TestSpawnHostServesASession(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	ref, err := spawnNow(t, spawnFor(t, env, ws))
	if err != nil {
		t.Fatalf("spawnHost: %v", err)
	}
	c := ref.child
	switch {
	case ref.held || c == nil:
		t.Fatalf("a new session's spawn answered %+v", ref)
	case ref.version != version.Version:
		t.Fatalf("the host is craze %q, want %q", ref.version, version.Version)
	case ref.entry.PID != c.pid || ref.entry.Provider != "cursor" || ref.entry.Workspace != absDir(ws) || ref.entry.CrazeSessionID == "":
		t.Fatalf("the ref's entry %+v (pid %d): the registry had not the session's identity when ok came", ref.entry, c.pid)
	case filepath.Base(ref.log) != ref.entry.HostID+".log" || filepath.Base(c.groups) != ref.entry.HostID+".pgids":
		t.Fatalf("the host's files: %s, %s", ref.log, c.groups)
	}
	if pgid, err := syscall.Getpgid(c.pid); err != nil || pgid != c.pid {
		t.Fatalf("the host's process group %d (%v), want its own, %d", pgid, err, c.pid)
	}
	agents := recordedGroups(t, c.groups)
	s := dialRef(t, ref)
	if err := s.Attach(stepCtx(t)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	stopOver(t, s, "1")
	select {
	case <-c.done:
	case <-time.After(serveStep):
		t.Fatalf("the host has not exited after session.stop")
	}
	if c.err != nil {
		t.Fatalf("the host exited %v, want 0", c.err)
	}
	waitReaped(t, c.pid)
	for _, pgid := range agents {
		waitGroupGone(t, pgid)
	}
	if fileExists(c.groups) {
		t.Fatal("a host that stopped cleanly left its agents' record")
	}
	assertHostGone(t, env, ref.entry)
	waitLog(t, ref.log, "craze serve: stopping: session.stop from client")
}

// TestSpawnHostEndsAHostWithNoAnswer is every handshake that gives no usable
// line: the host exiting first (a panic once it serves: EOF — at once, not a
// timeout, its exit status and log named), a line that is not JSON, one
// longer than 64 KiB, no line within the ready wait (the timer fired by the
// test once the host serves), and the caller's context ending. Each is its own
// failure; every host the spawner ended was sent SIGTERM and ran its stop
// sequence — its log says so — and every child is reaped, and nothing is left
// in the registry.
func TestSpawnHostEndsAHostWithNoAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []string
		failure spawnFailure
		// trigger ends a spawn that waits: the timer, or the context.
		trigger string
		want    string
	}{
		{name: "early exit", env: []string{cliChildPanic + "=craze test: a forced panic"}, failure: spawnExited, want: "panic: craze test: a forced panic"},
		{name: "malformed", env: []string{cliChildReady + "=malformed"}, failure: spawnMalformed, want: "craze serve: stopping: SIGTERM"},
		{name: "oversized", env: []string{cliChildReady + "=oversized"}, failure: spawnOversized, want: "craze serve: stopping: SIGTERM"},
		{name: "timeout", env: []string{cliChildReady + "=skip"}, failure: spawnTimedOut, trigger: "timer", want: "craze serve: stopping: SIGTERM"},
		{name: "cancelled", env: []string{cliChildReady + "=skip"}, failure: spawnCancelled, trigger: "context", want: "craze serve: stopping: SIGTERM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			cmds := spawnAsChild(t, func(int) []string { return tc.env })
			spawnBounds(t, serveStep, serveStep, serveStep)
			// The ready wait never ends on its own here: an early exit that
			// were read as a timeout would hang this test's step, not pass it.
			fire := make(chan time.Time)
			prevTimer := spawnReadyTimer
			spawnReadyTimer = func(time.Duration) <-chan time.Time { return fire }
			t.Cleanup(func() { spawnReadyTimer = prevTimer })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := goSpawn(t, ctx, spawnFor(t, env, ws))
			if tc.trigger != "" {
				// Serving, so the host acts on SIGTERM; then the wait ends.
				onlyHost(t, env)
				if tc.trigger == "timer" {
					close(fire)
				} else {
					cancel()
				}
			}
			res := spawned(t, ch)
			se := failure(t, res.err)
			if se.kind != tc.failure {
				t.Fatalf("failure %d, want %d: %v", se.kind, tc.failure, se)
			}
			if tc.failure == spawnCancelled && !errors.Is(res.err, context.Canceled) {
				t.Fatalf("a cancelled spawn: %v, want context.Canceled", res.err)
			}
			if res.ref.child != nil {
				t.Fatalf("a failed spawn answered a host: %+v", res.ref)
			}
			logs, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "host-logs", "*.log"))
			if len(logs) != 1 || !strings.Contains(se.msg, "; its log: "+logs[0]) {
				t.Fatalf("the failure %q; the logs %q", se.msg, logs)
			}
			if tc.failure == spawnExited && !strings.Contains(se.msg, "exit status 2") {
				t.Fatalf("an early exit's failure does not say how it exited: %q", se.msg)
			}
			waitLog(t, logs[0], tc.want)
			if entries, _ := rundir.Hosts(env); len(entries) != 0 {
				t.Fatalf("the ended host is still listed: %+v", entries)
			}
			pids := cmds.pids()
			if len(pids) != 1 {
				t.Fatalf("%d hosts spawned, want 1", len(pids))
			}
			waitReaped(t, pids[0])
			waitNoZombies(t)
		})
	}
}

// TestSpawnHostEndsAHostItCannotDial: a host that answered ok whose dial then
// fails is not left running unreached — session.stop over a connection of the
// spawner's own when its socket answers (the host's log names a client's
// stop), and when it does not (its socket gone), SIGTERM. Either way the host
// exits and is reaped, and nothing is left in the registry.
func TestSpawnHostEndsAHostItCannotDial(t *testing.T) {
	for _, reachable := range []bool{true, false} {
		name := "stop sent"
		if !reachable {
			name = "terminated"
		}
		t.Run(name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			spawnAsChild(t, nil)
			spawnBounds(t, serveStep, serveStep, serveStep)
			ref, err := spawnNow(t, spawnFor(t, env, ws))
			if err != nil {
				t.Fatalf("spawnHost: %v", err)
			}
			prev := spawnDial
			spawnDial = func(ctx context.Context, path string, opts remote.SessionOptions) (*remote.Session, error) {
				if !reachable {
					if err := os.Remove(path); err != nil {
						return nil, err
					}
				}
				return nil, errors.New("craze test: the dial failed")
			}
			t.Cleanup(func() { spawnDial = prev })
			if _, err := dialHost(stepCtx(t), ref, remote.SessionOptions{SessionID: ref.entry.CrazeSessionID}); err == nil {
				t.Fatal("the failed dial answered a session")
			}
			if !ref.child.exited() {
				t.Fatal("dialHost returned with the host it could not dial still running")
			}
			waitReaped(t, ref.child.pid)
			want := "craze serve: stopping: session.stop from client"
			if !reachable {
				want = "craze serve: stopping: SIGTERM"
			}
			waitLog(t, ref.log, want)
			if entries, _ := rundir.Hosts(env); len(entries) != 0 {
				t.Fatalf("the host is still listed: %+v", entries)
			}
		})
	}
}

// gateFIFO is a FIFO for cliChildGate; release writes its byte — once,
// bounded, waiting for the child to read it — and letGo, for a test's
// cleanup, lets a child still held there go without waiting for one that
// never came or was killed.
func gateFIFO(t *testing.T) (path string, release, letGo func()) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	letGo = func() {
		// ENXIO: nobody has it open to read.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_, _ = f.Write([]byte{1})
			_ = f.Close()
		}
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			done := make(chan error, 1)
			go func() {
				// Blocks until the child has it open to read.
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err == nil {
					_, err = f.Write([]byte{1})
					_ = f.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("release the gate: %v", err)
				}
			case <-time.After(serveStep):
				t.Errorf("nobody read the gate within %v", serveStep)
			}
		})
	}
	return path, release, letGo
}

// waitClaimed is the holder its lock names once a session is claimed —
// "<pid> <hostId>" — within serveStep.
func waitClaimed(t *testing.T, env rundir.Env, crazeID string) (pid int, hostID string) {
	t.Helper()
	p := filepath.Join(env.Home, ".cache", "craze", "locks", crazeID+".lock")
	deadline := time.Now().Add(serveStep)
	for {
		b, _ := os.ReadFile(p)
		if f := strings.Fields(string(b)); len(f) == 2 {
			if n, err := strconv.Atoi(f[0]); err == nil && n > 0 {
				return n, f[1]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not claimed after %v", crazeID, serveStep)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pollSignal makes spawnPolled close the channel it answers on the
// rendezvous's first read of the registry.
func pollSignal(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	prev := spawnPolled
	spawnPolled = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(func() { spawnPolled = prev })
	return ch
}

// TestSpawnHostRendezvousesWithAHeldSession is the held answer (plan 030
// §3.4, R2-3), forced both ways. A first spawn loads the session — a load
// the agent never answers, so it is never ready — and is held where it has
// claimed it and is not yet in the registry; a second spawn of the same
// session is answered held and waits in the rendezvous:
//   - slow to publish: once the rendezvous is known to be polling, the holder
//     is let go; it binds and publishes its identity, the first spawn's
//     answer is ok, and the second's is the holder — held, no host of its
//     own, found through the registry by its identity, never its ready flag;
//   - crashing: once the rendezvous is polling, the holder is killed; its
//     spawn answers an early exit, and the second spawn is made once more,
//     claims the session its holder left, and answers a host of its own.
func TestSpawnHostRendezvousesWithAHeldSession(t *testing.T) {
	for _, crash := range []bool{false, true} {
		name := "holder slow to publish"
		if crash {
			name = "holder crashes"
		}
		t.Run(name, func(t *testing.T) {
			env, ws := serveHome(t)
			// A load that is never answered: the holder is never ready, so a
			// rendezvous that waited for the registry's ready flag rather
			// than the session's identity would never find it.
			t.Setenv("CRAZE_FAKE_SCRIPT", "load-hang")
			const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000c1"
			seedIndexRow(t, sessions.Row{SessionID: "held-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id})
			gate, release, letGo := gateFIFO(t)
			spawns := spawnAsChild(t, func(n int) []string {
				if n == 1 {
					return []string{cliChildGate + "=" + gate}
				}
				return nil
			})
			spawnBounds(t, serveStep, serveStep, serveStep)
			polled := pollSignal(t)
			opts := spawnOptions{env: env, flags: tuiFlags{force: true, agentBin: fakeAgentPath(t)}, load: id}
			first := goSpawn(t, context.Background(), opts)
			// After goSpawn's cleanup, so it runs first: a failing test lets
			// the holder go before waiting for its spawn.
			t.Cleanup(letGo)
			holderPID, holderID := waitClaimed(t, env, id)
			second := goSpawn(t, context.Background(), opts)
			select {
			case <-polled:
			case <-time.After(serveStep):
				t.Fatal("the second spawn never polled the registry")
			}
			if _, ok := hostEntry(env, holderID); ok {
				t.Fatal("the holder is in the registry while it is held before its bind")
			}

			if !crash {
				release()
				a, b := spawned(t, first), spawned(t, second)
				if a.err != nil || b.err != nil {
					t.Fatalf("the holder's spawn: %v; the second: %v", a.err, b.err)
				}
				switch {
				case a.ref.held || a.ref.entry.HostID != holderID:
					t.Fatalf("the holder's spawn answered %+v", a.ref)
				case !b.ref.held || b.ref.child != nil || b.ref.entry.HostID != holderID || b.ref.entry.CrazeSessionID != id || b.ref.entry.Socket != a.ref.entry.Socket || b.ref.entry.Ready:
					t.Fatalf("the held spawn answered %+v", b.ref)
				case spawns.count() != 2:
					t.Fatalf("%d hosts spawned, want 2", spawns.count())
				}
				return
			}

			if err := syscall.Kill(holderPID, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			a, b := spawned(t, first), spawned(t, second)
			if se := failure(t, a.err); se.kind != spawnExited {
				t.Fatalf("the killed holder's spawn: %v", a.err)
			}
			if b.err != nil {
				t.Fatalf("the second spawn, after its holder died: %v", b.err)
			}
			switch {
			case b.ref.held || b.ref.child == nil || b.ref.entry.HostID == holderID || b.ref.entry.CrazeSessionID != id:
				t.Fatalf("the second spawn answered %+v", b.ref)
			case spawns.count() != 3:
				t.Fatalf("%d hosts spawned, want 3: the holder, the held one, the retry", spawns.count())
			}
			if got, _ := waitClaimed(t, env, id); got != b.ref.child.pid {
				t.Fatalf("the session is claimed by pid %d, not the retry's host %d", got, b.ref.child.pid)
			}
		})
	}
}

// TestSpawnHostLegacyLoadsTwoAtOnce (R2-6): a legacy row — no craze id — is
// loaded by its key, <provider>:<sessionId> (loadArg), and two spawns of it at
// once end as one host: whichever claims it first serves it under the craze id
// it gave the row, and the other is answered held and meets it through the
// registry. Every interleaving ends so — the host that claims holds the claim
// for its life — so nothing is forced; the race inside the host (both loaders
// reading the row with no id) is TestServeTwoLegacyLoadsAtOnceOneWins's.
func TestSpawnHostLegacyLoadsTwoAtOnce(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	row := sessions.Row{SessionID: "legacy-9", Provider: "cursor", CWD: absDir(ws)}
	seedIndexRow(t, row)
	if got := loadArg(row); got != "cursor:legacy-9" {
		t.Fatalf("loadArg of a legacy row: %q", got)
	}
	if got := loadArg(sessions.Row{SessionID: "s", Provider: "grok", CrazeID: "0199aaaa-bbbb-7ccc-8ddd-0000000000c2"}); got != "0199aaaa-bbbb-7ccc-8ddd-0000000000c2" {
		t.Fatalf("loadArg of a row with its craze id: %q", got)
	}
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	opts := spawnOptions{env: env, flags: tuiFlags{force: true, agentBin: fakeAgentPath(t)}, load: loadArg(row)}
	a, b := goSpawn(t, context.Background(), opts), goSpawn(t, context.Background(), opts)
	ra, rb := spawned(t, a), spawned(t, b)
	if ra.err != nil || rb.err != nil {
		t.Fatalf("the spawns: %v; %v", ra.err, rb.err)
	}
	host, held := ra.ref, rb.ref
	if host.held {
		host, held = held, host
	}
	stored, ok, err := (&sessions.Store{}).Find("cursor", "legacy-9")
	switch {
	case err != nil || !ok || stored.CrazeID == "":
		t.Fatalf("the row: %+v, %v, %v", stored, ok, err)
	case host.held || host.child == nil || host.entry.CrazeSessionID != stored.CrazeID:
		t.Fatalf("the serving spawn answered %+v; the row's id %s", host, stored.CrazeID)
	case !held.held || held.child != nil || held.entry.HostID != host.entry.HostID || held.entry.CrazeSessionID != stored.CrazeID:
		t.Fatalf("the held spawn answered %+v; the host %s", held, host.entry.HostID)
	}
}

// TestSpawnHostReapsEveryChild: twenty spawns while the launcher lives — hosts
// that refuse and exit of their own accord, and hosts that answer ok and are
// then stopped by a client — leave no zombie: setsid does not reparent, and
// one goroutine waits for each. (A host sits in its own session, so nothing
// else would.)
func TestSpawnHostReapsEveryChild(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	cmds := spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	for i := range 20 {
		opts := spawnFor(t, env, ws)
		if i%5 != 0 {
			opts.load = fmt.Sprintf("0199aaaa-bbbb-7ccc-8ddd-%012d", 900+i)
		}
		ref, err := spawnNow(t, opts)
		if i%5 != 0 {
			if se := failure(t, err); se.kind != spawnNotReady || !strings.HasPrefix(se.msg, "craze serve: no session ") {
				t.Fatalf("spawn %d: %v", i, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		if err := stopHost(ref.entry); err != nil {
			t.Fatalf("stop host %d: %v", i, err)
		}
	}
	if n := len(cmds.pids()); n != 20 {
		t.Fatalf("%d hosts spawned, want 20", n)
	}
	for _, pid := range cmds.pids() {
		waitReaped(t, pid)
	}
	waitNoZombies(t)
	if entries, _ := rundir.Hosts(env); len(entries) != 0 {
		t.Fatalf("hosts left: %+v", entries)
	}
}

// TestSpawnHostKillsTheAgentsItsHostLeaves (R2-2): an agent that outlives its
// host — the fake ignoring SIGTERM, SIGHUP and its pipes closing — under a
// host that never answers and never acts on SIGTERM (its goroutine parked
// where the line is due). The caller gives up once the agent's group is
// recorded; the spawner's SIGTERM goes unheeded, the grace runs out, the
// host's group is killed, and then the agent's group from the host's record:
// nothing survives, and the record is removed.
func TestSpawnHostKillsTheAgentsItsHostLeaves(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv("CRAZE_FAKE_STUBBORN", "1")
	spawnAsChild(t, func(int) []string { return []string{cliChildReady + "=block"} })
	spawnBounds(t, serveStep, serveStep, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := goSpawn(t, ctx, spawnFor(t, env, ws))
	e := onlyHost(t, env)
	record := filepath.Join(env.Home, ".cache", "craze", "host-logs", agentGroupsName(e.HostID))
	agents := recordedGroups(t, record)
	for _, pgid := range agents {
		if err := syscall.Kill(-pgid, 0); err != nil {
			t.Fatalf("the agent's group %d before the kill: %v", pgid, err)
		}
	}
	cancel()
	res := spawned(t, ch)
	if se := failure(t, res.err); se.kind != spawnCancelled {
		t.Fatalf("spawnHost: %v", res.err)
	}
	waitReaped(t, e.PID)
	waitGroupGone(t, e.PID)
	for _, pgid := range agents {
		waitGroupGone(t, pgid)
	}
	if fileExists(record) {
		t.Fatal("the spawner left the host's agents' record")
	}
	waitLog(t, filepath.Join(env.Home, ".cache", "craze", "host-logs", e.HostID+".log"), "craze serve: host "+e.HostID)
}

// TestSpawnHostKeepsTheReadyPipeFromTheAgent (Linux: /proc): the host marks
// its fd 3 close-on-exec the moment it starts, so its agent — spawned while
// the host still holds the pipe, which it does here for good (a host that
// never answers) — holds no descriptor of it; and neither of the spawner's
// marks, CRAZE_READY_FD and CRAZE_HOST_CHILD, is in the agent's environment.
// A host that answers closes the pipe as it does.
func TestSpawnHostKeepsTheReadyPipeFromTheAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	spawnAsChild(t, func(n int) []string {
		if n == 1 {
			return []string{cliChildReady + "=skip"}
		}
		return nil
	})
	spawnBounds(t, serveStep, serveStep, serveStep)
	var mu sync.Mutex
	var inodes []uint64
	prev := spawnPipe
	spawnPipe = func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return r, w, err
		}
		// Through SyscallConn, never Fd, which would put the read end in
		// blocking mode and take it off the poller the spawner's timeout and
		// cancel close it through.
		if rc, err := r.SyscallConn(); err == nil {
			_ = rc.Control(func(fd uintptr) {
				var st syscall.Stat_t
				if syscall.Fstat(int(fd), &st) == nil {
					mu.Lock()
					inodes = append(inodes, st.Ino)
					mu.Unlock()
				}
			})
		}
		return r, w, nil
	}
	t.Cleanup(func() { spawnPipe = prev })
	pipeOf := func(n int) string {
		mu.Lock()
		defer mu.Unlock()
		if len(inodes) <= n {
			t.Fatalf("the ready pipe of spawn %d was not read", n+1)
		}
		return fmt.Sprintf("pipe:[%d]", inodes[n])
	}
	holds := func(pid int, pipe string) bool {
		fds, err := filepath.Glob(fmt.Sprintf("/proc/%d/fd/*", pid))
		if err != nil || len(fds) == 0 {
			t.Fatalf("the descriptors of %d: %v", pid, err)
		}
		for _, fd := range fds {
			if link, _ := os.Readlink(fd); link == pipe {
				return true
			}
		}
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := goSpawn(t, ctx, spawnFor(t, env, ws))
	e := onlyHost(t, env)
	agents := recordedGroups(t, filepath.Join(env.Home, ".cache", "craze", "host-logs", agentGroupsName(e.HostID)))
	pipe := pipeOf(0)
	if !holds(e.PID, pipe) {
		t.Fatalf("the unanswering host does not hold its ready pipe %s", pipe)
	}
	for _, pid := range agents {
		if holds(pid, pipe) {
			t.Fatalf("the agent %d holds the host's ready pipe", pid)
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			t.Fatal(err)
		}
		for _, kv := range strings.Split(string(b), "\x00") {
			if strings.HasPrefix(kv, readyFDEnv+"=") || strings.HasPrefix(kv, hostChildEnv+"=") {
				t.Fatalf("the agent's environment holds %s", kv)
			}
		}
	}
	cancel()
	if se := failure(t, spawned(t, ch).err); se.kind != spawnCancelled {
		t.Fatalf("the unanswering host's spawn: %v", se)
	}

	ref, err := spawnNow(t, spawnFor(t, env, ws))
	if err != nil {
		t.Fatalf("spawnHost: %v", err)
	}
	if holds(ref.child.pid, pipeOf(1)) {
		t.Fatal("a host that answered still holds its ready pipe")
	}
}

// TestSpawnHostStartFailureReachesTheClient (R2-3): a session whose start
// fails before anyone dials — the agent refusing its auth — still answers ok
// (the handshake does not wait for the provider), and the client that dials
// afterwards is told the start failed, the agent's own words, over the
// socket: the host kept it (plan 030 §3.6's start-failed rule is C5's).
func TestSpawnHostStartFailureReachesTheClient(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "authfail")
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	ref, err := spawnNow(t, spawnFor(t, env, ws))
	if err != nil {
		t.Fatalf("spawnHost: %v", err)
	}
	// Failed before the dial, asked by a client that attaches nothing.
	c, err := remote.Dial(stepCtx(t), ref.entry.Socket, remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	deadline := time.Now().Add(serveStep)
	for {
		var st protocol.StateResult
		if err := c.Call(stepCtx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: ref.entry.CrazeSessionID}, &st); err != nil {
			t.Fatalf("session.state: %v", err)
		}
		if st.StartFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the start never failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = c.Close()
	s := dialRef(t, ref)
	var se *remote.StartError
	if err := s.Start(stepCtx(t)); !errors.As(err, &se) || se.Text == "" {
		t.Fatalf("the client's start: %v, want the host's start failure", err)
	}
	stopOver(t, s, "1")
	waitReaped(t, ref.child.pid)
}

// TestSpawnHostPassesNoHostStatus: the launching TUI's --no-host-status
// reaches its host (serveArgv), which leaves the agent's host hook gates in
// its environment as the in-process TUI would; without it the host strips
// them, whatever it reports itself (nothing).
func TestSpawnHostPassesNoHostStatus(t *testing.T) {
	for _, noHostStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-host-status %v", noHostStatus), func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "env")
			socks := shortRuntimeDir(t)
			for k, v := range map[string]string{
				"HERDR_ENV": "1", "HERDR_SOCKET_PATH": filepath.Join(socks, "h"), "HERDR_PANE_ID": "w9:p9",
				"ROOST_SOCKET": filepath.Join(socks, "r"), "ROOST_TAB_ID": "7", "ROOST_AGENT_HOOK": "/opt/roost/roostctl",
			} {
				t.Setenv(k, v)
			}
			spawnAsChild(t, nil)
			spawnBounds(t, serveStep, serveStep, serveStep)
			opts := spawnFor(t, env, ws)
			opts.flags.noHostStatus = noHostStatus
			ref, err := spawnNow(t, opts)
			if err != nil {
				t.Fatalf("spawnHost: %v", err)
			}
			// ok comes before the provider is ready; a prompt waits for it.
			waitServingEntry(t, env, true, func() error { return nil }, &lockedBuffer{})
			promptUnattached(t, ref.entry, "which gates")
			if lt := waitLastTurn(t, ref.entry); lt.Outcome != protocol.TurnDone {
				t.Fatalf("the turn ended %+v", lt)
			}
			snap, s := attachedTranscript(t, ref.entry)
			want := "envset: ROOST_TAB_ID HERDR_PANE_ID :end"
			if noHostStatus {
				want = "envset: ROOST_AGENT_HOOK ROOST_TAB_ID HERDR_ENV HERDR_PANE_ID :end"
			}
			if !strings.Contains(snap, want) {
				t.Fatalf("the agent's environment, want %q: %s", want, snap)
			}
			stopOver(t, s, "1")
			waitReaped(t, ref.child.pid)
		})
	}
}

// TestSpawnArgvCarriesEverySessionFlag is the spawner's option parity: every
// session flag a launching command line settled reaches the host's command
// line — parsed by craze serve's own flag set into the same values — with the
// host id and log the spawner chose, --load, and --no-host-status; and a flag
// the command line left unset is not passed, so the host resolves it as the
// launcher would. A value that looks like a flag stays a value.
func TestSpawnArgvCarriesEverySessionFlag(t *testing.T) {
	full := tuiFlags{
		workspace: "-ws", provider: "grok", model: "m-1", agentBin: "/bin/agent",
		pluginDirs: []string{"/p/one,two", "-p three"}, force: false, noForce: true, ask: true,
		cont: true, noHostStatus: true,
	}
	for _, tc := range []struct {
		name  string
		flags tuiFlags
		load  string
	}{
		{"every flag", full, ""},
		{"plan and a load", tuiFlags{force: true, plan: true}, "cursor:legacy-1"},
		{"nothing set", tuiFlags{force: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := serveArgv(&tc.flags, tc.load, "0123456789ab", "/h/0123456789ab.log")
			if argv[0] != "serve" {
				t.Fatalf("argv %q", argv)
			}
			cmd, f := parseServeFlags(t, argv[1:]...)
			if err := f.settle(); err != nil {
				t.Fatal(err)
			}
			want := tc.flags
			if err := want.settle(); err != nil {
				t.Fatal(err)
			}
			got := f.tuiFlags
			if got.workspace != want.workspace || got.provider != want.provider || got.model != want.model ||
				got.agentBin != want.agentBin || !slices.Equal(got.pluginDirs, want.pluginDirs) || got.force != want.force ||
				got.ask != want.ask || got.plan != want.plan || got.cont != want.cont || got.noHostStatus != want.noHostStatus {
				t.Fatalf("serve parsed %+v from %q, want %+v", got, argv, want)
			}
			if f.load != tc.load || f.hostID != "0123456789ab" || f.log != "/h/0123456789ab.log" {
				t.Fatalf("serve's own: load %q, host id %q, log %q", f.load, f.hostID, f.log)
			}
			for _, name := range []string{"workspace", "provider", "model", "agent-bin", "plugin-dir", "no-force", "ask", "plan", "continue", "load", "no-host-status"} {
				set := cmd.Flags().Changed(name)
				given := map[string]bool{
					"workspace": want.workspace != "", "provider": want.provider != "", "model": want.model != "",
					"agent-bin": want.agentBin != "", "plugin-dir": len(want.pluginDirs) > 0, "no-force": !want.force,
					"ask": want.ask, "plan": want.plan, "continue": want.cont, "load": tc.load != "", "no-host-status": want.noHostStatus,
				}[name]
				if set != given {
					t.Fatalf("--%s passed %v, want %v: %q", name, set, given, argv)
				}
			}
		})
	}
}

// TestParseReadyLine is the spawner's reading of the line itself: one line of
// at most readyLineMax bytes, its newline included, decoded; EOF before a byte
// is an exit, within a line a malformed one; a line past the bound is
// oversized however it ends; a not-ok line needs a reason.
func TestParseReadyLine(t *testing.T) {
	ok := `{"ok":true,"hostId":"0123456789ab","socket":"/s","crazeSessionId":"c","crazeVersion":"v","future":1}`
	pad := func(n int) string { return `{"ok":false,"error":"` + strings.Repeat("x", n) + `"}` }
	for _, tc := range []struct {
		name string
		in   string
		want spawnFailure
	}{
		{"ok, an unknown member ignored", ok + "\n", 0},
		{"ok, and more after it", ok + "\nmore", 0},
		{"exactly the bound", pad(readyLineMax-len(pad(0))-1) + "\n", 0},
		{"one past the bound", pad(readyLineMax-len(pad(0))) + "\n", spawnOversized},
		{"no newline, past the bound", pad(readyLineMax), spawnOversized},
		{"nothing", "", spawnExited},
		{"no newline", ok, spawnMalformed},
		{"not JSON", "hello\n", spawnMalformed},
		{"not ok, no reason", `{"ok":false}` + "\n", spawnMalformed},
		{"held", `{"ok":false,"error":"e","held":{"hostId":"h","crazeSessionId":"c"}}` + "\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, why := parseReady(strings.NewReader(tc.in))
			if got != tc.want {
				t.Fatalf("failure %d (%s), want %d", got, why, tc.want)
			}
		})
	}
}
