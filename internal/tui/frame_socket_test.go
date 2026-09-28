package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/transcript"
)

// The frame goldens over the socket (plan 027 §3.16, PR 4, C29): the host a
// socket run's model attaches to, which the frame runner builds through
// frameSocketHook (frame.go) and this file installs for the package's tests
// alone, so the production package links no server for `craze frame`.
//
// A socket run is the S4 host shape, made from the same builder as the two
// in-process runs:
//
//  1. The builder's session is built NoPrimary (frameNoPrimary, set while
//     runFrameModes calls the builder for the socket run): nothing reads the
//     host's primary, so nothing may be put on it.
//  2. Its engine is built as setSession builds one (engineOptions) and served
//     by internal/control on a short /tmp/czg-* socket (t.TempDir() overflows
//     sun_path on macOS), with the server's MaxBudget raised for the matrix.
//  3. A remote.Session dials it and attaches with when: "now" BEFORE the
//     engine starts, so the model's stream carries every event from the
//     first, as the primary does in process; the first Restore is the empty
//     session's, which the model draws nothing for (applyRestore).
//  4. The frame runner starts the program over that session (Config.Backend —
//     never Viewer: a socket golden is the host TUI over a remote backend),
//     then the host starts its engine as a headless host does: Start, then
//     Started. The model's Start returns on the session's readiness, and a
//     session/load's replay reaches it live.
//  5. The run's one connection is tapped (wireTap): a second dial, a second
//     attach or any reset fails the run — no golden may depend on the reset
//     path (§3.4).
//
// The capture's boundary is the host engine's head; the capture is taken as
// in process, and then the model's fold is held against the host engine's
// model at one seq (socketHost.match; TestSocketGoldensMatchTheEngine). The
// shutdown closes the session (a view close), then the host's engine, then
// its server (socketHost.end).

// installFrameSocketHost is the socket transport's host builder, for the
// whole package (TestMain).
func installFrameSocketHost() { frameSocketHook = buildSocketHost }

// frameNoPrimary is what a golden builder builds its session with: false for
// the in-process runs, whose model reads the primary, and true while the
// socket run calls the builder (runFrameModes) — the Stub's NewStubNoPrimary
// (frameStub) and agent.Options.NoPrimary for the fake agent and native. No
// builder's session differs otherwise.
var frameNoPrimary bool

// frameStub is the Stub a golden builder builds: NewStub, or NewStubNoPrimary
// while the socket run builds its host's session (frameNoPrimary).
func frameStub() *Stub { return newStub(frameNoPrimary) }

// buildFor calls build for a run over tr: NoPrimary for the socket's host
// (frameNoPrimary), which is false again however build returns — a builder's
// t.Fatal included.
func buildFor[T any](tr frameTransport, build func() T) T {
	frameNoPrimary = tr == transportSocket
	defer func() { frameNoPrimary = false }()
	return build()
}

// frameBudget is the matrix's subscription budget: the server's MaxBudget and
// what the socket run's attach asks for, far above the 4× default (§3.7) — a
// host with no primary never back-pressures a burst, so the budget is what
// stands between a golden's burst and a slow_consumer reset.
var frameBudget = control.Budget{MaxItems: 1 << 20, MaxBytes: 1 << 30}

// frameStreamBytes is the socket run's client-side stream bound, raised for
// the same reason (remote.Options.StreamBytes).
const frameStreamBytes = 1 << 30

// matchSnapshotBytes is the budget the host's model is cut at for the check:
// nothing a golden holds is windowed at it.
const matchSnapshotBytes = 1 << 30

// socketMatches counts the checks that found the model's fold to be the host
// engine's model: TestSocketGoldensMatchTheEngine holds it to moving.
// socketRuns counts the socket hosts built.
var socketMatches, socketRuns atomic.Int64

// reportSocketRuns prints the socket runs' totals when CRAZE_SOCKET_STATS is
// set (TestMain), as the parity watch prints its own.
func reportSocketRuns() {
	if os.Getenv("CRAZE_SOCKET_STATS") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "socket: %d socket runs, %d engine matches\n", socketRuns.Load(), socketMatches.Load())
}

// ------------------------------------------------------------ the registry

// socketHosts is the engine behind each socket run's session, keyed by the
// backend the model holds (plan 027 §3.16, "Test access to the session"):
// engineIn, and so engineOf, stubOf and the pump, reach a socket run's host
// engine — and its Stub — exactly as they reach an in-process one.
var socketHosts = struct {
	mu sync.Mutex
	m  map[backend.Backend]*engine.Engine
}{m: map[backend.Backend]*engine.Engine{}}

