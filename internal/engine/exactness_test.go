package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
)

// A2 (plan 024 §7): a second client that attaches mid-session from a snapshot
// reproduces the first client — the one that folded from seq 1 — exactly, on
// the history and state projections, at a common Seq. The same Fold runs on
// both sides (§3.1), so what these hold is the snapshot, Restore, the
// subscription's cut and the engine's instance being the committed sequence.

// traceClock is a Stub clock whose every reading is its own instant, in UTC
// and with no monotonic reading, so a time goes through the codec unchanged.
func traceClock() func() time.Time {
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var n atomic.Int64
	return func() time.Time { return base.Add(time.Duration(n.Add(1)) * time.Millisecond) }
}

// cutoff is the engine's committed cutoff once nothing is publishing: the Seq
// its model has folded, read off a snapshot (Sync first, so the outbox is
// empty).
func cutoff(t *testing.T, ctx context.Context, e *engine.Engine) uint64 {
	t.Helper()
	if err := e.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	a, err := e.Attach(ctx, engine.AttachOptions{SnapshotBytes: 1 << 30})
	if err != nil {
		t.Fatalf("attaching for the cutoff: %v", err)
	}
	a.Sub.Close()
	return a.Snapshot.Seq
}

// primaryReader is a client reading the primary on a goroutine of its own
// until the test ends: it keeps every event, in commit order, and when it has a
// model it folds each one there too — a client folding the primary's own
// values, which is why that model reads an error with ErrText (X14).
type primaryReader struct {
	mu    sync.Mutex
	evs   []agent.Event
	model *transcript.Model
	moved chan struct{} // closed and replaced as evs grows
	stop  chan struct{}
	done  chan struct{}
	halt  func()
}

func readPrimary(t *testing.T, e *engine.Engine, fold bool) *primaryReader {
	t.Helper()
	p := &primaryReader{moved: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	if fold {
		p.model = transcript.New(transcript.Options{ErrText: func(err error) string { return err.Error() }})
	}
	p.halt = sync.OnceFunc(func() {
		close(p.stop)
		<-p.done
	})
	go func() {
		defer close(p.done)
		for {
			select {
			case ev := <-e.Events():
				p.mu.Lock()
				if p.model != nil {
					p.model.Fold(ev)
				}
				p.evs = append(p.evs, ev)
				close(p.moved)
				p.moved = make(chan struct{})
				p.mu.Unlock()
			case <-p.stop:
				return
			}
		}
	}()
	t.Cleanup(p.halt)
	return p
}

// waitFor waits for the first event at index from or later that pred accepts,
// and returns its index.
func (p *primaryReader) waitFor(t *testing.T, from int, pred func(agent.Event) bool) int {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		p.mu.Lock()
		for i := from; i < len(p.evs); i++ {
			if pred(p.evs[i]) {
				p.mu.Unlock()
				return i
			}
		}
		from = max(from, len(p.evs))
		moved := p.moved
		p.mu.Unlock()
		select {
		case <-moved:
		case <-deadline:
			t.Fatalf("the primary reader has %d events and none of the latest is the one waited for", from)
		}
	}
}

// waitSeq waits until the reader holds seq, and fails if it holds more: the
// comparisons are at a common Seq.
func (p *primaryReader) waitSeq(t *testing.T, seq uint64) {
	t.Helper()
	p.waitFor(t, 0, func(ev agent.Event) bool { return ev.Seq == seq })
	p.mu.Lock()
	defer p.mu.Unlock()
	if last := p.evs[len(p.evs)-1].Seq; last != seq {
		t.Fatalf("the primary reader is at %d, past the cutoff %d: something published after the session went quiet", last, seq)
	}
}

// seqAt is the Seq of the event at index i.
func (p *primaryReader) seqAt(i int) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evs[i].Seq
}

func endedTurn(id string) func(agent.Event) bool {
	return func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded && ev.Turn.ID == id
	}
}

