package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/sessions"
)

// The test binary as craze itself (plan 030 C2): a test that needs a host in
// a process of its own — real signals, a real exit code — re-executes this
// binary with cliChildEnv set to the command line as JSON, and init below runs
// it through the root command exactly as Execute does, before TestMain or any
// test. No craze binary is built for it, so the same test runs under a CPU
// quota without a link step inside the quota.

// cliChildEnv carries the child's argv (a JSON array, without "craze").
const cliChildEnv = "CRAZE_CLI_TEST_CHILD"

// The child's test hooks, each set in its environment by the test that wants
// it, and read by init before the command runs:
const (
	// cliChildPanic makes craze serve panic with this message once it serves
	// (serveServing), on its own goroutine: the crash-output test.
	cliChildPanic = "CRAZE_CLI_TEST_PANIC"
	// cliChildLogMax is the host log's rotation size in bytes (hostLogMax).
	cliChildLogMax = "CRAZE_CLI_TEST_LOG_MAX"
	// cliChildReady is what a spawned craze serve does when its ready line is
	// due (serveAnnouncing): "skip" writes nothing and serves on, its pipe
	// open; "block" parks craze serve's goroutine for good, so a SIGTERM is
	// never acted on; "malformed" writes a line that is not JSON;
	// "oversized" writes one longer than readyLineMax. The spawn tests.
	cliChildReady = "CRAZE_CLI_TEST_READY"
	// cliChildGate is a FIFO a loading craze serve reads one byte from once
	// its session is claimed, before its socket binds (serveClaimed): the
	// held rendezvous's holder, held where it is claimed and not yet in the
	// registry.
	cliChildGate = "CRAZE_CLI_TEST_GATE"
	// cliChildRowGate is a FIFO a loading craze serve reads one byte from
	// once it has read its row, before it gives the row an id or claims it
	// (serveRowRead, the barrier holdLoaders makes of it in process), having
	// first written the row it read, as JSON, to <FIFO>.row: the session
	// list's two resumes of one legacy row at once, each host held there
	// until both are (rowGate).
	cliChildRowGate = "CRAZE_CLI_TEST_ROW_GATE"
	// cliChildNoIdle stops craze serve's idle watcher from ever looking
	// (idleTicks): a test about what a spawner does to a host whose socket it
	// removed must not race the host's own socket-lost stop.
	cliChildNoIdle = "CRAZE_CLI_TEST_NO_IDLE"
	// cliChildParent is the pid of the test process that started the child
	// (childEnv sets it): the child's watchdog ends the child once that
	// process is no longer its parent (childWatchdog).
	cliChildParent = "CRAZE_CLI_TEST_PARENT"
)

// The child's watchdog (plan 030 C5r): a child of the test binary can park —
// "block" never returns from its ready line and never acts on SIGTERM, the
// gates wait on FIFOs nobody may write, no-idle and skip serve on for good —
// and its own test ends it only if that test lives to run its cleanup. A test
// binary that dies first (its -timeout's panic, a kill) left one such host
// running for hours, reparented to init. So every child watches its parent:
// once the test process that started it is no longer its parent it SIGTERMs
// itself — a host that can still act runs its ordinary stop sequence, closing
// its agent and leaving no registry entry — and exits childTermGrace later
// whatever it is doing. childLifetime is the backstop behind that, longer
// than any test binary's timeout (make test-race's is 20 minutes).
const (
	childWatchEvery = 100 * time.Millisecond
	childTermGrace  = 5 * time.Second
	childLifetime   = 30 * time.Minute
	// childWatchdogExit is the exit code of a child its watchdog ended.
	childWatchdogExit = 98
)

// childWatchdog ends this child once parent is no longer its parent, or once
// it has lived childLifetime (the constants' comment). It runs for the
// child's life on a goroutine of its own, which a parked main goroutine does
// not stop.
func childWatchdog(parent int) {
	born := time.Now()
	tick := time.NewTicker(childWatchEvery)
	defer tick.Stop()
	for range tick.C {
		if os.Getppid() == parent && time.Since(born) < childLifetime {
			continue
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(childTermGrace)
		os.Exit(childWatchdogExit)
	}
}

// childEnv is the environment a test starts a craze child with: its own, argv
// (the JSON the child's init runs), and this process as the child's parent.
func childEnv(argv []byte) []string {
	return append(os.Environ(), cliChildEnv+"="+string(argv), cliChildParent+"="+strconv.Itoa(os.Getpid()))
}

func init() {
	raw, ok := os.LookupEnv(cliChildEnv)
	if !ok {
		return
	}
	// Not the agent's to inherit: the fake agent is another binary, but a
	// grandchild of this one must never run as craze by accident.
	_ = os.Unsetenv(cliChildEnv)
	// The watchdog first, before anything can park: the parent its test named,
	// else the parent this process has now.
	parent := os.Getppid()
	if v, ok := os.LookupEnv(cliChildParent); ok {
		_ = os.Unsetenv(cliChildParent)
		if n, err := strconv.Atoi(v); err == nil && n > 1 {
			parent = n
		}
	}
	go childWatchdog(parent)
	if msg, ok := os.LookupEnv(cliChildPanic); ok {
		_ = os.Unsetenv(cliChildPanic)
		serveServing = func() { panic(msg) }
	}
	if size, ok := os.LookupEnv(cliChildLogMax); ok {
		_ = os.Unsetenv(cliChildLogMax)
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "craze test child: bad", cliChildLogMax, size)
			os.Exit(97)
		}
		hostLogMax = n
	}
	if mode, ok := os.LookupEnv(cliChildReady); ok {
		_ = os.Unsetenv(cliChildReady)
		serveAnnouncing = childAnnouncing(mode)
	}
	if fifo, ok := os.LookupEnv(cliChildGate); ok {
		_ = os.Unsetenv(cliChildGate)
		serveClaimed = func() {
			f, err := os.Open(fifo)
			if err == nil {
				_, _ = f.Read(make([]byte, 1))
				_ = f.Close()
			}
		}
	}
	if fifo, ok := os.LookupEnv(cliChildRowGate); ok {
		_ = os.Unsetenv(cliChildRowGate)
		serveRowRead = func(row sessions.Row) {
			// Renamed into place, so the test never reads half of it.
			if b, err := json.Marshal(row); err == nil && os.WriteFile(fifo+".tmp", b, 0o600) == nil {
				_ = os.Rename(fifo+".tmp", fifo+".row")
			}
			f, err := os.Open(fifo)
			if err == nil {
				_, _ = f.Read(make([]byte, 1))
				_ = f.Close()
			}
		}
	}
	if _, ok := os.LookupEnv(cliChildNoIdle); ok {
		_ = os.Unsetenv(cliChildNoIdle)
		idleTicks = func() (<-chan time.Time, func() time.Time, func()) { return nil, time.Now, func() {} }
	}
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		fmt.Fprintln(os.Stderr, "craze test child:", err)
		os.Exit(97)
	}
	cmd := NewRootCmd()
	cmd.SetArgs(argv)
	ranCmd, err := cmd.ExecuteC()
	if err == nil {
		os.Exit(0)
	}
	line, code := diagnose(ranCmd, err)
	if line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	os.Exit(code)
}