// registerSocketHost records eng as the engine behind b until the returned
// func is called.
func registerSocketHost(b backend.Backend, eng *engine.Engine) func() {
	socketHosts.mu.Lock()
	socketHosts.m[b] = eng
	socketHosts.mu.Unlock()
	return func() {
		socketHosts.mu.Lock()
		delete(socketHosts.m, b)
		socketHosts.mu.Unlock()
	}
}

// wrapsBackend is a test's wrapper around a backend (V8's jitter): the
// registry looks through it.
type wrapsBackend interface{ wrapped() backend.Backend }

// hostEngineOf is the host engine a registered socket session — b, or the one
// a test's wrapper around it wraps — is served by, nil for any other backend.
func hostEngineOf(b backend.Backend) *engine.Engine {
	for b != nil {
		socketHosts.mu.Lock()
		eng := socketHosts.m[b]
		socketHosts.mu.Unlock()
		if eng != nil {
			return eng
		}
		w, ok := b.(wrapsBackend)
		if !ok {
			return nil
		}
		b = w.wrapped()
	}
	return nil
}

// ------------------------------------------------------------------ the host

// socketHost is one socket run's host: the engine, its server, and the
// model's session to it.
type socketHost struct {
	eng    *engine.Engine
	srv    *control.Server
	served chan error
	dir    string
	sess   *remote.Session
	tap    *wireTap
	// started is closed once the engine's start has returned.
	started    chan struct{}
	startOnce  sync.Once
	unregister func()
	// hooks is socketHostHooks as the host was built.
	hooks socketHostHookSet
}

// socketHostHooks, when a test sets them, are handed the host: built once it
// is attached, before the program runs; afterMatch once the check has passed
// (still parked); afterBarrier once the final barrier has returned. They are
// how the reset tests reach the host's tap, and how
// TestNoResetEscapesTheSocketRunsVerdict makes the host write a reset where
// astra r69 5's schedule has it.
var socketHostHooks socketHostHookSet

type socketHostHookSet struct {
	built, afterMatch, afterBarrier func(*socketHost)
}

// buildSocketHost is frameSocketHook: steps 1–3 above around cfg, whose
// session the builder made NoPrimary.
func buildSocketHost(cfg Config) (*frameHost, error) {
	sess := cfg.Session
	if sess == nil {
		return nil, errors.New("the socket run needs the session its builder made (Config.Session)")
	}
	if cfg.Backend != nil || len(cfg.Resume) > 0 || (cfg.NewSession != nil && !cfg.ProviderLocked) {
		return nil, errors.New("a Config with a backend or a picker has no socket run: pickers are the host TUI's alone")
	}
	if st, ok := sess.(*Stub); ok && !st.NoPrimary {
		return nil, errors.New("the builder built its Stub with a primary: a socket run's host is NoPrimary (frameStub)")
	}
	ws := configWorkspace(cfg.Workspace)
	prov := configProvider(cfg.Provider)
	eng, err := engine.New(sess, engineOptions(cfg.CrazeSessionID, cfg.SessionIndex, ws, prov.Name()))
	if err != nil {
		return nil, err
	}
	if cfg.OnEngine != nil {
		cfg.OnEngine(eng)
	}
	h := &socketHost{eng: eng, served: make(chan error, 1), started: make(chan struct{}), tap: &wireTap{}, hooks: socketHostHooks}
	fail := func(err error) (*frameHost, error) {
		_ = eng.Close()
		if h.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
			_ = h.srv.Close(ctx)
			cancel()
			<-h.served
		}
		if h.dir != "" {
			_ = os.RemoveAll(h.dir)
		}
		return nil, err
	}
	// Under /tmp itself, never $TMPDIR: macOS's /var/folders/… overflows
	// sun_path (§3.8).
	if h.dir, err = os.MkdirTemp("/tmp", "czg-"); err != nil {
		return fail(err)
	}
	path := filepath.Join(h.dir, "s")
	h.srv = control.New(control.Options{Workspace: ws, MaxBudget: frameBudget})
	h.srv.SetEngine(eng)
	l, err := net.Listen("unix", path)
	if err != nil {
		h.srv = nil
		return fail(err)
	}
	go func() { h.served <- h.srv.Serve(l) }()

	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	h.sess, err = remote.DialSession(ctx, path, remote.SessionOptions{
		Client: remote.Options{
			Client:      protocol.ClientInfo{Kind: "test", Name: "tui-frame-socket"},
			Dial:        h.tap.dial,
			StreamBytes: frameStreamBytes,
		},
		When:      protocol.WhenNow,
		Budget:    &protocol.AttachBudget{MaxItems: frameBudget.MaxItems, MaxBytes: frameBudget.MaxBytes},
		Provider:  prov.Name(),
		Workspace: ws,
	})
	if err != nil {
		return fail(fmt.Errorf("dial: %w", err))
	}
	// Before the engine starts (§3.16 step 3): the start's events reach the
	// model live.
	if err := h.sess.Attach(ctx); err != nil {
		_ = h.sess.Close()
		return fail(fmt.Errorf("attach: %w", err))
	}
	h.unregister = registerSocketHost(h.sess, eng)
	socketRuns.Add(1)
	if h.hooks.built != nil {
		h.hooks.built(h)
	}
	run := cfg
	run.Session = nil
	run.Backend = h.sess
	return &frameHost{cfg: run, head: h.head, start: h.start, match: h.match, barrier: h.barrier, end: h.end}, nil
}

