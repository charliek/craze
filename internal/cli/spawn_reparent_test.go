package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// Direct spawns leave the command's process tree (plan 041 X32, live check
// L6): Claude Code's TaskStop signals a Bash call's whole process tree, found
// by parent pid, and a host craze new --wait had started as its own child —
// setsid gives it a session, not another parent — was ended with the command.
// craze new --wait and craze send now start their hosts through an
// intermediate that exits at once (hostspawn.StartReparented), so such a
// kill reaches the command alone, which cancels its own turn (§3.1
// "Signals"), and the session goes on. The TUI launcher's spawns are
// unchanged; the hub's never come here.

// processTree is root and every descendant of it, found as a tree kill finds
// them — each process whose parent is in the tree — from a snapshot of every
// process's parent: /proc/<pid>/stat on Linux (rundir.ProcessIdentity), `ps`
// on macOS.
func processTree(t *testing.T, root int) []int {
	t.Helper()
	parents := map[int]int{}
	switch runtime.GOOS {
	case "linux":
		entries, err := os.ReadDir("/proc")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			// A process gone since the listing is no one's descendant.
			if id, err := rundir.ProcessIdentity(pid); err == nil {
				parents[pid] = id.PPID
			}
		}
	case "darwin":
		out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
		if err != nil {
			t.Fatalf("ps: %v", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			pid, perr := strconv.Atoi(f[0])
			ppid, qerr := strconv.Atoi(f[1])
			if perr == nil && qerr == nil {
				parents[pid] = ppid
			}
		}
	default:
		t.Skipf("no process tree to walk on %s", runtime.GOOS)
	}
	tree := []int{root}
	for i := 0; i < len(tree); i++ {
		for pid, ppid := range parents {
			if ppid == tree[i] && !slices.Contains(tree, pid) {
				tree = append(tree, pid)
			}
		}
	}
	return tree
}

// parentOf is pid's parent, failing the test when it cannot be read.
func parentOf(t *testing.T, pid int) int {
	t.Helper()
	id, err := rundir.ProcessIdentity(pid)
	if err != nil {
		t.Fatalf("process %d: %v", pid, err)
	}
	return id.PPID
}

// TestOnlyTheDirectSpawnLeavesTheSpawnersTree is X32's scope. The TUI
// launcher's spawn — built by its own options (launcher.hostOptions) — starts
// the host as this process's child, as it always has: the process the spawn
// built is the host, its parent this process, in this process's tree. A
// direct spawn (craze new --wait's and craze send's: hubEnv and reparent)
// starts it through an intermediate: the process the spawn built is not the
// host, and has exited 0 and been reaped; the host leads a session of its
// own, has another parent, and is no descendant of this process — yet the
// spawn's Child is that host: it is stopped, and seen gone, through it.
func TestOnlyTheDirectSpawnLeavesTheSpawnersTree(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	kids := spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	cursor, err := agent.ProviderByName("cursor")
	if err != nil {
		t.Fatal(err)
	}
	l := &launcher{env: env, own: cursor}
	flags := tuiFlags{force: true, agentBin: fakeAgentPath(t), workspace: ws}
	direct := spawnFor(t, env, ws)
	direct.hubEnv, direct.reparent = true, true
	me := os.Getpid()

	ref, err := spawnNow(t, l.hostOptions(flags, cursor, ""))
	if err != nil {
		t.Fatalf("the launcher's spawn: %v", err)
	}
	host := ref.entry.PID
	if built := kids.cmd(t, 0).Process.Pid; host != built || ref.child.PID() != built {
		t.Fatalf("the launcher's host is pid %d (its Child %d); its spawn built %d", host, ref.child.PID(), built)
	}
	if p := parentOf(t, host); p != me {
		t.Fatalf("the launcher's host's parent is %d, want this process %d", p, me)
	}
	if !slices.Contains(processTree(t, me), host) {
		t.Fatalf("the launcher's host %d is not in this process's tree", host)
	}
	if err := stopHost(ref.entry); err != nil {
		t.Fatal(err)
	}
	waitReaped(t, host)

	ref, err = spawnNow(t, direct)
	if err != nil {
		t.Fatalf("the direct spawn: %v", err)
	}
	host = ref.entry.PID
	mid := kids.cmd(t, 1)
	switch {
	case host == mid.Process.Pid || ref.child.PID() != host:
		t.Fatalf("the direct spawn's host is pid %d (its Child %d), its intermediate %d", host, ref.child.PID(), mid.Process.Pid)
	case mid.ProcessState == nil || !mid.ProcessState.Success():
		t.Fatalf("the intermediate %d has not exited 0 and been reaped: %v", mid.Process.Pid, mid.ProcessState)
	}
	if p := parentOf(t, host); p == me || p == mid.Process.Pid {
		t.Fatalf("the direct spawn's host's parent is %d (this process %d, the intermediate %d)", p, me, mid.Process.Pid)
	}
	if tree := processTree(t, me); slices.Contains(tree, host) {
		t.Fatalf("the direct spawn's host %d is in this process's tree %v", host, tree)
	}
	if sid, err := unix.Getsid(host); err != nil || sid != host {
		t.Fatalf("the direct spawn's host leads session %d (%v), want its own", sid, err)
	}
	// Its entry records its own start (C6 r3), as its spawn's report named
	// it: what finds it should that report never come.
	if id, err := rundir.ProcessIdentity(host); err != nil || ref.entry.PIDStart == 0 || ref.entry.PIDStart != id.Start {
		t.Fatalf("the direct spawn's host's entry records start %d; its pid's is %+v (%v)", ref.entry.PIDStart, id, err)
	}
	if ref.child.Exited() {
		t.Fatal("the direct spawn's Child says its serving host has exited")
	}
	if err := stopHost(ref.entry); err != nil {
		t.Fatal(err)
	}
	if !ref.child.WaitExit(serveStep) || ref.child.Err() != nil {
		t.Fatalf("the direct spawn's Child never saw its host go (err %v)", ref.child.Err())
	}
	waitReaped(t, host)
}

