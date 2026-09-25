package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// Background children (plan 026 §3.11, §7 A13). Every test here runs real
// child sessions on the routing scripted model (subagents_test.go): a
// background child's step is a worker the test owns, so a result becomes
// ready exactly when the test says, and a parent's step that must meet it at
// a known point holds on a channel of the test's own. Nothing sleeps. Every
// session's OnPending handler asks HasPending and hands the answer to the
// test (bg.pending): a handler called under a lock of the harness's would
// deadlock there, and the test's wait for it would fail.

// bg is a routed fixture's session opened with background children on: its
// own sink recorded (Options.Sink), every OnPending call reported with what
// HasPending said then, and its children watched.
type bg struct {
	*routed
	s       *Session
	own     events    // the session's sink: background children's events, finishes and undelivered reports
	pending chan bool // one per OnPending call: HasPending as the handler saw it
	kids    *kids
}

// openBG opens a background session, each mutate applied to its Options
// first.
func openBG(t *testing.T, mutate ...func(*Options)) *bg {
	t.Helper()
	f := newRouted(t)
	b := &bg{routed: f, pending: make(chan bool, 64)}
	opts := f.options()
	opts.Background = true
	opts.Sink = b.own.sink
	opts.OnPending = func() { b.pending <- b.s.HasPending() }
	for _, m := range mutate {
		m(&opts)
	}
	b.s = f.open(opts)
	b.kids = watchKids(b.s)
	return b
}

// withEnv makes the session's environment env, read under mu, so a test can
// give it a key mid-session.
func withEnv(env map[string]string, mu *sync.Mutex) func(*Options) {
	return func(o *Options) {
		o.Getenv = func(k string) string {
			mu.Lock()
			defer mu.Unlock()
			return env[k]
		}
	}
}

// bgPart is one agent call asking for the background, its arguments a task's.
func bgPart(t *testing.T, id, description, prompt string, more ...string) []fantasy.StreamPart {
	t.Helper()
	args := task(description, prompt, more...)
	args["run_in_background"] = true
	return agentPart(t, id, args)
}

// outputPart is one agent_output call for the child id, waiting up to waitMs.
func outputPart(t *testing.T, id, child string, waitMs int) []fantasy.StreamPart {
	t.Helper()
	return callParts(id, "agent_output", input(t, map[string]any{"id": child, "wait_ms": waitMs}))
}

// spawn runs the session's first turn, "go": one step starting a background
// child per prompt — each held by its own worker, streaming "did <prompt>" once
// released — and then "started". It returns the workers and the children's
// ids, in the prompts' order. Every later turn of the session sends "go" as its
// first user message, so its steps are routed under "go" too.
func (b *bg) spawn(t *testing.T, prompts ...string) ([]*worker, []string) {
	t.Helper()
	a := b.routers["test/a"]
	var parts [][]fantasy.StreamPart
	var ws []*worker
	for i, p := range prompts {
		w := newWorker()
		a.route(p, w.step(openText("did "+p), finishText()))
		ws = append(ws, w)
		parts = append(parts, bgPart(t, fmt.Sprintf("a%d", i+1), "job "+p, p))
	}
	a.route("go", callStep(parts...), answerWith("started"))
	var ev events
	if res, err := b.s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("the spawning turn = %+v, %v; want end_turn", res, err)
	}
	var ids []string
	for _, p := range prompts {
		st := startedWith(t, ev.list(), p)
		if !st.Background {
			t.Fatalf("SubagentStarted for %q = %+v; want a background child", p, st)
		}
		ids = append(ids, st.ID)
	}
	return ws, ids
}

// finish lets w's child finish and waits for its result to be pending.
func (b *bg) finish(t *testing.T, w *worker) {
	t.Helper()
	close(w.release)
	if !await(t, b.pending, "the child's result waiting to be delivered") {
		t.Fatal("OnPending's handler found nothing pending")
	}
}

// noPending fails the test if OnPending was called and not yet received.
func (b *bg) noPending(t *testing.T) {
	t.Helper()
	select {
	case v := <-b.pending:
		t.Fatalf("OnPending was called (HasPending %v); want no call", v)
	default:
	}
}

// resultOf is a copy of the result of the background child id.
func resultOf(t *testing.T, s *Session, id string) bgResult {
	t.Helper()
	s.subs.regMu.Lock()
	defer s.subs.regMu.Unlock()
	res := s.subs.results[id]
	if res == nil {
		t.Fatalf("no background result %s", id)
	}
	return *res
}

// block is one result as the model reads it: §3.11's format, spelled out.
func block(id, status, text string) string {
	return `<subagent_result id="` + id + `" type="general-purpose" status="` + status + `">` + "\n" + text + "\n</subagent_result>"
}

// ackText is a background call's acknowledgement for the child id.
func ackText(id string) string {
	return "Started sub-agent " + id + " (general-purpose) in the background. Its result will be delivered to you when it finishes; " +
		"call agent_output with its id to wait for it now."
}

// entryIndex is the index of the transcript entry with id, or -1.
func entryIndex(tr *store.Transcript, id string) int {
	return slices.IndexFunc(tr.Entries, func(e store.Entry) bool { return e.ID == id })
}

// delivered counts the transcript's copies of the result of child id: every
// wrapper naming it, in any entry, a tool result included.
func delivered(tr *store.Transcript, id string) int {
	n := 0
	for _, e := range tr.Entries {
		n += strings.Count(messageText(e.Message), `<subagent_result id="`+id+`"`)
	}
	return n
}

// lastUser is the text of a request's last message, which must be a user one.
func lastUser(t *testing.T, c fantasy.Call) string {
	t.Helper()
	m := c.Prompt[len(c.Prompt)-1]
	if m.Role != fantasy.MessageRoleUser {
		t.Fatalf("the request ends with a %s message: %v", m.Role, promptOf(c))
	}
	return messageText(m)
}

// p47 checks P47's invariant at the start of a step of turn: every
// reservation the turn holds is the step's own — taken at its boundary —
// never an earlier step's, which is committed or given back by now. It runs
// on the turn's goroutine, so it reports with Errorf.
func p47(t *testing.T, s *Session, turn, step int) {
	s.subs.regMu.Lock()
	defer s.subs.regMu.Unlock()
	for id, res := range s.subs.results {
		if res.state == resultReserved && res.own.turn == turn && res.own.step != step {
			t.Errorf("at turn %d's step %d, result %s is still reserved by its step %d", turn, step, id, res.own.step)
		}
	}
}

// oneRow is the usage row of a child on test/a that finished n steps.
func oneRow(n int64) []ModelUsage {
	return []ModelUsage{{Provider: "test", Model: "test/a", WireModel: "wire-a", Usage: oneStep(n)}}
}

// TestBackgroundDeliveryAtStepBoundary (A13): a background call returns its
// acknowledgement at once — not an error, no usage — and its child runs on.
// The child finishes while the parent's second step runs, and the parent's
// next step boundary takes its result up: one user part after that step's
// input, never a steer — no Steered, nothing unanswered — written by the
// third step's append as an entry of results, marked and carrying the child's
// usage, which commits it by that entry's id. The spawning call's entry
// carries no usage, and the child's own events and finish went to the
// session's sink, not the turn's.
func TestBackgroundDeliveryAtStepBoundary(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	w := newWorker()
	atStep2, ready := make(chan struct{}), make(chan struct{})
	a.route("go",
		callStep(bgPart(t, "a1", "scan", "child one")),
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			// Past step 2's boundary: the result is ready only after it.
			close(atStep2)
			<-ready
			callStep(globPart("g1"))(ctx, yield)
		},
		answerWith("thanks"))
	a.route("child one", w.step(openText("found it"), finishText()))
	var ev events
	out := start(context.Background(), b.s, "go", ev.sink)
	await(t, w.reached, "the background child mid-step")
	await(t, atStep2, "the parent's second step")
	b.finish(t, w)
	close(ready)
	got := await(t, out, "the turn")
	if got.err != nil || got.res.StopReason != StopEndTurn || len(got.res.Unanswered) != 0 {
		t.Fatalf("Run = %+v, %v; want end_turn, nothing unanswered", got.res, got.err)
	}

	evs := ev.list()
	st := startedWith(t, evs, "child one")
	if !st.Background || st.CallID != "t1.1.1" {
		t.Fatalf("SubagentStarted = %+v; want the background child of t1.1.1", st)
	}
	if ack := callResult(t, evs, "t1.1.1"); ack.Text != ackText(st.ID) || ack.IsError || ack.Child != nil {
		t.Fatalf("the spawning call = %+v; want the acknowledgement, no error, no usage", ack)
	}
	if n, m := len(of[SubagentEvent](evs)), len(of[SubagentFinished](evs)); n != 0 || m != 0 || len(of[Steered](evs)) != 0 {
		t.Fatalf("the turn's sink had %d child events, %d finishes and %d steers; want none", n, m, len(of[Steered](evs)))
	}
	own := b.own.list()
	if fin := finishedOf(t, own, st.ID); fin.Status != SubagentCompleted || fin.Text != "found it" || fin.Usage != oneStep(1) {
		t.Fatalf("SubagentFinished = %+v; want completed, its answer, one step's usage", fin)
	}
	if n := len(of[SubagentEvent](own)); n == 0 {
		t.Fatal("the child's own events did not reach the session's sink")
	}

	want := block(st.ID, SubagentCompleted, "found it")
	reqs := a.requests("go")
	if len(reqs) != 3 || lastUser(t, reqs[2]) != want || strings.Contains(requestText(reqs[1], true), "<subagent_result") {
		t.Fatalf("the parent's requests: %d; want the result in the third alone, last: %v", len(reqs), promptOf(reqs[len(reqs)-1]))
	}
	tr := transcript(t, b.s)
	if lines := heads(entries(tr)); len(lines) != 7 || lines[5] != "user test/a high: "+want || lines[6] != "assistant test/a high end_turn: thanks" {
		t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
	}
	results := tr.Entries[5]
	if !results.SubagentResults || !reflect.DeepEqual(results.SubagentUsage, oneRow(1)) || tr.Entries[2].SubagentUsage != nil {
		t.Fatalf("the results entry = %+v; the spawning step's tool entry rows %+v; want the one marked with the child's row, the other none",
			results.MessageEntry, tr.Entries[2].SubagentUsage)
	}
	if res := resultOf(t, b.s, st.ID); res.state != resultCommitted || res.entry != results.ID {
		t.Fatalf("the result is %v, committed by %q; want committed by the results entry %s", res.state, res.entry, results.ID)
	}
	if d := of[StepDone](evs); len(d) != 3 || d[2].SubagentUsage != nil || d[0].SubagentUsage != nil {
		t.Fatalf("StepDones %+v; want three, carrying no rows: a delivered result's usage is its entry's", d)
	}
	settled(t, b.s)
}

