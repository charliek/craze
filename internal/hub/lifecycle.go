package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/version"
)

// The hub process, craze hub (plan 032 §3.5): Run, from its lock to its
// teardown.
//
// Start, staged — each stage undone, in reverse, by a failure at any later
// one, and the lock let go last:
//
//  1. The namespace's lock, hubs/<ns>.lock (rundir.LockHub), without
//     blocking, its holder line written. A hub that loses it to another
//     answers held on its ready pipe — the holder its line names — and exits
//     0: its spawner waits for the holder (Ensure's rendezvous). The hub's
//     log opens only once the lock is held, so a loser never writes, rotates
//     or truncates the winner's.
//  2. The runtime directory, validated (rundir.HubSocket).
//  3. hub.sock probed (rundir.ClearStaleSocket, safe on that path only
//     because the lock is held): absent, or a socket that refuses — stale —
//     and is still the one probed, which is unlinked. A live socket, a
//     non-socket and a socket replaced while it was probed are refused, and
//     left as they are.
//  4. hub.sock bound, mode 0600, never unlinked by its listener's close
//     (SetUnlinkOnClose(false)), its (dev, ino) recorded.
//  5. The record, hubs/<ns>.json, written (HubLock.WriteRecord).
//  6. ok on the ready pipe.
//
// Then the hub serves (server.go), sweeps (rundir.SweepOrphans, at once and
// every sweepEvery: a dead host's orphan lock, temporaries and refused
// socket; it kills no agent) and watches, on one goroutine (loop):
//
//   - Lost: each lostEvery, the record and the socket must still be the files
//     it made, by (dev, ino); one gone or replaced is a hub nobody can find,
//     which tears down so a replacement can start.
//   - Signals: SIGTERM and SIGINT tear down; SIGHUP is logged and ignored
//     (caught with signal.Notify by its caller, never ignored, which a host
//     it spawns would inherit).
//   - Idle (P12, owner decision 7): the hub is busy while it has a client —
//     a connection past hello — a create in flight (session.create's, whose
//     waiter may have gone: create.go), or any live host is in the HOME
//     registry (rundir.Hosts, every hostsEvery, and at every round of the
//     roster's poll while it polls: pollHosts). When none holds — at start
//     too — it arms a check after the grace, capturing the lifecycle epoch,
//     which every admission (a hello, a create) and every host appearing
//     bumps. At the check, under the lifecycle lock, it reads the registry
//     again and re-checks all three and the epoch: still idle and unchanged,
//     it decides, and from that instant every connection is closed
//     unanswered and every hello refused closing. A
//     registry read that fails is no host's leaving: the hosts the last good
//     read listed stand, and until a read succeeds again no grace is armed
//     and no check decides (said in the log, at most every readErrSayEvery).
//
// The roster (roster.go) is served from the start; its poll runs only while a
// client wants it.
//
// Teardown, bounded whatever its peers do — by teardownBound, and the two
// waits of its own below past it: quiesce (refuse
// every new connection, every request and every create; the lifecycle lock
// is the barrier; no answer waits for the poll any more), wait for the work
// in flight — the creates in flight among it, whose waiters wait for them,
// and those whose waiters have gone — then cut every create still running
// (its waiter's connection closes unanswered; a host already started runs
// on: creator.stop) and wait, bounded by createCleanupWait (3 s) of its own
// past teardownBound, for the cut creates whose host was launched to be done
// with it — a host cut before its ready line ended (SIGTERM, createCutGrace,
// SIGKILL) and reaped before Run returns; a stalled world check, which can
// launch nothing now, is not waited for, and a launch whose start has not
// returned (its child stalled before its exec, in its chdir on a hung
// filesystem: no pid yet, and no signal ends it) is left as it is — then,
// bounded by teardownBound still, or by teardownTail (1 s) of its own once
// the waits before have used it up: close the listener, end every roster
// subscription with reset{hub_closing} (resetWait), close every connection —
// a splice's legs half-closed first, then closed once it has ended or
// spliceDrain has passed — stop the roster's poll and the sweep, unlink the
// socket and remove the record — each only while it is still the file the
// hub made — and release the lock last (an explicit LOCK_UN, then close:
// rundir.HubLock.Release). A launch's start runs outside the lifecycle lock
// (creator.launch), so the quiesce that begins the teardown never waits on
// it.

