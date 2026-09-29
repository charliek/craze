package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/version"
)

// The ready handshake's host end (plan 030 C3, §3.4), in this process: craze
// serve given a ready pipe of the test's own — the write end as its
// readyPipe, the read end the test's — so what it answers, and when, is read
// directly, without a spawn. The spawner's end, and the host in a process of
// its own, are spawn_test.go's. And the host log's naming rule the handshake
// made possible (hostLogNamed). Each wait is bounded on its own (serveStep).

// runServeReady is runServeIn with a ready pipe: the host writes its line on
// the pipe whose read end it answers. closed closes that read end before the
// host starts: a launcher already gone.
func runServeReady(t *testing.T, closed bool, argv ...string) (*serveRun, *os.File) {
	t.Helper()
	rd, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	if closed {
		_ = rd.Close()
	}
	cmd, f := parseServeFlags(t, argv...)
	f.ready = &readyPipe{f: w}
	r := &serveRun{sigs: make(chan os.Signal, 4), stderr: &lockedBuffer{}, finished: make(chan struct{})}
	cmd.SetErr(r.stderr)
	go func() {
		r.err = runServe(cmd, f, hostEnv{}, r.sigs)
		close(r.finished)
	}()
	t.Cleanup(func() {
		select {
		case <-r.finished:
			return
		default:
		}
		select {
		case r.sigs <- syscall.SIGTERM:
		default:
		}
		select {
		case <-r.finished:
		case <-time.After(serveStep):
			t.Errorf("craze serve did not return after SIGTERM; stderr: %s", r.stderr)
		}
	})
	return r, rd
}

// readyFrom is the one ready line on rd, read as the spawner reads it
// (parseReady), within serveStep.
func readyFrom(t *testing.T, rd *os.File) readyLine {
	t.Helper()
	type got struct {
		line    readyLine
		failure spawnFailure
		why     string
	}
	ch := make(chan got, 1)
	go func() {
		line, failure, why := parseReady(rd)
		ch <- got{line, failure, why}
	}()
	select {
	case g := <-ch:
		if g.failure != 0 {
			t.Fatalf("the ready line: failure %d %s", g.failure, g.why)
		}
		return g.line
	case <-time.After(serveStep):
		t.Fatalf("no ready line within %v", serveStep)
		return readyLine{}
	}
}

// assertPipeEnds: nothing follows the line — the host closed its end.
func assertPipeEnds(t *testing.T, rd *os.File) {
	t.Helper()
	if err := rd.SetReadDeadline(time.Now().Add(serveStep)); err != nil {
		t.Fatal(err)
	}
	if n, err := rd.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after the ready line: %d bytes, %v; want EOF", n, err)
	}
}

// TestServeAnnouncesOnlyOnceItsIdentityIsInTheRegistry: the ok line follows
// the registry write that carries the session's identity (plan 030 §3.4), so
// a launcher that reads it can find the session by its craze id. Forced: the
// registry's first rewrite for the engine is held where it starts — the host
// bound and serving, its entry still without a session — and no line is
// there; let go, the line comes, and the registry lists the host under the
// session it names. The line names the host (its --host-id), its socket, the
// session and this craze.
func TestServeAnnouncesOnlyOnceItsIdentityIsInTheRegistry(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	let := func() { releaseOnce.Do(func() { close(release) }) }
	prev := serveBound
	serveBound = func(h *controlHost) {
		h.mu.Lock()
		inner := h.update
		h.update = func(fn func(*rundir.Entry)) error {
			enterOnce.Do(func() {
				close(entered)
				<-release
			})
			return inner(fn)
		}
		h.mu.Unlock()
	}
	t.Cleanup(func() { serveBound = prev })
	hostID := rundir.NewHostID()
	r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--host-id", hostID)
	// Before runServeReady's own cleanup: a failing test lets the write go.
	t.Cleanup(let)
	select {
	case <-entered:
	case <-time.After(serveStep):
		t.Fatalf("the registry was never rewritten; stderr: %s", r.stderr)
	}
	if e, ok := hostEntry(env, hostID); !ok || e.CrazeSessionID != "" {
		t.Fatalf("with the identity's write held the registry lists %+v (%v)", e, ok)
	}
	if err := rd.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, err := rd.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("with the identity's write held the host answered: %d bytes, %v", n, err)
	}
	if err := rd.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	let()
	line := readyFrom(t, rd)
	e, ok := hostEntry(env, hostID)
	switch {
	case !line.OK || line.HostID != hostID || line.CrazeVersion != version.Version || line.Error != "" || line.Held != nil:
		t.Fatalf("the ready line: %+v", line)
	case !ok || e.CrazeSessionID != line.CrazeSessionID || e.Socket != line.Socket || line.CrazeSessionID == "":
		t.Fatalf("when the line came the registry listed %+v (%v); the line: %+v", e, ok, line)
	}
	assertPipeEnds(t, rd)
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatal(err)
	}
	assertHostGone(t, env, e)
}

