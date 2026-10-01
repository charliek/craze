package fakehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
)

// oversizedEventBytes is well over the event log's default MaxRecordBytes
// (internal/journal.DefaultMaxRecordBytes, 8 MiB): an event this large is
// never delivered, and its subscription resets omitted (fixture 12).
const oversizedEventBytes = (8 << 20) + (1 << 20)

// Options configures a Host. The zero value is already deterministic: a
// fixed clock, a fixed pseudo-random token source, a fixed host id, craze
// version, pid, workspace and durable session id — every source of
// nondeterminism control.Options exposes a caller to pin (plan 027 X12.12).
// Two Hosts built from the zero value produce byte-identical wire traffic
// across runs and machines, except for the Stub's own randomly minted
// incarnation (doc.go): the wire fixtures name that by placeholder instead.
type Options struct {
	// HostID is hello's endpoint.hostId: 12 hex digits. "" is a fixed one.
	HostID string
	// CrazeVersion is hello's endpoint.crazeVersion. "" is a fixed one.
	CrazeVersion string
	// PID is hello's endpoint.pid. 0 is a fixed one — never os.Getpid,
	// which would make every fixture host-process-dependent.
	PID int
	// Workspace is the session info document's workspace. "" is "/work".
	Workspace string
	// CrazeSessionID is the durable session id every incarnation of this
	// Host shares (Restart mints a new incarnation, never a new session).
	// "" is a fixed one.
	CrazeSessionID string
	// Log receives the server's own connection log lines; nil discards them.
	Log func(string)

	// The rest are plan 030's opt-ins (§3.6a, §3.7). Each is off by default,
	// and off, the Host's wire is exactly an S2 host's — which is an older
	// host's to a plan 030 client, so the fixtures from before plan 030 are
	// that direction of §3.8's compatibility, unchanged (X1).
	//
	// Stop serves session.stop (capabilities.stop: true) through the
	// fixtures' own coordinator: the server raises its stop fence and answers
	// the receipt exactly as a real host's does, and the coordinator records
	// the stop and does nothing more until the run_stop op (RunStop) runs its
	// sequence — the engine's close — so a fixture places the session's end
	// exactly, with a duplicate stop or a refused attach before it. Off,
	// session.stop is refused unsupported, reason stop_unsupported.
	Stop bool
	// PermissionMode is the info document's permissionMode; "" leaves it
	// out.
	PermissionMode protocol.PermissionMode
	// StartedAt puts the info document's startedAt in: the Host's pinned
	// clock as New reads it (2026-01-01T00:00:00Z). Off leaves it out.
	StartedAt bool
	// RowFacts puts the row facts on the sessions.list row and
	// capabilities.rowFacts in the info document (plan 030 §3.8): the real
	// server's, from the engine over the Stub, every time on the Host's
	// pinned clock. Off, the row is S2's.
	RowFacts bool

	// Models, when non-empty, is the Stub's model catalog in place of its own
	// (grok, fast): plan 031's opt-in, so a fixture can advertise what a
	// native session does — remembered models carrying their rank, which the
	// catalogs' "recent" says (§3.6, P9). Every incarnation gets it. Empty,
	// the catalog is the Stub's, and the wire is exactly what it was.
	Models []agent.ModelInfo
}

func (o Options) withDefaults() Options {
	if o.HostID == "" {
		o.HostID = "0123456789ab"
	}
	if o.CrazeVersion == "" {
		o.CrazeVersion = "0.0.0-fakehost"
	}
	if o.PID == 0 {
		o.PID = 4242
	}
	if o.Workspace == "" {
		o.Workspace = "/work"
	}
	if o.CrazeSessionID == "" {
		o.CrazeSessionID = "session-fake-1"
	}
	return o
}