// engineOnly names an engine to engineBehind and is nothing else: the host's
// head is read by streamHead's own rule through it (socketHost.head).
type engineOnly struct {
	backend.Backend
	eng *engine.Engine
}

func (e engineOnly) engine() *engine.Engine { return e.eng }

// head is the host engine's stream head, by streamHead's rule: its SyncSeq,
// and 0 once it is closing.
func (h *socketHost) head(timeout time.Duration) (uint64, error) {
	return streamHead(&sessionOwner{eng: engineOnly{eng: h.eng}}, timeout)
}

// start starts the host's engine as a headless host does: Start, then
// Started with what it came to — as the in-process TUI's start command and
// its startedMsg both reach the engine. Its outcome reaches the model as the
// session's ready.
func (h *socketHost) start() {
	h.startOnce.Do(func() {
		go func() {
			defer close(h.started)
			err := h.eng.Start(context.Background())
			h.eng.Started(err)
		}()
	})
}

// match is the check at the end of every socket golden
// (TestSocketGoldensMatchTheEngine): shared, the model's fold — read while the
// program is parked, so it holds still — against the host engine's model cut
// at the same seq, both held here in process: no wire digest (astra r3 21).
// The host's model is its own snapshot, restored at a budget nothing is
// windowed at — Plan 024 A2's reading of "the engine's own model" — and the
// two views (history, state, the last-ended asks) are compared canonically:
// times in UTC with no monotonic reading, errors as the codec carries them,
// since the fold over the socket folded decoded events and the host folded
// the publisher's. false says the two are not at one seq: the host has moved
// on, and the caller lets the fold follow it.
func (h *socketHost) match(shared *transcript.Model) (bool, error) {
	if shared == nil {
		return false, nil
	}
	snap, err := h.eng.TranscriptSnapshot("", matchSnapshotBytes)
	if err != nil {
		return false, fmt.Errorf("frame: the host's model for the check: %w", err)
	}
	if snap.Seq != shared.Seq() {
		return false, nil
	}
	if d := diffSocketViews(socketViewOf(transcript.Restore(snap, transcript.Options{})), socketViewOf(shared)); d != "" {
		return false, fmt.Errorf("frame: over the socket the TUI's fold is not the host engine's model at seq %d: %s", snap.Seq, d)
	}
	socketMatches.Add(1)
	if h.hooks.afterMatch != nil {
		h.hooks.afterMatch(h)
	}
	return true, nil
}

// barrier is the socket run's final barrier (astra r69 5): a session.sync
// round trip on the model's own connection. The host answers it only once
// the connection's attachment has queued every record up to its head — or
// its terminal reset — to the connection's one FIFO writer (control's reply
// barrier), so its reply follows every line the host has written to the
// connection by then, and the client's one reader has read them all — through
// the tap — by the time the reply reaches the call. So a reset the host wrote
// up to here is in the verdict however slow the reader was; one written
// after it precedes the view close's detach reply, which the verdict
// requires to have been read (wireTap.verdict).
func (h *socketHost) barrier(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var res protocol.SyncResult
	err := h.sess.Client().Call(ctx, protocol.MethodSessionSync, protocol.SyncParams{SessionID: h.sess.Info().CrazeSessionID}, &res)
	if err != nil {
		return fmt.Errorf("frame: the socket run's final barrier (session.sync): %w", err)
	}
	if h.hooks.afterBarrier != nil {
		h.hooks.afterBarrier(h)
	}
	return nil
}