// capTool is a tool report at internal/agent/tools.go's per-field caps: three
// 8 KiB output tails, two 512 B heads, 512 B of raw input, 2 KiB of content,
// eight locations and, with diffs, eight diffs of 64 KiB a side — the ~1 MiB
// a cursor edit can retain (§2.6).
func capTool(id, status string, diffs bool) *agent.ToolEvent {
	code := 1
	tool := &agent.ToolEvent{
		ID: id, Status: status, Kind: "execute", Title: "Run make", ToolName: "Shell",
		RawInput: strings.Repeat("r", 512), ContentText: strings.Repeat("c", 2<<10),
		Locations: []string{"a", "b", "c", "d", "e", "f", "g", "h"},
		Output: &agent.ToolOutput{ExitCode: &code, Truncated: true,
			Stdout: strings.Repeat("o", 8<<10), Stderr: strings.Repeat("e", 8<<10), Content: strings.Repeat("n", 8<<10),
			StdoutHead: strings.Repeat("O", 512), StderrHead: strings.Repeat("E", 512)},
	}
	if diffs {
		tool.Kind, tool.Title = "edit", "Edit eight files"
		for i := range 8 {
			tool.Diffs = append(tool.Diffs, agent.ToolDiff{
				Path: fmt.Sprintf("/w/f%d.go", i), OldText: strings.Repeat("-", 64<<10), NewText: strings.Repeat("+", 64<<10),
				Added: 900, Removed: 800, Truncated: true,
			})
		}
	}
	return tool
}