// clock is the Host's one clock: control.Options.Clock, engine.Options's
// ReceiptClock and the Stub's own Clock all read it, so every timestamp on
// the wire but the static retry horizon comes from one deterministic source.
// It starts fixed and only moves when AdvanceClock is called.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// clockStart is where every Host's clock starts.
var clockStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newClock() *clock {
	return &clock{t: clockStart}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// detTokens is control.Options.Tokens's deterministic source: a fixed-seed
// PRNG, never crypto/rand, so a host id (when minted) and every resume
// token are the same on every run of the fixtures.
type detTokens struct{ r *rand.Rand }

func newDetTokens() *detTokens { return &detTokens{r: rand.New(rand.NewSource(1))} }

func (d *detTokens) Read(p []byte) (int, error) { return d.r.Read(p) }

// dropHooks are DropConnections' test seams (host_test.go), both run with the
// accept gate shut: closed once every connection is closed, before the wait;
// drained once the wait has succeeded, before the gate reopens. Nil is none;
// a test sets them before calling DropConnections, on the same goroutine.
type dropHooks struct {
	closed, drained func()
}

// hostHooks are a Host's test seams (wire_test.go), fixed when the Host is
// built (newHost): the first incarnation is built there too, and the engine's
// goroutines read them from then on, so they are never written again and need
// no lock. The zero value is none, which is what New builds with.
type hostHooks struct {
	// session is what each incarnation's engine drives in place of its Stub —
	// a test's decorator of it (wire_test.go's heldStub, which holds a
	// prompt's continuation back before the Stub opens its turn). nil is the
	// Stub itself.
	session func(*tui.Stub) agent.Session
	// turnOpening runs when the end op finds the engine's current turn claimed
	// and not yet open in the Stub, once, before it waits for the opening
	// (waitForTurnOpen).
	turnOpening func()
	// cancelled runs when the end op's cancel has returned, before the op
	// waits for the turn to settle.
	cancelled func()
}

// Host is the fake host (plan 027 §3.11): the real control.Server serving the
// real engine over a tui.Stub, on one caller-supplied listener. Restart
// replaces the engine with a fresh incarnation of the same durable session,
// as SetEngine does in production. It is safe to build several Hosts in one
// test binary.
type Host struct {
	opts Options
	clk  *clock

	srv *control.Server
	ln  *stallListener

	// drop is DropConnections' test seams (host_test.go).
	drop dropHooks
	// hooks are the seams a test builds the Host with (newHost).
	hooks hostHooks

	mu   sync.Mutex
	stub *tui.Stub
	eng  *engine.Engine
	// stops is how many session.stop requests the server handed the
	// fixtures' coordinator (Options.Stop): at most one, the server's own
	// rule. stopped says run_stop has run the sequence.
	stops int
	// stopHeard is closed on the first stop the server hands over, so
	// RunStop can wait for it (stopHeardWait).
	stopHeard     chan struct{}
	stopHeardOnce sync.Once
	stopped       bool
	// reg is the Host's registration (Register), nil while it has none: each
	// new incarnation is written into its entry, as a real host's is.
	reg *rundir.Host
}

// New builds a Host and its first incarnation, started. Nothing is served
// until Serve is called.
func New(o Options) (*Host, error) { return newHost(o, hostHooks{}) }

// newHost is New with a test's seams (hostHooks).
func newHost(o Options, hooks hostHooks) (*Host, error) {
	o = o.withDefaults()
	h := &Host{opts: o, clk: newClock(), hooks: hooks, stopHeard: make(chan struct{})}
	co := control.Options{
		Log:            o.Log,
		HostID:         o.HostID,
		CrazeVersion:   o.CrazeVersion,
		PID:            o.PID,
		Workspace:      o.Workspace,
		Tokens:         newDetTokens(),
		Clock:          h.clk.now,
		PermissionMode: o.PermissionMode,
		RowFacts:       o.RowFacts,
	}
	if o.Stop {
		co.Stop = h.stopRequested
	}
	if o.StartedAt {
		co.StartedAt = h.clk.now()
	}
	h.srv = control.New(co)
	if err := h.newIncarnation(); err != nil {
		return nil, err
	}
	return h, nil
}

// newIncarnation builds a fresh Stub and engine over the same durable session
// id, starts it and hands it to the server (control.Server.SetEngine): a new
// incarnation, exactly what Restart is for. The Stub's InstallOnStart is on,
// so every incarnation's first delta carries every section, as a real
// session's session/new or session/load does (plan 027 §3.13).
func (h *Host) newIncarnation() error {
	stub := tui.NewStubNoPrimary()
	stub.Clock = h.clk.now
	stub.InstallOnStart = true
	if len(h.opts.Models) > 0 {
		stub.SetModels(h.opts.Models)
	}
	var sess agent.Session = stub
	if f := h.hooks.session; f != nil {
		sess = f(stub)
	}
	eng, err := engine.New(sess, engine.Options{
		CrazeSessionID: h.opts.CrazeSessionID,
		ReceiptClock:   h.clk.now,
	})
	if err != nil {
		return err
	}
	if err := eng.Start(context.Background()); err != nil {
		return err
	}
	// The install delta Start enqueued (InstallOnStart) is committed by the
	// log's drainer, on its own goroutine: committed here, before any op can
	// publish, so it is always the incarnation's first event rather than
	// wherever the drainer happens to land among the ops' direct publishes.
	if err := syncLog(eng); err != nil {
		_ = eng.Close()
		return err
	}
	h.mu.Lock()
	old := h.eng
	h.stub, h.eng = stub, eng
	reg := h.reg
	h.mu.Unlock()
	h.srv.SetEngine(eng)
	if old != nil {
		go func() { _ = old.Close() }()
	}
	if reg != nil {
		st := eng.State()
		err := reg.Update(func(e *rundir.Entry) { e.Incarnation, e.ProviderSessionID = st.Incarnation, st.SessionID })
		if err != nil && !errors.Is(err, rundir.ErrClosed) {
			return fmt.Errorf("fakehost: the registry entry: %w", err)
		}
	}
	return nil
}

// syncWait bounds syncLog's flush: generous next to how long the drainer
// takes to commit what an op enqueued (microseconds; the Stub's log has no
// primary, so no reader can hold it up), so hitting it at all means
// something is wedged, and it is reported as an error rather than a hang.
const syncWait = 10 * time.Second

// syncLog commits everything eng's log has enqueued so far (engine.Sync, the
// log's Flush barrier). Events reach the log two ways: the Stub's own emits
// publish directly and are committed before they return, while a delta the
// Stub enqueues (InstallOnStart's install) and every ending the ask registry
// writes (a cancelled turn's asks: EndTurn) go through the log's outbox and
// are committed by its drainer, on its own goroutine. Unflushed, the two race
// for seqs: whether an enqueued event lands before or after the next op's
// direct publish is the scheduler's choice, and a fixture's entry ids — each
// one a seq — move with it (-cpu=1 turned fixture 11's install delta from
// first to last). So the Host flushes after the engine's start and after
// every op (Do): each op's enqueued events are committed before the op
// returns, and the seq order is the script's order whatever the scheduler
// does.
func syncLog(eng *engine.Engine) error {
	ctx, cancel := context.WithTimeout(context.Background(), syncWait)
	defer cancel()
	if err := eng.Sync(ctx); err != nil {
		return fmt.Errorf("fakehost: committing the log's enqueued events: %w", err)
	}
	return nil
}

// currentStub is the Stub of the current incarnation: taken under mu, since
// Restart swaps it.
func (h *Host) currentStub() *tui.Stub {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stub
}

// currentEngine is the engine of the current incarnation.
func (h *Host) currentEngine() *engine.Engine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.eng
}

