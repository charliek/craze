package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native adapter's sub-agents (plan 026 §3.9, §7 A9's adapter half and
// A11). Two kinds of case are here. The real ones run a native session whose
// parent calls the agent tool: the harness's runner opens real child sessions,
// and every alias's model is one routing scripted model (nativeRouter),
// shared by the parent and its children and routed by each request's first
// user message, which is the prompt of the turn that sent it — so children
// streaming at once never take each other's steps (internal/harness's router,
// kept small here). The synthetic ones drive the adapter's sink with the
// three harness events directly, for the schedules only a sink can be handed
// on purpose: a Task-only stamp, an enqueued payload mutated behind the
// outbox, thirty-odd children finishing at one clock reading.
//
// Every schedule is forced with a barrier — a held scripted step, the log's
// own hooks, the cancel's seam — and never a sleep; the one poll (waitFor)
// reads a goroutine's stack, a positive signal, never the passing of time.

// nativeRouter is a fantasy.LanguageModel that answers each request with the
// next step queued for its turn's prompt: the first user message's text.
type nativeRouter struct {
	provider, wire string

	mu     sync.Mutex
	queues map[string][]step
}

// route queues steps for the turn whose prompt is prompt.
func (m *nativeRouter) route(prompt string, steps ...step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues[prompt] = append(m.queues[prompt], steps...)
}

func (m *nativeRouter) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	key := firstUserPrompt(call)
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[key]
	if len(q) == 0 {
		return nil, fmt.Errorf("router: no step queued for %q", key)
	}
	next := q[0]
	m.queues[key] = q[1:]
	return func(yield func(fantasy.StreamPart) bool) { next(ctx, yield) }, nil
}

func (m *nativeRouter) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("router: Generate is not used")
}

func (m *nativeRouter) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("router: GenerateObject is not used")
}

func (m *nativeRouter) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("router: StreamObject is not used")
}

func (m *nativeRouter) Provider() string { return m.provider }
func (m *nativeRouter) Model() string    { return m.wire }

// firstUserPrompt is the text of a request's first user message: the prompt
// of the turn that sent it (a mode's reminder follows it, a steer follows a
// step, so neither is ever first).
func firstUserPrompt(call fantasy.Call) string {
	for _, msg := range call.Prompt {
		if msg.Role == fantasy.MessageRoleUser {
			return userTextOf(msg)
		}
	}
	return ""
}

// routedNative is a native fixture whose every alias is served by a router.
func routedNative(t *testing.T) (*nativeFixture, map[string]*nativeRouter) {
	t.Helper()
	f := newNativeFixture(t)
	routers := map[string]*nativeRouter{}
	for alias, m := range nativeTestTable("").Models {
		routers[alias] = &nativeRouter{provider: m.Provider, wire: m.WireModel,
			queues: map[string][]step{}}
	}
	f.edit = func(o *harness.Options) {
		o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) { return routers[r.Alias], nil }
	}
	return f, routers
}

// agentCall is one agent call in a parent's step: a description, a prompt and
// any more keys and values after them.
func agentCall(t *testing.T, id, description, prompt string, more ...string) []fantasy.StreamPart {
	t.Helper()
	args := map[string]any{"description": description, "prompt": prompt}
	for i := 0; i+1 < len(more); i += 2 {
		args[more[i]] = more[i+1]
	}
	return nativeCallParts(id, tool.AgentTool, nativeArgs(t, args))
}

// callsStep is one step making every call in calls and finishing "tool-calls".
func callsStep(calls ...[]fantasy.StreamPart) step {
	return reply(append(calls, finishParts(fantasy.FinishReasonToolCalls))...)
}

// openTextParts opens a text block and streams chunks into it, leaving it
// open: what a held step yields before it waits.
func openTextParts(chunks ...string) []fantasy.StreamPart {
	parts := textParts(chunks...)
	return parts[:len(parts)-1]
}

// closeTextParts streams chunks into an openTextParts block, ends it and ends
// the step: what a held step yields once released.
func closeTextParts(chunks ...string) []fantasy.StreamPart {
	var parts []fantasy.StreamPart
	for _, c := range chunks {
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: c})
	}
	parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "0"})
	return append(parts, finishParts(fantasy.FinishReasonStop)...)
}

// readStep is a step that reads one file of the workspace: a real tool, with
// no ripgrep behind it.
func readStep(id, path string) step {
	return nativeCallStep(id, "read", `{"filePath":"`+path+`"}`)
}

// subagentEvents are the roster events of one child, in order.
func subagentEvents(evs []Event, id string) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == id {
			out = append(out, ev)
		}
	}
	return out
}

// spawnedWith is the id of the child whose spawned event carries prompt.
func spawnedWith(t *testing.T, evs []Event, prompt string) string {
	t.Helper()
	for _, ev := range evs {
		if ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeSpawned && ev.Subagent.Prompt == prompt {
			return ev.Subagent.ID
		}
	}
	t.Fatalf("no spawned event for the prompt %q", prompt)
	return ""
}

// indexWhere is the index of the first event pred accepts at or after from,
// or -1.
func indexWhere(evs []Event, from int, pred func(Event) bool) int {
	for i := from; i < len(evs); i++ {
		if pred(evs[i]) {
			return i
		}
	}
	return -1
}

// lastWhere is the index of the last event pred accepts, or -1.
func lastWhere(evs []Event, pred func(Event) bool) int {
	for i := len(evs) - 1; i >= 0; i-- {
		if pred(evs[i]) {
			return i
		}
	}
	return -1
}

// ascending reports whether every index was found and each is past the one
// before it.
func ascending(idx ...int) bool {
	for i, n := range idx {
		if n < 0 || (i > 0 && n <= idx[i-1]) {
			return false
		}
	}
	return true
}

