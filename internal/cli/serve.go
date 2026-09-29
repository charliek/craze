package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// The TUI process serves its session (plan 027 C13): every craze TUI binds a
// control socket in the runtime namespace and serves the engine it runs over
// it (§3.7–§3.8), and claims every session it loads or starts, so that a
// second craze cannot load the same session beside it (SQ16, §3.9).
//
// The order runTUI follows, and why:
//
//  1. A host id is minted first. The session claims write it into their lock
//     files, with a socket or without one.
//  2. resolveLoad claims a --continue's row before anything is built, and
//     refuses one another craze holds: a refused run spawns no agent and
//     creates no socket.
//  3. Only a run that reaches tui.Run binds and serves (serveControl), unless
//     the opt-out says not to; a failure to serve is a warning.
//  4. tui.Run hands every engine it installs to runHost.onEngine: the server
//     serves it, a new session's id is claimed, and the registry entry is
//     rewritten — again once the engine is ready.
//  5. tui.Run returns after its exit tail has closed the engine, whose end
//     closes each connection once what it admitted is written (X16 4). Then
//     runHost.close, before craze's deferred stderr is flushed — so a flush
//     that blocks cannot hold the socket or a claim, and the teardown's own
//     warnings are flushed — and deferred too, so every return after step 1
//     runs it: the server gets flushWait to finish writing, is closed, the
//     socket and registry entry are unlinked, and last every session claim is
//     released.

// controlSocketEnv turns the control socket off for one run (plan 027 §4 item
// 6).
const controlSocketEnv = "CRAZE_CONTROL_SOCKET"

// flushWait is how long the server is given, once the engine has closed, to
// write what its connections still owe — an attached client's final records
// and reset{session_closed} (§3.7) — before it is closed. A variable only so
// that the teardown-order test can lengthen it (never in parallel): that test
// queues megabytes behind a full socket buffer, which a starved CPU cannot
// write in 500 ms, and what it proves is that the teardown waits for the
// flush — not how fast the flush is.
var flushWait = 500 * time.Millisecond

// The teardown's other bounds, and SQ16's.
const (
	// flushPoll is how often the flush wait looks at the open connections.
	flushPoll = 10 * time.Millisecond
	// closeWait bounds Server.Close: past it a handler parked where nothing
	// can interrupt it (the index flock) is left to finish on its own.
	closeWait = time.Second
	// indexWait bounds the session index's lock when a row is given its craze
	// id before it is claimed (sessions.Store.EnsureCrazeID).
	indexWait = 2 * time.Second
)

// teardownStep is told each step of runHost.close as it happens: a seam for
// the teardown order's test, a no-op in production.
var teardownStep = func(string) {}

// hostClose is controlHost.close's Host.Close: a seam for the test of a
// teardown warning, (*rundir.Host).Close in production.
var hostClose = (*rundir.Host).Close

// controlSocketOn is whether this run serves a control socket (plan 027 §4
// item 6). Either switch turns it off and neither can turn it on against the
// other: CRAZE_CONTROL_SOCKET set to a false value, or `control_socket =
// false` in config.toml. It fails closed, as the journal's switch does,
// because it is an access switch — a typo must not open a control surface the
// user meant to close: a CRAZE_CONTROL_SOCKET strconv.ParseBool cannot read,
// and a config.toml that cannot be read or parsed or whose control_socket is
// not a bool (tui.ConfigControlSocket), are off too, each saying so in one
// line on diag. An explicit false is the user's own choice and is silent.
//
// CRAZE_CONTROL_SOCKET is read trimmed, and empty counts as unset.
func controlSocketOn(diag io.Writer) bool {
	if raw := strings.TrimSpace(os.Getenv(controlSocketEnv)); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			fmt.Fprintf(diag, "craze: control socket off: %s=%q is not a bool\n", controlSocketEnv, raw)
			return false
		}
		if !on {
			return false
		}
	}
	if on, why := tui.ConfigControlSocket(); !on {
		if why != "" {
			fmt.Fprintf(diag, "craze: control socket off: %s\n", why)
		}
		return false
	}
	return true
}