// recordStubSession runs a scripted session over tui.Stub through a real engine
// and returns every event it committed, in commit order with Seq, as the
// engine's own observer saw them (each error the *agent.RemoteError the log
// hands its observer, X14); the engine; the Seq of the oversized event; and
// what a subscription opened before the first event received.
//
// The session carries everything §7 A2 names: thoughts; text, incl. a run
// saturated past the stream cap; tools at the payload caps, an id-less one and
// updates; a permission, a question and a plan, answered, auto and cancelled;
// sub-agents streaming and in receipt mode; todos, repeated; every queue verb
// and a duplicate `queued`; every settings section (title, mode, model,
// config, commands, plugins, send-now); an interjection; a command line; an
// error; a turn cancelled by a send-now, then its successor and a drained row;
// a foreign turn; a replay bracket; and one event over MaxRecordBytes (8 MiB),
// which the observer folds whole and a subscriber sees Omitted.
func recordStubSession(t *testing.T) (trace []agent.Event, e *engine.Engine, oversized uint64, recs []agent.Record) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
	t.Cleanup(cancel)
	stub := tui.NewStub()
	stub.Clock = traceClock()
	stub.SetProvider(agent.GrokProvider()) // a provider that interjects
	e, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	primary := readPrimary(t, e, false)
	sub, err := e.Subscribe(agent.SubscribeOptions{MaxItems: 1 << 14, MaxBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	var subRecs []agent.Record
	subDone := make(chan struct{})
	go func() {
		defer close(subDone)
		for rec := range sub.Records() {
			subRecs = append(subRecs, rec)
		}
	}()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	emit := stub.Emit
	var c engine.Command

	// Settings, before the first turn.
	for _, s := range []engine.Setting{
		{Kind: engine.SettingModel, Value: "fast"},
		{Kind: engine.SettingMode, Value: "plan"},
		{Kind: engine.SettingConfig, ID: "effort", Value: "high"},
	} {
		_, err := e.Set(ctx, c, s)
		must("set "+string(s.Kind), err)
	}
	must("set title", e.SetTitle(c, "exactness"))
	emit(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
		Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "review", Description: "Review the diff"}}},
		Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Plugin: "p", Bare: "x", Qualified: "p:x", Kind: "skill"}}},
	}})

	// Turn 1, held open, carrying everything a turn can.
	open := stub.HangNext()
	res, err := e.Submit(c, "fix <the> tests", engine.SubmitQueue, "")
	must("submit", err)
	select {
	case <-open:
	case <-ctx.Done():
		t.Fatal("the first turn never opened")
	}
	emit(agent.Event{Type: agent.EventThought, Text: "weighing "})
	emit(agent.Event{Type: agent.EventThought, Text: "the options"})
	emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Status: "pending", Title: "Shell", RawInput: "make test"}})
	emit(agent.Event{Type: agent.EventText, Text: "Running the tests."})
	emit(agent.Event{Type: agent.EventTool, Tool: capTool("t1", "completed", false)})
	emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{Title: "an id-less tool"}})
	todos := []agent.Todo{{ID: "1", Content: "one", Status: "pending"}, {ID: "2", Content: "two", Status: "pending"}}
	emit(agent.Event{Type: agent.EventTodos, Todos: todos})
	emit(agent.Event{Type: agent.EventTodos, Todos: todos})

	emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell: rm -rf build", Options: []agent.PermissionOption{
		{OptionID: "allow-once", Name: "Allow", Kind: "allow_once"}, {OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
	}}})
	must("answer the permission", e.Answer(c, "perm-1", agent.AskAnswer{OptionID: "allow-once"}))
	question := []agent.Question{{ID: "q1", Prompt: "Which suite?", Options: []agent.Option{{ID: "a", Label: "unit"}, {ID: "b", Label: "all"}}}}
	emit(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "q-1", Title: "Scope", Questions: question}})
	must("answer the question", e.Answer(c, "q-1", agent.AskAnswer{Answers: map[string][]string{"q1": {"a"}}}))
	emit(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "q-auto", Questions: question, Auto: true, Answers: map[string][]string{"q1": {"b"}}}})
	// Left open: the cancel below ends it.
	emit(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Name: "Fix", Overview: "the overview", Plan: "1. fix\n2. test", Todos: todos}})
	must("sync", e.Sync(ctx))

	emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning, Prompt: "look around", Description: "Look"}, SubagentChange: agent.SubagentChangeSpawned})
	emit(agent.Event{Type: agent.EventUser, Agent: "sub-1", Text: "look around"})
	emit(agent.Event{Type: agent.EventThought, Agent: "sub-1", Text: "reading"})
	emit(agent.Event{Type: agent.EventTool, Agent: "sub-1", Tool: &agent.ToolEvent{ID: "c1", Status: "pending", Title: "Read"}})
	emit(agent.Event{Type: agent.EventText, Agent: "sub-1", Text: "found it"})
	emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentCompleted, Output: "found it"}, SubagentChange: agent.SubagentChangeFinished})
	task := agent.TaskInfo{Description: "research", Prompt: "dig", AgentID: "sub-r", SubagentType: "explore", Status: agent.SubagentRunning}
	emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "task-1", Kind: "other", Title: "Subagent research", Status: "pending", Task: &task}})
	emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-2", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned})
	emit(agent.Event{Type: agent.EventThought, Agent: "sub-2", Text: "still going"})
	receipt := task
	receipt.Receipt, receipt.DurationMs, receipt.Status = true, 1200, agent.SubagentCompleted
	emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "task-1", Kind: "other", Title: "Subagent research", Status: "completed", Task: &receipt}})

	emit(agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{PluginCommand: agent.PluginCommand{Plugin: "p", Bare: "x", Qualified: "p:x", Kind: "skill"}, Text: "expanded"}})
	emit(agent.Event{Type: agent.EventError, Err: errors.New("the agent stumbled")})
	must("interject", e.Interject(ctx, c, "also the docs"))

	q1, err := e.Queue(c, "later one")
	must("queue", err)
	q2, err := e.Queue(c, "later two")
	must("queue", err)
	must("edit", e.EditQueued(c, q1.ID, "later one, edited", nil))
	_, err = e.Unqueue(c, q2.ID)
	must("unqueue", err)
	must("sync", e.Sync(ctx))
	row := e.State().Queue[0]
	emit(agent.Event{Type: agent.EventQueue, Queue: &row, QueueChange: agent.QueueQueued}) // the duplicate

	for i := range 80 { // a run saturated past the 64 KiB stream cap
		emit(agent.Event{Type: agent.EventText, Text: strings.Repeat(string(rune('a'+i%26)), 1<<10)})
	}
	emit(agent.Event{Type: agent.EventTool, Tool: capTool("edit-1", "completed", true)})
	emit(agent.Event{Type: agent.EventText, Text: strings.Repeat("O", 8<<20+1<<10)}) // over MaxRecordBytes

	// The send-now cancels the held turn (the open plan ends with it), fires
	// as its successor, and the edited row drains after it.
	sent, err := e.Submit(c, "right now", engine.SubmitSendNow, "")
	if err != nil || !sent.Armed {
		t.Fatalf("send-now: %+v, %v", sent, err)
	}
	at := primary.waitFor(t, 0, endedTurn(res.Turn))
	primary.waitFor(t, at, chainOver)

	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "f-1", Text: "on my own", Running: true})
	emit(agent.Event{Type: agent.EventText, Text: "foreign reply"})
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "f-1"})
	emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
	emit(agent.Event{Type: agent.EventUser, Text: "an old prompt", Replayed: true})
	emit(agent.Event{Type: agent.EventText, Text: "an old answer", Replayed: true})
	emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}})
	last, err := e.Submit(c, "one more", engine.SubmitQueue, "")
	must("the last submit", err)
	primary.waitFor(t, 0, endedTurn(last.Turn))

	n := cutoff(t, ctx, e)
	primary.waitSeq(t, n)
	primary.halt()
	sub.Close()
	<-subDone

	trace = append([]agent.Event(nil), primary.evs...)
	for i := range trace {
		if trace[i].Seq != uint64(i+1) {
			t.Fatalf("the primary's event %d has seq %d", i, trace[i].Seq)
		}
		if trace[i].Err != nil {
			trace[i].Err = agent.RemoteErrorOf(trace[i].Err)
		}
		if trace[i].Type == agent.EventText && len(trace[i].Text) > 8<<20 {
			oversized = trace[i].Seq
		}
	}
	return trace, e, oversized, subRecs
}

