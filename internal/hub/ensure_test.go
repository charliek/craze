package hub

import (
	"context"
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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// Ensure's tests (plan 032 §3.8, §3.18). A hub a test does not want spawned
// is guarded by noCommand; one it does is this test binary re-executed
// (asChildren). A record's pid is a process of the test's own — a sleeper —
// where the record must name a live process that is no hub: P17 may signal
// it, and the test sees whether it did.

// generous sets the bounds a test is not about to an amount a starved hub
// child meets: its start, its hello, its rendezvous.
func generous(t *testing.T) {
	t.Helper()
	setVar(t, &readyWait, step)
	setVar(t, &helloTimeout, 5*time.Second)
	setVar(t, &rendezvousWait, step)
}

// ensure runs Ensure bounded by step.
func ensure(t *testing.T, env rundir.Env, need protocol.ConnectionCapabilities) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), step)
	defer cancel()
	return Ensure(ctx, env, need)
}

// TestEnsureRefusesWithoutACommand (§3.18's leak guard): a test binary that
// has not installed Command gets ErrNoHub at once — before it reads a record,
// so it never reaches a hub, let alone spawns one; here a live hub it would
// otherwise have found.
func TestEnsureRefusesWithoutACommand(t *testing.T) {
	env := processEnv(t)
	setVar(t, &Command, nil)
	var accepted atomic.Int32
	hk := quiet()
	hk.accepted = func(*net.UnixConn) { accepted.Add(1) }
	rn := runIn(t, env, hk)
	rn.line(t)
	start := time.Now()
	sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
	if !errors.Is(err, ErrNoHub) || sock != "" {
		t.Fatalf("Ensure in a test binary with no Command = %q, %v; want ErrNoHub", sock, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Ensure took %v to refuse", took)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("Ensure dialled the hub %d times", n)
	}
}

// TestEnsureFindsALiveHub: a hub that answers as its record says is the
// answer; nothing is spawned.
func TestEnsureFindsALiveHub(t *testing.T) {
	env := processEnv(t)
	noCommand(t)
	rn := runIn(t, env, quiet())
	line := rn.line(t)
	sock, err := ensure(t, env, protocol.ConnectionCapabilities{RosterSubscribe: true, Connect: true})
	if err != nil || sock != line.Socket {
		t.Fatalf("Ensure = %q, %v; want the live hub's %s", sock, err, line.Socket)
	}
}

// TestEnsureUsesAHubItCannotIdentify (the C7 amendment): a record with no
// start token, or one whose token cannot be checked here, names a hub whose
// identity cannot be verified — not a stale one: it is dialled, and used
// when its hello answers its record's hub id.
func TestEnsureUsesAHubItCannotIdentify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, rec *rundir.HubRecord)
	}{
		{"no start token", func(t *testing.T, rec *rundir.HubRecord) { rec.StartToken = "" }},
		{"a token it cannot check", func(t *testing.T, rec *rundir.HubRecord) {
			setVar(t, &startToken, func(int) (string, error) { return "", errors.New("no scope here") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := processEnv(t)
			noCommand(t)
			rn := runIn(t, env, quiet()) // quiet: its rewritten record is never Lost
			line := rn.line(t)
			rec, _, err := rundir.ReadHubRecord(env)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, &rec)
			writeRecord(t, env, rec)
			sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
			if err != nil || sock != line.Socket {
				t.Fatalf("Ensure = %q, %v; want the hub it cannot identify, %s", sock, err, line.Socket)
			}
		})
	}
}

// TestEnsureNeverKillsAWedgedHubItCannotIdentify (P17, the C7 amendment): a
// hub whose two hellos time out is replaced only on its identity; with no
// start token, or one that cannot be checked, it is reported and never
// signalled.
func TestEnsureNeverKillsAWedgedHubItCannotIdentify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token func(t *testing.T, pid int) string
	}{
		{"no start token", func(*testing.T, int) string { return "" }},
		{"a token it cannot check", func(t *testing.T, pid int) string {
			tok := token(t, pid)
			setVar(t, &startToken, func(int) (string, error) { return "", errors.New("no scope here") })
			return tok
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := processEnv(t)
			noCommand(t)
			setVar(t, &helloTimeout, 200*time.Millisecond)
			pid := sleeper(t)
			d := newDecoy(t, decoySilent, protocol.HubHelloResult{})
			hubID := rundir.NewHubID()
			writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: tc.token(t, pid), Socket: d.path})
			_, err := ensure(t, env, protocol.ConnectionCapabilities{})
			var wedged *WedgedError
			if !errors.As(err, &wedged) || wedged.PID != pid {
				t.Fatalf("Ensure = %v; want the wedged hub (pid %d) reported", err, pid)
			}
			if !alive(t, pid) {
				t.Fatalf("Ensure signalled pid %d, whose identity it could not verify", pid)
			}
			if n := d.accepted.Load(); n < 2 {
				t.Fatalf("the wedged hub was said hello to %d times, want two before it is judged", n)
			}
		})
	}
}

