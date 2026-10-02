package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"golang.org/x/sys/unix"
)

// The hub's tests (plan 032 §3.18). In process, a hub is Run on a goroutine
// of the test's with its schedule in the test's hands (hooks): its tickers,
// its idle grace, the instants around its decisions. Where a real process is
// the point — the namespace lock between processes, a hub that does not
// answer, a SIGSTOPped hub — this test binary is re-executed as a hub (init
// below, hubTestChild), the way internal/cli's serve_child_test.go runs craze.
//
// Every wait is bounded on its own (step), never one deadline for a whole
// test, and the schedules are forced, never waited out: each lifecycle test
// is also run under a 5% CPU quota (bin/starve-pkg.sh).

// step bounds each wait of a test. It is generous because these tests run
// starved too, where a hub child's start takes seconds.
const step = 30 * time.Second

// The re-executed child's environment.
const (
	// hubTestChild runs this test binary as a hub (init).
	hubTestChild = "CRAZE_HUB_TEST_CHILD"
	// hubTestParent is the test process's pid: the child's watchdog ends it
	// once that process is no longer its parent.
	hubTestParent = "CRAZE_HUB_TEST_PARENT"
	// hubTestReady is what the child does when its ready line is due:
	// "block" parks it there for good, its record written and its socket
	// bound.
	hubTestReady = "CRAZE_HUB_TEST_READY"
	// hubTestHold, a duration, runs the hub off the child's main thread and
	// holds that thread for so long at each SIGUSR1 (holdThread): a SIGSTOP
	// sent during a hold stops the child only once the hold ends.
	hubTestHold = "CRAZE_HUB_TEST_HOLD"
	// hubTestStall, a directory, stalls the child's teardown just before it
	// releases the lock — its record and socket gone — until the test lets
	// it go (stallAt).
	hubTestStall = "CRAZE_HUB_TEST_STALL"
	// hubTestPIDFile is a path the child writes its pid to as it starts.
	hubTestPIDFile = "CRAZE_HUB_TEST_PIDFILE"
	// hubTestStderrDir is a directory the child writes its own stderr into,
	// as <dir>/<pid>: a spawned hub's stderr is /dev/null, and a SIGQUIT's
	// goroutine dump goes to stderr — so a child that will not exit can say
	// where it is parked (asChildren, endProcess; plan 032 X58).
	hubTestStderrDir = "CRAZE_HUB_TEST_STDERR_DIR"
)

// The child's watchdog (internal/cli's childWatchdog): a test binary that dies
// first must not leave a hub running. Once the test process is no longer its
// parent the child SIGTERMs itself — a hub that can act tears down — and
// exits childTermGrace later whatever it is doing; childLifetime is the
// backstop.
const (
	childWatchEvery = 100 * time.Millisecond
	childTermGrace  = 5 * time.Second
	childLifetime   = 30 * time.Minute
)

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
		os.Exit(98)
	}
}

