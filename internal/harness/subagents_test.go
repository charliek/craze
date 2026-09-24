package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// The sub-agent runner (plan 026 §3.7–§3.9, §7 A6–A9). Every test here runs
// real child sessions: the parent's model calls the agent tool, the runner
// opens a child through Open and runs its turn on Fantasy's own loop. Their
// models are one routing scripted model per alias (router), shared by the
// parent and every child on that alias and routed by each request's first
// user message, which is the prompt of the turn that sent it; a child's
// scripted step can hold on a channel the test owns (worker), which is how a
// test fixes a schedule — two children mid-step at once, a cancel while one
// waits for its provider — with barriers and never a sleep. What outlives a
// call is checked the same way: a worker's step closes its exited channel as
// it returns, and every one must be closed by the time the call's turn has
// returned. Goroutine leaks are checked after settlement with the package's
// existing helper (goroutines and waitFor, runner_test.go).

// router is a fantasy.LanguageModel that answers each request with the next
// step queued for the prompt that request's turn was sent: the first user
// message in it. A parent's turn is keyed by the prompt it ran, a child's by
// the prompt its runner sent it, so children streaming at once never take
// each other's steps, as a single queue would let them.
type router struct {
	provider, wire string

	mu     sync.Mutex
	queues map[string][]step
	calls  map[string][]fantasy.Call
}

// newRouted is a fixture whose every alias is served by a router.
func newRouted(t *testing.T) *routed {
	t.Helper()
	f := newFixture(t, "http://127.0.0.1:1/v1")
	rf := &routed{fixture: f, routers: map[string]*router{}}
	for alias, m := range f.table.Models {
		rf.routers[alias] = &router{provider: m.Provider, wire: m.WireModel,
			queues: map[string][]step{}, calls: map[string][]fantasy.Call{}}
	}
	return rf
}

// routed is a fixture whose models are routers.
type routed struct {
	*fixture
	routers map[string]*router
}

// options are the fixture's, with every alias built as its router.
func (f *routed) options() Options {
	o := f.fixture.options()
	o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) { return f.routers[r.Alias], nil }
	return o
}

// route queues steps for the turn that runs prompt.
func (m *router) route(prompt string, steps ...step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues[prompt] = append(m.queues[prompt], steps...)
}

// requests are the requests the turn that ran prompt sent.
func (m *router) requests(prompt string) []fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls[prompt])
}

func (m *router) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	key := firstUserText(call)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[key] = append(m.calls[key], call)
	q := m.queues[key]
	if len(q) == 0 {
		return nil, fmt.Errorf("router: no step queued for %q", key)
	}
	next := q[0]
	m.queues[key] = q[1:]
	return func(yield func(fantasy.StreamPart) bool) { next(ctx, yield) }, nil
}

func (m *router) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("router: Generate is not used")
}

func (m *router) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("router: GenerateObject is not used")
}

func (m *router) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("router: StreamObject is not used")
}

func (m *router) Provider() string { return m.provider }
func (m *router) Model() string    { return m.wire }

// firstUserText is the text of a request's first user message: the prompt of
// the turn that sent it. A mode's reminder follows the prompt, and a steer
// follows a step, so neither is ever first.
func firstUserText(call fantasy.Call) string {
	for _, msg := range call.Prompt {
		if msg.Role == fantasy.MessageRoleUser {
			return messageText(msg)
		}
	}
	return ""
}

// systemText is a request's system prompt.
func systemText(call fantasy.Call) string {
	for _, msg := range call.Prompt {
		if msg.Role == fantasy.MessageRoleSystem {
			return messageText(msg)
		}
	}
	return ""
}

// worker is a scripted step a test owns: it yields its first parts, closes
// reached, holds until the test closes release — or, unless it ignores the
// cancel, until its context is done — and closes exited as it returns, however
// it returns. A context done by the time it goes on ends it with the
// context's error part, as a provider's stream ends when its request is
// cancelled.
type worker struct {
	name                     string
	reached, release, exited chan struct{}
	// ignoreCancel holds the step until release even when its context ends
	// first, so a test can land two causes before the child sees either.
	ignoreCancel bool
	// notify, when set, is sent the worker once it has reached: how a test
	// waits for any n of several workers without knowing which.
	notify chan<- *worker
}

func newWorker() *worker {
	return &worker{reached: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
}

// workers are n workers, each named for the prompt of the child it serves —
// "<prefix> 1" to "<prefix> n" — and routed on m with those prompts, each
// streaming "did <prompt>" once released, all notifying one channel.
func workers(m *router, prefix string, n int) (map[string]*worker, <-chan *worker) {
	notify := make(chan *worker, n)
	out := map[string]*worker{}
	for i := 1; i <= n; i++ {
		w := newWorker()
		w.name, w.notify = fmt.Sprintf("%s %d", prefix, i), notify
		m.route(w.name, w.step(openText("did "+w.name), finishText()))
		out[w.name] = w
	}
	return out, notify
}

func (w *worker) step(before, after []fantasy.StreamPart) step {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		defer close(w.exited)
		for _, p := range before {
			if !yield(p) {
				return
			}
		}
		close(w.reached)
		if w.notify != nil {
			w.notify <- w
		}
		if w.ignoreCancel {
			<-w.release
		} else {
			select {
			case <-ctx.Done():
			case <-w.release:
			}
		}
		if ctx.Err() != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		}
		for _, p := range after {
			if !yield(p) {
				return
			}
		}
	}
}

// agentPart is one agent call in a parent's step, its arguments args.
func agentPart(t *testing.T, id string, args map[string]any) []fantasy.StreamPart {
	t.Helper()
	return callParts(id, "agent", input(t, args))
}

// task is an agent call's arguments: a description and a prompt, and any
// more keys and values after them.
func task(description, prompt string, more ...string) map[string]any {
	args := map[string]any{"description": description, "prompt": prompt}
	for i := 0; i+1 < len(more); i += 2 {
		args[more[i]] = more[i+1]
	}
	return args
}

// kids records the child sessions a runner opened, by id (subagentSeams.opened).
type kids struct {
	mu   sync.Mutex
	byID map[string]*Session
	ids  []string
}

func watchKids(s *Session) *kids {
	k := &kids{byID: map[string]*Session{}}
	s.subs.seams.opened = func(id string, c *Session) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.byID[id] = c
		k.ids = append(k.ids, id)
	}
	return k
}

func (k *kids) get(id string) *Session {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.byID[id]
}

func (k *kids) all() []*Session {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]*Session, 0, len(k.ids))
	for _, id := range k.ids {
		out = append(out, k.byID[id])
	}
	return out
}

// registered is how many children s's runner has registered now.
func registered(s *Session) int {
	s.subs.regMu.Lock()
	defer s.subs.regMu.Unlock()
	return len(s.subs.live)
}

// storeClosed reports whether st refuses an append as closed. An open store
// would hold the probe, so only a failing check leaves anything behind.
func storeClosed(st *store.Store) bool {
	err := st.AppendUser(store.MessageEntry{Message: fantasy.NewUserMessage("probe"),
		Model: store.Model{Provider: "p", Alias: "a", WireModel: "w"}})
	return errors.Is(err, store.ErrClosed)
}

// stopChild cancels the registered child id's context with errStoppedByUser:
// PR 2's CancelSubagent, reached here through the registry (plan 026 §3.10).
// It reports whether id was registered.
func stopChild(s *Session, id string) bool {
	s.subs.regMu.Lock()
	h := s.subs.live[id]
	s.subs.regMu.Unlock()
	if h == nil {
		return false
	}
	h.cancel(errStoppedByUser)
	return true
}

// settled checks that s's runner holds no slot and no registered child.
func settled(t *testing.T, s *Session) {
	t.Helper()
	if n, m := len(s.subs.slots), registered(s); n != 0 || m != 0 {
		t.Fatalf("after the calls: %d slots held and %d children registered; want none", n, m)
	}
}

// startedWith is the SubagentStarted whose prompt is prompt.
func startedWith(t *testing.T, evs []Event, prompt string) SubagentStarted {
	t.Helper()
	for _, s := range of[SubagentStarted](evs) {
		if s.Prompt == prompt {
			return s
		}
	}
	t.Fatalf("no SubagentStarted for %q in %v", prompt, of[SubagentStarted](evs))
	return SubagentStarted{}
}

