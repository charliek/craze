package hub

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// TestAHubServesItsHello (plan 032 §3.5, §3.6): a hub comes up through its
// stages — ready ok naming its id, its namespace and its socket; a record
// naming the same, its pid and this process's start token — answers hello
// as a hub, refuses a host's methods host_only, every method before hello and
// a session.connect that is not the first request, and on SIGTERM leaves no record and no socket and lets its lock go.
func TestAHubServesItsHello(t *testing.T) {
	env := testEnv(t)
	rn := runIn(t, env, quiet())
	line := rn.line(t)
	sock, err := rundir.HubSocket(env)
	if err != nil {
		t.Fatal(err)
	}
	if !line.OK || !rundir.ValidHostID(line.HubID) || line.NS != nsOf(t, env) || line.Socket != sock {
		t.Fatalf("ready line %+v, want ok, a hub id, namespace %s and socket %s", line, nsOf(t, env), sock)
	}
	rec, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	if rec.HubID != line.HubID || rec.PID != os.Getpid() || rec.Socket != sock || rec.StartToken != token(t, os.Getpid()) {
		t.Fatalf("record %+v does not name the hub of ready line %+v", rec, line)
	}
	lockHeld(t, env)

	c := dial(t, sock)
	if resp := c.call(t, protocol.MethodSessionsList, nil); resp.Error == nil || resp.Error.Data.Reason != protocol.ReasonHelloRequired {
		t.Fatalf("sessions.list before hello: %+v, want hello_required", resp)
	}
	res := c.hello(t)
	// A hub given no way to start a host (Options.Creates nil) creates
	// nothing, and says so (create_test.go has one that does).
	caps := protocol.HubCapabilities()
	caps.SessionCreate = false
	if res.Endpoint.Kind != protocol.EndpointHub || res.Endpoint.HostID != line.HubID || res.Endpoint.PID != os.Getpid() ||
		res.Capabilities != caps || res.Limits != protocol.HostLimits() || res.Protocol != 1 {
		t.Fatalf("hello answered %+v", res)
	}
	for _, m := range []string{protocol.MethodSessionAttach, protocol.MethodSessionPrompt, protocol.MethodAsksAnswer} {
		resp := c.call(t, m, map[string]any{"sessionId": "s"})
		if resp.Error == nil || resp.Error.Data.Code != protocol.CodeUnsupported || resp.Error.Data.Reason != protocol.ReasonHostOnly {
			t.Fatalf("%s: %+v, want unsupported/host_only", m, resp)
		}
	}
	if resp := c.call(t, "no.such.method", nil); resp.Error == nil || resp.Error.Data.Reason != protocol.ReasonUnknownMethod {
		t.Fatalf("an unknown method: %+v", resp)
	}
	// The roster is served (roster_test.go): an empty registry is an empty
	// roster at cursor 0, its epoch the hub's id.
	if resp := c.call(t, protocol.MethodSessionsList, nil); resp.Error != nil ||
		string(resp.Result) != `{"epoch":"`+line.HubID+`","cursor":0,"sessions":[]}` {
		t.Fatalf("sessions.list over an empty registry: %+v (%s)", resp, resp.Result)
	}
	// session.connect is served (splice_test.go), as a connection's first
	// request after hello: here it is not.
	if resp := c.call(t, protocol.MethodSessionConnect, map[string]any{"sessionId": "s"}); resp.Error == nil ||
		resp.Error.Data.Reason != protocol.ReasonConnectNotFirst {
		t.Fatalf("session.connect after other requests: %+v, want connect_not_first", resp)
	}
	if resp := c.call(t, protocol.MethodSessionCreate, map[string]any{"cwd": "/"}); resp.Error == nil ||
		resp.Error.Data.Code != protocol.CodeUnsupported || resp.Error.Data.Reason != protocol.ReasonUnsupported {
		t.Fatalf("session.create on a hub that creates nothing: %+v, want unsupported", resp)
	}

	rn.sigs <- syscall.SIGTERM
	if err := rn.stopped(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	absent(t, "the record", recordPath(t, env))
	absent(t, "the socket", sock)
	lockFree(t, env)
	if !strings.Contains(rn.stderr.String(), "stopping: SIGTERM") {
		t.Fatalf("the hub did not say why it stopped: %s", rn.stderr)
	}
}

// TestTheLockRace (§3.5 stage 1): of two hubs one holds the namespace; the
// other answers held and returns nil having made nothing — naming the holder
// its lock's line names, or nobody while the holder has not written its line
// (the instant after its flock, played here by a flock with no line).
func TestTheLockRace(t *testing.T) {
	t.Run("before the holder line", func(t *testing.T) {
		env := testEnv(t)
		dir := hubsDir(t, env)
		f, err := os.OpenFile(filepath.Join(dir, nsOf(t, env)+".lock"), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		rn := runIn(t, env, quiet())
		line := rn.line(t)
		if line.OK || line.Held == nil || *line.Held != (ReadyHeld{}) || line.Error != "" {
			t.Fatalf("a loser before the holder's line answered %+v, want held naming nobody", line)
		}
		if err := rn.stopped(t); err != nil {
			t.Fatalf("a loser's Run: %v, want nil", err)
		}
		absent(t, "a record", recordPath(t, env))
		sock, _ := rundir.HubSocket(env)
		absent(t, "a socket", sock)
	})
	t.Run("after the holder line", func(t *testing.T) {
		env := testEnv(t)
		first := runIn(t, env, quiet())
		won := first.line(t)
		rn := runIn(t, env, quiet())
		line := rn.line(t)
		if line.OK || line.Held == nil || *line.Held != (ReadyHeld{PID: os.Getpid(), HubID: won.HubID}) {
			t.Fatalf("a loser answered %+v, want held naming pid %d, hub %s", line, os.Getpid(), won.HubID)
		}
		if err := rn.stopped(t); err != nil {
			t.Fatalf("a loser's Run: %v, want nil", err)
		}
		// The winner is untouched: its record and socket are its own.
		rec, _, err := rundir.ReadHubRecord(env)
		if err != nil || rec.HubID != won.HubID {
			t.Fatalf("the winner's record after the race: %+v, %v", rec, err)
		}
		if res := dial(t, won.Socket).hello(t); res.Endpoint.HostID != won.HubID {
			t.Fatalf("the winner's socket answers %+v", res)
		}
	})
}

// TestAStaleSocket (§3.5 stage 3): only a socket that refuses — and is
// still the one probed — is stale and replaced; a live one, anything not a
// socket, and a socket replaced while it was probed are left as they are,
// and the hub does not come up.
func TestAStaleSocket(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		env := testEnv(t)
		sock, err := rundir.HubSocket(env)
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		ln.SetUnlinkOnClose(false)
		_ = ln.Close()
		rn := runIn(t, env, quiet())
		if line := rn.line(t); !line.OK {
			t.Fatalf("a hub over a stale socket: %+v", line)
		}
		// The inode the stale socket freed may be the new one's, so its
		// replacement is seen by what answers there.
		dial(t, sock).hello(t)
		if !strings.Contains(rn.stderr.String(), "removed a stale socket") {
			t.Fatalf("the hub did not say it removed a stale socket: %s", rn.stderr)
		}
	})
	t.Run("live", func(t *testing.T) {
		env := testEnv(t)
		sock, err := rundir.HubSocket(env)
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		failedStart(t, env, runIn(t, env, quiet()), "is live")
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("the live socket was touched: %v", err)
		}
		_ = c.Close()
	})
	t.Run("not a socket", func(t *testing.T) {
		env := testEnv(t)
		sock, err := rundir.HubSocket(env)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sock, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		failedStart(t, env, runIn(t, env, quiet()), "is not a socket")
		if b, err := os.ReadFile(sock); err != nil || string(b) != "mine" {
			t.Fatalf("the file at the socket's path was touched: %q, %v", b, err)
		}
	})
	t.Run("replaced between probe and unlink", func(t *testing.T) {
		env := testEnv(t)
		hk := quiet()
		var replacement rundir.FileID
		hk.clearStale = func(path string) (rundir.SocketFate, error) {
			// The probe's instant: a refusing socket, replaced at its path
			// before its unlink; what the real probe answers then.
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				return rundir.SocketLive, err
			}
			ln.SetUnlinkOnClose(false)
			t.Cleanup(func() { _ = ln.Close() })
			replacement, err = rundir.SocketID(path)
			if err != nil {
				return rundir.SocketLive, err
			}
			return rundir.SocketReplaced, nil
		}
		failedStart(t, env, runIn(t, env, hk), "was replaced")
		sock, _ := rundir.HubSocket(env)
		if now, err := rundir.SocketID(sock); err != nil || now != replacement {
			t.Fatalf("the replacement was touched: %v, %v (it was %v)", now, err, replacement)
		}
	})
}