func init() {
	if _, ok := os.LookupEnv(hubTestChild); !ok {
		return
	}
	_ = os.Unsetenv(hubTestChild)
	if dir, ok := os.LookupEnv(hubTestStderrDir); ok {
		_ = os.Unsetenv(hubTestStderrDir)
		if f, err := os.OpenFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			_ = unix.Dup2(int(f.Fd()), 2)
		}
	}
	parent := os.Getppid()
	if v, ok := os.LookupEnv(hubTestParent); ok {
		_ = os.Unsetenv(hubTestParent)
		if n, err := strconv.Atoi(v); err == nil && n > 1 {
			parent = n
		}
	}
	go childWatchdog(parent)
	if p := os.Getenv(hubTestPIDFile); p != "" {
		_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	hk := &hooks{}
	if os.Getenv(hubTestReady) == "block" {
		hk.beforeReady = func() { select {} }
	}
	if dir := os.Getenv(hubTestStall); dir != "" {
		hk.beforeRelease = func() { stallAt(dir) }
	}
	hold, _ := time.ParseDuration(os.Getenv(hubTestHold))
	for _, k := range []string{hubTestReady, hubTestStall, hubTestPIDFile, hubTestHold} {
		_ = os.Unsetenv(k)
	}
	ready, err := TakeReadyPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hub test child:", err)
		os.Exit(97)
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	grace, _ := IdleGraceFromEnv(os.Getenv)
	run := func() {
		err := Run(context.Background(), Options{Env: rundir.ProcessEnv(), Ready: ready, Signals: sigs,
			Stderr: os.Stderr, IdleGrace: grace, hooks: hk})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if hold <= 0 {
		run()
	}
	// init's goroutine has the main thread (the runtime locks it there for
	// init, which never returns here): it keeps that thread for the holds,
	// and the hub runs on others. No GC either: its stop-the-world would wait
	// out a hold, and stop the hub with it.
	runtime.LockOSThread()
	debug.SetGCPercent(-1)
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go run()
	for range usr1 {
		holdThread(hold)
	}
}

// stallAt is the child's teardown held just before its Release
// (hubTestStall): it says so (dir/stalled) and waits until the test lets it
// go (dir/go). The watchdog ends a child whose test has gone.
func stallAt(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "stalled"), nil, 0o600)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMain isolates the package: a test that forgot its own HOME, CRAZE_HOME
// or runtime directory would otherwise reach the developer's own hub lock,
// record and socket.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "czhub")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	_ = os.Setenv("HOME", filepath.Join(dir, "home"))
	_ = os.Setenv("CRAZE_HOME", filepath.Join(dir, "craze"))
	_ = os.Setenv("CRAZE_RUNTIME_DIR", filepath.Join(dir, "run"))
	_ = os.Unsetenv("XDG_RUNTIME_DIR")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// shortDir is a fresh 0700 directory under the real /tmp, short enough for
// sun_path on macOS too.
func shortDir(t *testing.T, prefix string) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// testEnv is an isolated namespace for an in-process hub: its own 0700 HOME
// (the cache tree), CRAZE_HOME and runtime base.
func testEnv(t *testing.T) rundir.Env {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"),
		CrazeRuntimeDir: shortDir(t, "czh"), EUID: os.Geteuid()}
}

// processEnv is testEnv exported to this process's environment — HOME,
// CRAZE_HOME, CRAZE_RUNTIME_DIR — so a hub child spawned now inherits it:
// rundir.ProcessEnv(), which the child reads.
func processEnv(t *testing.T) rundir.Env {
	t.Helper()
	e := testEnv(t)
	t.Setenv("HOME", e.Home)
	t.Setenv("CRAZE_HOME", e.CrazeDir)
	t.Setenv("CRAZE_RUNTIME_DIR", e.CrazeRuntimeDir)
	env := rundir.ProcessEnv()
	if env.Home != e.Home || env.CrazeDir != e.CrazeDir || env.CrazeRuntimeDir != e.CrazeRuntimeDir {
		t.Fatalf("the process environment reads %+v, want %+v", env, e)
	}
	return env
}