// DefaultIdleGrace is how long a hub with no client and no live host waits
// before it exits (P12): 60 s.
const DefaultIdleGrace = 60 * time.Second

// IdleEnv overrides the idle grace (a Go duration), for tests.
const IdleEnv = "CRAZE_HUB_IDLE"

// IdleGraceFromEnv is the idle grace getenv(IdleEnv) sets, DefaultIdleGrace
// when it is unset, and — with why it was not taken — the default for a value
// that is not a positive Go duration.
func IdleGraceFromEnv(getenv func(string) string) (time.Duration, string) {
	v := strings.TrimSpace(getenv(IdleEnv))
	if v == "" {
		return DefaultIdleGrace, ""
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return DefaultIdleGrace, fmt.Sprintf("%s=%q is not a positive duration; the grace is %v", IdleEnv, v, DefaultIdleGrace)
	}
	return d, ""
}

// The lifecycle's periods and bounds: variables only so a test can shorten
// them (never in parallel).
var (
	// lostEvery is how often the hub checks that its record and socket are
	// its own (Lost).
	lostEvery = time.Second
	// hostsEvery is how often the hub reads the registry for live hosts
	// while it has nothing else to read it for (plan 032 §3.5: 5 s; every
	// round of the roster's poll reads it too: pollHosts).
	hostsEvery = 5 * time.Second
	// sweepEvery is how often the hub sweeps orphans (§3.9).
	sweepEvery = 10 * time.Minute
	// teardownBound bounds the whole teardown: what in-flight work and peers
	// have not finished by then is cut off.
	teardownBound = 10 * time.Second
	// teardownTail bounds the teardown's steps after the creates' — the
	// subscriptions' resets, the splices' drain, the connections' close, the
	// sweep's end — once the waits before them have used teardownBound up.
	teardownTail = time.Second
	// readErrSayEvery is how often a registry that keeps failing to read is
	// said in the hub's log again.
	readErrSayEvery = time.Minute
)

// Seams over rundir, each its real function in production: a test replaces
// one (never in parallel) to force a schedule.
var (
	hostsRead    = rundir.Hosts
	hostsScan    = rundir.HostsScan
	sweepOrphans = rundir.SweepOrphans
)

// Options is a hub's: what it serves, and how it was started.
type Options struct {
	// Env is the namespace's and the registry's (rundir.ProcessEnv()).
	Env rundir.Env
	// Codecs is the build's event and snapshot codecs, for hello: the
	// caller's (internal/cli, which knows them), so the hub imports no
	// provider package.
	Codecs protocol.Codecs
	// Ready is the spawner's ready pipe (TakeReadyPipe), nil for a hub run by
	// hand.
	Ready *ReadyPipe
	// Signals is the process's signals (signal.Notify's SIGHUP, SIGINT and
	// SIGTERM), or a test's own channel; nil is none.
	Signals <-chan os.Signal
	// Stderr is where the hub speaks before its log is open, and when it
	// cannot be; nil discards.
	Stderr io.Writer
	// OpenLog opens the hub's log at path (LogPath's) once the lock is held:
	// the writer, and its close. nil, or one that fails, leaves the hub
	// speaking on Stderr.
	OpenLog func(path string) (io.Writer, func(), error)
	// Creates is what session.create needs (create.go): nil — a test's hub
	// alone — creates nothing, and its hello says sessionCreate false.
	Creates *Creates
	// IdleGrace is the idle exit's grace; 0 is DefaultIdleGrace.
	IdleGrace time.Duration

	// hooks is a test's schedule; nil in production.
	hooks *hooks
}