// runHost is what a TUI run holds for other processes: its session claims,
// always, and its control socket, when it serves one.
type runHost struct {
	claims *sessionClaims
	ctl    *controlHost // nil when not serving

	socketOnce sync.Once
	claimsOnce sync.Once
}

// onEngine is tui.Config.OnEngine: it runs inside Update when a picker
// confirms, so nothing here blocks. In order: the server serves the engine (a
// replacement closes every connection of the old one, X25); the engine's
// craze id is claimed unless this process already holds it — a --continue or
// picker load claimed it before the build, and a second flock on another
// descriptor would contend with ourselves; the registry entry is rewritten
// for it, and again once it is ready (controlHost.track).
func (r *runHost) onEngine(eng *engine.Engine) { r.publish(eng) }

// publish is onEngine, answering a channel closed once the registry entry
// first carries eng's identity — the first of its rewrites to land (every
// rewrite writes the whole identity) — which is when a resolver reading the
// registry can find the session by its craze id: craze serve's ready line
// waits for it (plan 030 §3.4). nil, never closed, when no socket is served.
func (r *runHost) publish(eng *engine.Engine) <-chan struct{} {
	if r.ctl != nil {
		r.ctl.server.SetEngine(eng)
	}
	st := eng.State()
	r.claims.ensure(st.CrazeSessionID)
	if r.ctl == nil {
		return nil
	}
	return r.ctl.track(eng, st)
}

// close is the run's teardown: the socket first (controlHost.close), then
// every session claim — last, so a session stays claimed until nothing of
// this process can still act on it. Each half runs once; a later call, or one
// after craze serve's own stop sequence ran them (serveHost.stop), is a
// no-op.
func (r *runHost) close() {
	r.closeSocket()
	r.releaseClaims()
}

// closeSocket is the socket's half of close, once: S2's close order
// (controlHost.close), when there is a socket.
func (r *runHost) closeSocket() {
	r.socketOnce.Do(func() {
		if r.ctl != nil {
			r.ctl.close()
		}
	})
}

// releaseClaims is the claims' half of close, once.
func (r *runHost) releaseClaims() {
	r.claimsOnce.Do(func() {
		r.claims.releaseAll()
		teardownStep("released")
	})
}

// keepClaims is craze serve's stop sequence leaving the claims to the
// process's exit, which ends a start that did not join at the same moment
// (serveHost.stop): release is a no-op from here. In a test's process, which
// does not exit, they are held until the test ends.
func (r *runHost) keepClaims() {
	r.claimsOnce.Do(func() { teardownStep("claims left to the exit") })
}

// controlHost is a bound control socket and the server on it (plan 027 §3.7,
// §3.8), and the one goroutine that rewrites its registry entry.
type controlHost struct {
	host   *rundir.Host
	server *control.Server
	diag   io.Writer

	mu sync.Mutex
	// update is how the writer rewrites the entry: host.Update, or a test's
	// stand-in that fails a rewrite. Read under mu.
	update func(func(*rundir.Entry)) error
	// eng is the engine last handed to track: a rewrite queued for any other
	// is dropped when its turn comes.
	eng *engine.Engine
	// engStop ends eng's ready watcher, when eng is replaced or the host
	// closes.
	engStop chan struct{}
	queue   []rewrite
	warned  bool

	wake       chan struct{} // a rewrite was queued
	stop       chan struct{} // the host is closing
	writerDone chan struct{}
}

// rewrite is one registry rewrite, for the engine it describes, and the
// landing it reports to when it succeeds.
type rewrite struct {
	eng    *engine.Engine
	fn     func(*rundir.Entry)
	landed *landing
}

// landing is the first successful rewrite of one engine's identity: ch is
// closed when it lands, once, whichever of that engine's rewrites it is.
type landing struct {
	once sync.Once
	ch   chan struct{}
}

func (l *landing) land() { l.once.Do(func() { close(l.ch) }) }