// TestBackgroundDeliveryAtStepZero (A13): a result that becomes ready between
// turns is taken up at the next turn's step 0, after the person's prompt, and
// written as its own entry right after the prompt's.
func TestBackgroundDeliveryAtStepZero(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	if res := resultOf(t, b.s, ids[0]); res.state != resultRunning {
		t.Fatalf("after the spawning turn the result is %v; want running", res.state)
	}
	b.finish(t, ws[0])
	a.route("go", answerWith("got it"))
	var ev events
	res, err := b.s.Run(context.Background(), "next", ev.sink)
	if err != nil || res.StopReason != StopEndTurn || len(res.Unanswered) != 0 {
		t.Fatalf("Run = %+v, %v; want end_turn", res, err)
	}
	want := block(ids[0], SubagentCompleted, "did child one")
	reqs := a.requests("go")
	prompt := promptOf(reqs[len(reqs)-1])
	if prompt[len(prompt)-2] != "user: next" || prompt[len(prompt)-1] != "user: "+want {
		t.Fatalf("the turn's request: %v; want the prompt, then the result", prompt)
	}
	tr := transcript(t, b.s)
	n := len(tr.Entries)
	if lines := entries(tr); lines[n-3] != "user test/a high: next" || lines[n-2] != "user test/a high: "+want || !tr.Entries[n-2].SubagentResults {
		t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
	}
	if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.entry != tr.Entries[n-2].ID {
		t.Fatalf("the result is %v by %q; want committed by its entry", r.state, r.entry)
	}
	b.noPending(t)
	settled(t, b.s)
}

// TestBackgroundDeliveryByWake (A13): Wake is a turn whose prompt is the
// waiting results: persisted as its user entry, marked, with their usage,
// which commits them. A second Wake, with nothing waiting, writes and emits
// nothing, spends no turn number, and says so (ErrNothingPending).
func TestBackgroundDeliveryByWake(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	b.finish(t, ws[0])
	a.route("go", answerWith("reacting"))
	var ev events
	res, err := b.s.Wake(context.Background(), ev.sink)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Wake = %+v, %v; want end_turn", res, err)
	}
	want := block(ids[0], SubagentCompleted, "did child one")
	reqs := a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != want {
		t.Fatalf("the wake's prompt = %q; want the result", got)
	}
	tr := transcript(t, b.s)
	n := len(tr.Entries)
	wake := tr.Entries[n-2]
	if messageText(wake.Message) != want || !wake.SubagentResults || !reflect.DeepEqual(wake.SubagentUsage, oneRow(1)) ||
		entries(tr)[n-1] != "assistant test/a high end_turn: reacting" {
		t.Fatalf("transcript:\n%s", strings.Join(entries(tr), "\n"))
	}
	if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.entry != wake.ID {
		t.Fatalf("the result is %v by %q; want committed by the wake's entry %s", r.state, r.entry, wake.ID)
	}

	b.s.mu.Lock()
	turns := b.s.turns
	b.s.mu.Unlock()
	var none events
	if _, err := b.s.Wake(context.Background(), none.sink); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("a second Wake = %v; want ErrNothingPending", err)
	}
	b.s.mu.Lock()
	after := b.s.turns
	b.s.mu.Unlock()
	if len(none.list()) != 0 || len(transcript(t, b.s).Entries) != n || after != turns {
		t.Fatalf("the empty wake emitted %d events, left %d entries (was %d), turns %d (was %d); want nothing changed",
			len(none.list()), len(transcript(t, b.s).Entries), n, after, turns)
	}
	settled(t, b.s)
}

// TestInternalResultsNeverUnanswered (A13): a result is never a steer. One
// that finishes during a turn's final step — after its last boundary — is
// neither in that turn's Result.Unanswered nor lost: it stays pending for
// the next consumer. One a failed step had taken is given back, pending, and
// not unanswered either.
func TestInternalResultsNeverUnanswered(t *testing.T) {
	t.Run("finishes during the final step", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		w := newWorker()
		atFinal, ready := make(chan struct{}), make(chan struct{})
		a.route("go",
			callStep(bgPart(t, "a1", "scan", "child one")),
			func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				close(atFinal)
				<-ready
				answerWith("done")(ctx, yield)
			},
			answerWith("reacting"))
		a.route("child one", w.step(openText("found it"), finishText()))
		var ev events
		out := start(context.Background(), b.s, "go", ev.sink)
		await(t, atFinal, "the parent's final step")
		b.finish(t, w)
		close(ready)
		got := await(t, out, "the turn")
		if got.err != nil || got.res.StopReason != StopEndTurn || len(got.res.Unanswered) != 0 || len(of[Steered](ev.list())) != 0 {
			t.Fatalf("Run = %+v, %v, steered %d; want end_turn, nothing unanswered or steered", got.res, got.err, len(of[Steered](ev.list())))
		}
		id := startedWith(t, ev.list(), "child one").ID
		if r := resultOf(t, b.s, id); r.state != resultPending || !b.s.HasPending() {
			t.Fatalf("after the turn the result is %v; want pending", r.state)
		}
		if res, err := b.s.Wake(context.Background(), nil); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Wake = %+v, %v", res, err)
		}
		if tr := transcript(t, b.s); delivered(tr, id) != 1 || resultOf(t, b.s, id).state != resultCommitted {
			t.Fatalf("the result was delivered %d times; want once, by the wake", delivered(tr, id))
		}
	})
	t.Run("taken by a step that failed", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one")
		b.finish(t, ws[0])
		a.route("go", reply(errorPart(errors.New("the provider went away"))))
		res, err := b.s.Run(context.Background(), "next", nil)
		if err == nil || len(res.Unanswered) != 0 {
			t.Fatalf("Run = %+v, %v; want a failure with nothing unanswered", res, err)
		}
		if !await(t, b.pending, "the result given back") {
			t.Fatal("OnPending's handler found nothing pending after the restore")
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultPending {
			t.Fatalf("the result is %v; want pending again", r.state)
		}
	})
}

// TestDeliveryStatesRestore (A13): a consumer that writes nothing gives its
// reservations back as they were — a user turn's failed step and a cancelled
// one alike, pending to pending and suspended to suspended — with OnPending
// said once a result is pending again; a wake's go to suspended whatever
// they were, and nothing is said.
func TestDeliveryStatesRestore(t *testing.T) {
	t.Run("a failed consumer", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one")
		b.finish(t, ws[0])
		a.route("go", reply(errorPart(errors.New("down"))))
		if _, err := b.s.Run(context.Background(), "next", nil); err == nil {
			t.Fatal("the turn did not fail")
		}
		if !await(t, b.pending, "the restore's OnPending") || resultOf(t, b.s, ids[0]).state != resultPending {
			t.Fatalf("the result is %v; want pending", resultOf(t, b.s, ids[0]).state)
		}
	})
	t.Run("a cancelled consumer", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one")
		b.finish(t, ws[0])
		g := newGate()
		a.route("go", g.hold(nil, finish(fantasy.FinishReasonStop)))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, b.s, "next", nil)
		await(t, g.reached, "the step carrying the result in its provider")
		if r := resultOf(t, b.s, ids[0]); r.state != resultReserved || r.own != (owner{turn: 2, step: 1}) {
			t.Fatalf("mid-step the result is %v by %+v; want reserved by turn 2's step 1", r.state, r.own)
		}
		cancel()
		if got := await(t, out, "the cancelled turn"); got.err != nil || !only(got.res, StopCancelled) {
			t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
		}
		if !await(t, b.pending, "the restore's OnPending") || resultOf(t, b.s, ids[0]).state != resultPending {
			t.Fatalf("the result is %v; want pending", resultOf(t, b.s, ids[0]).state)
		}
	})
	t.Run("a wake's go to suspended, and a user turn keeps them so", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one")
		b.finish(t, ws[0])
		a.route("go", reply(errorPart(errors.New("down"))))
		if _, err := b.s.Wake(context.Background(), nil); err == nil {
			t.Fatal("the wake did not fail")
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultSuspended || b.s.HasPending() {
			t.Fatalf("after the failed wake the result is %v (HasPending %v); want suspended", r.state, b.s.HasPending())
		}
		b.noPending(t)
		// A person's turn takes it at step 0, fails, and gives it back as it was.
		a.route("go", reply(errorPart(errors.New("down again"))))
		if _, err := b.s.Run(context.Background(), "next", nil); err == nil {
			t.Fatal("the turn did not fail")
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultSuspended {
			t.Fatalf("after the failed turn the result is %v; want suspended, as it was", r.state)
		}
		b.noPending(t)
	})
}

// TestSameStepAgentOutputNeverWaits (A13, P41, P47): agent_output never waits
// on a reservation. Two calls in the step whose boundary took a result both
// answer at once that it is in the input; two calls on a result that became
// ready after their step's boundary deliver it once — the first to reserve it
// — and the other answers at once. At every step every reservation is the
// step's own (P47).
func TestSameStepAgentOutputNeverWaits(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one", "child two")
	b.finish(t, ws[0])
	atStep2, ready := make(chan struct{}), make(chan struct{})
	a.route("go",
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			p47(t, b.s, 2, 1)
			callStep(outputPart(t, "o1", ids[0], 600000), outputPart(t, "o2", ids[0], 600000))(ctx, yield)
		},
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			p47(t, b.s, 2, 2)
			close(atStep2)
			<-ready
			callStep(outputPart(t, "o3", ids[1], 600000), outputPart(t, "o4", ids[1], 600000))(ctx, yield)
		},
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			p47(t, b.s, 2, 3)
			callStep(outputPart(t, "o5", ids[0], 0), outputPart(t, "o6", ids[1], 0))(ctx, yield)
		},
		answerWith("done"))
	var ev events
	out := start(context.Background(), b.s, "next", ev.sink)
	await(t, atStep2, "the parent's second step")
	b.finish(t, ws[1])
	close(ready)
	if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", got.res, got.err)
	}
	evs := ev.list()
	for _, id := range []string{"t2.1.1", "t2.1.2"} {
		if r := callResult(t, evs, id); r.Text != outputIncluded || r.IsError || r.Child != nil {
			t.Fatalf("%s = %+v; want %q at once", id, r, outputIncluded)
		}
	}
	two := block(ids[1], SubagentCompleted, "did child two")
	r3, r4 := callResult(t, evs, "t2.2.1"), callResult(t, evs, "t2.2.2")
	if got := []string{r3.Text, r4.Text}; !slices.Equal(got, []string{two, outputIncluded}) && !slices.Equal(got, []string{outputIncluded, two}) {
		t.Fatalf("the two calls on the second result = %q, %q; want it once and the other told it is included", r3.Text, r4.Text)
	}
	for _, id := range []string{"t2.3.1", "t2.3.2"} {
		if r := callResult(t, evs, id); r.Text != outputDelivered {
			t.Fatalf("%s = %+v; want %q", id, r, outputDelivered)
		}
	}
	tr := transcript(t, b.s)
	for _, id := range ids {
		if n := delivered(tr, id); n != 1 || resultOf(t, b.s, id).state != resultCommitted {
			t.Fatalf("result %s delivered %d times, %v; want once, committed", id, n, resultOf(t, b.s, id).state)
		}
	}
	// The second result's usage rides on the tool entry of the call that read
	// it, and on nothing else.
	tool2 := tr.Entries[entryIndex(tr, resultOf(t, b.s, ids[1]).entry)]
	if tool2.Message.Role != fantasy.MessageRoleTool || !reflect.DeepEqual(tool2.SubagentUsage, oneRow(1)) {
		t.Fatalf("the second result's entry (%s) carries %+v; want the tool entry with its row", tool2.Message.Role, tool2.SubagentUsage)
	}
	settled(t, b.s)
}

