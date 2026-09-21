package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
)

// progressInterval is the least time between two snapshots a call's
// Progress passes on (plan 019 §3.5).
const progressInterval = 100 * time.Millisecond

// Options configure NewDispatcher.
type Options struct {
	// Tools are the session's profile's tools.
	Tools []Tool
	// Gate judges every call before it runs; nil is AllowAll.
	Gate Gate
	// Env is the session's. Workspace and Home must be absolute. The
	// dispatcher sets Progress per call, and Locks when it is nil.
	Env Env
}

// Dispatcher runs a session's tool calls: prepare, then gate, then run,
// then redact everything outward, then truncate the model's text (plan 019
// §3.1). It never switches on a tool's name: each tool's Prepare validates
// its own input, so a second profile needs nothing from it.
//
// It is built for the way Fantasy drives tools (plan 019 §2.4). Prepare is
// called from OnToolCall, on the stream goroutine, one call at a time in
// call order. Run is called from Fantasy's tool goroutines, up to five at
// once, and Discard from anywhere. Every Prepare leaves one entry, keyed by
// the call's harness id, that the first Run or Discard of that id releases;
// Run executes exactly the Prepared value Prepare made, so the gate judges
// what runs.
//
// Nothing here returns a Go error once the dispatcher is built, and nothing
// a tool or gate does — a failure, a panic — escapes as one: Fantasy treats
// a tool's error as fatal to the turn.
type Dispatcher struct {
	tools map[string]registered
	names []string // tool ids in profile order, for the unknown-tool text
	gate  Gate
	// env is the session's, with Progress and Redactor left out: both are
	// set per call, Progress from the caller and Redactor from red.
	env Env
	// red is the session's redactor, swappable: a session learns a provider's
	// key when it switches to that provider's model, and every call from then
	// on must redact it (SetRedactor).
	red atomic.Pointer[redact.Replacer]
	// planPath is Env.PlanPath, set once the session knows it (SetPlanPath).
	planPath atomic.Pointer[string]

	// interval is the progress throttle's; tests replace it.
	interval time.Duration

	mu      sync.Mutex
	pending map[string]*entry
}

// registered is a tool with the Spec it was validated under.
type registered struct {
	tool Tool
	spec Spec
}

// entry is one prepared call waiting for its Run.
type entry struct {
	spec     Spec
	req      Request  // redacted: what the gate sees
	prepared Prepared // nil when the call will not run
	failed   Result   // what Run returns instead when prepared is nil
}

// NewDispatcher checks the tools (as Registry.Register does) and the Env.
func NewDispatcher(o Options) (*Dispatcher, error) {
	specs, err := validateTools(o.Tools)
	if err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	if !filepath.IsAbs(o.Env.Workspace) || !filepath.IsAbs(o.Env.Home) {
		return nil, fmt.Errorf("tool: the workspace (%q) and the home (%q) must be absolute", o.Env.Workspace, o.Env.Home)
	}
	d := &Dispatcher{
		tools:    make(map[string]registered, len(o.Tools)),
		gate:     o.Gate,
		env:      o.Env,
		interval: progressInterval,
		pending:  make(map[string]*entry),
	}
	for i, s := range specs {
		d.tools[s.ID] = registered{o.Tools[i], s}
		d.names = append(d.names, s.ID)
	}
	if d.gate == nil {
		d.gate = AllowAll
	}
	if d.env.Locks == nil {
		d.env.Locks = &PathLocks{}
	}
	d.red.Store(o.Env.Redactor)
	d.env.Progress, d.env.Redactor = nil, nil // set per call, from red
	return d, nil
}

// SetRedactor makes r the redactor of every call prepared or run from now
// on. r must redact everything the one before it did — a session only ever
// learns more keys — and, like every Replacer, must not be changed after it
// is handed over. It is safe to call while calls run.
//
// A call already running keeps the Env it was given, so what it redacts as
// it goes — a command's live output, and the spill file written from it —
// still uses the redactor of when it started; its result does not, since the
// dispatcher redacts that when the call returns. A call that began before
// the session knew a key belongs to that earlier state (plan 019 §3.8).
func (d *Dispatcher) SetRedactor(r *redact.Replacer) { d.red.Store(r) }