// openWriter opens the FIFO path for writing, within serveStep — which
// blocks until something has it open for reading — failing the test early
// when ended says the reader can never come.
func openWriter(t *testing.T, path string, ended func() string) *os.File {
	t.Helper()
	opened := make(chan *os.File, 1)
	go func() {
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			opened <- f
		}
	}()
	deadline := time.After(serveStep)
	for {
		select {
		case f := <-opened:
			t.Cleanup(func() { _ = f.Close() })
			return f
		case <-deadline:
			t.Fatalf("nothing opened %s within %v", path, serveStep)
		case <-time.After(20 * time.Millisecond):
			if why := ended(); why != "" {
				// The open stays parked; unblocked by opening the read end.
				if r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = r.Close()
				}
				t.Fatalf("nothing will open %s: %s", path, why)
			}
		}
	}
}

// TestATreeKillOfNewWaitCancelsOnlyItsTurn is X32 end to end, the way live
// check L6 failed: craze new --wait runs in a process of its own; its turn is
// running (the fake agent holds its first step at the gate); then SIGTERM is
// sent to every process in the command's tree, walked by parent pid, exactly
// as a tree kill does. The host is not in that tree, so only the command is
// signalled: its owned signal path cancels its own turn by id — `cancelled`,
// exit 3 — and detaches; the host still serves, never stopped, its turn
// ended cancelled; and a later craze send runs in it. The control is the same
// kill with the host started as the command's own child (startReparented =
// hostspawn.StartCmd): the tree holds the host, and the kill ends it.
func TestATreeKillOfNewWaitCancelsOnlyItsTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reparent bool
	}{{"reparented", true}, {"control, in the tree", false}} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, _ := unattendedHome(t)
			gate := filepath.Join(t.TempDir(), "gate")
			if err := syscall.Mkfifo(gate, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRAZE_FAKE_SCRIPT", "long-turn")
			t.Setenv("CRAZE_FAKE_GATE", gate)
			extra := []string{cliChildSpawnHosts + "=1"}
			if !tc.reparent {
				extra = append(extra, cliChildNoReparent+"=1")
			}
			waiter := startCrazeChildWith(t, extra, "new", "--wait", "--json", "--provider", "cursor", "-C", ws, "a long task")

			// The fake opens its gate when its first turn reaches its first
			// step: from then on the turn is running.
			w := openWriter(t, gate, func() string {
				if waiter.running() {
					return ""
				}
				return "craze new --wait exited: " + waiter.output.String()
			})
			e := onlyHost(t, env)
			tree := processTree(t, waiter.cmd.Process.Pid)
			if in := slices.Contains(tree, e.PID); in != !tc.reparent {
				t.Fatalf("the host %d is in the command's tree %v: %v, want %v", e.PID, tree, in, !tc.reparent)
			}
			for _, pid := range tree {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
			code := waiter.wait(t, serveStep)
			logPath := filepath.Join(hostLogsDir(t, env), e.HostID+".log")
			hostLog := func() string { b, _ := os.ReadFile(logPath); return string(b) }

			if !tc.reparent {
				waitReaped(t, e.PID)
				if log := hostLog(); !strings.Contains(log, "craze serve: stopping: SIGTERM") {
					t.Fatalf("the host in the tree did not stop on the kill's SIGTERM (command exit %d): %s", code, log)
				}
				return
			}
			m := decodeResult(t, waiter.output.String())
			if code != 3 || m["status"] != "cancelled" || m["turn"] != "turn-1" || m["session"] != e.CrazeSessionID {
				t.Fatalf("craze new --wait after the tree kill: exit %d, %v", code, m)
			}
			if now, ok := hostServing(env, e.CrazeSessionID); !ok || now.HostID != e.HostID || !processAlive(e.PID) {
				t.Fatalf("the host after the tree kill: %+v %v (alive %v)", now, ok, processAlive(e.PID))
			}
			if log := hostLog(); strings.Contains(log, "stopping:") {
				t.Fatalf("the host was stopped: %s", log)
			}
			if lt := listRow(t, e).LastTurn; lt == nil || lt.TurnID != "turn-1" || lt.Outcome != protocol.TurnCancelled {
				t.Fatalf("the host's last turn: %+v", lt)
			}
			// The next turn's two steps, let through at the gate.
			if _, err := w.Write([]byte{1, 1}); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := executeErr([]string{"send", psShort(e.CrazeSessionID), "--wait", "--json", "and on"})
			if m := decodeResult(t, stdout); code != 0 || m["status"] != "completed" || m["turn"] != "turn-2" || m["session"] != e.CrazeSessionID {
				t.Fatalf("craze send after the tree kill: exit %d, %v, %q", code, m, stderr)
			}
		})
	}
}