// TestWakeAgentOutputOnOwnResult (A13, P47): a wake's model calling
// agent_output on the result it was woken with is told at once it is in its
// input: the wake's first step owns it from before the model is asked.
func TestWakeAgentOutputOnOwnResult(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	b.finish(t, ws[0])
	a.route("go",
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			p47(t, b.s, 2, 1)
			callStep(outputPart(t, "o1", ids[0], 600000))(ctx, yield)
		},
		answerWith("ok"))
	var ev events
	if res, err := b.s.Wake(context.Background(), ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Wake = %+v, %v", res, err)
	}
	if r := callResult(t, ev.list(), "t2.1.1"); r.Text != outputIncluded {
		t.Fatalf("the wake's agent_output = %+v; want %q", r, outputIncluded)
	}
	tr := transcript(t, b.s)
	if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || delivered(tr, ids[0]) != 1 || !tr.Entries[entryIndex(tr, r.entry)].SubagentResults {
		t.Fatalf("the result is %v, delivered %d times; want committed once, by the wake's entry", r.state, delivered(tr, ids[0]))
	}
}

// TestAppendCommitsOnlyWrittenReservations (A13, P42, P48): each save path
// commits exactly the reservations it wrote, by the ids of the entries that
// hold them — a step's leading entry of results, the tool entry holding an
// agent_output call's own result — and a success is not undone by the cancel
// after it. The paths: a finished step, the partial answer a cancel cut
// short, and a step the runner synthesizes (driven directly: Fantasy v0.43.2
// never takes that course), where an agent_output call whose result was never
// recorded commits nothing and its result is given back.
func TestAppendCommitsOnlyWrittenReservations(t *testing.T) {
	t.Run("a finished step", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one", "child two")
		b.finish(t, ws[0])
		atStep, ready := make(chan struct{}), make(chan struct{})
		a.route("go",
			func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				close(atStep)
				<-ready
				callStep(outputPart(t, "o1", ids[1], 0))(ctx, yield)
			},
			answerWith("done"))
		var ev events
		out := start(context.Background(), b.s, "next", ev.sink)
		await(t, atStep, "the step after its boundary")
		b.finish(t, ws[1])
		close(ready)
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatal(got.err)
		}
		tr := transcript(t, b.s)
		d := of[StepDone](ev.list())[0]
		n := len(d.Entries)
		// user next, the results, the call, its result: the last four ids.
		if n < 4 || resultOf(t, b.s, ids[0]).entry != d.Entries[n-3] || resultOf(t, b.s, ids[1]).entry != d.Entries[n-1] {
			t.Fatalf("the step wrote %v; want the first result committed by its leading entry and the second by the tool entry", d.Entries)
		}
		lead, toolEntry := tr.Entries[entryIndex(tr, d.Entries[n-3])], tr.Entries[entryIndex(tr, d.Entries[n-1])]
		if !lead.SubagentResults || !reflect.DeepEqual(lead.SubagentUsage, oneRow(1)) || !reflect.DeepEqual(toolEntry.SubagentUsage, oneRow(1)) ||
			d.SubagentUsage != nil {
			t.Fatalf("rows: results entry %+v, tool entry %+v, StepDone %+v; want one each on the entries, none on the StepDone",
				lead.SubagentUsage, toolEntry.SubagentUsage, d.SubagentUsage)
		}
	})
	t.Run("the partial answer", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one")
		b.finish(t, ws[0])
		g := newGate()
		a.route("go", g.hold(openText("partial"), finishText()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, b.s, "next", nil)
		await(t, g.reached, "the answer streaming")
		cancel()
		if got := await(t, out, "the cancelled turn"); got.err != nil || got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
		}
		tr := transcript(t, b.s)
		n := len(tr.Entries)
		want := block(ids[0], SubagentCompleted, "did child one")
		if lines := entries(tr); lines[n-3] != "user test/a high: next" || lines[n-2] != "user test/a high: "+want ||
			lines[n-1] != "assistant test/a high cancelled interrupted: partial" {
			t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.entry != tr.Entries[n-2].ID {
			t.Fatalf("the result is %v by %q; want committed by the entry the interrupted save wrote", r.state, r.entry)
		}
		b.noPending(t)
	})
	t.Run("a synthesized step", func(t *testing.T) {
		b := openBG(t)
		ws, ids := b.spawn(t, "one", "two", "three")
		b.finish(t, ws[0])
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		outIn := func(id string) string { return input(t, map[string]any{"id": id, "wait_ms": 0}) }
		b.s.newAgent = func(_ fantasy.LanguageModel, _ string, tools []fantasy.AgentTool) fantasy.Agent {
			return fakeAgent{tools: tools, run: func(ctx context.Context, tools []fantasy.AgentTool, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				// Step 0's boundary takes the first result; the other two become
				// ready after it, for the two agent_output calls.
				_, prep, _ := c.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{StepNumber: 0, Messages: []fantasy.Message{fantasy.NewUserMessage("next")}})
				if len(prep.Messages) != 2 {
					t.Errorf("step 0's input = %d messages; want the prompt and the result", len(prep.Messages))
				}
				b.finish(t, ws[1])
				b.finish(t, ws[2])
				_ = c.OnStepStart(0)
				_ = c.OnTextDelta("0", "checking")
				_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c1", ToolName: "agent_output", Input: outIn(ids[1])})
				_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c2", ToolName: "agent_output", Input: outIn(ids[2])})
				out := tools[slices.IndexFunc(tools, func(tl fantasy.AgentTool) bool { return tl.Info().Name == "agent_output" })]
				r1, _ := out.Run(ctx, fantasy.ToolCall{ID: "c1", Name: "agent_output", Input: outIn(ids[1])})
				_ = c.OnToolResult(fantasy.ToolResultContent{ToolCallID: "c1", ToolName: "agent_output",
					Result: fantasy.ToolResultOutputContentText{Text: r1.Content}, ClientMetadata: r1.Metadata})
				// The second ran and reserved its result, but its result was never
				// recorded: the synthesized step answers it aborted.
				_, _ = out.Run(ctx, fantasy.ToolCall{ID: "c2", Name: "agent_output", Input: outIn(ids[2])})
				cancel()
				return nil, ctx.Err()
			}}
		}
		var ev events
		if res, err := b.s.Run(ctx, "next", ev.sink); err != nil || res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", res, err)
		}
		tr := transcript(t, b.s)
		n := len(tr.Entries)
		lines := entries(tr)
		if lines[n-4] != "user test/a high: next" || lines[n-3] != "user test/a high: "+block(ids[0], SubagentCompleted, "did one") ||
			!strings.HasPrefix(lines[n-2], "assistant test/a high cancelled interrupted: checking [call c1 agent_output") ||
			lines[n-1] != "tool test/a high interrupted: [result c1: "+block(ids[1], SubagentCompleted, "did two")+"] [error c2: "+tool.AbortedText+"]" {
			t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.entry != tr.Entries[n-3].ID {
			t.Fatalf("the first result is %v by %q; want committed by the leading entry", r.state, r.entry)
		}
		if r := resultOf(t, b.s, ids[1]); r.state != resultCommitted || r.entry != tr.Entries[n-1].ID {
			t.Fatalf("the second result is %v by %q; want committed by the tool entry", r.state, r.entry)
		}
		if r := resultOf(t, b.s, ids[2]); r.state != resultPending {
			t.Fatalf("the third result is %v; want pending: its call's result was not written", r.state)
		}
		if !reflect.DeepEqual(tr.Entries[n-1].SubagentUsage, oneRow(1)) {
			t.Fatalf("the synthesized tool entry carries %+v; want the written call's row alone", tr.Entries[n-1].SubagentUsage)
		}
		if !await(t, b.pending, "the third result given back") {
			t.Fatal("OnPending's handler found nothing pending")
		}
	})
}

// TestFailedWakeSuspendsNoLoop (A13, P44): a wake whose provider fails sets
// its results aside: they are not pending, nothing is said, and a second wake
// finds nothing to deliver — no loop. The next turn a person starts delivers
// them at its step 0.
func TestFailedWakeSuspendsNoLoop(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	b.finish(t, ws[0])
	a.route("go", reply(errorPart(errors.New("the provider is down"))))
	if _, err := b.s.Wake(context.Background(), nil); err == nil {
		t.Fatal("the wake did not fail")
	}
	if r := resultOf(t, b.s, ids[0]); r.state != resultSuspended || b.s.HasPending() {
		t.Fatalf("the result is %v, HasPending %v; want suspended and nothing pending", r.state, b.s.HasPending())
	}
	b.noPending(t)
	if _, err := b.s.Wake(context.Background(), nil); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("a second wake = %v; want ErrNothingPending", err)
	}
	a.route("go", answerWith("got it"))
	if res := run(t, b.s, "next"); res.StopReason != StopEndTurn {
		t.Fatalf("the person's turn = %+v", res)
	}
	reqs := a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != block(ids[0], SubagentCompleted, "did child one") {
		t.Fatalf("the person's turn's request ends %q; want the suspended result", got)
	}
	if tr := transcript(t, b.s); resultOf(t, b.s, ids[0]).state != resultCommitted || delivered(tr, ids[0]) != 1 {
		t.Fatalf("the result is %v, delivered %d times; want committed once", resultOf(t, b.s, ids[0]).state, delivered(tr, ids[0]))
	}
}

// TestCleanEmptyWakeSuspends (A13, P51): a wake that ends cleanly having
// persisted nothing — an empty response with usage, or thinking alone — sets
// its results aside as a failed one does: nothing wrote them.
func TestCleanEmptyWakeSuspends(t *testing.T) {
	for name, st := range map[string]step{
		"an empty response with usage": reply(finish(fantasy.FinishReasonStop)),
		"thinking only":                reply(reasoningParts("let me think"), finish(fantasy.FinishReasonStop)),
	} {
		t.Run(name, func(t *testing.T) {
			b := openBG(t)
			a := b.routers["test/a"]
			ws, ids := b.spawn(t, "child one")
			b.finish(t, ws[0])
			before := len(transcript(t, b.s).Entries)
			a.route("go", st)
			if res, err := b.s.Wake(context.Background(), nil); err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("Wake = %+v, %v; want a clean end", res, err)
			}
			if r := resultOf(t, b.s, ids[0]); r.state != resultSuspended || b.s.HasPending() || len(transcript(t, b.s).Entries) != before {
				t.Fatalf("the result is %v, HasPending %v, entries %d (were %d); want suspended, nothing written",
					r.state, b.s.HasPending(), len(transcript(t, b.s).Entries), before)
			}
			b.noPending(t)
		})
	}
}

