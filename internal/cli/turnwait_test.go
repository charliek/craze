package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
)

// The waiter (turnwait.go; plan 041 §3.1, §7 W1), against a real control
// server in front of an engine over a scripted session (stubSession,
// queue_test.go): every edge — events ahead of a submit's reply, a queued
// row refused and put back, a reconnect, a new incarnation, an ask, a
// signal — is placed by the test, never by a sleep. Each wait is bounded by
// stubWatchdog, and every turn a test holds is let go before the rig closes.

// waitRigID is the rig's craze session id; waitRigShort is its short id, as
// craze ps prints it.
const (
	waitRigID    = "0199c0de-1111-7000-8000-00000000a001"
	waitRigShort = "0000a001"
)

// waitRig is one session's host as craze wait meets it: an engine over a
// scripted session, the real control server in front of it, and its socket
// listed in a registry under a test HOME (serveHome).
type waitRig struct {
	t   *testing.T
	env rundir.Env
	s   *stubSession
	eng *engine.Engine
	srv *control.Server
	// reg is the host's registry entry, which a test may take away.
	reg *rundir.Host

	mu       sync.Mutex
	attached int
}

// newWaitRig is a rig whose session's event log keeps ring events (0: the
// default) and runs turns in order, the last repeating.
func newWaitRig(t *testing.T, ring int, turns ...stubTurn) *waitRig {
	t.Helper()
	env, _ := serveHome(t)
	r := &waitRig{t: t, env: env}
	r.s, r.eng = r.newEngine(ring, turns...)
	r.srv = control.New(control.Options{Workspace: "/w"})
	t.Cleanup(r.srv.AddAttachmentsListener(func(n int) {
		r.mu.Lock()
		r.attached = n
		r.mu.Unlock()
	}))
	r.srv.SetEngine(r.eng)
	reg, err := rundir.Bind(env, "0000000000a1", rundir.Entry{StartedAt: time.Now(), CrazeSessionID: waitRigID,
		Incarnation: r.eng.State().Incarnation, Provider: "grok", Workspace: "/w", Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	r.reg = reg
	served := make(chan error, 1)
	go func() { served <- r.srv.Serve(reg.Listener()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), serveStep)
		defer cancel()
		_ = r.srv.Close(ctx)
		<-served
		_ = reg.Close()
	})
	return r
}

// newEngine is a started engine over a new scripted session, with the rig's
// craze session id: what SetEngine swaps in for a new incarnation.
func (r *waitRig) newEngine(ring int, turns ...stubTurn) (*stubSession, *engine.Engine) {
	r.t.Helper()
	s := newStubSessionOn(r.t, agent.EventLogOptions{RingEvents: ring, NoPrimary: true}, turns...)
	eng, err := engine.New(s, engine.Options{CrazeSessionID: waitRigID})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = eng.Close() })
	s.hold(eng)
	if err := eng.Start(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	return s, eng
}

// gate is a channel a held turn waits on, opened once: by the test, or —
// registered after the rig, so first — by the test's end, so no engine's
// close waits on a turn that is never let go.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func (r *waitRig) gate() *gate {
	g := &gate{ch: make(chan struct{})}
	r.t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// script replaces the session's turns.
func (r *waitRig) script(turns ...stubTurn) {
	r.s.mu.Lock()
	r.s.turns = turns
	r.s.mu.Unlock()
}

// heldTurn is a turn that waits for g, then emits emit and ends end_turn.
func heldTurn(g *gate, emit ...agent.Event) stubTurn {
	turn := answerTurn(emit...)
	turn.before = func() { <-g.ch }
	return turn
}

// answerTurn is a turn that emits emit and ends end_turn at once (endTurn).
func answerTurn(emit ...agent.Event) stubTurn {
	turn := endTurn()
	turn.emit = append(emit, turn.emit...)
	return turn
}

func textEv(s string) agent.Event { return agent.Event{Type: agent.EventText, Text: s} }

func toolEv(id, kind, status string, paths ...string) agent.Event {
	return agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: id, Kind: kind, Status: status, Locations: paths}}
}

func usageEv(turnPico int64, unpriced bool) agent.Event {
	return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{
		ContextTokens: 1200, ContextWindow: 10000, Turn: agent.Spend{CostPicoUSD: turnPico, Unpriced: unpriced},
		Session: agent.Spend{CostPicoUSD: 2 * turnPico}}}}
}

