package harness

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// Overflow recovery (plan 028 §3.12, §3.11 items 5 and 6; §7 A24): a request
// the provider refuses as too large for the window is settled and discarded,
// the context is compacted in the text form, and the turn goes on from the
// summary — once. Every schedule is driven by gates, never a sleep. The
// sessions compact nothing on their own here: every step reports usage under
// the threshold, so the only compaction is the overflow's.

// overflowErr is a provider refusing a request as longer than the window,
// before any output: an HTTP 400 carrying the flag (fantasy's openai client
// sets it from the message).
func overflowErr() error {
	return &fantasy.ProviderError{Title: "bad request", StatusCode: 400, ContextTooLargeErr: true,
		Message: "This model's maximum context length is 1000 tokens. However, your messages resulted in 2000 tokens."}
}

// midStreamOverflow is the same refusal arriving in-band, after output began:
// no status, as a stream's error event rides in a 200 response.
func midStreamOverflow() error {
	return &fantasy.ProviderError{Title: "stream error", Message: "prompt is too long", ContextTooLargeErr: true}
}

// overflowed is a step that fails at once with overflowErr.
func overflowed() step { return errorStep(overflowErr()) }

// toolInput is a tool call the model began and never completed: its input's
// start alone.
func toolInput(id, name string) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: name}}
}

// overflowFixture is a fixture whose test/a has a known window (so the text
// form's budget is 70% of it), and a session on it.
func overflowFixture(t *testing.T, opts func(*fixture) Options) (*fixture, *Session, *scripted) {
	t.Helper()
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", testWindow, 0)
	o := f.options()
	if opts != nil {
		o = opts(f)
	}
	s := f.open(o)
	return f, s, f.models["test/a"]
}

// isTextForm reports whether c is a compaction's text-form request: the
// compaction prompt, and no tools offered (§3.8 item 5, P17).
func isTextForm(c fantasy.Call) bool { return isSummarizer(c) && len(c.Tools) == 0 }

// finishedByID is every ToolFinished, by its call's id, and the index in evs
// of each call's first.
func finishedByID(evs []Event) (map[string][]ToolFinished, map[string]int) {
	out, at := map[string][]ToolFinished{}, map[string]int{}
	for i, ev := range evs {
		if f, ok := ev.(ToolFinished); ok {
			out[f.ID] = append(out[f.ID], f)
			if _, seen := at[f.ID]; !seen {
				at[f.ID] = i
			}
		}
	}
	return out, at
}