// isRoster reports whether ev is child id's roster event of change.
func isRoster(ev Event, id, change string) bool {
	return ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == id && ev.SubagentChange == change
}

// isParentRowDone reports whether ev is the parent's row for call, terminal.
func isParentRowDone(ev Event, call string) bool {
	return ev.Type == EventTool && ev.Agent == "" && ev.Tool != nil && ev.Tool.ID == call && !toolStatusInFlight(ev.Tool.Status)
}

// TestNativeSubagentPublishOrder (A11): two children fanned out in one step,
// each reading a file and answering, and for each one the published order is
// spawned → its user line (the task) → the agent row's stamp → its own events →
// finished → the parent's terminal row for the call; every progress event of
// it lies between spawned and finished. What the roster, the agent rows and
// the snapshot end up holding is the lifecycle the harness reported.
func TestNativeSubagentPublishOrder(t *testing.T) {
	f, r := routedNative(t)
	ws := nativeWorkspaceWith(t, map[string]string{"main.go": "package main\n", "notes.txt": "alpha\n"})
	s := f.started(Options{Workspace: ws})
	w := newNativeWatcher(t, s)
	a := r["test/a"]
	a.route("fan out", callsStep(agentCall(t, "a1", "first", "child one"), agentCall(t, "a2", "second", "child two")),
		answer("both done"))
	a.route("child one", readStep("r1", "main.go"), answer("one ", "done"))
	a.route("child two", readStep("r2", "notes.txt"), answer("two done"))

	if _, err := s.Prompt(context.Background(), "fan out"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	evs := w.events()

	for _, c := range []struct{ prompt, call, answer, read string }{
		{"child one", "t1.1.1", "one done", "main.go"},
		{"child two", "t1.1.2", "two done", "notes.txt"},
	} {
		id := spawnedWith(t, evs, c.prompt)
		spawned := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, id, SubagentChangeSpawned) })
		user := indexWhere(evs, 0, func(ev Event) bool { return ev.Type == EventUser && ev.Agent == id })
		stamp := indexWhere(evs, 0, func(ev Event) bool {
			return ev.Type == EventTool && ev.Agent == "" && ev.Tool.ID == c.call && ev.Tool.Task != nil && ev.Tool.Task.AgentID == id
		})
		first := indexWhere(evs, 0, func(ev Event) bool { return ev.Agent == id && ev.Type != EventUser })
		last := lastWhere(evs, func(ev Event) bool { return ev.Agent == id })
		finished := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, id, SubagentChangeFinished) })
		done := indexWhere(evs, 0, func(ev Event) bool { return isParentRowDone(ev, c.call) })
		if !ascending(spawned, user, stamp, first) || !ascending(last, finished, done) {
			t.Fatalf("%s: spawned %d, user %d, stamp %d, first child event %d, last %d, finished %d, the call's row done %d; want them in that order",
				c.prompt, spawned, user, stamp, first, last, finished, done)
		}
		if got := evs[user].Text; got != c.prompt {
			t.Fatalf("%s: the child's user line is %q, want its task", c.prompt, got)
		}
		progress := 0
		for i, ev := range evs {
			if isRoster(ev, id, SubagentChangeProgress) {
				progress++
				if i < spawned || i > finished {
					t.Fatalf("%s: a progress event at %d, outside spawned %d .. finished %d", c.prompt, i, spawned, finished)
				}
			}
		}
		// The read's ToolCalled and each of the two steps' usage.
		if progress != 3 {
			t.Fatalf("%s: %d progress events, want 3 (a tool call and two steps' tokens)", c.prompt, progress)
		}

		fin := *evs[finished].Subagent
		if fin.Status != SubagentCompleted || fin.Output != c.answer || fin.ToolCalls != 1 || fin.Turns != 2 ||
			fin.TokensUsed != 30 || !slices.Equal(fin.ToolsUsed, []string{"read"}) || fin.Activity != c.read ||
			fin.ToolCallID != c.call || fin.SubagentType != tool.DefaultAgentType || fin.Model != "test/a" ||
			fin.Description == "" || !fin.Transcript || fin.EndedAt.IsZero() || fin.StartedAt.IsZero() {
			t.Fatalf("%s: the finished row is %+v", c.prompt, fin)
		}
		row := evs[lastWhere(evs, func(ev Event) bool { return ev.Type == EventTool && ev.Agent == "" && ev.Tool.ID == c.call })].Tool
		if row.Kind != string(tool.KindTask) || row.Task == nil || row.Task.AgentID != id || row.Task.Model != "test/a" ||
			row.Task.Status != SubagentCompleted || !row.Task.Receipt || row.Task.Prompt != c.prompt ||
			row.Task.SubagentType != tool.DefaultAgentType || row.Task.DurationMs != fin.DurationMs {
			t.Fatalf("%s: the agent row ended as %+v (task %+v)", c.prompt, row, row.Task)
		}
		// The child's own read row went out tagged with it, and only there.
		reads := 0
		for _, ev := range evs {
			if ev.Type == EventTool && ev.Tool.Title == c.read {
				if ev.Agent != id {
					t.Fatalf("%s: its read row went out tagged %q", c.prompt, ev.Agent)
				}
				reads++
			}
		}
		if reads == 0 {
			t.Fatalf("%s: its read row was never published", c.prompt)
		}
	}

	snap := s.Snapshot()
	if len(snap.Subagents) != 2 {
		t.Fatalf("the roster holds %d rows, want the two children", len(snap.Subagents))
	}
	for _, row := range snap.Subagents {
		// Every roster field the snapshot shows arrived in an event: the last
		// payload for the id is the row.
		evs := subagentEvents(evs, row.ID)
		if last := *evs[len(evs)-1].Subagent; !sameSubagentFull(last, row) {
			t.Fatalf("the snapshot's row %+v is not the last payload published for it %+v", row, last)
		}
	}
	if len(snap.Tools) != 2 {
		t.Fatalf("Snapshot().Tools holds %d rows, want the parent's two agent calls only", len(snap.Tools))
	}
	for _, row := range snap.Tools {
		if !row.IsTask() || toolStatusInFlight(row.Status) {
			t.Fatalf("a parent row in the snapshot: %+v", row)
		}
	}
}