// hooks is a test's control of a hub's schedule (never in production, never
// in parallel): each field nil is the production behaviour.
type hooks struct {
	// lostTick, hostsTick and sweepTick replace the tickers.
	lostTick, hostsTick, sweepTick <-chan time.Time
	// graceTimer arms the idle grace: the channel that fires at its end, and
	// its stop.
	graceTimer func(time.Duration) (<-chan time.Time, func())
	// beforeIdle runs when a grace ends, before the decision.
	beforeIdle func()
	// decided is told each idle decision: true is the teardown.
	decided func(teardown bool)
	// clearStale replaces stage 3's rundir.ClearStaleSocket.
	clearStale func(string) (rundir.SocketFate, error)
	// afterClear runs between stage 3 and stage 4, with the socket's path.
	afterClear func(string)
	// beforeReady runs at stage 6, before the ready line.
	beforeReady func()
	// serving is told the hub once it serves.
	serving func(*hub)
	// beforeRelease runs in the teardown just before the lock is released.
	beforeRelease func()
	// looping runs on the loop's goroutine once its first registry read and
	// arming are done, before it first waits.
	looping func()
	// peer replaces the accept check (rundir.PeerCheck).
	peer func(*net.UnixConn) (pid, uid int, err error)
	// accepted is told each connection the hub admits, before it is served.
	accepted func(*net.UnixConn)

	// The roster's (roster.go): rosterTicks replaces its poll's ticker;
	// rosterNow is its clock, the poll's and freshness's; rosterDial its
	// dial, and with it rosterCheck its peer check (nil: none);
	// rosterBudget each share of an attempt's budget; flushTimer
	// arms a subscription's flush spacing — every time one is spaced, its
	// wait whatever it is; rosterApplied is told each Snapshot the roster has
	// taken, after it did.
	rosterTicks   <-chan time.Time
	rosterNow     func() time.Time
	rosterDial    func(ctx context.Context, path string) (net.Conn, error)
	rosterCheck   func(*net.UnixConn) error
	rosterBudget  time.Duration
	flushTimer    func(time.Duration) (<-chan time.Time, func())
	rosterApplied func(roster.Snapshot)
	// subscribed runs between a subscription's registration and its reply's
	// write; subscriptions is told, on the connection's goroutine, how many
	// subscriptions the connection holds once it has registered a new one.
	subscribed    func()
	subscriptions func(n int)
	// rosterLocking is told, on c's own goroutine (a sessions.list, what
	// "list") or a subscription's flusher ("flush"), as it is about to take
	// c's write lock for a line of the roster's; rosterTaken once it holds
	// it and has taken that line's roster — the list's snapshot, the
	// flusher's notification — before the line is written.
	rosterLocking func(c *conn, what string)
	rosterTaken   func(c *conn, what string)

	// session.connect's (splice.go): connectDial replaces its dial, and
	// connectCheck its peer check (rundir.DialCheck); handedOff is told each
	// splice as it is handed the connection — its buffered bytes taken,
	// before it runs — and may change them.
	connectDial  func(ctx context.Context, path string) (net.Conn, error)
	connectCheck func(*net.UnixConn) error
	handedOff    func(*splice)
}

// LogPath is the hub's log for env's namespace:
// <Home>/.cache/craze/host-logs/hub-<ns>.log, beside the hosts' logs and
// validated as they are (rundir.HostLogDir). Its name is no host id's, so the
// host logs' sweep (rundir.SweepHostLogs) never takes it, nor its rotation.
func LogPath(env rundir.Env) (string, error) {
	ns, err := rundir.Namespace(env.CrazeDir)
	if err != nil {
		return "", err
	}
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub-"+ns+".log"), nil
}

// Run is craze hub (the file's comment): it returns once the hub has torn
// down — nil, however it was asked to stop — or at once, nil, for a hub that
// lost the namespace's lock (its ready line says held). An error is a hub
// that could not come up; its ready line says why.
func Run(ctx context.Context, o Options) error {
	return newHub(o).run(ctx)
}

// hub is one craze hub process's state.
type hub struct {
	o       Options
	hk      hooks
	grace   time.Duration
	pid     int
	version string

	outMu    sync.Mutex
	out      io.Writer
	closeLog func()

	id   string
	ns   string
	lock *rundir.HubLock
	// sock is the socket's path; sockID its (dev, ino) once bound.
	sock   string
	sockID rundir.FileID
	bound  bool
	ln     *net.UnixListener

	life lifecycle
	srv  *server
	// rs is the roster, from serve on.
	rs *rosterState
	// cr is session.create's, nil for a hub given no Options.Creates.
	cr *creator

	// The loop's own: the armed grace, its stop, and the epoch it was armed
	// at.
	graceC    <-chan time.Time
	graceStop func()
	armed     uint64

	sweepStop chan struct{}
	sweepDone chan struct{}
}