// submit is a prompt another client sends, straight to the engine.
func (r *waitRig) submit(text string) engine.SubmitResult {
	r.t.Helper()
	res, err := r.eng.Submit(engine.Command{}, text, engine.SubmitQueue, "")
	if err != nil {
		r.t.Fatalf("submit %q: %v", text, err)
	}
	return res
}

// entry is the rig's registry entry.
func (r *waitRig) entry() rundir.Entry {
	r.t.Helper()
	entries, err := rundir.Hosts(r.env)
	if err != nil || len(entries) != 1 {
		r.t.Fatalf("the registry: %+v, %v", entries, err)
	}
	return entries[0]
}

// dial is a connection attached to the rig's session as craze wait makes one
// (dialWait), from cursor; closed when the test ends.
func (r *waitRig) dial(cursor *protocol.Cursor) *waitConn {
	r.t.Helper()
	conn, err := dialWait(stepCtx(r.t), r.entry(), waitRigID, waitWhat, cursor)
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	r.t.Cleanup(func() { conn.close(nil) })
	return conn
}

// dialTapped is dial over a transport the test watches (tap): what the
// client writes, and connections it can drop.
func (r *waitRig) dialTapped() (*waitConn, *tap) {
	r.t.Helper()
	return r.dialTappedFrom(nil, &tap{})
}

// dialTappedFrom is dialTapped from cursor, over tp — whose hooks a test
// has set before the attach.
func (r *waitRig) dialTappedFrom(cursor *protocol.Cursor, tp *tap) (*waitConn, *tap) {
	r.t.Helper()
	e := r.entry()
	c, err := remote.Dial(stepCtx(r.t), e.Socket, remote.Options{Dial: tp.dial})
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	st, err := c.Attach(stepCtx(r.t), remote.AttachOptions{SessionID: waitRigID, Cursor: cursor, When: protocol.WhenReady})
	if err != nil {
		_ = c.Close()
		r.t.Fatalf("attach: %v", err)
	}
	conn := &waitConn{c: c, st: st, entry: e, sessionID: waitRigID}
	r.t.Cleanup(func() { conn.close(nil) })
	return conn, tp
}

// waitAttached waits for n clients attached to the session.
func (r *waitRig) waitAttached(n int) {
	r.t.Helper()
	waitFor(r.t, "an attached client", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.attached >= n
	})
}

// waitStarted waits for the engine to run turn.
func (r *waitRig) waitStarted(turn string) {
	r.t.Helper()
	waitFor(r.t, "the turn to start", func() bool { return r.eng.State().Turn == turn })
}

// waitQueued waits for the engine's queue to hold n rows.
func (r *waitRig) waitQueued(n int) {
	r.t.Helper()
	waitFor(r.t, "the queued row", func() bool { return len(r.eng.State().Queue) == n })
}

// waitIdle waits for the engine to have settled a turn and run nothing.
func (r *waitRig) waitIdle() {
	r.t.Helper()
	waitFor(r.t, "the turn's end", func() bool {
		st := r.eng.State()
		return st.LastTurn != nil && st.Turn == "" && len(st.Queue) == 0 && !r.eng.Busy()
	})
}

// tap is a client transport a test watches: every byte the client writes,
// and every connection it dialled, which drop closes under it. A test can
// also act at the moment the client writes a request (on), swallow every
// request of a kind (swallow), and hold the client's redials (holdRedials).
type tap struct {
	mu    sync.Mutex
	out   strings.Builder
	in    strings.Builder
	conns []net.Conn
	// hooks run once each, on the first write that holds their match,
	// before it goes out; fn answers whether it goes out at all.
	hooks []*tapHook
	// swallowed is every match whose writes never go out.
	swallowed []string
	// hold, when set, keeps every dial after the first waiting for it.
	hold *gate
	// lose is a match a read from the host must not deliver: the read that
	// holds it closes its connection instead, and lost is closed (loseReply).
	lose string
	lost chan struct{}
}

type tapHook struct {
	match string
	fn    func() bool
	ran   bool
}

type tapConn struct {
	net.Conn
	tp *tap
}

