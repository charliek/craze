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
			exitChild(1)
		}
		exitChild(0)
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

// exitChild ends a test child with code through the exit system call, not
// os.Exit: a child's exit status is all its test reads, so it owes no race
// summary (under -race, os.Exit first runs the race runtime's finalizer) and
// runs no exit hook. It came from plan 032 X67, which took "hub child did not
// exit within 30s" for a child slow to exit under -race. It was not: the
// sightings lasted 1–2.5 s in all, and the report was alive's own — a child
// reaped between its kill(pid, 0) and its /proc read was taken for a live one
// (SF-118, plan 037).
func exitChild(code int) { syscall.Exit(code) }

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
	// The macOS login session is pinned to the GUI's for the package and
	// every hub or host child a test starts (plan 036): the provider
	// availability check marks cursor unavailable outside it, so a Mac runner
	// whose session is not the GUI one would otherwise see a fake cursor
	// unavailable. A test about the session sets rundir.GUISessionEnv itself,
	// over this.
	_ = os.Setenv(rundir.GUISessionEnv, "1")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestTheLoginSessionIsPinned (plan 036): TestMain pins the macOS login
// session to the GUI's for the package, so the provider availability check
// never marks a fake cursor unavailable for the machine's own session, on
// either OS: known, and the GUI's, here too where it would not be known.
func TestTheLoginSessionIsPinned(t *testing.T) {
	if _, gui, known := rundir.GUISession(); !gui || !known {
		t.Fatalf("the package's login session: gui %v, known %v; want the GUI's, pinned by TestMain (%s=1)", gui, known, rundir.GUISessionEnv)
	}
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

// tick hands the hub's loop one tick on ch, within step. A hub that has
// already returned, or a loop that never takes the tick, fails the test with
// what the hub said — its log names why it stopped — rather than parking the
// test on an unbuffered send for good (plan 032 X76: V1's -race ×20 saw
// TestALostRecordOrSocketStopsTheHub parked on its second lost tick for 54
// minutes, the hub already gone and its reason never printed).
func (rn *running) tick(t *testing.T, ch chan<- time.Time) {
	t.Helper()
	select {
	case ch <- time.Now():
	case <-rn.done:
		t.Fatalf("the hub had stopped (Run: %v) before the test's tick; it said: %s", rn.err, rn.stderr)
	case <-time.After(step):
		t.Fatalf("the hub's loop took no tick within %v; it said: %s", step, rn.stderr)
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
// not a zombie. A look that fails (osLooks.alive says which do) fails the
// test: it is this helper's own failure, never taken for either answer.
func alive(t testing.TB, pid int) bool {
	t.Helper()
	a, err := osLooks.alive(pid)
	if err != nil {
		t.Fatalf("whether process %d is alive: %v", pid, err)
	}
	return a
}

// procLooks is how alive looks at a process, in this order: kill(pid, 0),
// then pid's /proc stat (nil where there is no /proc). A test swaps them to
// put a reap between the two (TestAliveTreatsAReapBetweenTheTwoReadsAsGone).
type procLooks struct {
	kill func(pid int) error
	stat func(pid int) ([]byte, error)
}

// osLooks are the process's own looks: the stat on Linux only.
var osLooks = func() procLooks {
	l := procLooks{kill: func(pid int) error { return syscall.Kill(pid, 0) }}
	if runtime.GOOS == "linux" {
		l.stat = func(pid int) ([]byte, error) { return os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) }
	}
	return l
}()

// alive is whether pid has not exited, from l's two looks. A process kill
// does not find is gone. One it finds is gone too when it is a zombie, or —
// on Linux — when its stat is no more by the read: ENOENT at the open, ESRCH
// at the read, a reaper that took the zombie between the two looks (SF-118:
// taking that for a live process made a child that exited at once look like
// one that outlived its whole step). Any other error, or a stat with no state
// letter, is an error: the helper cannot say. A stat that says X has exited
// too: the reaper moves a zombie to EXIT_DEAD, and a read that already holds
// the task still formats it.
func (l procLooks) alive(pid int) (bool, error) {
	if err := l.kill(pid); err != nil && !errors.Is(err, syscall.EPERM) {
		return false, nil
	}
	if l.stat == nil {
		// No /proc: a process kill finds has not exited, a zombie included.
		return true, nil
	}
	b, err := l.stat(pid)
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ESRCH):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("its stat, after kill found it: %w", err)
	}
	state, err := parseState(b)
	if err != nil {
		return false, err
	}
	return !exitedState(state), nil
}