// SetPlanPath makes path the plan file every call prepared or run from now on
// is told of (Env.PlanPath). The session calls it once, in Open, as soon as its
// transcript — whose sibling the plan file is — has a name, which is after the
// dispatcher is built.
func (d *Dispatcher) SetPlanPath(path string) { d.planPath.Store(&path) }

// redactor is the current one.
func (d *Dispatcher) redactor() *redact.Replacer { return d.red.Load() }

// callEnv is the Env one call gets: the session's, with this call's
// progress and the redactor of the moment.
func (d *Dispatcher) callEnv(progress Progress) Env {
	env := d.env
	env.Progress, env.Redactor = progress, d.redactor()
	if p := d.planPath.Load(); p != nil {
		env.PlanPath = *p
	}
	return env
}

// Prepare parses a call and holds it for Run. It returns the call's Request,
// redacted, for the event that announces the call. ok is false when the
// call will not run — the tool is unknown, its Prepare refused the input or
// panicked, or c.ID is unusable — and res is then the error result, class
// invalid_input or tool_error, which Run also returns for c.ID; when ok is
// true, res is zero.
//
// c.ID must be the harness's id: unique among pending calls and safe as a
// file name. A second Prepare of a pending id is a bug in the caller; both
// calls fail rather than one running twice.
func (d *Dispatcher) Prepare(c Call) (req Request, res Result, ok bool) {
	req = Request{ID: c.ID, CallID: c.CallID, Tool: c.Tool, Input: c.Input}
	e := &entry{}
	var targets []string // the gate's copy alone carries them; see Request
	t, known := d.tools[c.Tool]
	switch {
	case !validID(c.ID):
		e.failed = errorResult(ClassToolError, fmt.Sprintf("internal error: the harness gave this call an unusable id %q", c.ID))
	case !known:
		// Fantasy's own words for the same case (agent.go:1225), which is
		// what the model reads when Fantasy refuses the call itself.
		e.failed = errorResult(ClassInvalidInput,
			fmt.Sprintf("tool not found: %s. Available tools: %s", c.Tool, strings.Join(d.names, ", ")))
	default:
		e.spec = t.spec
		req.Kind, req.ReadOnly = t.spec.Kind, t.spec.ReadOnly
		var tr Request
		e.prepared, tr, e.failed = d.prepare(t.tool, c)
		req.Title, req.Paths, req.Command, req.Workdir = tr.Title, tr.Paths, tr.Command, tr.Workdir
		targets = tr.Targets
	}
	req = d.redactRequest(req)
	e.req = req
	// The gate is the one consumer of Targets, and it gets them as the tool
	// resolved them: a gate deciding where a call may write cannot be shown
	// text a redactor has rewritten, and req — the copy the announcing event
	// carries — has none (plan 023 §3.1).
	e.req.Targets = targets

	d.mu.Lock()
	if old, dup := d.pending[c.ID]; dup {
		old.prepared, old.failed = nil, errorResult(ClassToolError,
			fmt.Sprintf("internal error: call id %q was used twice; neither call ran", c.ID))
		e = old
	}
	d.pending[c.ID] = e
	d.mu.Unlock()

	if e.prepared == nil {
		return req, d.redactResult(e.failed), false
	}
	return req, Result{}, true
}

// prepare runs the tool's Prepare and its Request, turning an error into an
// invalid_input result and a panic into a tool_error one. p is nil exactly
// when the call failed.
func (d *Dispatcher) prepare(t Tool, c Call) (p Prepared, req Request, fail Result) {
	defer func() {
		if r := recover(); r != nil {
			p, req, fail = nil, Request{}, errorResult(ClassToolError,
				fmt.Sprintf("tool %q panicked while preparing the call: %v", c.Tool, r))
		}
	}()
	p, err := t.Prepare(d.callEnv(func(string) {}), c)
	if err == nil && p == nil {
		err = errors.New("internal error: the tool prepared nothing")
	}
	if err != nil {
		// opencode's InvalidArgumentsError (tool/tool.ts:31-33).
		return nil, Request{}, errorResult(ClassInvalidInput, fmt.Sprintf(
			"The %s tool was called with invalid arguments: %s.\nPlease rewrite the input so it satisfies the expected schema.",
			c.Tool, strings.TrimSuffix(err.Error(), ".")))
	}
	return p, p.Request(), Result{}
}