func (c tapConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.tp.mu.Lock()
	if lose := c.tp.lose; lose != "" && strings.Contains(string(b[:n]), lose) {
		// Never delivered: the connection goes with it.
		c.tp.lose = ""
		close(c.tp.lost)
		c.tp.mu.Unlock()
		_ = c.Close()
		return 0, net.ErrClosed
	}
	c.tp.in.Write(b[:n])
	c.tp.mu.Unlock()
	return n, err
}

// loseReply makes the first read from the host that holds match never reach
// the client — its connection is closed under it instead — and answers what
// is closed once that has happened: a reply lost with its connection.
func (tp *tap) loseReply(match string) <-chan struct{} {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.lose, tp.lost = match, make(chan struct{})
	return tp.lost
}

// read says the client has read match from the host: a reply, or a
// notification, on its way to the client's stream.
func (tp *tap) read(match string) bool {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return strings.Contains(tp.in.String(), match)
}

func (c tapConn) Write(b []byte) (int, error) {
	tp := c.tp
	tp.mu.Lock()
	tp.out.Write(b)
	var hook *tapHook
	for _, h := range tp.hooks {
		if !h.ran && strings.Contains(string(b), h.match) {
			h.ran, hook = true, h
			break
		}
	}
	swallow := slices.ContainsFunc(tp.swallowed, func(m string) bool { return strings.Contains(string(b), m) })
	tp.mu.Unlock()
	if hook != nil && !hook.fn() || swallow {
		return len(b), nil
	}
	return c.Conn.Write(b)
}

// on runs fn once, as the client writes its first request holding match.
func (tp *tap) on(match string, fn func() (forward bool)) {
	tp.mu.Lock()
	tp.hooks = append(tp.hooks, &tapHook{match: match, fn: fn})
	tp.mu.Unlock()
}

// swallow keeps every request holding match from the host: it never answers.
func (tp *tap) swallow(match string) {
	tp.mu.Lock()
	tp.swallowed = append(tp.swallowed, match)
	tp.mu.Unlock()
}

// holdRedials keeps every later dial waiting for g.
func (tp *tap) holdRedials(g *gate) {
	tp.mu.Lock()
	tp.hold = g
	tp.mu.Unlock()
}