// nsOf is env's namespace.
func nsOf(t *testing.T, env rundir.Env) string {
	t.Helper()
	ns, err := rundir.Namespace(env.CrazeDir)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

// hubsDir is env's hubs directory, made as LockHub makes it when it is
// missing.
func hubsDir(t *testing.T, env rundir.Env) string {
	t.Helper()
	if dir := filepath.Join(env.Home, ".cache", "craze", "hubs"); dirExists(dir) {
		return dir
	}
	l, err := rundir.LockHub(env, rundir.NewHubID())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(l.Path())
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// recordPath is env's hub record's path.
func recordPath(t *testing.T, env rundir.Env) string {
	t.Helper()
	return filepath.Join(env.Home, ".cache", "craze", "hubs", nsOf(t, env)+".json")
}

// writeRecord writes rec as env's hub record, as a hub would (a temporary,
// then a rename), NS filled in.
func writeRecord(t *testing.T, env rundir.Env, rec rundir.HubRecord) {
	t.Helper()
	dir := hubsDir(t, env)
	rec.NS = nsOf(t, env)
	if rec.Protocol == 0 {
		rec.Protocol = protocol.ProtocolVersion
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".rec.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, rec.NS+".json")); err != nil {
		t.Fatal(err)
	}
}

// lockFree checks env's hub lock can be taken now — and lets it go.
func lockFree(t *testing.T, env rundir.Env) {
	t.Helper()
	l, err := rundir.LockHub(env, rundir.NewHubID())
	if err != nil {
		t.Fatalf("the hub lock is not free: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

// lockHeld checks another holds env's hub lock now.
func lockHeld(t *testing.T, env rundir.Env) {
	t.Helper()
	l, err := rundir.LockHub(env, rundir.NewHubID())
	var held *rundir.HubHeldError
	if !errors.As(err, &held) {
		if err == nil {
			_ = l.Release()
		}
		t.Fatalf("the hub lock is not held: LockHub = %v", err)
	}
}

// absent checks nothing is at path.
func absent(t *testing.T, what, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s %s is still there (%v)", what, path, err)
	}
}

// lockedBuffer is a bytes.Buffer safe for the hub's writes and the test's
// reads at once.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// quiet is a schedule with no ticker that ever fires and no grace that ends
// on its own: a test drives whichever it needs.
func quiet() *hooks {
	return &hooks{
		lostTick:  make(chan time.Time),
		hostsTick: make(chan time.Time),
		sweepTick: make(chan time.Time),
		graceTimer: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
	}
}

// grace is a test's hand on a hub's idle grace: every arming hands the loop
// the same unbuffered channel, so a fire (fire) is taken only by a loop that
// has a grace armed — the end of whichever grace is armed then — and each
// arming is noted (armed).
type grace struct {
	ch    chan time.Time
	armed chan struct{}
}

// install makes hk's grace the test's.
func (g *grace) install(hk *hooks) {
	g.ch, g.armed = make(chan time.Time), make(chan struct{}, 1024)
	hk.graceTimer = func(time.Duration) (<-chan time.Time, func()) {
		g.armed <- struct{}{}
		return g.ch, func() {}
	}
}

// fire ends the grace the hub has armed, waiting up to step for it to have
// one.
func (g *grace) fire(t *testing.T) {
	t.Helper()
	select {
	case g.ch <- time.Now():
	case <-time.After(step):
		t.Fatalf("the hub had no idle grace armed within %v", step)
	}
}

// none checks the hub has armed no grace since the last look.
func (g *grace) none(t *testing.T) {
	t.Helper()
	select {
	case <-g.armed:
		t.Fatal("the hub armed an idle grace")
	default:
	}
}

// running is a hub Run in this process.
type running struct {
	sigs   chan os.Signal
	stderr *lockedBuffer
	ready  chan []byte // its ready line, once; closed with none
	done   chan struct{}
	err    error // Run's, once done is closed
	// returned is when Run returned, taken on Run's own goroutine before done
	// is closed: a teardown's length, whenever the test next runs.
	returned time.Time
	h        chan *hub
}

// runIn runs a hub over env with hk's schedule (nil: production's), its
// ready line read from a pipe of the test's. One still running when the test
// ends is sent SIGTERM and waited for.
func runIn(t *testing.T, env rundir.Env, hk *hooks) *running {
	t.Helper()
	return runWith(t, env, hk, nil)
}

// runWith is runIn with session.create's options (nil: a hub that creates
// nothing).
func runWith(t *testing.T, env rundir.Env, hk *hooks, creates *Creates) *running {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rn := &running{sigs: make(chan os.Signal, 4), stderr: &lockedBuffer{}, ready: make(chan []byte, 1),
		done: make(chan struct{}), h: make(chan *hub, 1)}
	if hk == nil {
		hk = &hooks{}
	}
	prev := hk.serving
	hk.serving = func(h *hub) {
		if prev != nil {
			prev(h)
		}
		rn.h <- h
	}
	go func() {
		defer r.Close()
		raw, failure, _ := readLine(r)
		if failure != "" {
			close(rn.ready)
			return
		}
		rn.ready <- raw
	}()
	go func() {
		rn.err = Run(context.Background(), Options{Env: env, Ready: NewReadyPipe(w), Signals: rn.sigs,
			Stderr: rn.stderr, IdleGrace: time.Hour, Codecs: protocol.Codecs{Event: 1, Snapshot: 1}, Creates: creates, hooks: hk})
		rn.returned = time.Now()
		close(rn.done)
	}()
	t.Cleanup(func() {
		select {
		case <-rn.done:
			return
		default:
		}
		rn.sigs <- syscall.SIGTERM
		select {
		case <-rn.done:
		case <-time.After(step):
			t.Errorf("the hub did not stop within %v of SIGTERM; it said: %s", step, rn.stderr)
		}
	})
	return rn
}

// readLine reads one line from r, as hostspawn.ReadLine does: "" failure
// for a line.
func readLine(r io.Reader) ([]byte, string, string) {
	var buf []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return buf, "", ""
			}
			buf = append(buf, b[0])
		}
		if err != nil {
			return nil, "eof", err.Error()
		}
	}
}

// line is the hub's ready line, within step.
func (rn *running) line(t *testing.T) ReadyLine {
	t.Helper()
	select {
	case raw, ok := <-rn.ready:
		if !ok {
			t.Fatalf("the hub closed its ready pipe with no line; it said: %s", rn.stderr)
		}
		line, bad := parseReady(raw)
		if bad != "" {
			t.Fatalf("the hub's ready line %q: %s", raw, bad)
		}
		return line
	case <-time.After(step):
		t.Fatalf("no ready line within %v; the hub said: %s", step, rn.stderr)
	}
	return ReadyLine{}
}

// serving is the hub once it serves, within step.
func (rn *running) serving(t *testing.T) *hub {
	t.Helper()
	select {
	case h := <-rn.h:
		rn.h <- h
		return h
	case <-rn.done:
		t.Fatalf("the hub returned (%v) before it served; it said: %s", rn.err, rn.stderr)
	case <-time.After(step):
		t.Fatalf("the hub did not serve within %v; it said: %s", step, rn.stderr)
	}
	return nil
}

// stopped waits for Run to return, within step.
func (rn *running) stopped(t *testing.T) error {
	t.Helper()
	select {
	case <-rn.done:
		return rn.err
	case <-time.After(step):
		t.Fatalf("the hub has not stopped within %v; it said: %s", step, rn.stderr)
		return nil
	}
}

// isRunning reports whether Run has not returned.
func (rn *running) isRunning() bool {
	select {
	case <-rn.done:
		return false
	default:
		return true
	}
}

// client is a raw connection to a hub.
type client struct {
	nc net.Conn
	lr *protocol.LineReader
	id int
}

// dial connects to the hub at sock, within step.
func dial(t *testing.T, sock string) *client {
	t.Helper()
	nc, err := net.DialTimeout("unix", sock, step)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return &client{nc: nc, lr: protocol.NewLineReader(nc, protocol.OutboundLineMax)}
}

// call sends method with params and reads the answer, each within step.
func (c *client) call(t *testing.T, method string, params any) protocol.Response {
	t.Helper()
	c.id++
	req := map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	_ = c.nc.SetDeadline(time.Now().Add(step))
	defer func() { _ = c.nc.SetDeadline(time.Time{}) }()
	if err := protocol.WriteLine(c.nc, req); err != nil {
		t.Fatalf("%s: write: %v", method, err)
	}
	line, err := c.lr.ReadLine()
	if err != nil {
		t.Fatalf("%s: read: %v", method, err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("%s: %q: %v", method, line, err)
	}
	return resp
}

// hello says hello and answers the hub's result.
func (c *client) hello(t *testing.T) protocol.HubHelloResult {
	t.Helper()
	resp := c.call(t, protocol.MethodHello, protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}})
	if resp.Error != nil {
		t.Fatalf("hello refused: %v", resp.Error)
	}
	var res protocol.HubHelloResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// waitFor polls cond every 5 ms until it holds, within step.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(step)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, step)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// clients is how many clients h counts now.
func clients(h *hub) int {
	h.life.mu.Lock()
	defer h.life.mu.Unlock()
	return h.life.clients
}

// ------------------------------------------------------------ real children

// children is every hub child a test spawned (asChildren).
type children struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
	n    atomic.Int32
}