// TestBackgroundResultCapped (A13, panel astra r2-10): a background child's
// 60 KiB answer is cut when it becomes deliverable — the head kept, the whole
// in a spill file named for the spawning call — since no dispatcher will cut
// it: delivered into a live step and by a wake alike.
func TestBackgroundResultCapped(t *testing.T) {
	long := strings.Repeat(strings.Repeat("x", 59)+"\n", 1024) // 60 KiB
	check := func(t *testing.T, b *bg, text string) {
		t.Helper()
		spill := filepath.Join(b.home, tool.SpillDir, "tool_t1.1.1")
		if len(text) > tool.MaxBytes+2048 || strings.Count(text, "Full output saved to: "+spill) != 1 || !strings.HasSuffix(text, "\n</subagent_result>") {
			t.Fatalf("the delivered result: %d bytes, spill named %v; want it cut once, naming %s", len(text), strings.Contains(text, spill), spill)
		}
		if got, err := os.ReadFile(spill); err != nil || string(got) != long {
			t.Fatalf("the spill file holds %d bytes (%v); want the whole answer", len(got), err)
		}
	}
	t.Run("into a live step", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		w := newWorker()
		atStep2, ready := make(chan struct{}), make(chan struct{})
		a.route("go", callStep(bgPart(t, "a1", "long", "write a lot")),
			func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				close(atStep2)
				<-ready
				callStep(globPart("g1"))(ctx, yield)
			},
			answerWith("ok"))
		a.route("write a lot", w.step(nil, cat(textParts(long), finish(fantasy.FinishReasonStop))))
		out := start(context.Background(), b.s, "go", nil)
		await(t, atStep2, "the parent's second step")
		b.finish(t, w)
		close(ready)
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatal(got.err)
		}
		reqs := a.requests("go")
		check(t, b, lastUser(t, reqs[2]))
	})
	t.Run("by a wake", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		w := newWorker()
		a.route("go", callStep(bgPart(t, "a1", "long", "write a lot")), answerWith("started"), answerWith("ok"))
		a.route("write a lot", w.step(nil, cat(textParts(long), finish(fantasy.FinishReasonStop))))
		run(t, b.s, "go")
		b.finish(t, w)
		if _, err := b.s.Wake(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		reqs := a.requests("go")
		check(t, b, lastUser(t, reqs[len(reqs)-1]))
	})
}

// TestAgentOutputWait (A13): agent_output waits for a child still running —
// holding no lock, and without cancelling it — and delivers its result the
// moment it is ready; a wait that runs out says the child is still running,
// not an error, and so does a wait of 0 at once. An id that names no
// background child of the session is invalid_input listing the ones still to
// be delivered.
func TestAgentOutputWait(t *testing.T) {
	t.Run("a wait that gets the result", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one")
		waiting := make(chan string, 1)
		b.s.subs.seams.outputWaiting = func(id string) { waiting <- id }
		a.route("go", callStep(outputPart(t, "o1", ids[0], 600000)), answerWith("ok"))
		var ev events
		out := start(context.Background(), b.s, "next", ev.sink)
		if id := await(t, waiting, "the agent_output call waiting"); id != ids[0] {
			t.Fatalf("the call waits for %q; want %q", id, ids[0])
		}
		close(ws[0].release)
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatal(got.err)
		}
		r := callResult(t, ev.list(), "t2.1.1")
		if r.Text != block(ids[0], SubagentCompleted, "did child one") || r.IsError || r.Child == nil ||
			r.Child.Usage != (tool.Usage{Input: 10, Output: 5, CacheRead: 4}) {
			t.Fatalf("the call = %+v; want the result, with its usage", r)
		}
		if res := resultOf(t, b.s, ids[0]); res.state != resultCommitted {
			t.Fatalf("the result is %v; want committed by the call's tool entry", res.state)
		}
	})
	t.Run("a wait that runs out", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "child one")
		a.route("go", callStep(outputPart(t, "o1", ids[0], 1), outputPart(t, "o2", ids[0], 0)), answerWith("ok"))
		var ev events
		if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"t2.1.1", "t2.1.2"} {
			if r := callResult(t, ev.list(), id); r.Text != "Sub-agent "+ids[0]+" is still running." || r.IsError {
				t.Fatalf("%s = %+v; want still running, not an error", id, r)
			}
		}
		if res := resultOf(t, b.s, ids[0]); res.state != resultRunning || isClosed(ws[0].exited) {
			t.Fatalf("the result is %v; want the child running still, uncancelled", res.state)
		}
		b.finish(t, ws[0])
	})
	t.Run("an unknown id", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		_, ids := b.spawn(t, "child one")
		a.route("fg", answerWith("fg done"))
		a.route("go", callStep(agentPart(t, "f1", task("fg", "fg"))), answerWith("ok"))
		var ev events
		if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
			t.Fatal(err)
		}
		fg := startedWith(t, ev.list(), "fg").ID
		a.route("go", callStep(outputPart(t, "o1", fg, 0), outputPart(t, "o2", "nope", 0)), answerWith("ok"))
		var again events
		if _, err := b.s.Run(context.Background(), "again", again.sink); err != nil {
			t.Fatal(err)
		}
		for id, name := range map[string]string{"t3.1.1": fg, "t3.1.2": "nope"} {
			want := "Unknown sub-agent id `" + name + "`. The background sub-agents whose results are still to be delivered: `" + ids[0] + "`."
			if r := callResult(t, again.list(), id); r.Text != want || r.Class != tool.ClassInvalidInput {
				t.Fatalf("%s = %+v; want %q", id, r, want)
			}
		}
	})
}

// TestBackgroundSurvivesTurnCancel (A13): a turn's cancel does not reach a
// background child — its context is the session's — even one that lands
// while its call is still starting it, after it opened: the acknowledgement
// stands, not "aborted", and the child runs on to its own end.
func TestBackgroundSurvivesTurnCancel(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	w := newWorker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.s.subs.seams.opened = func(string, *Session) { cancel() }
	a.route("go", callStep(bgPart(t, "a1", "scan", "child one")), answerWith("never"))
	a.route("child one", w.step(openText("found it"), finishText()))
	var ev events
	res, err := b.s.Run(ctx, "go", ev.sink)
	if err != nil || res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", res, err)
	}
	st := startedWith(t, ev.list(), "child one")
	if ack := callResult(t, ev.list(), "t1.1.1"); ack.Text != ackText(st.ID) || ack.IsError {
		t.Fatalf("the spawning call = %+v; want its acknowledgement, final", ack)
	}
	await(t, w.reached, "the child running after its parent's turn was cancelled")
	b.finish(t, w)
	if fin := finishedOf(t, b.own.list(), st.ID); fin.Status != SubagentCompleted || fin.Text != "found it" {
		t.Fatalf("SubagentFinished = %+v; want completed on its own", fin)
	}
	settled(t, b.s)
}

// TestHeadlessBackgroundIsForeground (A13): with Options.Background off — the
// default, headless `craze prompt` — run_in_background is accepted and the
// call runs in the foreground: it blocks and returns the child's answer, its
// events are the turn's, and no result waits anywhere.
func TestHeadlessBackgroundIsForeground(t *testing.T) {
	f := newRouted(t)
	var own events
	opts := f.options()
	opts.Sink = own.sink
	s := f.open(opts)
	a := f.routers["test/a"]
	a.route("go", callStep(bgPart(t, "a1", "scan", "child one")), answerWith("done"))
	a.route("child one", answerWith("found it"))
	var ev events
	if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	evs := ev.list()
	st := startedWith(t, evs, "child one")
	if r := callResult(t, evs, "t1.1.1"); st.Background || r.Text != "found it" || r.Child == nil {
		t.Fatalf("SubagentStarted %+v, the call %+v; want a foreground child and its answer, with its usage", st, r)
	}
	if finishedOf(t, evs, st.ID).Status != SubagentCompleted || len(own.list()) != 0 || s.HasPending() || len(s.subs.results) != 0 {
		t.Fatalf("the session's sink had %d events, HasPending %v; want the child's finish on the turn's sink and nothing else", len(own.list()), s.HasPending())
	}
	settled(t, s)
}

// TestMixedUsageAttributedOnce (A13, §3.7's one accounting rule): a
// foreground and a background child in one step. The foreground child's
// usage is on the step's tool entry and its StepDone; the background one's is
// on the entry that delivers its result alone, never the spawning step's nor
// any StepDone. Summed over the transcript, each child counts once, on its
// own model.
func TestMixedUsageAttributedOnce(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	w := newWorker()
	a.route("go", callStep(
		agentPart(t, "f1", task("fg", "on b", "model", "test/b")),
		bgPart(t, "b1", "bg", "on c", "model", "other/c")),
		answerWith("started"), answerWith("got it"))
	b.routers["test/b"].route("on b", answerWith("b done"))
	b.routers["other/c"].route("on c", w.step(openText("c done"), finishText()))
	var ev events
	if _, err := b.s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	b.finish(t, w)
	if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
		t.Fatal(err)
	}
	onB := []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: oneStep(1)}}
	onC := []ModelUsage{{Provider: "other", Model: "other/c", WireModel: "wire-c", Usage: oneStep(1)}}
	tr := transcript(t, b.s)
	if !reflect.DeepEqual(tr.Entries[2].SubagentUsage, onB) {
		t.Fatalf("the spawning step's tool entry carries %+v; want the foreground child's row alone", tr.Entries[2].SubagentUsage)
	}
	var total []ModelUsage
	for _, e := range tr.Entries {
		total = append(total, e.SubagentUsage...)
	}
	if !reflect.DeepEqual(total, append(slices.Clone(onB), onC...)) {
		t.Fatalf("the transcript's rows = %+v; want each child's once", total)
	}
	var steps []ModelUsage
	for _, d := range of[StepDone](ev.list()) {
		steps = append(steps, d.SubagentUsage...)
	}
	if !reflect.DeepEqual(steps, onB) {
		t.Fatalf("the StepDones' rows = %+v; want the foreground child's alone", steps)
	}
}

// TestCoalescedResults (A13): two results waiting at one boundary are one
// user part, in the order they finished, a blank line between them, written
// as one entry whose usage is their rows merged per model.
func TestCoalescedResults(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one", "child two")
	b.finish(t, ws[1])
	b.finish(t, ws[0])
	a.route("go", answerWith("got both"))
	run(t, b.s, "next")
	want := block(ids[1], SubagentCompleted, "did child two") + "\n\n" + block(ids[0], SubagentCompleted, "did child one")
	reqs := a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != want {
		t.Fatalf("the part = %q; want both, in the order they finished", got)
	}
	tr := transcript(t, b.s)
	e := tr.Entries[len(tr.Entries)-2]
	if !e.SubagentResults || !reflect.DeepEqual(e.SubagentUsage, oneRow(2)) ||
		resultOf(t, b.s, ids[0]).entry != e.ID || resultOf(t, b.s, ids[1]).entry != e.ID {
		t.Fatalf("the results entry = %+v; want one entry committing both, one row of both", e.MessageEntry)
	}
}

// TestNoDoubleDelivery (A13): three results, each delivered once, whichever
// consumer met it — a wake, a turn's step 0, two agent_output calls in one
// step — and then every other consumer is told so, or finds nothing.
func TestNoDoubleDelivery(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "one", "two", "three")
	b.finish(t, ws[0])
	a.route("go", callStep(outputPart(t, "o1", ids[0], 0)), answerWith("read one"))
	var ev events
	if _, err := b.s.Wake(context.Background(), ev.sink); err != nil {
		t.Fatal(err)
	}
	b.finish(t, ws[1])
	atStep2, ready := make(chan struct{}), make(chan struct{})
	a.route("go",
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			p47(t, b.s, 3, 1)
			callStep(outputPart(t, "o2", ids[0], 0), outputPart(t, "o3", ids[1], 0))(ctx, yield)
		},
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			p47(t, b.s, 3, 2)
			close(atStep2)
			<-ready
			callStep(outputPart(t, "o4", ids[2], 600000), outputPart(t, "o5", ids[2], 600000))(ctx, yield)
		},
		answerWith("read all"))
	out := start(context.Background(), b.s, "next", ev.sink)
	await(t, atStep2, "the turn's second step")
	b.finish(t, ws[2])
	close(ready)
	if got := await(t, out, "the turn"); got.err != nil {
		t.Fatal(got.err)
	}
	evs := ev.list()
	for id, want := range map[string]string{"t2.1.1": outputIncluded, "t3.1.1": outputDelivered, "t3.1.2": outputIncluded} {
		if r := callResult(t, evs, id); r.Text != want {
			t.Fatalf("%s = %q; want %q", id, r.Text, want)
		}
	}
	tr := transcript(t, b.s)
	for _, id := range ids {
		if n := delivered(tr, id); n != 1 || resultOf(t, b.s, id).state != resultCommitted {
			t.Fatalf("result %s: delivered %d times, %v; want once, committed", id, n, resultOf(t, b.s, id).state)
		}
	}
	if _, err := b.s.Wake(context.Background(), nil); !errors.Is(err, ErrNothingPending) || b.s.HasPending() {
		t.Fatalf("a wake after = %v; want ErrNothingPending", err)
	}
	settled(t, b.s)
}

