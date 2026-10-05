package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// craze hub as a process of its own (plan 032 §3.5, §3.18): the test binary
// run as craze (serve_child_test.go's init) with argv ["hub"], spawned by
// hub.Ensure through the seam a test installs (hubAsChild) — the only way a
// test binary spawns a hub at all: without the seam, Ensure is hub.ErrNoHub
// (TestEnsureSpawnsNoHubInThisTestBinary). Each hub child's watchdog ends it
// if the test process dies first (TestAnOrphanedHubChildExits), and every
// child a test spawned is ended when the test ends.

// hubChildren is every hub child a test spawned.
type hubChildren struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
}

// pids is every spawned hub child's pid.
func (h *hubChildren) pids() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []int
	for _, c := range h.cmds {
		if c.Process != nil {
			out = append(out, c.Process.Pid)
		}
	}
	return out
}

// hubAsChild installs hub.Command for one test: every hub Ensure spawns is
// this test binary run as `craze hub`, in the test's environment plus extra.
// When the test ends each one still running is sent SIGTERM — its teardown —
// and SIGKILL if it has not exited within serveStep, which fails the test.
func hubAsChild(t *testing.T, extra ...string) *hubChildren {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argvJSON, err := json.Marshal([]string{"hub"})
	if err != nil {
		t.Fatal(err)
	}
	kids := &hubChildren{}
	// Each child's own stderr, for the dump a hung one is asked for below
	// (registered before the cleanup that reads it, so removed after it).
	stderrs := t.TempDir()
	prev := hub.Command
	hub.Command = func(argv []string) (*exec.Cmd, error) {
		if !slices.Equal(argv, []string{"hub"}) {
			return nil, fmt.Errorf("the hub's command line is %q", argv)
		}
		// -test.run matches nothing, so a child whose init fell through runs
		// no test.
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(append(childEnv(argvJSON), cliChildStderrDir+"="+stderrs), extra...)
		kids.mu.Lock()
		kids.cmds = append(kids.cmds, cmd)
		kids.mu.Unlock()
		return cmd, nil
	}
	t.Cleanup(func() {
		hub.Command = prev
		for _, pid := range kids.pids() {
			endHubChild(t, pid, stderrs)
		}
	})
	return kids
}

// endHubChild ends pid, a hub child whose stderr is in dir/<pid>, and fails
// the test if it will not go: SIGTERM, serveStep to exit, then SIGKILL. One
// still there says first where it is — its state and signal masks, then the
// goroutine dump a SIGQUIT writes to its stderr — and how long it outlived
// its SIGTERM, measured. (Plan 032 X54 took this report for a child that
// would not exit; it was hubAlive's own race, SF-118.) A look that fails ends
// the wait as it is: the child is killed and the failure said, taken for
// neither answer, and the cleanup goes on to the children after it.
func endHubChild(t *testing.T, pid int, dir string) {
	t.Helper()
	var lookErr error
	alive := func() bool {
		if lookErr != nil {
			return false
		}
		a, err := hubLiveness(pid)
		lookErr = err
		return a
	}
	defer func() {
		if lookErr != nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("ending hub child %d, whether it is alive: %v; killed", pid, lookErr)
		}
	}()
	if !alive() {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	termed := time.Now()
	deadline := termed.Add(serveStep)
	for alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !alive() {
		return
	}
	// How long it has outlived its SIGTERM, measured at the look that still
	// saw it.
	outlived := time.Since(termed).Round(time.Millisecond)
	state := procSignalState(pid)
	_ = syscall.Kill(pid, syscall.SIGQUIT)
	quit := time.Now().Add(5 * time.Second)
	for alive() && time.Now().Before(quit) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	dump, _ := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)))
	t.Errorf("hub child %d had not exited %v after its SIGTERM; killed\n%s\n--- its stderr, a SIGQUIT's dump:\n%s", pid, outlived, state, dump)
}

// procSignalState is what /proc says of pid's state and signals — State,
// SigQ, SigPnd, ShdPnd, SigBlk, SigIgn, SigCgt, and its wait channel — for a
// failure's message; "" where there is no /proc.
func procSignalState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		if runtime.GOOS != "linux" {
			return ""
		}
		// A process kill still finds but /proc cannot show: say both.
		return fmt.Sprintf("/proc/%d/status: %v; kill(%d, 0): %v", pid, err, pid, syscall.Kill(pid, 0))
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		for _, key := range []string{"State:", "SigQ:", "SigPnd:", "ShdPnd:", "SigBlk:", "SigIgn:", "SigCgt:"} {
			if strings.HasPrefix(line, key) {
				out = append(out, line)
			}
		}
	}
	if w, err := os.ReadFile(fmt.Sprintf("/proc/%d/wchan", pid)); err == nil {
		out = append(out, "wchan:\t"+string(w))
	}
	return strings.Join(out, "\n")
}

// hubAlive reports whether pid has not exited: not gone, and not a zombie
// its reaper has not taken yet. A look that fails (hubLiveness says which
// do) fails the test: it is this helper's own failure, never taken for
// either answer.
func hubAlive(t testing.TB, pid int) bool {
	t.Helper()
	a, err := hubLiveness(pid)
	if err != nil {
		t.Fatalf("whether process %d is alive: %v", pid, err)
	}
	return a
}