func (tp *tap) dial(ctx context.Context, path string) (net.Conn, error) {
	tp.mu.Lock()
	hold := tp.hold
	if len(tp.conns) == 0 {
		hold = nil
	}
	tp.mu.Unlock()
	if hold != nil {
		select {
		case <-hold.ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	tp.mu.Lock()
	tp.conns = append(tp.conns, c)
	tp.mu.Unlock()
	return tapConn{Conn: c, tp: tp}, nil
}

// lines is every request line the client wrote whose method is method.
func (tp *tap) lines(method string) []string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []string
	for _, l := range strings.Split(tp.out.String(), "\n") {
		if strings.Contains(l, `"method":"`+method+`"`) {
			out = append(out, l)
		}
	}
	return out
}

// answered says the n-th request (from 1) the client wrote whose method is
// method has had its reply read: a response is the only line the host writes
// that starts with the jsonrpc member and then an id.
func (tp *tap) answered(method string, n int) bool {
	ls := tp.lines(method)
	if len(ls) < n {
		return false
	}
	var req protocol.Request
	if err := json.Unmarshal([]byte(ls[n-1]), &req); err != nil || len(req.ID) == 0 {
		return false
	}
	return tp.read(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,`)
}

// drop closes the client's latest connection under it, as a broken socket.
func (tp *tap) drop() {
	tp.mu.Lock()
	c := tp.conns[len(tp.conns)-1]
	tp.mu.Unlock()
	_ = c.Close()
}

// waitRun is one waitTurn on a goroutine of its own.
type waitRun struct {
	done chan struct{}
	res  *turnResult
	fail *waitFailure
}

func goWait(conn *waitConn, start waitStart, opts waitOptions) *waitRun {
	w := &waitRun{done: make(chan struct{})}
	go func() {
		w.res, w.fail = waitTurn(context.Background(), conn, start, opts)
		close(w.done)
	}()
	return w
}

// result is the wait's answer, within stubWatchdog.
func (w *waitRun) result(t *testing.T) (*turnResult, *waitFailure) {
	t.Helper()
	select {
	case <-w.done:
		return w.res, w.fail
	case <-time.After(stubWatchdog):
		t.Fatalf("the wait did not end within %v", stubWatchdog)
		return nil, nil
	}
}

// wantResult is a wait that ended with a result of status.
func wantResult(t *testing.T, res *turnResult, f *waitFailure, status string) *turnResult {
	t.Helper()
	if f != nil {
		t.Fatalf("the wait failed: %q (handle %q)", f.msg, f.handle)
	}
	if res == nil || res.Status != status {
		t.Fatalf("the result is %+v, want status %s", res, status)
	}
	return res
}

// TestWaitTurnMatchesEventsBeforeTheSubmitReply is §7 W1 1: a turn's started
// AND its ended reaching the stream before the submit's reply are buffered
// and matched to the target the reply names. The reply is held until the
// waiter has buffered the ending.
func TestWaitTurnMatchesEventsBeforeTheSubmitReply(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(toolEv("t1", "edit", "completed", "a.go"), textEv("the answer")))
	conn := r.dial(nil)
	buffered := make(chan struct{})
	var once sync.Once
	prev := waitBuffered
	waitBuffered = func(p pumped) {
		if p.it.Kind == remote.KindEvent && strings.Contains(string(p.it.Body), `"phase":"ended"`) {
			once.Do(func() { close(buffered) })
		}
	}
	t.Cleanup(func() { waitBuffered = prev })
	submit := func(ctx context.Context, ident uint64) (waitTarget, error) {
		target, err := submitPrompt(conn, "go")(ctx, ident)
		select {
		case <-buffered:
		case <-time.After(stubWatchdog):
		}
		return target, err
	}
	res, f := goWait(conn, waitStart{submit: submit}, waitOptions{}).result(t)
	wantResult(t, res, f, statusCompleted)
	if res.Turn != "turn-1" || res.message() != "the answer" || *res.ToolCalls != 1 || !slices.Equal(*res.Files, []string{"a.go"}) ||
		res.Partial || res.StopReason != "end_turn" {
		t.Fatalf("the result: %+v", res)
	}
}

// TestWaitTurnFollowsARestoredRow is §7 W1 2 (P3): the target is a queued
// row; its turn is refused by the agent's own turn and the row put back
// (`sent → started → queued → synthetic ended`), then runs again (`sent →
// started → ended(end_turn)`): the result is that later run's, completed —
// never the synthetic ending.
func TestWaitTurnFollowsARestoredRow(t *testing.T) {
	r := newWaitRig(t, 0)
	first := r.gate()
	r.script(heldTurn(first, textEv("first")), stubTurn{err: agent.ErrForeignTurn}, answerTurn(textEv("mine, at last")))
	r.submit("first")
	conn := r.dial(nil)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{})
	r.waitQueued(1)
	first.open()
	res, f := w.result(t)
	wantResult(t, res, f, statusCompleted)
	if res.Row != "q-1" || res.Turn != "turn-3" || res.message() != "mine, at last" || res.Partial {
		t.Fatalf("the result: %+v", res)
	}
	if got := r.s.sent(); !slices.Equal(got, []string{"first", "mine", "mine"}) {
		t.Fatalf("the prompts the session was handed: %q, want the row refused once and run again", got)
	}
}

// TestWaitTurnMessageIsTheTurnsOwn is §7 W1 3, for a turn followed live: a
// turn that writes no text reports message "" — never the turn before's —
// both for a turn of craze's own and for one the agent runs itself.
func TestWaitTurnMessageIsTheTurnsOwn(t *testing.T) {
	t.Run("craze's own", func(t *testing.T) {
		r := newWaitRig(t, 0, answerTurn(textEv("an earlier reply")), answerTurn(toolEv("t1", "execute", "completed")))
		r.submit("one")
		r.waitIdle()
		conn := r.dial(nil)
		res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "two")}, waitOptions{}).result(t)
		wantResult(t, res, f, statusCompleted)
		if res.Turn != "turn-2" || res.Message == nil || *res.Message != "" || *res.ToolCalls != 1 {
			t.Fatalf("the result: %+v (message %q)", res, res.message())
		}
	})
	t.Run("the agent's own", func(t *testing.T) {
		r := newWaitRig(t, 0, answerTurn(textEv("an earlier reply")))
		r.submit("one")
		r.waitIdle()
		r.s.setForeign(true)
		waitFor(t, "the foreign turn", func() bool { return r.s.ForeignTurn() })
		conn := r.dial(nil)
		w := goWait(conn, waitStart{}, waitOptions{})
		r.waitAttached(1)
		r.s.emit(toolEv("f1", "read", "completed"))
		r.s.setForeign(false)
		res, f := w.result(t)
		wantResult(t, res, f, statusCompleted)
		if res.Turn != "interject-fallback-1" || res.Message == nil || *res.Message != "" || !res.Partial {
			t.Fatalf("the result: %+v (message %q)", res, res.message())
		}
	})
}