// TestEnsureTakesEOFOrClosingForNoHub (§3.8 step 1): a hub that closes the
// connection, or answers closing, during the hello is no hub — not a wedged
// one: one hello, nothing signalled, and a hub is spawned in its place.
func TestEnsureTakesEOFOrClosingForNoHub(t *testing.T) {
	for _, mode := range []string{decoyEOF, decoyClosing} {
		t.Run(mode, func(t *testing.T) {
			env := processEnv(t)
			generous(t)
			kids := asChildren(t, nil)
			pid := sleeper(t)
			d := newDecoy(t, mode, protocol.HubHelloResult{})
			writeRecord(t, env, rundir.HubRecord{HubID: rundir.NewHubID(), PID: pid, StartToken: token(t, pid), Socket: d.path})
			sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
			want, _ := rundir.HubSocket(env)
			if err != nil || sock != want {
				t.Fatalf("Ensure = %q, %v; want a spawned hub at %s", sock, err, want)
			}
			if n := d.accepted.Load(); n != 1 {
				t.Fatalf("the hub that went said hello to %d times, want once", n)
			}
			if !alive(t, pid) {
				t.Fatal("Ensure signalled a hub that answered EOF or closing")
			}
			if n := kids.n.Load(); n != 1 {
				t.Fatalf("Ensure spawned %d hubs, want one", n)
			}
		})
	}
}

// TestEnsureNeverDialsAStaleRecord (§3.8 step 1): a record whose pid is gone,
// or another process's now (its start token not the record's), names no hub:
// it is never dialled — here a decoy at its socket that would answer as the
// record's hub — and a hub is spawned.
func TestEnsureNeverDialsAStaleRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  func(t *testing.T) (int, string)
	}{
		{"a pid reused", func(t *testing.T) (int, string) {
			// A live process, with another's token.
			return sleeper(t), token(t, os.Getpid())
		}},
		{"a pid gone", func(t *testing.T) (int, string) {
			cmd := exec.Command("true")
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			return cmd.Process.Pid, token(t, os.Getpid())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := processEnv(t)
			generous(t)
			asChildren(t, nil)
			pid, tok := tc.pid(t)
			hubID := rundir.NewHubID()
			d := newDecoy(t, decoyAnswer, hubResult(hubID, "decoy", protocol.HubCapabilities()))
			writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: tok, Socket: d.path})
			sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
			want, _ := rundir.HubSocket(env)
			if err != nil || sock != want {
				t.Fatalf("Ensure = %q, %v; want a spawned hub at %s, not the stale record's %s", sock, err, want, d.path)
			}
			if n := d.accepted.Load(); n != 0 {
				t.Fatalf("Ensure dialled a stale record's socket %d times", n)
			}
		})
	}
}

// TestEnsureReportsAnOlderHub (§3.8 step 1): a hub without a capability the
// caller needs is reported in the plan's words, not used, not replaced and
// not tried again; for a need it meets, it is used.
func TestEnsureReportsAnOlderHub(t *testing.T) {
	env := processEnv(t)
	noCommand(t)
	pid := sleeper(t)
	hubID := rundir.NewHubID()
	d := newDecoy(t, decoyAnswer, hubResult(hubID, "0.9.1", protocol.ConnectionCapabilities{RosterSubscribe: true, Connect: true}))
	writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: token(t, pid), Socket: d.path})
	_, err := ensure(t, env, protocol.ConnectionCapabilities{SessionCreate: true})
	var lacks *LacksError
	if !errors.As(err, &lacks) || err.Error() != "this hub (craze 0.9.1) cannot create sessions; it exits when idle" {
		t.Fatalf("Ensure for a create = %v; want the older hub reported", err)
	}
	if n := d.accepted.Load(); n != 1 {
		t.Fatalf("the older hub was said hello to %d times, want once (no retry)", n)
	}
	if !alive(t, pid) {
		t.Fatal("Ensure signalled an older hub")
	}
	sock, err := ensure(t, env, protocol.ConnectionCapabilities{Connect: true})
	if err != nil || sock != d.path {
		t.Fatalf("Ensure for a connect = %q, %v; want the older hub, which can", sock, err)
	}
}