// TestOverflowCompactsAndRetriesOnce (A24, R2-9): a seeded session — it has a
// conversation to compact — whose request overflows compacts in the text form
// and carries the turn on from the summary, in the same turn. At the turn's
// first request the prompt is not in the summary (no append wrote it), so it
// is sent again, once; at request N it is, and no prompt is sent. The steer a
// failed request drained and the background result it reserved go out again
// after the summary, uncommitted until the replacement's step writes them;
// and a wake whose first request overflowed sends its held results again.
func TestOverflowCompactsAndRetriesOnce(t *testing.T) {
	t.Run("at the turn's first request, the prompt again", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(answerWith("hi"))
		run(t, s, "hello")
		a.push(overflowed(), summaryOf("a greeting."), answerWith("done"))
		var ev events
		res, err := s.Run(context.Background(), "again", ev.sink)
		if err != nil || res.StopReason != StopEndTurn || res.Usage != oneStep(1) {
			t.Fatalf("the turn = %+v, %v; want end_turn with the replacement's usage alone", res, err)
		}
		reqs := a.requests()
		if len(reqs) != 4 || !isTextForm(reqs[2]) || !startsFromSummary(reqs[3]) {
			t.Fatalf("%d requests; want turn one, the one that overflowed, the text-form summarizer, then the replacement from the summary", len(reqs))
		}
		got := promptOf(reqs[3])
		if len(got) != 3 || !strings.HasPrefix(got[1], "user: <"+compactedTag+">") || got[2] != "user: again" {
			t.Fatalf("the replacement request = %q; want the system prompt, the summary, then the prompt, once", got)
		}
		evs := ev.list()
		if steps := stepsOf(evs); !slices.Equal(steps, []int{2}) {
			t.Fatalf("StepDone steps = %v; want the replacement alone, step 2: the failed request was step 1", steps)
		}
		cs := of[Compacted](evs)
		if len(cs) != 2 || cs[0].Phase != CompactionStarted || cs[0].Reason != store.CompactionOverflow ||
			cs[1].Phase != CompactionEnded || cs[1].Reason != store.CompactionOverflow || cs[1].Err != "" {
			t.Fatalf("Compacted = %+v; want an overflow compaction, started then ended with a summary", cs)
		}
		equal(t, "transcript", entries(transcript(t, s)), []string{
			"user test/a high: hello",
			"assistant test/a high end_turn: hi",
			"compaction overflow no-tail " + store.SegmentName(1),
			"user test/a high: again",
			"assistant test/a high end_turn: done",
		})
	})

	t.Run("at request N, no prompt", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(toolStep(1, under), overflowed(), summaryOf("one call."), answerWith("done"))
		var ev events
		res, err := s.Run(context.Background(), "loop", ev.sink)
		if err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("the turn = %+v, %v; want end_turn", res, err)
		}
		reqs := a.requests()
		if len(reqs) != 4 || !isTextForm(reqs[2]) || !startsFromSummary(reqs[3]) {
			t.Fatalf("%d requests; want step 1, the one that overflowed, the text-form summarizer, then the replacement", len(reqs))
		}
		if got := promptOf(reqs[3]); len(got) != 2 || !strings.HasPrefix(got[1], "user: <"+compactedTag+">") {
			t.Fatalf("the replacement request = %q; want the system prompt and the summary alone: the prompt is in the summary", got)
		}
		if steps := stepsOf(ev.list()); !slices.Equal(steps, []int{1, 3}) {
			t.Fatalf("StepDone steps = %v; want 1 and 3: the failed request was step 2", steps)
		}
		lines := entries(transcript(t, s))
		want := []string{
			"user test/a high: loop",
			`assistant test/a high tool_use: [call c1 nope {"n":1}]`,
			"tool test/a high: [error c1: ",
			"compaction overflow no-tail " + store.SegmentName(1),
			"assistant test/a high end_turn: done",
		}
		if len(lines) != len(want) {
			t.Fatalf("transcript:\n%s\nwant %d lines", strings.Join(lines, "\n"), len(want))
		}
		for i := range want {
			if !strings.HasPrefix(lines[i], want[i]) {
				t.Fatalf("transcript line %d = %q, want %q…:\n%s", i, lines[i], want[i], strings.Join(lines, "\n"))
			}
		}
	})

	t.Run("a drained steer and a reserved result are carried", func(t *testing.T) {
		b := openBG(t)
		setWindow(b.s, testWindow, 0)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "one")
		g := newGate()
		a.route("go", g.hold(bareCall("c1", "nope", `{"n":1}`), finishUsing(fantasy.FinishReasonToolCalls, under)), overflowed())
		a.route(textFormKey, summaryOf("the child."))
		a.route(compactedKey, answerWith("done"))
		var ev events
		out := start(context.Background(), b.s, "next", ev.sink)
		await(t, g.reached, "step 1's request")
		if err := sendSteer(b.s, "and this"); err != nil {
			t.Fatal(err)
		}
		b.finish(t, ws[0])
		close(g.release)
		o := await(t, out, "the turn")
		if o.err != nil || o.res.StopReason != StopEndTurn || len(o.res.Unanswered) != 0 {
			t.Fatalf("the turn = %+v, %v; want end_turn with the steer answered", o.res, o.err)
		}
		result := "user: " + block(ids[0], SubagentCompleted, "did one")
		// The request that overflowed took both up: step 2's, which drained
		// the steer and reserved the result.
		turn := a.requests("go")
		if got := promptOf(turn[len(turn)-1]); len(got) < 2 || got[len(got)-2] != "user: and this" || got[len(got)-1] != result {
			t.Fatalf("the request that overflowed ends %q; want the steer, then the result", got[max(0, len(got)-2):])
		}
		restart := a.requests(compactedKey)
		if len(restart) != 1 {
			t.Fatalf("%d requests from the summary, want the replacement's one", len(restart))
		}
		if got := promptOf(restart[0]); len(got) != 4 || got[2] != "user: and this" || got[3] != result {
			t.Fatalf("the replacement request = %q; want the system prompt, the summary, then the carried steer and result", got)
		}
		if st := of[Steered](ev.list()); len(st) != 1 || st[0].Text != "and this" {
			t.Fatalf("Steered = %+v; want the one steer, reported once", st)
		}
		// Reserved by the failed request, step 2 of the spawning session's
		// turn 2, and committed by the replacement's append.
		if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.own != (owner{turn: 2, step: 2}) {
			t.Fatalf("the result is %v, owned by %+v; want committed, still the failed request's reservation", r.state, r.own)
		}
		tr := transcript(t, b.s)
		lines := entries(tr)
		at := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "compaction overflow") })
		if at < 0 || delivered(tr, ids[0]) != 1 || !slices.Equal(lines[at+1:at+2], []string{"user test/a high: and this"}) ||
			strings.Count(strings.Join(lines, "\n"), "user test/a high: and this") != 1 {
			t.Fatalf("transcript (delivered %d):\n%s\nwant the steer and the result once each, after the compaction", delivered(tr, ids[0]), strings.Join(lines, "\n"))
		}
	})

	t.Run("a wake's held results", func(t *testing.T) {
		b := openBG(t)
		setWindow(b.s, testWindow, 0)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "one")
		b.finish(t, ws[0])
		a.route("go", overflowed())
		a.route(textFormKey, summaryOf("the child."))
		a.route(compactedKey, answerWith("noted"))
		res, err := b.s.Wake(context.Background(), nil)
		if err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("the wake = %+v, %v; want end_turn", res, err)
		}
		want := block(ids[0], SubagentCompleted, "did one")
		turn := a.requests("go")
		if got := lastUser(t, turn[len(turn)-1]); got != want {
			t.Fatalf("the wake's first request ends %q; want its results", got)
		}
		restart := a.requests(compactedKey)
		if len(restart) != 1 {
			t.Fatalf("%d requests from the summary, want the replacement's one", len(restart))
		}
		if got := lastUser(t, restart[0]); got != want || strings.Count(strings.Join(promptOf(restart[0]), "\n"), `<subagent_result id="`+ids[0]+`"`) != 1 {
			t.Fatalf("the replacement request = %q; want the wake's results sent again, once, as its prompt", promptOf(restart[0]))
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.own != (owner{turn: 2, step: 1, wake: true}) {
			t.Fatalf("the result is %v, owned by %+v; want committed by the wake's own entry", r.state, r.own)
		}
		tr := transcript(t, b.s)
		lines := entries(tr)
		at := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "compaction overflow") })
		if at < 0 || at+2 >= len(lines) || !strings.Contains(lines[at+1], `<subagent_result id="`+ids[0]+`"`) ||
			lines[at+2] != "assistant test/a high end_turn: noted" || delivered(tr, ids[0]) != 1 {
			t.Fatalf("transcript:\n%s\nwant the compaction, then the wake's results once, then its answer", strings.Join(lines, "\n"))
		}
	})

	t.Run("over the wire", func(t *testing.T) {
		// The real stack: the provider's HTTP 400, classified by Fantasy's
		// client from its message, recovered from as the scripted one is.
		w := newWire(t,
			sseReply(textChunk("hi"), finishChunk("stop", true)),
			func(rw http.ResponseWriter, _ *http.Request) {
				jsonError(rw, 400, "This model's maximum context length is 1000 tokens. However, your messages resulted in 2000 tokens.")
			},
			sseReply(textChunk(segmentSummary("a greeting.")), finishChunk("stop", true)),
			sseReply(textChunk("done"), finishChunk("stop", true)),
		)
		f, opts := wireFixture(t, w)
		windowed(f, "test/a", testWindow, 0)
		s := f.open(opts)
		run(t, s, "hello")
		if res := run(t, s, "again"); res.StopReason != StopEndTurn {
			t.Fatalf("the turn = %+v; want end_turn", res)
		}
		bodies := w.requests()
		if len(bodies) != 4 {
			t.Fatalf("the server saw %d requests, want 4", len(bodies))
		}
		if _, tools := fields(t, bodies[2])["tools"]; tools {
			t.Fatal("the summarizer's request offered tools: want the text form")
		}
		sent := messages(t, bodies[3])
		if len(sent) != 3 || !bytes.Contains(sent[1], []byte(compactedTag)) || !bytes.Contains(sent[2], []byte(`"again"`)) {
			t.Fatalf("the replacement request's messages = %s; want the system prompt, the summary, then the prompt", sent)
		}
	})
}