// TestWaitTurnTimeoutHandleAndReplay is §7 W1 4: --timeout on a longer turn
// is running, exit 5, with a handle; `wait --from` that handle after the turn
// ended replays it from the handle's cursor — that exact turn's full result,
// not partial; and a replay the host refuses (its ring evicted the cursor)
// is settled from lastTurn, partial.
func TestWaitTurnTimeoutHandleAndReplay(t *testing.T) {
	for _, c := range []struct {
		name    string
		ring    int
		partial bool
	}{
		{"replayed", 0, false},
		{"the replay refused", 4, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newWaitRig(t, c.ring)
			g := r.gate()
			chunks := []agent.Event{toolEv("t1", "edit", "completed", "x.go")}
			for _, s := range []string{"the ", "whole ", "answer ", "in ", "chunks"} {
				chunks = append(chunks, textEv(s))
			}
			r.script(heldTurn(g, chunks...))
			conn := r.dial(nil)
			fire := make(chan time.Time)
			w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{timeout: fire})
			r.waitStarted("turn-1")
			// The waiter has the turn once its reply is in: the timeout fires on
			// a target, never before one.
			waitFor(t, "the waiter's target", func() bool { return len(r.s.sent()) == 1 })
			close(fire)
			res, f := w.result(t)
			res = wantResult(t, res, f, statusRunning)
			if res.Turn != "turn-1" || res.Handle == "" || res.Message != nil || statusExit(res.Status) != 5 {
				t.Fatalf("the timed-out result: %+v", res)
			}
			h, err := parseHandle(res.Handle)
			if err != nil || h.target != (waitTarget{kind: targetTurn, id: "turn-1"}) || h.incarnation != r.eng.State().Incarnation {
				t.Fatalf("the handle %q: %+v, %v", res.Handle, h, err)
			}
			g.open()
			r.waitIdle()
			from := r.dial(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq})
			res, f = goWait(from, waitStart{from: &h}, waitOptions{}).result(t)
			wantResult(t, res, f, statusCompleted)
			if res.Turn != "turn-1" || res.message() != "the whole answer in chunks" || *res.ToolCalls != 1 ||
				res.Partial != c.partial {
				t.Fatalf("the replayed result: %+v (message %q)", res, res.message())
			}
		})
	}
}