// TestEnsureReapsAContenderThatDoesNotAnswer (§3.8 step 2): a spawned hub
// that has not answered when the ready wait ends — its record written, its
// socket bound, parked before its ready line — is ended and reaped, and the
// retry's spawn answers. No start token can be checked here, so the retry
// could never replace a parked hub itself (P17): only the ready wait's end
// can have ended it.
func TestEnsureReapsAContenderThatDoesNotAnswer(t *testing.T) {
	env := processEnv(t)
	generous(t)
	setVar(t, &startToken, func(int) (string, error) { return "", errors.New("no scope here") })
	kids := asChildren(t, func(n int) []string {
		if n == 1 {
			return []string{hubTestReady + "=block"}
		}
		return nil
	})
	setVar(t, &contenderGrace, time.Second)
	fire := make(chan time.Time)
	var timers atomic.Int32
	setVar(t, &readyTimer, func(d time.Duration) <-chan time.Time {
		if timers.Add(1) == 1 {
			return fire
		}
		return time.After(d)
	})
	type result struct {
		sock string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
		got <- result{sock, err}
	}()
	// The first hub is parked at its ready line once its record names it:
	// the only hub spawned while the first ready wait runs. (Its pid is read
	// from the record: the spawn's own Cmd is the spawning goroutine's until
	// Ensure returns.)
	var first int
	waitFor(t, "the first hub's record", func() bool {
		rec, _, err := rundir.ReadHubRecord(env)
		first = rec.PID
		return err == nil
	})
	fire <- time.Now()
	var r result
	select {
	case r = <-got:
	case <-time.After(step):
		t.Fatalf("Ensure did not return within %v", step)
	}
	if r.err != nil {
		t.Fatalf("Ensure: %v", r.err)
	}
	if pids := kids.pids(); len(pids) != 2 || pids[0] != first {
		t.Fatalf("the hubs spawned are %v; the first record named %d", pids, first)
	}
	if alive(t, first) {
		t.Fatalf("the hub that did not answer (pid %d) is still running (%s)", first, procState(first))
	}
	rec, _, err := rundir.ReadHubRecord(env)
	if err != nil || rec.PID == first || !slices.Contains(kids.pids(), rec.PID) {
		t.Fatalf("the record after the retry: %+v, %v; want the second hub's", rec, err)
	}
	if res := dial(t, r.sock).hello(t); res.Endpoint.HostID != rec.HubID {
		t.Fatalf("Ensure's socket answers %+v, not the record's hub", res)
	}
}

