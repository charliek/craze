package fakehost

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
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

func newClock() *clock {
	return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
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

	mu   sync.Mutex
	stub *tui.Stub
	eng  *engine.Engine
}

// New builds a Host and its first incarnation, started. Nothing is served
// until Serve is called.
func New(o Options) (*Host, error) {
	o = o.withDefaults()
	h := &Host{opts: o, clk: newClock()}
	h.srv = control.New(control.Options{
		Log:          o.Log,
		HostID:       o.HostID,
		CrazeVersion: o.CrazeVersion,
		PID:          o.PID,
		Workspace:    o.Workspace,
		Tokens:       newDetTokens(),
		Clock:        h.clk.now,
	})
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
	eng, err := engine.New(stub, engine.Options{
		CrazeSessionID: h.opts.CrazeSessionID,
		ReceiptClock:   h.clk.now,
	})
	if err != nil {
		return err
	}
	if err := eng.Start(context.Background()); err != nil {
		return err
	}
	h.mu.Lock()
	old := h.eng
	h.stub, h.eng = stub, eng
	h.mu.Unlock()
	h.srv.SetEngine(eng)
	if old != nil {
		go func() { _ = old.Close() }()
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
// drop_connections, restart, quit. Beyond it, the fixtures need four more:
// resume_writes (stall_writes's deterministic counterpart, fixture 4: see
// StallWrites), spawn_subagent (fixture 11's child), oversized_event
// (fixture 12's omitted record) and advance_clock (fixture 13's retired
// client, past the binding table's idle bound).

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
func (h *Host) Tool(agentID, id, name, status string) {
	h.currentStub().Emit(agent.Event{Type: agent.EventTool, Agent: agentID, Tool: &agent.ToolEvent{ID: id, Name: name, Status: status}})
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
func (h *Host) EndTurn() {
	_, _ = h.currentStub().Cancel(context.Background())
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

// DropConnections closes every connection the server has accepted so far, as
// a network drop would (fixture 13).
func (h *Host) DropConnections() { h.listener().dropAll() }

// Restart replaces the engine with a fresh incarnation of the same session
// (fixture 3).
func (h *Host) Restart() error { return h.newIncarnation() }

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

	// question
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
// both use.
func (h *Host) Do(raw json.RawMessage) error {
	var p opParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("fakehost: op: %w", err)
	}
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
		h.Tool(p.Agent, p.ID, p.ToolName, p.Status)
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
		h.EndTurn()
	case "foreign_turn":
		h.ForeignTurn(p.ID, p.Text, p.Reason, p.Running != nil && *p.Running)
	case "stall_writes":
		h.StallWrites(time.Duration(p.MS) * time.Millisecond)
	case "resume_writes":
		h.ResumeWrites()
	case "drop_connections":
		h.DropConnections()
	case "restart":
		return h.Restart()
	case "quit":
		return h.Quit(context.Background())
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