// hubLiveness is whether pid has not exited, from two looks: kill(pid, 0),
// then — on Linux — its /proc stat. A process kill does not find is gone.
// One it finds is gone too when it is a zombie, or when its stat is no more
// by the read: ENOENT at the open, ESRCH at the read, a reaper that took the
// zombie between the two looks (SF-118: taking that for a live process made
// a hub that exited at once look like one that outlived its whole step).
// Any other error, or a stat with no state letter, is an error: the helper
// cannot say. internal/hub's alive is the same, and its tests pin it.
func hubLiveness(pid int) (bool, error) {
	if !processAlive(pid) {
		return false, nil
	}
	if runtime.GOOS != "linux" {
		// No /proc: a process kill finds has not exited, a zombie included.
		return true, nil
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ESRCH):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("its stat, after kill found it: %w", err)
	}
	state, ok := statLetter(b)
	if !ok {
		return false, fmt.Errorf("a stat with no state letter: %q", b)
	}
	return state != "Z", nil
}

// waitHubGone waits for pid to exit, within serveStep.
func waitHubGone(t *testing.T, pid int, log func() string) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for hubAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("hub %d is still running after %v (%s); its log: %s", pid, serveStep, procState(pid), log())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ensureHub is hub.Ensure in this process's environment, bounded by
// serveStep, with its record.
func ensureHub(t *testing.T, env rundir.Env) (string, rundir.HubRecord) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	sock, err := hub.Ensure(ctx, env, protocol.ConnectionCapabilities{RosterSubscribe: true, Connect: true})
	if err != nil {
		t.Fatalf("hub.Ensure: %v", err)
	}
	rec, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatalf("the hub's record: %v", err)
	}
	if rec.Socket != sock {
		t.Fatalf("Ensure answered %s; the record names %s", sock, rec.Socket)
	}
	return sock, rec
}

// hubLog is the hub's log as it stands, for a failure's message.
func hubLog(env rundir.Env) func() string {
	return func() string {
		path, err := hub.LogPath(env)
		if err != nil {
			return err.Error()
		}
		b, _ := os.ReadFile(path)
		return string(b)
	}
}