// logHold is a log hook that blocks, once armed and only once, until release
// is closed, and says so by closing held. It is set before the session starts
// (the log reads its hooks unsynchronised), so arming it is what makes it
// act, and a failed test's cleanup releases it.
type logHold struct {
	armed         *atomic.Bool
	once          atomic.Bool
	held, release chan struct{}
}

func newLogHold(t *testing.T, armed *atomic.Bool) *logHold {
	h := &logHold{armed: armed, held: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { closeOnce(h.release) })
	return h
}

func (h *logHold) hold() {
	if h.armed.Load() && h.once.CompareAndSwap(false, true) {
		close(h.held)
		<-h.release
	}
}

// goroutineParkedIn reports whether some goroutine is blocked in a select
// with every one of frames on its stack, as runtime.Stack prints them.
func goroutineParkedIn(frames ...string) bool {
	for g := range strings.SplitSeq(string(allStacks()), "\n\n") {
		if strings.Contains(g, " [select") && !slices.ContainsFunc(frames, func(f string) bool { return !strings.Contains(g, f) }) {
			return true
		}
	}
	return false
}

// TestFlushBarrierUnderCancel (A11, panel P1): the barrier behind a child's
// finished holds when the turn is cancelled while it waits. The schedule:
// the outbox's drainer is held with the child's last events in it (the log's
// outboxAdmitting hook), so the finished the sink has just enqueued cannot be
// committed; the sink's flush parks on it and is held at the park (the
// flushParked hook); the turn's cancel lands — Cancel's own seam says so,
// from inside its section — and only then is the flush let into its wait.
// That wait is on Background and the session's done, so it goes on waiting:
// the goroutine is seen blocked there. A flush on the turn's context would
// have returned at once instead and let the parent's ToolFinished — published
// directly, with the drainer outside the boundary — overtake the finished
// still in the outbox; the test watches for exactly that as the other way
// the wait can end. Then the drainer goes, and the order is finished, then
// the call's row.
func TestFlushBarrierUnderCancel(t *testing.T) {
	f, r := routedNative(t)
	s := f.session(Options{})
	var armed atomic.Bool
	drainer, flush := newLogHold(t, &armed), newLogHold(t, &armed)
	s.log.hooks = &logHooks{
		outboxAdmitting: func(int) { drainer.hold() },
		flushParked:     func(uint64) { flush.hold() },
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	w := newNativeWatcher(t, s)
	landed := make(chan struct{})
	s.mu.Lock()
	s.cancelSeam = func() { close(landed) }
	s.mu.Unlock()

	a := r["test/a"]
	child := newHeld(t)
	a.route("go", callsStep(agentCall(t, "a1", "one task", "the task")), answer("never asked"))
	a.route("the task", child.step(openTextParts("half "), closeTextParts("done")))
	out := startPrompt(s, "go")
	await(t, child.reached, "the child mid-step")
	armed.Store(true)
	close(child.release)
	await(t, drainer.held, "the drainer held with the child's last events")
	await(t, flush.held, "the sink's flush parked behind them")

	cancelled := make(chan error, 1)
	go func() {
		_, err := s.Cancel(context.Background())
		cancelled <- err
	}()
	await(t, landed, "the turn's cancel, inside Cancel's section")
	close(flush.release)
	overtook := func() bool {
		return slices.ContainsFunc(w.events(), func(ev Event) bool { return isParentRowDone(ev, "t1.1.1") })
	}
	waitFor(t, "the flush waiting on, or the call's row overtaking it", func() bool {
		return overtook() || goroutineParkedIn("agent.(*EventLog).Flush(", "agent.(*nativeSession).subagentFinished(")
	})
	if overtook() {
		t.Fatal("the flush returned on the cancel: the parent's ToolFinished was published with the child's finished still in the outbox")
	}
	close(drainer.release)
	await(t, out, "the cancelled turn")
	if err := await(t, cancelled, "Cancel"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	w.waitType(EventDone)

	evs := w.events()
	id := spawnedWith(t, evs, "the task")
	finished := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, id, SubagentChangeFinished) })
	done := indexWhere(evs, 0, func(ev Event) bool { return isParentRowDone(ev, "t1.1.1") })
	if finished < 0 || done < 0 || finished > done {
		t.Fatalf("finished at %d, the call's row done at %d; want finished first", finished, done)
	}
	// The child had persisted its answer before the cancel reached the call:
	// it keeps it (harness §3.8's precedence), and the row says so.
	if st := evs[finished].Subagent.Status; st != SubagentCompleted {
		t.Fatalf("the child ended %q, want completed: its final step was saved before the cancel", st)
	}
}

// taskSink is a session nobody started, whose sink a test drives with the
// harness's events by hand, and a watcher on it. hooks, when not nil, are the
// log's, installed before anything reads them.
func taskSink(t *testing.T, hooks *logHooks) (*nativeSession, *nativeWatcher) {
	t.Helper()
	s := newNative(Options{}, nil)
	closeAtCleanup(t, s)
	s.log.hooks = hooks
	return s, newNativeWatcher(t, s)
}

// agentRequest is the prepared agent call the harness reports on ToolCalled.
func agentRequest(t *testing.T, description, prompt string) harness.ToolRequest {
	t.Helper()
	return harness.ToolRequest{Tool: tool.AgentTool, Kind: tool.KindTask, Title: description,
		Input: nativeArgs(t, map[string]any{"description": description, "prompt": prompt})}
}