// Serve wraps l (StallWrites, DropConnections) and serves it: blocking, as
// control.Server.Serve is; a caller runs it in a goroutine.
func (h *Host) Serve(l net.Listener) error {
	sl := newStallListener(l)
	h.mu.Lock()
	h.ln = sl
	h.mu.Unlock()
	return h.srv.Serve(sl)
}

// Register binds the Host's control socket in env's runtime tree and lists it
// in env's registry exactly as a craze host does (rundir.Bind; plan 032
// §3.15): its lifetime lock held, its entry naming the socket, the Host's id
// and durable session id, the current incarnation and provider session id,
// its provider's name and workspace, ready, and started when its clock
// started. That is what a hub (or `craze attach`, or the TUI's list) finds a
// host by, so several Hosts with distinct ids can be listed side by side in
// one registry. The caller serves the returned host's Listener (Serve) and
// closes it — which unlists the Host and unlinks its socket — after Close.
// Each later incarnation (Restart) is written into the entry. A Host is
// registered at most once.
func (h *Host) Register(env rundir.Env) (*rundir.Host, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reg != nil {
		return nil, fmt.Errorf("fakehost: host %s is registered already", h.opts.HostID)
	}
	st := h.eng.State()
	reg, err := rundir.Bind(env, h.opts.HostID, rundir.Entry{
		StartedAt:         clockStart,
		CrazeSessionID:    h.opts.CrazeSessionID,
		ProviderSessionID: st.SessionID,
		Incarnation:       st.Incarnation,
		Provider:          st.Provider.Name,
		Workspace:         h.opts.Workspace,
		Ready:             true,
	})
	if err != nil {
		return nil, err
	}
	h.reg = reg
	return reg, nil
}

// listener is the current stallListener, under mu like every other field a
// caller and Serve's own goroutine can touch concurrently.
func (h *Host) listener() *stallListener {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ln
}