// finishedOf is the SubagentFinished of child id.
func finishedOf(t *testing.T, evs []Event, id string) SubagentFinished {
	t.Helper()
	for _, f := range of[SubagentFinished](evs) {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("no SubagentFinished for %s", id)
	return SubagentFinished{}
}

// callResult is the parent's ToolFinished result for its call id.
func callResult(t *testing.T, evs []Event, id string) tool.Result {
	t.Helper()
	for _, f := range of[ToolFinished](evs) {
		if f.ID == id {
			return f.Result
		}
	}
	t.Fatalf("no ToolFinished for %s", id)
	return tool.Result{}
}

// oneStep is a child step's usage as a SubagentFinished sums it: stepUsage,
// once per step.
func oneStep(n int64) Usage {
	return Usage{Input: 10 * n, Output: 5 * n, CacheRead: 4 * n}
}

// A glob call: cheap, parallel, and harmless in any mode.
func globPart(id string) []fantasy.StreamPart {
	return callParts(id, "glob", `{"pattern":"*.none"}`)
}

// TestAgentFanOutRunsConcurrently (A6): two agent calls in one step start
// two children, and both are mid-step — each holding its provider's stream —
// before either is let go: they run at once, not one after the other. Each
// call's result is its own child's final message; both children are closed
// and their slots given back once the calls return.
func TestAgentFanOutRunsConcurrently(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	k := watchKids(s)
	a := f.routers["test/a"]
	w1, w2 := newWorker(), newWorker()
	a.route("fan out", callStep(agentPart(t, "a1", task("first", "child one")), agentPart(t, "a2", task("second", "child two"))),
		answerWith("both done"))
	a.route("child one", w1.step(openText("answer one"), finishText()))
	a.route("child two", w2.step(openText("answer two"), finishText()))

	var ev events
	out := start(context.Background(), s, "fan out", ev.sink)
	await(t, w1.reached, "the first child mid-step")
	await(t, w2.reached, "the second child mid-step")
	if n, m := len(s.subs.slots), registered(s); n != 2 || m != 2 {
		t.Fatalf("with both children mid-step: %d slots and %d registered; want 2 and 2", n, m)
	}
	if st, fin := of[SubagentStarted](ev.list()), of[SubagentFinished](ev.list()); len(st) != 2 || len(fin) != 0 {
		t.Fatalf("both children should have started and neither finished: %d started, %d finished", len(st), len(fin))
	}
	close(w1.release)
	close(w2.release)
	got := await(t, out, "the fan-out turn")
	if got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
	}
	evs := ev.list()
	for callID, want := range map[string]string{"t1.1.1": "answer one", "t1.1.2": "answer two"} {
		if res := callResult(t, evs, callID); res.Text != want || res.IsError {
			t.Fatalf("call %s = %+v; want its own child's answer %q", callID, res, want)
		}
	}
	for _, c := range k.all() {
		if !storeClosed(c.store) {
			t.Fatalf("child %s's store is still open", c.ID())
		}
	}
	settled(t, s)
}

// TestAgentCapOccupancy (A6): five agent calls in one step — Fantasy runs all
// five at once — and never more than four children: four open and hold, the
// fifth waits for a slot and starts nothing, and it starts as soon as one of
// the four is let go. Which four win is Fantasy's scheduling, so the test
// asserts occupancy and never which call waited.
func TestAgentCapOccupancy(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	a := f.routers["test/a"]
	pool, reached := workers(a, "task", 5)
	var parts [][]fantasy.StreamPart
	for i := 1; i <= 5; i++ {
		parts = append(parts, agentPart(t, fmt.Sprintf("a%d", i), task(fmt.Sprintf("job %d", i), fmt.Sprintf("task %d", i))))
	}
	a.route("five", callStep(parts...), answerWith("all done"))
	waiting := make(chan string, 5)
	s.subs.seams.waiting = func(c tool.SubagentCall) { waiting <- c.Prompt }
	var mu sync.Mutex
	most := 0
	s.subs.seams.opened = func(string, *Session) {
		mu.Lock()
		defer mu.Unlock()
		most = max(most, registered(s))
	}

	var ev events
	out := start(context.Background(), s, "five", ev.sink)
	var held []*worker
	for range 4 {
		held = append(held, await(t, reached, "a child holding a slot"))
	}
	waiter := await(t, waiting, "the fifth call waiting for a slot")
	for _, w := range held {
		if w.name == waiter {
			t.Fatalf("%q waits for a slot but its child also runs", waiter)
		}
	}
	// The waiter is past the seam and in its select, with every slot held:
	// nothing can free one but the test.
	if n, m, st := len(s.subs.slots), registered(s), len(of[SubagentStarted](ev.list())); n != 4 || m != 4 || st != 4 {
		t.Fatalf("four held: %d slots, %d registered, %d started; want 4, 4, 4", n, m, st)
	}
	select {
	case w := <-reached:
		t.Fatalf("%q's child ran while every slot was held", w.name)
	default:
	}

	close(held[0].release)
	if got := await(t, reached, "the waiter's child, once a slot freed"); got.name != waiter {
		t.Fatalf("%q ran when a slot freed; want the waiter %q", got.name, waiter)
	}
	// The freed slot was given back before the waiter could take it.
	if n := len(s.subs.slots); n != 4 {
		t.Fatalf("%d slots held with three old children and the waiter's; want 4", n)
	}
	for name, w := range pool {
		if name != held[0].name {
			close(w.release)
		}
	}
	if got := await(t, out, "the five-call turn"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
	}
	for i := 1; i <= 5; i++ {
		want := fmt.Sprintf("did task %d", i)
		if res := callResult(t, ev.list(), fmt.Sprintf("t1.1.%d", i)); res.IsError || res.Text != want {
			t.Fatalf("call %d = %+v; want its child's answer %q", i, res, want)
		}
	}
	if most > maxChildren {
		t.Fatalf("%d children were registered at once; the cap is %d", most, maxChildren)
	}
	settled(t, s)
}