// hostLogsDir is env's host logs' directory.
func hostLogsDir(t *testing.T, env rundir.Env) string {
	t.Helper()
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAnUnreportedHostIsEndedUnlessShared is C6 r1, 1 and r2, 1: the
// intermediate starts its host and is held before reporting it
// (cliChildReparentHold); the host lists itself and writes its ready line
// whole (cliChildAnnounced), so closing the ready pipe can no longer stop it;
// and only then does the spawner give up on the report (its timer fired by
// the test). The spawn fails, and:
//
//   - own, stalled: the intermediate never reports, and the host its spawner
//     was never told of is found by the host id the spawner gave it and
//     ended — SIGTERM, its stop sequence, gone from the registry (craze new
//     --wait's);
//   - shared, stalled: that host serves on (craze send's reload);
//   - shared, reporting: a second sender has prompted the host, and the
//     intermediate is let go once its spawner has closed the report's read
//     end (hostspawn.ReparentAbandoning) — it attempts its report, finds
//     nobody to read it, and exits, never signalling the host, which serves
//     on, the second sender's turn done in it.
func TestAnUnreportedHostIsEndedUnlessShared(t *testing.T) {
	for _, tc := range []struct {
		name              string
		shared, reporting bool
	}{{"own, stalled", false, false}, {"shared, stalled", true, false}, {"shared, reporting", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			dir := t.TempDir()
			announced, release := filepath.Join(dir, "announced"), filepath.Join(dir, "release")
			kids := spawnAsChild(t, func(int) []string {
				return []string{cliChildReparentHold + "=" + release, cliChildAnnounced + "=" + announced}
			})
			spawnBounds(t, serveStep, serveStep, serveStep)
			fire := make(chan time.Time)
			prevTimer, prevAbandoning := hostspawn.ReparentTimer, hostspawn.ReparentAbandoning
			hostspawn.ReparentTimer = func(time.Duration) <-chan time.Time { return fire }
			if tc.reporting {
				// On the spawn's goroutine: errors, never a Fatal.
				hostspawn.ReparentAbandoning = func() {
					mid := kids.cmd(t, 0).Process.Pid
					if err := os.WriteFile(release, nil, 0o600); err != nil {
						t.Error(err)
						return
					}
					// Its exit, and its reaping by the spawn's own wait.
					for deadline := time.Now().Add(serveStep); processAlive(mid); time.Sleep(5 * time.Millisecond) {
						if time.Now().After(deadline) {
							t.Errorf("the intermediate %d, let go, did not exit within %v", mid, serveStep)
							return
						}
					}
				}
			}
			t.Cleanup(func() { hostspawn.ReparentTimer, hostspawn.ReparentAbandoning = prevTimer, prevAbandoning })
			t.Cleanup(func() { stopEveryHost(t, env) })
			opts := spawnFor(t, env, ws)
			opts.hubEnv, opts.reparent, opts.shared = true, true, tc.shared

			res := goSpawn(t, context.Background(), opts)
			waitUntil(t, "the host's ready line to go out", func() bool { _, err := os.Stat(announced); return err == nil })
			e := onlyHost(t, env)
			if tc.reporting {
				// Its session started, so it takes the prompt.
				e = waitServingEntry(t, env, true, func() error { return nil }, &lockedBuffer{})
				promptUnattached(t, e, "from B")
				if lt := waitLastTurn(t, e); lt.TurnID != "turn-1" || lt.Outcome != protocol.TurnDone {
					t.Fatalf("the second sender's turn: %+v", lt)
				}
			}
			close(fire)
			se := failure(t, spawned(t, res).err)
			if se.kind != spawnStartFailed || !strings.Contains(se.msg, "its intermediate reported no host (no report within") {
				t.Fatalf("the spawn: %v %q", se.kind, se.msg)
			}
			logPath := filepath.Join(hostLogsDir(t, env), e.HostID+".log")
			hostLog := func() string { b, _ := os.ReadFile(logPath); return string(b) }
			if !tc.shared {
				waitReaped(t, e.PID)
				if log := hostLog(); !strings.Contains(log, "craze serve: stopping: SIGTERM") {
					t.Fatalf("the unreported host did not stop on its spawner's SIGTERM: %s", log)
				}
				assertNoHosts(t, env)
				return
			}
			if now, ok := hostServing(env, e.CrazeSessionID); !ok || now.HostID != e.HostID || !processAlive(e.PID) {
				t.Fatalf("the shared spawn's unreported host: %+v %v (alive %v)", now, ok, processAlive(e.PID))
			}
			if log := hostLog(); strings.Contains(log, "stopping:") {
				t.Fatalf("the shared spawn's host was stopped: %s", log)
			}
			if tc.reporting {
				if lt := listRow(t, e).LastTurn; lt == nil || lt.TurnID != "turn-1" {
					t.Fatalf("the second sender's turn in the host after: %+v", lt)
				}
			}
		})
	}
}