// TestAMidStreamOverflowSettlesTheAttempt (A24, R2-1): a request that streamed
// text and began a tool call before the provider refused it as too large is
// discarded by the failed-request transition. Its call is settled not run —
// every ToolStarted has its ToolFinished — before the compaction starts; the
// text the user saw stream is dropped, so nothing of the attempt is ever
// persisted; and its step number is spent, so the replacement's ids are new.
// A replacement that is itself interrupted persists its own text alone. A
// cancel during the overflow's compaction persists nothing either, and hands
// back the steer the failed request drained.
func TestAMidStreamOverflowSettlesTheAttempt(t *testing.T) {
	t.Run("an interrupted replacement persists its own text alone", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		g := newGate()
		a.push(toolStep(1, under),
			reply(textParts("partial "), toolInput("c2", "nope"), errorPart(midStreamOverflow())),
			summaryOf("one call."),
			g.hold(cat(openText("mine"), toolInput("c3", "nope")), finishText()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var ev events
		out := start(ctx, s, "loop", ev.sink)
		await(t, g.reached, "the replacement's request")
		cancel()
		o := await(t, out, "the turn")
		if o.err != nil || o.res.StopReason != StopCancelled {
			t.Fatalf("the turn = %+v, %v; want cancelled", o.res, o.err)
		}
		evs := ev.list()
		if got := callIDs(evs); !slices.Equal(got, []string{"t1.1.1", "t1.2.1", "t1.3.1"}) {
			t.Fatalf("tool ids = %v; want the failed request's t1.2.1 spent, the replacement's t1.3.1", got)
		}
		fin, at := finishedByID(evs)
		for _, id := range callIDs(evs) {
			if len(fin[id]) != 1 {
				t.Fatalf("call %s has %d ToolFinished; want one each", id, len(fin[id]))
			}
		}
		if r := fin["t1.2.1"][0].Result; r.Class != tool.ClassNotExecuted {
			t.Fatalf("the failed request's call finished %+v; want not executed", r)
		}
		if started := eventIndex(evs, isCompacted); at["t1.2.1"] > started {
			t.Fatalf("the failed request's call was settled at event %d, the compaction started at %d; want it settled first", at["t1.2.1"], started)
		}
		var seen strings.Builder
		for _, d := range of[TextDelta](evs) {
			seen.WriteString(d.Text)
		}
		if seen.String() != "partial mine" {
			t.Fatalf("the text streamed = %q; want both attempts' as the user saw them", seen.String())
		}
		if steps := stepsOf(evs); !slices.Equal(steps, []int{1}) {
			t.Fatalf("StepDone steps = %v; want step 1 alone: neither later request finished", steps)
		}
		lines := entries(transcript(t, s))
		if len(lines) != 5 || lines[3] != "compaction overflow no-tail "+store.SegmentName(1) ||
			lines[4] != "assistant test/a high cancelled interrupted: mine" || strings.Contains(strings.Join(lines, "\n"), "partial") {
			t.Fatalf("transcript:\n%s\nwant step 1, the compaction, then the replacement's own text, interrupted; nothing of the failed request", strings.Join(lines, "\n"))
		}
	})

	t.Run("a cancel during the compaction", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		g1, g2 := newGate(), newGate()
		a.push(g1.hold(bareCall("c1", "nope", `{"n":1}`), finishUsing(fantasy.FinishReasonToolCalls, under)),
			reply(textParts("partial "), toolInput("c2", "nope"), errorPart(midStreamOverflow())),
			heldSummary(g2, "never"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var ev events
		out := start(ctx, s, "loop", ev.sink)
		await(t, g1.reached, "step 1's request")
		if err := sendSteer(s, "and this"); err != nil {
			t.Fatal(err)
		}
		close(g1.release)
		await(t, g2.reached, "the overflow's summarizer")
		cancel()
		o := await(t, out, "the turn")
		if o.err != nil || o.res.StopReason != StopCancelled || o.res.Usage != (Usage{}) || !slices.Equal(o.res.Unanswered, []string{"and this"}) {
			t.Fatalf("the turn = %+v, %v; want cancelled, no usage, the drained steer unanswered", o.res, o.err)
		}
		evs := ev.list()
		fin, at := finishedByID(evs)
		if len(fin["t1.2.1"]) != 1 || at["t1.2.1"] > eventIndex(evs, isCompacted) {
			t.Fatalf("the failed request's call has %d ToolFinished (at %d, the compaction at %d); want one, before the compaction",
				len(fin["t1.2.1"]), at["t1.2.1"], eventIndex(evs, isCompacted))
		}
		if st := of[Steered](evs); len(st) != 1 {
			t.Fatalf("Steered = %+v; want the steer reported once, when the failed request took it", st)
		}
		lines := entries(transcript(t, s))
		if len(lines) != 3 || strings.Contains(strings.Join(lines, "\n"), "partial") {
			t.Fatalf("transcript:\n%s\nwant step 1 alone: nothing of the failed request, and no compaction entry (none was billed)", strings.Join(lines, "\n"))
		}
	})
}

// TestAChildsFailedAttemptNeverLeaksText (A24, R3-1): a child whose request
// streamed text and then overflowed recovers like any session, and the
// discarded attempt's text is no part of what the child answers — not its
// SubagentFinished text, not its parent's tool result, not the background
// result delivered — while the replacement's is all of it.
func TestAChildsFailedAttemptNeverLeaksText(t *testing.T) {
	child := func(m *scripted) {
		m.push(toolStep(1, under), reply(textParts("LEAKED "), errorPart(midStreamOverflow())), summaryOf("the child's work."), answerWith("child done"))
	}
	childRequests := func(t *testing.T, m *scripted) {
		t.Helper()
		if reqs := m.requests(); len(reqs) != 4 || !isTextForm(reqs[2]) {
			t.Fatalf("the child's requests = %d; want its step, the one that overflowed, its compaction, and the replacement", len(reqs))
		}
	}

	t.Run("foreground", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/b", testWindow, 0)
		s := f.open(f.options())
		f.models["test/a"].push(callStep(agentPart(t, "a1", task("work", "child work", "model", "test/b"))), answerWith("ok"))
		child(f.models["test/b"])
		var ev events
		if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		childRequests(t, f.models["test/b"])
		fins := of[SubagentFinished](ev.list())
		if len(fins) != 1 || fins[0].Status != SubagentCompleted || fins[0].Text != "child done" {
			t.Fatalf("SubagentFinished = %+v; want completed, its text the replacement's alone", fins)
		}
		if res := callResult(t, ev.list(), "t1.1.1"); res.Text != "child done" {
			t.Fatalf("the agent call's result = %q; want the replacement's text alone", res.Text)
		}
	})

	t.Run("background", func(t *testing.T) {
		var b *bg
		b = openBG(t, func(o *Options) {
			routed := o.NewModel
			o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
				if r.Alias == "test/b" {
					return b.models["test/b"], nil // one child: a queue, not a router
				}
				return routed(r)
			}
		})
		windowed(b.fixture, "test/b", testWindow, 0)
		b.routers["test/a"].route("go", callStep(bgPart(t, "a1", "job", "child work", "model", "test/b")), answerWith("started"))
		child(b.models["test/b"])
		var ev events
		if res, err := b.s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		if !await(t, b.pending, "the child's result waiting to be delivered") {
			t.Fatal("OnPending's handler found nothing pending")
		}
		childRequests(t, b.models["test/b"])
		id := startedWith(t, ev.list(), "child work").ID
		if fin := finishedOf(t, b.own.list(), id); fin.Status != SubagentCompleted || fin.Text != "child done" {
			t.Fatalf("SubagentFinished = %+v; want completed, its text the replacement's alone", fin)
		}
		if r := resultOf(t, b.s, id); r.text != "child done" {
			t.Fatalf("the result to be delivered = %q; want the replacement's text alone", r.text)
		}
	})
}