// TestAgentSlotReleasedOnEveryPath (A6, panel P2, P36): whatever ends a call,
// it gives back its slot and retires its registry entry, and a child it
// opened is closed — its store refusing writes — before either.
func TestAgentSlotReleasedOnEveryPath(t *testing.T) {
	t.Run("a failed Open", func(t *testing.T) {
		// A real refusal: the parent froze an instruction holding a value that
		// was no key when it opened; the environment now says it is one, and
		// the child's Open refuses a prompt holding a key (errChildPromptKey).
		f := newRouted(t)
		const late = "sk-late-key-0001"
		env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
		var envMu sync.Mutex
		opts := f.options()
		opts.Getenv = func(k string) string {
			envMu.Lock()
			defer envMu.Unlock()
			return env[k]
		}
		opts.Prompt = PromptExtras{Instructions: []PromptDoc{{Path: filepath.Join(f.workspace, "AGENTS.md"), Text: "The token is " + late + ".\n"}}}
		s := f.open(opts)
		envMu.Lock()
		env["NOKEY_API_KEY"] = late
		envMu.Unlock()
		k := watchKids(s)
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("doomed", "never runs"))), answerWith("ok"))
		var ev events
		if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v; want the parent to carry on", res, err)
		}
		res := callResult(t, ev.list(), "t1.1.1")
		if want := "The sub-agent failed: " + errChildPromptKey.Error() + "."; res.Text != want || res.Class != tool.ClassToolError || res.Child != nil {
			t.Fatalf("the call = %+v; want %q, tool_error, no usage", res, want)
		}
		if len(k.all()) != 0 || len(of[SubagentStarted](ev.list())) != 0 {
			t.Fatal("a child that never opened was reported")
		}
		if n := transcripts(t, f.home); n != 1 {
			t.Fatalf("%d transcripts under the home; want the parent's alone", n)
		}
		settled(t, s)
	})

	t.Run("a panic during Run", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		k := watchKids(s)
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("explodes", "persist then panic"))), answerWith("ok"))
		a.route("persist then panic",
			callStep(textParts("first"), globPart("g1")),
			func(context.Context, func(fantasy.StreamPart) bool) { panic("the child's model exploded") })
		var ev events
		if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v; want the parent to carry on", res, err)
		}
		res := callResult(t, ev.list(), "t1.1.1")
		if res.Class != tool.ClassToolError || !strings.Contains(res.Text, `tool "agent" panicked: the child's model exploded`) {
			t.Fatalf("the call = %+v; want the dispatcher's recovered panic", res)
		}
		kid := k.all()[0]
		if !storeClosed(kid.store) {
			t.Fatal("the panicked child's store is still open")
		}
		// It had persisted its first step before it panicked.
		if lines := entries(transcript(t, kid)); len(lines) != 3 {
			t.Fatalf("the child's transcript:\n%s", strings.Join(lines, "\n"))
		}
		fin := finishedOf(t, ev.list(), kid.ID())
		if fin.Status != SubagentFailed || fin.Error != subagentPanicked || fin.Usage != oneStep(1) || fin.Text != "first" {
			t.Fatalf("SubagentFinished = %+v; want failed, panicked, one step's usage, its last output", fin)
		}
		settled(t, s)
	})

	t.Run("a cancel at acquisition", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		k := watchKids(s)
		a := f.routers["test/a"]
		_, reached := workers(a, "hold", 5)
		var parts [][]fantasy.StreamPart
		for i := 1; i <= 5; i++ {
			parts = append(parts, agentPart(t, fmt.Sprintf("a%d", i), task("hold", fmt.Sprintf("hold %d", i))))
		}
		a.route("five", callStep(parts...))
		waiting := make(chan string, 5)
		s.subs.seams.waiting = func(c tool.SubagentCall) { waiting <- c.Prompt }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var ev events
		out := start(ctx, s, "five", ev.sink)
		for range 4 {
			await(t, reached, "a child holding a slot")
		}
		waiter := await(t, waiting, "the fifth call waiting")
		cancel()
		if got := await(t, out, "the cancelled turn"); got.err != nil || got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
		}
		for i := 1; i <= 5; i++ {
			if res := callResult(t, ev.list(), fmt.Sprintf("t1.1.%d", i)); res.Class != tool.ClassAborted || res.Text != tool.AbortedText {
				t.Fatalf("call %d = %+v; want aborted", i, res)
			}
		}
		if n := len(k.all()); n != 4 {
			t.Fatalf("%d children opened; want the four that held a slot, not the waiter (%q)", n, waiter)
		}
		for _, c := range k.all() {
			if !storeClosed(c.store) {
				t.Fatalf("child %s's store is still open", c.ID())
			}
		}
		settled(t, s)
	})

	t.Run("a slot won with the context already done is given back", func(t *testing.T) {
		// Both select cases are ready, and select picks at random: whichever
		// it picks, the call does not hold a slot. Many times, so both picks
		// are made.
		f := newRouted(t)
		s := f.open(f.options())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for range 200 {
			if s.subs.acquire(ctx, tool.SubagentCall{}) || len(s.subs.slots) != 0 {
				t.Fatalf("acquire with a done context = true or a slot kept (%d held)", len(s.subs.slots))
			}
		}
		if !s.subs.acquire(context.Background(), tool.SubagentCall{}) {
			t.Fatal("control: a live call got no free slot")
		}
		s.subs.release()
		s.tools.close() // the session's closing channel, as Close closes it
		for range 200 {
			if s.subs.acquire(context.Background(), tool.SubagentCall{}) || len(s.subs.slots) != 0 {
				t.Fatalf("acquire in a closing session = true or a slot kept (%d held)", len(s.subs.slots))
			}
		}
	})

	t.Run("a Close before registration", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		k := watchKids(s)
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("late", "never registers"))))
		closed := make(chan error, 1)
		// Past the slot and its recheck, the session starts closing: Close
		// seals the registry before it closes the tools' channel, so by the
		// time the channel is closed the registration is refused.
		s.subs.seams.acquired = func(tool.SubagentCall) {
			go func() { closed <- s.Close() }()
			<-s.tools.closing
		}
		// The refusal is the registry's: no child is even opened. (The call's
		// context is done by now too, which would abort it a step later.)
		var opens atomic.Int32
		s.subs.seams.open = func(o Options) (*Session, error) {
			opens.Add(1)
			return Open(o)
		}
		var ev events
		if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled by the Close", res, err)
		}
		if err := await(t, closed, "Close"); err != nil {
			t.Fatal(err)
		}
		if res := callResult(t, ev.list(), "t1.1.1"); res.Class != tool.ClassAborted {
			t.Fatalf("the call = %+v; want aborted", res)
		}
		if opens.Load() != 0 || len(k.all()) != 0 || len(of[SubagentStarted](ev.list())) != 0 {
			t.Fatalf("a child was opened (%d) after Close had sealed the registry", opens.Load())
		}
		settled(t, s)
	})

	t.Run("a Close during Open", func(t *testing.T) {
		// The handle's latch (panel P36): the child is registered, Close
		// signals it while it is still opening — there is no session to
		// signal yet — and the opened child, attached, is closed before it
		// runs, never started.
		f := newRouted(t)
		s := f.open(f.options())
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("opening", "never runs"))))
		closed := make(chan error, 1)
		var child *Session
		s.subs.seams.open = func(o Options) (*Session, error) {
			go func() { closed <- s.Close() }()
			<-s.tools.closing // closeChildren has latched the registered handle
			c, err := Open(o)
			child = c
			return c, err
		}
		var ev events
		if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled by the Close", res, err)
		}
		if err := await(t, closed, "Close"); err != nil {
			t.Fatal(err)
		}
		if res := callResult(t, ev.list(), "t1.1.1"); res.Class != tool.ClassAborted {
			t.Fatalf("the call = %+v; want aborted", res)
		}
		if child == nil || !storeClosed(child.store) || len(of[SubagentStarted](ev.list())) != 0 {
			t.Fatalf("the child opened during Close: opened %v, started %d; want it closed and never started",
				child != nil, len(of[SubagentStarted](ev.list())))
		}
		if len(f.routers["test/a"].requests("never runs")) != 0 {
			t.Fatal("the latched child sent a request")
		}
		settled(t, s)
	})
}

// TestChildHandleClosingLatch (§3.8, panel P36): a Close's signal and the
// runner's attach of an opened child meet on the handle, in either order, and
// the child is signalled either way — by the Close when it was attached
// first, by the attach when the Close came first — and told it may not run
// only in the second case. Run concurrently, the two never leave a child
// unsignalled. (A Close during a real Open cannot land between the handle's
// latch and the parent's own cancel, which the runner's context check also
// catches; this is the latch alone.)
func TestChildHandleClosingLatch(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	parent := f.open(f.options())
	n := 0
	child := func() *Session {
		n++
		return f.open(childOf(f, parent, ChildOptions{ID: fmt.Sprintf("child-%04d", n), AllTools: true, Mode: "agent"}))
	}
	signalled := func(c *Session) bool {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		return closed && isClosed(c.tools.closing)
	}
	t.Run("signal, then attach", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		h := &childHandle{id: "a", cancel: cancel}
		h.signalClose()
		c := child()
		if h.attachChild(c) {
			t.Fatal("a child attached after its Close signal may run")
		}
		if !signalled(c) || !errors.Is(context.Cause(ctx), errClosing) {
			t.Fatal("the attach did not honour the latched signal")
		}
		if _, err := c.Run(context.Background(), "go", nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("the latched child's Run = %v; want ErrClosed", err)
		}
	})
	t.Run("attach, then signal", func(t *testing.T) {
		_, cancel := context.WithCancelCause(context.Background())
		h := &childHandle{id: "b", cancel: cancel}
		c := child()
		if !h.attachChild(c) || signalled(c) {
			t.Fatal("control: a child attached before any signal may not run, or was signalled")
		}
		h.signalClose()
		if !signalled(c) {
			t.Fatal("the signal did not reach the attached child")
		}
	})
	t.Run("the two at once", func(t *testing.T) {
		for range 50 {
			_, cancel := context.WithCancelCause(context.Background())
			h := &childHandle{id: "c", cancel: cancel}
			c := child()
			sent := make(chan struct{})
			go func() { h.signalClose(); close(sent) }()
			h.attachChild(c)
			<-sent
			if !signalled(c) {
				t.Fatal("a signal and an attach at once left the child unsignalled")
			}
		}
	})
}

// transcripts counts the session files under home.
func transcripts(t *testing.T, home string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".jsonl") {
			n++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return n
}