// TestASecondSubscriberAttachedMidTurnReproducesTheFirst (A2): the Stub session
// above, recorded once, is cut at EVERY Seq k: the model that folded events
// 1..k is snapshotted (a budget nothing is windowed at) and restored — in
// process, and through the codec — and each restored model equals the prefix
// model immediately, and, folding k+1..end, equals the model that folded
// everything at the common end. Deterministic, with no goroutine per cut: the
// prefix model at cut k is one model that has folded exactly 1..k, advanced
// one event per cut, which is the fresh model of that prefix. The whole fold
// is also the engine's own model (its snapshot, restored), and the oversized
// event reached a subscriber Omitted.
func TestASecondSubscriberAttachedMidTurnReproducesTheFirst(t *testing.T) {
	trace, e, oversized, recs := recordStubSession(t)
	if len(trace) > 2000 {
		t.Fatalf("the trace has %d events, over its 2,000 bound", len(trace))
	}
	if oversized == 0 || recs[oversized-1].Seq != oversized || recs[oversized-1].Omitted == nil {
		t.Fatalf("the oversized event (seq %d) did not reach the subscriber as an Omitted record", oversized)
	}

	full := transcript.New(transcript.Options{})
	for _, ev := range trace {
		full.Fold(ev)
	}
	fullView := engine.ViewOf(full)
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	a, err := e.Attach(ctx, engine.AttachOptions{SnapshotBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	a.Sub.Close()
	if d := engine.DiffViews(fullView, engine.ViewOf(transcript.Restore(a.Snapshot, transcript.Options{})), false); d != "" {
		t.Fatalf("the trace folded whole is not the engine's own model: %s", d)
	}

	start := time.Now()
	prefix := transcript.New(transcript.Options{})
	for k := 0; k <= len(trace); k++ {
		if k > 0 {
			prefix.Fold(trace[k-1])
		}
		s, err := prefix.Snapshot(1 << 30)
		if err != nil {
			t.Fatalf("cut %d: %v", k, err)
		}
		if s.Seq != uint64(k) || s.Main.Windowed {
			t.Fatalf("cut %d: the snapshot is at %d, windowed %v", k, s.Seq, s.Main.Windowed)
		}
		b, err := transcript.EncodeSnapshot(s)
		if err != nil {
			t.Fatalf("cut %d: %v", k, err)
		}
		d, err := transcript.DecodeSnapshot(b)
		if err != nil {
			t.Fatalf("cut %d: %v", k, err)
		}
		restored := []*transcript.Model{transcript.Restore(s, transcript.Options{}), transcript.Restore(d, transcript.Options{})}
		at := engine.ViewOf(prefix)
		for i, r := range restored {
			if diff := engine.DiffViews(at, engine.ViewOf(r), false); diff != "" {
				t.Fatalf("cut %d (%s), immediately after restore: %s", k, []string{"in process", "through the codec"}[i], diff)
			}
		}
		for _, ev := range trace[k:] {
			restored[0].Fold(ev)
			restored[1].Fold(ev)
		}
		for i, r := range restored {
			if diff := engine.DiffViews(fullView, engine.ViewOf(r), false); diff != "" {
				t.Fatalf("cut %d (%s), at the common end %d: %s", k, []string{"in process", "through the codec"}[i], len(trace), diff)
			}
		}
	}
	t.Logf("%d events, oversized at %d: %d cuts in %v", len(trace), oversized, len(trace)+1, time.Since(start).Round(time.Millisecond))
}

// fakeAgentBin is cmd/craze-fake-agent, built once per test binary (or taken
// from CRAZE_FAKE_AGENT_BIN), as internal/agent's and internal/cli's tests
// build it.
var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
)

func fakeAgentBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CRAZE_FAKE_AGENT_BIN"); p != "" {
		return p
	}
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "craze-fake-agent-")
		if err != nil {
			fakeErr = err
			return
		}
		fakeBin = filepath.Join(dir, "craze-fake-agent")
		out, err := exec.Command("go", "build", "-o", fakeBin, "github.com/charliek/craze/cmd/craze-fake-agent").CombinedOutput()
		if err != nil {
			fakeErr = fmt.Errorf("%w\n%s", err, out)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakeBin
}