// exitedState is whether a /proc state letter is a process that has exited:
// a zombie (Z), or one its reaper has taken to EXIT_DEAD (X).
func exitedState(state string) bool { return state == "Z" || state == "X" }

// procState is pid's state letter from /proc on Linux, "?" elsewhere.
func procState(pid int) string {
	return statState(fmt.Sprintf("/proc/%d/stat", pid))
}

// taskState is the state letter of pid's thread tid, as procState.
func taskState(pid, tid int) string {
	return statState(fmt.Sprintf("/proc/%d/task/%d/stat", pid, tid))
}

// statState is the state letter in a /proc stat file, "?" when it cannot be
// read or has none: for a failure's message and a thread's state, never for
// whether a process is alive (procLooks.alive says why).
func statState(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "?"
	}
	state, err := parseState(b)
	if err != nil {
		return "?"
	}
	return state
}

// parseState is the state letter in a /proc stat file's contents: the field
// after the command's closing parenthesis — exactly ") ", one letter, then a
// space or the contents' end. Anything else has no state letter. (The copy
// in internal/cli is the same: a test helper cannot be shared without a
// package of its own.)
func parseState(b []byte) (string, error) {
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || len(s) < i+3 || s[i+1] != ' ' || !isLetter(s[i+2]) || (len(s) > i+3 && s[i+3] != ' ') {
		return "", fmt.Errorf("a stat with no state letter: %q", s)
	}
	return s[i+2 : i+3], nil
}

// isLetter is whether c is an ASCII letter: every /proc state is one.
func isLetter(c byte) bool { return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' }

// TestAliveTreatsAReapBetweenTheTwoReadsAsGone (SF-118): kill(pid, 0) finds
// the process, and by the read its stat is no more — the reaper took the
// zombie between the two looks — so it has exited. The old alive took that
// unreadable stat for a live process, and endProcess's re-check then
// reported a child that had exited at once as one that outlived its step.
func TestAliveTreatsAReapBetweenTheTwoReadsAsGone(t *testing.T) {
	const pid = 4242
	found := func(int) error { return nil }
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"reaped before the stat's open", &os.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.ENOENT}},
		{"reaped between the stat's open and its read", &os.PathError{Op: "read", Path: "/proc/4242/stat", Err: syscall.ESRCH}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			looks := procLooks{kill: found, stat: func(int) ([]byte, error) { return nil, tc.err }}
			if a, err := looks.alive(pid); a || err != nil {
				t.Fatalf("kill found it, then its stat said %v: alive %v, %v; want gone", tc.err, a, err)
			}
		})
	}
}

// TestAliveSaysOnlyWhatItSaw: alive's other answers from the same two looks
// — a running process, a zombie, one reaped to EXIT_DEAD, one kill does not
// find, one kill may not signal, and no /proc at all — and the looks it
// cannot read: a stat it may not read, or one with no state letter (") ",
// one letter, then a space or the end — nothing else), is the helper's own
// failure, never gone or alive (SF-118).
func TestAliveSaysOnlyWhatItSaw(t *testing.T) {
	const pid = 4242
	found := func(int) error { return nil }
	stat := func(s string) func(int) ([]byte, error) {
		return func(int) ([]byte, error) { return []byte(s), nil }
	}
	for _, tc := range []struct {
		name    string
		looks   procLooks
		alive   bool
		failure string
	}{
		{"running", procLooks{found, stat("4242 (craze hub) S 1 4242 4242 0 -1")}, true, ""},
		{"a zombie", procLooks{found, stat("4242 (a (paren) name) Z 1 4242 4242 0 -1")}, false, ""},
		{"reaped to EXIT_DEAD", procLooks{found, stat("4242 (craze hub) X 1 4242 4242 0 -1")}, false, ""},
		{"a state that ends the stat", procLooks{found, stat("4242 (craze) R")}, true, ""},
		{"gone at the kill", procLooks{func(int) error { return syscall.ESRCH }, func(int) ([]byte, error) {
			t.Error("alive read the stat of a process kill did not find")
			return nil, nil
		}}, false, ""},
		{"not ours to signal", procLooks{func(int) error { return syscall.EPERM }, stat("4242 (init) S 0")}, true, ""},
		{"no /proc", procLooks{found, nil}, true, ""},
		{"a stat it may not read", procLooks{found, func(int) ([]byte, error) {
			return nil, &os.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.EACCES}
		}}, false, "permission denied"},
		{"a stat with no state letter", procLooks{found, stat("4242 (craze")}, false, "no state letter"},
		{"an empty stat", procLooks{found, stat("")}, false, "no state letter"},
		{"a space for a state", procLooks{found, stat("4242 (craze)  ")}, false, "no state letter"},
		{"two state letters", procLooks{found, stat("4242 (craze) ZZ")}, false, "no state letter"},
		{"a state with no space after it", procLooks{found, stat("4242 (craze) S1 4242")}, false, "no state letter"},
		{"no space before the state", procLooks{found, stat("4242 (craze)S 1")}, false, "no state letter"},
		{"a digit for a state", procLooks{found, stat("4242 (craze) 1 4242")}, false, "no state letter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := tc.looks.alive(pid)
			switch {
			case tc.failure != "" && (err == nil || !strings.Contains(err.Error(), tc.failure)):
				t.Fatalf("alive %v, %v; want the helper's failure %q", a, err, tc.failure)
			case tc.failure == "" && (err != nil || a != tc.alive):
				t.Fatalf("alive %v, %v; want %v", a, err, tc.alive)
			}
		})
	}
}