// pids is every spawned child's pid that has started. Read only once every
// Ensure that spawned them has returned to the test: a Cmd's Process is its
// Start's until then.
func (c *children) pids() []int {
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

// asChildren installs Command for one test: every hub Ensure spawns is this
// test binary run as a hub (init), in the test's environment plus extra(n)
// for the n-th, counting from 1. Every child is ended when the test ends —
// SIGCONT, SIGTERM, then SIGKILL — and the test fails if one will not go.
func asChildren(t *testing.T, extra func(n int) []string) *children {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &children{}
	// Each child's own stderr, for the dump a hung one is asked for
	// (registered before the cleanup that reads it, so removed after it).
	stderrs := t.TempDir()
	prev := Command
	Command = func(argv []string) (*exec.Cmd, error) {
		if len(argv) != 1 || argv[0] != "hub" {
			return nil, fmt.Errorf("the hub's command line is %q, want [hub]", argv)
		}
		n := int(c.n.Add(1))
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), hubTestChild+"=1", hubTestParent+"="+strconv.Itoa(os.Getpid()),
			hubTestStderrDir+"="+stderrs)
		if extra != nil {
			cmd.Env = append(cmd.Env, extra(n)...)
		}
		c.mu.Lock()
		c.cmds = append(c.cmds, cmd)
		c.mu.Unlock()
		return cmd, nil
	}
	t.Cleanup(func() {
		Command = prev
		for _, pid := range c.pids() {
			endProcess(t, pid, stderrs)
		}
	})
	return c
}