// TestAttachOverTheFakeAgentReproducesTheFirst (A2): a real live session over
// cmd/craze-fake-agent, through a real engine. A primary reader folds from seq
// 1 on a goroutine of its own, the whole time. At eight cuts — each turn's
// first tool report, as the primary reader folds it — a second client attaches
// from a goroutine of its own (the primary keeps being read) and folds its
// subscription. The fixture is held there, after its first tool, by the fake
// agent's gate (CRAZE_FAKE_GATE) until the attach has returned, and only then
// released (r4 finding 2): so every snapshot is cut mid-turn, before the turn's
// ending, and the second client folds the rest of the turn from its
// subscription — at least one record — rather than restoring a turn that had
// already finished. Each is compared with the primary reader's fold at a
// common Seq — the turn's settled cutoff, which the second client folds
// through — never "at exit" (§3.6 item 7).
func TestAttachOverTheFakeAgentReproducesTheFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
	defer cancel()
	release := fakeAgentGate(t, ctx)
	sess := agent.New(agent.Options{
		Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=tasks"},
		Workspace: t.TempDir(), Stderr: io.Discard,
	})
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	first := readPrimary(t, e, true)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	from := 0
	for cut := range 8 {
		res, err := e.Submit(engine.Command{}, fmt.Sprintf("prompt %d", cut), engine.SubmitQueue, "")
		if err != nil || res.Turn == "" {
			t.Fatalf("cut %d: submit: %+v, %v", cut, res, err)
		}
		at := first.waitFor(t, from, func(ev agent.Event) bool { return ev.Type == agent.EventTool })

		type attached struct {
			c   *engine.AttachClient
			err error
		}
		ready, target, folded := make(chan attached, 1), make(chan uint64, 1), make(chan error, 1)
		go func() {
			c, err := engine.NewAttachClient(ctx, e, engine.AttachOptions{})
			ready <- attached{c, err}
			if err != nil {
				return
			}
			select {
			case n := <-target:
				folded <- c.FoldThrough(ctx, n)
			case <-ctx.Done():
				folded <- ctx.Err()
			}
		}()

		// The fixture is held after this tool until the attach has returned.
		a := <-ready
		if a.err != nil {
			t.Fatalf("cut %d: attach: %v", cut, a.err)
		}
		release()
		ended := first.waitFor(t, at, endedTurn(res.Turn))
		from = ended + 1
		snapSeq, endedSeq := a.c.Attachments[0].Snapshot.Seq, first.seqAt(ended)
		if snapSeq < first.seqAt(at) || snapSeq >= endedSeq {
			t.Fatalf("cut %d: the snapshot is at %d, want mid-turn: at or after the first tool (%d), before the turn's ending (%d)", cut, snapSeq, first.seqAt(at), endedSeq)
		}
		n := cutoff(t, ctx, e)
		first.waitSeq(t, n)
		target <- n
		if err := <-folded; err != nil {
			t.Fatalf("cut %d: the second client folding through %d: %v", cut, n, err)
		}
		first.mu.Lock()
		d := engine.DiffModels(first.model, a.c.Model, true)
		first.mu.Unlock()
		if d != "" {
			t.Fatalf("cut %d (attached at %d, compared at %d): %s", cut, a.c.Attachments[0].Snapshot.Seq, n, d)
		}
		if got := a.c.Seq(); got != n {
			t.Fatalf("cut %d: the second client is at %d, the first at %d", cut, got, n)
		}
		if a.c.Folded[0] < 1 {
			t.Fatalf("cut %d: the second client folded nothing after its snapshot at %d", cut, snapSeq)
		}
		t.Logf("cut %d: attached at seq %d (snapshot), compared at %d, the second client folded %d records", cut, a.c.Attachments[0].Snapshot.Seq, n, a.c.Folded[0])
		a.c.Close()
	}
}