// end closes what the host holds — the model's session (a view close,
// already made by the run's shutdown; idempotent), then the engine, then the
// server — and answers what a golden may not depend on: a session built with
// a primary nobody read, a reconnect, a re-attach, a reset, or a view close
// whose detach went unanswered, behind which a reset may lie unread
// (wireTap).
func (h *socketHost) end() error {
	var errs []error
	_ = h.sess.Close()
	h.unregister()
	if n := len(h.eng.Events()); n > 0 {
		errs = append(errs, fmt.Errorf("the host's session put %d events on a primary nobody reads: its builder must build it NoPrimary (frameNoPrimary)", n))
	}
	_ = h.eng.Close()
	h.start() // a run that never started it: nothing to wait for but this
	<-h.started
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	if err := h.srv.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("the host's server close: %w", err))
	}
	if err := <-h.served; err != nil {
		errs = append(errs, fmt.Errorf("the host's server: %w", err))
	}
	_ = os.RemoveAll(h.dir)
	if err := h.tap.verdict(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("frame: the socket run: %w", err)
	}
	return nil
}

// ------------------------------------------------------------------ the tap

// wireTap watches the socket run's connections (remote.Options.Dial): the
// dials, the attaches the client writes and the resets it reads, and the view
// close's detach and its answer. One dial and one attach are the whole of a
// golden's transport, and no reset may be written to it (§3.4, §3.16: any
// reset fails the test).
//
// What it reads is what the client's one reader has read, in the host's
// write order, so every line the host wrote before one the tap has read has
// been read too. Two replies make that everything (astra r69 5): the final
// barrier's (socketHost.barrier), which follows every line written up to it,
// and the view close's detach reply, which the host queues only once the
// attachment's forwarder has stopped — after every line of the subscription
// it wrote. A detach the tap never saw answered — the view close gave up on
// it (remote's close bound) — leaves the lines before its reply unread, and
// fails the run.
type wireTap struct {
	mu       sync.Mutex
	dials    int
	attaches int
	resets   []string
	// detaches is each session.detach the client wrote, by its request id,
	// and whether the tap has read its reply.
	detaches map[string]bool
	conns    []*tapConn
	// holdUntil is when the reads the tap holds (holdReads) go on.
	holdUntil time.Time
}

func (w *wireTap) dial(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	tc := &tapConn{Conn: c, w: w, closed: make(chan struct{})}
	w.mu.Lock()
	w.dials++
	w.conns = append(w.conns, tc)
	w.mu.Unlock()
	return tc, nil
}

// counts is what the tap saw of the stream's transport: the attaches the
// client wrote and the resets it read, by reason.
func (w *wireTap) counts() (attaches int, resets []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attaches, slices.Clone(w.resets)
}

// holdReads makes the client's reader wait d before it reads another byte —
// a reader delayed, as astra r69 5's schedule has it — kicking a read already
// waiting on the socket out of it (a deadline the tap swallows), so nothing
// arriving meanwhile is read before d is up. A closed connection lets go.
func (w *wireTap) holdReads(d time.Duration) {
	w.mu.Lock()
	w.holdUntil = time.Now().Add(d)
	conns := slices.Clone(w.conns)
	w.mu.Unlock()
	for _, c := range conns {
		c.kicked.Store(true)
		_ = c.SetReadDeadline(time.Now())
	}
}

// held waits out a hold (holdReads), or until c is closed.
func (w *wireTap) held(c *tapConn) {
	w.mu.Lock()
	until := w.holdUntil
	w.mu.Unlock()
	if d := time.Until(until); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.closed:
		}
	}
}

// verdict is what the tap saw a golden may not depend on.
func (w *wireTap) verdict() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var errs []error
	if w.dials != 1 {
		errs = append(errs, fmt.Errorf("the client dialled %d times (a reconnect)", w.dials))
	}
	if w.attaches != 1 {
		errs = append(errs, fmt.Errorf("the client attached %d times (a re-attach)", w.attaches))
	}
	if len(w.resets) > 0 {
		errs = append(errs, fmt.Errorf("the host reset the stream: %v", w.resets))
	}
	answered := 0
	for _, ok := range w.detaches {
		if ok {
			answered++
		}
	}
	if len(w.detaches) == 0 || answered < len(w.detaches) {
		errs = append(errs, fmt.Errorf("the view close's detach went unanswered (%d of %d detaches answered): a reset written before its reply may be unread", answered, len(w.detaches)))
	}
	return errors.Join(errs...)
}

// wireLine is the part of a line the tap reads.
type wireLine struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Reason string `json:"reason"`
	} `json:"params"`
}