// TestWaitTurnReattachesAndRefusesANewIncarnation is §7 W1 5: a connection
// dropped mid-turn is redialled and re-attached from the stream's cursor —
// the turn's events after the drop are folded, and the result is whole, not
// partial; a new incarnation of the session (its engine replaced: turn ids
// start again) is exit 1, the ending not observed, with the handle.
func TestWaitTurnReattachesAndRefusesANewIncarnation(t *testing.T) {
	t.Run("a dropped connection", func(t *testing.T) {
		r := newWaitRig(t, 0)
		g := r.gate()
		r.script(heldTurn(g, textEv("after the drop")))
		conn, tp := r.dialTapped()
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{})
		r.waitStarted("turn-1")
		tp.drop()
		waitFor(t, "a redial", func() bool {
			tp.mu.Lock()
			defer tp.mu.Unlock()
			return len(tp.conns) == 2
		})
		g.open()
		res, f := w.result(t)
		wantResult(t, res, f, statusCompleted)
		if res.message() != "after the drop" || res.Partial {
			t.Fatalf("the result: %+v", res)
		}
		attaches := tp.lines(protocol.MethodSessionAttach)
		if len(attaches) != 2 || !strings.Contains(attaches[1], `"cursor":{`) {
			t.Fatalf("the attaches: %q, want the second from the cursor", attaches)
		}
	})
	t.Run("a new incarnation", func(t *testing.T) {
		r := newWaitRig(t, 0)
		g := r.gate()
		r.script(heldTurn(g))
		conn := r.dial(nil)
		replied := make(chan struct{})
		submit := func(ctx context.Context, ident uint64) (waitTarget, error) {
			defer close(replied)
			return submitPrompt(conn, "go")(ctx, ident)
		}
		w := goWait(conn, waitStart{submit: submit}, waitOptions{})
		// The reply in: replaced before it, the prompt's outcome is unknown
		// (resume_lost), which is the submit's failure, not this test's.
		<-replied
		old := r.eng.State().Incarnation
		// The new incarnation runs a turn-1 of its own: the same id, another
		// turn (P7).
		// Its gate opens before its engine closes: registered after it.
		again := &gate{ch: make(chan struct{})}
		_, eng := r.newEngine(0, heldTurn(again, textEv("another turn's")))
		t.Cleanup(again.open)
		if res, err := eng.Submit(engine.Command{}, "another prompt", engine.SubmitQueue, ""); err != nil || res.Turn != "turn-1" {
			t.Fatalf("the new incarnation's turn: %+v, %v", res, err)
		}
		r.srv.SetEngine(eng)
		res, f := w.result(t)
		if res != nil || f == nil || f.msg != "the turn's ending was not observed" {
			t.Fatalf("the wait across a new incarnation: %+v, %+v", res, f)
		}
		h, err := parseHandle(f.handle)
		if err != nil || h.incarnation != old || h.target.id != "turn-1" {
			t.Fatalf("the failure's handle %q: %+v %v", f.handle, h, err)
		}
	})
}

// TestWaitTurnAutoAnswersAreNotNeedsInput is §7 W1 6's automatic half (P9):
// a question and a plan craze's policy answered never make needs_input; they
// are listed in autoAnswered, and the plan's text is the result's plan.
func TestWaitTurnAutoAnswersAreNotNeedsInput(t *testing.T) {
	r := newWaitRig(t, 0)
	r.script(stubTurn{
		before: func() {
			q := &agent.QuestionEvent{Title: "Question", Questions: []agent.Question{{ID: "q1", Prompt: "Pick one",
				Options: []agent.Option{{ID: "opt-a", Label: "A"}, {ID: "opt-b", Label: "B"}}}}}
			r.s.asks.Automatic(agent.TurnToken{}, agent.AskRequest{Kind: agent.AskQuestion, Body: agent.AskBody{Question: q}},
				agent.AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}})
			p := &agent.PlanEvent{Name: "Refactor", Plan: "1. split the file"}
			r.s.asks.Automatic(agent.TurnToken{}, agent.AskRequest{Kind: agent.AskPlan, Body: agent.AskBody{Plan: p}},
				agent.AskAnswer{Accept: true})
			_ = r.s.log.Flush(context.Background(), r.s.closed)
		},
		emit: []agent.Event{textEv("done as planned"), {Type: agent.EventDone, StopReason: "end_turn"}},
		res:  agent.Result{StopReason: "end_turn"},
	})
	conn := r.dial(nil)
	res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
	wantResult(t, res, f, statusCompleted)
	want := []autoAnswer{{Kind: "question", Summary: "Pick one", Choice: "A"}, {Kind: "plan", Summary: "Refactor", Choice: "accepted"}}
	if !slices.Equal(res.AutoAnswered, want) || res.Plan != "1. split the file" || res.message() != "done as planned" || res.Ask != nil {
		t.Fatalf("the result: %+v", res)
	}
}