// TestNewWaitTimeoutCutsAStalledIntermediate is C6 r1, 2 end to end: craze
// new --wait --timeout 2s whose host's intermediate stalls before reporting
// it (cliChildReparentHold) returns at its timeout — ReparentWait is far
// longer — `timed out before the prompt was sent`, exit 1; and the host it
// started stops, whichever way the timeout fell: listed already, it is found
// and ended; not yet, it finds its ready pipe closed when it announces itself
// and stops on its own.
func TestNewWaitTimeoutCutsAStalledIntermediate(t *testing.T) {
	env, ws := serveHome(t)
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t)))
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	// Held for good: the file is never made.
	never := filepath.Join(t.TempDir(), "never")
	spawnAsChild(t, func(int) []string { return []string{cliChildReparentHold + "=" + never} })
	spawnBounds(t, serveStep, serveStep, serveStep)
	prev := hostspawn.ReparentWait
	hostspawn.ReparentWait = 2 * serveStep
	t.Cleanup(func() { hostspawn.ReparentWait = prev })
	t.Cleanup(func() { stopEveryHost(t, env) })

	const timeout = 2 * time.Second
	start := time.Now()
	stdout, stderr, code := executeErr([]string{"new", "--wait", "--timeout", timeout.String(), "--provider", "cursor", "-C", ws, "never sent"})
	took := time.Since(start)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "craze new: timed out before the prompt was sent") {
		t.Fatalf("craze new --wait past its timeout: exit %d, %q %q", code, stdout, stderr)
	}
	if took > timeout+serveStep {
		t.Fatalf("craze new --wait --timeout %v returned after %v (the intermediate's own bound is %v)", timeout, took, hostspawn.ReparentWait)
	}
	waitUntil(t, "the host to stop", func() bool {
		logs := hostLogs(t, env)
		if len(logs) != 1 {
			return false
		}
		b, _ := os.ReadFile(logs[0])
		entries, err := rundir.Hosts(env)
		return err == nil && len(entries) == 0 && strings.Contains(string(b), "craze serve: stopping:")
	})
}