// noCommand installs a Command that fails the test if Ensure spawns.
func noCommand(t *testing.T) {
	t.Helper()
	prev := Command
	Command = func([]string) (*exec.Cmd, error) {
		t.Error("Ensure spawned a hub")
		return nil, errors.New("no spawn in this test")
	}
	t.Cleanup(func() { Command = prev })
}

// alive reports whether pid is a process that has not exited: not gone, and
// not a zombie.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return procState(pid) != "Z"
}

// procState is pid's state letter from /proc on Linux, "?" elsewhere.
func procState(pid int) string {
	return statState(fmt.Sprintf("/proc/%d/stat", pid))
}

// taskState is the state letter of pid's thread tid, as procState.
func taskState(pid, tid int) string {
	return statState(fmt.Sprintf("/proc/%d/task/%d/stat", pid, tid))
}

// statState is the state letter in a /proc stat file, "?" when it cannot be
// read.
func statState(path string) string {
	b, err := os.ReadFile(path)
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

// endProcess ends pid — a child of this test, its stderr in dir/<pid>
// (hubTestStderrDir) — and fails the test if it will not go: SIGCONT and
// SIGTERM, step to exit, then SIGKILL. One that has not gone within step
// says first where it is — /proc's state and signal masks, then the
// goroutine dump a SIGQUIT writes to its stderr — before its SIGKILL (plan
// 032 X58: a hub child ignored its SIGTERM for the whole step, twice, on
// loaded boxes).
func endProcess(t *testing.T, pid int, dir string) {
	t.Helper()
	if !alive(pid) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGCONT)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(step)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		state := procSignalState(pid)
		var dump []byte
		if dir != "" {
			_ = syscall.Kill(pid, syscall.SIGQUIT)
			quit := time.Now().Add(5 * time.Second)
			for alive(pid) && time.Now().Before(quit) {
				time.Sleep(10 * time.Millisecond)
			}
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
		if dir != "" {
			dump, _ = os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)))
		}
		t.Errorf("hub child %d did not exit within %v of SIGTERM; killed\n%s\n--- its stderr, a SIGQUIT's dump:\n%s", pid, step, state, dump)
	}
}

