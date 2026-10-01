package hub

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"slices"
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
			if !alive(pid) {
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
			if !alive(pid) {
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
	if !alive(pid) {
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
	if alive(first) {
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
func TestEnsureReplacesASIGSTOPpedHub(t *testing.T) {
	env := processEnv(t)
	generous(t)
	setVar(t, &helloTimeout, 3*time.Second)
	kids := asChildren(t, nil)
	if _, err := ensure(t, env, protocol.ConnectionCapabilities{}); err != nil {
		t.Fatal(err)
	}
	old, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(old.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(old.PID, syscall.SIGCONT) })
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
	if !alive(rec.PID) {
		t.Fatalf("the hub the record names (pid %d) is not running", rec.PID)
	}
}