// saw records one whole line: out is the client's, else the host's.
func (w *wireTap) saw(line []byte, out bool) {
	var l wireLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case out && l.Method == protocol.MethodSessionAttach:
		w.attaches++
	case out && l.Method == protocol.MethodSessionDetach:
		if w.detaches == nil {
			w.detaches = map[string]bool{}
		}
		w.detaches[string(l.ID)] = false
	case !out && l.Method == protocol.NotifyReset:
		w.resets = append(w.resets, l.Params.Reason)
	case !out && l.Method == "" && len(l.ID) > 0:
		if _, ok := w.detaches[string(l.ID)]; ok {
			w.detaches[string(l.ID)] = true
		}
	}
}

// tapConn is one tapped connection. Reads come from the client's one reader
// and writes from its one writer, so each side's partial line is its own.
type tapConn struct {
	net.Conn
	w          *wireTap
	rbuf, wbuf []byte
	// kicked says the tap set a past read deadline to kick a waiting read out
	// (holdReads): the timeout it causes is the tap's, not the client's.
	kicked    atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *tapConn) Read(p []byte) (int, error) {
	for {
		c.w.held(c)
		n, err := c.Conn.Read(p)
		if n == 0 && errors.Is(err, os.ErrDeadlineExceeded) && c.kicked.CompareAndSwap(true, false) {
			_ = c.SetReadDeadline(time.Time{})
			continue
		}
		c.rbuf = c.lines(c.rbuf, p[:n], false)
		return n, err
	}
}

func (c *tapConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *tapConn) Write(p []byte) (int, error) {
	c.wbuf = c.lines(c.wbuf, p, true)
	return c.Conn.Write(p)
}

// lines appends b to buf, records every whole line in it — out says it is the
// client's — and answers the partial rest.
func (c *tapConn) lines(buf, b []byte, out bool) []byte {
	buf = append(buf, b...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return buf
		}
		c.w.saw(bytes.TrimSpace(buf[:i]), out)
		buf = buf[i+1:]
	}
}

// ------------------------------------------------------------ the comparison

// socketView is what the check holds equal: both projections and the
// last-ended asks (Plan 024's ModelView).
type socketView struct {
	History transcript.History
	State   transcript.State
	Ended   []transcript.AskEnding
}

func socketViewOf(m *transcript.Model) socketView {
	return socketView{History: m.History(), State: m.State(), Ended: m.EndedAsks()}
}

// diffSocketViews is "" when the host's view and the TUI's agree once both
// are canonical, else where they first differ.
func diffSocketViews(host, tui socketView) string {
	host, tui = canonical(host), canonical(tui)
	if !reflect.DeepEqual(host.History.Main, tui.History.Main) {
		return "the main transcript differs: " + transcriptDiff(host.History.Main, tui.History.Main)
	}
	if len(host.History.Subs) != len(tui.History.Subs) {
		return fmt.Sprintf("the TUI holds %d child transcripts, the host %d", len(tui.History.Subs), len(host.History.Subs))
	}
	for i := range host.History.Subs {
		h, g := host.History.Subs[i], tui.History.Subs[i]
		if h.ID != g.ID {
			return fmt.Sprintf("child %d is %q on the TUI, %q on the host", i, g.ID, h.ID)
		}
		if !reflect.DeepEqual(h, g) {
			return fmt.Sprintf("child %q differs: %s", h.ID, transcriptDiff(h.TranscriptHistory, g.TranscriptHistory))
		}
	}
	if !reflect.DeepEqual(host.State, tui.State) {
		return fmt.Sprintf("the states differ:\n host %+v\n  TUI %+v", host.State, tui.State)
	}
	if !reflect.DeepEqual(host.Ended, tui.Ended) {
		return fmt.Sprintf("the last-ended asks differ:\n host %+v\n  TUI %+v", host.Ended, tui.Ended)
	}
	return ""
}

// transcriptDiff names the first entry at which two transcripts part.
func transcriptDiff(host, tui transcript.TranscriptHistory) string {
	for i := range min(len(host.Entries), len(tui.Entries)) {
		if !reflect.DeepEqual(host.Entries[i], tui.Entries[i]) {
			return fmt.Sprintf("entry %d:\n host %+v\n  TUI %+v", i, host.Entries[i], tui.Entries[i])
		}
	}
	if len(host.Entries) != len(tui.Entries) {
		return fmt.Sprintf("the TUI holds %d entries, the host %d", len(tui.Entries), len(host.Entries))
	}
	return fmt.Sprintf("the flags: host trimmed %v windowed %v stream %v todo %d/%v, TUI %v %v %v %d/%v",
		host.Trimmed, host.Windowed, host.StreamOpen, host.TodoPlanned, host.TodoDone,
		tui.Trimmed, tui.Windowed, tui.StreamOpen, tui.TodoPlanned, tui.TodoDone)
}