// Close closes the server (every connection, every listener) and the current
// engine.
func (h *Host) Close(ctx context.Context) error {
	err := h.srv.Close(ctx)
	if cerr := h.currentEngine().Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// HostID is the server's hostId, as pinned by Options (or its default).
func (h *Host) HostID() string { return h.srv.HostID() }

// OpenConns is how many connections the server holds open now
// (control.Server.OpenConns): what a client's test counts its own by — a
// poller that keeps one per host, and closes them all when it stops.
func (h *Host) OpenConns() int { return h.srv.OpenConns() }

// OnAttachments is the server's (control.Server.OnAttachments): f is told the
// number of attached clients at every change, in order — a client's
// half-close takes its attachment out of the count — under the server's
// locks, so it must only record the number. What a test of a splice in front
// of this Host watches (plan 032 A11).
func (h *Host) OnAttachments(f func(n int)) { h.srv.OnAttachments(f) }

// SessionID is the durable craze session id every incarnation shares.
func (h *Host) SessionID() string { return h.opts.CrazeSessionID }

// Incarnation is the current incarnation's id: the one thing this Host
// cannot pin (the Stub's log mints a random UUIDv7). A fixture names it by
// placeholder instead (wire_test.go).
func (h *Host) Incarnation() string { return h.currentEngine().State().Incarnation }

// ---------------------------------------------------------------- host ops
//
// Each of these is one control operation, exactly as cmd/craze-fake-host
// reads it from stdin NDJSON ({"name": "...", ...}) and as a wire fixture's
// "op" lines script it (wire_test.go). The base list is the brief's: text,
// thought, tool, permission, question, plan, end, foreign_turn, stall_writes,
// drop_connections, restart, quit. Beyond it, the fixtures need five more:
// resume_writes (stall_writes's deterministic counterpart, fixture 4: see
// StallWrites), spawn_subagent (fixture 11's child), oversized_event
// (fixture 12's omitted record), advance_clock (fixture 13's retired
// client, past the binding table's idle bound) and hang_next (fixture 9's
// prompt, kept from racing its own reply: see HangNext). Plan 030 adds
// run_stop (RunStop): the stop sequence of a Host built with Options.Stop,
// run where a fixture's script says. Plan 032 adds unlist (Unlist): a
// registered Host leaving the registry, for the hub's roster (fixture 20).
// Do is what runs an
// op to completion — the op, then the log flushed (syncLog) — so a caller
// that needs the script's seq order goes through Do, as both of those do; the
// "end" op also waits there, before it cancels, for the Stub to have opened
// the turn (waitForTurnOpen).

// Text emits an EventText, agent "" for the main session.
func (h *Host) Text(agentID, text string) {
	h.currentStub().Emit(agent.Event{Type: agent.EventText, Agent: agentID, Text: text})
}

// Thought emits an EventThought.
func (h *Host) Thought(agentID, text string) {
	h.currentStub().Emit(agent.Event{Type: agent.EventThought, Agent: agentID, Text: text})
}

// Tool emits an EventTool with the given id, name and status, agentID "" for
// the main session.
func (h *Host) Tool(agentID, id, name, status string) { h.ToolTitled(agentID, id, name, "", status) }

// ToolTitled is Tool with the call's title — what a row's doing names a
// running tool by (plan 030 §3.10) — "" for none, which is Tool.
func (h *Host) ToolTitled(agentID, id, name, title, status string) {
	h.currentStub().Emit(agent.Event{Type: agent.EventTool, Agent: agentID, Tool: &agent.ToolEvent{ID: id, Name: name, Title: title, Status: status}})
}

// PermOption is one option a Permission call offers.
type PermOption struct{ OptionID, Name, Kind string }

// Permission opens a permission ask.
func (h *Host) Permission(id, tool string, opts []PermOption) {
	options := make([]agent.PermissionOption, len(opts))
	for i, o := range opts {
		options[i] = agent.PermissionOption{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind}
	}
	h.currentStub().Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: id, Tool: tool, Options: options}})
}

// QuestionOption is one option of one QuestionSpec.
type QuestionOption struct{ ID, Label string }

// QuestionSpec is one question of a Question call.
type QuestionSpec struct {
	ID            string
	Prompt        string
	Options       []QuestionOption
	AllowMultiple bool
}

// Question opens a question ask.
func (h *Host) Question(id, title string, qs []QuestionSpec) {
	questions := make([]agent.Question, len(qs))
	for i, q := range qs {
		opts := make([]agent.Option, len(q.Options))
		for j, o := range q.Options {
			opts[j] = agent.Option{ID: o.ID, Label: o.Label}
		}
		questions[i] = agent.Question{ID: q.ID, Prompt: q.Prompt, Options: opts, AllowMultiple: q.AllowMultiple}
	}
	h.currentStub().Emit(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: id, Title: title, Questions: questions}})
}