// startAgentRow opens the parent's agent row call through the sink, as the
// harness does before the call runs.
func startAgentRow(t *testing.T, s *nativeSession, call, description, prompt string) {
	t.Helper()
	s.sink(harness.ToolStarted{ID: call, Step: 1, Tool: tool.AgentTool, Kind: tool.KindTask})
	s.sink(harness.ToolCalled{ID: call, CallID: "p-" + call, Request: agentRequest(t, description, prompt)})
}

// rowsFor is every row published for call in the parent's set, in order.
func rowsFor(evs []Event, call string) []ToolEvent {
	var out []ToolEvent
	for _, ev := range evs {
		if ev.Type == EventTool && ev.Agent == "" && ev.Tool.ID == call {
			out = append(out, *ev.Tool)
		}
	}
	return out
}

// TestTaskOnlyUpdatePublishes (A11, panel P5): the agent row's stamp at spawn
// changes its Task and nothing else, and it is published: the merge compares
// Task. It is also a copy: the row published before the stamp still says no
// child, so the stamp did not write through the pointer the earlier row
// shares. The same stamp again changes nothing and publishes nothing.
func TestTaskOnlyUpdatePublishes(t *testing.T) {
	s, w := taskSink(t, nil)
	startAgentRow(t, s, "t1.1.1", "scan it", "scan the tree")
	s.sink(harness.SubagentStarted{ID: "kid", CallID: "t1.1.1", Type: "explore", Description: "scan it",
		Prompt: "scan the tree", Model: "test/b", At: time.Now()})
	w.waitCount("the stamped row", 3, func(ev Event) bool { return ev.Type == EventTool && ev.Tool.ID == "t1.1.1" })

	rows := rowsFor(w.events(), "t1.1.1")
	called, stamped := rows[1], rows[2]
	if called.Task == nil || called.Task.AgentID != "" || called.Task.Model != "" || called.Task.Prompt != "scan the tree" ||
		called.Task.Description != "scan it" || called.Task.SubagentType != tool.DefaultAgentType {
		t.Fatalf("the row published at ToolCalled has task %+v; want the call's own, no child", called.Task)
	}
	if stamped.Task == nil || stamped.Task.AgentID != "kid" || stamped.Task.Model != "test/b" || stamped.Task.Prompt != "scan the tree" {
		t.Fatalf("the stamped row's task is %+v", stamped.Task)
	}
	// Task-only: everything a consumer draws besides it is the row before.
	c, st := called, stamped
	c.Task, st.Task, c.At, st.At = nil, nil, time.Time{}, time.Time{}
	if fmt.Sprint(c) != fmt.Sprint(st) {
		t.Fatalf("the stamp changed more than the task:\n%+v\n%+v", called, stamped)
	}
	// A restamp with the same values: the merge sees no change.
	s.stampTool("", "t1.1.1", func(t *ToolEvent) { t.Task.AgentID, t.Task.Model = "kid", "test/b" })
	s.sink(harness.ToolFinished{ID: "t1.1.1", Result: tool.Result{Text: "ok"}})
	w.waitCount("the finished row", 4, func(ev Event) bool { return ev.Type == EventTool && ev.Tool.ID == "t1.1.1" })
	if rows := rowsFor(w.events(), "t1.1.1"); len(rows) != 4 || toolStatusInFlight(rows[3].Status) {
		t.Fatalf("after an unchanged restamp and the finish: %d rows (last %+v); want the finish as the fourth", len(rows), rows[len(rows)-1])
	}
}

// TestRosterClonedBeforeEnqueue (A11, panel P5): a roster change is enqueued
// as the row's own clone. The drainer is held with a progress event in the
// outbox, the roster row behind it is written — its ToolsUsed element, its
// labels — as another change of the roster would, and the event that is then
// delivered still says what it said when it was enqueued. Run under -race, a
// payload that shared the row's backing array is also a data race between the
// write here and the reader's.
func TestRosterClonedBeforeEnqueue(t *testing.T) {
	s := newNative(Options{}, nil)
	closeAtCleanup(t, s)
	var armed atomic.Bool
	drainer := newLogHold(t, &armed)
	s.log.hooks = &logHooks{outboxAdmitting: func(int) { drainer.hold() }}
	w := newNativeWatcher(t, s)

	startAgentRow(t, s, "t1.1.1", "look", "look around")
	s.sink(harness.SubagentStarted{ID: "kid", CallID: "t1.1.1", Type: "general-purpose", Description: "look",
		Prompt: "look around", Model: "test/a", At: time.Now()})
	w.wait("the spawned event", func(ev Event) bool { return isRoster(ev, "kid", SubagentChangeSpawned) })
	armed.Store(true)
	s.sink(harness.SubagentEvent{ID: "kid", Event: harness.ToolCalled{ID: "k1", Request: harness.ToolRequest{
		Tool: "read", Kind: tool.KindRead, ReadOnly: true, Title: "main.go"}}})
	await(t, drainer.held, "the drainer, holding the progress event")

	s.rosterMu.Lock()
	row := s.roster["kid"]
	row.info.ToolsUsed[0] = "mutated"
	row.info.Activity, row.info.Description = "mutated", "mutated"
	s.rosterMu.Unlock()
	close(drainer.release)

	ev := w.wait("the progress event", func(ev Event) bool { return isRoster(ev, "kid", SubagentChangeProgress) })
	if got := ev.Subagent; !slices.Equal(got.ToolsUsed, []string{"read"}) || got.Activity != "main.go" || got.Description != "look" || got.ToolCalls != 1 {
		t.Fatalf("the delivered progress payload is %+v; want what the roster held when it was enqueued", got)
	}
	if snap := s.Snapshot().Subagents; len(snap) != 1 || snap[0].ToolsUsed[0] != "mutated" {
		t.Fatalf("control: the roster was not written behind the outbox: %+v", snap)
	}
}