// TestWaitTurnNeedsInputBlockedBy is §7 W1 6's open-ask half over the rig: an
// open permission is needs_input with the ask — for the target's own turn,
// and for a queued target, the turn that blocks it (blockedBy). The real
// host's permission is TestWaitNeedsInputOnAnOpenPermission (wait_test.go).
func TestWaitTurnNeedsInputBlockedBy(t *testing.T) {
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(stubTurn{before: func() {
		r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell: rm -rf build",
			Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
		<-g.ch
	}, emit: []agent.Event{{Type: agent.EventDone, StopReason: "end_turn"}}, res: agent.Result{StopReason: "end_turn"}})
	conn := r.dial(nil)
	res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
	res = wantResult(t, res, f, statusNeedsInput)
	if res.Ask == nil || *res.Ask != (resultAsk{ID: "perm-1", Kind: "permission", Summary: "Shell: rm -rf build"}) ||
		res.Turn != "turn-1" || res.Handle == "" || res.BlockedBy != "" {
		t.Fatalf("the needs_input result: %+v, ask %+v", res, res.Ask)
	}
	queued := r.dial(nil)
	res, f = goWait(queued, waitStart{submit: submitPrompt(queued, "behind it")}, waitOptions{}).result(t)
	res = wantResult(t, res, f, statusNeedsInput)
	if res.Ask == nil || res.Ask.ID != "perm-1" || res.Row != "q-1" || res.BlockedBy != "turn-1" || res.Turn != "" {
		t.Fatalf("the queued target's result: %+v", res)
	}
}

// TestWaitTurnUsageIsTheTurnsOwn is §7 W1 7 (P6): the usage is the last
// section folded before the turn's ending, even when its successor starts
// in the same settlement and reports its own at once; the footer shows the
// turn's cost — `unpriced` when nothing in it had a price.
func TestWaitTurnUsageIsTheTurnsOwn(t *testing.T) {
	for _, c := range []struct {
		name     string
		unpriced bool
		cost     string
	}{
		{"priced", false, "$0.04"},
		{"unpriced", true, "unpriced"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newWaitRig(t, 0)
			g := r.gate()
			pico := int64(40_000_000_000)
			if c.unpriced {
				pico = 0
			}
			r.script(heldTurn(g, textEv("one"), usageEv(pico, c.unpriced)),
				answerTurn(usageEv(9_990_000_000_000, false), textEv("two")))
			conn := r.dial(nil)
			w := goWait(conn, waitStart{submit: submitPrompt(conn, "one")}, waitOptions{})
			r.waitStarted("turn-1")
			r.submit("two")
			r.waitQueued(1)
			g.open()
			res, f := w.result(t)
			wantResult(t, res, f, statusCompleted)
			if res.usage == nil || res.usage.Turn.CostPicoUSD != pico || res.usage.Turn.Unpriced != c.unpriced {
				t.Fatalf("the usage: %+v", res.usage)
			}
			footer := res.footer()
			if !strings.Contains(footer, " · "+c.cost+" · context 1.2k/10k") {
				t.Fatalf("the footer:\n%s", footer)
			}
			r.waitIdle()
		})
	}
}

// signalRig is a rig whose first turn runs until it is cancelled — the
// session's cancel lets it end, stopped cancelled.
func signalRig(t *testing.T) *waitRig {
	t.Helper()
	r := newWaitRig(t, 0)
	cancelled := r.gate()
	r.s.mu.Lock()
	r.s.onCancel = cancelled.open
	r.s.mu.Unlock()
	r.script(stubTurn{before: func() { <-cancelled.ch },
		emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}}, res: agent.Result{StopReason: "cancelled"}})
	return r
}