// Plan opens a plan ask.
func (h *Host) Plan(id, name, overview, planText string, todos []agent.Todo) {
	h.currentStub().Emit(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: id, Name: name, Overview: overview, Plan: planText, Todos: todos}})
}

// EndTurn ends whatever turn the Stub currently has open. The Stub's only way
// to end a still-open turn without a real agent's completion is Cancel, so
// this is what "end" means here: the ending is StopReason "cancelled".
//
// Cancel is fired and forgotten (tui.Stub.Cancel never blocks): it only
// signals the turn's own goroutine (parked in HangNext's select, or running
// its ordinary continuation) to wake, and that goroutine emits EventDone and
// settles the turn — clearing the engine's current turn and enqueuing the
// ending — on its own schedule, not this one. EndTurn itself stays a plain
// mirror of Cancel; the "end" op (do, below) is what waits for that
// settlement before the flush a caller of Do relies on — and, before it
// cancels, for the Stub to have opened the turn at all (waitForTurnOpen): a
// Cancel that lands on a prompt claimed and not yet open withdraws it
// instead, with no EventDone.
func (h *Host) EndTurn() {
	_, _ = h.currentStub().Cancel(context.Background())
}

// turnSettleWait bounds the "end" op's wait for the cancelled turn's own
// goroutine to settle: generous next to how long that wakeup and its
// settlement actually take (microseconds — the same class of budget as
// syncWait and dropConnectionsWait), so hitting it at all means something is
// wedged, and it is reported as an error rather than a hang.
const turnSettleWait = 10 * time.Second

// waitForTurnSettled blocks until eng reports no turn current. EndTurn's
// Cancel returns before the cancelled turn's goroutine wakes, emits its
// EventDone and settles (engine.settleLocked clears the current turn and
// enqueues the turn's ending — both under the lock State reads — before
// returning), so a State read of Turn == "" is the first moment that ending
// is guaranteed already enqueued, and the moment "end" (do, below) may flush
// it. Bounded like every other host op's wait: an error at the deadline
// instead of a hang.
func waitForTurnSettled(eng *engine.Engine, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		if eng.State().Turn == "" {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("fakehost: end: turn still current after %s", wait)
		}
		time.Sleep(time.Millisecond)
	}
}

// turnOpenWait bounds the "end" op's wait for the Stub to open the turn it
// cancels (waitForTurnOpen): generous next to how long the continuation takes
// to get there (microseconds — a goroutine the engine has already launched),
// the same class of budget as turnSettleWait, so hitting it at all means
// something is wedged, and it is reported as an error rather than a hang.
const turnOpenWait = 10 * time.Second