// fakeAgentGate points the fake agent's CRAZE_FAKE_GATE at a fresh FIFO and
// returns what releases one held turn: it writes the one byte the fixture
// waits for. Opening the FIFO waits for the fake to be at its gate, so the
// write is done on a goroutine and bounded by ctx, the watchdog.
func fakeAgentGate(t *testing.T, ctx context.Context) func() {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_GATE", fifo)
	return func() {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err != nil {
				done <- err
				return
			}
			_, err = w.Write([]byte{1})
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("releasing the fake agent's gate: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("releasing the fake agent's gate: %v", ctx.Err())
		}
	}
}

// subFold is a client folding a plain subscription opened before the first
// event: the first client of the windowed test.
type subFold struct {
	sub   *agent.Subscription
	model *transcript.Model
}

func (f *subFold) foldThrough(t *testing.T, seq uint64) {
	t.Helper()
	for f.model.Seq() < seq {
		select {
		case rec, ok := <-f.sub.Records():
			if !ok {
				t.Fatalf("the first client's subscription ended: %v", f.sub.Err())
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			f.model.Fold(ev)
		case <-time.After(watchdog):
			t.Fatalf("the first client is at %d, waiting for %d", f.model.Seq(), seq)
		}
	}
}

// TestAWindowedSnapshotReproducesTheSuffix (A2): through the engine, an attach
// whose SnapshotBytes windows the snapshot — the main transcript keeps its
// newest four entries and the child none, mid-run — restores to the first
// client's suffix with Windowed set, and folding on keeps it so: a chunk into
// either open run, a new tool, an update to a kept tool, a tool ending the
// child's omitted run and a new run after it. An update to a tool the window
// omitted leaves the restored model's rows unchanged, while the first client
// updates its own row (§3.5).
//
// OPEN OWNER QUESTION (C2's implementer, not decided here): §3.5's "an update
// to a tool the window omitted applies to nothing" stops reproducing the first
// client once the FIRST model trims that omitted row — it forgets the id and a
// later update appends a new row there, while the restored model, which never
// forgets an omitted id, still applies it to nothing. The owner is choosing
// between a byte ledger of the omitted entries (exact) and recording the
// divergence. So every schedule here updates the omitted tool while the first
// model still holds its row (nothing is trimmed: a few KiB against 8 MiB); a
// follow-up commit extends this test once that is decided.
func TestAWindowedSnapshotReproducesTheSuffix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*watchdog)
	defer cancel()
	stub := tui.NewStubNoPrimary()
	stub.Clock = traceClock()
	e, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	first := &subFold{sub: sub, model: transcript.New(transcript.Options{})}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		stub.Emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%d", i), Status: "pending", RawInput: strings.Repeat("r", 400)}})
		stub.Emit(agent.Event{Type: agent.EventText, Text: strings.Repeat("a", 300)})
	}
	stub.Emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned})
	stub.Emit(agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c0", Status: "pending"}})
	stub.Emit(agent.Event{Type: agent.EventThought, Agent: "sub", Text: strings.Repeat("s", 2000)})
	stub.Emit(agent.Event{Type: agent.EventThought, Text: "main thinks"})

	// The largest budget at which the main transcript keeps at most four.
	whole, err := e.Attach(ctx, engine.AttachOptions{SnapshotBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	whole.Sub.Close()
	fb, err := transcript.EncodeSnapshot(whole.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var a *engine.Attachment
	for budget := len(fb); a == nil; budget -= 50 {
		try, err := e.Attach(ctx, engine.AttachOptions{SnapshotBytes: budget})
		if err != nil {
			t.Fatalf("attach at %d: %v", budget, err)
		}
		if len(try.Snapshot.Main.Entries) <= 4 {
			a = try
			break
		}
		try.Sub.Close()
	}
	s := a.Snapshot
	if !s.Main.Windowed || len(s.Subs) != 1 || len(s.Subs[0].Entries) != 0 || s.Subs[0].OmittedRun != transcript.KindThought || len(s.Main.OmittedTools) == 0 {
		t.Fatalf("the window: main %d entries (windowed %v, omitted %v), child %d (run %v)",
			len(s.Main.Entries), s.Main.Windowed, s.Main.OmittedTools, len(s.Subs[0].Entries), s.Subs[0].OmittedRun)
	}
	omitted := ""
	for tid := range s.Main.OmittedTools {
		omitted = tid
	}
	c := engine.AdoptAttachment(e, engine.AttachOptions{}, a)
	t.Cleanup(c.Close)

	check := func(what string) {
		t.Helper()
		n := cutoff(t, ctx, e)
		first.foldThrough(t, n)
		if err := c.FoldThrough(ctx, n); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if c.Seq() != n || first.model.Seq() != n || len(c.Reattached) != 0 {
			t.Fatalf("%s: the clients are at %d and %d, want %d (re-attached %q)", what, first.model.Seq(), c.Seq(), n, c.Reattached)
		}
		if d := engine.DiffSuffix(first.model, c.Model, false); d != "" {
			t.Fatalf("%s: %s", what, d)
		}
	}
	check("restored")
	stub.Emit(agent.Event{Type: agent.EventThought, Text: " more"})
	check("a chunk into the main run")
	stub.Emit(agent.Event{Type: agent.EventThought, Agent: "sub", Text: " more"})
	check("a chunk into the child's omitted run")
	if got := c.Model.Sub("sub").Len(); got != 0 {
		t.Fatalf("a chunk into an omitted run drew %d entries", got)
	}
	stub.Emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "new", Status: "pending"}})
	check("a new tool closes the main run")

	before := engine.ViewOf(c.Model)
	stub.Emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: omitted, Status: "completed"}})
	check("an update to an omitted tool")
	after := engine.ViewOf(c.Model)
	after.State.Seq = before.State.Seq
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("an update to the omitted tool %q changed the restored model", omitted)
	}
	if ts := first.model.State().Tools[transcript.ToolKey{ID: omitted}]; ts == nil || ts.Status != "completed" {
		t.Fatalf("the first client did not update its own row of %q: %+v", omitted, ts)
	}
	stub.Emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "new", Status: "completed"}})
	check("an update to a kept tool")
	stub.Emit(agent.Event{Type: agent.EventTool, Agent: "sub", Tool: &agent.ToolEvent{ID: "c1"}})
	check("a tool ends the child's omitted run")
	stub.Emit(agent.Event{Type: agent.EventText, Agent: "sub", Text: "after"})
	check("a new run in the child")
	if got := c.Model.Sub("sub").Len(); got != 2 {
		t.Fatalf("the child's entries after its omitted run: %d", got)
	}
}