// TestEnsureReplacesASIGSTOPpedHub (P17): a hub that answers no hello — two
// that time out, not refused, not EOF — and whose pid carries its record's
// start token is sent SIGTERM, then SIGKILL when it does not go (a stopped
// process acts on neither until it is killed), and a new hub takes its place.
//
// The hub has stopped before Ensure looks (waitStopped): on Linux kill(2)
// returns before a process has stopped, and a hub whose stop lands after
// Ensure's hello is a hub that answered, which Ensure uses — rightly. "its
// stop late" forces that schedule: the hub's main thread, the one the kernel
// wakes to stop the rest, is held across the SIGSTOP (holdThread) while its
// other threads would answer; without the wait it fails every time.
func TestEnsureReplacesASIGSTOPpedHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold time.Duration
	}{
		{"its stop at once", 0},
		{"its stop late", 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hold > 0 && runtime.GOOS != "linux" {
				t.Skip("a stop that lands after kill(2) returns is Linux's: macOS suspends the task first")
			}
			env := processEnv(t)
			generous(t)
			setVar(t, &helloTimeout, 3*time.Second)
			kids := asChildren(t, func(n int) []string {
				if n == 1 && tc.hold > 0 {
					return []string{hubTestHold + "=" + tc.hold.String()}
				}
				return nil
			})
			if _, err := ensure(t, env, protocol.ConnectionCapabilities{}); err != nil {
				t.Fatal(err)
			}
			old, _, err := rundir.ReadHubRecord(env)
			if err != nil {
				t.Fatal(err)
			}
			if tc.hold > 0 {
				if err := syscall.Kill(old.PID, syscall.SIGUSR1); err != nil {
					t.Fatal(err)
				}
				waitFor(t, "the hub's main thread held", func() bool { return taskState(old.PID, old.PID) == "D" })
			}
			if err := syscall.Kill(old.PID, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(old.PID, syscall.SIGCONT) })
			waitStopped(t, old.PID)
			sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
			if err != nil {
				t.Fatalf("Ensure past a stopped hub: %v", err)
			}
			waitGone(t, old.PID)
			rec, _, err := rundir.ReadHubRecord(env)
			if err != nil || rec.HubID == old.HubID || rec.PID == old.PID {
				t.Fatalf("the record after the replacement: %+v, %v (the stopped hub's was %+v)", rec, err, old)
			}
			if res := dial(t, sock).hello(t); res.Endpoint.HostID != rec.HubID {
				t.Fatalf("Ensure's socket answers %+v, not the new hub", res)
			}
			if n := kids.n.Load(); n != 2 {
				t.Fatalf("%d hubs spawned, want 2", n)
			}
		})
	}
}

// TestEnsureIsBoundedByItsContext (§3.8): every step is bounded by the
// caller's context — here a spawned hub that never answers, with a ready wait
// far longer than the context — and the hub it started is ended after it.
func TestEnsureIsBoundedByItsContext(t *testing.T) {
	env := processEnv(t)
	generous(t)
	setVar(t, &contenderGrace, time.Second)
	kids := asChildren(t, func(int) []string { return []string{hubTestReady + "=block"} })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := Ensure(ctx, env, protocol.ConnectionCapabilities{})
	if took := time.Since(start); !errors.Is(err, context.DeadlineExceeded) || took > 4*time.Second {
		t.Fatalf("Ensure under a 1s context = %v after %v", err, took)
	}
	for _, pid := range kids.pids() {
		waitGone(t, pid)
	}
}

// TestTwoRacingEnsuresMakeOneHub (A6): two Ensures that both find no hub both
// spawn one — held at the spawn until both are there — and the two hubs race
// for the namespace's lock: one serves, the other answers held and exits, and
// both Ensures answer the one hub.
func TestTwoRacingEnsuresMakeOneHub(t *testing.T) {
	env := processEnv(t)
	generous(t)
	kids := asChildren(t, nil)
	inner := Command
	var arrived atomic.Int32
	gate := make(chan struct{})
	Command = func(argv []string) (*exec.Cmd, error) {
		if arrived.Add(1) == 2 {
			close(gate)
		}
		select {
		case <-gate:
		case <-time.After(step):
		}
		return inner(argv)
	}
	type result struct {
		sock string
		err  error
	}
	got := make(chan result, 2)
	for range 2 {
		go func() {
			sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
			got <- result{sock, err}
		}()
	}
	var socks []string
	for range 2 {
		select {
		case r := <-got:
			if r.err != nil {
				t.Fatalf("a racing Ensure: %v", r.err)
			}
			socks = append(socks, r.sock)
		case <-time.After(step):
			t.Fatalf("a racing Ensure did not return within %v", step)
		}
	}
	if socks[0] != socks[1] {
		t.Fatalf("the racing Ensures answered two hubs: %q", socks)
	}
	if n := kids.n.Load(); n < 2 {
		t.Fatalf("%d hubs spawned; the race needs two", n)
	}
	rec, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range kids.pids() {
		if pid != rec.PID {
			waitGone(t, pid)
		}
	}
	if !alive(t, rec.PID) {
		t.Fatalf("the hub the record names (pid %d) is not running", rec.PID)
	}
}