// waitForTurnOpen blocks until the turn eng has current is open in stub
// (tui.Stub.InTurn, the Stub's own barrier for what a cancel will do), or until
// no turn is current. The engine claims a prompt (Stub.Begin) and enqueues its
// started under its own lock, but the Stub opens the turn later, on the
// continuation's goroutine — which the session.prompt reply does not wait for,
// and which a loaded scheduler can run after a script's next several lines
// (WF16: CI's -race run of fixture 16). A cancel landing in between finds a
// claim with no turn open and withdraws the prompt: no EventDone, and a
// synthetic ending in place of the turn's own — one event short of what the
// script recorded. So "end" waits here before it cancels (do), and the turn it
// ends is always the open one the script means. Nothing current — an idle
// session, or a withdrawn prompt already settled — is nothing to wait for.
// opening, when set, runs once the wait is known to be needed
// (hostHooks.turnOpening). Bounded like every other host op's wait: an error
// at the deadline instead of a hang.
func waitForTurnOpen(eng *engine.Engine, stub *tui.Stub, wait time.Duration, opening func()) error {
	deadline := time.Now().Add(wait)
	for {
		if eng.State().Turn == "" || stub.InTurn() {
			return nil
		}
		if opening != nil {
			opening()
			opening = nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("fakehost: end: the current turn still not open after %s", wait)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitTurnOpen is waitForTurnOpen on the current incarnation.
func (h *Host) awaitTurnOpen() error {
	h.mu.Lock()
	stub, eng := h.stub, h.eng
	h.mu.Unlock()
	return waitForTurnOpen(eng, stub, turnOpenWait, h.hooks.turnOpening)
}

// ForeignTurn brackets a foreign turn (agent.EventForeignTurn): running true
// to open it, running false with the same id to close it.
func (h *Host) ForeignTurn(id, text, reason string, running bool) {
	h.currentStub().SetForeignTurn(agent.ForeignTurnInfo{ID: id, Text: text, Reason: reason, Running: running})
}

// StallWrites holds up every byte the server writes on every connection it
// has accepted, present and future, until d has passed, or until
// ResumeWrites runs first (fixture 4). A fixture that needs the stall held
// for an exact sequence of ops, regardless of how long they take to run
// (under -race, say), sets d generously and calls ResumeWrites itself rather
// than waiting out d — the reason StallWrites{ms} alone is not what the
// runner uses for a deterministic overflow: an op's own duration is real
// wall-clock time, and racing it against a fixed deadline is exactly the
// timing dependence plan 027 X6 and the C9 brief's addendum forbid.
func (h *Host) StallWrites(d time.Duration) { h.listener().stall(d) }

// ResumeWrites clears an active stall at once (fixture 4): the deterministic
// way to end one, instead of waiting out StallWrites's own duration.
func (h *Host) ResumeWrites() { h.listener().resume() }

// dropConnectionsWait bounds DropConnections' wait for the server to finish
// releasing what it dropped: generous next to how fast a closed socket's
// read error actually reaches control.conn.close (microseconds), so hitting
// it at all means something else is wrong, not that the wait itself is
// underprovisioned.
const dropConnectionsWait = 5 * time.Second

// DropConnections closes every connection the server has accepted so far, as
// a network drop would (fixture 13), and does not return until the server
// has forgotten every one of THOSE connections: dropAll only closes the raw
// sockets, and control.Server's own cleanup (conn.close, unbind among it)
// runs asynchronously on each connection's reader as it notices — so a
// script's following advance_clock, run the instant this returns, always
// measures idleness from a release that has already happened, rather than
// racing that cleanup and sometimes recording it after the clock has already
// moved.
//
// The mechanism is the listener's accept gate, not arithmetic (C10c): for
// the whole op the gate is shut, so no connection is handed to the server —
// a client that redials meanwhile waits, as it would in the kernel's
// backlog, and is accepted only once the gate reopens. With nothing new
// able to arrive, "every connection this call dropped is forgotten" is
// exactly OpenConns() == 0: no count of accepted or closed connections to
// get wrong, and no reconnect to mistake for, or offset against, a
// connection still mid-cleanup (C9a review item 2, C9b, r23 finding b). The
// one connection that can be in flight past the gate — handed to the
// server's accept loop just before the gate shut, and not yet registered —
// is still in dropAll's set, so it is closed too, and the wait counts it
// until the loop is back in Accept (stallListener.handoffPending, read
// before OpenConns so no registration falls between the two reads).
//
// If the deadline passes with a connection still not forgotten, it returns
// an error instead of returning silently: the fixture runner fails the test
// on it, and the binary prints it to stderr (cmd/craze-fake-host's Do
// loop). The gate reopens either way.
func (h *Host) DropConnections() error {
	l := h.listener()
	l.shutGate()
	defer l.openGate()
	l.dropAll()
	if f := h.drop.closed; f != nil {
		f()
	}
	remaining := func() int {
		n := 0
		if l.handoffPending() {
			n = 1
		}
		return n + h.srv.OpenConns()
	}
	if err := waitForOpenConnsZero(dropConnectionsWait, remaining); err != nil {
		return err
	}
	if f := h.drop.drained; f != nil {
		f()
	}
	return nil
}

// waitForOpenConnsZero is DropConnections' wait, factored out so a unit test
// can hand it a stub openConns that never reaches zero — a connection that
// never finishes closing — without needing a real one to actually hang
// forever (host_test.go's TestDropConnectionsErrorsOnStuckConnection).
// DropConnections hands it the server's OpenConns plus a connection the
// server's accept loop may hold unregistered (never negative), so this
// loop's own "reaches zero" contract stays the same either way.
func waitForOpenConnsZero(wait time.Duration, openConns func() int) error {
	deadline := time.Now().Add(wait)
	for {
		if n := openConns(); n == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("fakehost: drop_connections: %d connection(s) still open after %s", openConns(), wait)
		}
		time.Sleep(time.Millisecond)
	}
}

// Restart replaces the engine with a fresh incarnation of the same session
// (fixture 3).
func (h *Host) Restart() error { return h.newIncarnation() }

// HangNext arms the Stub's next prompt to open a turn that stays open until
// EndTurn cancels it, instead of completing on its own (fixture 9's "seen by
// both", C9a review item 3): the Stub's ordinary echo — its whole run, text
// and done included — is one asynchronous continuation the engine starts
// once the prompt's turn opens, and the reply's barrier only waits for the
// turn's OPENING to reach the client's own subscription, not for that
// continuation to finish, so nothing about their relative order on the wire
// is guaranteed. Hung instead, nothing follows the turn's started event on
// any subscription until this script says so (EndTurn) — armed before the
// prompt is sent, so it is in effect before the engine can claim it. The turn
// is open only once the prompt's continuation has run, which the prompt's
// reply does not wait for: the "end" op waits for that itself
// (waitForTurnOpen).
func (h *Host) HangNext() { h.currentStub().HangNext() }

// SpawnSubagent registers a running child (fixture 11).
func (h *Host) SpawnSubagent(id string) {
	h.currentStub().Emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: id, Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned})
}