// serveControl binds the control socket and serves it (plan 027 §3.8's
// "Bind"): the host lock, the socket (0600 in its 0700 directory), the
// registry entry, then a server checking each peer's uid before it reads a
// byte. workspace is the absolute workspace. A failure is one line on diag —
// `craze: control socket off: <why>` — and nil: the run continues exactly as
// it would without a socket.
//
// The info document tells a client what this host is (plan 030 §3.7): its
// permission mode, from the run's --force/--no-force (force), and its start,
// the one instant the registry entry's startedAt records too — for the
// server's life, a picker replacing the engine included. The server is built
// with no coordinator: a TUI-hosted session is stopped by its own TUI's quit,
// and refuses session.stop, stop_unsupported (§3.6a).
func serveControl(env rundir.Env, hostID, workspace string, force bool, diag io.Writer) *controlHost {
	h, err := bindControl(env, hostID, workspace, force, nil, diag)
	if err != nil {
		fmt.Fprintf(diag, "craze: control socket off: %v\n", err)
		return nil
	}
	return h
}

// bindControl is serveControl's bind and serve, answering a failure rather
// than warning about it: for craze serve a socket it cannot bind is fatal
// (plan 030 §3.3), since a headless host is reachable through nothing else.
// A failure has bound nothing — rundir.Bind unwinds what it built — and
// started no goroutine. stop is the host's lifecycle coordinator
// (control.Options.Stop): set, the server serves session.stop and advertises
// capabilities.stop; nil — the TUI-hosted path — it refuses it,
// stop_unsupported.
func bindControl(env rundir.Env, hostID, workspace string, force bool, stop control.StopFunc, diag io.Writer) (*controlHost, error) {
	started := time.Now().UTC()
	host, err := rundir.Bind(env, hostID, rundir.Entry{StartedAt: started, Workspace: workspace})
	if err != nil {
		return nil, err
	}
	h := &controlHost{
		host: host,
		// No Log: nothing may reach the terminal while the TUI owns it, and
		// each connection's note reaches the session's journal through the
		// engine (control_conn) — a headless host's too, whose log would
		// only repeat it.
		server: control.New(control.Options{
			PeerCheck:      rundir.PeerCheck(os.Geteuid()),
			HostID:         hostID,
			Workspace:      workspace,
			PermissionMode: permissionMode(force),
			StartedAt:      started,
			Stop:           stop,
		}),
		diag:       diag,
		update:     host.Update,
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		writerDone: make(chan struct{}),
	}
	go func() {
		// nil once the server has closed; anything else stopped accepting
		// for good, and the run goes on without new connections.
		if err := h.server.Serve(host.Listener()); err != nil {
			fmt.Fprintf(diag, "craze: control socket stopped accepting: %v\n", err)
		}
	}()
	go h.writeLoop()
	return h, nil
}

// permissionMode is the info document's word for how a host spawned its
// agent (plan 030 §3.7, SF-60): --force is bypass, --no-force prompt.
func permissionMode(force bool) protocol.PermissionMode {
	if force {
		return protocol.PermissionBypass
	}
	return protocol.PermissionPrompt
}

// track rewrites the registry entry for eng, not ready, and once eng.Ready()
// closes again, ready when the start succeeded. An engine that closed rather
// than started gets no second rewrite, and neither does one replaced first.
// Each rewrite writes the engine's whole identity as it stands at that moment
// (identity), never a part of it: a rewrite that failed is made good by the
// next that succeeds, so the entry is never ready under a stale or empty
// identity. It queues and returns: the writes happen on the writer goroutine,
// in order. The channel it answers is closed when the first of eng's
// rewrites lands — a failed one is made good by the next (the ready rewrite)
// — and never for an engine replaced first or a host closed first.
func (h *controlHost) track(eng *engine.Engine, st engine.State) <-chan struct{} {
	stop := make(chan struct{})
	landed := &landing{ch: make(chan struct{})}
	h.mu.Lock()
	if h.engStop != nil {
		close(h.engStop)
	}
	h.eng, h.engStop = eng, stop
	h.mu.Unlock()
	h.enqueue(eng, identity(st, false), landed)
	go func() {
		select {
		case <-eng.Ready():
		case <-stop:
			return
		case <-h.stop:
			return
		}
		st := eng.State()
		// Ready also closes when the engine closes; a session that closed
		// rather than started owes no rewrite (control's watchReady reads it
		// the same way). A start that failed does: ready false, and whatever
		// provider session id it got.
		if st.Activity == engine.ActivityClosing && !st.StartFailed {
			return
		}
		h.enqueue(eng, identity(st, !st.StartFailed), landed)
	}()
	return landed.ch
}