// TestParentCancelAbortsChildren (A7): the parent's cancel reaches both
// children mid-step through their contexts; both calls answer aborted, both
// children report cancelled with what they had streamed, and the turn ends
// cancelled. Nothing outlives it: each child's step had returned — its
// exited barrier closed — by the time Run did, and after Close the
// goroutines return to where they were.
func TestParentCancelAbortsChildren(t *testing.T) {
	f := newRouted(t)
	baseline := goroutines()
	s, err := Open(f.options())
	if err != nil {
		t.Fatal(err)
	}
	k := watchKids(s)
	a := f.routers["test/a"]
	w1, w2 := newWorker(), newWorker()
	a.route("fan out", callStep(agentPart(t, "a1", task("first", "child one")), agentPart(t, "a2", task("second", "child two"))))
	a.route("child one", w1.step(openText("partial one"), finishText()))
	a.route("child two", w2.step(openText("partial two"), finishText()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ev events
	out := start(ctx, s, "fan out", ev.sink)
	await(t, w1.reached, "the first child mid-step")
	await(t, w2.reached, "the second child mid-step")
	cancel()
	got := await(t, out, "the cancelled turn")
	for _, w := range []*worker{w1, w2} {
		if !isClosed(w.exited) {
			t.Fatal("a child's step was still running when the parent's turn returned")
		}
	}
	if got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	evs := ev.list()
	for callID, text := range map[string]string{"t1.1.1": "partial one", "t1.1.2": "partial two"} {
		if res := callResult(t, evs, callID); res.Class != tool.ClassAborted || res.Text != tool.AbortedText {
			t.Fatalf("call %s = %+v; want aborted", callID, res)
		}
		st := of[SubagentStarted](evs)[slices.IndexFunc(of[SubagentStarted](evs), func(e SubagentStarted) bool { return e.CallID == callID })]
		if fin := finishedOf(t, evs, st.ID); fin.Status != SubagentCancelled || fin.Text != text {
			t.Fatalf("child of %s finished %+v; want cancelled with %q", callID, fin, text)
		}
	}
	for _, c := range k.all() {
		if !storeClosed(c.store) {
			t.Fatalf("child %s's store is still open", c.ID())
		}
	}
	settled(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return goroutines() <= baseline }, fmt.Sprintf("the goroutines to return to %d", baseline))
}

// TestCloseJoinsChildren (A7): Close with two children mid-step returns only
// once both have run out and closed: their steps have returned, their stores
// refuse writes, their lifecycle is reported finished, their slots are free
// and the registry is sealed. The parent's turn ends cancelled.
func TestCloseJoinsChildren(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	k := watchKids(s)
	a := f.routers["test/a"]
	w1, w2 := newWorker(), newWorker()
	a.route("fan out", callStep(agentPart(t, "a1", task("first", "child one")), agentPart(t, "a2", task("second", "child two"))))
	a.route("child one", w1.step(openText("one"), finishText()))
	a.route("child two", w2.step(openText("two"), finishText()))
	var ev events
	out := start(context.Background(), s, "fan out", ev.sink)
	await(t, w1.reached, "the first child mid-step")
	await(t, w2.reached, "the second child mid-step")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, w := range []*worker{w1, w2} {
		if !isClosed(w.exited) {
			t.Fatal("Close returned while a child's step still ran")
		}
	}
	for _, c := range k.all() {
		if !storeClosed(c.store) {
			t.Fatalf("Close returned with child %s's store open", c.ID())
		}
	}
	if fin := of[SubagentFinished](ev.list()); len(fin) != 2 || fin[0].Status != SubagentCancelled || fin[1].Status != SubagentCancelled {
		t.Fatalf("Close returned with the children finished %+v; want both cancelled", fin)
	}
	if !s.subs.sealed {
		t.Fatal("the registry is not sealed after Close")
	}
	settled(t, s)
	if got := await(t, out, "the closed turn"); got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
}

// TestCloseMidFanOutSettles (A7): Close while four children hold every slot
// and a fifth call waits for one settles all five — the waiter aborted
// without opening a child — and leaves nothing running.
func TestCloseMidFanOutSettles(t *testing.T) {
	f := newRouted(t)
	baseline := goroutines()
	s, err := Open(f.options())
	if err != nil {
		t.Fatal(err)
	}
	k := watchKids(s)
	a := f.routers["test/a"]
	pool, reached := workers(a, "hold", 5)
	var parts [][]fantasy.StreamPart
	for i := 1; i <= 5; i++ {
		parts = append(parts, agentPart(t, fmt.Sprintf("a%d", i), task("hold", fmt.Sprintf("hold %d", i))))
	}
	a.route("five", callStep(parts...))
	waiting := make(chan struct{}, 5)
	s.subs.seams.waiting = func(tool.SubagentCall) { waiting <- struct{}{} }
	var ev events
	out := start(context.Background(), s, "five", ev.sink)
	for range 4 {
		await(t, reached, "a child holding a slot")
	}
	await(t, waiting, "the fifth call waiting")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got := await(t, out, "the closed turn")
	if got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	opened := k.all()
	if len(opened) != 4 {
		t.Fatalf("%d children opened; want 4", len(opened))
	}
	for _, c := range opened {
		if !storeClosed(c.store) {
			t.Fatalf("child %s's store is still open", c.ID())
		}
	}
	started := 0
	for _, w := range pool {
		if isClosed(w.reached) {
			started++
			if !isClosed(w.exited) {
				t.Fatal("a child's step outlived Close")
			}
		}
	}
	if started != 4 {
		t.Fatalf("%d children reached their provider; want 4", started)
	}
	for i := 1; i <= 5; i++ {
		if res := callResult(t, ev.list(), fmt.Sprintf("t1.1.%d", i)); res.Class != tool.ClassAborted {
			t.Fatalf("call %d = %+v; want aborted", i, res)
		}
	}
	settled(t, s)
	waitFor(t, func() bool { return goroutines() <= baseline }, fmt.Sprintf("the goroutines to return to %d", baseline))
}

// TestCloseSignalsChildClosingAfterCancel (A7, panel P3): a child runs a
// command that ignores SIGTERM. The parent's turn is cancelled ordinarily
// first, which reaches the child's context first and fixes its cause, so the
// command is owed its grace; then Close. Close signals the child's own
// closing channel before it joins, and the command is killed at once: Close
// returns well inside the grace. The control is the ordinary cancel alone,
// which waits the grace out — the grace is real for a child's command too.
//
// The command's output is drained before it opens a FIFO the test reads, so
// the read returning says the call is being supervised (its reader running),
// not still starting — where a cancel would kill it at once whatever the
// cause (TestCloseAfterACancelKillsAtOnce says why that matters). It is a
// barrier, not a poll.
func TestCloseSignalsChildClosingAfterCancel(t *testing.T) {
	stubborn := func(t *testing.T, f *routed) (ready <-chan error) {
		fifo := filepath.Join(t.TempDir(), "ready")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := "trap '' TERM; seq 1 40000; echo ok > " + fifo + "; sleep 30"
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("stubborn", "run the stubborn command"))))
		a.route("run the stubborn command", callStep(callParts("b1", "bash", input(t, map[string]any{"command": cmd}))))
		done := make(chan error, 1)
		go func() {
			_, err := os.ReadFile(fifo) // blocks until the command opens it to write
			done <- err
		}()
		return done
	}
	t.Run("control: an ordinary cancel alone waits out the grace", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		ready := stubborn(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, s, "go", nil)
		if err := await(t, ready, "the child's command, supervised"); err != nil {
			t.Fatal(err)
		}
		at := time.Now()
		cancel()
		got := await(t, out, "the cancelled turn")
		if took := time.Since(at); got.res.StopReason != StopCancelled || took < 2500*time.Millisecond {
			t.Fatalf("the cancelled turn returned %+v after %v; want cancelled after the ~3 s grace", got.res, took)
		}
	})
	t.Run("Close after the cancel", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		ready := stubborn(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var ev events
		out := start(ctx, s, "go", ev.sink)
		if err := await(t, ready, "the child's command, supervised"); err != nil {
			t.Fatal(err)
		}
		// The ordinary cancel propagates to the child's context as cancel
		// returns: its cause is fixed as an ordinary one before Close begins.
		cancel()
		at := time.Now()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(at); took > 2*time.Second {
			t.Fatalf("Close took %v; want the child's command killed at once, not after its grace", took)
		}
		if got := await(t, out, "the turn"); got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
		}
		if res := callResult(t, ev.list(), "t1.1.1"); res.Class != tool.ClassAborted {
			t.Fatalf("the call = %+v; want aborted", res)
		}
	})
}