// OversizedEvent emits a text event too large for any subscription to
// deliver, so its record is omitted (fixture 12).
func (h *Host) OversizedEvent() {
	h.currentStub().Emit(agent.Event{Type: agent.EventText, Text: strings.Repeat("o", oversizedEventBytes)})
}

// AdvanceClock moves the Host's clock forward by d: control.Options.Clock and
// engine.Options.ReceiptClock both read it, so this is how a fixture reaches
// the binding table's idle bound or the receipts table's age bound with no
// wall-clock wait (fixture 13).
func (h *Host) AdvanceClock(d time.Duration) { h.clk.advance(d) }

// Quit closes the Host: cmd/craze-fake-host's "quit" op and stdin EOF both
// mean this.
func (h *Host) Quit(ctx context.Context) error { return h.Close(ctx) }

// Unlist takes a registered Host out of the registry (plan 032 §3.15): its
// registration (Register's) is closed — its entry, lifetime lock and socket
// gone, as a host's are when it exits, so the hub's next read of the
// registry finds it gone — while the connections it has stay served. Its
// caller's own close of the registration later does nothing more; a Host
// never registered is an error.
func (h *Host) Unlist() error {
	h.mu.Lock()
	reg := h.reg
	h.mu.Unlock()
	if reg == nil {
		return fmt.Errorf("fakehost: unlist: host %s is not registered", h.opts.HostID)
	}
	return reg.Close()
}

// stopRequested is the fixtures' coordinator (control.StopFunc, with
// Options.Stop): it records the stop the server handed it — once, the
// server's own rule — and returns, the sequence left to RunStop.
func (h *Host) stopRequested(control.StopRequest) {
	h.mu.Lock()
	h.stops++
	h.mu.Unlock()
	h.stopHeardOnce.Do(func() { close(h.stopHeard) })
}

// stopHeardWait bounds how long RunStop waits for the server to hand the
// coordinator its stop. Since plan 030 C1r the server queues a stop's {}
// receipt before it calls the coordinator (X3), so a fixture that read the
// receipt can reach run_stop an instant before the handler's call lands.
const stopHeardWait = 10 * time.Second

// RunStop is the run_stop op: the fixtures' coordinator runs the stop
// sequence it was handed (plan 030 §3.6a) — here, the current engine's close,
// which authors a running turn's ending and ends every attachment with its
// closing records and reset{session_closed}, and every connection with them.
// The server itself is closed by Close, as a fixture's runner does at its
// end. It fails unless exactly one stop was handed over and the sequence has
// not run yet: a fixture that says run_stop has proven the coordinator heard
// its stop, once, however many were sent.
func (h *Host) RunStop() error {
	if h.opts.Stop {
		select {
		case <-h.stopHeard:
		case <-time.After(stopHeardWait):
		}
	}
	h.mu.Lock()
	stops, stopped := h.stops, h.stopped
	h.stopped = true
	h.mu.Unlock()
	switch {
	case !h.opts.Stop:
		return fmt.Errorf("fakehost: run_stop: this Host does not serve session.stop (Options.Stop)")
	case stops != 1:
		return fmt.Errorf("fakehost: run_stop: the coordinator was handed %d stops, want exactly 1", stops)
	case stopped:
		return fmt.Errorf("fakehost: run_stop: the stop sequence has already run")
	}
	return h.currentEngine().Close()
}