// TestAnOverflowRestartCarriesExactlyOneReminder (A24, R4-1): a session in
// plan mode, seeded with two turns so the third's first request carries the
// full plan text (the alternation back at an even count), overflows at that
// request; the compaction keeps no tail, so no reminder is retained. The
// replacement request carries the failed request's reminder, reused as it is,
// and no other — not the sparse text a composition would pick at the
// alternation the failed request advanced — and the reuse leaves the
// alternation where the failed request put it: the next turn reads the
// sparse text.
func TestAnOverflowRestartCarriesExactlyOneReminder(t *testing.T) {
	_, s, a := overflowFixture(t, func(f *fixture) Options { return modeOptions(f, "plan") })
	a.push(answerWith("one"), answerWith("two"))
	run(t, s, "turn one")
	run(t, s, "turn two")
	a.push(overflowed(), summaryOf("two turns of planning."), answerWith("three"), answerWith("four"))
	run(t, s, "turn three")
	run(t, s, "turn four")

	reqs := a.requests()
	if len(reqs) != 6 || !isTextForm(reqs[3]) || !startsFromSummary(reqs[4]) {
		t.Fatalf("%d requests; want turns one and two, turn three's that overflowed, the summarizer, its replacement, then turn four", len(reqs))
	}
	failed, _ := reminderIn(t, a, 2)
	if !strings.Contains(failed, "Plan mode is active") {
		t.Fatalf("the request that overflowed carries %q; want the full plan text", failed)
	}
	if ces := compactionEntries(t, s); len(ces) != 1 || ces[0].Compaction.FirstKeptID != "" {
		t.Fatalf("compaction entries = %+v; want one, with no tail", ces)
	}
	got := promptOf(reqs[4])
	if rems := remindersIn(t, a, 4); len(rems) != 1 || rems[0] != failed || len(got) != 4 || got[2] != "user: turn three" || got[3] != failed {
		t.Fatalf("the replacement request = %q; want the summary, the prompt, then the failed request's reminder alone, byte for byte", got)
	}
	if own, _ := reminderIn(t, a, 5); !strings.Contains(own, "Plan mode is still active") {
		t.Fatalf("turn four's reminder = %q; want the sparse text: the reuse did not advance the alternation", own)
	}
	var rems []string
	for _, l := range entries(transcript(t, s)) {
		if strings.HasPrefix(l, "reminder ") {
			rems = append(rems, l)
		}
	}
	if want := []string{"reminder plan_full_empty", "reminder plan_sparse", "reminder plan_full_empty", "reminder plan_sparse"}; !slices.Equal(rems, want) {
		t.Fatalf("reminder entries = %q, want %q: one a turn", rems, want)
	}
}