// childAnnouncing is serveAnnouncing for cliChildReady's mode.
func childAnnouncing(mode string) func(*readyPipe) bool {
	return func(p *readyPipe) bool {
		f := p.file()
		switch mode {
		case "skip":
		case "block":
			select {}
		case "malformed":
			_, _ = f.WriteString("this is not a ready line\n")
			p.close()
		case "oversized":
			// Written whole or not at all: past what the spawner reads, the
			// write blocks until the spawner closes its end, and fails.
			_, _ = f.WriteString(`{"ok":true,"pad":"` + strings.Repeat("x", readyLineMax) + "\"}\n")
			p.close()
		default:
			fmt.Fprintln(os.Stderr, "craze test child: bad", cliChildReady, mode)
			os.Exit(97)
		}
		return true
	}
}

// crazeChild is one craze run in a process of its own.
type crazeChild struct {
	cmd    *exec.Cmd
	output *lockedBuffer
	done   chan struct{}
	err    error // cmd.Wait's, once done is closed
}

// startCrazeChild starts `craze <argv...>` as a child of the test, in the
// test's environment (HOME, CRAZE_HOME and the rest as the test set them),
// stdout and stderr together in output. A child still running when the test
// ends is sent SIGTERM, then SIGKILL, so no host outlives its test.
func startCrazeChild(t *testing.T, argv ...string) *crazeChild {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	// -test.run matches nothing, so a child whose init somehow fell through
	// runs no test.
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = childEnv(b)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &crazeChild{cmd: cmd, output: out, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(func() {
		select {
		case <-c.done:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(serveStep):
			_ = cmd.Process.Kill()
			<-c.done
		}
	})
	return c
}

// wait is the child's exit code once it has exited, within d.
func (c *crazeChild) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(d):
		t.Fatalf("the child has not exited after %v; output: %s", d, c.output)
	}
	if c.err == nil {
		return 0
	}
	if ee, ok := c.err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("waiting for the child: %v", c.err)
	return -1
}

// running reports whether the child has not exited yet.
func (c *crazeChild) running() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// TestAnOrphanedTestChildExits (plan 030 C5r): a craze serve child of this
// test binary whose idle watcher never looks (cliChildNoIdle) — a host that
// would serve for ever — is started through a shell that names itself the
// child's parent and exits once the host serves, as a test binary that dies
// does. The child's watchdog sees its parent gone and SIGTERMs it: the host
// runs its stop sequence and exits, leaving nothing in the registry.
func TestAnOrphanedTestChildExits(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal([]string{"serve", "--agent-bin", fakeAgentPath(t), "--workspace", ws})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "child.log")
	// The shell starts the child with itself as the parent the child
	// watches, prints its pid, and exits when its stdin closes.
	sh := exec.Command("/bin/sh", "-c",
		`CRAZE_CLI_TEST_PARENT=$$ "$0" -test.run='^$' </dev/null >"$1" 2>&1 & echo $!; read _`, exe, logPath)
	sh.Env = append(os.Environ(), cliChildEnv+"="+string(argv), cliChildNoIdle+"=1")
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
	gone := func() bool { return !processAlive(pid) || procState(pid) == "Z" }
	t.Cleanup(func() {
		if !gone() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	output := func() string { b, _ := os.ReadFile(logPath); return string(b) }
	e := waitServingEntry(t, env, true, func() error {
		if gone() {
			return fmt.Errorf("the child exited: %s", output())
		}
		return nil
	}, &lockedBuffer{})
	if e.PID != pid {
		t.Fatalf("the serving host is pid %d, want the child %d", e.PID, pid)
	}

	_ = stdin.Close()
	select {
	case <-shDone:
	case <-time.After(serveStep):
		t.Fatal("the shell did not exit when its stdin closed")
	}
	deadline := time.Now().Add(serveStep)
	for !gone() {
		if time.Now().After(deadline) {
			t.Fatalf("the orphaned child %d is still there after %v (%s); output: %s", pid, serveStep, procState(pid), output())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := output(); !strings.Contains(out, "craze serve: stopping: SIGTERM") {
		t.Fatalf("the orphan did not run its stop sequence: %s", out)
	}
	assertNoHosts(t, env)
}