// opParams is one op's wire shape: {"name": "...", ...}, the union of every
// op's own fields (Do). It is the same shape cmd/craze-fake-host reads from
// stdin and a fixture's "op" lines carry.
type opParams struct {
	Name string `json:"name"`

	// text, thought, tool, permission, question, plan, foreign_turn,
	// spawn_subagent: which one they open, act on or belong to.
	ID    string `json:"id,omitempty"`
	Agent string `json:"agent,omitempty"`
	Text  string `json:"text,omitempty"`

	// tool
	ToolName string `json:"toolName,omitempty"`
	Status   string `json:"status,omitempty"`

	// permission
	Tool    string         `json:"tool,omitempty"`
	Options []opPermOption `json:"options,omitempty"`

	// question, and tool: the call's title
	Title     string       `json:"title,omitempty"`
	Questions []opQuestion `json:"questions,omitempty"`

	// plan
	PlanName string       `json:"planName,omitempty"`
	Overview string       `json:"overview,omitempty"`
	Plan     string       `json:"plan,omitempty"`
	Todos    []agent.Todo `json:"todos,omitempty"`

	// foreign_turn
	Running *bool  `json:"running,omitempty"`
	Reason  string `json:"reason,omitempty"`

	// stall_writes, advance_clock
	MS int `json:"ms,omitempty"`

	// text, when Text is "": Bytes of filler ("x") instead of literal text —
	// fixture 4's deterministic overflow, a single push too large for a small
	// requested budget to ever hold, needs no exact wording.
	Bytes int `json:"bytes,omitempty"`
}

type opPermOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type opQuestionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type opQuestion struct {
	ID            string             `json:"id"`
	Prompt        string             `json:"prompt"`
	Options       []opQuestionOption `json:"options,omitempty"`
	AllowMultiple bool               `json:"allowMultiple,omitempty"`
}

// Do runs one op from its wire bytes ({"name": "...", ...}): the NDJSON
// dispatch cmd/craze-fake-host's stdin loop and a wire fixture's "op" lines
// both use. Every op but quit (which has closed the engine) ends with the
// current engine's log flushed (syncLog): whatever the op enqueued is
// committed before Do returns, so the next op's events always follow it.
func (h *Host) Do(raw json.RawMessage) error {
	var p opParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("fakehost: op: %w", err)
	}
	if err := h.do(p); err != nil {
		return err
	}
	if p.Name == "quit" || p.Name == "run_stop" {
		// The engine has closed: its log has nothing left to flush.
		return nil
	}
	return syncLog(h.currentEngine())
}

// do is Do's dispatch: one op, not yet flushed.
func (h *Host) do(p opParams) error {
	switch p.Name {
	case "text":
		text := p.Text
		if text == "" && p.Bytes > 0 {
			text = strings.Repeat("x", p.Bytes)
		}
		h.Text(p.Agent, text)
	case "thought":
		h.Thought(p.Agent, p.Text)
	case "tool":
		h.ToolTitled(p.Agent, p.ID, p.ToolName, p.Title, p.Status)
	case "permission":
		opts := make([]PermOption, len(p.Options))
		for i, o := range p.Options {
			opts[i] = PermOption(o)
		}
		h.Permission(p.ID, p.Tool, opts)
	case "question":
		qs := make([]QuestionSpec, len(p.Questions))
		for i, q := range p.Questions {
			opts := make([]QuestionOption, len(q.Options))
			for j, o := range q.Options {
				opts[j] = QuestionOption(o)
			}
			qs[i] = QuestionSpec{ID: q.ID, Prompt: q.Prompt, Options: opts, AllowMultiple: q.AllowMultiple}
		}
		h.Question(p.ID, p.Title, qs)
	case "plan":
		h.Plan(p.ID, p.PlanName, p.Overview, p.Plan, p.Todos)
	case "end":
		if err := h.awaitTurnOpen(); err != nil {
			return err
		}
		h.EndTurn()
		if f := h.hooks.cancelled; f != nil {
			f()
		}
		return waitForTurnSettled(h.currentEngine(), turnSettleWait)
	case "hang_next":
		h.HangNext()
	case "foreign_turn":
		h.ForeignTurn(p.ID, p.Text, p.Reason, p.Running != nil && *p.Running)
	case "stall_writes":
		h.StallWrites(time.Duration(p.MS) * time.Millisecond)
	case "resume_writes":
		h.ResumeWrites()
	case "drop_connections":
		return h.DropConnections()
	case "restart":
		return h.Restart()
	case "quit":
		return h.Quit(context.Background())
	case "run_stop":
		return h.RunStop()
	case "unlist":
		return h.Unlist()
	case "spawn_subagent":
		h.SpawnSubagent(p.ID)
	case "oversized_event":
		h.OversizedEvent()
	case "advance_clock":
		h.AdvanceClock(time.Duration(p.MS) * time.Millisecond)
	default:
		return fmt.Errorf("fakehost: unknown op %q", p.Name)
	}
	return nil
}
