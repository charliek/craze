package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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