// identity is a rewrite of the registry entry to st, one engine's state at one
// moment: every member that describes the engine — its craze id, incarnation,
// provider and the provider's session id — and whether it is ready.
func identity(st engine.State, ready bool) func(*rundir.Entry) {
	return func(e *rundir.Entry) {
		e.CrazeSessionID = st.CrazeSessionID
		e.Incarnation = st.Incarnation
		e.Provider = st.Provider.Name
		e.ProviderSessionID = st.SessionID
		e.Ready = ready
	}
}

// enqueue queues a rewrite for the writer, without blocking; landed, when
// set, is told if it succeeds.
func (h *controlHost) enqueue(eng *engine.Engine, fn func(*rundir.Entry), landed *landing) {
	h.mu.Lock()
	h.queue = append(h.queue, rewrite{eng: eng, fn: fn, landed: landed})
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// writeLoop is the registry's one writer: rewrites land in the order they
// were queued, a rewrite for an engine no longer current is dropped, and it
// stops when the host closes (Update answers rundir.ErrClosed) or close says
// so.
func (h *controlHost) writeLoop() {
	defer close(h.writerDone)
	for {
		select {
		case <-h.wake:
		case <-h.stop:
			return
		}
		for {
			h.mu.Lock()
			if len(h.queue) == 0 {
				h.mu.Unlock()
				break
			}
			r := h.queue[0]
			h.queue = h.queue[1:]
			current, update := r.eng == h.eng, h.update
			h.mu.Unlock()
			if !current {
				continue
			}
			if err := update(r.fn); err != nil {
				if errors.Is(err, rundir.ErrClosed) {
					return
				}
				h.warnOnce(err)
				continue
			}
			if r.landed != nil {
				r.landed.land()
			}
		}
	}
}

// warnOnce says, once per run, that the registry entry could not be
// rewritten: a resolver may then read stale facts about this host.
func (h *controlHost) warnOnce(err error) {
	h.mu.Lock()
	warned := h.warned
	h.warned = true
	h.mu.Unlock()
	if !warned {
		fmt.Fprintf(h.diag, "craze: control socket registry entry not updated: %v\n", err)
	}
}

// close is §3.7's close order, fit to X16: tui.Run has closed the engine, and
// the engine's end is closing each connection once what it admitted is
// written. So: the server is given flushWait for that; then Server.Close
// closes the listener and every connection left and joins their goroutines,
// bounded by closeWait (a handler parked in the index flock is not waited
// past it, and there is nothing to tell the user about it); then Host.Close
// unlinks the registry entry and the socket, identity-checked, and the host
// lock under itself — a failure there is one line on diag, since it can leave
// a stale entry or socket behind. The registry writer and the ready watcher
// stop last.
//
// Not bounded: a registry write stalled on a hung filesystem (an NFS hard
// mount) holds Host.Update's lock, so Host.Close — and the claims released
// after it — wait for it, which is accepted because such a filesystem stalls
// the session index's own writes too.
func (h *controlHost) close() {
	teardownStep("flushing")
	deadline := time.Now().Add(flushWait)
	for h.server.OpenConns() > 0 && time.Now().Before(deadline) {
		time.Sleep(flushPoll)
	}
	teardownStep("flushed")
	ctx, cancel := context.WithTimeout(context.Background(), closeWait)
	_ = h.server.Close(ctx)
	cancel()
	teardownStep("server closed")
	if err := hostClose(h.host); err != nil {
		fmt.Fprintf(h.diag, "craze: control socket not cleaned up: %v\n", err)
	}
	teardownStep("unlinked")
	h.mu.Lock()
	if h.engStop != nil {
		close(h.engStop)
		h.engStop = nil
	}
	h.mu.Unlock()
	close(h.stop)
	<-h.writerDone
}

// sessionClaims is every session claim this process holds (plan 027 §3.9,
// SQ16), by craze id, held for the process's life and released, in any order,
// at the very end of the run's teardown. Every claim is taken by
// claimSession, the one function both load paths and a new session's engine
// go through.
type sessionClaims struct {
	env    rundir.Env
	hostID string
	diag   io.Writer
	// index gives a row its durable craze id: the session index
	// (*sessions.Store), or a test's stand-in that changes the index first.
	index crazeIDIndex
	// indexWait bounds the index lock EnsureCrazeID takes; a test shortens it.
	indexWait time.Duration
	// required makes every claim a condition of running (craze serve, plan
	// 030 §3.3): a claim that cannot be taken is an *unclaimedError from
	// claimRow and require, never the warning the TUI's X30 fallback gives
	// (astra r3-c2 1). Set before the first claim, never after.
	required bool

	mu   sync.Mutex
	held map[string]*rundir.Claim
	// skipped is every id whose claim could not even be attempted, and has
	// already been warned about: onEngine does not try it again.
	skipped map[string]bool
	closed  bool
}

// crazeIDIndex is the one method of the session index claimRow uses.
type crazeIDIndex interface {
	EnsureCrazeID(row sessions.Row, within time.Duration) (string, error)
}

func newSessionClaims(env rundir.Env, hostID string, diag io.Writer) *sessionClaims {
	return &sessionClaims{
		env:       env,
		hostID:    hostID,
		diag:      diag,
		index:     &sessions.Store{KnownProvider: knownProvider},
		indexWait: indexWait,
		held:      map[string]*rundir.Claim{},
		skipped:   map[string]bool{},
	}
}

// errClaimsClosed is a claim that completed after the run's teardown released
// every claim: it is released at once and refused.
var errClaimsClosed = errors.New("craze is exiting")

// claimSession is §3.9's one function: it takes <HOME>/.cache/craze/locks/
// <crazeID>.lock without blocking and writes "<pid> <hostId>" into it
// (rundir.ClaimSession), and keeps the *rundir.Claim for the process's life.
// An id this process already holds answers at once, taking nothing — a second
// flock on another descriptor would contend with ourselves. A session another
// process holds is a *rundir.HeldError naming it; any other error is a claim
// that could not be attempted.
//
// release gives back a claim this call took, and nothing else: it is for a
// picker whose answer arrived for an attempt it had abandoned.
func (c *sessionClaims) claimSession(crazeID string) (release func(), err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errClaimsClosed
	}
	if _, ok := c.held[crazeID]; ok {
		return func() {}, nil
	}
	claim, err := rundir.ClaimSession(c.env, crazeID, c.hostID)
	if err != nil {
		return nil, err
	}
	c.held[crazeID] = claim
	return func() { c.release(crazeID, claim) }, nil
}