// TestStopVersusParentCancelPrecedence (A7, panel P4): a stop by the user
// alone is not an error — the parent reads that the child was stopped and
// what it had got to, and its turn goes on — but the parent's cancel or close
// outranks a stop, in either order, and aborts the call; and a stop, or a
// parent's cancel, that lands after the child's final step was persisted
// keeps that step's outcome.
func TestStopVersusParentCancelPrecedence(t *testing.T) {
	t.Run("a stop alone", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		a := f.routers["test/a"]
		w := newWorker()
		a.route("go", callStep(agentPart(t, "a1", task("long", "a long task"))), answerWith("carried on"))
		a.route("a long task", w.step(openText("half done"), finishText()))
		var ev events
		out := start(context.Background(), s, "go", ev.sink)
		await(t, w.reached, "the child mid-step")
		if !stopChild(s, of[SubagentStarted](ev.list())[0].ID) {
			t.Fatal("the child was not registered")
		}
		got := await(t, out, "the turn")
		if got.err != nil || got.res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v; want the parent's turn to carry on to end_turn", got.res, got.err)
		}
		res := callResult(t, ev.list(), "t1.1.1")
		want := "The user stopped this sub-agent before it finished.\n\nIts last output was:\nhalf done"
		if res.Text != want || res.IsError || res.Child == nil {
			t.Fatalf("the call = %+v; want %q, not an error, with usage", res, want)
		}
		if fin := of[SubagentFinished](ev.list())[0]; fin.Status != SubagentCancelled {
			t.Fatalf("SubagentFinished = %+v; want cancelled", fin)
		}
	})
	for _, order := range []string{"stop, then the parent's cancel", "the parent's cancel, then a stop", "stop, then Close"} {
		t.Run(order, func(t *testing.T) {
			f := newRouted(t)
			s := f.open(f.options())
			a := f.routers["test/a"]
			w := newWorker()
			w.ignoreCancel = true // both causes land before the child sees either
			a.route("go", callStep(agentPart(t, "a1", task("long", "a long task"))))
			a.route("a long task", w.step(openText("half done"), finishText()))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ev events
			out := start(ctx, s, "go", ev.sink)
			await(t, w.reached, "the child mid-step")
			id := of[SubagentStarted](ev.list())[0].ID
			closed := make(chan error, 1)
			switch order {
			case "stop, then the parent's cancel":
				stopChild(s, id)
				cancel()
			case "the parent's cancel, then a stop":
				cancel()
				stopChild(s, id)
			case "stop, then Close":
				stopChild(s, id)
				go func() { closed <- s.Close() }()
				<-s.tools.closing
			}
			close(w.release)
			got := await(t, out, "the turn")
			if got.err != nil || got.res.StopReason != StopCancelled {
				t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
			}
			if res := callResult(t, ev.list(), "t1.1.1"); res.Class != tool.ClassAborted || res.Text != tool.AbortedText {
				t.Fatalf("the call = %+v; want aborted: the parent's cancel or close outranks a stop", res)
			}
			if order == "stop, then Close" {
				if err := await(t, closed, "Close"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, late := range []string{"a stop", "the parent's cancel"} {
		t.Run(late+" after the final step", func(t *testing.T) {
			f := newRouted(t)
			s := f.open(f.options())
			a := f.routers["test/a"]
			a.route("go", callStep(agentPart(t, "a1", task("quick", "a quick task"))), answerWith("next"))
			a.route("a quick task", answerWith("the final answer"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ev events
			landed := false
			sink := func(e Event) {
				ev.sink(e)
				// The child's final step is persisted, and its Run has not
				// returned: the StepDone is emitted inside it.
				if se, ok := e.(SubagentEvent); ok {
					if d, ok := se.Event.(StepDone); ok && d.Saved && d.StopReason == StopEndTurn {
						if late == "a stop" {
							landed = stopChild(s, se.ID)
						} else {
							cancel()
							landed = true
						}
					}
				}
			}
			got := await(t, start(ctx, s, "go", sink), "the turn")
			if !landed {
				t.Fatal("the late cause never landed")
			}
			if late == "a stop" && (got.err != nil || got.res.StopReason != StopEndTurn) {
				t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
			}
			res := callResult(t, ev.list(), "t1.1.1")
			if res.Text != "the final answer" || res.IsError {
				t.Fatalf("the call = %+v; want the persisted final answer, kept", res)
			}
			if fin := of[SubagentFinished](ev.list())[0]; fin.Status != SubagentCompleted {
				t.Fatalf("SubagentFinished = %+v; want completed", fin)
			}
		})
	}
}

// TestAgentResultTable (A8): what the parent's model reads for each way a
// child can end (§3.7's table), the two max_turn_requests rows told apart by
// the doom-loop guard's Diag. Every row carries the child's usage.
func TestAgentResultTable(t *testing.T) {
	longSteps := func(n int) []step {
		var out []step
		for i := range n {
			out = append(out, callStep(textParts(fmt.Sprintf("step %d", i)), bareCall(fmt.Sprintf("n%d", i), "nope", fmt.Sprintf(`{"n":%d}`, i))))
		}
		return out
	}
	doomSteps := func() []step {
		var out []step
		for i := range 6 {
			out = append(out, callStep(textParts(fmt.Sprintf("try %d", i+1)), bareCall(fmt.Sprintf("d%d", i), "glob", `{"pattern":"*.none"}`)))
		}
		return out
	}
	hungUp := errors.New("the provider hung up")
	failure := `The sub-agent failed: harness: provider error (provider "test", model "test/a"): the provider hung up.`
	cases := []struct {
		name   string
		steps  []step
		text   string
		isErr  bool
		status string
	}{
		{"end_turn", []step{answerWith("the answer")}, "the answer", false, SubagentCompleted},
		{"end_turn with no text", []step{reply(reasoningParts("hmm"), finish(fantasy.FinishReasonStop))},
			"The sub-agent finished without a final message.", false, SubagentCompleted},
		{"cut off", []step{reply(textParts("partial"), finish(fantasy.FinishReasonLength))},
			"partial\n\n(The sub-agent's output was cut off.)", false, SubagentCompleted},
		{"refusal", []step{reply(textParts("I will not"), finish(fantasy.FinishReasonContentFilter))},
			"I will not\n\n(The sub-agent's model refused to continue.)", false, SubagentCompleted},
		{"the doom-loop guard", doomSteps(),
			"try 5\n\n(The sub-agent was stopped for repeating the same tool call.)", false, SubagentCompleted},
		{"the step limit", longSteps(maxSteps + 1),
			fmt.Sprintf("step %d\n\n(The sub-agent reached its step limit.)", maxSteps-1), false, SubagentCompleted},
		{"a failure with last output", []step{callStep(textParts("looking"), globPart("g1")), reply(openText("half an answer"), errorPart(hungUp))},
			failure + "\n\nIts last output was:\nhalf an answer", true, SubagentFailed},
		{"a failure with nothing streamed since a finished step", []step{callStep(textParts("looking"), globPart("g1")), reply(errorPart(hungUp)), reply(errorPart(hungUp))},
			failure + "\n\nIts last output was:\nlooking", true, SubagentFailed},
		{"a failure with no output at all", []step{reply(errorPart(hungUp)), reply(errorPart(hungUp))},
			failure, true, SubagentFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouted(t)
			s := f.open(f.options())
			a := f.routers["test/a"]
			a.route("go", callStep(agentPart(t, "a1", task("the task", "do the task"))), answerWith("ok"))
			a.route("do the task", tc.steps...)
			var ev events
			if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v; want the parent to carry on", res, err)
			}
			res := callResult(t, ev.list(), "t1.1.1")
			class := tool.ErrorClass("")
			if tc.isErr {
				class = tool.ClassToolError
			}
			if res.Text != tc.text || res.IsError != tc.isErr || res.Class != class {
				t.Fatalf("the call = %q (error %v, class %q);\nwant %q (error %v, class %q)", res.Text, res.IsError, res.Class, tc.text, tc.isErr, class)
			}
			if res.Child == nil || res.Child.Model != "test/a" || res.Child.Provider != "test" || res.Child.WireModel != "wire-a" {
				t.Fatalf("the call's usage = %+v; want the child's, on test/a", res.Child)
			}
			if fin := of[SubagentFinished](ev.list())[0]; fin.Status != tc.status {
				t.Fatalf("SubagentFinished = %+v; want %s", fin, tc.status)
			}
		})
	}
}

// TestAgentResultTruncated (A8): a child's 60 KiB answer reaches the parent
// through the dispatcher's head truncation, with the whole of it in a spill
// file named for the call.
func TestAgentResultTruncated(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	a := f.routers["test/a"]
	long := strings.Repeat(strings.Repeat("x", 59)+"\n", 1024) // 60 KiB
	a.route("go", callStep(agentPart(t, "a1", task("long", "write a lot"))), answerWith("ok"))
	a.route("write a lot", answerWith(long))
	var ev events
	if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	res := callResult(t, ev.list(), "t1.1.1")
	if res.IsError || res.Trunc.Spill == "" || len(res.Text) > tool.MaxBytes+1024 || !strings.Contains(res.Text, "Full output saved to: "+res.Trunc.Spill) {
		t.Fatalf("the call: error %v, %d bytes, spill %q; want a head-truncated success naming its spill file", res.IsError, len(res.Text), res.Trunc.Spill)
	}
	if b, err := os.ReadFile(res.Trunc.Spill); err != nil || string(b) != long || filepath.Base(res.Trunc.Spill) != "tool_t1.1.1" {
		t.Fatalf("the spill file %s holds %d bytes (%v); want the whole answer", res.Trunc.Spill, len(b), err)
	}
}

// TestAgentErrorResultTruncated (A8, panel P8): a child that streams 60 KiB
// and then fails is an error the dispatcher would not cut; the agent tool
// cuts it through the same truncator, keeping its class and the child's
// usage, with the whole text — redacted — in a spill file.
func TestAgentErrorResultTruncated(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	a := f.routers["test/a"]
	long := strings.Repeat(strings.Repeat("y", 59)+"\n", 1024)
	a.route("go", callStep(agentPart(t, "a1", task("long", "fail after a lot"))), answerWith("ok"))
	a.route("fail after a lot", callStep(globPart("g1")), reply(openText(long), errorPart(errors.New("gone"))))
	var ev events
	if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	res := callResult(t, ev.list(), "t1.1.1")
	if !res.IsError || res.Class != tool.ClassToolError || res.Trunc.Spill == "" || len(res.Text) > tool.MaxBytes+1024 ||
		!strings.HasPrefix(res.Text, "The sub-agent failed: ") || !strings.Contains(res.Text, "Full output saved to: "+res.Trunc.Spill) {
		t.Fatalf("the call: error %v, class %q, %d bytes, spill %q; want a truncated tool_error naming its spill file",
			res.IsError, res.Class, len(res.Text), res.Trunc.Spill)
	}
	if res.Child == nil || res.Child.Usage != (tool.Usage{Input: 10, Output: 5, CacheRead: 4}) {
		t.Fatalf("the call's usage = %+v; want the one finished step's", res.Child)
	}
	b, err := os.ReadFile(res.Trunc.Spill)
	if err != nil || !strings.HasPrefix(string(b), "The sub-agent failed: ") || !strings.HasSuffix(string(b), "Its last output was:\n"+long) {
		t.Fatalf("the spill file holds %d bytes (%v); want the whole error text", len(b), err)
	}
}

// TestAgentUnknownModelOrEffort (§3.6): a call naming a model or an effort
// that does not resolve is invalid_input with the list to choose from, before
// any slot is taken or any child opened; so is an unknown type.
func TestAgentUnknownModelOrEffort(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	k := watchKids(s)
	var took atomic.Int32
	s.subs.seams.acquired = func(tool.SubagentCall) { took.Add(1) }
	s.subs.seams.waiting = func(tool.SubagentCall) { took.Add(1) }
	a := f.routers["test/a"]
	a.route("go", callStep(
		agentPart(t, "a1", task("m", "p", "model", "nope")),
		agentPart(t, "a2", task("e", "p", "effort", "ultra")),
		agentPart(t, "a3", task("t", "p", "subagent_type", "nobody"))), answerWith("ok"))
	var ev events
	if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	evs := ev.list()
	for id, prefix := range map[string]string{
		"t1.1.1": "Unknown model `nope`. Models: ",
		"t1.1.2": "Effort `ultra` is not offered by `test/a`. Its efforts: low, high.",
		"t1.1.3": "Unknown agent type `nobody`. Available types: general-purpose, explore, plan.",
	} {
		if res := callResult(t, evs, id); res.Class != tool.ClassInvalidInput || !strings.HasPrefix(res.Text, prefix) || res.Child != nil {
			t.Fatalf("call %s = %+v; want invalid_input starting %q", id, res, prefix)
		}
	}
	if took.Load() != 0 || len(k.all()) != 0 || len(of[SubagentStarted](evs)) != 0 {
		t.Fatalf("a refused call took a slot (%d) or opened a child (%d)", took.Load(), len(k.all()))
	}
	settled(t, s)
}

// TestSubagentEventOrder (A9, §3.9): for each child, SubagentStarted comes
// after the parent announced the call and before every event of the child;
// every one of the child's events comes before its SubagentFinished; and that
// comes before the parent's ToolFinished for the call. Two children, one held
// while the other runs to its end, so their events interleave.
func TestSubagentEventOrder(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	a := f.routers["test/a"]
	w := newWorker()
	a.route("fan out", callStep(agentPart(t, "a1", task("first", "child one")), agentPart(t, "a2", task("second", "child two"))),
		answerWith("done"))
	a.route("child one", callStep(textParts("looking"), globPart("g1")), w.step(openText("one "), finishText()))
	a.route("child two", callStep(textParts("searching"), globPart("g2")), answerWith("two"))
	var ev events
	sink := func(e Event) {
		ev.sink(e)
		if fin, ok := e.(SubagentFinished); ok && fin.Text == "two" {
			close(w.release) // the second is done: let the first go on
		}
	}
	if res, err := s.Run(context.Background(), "fan out", sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	evs := ev.list()
	for _, st := range of[SubagentStarted](evs) {
		at := func(match func(Event) bool) []int {
			var out []int
			for i, e := range evs {
				if match(e) {
					out = append(out, i)
				}
			}
			return out
		}
		called := at(func(e Event) bool { c, ok := e.(ToolCalled); return ok && c.ID == st.CallID })
		started := at(func(e Event) bool { c, ok := e.(SubagentStarted); return ok && c.ID == st.ID })
		inner := at(func(e Event) bool { c, ok := e.(SubagentEvent); return ok && c.ID == st.ID })
		fin := at(func(e Event) bool { c, ok := e.(SubagentFinished); return ok && c.ID == st.ID })
		tf := at(func(e Event) bool { c, ok := e.(ToolFinished); return ok && c.ID == st.CallID })
		// Each child's two steps: a text, a call's three events and a StepDone,
		// then a text and a StepDone.
		if len(called) != 1 || len(started) != 1 || len(fin) != 1 || len(tf) != 1 || len(inner) < 7 {
			t.Fatalf("child %s: called %v started %v events %d finished %v tool finished %v", st.ID, called, started, len(inner), fin, tf)
		}
		if ordered := called[0] < started[0] && started[0] < inner[0] && inner[len(inner)-1] < fin[0] && fin[0] < tf[0]; !ordered {
			t.Fatalf("child %s out of order: called %d, started %d, events %d..%d, finished %d, tool finished %d",
				st.ID, called[0], started[0], inner[0], inner[len(inner)-1], fin[0], tf[0])
		}
	}
	// The two children's events did interleave: the second finished while
	// the first was mid-step.
	one, two := startedWith(t, evs, "child one"), startedWith(t, evs, "child two")
	if slices.IndexFunc(evs, func(e Event) bool { f, ok := e.(SubagentFinished); return ok && f.ID == two.ID }) >
		slices.IndexFunc(evs, func(e Event) bool { f, ok := e.(SubagentFinished); return ok && f.ID == one.ID }) {
		t.Fatal("control: the first child finished before the second; the schedule did not interleave them")
	}
}

// TestChildEventsBypassTheParentTurnLock (§3.9's lock order): a child's own
// events go to the parent's sink under the child turn's lock and never the
// parent turn's. The parent's sink is held inside one of the parent's own
// events — the second child's SubagentStarted, which comes through the
// parent turn's lock — until the first child's next event arrives; that event
// arrives, concurrently. Forwarded through the parent turn's lock instead, it
// would wait behind the held sink for good.
func TestChildEventsBypassTheParentTurnLock(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	a := f.routers["test/a"]
	w := newWorker()
	a.route("fan out", callStep(agentPart(t, "a1", task("first", "child one")), agentPart(t, "a2", task("second", "child two"))),
		answerWith("done"))
	a.route("child one", w.step(nil, cat(textParts("after the release"), finish(fantasy.FinishReasonStop))))
	a.route("child two", answerWith("two"))
	oneStarted := make(chan struct{})
	// The second child opens only once the first has started, so its
	// SubagentStarted is the parent's event the sink is held in.
	s.subs.seams.open = func(o Options) (*Session, error) {
		if o.Child.ParentCall == "t1.1.2" {
			<-oneStarted
		}
		return Open(o)
	}
	var ev events
	var once sync.Once
	arrived := make(chan struct{})
	var held atomic.Bool
	sink := func(e Event) {
		ev.sink(e)
		switch e := e.(type) {
		case SubagentStarted:
			if e.Prompt == "child one" {
				close(oneStarted)
				return
			}
			// The parent turn's lock is held here: let the first child go on
			// and wait for its next event.
			held.Store(true)
			close(w.release)
			select {
			case <-arrived:
			case <-time.After(waitTimeout):
				t.Errorf("the first child's event did not arrive while the parent turn's lock was held")
			}
		case SubagentEvent:
			if d, ok := e.Event.(TextDelta); ok && d.Text == "after the release" && held.Load() {
				once.Do(func() { close(arrived) })
			}
		}
	}
	if res, err := s.Run(context.Background(), "fan out", sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !isClosed(arrived) {
		t.Fatal("control: the held sink never saw the first child's event")
	}
}

// TestChildUsageObservedOnEveryPath (A9, panel P6, P7, P40): a child's usage
// is what its StepDones reported, summed — so it counts on every path it can
// end by, whatever its Run returned — and the parent's step records it on its
// tool entry as subagent_usage, one row per model the step's children ran on,
// on a finished step and on one the runner synthesized alike, and on its
// StepDone even when the append failed and nothing was persisted.
func TestChildUsageObservedOnEveryPath(t *testing.T) {
	t.Run("steps, then the parent's cancel", func(t *testing.T) {
		// A cancelled child's Result.Usage is zero (turn.go's finish); the two
		// steps it was billed for are counted all the same.
		f := newRouted(t)
		s := f.open(f.options())
		a := f.routers["test/a"]
		w := newWorker()
		a.route("go", callStep(agentPart(t, "a1", task("spend", "spend tokens"))))
		a.route("spend tokens", callStep(globPart("g1")), callStep(globPart("g2")), w.step(openText("more"), finishText()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var ev events
		out := start(ctx, s, "go", ev.sink)
		await(t, w.reached, "the child's third step")
		cancel()
		await(t, out, "the turn")
		fin := of[SubagentFinished](ev.list())
		if len(fin) != 1 || fin[0].Status != SubagentCancelled || fin[0].Usage != oneStep(2) || fin[0].Steps != 2 || fin[0].ToolCalls != 2 {
			t.Fatalf("SubagentFinished = %+v; want cancelled with two steps' usage, two steps and two calls", fin)
		}
		if res := callResult(t, ev.list(), "t1.1.1"); res.Class != tool.ClassAborted || res.Child == nil ||
			res.Child.Usage != (tool.Usage{Input: 20, Output: 10, CacheRead: 8}) {
			t.Fatalf("the call = %+v; want aborted, carrying the two steps' usage", res)
		}
	})
	t.Run("a provider failure", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("spend", "spend tokens"))), answerWith("ok"))
		a.route("spend tokens", callStep(globPart("g1")), reply(openText("x"), errorPart(errors.New("down"))))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
			t.Fatal(err)
		}
		if fin := of[SubagentFinished](ev.list())[0]; fin.Status != SubagentFailed || fin.Usage != oneStep(1) {
			t.Fatalf("SubagentFinished = %+v; want failed with one step's usage", fin)
		}
	})
	t.Run("bad call ids", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("spend", "spend tokens"))), answerWith("ok"))
		a.route("spend tokens", callStep(globPart("g1")), callStep(globPart("dup"), globPart("dup")))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
			t.Fatal(err)
		}
		fin := of[SubagentFinished](ev.list())[0]
		if fin.Status != SubagentFailed || fin.Usage != oneStep(2) || !strings.Contains(fin.Error, ErrBadToolCalls.Error()) {
			t.Fatalf("SubagentFinished = %+v; want failed on bad ids with both steps' usage", fin)
		}
	})
	t.Run("a save failure", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		k := watchKids(s)
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("spend", "spend tokens"))), answerWith("ok"))
		a.route("spend tokens", callStep(globPart("g1")), answerWith("lost"))
		var ev events
		sink := func(e Event) {
			ev.sink(e)
			if se, ok := e.(SubagentEvent); ok {
				if d, ok := se.Event.(TextDelta); ok && d.Text == "lost" {
					_ = k.get(se.ID).store.Close() // the child's answer will not save
				}
			}
		}
		if _, err := s.Run(context.Background(), "go", sink); err != nil {
			t.Fatal(err)
		}
		fin := of[SubagentFinished](ev.list())[0]
		if fin.Status != SubagentFailed || fin.Usage != oneStep(2) || !strings.Contains(fin.Error, "saving the answer") {
			t.Fatalf("SubagentFinished = %+v; want failed on the save with both steps' usage", fin)
		}
	})

	t.Run("persisted per model on the step's tool entry", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		a := f.routers["test/a"]
		a.route("go", callStep(
			agentPart(t, "a1", task("b one", "on b, one", "model", "test/b")),
			agentPart(t, "a2", task("c", "on c", "model", "other/c")),
			agentPart(t, "a3", task("b two", "on b, two", "model", "test/b"))), answerWith("ok"))
		f.routers["test/b"].route("on b, one", callStep(globPart("g1")), answerWith("b1"))
		f.routers["test/b"].route("on b, two", answerWith("b2"))
		f.routers["other/c"].route("on c", answerWith("c"))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
			t.Fatal(err)
		}
		rows := []ModelUsage{
			{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: oneStep(3)},
			{Provider: "other", Model: "other/c", WireModel: "wire-c", Usage: oneStep(1)},
		}
		tr := transcript(t, s)
		toolEntry := tr.Entries[2]
		if toolEntry.Message.Role != fantasy.MessageRoleTool || !reflect.DeepEqual(toolEntry.SubagentUsage, rows) || toolEntry.Usage != nil {
			t.Fatalf("the step's tool entry carries rows %+v and usage %+v; want %+v and none", toolEntry.SubagentUsage, toolEntry.Usage, rows)
		}
		if asst := tr.Entries[1]; asst.Usage == nil || *asst.Usage != oneStep(1) || asst.Model.Alias != "test/a" || asst.SubagentUsage != nil {
			t.Fatalf("the step's assistant entry = %+v; want the parent's own usage on test/a and no rows", asst.MessageEntry)
		}
		for _, e := range tr.Entries[3:] {
			if e.SubagentUsage != nil {
				t.Fatalf("entry %s carries rows; only the agent step's tool entry should", e.ID)
			}
		}
		if d := of[StepDone](ev.list()); !reflect.DeepEqual(d[0].SubagentUsage, rows) || d[1].SubagentUsage != nil {
			t.Fatalf("the StepDones' rows = %+v, %+v; want the step's rows, then none", d[0].SubagentUsage, d[1].SubagentUsage)
		}
	})

	t.Run("persisted on the synthesized entry", func(t *testing.T) {
		// A Fantasy that returns without finishing a step whose agent call ran
		// (TestSynthesisDefence's course): the runner writes the step itself,
		// and the child's usage with its result.
		f := newRouted(t)
		s := f.open(f.options())
		f.routers["test/a"].route("spend tokens", callStep(globPart("g1")), answerWith("spent"))
		in := input(t, task("spend", "spend tokens"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.newAgent = func(_ fantasy.LanguageModel, _ string, tools []fantasy.AgentTool) fantasy.Agent {
			return fakeAgent{tools: tools, run: func(ctx context.Context, tools []fantasy.AgentTool, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				_ = c.OnStepStart(0)
				_ = c.OnToolInputStart("a1", "agent")
				_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "a1", ToolName: "agent", Input: in})
				i := slices.IndexFunc(tools, func(tl fantasy.AgentTool) bool { return tl.Info().Name == "agent" })
				resp, _ := tools[i].Run(ctx, fantasy.ToolCall{ID: "a1", Name: "agent", Input: in})
				_ = c.OnToolResult(fantasy.ToolResultContent{ToolCallID: "a1", ToolName: "agent",
					Result: fantasy.ToolResultOutputContentText{Text: resp.Content}, ClientMetadata: resp.Metadata})
				cancel()
				return nil, ctx.Err()
			}}
		}
		var ev events
		if res, err := s.Run(ctx, "go", ev.sink); err != nil || res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", res, err)
		}
		if len(of[Diag](ev.list())) == 0 || of[Diag](ev.list())[0].Kind != DiagSynthesized {
			t.Fatal("control: the step was not synthesized")
		}
		tr := transcript(t, s)
		toolEntry := tr.Entries[len(tr.Entries)-1]
		rows := []ModelUsage{{Provider: "test", Model: "test/a", WireModel: "wire-a", Usage: oneStep(2)}}
		if !toolEntry.Interrupted || !reflect.DeepEqual(toolEntry.SubagentUsage, rows) || toolEntry.Usage != nil {
			t.Fatalf("the synthesized tool entry: interrupted %v, rows %+v, usage %+v; want interrupted with %+v",
				toolEntry.Interrupted, toolEntry.SubagentUsage, toolEntry.Usage, rows)
		}
	})

	t.Run("a failed append reports it on StepDone and persists it nowhere", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("spend", "spend tokens"))))
		a.route("spend tokens", answerWith("spent"))
		var ev events
		sink := func(e Event) {
			ev.sink(e)
			if _, ok := e.(SubagentFinished); ok {
				_ = s.store.Close() // the parent's step will not save
			}
		}
		if _, err := s.Run(context.Background(), "go", sink); !errors.Is(err, store.ErrClosed) {
			t.Fatalf("Run = %v; want the parent's save failure", err)
		}
		d := of[StepDone](ev.list())
		rows := []ModelUsage{{Provider: "test", Model: "test/a", WireModel: "wire-a", Usage: oneStep(1)}}
		if len(d) != 1 || d[0].Saved || d[0].SaveError == "" || !reflect.DeepEqual(d[0].SubagentUsage, rows) {
			t.Fatalf("the parent's StepDone = %+v; want unsaved, with the rows %+v", d, rows)
		}
		noTranscript(t, s)
	})
}