// childLife drives one whole child through the sink, as the runner reports it:
// started, a tool call with a step's usage, and finished at the clock reading
// at.
func childLife(s *nativeSession, id, call string, at time.Time) {
	s.sink(harness.SubagentStarted{ID: id, CallID: call, Type: "general-purpose", Description: "task " + id,
		Prompt: "do " + id, Model: "test/a", At: at})
	s.sink(harness.SubagentEvent{ID: id, Event: harness.ToolStarted{ID: id + ".1", Step: 1, Tool: "read", Kind: tool.KindRead, ReadOnly: true}})
	s.sink(harness.SubagentEvent{ID: id, Event: harness.ToolCalled{ID: id + ".1", Request: harness.ToolRequest{
		Tool: "read", Kind: tool.KindRead, ReadOnly: true, Title: "f.go"}}})
	s.sink(harness.SubagentEvent{ID: id, Event: harness.ToolFinished{ID: id + ".1", Result: tool.Result{Text: "x", Content: "x"}}})
	s.sink(harness.SubagentEvent{ID: id, Event: harness.StepDone{Step: 1, Usage: harness.Usage{Input: 10, Output: 5}}})
}

// finishChild reports child id finished at at.
func finishChild(s *nativeSession, id string, at time.Time) {
	s.sink(harness.SubagentFinished{ID: id, Status: harness.SubagentCompleted, Text: "done " + id,
		Usage: harness.Usage{Input: 10, Output: 5}, Model: "test/a", Provider: "test", WireModel: "wire-a",
		ToolCalls: 1, Steps: 1, At: at})
}

// TestNativeSubagentEviction (A11, S1c's seam review): the roster keeps 32
// finished rows and evicts past them by the live rule — oldest FINISH first,
// by EndedAt and then finish order, never the row that has just finished —
// and not by spawn order: a child spawned before another and finished after
// it outlives it. Every finish here carries the same clock reading, as a µs
// clock gives two finishes on a fast machine, so EndedAt is stamped strictly
// increasing (1 ns apart) and no tie reaches the order. A long-runner spawned
// first and finished last, with a clock reading EARLIER than everyone's, is
// not evicted: its stamp is moved past the last finish, and it is the row
// just finished. Evicted rows go from the snapshot, and so do their tool rows:
// a late lossy progress, a late step and a restated finish for one publish
// nothing. And every roster event went through the outbox, never a direct
// publish (S1c's rule: each is enqueued in the section that made its change).
func TestNativeSubagentEviction(t *testing.T) {
	var mu sync.Mutex
	drained := map[uint64]bool{}
	s, w := taskSink(t, &logHooks{outboxSending: func(seq uint64, _ bool) {
		mu.Lock()
		drained[seq] = true
		mu.Unlock()
	}})
	at := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	s.sink(harness.SubagentStarted{ID: "long", CallID: "t1.1.0", Type: "general-purpose", Description: "long",
		Prompt: "run long", Model: "test/a", At: at})
	// a is spawned before b and finishes after it.
	childLife(s, "a", "t1.1.a", at)
	childLife(s, "b", "t1.1.b", at)
	finishChild(s, "b", at)
	finishChild(s, "a", at)
	for i := 1; i <= 31; i++ {
		id := fmt.Sprintf("c%02d", i)
		childLife(s, id, fmt.Sprintf("t1.1.%d", i), at)
		finishChild(s, id, at)
	}
	snap := s.Snapshot().Subagents
	if len(snap) != 33 || snap[0].ID != "long" || snap[1].ID != "a" || snap[2].ID != "c01" {
		t.Fatalf("after 33 finishes the roster is %v; want the long-runner, a, then c01 onwards: b, the oldest finish, evicted", ids(snap))
	}
	// Strictly increasing EndedAt in finish order, 1 ns apart from one reading.
	if d := snap[2].EndedAt.Sub(snap[1].EndedAt); d != time.Nanosecond {
		t.Fatalf("c01 ended %v after a; want 1ns: ties are moved past the last finish", d)
	}
	for i := 3; i < len(snap); i++ {
		if d := snap[i].EndedAt.Sub(snap[i-1].EndedAt); d != time.Nanosecond {
			t.Fatalf("%s ended %v after %s; want 1ns", snap[i].ID, d, snap[i-1].ID)
		}
	}
	finishChild(s, "long", at.Add(-time.Hour))
	snap = s.Snapshot().Subagents
	if len(snap) != 32 || snap[0].ID != "long" || snap[1].ID != "c01" {
		t.Fatalf("after the long-runner's finish the roster is %v; want it kept and a evicted", ids(snap))
	}
	if !snap[0].EndedAt.After(snap[len(snap)-1].EndedAt) {
		t.Fatalf("the long-runner's EndedAt %v is not past the last finish %v", snap[0].EndedAt, snap[len(snap)-1].EndedAt)
	}
	for _, row := range snap {
		if row.Status != SubagentCompleted {
			t.Fatalf("row %s is %q", row.ID, row.Status)
		}
	}

	s.toolMu.Lock()
	_, keptB := s.childTools["b"]
	_, keptA := s.childTools["a"]
	_, kept1 := s.childTools["c01"]
	s.toolMu.Unlock()
	if keptB || keptA || !kept1 {
		t.Fatalf("tool sets: evicted b kept %v, evicted a kept %v, live c01 kept %v; want the evicted ones gone with their rows",
			keptB, keptA, kept1)
	}
	marker := func(text string) {
		s.emit(Event{Type: EventText, Text: text})
		w.wait(text, func(ev Event) bool { return ev.Type == EventText && ev.Text == text })
	}
	marker("before the late events")
	before := len(w.events())
	s.sink(harness.SubagentEvent{ID: "b", Event: harness.ToolProgress{ID: "b.1", Output: "late"}})
	s.sink(harness.SubagentEvent{ID: "b", Event: harness.StepDone{Step: 2, Usage: harness.Usage{Input: 1}}})
	finishChild(s, "b", at)
	marker("after them")
	for _, ev := range w.events()[before:] {
		if ev.Agent == "b" || (ev.Subagent != nil && ev.Subagent.ID == "b") {
			t.Fatalf("an evicted child published %+v", ev)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	roster := 0
	for _, ev := range w.events() {
		switch {
		case ev.Type == EventSubagent:
			roster++
			if !drained[ev.Seq] {
				t.Fatalf("the %s event of %s (seq %d) was published directly, not through the outbox", ev.SubagentChange, ev.Subagent.ID, ev.Seq)
			}
		case ev.Agent != "" && drained[ev.Seq]:
			t.Fatalf("a child's %s event (seq %d) went through the outbox; it is published directly", ev.Type, ev.Seq)
		}
	}
	if roster == 0 {
		t.Fatal("control: no roster event was seen")
	}
}

// TestNativeSubagentFinishSettlesItsRows (§3.9): a child that ends with a call
// still open — cancelled mid-call — has that row settled to its outcome, and
// the settled row goes out before its finished.
func TestNativeSubagentFinishSettlesItsRows(t *testing.T) {
	s, w := taskSink(t, nil)
	at := time.Now()
	s.sink(harness.SubagentStarted{ID: "kid", CallID: "t1.1.1", Type: "general-purpose", Description: "d",
		Prompt: "p", Model: "test/a", At: at})
	s.sink(harness.SubagentEvent{ID: "kid", Event: harness.ToolStarted{ID: "k.1", Step: 1, Tool: "bash", Kind: tool.KindExecute}})
	s.sink(harness.SubagentFinished{ID: "kid", Status: harness.SubagentCancelled, At: at})
	w.wait("the finished event", func(ev Event) bool { return isRoster(ev, "kid", SubagentChangeFinished) })
	evs := w.events()
	settled := indexWhere(evs, 0, func(ev Event) bool {
		return ev.Type == EventTool && ev.Agent == "kid" && ev.Tool.ID == "k.1" && ev.Tool.Status == toolCancelled
	})
	finished := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, "kid", SubagentChangeFinished) })
	if settled < 0 || settled > finished {
		t.Fatalf("the open row settled at %d, the child finished at %d; want the row settled cancelled first", settled, finished)
	}
}