func newHub(o Options) *hub {
	h := &hub{o: o, grace: o.IdleGrace, pid: os.Getpid(), version: version.Version, out: o.Stderr}
	if o.hooks != nil {
		h.hk = *o.hooks
	}
	if h.out == nil {
		h.out = io.Discard
	}
	if h.grace <= 0 {
		h.grace = DefaultIdleGrace
	}
	h.life.init()
	if o.Creates != nil {
		h.cr = newCreator(h, *o.Creates)
	}
	return h
}

// logf writes one line to the hub's log: a UTC timestamp, "craze hub: ", the
// message.
func (h *hub) logf(format string, args ...any) {
	line := time.Now().UTC().Format(time.RFC3339) + " craze hub: " + fmt.Sprintf(format, args...) + "\n"
	h.outMu.Lock()
	defer h.outMu.Unlock()
	_, _ = io.WriteString(h.out, line)
}

func (h *hub) run(ctx context.Context) error {
	ns, err := rundir.Namespace(h.o.Env.CrazeDir)
	if err != nil {
		return h.startFailed(err)
	}
	h.ns = ns
	h.id = rundir.NewHubID()
	lock, err := rundir.LockHub(h.o.Env, h.id)
	if err != nil {
		var held *rundir.HubHeldError
		if errors.As(err, &held) {
			h.logf("%v", err)
			_ = h.o.Ready.Send(ReadyLine{Held: &ReadyHeld{PID: held.PID, HubID: held.HubID}})
			return nil
		}
		return h.startFailed(fmt.Errorf("take the hub lock: %w", err))
	}
	h.lock = lock
	h.openLog()
	h.logf("hub %s (pid %d, craze %s) starting for namespace %s", h.id, h.pid, h.version, h.ns)
	if err := h.start(); err != nil {
		h.undo()
		err = h.startFailed(err)
		h.closeLogNow()
		return err
	}
	if h.hk.beforeReady != nil {
		h.hk.beforeReady()
	}
	if err := h.o.Ready.Send(ReadyLine{OK: true, HubID: h.id, NS: h.ns, Socket: h.sock}); err != nil {
		// The spawner went, or stopped waiting: the hub serves on, for
		// whoever finds its record, and its idle exit ends it otherwise.
		h.logf("the spawner did not take the ready line (%v); serving on", err)
	}
	h.logf("serving %s", h.sock)
	cause := h.serve(ctx)
	h.teardown(cause)
	h.closeLogNow()
	return nil
}

// startFailed is a hub that could not come up: its ready line and its log say
// why, and Run returns it.
func (h *hub) startFailed(err error) error {
	h.logf("cannot start: %v", err)
	_ = h.o.Ready.Send(ReadyLine{Error: err.Error()})
	return err
}

// openLog opens the hub's log (Options.OpenLog at LogPath) once the lock is
// held. A log that cannot be opened is said on Stderr, which the hub keeps
// speaking on.
func (h *hub) openLog() {
	if h.o.OpenLog == nil {
		return
	}
	path, err := LogPath(h.o.Env)
	var w io.Writer
	var closeLog func()
	if err == nil {
		w, closeLog, err = h.o.OpenLog(path)
	}
	if err != nil {
		h.logf("no log: %v", err)
		return
	}
	h.outMu.Lock()
	h.out, h.closeLog = w, closeLog
	h.outMu.Unlock()
}

// closeLogNow closes the log, if one is open — only ever on a clean return:
// a panic's report is the one thing it must not lose (craze serve's rule).
func (h *hub) closeLogNow() {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	if h.closeLog != nil {
		h.closeLog()
		h.closeLog = nil
		h.out = io.Discard
		if h.o.Stderr != nil {
			h.out = h.o.Stderr
		}
	}
}