// TestTwoChildrenEditOneFile (A9, §3.2, panel P31): two children edit one
// file at once, each its own line; both are held until both are mid-step and
// then let go together. They share the parent's path-lock table — the same
// table, not two — so the read-modify-write of each edit is serialized and
// neither edit is lost.
func TestTwoChildrenEditOneFile(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	k := watchKids(s)
	shared := f.put("shared.txt", "one\ntwo\n")
	a := f.routers["test/a"]
	edit := func(id, from, to string) []fantasy.StreamPart {
		return callParts(id, "edit", input(t, map[string]any{"filePath": shared, "oldString": from, "newString": to}))
	}
	w1, w2 := newWorker(), newWorker()
	a.route("fan out", callStep(agentPart(t, "a1", task("first", "edit line one")), agentPart(t, "a2", task("second", "edit line two"))),
		answerWith("done"))
	a.route("edit line one", w1.step(nil, cat(edit("e1", "one", "ONE"), finish(fantasy.FinishReasonToolCalls))), answerWith("edited one"))
	a.route("edit line two", w2.step(nil, cat(edit("e2", "two", "TWO"), finish(fantasy.FinishReasonToolCalls))), answerWith("edited two"))
	out := start(context.Background(), s, "fan out", nil)
	await(t, w1.reached, "the first child")
	await(t, w2.reached, "the second child")
	for _, c := range k.all() {
		if c.tools.locks != s.tools.locks {
			t.Fatalf("child %s has a path-lock table of its own", c.ID())
		}
	}
	close(w1.release)
	close(w2.release)
	if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", got.res, got.err)
	}
	if b, err := os.ReadFile(shared); err != nil || string(b) != "ONE\nTWO\n" {
		t.Fatalf("the file holds %q (%v); want both edits", b, err)
	}
}