func ids(rows []SubagentInfo) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestNativeSubagentRace (A11, panel P16): the sink is concurrent. The parent's
// own tool calls run beside two children streaming text and tools, while a
// reader takes snapshots; then the turn is cancelled with both children mid-
// step, and a second session is closed the same way. Run under -race it is a
// data race anywhere the sink's cases share state; here it also checks that
// every row settles and that Close returns (closeAtCleanup bounds it).
func TestNativeSubagentRace(t *testing.T) {
	for _, end := range []string{"Cancel", "Close"} {
		t.Run(end, func(t *testing.T) {
			f, r := routedNative(t)
			ws := nativeWorkspaceWith(t, map[string]string{"main.go": "package main\n", "notes.txt": "alpha\n"})
			s := f.started(Options{Workspace: ws})
			w := newNativeWatcher(t, s)
			a := r["test/a"]
			one, two := newHeld(t), newHeld(t)
			a.route("race", callsStep(
				agentCall(t, "a1", "one", "child one"), agentCall(t, "a2", "two", "child two"),
				nativeCallParts("p1", "read", `{"filePath":"main.go"}`),
				nativeCallParts("p2", "read", `{"filePath":"notes.txt"}`),
			), answer("never asked"))
			for _, c := range []struct {
				prompt string
				h      *held
			}{{"child one", one}, {"child two", two}} {
				a.route(c.prompt, callsStep(
					nativeCallParts(c.prompt+"-r1", "read", `{"filePath":"main.go"}`),
					nativeCallParts(c.prompt+"-r2", "read", `{"filePath":"notes.txt"}`),
				), c.h.step(append(thoughtParts("hm"), openTextParts("a ", "b ", "c ")...), closeTextParts()))
			}
			stop := make(chan struct{})
			var readers sync.WaitGroup
			readers.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
						snap := s.Snapshot()
						for _, row := range snap.Subagents {
							_ = len(row.ToolsUsed) + len(row.Output)
						}
					}
				}
			})
			out := startPrompt(s, "race")
			await(t, one.reached, "child one mid-step")
			await(t, two.reached, "child two mid-step")
			switch end {
			case "Cancel":
				if _, err := s.Cancel(context.Background()); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
				await(t, out, "the cancelled turn")
				w.waitType(EventDone)
			case "Close":
				within(t, "Close", func() { _ = s.Close() })
				await(t, out, "the closed turn")
			}
			close(stop)
			readers.Wait()
			// Either way both children were stopped mid-step, and each one's
			// finished reached the roster: the harness joins them before the
			// turn — which Close waits for — returns.
			snap := s.Snapshot()
			if len(snap.Subagents) != 2 {
				t.Fatalf("the roster holds %d rows, want 2", len(snap.Subagents))
			}
			for _, row := range snap.Subagents {
				if row.Status != SubagentCancelled {
					t.Fatalf("child %s ended %q after %s, want cancelled", row.Description, row.Status, end)
				}
			}
			for _, row := range snap.Tools {
				if toolStatusInFlight(row.Status) {
					t.Fatalf("a parent row still running after %s: %+v", end, row)
				}
			}
			s.toolMu.Lock()
			defer s.toolMu.Unlock()
			for owner, set := range s.childTools {
				for _, id := range set.order {
					if row := set.rows[id]; toolStatusInFlight(row.Status) {
						t.Fatalf("child %s's row %s still running after %s", owner, id, end)
					}
				}
			}
		})
	}
}