var (
	canonTimeType  = reflect.TypeFor[time.Time]()
	canonErrorType = reflect.TypeFor[error]()
)

// canonical is a deep copy of v with every time in UTC with no monotonic
// reading and every error the *agent.RemoteError the event codec makes of it:
// what a fold of the publisher's own values — the host's — and a fold of
// decoded ones — the socket TUI's — agree on once the codec's representation
// is set aside (engine.Canon's rule, which internal/engine's tests hold A2
// to). It never writes v's own storage.
func canonical[T any](v T) T {
	return canonValue(reflect.ValueOf(&v).Elem()).Interface().(T)
}

func canonValue(v reflect.Value) reflect.Value {
	t := v.Type()
	if t == canonTimeType {
		return reflect.ValueOf(v.Interface().(time.Time).UTC().Round(0))
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t).Elem()
		if t == canonErrorType {
			out.Set(reflect.ValueOf(agent.RemoteErrorOf(v.Interface().(error))))
			return out
		}
		out.Set(canonValue(v.Elem()))
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t.Elem())
		out.Elem().Set(canonValue(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(canonValue(v.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(canonValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := range v.Len() {
			out.Index(i).Set(canonValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(canonValue(it.Key()), canonValue(it.Value()))
		}
		return out
	}
	return v
}

// ------------------------------------------------------------------ the tests

// socketFrame runs script once over the socket, against a Stub build makes
// NoPrimary, and answers the frame and the run's error.
func socketFrame(t *testing.T, build func(*Stub) Config, script string, opts FrameOpts) (string, error) {
	t.Helper()
	isolateSkillsHome(t)
	cfg := build(buildFor(transportSocket, frameStub))
	opts.transport = transportSocket
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	plain, _, err := RunFrameScript(cfg, 80, 24, script, opts)
	return plain, err
}

// stubFrameConfig is runStubFrame's Config over stub.
func stubFrameConfig(t *testing.T) func(*Stub) Config {
	return func(stub *Stub) Config {
		return Config{Session: stub, Theme: "tokyo-night", Workspace: frameWorkspace(t), Model: "grok", Yolo: true}
	}
}

// droppingBackend is a backend whose stream loses the first event drop picks:
// a fold that diverges from the host's.
type droppingBackend struct {
	backend.Backend
	drop    func(agent.Event) bool
	dropped atomic.Bool
}

func (b *droppingBackend) wrapped() backend.Backend { return b.Backend }

func (b *droppingBackend) Read(ctx context.Context) (backend.Item, error) {
	for {
		it, err := b.Backend.Read(ctx)
		if err == nil && it.Kind == backend.ItemEvent && !b.dropped.Load() && b.drop(it.Event) {
			b.dropped.Store(true)
			continue
		}
		return it, err
	}
}

// TestSocketGoldensMatchTheEngine (plan 027 §3.16, A1's automated half): at
// the end of every socket golden the TUI's fold — m.shared, read while the
// program is parked — is held against the host engine's model at the same
// seq, both in this process (socketHost.match, from captureThenMatch). The
// check runs: a socket run of a turn moves the count of matches. And it
// fails on a divergent fold: the same run over a stream that loses one event
// — the reply's text, so every later event still folds and the settle and
// the waits are met — ends with the mismatch, naming the seq.
func TestSocketGoldensMatchTheEngine(t *testing.T) {
	const script = "<wait:idle>hi<enter><wait:idle>"
	before := socketMatches.Load()
	plain, err := socketFrame(t, stubFrameConfig(t), script, FrameOpts{})
	if err != nil {
		t.Fatalf("the socket run: %v", err)
	}
	if !strings.Contains(plain, "echo: hi") {
		t.Fatalf("fixture: the turn is not on screen:\n%s", plain)
	}
	if got := socketMatches.Load(); got != before+1 {
		t.Fatalf("the socket run made %d checks, want one", got-before)
	}

	var wrapped *droppingBackend
	prev := sessionBackendHook
	sessionBackendHook = func(b backend.Backend) backend.Backend {
		wrapped = &droppingBackend{Backend: b, drop: func(ev agent.Event) bool { return ev.Type == agent.EventText }}
		return wrapped
	}
	t.Cleanup(func() { sessionBackendHook = prev })
	before = socketMatches.Load()
	plain, err = socketFrame(t, stubFrameConfig(t), script, FrameOpts{})
	if wrapped == nil || !wrapped.dropped.Load() {
		t.Fatal("fixture: the stream lost no event")
	}
	if strings.Contains(plain, "echo: hi") {
		t.Fatalf("fixture: the lost reply is on screen:\n%s", plain)
	}
	if err == nil || !strings.Contains(err.Error(), "the TUI's fold is not the host engine's model") {
		t.Fatalf("a fold that lost an event passed the check: %v", err)
	}
	if got := socketMatches.Load(); got != before {
		t.Fatalf("a divergent fold counted %d matches", got-before)
	}
	t.Logf("the divergent run's error: %v", err)
}

// TestASocketRunFailsOnAnyReset (§3.4, §3.16: a golden must never depend on
// the reset path): a host that resets the run's stream — a real reset, the
// omitted one, for an event over the log's record bound, published once the
// script has settled — fails the socket run, whatever its frame shows, and
// so does the re-attach the client answers it with.
//
// How many of each the run sees is the protocol's, not this test's: a reset
// {omitted} is answered by a re-attach without a cursor, and the first
// re-attach's subscription can still meet the omitted record, so one omission
// brings one or two omitted resets and one or two re-attaches — at most two
// per omission (§3.4's table, X22). The run's error names what the tap saw
// (the orchestrator's -race gate at 680d345 saw two of each: "attached 3
// times", "[omitted omitted]").
func TestASocketRunFailsOnAnyReset(t *testing.T) {
	var stub *Stub
	build := func(s *Stub) Config {
		stub = s
		return stubFrameConfig(t)(s)
	}
	var host *socketHost
	prev := socketHostHooks
	t.Cleanup(func() { socketHostHooks = prev })
	socketHostHooks.built = func(h *socketHost) { host = h }
	_, err := socketFrame(t, build, "<wait:idle>hi<enter><wait:text:echo: hi><wait:idle>", FrameOpts{
		beforeQuit: func(func(tea.Msg), func() frameState) {
			stub.Emit(agent.Event{Type: agent.EventText, Text: strings.Repeat("O", 8<<20+1<<10)})
		},
	})
	if host == nil {
		t.Fatal("fixture: no socket host was built")
	}
	assertOneOmission(t, host, err)
	if !strings.Contains(err.Error(), "(a re-attach)") {
		t.Fatalf("the re-attach the reset brought went unremarked: %v", err)
	}
	t.Logf("the run's error: %v", err)
}

// assertOneOmission fails t unless err is the run failing on the reset that
// one omitted record brings, and the tap saw exactly what the protocol allows
// for it (§3.4's table, X22): one or two resets, every one omitted, and one or
// two re-attaches beside the first attach.
func assertOneOmission(t *testing.T, h *socketHost, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "the host reset the stream: [omitted") {
		t.Fatalf("a run whose host reset its stream passed, or failed otherwise: %v", err)
	}
	attaches, resets := h.tap.counts()
	if len(resets) < 1 || len(resets) > 2 {
		t.Fatalf("one omission brought %d resets %v, want one or two", len(resets), resets)
	}
	for _, r := range resets {
		if r != string(protocol.ResetOmitted) {
			t.Fatalf("the host reset the stream for %q, want only omitted: %v", r, resets)
		}
	}
	if re := attaches - 1; re < 1 || re > 2 {
		t.Fatalf("one omission brought %d re-attaches, want one or two (at most two per omission)", re)
	}
}

// TestASocketRunsHostIsReachedThroughItsSession (§3.16, "Test access to the
// session"): the registry names a socket run's host engine by the session the
// model holds — through V8's jitter wrapper around it too — so engineOf and
// stubOf reach the host's engine and its Stub exactly as they reach an
// in-process model's; the host's engine is the one setSession would have
// built (engineOptions: here, the durable craze id the Config names); and
// once the host has ended nothing names it.
func TestASocketRunsHostIsReachedThroughItsSession(t *testing.T) {
	isolateSkillsHome(t)
	stub := buildFor(transportSocket, frameStub)
	host, err := buildSocketHost(Config{Session: stub, CrazeSessionID: "018f-the-row", Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: true})
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	t.Cleanup(func() {
		if !ended {
			_ = host.end()
		}
	})
	if host.cfg.Backend == nil || host.cfg.Session != nil || host.cfg.Viewer {
		t.Fatalf("the socket run's Config: backend %T, session %T, viewer %v", host.cfg.Backend, host.cfg.Session, host.cfg.Viewer)
	}
	eng := hostEngineOf(host.cfg.Backend)
	if eng == nil || eng.Session() != agent.Session(stub) {
		t.Fatalf("the registry names %v for the socket run's session", eng)
	}
	m := New(host.cfg)
	if engineOf(t, m) != eng || stubOf(t, m) != stub {
		t.Fatal("the socket run's model does not reach its host's engine and Stub")
	}
	if got := eng.State().CrazeSessionID; got != "018f-the-row" {
		t.Fatalf("the host's engine has craze id %q: it is not built as setSession builds one", got)
	}
	jittered(t, 1, 0)
	j := New(host.cfg)
	if _, ok := j.eng.(*jitterBackend); !ok || engineOf(t, j) != eng {
		t.Fatalf("through the jitter wrapper (%T) the model does not reach its host's engine", j.eng)
	}
	ended = true
	if err := host.end(); err != nil {
		t.Fatalf("the host's end: %v", err)
	}
	if _, ok := engineIn(m); ok {
		t.Fatal("an ended host is still named by its session")
	}
}

// TestNoResetEscapesTheSocketRunsVerdict (astra r69 5): a reset the host writes
// to a socket run's connection fails the run however late it comes and however
// slow the client's reader is. The host publishes an event over the log's
// record bound — a subscriber is handed it Omitted, and the host writes
// reset{omitted} — with the client's reader held past remote's view-close
// bound (3 s), so the view close gives up on its detach before the reader
// could read the reset:
//
//   - once the engine check has passed, before the final barrier — the
//     reviewer's schedule: the barrier's reply follows the reset, so the reset
//     is read, and the run fails on it;
//   - once the barrier has returned, just before the view close: the detach's
//     reply follows the reset, and a detach the view close gave up on fails
//     the run — the reset behind it is never read, and still escapes nothing.
func TestNoResetEscapesTheSocketRunsVerdict(t *testing.T) {
	const hold = 3500 * time.Millisecond // past remote's closeBound
	for _, tc := range []struct {
		name string
		at   func(hooks *socketHostHookSet, f func(*socketHost))
		// omitted says the run must have read the reset (the barrier's arm);
		// want is what the run's error must say otherwise.
		omitted bool
		want    string
	}{
		{"after the check", func(k *socketHostHookSet, f func(*socketHost)) { k.afterMatch = f }, true, ""},
		{"after the barrier", func(k *socketHostHookSet, f func(*socketHost)) { k.afterBarrier = f }, false, "the view close's detach went unanswered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stub *Stub
			build := func(s *Stub) Config {
				stub = s
				return stubFrameConfig(t)(s)
			}
			prev := socketHostHooks
			t.Cleanup(func() { socketHostHooks = prev })
			published := false
			var host *socketHost
			tc.at(&socketHostHooks, func(h *socketHost) {
				host = h
				h.tap.holdReads(hold)
				stub.Emit(agent.Event{Type: agent.EventText, Text: strings.Repeat("O", 8<<20+1<<10)})
				// Written by the time the log has committed it: the forwarder
				// meets the record Omitted and queues the reset.
				if _, err := h.eng.SyncSeq(context.Background()); err != nil {
					t.Errorf("the host's sync: %v", err)
				}
				published = true
			})
			_, err := socketFrame(t, build, "<wait:idle>hi<enter><wait:text:echo: hi><wait:idle>", FrameOpts{})
			if !published {
				t.Fatal("fixture: the host never published the oversized event")
			}
			switch {
			case tc.omitted:
				// One omission: one or two resets and re-attaches (X22).
				assertOneOmission(t, host, err)
			case err != nil && strings.Contains(err.Error(), "the host reset the stream: [omitted"):
				// Under load the reset can reach the tap before the barrier
				// even in the "after the barrier" arm — and once the reader
				// was held past the close bound, a re-attach the reset
				// brings can itself lose the race to the view close's own
				// shutdown and never happen. Either way the run still fails
				// because of the reset, the property this test proves
				// either way, so bound the resets as assertOneOmission does
				// (one or two, every one omitted) without its re-attach
				// floor: X22's re-attach count is a ceiling here, not a
				// guarantee.
				_, resets := host.tap.counts()
				if len(resets) < 1 || len(resets) > 2 {
					t.Fatalf("one omission brought %d resets %v, want one or two", len(resets), resets)
				}
				for _, r := range resets {
					if r != string(protocol.ResetOmitted) {
						t.Fatalf("the host reset the stream for %q, want only omitted: %v", r, resets)
					}
				}
			case err != nil && strings.Contains(err.Error(), tc.want):
				// The documented schedule: the detach's reply follows the
				// reset, and the view close gave up on the detach before
				// ever reading anything behind it.
			default:
				t.Fatalf("a run whose host wrote a reset passed, or failed otherwise: %v", err)
			}
			t.Logf("the run's error: %v", err)
		})
	}
}