// Discard releases a prepared call that will never run: Fantasy refused it
// itself (an invalid call), or its step ended without dispatching it. It is
// a no-op for an id that is not pending.
func (d *Dispatcher) Discard(id string) {
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}

// Pending is how many prepared calls are waiting for Run or Discard: zero
// whenever no step is in flight, unless a call leaked.
func (d *Dispatcher) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}

// Run executes the call Prepare held under id and releases it. It is safe
// to call concurrently for different ids, never returns a Go error, and
// never blocks on progress: snapshots go to progress (nil discards them)
// through a throttle that drops any sent within 100 ms of the last one or
// while progress is still taking the last one.
//
// progress must return promptly — the runner drops progress for calls it
// has seen finish and hands the rest to a non-blocking send — but a
// consumer that blocks costs Run and the tool nothing: each snapshot is
// delivered on its own goroutine, at most one per call is in flight, and a
// consumer that never returns strands that one goroutine and no more. A
// snapshot being delivered when Run returns may still arrive after it,
// which is why the runner drops progress for a finished call.
//
// The result is, in order: the held error result for a call that will not
// run; aborted, when ctx is already done; denied, when the gate refuses,
// fails, or asks (there is no one to ask); otherwise the tool's result, with
// a panic as tool_error. Every outward field is then redacted, and a
// successful result's text truncated per its Spec.
func (d *Dispatcher) Run(ctx context.Context, id string, progress Progress) Result {
	d.mu.Lock()
	e := d.pending[id]
	delete(d.pending, id)
	d.mu.Unlock()
	if e == nil {
		e = &entry{failed: errorResult(ClassToolError, fmt.Sprintf("internal error: no prepared tool call %q", id))}
	}

	// Every outcome, the dispatcher's own errors included, leaves through
	// here, so none can skip redaction; theirs are errors with a class, so
	// the rest leaves them as they are. Redaction comes before truncation,
	// so the spill file holds redacted text; truncation then adds only fixed
	// words and the spill path, which it redacts as it adds it. Each piece
	// of the result is redacted exactly once.
	res := d.outcome(ctx, e, progress)
	res.StopTurn = false
	if res.IsError && res.Class == "" {
		res.Class = ClassToolError
	}
	res = d.redactResult(res)
	if !res.IsError && e.spec.Truncate != None {
		res.Text, res.Trunc = truncate(d.env.Home, id, res.Text, e.spec.Truncate, limits{MaxLines, MaxBytes}, d.redactor())
	}
	return res
}

// outcome is the call's result as the tool, the gate or the dispatcher's
// own checks produced it, before Run finishes it.
func (d *Dispatcher) outcome(ctx context.Context, e *entry, progress Progress) Result {
	switch {
	case e.prepared == nil:
		return e.failed
	case ctx.Err() != nil:
		return errorResult(ClassAborted, AbortedText)
	}
	if fail := d.check(ctx, e.req); fail.IsError {
		return fail
	}
	pg := &progressGate{deliver: progress, redactor: d.redactor(), now: time.Now, interval: d.interval}
	defer pg.close()
	return d.run(ctx, e, d.callEnv(pg.send))
}

// check asks the gate and returns the result for a call it does not allow,
// or the zero Result to run it. A gate that fails, panics, or answers
// nothing denies: a call nobody could judge does not run.
func (d *Dispatcher) check(ctx context.Context, req Request) (fail Result) {
	defer func() {
		if r := recover(); r != nil {
			fail = errorResult(ClassDenied, fmt.Sprintf("The tool call was not run: its permission check panicked: %v", r))
		}
	}()
	dec, err := d.gate.Check(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return errorResult(ClassAborted, AbortedText)
		}
		return errorResult(ClassDenied, "The tool call was not run: its permission check failed: "+err.Error())
	}
	switch dec := dec.(type) {
	case Allow:
		return Result{}
	case Deny:
		reason := dec.Reason
		if strings.TrimSpace(reason) == "" {
			reason = deniedText
		}
		return errorResult(ClassDenied, reason)
	case Ask:
		return errorResult(ClassDenied, NoApprovalChannel)
	default:
		return errorResult(ClassDenied, "The tool call was not run: its permission check gave no decision.")
	}
}