// hubHello says hello to the hub at sock and answers its result.
func hubHello(t *testing.T, sock string) protocol.HubHelloResult {
	t.Helper()
	nc, err := net.DialTimeout("unix", sock, serveStep)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(serveStep))
	if err := protocol.WriteLine(nc, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "hello",
		"params": protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}}}); err != nil {
		t.Fatal(err)
	}
	line, err := protocol.NewLineReader(nc, protocol.OutboundLineMax).ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result protocol.HubHelloResult `json:"result"`
		Error  *protocol.Error         `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil || resp.Error != nil {
		t.Fatalf("hello: %s (%v)", line, err)
	}
	return resp.Result
}

// TestEnsureSpawnsNoHubInThisTestBinary (§3.18's leak guard): this package's
// tests spawn hubs only through the seam a test installs; without one, Ensure
// is hub.ErrNoHub at once.
func TestEnsureSpawnsNoHubInThisTestBinary(t *testing.T) {
	env, _ := serveHome(t)
	if hub.Command != nil {
		t.Fatal("hub.Command is installed outside a test")
	}
	_, err := hub.Ensure(context.Background(), env, protocol.ConnectionCapabilities{})
	if !errors.Is(err, hub.ErrNoHub) {
		t.Fatalf("hub.Ensure with no seam = %v, want hub.ErrNoHub", err)
	}
	if _, _, err := rundir.ReadHubRecord(env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a hub record exists after a refused Ensure: %v", err)
	}
}

// TestHubChildServesUntilSIGTERM: a craze hub child spawned by Ensure serves
// its hello as its record says, in HOME, handed the environment contract's
// environment (no launch's choices, no terminal's marks, CRAZE_HOME
// absolute), says so in its log; on SIGTERM it tears down, leaving no record
// and no socket.
func TestHubChildServesUntilSIGTERM(t *testing.T) {
	env, _ := serveHome(t)
	t.Setenv("CRAZE_PROVIDER", "grok")
	t.Setenv("CRAZE_AGENT_BIN", "/nowhere/agent")
	t.Setenv("TMUX", "/tmp/tmux-test,1,0")
	t.Setenv("HERDR_ENV", "1")
	kids := hubAsChild(t)
	sock, rec := ensureHub(t, env)
	if !slices.Contains(kids.pids(), rec.PID) {
		t.Fatalf("the record names pid %d; the hubs spawned are %v", rec.PID, kids.pids())
	}
	res := hubHello(t, sock)
	if res.Endpoint.Kind != protocol.EndpointHub || res.Endpoint.HostID != rec.HubID || res.Endpoint.PID != rec.PID {
		t.Fatalf("hello answered %+v; the record is %+v", res.Endpoint, rec)
	}
	if runtime.GOOS == "linux" {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", rec.PID))
		if err != nil {
			t.Fatal(err)
		}
		vars := strings.Split(string(environ), "\x00")
		for _, gone := range []string{"CRAZE_PROVIDER=", "CRAZE_AGENT_BIN=", "TMUX=", "HERDR_ENV="} {
			for _, v := range vars {
				if strings.HasPrefix(v, gone) {
					t.Errorf("the hub was handed %s", v)
				}
			}
		}
		if !slices.Contains(vars, hub.HubChildEnv+"=1") || !slices.Contains(vars, "CRAZE_HOME="+os.Getenv("CRAZE_HOME")) {
			t.Errorf("the hub's environment lacks its mark or its craze directory: %q", vars)
		}
		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", rec.PID))
		if err != nil || cwd != env.Home {
			t.Errorf("the hub runs in %q (%v), want HOME %s", cwd, err, env.Home)
		}
	}
	log := hubLog(env)
	if !strings.Contains(log(), "serving "+sock) {
		t.Fatalf("the hub's log does not say it serves: %s", log())
	}
	if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitHubGone(t, rec.PID, log)
	if _, _, err := rundir.ReadHubRecord(env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record is still there: %v", err)
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket is still there: %v", err)
	}
	if !strings.Contains(log(), "stopping: SIGTERM") {
		t.Fatalf("the hub's log does not say why it stopped: %s", log())
	}
}

// TestHubChildExitsWhenIdle (P12): a hub nobody uses — Ensure's own hello
// closed, no host in the registry — exits after its grace (CRAZE_HUB_IDLE),
// and its files go with it.
func TestHubChildExitsWhenIdle(t *testing.T) {
	env, _ := serveHome(t)
	t.Setenv(hub.IdleEnv, "1s")
	hubAsChild(t)
	sock, rec := ensureHub(t, env)
	log := hubLog(env)
	waitHubGone(t, rec.PID, log)
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket is still there: %v", err)
	}
	if !strings.Contains(log(), "stopping: idle") {
		t.Fatalf("the hub's log does not say it stopped idle: %s", log())
	}
}

// TestHubChildStaysWhileAHostLives (P12): a live host in the HOME registry
// keeps the hub past its grace; once the host has gone, the hub's next read of
// the registry starts the grace, and it exits.
func TestHubChildStaysWhileAHostLives(t *testing.T) {
	env, _ := serveHome(t)
	t.Setenv(hub.IdleEnv, "1s")
	host, err := rundir.Bind(env, rundir.NewHostID(), rundir.Entry{CrazeSessionID: "s-host"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	hubAsChild(t)
	_, rec := ensureHub(t, env)
	log := hubLog(env)
	// Three graces with the host up: the hub stays.
	time.Sleep(3 * time.Second)
	if !hubAlive(t, rec.PID) {
		t.Fatalf("the hub exited with a live host in the registry: %s", log())
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	waitHubGone(t, rec.PID, log)
	if !strings.Contains(log(), "stopping: idle") {
		t.Fatalf("the hub's log does not say it stopped idle: %s", log())
	}
}

// TestAnOrphanedHubChildExits (serve_child_test.go's watchdog, for hubs): a
// craze hub child — run by hand here, no ready pipe, its grace an hour —
// whose parent, the test binary, is gone (a shell playing it exits) is ended
// by its watchdog: it tears down as on SIGTERM and leaves no record.
func TestAnOrphanedHubChildExits(t *testing.T) {
	env, _ := serveHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal([]string{"hub"})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "child.log")
	sh := exec.Command("/bin/sh", "-c",
		`CRAZE_CLI_TEST_PARENT=$$ "$0" -test.run='^$' </dev/null >"$1" 2>&1 & echo $!; read _`, exe, logPath)
	sh.Env = append(os.Environ(), cliChildEnv+"="+string(argv), hub.IdleEnv+"=1h")
	stdin, err := sh.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sh.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	shDone := make(chan struct{})
	go func() { _ = sh.Wait(); close(shDone) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		<-shDone
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the shell's pid line: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("the shell's pid line %q: %v", line, err)
	}
	t.Cleanup(func() {
		if hubAlive(t, pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	output := func() string { b, _ := os.ReadFile(logPath); return string(b) + hubLog(env)() }
	deadline := time.Now().Add(serveStep)
	for {
		rec, _, err := rundir.ReadHubRecord(env)
		if err == nil && rec.PID == pid {
			break
		}
		if !hubAlive(t, pid) || time.Now().After(deadline) {
			t.Fatalf("the hub child %d did not serve within %v: %s", pid, serveStep, output())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = stdin.Close()
	select {
	case <-shDone:
	case <-time.After(serveStep):
		t.Fatal("the shell did not exit when its stdin closed")
	}
	deadline = time.Now().Add(serveStep)
	for hubAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the orphaned hub %d is still running after %v: %s", pid, serveStep, output())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := output(); !strings.Contains(out, "stopping: SIGTERM") {
		t.Fatalf("the orphan did not tear down: %s", out)
	}
	if _, _, err := rundir.ReadHubRecord(env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the orphan left its record: %v", err)
	}
}