// TestServeAnswersWhyItNeverServed: a spawned host that returns before it is
// ready answers not ok with its refusal, word for word as craze serve exits
// with it, and closes the pipe. A session another craze holds adds who holds
// it — its host id and pid, as the lock names them, and the session — which
// the launcher's rendezvous needs; any other refusal names no holder.
func TestServeAnswersWhyItNeverServed(t *testing.T) {
	t.Run("no such session", func(t *testing.T) {
		serveHome(t)
		r, rd := runServeReady(t, false, "--load", "0199aaaa-bbbb-7ccc-8ddd-0000000000e1")
		line := readyFrom(t, rd)
		code, msg := exitCode(t, r.result(t, serveStep))
		if line.OK || line.Held != nil || line.Error != msg || code != 1 || msg != "craze serve: no session 0199aaaa-bbbb-7ccc-8ddd-0000000000e1" {
			t.Fatalf("the line %+v; exit %d %q", line, code, msg)
		}
		assertPipeEnds(t, rd)
	})
	t.Run("a usage error", func(t *testing.T) {
		serveHome(t)
		r, rd := runServeReady(t, false, "--host-id", "NOT-AN-ID")
		line := readyFrom(t, rd)
		code, msg := exitCode(t, r.result(t, serveStep))
		if line.OK || line.Error != msg || code != 2 || !strings.HasPrefix(msg, `craze serve: --host-id "NOT-AN-ID"`) {
			t.Fatalf("the line %+v; exit %d %q", line, code, msg)
		}
	})
	t.Run("held", func(t *testing.T) {
		env, ws := serveHome(t)
		const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000e2"
		seedIndexRow(t, sessions.Row{SessionID: "h-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id})
		holder := rundir.NewHostID()
		claim, err := rundir.ClaimSession(env, id, holder)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = claim.Release() })
		built := recordBuilt(t)
		r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--load", id)
		line := readyFrom(t, rd)
		code, msg := exitCode(t, r.result(t, serveStep))
		want := readyHeld{HostID: holder, PID: os.Getpid(), CrazeSessionID: id}
		if line.OK || line.Held == nil || *line.Held != want || line.Error != msg || code != 1 ||
			!strings.HasPrefix(msg, "craze serve: that session is open in another craze (pid ") {
			t.Fatalf("the line %+v (held %+v); exit %d %q", line, line.Held, code, msg)
		}
		assertPipeEnds(t, rd)
		select {
		case o := <-built:
			t.Fatalf("a held load built a session: %+v", o)
		default:
		}
		if entries, _ := rundir.Hosts(env); len(entries) != 0 {
			t.Fatalf("a held load left %+v", entries)
		}
	})
}

// TestServeStopsWhenItsLauncherIsGone: a spawned host whose launcher has gone
// before it was ready — the pipe's read end closed — finds its line refused
// (EPIPE, never a signal) and stops rather than live on as a host nobody knows
// it started: runServe returns nil, the log saying why, and nothing is left.
func TestServeStopsWhenItsLauncherIsGone(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r, _ := runServeReady(t, true, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	out := r.stderr.String()
	if !strings.Contains(out, "craze serve: the launcher did not take the ready line") ||
		!strings.Contains(out, "craze serve: stopping: its launcher went before it was ready") {
		t.Fatalf("the log: %s", out)
	}
	if entries, _ := rundir.Hosts(env); len(entries) != 0 {
		t.Fatalf("the host left %+v", entries)
	}
	if left, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "host-logs", "*.pgids")); len(left) != 0 {
		t.Fatalf("the host left its agents' record: %q", left)
	}
}

// TestServeRefusesALogNotNamedForItsHost (astra r4 1): a --log in the host
// logs' directory must be named for the host that writes it, <hostId>.log —
// its --host-id's, or with none the id it would mint and nobody can know — so
// that the sweep, which keeps a live host's files by that host's lock, can
// never find a live host's log under another id. Anything else there is exit
// 2 before anything is created: not the cache tree, not the log. A --log
// elsewhere is the user's own file, named as they like (the bind-failure and
// claim tests write theirs in a temp directory).
func TestServeRefusesALogNotNamedForItsHost(t *testing.T) {
	env, ws := serveHome(t)
	logs := filepath.Join(env.Home, ".cache", "craze", "host-logs")
	hostID, other := rundir.NewHostID(), rundir.NewHostID()
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"no host id", []string{"--log", filepath.Join(logs, other+".log")}},
		{"another host's id", []string{"--host-id", hostID, "--log", filepath.Join(logs, other+".log")}},
		{"not a host's name", []string{"--host-id", hostID, "--log", filepath.Join(logs, "mine.log")}},
		{"a rotation's name", []string{"--host-id", hostID, "--log", filepath.Join(logs, hostID+".log.1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := append([]string{"--agent-bin", fakeAgentPath(t), "--workspace", ws}, tc.argv...)
			r := runServeIn(t, hostEnv{}, argv...)
			code, msg := exitCode(t, r.result(t, serveStep))
			if code != 2 || !strings.Contains(msg, "a log in the host logs' directory is named for the host that writes it") {
				t.Fatalf("exit %d %q", code, msg)
			}
			if _, err := os.Lstat(filepath.Join(env.Home, ".cache")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the refusal made the cache tree: %v", err)
			}
		})
	}
}

// TestServeKeepsAQuietHostsOldLog (astra r4 1): a host that has said nothing
// for longer than the sweep's age keeps its log — named for it, so another
// host's start-up sweep finds its lock held — while that same sweep takes a
// gone host's log of the same age.
func TestServeKeepsAQuietHostsOldLog(t *testing.T) {
	env, ws := serveHome(t)
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	hostID := rundir.NewHostID()
	own := filepath.Join(dir, hostID+".log")
	gone := filepath.Join(dir, rundir.NewHostID()+".log")
	if err := os.WriteFile(gone, []byte("gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--host-id", hostID, "--log", own)
	e := r.waitServing(t, env, true)
	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, p := range []string{own, gone} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := rundir.SweepHostLogs(env, time.Now(), hostLogKeep, "")
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(own) || fileExists(gone) || n != 1 {
		t.Fatalf("the sweep removed %d: the quiet host's log there %v, the gone host's %v", n, fileExists(own), fileExists(gone))
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatal(err)
	}
	assertHostGone(t, env, e)
}