// TestASecondOverflowFails (A24, §3.12): a turn recovers from one overflow.
// Its replacement overflowing too fails the turn with ErrContextTooLarge —
// whose text says the compaction was tried — and no second compaction runs;
// so does a compaction whose summarizer fails, whatever it failed with, its
// failure entry written and no request sent after it. And a new session
// whose first request overflows has nothing stored to compact (§3.8 item 5,
// R2-9): it fails at once, as any failed request does — what streamed saved,
// interrupted — with no compaction tried (TestWireErrors' case, with output
// before the refusal). So does an overflow of the turn's last allowed request
// (§3.11 items 2, 8): its replacement would be a request past the allowance.
// Neither of those two compacted anything, and their text says the request
// alone is too large, never "even after compacting" (C9c item 4).
func TestASecondOverflowFails(t *testing.T) {
	check := func(t *testing.T, res Result, err error, compacted bool) {
		t.Helper()
		var pe *ProviderError
		if !errors.Is(err, ErrContextTooLarge) || !errors.As(err, &pe) || !empty(res) {
			t.Fatalf("the turn = %+v, %v; want ErrContextTooLarge and an empty Result", res, err)
		}
		const after, alone = "even after compacting", `the request alone is too large for model "test/a"'s context window`
		switch said := err.Error(); {
		case compacted && !strings.Contains(said, after):
			t.Fatalf("the turn's error = %q; want it to say it was compacted (%q)", said, after)
		case !compacted && (strings.Contains(said, after) || !strings.Contains(said, alone)):
			t.Fatalf("the turn's error = %q; nothing was compacted, want it to say %q and never %q", said, alone, after)
		}
	}

	t.Run("a second overflow", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(answerWith("hi"))
		run(t, s, "hello")
		a.push(overflowed(), summaryOf("a greeting."), overflowed(), summaryOf("never"))
		var ev events
		res, err := s.Run(context.Background(), "again", ev.sink)
		check(t, res, err, true)
		if reqs := a.requests(); len(reqs) != 4 || summarizers(reqs) != 1 {
			t.Fatalf("%d requests, %d of them the summarizer; want turn one, the overflow, one compaction, and the replacement's overflow", len(reqs), summarizers(reqs))
		}
		if cs := of[Compacted](ev.list()); len(cs) != 2 {
			t.Fatalf("Compacted = %+v; want the one compaction", cs)
		}
		if got := compactions(t, s); !slices.Equal(got, []string{"overflow test/a ok"}) {
			t.Fatalf("compactions = %q; want the one", got)
		}
	})

	t.Run("a failed compaction", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(answerWith("hi"))
		run(t, s, "hello")
		a.push(overflowed(), errorStep(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"}), answerWith("never"))
		var ev events
		res, err := s.Run(context.Background(), "again", ev.sink)
		check(t, res, err, true)
		if errors.Is(err, ErrModelNotFound) {
			t.Fatalf("the turn failed with the summarizer's error, %v; want the overflow's", err)
		}
		if reqs := a.requests(); len(reqs) != 3 {
			t.Fatalf("%d requests; want turn one, the overflow and the summarizer, and nothing after", len(reqs))
		}
		if got := compactions(t, s); !slices.Equal(got, []string{"overflow test/a failed"}) {
			t.Fatalf("compactions = %q; want the one failure", got)
		}
		if cs := of[Compacted](ev.list()); len(cs) != 2 || cs[1].Err == "" {
			t.Fatalf("Compacted = %+v; want started, then ended with the summarizer's failure", cs)
		}
	})

	t.Run("nothing stored to compact", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(reply(textParts("partial"), errorPart(midStreamOverflow())), summaryOf("never"))
		var ev events
		res, err := s.Run(context.Background(), "hi", ev.sink)
		check(t, res, err, false)
		if reqs := a.requests(); len(reqs) != 1 {
			t.Fatalf("%d requests; want the one that overflowed, and no compaction", len(reqs))
		}
		if cs := of[Compacted](ev.list()); len(cs) != 0 {
			t.Fatalf("Compacted = %+v; want none", cs)
		}
		if _, err := os.Stat(s.store.Path()); err != nil {
			t.Fatalf("no transcript (%v); want the prompt and what streamed saved, as for any failed request", err)
		}
		equal(t, "transcript", entries(transcript(t, s)), []string{"user test/a high: hi", "assistant test/a high interrupted: partial"})
	})

	t.Run("at the step allowance", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		for n := 1; n < maxSteps; n++ {
			a.push(toolStep(n, under))
		}
		a.push(overflowed(), summaryOf("never"), answerWith("never"))
		res, err := s.Run(context.Background(), "loop", nil)
		check(t, res, err, false)
		if reqs := a.requests(); len(reqs) != maxSteps || summarizers(reqs) != 0 {
			t.Fatalf("%d requests, %d of them the summarizer; want the %d allowed and no compaction", len(reqs), summarizers(reqs), maxSteps)
		}
	})
}