// TestNativeSinkIsConcurrent (A11, panel P16): the sink's cases hammered from
// many goroutines at once, as the parent's turn and its children reach it —
// the parent's tool calls, two children's whole lives each on its own
// goroutine, snapshots — and Close landing in the middle. Under -race it is
// the audit above the sink's switch, checked.
func TestNativeSinkIsConcurrent(t *testing.T) {
	s, _ := taskSink(t, nil)
	at := time.Now()
	var wg sync.WaitGroup
	for c := range 8 {
		wg.Go(func() {
			id := fmt.Sprintf("t1.1.%d", c)
			s.sink(harness.ToolStarted{ID: id, Step: 1, Tool: "bash", Kind: tool.KindExecute})
			for p := range 20 {
				s.sink(harness.ToolProgress{ID: id, Output: strings.Repeat("o", p+1)})
			}
			s.sink(harness.ToolFinished{ID: id, Result: tool.Result{Text: "done"}})
		})
	}
	for k := range 2 {
		wg.Go(func() {
			id, call := fmt.Sprintf("kid%d", k), fmt.Sprintf("t1.1.a%d", k)
			startAgentRow(t, s, call, "task", "do it")
			childLife(s, id, call, at)
			for range 50 {
				s.sink(harness.SubagentEvent{ID: id, Event: harness.TextDelta{Text: "x"}})
				s.sink(harness.SubagentEvent{ID: id, Event: harness.StepDone{Usage: harness.Usage{Output: 1}}})
			}
			finishChild(s, id, at)
			s.sink(harness.ToolFinished{ID: call, Result: tool.Result{Text: "done " + id}})
		})
	}
	wg.Go(func() {
		for range 200 {
			_ = s.Snapshot()
		}
	})
	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	wg.Wait()
	await(t, closed, "Close, landing among them")
	for _, row := range s.Snapshot().Subagents {
		if row.Status != SubagentCompleted {
			t.Fatalf("child %s is %q after its finish", row.ID, row.Status)
		}
	}
}

// TestSubagentCanaryRedaction is A9's canary test, the adapter's half (the
// harness's half is internal/harness's test of the same name): what the
// adapter publishes of a child — the roster in the snapshot and in its
// events, the child's user line, the agent row's task, the journal — never
// holds a provider key, even one only a sanitizer would put back together (a
// zero-width space splitting it, which no redactor can see), and a child's
// streamed text is the parent's contract exactly: the same deltas, sanitized
// and not redacted, across a chunk boundary; its roster Output — the
// runner's redacted text — holds the marker.
func TestSubagentCanaryRedaction(t *testing.T) {
	split := nativeCanary[:6] + zeroWidthSpace + nativeCanary[6:]
	t.Run("payloads", func(t *testing.T) {
		f, r := routedNative(t)
		dir := filepath.Join(t.TempDir(), "journal")
		s := f.started(Options{JournalDir: dir})
		jw := journalOf(t, s.log)
		inc := s.Incarnation()
		w := newNativeWatcher(t, s)
		prompt := "use " + split + " then " + nativeCanary
		sent := s.hs.Redact(prompt)
		if !strings.Contains(sent, redact.Marker) || !strings.Contains(sent, split) {
			t.Fatalf("control: the child is sent %q; want the whole key redacted and the split one not", sent)
		}
		a := r["test/a"]
		a.route("go", callsStep(agentCall(t, "a1", "scan "+split, prompt)), answer("ok"))
		a.route(sent, answer("checked"))
		if _, err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		w.waitType(EventDone)
		snap := s.Snapshot()
		if len(snap.Subagents) != 1 {
			t.Fatalf("the roster holds %d rows", len(snap.Subagents))
		}
		row := snap.Subagents[0]
		if !strings.Contains(row.Prompt, redact.Marker) || !strings.Contains(row.Description, redact.Marker) {
			t.Fatalf("the roster row %+v; want the key redacted in its prompt and description", row)
		}
		leaked := func(where string, v any) {
			t.Helper()
			if strings.Contains(fmt.Sprintf("%+v", v), nativeCanary) {
				t.Fatalf("%s holds the key: %+v", where, v)
			}
		}
		leaked("the snapshot's roster", snap.Subagents)
		leaked("the snapshot's rows", snap.Tools)
		for _, ev := range w.events() {
			if ev.Type == EventSubagent || ev.Agent != "" || ev.Type == EventTool {
				leaked(fmt.Sprintf("a %s event", ev.Type), ev)
			}
		}
		user := w.wait("the child's user line", func(ev Event) bool { return ev.Type == EventUser && ev.Agent == row.ID })
		if user.Text != row.Prompt {
			t.Fatalf("the child's user line %q is not its roster prompt %q", user.Text, row.Prompt)
		}
		closeJournaled(t, s, jw)
		for _, l := range assertOneJournal(t, dir, jw, inc) {
			leaked("a journal line", l)
		}
	})
	t.Run("streamed", func(t *testing.T) {
		f, r := routedNative(t)
		s := f.started(Options{})
		w := newNativeWatcher(t, s)
		half := len(nativeCanary) / 2
		deltas := textParts("the key is "+nativeCanary[:half], nativeCanary[half:]+".")
		a := r["test/a"]
		a.route("go", callsStep(agentCall(t, "a1", "echo", "say the key")), reply(deltas, finishParts(fantasy.FinishReasonStop)))
		a.route("say the key", reply(deltas, finishParts(fantasy.FinishReasonStop)))
		if _, err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		w.waitType(EventDone)
		id := s.Snapshot().Subagents[0].ID
		var child, parent []string
		for _, ev := range w.events() {
			if ev.Type == EventText && ev.Agent == id {
				child = append(child, ev.Text)
			}
			if ev.Type == EventText && ev.Agent == "" {
				parent = append(parent, ev.Text)
			}
		}
		if !slices.Equal(child, parent) || !strings.Contains(strings.Join(child, ""), nativeCanary) {
			t.Fatalf("the child's deltas %q and the parent's %q; want the same, under one contract", child, parent)
		}
		if out := s.Snapshot().Subagents[0].Output; !strings.Contains(out, redact.Marker) || strings.Contains(out, nativeCanary) {
			t.Fatalf("the roster Output %q; want the key redacted", out)
		}
	})
}