// release gives back one claim, if it is still the one held for id.
func (c *sessionClaims) release(id string, claim *rundir.Claim) {
	c.mu.Lock()
	if c.held[id] == claim {
		delete(c.held, id)
	}
	c.mu.Unlock()
	_ = claim.Release()
}

// tried reports whether id is claimed by this process, or was tried and could
// not be.
func (c *sessionClaims) tried(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, held := c.held[id]
	return held || c.skipped[id]
}

// ensure claims a session's craze id for its engine (runHost.onEngine) unless
// this process already holds it, or already tried and warned. A new session's
// id is a fresh UUIDv7, so a held one cannot happen; it is a warning if it
// does, as is a claim that could not be attempted: the session runs either
// way.
func (c *sessionClaims) ensure(id string) {
	if id == "" || c.tried(id) {
		return
	}
	if _, err := c.claimSession(id); err != nil && !errors.Is(err, errClaimsClosed) {
		c.skip(id, err)
	}
}

// require claims id for a host that must hold it (required: craze serve's new
// session, whose id it has just minted, before anything is built), or says
// why it cannot; an id this process holds already answers at once. It is
// claimSession with every failure an *unclaimedError, a session another
// process holds included: a fresh UUIDv7 held elsewhere is not a session to
// attach to.
func (c *sessionClaims) require(id string) *unclaimedError {
	if _, err := c.claimSession(id); err != nil {
		return &unclaimedError{err: err}
	}
	return nil
}

// unclaimedError is a claim a host that must hold one could not take
// (sessionClaims.required): the lock tree or the index it needs cannot be
// used, and the host does not run.
type unclaimedError struct{ err error }

