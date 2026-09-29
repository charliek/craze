package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
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
// the launcher's rendezvous needs; any other refusal names no holder. A
// refusal of the session asked for — no such session, a spawn flag its
// provider cannot take — says it is the choice's (refused); a host that could
// not come up — its socket would not bind, a command line no launcher writes
// — does not (astra r7-c4 2).
func TestServeAnswersWhyItNeverServed(t *testing.T) {
	t.Run("no such session", func(t *testing.T) {
		serveHome(t)
		r, rd := runServeReady(t, false, "--load", "0199aaaa-bbbb-7ccc-8ddd-0000000000e1")
		line := readyFrom(t, rd)
		code, msg := exitCode(t, r.result(t, serveStep))
		if line.OK || line.Held != nil || !line.Refused || line.Error != msg || code != 1 || msg != "craze serve: no session 0199aaaa-bbbb-7ccc-8ddd-0000000000e1" {
			t.Fatalf("the line %+v; exit %d %q", line, code, msg)
		}
		assertPipeEnds(t, rd)
	})
	t.Run("a flag the session's provider cannot take", func(t *testing.T) {
		serveHome(t)
		ws := t.TempDir()
		const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000e3"
		seedIndexRow(t, sessions.Row{SessionID: "n-1", Provider: "native", CWD: absDir(ws), CrazeID: id})
		r, rd := runServeReady(t, false, "--load", id, "--agent-bin", fakeAgentPath(t))
		line := readyFrom(t, rd)
		code, msg := exitCode(t, r.result(t, serveStep))
		if line.OK || line.Held != nil || !line.Refused || line.Error != msg || code != 2 ||
			msg != "craze: --agent-bin cannot be used with provider native, which runs inside craze" {
			t.Fatalf("the line %+v; exit %d %q", line, code, msg)
		}
		assertPipeEnds(t, rd)
	})
	t.Run("a usage error", func(t *testing.T) {
		serveHome(t)
		r, rd := runServeReady(t, false, "--host-id", "NOT-AN-ID")
		line := readyFrom(t, rd)
		code, msg := exitCode(t, r.result(t, serveStep))
		if line.OK || line.Refused || line.Error != msg || code != 2 || !strings.HasPrefix(msg, `craze serve: --host-id "NOT-AN-ID"`) {
			t.Fatalf("the line %+v; exit %d %q", line, code, msg)
		}
	})
	t.Run("the socket cannot bind", func(t *testing.T) {
		env, ws := serveHome(t)
		t.Setenv("CRAZE_RUNTIME_DIR", unusableRuntimeDir(t))
		r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
		line := readyFrom(t, rd)
		code, msg := exitCode(t, r.result(t, serveStep))
		if line.OK || line.Held != nil || line.Refused || line.Error != msg || code != 1 ||
			!strings.HasPrefix(msg, "craze serve: the control socket: ") {
			t.Fatalf("the line %+v; exit %d %q", line, code, msg)
		}
		assertPipeEnds(t, rd)
		if entries, _ := rundir.Hosts(env); len(entries) != 0 {
			t.Fatalf("a host that could not bind left %+v", entries)
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
		if line.OK || line.Held == nil || *line.Held != want || line.Refused || line.Error != msg || code != 1 ||
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

// stubbornLine is what a stubborn fake agent (CRAZE_FAKE_STUBBORN) writes to
// its stderr — the host's log — once it ignores every signal but SIGKILL
// (CRAZE_FAKE_STDERR, written after the fake's signal.Ignore).
const stubbornLine = "craze test: the agent ignores SIGTERM now"

// stubbornAgent makes the tests' fake agents stubborn, and say so on their
// stderr (stubbornLine).
func stubbornAgent(t *testing.T) {
	t.Helper()
	t.Setenv("CRAZE_FAKE_STUBBORN", "1")
	t.Setenv("CRAZE_FAKE_STDERR", stubbornLine)
}

// waitStubborn waits, within serveStep, for r's agent to have said it is
// stubborn: from then on only its group's SIGKILL ends it before the fake's
// own bound — a SIGTERM that came earlier would end it as it starts.
func waitStubborn(t *testing.T, r *serveRun) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for !strings.Contains(r.stderr.String(), stubbornLine) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent never said it was stubborn; stderr: %s", r.stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeFailsAStartWhoseAgentCannotBeRecorded (astra r5-c3 2): a spawned
// host that cannot write its agent's process group down — the record's name
// taken by a directory, so its open fails — does not run that agent
// unrecorded. The agent, one that outlives its pipes (the record's failure is
// held until it is), is ended there and then, and the session's start fails,
// reported as every start failure is: on the host's log, and to a client
// reading the session's state, the record's failure in its words. The
// handshake is ok all the same: it never waits for the provider.
func TestServeFailsAStartWhoseAgentCannotBeRecorded(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	stubbornAgent(t)
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	hostID := rundir.NewHostID()
	if err := os.Mkdir(filepath.Join(dir, agentGroupsName(hostID)), 0o700); err != nil {
		t.Fatal(err)
	}
	type record struct {
		pgid int
		err  error
	}
	recorded, proceed := make(chan record, 4), make(chan struct{})
	var proceedOnce sync.Once
	goOn := func() { proceedOnce.Do(func() { close(proceed) }) }
	prev := serveAgentGroup
	serveAgentGroup = func(pgid int, err error) {
		recorded <- record{pgid, err}
		<-proceed
	}
	t.Cleanup(func() { serveAgentGroup = prev })

	r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--host-id", hostID)
	// Before runServeReady's own cleanup: a failing test lets the start go.
	t.Cleanup(goOn)
	if line := readyFrom(t, rd); !line.OK {
		t.Fatalf("the ready line: %+v", line)
	}
	var rec record
	select {
	case rec = <-recorded:
	case <-time.After(serveStep):
		t.Fatalf("no agent was spawned within %v; stderr: %s", serveStep, r.stderr)
	}
	if rec.err == nil || rec.pgid <= 0 {
		t.Fatalf("the record of group %d: %v, want a failure", rec.pgid, rec.err)
	}
	waitStubborn(t, r)
	goOn()
	e, ok := hostEntry(env, hostID)
	if !ok {
		t.Fatal("the host is not in the registry")
	}
	c, err := remote.Dial(stepCtx(t), e.Socket, remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	deadline := time.Now().Add(serveStep)
	var st protocol.StateResult
	for {
		if err := c.Call(stepCtx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: e.CrazeSessionID}, &st); err != nil {
			t.Fatalf("session.state: %v", err)
		}
		if st.Activity != protocol.ActivityStarting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session is still starting after %v", serveStep)
		}
		time.Sleep(10 * time.Millisecond)
	}
	const want = "agent: the agent's process group could not be recorded: "
	if !st.StartFailed || !strings.HasPrefix(st.Err, want) {
		t.Fatalf("the session's state: activity %s, start failed %v, %q; want a failed start, %q…", st.Activity, st.StartFailed, st.Err, want)
	}
	waitGroupGone(t, rec.pgid)
	if out := r.stderr.String(); !strings.Contains(out, "craze serve: the session did not start: "+want) {
		t.Fatalf("the log: %s", out)
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatal(err)
	}
	assertHostGone(t, env, e)
}

// TestServeKeepsItsAgentsRecordUntilItsStartHasJoined (astra r5-c3 3, r6-fix3):
// a stop that finds the session's start caught between spawning its agent —
// recorded already — and adopting it (held there by the test) closes an
// engine with no agent to end; the start ends that agent itself once it goes
// on. Until then the record is the one thing naming the agent, and it stays:
// through the engine's close and the socket's, to the start's join — and the
// session's claim is held across the join too, released only after it (a
// start still running may hold what the claim stands for).
//   - joined: let go, the start ends the agent (one that outlives its pipes),
//     the claim is released, craze serve returns, and the record goes after
//     it: nothing is left running, and nothing on disk;
//   - the join times out: the host kills the agent it recorded itself —
//     SIGTERM, which a stubborn agent ignores, the grace, SIGKILL — removes
//     the record and returns, leaving the claim to its process's exit. With no
//     test-side cleanup, nothing is left running.
func TestServeKeepsItsAgentsRecordUntilItsStartHasJoined(t *testing.T) {
	for _, joined := range []bool{true, false} {
		name := "joined"
		if !joined {
			name = "the join times out"
		}
		t.Run(name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			stubbornAgent(t)
			if !joined {
				prev, prevGrace := serveStartJoin, serveAgentTermGrace
				serveStartJoin, serveAgentTermGrace = 100*time.Millisecond, 200*time.Millisecond
				t.Cleanup(func() { serveStartJoin, serveAgentTermGrace = prev, prevGrace })
			}
			held, release := make(chan int, 1), make(chan struct{})
			var holdOnce, releaseOnce sync.Once
			let := func() { releaseOnce.Do(func() { close(release) }) }
			prevGroup := serveAgentGroup
			serveAgentGroup = func(pgid int, err error) {
				if err != nil {
					return
				}
				holdOnce.Do(func() {
					held <- pgid
					<-release
				})
			}
			t.Cleanup(func() { serveAgentGroup = prevGroup })
			var stepMu sync.Mutex
			var steps []string
			joining := make(chan struct{})
			var joiningOnce sync.Once
			prevStep := teardownStep
			teardownStep = func(s string) {
				stepMu.Lock()
				steps = append(steps, s)
				stepMu.Unlock()
				if s == "joining" {
					joiningOnce.Do(func() { close(joining) })
				}
			}
			t.Cleanup(func() { teardownStep = prevStep })

			hostID := rundir.NewHostID()
			record := filepath.Join(env.Home, ".cache", "craze", "host-logs", agentGroupsName(hostID))
			r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--host-id", hostID)
			// Before runServeReady's own cleanup: a failing test lets the
			// start go, so the host can still stop.
			t.Cleanup(let)
			line := readyFrom(t, rd)
			if !line.OK {
				t.Fatalf("the ready line: %+v", line)
			}
			var pgid int
			select {
			case pgid = <-held:
			case <-time.After(serveStep):
				t.Fatalf("no agent was recorded within %v; stderr: %s", serveStep, r.stderr)
			}
			if got := recordedGroups(t, record); len(got) != 1 || got[0] != pgid {
				t.Fatalf("the record names %v, want the agent's group %d", got, pgid)
			}

			r.sigs <- syscall.SIGTERM
			select {
			case <-joining:
			case <-time.After(serveStep):
				t.Fatalf("the stop sequence never came to the start's join; stderr: %s", r.stderr)
			}
			if !fileExists(record) {
				t.Fatal("the stop removed the agents' record while the start still held an agent it had not adopted")
			}
			if err := syscall.Kill(-pgid, 0); err != nil {
				t.Fatalf("the agent's group %d, which nothing has ended yet: %v", pgid, err)
			}
			waitStubborn(t, r)
			claimed := func() error {
				_, err := anotherCraze(t).claimSession(line.CrazeSessionID)
				return err
			}

			if joined {
				// The join has not come, and the claim is still this host's: a
				// second host loading the session now is refused it, held —
				// the two-host schedule a claim released ahead of the join
				// would let through to whatever the start still holds.
				seedIndexRow(t, sessions.Row{SessionID: "fake-held", Provider: "cursor", CWD: absDir(ws), CrazeID: line.CrazeSessionID, Title: "held"})
				second := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--load", line.CrazeSessionID)
				var h *rundir.HeldError
				if err := second.result(t, serveStep); !errors.As(err, &h) {
					t.Fatalf("a second host loading the session during the first's join: %v, want refused held", err)
				}
				select {
				case <-r.finished:
					t.Fatal("craze serve returned before its start did")
				default:
				}
				let()
				if err := r.result(t, serveStep); err != nil {
					t.Fatal(err)
				}
				waitGroupGone(t, pgid)
				if fileExists(record) {
					t.Fatal("a host whose start joined left its agents' record")
				}
				if err := claimed(); err != nil {
					t.Fatalf("the session's claim once the host stopped: %v", err)
				}
				// The first host's claims went after its join (the second
				// host's own teardown is in the record too, before it).
				stepMu.Lock()
				got := slices.Clone(steps)
				stepMu.Unlock()
				if j := slices.Index(got, "joined"); j < 0 || !slices.Contains(got[j:], "released") || got[len(got)-1] != "released" {
					t.Fatalf("the steps are %q: the claims were not released after the start's join", got)
				}
				return
			}
			if err := r.result(t, serveStep); err != nil {
				t.Fatal(err)
			}
			// No test-side kill: the host's own last resort ended the agent
			// and removed the record.
			waitGroupGone(t, pgid)
			if fileExists(record) {
				t.Fatal("a host whose start did not join left its agents' record")
			}
			var h *rundir.HeldError
			if err := claimed(); !errors.As(err, &h) {
				t.Fatalf("a host whose start did not join released its claim before its exit: %v", err)
			}
			stepMu.Lock()
			got := slices.Clone(steps)
			stepMu.Unlock()
			if want := []string{"joining", "join timed out", "claims left to the exit"}; !slices.Equal(got[len(got)-3:], want) {
				t.Fatalf("the stop's last steps are %q, want %q", got, want)
			}
			if out := r.stderr.String(); !strings.Contains(out, "craze serve: the session's start had not returned when the host stopped") {
				t.Fatalf("the log: %s", out)
			}
		})
	}
}