// TestEnsureOutlastsAHubTearingDown (§3.8 step 4, review r18): a hub whose
// teardown keeps the namespace's lock past a whole rendezvous — stalled here
// just before its Release, its record and socket already gone — is waited
// out. Ensure's first spawn loses the lock to it and its rendezvous ends with
// the hub still there (errHolderLeaving); the second spawn loses too, and the
// hub lets the lock go only once that second contender has answered held, so
// the second rendezvous ends with the holder gone (errHolderGone) or, starved,
// with it still leaving. Either is another round, never Ensure's answer: it
// ends with a hub of its own answering. With one retry and no rounds, the
// second rendezvous' end was Ensure's error while the lock was free.
func TestEnsureOutlastsAHubTearingDown(t *testing.T) {
	env := processEnv(t)
	generous(t)
	setVar(t, &rendezvousWait, 2*time.Second)
	gate := shortDir(t, "czg")
	pids := shortDir(t, "czp")
	kids := asChildren(t, func(n int) []string {
		extra := []string{hubTestPIDFile + "=" + filepath.Join(pids, strconv.Itoa(n))}
		if n == 1 {
			extra = append(extra, hubTestStall+"="+gate)
		}
		return extra
	})
	if _, err := ensure(t, env, protocol.ConnectionCapabilities{}); err != nil {
		t.Fatal(err)
	}
	old, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(old.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the old hub's teardown stalls before its Release", func() bool {
		_, err := os.Stat(filepath.Join(gate, "stalled"))
		return err == nil
	})
	lockHeld(t, env)

	type result struct {
		sock string
		err  error
	}
	got := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 8*step)
	defer cancel()
	go func() {
		sock, err := Ensure(ctx, env, protocol.ConnectionCapabilities{})
		got <- result{sock, err}
	}()
	// The second contender (the third hub spawned) is spawned only once the
	// first's rendezvous has ended with the old hub still holding the lock;
	// it has answered held once it has exited.
	for want := int32(2); want <= 3; want++ {
		deadline := time.Now().Add(step)
		for kids.n.Load() < want {
			select {
			case r := <-got:
				t.Fatalf("Ensure = %q, %v while the old hub (pid %d) held the lock in its teardown, with %d hubs spawned", r.sock, r.err, old.PID, kids.n.Load())
			case <-time.After(5 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatalf("hub %d not spawned within %v", want, step)
			}
		}
	}
	second := childPID(t, pids, 3)
	waitGone(t, second)
	if err := os.WriteFile(filepath.Join(gate, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-got:
	case <-time.After(2 * step):
		t.Fatalf("Ensure did not return within %v of the old hub's letting its lock go", 2*step)
	}
	if r.err != nil {
		t.Fatalf("Ensure once the old hub let its lock go: %v (%d hubs spawned)", r.err, kids.n.Load())
	}
	waitGone(t, old.PID)
	rec, _, err := rundir.ReadHubRecord(env)
	if err != nil || rec.HubID == old.HubID || rec.PID == old.PID || rec.PID == second {
		t.Fatalf("the record after the old hub went: %+v, %v (the old hub's was %+v; the second contender was pid %d)", rec, err, old, second)
	}
	if res := dial(t, r.sock).hello(t); res.Endpoint.HostID != rec.HubID {
		t.Fatalf("Ensure's socket answers %+v, not the record's hub", res)
	}
}

// childPID is the pid hub child n wrote (hubTestPIDFile), within step.
func childPID(t *testing.T, dir string, n int) int {
	t.Helper()
	var pid int
	waitFor(t, fmt.Sprintf("hub child %d's pid", n), func() bool {
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(n)))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(b))
		return err == nil && pid > 0
	})
	return pid
}

// eagain is the error a dial to a listener whose backlog is full returns on
// Linux, at once.
func eagain(socket string) error {
	return &net.OpError{Op: "dial", Net: "unix", Addr: &net.UnixAddr{Name: socket, Net: "unix"},
		Err: os.NewSyscallError("connect", syscall.EAGAIN)}
}

// backlogFor makes every hello's dial meet a full backlog (eagain) n times —
// for good with n < 0 — before it dials for real, and counts the dials.
func backlogFor(t *testing.T, n int) *atomic.Int32 {
	t.Helper()
	var dials atomic.Int32
	through := helloDial
	setVar(t, &helloDial, func(ctx context.Context, socket string) (net.Conn, error) {
		if k := dials.Add(1); n < 0 || int(k) <= n {
			return nil, eagain(socket)
		}
		return through(ctx, socket)
	})
	return &dials
}