// TestASecondSignalHurriesAnAbandonedSpawn is C8, 3: craze new --wait's
// host is up and listed but never acts on SIGTERM (cliChildReady=block), and
// its intermediate never reports it (cliChildReparentHold). A first signal
// ends the report's wait: the start is abandoned, and its clean-up finds the
// host and sends it SIGTERM, which it ignores — a clean-up that would wait
// out the host's grace (30 s here) and the SIGKILL past it. A second signal,
// once the clean-up has begun (hostspawn.ReparentAbandoning), cuts that short
// as it cuts C3's undo short: the command returns at once, `cancelled`, exit
// 3, the host sent SIGTERM and left (its log says so), still running.
func TestASecondSignalHurriesAnAbandonedSpawn(t *testing.T) {
	env, ws := serveHome(t)
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t)))
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	never := filepath.Join(t.TempDir(), "never")
	spawnAsChild(t, func(int) []string { return []string{cliChildReparentHold + "=" + never, cliChildReady + "=block"} })
	spawnBounds(t, serveStep, serveStep, serveStep)
	sigs := withWaitSignals(t)
	abandoning := make(chan struct{}, 1)
	prev := hostspawn.ReparentAbandoning
	hostspawn.ReparentAbandoning = func() { abandoning <- struct{}{} }
	t.Cleanup(func() { hostspawn.ReparentAbandoning = prev })

	run := goCraze("new", "--wait", "--json", "--provider", "cursor", "-C", ws, "never sent")
	e := onlyHost(t, env)
	t.Cleanup(func() {
		// It never acts on SIGTERM: killed, and the agents it recorded.
		_ = syscall.Kill(-e.PID, syscall.SIGKILL)
		waitReaped(t, e.PID)
		hostspawn.KillRecordedAgents(filepath.Join(hostLogsDir(t, env), hostspawn.AgentGroupsName(e.HostID)), 0, func(string, ...any) {})
	})
	sigs <- syscall.SIGINT
	select {
	case <-abandoning:
	case <-time.After(serveStep):
		t.Fatal("the start was never abandoned")
	}
	sigs <- syscall.SIGINT
	hurried := time.Now()
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("craze new --wait has not returned %v after its second signal (the host's grace is %v)", time.Since(hurried), hostspawn.TermGrace)
	}
	m := decodeResult(t, run.stdout)
	if run.code != 3 || m["status"] != "cancelled" || m["error"] != "interrupted before the prompt was sent" {
		t.Fatalf("a hurried abandon: exit %d, %v, %q", run.code, m, run.stderr)
	}
	b, _ := os.ReadFile(filepath.Join(hostLogsDir(t, env), e.HostID+".log"))
	if !strings.Contains(string(b), "ending this host") || !strings.Contains(string(b), "hurried: SIGTERM sent") || !processAlive(e.PID) {
		t.Fatalf("the host after a hurried abandon (alive %v): %s", processAlive(e.PID), b)
	}
}