// failedStart checks a hub that could not come up: its ready line says why
// (holding want), Run returns that error, and it has let the lock go with no
// record left.
func failedStart(t *testing.T, env rundir.Env, rn *running, want string) {
	t.Helper()
	line := rn.line(t)
	if line.OK || line.Held != nil || !strings.Contains(line.Error, want) {
		t.Fatalf("ready line %+v, want not ok saying %q", line, want)
	}
	if err := rn.stopped(t); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Run = %v, want an error saying %q", err, want)
	}
	lockFree(t, env)
	if fi, err := os.Lstat(recordPath(t, env)); err == nil && fi.Mode().IsRegular() {
		t.Fatal("a failed start left a record")
	}
}

// TestAStartThatFailsUndoesItsStages (§3.5): a failure at each stage undoes
// every stage before it — the listener closed, the socket it bound unlinked,
// the record removed, each by identity — and lets the lock go last; what it
// did not make is left.
func TestAStartThatFailsUndoesItsStages(t *testing.T) {
	t.Run("the lock", func(t *testing.T) {
		env := testEnv(t)
		cache := filepath.Join(env.Home, ".cache", "craze")
		if err := os.MkdirAll(cache, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "hubs"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		rn := runIn(t, env, quiet())
		line := rn.line(t)
		if line.OK || line.Held != nil || !strings.Contains(line.Error, "take the hub lock") {
			t.Fatalf("ready line %+v, want not ok: the lock", line)
		}
		if err := rn.stopped(t); err == nil {
			t.Fatal("Run returned nil for a lock it could not take")
		}
		sock := filepath.Join(env.CrazeRuntimeDir, nsOf(t, env), "hub.sock")
		absent(t, "a socket", sock)
	})
	t.Run("the runtime directory", func(t *testing.T) {
		env := testEnv(t)
		if err := os.Chmod(env.CrazeRuntimeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		failedStart(t, env, runIn(t, env, quiet()), "the runtime directory")
	})
	t.Run("the bind", func(t *testing.T) {
		env := testEnv(t)
		hk := quiet()
		hk.afterClear = func(path string) {
			// Something put at the path between the probe and the bind.
			_ = os.WriteFile(path, []byte("in the way"), 0o600)
		}
		failedStart(t, env, runIn(t, env, hk), "bind")
		sock, _ := rundir.HubSocket(env)
		if b, err := os.ReadFile(sock); err != nil || string(b) != "in the way" {
			t.Fatalf("what was in the way was touched: %q, %v", b, err)
		}
	})
	t.Run("the record", func(t *testing.T) {
		env := testEnv(t)
		if err := os.Mkdir(filepath.Join(hubsDir(t, env), nsOf(t, env)+".json"), 0o700); err != nil {
			t.Fatal(err)
		}
		failedStart(t, env, runIn(t, env, quiet()), "record")
		sock, _ := rundir.HubSocket(env)
		absent(t, "the socket it bound", sock)
		if fi, err := os.Lstat(recordPath(t, env)); err != nil || !fi.IsDir() {
			t.Fatalf("the directory in the record's place was touched: %v", err)
		}
	})
}

// idleHub is a hub whose idle grace and registry reads are the test's.
type idleHub struct {
	rn      *running
	h       *hub
	g       *grace
	hosts   chan time.Time
	decided chan bool
}

// runIdle runs a hub with the test's grace, registry tick and decision
// hooks, beforeIdle at each grace's end, and waits until its loop has made
// its first arming.
func runIdle(t *testing.T, env rundir.Env, beforeIdle func(*idleHub)) *idleHub {
	t.Helper()
	ih := &idleHub{g: &grace{}, hosts: make(chan time.Time), decided: make(chan bool, 16)}
	hk := quiet()
	hk.hostsTick = ih.hosts
	ih.g.install(hk)
	hk.decided = func(teardown bool) { ih.decided <- teardown }
	looping := make(chan struct{})
	hk.looping = func() { close(looping) }
	if beforeIdle != nil {
		hk.beforeIdle = func() { beforeIdle(ih) }
	}
	ih.rn = runIn(t, env, hk)
	ih.rn.line(t)
	ih.h = ih.rn.serving(t)
	select {
	case <-looping:
	case <-time.After(step):
		t.Fatalf("the hub's loop did not start within %v", step)
	}
	return ih
}

// decision is the hub's next idle decision, within step.
func (ih *idleHub) decision(t *testing.T) bool {
	t.Helper()
	select {
	case d := <-ih.decided:
		return d
	case <-time.After(step):
		t.Fatalf("no idle decision within %v", step)
		return false
	}
}

// tickHosts makes the hub read the registry now.
func (ih *idleHub) tickHosts(t *testing.T) {
	t.Helper()
	select {
	case ih.hosts <- time.Now():
	case <-time.After(step):
		t.Fatalf("the hub's loop did not take a registry tick within %v", step)
	}
}

// bindHost is a live host in env's registry, closed when the test ends.
func bindHost(t *testing.T, env rundir.Env) *rundir.Host {
	t.Helper()
	h, err := rundir.Bind(env, rundir.NewHostID(), rundir.Entry{CrazeSessionID: "s-" + rundir.NewHostID()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestAHubNobodyConnectsToExits (P12): with no client and no live host the
// hub arms its grace at start, and at its end tears down: no record, no
// socket, the lock free.
func TestAHubNobodyConnectsToExits(t *testing.T) {
	env := testEnv(t)
	ih := runIdle(t, env, nil)
	ih.g.fire(t)
	if !ih.decision(t) {
		t.Fatal("an idle hub's grace ended and it stayed")
	}
	if err := ih.rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	absent(t, "the record", recordPath(t, env))
	absent(t, "the socket", ih.h.sock)
	lockFree(t, env)
	if !strings.Contains(ih.rn.stderr.String(), "stopping: idle") {
		t.Fatalf("the hub did not say it stopped idle: %s", ih.rn.stderr)
	}
}

// TestALiveHostKeepsTheHub (P12): a live host in the HOME registry keeps the
// hub — no grace is armed while it lives — and its leaving starts the grace.
func TestALiveHostKeepsTheHub(t *testing.T) {
	env := testEnv(t)
	host := bindHost(t, env)
	ih := runIdle(t, env, nil)
	ih.g.none(t)
	ih.tickHosts(t)
	ih.tickHosts(t) // a second tick is taken only once the first is done
	ih.g.none(t)
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	ih.tickHosts(t)
	ih.g.fire(t)
	if !ih.decision(t) {
		t.Fatal("the hub stayed after its last host left and its grace ended")
	}
	if err := ih.rn.stopped(t); err != nil {
		t.Fatal(err)
	}
}

// TestAClientOrAHostAtExpiryKeepsTheHub (P12, prox's epoch rule): at the end
// of a grace the decision re-reads everything under the lifecycle lock, so
// whatever arrived in the instant before it keeps the hub — a client still
// connected, a client that came and went (the epoch it bumped), a host that
// appeared — and only a later grace with nothing ends it.
func TestAClientOrAHostAtExpiryKeepsTheHub(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   func(t *testing.T, env rundir.Env, ih *idleHub) func()
	}{
		{"a client connected", func(t *testing.T, env rundir.Env, ih *idleHub) func() {
			c := dial(t, ih.h.sock)
			c.hello(t)
			return func() { _ = c.nc.Close() }
		}},
		{"a client that came and went", func(t *testing.T, env rundir.Env, ih *idleHub) func() {
			c := dial(t, ih.h.sock)
			c.hello(t)
			_ = c.nc.Close()
			waitFor(t, "the hub counts the client gone", func() bool { return clients(ih.h) == 0 })
			return func() {}
		}},
		{"a host that appeared", func(t *testing.T, env rundir.Env, ih *idleHub) func() {
			h := bindHost(t, env)
			return func() {
				_ = h.Close()
				ih.tickHosts(t)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			// The instant before the first decision: the hub's loop is held
			// there while the test makes the arrival.
			var once atomic.Bool
			arrive, arrived := make(chan struct{}, 1), make(chan struct{})
			var release sync.Once
			ih := runIdle(t, env, func(*idleHub) {
				if once.CompareAndSwap(false, true) {
					arrive <- struct{}{}
					<-arrived
				}
			})
			// After runIdle's: a failed test lets the loop go before its
			// cleanup waits for the hub to stop.
			t.Cleanup(func() { release.Do(func() { close(arrived) }) })
			ih.g.fire(t)
			select {
			case <-arrive:
			case <-time.After(step):
				t.Fatalf("the grace's end did not reach its decision within %v", step)
			}
			leave := tc.at(t, env, ih)
			release.Do(func() { close(arrived) })
			if ih.decision(t) {
				t.Fatalf("%s at the grace's end, and the hub decided to exit", tc.name)
			}
			if !ih.rn.isRunning() {
				t.Fatal("the hub stopped")
			}
			c := dial(t, ih.h.sock)
			c.hello(t)
			_ = c.nc.Close()
			leave()
			// Nothing left: a grace armed from now ends it. An arming the
			// test's fire meets first may be an older idle period's — the
			// epoch the last client bumped then decides it, and the loop
			// arms again.
			for {
				ih.g.fire(t)
				if ih.decision(t) {
					break
				}
			}
			if err := ih.rn.stopped(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// registry is a hub's registry reads in a test's hands (hostsRead): the
// hosts it lists, or the error it fails with.
type registry struct {
	mu    sync.Mutex
	hosts []rundir.Entry
	err   error
}

// install makes r the hub's registry for one test.
func (r *registry) install(t *testing.T) {
	t.Helper()
	setVar(t, &hostsRead, func(rundir.Env) ([]rundir.Entry, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.err != nil {
			return nil, r.err
		}
		return slices.Clone(r.hosts), nil
	})
}

// list makes the registry list hosts; fail makes it fail.
func (r *registry) list(hosts ...rundir.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts, r.err = hosts, nil
}

func (r *registry) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// TestARegistryReadThatFailsKeepsTheHub (P12, review r18): a registry read
// that fails is no host's leaving. The hosts the last good read listed stand
// — a hub with live hosts arms no grace on a failed read — and a failed read
// at a grace's end decides nothing: the hub stays, and arms no grace until a
// read succeeds. Once one does, the idle rule is as before. The failure is
// said once however often it repeats within readErrSayEvery, and the
// recovery once.
func TestARegistryReadThatFailsKeepsTheHub(t *testing.T) {
	broken := errors.New("the registry is broken")
	host := rundir.Entry{HostID: rundir.NewHostID()}
	said := func(t *testing.T, ih *idleHub) {
		t.Helper()
		log := ih.rn.stderr.String()
		if n := strings.Count(log, "the registry cannot be read"); n != 1 {
			t.Fatalf("the failure said %d times, want once: %s", n, log)
		}
		if n := strings.Count(log, "the registry can be read again"); n != 1 {
			t.Fatalf("the recovery said %d times, want once: %s", n, log)
		}
	}
	// exits checks a hub with nothing — every read good — still goes idle.
	exits := func(t *testing.T, ih *idleHub, reg *registry) {
		t.Helper()
		reg.list()
		ih.tickHosts(t)
		for {
			ih.g.fire(t)
			if ih.decision(t) {
				break
			}
		}
		if err := ih.rn.stopped(t); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("hosts live, then reads that fail", func(t *testing.T) {
		reg := &registry{}
		reg.install(t)
		reg.list(host)
		ih := runIdle(t, testEnv(t), nil)
		ih.g.none(t)
		reg.fail(broken)
		ih.tickHosts(t)
		ih.tickHosts(t) // a second tick is taken only once the first is done
		ih.g.none(t)
		if !ih.rn.isRunning() {
			t.Fatal("the hub stopped")
		}
		reg.list(host)
		ih.tickHosts(t)
		ih.tickHosts(t)
		ih.g.none(t)
		exits(t, ih, reg)
		said(t, ih)
	})

	t.Run("a read that fails at the decision", func(t *testing.T) {
		reg := &registry{}
		reg.install(t)
		var once atomic.Bool
		ih := runIdle(t, testEnv(t), func(*idleHub) {
			if once.CompareAndSwap(false, true) {
				// A host appears as the grace ends — and the read that would
				// list it fails.
				reg.list(host)
				reg.fail(broken)
			}
		})
		select {
		case <-ih.g.armed:
		default:
			t.Fatal("a hub with nothing armed no grace at start")
		}
		ih.g.fire(t)
		if ih.decision(t) {
			t.Fatal("the registry read at the grace's end failed, and the hub decided to exit")
		}
		ih.tickHosts(t)
		ih.tickHosts(t)
		ih.g.none(t)
		if !ih.rn.isRunning() {
			t.Fatal("the hub stopped")
		}
		reg.list(host)
		ih.tickHosts(t)
		ih.tickHosts(t)
		ih.g.none(t)
		exits(t, ih, reg)
		said(t, ih)
	})
}

// TestALostRecordOrSocketStopsTheHub (§3.5's Lost): a record removed, or a
// socket replaced, is a hub nobody finds; at its next look it tears down —
// removing what is still its own and nothing else — so a replacement can
// start.
func TestALostRecordOrSocketStopsTheHub(t *testing.T) {
	t.Run("record removed", func(t *testing.T) {
		env := testEnv(t)
		hk := quiet()
		lost := make(chan time.Time)
		hk.lostTick = lost
		rn := runIn(t, env, hk)
		rn.line(t)
		h := rn.serving(t)
		lost <- time.Now() // still its own: nothing happens
		dial(t, h.sock).hello(t)
		if err := os.Remove(recordPath(t, env)); err != nil {
			t.Fatal(err)
		}
		lost <- time.Now()
		if err := rn.stopped(t); err != nil {
			t.Fatal(err)
		}
		absent(t, "the socket", h.sock)
		lockFree(t, env)
		if !strings.Contains(rn.stderr.String(), "is gone") {
			t.Fatalf("the hub did not say why: %s", rn.stderr)
		}
	})
	t.Run("socket replaced", func(t *testing.T) {
		env := testEnv(t)
		hk := quiet()
		lost := make(chan time.Time)
		hk.lostTick = lost
		rn := runIn(t, env, hk)
		rn.line(t)
		h := rn.serving(t)
		if err := os.Remove(h.sock); err != nil {
			t.Fatal(err)
		}
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.sock, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		ln.SetUnlinkOnClose(false)
		defer ln.Close()
		replacement, err := rundir.SocketID(h.sock)
		if err != nil {
			t.Fatal(err)
		}
		lost <- time.Now()
		if err := rn.stopped(t); err != nil {
			t.Fatal(err)
		}
		if now, err := rundir.SocketID(h.sock); err != nil || now != replacement {
			t.Fatalf("the replacement socket was touched: %v, %v", now, err)
		}
		absent(t, "the record", recordPath(t, env))
		lockFree(t, env)
		if !strings.Contains(rn.stderr.String(), "is another file now") {
			t.Fatalf("the hub did not say why: %s", rn.stderr)
		}
	})
}

// TestTeardownRemovesItsFilesBeforeItsLock (§3.5): when the lock is let go
// the record and the socket are already gone, so the next hub to take it
// never finds a predecessor's files with nobody behind them.
func TestTeardownRemovesItsFilesBeforeItsLock(t *testing.T) {
	env := testEnv(t)
	hk := quiet()
	// The teardown is held just before its release while the test looks.
	reached, checked := make(chan struct{}), make(chan struct{})
	var release sync.Once
	hk.beforeRelease = func() {
		close(reached)
		<-checked
	}
	rn := runIn(t, env, hk)
	// After runIn's: cleanups run last first, so a failed test lets the
	// teardown go before it waits for it.
	t.Cleanup(func() { release.Do(func() { close(checked) }) })
	sock := rn.line(t).Socket
	rn.serving(t)
	rn.sigs <- syscall.SIGINT
	select {
	case <-reached:
	case <-time.After(step):
		t.Fatalf("the teardown did not reach its lock's release within %v: %s", step, rn.stderr)
	}
	absent(t, "the record", recordPath(t, env))
	absent(t, "the socket", sock)
	lockHeld(t, env)
	release.Do(func() { close(checked) })
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	lockFree(t, env)
}

// TestTeardownIsBoundedWithAStuckPeer (§3.5): a peer that sends and never
// reads leaves the hub's answer to it stuck in its write — work in flight —
// and the teardown waits for it only up to its bound, then closes the
// connection under it and still removes its files and lets the lock go.
func TestTeardownIsBoundedWithAStuckPeer(t *testing.T) {
	setVar(t, &teardownBound, time.Second)
	setVar(t, &writeWait, time.Hour)
	env := testEnv(t)
	hk := quiet()
	// The hub's send buffer at its least, so a few unread answers fill it.
	hk.accepted = func(uc *net.UnixConn) { _ = uc.SetWriteBuffer(1) }
	rn := runIn(t, env, hk)
	sock := rn.line(t).Socket
	h := rn.serving(t)
	c := dial(t, sock)
	c.hello(t)
	// Requests the peer never reads the answers to, on a goroutine of their
	// own: the hub answers until its socket is full, then its reader blocks
	// in that write — work in flight that cannot end.
	req, _ := protocol.MarshalLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": protocol.MethodSessionsList})
	batch := []byte(strings.Repeat(string(req), 256))
	flooded := make(chan struct{})
	go func() {
		defer close(flooded)
		for {
			if _, err := c.nc.Write(batch); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = c.nc.Close()
		<-flooded
	})
	stuck := time.Time{}
	waitFor(t, "an answer stuck in its write for a second", func() bool {
		if !writingNow(h) {
			stuck = time.Time{}
			return false
		}
		if stuck.IsZero() {
			stuck = time.Now()
		}
		return time.Since(stuck) >= time.Second
	})
	start := time.Now()
	rn.sigs <- syscall.SIGTERM
	if err := rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > teardownBound+5*time.Second {
		t.Fatalf("the teardown took %v with a stuck peer, past its bound %v", took, teardownBound)
	}
	if !strings.Contains(rn.stderr.String(), "work in flight did not end") {
		t.Fatalf("the teardown did not cut off the stuck answer: %s", rn.stderr)
	}
	absent(t, "the record", recordPath(t, env))
	absent(t, "the socket", sock)
	lockFree(t, env)
}

// writingNow reports whether one of h's connections is in a write, with an
// answer in flight.
func writingNow(h *hub) bool {
	h.life.mu.Lock()
	inflight := h.life.inflight
	h.life.mu.Unlock()
	if inflight == 0 {
		return false
	}
	for _, c := range h.life.openConns() {
		if c.writing.Load() {
			return true
		}
	}
	return false
}

// TestSIGHUPIsIgnored: SIGHUP is said and dropped; the hub serves on.
func TestSIGHUPIsIgnored(t *testing.T) {
	env := testEnv(t)
	rn := runIn(t, env, quiet())
	sock := rn.line(t).Socket
	rn.serving(t)
	rn.sigs <- syscall.SIGHUP
	waitFor(t, "the hub says it ignored SIGHUP", func() bool { return strings.Contains(rn.stderr.String(), "SIGHUP ignored") })
	dial(t, sock).hello(t)
	if !rn.isRunning() {
		t.Fatal("SIGHUP stopped the hub")
	}
}

// TestAConnectionMustSayHelloInTime (§3.5): a connection that says nothing
// for helloWait is closed.
func TestAConnectionMustSayHelloInTime(t *testing.T) {
	setVar(t, &helloWait, 200*time.Millisecond)
	env := testEnv(t)
	rn := runIn(t, env, quiet())
	c := dial(t, rn.line(t).Socket)
	_ = c.nc.SetReadDeadline(time.Now().Add(step))
	if line, err := c.lr.ReadLine(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a silent connection was not closed: %q, %v", line, err)
	}
}

// TestConnectionsPastTheCapAndForeignPeersAreClosed (§3.5): past connsMax a
// connection is closed at accept, unanswered; so is a peer the accept check
// refuses (another user's, played by a check that refuses everyone).
func TestConnectionsPastTheCapAndForeignPeersAreClosed(t *testing.T) {
	t.Run("the cap", func(t *testing.T) {
		setVar(t, &connsMax, 2)
		env := testEnv(t)
		rn := runIn(t, env, quiet())
		sock := rn.line(t).Socket
		h := rn.serving(t)
		dial(t, sock).hello(t)
		dial(t, sock).hello(t)
		waitFor(t, "two clients", func() bool { return clients(h) == 2 })
		third := dial(t, sock)
		_ = third.nc.SetDeadline(time.Now().Add(step))
		_ = protocol.WriteLine(third.nc, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "hello",
			"params": protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}}})
		if line, err := third.lr.ReadLine(); err == nil {
			t.Fatalf("a connection past the cap was answered: %s", line)
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal("a connection past the cap was left open")
		}
	})
	t.Run("a refused peer", func(t *testing.T) {
		env := testEnv(t)
		hk := quiet()
		hk.peer = rundir.PeerCheck(-1)
		rn := runIn(t, env, hk)
		c := dial(t, rn.line(t).Socket)
		_ = c.nc.SetDeadline(time.Now().Add(step))
		_ = protocol.WriteLine(c.nc, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "hello",
			"params": protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}}})
		if line, err := c.lr.ReadLine(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("a refused peer was answered or left open: %q, %v", line, err)
		}
		waitFor(t, "the hub says it refused the peer", func() bool { return strings.Contains(rn.stderr.String(), "refused a connection") })
	})
}

// TestTheOrphanSweepRuns (§3.9): at start and at every tick, its report in
// the log.
func TestTheOrphanSweepRuns(t *testing.T) {
	var n atomic.Int32
	setVar(t, &sweepOrphans, func(env rundir.Env, now time.Time) (rundir.OrphanReport, error) {
		n.Add(1)
		return rundir.OrphanReport{Locks: []string{"0123456789ab"}}, nil
	})
	env := testEnv(t)
	hk := quiet()
	tick := make(chan time.Time)
	hk.sweepTick = tick
	rn := runIn(t, env, hk)
	rn.line(t)
	waitFor(t, "the sweep at start", func() bool { return n.Load() == 1 })
	tick <- time.Now()
	waitFor(t, "the sweep at a tick", func() bool { return n.Load() == 2 })
	if !strings.Contains(rn.stderr.String(), "orphan sweep: 1 orphan locks") {
		t.Fatalf("the sweep's report is not in the log: %s", rn.stderr)
	}
}

// TestTheHubLogIsNeverSwept (§3.5): the host logs' sweep takes only files
// named for a host id, so the hub's log and its rotation, however old, stay.
func TestTheHubLogIsNeverSwept(t *testing.T) {
	env := testEnv(t)
	path, err := LogPath(env)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "hub-"+nsOf(t, env)+".log" {
		t.Fatalf("the hub's log is %s", path)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, p := range []string{path, path + ".1"} {
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A host's log as old, the sweep's positive control.
	hostLog := filepath.Join(filepath.Dir(path), rundir.NewHostID()+".log")
	if err := os.WriteFile(hostLog, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(hostLog, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := rundir.SweepHostLogs(env, time.Now(), 7*24*time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	absent(t, "the gone host's log", hostLog)
	for _, p := range []string{path, path + ".1"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the host-log sweep took %s: %v", p, err)
		}
	}
}

// TestIdleGraceFromEnv: CRAZE_HUB_IDLE as a positive Go duration, else the
// default — with why, for a value it could not take.
func TestIdleGraceFromEnv(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want time.Duration
		why  bool
	}{
		{"", DefaultIdleGrace, false},
		{"2s", 2 * time.Second, false},
		{" 500ms ", 500 * time.Millisecond, false},
		{"0", DefaultIdleGrace, true},
		{"-1s", DefaultIdleGrace, true},
		{"soon", DefaultIdleGrace, true},
	} {
		got, why := IdleGraceFromEnv(func(k string) string {
			if k == IdleEnv {
				return tc.v
			}
			return ""
		})
		if got != tc.want || (why != "") != tc.why {
			t.Errorf("%s=%q: %v, %q", IdleEnv, tc.v, got, why)
		}
	}
}

// TestParseReadyLine: what makes a hub's ready line one (ReadyLine.invalid).
func TestParseReadyLine(t *testing.T) {
	for _, tc := range []struct {
		in  string
		bad bool
	}{
		{`{"ok":true,"hubId":"0123456789ab","ns":"0123abcd","socket":"/s"}`, false},
		{`{"ok":true,"hubId":"0123456789ab","ns":"0123abcd","socket":"/s","future":1}`, false},
		{`{"ok":true,"hubId":"x","ns":"0123abcd","socket":"/s"}`, true},
		{`{"ok":true,"hubId":"0123456789ab","ns":"0123","socket":"/s"}`, true},
		{`{"ok":true,"hubId":"0123456789ab","ns":"0123abcd","socket":"s"}`, true},
		{`{"ok":true,"hubId":"0123456789ab","ns":"0123abcd","socket":"/s","error":"e"}`, true},
		{`{"held":{}}`, false},
		{`{"held":{"pid":7,"hubId":"0123456789ab"}}`, false},
		{`{"held":{"pid":-1}}`, true},
		{`{"held":{"hubId":"nope"}}`, true},
		{`{"ok":false,"error":"e"}`, false},
		{`{"ok":false}`, true},
		{`not json`, true},
	} {
		_, why := parseReady([]byte(tc.in))
		if (why != "") != tc.bad {
			t.Errorf("%s: %q", tc.in, why)
		}
	}
	// A line round-trips.
	l := ReadyLine{OK: true, HubID: "0123456789ab", NS: "0123abcd", Socket: "/s"}
	b, _ := json.Marshal(l)
	if got, why := parseReady(b); why != "" || got != l {
		t.Fatalf("%s: %+v, %q", b, got, why)
	}
}