// endProcess ends pid — a child of this test, its stderr in dir/<pid>
// (hubTestStderrDir) — and fails the test if it will not go: SIGCONT and
// SIGTERM, step to exit, then SIGKILL. One that has not gone within step
// says first where it is — /proc's state and signal masks, then the
// goroutine dump a SIGQUIT writes to its stderr — before its SIGKILL. (Plan
// 032 X58 and X67 took this report for a child that ignored its SIGTERM; it
// was alive's own race, SF-118, and the report now says the time it
// measured.) A look that fails ends the wait as it is: the child is killed
// and the failure said, taken for neither answer, and the cleanup goes on to
// the children after it.
func endProcess(t *testing.T, pid int, dir string) {
	t.Helper()
	var lookErr error
	alive := func() bool {
		if lookErr != nil {
			return false
		}
		a, err := osLooks.alive(pid)
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
	_ = syscall.Kill(pid, syscall.SIGCONT)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	termed := time.Now()
	deadline := termed.Add(step)
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
	var dump []byte
	// What the SIGQUIT's wait sees: one gone at once was ending just then,
	// one still there was not ending at all. The look comes first and its
	// answer is the message's: a look that fails says so (and the deferred
	// report says how), never "gone" or "still there".
	quitWait := "no SIGQUIT: no stderr to dump it to"
	if dir != "" {
		_ = syscall.Kill(pid, syscall.SIGQUIT)
		quitStart := time.Now()
		quit := quitStart.Add(5 * time.Second)
		for alive() && time.Now().Before(quit) {
			time.Sleep(10 * time.Millisecond)
		}
		there := alive()
		took := time.Since(quitStart).Round(time.Millisecond)
		switch {
		case lookErr != nil:
			quitWait = "whether it was there in the SIGQUIT's wait, the look failed"
		case there:
			quitWait = "it was still there after the SIGQUIT's wait"
		default:
			quitWait = fmt.Sprintf("it was gone %v into the SIGQUIT's wait", took)
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if dir != "" {
		dump, _ = os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)))
	}
	t.Errorf("hub child %d had not exited %v after its SIGTERM; killed (%s)\n%s\n--- its stderr, a SIGQUIT's dump:\n%s", pid, outlived, quitWait, state, dump)
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
	// Each thread's state and wait channel too: a child that logged "stopped"
	// and called exit, yet stayed for the whole step, was in its exit with a
	// thread the exit had to wait for (SF-118, reopened by plan 035 X34), and
	// which thread, waiting where, is what the process-wide lines cannot say.
	if tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid)); err == nil {
		for _, task := range tasks {
			tid, err := strconv.Atoi(task.Name())
			if err != nil {
				continue
			}
			w, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/wchan", pid, tid))
			out = append(out, fmt.Sprintf("task %d:\t%s %s", tid, taskState(pid, tid), w))
		}
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
	waitFor(t, fmt.Sprintf("process %d exits", pid), func() bool { return !alive(t, pid) })
}

// stillThere is a cleanup's wait, within step, for pids — each already sent
// its end — to have exited: the ones still there when it ends. It never
// stops the cleanup: a look that fails is said (t.Errorf), taken for neither
// answer, and the wait goes on to the next pid. A cleanup that ends several
// children signals every one before it waits here, so no failure in the
// wait leaves a child unsignalled.
func stillThere(t testing.TB, pids []int) []int {
	t.Helper()
	deadline := time.Now().Add(step)
	var left []int
	for _, pid := range pids {
		for {
			a, err := osLooks.alive(pid)
			if err != nil {
				t.Errorf("ending child %d, whether it is alive: %v", pid, err)
				break
			}
			if !a {
				break
			}
			if !time.Now().Before(deadline) {
				left = append(left, pid)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return left
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