// run calls the prepared call's Run, turning a panic into a tool_error
// result. Fantasy recovers a tool's panic too (agent.go:888-899), but then
// the result would skip redaction and truncation.
func (d *Dispatcher) run(ctx context.Context, e *entry, env Env) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = errorResult(ClassToolError, fmt.Sprintf("tool %q panicked: %v", e.spec.ID, r))
		}
	}()
	return e.prepared.Run(ctx, env)
}

func errorResult(class ErrorClass, text string) Result {
	return Result{Text: text, IsError: true, Class: class}
}

// redactRequest returns a copy of r with every text field redacted, and
// without its Targets, which nothing outward may see (see Request). The copy
// shares nothing with r, so an event holding it cannot see a tool's later
// changes to its own slices.
func (d *Dispatcher) redactRequest(r Request) Request {
	red := d.redactor()
	r.Targets = nil
	r.CallID = red.String(r.CallID)
	r.Tool = red.String(r.Tool)
	r.Title = red.String(r.Title)
	r.Command = red.String(r.Command)
	r.Workdir = red.String(r.Workdir)
	if r.Paths != nil {
		paths := make([]string, len(r.Paths))
		for i, p := range r.Paths {
			paths[i] = red.String(p)
		}
		r.Paths = paths
	}
	if r.Input != nil {
		// The marker holds no quote or backslash, so a key redacted inside a
		// JSON string leaves the JSON as valid as it was.
		r.Input = json.RawMessage(red.String(string(r.Input)))
	}
	return r
}

// redactResult returns a copy of r with every outward text field redacted
// (plan 019 §3.8), the spill path of a tool that truncated itself included:
// it is built from the harness's home, which is text like any other.
func (d *Dispatcher) redactResult(r Result) Result {
	red := d.redactor()
	r.Text = red.String(r.Text)
	r.Content = red.String(r.Content)
	r.Trunc.Spill = red.String(r.Trunc.Spill)
	if r.Output != nil {
		out := *r.Output
		out.Output = red.String(out.Output)
		r.Output = &out
	}
	if r.Edits != nil {
		edits := make([]FileEdit, len(r.Edits))
		for i, ed := range r.Edits {
			edits[i] = FileEdit{Path: red.String(ed.Path), Old: red.String(ed.Old), New: red.String(ed.New)}
		}
		r.Edits = edits
	}
	return r
}

// progressGate is one call's Progress: throttled, lossy, never blocking.
// An accepted snapshot is redacted and delivered on its own goroutine, so a
// slow consumer costs the tool nothing; while one is being delivered, the
// next is dropped rather than queued — so at most one goroutine per call
// ever waits on the consumer, for as long as the consumer takes — and after
// close none is accepted.
type progressGate struct {
	deliver  Progress
	redactor *redact.Replacer
	now      func() time.Time
	interval time.Duration

	mu   sync.Mutex
	last time.Time
	busy bool
	done bool
}

func (g *progressGate) send(snapshot string) {
	if g.deliver == nil {
		return
	}
	g.mu.Lock()
	now := g.now()
	if g.done || g.busy || now.Sub(g.last) < g.interval {
		g.mu.Unlock()
		return
	}
	g.busy, g.last = true, now
	g.mu.Unlock()

	go func() {
		defer func() {
			// A consumer's panic loses one snapshot, which the contract
			// allows; it must not take the process down from a goroutine
			// nothing else can recover.
			_ = recover()
			g.mu.Lock()
			g.busy = false
			g.mu.Unlock()
		}()
		// Run may have ended while this goroutine was starting; its
		// snapshot is dropped rather than delivered after the result.
		g.mu.Lock()
		done := g.done
		g.mu.Unlock()
		if !done {
			g.deliver(g.redactor.String(snapshot))
		}
	}()
}

func (g *progressGate) close() {
	g.mu.Lock()
	g.done = true
	g.mu.Unlock()
}