// start is the staged start's stages 2–5 (the file's comment); whatever it
// built is on h for undo.
func (h *hub) start() error {
	sock, err := rundir.HubSocket(h.o.Env)
	if err != nil {
		return fmt.Errorf("the runtime directory: %w", err)
	}
	h.sock = sock
	clear := rundir.ClearStaleSocket
	if h.hk.clearStale != nil {
		clear = h.hk.clearStale
	}
	fate, err := clear(sock)
	switch {
	case err != nil:
		return fmt.Errorf("probe %s: %w", sock, err)
	case fate == rundir.SocketRemoved:
		h.logf("removed a stale socket at %s", sock)
	case fate == rundir.SocketLive:
		return fmt.Errorf("%s is live: another process serves it, and it is left as it is", sock)
	case fate == rundir.SocketNotSocket:
		return fmt.Errorf("%s is not a socket, and is left as it is", sock)
	case fate == rundir.SocketReplaced:
		return fmt.Errorf("%s was replaced while it was probed, and is left as it is", sock)
	}
	if h.hk.afterClear != nil {
		h.hk.afterClear(sock)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return fmt.Errorf("bind %s: %w", sock, err)
	}
	// Its close must never unlink the path: only the identity-checked unlink
	// does (teardown, undo).
	ln.SetUnlinkOnClose(false)
	h.ln = ln
	id, err := rundir.SocketID(sock)
	if err != nil {
		return fmt.Errorf("the socket just bound: %w", err)
	}
	h.sockID, h.bound = id, true
	if err := os.Chmod(sock, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", sock, err)
	}
	if _, err := h.lock.WriteRecord(rundir.HubRecord{Socket: sock, CrazeVersion: h.version}); err != nil {
		return err
	}
	return nil
}

// undo unwinds a start that failed part way, each stage only if it was
// reached, the lock last: the listener closed, the socket unlinked while it
// is still the one bound, the record removed while it is still the one
// written (Release removes it before it unlocks).
func (h *hub) undo() {
	if h.ln != nil {
		_ = h.ln.Close()
	}
	if h.bound {
		if err := rundir.UnlinkIfOurs(h.sock, h.sockID); err != nil {
			h.logf("unlink %s: %v", h.sock, err)
		}
	}
	if err := h.lock.Release(); err != nil {
		h.logf("release the hub lock: %v", err)
	}
}

// serve runs the server, the sweep and the loop until the hub is asked to
// stop, and answers why.
func (h *hub) serve(ctx context.Context) string {
	h.rs = newRoster(h)
	h.srv = &server{h: h, ln: h.ln, peer: rundir.PeerCheck(os.Geteuid()), acceptDone: make(chan struct{})}
	if h.hk.peer != nil {
		h.srv.peer = h.hk.peer
	}
	go h.srv.accept()
	sweepTick := h.hk.sweepTick
	if sweepTick == nil {
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		sweepTick = t.C
	}
	h.sweepStop, h.sweepDone = make(chan struct{}), make(chan struct{})
	go h.sweeper(sweepTick)
	if h.hk.serving != nil {
		h.hk.serving(h)
	}
	return h.loop(ctx)
}

// loop is the hub's watch (the file's comment), on one goroutine: it answers
// why the hub stops.
func (h *hub) loop(ctx context.Context) string {
	lost, hosts := h.hk.lostTick, h.hk.hostsTick
	if lost == nil {
		t := time.NewTicker(lostEvery)
		defer t.Stop()
		lost = t.C
	}
	if hosts == nil {
		t := time.NewTicker(hostsEvery)
		defer t.Stop()
		hosts = t.C
	}
	defer h.disarm()
	h.refreshHosts()
	h.arm()
	if h.hk.looping != nil {
		h.hk.looping()
	}
	for {
		select {
		case <-ctx.Done():
			return "its context ended"
		case sig := <-h.o.Signals:
			switch sig {
			case syscall.SIGHUP:
				h.logf("SIGHUP ignored")
				continue
			case syscall.SIGTERM:
				return "SIGTERM"
			case syscall.SIGINT:
				return "SIGINT"
			}
			return "signal " + sig.String()
		case <-lost:
			if why := h.lost(); why != "" {
				return why + ", so nobody can find this hub"
			}
		case <-hosts:
			h.refreshHosts()
			h.arm()
		case <-h.life.kick:
			h.arm()
		case <-h.graceC:
			armed := h.armed
			h.graceC, h.graceStop = nil, nil
			if h.hk.beforeIdle != nil {
				h.hk.beforeIdle()
			}
			done := h.decideIdle(armed)
			if h.hk.decided != nil {
				h.hk.decided(done)
			}
			if done {
				return fmt.Sprintf("idle: no client and no live host for %v", h.grace)
			}
			h.arm()
		}
	}
}