// TestForegroundWaiter (§3.11's slots, panel CodeRabbit 10, astra r2-15): the
// four slots are shared. A foreground call waits only behind another
// foreground holder, and starts when that one leaves; with every slot held
// by background children it fails fast, as a background call always does,
// with the text the model can act on; and a waiter judges again at every
// change of occupancy — here, the last foreground holder leaving as a
// background child takes its slot, which leaves it nothing to wait for.
func TestForegroundWaiter(t *testing.T) {
	t.Run("waits behind a foreground holder", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, _ := b.spawn(t, "bg 1", "bg 2", "bg 3")
		pool, reached := workers(a, "fg", 2)
		waiting := make(chan string, 2)
		b.s.subs.seams.waiting = func(c tool.SubagentCall) { waiting <- c.Prompt }
		a.route("go", callStep(agentPart(t, "f1", task("x", "fg 1")), agentPart(t, "f2", task("y", "fg 2"))), answerWith("ok"))
		out := start(context.Background(), b.s, "next", nil)
		holder := await(t, reached, "a foreground child holding the last slot")
		waiter := await(t, waiting, "the other foreground call waiting")
		if holder.name == waiter {
			t.Fatalf("%q both holds and waits", waiter)
		}
		close(holder.release)
		if got := await(t, reached, "the waiter's child, once the foreground holder left"); got.name != waiter {
			t.Fatalf("%q ran; want the waiter %q", got.name, waiter)
		}
		close(pool[waiter].release)
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatal(got.err)
		}
		for _, w := range ws {
			b.finish(t, w)
		}
		settled(t, b.s)
	})
	t.Run("fails fast when every holder is background", func(t *testing.T) {
		b := openBG(t)
		a := b.routers["test/a"]
		ws, _ := b.spawn(t, "bg 1", "bg 2", "bg 3", "bg 4")
		var opened []string
		b.s.subs.seams.opened = func(id string, _ *Session) { opened = append(opened, id) }
		a.route("go", callStep(agentPart(t, "f1", task("x", "fg")), bgPart(t, "b5", "y", "bg 5")), answerWith("ok"))
		var ev events
		if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
			t.Fatal(err)
		}
		want := "All 4 sub-agent slots are in use; wait for one with agent_output or stop one."
		for _, id := range []string{"t2.1.1", "t2.1.2"} {
			if r := callResult(t, ev.list(), id); r.Text != want || r.Class != tool.ClassToolError {
				t.Fatalf("%s = %+v; want %q at once", id, r, want)
			}
		}
		if len(opened) != 0 {
			t.Fatalf("%d children opened with every slot held", len(opened))
		}
		for _, w := range ws {
			b.finish(t, w)
		}
		settled(t, b.s)
	})
	t.Run("judges again at every change", func(t *testing.T) {
		b := openBG(t)
		r := b.s.subs
		ctx := context.Background()
		for range 3 {
			if r.take(ctx, tool.SubagentCall{}, true) != slotTaken {
				t.Fatal("a background call found no free slot")
			}
		}
		if r.take(ctx, tool.SubagentCall{}, false) != slotTaken {
			t.Fatal("the foreground holder found no free slot")
		}
		waiting := make(chan struct{}, 2)
		r.seams.waiting = func(tool.SubagentCall) { waiting <- struct{}{} }
		got := make(chan slotOutcome, 1)
		go func() { got <- r.take(ctx, tool.SubagentCall{}, false) }()
		await(t, waiting, "the foreground call waiting")
		// The last foreground holder leaves and a background call takes its
		// slot, in one change: the waiter has nothing left to wait for.
		r.regMu.Lock()
		r.fg, r.bg = r.fg-1, r.bg+1
		r.changedLocked()
		r.regMu.Unlock()
		if o := await(t, got, "the waiter judging again"); o != slotBusy {
			t.Fatalf("the waiter = %v; want busy once every holder is background", o)
		}
		// Control: a waiter behind a foreground holder takes the slot it gives back.
		r.regMu.Lock()
		r.fg, r.bg = r.fg+1, r.bg-1
		r.regMu.Unlock()
		go func() { got <- r.take(ctx, tool.SubagentCall{}, false) }()
		await(t, waiting, "a second waiter")
		r.release()
		if o := await(t, got, "the second waiter"); o != slotTaken {
			t.Fatalf("the second waiter = %v; want the slot the holder gave back", o)
		}
		r.release()
		for range 3 {
			r.releaseSlot(true)
		}
		settled(t, b.s)
	})
}

// TestCloseWithBackgroundChildren (A13): Close with two background children
// mid-step. Each is told it is closing — its turn cancelled with the closing
// cause, not an ordinary cancel — and joined: when Close returns, both
// children's goroutines have returned, their stores are closed, nothing is
// registered or held, and each undelivered result has been reported once, as
// a SubagentUndelivered after its SubagentFinished.
func TestCloseWithBackgroundChildren(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	causes := make(chan error, 2)
	reached := make(chan struct{}, 2)
	exited := []chan struct{}{make(chan struct{}), make(chan struct{})}
	for i, p := range []string{"child one", "child two"} {
		a.route(p, func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			defer close(exited[i])
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "0"})
			reached <- struct{}{}
			<-ctx.Done()
			causes <- context.Cause(ctx)
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
		})
	}
	a.route("go", callStep(bgPart(t, "a1", "one", "child one"), bgPart(t, "a2", "two", "child two")), answerWith("started"))
	run(t, b.s, "go")
	await(t, reached, "a child mid-step")
	await(t, reached, "the other child mid-step")
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}
	for i, ch := range exited {
		if !isClosed(ch) {
			t.Fatalf("child %d's step is still running after Close returned", i+1)
		}
	}
	for range 2 {
		if cause := <-causes; !errors.Is(cause, tool.ErrClosing) {
			t.Fatalf("a child's turn ended with cause %v; want the closing signal", cause)
		}
	}
	for _, c := range b.kids.all() {
		if !storeClosed(c.store) {
			t.Fatalf("child %s's store is still open", c.ID())
		}
	}
	settled(t, b.s)
	own := b.own.list()
	undelivered := of[SubagentUndelivered](own)
	if len(undelivered) != 2 || b.s.HasPending() {
		t.Fatalf("SubagentUndelivered %+v; want one per child, and nothing pending after Close", undelivered)
	}
	for _, u := range undelivered {
		fin := slices.IndexFunc(own, func(ev Event) bool { f, ok := ev.(SubagentFinished); return ok && f.ID == u.ID })
		rep := slices.IndexFunc(own, func(ev Event) bool { x, ok := ev.(SubagentUndelivered); return ok && x.ID == u.ID })
		if fin < 0 || fin > rep || own[fin].(SubagentFinished).Status != SubagentCancelled || u.Type != "general-purpose" || u.Usage != nil {
			t.Fatalf("child %s: finished at %d, reported at %d (%+v); want cancelled, then reported with no usage", u.ID, fin, rep, u)
		}
	}
}

// TestCancelSubagentStopsBackgroundChild (A13, §3.10): the user's stop of a
// background child is the stop of any child — cancelled, "stopped by the
// user", what it had got to — and its result is delivered like any other.
func TestCancelSubagentStopsBackgroundChild(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	await(t, ws[0].reached, "the child mid-step")
	if !stopChild(b.s, ids[0]) {
		t.Fatal("the stop was refused")
	}
	if !await(t, b.pending, "the stopped child's result") {
		t.Fatal("nothing pending after the stop")
	}
	fin := finishedOf(t, b.own.list(), ids[0])
	if fin.Status != SubagentCancelled || fin.Error != subagentStoppedLabel {
		t.Fatalf("SubagentFinished = %+v; want cancelled, stopped by the user", fin)
	}
	a.route("go", answerWith("noted"))
	run(t, b.s, "next")
	want := block(ids[0], SubagentCancelled, "The user stopped this sub-agent before it finished.\n\nIts last output was:\ndid child one")
	reqs := a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != want {
		t.Fatalf("the delivered result = %q; want %q", got, want)
	}
	settled(t, b.s)
}

// TestOnPendingHoldsNoLock (A13): OnPending is called with no lock of the
// harness's held — from a finishing child's goroutine and from a turn giving
// a result back alike — so its handler may call back into the session: the
// fixture's asks HasPending, and this one also Redact, SteerToken and Mode,
// every one of which takes a lock the runner or the turn holds at some point.
func TestOnPendingHoldsNoLock(t *testing.T) {
	var s *Session
	seen := make(chan bool, 4)
	b := openBG(t, func(o *Options) {
		o.OnPending = func() {
			_ = s.Redact(canary)
			_ = s.SteerToken()
			_ = s.Mode()
			seen <- s.HasPending()
		}
	})
	s = b.s
	a := b.routers["test/a"]
	ws, _ := b.spawn(t, "child one")
	close(ws[0].release)
	if !await(t, seen, "OnPending from the child's goroutine") {
		t.Fatal("the handler saw nothing pending")
	}
	a.route("go", reply(errorPart(errors.New("down"))))
	if _, err := s.Run(context.Background(), "next", nil); err == nil {
		t.Fatal("the turn did not fail")
	}
	if !await(t, seen, "OnPending from the turn's restore") {
		t.Fatal("the handler saw nothing pending after the restore")
	}
}

// TestResultReplayRedacted (astra r14, major 3; the replay canary): a result
// holding a string no session knows as a key is committed as it is. The
// session then learns it (a switch), and the next request replays the entry
// of results with the marker in its place, as it would a tool result; the
// person's prompt that held it too is replayed as it was typed (the control:
// only an entry of results is redacted on replay).
func TestResultReplayRedacted(t *testing.T) {
	const late = "sk-learned-after-the-result"
	env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
	var mu sync.Mutex
	b := openBG(t, withEnv(env, &mu))
	a := b.routers["test/a"]
	w := newWorker()
	a.route("go", callStep(bgPart(t, "a1", "scan", "child one")), answerWith("started"), answerWith("got it"))
	a.route("child one", w.step(openText("the value is "+late), finishText()))
	run(t, b.s, "go")
	b.finish(t, w)
	run(t, b.s, "remember "+late)
	tr := transcript(t, b.s)
	if e := tr.Entries[len(tr.Entries)-2]; !e.SubagentResults || !strings.Contains(messageText(e.Message), late) {
		t.Fatalf("the committed results entry = %q; want it holding the string, no key yet", messageText(e.Message))
	}
	mu.Lock()
	env["NOKEY_API_KEY"] = late
	mu.Unlock()
	if err := b.s.SetModel("nokey/d"); err != nil {
		t.Fatal(err)
	}
	d := b.routers["nokey/d"]
	d.route("go", answerWith("ok"))
	run(t, b.s, "later")
	req := d.requests("go")[0]
	var results, typed string
	for _, m := range req.Prompt {
		switch text := messageText(m); {
		case strings.HasPrefix(text, "<subagent_result"):
			results = text
		case strings.HasPrefix(text, "remember "):
			typed = text
		}
	}
	if strings.Contains(results, late) || !strings.Contains(results, "the value is "+redact.Marker) {
		t.Fatalf("the replayed results = %q; want the marker in place of the key", results)
	}
	if typed != "remember "+late {
		t.Fatalf("control: the person's prompt replayed as %q; want it as typed", typed)
	}
}