// TestWaitTurnSignals is §7 W1 8 (P8): an owned wait's signal removes its
// queued row (dropped) or cancels its own running turn by its id (cancelled);
// a signal before the submit's reply waits for the reply and acts on it;
// plain wait never touches the session; a second signal ends the cleanup at
// once, even while a cancel is out.
func TestWaitTurnSignals(t *testing.T) {
	t.Run("a queued row is removed", func(t *testing.T) {
		r := newWaitRig(t, 0)
		g := r.gate()
		r.script(heldTurn(g, textEv("first")), answerTurn(textEv("never")))
		r.submit("first")
		sigs := make(chan os.Signal, 2)
		conn, tp := r.dialTapped()
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
		r.waitQueued(1)
		waitFor(t, "the waiter's target", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		sigs <- syscall.SIGTERM
		res, f := w.result(t)
		res = wantResult(t, res, f, statusDropped)
		if res.Row != "q-1" || statusExit(res.Status) != 3 || res.Message == nil || *res.Message != "" {
			t.Fatalf("the dropped result: %+v", res)
		}
		if len(tp.lines(protocol.MethodSessionCancel)) != 0 || len(r.eng.State().Queue) != 0 {
			t.Fatalf("the cleanup sent %q; the queue holds %+v", tp.lines(protocol.MethodSessionCancel), r.eng.State().Queue)
		}
		g.open()
		r.waitIdle()
		if got := r.s.sent(); !slices.Equal(got, []string{"first"}) {
			t.Fatalf("the session ran %q", got)
		}
	})
	t.Run("its own running turn is cancelled by id", func(t *testing.T) {
		r := signalRig(t)
		conn, tp := r.dialTapped()
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{sigs: sigs})
		r.waitStarted("turn-1")
		waitFor(t, "the waiter's target", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		sigs <- syscall.SIGINT
		res, f := w.result(t)
		wantResult(t, res, f, statusCancelled)
		cancels := tp.lines(protocol.MethodSessionCancel)
		if len(cancels) != 1 || !strings.Contains(cancels[0], `"turnId":"turn-1"`) {
			t.Fatalf("the cancels sent: %q, want one naming turn-1", cancels)
		}
	})
	t.Run("a signal before the reply waits for it", func(t *testing.T) {
		r := signalRig(t)
		conn, tp := r.dialTapped()
		sigs := make(chan os.Signal, 2)
		reply := r.gate()
		submit := func(ctx context.Context, ident uint64) (waitTarget, error) {
			target, err := submitPrompt(conn, "go")(ctx, ident)
			<-reply.ch
			return target, err
		}
		w := goWait(conn, waitStart{submit: submit}, waitOptions{sigs: sigs})
		r.waitStarted("turn-1")
		sigs <- syscall.SIGHUP
		waitFor(t, "the signal taken", func() bool { return len(sigs) == 0 })
		reply.open()
		res, f := w.result(t)
		wantResult(t, res, f, statusCancelled)
		if cancels := tp.lines(protocol.MethodSessionCancel); len(cancels) != 1 || !strings.Contains(cancels[0], `"turnId":"turn-1"`) {
			t.Fatalf("the cancels sent: %q", cancels)
		}
	})
	t.Run("plain wait never cancels", func(t *testing.T) {
		r := signalRig(t)
		r.submit("someone else's")
		r.waitStarted("turn-1")
		// Attached mid-turn: the snapshot shows turn-1 running.
		conn, tp := r.dialTapped()
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{}, waitOptions{sigs: sigs})
		// Caught up, and following turn-1: it has looked for an open ask
		// (session.snapshot).
		waitFor(t, "the waiter to follow the turn", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
		sigs <- syscall.SIGTERM
		res, f := w.result(t)
		if res != nil || f == nil || f.msg != "interrupted; the turn goes on" || !f.follow || f.handle == "" {
			t.Fatalf("plain wait's signal: %+v, %+v", res, f)
		}
		if len(tp.lines(protocol.MethodSessionCancel)) != 0 || len(tp.lines(protocol.MethodQueueRemove)) != 0 ||
			r.eng.State().Turn != "turn-1" || r.s.cancelsReturned() != 0 {
			t.Fatalf("plain wait touched the session: %q, state %+v", tp.lines(protocol.MethodSessionCancel), r.eng.State())
		}
	})
	t.Run("a second signal ends it at once", func(t *testing.T) {
		r := signalRig(t)
		conn, tp := r.dialTapped()
		// The agent never answers the cancel, and the cleanup's own bound is
		// longer than the test: only the second signal can end it.
		hold := r.gate()
		r.s.mu.Lock()
		r.s.cancelHold = hold.ch
		r.s.mu.Unlock()
		prev := cleanupWait
		cleanupWait = time.Hour
		t.Cleanup(func() { cleanupWait = prev })
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{sigs: sigs})
		r.waitStarted("turn-1")
		waitFor(t, "the waiter's target", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		sigs <- syscall.SIGINT
		waitFor(t, "the cancel sent", func() bool { return len(tp.lines(protocol.MethodSessionCancel)) == 1 })
		sigs <- syscall.SIGINT
		res, f := w.result(t)
		if res != nil || f == nil || f.msg != "interrupted again before the cancel was confirmed" || f.handle == "" {
			t.Fatalf("the second signal: %+v, %+v", res, f)
		}
	})
}