// TestAFullBacklogIsNoStrike (P17, review r18): a listener whose backlog is
// full refuses a dial at once with EAGAIN — a hub not accepting yet, busy or
// not scheduled — which is no hello that timed out: the hello dials again
// until its bound, answers once a dial gets through, and is one timeout only
// when none has by then. So a hub behind a full backlog for a moment is found,
// never signalled and never spawned over.
func TestAFullBacklogIsNoStrike(t *testing.T) {
	answer := func(t *testing.T) (*decoy, string) {
		hubID := rundir.NewHubID()
		return newDecoy(t, decoyAnswer, hubResult(hubID, "decoy", protocol.HubCapabilities())), hubID
	}
	t.Run("dialled again until one gets through", func(t *testing.T) {
		d, hubID := answer(t)
		dials := backlogFor(t, 3)
		res, out, err := dialHello(context.Background(), d.path, step)
		if out != helloOK || err != nil || res.Endpoint.HostID != hubID {
			t.Fatalf("a hello past three full backlogs = %v, %v, %+v; want the hub's answer", out, err, res)
		}
		if n := dials.Load(); n != 4 {
			t.Fatalf("%d dials, want 4", n)
		}
	})
	t.Run("full until the bound is one timeout", func(t *testing.T) {
		d, _ := answer(t)
		dials := backlogFor(t, -1)
		const wait = 300 * time.Millisecond
		start := time.Now()
		_, out, err := dialHello(context.Background(), d.path, wait)
		took := time.Since(start)
		if out != helloTimedOut || err != nil {
			t.Fatalf("a hello whose backlog stays full = %v, %v; want one timeout", out, err)
		}
		if took < wait {
			t.Fatalf("the hello was judged timed out after %v, inside its %v bound", took, wait)
		}
		if n := dials.Load(); n < 2 {
			t.Fatalf("%d dials within the bound; a full backlog is dialled again", n)
		}
		if n := d.accepted.Load(); n != 0 {
			t.Fatalf("the decoy accepted %d connections", n)
		}
	})
	t.Run("Ensure finds the hub", func(t *testing.T) {
		env := processEnv(t)
		noCommand(t)
		setVar(t, &helloTimeout, 5*time.Second)
		d, hubID := answer(t)
		pid := sleeper(t)
		writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: token(t, pid), Socket: d.path})
		dials := backlogFor(t, 3)
		sock, err := ensure(t, env, protocol.ConnectionCapabilities{})
		if err != nil || sock != d.path {
			t.Fatalf("Ensure past a full backlog = %q, %v; want the hub at %s", sock, err, d.path)
		}
		if !alive(t, pid) {
			t.Fatalf("Ensure signalled the hub (pid %d) behind a full backlog", pid)
		}
		if n := dials.Load(); n != 4 {
			t.Fatalf("%d dials, want 4", n)
		}
	})
}

// TestEnsureReportsAHubThatOutlivesItsKill (P17, review r18): a wedged hub
// still carrying its start token wedgedKillWait after its SIGKILL may still
// hold the namespace's lock, so nothing is spawned over it: it is reported
// (WedgedError). The hub here is a process of the test's own that is never
// reaped while the test runs — its pid is no other process's meanwhile, so
// Ensure's signals reach nothing else — and the start-token seam says it
// carries its token still, as a process the kill has not ended would.
func TestEnsureReportsAHubThatOutlivesItsKill(t *testing.T) {
	env := processEnv(t)
	noCommand(t)
	setVar(t, &helloTimeout, 200*time.Millisecond)
	setVar(t, &wedgedTermWait, 100*time.Millisecond)
	setVar(t, &wedgedKillWait, 100*time.Millisecond)
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pid := cmd.Process.Pid
	const tok = "st1/test/unkillable"
	setVar(t, &startToken, func(p int) (string, error) {
		if p == pid {
			return tok, nil
		}
		return rundir.StartToken(p)
	})
	d := newDecoy(t, decoySilent, protocol.HubHelloResult{})
	writeRecord(t, env, rundir.HubRecord{HubID: rundir.NewHubID(), PID: pid, StartToken: tok, Socket: d.path})
	_, err := ensure(t, env, protocol.ConnectionCapabilities{})
	var wedged *WedgedError
	if !errors.As(err, &wedged) || wedged.PID != pid || !strings.Contains(wedged.Why, "after SIGKILL") {
		t.Fatalf("Ensure = %v; want the hub that outlived its SIGKILL (pid %d) reported, nothing spawned", err, pid)
	}
}
