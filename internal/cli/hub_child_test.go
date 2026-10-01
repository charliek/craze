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
	prev := hub.Command
	hub.Command = func(argv []string) (*exec.Cmd, error) {
		if !slices.Equal(argv, []string{"hub"}) {
			return nil, fmt.Errorf("the hub's command line is %q", argv)
		}
		// -test.run matches nothing, so a child whose init fell through runs
		// no test.
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(childEnv(argvJSON), extra...)
		kids.mu.Lock()
		kids.cmds = append(kids.cmds, cmd)
		kids.mu.Unlock()
		return cmd, nil
	}
	t.Cleanup(func() {
		hub.Command = prev
		for _, pid := range kids.pids() {
			if !hubAlive(pid) {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGTERM)
			deadline := time.Now().Add(serveStep)
			for hubAlive(pid) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if hubAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("hub child %d did not exit within %v of SIGTERM; killed", pid, serveStep)
			}
		}
	})
	return kids
}

// hubAlive reports whether pid has not exited: not gone, and not a zombie
// its reaper has not taken yet.
func hubAlive(pid int) bool {
	return processAlive(pid) && procState(pid) != "Z"
}

// waitHubGone waits for pid to exit, within serveStep.
func waitHubGone(t *testing.T, pid int, log func() string) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for hubAlive(pid) {
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
	if !hubAlive(rec.PID) {
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
		if hubAlive(pid) {
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
		if !hubAlive(pid) || time.Now().After(deadline) {
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
	for processAlive(pid) && procState(pid) != "Z" {
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
