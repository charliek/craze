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
//     runHost.close, deferred so every return after step 1 runs it: the
//     server gets flushWait to finish writing, is closed, the socket and
//     registry entry are unlinked, and last every session claim is released.

// controlSocketEnv turns the control socket off for one run (plan 027 §4 item
// 6).
const controlSocketEnv = "CRAZE_CONTROL_SOCKET"

// The teardown's bounds, and SQ16's.
const (
	// flushWait is how long the server is given, once the engine has closed,
	// to write what its connections still owe — an attached client's final
	// records and reset{session_closed} (§3.7) — before it is closed.
	flushWait = 500 * time.Millisecond
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

	closeOnce sync.Once
}

// onEngine is tui.Config.OnEngine: it runs inside Update when a picker
// confirms, so nothing here blocks. In order: the server serves the engine (a
// replacement closes every connection of the old one, X25); the engine's
// craze id is claimed unless this process already holds it — a --continue or
// picker load claimed it before the build, and a second flock on another
// descriptor would contend with ourselves; the registry entry is rewritten
// for it, and again once it is ready (controlHost.track).
func (r *runHost) onEngine(eng *engine.Engine) {
	if r.ctl != nil {
		r.ctl.server.SetEngine(eng)
	}
	st := eng.State()
	r.claims.ensure(st.CrazeSessionID)
	if r.ctl != nil {
		r.ctl.track(eng, st)
	}
}

// close is the run's teardown: the socket first (controlHost.close), then
// every session claim — last, so a session stays claimed until nothing of
// this process can still act on it. It runs once; a later call is a no-op.
func (r *runHost) close() {
	r.closeOnce.Do(func() {
		if r.ctl != nil {
			r.ctl.close()
		}
		r.claims.releaseAll()
		teardownStep("released")
	})
}

// controlHost is a bound control socket and the server on it (plan 027 §3.7,
// §3.8), and the one goroutine that rewrites its registry entry.
type controlHost struct {
	host   *rundir.Host
	server *control.Server
	diag   io.Writer

	mu sync.Mutex
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

// rewrite is one registry rewrite, for the engine it describes.
type rewrite struct {
	eng *engine.Engine
	fn  func(*rundir.Entry)
}

// serveControl binds the control socket and serves it (plan 027 §3.8's
// "Bind"): the host lock, the socket (0600 in its 0700 directory), the
// registry entry, then a server checking each peer's uid before it reads a
// byte. workspace is the absolute workspace. A failure is one line on diag —
// `craze: control socket off: <why>` — and nil: the run continues exactly as
// it would without a socket.
func serveControl(env rundir.Env, hostID, workspace string, diag io.Writer) *controlHost {
	host, err := rundir.Bind(env, hostID, rundir.Entry{StartedAt: time.Now().UTC(), Workspace: workspace})
	if err != nil {
		fmt.Fprintf(diag, "craze: control socket off: %v\n", err)
		return nil
	}
	h := &controlHost{
		host: host,
		// No Log: nothing may reach the terminal while the TUI owns it, and
		// each connection's note reaches the session's journal through the
		// engine (control_conn).
		server: control.New(control.Options{
			PeerCheck: rundir.PeerCheck(os.Geteuid()),
			HostID:    hostID,
			Workspace: workspace,
		}),
		diag:       diag,
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
	return h
}

// track rewrites the registry entry for eng — its craze id, incarnation and
// provider, and not ready — and, once eng.Ready() closes, again with the
// provider's session id and whether the start succeeded. An engine that closed
// rather than started gets no second rewrite, and neither does one replaced
// first. It queues and returns: the writes happen on the writer goroutine, in
// order.
func (h *controlHost) track(eng *engine.Engine, st engine.State) {
	stop := make(chan struct{})
	h.mu.Lock()
	if h.engStop != nil {
		close(h.engStop)
	}
	h.eng, h.engStop = eng, stop
	h.mu.Unlock()
	h.enqueue(eng, func(e *rundir.Entry) {
		e.CrazeSessionID = st.CrazeSessionID
		e.Incarnation = st.Incarnation
		e.Provider = st.Provider.Name
		e.ProviderSessionID = ""
		e.Ready = false
	})
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
		h.enqueue(eng, func(e *rundir.Entry) {
			if st.Provider.Name != "" {
				e.Provider = st.Provider.Name
			}
			e.ProviderSessionID = st.SessionID
			e.Ready = !st.StartFailed
		})
	}()
}

// enqueue queues a rewrite for the writer, without blocking.
func (h *controlHost) enqueue(eng *engine.Engine, fn func(*rundir.Entry)) {
	h.mu.Lock()
	h.queue = append(h.queue, rewrite{eng: eng, fn: fn})
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
			current := r.eng == h.eng
			h.mu.Unlock()
			if !current {
				continue
			}
			if err := h.host.Update(r.fn); err != nil {
				if errors.Is(err, rundir.ErrClosed) {
					return
				}
				h.warnOnce(err)
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
// lock under itself. The registry writer and the ready watcher stop last.
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
	_ = h.host.Close()
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
	index  *sessions.Store
	// indexWait bounds the index lock EnsureCrazeID takes; a test shortens it.
	indexWait time.Duration

	mu   sync.Mutex
	held map[string]*rundir.Claim
	// skipped is every id whose claim could not even be attempted, and has
	// already been warned about: onEngine does not try it again.
	skipped map[string]bool
	closed  bool
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
// Only two answers refuse: an error wrapping atomicfile.ErrLockBusy (another
// writer held the index for the whole bound) and a *rundir.HeldError (another
// craze holds the session). Anything else — an index that cannot be written, a
// lock tree that fails validation, an I/O error — is a claim that could not
// even be attempted: it is a warning, and the load proceeds unclaimed with the
// row's own id. The lock protects against a second craze; it must not lock
// the user out of their own session because of their filesystem.
func (c *sessionClaims) claimRow(row sessions.Row) (id string, release func(), err error) {
	noop := func() {}
	id, err = c.index.EnsureCrazeID(row, c.indexWait)
	switch {
	case errors.Is(err, atomicfile.ErrLockBusy):
		return "", nil, err
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
	case err != nil:
		c.skip(id, err)
		return id, noop, nil
	}
	return id, release, nil
}

// pickerClaim is tui.Config.ClaimSession: claimRow, run from the picker's
// tea.Cmd, with each refusal worded for the picker's error row.
func (c *sessionClaims) pickerClaim(row sessions.Row) (string, func(), error) {
	id, release, err := c.claimRow(row)
	if err != nil {
		return "", nil, errors.New(refusal(err))
	}
	return id, release, nil
}

// refusal is claimRow's refusal as the user reads it: the session's holder,
// or the busy index. PR 2 names no `craze attach` — it does not exist until
// PR 4, which adds the hint.
func refusal(err error) string {
	var held *rundir.HeldError
	switch {
	case errors.As(err, &held):
		pid := "?"
		if held.Holder.PID > 0 {
			pid = strconv.Itoa(held.Holder.PID)
		}
		return "that session is open in another craze (pid " + pid + ")"
	case errors.Is(err, atomicfile.ErrLockBusy):
		return "the session index is busy — try again"
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