// TestNativeSubagentUsageJournaledWhenUnsaved (A9, panel P40): the parent's
// step that ran a child is not written — a directory stands where its
// transcript would be linked, so the first append's link fails — and the
// usage the step's tool entry would have carried is journaled as a
// subagent_usage note, one row of per-model scalars, read back from the real
// journal. A step that was saved writes no note (the control).
func TestNativeSubagentUsageJournaledWhenUnsaved(t *testing.T) {
	t.Run("unsaved", func(t *testing.T) {
		f, r := routedNative(t)
		dir := filepath.Join(t.TempDir(), "journal")
		ws := t.TempDir()
		s := f.started(Options{Workspace: ws, JournalDir: dir})
		jw := journalOf(t, s.log)
		inc := s.Incarnation()
		sessions := filepath.Join(f.dir, "sessions", store.Slug(filepath.Clean(ws)))
		planted := filepath.Join(sessions, "20260918T120000Z_"+s.Snapshot().SessionID+".jsonl")
		if err := os.MkdirAll(planted, 0o700); err != nil {
			t.Fatal(err)
		}
		a := r["test/a"]
		a.route("go", callsStep(agentCall(t, "a1", "spend", "spend tokens")), answer("never asked"))
		a.route("spend tokens", answer("spent"))
		if _, err := s.Prompt(context.Background(), "go"); err == nil {
			t.Fatal("the turn whose step could not be saved did not fail")
		}
		// The control: the child's own transcript was written, beside the
		// place the parent's could not be.
		if kids, _ := filepath.Glob(filepath.Join(sessions, "*.jsonl")); len(kids) != 2 {
			t.Fatalf("the sessions directory holds %v; want the child's transcript and the planted directory", kids)
		}
		closeJournaled(t, s, jw)
		notes := diags(assertOneJournal(t, dir, jw, inc), diagSubagentUsage)
		if len(notes) != 1 {
			t.Fatalf("%d subagent_usage notes, want one", len(notes))
		}
		n := notes[0]
		want := map[string]any{"step": float64(1), "rows": float64(1), "provider_1": "test", "model_1": "test/a",
			"wire_model_1": "wire-a", "input_1": float64(10), "output_1": float64(5), "reasoning_1": float64(0),
			"cache_read_1": float64(0), "cache_creation_1": float64(0)}
		for k, v := range want {
			if n[k] != v {
				t.Fatalf("the note's %s is %v, want %v (note %v)", k, n[k], v, n)
			}
		}
		if e, _ := n["save_error"].(string); e == "" {
			t.Fatalf("the note says no save error: %v", n)
		}
	})
	t.Run("saved", func(t *testing.T) {
		f, r := routedNative(t)
		dir := filepath.Join(t.TempDir(), "journal")
		s := f.started(Options{JournalDir: dir})
		jw := journalOf(t, s.log)
		inc := s.Incarnation()
		a := r["test/a"]
		a.route("go", callsStep(agentCall(t, "a1", "spend", "spend tokens")), answer("ok"))
		a.route("spend tokens", answer("spent"))
		if _, err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		closeJournaled(t, s, jw)
		if notes := diags(assertOneJournal(t, dir, jw, inc), diagSubagentUsage); len(notes) != 0 {
			t.Fatalf("a saved step was journaled: %v", notes)
		}
	})
}

// TestNativeSubagentFailedAndUnstarted: a child whose provider fails is a
// failed roster row with its error, its open tool rows settled failed, and a
// failed agent row showing the child's duration and model; a call refused
// before any child existed (an unknown type) is a failed agent row with no
// child and no roster row at all.
func TestNativeSubagentFailedAndUnstarted(t *testing.T) {
	f, r := routedNative(t)
	ws := nativeWorkspaceWith(t, map[string]string{"main.go": "package main\n"})
	s := f.started(Options{Workspace: ws})
	w := newNativeWatcher(t, s)
	a := r["test/a"]
	a.route("go", callsStep(agentCall(t, "a1", "breaks", "break down"),
		agentCall(t, "a2", "nobody", "no one", "subagent_type", "nobody")), answer("noted"))
	a.route("break down", readStep("r1", "main.go"), reply(textParts("partial"), errorParts(errors.New("provider down"))))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	snap := s.Snapshot()
	if len(snap.Subagents) != 1 {
		t.Fatalf("the roster holds %v; want the one child that started", ids(snap.Subagents))
	}
	row := snap.Subagents[0]
	if row.Status != SubagentFailed || !strings.Contains(row.Error, "provider down") || row.Output != "partial" || row.ToolCallID != "t1.1.1" {
		t.Fatalf("the failed child's row is %+v", row)
	}
	byID := map[string]ToolEvent{}
	for _, t := range snap.Tools {
		byID[t.ID] = t
	}
	if got := byID["t1.1.1"]; got.Status != toolFailed || got.Task.Status != SubagentFailed || got.Task.AgentID != row.ID ||
		got.Task.Model != "test/a" || !got.Task.Receipt {
		t.Fatalf("the failed child's agent row is %+v (task %+v)", got, got.Task)
	}
	if got := byID["t1.1.2"]; got.Status != toolFailed || got.Task.Status != SubagentFailed || got.Task.AgentID != "" ||
		got.Task.Model != "" || !got.Task.Receipt || got.Task.SubagentType != "nobody" {
		t.Fatalf("the refused call's agent row is %+v (task %+v)", got, got.Task)
	}
}