// TestResultRedactedWhenReserved (astra r14, major 3): a result made
// deliverable before the session knew a key holds it as it is; the key is
// learned while the result waits, and the consumer that takes it — a turn's
// step 0, or a wake — redacts it again as it takes it: the model and the
// transcript get the marker.
func TestResultRedactedWhenReserved(t *testing.T) {
	const late = "sk-learned-while-it-waited"
	for _, byWake := range []bool{false, true} {
		t.Run(fmt.Sprintf("wake=%v", byWake), func(t *testing.T) {
			env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
			var mu sync.Mutex
			b := openBG(t, withEnv(env, &mu))
			a := b.routers["test/a"]
			w := newWorker()
			a.route("go", callStep(bgPart(t, "a1", "scan", "child one")), answerWith("started"))
			a.route("child one", w.step(openText("the value is "+late), finishText()))
			run(t, b.s, "go")
			b.finish(t, w)
			mu.Lock()
			env["NOKEY_API_KEY"] = late
			mu.Unlock()
			if err := b.s.SetModel("test/a"); err != nil {
				t.Fatal(err)
			}
			a.route("go", answerWith("ok"))
			if byWake {
				if _, err := b.s.Wake(context.Background(), nil); err != nil {
					t.Fatal(err)
				}
			} else {
				run(t, b.s, "next")
			}
			reqs := a.requests("go")
			if got := lastUser(t, reqs[len(reqs)-1]); strings.Contains(got, late) || !strings.Contains(got, "the value is "+redact.Marker) {
				t.Fatalf("the delivered result = %q; want the key redacted", got)
			}
			if tr := transcript(t, b.s); strings.Contains(strings.Join(entries(tr), "\n"), late) {
				t.Fatalf("the transcript holds the key:\n%s", strings.Join(entries(tr), "\n"))
			}
		})
	}
}

// TestBackgroundKeysCoveredUntilDelivered (astra r14): a background child
// learns a key its parent never does. The parent's Redact covers it while the
// child runs, while its result waits — through an unrelated turn's end, here
// a failed wake's, which forgets the keys of the children it retired — and
// through the turn that delivers it; not after that turn, since the parent
// never learned it.
func TestBackgroundKeysCoveredUntilDelivered(t *testing.T) {
	const childKey = "sk-only-the-background-child-knows-it"
	env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
	var mu sync.Mutex
	b := openBG(t, withEnv(env, &mu))
	mu.Lock()
	env["NOKEY_API_KEY"] = childKey
	mu.Unlock()
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	if b.s.Redact(childKey) != redact.Marker {
		t.Fatal("while the child runs its key is not covered")
	}
	b.finish(t, ws[0])
	if b.s.Redact(childKey) != redact.Marker {
		t.Fatal("while its result waits the child's key is not covered")
	}
	a.route("go", reply(errorPart(errors.New("down"))))
	if _, err := b.s.Wake(context.Background(), nil); err == nil {
		t.Fatal("the wake did not fail")
	}
	if b.s.Redact(childKey) != redact.Marker || resultOf(t, b.s, ids[0]).state != resultSuspended {
		t.Fatal("after an unrelated turn ended the child's key is no longer covered")
	}
	// The delivering turn: its step 0 takes the result up and its append
	// commits it; the next step checks the key is still covered.
	var during string
	a.route("go", callStep(globPart("g1")), func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		during = b.s.Redact(childKey)
		answerWith("done")(ctx, yield)
	})
	run(t, b.s, "next")
	if resultOf(t, b.s, ids[0]).state != resultCommitted || during != redact.Marker {
		t.Fatalf("during the delivering turn Redact gave %q; want the key covered", during)
	}
	if b.s.Redact(childKey) != childKey {
		t.Fatal("after the delivering turn the parent still covers a key it never learned")
	}
}

// TestUndeliveredReportedAtClose (A13, astra r14, major 4): Close reports,
// once each, every background result never delivered — one waiting, one a
// failed wake set aside, one whose child was still running — with the usage
// each child spent, a row per model; never one that was delivered.
func TestUndeliveredReportedAtClose(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "delivered", "suspended", "waiting", "running")
	b.finish(t, ws[0])
	a.route("go", answerWith("got it"))
	run(t, b.s, "next")
	b.finish(t, ws[1])
	a.route("go", reply(errorPart(errors.New("down"))))
	if _, err := b.s.Wake(context.Background(), nil); err == nil {
		t.Fatal("the wake did not fail")
	}
	b.finish(t, ws[2])
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reported in launch order, which is Fantasy's scheduling of the four
	// parallel calls: compared by id.
	got := map[string]SubagentUndelivered{}
	for _, u := range of[SubagentUndelivered](b.own.list()) {
		if _, dup := got[u.ID]; dup {
			t.Fatalf("%s was reported twice", u.ID)
		}
		got[u.ID] = u
	}
	want := map[string]SubagentUndelivered{
		ids[1]: {ID: ids[1], Type: "general-purpose", Usage: oneRow(1)},
		ids[2]: {ID: ids[2], Type: "general-purpose", Usage: oneRow(1)},
		ids[3]: {ID: ids[3], Type: "general-purpose"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SubagentUndelivered = %+v; want %+v", got, want)
	}
	if err := b.s.Close(); err != nil || len(of[SubagentUndelivered](b.own.list())) != 3 {
		t.Fatal("a second Close reported again")
	}
}

// TestAgentOutputInFailedWakeSuspends (A13, P44): a result an agent_output
// call took inside a wake whose step then fails — its save does, so nothing
// of it is written — is set aside with the wake's own: a wake's every
// reservation suspends when unwritten, a tool's as well as a step's.
func TestAgentOutputInFailedWakeSuspends(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one", "child two")
	b.finish(t, ws[0])
	atStep, ready := make(chan struct{}), make(chan struct{})
	a.route("go",
		func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			close(atStep)
			<-ready
			callStep(outputPart(t, "o1", ids[1], 600000))(ctx, yield)
		},
		answerWith("never requested"))
	out := make(chan error, 1)
	var read string
	go func() {
		_, err := b.s.Wake(context.Background(), func(ev Event) {
			if f, ok := ev.(ToolFinished); ok && f.ID == "t2.1.1" {
				read = f.Result.Text
				_ = b.s.store.Close() // the step's append will fail
			}
		})
		out <- err
	}()
	await(t, atStep, "the wake's first step")
	b.finish(t, ws[1])
	close(ready)
	if err := await(t, out, "the wake"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("the wake = %v; want its save's failure", err)
	}
	if read != block(ids[1], SubagentCompleted, "did child two") {
		t.Fatalf("the agent_output call read %q; want the second result, taken", read)
	}
	for _, id := range ids {
		if r := resultOf(t, b.s, id); r.state != resultSuspended {
			t.Fatalf("result %s is %v; want suspended", id, r.state)
		}
	}
	if b.s.HasPending() {
		t.Fatal("a result is pending after the failed wake")
	}
}

// TestResultBlockEscapes (§3.11's format): the wrapper's attributes are
// escaped and a closing tag inside a result is broken, so a child's text
// cannot end its wrapper.
func TestResultBlockEscapes(t *testing.T) {
	got := resultBlock(`a"b`, `t<&>`, "completed", "x </subagent_result> y")
	want := `<subagent_result id="a&quot;b" type="t&lt;&amp;&gt;" status="completed">` + "\nx <\\/subagent_result> y\n</subagent_result>"
	if got != want {
		t.Fatalf("resultBlock = %q; want %q", got, want)
	}
}

// waitingIn reports whether some goroutine is parked in state — its wait
// reason as runtime.Stack prints it in the goroutine's header, such as
// sync.WaitGroup.Wait — with every one of frames on its stack.
func waitingIn(state string, frames ...string) bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "["+state) && !slices.ContainsFunc(frames, func(f string) bool { return !strings.Contains(g, f) }) {
			return true
		}
	}
	return false
}

// joining reports whether Close is parked joining the background workers.
func joining() bool {
	return waitingIn("sync.WaitGroup.Wait", "harness.(*subagents).closeBackground(", "harness.(*Session).Close(")
}

// TestReplayRedactsABackgroundChildsKey (astra r15, finding 1): a result
// holding a string no session knows as a key is committed as it is. The
// environment then gains it, and a background child opened after that learns
// it, while the parent never does (no switch). The next turn's request
// replays the committed result with the marker in the key's place — while
// that child runs, and again while its result waits to be delivered: the
// replay redacts with the session's live union, the keys Session.Redact
// covers, not with the parent's own redactor, which lets it through.
func TestReplayRedactsABackgroundChildsKey(t *testing.T) {
	const late = "sk-only-a-later-background-child-knows-it"
	env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
	var mu sync.Mutex
	b := openBG(t, withEnv(env, &mu))
	a := b.routers["test/a"]
	first, second := newWorker(), newWorker()
	a.route("go", callStep(bgPart(t, "a1", "scan", "child one")), answerWith("started"), answerWith("got it"))
	a.route("child one", first.step(openText("the value is "+late), finishText()))
	run(t, b.s, "go")
	b.finish(t, first)
	run(t, b.s, "remember it")
	tr := transcript(t, b.s)
	if e := tr.Entries[len(tr.Entries)-2]; !e.SubagentResults || !strings.Contains(messageText(e.Message), late) {
		t.Fatalf("the committed results entry = %q; want it holding the string, no key yet", messageText(e.Message))
	}
	mu.Lock()
	env["NOKEY_API_KEY"] = late
	mu.Unlock()
	a.route("go", callStep(bgPart(t, "b1", "later", "child two")), answerWith("started two"))
	a.route("child two", second.step(openText("did child two"), finishText()))
	run(t, b.s, "start another")
	if b.s.Redact(late) != redact.Marker || b.s.tools.redactor().String(late) != late || slices.Contains(b.s.tools.knownKeys(), late) {
		t.Fatal("control: want the key covered through the second child alone, the parent never learning it")
	}
	replayed := func(when string) {
		t.Helper()
		a.route("go", answerWith("ok"))
		run(t, b.s, when)
		reqs := a.requests("go")
		var results []string
		for _, m := range reqs[len(reqs)-1].Prompt {
			if text := messageText(m); strings.Contains(text, "the value is ") {
				results = append(results, text)
			}
		}
		if len(results) != 1 || strings.Contains(results[0], late) || !strings.Contains(results[0], "the value is "+redact.Marker) {
			t.Fatalf("%s, the replayed results = %q; want the first child's, the marker in place of the key", when, results)
		}
	}
	replayed("while the child that knows it runs")
	b.finish(t, second)
	if b.s.tools.redactor().String(late) != late || slices.Contains(b.s.tools.knownKeys(), late) {
		t.Fatal("control: the parent learned the key")
	}
	replayed("while its result waits") // its turn's step 0 takes it up, after the replay was made
}