// arm arms the idle grace when the hub is idle and none is armed for this
// idle period (its epoch), and disarms it when the hub is busy.
func (h *hub) arm() {
	idle, epoch := h.life.idle()
	if !idle {
		h.disarm()
		return
	}
	if h.graceC != nil && h.armed == epoch {
		return
	}
	h.disarm()
	timer := h.hk.graceTimer
	if timer == nil {
		timer = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		}
	}
	h.graceC, h.graceStop = timer(h.grace)
	h.armed = epoch
}

func (h *hub) disarm() {
	if h.graceStop != nil {
		h.graceStop()
	}
	h.graceC, h.graceStop = nil, nil
}

// refreshHosts reads the registry for live hosts (the idle rule's third
// condition). A registry that cannot be read keeps the last good read's hosts
// and holds the idle exit off (setHostsLocked).
func (h *hub) refreshHosts() {
	entries, err := hostsRead(h.o.Env)
	h.sayRead(h.life.setHosts(entries, err))
}

// pollHosts is the roster's registry read (roster.HubOptions.Hosts), once a
// round while it polls: the idle rule takes it too — a host appearing bumps
// the epoch — and the loop re-arms on it.
func (h *hub) pollHosts() ([]rundir.Entry, error) {
	entries, err := hostsRead(h.o.Env)
	h.sayRead(h.life.setHosts(entries, err))
	h.life.wakeLoop()
	return entries, err
}

// sayRead logs what a registry read had to say (setHostsLocked), if anything.
func (h *hub) sayRead(say string) {
	if say != "" {
		h.logf("%s", say)
	}
}

// decideIdle is the idle decision at the end of a grace armed at epoch armed:
// under the lifecycle lock, the registry read again — and read, not failed —
// and the hub still idle — no client, no create in flight, no live host —
// with the epoch unchanged. Then it is decided: closing is set under the
// same lock, and nothing is admitted after it.
func (h *hub) decideIdle(armed uint64) bool {
	l := &h.life
	l.mu.Lock()
	entries, err := hostsRead(h.o.Env)
	say := l.setHostsLocked(entries, err, time.Now())
	done := !l.closing && l.clients == 0 && l.creates == 0 && !l.hostsLive && !l.readErr && l.epoch == armed
	if done {
		l.closing = true
	}
	l.mu.Unlock()
	h.sayRead(say)
	return done
}

// lost says why the hub can no longer be found — its record or its socket
// gone or another file — or "" while both are its own (Host.Lost's rule: a
// stat that fails another way says nothing).
func (h *hub) lost() string {
	if why := h.lock.RecordLost(); why != "" {
		return why
	}
	id, err := rundir.SocketID(h.sock)
	if err == nil {
		if id != h.sockID {
			return "its socket " + h.sock + " is another file now"
		}
		return ""
	}
	switch _, lerr := os.Lstat(h.sock); {
	case errors.Is(lerr, fs.ErrNotExist):
		return "its socket " + h.sock + " is gone"
	case lerr == nil:
		return "its socket " + h.sock + " is another file now"
	}
	return ""
}

// sweeper runs the orphan sweep at once and at every tick until stopped.
func (h *hub) sweeper(tick <-chan time.Time) {
	defer close(h.sweepDone)
	for {
		r, err := sweepOrphans(h.o.Env, time.Now())
		if err != nil {
			h.logf("orphan sweep: %s; %v", r, err)
		} else {
			h.logf("orphan sweep: %s", r)
		}
		select {
		case <-h.sweepStop:
			return
		case <-tick:
		}
	}
}