// TestSubagentCanaryRedaction (A9, panel P9, P38; the harness half): a key in
// an agent call's description and prompt reaches neither the child's request
// nor its transcript nor any lifecycle event — the runner redacts the prompt
// before the child's Run and every string it reports — and a child's text is
// forwarded exactly under the parent's own contract: raw, a key split across
// two chunks included, as the parent's own TextDelta carries it. What the
// runner assembles from that text — SubagentFinished and the call's result —
// is redacted.
func TestSubagentCanaryRedaction(t *testing.T) {
	f := newRouted(t)
	s := f.open(f.options())
	k := watchKids(s)
	a := f.routers["test/a"]
	prompt := "use the key " + canary + " to check"
	sent := s.Redact(prompt)
	if sent == prompt || !strings.Contains(sent, redact.Marker) {
		t.Fatalf("control: the session does not redact the canary: %q", sent)
	}
	half := len(canary) / 2
	a.route("go", callStep(
		agentPart(t, "a1", task("scan "+canary, prompt)),
		agentPart(t, "a2", task("echo", "say the key"))),
		reply(textParts("the key is "+canary[:half], canary[half:]+"."), finish(fantasy.FinishReasonStop)))
	a.route(sent, answerWith("checked"))
	a.route("say the key", reply(textParts("the key is "+canary[:half], canary[half:]+"."), finish(fantasy.FinishReasonStop)))
	var ev events
	if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	evs := ev.list()
	for _, c := range a.requests(sent) {
		if strings.Contains(requestText(c, true), canary) || !strings.Contains(systemText(c), childRoleHeading) {
			t.Fatal("the child's request holds the canary, or is not a child's")
		}
	}
	checker := startedWith(t, evs, sent)
	b, err := os.ReadFile(k.get(checker.ID).store.Path())
	if err != nil || strings.Contains(string(b), canary) || !strings.Contains(string(b), redact.Marker) {
		t.Fatalf("the child's transcript (%v) holds the canary, or not the redacted prompt", err)
	}
	for _, e := range evs {
		switch e := e.(type) {
		case SubagentStarted, SubagentFinished, ToolFinished:
			if found := leaks(e, canary); len(found) != 0 {
				t.Fatalf("%T leaks the canary at %v", e, found)
			}
		}
	}
	echo := startedWith(t, evs, "say the key")
	var childText, parentText []string
	for _, e := range evs {
		switch e := e.(type) {
		case SubagentEvent:
			if d, ok := e.Event.(TextDelta); ok && e.ID == echo.ID {
				childText = append(childText, d.Text)
			}
		case TextDelta:
			parentText = append(parentText, e.Text)
		}
	}
	if !slices.Equal(childText, parentText) || !strings.Contains(strings.Join(childText, ""), canary) {
		t.Fatalf("the child's text %q and the parent's %q; want the same raw deltas under one contract", childText, parentText)
	}
	if fin := finishedOf(t, evs, echo.ID); !strings.Contains(fin.Text, redact.Marker) {
		t.Fatalf("SubagentFinished.Text = %q; want the key redacted", fin.Text)
	}
}