// withChildKey makes every background child of b learn, as it opens, a key
// its parent never does: key of the child's id, put in the environment just
// before the child's Open reads it, after the parent's own.
func withChildKey(b *bg, env map[string]string, mu *sync.Mutex, key func(id string) string) {
	b.s.subs.seams.open = func(o Options) (*Session, error) {
		mu.Lock()
		env["NOKEY_API_KEY"] = key(o.Child.ID)
		mu.Unlock()
		return Open(o)
	}
}

// TestAgentOutputRepliesRedacted (astra r15, finding 2): whatever agent_output
// answers is redacted last with the session's live union, a fixed reply as
// much as a result, as settle prepares a foreground call's aborted result
// (TestSubagentAbortTextRedacted). The parent opens; then, as a background
// child opens — so the child learns it, and the parent never does — the
// environment gains a key equal to one of agent_output's fixed answers:
// already included, already delivered, still running, the refusal of an
// unknown id, or aborted. The call that answers it answers the marker, and
// the key reaches neither the parent's ToolFinished nor its transcript: the
// dispatcher and the tool entry redact with the parent's keys alone.
func TestAgentOutputRepliesRedacted(t *testing.T) {
	for _, c := range []struct {
		name  string
		key   func(id string) string
		class tool.ErrorClass
		// drive runs the turn that makes the call, on a session whose child id
		// is held by w, and returns the call's harness id and the turn's events.
		drive func(t *testing.T, b *bg, w *worker, id string) (string, []Event)
	}{
		{
			name: "already included",
			key:  func(string) string { return outputIncluded },
			drive: func(t *testing.T, b *bg, w *worker, id string) (string, []Event) {
				b.finish(t, w)
				b.routers["test/a"].route("go", callStep(outputPart(t, "o1", id, 0)), answerWith("ok"))
				var ev events
				if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
					t.Fatal(err)
				}
				return "t2.1.1", ev.list()
			},
		},
		{
			name: "already delivered",
			key:  func(string) string { return outputDelivered },
			drive: func(t *testing.T, b *bg, w *worker, id string) (string, []Event) {
				b.finish(t, w)
				// Step 0 takes the result up and the first step's append commits
				// it; the second step's call asks for it again.
				b.routers["test/a"].route("go", callStep(globPart("g1")), callStep(outputPart(t, "o1", id, 0)), answerWith("ok"))
				var ev events
				if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
					t.Fatal(err)
				}
				return "t2.2.1", ev.list()
			},
		},
		{
			name: "still running",
			key:  func(id string) string { return fmt.Sprintf(outputStillRunning, id) },
			drive: func(t *testing.T, b *bg, _ *worker, id string) (string, []Event) {
				b.routers["test/a"].route("go", callStep(outputPart(t, "o1", id, 0)), answerWith("ok"))
				var ev events
				if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
					t.Fatal(err)
				}
				return "t2.1.1", ev.list()
			},
		},
		{
			name: "an unknown id",
			key: func(id string) string {
				return "Unknown sub-agent id `nope`. The background sub-agents whose results are still to be delivered: `" + id + "`."
			},
			class: tool.ClassInvalidInput,
			drive: func(t *testing.T, b *bg, _ *worker, _ string) (string, []Event) {
				b.routers["test/a"].route("go", callStep(outputPart(t, "o1", "nope", 0)), answerWith("ok"))
				var ev events
				if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
					t.Fatal(err)
				}
				return "t2.1.1", ev.list()
			},
		},
		{
			name:  "aborted",
			key:   func(string) string { return tool.AbortedText },
			class: tool.ClassAborted,
			drive: func(t *testing.T, b *bg, _ *worker, id string) (string, []Event) {
				waiting := make(chan string, 1)
				b.s.subs.seams.outputWaiting = func(id string) { waiting <- id }
				b.routers["test/a"].route("go", callStep(outputPart(t, "o1", id, 600000)), answerWith("never"))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var ev events
				out := start(ctx, b.s, "next", ev.sink)
				await(t, waiting, "the agent_output call waiting for the running child")
				cancel()
				if got := await(t, out, "the cancelled turn"); got.err != nil || got.res.StopReason != StopCancelled {
					t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
				}
				return "t2.1.1", ev.list()
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
			var mu sync.Mutex
			b := openBG(t, withEnv(env, &mu))
			withChildKey(b, env, &mu, c.key)
			ws, ids := b.spawn(t, "child one")
			key := c.key(ids[0])
			if b.s.Redact(key) != redact.Marker || b.s.tools.redactor().String(key) != key || slices.Contains(b.s.tools.knownKeys(), key) {
				t.Fatal("control: want the key covered through the child alone, the parent never learning it")
			}
			callID, evs := c.drive(t, b, ws[0], ids[0])
			res := callResult(t, evs, callID)
			if res.Text != redact.Marker || res.IsError != (c.class != "") || res.Class != c.class {
				t.Fatalf("the call = %+v; want the marker, class %q: its whole text is the child's key", res, c.class)
			}
			for _, f := range of[ToolFinished](evs) {
				if found := leaks(f, key); len(found) != 0 {
					t.Fatalf("the parent's ToolFinished for %s leaks the key at %v", f.ID, found)
				}
			}
			if lines := entries(transcript(t, b.s)); strings.Contains(strings.Join(lines, "\n"), key) {
				t.Fatalf("the parent's transcript holds the key:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// TestUndeliveredReportRedacted (astra r15, finding 3): a background child on
// a model whose wire id holds a string no session knows as a key finishes,
// having spent a step, and its result waits. The environment then gains the
// key and the parent's switch learns it. Close reports the result undelivered
// with its usage row's wire id redacted by the keys the session knows as it
// reports it: the one record of that result that is never redacted again at
// a reservation.
func TestUndeliveredReportRedacted(t *testing.T) {
	const late = "sk-learned-before-close"
	env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
	var mu sync.Mutex
	b := openBG(t, withEnv(env, &mu), func(o *Options) {
		m := o.Table.Models["other/c"]
		m.WireModel = "wire-" + late
		o.Table.Models["other/c"] = m
	})
	w := newWorker()
	b.routers["test/a"].route("go", callStep(bgPart(t, "b1", "bg", "on c", "model", "other/c")), answerWith("started"))
	b.routers["other/c"].route("on c", w.step(openText("c done"), finishText()))
	var ev events
	if _, err := b.s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	id := startedWith(t, ev.list(), "on c").ID
	b.finish(t, w)
	if r := resultOf(t, b.s, id); r.usage == nil || r.usage.WireModel != "wire-"+late {
		t.Fatalf("control: the waiting result's usage = %+v; want the wire id as it is, no key yet", r.usage)
	}
	mu.Lock()
	env["NOKEY_API_KEY"] = late
	mu.Unlock()
	if err := b.s.SetModel("nokey/d"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if b.s.Redact(late) != redact.Marker {
		t.Fatal("control: the parent's switch did not learn the key")
	}
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}
	got := of[SubagentUndelivered](b.own.list())
	want := []SubagentUndelivered{{ID: id, Type: "general-purpose",
		Usage: []ModelUsage{{Provider: "other", Model: "other/c", WireModel: "wire-" + redact.Marker, Usage: oneStep(1)}}}}
	if !reflect.DeepEqual(got, want) || len(leaks(got, late)) != 0 {
		t.Fatalf("SubagentUndelivered = %+v; want %+v, the key redacted", got, want)
	}
}

// TestCloseJoinsASettlingWorker (astra r15, finding 4): a background child's
// turn has returned and its worker is held there, before anything of its
// settlement — its outcome, its finish, its result's publication. A Close
// started then is parked joining the workers, and has not returned, while the
// worker is held; once it is let go Close returns, after the worker
// published: the child's finish comes first, and the report of its result
// carries the usage the published result holds. A Close that joined nothing
// would have returned at once, reporting a child still running, with none.
func TestCloseJoinsASettlingWorker(t *testing.T) {
	b := openBG(t)
	held, release := make(chan string, 1), make(chan struct{})
	b.s.subs.seams.returned = func(id string) {
		held <- id
		<-release
	}
	// A failure must not leave the worker, and the cleanup's Close, held.
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	ws, ids := b.spawn(t, "child one")
	close(ws[0].release)
	if id := await(t, held, "the worker, its child's turn returned"); id != ids[0] {
		t.Fatalf("the worker of %s is held; want %s's", id, ids[0])
	}
	done := make(chan struct{})
	var closeErr error
	go func() {
		closeErr = b.s.Close()
		close(done)
	}()
	waitFor(t, func() bool { return isClosed(done) || joining() }, "Close joining the workers, or returning")
	if isClosed(done) {
		t.Fatalf("Close returned (%v) while a background worker was still settling its child", closeErr)
	}
	if r := resultOf(t, b.s, ids[0]); r.state != resultRunning || len(of[SubagentUndelivered](b.own.list())) != 0 {
		t.Fatalf("control: with the worker held the result is %v; want running, nothing reported", r.state)
	}
	letGo()
	await(t, done, "Close, the worker let go")
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	own := b.own.list()
	fin := slices.IndexFunc(own, func(ev Event) bool { f, ok := ev.(SubagentFinished); return ok && f.ID == ids[0] })
	rep := slices.IndexFunc(own, func(ev Event) bool { _, ok := ev.(SubagentUndelivered); return ok })
	want := []SubagentUndelivered{{ID: ids[0], Type: "general-purpose", Usage: oneRow(1)}}
	if got := of[SubagentUndelivered](own); fin < 0 || fin > rep || !reflect.DeepEqual(got, want) {
		t.Fatalf("finished at %d, reported at %d: %+v; want the finish, then the published result's report %+v", fin, rep, got, want)
	}
	if kids := b.kids.all(); len(kids) != 1 || !storeClosed(kids[0].store) {
		t.Fatal("the child's store is still open after Close returned")
	}
	settled(t, b.s)
}

// TestAppendBeatsALaterCancel (astra r15, finding 5; P42): a result taken up
// at a turn's step 0 is written by its first step's append, and the turn is
// cancelled from that step's StepDone — after the append committed it. It
// stays committed: the turn's end gives nothing back and says nothing
// (OnPending); no later turn or wake delivers it again; and its usage is on
// the one entry that wrote it, on no StepDone, and in no report at Close.
func TestAppendBeatsALaterCancel(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	b.finish(t, ws[0])
	// One step: the turn is cancelled as it ends. A second request, made on
	// the cancelled context, finds nothing queued — a step left queued would
	// be the next turn's — and the turn reads cancelled either way.
	a.route("go", callStep(globPart("g1")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ev events
	var saved StepDone
	res, err := b.s.Run(ctx, "next", func(e Event) {
		ev.sink(e)
		if d, ok := e.(StepDone); ok && d.Step == 1 {
			saved = d
			cancel()
		}
	})
	if err != nil || res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", res, err)
	}
	n := len(saved.Entries)
	if !saved.Saved || n != 4 {
		t.Fatalf("the first step's StepDone = %+v; want it saved: the prompt, the results, the call and its result", saved)
	}
	tr := transcript(t, b.s)
	lead := tr.Entries[entryIndex(tr, saved.Entries[1])]
	if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.entry != lead.ID || !lead.SubagentResults {
		t.Fatalf("the result is %v by %q; want committed by the step's entry of results %s", r.state, r.entry, lead.ID)
	}
	b.noPending(t)
	a.route("go", answerWith("ok"))
	run(t, b.s, "again")
	if _, err := b.s.Wake(context.Background(), nil); !errors.Is(err, ErrNothingPending) || b.s.HasPending() {
		t.Fatalf("a wake after = %v; want ErrNothingPending", err)
	}
	reqs := a.requests("go")
	if n := strings.Count(requestText(reqs[len(reqs)-1], false), `<subagent_result id="`+ids[0]+`"`); n != 1 {
		t.Fatalf("the next turn's request carries the result %d times; want once, replayed from its entry", n)
	}
	tr = transcript(t, b.s)
	var rows []ModelUsage
	for _, e := range tr.Entries {
		rows = append(rows, e.SubagentUsage...)
	}
	if delivered(tr, ids[0]) != 1 || !reflect.DeepEqual(rows, oneRow(1)) {
		t.Fatalf("the result delivered %d times, the transcript's rows %+v; want once, one row", delivered(tr, ids[0]), rows)
	}
	for _, d := range of[StepDone](ev.list()) {
		if d.SubagentUsage != nil {
			t.Fatalf("StepDone %d carries %+v; want no rows: the entry that wrote the result carries them", d.Step, d.SubagentUsage)
		}
	}
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}
	if u := of[SubagentUndelivered](b.own.list()); len(u) != 0 {
		t.Fatalf("SubagentUndelivered = %+v; want none: the result was committed", u)
	}
}

// TestBackgroundWorkerPanicFailsTheChild (astra r15; §3.11's worker): a panic
// on a background child's worker outside the child's own turn — here in the
// session's sink, handed the child's SubagentFinished — is the worker's to
// recover. The child is failed: its result is the runner's failure naming the
// panic, with the child's last output and what it spent; its registration and
// its slot are given back; and the session goes on, delivering the failure at
// its next turn.
func TestBackgroundWorkerPanicFailsTheChild(t *testing.T) {
	var once atomic.Bool
	b := openBG(t, func(o *Options) {
		own := o.Sink
		o.Sink = func(ev Event) {
			if _, ok := ev.(SubagentFinished); ok && once.CompareAndSwap(false, true) {
				panic("the session's sink exploded")
			}
			own(ev)
		}
	})
	a := b.routers["test/a"]
	ws, ids := b.spawn(t, "child one")
	b.finish(t, ws[0])
	want := "The sub-agent failed: it panicked: the session's sink exploded.\n\nIts last output was:\ndid child one"
	if r := resultOf(t, b.s, ids[0]); r.state != resultPending || r.status != SubagentFailed || r.text != want ||
		r.usage == nil || r.usage.Usage != (tool.Usage{Input: 10, Output: 5, CacheRead: 4}) {
		t.Fatalf("the result = %+v (usage %+v); want pending, failed, naming the panic, with the child's usage", r, r.usage)
	}
	settled(t, b.s)
	a.route("go", answerWith("noted"))
	if res := run(t, b.s, "next"); res.StopReason != StopEndTurn {
		t.Fatalf("the next turn = %+v; want end_turn", res)
	}
	reqs := a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != block(ids[0], SubagentFailed, want) {
		t.Fatalf("the next turn's request ends %q; want the failure delivered", got)
	}
	if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted {
		t.Fatalf("the result is %v; want committed", r.state)
	}
}

// TestBackgroundRegistrationRacesClose (astra r15; §3.8's protocol, a
// background call's): a Close that has sealed the registry before a
// background call registers aborts the call — no child opens, no worker
// starts, and nothing is left registered, held or to be reported — and one
// that seals right after the call launched its worker joins it: Close returns
// only once the worker has settled its child and published its result, which
// it then reports.
func TestBackgroundRegistrationRacesClose(t *testing.T) {
	t.Run("sealed before the call registers", func(t *testing.T) {
		b := openBG(t)
		acquired, proceed := make(chan struct{}), make(chan struct{})
		b.s.subs.seams.acquired = func(tool.SubagentCall) {
			close(acquired)
			<-proceed
		}
		// A failure must not leave the call, and so the turn and every Close
		// waiting on it, held.
		goOn := sync.OnceFunc(func() { close(proceed) })
		defer goOn()
		b.routers["test/a"].route("go", callStep(bgPart(t, "a1", "scan", "child one")), answerWith("never"))
		finished := make(chan tool.Result, 1)
		var ev events
		out := start(context.Background(), b.s, "go", func(e Event) {
			ev.sink(e)
			if f, ok := e.(ToolFinished); ok && f.ID == "t1.1.1" {
				finished <- f.Result
			}
		})
		await(t, acquired, "the background call holding its slot, about to register")
		// Close is held on the session's lock once it has sealed the registry
		// and before it signals anything — the turn, the tools' closing — so
		// the seal is the only thing that can refuse the call. A child that
		// opened would wait on the same lock.
		b.s.mu.Lock()
		unlock := sync.OnceFunc(b.s.mu.Unlock)
		defer unlock()
		closed := make(chan error, 1)
		go func() { closed <- b.s.Close() }()
		waitFor(t, func() bool { return waitingOnMutexIn("harness.(*Session).signalClose(", "harness.(*Session).Close(") },
			"Close, the registry sealed, waiting to signal")
		goOn()
		res := await(t, finished, "the call's result")
		if res.Class != tool.ClassAborted || res.Text != tool.AbortedText {
			t.Fatalf("the call = %+v; want aborted: the registry was sealed", res)
		}
		b.s.subs.regMu.Lock()
		results := len(b.s.subs.results)
		b.s.subs.regMu.Unlock()
		if len(b.kids.all()) != 0 || results != 0 || len(of[SubagentStarted](ev.list())) != 0 {
			t.Fatalf("%d children opened, %d results recorded; want none", len(b.kids.all()), results)
		}
		settled(t, b.s)
		unlock()
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatalf("Run = %+v, %v", got.res, got.err)
		}
		if err := await(t, closed, "Close"); err != nil {
			t.Fatal(err)
		}
		if own := b.own.list(); len(own) != 0 {
			t.Fatalf("the session's sink had %v; want nothing: no child ran", own)
		}
	})
	t.Run("sealed right after the worker launched", func(t *testing.T) {
		b := openBG(t)
		held, release := make(chan string, 1), make(chan struct{})
		b.s.subs.seams.returned = func(id string) {
			held <- id
			<-release
		}
		letGo := sync.OnceFunc(func() { close(release) })
		defer letGo()
		b.routers["test/a"].route("go", callStep(bgPart(t, "a1", "scan", "child one")), answerWith("never"))
		launched, proceed := make(chan string, 1), make(chan struct{})
		goOn := sync.OnceFunc(func() { close(proceed) })
		defer goOn()
		var ev events
		out := start(context.Background(), b.s, "go", func(e Event) {
			ev.sink(e)
			if st, ok := e.(SubagentStarted); ok && st.Background {
				// The call counted its worker and started it before it
				// reported the child started.
				launched <- st.ID
				<-proceed
			}
		})
		id := await(t, launched, "the worker launched")
		done := make(chan struct{})
		var closeErr error
		go func() {
			closeErr = b.s.Close()
			close(done)
		}()
		await(t, b.s.tools.closing, "Close, the registry sealed and the child signalled")
		goOn()
		if got := await(t, held, "the worker, its child's turn over"); got != id {
			t.Fatalf("the worker of %s is held; want %s's", got, id)
		}
		waitFor(t, func() bool { return isClosed(done) || joining() }, "Close joining the workers, or returning")
		if isClosed(done) {
			t.Fatalf("Close returned (%v) while the worker it launched before the seal was still settling", closeErr)
		}
		letGo()
		await(t, done, "Close, the worker let go")
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatalf("Run = %+v, %v", got.res, got.err)
		}
		if ack := callResult(t, ev.list(), "t1.1.1"); ack.Text != ackText(id) || ack.IsError {
			t.Fatalf("the spawning call = %+v; want its acknowledgement, final", ack)
		}
		own := b.own.list()
		fin := slices.IndexFunc(own, func(ev Event) bool { f, ok := ev.(SubagentFinished); return ok && f.ID == id })
		rep := slices.IndexFunc(own, func(ev Event) bool { _, ok := ev.(SubagentUndelivered); return ok })
		want := []SubagentUndelivered{{ID: id, Type: "general-purpose"}}
		if got := of[SubagentUndelivered](own); fin < 0 || fin > rep || own[fin].(SubagentFinished).Status != SubagentCancelled ||
			!reflect.DeepEqual(got, want) {
			t.Fatalf("finished at %d, reported at %d: %+v; want the child cancelled by the closing, then reported %+v", fin, rep, got, want)
		}
		if kids := b.kids.all(); len(kids) != 1 || !storeClosed(kids[0].store) {
			t.Fatal("the child's store is still open after Close returned")
		}
		settled(t, b.s)
	})
}

// TestBackgroundFailureRacesAStop (astra r15; X26, §3.10): a background child
// whose provider fails, and the user's stop landing after its Run returned
// and before its end is latched — taken, where the latch would refuse it. A
// stop claims only a cancelled ending, so the child stays failed: its finish
// and its result say its provider failed, never that the user stopped it,
// and the failure is what the parent's model is delivered.
func TestBackgroundFailureRacesAStop(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	stops := make(chan error, 1)
	b.s.subs.seams.returned = func(id string) { stops <- b.s.CancelSubagent(id) }
	a.route("go", callStep(bgPart(t, "a1", "racing", "end on your own")), answerWith("started"))
	a.route("end on your own", callStep(textParts("looking"), globPart("g1")),
		reply(openText("half an answer"), errorPart(errors.New("the provider hung up"))))
	var ev events
	if res, err := b.s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", res, err)
	}
	id := startedWith(t, ev.list(), "end on your own").ID
	if err := await(t, stops, "the stop between the child's Run and its latch"); err != nil {
		t.Fatalf("the stop = %v; want nil, taken before the latch", err)
	}
	if !await(t, b.pending, "the failed child's result") {
		t.Fatal("OnPending's handler found nothing pending")
	}
	fin := finishedOf(t, b.own.list(), id)
	if fin.Status != SubagentFailed || fin.Error == subagentStoppedLabel || !strings.Contains(fin.Error, "the provider hung up") {
		t.Fatalf("SubagentFinished = %+v; want failed on the provider, not stopped", fin)
	}
	r := resultOf(t, b.s, id)
	if r.status != SubagentFailed || !strings.HasPrefix(r.text, "The sub-agent failed: ") ||
		!strings.Contains(r.text, "the provider hung up") || !strings.HasSuffix(r.text, "Its last output was:\nhalf an answer") {
		t.Fatalf("the result = %s: %q; want the provider's failure with the child's last output", r.status, r.text)
	}
	a.route("go", answerWith("noted"))
	run(t, b.s, "next")
	reqs := a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != block(id, SubagentFailed, r.text) {
		t.Fatalf("the delivered result = %q; want the failure", got)
	}
	settled(t, b.s)
}