func (e *unclaimedError) Error() string {
	return "the session cannot be claimed: " + e.err.Error()
}

func (e *unclaimedError) Unwrap() error { return e.err }

// exit is craze serve's refusal for it: exit 1, in serve's own words — the
// TUI never runs with required set, so no shared refusal says it.
func (e *unclaimedError) exit() error {
	return &exitError{code: 1, msg: "craze serve: " + e.Error(), cause: e}
}

// unclaimedBy is the *unclaimedError err carries, or nil.
func unclaimedBy(err error) *unclaimedError {
	var u *unclaimedError
	if errors.As(err, &u) {
		return u
	}
	return nil
}

// skip warns, once, that id's claim was not taken, and remembers it.
func (c *sessionClaims) skip(id string, err error) {
	c.mu.Lock()
	seen := c.skipped[id]
	if id != "" {
		c.skipped[id] = true
	}
	c.mu.Unlock()
	if !seen {
		fmt.Fprintf(c.diag, "craze: session lock not taken: %v\n", err)
	}
}

// claimRow is SQ16's two steps for a row about to be loaded, before anything
// is built, for --continue and the resume picker alike (§3.9): the row is
// given its durable craze id under the index's own lock, bounded
// (EnsureCrazeID), and that id is claimed (claimSession). It answers the id
// the engine is to be built with and a release for the claim it took.
//
// Three answers refuse: an error wrapping atomicfile.ErrLockBusy (another
// writer held the index for the whole bound), one wrapping
// sessions.ErrNotInIndex (a row with no id left the index — evicted — since it
// was read: another loader may already hold it under the id it was given, and
// an id minted here would load the same provider session beside it; the index
// is read again by trying again), and a *rundir.HeldError (another craze holds
// the session). Anything else — an index or a lock tree that cannot be read or
// written at all, an I/O error — is a claim that could not even be attempted:
// it is a warning, and the load proceeds unclaimed with the row's own id
// (X30). The lock protects against a second craze; it must not lock the user
// out of their own session because of their filesystem.
//
// Unless the claims are required (craze serve): then a claim that could not
// be attempted refuses too, an *unclaimedError, and so does a legacy row that
// cannot be given its durable id — an id the engine minted instead would be
// this host's alone, and a second host loading the row would claim another.
func (c *sessionClaims) claimRow(row sessions.Row) (id string, release func(), err error) {
	noop := func() {}
	id, err = c.index.EnsureCrazeID(row, c.indexWait)
	switch {
	case errors.Is(err, atomicfile.ErrLockBusy), errors.Is(err, sessions.ErrNotInIndex):
		return "", nil, err
	case err != nil && row.CrazeID == "" && c.required:
		return "", nil, &unclaimedError{err: err}
	case err != nil && row.CrazeID == "":
		// No durable id, and none can be written: nothing to claim. The engine
		// mints one, which the row takes on its next write and onEngine claims.
		c.skip("", err)
		return "", noop, nil
	case err != nil:
		// The row's id is durable already — an id is never replaced once a
		// row has one — so it is still the one to claim.
		id = row.CrazeID
	}
	release, err = c.claimSession(id)
	var held *rundir.HeldError
	switch {
	case errors.As(err, &held), errors.Is(err, errClaimsClosed):
		return "", nil, err
	case err != nil && c.required:
		return "", nil, &unclaimedError{err: err}
	case err != nil:
		c.skip(id, err)
		return id, noop, nil
	}
	return id, release, nil
}

// pickerClaim is tui.Config.ClaimSession: claimRow, run from the picker's
// tea.Cmd, with each refusal worded for the picker's error row
// (pickerRefusal).
func (c *sessionClaims) pickerClaim(row sessions.Row) (string, func(), error) {
	id, release, err := c.claimRow(row)
	if err != nil {
		return "", nil, errors.New(c.pickerRefusal(err))
	}
	return id, release, nil
}