// TestAnUnknownWindowsOverflowIsBudgetedByTheRequestThatOverflowed (review
// r1-c12, P17, §3.8 item 5): with an unknown window, an overflow's text form
// is budgeted at 60% of the estimate of the request that actually overflowed
// — the system prompt and tools, the history as sent, and the prompt the
// turn still holds — not of the aligned summarizer request compact would
// have built, which never went out and holds no prompt. A stored turn of
// about 10k tokens and a held prompt of about 20k: 60% of the refused ~30k
// request holds the stored turn; 60% of the stored context alone never
// could, and the compaction failed locally, the summarizer never asked, the
// turn failing with the overflow.
func TestAnUnknownWindowsOverflowIsBudgetedByTheRequestThatOverflowed(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	a := f.models["test/a"]
	if w := s.cur.r.ContextWindow; w != 0 {
		t.Fatalf("test setup: test/a's window is %d, want it unknown", w)
	}
	stored := strings.Repeat("a stored answer. ", 2400) // ~40 KB: ~10k tokens, ordinary text no cap cuts
	a.push(answerWith(stored))
	run(t, s, "hello")

	prompt := strings.Repeat("a long prompt. ", 5500) // ~80 KB: ~20k tokens, held until a step writes it
	a.push(overflowed(), summaryOf("a greeting."), answerWith("done"))
	res, err := s.Run(context.Background(), prompt, nil)
	if err != nil {
		t.Fatalf("Run = %v; want the turn to recover: 60%% of the request that overflowed holds the stored turn", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("Run's stop reason = %q, want end_turn", res.StopReason)
	}
	reqs := a.requests()
	if len(reqs) != 4 || !isTextForm(reqs[2]) {
		t.Fatalf("%d requests (the third a text form: %v); want turn one, the overflow, the text-form summarizer, and the replacement",
			len(reqs), len(reqs) > 2 && isTextForm(reqs[2]))
	}
	if !strings.Contains(messageText(reqs[2].Prompt[len(reqs[2].Prompt)-1]), "[Assistant]: "+stored) {
		t.Fatal("the text form's prompt does not hold the stored turn whole")
	}
	if got := compactions(t, s); !slices.Equal(got, []string{"overflow test/a ok"}) {
		t.Fatalf("compactions = %q; want the overflow's, a summary", got)
	}
}