// teardown stops the hub (the file's comment), bounded by teardownBound.
func (h *hub) teardown(cause string) {
	h.logf("stopping: %s", cause)
	deadline := time.Now().Add(teardownBound)
	h.life.quiesce()
	h.rs.quiesce()
	if !waitUntil(&h.life.work, deadline) {
		h.logf("work in flight did not end within %v; cut off", teardownBound)
	}
	if h.cr != nil {
		if !waitUntil(&h.life.createWG, deadline) {
			h.logf("creates in flight did not end within %v; cut off, their hosts left as they are", teardownBound)
		}
		h.cr.stop()
		// The launches in flight and the cut creates whose host was launched
		// are done with it before Run returns (r43 5, r45 F2): a host cut
		// before its ready line, or whose start returned after the closing,
		// is ended and reaped, in createCutGrace; a stalled world check
		// launched nothing and is not waited for. A start that has not
		// returned is waited for only so long: its child — stalled in its
		// chdir into the create's directory on a hung filesystem, before its
		// exec — has no pid the hub has been told yet, and a process in that
		// uninterruptible wait does not die of SIGKILL either; it is left as
		// it is, and the launch ends the host itself should its start ever
		// return (creator.launch).
		if !waitUntil(&h.cr.launches, time.Now().Add(createCleanupWait)) {
			h.logf("launches in flight, or hosts the cut creates launched, were not done within %v; a start that has not returned is left as it is",
				createCleanupWait)
		}
	}
	// What follows is bounded by the deadline, or — once the waits before
	// have used it up (a create stalled in its launch holds createWG to the
	// deadline) — by teardownTail of its own: a write deadline already passed
	// fails a write at once, and a subscriber that reads would never be told
	// hub_closing (r45 F2).
	if tail := time.Now().Add(teardownTail); deadline.Before(tail) {
		deadline = tail
	}
	_ = h.ln.Close()
	<-h.srv.acceptDone
	h.rs.closeSubscriptions(deadline)
	// Every connection closed; each splice half-closed first, then closed.
	h.srv.closeAll(deadline)
	if !waitUntil(&h.srv.conns, deadline) {
		h.logf("connections did not close within %v", teardownBound)
	}
	h.rs.stop()
	close(h.sweepStop)
	select {
	case <-h.sweepDone:
	case <-time.After(time.Until(deadline)):
		h.logf("the orphan sweep did not end within %v", teardownBound)
	}
	if err := rundir.UnlinkIfOurs(h.sock, h.sockID); err != nil {
		h.logf("unlink %s: %v", h.sock, err)
	}
	if err := h.lock.RemoveRecord(); err != nil {
		h.logf("remove the record: %v", err)
	}
	if h.hk.beforeRelease != nil {
		h.hk.beforeRelease()
	}
	if err := h.lock.Release(); err != nil {
		h.logf("release the hub lock: %v", err)
	}
	h.logf("stopped")
}

// waitUntil waits for wg until deadline: whether it was done in time.
func waitUntil(wg *sync.WaitGroup, deadline time.Time) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// lifecycle is the hub's admission state, under one lock — the lifecycle
// lock: what the idle decision, the teardown's quiesce and every admission
// serialise on (prox's lifecycleMu).
type lifecycle struct {
	mu sync.Mutex
	// closing is the decision to stop — idle, or asked — after which nothing
	// is admitted.
	closing bool
	// epoch is bumped by every admission (a hello answered) and every host
	// appearing: an idle check armed at an older epoch decides nothing.
	epoch uint64
	// clients is how many connections are past hello and open.
	clients int
	// hostsLive is whether the last good registry read found a live host,
	// and hostIDs which; readErr whether the last read failed (the hub is not
	// idle while it has), and readErrSaid when that was last said.
	hostsLive   bool
	hostIDs     map[string]bool
	readErr     bool
	readErrSaid time.Time
	// conns is every open connection.
	conns map[*conn]struct{}
	// work is every request being answered (begin, end), and inflight how
	// many.
	work     sync.WaitGroup
	inflight int
	// creates is how many creates are in flight (beginCreate, endCreate),
	// each also counted on createWG.
	creates  int
	createWG sync.WaitGroup
	// kick wakes the loop to re-arm: a client left.
	kick chan struct{}
}

func (l *lifecycle) init() {
	l.conns = map[*conn]struct{}{}
	l.hostIDs = map[string]bool{}
	l.kick = make(chan struct{}, 1)
}

// admitConn admits an accepted connection, counting its goroutine on wg:
// false when the hub is closing or already holds connsMax.
func (l *lifecycle) admitConn(c *conn, wg *sync.WaitGroup) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing || len(l.conns) >= connsMax {
		return false
	}
	l.conns[c] = struct{}{}
	wg.Add(1)
	return true
}