// pickerRefusal is claimRow's refusal as the resume picker's error row says
// it: refusal's words and, for a session another craze holds, where it can be
// reached instead (plan 027 §3.9; X31 5: PR 4 adds the hint) — `— craze
// attach --session <hostId>` only when the holder's live entry serves that
// very session (holderEntry), else why it cannot be (`— it serves no control
// socket`, `— it serves another session`), and nothing more when its lock
// names no holder yet (pid ?). The picker itself still builds nothing:
// turning a running picker's TUI into a client is not worth its complexity
// (§3.9).
//
// The hint names the holder's host id, which --session resolves exactly as it
// does a craze session id: twelve characters, so the whole command sits on one
// row of the picker's box, where a craze id's thirty-six would be broken at
// its hyphens across two.
//
// It reads the registry, so it runs where the claim does: in the picker's
// tea.Cmd, never in Update.
func (c *sessionClaims) pickerRefusal(err error) string {
	msg := refusal(err)
	var held *rundir.HeldError
	if !errors.As(err, &held) {
		return msg
	}
	entry, serves := holderEntry(c.env, held)
	if serves == holderServes {
		return msg + " — craze attach --session " + entry.HostID
	}
	return msg + serves.refusalSuffix()
}

// refusal is claimRow's refusal as the user reads it: the session's holder,
// the busy index, or the index that changed under the load. Where a held
// session can be reached instead is its callers' to add: the picker's hint
// (pickerRefusal), --continue's attach or its no-socket refusal (attachHeld).
//
// A held session is "already running" (plan 030 §3.7, SQ16's wording): its
// holder is, from plan 030 on, most often a detached host (craze serve) with
// no terminal of its own, not another craze someone has open — so the refusal
// names what the session is doing, and the pid who holds it.
func refusal(err error) string {
	var held *rundir.HeldError
	switch {
	case errors.As(err, &held):
		pid := "?"
		if held.Holder.PID > 0 {
			pid = strconv.Itoa(held.Holder.PID)
		}
		return "that session is already running (pid " + pid + ")"
	case errors.Is(err, atomicfile.ErrLockBusy):
		return "the session index is busy — try again"
	case errors.Is(err, sessions.ErrNotInIndex):
		return "the session index changed — try again"
	}
	return err.Error()
}

// releaseAll releases every claim, and any claim that completes afterwards is
// refused (errClaimsClosed): the run is over.
func (c *sessionClaims) releaseAll() {
	c.mu.Lock()
	c.closed = true
	held := c.held
	c.held = map[string]*rundir.Claim{}
	c.mu.Unlock()
	for _, claim := range held {
		_ = claim.Release()
	}
}

// hostLifecycle is a headless host's lifecycle coordinator (plan 030 §3.6a):
// the one place its stop is decided. Every way a stop reaches the host asks it
// through request — session.stop through the control server's seam
// (control.Options.Stop, stopFunc: the server calls it at most once, its own
// attach fence already up), SIGINT and SIGTERM through craze serve's signal
// loop, and the idle watcher (idle.go) once its close fence has found the host
// eligible, or its socket lost — and the first request IS the stop. craze serve's own goroutine,
// parked on stopping, then runs the stop sequence once (serveHost.stop): the
// attach fence, the engine's close, S2's close order, the claims, and the exit.
// Every later request joins it: answered — a session.stop's receipt is the
// server's — and otherwise nothing.
//
// request never blocks and never calls into the server or the engine: the
// server calls it from a handler, which Server.Close waits for, so the work is
// always the parked goroutine's.
type hostLifecycle struct {
	once     sync.Once
	stopping chan struct{}

	mu  sync.Mutex
	why string
}

func newHostLifecycle() *hostLifecycle {
	return &hostLifecycle{stopping: make(chan struct{})}
}

// request asks for the stop, saying why: the first request closes stopping,
// and every later one joins it.
func (l *hostLifecycle) request(why string) {
	l.once.Do(func() {
		l.mu.Lock()
		l.why = why
		l.mu.Unlock()
		close(l.stopping)
	})
}

// stopFunc is control.Options.Stop: a session.stop the server accepted.
func (l *hostLifecycle) stopFunc(r control.StopRequest) {
	l.request("session.stop from client " + r.Client)
}

// cause is why the host is stopping: the first request's words, "" before
// any.
func (l *hostLifecycle) cause() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.why
}