// procSignalState is what /proc says of pid's state and signals — State,
// SigQ, SigPnd, ShdPnd, SigBlk, SigIgn, SigCgt, and its wait channel — for a
// failure's message; "" where there is no /proc.
func procSignalState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return ""
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

// waitStopped waits for pid to be stopped (SIGSTOP), within step. kill(2)
// returns once the signal is queued; on Linux a multi-threaded process stops
// when the thread the kernel woke for the signal — its main one, when it can
// take it — runs and stops the others, and until then they run on: on a
// loaded machine a hub answers a hello a millisecond after its SIGSTOP was
// sent, and a main thread that cannot take the signal yet (holdThread) keeps
// the rest serving for as long as that lasts. So on Linux every thread's
// state in /proc is the judge (allStopped). macOS suspends the whole task
// before kill returns; ps's state is enough there.
func waitStopped(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("every thread of process %d stops", pid), func() bool {
		if runtime.GOOS == "linux" {
			return allStopped(pid)
		}
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "T")
	})
}

// allStopped reports whether every thread of pid is stopped (/proc's T) or
// has exited.
func allStopped(pid int) bool {
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return false
	}
	for _, task := range tasks {
		tid, err := strconv.Atoi(task.Name())
		if err != nil {
			continue
		}
		switch taskState(pid, tid) {
		case "T", "Z", "X", "?": // "?": gone since the listing
		default:
			return false
		}
	}
	return true
}

// waitGone waits for pid to have exited (gone, or a zombie its reaper has not
// taken yet), within step.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("process %d exits", pid), func() bool { return !alive(pid) })
}

// sleeper is a process of the test's own that does nothing — a pid a record
// can name — killed when the test ends.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid
}

// token is pid's start token now.
func token(t *testing.T, pid int) string {
	t.Helper()
	tok, err := rundir.StartToken(pid)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// decoy is a listener at a socket of the test's that plays a hub: how it
// answers is mode's.
type decoy struct {
	path     string
	accepted atomic.Int32
}

// The decoy's modes.
const (
	// decoyEOF reads the hello and closes the connection: EOF, not a reset,
	// for its reader.
	decoyEOF = "eof"
	// decoyClosing answers hello unavailable, reason closing.
	decoyClosing = "closing"
	// decoySilent accepts and never answers.
	decoySilent = "silent"
	// decoyAnswer answers hello with the decoy's result.
	decoyAnswer = "answer"
)

// newDecoy listens at a fresh path in mode; result is decoyAnswer's.
func newDecoy(t *testing.T, mode string, result protocol.HubHelloResult) *decoy {
	t.Helper()
	d := &decoy{path: filepath.Join(shortDir(t, "czd"), "hub.sock")}
	ln, err := net.Listen("unix", d.path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.accepted.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			switch mode {
			case decoySilent:
			default:
				go func() {
					line, err := protocol.NewLineReader(c, 0).ReadLine()
					if err != nil || mode == decoyEOF {
						_ = c.Close()
						return
					}
					var req protocol.Request
					_ = json.Unmarshal(line, &req)
					resp := protocol.Response{JSONRPC: "2.0", ID: req.ID}
					if mode == decoyClosing {
						resp.Error = refused(protocol.CodeUnavailable, protocol.ReasonClosing, "closing")
					} else {
						resp.Result, _ = json.Marshal(result)
					}
					_ = protocol.WriteLine(c, resp)
				}()
			}
		}
	}()
	return d
}

// hubResult is a hub's hello result for a decoy: hubID's, craze v, caps.
func hubResult(hubID, v string, caps protocol.ConnectionCapabilities) protocol.HubHelloResult {
	return protocol.HubHelloResult{Protocol: 1,
		Endpoint:     protocol.Endpoint{Kind: protocol.EndpointHub, HostID: hubID, CrazeVersion: v, PID: 1},
		Capabilities: caps, Limits: protocol.HostLimits()}
}

// setVar sets *p to v for one test (never in parallel).
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}