// admitClient is a hello's admission: false when the hub is closing.
func (l *lifecycle) admitClient(c *conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return false
	}
	c.client = true
	l.clients++
	l.epoch++
	return true
}

// dropConn forgets a closed connection; a client's leaving wakes the loop,
// which arms the grace if the hub is now idle.
func (l *lifecycle) dropConn(c *conn) {
	l.mu.Lock()
	delete(l.conns, c)
	wasClient := c.client
	if wasClient {
		l.clients--
	}
	l.mu.Unlock()
	if wasClient {
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}
}

// wakeLoop wakes the loop to re-arm (kick's slot of one).
func (l *lifecycle) wakeLoop() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// openConns is every connection still open.
func (l *lifecycle) openConns() []*conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*conn, 0, len(l.conns))
	for c := range l.conns {
		out = append(out, c)
	}
	return out
}

// begin admits one request's answer as in-flight work: false once the hub is
// closing.
func (l *lifecycle) begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return false
	}
	l.work.Add(1)
	l.inflight++
	return true
}

func (l *lifecycle) end() {
	l.mu.Lock()
	l.inflight--
	l.mu.Unlock()
	l.work.Done()
}

// beginCreate admits one create in flight (create.go): refused unavailable,
// reason closing, once the hub is closing, and reason busy at createsMax in
// flight. An admission, it bumps the epoch.
func (l *lifecycle) beginCreate() *protocol.Error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.closing:
		return closingErr()
	case l.creates >= createsMax:
		return refused(protocol.CodeUnavailable, protocol.ReasonBusy,
			"the hub has %d creates in flight already: try again once one has answered", l.creates)
	}
	l.creates++
	l.epoch++
	l.createWG.Add(1)
	return nil
}

// endCreate is a create's end: it wakes the loop, which arms the grace if
// the hub is now idle.
func (l *lifecycle) endCreate() {
	l.mu.Lock()
	l.creates--
	l.mu.Unlock()
	l.createWG.Done()
	l.wakeLoop()
}

// isClosing reports whether the hub has decided to close (quiesce, the idle
// decision): nothing is admitted, and no create launches a host.
func (l *lifecycle) isClosing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closing
}

// quiesce closes admission (the decision, when an idle check has not made it
// already); taking the lock is the barrier behind which every admission made
// before it has finished registering.
func (l *lifecycle) quiesce() {
	l.mu.Lock()
	l.closing = true
	l.mu.Unlock()
}

// idle reports whether the hub is idle now — no client, no create in
// flight, no live host as last read, and that read not failed — and the
// epoch.
func (l *lifecycle) idle() (bool, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.clients == 0 && l.creates == 0 && !l.hostsLive && !l.readErr, l.epoch
}

// setHosts records a registry read (setHostsLocked).
func (l *lifecycle) setHosts(entries []rundir.Entry, err error) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setHostsLocked(entries, err, time.Now())
}

// setHostsLocked records a registry read, under the lifecycle lock, made at
// now. A read that failed changes no host: the last good read's stand, and
// readErr holds the idle exit off (idle, decideIdle) until a read succeeds —
// a failure is no host's leaving. A good read replaces the hosts; one not in
// the last good read bumps the epoch. It answers what the hub's log is to
// say: a failure, at its start and then at most every readErrSayEvery while
// it lasts, and the registry's being readable again.
func (l *lifecycle) setHostsLocked(entries []rundir.Entry, err error, now time.Time) string {
	if err != nil {
		if l.readErr && now.Sub(l.readErrSaid) < readErrSayEvery {
			return ""
		}
		l.readErr, l.readErrSaid = true, now
		return fmt.Sprintf("the registry cannot be read (%v); the hosts it last listed stand, and the hub does not go idle until it can be", err)
	}
	was := l.readErr
	l.readErr = false
	ids := make(map[string]bool, len(entries))
	for _, e := range entries {
		ids[e.HostID] = true
		if !l.hostIDs[e.HostID] {
			l.epoch++
		}
	}
	l.hostIDs = ids
	l.hostsLive = len(ids) > 0
	if was {
		return "the registry can be read again"
	}
	return ""
}