// TestChildGateTightensWithParentMode (A3, panel P13, P37, P50): the parent
// switches mode while its child waits for its provider, and back again, with
// no check of the child's in between: the child's next calls are judged
// under the stricter mode all the same, and the switch back loosens nothing.
// Ask stops the child's command and its edit; plan stops only its edit. The
// control is no switch: both run. A child registered after a switch opens in
// the new mode.
func TestChildGateTightensWithParentMode(t *testing.T) {
	const (
		askSwitched  = "Rejected: no edits, writes, or shell commands — the agent that started you switched to ask mode, which is read-only."
		planSwitched = "Rejected: file edits are not allowed — the agent that started you switched to plan mode."
	)
	for _, tc := range []struct {
		name            string
		via             string // the mode switched to and back from; "" for none
		bashErr, editOK string
	}{
		{"agent to ask to agent", "ask", askSwitched, ""},
		{"agent to plan to agent", "plan", "", ""},
		{"control: no switch", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouted(t)
			s := f.open(f.options())
			target := f.put("notes.txt", "alpha\n")
			a := f.routers["test/a"]
			w := newWorker()
			a.route("go", callStep(agentPart(t, "a1", task("work", "change things"))), answerWith("ok"))
			a.route("change things",
				w.step(nil, cat(callParts("b1", "bash", input(t, map[string]any{"command": "echo ran"})),
					callParts("e1", "edit", input(t, map[string]any{"filePath": target, "oldString": "alpha", "newString": "beta"})),
					finish(fantasy.FinishReasonToolCalls))),
				answerWith("done"))
			var ev events
			out := start(context.Background(), s, "go", ev.sink)
			await(t, w.reached, "the child waiting for its provider")
			if tc.via != "" {
				if err := s.SetMode(tc.via); err != nil {
					t.Fatal(err)
				}
				if err := s.SetMode("agent"); err != nil {
					t.Fatal(err)
				}
			}
			close(w.release)
			if got := await(t, out, "the turn"); got.err != nil {
				t.Fatal(got.err)
			}
			results := map[string]tool.Result{}
			for _, e := range ev.list() {
				if se, ok := e.(SubagentEvent); ok {
					if fin, ok := se.Event.(ToolFinished); ok {
						results[fin.ID] = fin.Result
					}
				}
			}
			bash, edit := results["t1.1.1"], results["t1.1.2"]
			switch tc.via {
			case "ask":
				if bash.Text != askSwitched || edit.Text != askSwitched || bash.Class != tool.ClassDenied {
					t.Fatalf("bash %+v, edit %+v; want both denied: the parent switched to ask", bash, edit)
				}
			case "plan":
				if bash.IsError || edit.Text != planSwitched {
					t.Fatalf("bash %+v, edit %+v; want bash to run and the edit denied: the parent switched to plan", bash, edit)
				}
			default:
				if bash.IsError || edit.IsError {
					t.Fatalf("control: bash %+v, edit %+v; want both to run", bash, edit)
				}
			}
			if b, _ := os.ReadFile(target); (string(b) == "beta\n") != (tc.via == "") {
				t.Fatalf("the file holds %q", b)
			}
		})
	}
	t.Run("a child registered after a switch opens in the new mode", func(t *testing.T) {
		f := newRouted(t)
		s := f.open(f.options())
		target := f.put("notes.txt", "alpha\n")
		if err := s.SetMode("plan"); err != nil {
			t.Fatal(err)
		}
		a := f.routers["test/a"]
		a.route("go", callStep(agentPart(t, "a1", task("work", "change things"))), answerWith("ok"))
		a.route("change things",
			callStep(callParts("e1", "edit", input(t, map[string]any{"filePath": target, "oldString": "alpha", "newString": "beta"}))),
			answerWith("done"))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
			t.Fatal(err)
		}
		if st := of[SubagentStarted](ev.list())[0]; st.Mode != "plan" {
			t.Fatalf("the child opened in %q; want plan", st.Mode)
		}
		for _, e := range ev.list() {
			if se, ok := e.(SubagentEvent); ok {
				if fin, ok := se.Event.(ToolFinished); ok && fin.Result.Text != "Rejected: file edits are not allowed — the agent that started you is in plan mode." {
					t.Fatalf("the child's edit = %+v; want the child's own plan-mode denial", fin.Result)
				}
			}
		}
	})
}

// TestSubagentRunnerRefusesInsideAChild (§3.2, §3.8): depth is 1. A child has
// no runner and no agent tool, and a runner over a child's session refuses a
// call before anything else.
func TestSubagentRunnerRefusesInsideAChild(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	parent := f.open(f.options())
	child := f.open(childOf(f, parent, ChildOptions{ID: "child-0001", AllTools: true, Mode: "agent"}))
	if child.subs != nil || slices.Contains(specIDs(child), "agent") || parent.subs == nil {
		t.Fatalf("child runner %v, child tools %v; want no runner and no agent tool (the parent's runner: %v)",
			child.subs, specIDs(child), parent.subs)
	}
	res := newSubagents(child).Run(context.Background(), tool.SubagentCall{ID: "t1.1.1", Description: "d", Prompt: "p", Type: "general-purpose"})
	if res.Text != subagentNested || res.Class != tool.ClassToolError {
		t.Fatalf("a runner over a child = %+v; want the depth refusal", res)
	}
}

// TestSubagentEventsRoundTrip: the lifecycle events are plain data, as every
// other event is, and survive a JSON round trip — the journal records what the
// sink was handed. SubagentEvent carries another event in an interface, which
// encodes as that event; the control shows it decodes to it too.
func TestSubagentEventsRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 123456789, time.UTC)
	for _, ev := range []Event{
		SubagentStarted{ID: "c1", CallID: "t1.1.1", Type: "explore", Description: "d", Prompt: "p", Model: "test/a", Effort: "high", Mode: "plan", At: at},
		SubagentFinished{ID: "c1", Status: SubagentFailed, Error: "e", Text: "t", Usage: Usage{Input: 1, Output: 2, Reasoning: 3, CacheRead: 4, CacheCreation: 5},
			Model: "test/a", Provider: "test", WireModel: "wire-a", ToolCalls: 2, Steps: 3, Duration: time.Second, At: at},
		StepDone{Step: 1, SubagentUsage: []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: Usage{Input: 7}}}},
	} {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("%T does not encode: %v", ev, err)
		}
		back := reflect.New(reflect.TypeOf(ev))
		if err := json.Unmarshal(b, back.Interface()); err != nil {
			t.Fatalf("%T does not decode: %v", ev, err)
		}
		if got := back.Elem().Interface(); !reflect.DeepEqual(got, ev) {
			t.Errorf("%T round-tripped to\n%#v\nwant\n%#v", ev, got, ev)
		}
	}
	b, err := json.Marshal(SubagentEvent{ID: "c1", Event: TextDelta{Text: "hi"}})
	if err != nil || string(b) != `{"ID":"c1","Event":{"Text":"hi"}}` {
		t.Fatalf("SubagentEvent encodes as %s (%v)", b, err)
	}
	var back struct {
		ID    string
		Event TextDelta
	}
	if err := json.Unmarshal(b, &back); err != nil || back.Event.Text != "hi" {
		t.Fatalf("control: the wrapped event does not decode as itself: %+v (%v)", back, err)
	}
}
