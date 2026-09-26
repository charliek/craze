package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// The segmented turn (plan 028 §3.11, owner decision 2; §7 A23): a stop
// condition ends Agent.Stream after the tool step that reached the
// threshold, the turn compacts, and a second Agent.Stream carries the same
// turn on from the store. Every schedule here is driven by gates — a held
// summarizer is where a steer, a cancel or a plan write lands "during the
// compaction" — never by a sleep.

// toolStep is one step calling a tool that does not exist, with arguments of
// its own so no two steps repeat a call (the doom-loop guard): the call is
// recorded and answered by Fantasy, never run, and the step is one the model
// is not done with (tool_use). Its request reported input tokens: the
// context after it is input + 9 (finishUsing).
func toolStep(n int, input int64) step {
	return reply(bareCall(fmt.Sprintf("c%d", n), "nope", fmt.Sprintf(`{"n":%d}`, n)), finishUsing(fantasy.FinishReasonToolCalls, input))
}

// The window every session here runs on: a threshold of 85,000 tokens, which
// a step reporting over (90,000) reaches and one reporting under (1,000) does
// not.
const (
	testWindow = 100000
	over       = 90000
	under      = 1000
)

// segmentSummary is a summarizer's reply compact accepts, worded so that the
// summary message it becomes is not mistaken for a summarizer's request: a
// request whose last message is the summary — a restart's first, with no
// tail — must not carry the compaction prompt's heading (isSummarizer).
func segmentSummary(what string) string { return longSummary("The work so far: " + what) }

// summaryOf is a summarizer's reply, and heldSummary the same held at g: the
// compaction runs until the test releases it.
func summaryOf(what string) step { return answerWith(segmentSummary(what)) }
func heldSummary(g *gate, what string) step {
	return g.hold(nil, cat(textParts(segmentSummary(what)), finish(fantasy.FinishReasonStop)))
}

// stepsOf are the StepDone events' numbers, in order.
func stepsOf(evs []Event) []int {
	var out []int
	for _, d := range of[StepDone](evs) {
		out = append(out, d.Step)
	}
	return out
}

// usageOf is the StepDone events' usage, summed.
func usageOf(evs []Event) Usage {
	var total Usage
	for _, d := range of[StepDone](evs) {
		total = addUsage(total, d.Usage)
	}
	return total
}

// callIDs are the ToolStarted events' harness ids, in order.
func callIDs(evs []Event) []string {
	var out []string
	for _, st := range of[ToolStarted](evs) {
		out = append(out, st.ID)
	}
	return out
}

// eventIndex is the index of the first event ok picks, or -1.
func eventIndex(evs []Event, ok func(Event) bool) int {
	return slices.IndexFunc(evs, ok)
}

// isCompacted and isStep pick a Compacted event, and the StepDone of step n.
func isCompacted(e Event) bool { _, ok := e.(Compacted); return ok }
func isStep(n int) func(Event) bool {
	return func(e Event) bool { d, ok := e.(StepDone); return ok && d.Step == n }
}

// TestMidTurnCompactionContinuesTheTurn (A23): one turn, two segments. The
// tool step that reaches the threshold ends the first Agent.Stream; the turn
// compacts and a second Agent.Stream carries on from the summary, in the same
// turn: one Result, global step numbers and unique tool ids across the
// segments, the compaction recorded as the turn it is in and reported between
// the two segments' steps, a steer accepted during the summarizer taken at the
// next request — the restart's first, also when that request is the turn's
// last — and Result.Usage the sum of every segment's steps, the compaction's
// usage on its entry alone.
func TestMidTurnCompactionContinuesTheTurn(t *testing.T) {
	for _, final := range []bool{false, true} {
		name := "a tool step after the restart"
		if final {
			name = "the restart's first request is the turn's last"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			windowed(f, "test/a", testWindow, 0)
			s := f.open(f.options())
			a := f.models["test/a"]
			g := newGate()
			a.push(toolStep(1, over), heldSummary(g, "looping."))
			var wantUsage Usage
			if final {
				a.push(answerWith("done"))
				wantUsage = Usage{Input: over + 10, Output: 10, CacheRead: 8}
			} else {
				a.push(toolStep(2, under), answerWith("done"))
				wantUsage = Usage{Input: over + under + 10, Output: 15, CacheRead: 12}
			}

			var ev events
			out := start(context.Background(), s, "loop", ev.sink)
			await(t, g.reached, "the summarizer's request")
			if err := sendSteer(s, "and this"); err != nil {
				t.Fatalf("a steer during the mid-turn compaction was refused: %v", err)
			}
			close(g.release)
			o := await(t, out, "the turn")
			if o.err != nil || o.res.StopReason != StopEndTurn || len(o.res.Unanswered) != 0 {
				t.Fatalf("the turn = %+v, %v; want end_turn with the steer answered", o.res, o.err)
			}
			if o.res.Usage != wantUsage {
				t.Fatalf("Result.Usage = %+v, want every segment's steps summed, %+v, and not the compaction's", o.res.Usage, wantUsage)
			}
			evs := ev.list()
			wantSteps, wantIDs, wantEntries := []int{1, 2}, []string{"t1.1.1"}, []string{
				"user test/a high: loop",
				`assistant test/a high tool_use: [call c1 nope {"n":1}]`,
				"tool test/a high: [error c1: ", // Fantasy's own "tool not found"
				"compaction auto no-tail " + store.SegmentName(1),
				"user test/a high: and this",
				"assistant test/a high end_turn: done",
			}
			if !final {
				wantSteps, wantIDs = []int{1, 2, 3}, []string{"t1.1.1", "t1.2.1"}
				wantEntries = slices.Concat(wantEntries[:5], []string{
					`assistant test/a high tool_use: [call c2 nope {"n":2}]`,
					"tool test/a high: [error c2: ",
				}, wantEntries[5:])
			}
			if got := stepsOf(evs); !slices.Equal(got, wantSteps) {
				t.Fatalf("StepDone steps = %v, want %v: global across the segments", got, wantSteps)
			}
			if got := callIDs(evs); !slices.Equal(got, wantIDs) {
				t.Fatalf("tool ids = %v, want %v: numbered by the global step", got, wantIDs)
			}
			if got := usageOf(evs); got != wantUsage {
				t.Fatalf("the StepDones' usage = %+v, want %+v", got, wantUsage)
			}
			if st := of[Steered](evs); len(st) != 1 || st[0].Text != "and this" {
				t.Fatalf("Steered = %+v; want the one steer, reported once", st)
			}
			cs := of[Compacted](evs)
			if len(cs) != 2 || cs[0].Phase != CompactionStarted || cs[1].Phase != CompactionEnded || cs[1].Err != "" ||
				cs[1].Reason != store.CompactionAuto || cs[1].Usage != oneStep(1) {
				t.Fatalf("Compacted = %+v; want started, then ended with the summarizer's usage", cs)
			}
			if at, one, two := eventIndex(evs, isCompacted), eventIndex(evs, isStep(1)), eventIndex(evs, isStep(2)); at < one || at > two {
				t.Fatalf("the compaction was reported at %d, step 1 at %d and step 2 at %d; want it between the two", at, one, two)
			}

			reqs := a.requests()
			if len(reqs) != len(wantSteps)+1 || !isSummarizer(reqs[1]) || !startsFromSummary(reqs[2]) {
				t.Fatalf("%d requests (summarizer at 1: %v, the third from the summary: %v); want the first step, the summarizer, then the rest from the summary",
					len(reqs), len(reqs) > 1 && isSummarizer(reqs[1]), len(reqs) > 2 && startsFromSummary(reqs[2]))
			}
			if got := promptOf(reqs[2]); len(got) != 3 || got[2] != "user: and this" {
				t.Fatalf("the restart's first request = %q; want the system prompt, the summary, then the steer", got)
			}
			ces := compactionEntries(t, s)
			if len(ces) != 1 || ces[0].Turn != 1 || ces[0].Usage == nil || *ces[0].Usage != oneStep(1) {
				t.Fatalf("compaction entries = %+v; want one, recorded as turn 1, carrying the summarizer's usage", ces)
			}
			lines := entries(transcript(t, s))
			if len(lines) != len(wantEntries) {
				t.Fatalf("transcript:\n%s\nwant %d lines", strings.Join(lines, "\n"), len(wantEntries))
			}
			for i, want := range wantEntries {
				if !strings.HasPrefix(lines[i], want) {
					t.Fatalf("transcript line %d = %q, want %q…:\n%s", i, lines[i], want, strings.Join(lines, "\n"))
				}
			}
		})
	}

	t.Run("the reminder is re-sent once, its parity unchanged", func(t *testing.T) {
		// Plan mode: the turn's first request carries the full text (the
		// alternation advances to 1); the compaction keeps no tail, so the
		// summary swallowed it, and the restart's first request carries the
		// standing reminder again — the sparse text, parity 1 — without
		// advancing the alternation, so the next turn reads the sparse text
		// too (a turn's own reminder advances it; a re-send does not).
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(modeOptions(f, "plan"))
		a := f.models["test/a"]
		a.push(toolStep(1, over), summaryOf("planning."), toolStep(2, under), answerWith("done"), answerWith("again"))
		run(t, s, "plan it")
		run(t, s, "and more")
		reqs := a.requests()
		if len(reqs) != 5 || !isSummarizer(reqs[1]) {
			t.Fatalf("%d requests; want step 1, the summarizer, steps 2 and 3, then the next turn", len(reqs))
		}
		for n, want := range map[int]string{0: "Plan mode is active", 2: "Plan mode is still active", 3: "Plan mode is still active", 4: "Plan mode is still active"} {
			if text, at := reminderIn(t, a, n); at < 0 || !strings.Contains(text, want) {
				t.Fatalf("request %d's reminder is %q, want the %q text", n+1, text, want)
			}
		}
		if got := remindersIn(t, a, 3); len(got) != 1 {
			t.Fatalf("the restart's second request carries %d reminders, want the re-sent one alone: %q", len(got), got)
		}
		lines := entries(transcript(t, s))
		var rems []string
		for _, l := range lines {
			if strings.HasPrefix(l, "reminder ") {
				rems = append(rems, l)
			}
		}
		if want := []string{"reminder plan_full_empty", "reminder plan_sparse", "reminder plan_sparse"}; !slices.Equal(rems, want) {
			t.Fatalf("reminder entries = %q, want %q:\n%s", rems, want, strings.Join(lines, "\n"))
		}
	})

	t.Run("a retained standing reminder is not re-sent", func(t *testing.T) {
		// The compaction keeps a tail — the turn's first step, its reminder
		// entry with it — so the restart's history already says the mode,
		// and its first request composes nothing.
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(modeOptions(f, "plan"))
		a := f.models["test/a"]
		a.push(answerWith(strings.Repeat("x", 100_000))) // over the tail budget on its own
		run(t, s, "turn one")
		a.push(toolStep(1, over), summaryOf("turn two."), answerWith("done"))
		run(t, s, "turn two")
		reqs := a.requests()
		if len(reqs) != 4 || !isSummarizer(reqs[2]) {
			t.Fatalf("%d requests; want turn one, turn two's step 1, the summarizer, then the restart", len(reqs))
		}
		ces := compactionEntries(t, s)
		if len(ces) != 1 || ces[0].Compaction.FirstKeptID == "" {
			t.Fatalf("compaction entries = %+v; want one that kept a tail", ces)
		}
		if got := remindersIn(t, a, 3); len(got) != 1 || !strings.Contains(got[0], "Plan mode is still active") {
			t.Fatalf("the restart's request carries %q; want the retained sparse reminder alone, nothing re-sent", got)
		}
		lines := entries(transcript(t, s))
		if n := strings.Count(strings.Join(lines, "\n"), "reminder plan_sparse"); n != 1 {
			t.Fatalf("the transcript holds %d sparse reminder entries, want turn two's alone:\n%s", n, strings.Join(lines, "\n"))
		}
	})

	t.Run("the step allowance spans the segments", func(t *testing.T) {
		// A compaction after step 100 leaves the turn 100 more requests, not
		// 200: the 200th step is the last, and the turn ends
		// max_turn_requests as it does with one segment.
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(f.options())
		a := f.models["test/a"]
		for n := 1; n <= maxSteps+1; n++ {
			input := int64(under)
			if n == 100 {
				input = over
			}
			a.push(toolStep(n, input))
			if n == 100 {
				a.push(summaryOf("a hundred steps."))
			}
		}
		var ev events
		res, err := s.Run(context.Background(), "loop", ev.sink)
		if err != nil || res.StopReason != StopMaxTurnRequests {
			t.Fatalf("the turn = %+v, %v; want max_turn_requests", res, err)
		}
		reqs := a.requests()
		if len(reqs) != maxSteps+1 || summarizers(reqs) != 1 {
			t.Fatalf("%d requests, %d of them the summarizer; want %d steps and one compaction", len(reqs), summarizers(reqs), maxSteps)
		}
		if steps := stepsOf(ev.list()); len(steps) != maxSteps || steps[maxSteps-1] != maxSteps {
			t.Fatalf("%d StepDones, the last %d; want %d, numbered to %d", len(steps), steps[len(steps)-1], maxSteps, maxSteps)
		}
	})

	t.Run("suspended results are not re-taken at the restart", func(t *testing.T) {
		// Three background children. One's result was suspended by a failed
		// wake: the person's turn takes it at its first request (step 1,
		// the turn's), where it is committed. Two's finishes during the
		// summarizer and is set aside as suspended (by hand: no path
		// suspends a result mid-turn, and the gate is pinned all the same):
		// the restart's first request is not the turn's first, so it leaves
		// it, and the next turn takes it. Three's finishes during the
		// summarizer and waits pending, which every boundary takes: the
		// restart's first request, step 2 of the turn, delivers it.
		b := openBG(t)
		setWindow(b.s, testWindow, 0)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "one", "two", "three")
		b.finish(t, ws[0])
		a.route("go", errorStep(errors.New("the provider is down")))
		if _, err := b.s.Wake(context.Background(), nil); err == nil {
			t.Fatal("the wake did not fail")
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultSuspended {
			t.Fatalf("after the failed wake the first result is %v; want suspended", r.state)
		}

		g := newGate()
		a.route("go", toolStep(1, over), heldSummary(g, "children."))
		a.route(compactedKey, answerWith("done"), answerWith("ok"))
		var ev events
		out := start(context.Background(), b.s, "next", ev.sink)
		await(t, g.reached, "the summarizer's request")
		b.finish(t, ws[1])
		b.finish(t, ws[2])
		b.s.subs.regMu.Lock()
		b.s.subs.results[ids[1]].state = resultSuspended
		b.s.subs.regMu.Unlock()
		close(g.release)
		o := await(t, out, "the turn")
		if o.err != nil || o.res.StopReason != StopEndTurn {
			t.Fatalf("the turn = %+v, %v; want end_turn", o.res, o.err)
		}
		restart := a.requests(compactedKey)
		if len(restart) != 1 {
			t.Fatalf("%d requests from the summary, want the restart's one", len(restart))
		}
		if got := lastUser(t, restart[0]); got != block(ids[2], SubagentCompleted, "did three") {
			t.Fatalf("the restart's request ends %q; want the third child's pending result alone", got)
		}
		if text := strings.Join(promptOf(restart[0]), "\n"); strings.Contains(text, ids[1]) {
			t.Fatalf("the restart's request carries the suspended result:\n%s", text)
		}
		// The spawning turn was 1 and the failed wake 2: this is turn 3.
		if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.own != (owner{turn: 3, step: 1}) {
			t.Fatalf("the first result is %v, owned by %+v; want committed by the turn's first step", r.state, r.own)
		}
		if r := resultOf(t, b.s, ids[1]); r.state != resultSuspended {
			t.Fatalf("the second result is %v; want still suspended", r.state)
		}
		if r := resultOf(t, b.s, ids[2]); r.state != resultCommitted || r.own != (owner{turn: 3, step: 2}) {
			t.Fatalf("the third result is %v, owned by %+v; want committed by the restart's first step, step 2 of the turn", r.state, r.own)
		}
		tr := transcript(t, b.s)
		if delivered(tr, ids[0]) != 1 || delivered(tr, ids[2]) != 1 || delivered(tr, ids[1]) != 0 {
			t.Fatalf("delivered: one %d, two %d, three %d; want 1, 0, 1", delivered(tr, ids[0]), delivered(tr, ids[1]), delivered(tr, ids[2]))
		}

		// The next turn a person starts takes the suspended result at its
		// first request.
		if res := run(t, b.s, "after"); res.StopReason != StopEndTurn {
			t.Fatalf("the next turn = %+v", res)
		}
		next := a.requests(compactedKey)
		if got := lastUser(t, next[len(next)-1]); got != block(ids[1], SubagentCompleted, "did two") {
			t.Fatalf("the next turn's request ends %q; want the suspended result", got)
		}
	})
}

// TestACancelBetweenRequestsPersistsNothingTwice (A23, P1): a cancel that
// lands during the mid-turn summarizer ends the turn cancelled with nothing
// written again — the completed step is in the transcript once, no
// interrupted answer follows it, and no compaction entry, since no attempt
// was billed — with no usage, as a cancel after a tool step has always
// ended, and the steer accepted meanwhile back unanswered. A background
// result the step delivered stays committed, and one that became ready
// during the summarizer stays pending: the reservations are what they were.
func TestACancelBetweenRequestsPersistsNothingTwice(t *testing.T) {
	t.Run("the transcript and the result", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(f.options())
		a := f.models["test/a"]
		g := newGate()
		a.push(toolStep(1, over), heldSummary(g, "looping."), answerWith("never"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var ev events
		out := start(ctx, s, "loop", ev.sink)
		await(t, g.reached, "the summarizer's request")
		if err := sendSteer(s, "and this"); err != nil {
			t.Fatal(err)
		}
		cancel()
		o := await(t, out, "the turn")
		if o.err != nil || o.res.StopReason != StopCancelled || o.res.Usage != (Usage{}) || !slices.Equal(o.res.Unanswered, []string{"and this"}) {
			t.Fatalf("the turn = %+v, %v; want cancelled, no usage, the steer unanswered", o.res, o.err)
		}
		lines := entries(transcript(t, s))
		if len(lines) != 3 || lines[0] != "user test/a high: loop" || lines[1] != `assistant test/a high tool_use: [call c1 nope {"n":1}]` ||
			!strings.HasPrefix(lines[2], "tool test/a high: [error c1: ") {
			t.Fatalf("transcript:\n%s\nwant the one step, once, and nothing after it", strings.Join(lines, "\n"))
		}
		if reqs := a.requests(); len(reqs) != 2 {
			t.Fatalf("%d requests; want the step's and the summarizer's, and no more", len(reqs))
		}
		evs := ev.list()
		if steps := stepsOf(evs); !slices.Equal(steps, []int{1}) {
			t.Fatalf("StepDone steps = %v, want the one step", steps)
		}
		if cs := of[Compacted](evs); len(cs) != 2 || cs[1].Phase != CompactionEnded || cs[1].Err != "cancelled" {
			t.Fatalf("Compacted = %+v; want started, then ended cancelled", cs)
		}
		if st := of[Steered](evs); len(st) != 1 {
			t.Fatalf("Steered = %+v; want the steer reported once, as the turn settled", st)
		}
	})

	t.Run("the reservations", func(t *testing.T) {
		b := openBG(t)
		setWindow(b.s, testWindow, 0)
		a := b.routers["test/a"]
		ws, ids := b.spawn(t, "one", "two")
		b.finish(t, ws[0])
		g := newGate()
		a.route("go", toolStep(1, over), heldSummary(g, "children."))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, b.s, "next", nil)
		await(t, g.reached, "the summarizer's request")
		b.finish(t, ws[1])
		cancel()
		o := await(t, out, "the turn")
		if o.err != nil || o.res.StopReason != StopCancelled {
			t.Fatalf("the turn = %+v, %v; want cancelled", o.res, o.err)
		}
		if r := resultOf(t, b.s, ids[0]); r.state != resultCommitted || r.own != (owner{turn: 2, step: 1}) {
			t.Fatalf("the first result is %v, owned by %+v; want committed by the turn's first step, as before the cancel", r.state, r.own)
		}
		if r := resultOf(t, b.s, ids[1]); r.state != resultPending {
			t.Fatalf("the second result is %v; want pending: no step took it", r.state)
		}
		b.noPending(t) // nothing was given back, so nothing is said
		if tr := transcript(t, b.s); delivered(tr, ids[0]) != 1 || delivered(tr, ids[1]) != 0 {
			t.Fatalf("delivered: one %d, two %d; want 1, 0", delivered(tr, ids[0]), delivered(tr, ids[1]))
		}
	})
}

// TestReminderSurvivesAPlanFileWriteAcrossARestart (plan 028 §3.15, R5-1):
// the restart case of TestReminderSurvivesAPlanFileWrite. The turn's first
// request reads the full plan text for a plan not yet written; the plan is
// written while the turn compacts; the compaction keeps a tail with that
// request's reminder entry in it, and the restart's request renders the
// reminder from its variant — "No plan written yet", byte for byte what was
// sent — never from the file, and composes none of its own. The control is
// the next turn's reminder, composed afresh at the full text: it reads the
// written plan.
func TestReminderSurvivesAPlanFileWriteAcrossARestart(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", testWindow, 0)
	s := f.open(modeOptions(f, "plan"))
	a := f.models["test/a"]
	a.push(answerWith(strings.Repeat("x", 100_000))) // over the tail budget on its own
	run(t, s, "turn one")
	if err := s.SetMode("plan"); err != nil { // the alternation over: the full text next
		t.Fatal(err)
	}
	g := newGate()
	a.push(toolStep(1, over), heldSummary(g, "turn two."), answerWith("done"))
	out := start(context.Background(), s, "turn two", nil)
	await(t, g.reached, "the summarizer's request")
	plan := planPathOf(s)
	if err := os.WriteFile(plan, []byte("## The plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if o := await(t, out, "turn two"); o.err != nil || o.res.StopReason != StopEndTurn {
		t.Fatalf("turn two = %+v, %v; want end_turn", o.res, o.err)
	}
	if err := s.SetMode("plan"); err != nil {
		t.Fatal(err)
	}
	a.push(answerWith("three"))
	run(t, s, "turn three")

	reqs := a.requests()
	if len(reqs) != 5 || !isSummarizer(reqs[2]) || !startsFromSummary(reqs[3]) {
		t.Fatalf("%d requests; want turn one, turn two's step 1, the summarizer, the restart from the summary, then turn three", len(reqs))
	}
	first, _ := reminderIn(t, a, 1)
	if !strings.Contains(first, "No plan written yet") {
		t.Fatalf("turn two's first request's reminder is not the one for an empty plan: %q", first)
	}
	if got := remindersIn(t, a, 3); len(got) != 1 || got[0] != first {
		t.Fatalf("the restart's request carries %q; want turn two's reminder alone, byte for byte %q", got, first)
	}
	if own, _ := reminderIn(t, a, 4); !strings.Contains(own, "A plan file exists") {
		t.Fatalf("turn three's own reminder is not the written plan's (control): %q", own)
	}
	lines := entries(transcript(t, s))
	var rems []string
	for _, l := range lines {
		if strings.HasPrefix(l, "reminder ") {
			rems = append(rems, l)
		}
	}
	if want := []string{"reminder plan_full_empty", "reminder plan_full_empty", "reminder plan_full_written"}; !slices.Equal(rems, want) {
		t.Fatalf("reminder entries = %q, want %q:\n%s", rems, want, strings.Join(lines, "\n"))
	}
}

// TestTwoSegmentsThenAProviderFailureKeepTheUsage (A23, R3-3): a turn whose
// third request fails, after two segments, returns the error and a Result
// with nothing but Unanswered — today's contract for a failed turn — while the
// StepDone events it emitted carry every step's usage, which is where an
// observer's spend comes from; the compaction's is on its Compacted alone.
func TestTwoSegmentsThenAProviderFailureKeepTheUsage(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", testWindow, 0)
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(toolStep(1, over), summaryOf("looping."), toolStep(2, under),
		errorStep(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"}))
	var ev events
	res, err := s.Run(context.Background(), "loop", ev.sink)
	if !errors.Is(err, ErrModelNotFound) || !empty(res) {
		t.Fatalf("the turn = %+v, %v; want ErrModelNotFound and an empty Result", res, err)
	}
	if reqs := a.requests(); len(reqs) != 4 || summarizers(reqs) != 1 {
		t.Fatalf("%d requests, %d of them the summarizer; want step 1, the summarizer, step 2, and the failed third", len(reqs), summarizers(reqs))
	}
	evs := ev.list()
	if got, want := usageOf(evs), (Usage{Input: over + under, Output: 10, CacheRead: 8}); got != want || !slices.Equal(stepsOf(evs), []int{1, 2}) {
		t.Fatalf("StepDones %v with usage %+v; want steps 1 and 2 with %+v", stepsOf(evs), got, want)
	}
	if cs := of[Compacted](evs); len(cs) != 2 || cs[1].Usage != oneStep(1) {
		t.Fatalf("Compacted = %+v; want the summarizer's usage on its ended event", cs)
	}
}

// TestNoRestartAfter (plan 028 §3.11 item 2): a segment that reaches the
// threshold and also ends by a save failure, unusable call ids, the
// doom-loop guard, an approved plan, a cancel or the step allowance ends the
// turn as it always has — no summarizer runs and no request follows.
func TestNoRestartAfter(t *testing.T) {
	// A step at the threshold whose five calls repeat one another: the guard
	// refuses the third and stops the turn at the fifth.
	doom := func() step {
		var parts [][]fantasy.StreamPart
		for i := 1; i <= doomStopAt; i++ {
			parts = append(parts, bareCall(fmt.Sprintf("c%d", i), "nope", `{"same":true}`))
		}
		return reply(cat(parts...), finishUsing(fantasy.FinishReasonToolCalls, over))
	}
	// A step at the threshold whose two calls share a provider id.
	badIDs := reply(cat(bareCall("c1", "nope", `{"n":1}`), bareCall("c1", "nope", `{"n":2}`)), finishUsing(fantasy.FinishReasonToolCalls, over))

	check := func(t *testing.T, s *Session, a *scripted, res Result, err error, wantStop string, wantErr error, requests int) {
		t.Helper()
		switch {
		case wantErr != nil && !errors.Is(err, wantErr):
			t.Fatalf("the turn = %+v, %v; want %v", res, err, wantErr)
		case wantErr == nil && (err != nil || res.StopReason != wantStop):
			t.Fatalf("the turn = %+v, %v; want %s", res, err, wantStop)
		}
		if reqs := a.requests(); len(reqs) != requests || summarizers(reqs) != 0 {
			t.Fatalf("%d requests, %d of them the summarizer; want %d and no compaction", len(reqs), summarizers(reqs), requests)
		}
		// A turn that persisted nothing left no file, and so no entry.
		if _, err := os.Stat(s.store.Path()); err == nil {
			if n := len(compactionEntries(t, s)); n != 0 {
				t.Fatalf("%d compaction entries, want none", n)
			}
		}
	}

	t.Run("a save failure", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		opts := f.options()
		opts.storeOpenFile = func(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
			file, err := os.OpenFile(name, flag, perm)
			if err != nil {
				return nil, err
			}
			return &failWriter{f: file, failAt: 1}, nil // the step's own write
		}
		s := f.open(opts)
		a := f.models["test/a"]
		a.push(toolStep(1, over), summaryOf("never"), answerWith("never"))
		res, err := s.Run(context.Background(), "loop", nil)
		if err == nil || !strings.Contains(err.Error(), "saving the answer") {
			t.Fatalf("the turn = %+v, %v; want the save failure", res, err)
		}
		check(t, s, a, res, err, "", err, 1)
	})

	t.Run("unusable call ids", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(f.options())
		a := f.models["test/a"]
		a.push(badIDs, summaryOf("never"), answerWith("never"))
		res, err := s.Run(context.Background(), "loop", nil)
		check(t, s, a, res, err, "", ErrBadToolCalls, 1)
	})

	t.Run("the doom-loop guard", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(f.options())
		a := f.models["test/a"]
		a.push(doom(), summaryOf("never"), answerWith("never"))
		res, err := s.Run(context.Background(), "loop", nil)
		check(t, s, a, res, err, StopMaxTurnRequests, nil, 1)
	})

	t.Run("an approved plan", func(t *testing.T) {
		asker := newParkedAsker()
		_, s, a := planSession(t, asker)
		setWindow(s, testWindow, 0)
		a.push(reply(callParts("c1", "exit_plan_mode", "{}"), finishUsing(fantasy.FinishReasonToolCalls, over)), summaryOf("never"), answerWith("never"))
		out := start(context.Background(), s, "plan it", nil)
		await(t, asker.asked, "the plan to be presented")
		asker.plan <- tool.PlanApproved
		o := await(t, out, "the turn")
		check(t, s, a, o.res, o.err, StopEndTurn, nil, 1)
	})

	t.Run("a cancel during the step's tool", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(f.options())
		a := f.models["test/a"]
		started := filepath.Join(f.workspace, "started")
		a.push(reply(callParts("c1", "bash", input(t, map[string]any{"command": ": > " + started + "; sleep 30"})), finishUsing(fantasy.FinishReasonToolCalls, over)),
			summaryOf("never"), answerWith("never"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, s, "wait", nil)
		waitFor(t, func() bool { return exists(started) }, "the command to start")
		cancel()
		o := await(t, out, "the turn")
		check(t, s, a, o.res, o.err, StopCancelled, nil, 1)
		if lines := entries(transcript(t, s)); len(lines) != 3 || !strings.HasPrefix(lines[2], "tool test/a high: [error c1: "+tool.AbortedText) {
			t.Fatalf("transcript:\n%s\nwant the aborted step persisted once", strings.Join(lines, "\n"))
		}
	})

	t.Run("the step allowance", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(f.options())
		a := f.models["test/a"]
		for n := 1; n < maxSteps; n++ {
			a.push(toolStep(n, under))
		}
		a.push(toolStep(maxSteps, over), summaryOf("never"), answerWith("never"))
		res, err := s.Run(context.Background(), "loop", nil)
		check(t, s, a, res, err, StopMaxTurnRequests, nil, maxSteps)
	})

	t.Run("a final step", func(t *testing.T) {
		// A final step never compacts mid-turn (§3.6), however large the
		// context it leaves: the turn is over, and the next turn's pre-turn
		// check is where that context is judged.
		f := newFixture(t, "http://127.0.0.1:1/v1")
		windowed(f, "test/a", testWindow, 0)
		s := f.open(f.options())
		a := f.models["test/a"]
		a.push(toolStep(1, under), answerSpending("done", over), summaryOf("never"), answerWith("never"))
		res, err := s.Run(context.Background(), "loop", nil)
		check(t, s, a, res, err, StopEndTurn, nil, 2)
	})
}

// TestTheSteerGateAcrossSegments (plan 028 §3.11 table, R2-2): the four
// cases of the gate that decides whether a request takes up the steers
// accepted so far. The turn's first request takes none (a steer accepted
// while it streams waits for the next request) unless a pre-turn compaction
// ran, when it takes the ones accepted during it; a segment's first request
// after a restart takes the ones accepted during the mid-turn compaction,
// whether or not a pre-turn one ran; and every later step takes what came
// since, as always.
func TestTheSteerGateAcrossSegments(t *testing.T) {
	for _, preTurn := range []bool{false, true} {
		t.Run(fmt.Sprintf("preTurn=%v", preTurn), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			windowed(f, "test/a", testWindow, 0)
			s := f.open(f.options())
			a := f.models["test/a"]
			before := int64(under)
			if preTurn {
				before = over
			}
			a.push(answerSpending("one", before))
			run(t, s, "turn one")

			g0, g1, g2, g3 := newGate(), newGate(), newGate(), newGate()
			if preTurn {
				a.push(heldSummary(g0, "turn one."))
			}
			a.push(
				g1.hold(bareCall("c1", "nope", `{"n":1}`), finishUsing(fantasy.FinishReasonToolCalls, over)), // step 1, at the threshold
				heldSummary(g2, "turn two."),
				g3.hold(bareCall("c2", "nope", `{"n":2}`), finishUsing(fantasy.FinishReasonToolCalls, under)), // step 2, the restart's first
				answerWith("done"), // step 3
			)
			var ev events
			out := start(context.Background(), s, "turn two", ev.sink)
			steer := func(g *gate, what, text string) {
				await(t, g.reached, what)
				if err := sendSteer(s, text); err != nil {
					t.Fatalf("Steer(%q) %s: %v", text, what, err)
				}
				close(g.release)
			}
			if preTurn {
				steer(g0, "during the pre-turn compaction", "s0")
			}
			steer(g1, "during the turn's first request", "s1")
			steer(g2, "during the mid-turn compaction", "s2")
			steer(g3, "during the restart's first request", "s3")
			o := await(t, out, "turn two")
			if o.err != nil || o.res.StopReason != StopEndTurn || len(o.res.Unanswered) != 0 {
				t.Fatalf("turn two = %+v, %v; want end_turn with every steer answered", o.res, o.err)
			}

			reqs := a.requests()
			reqs = reqs[1:] // turn one's
			if preTurn {
				if !isSummarizer(reqs[0]) {
					t.Fatalf("the pre-turn compaction did not run: %q", promptOf(reqs[0]))
				}
				reqs = reqs[1:]
			}
			if len(reqs) != 4 || !isSummarizer(reqs[1]) {
				t.Fatalf("%d requests after the pre-turn part; want step 1, the summarizer, step 2, step 3", len(reqs))
			}
			carries := func(c fantasy.Call, text string) bool { return slicesContains(promptOf(c), "user: "+text) }
			// The turn's first request: s0 iff a pre-turn compaction ran,
			// never s1.
			if carries(reqs[0], "s0") != preTurn || carries(reqs[0], "s1") {
				t.Fatalf("the first request carries s0: %v (want %v), s1: %v (want false): %q", carries(reqs[0], "s0"), preTurn, carries(reqs[0], "s1"), promptOf(reqs[0]))
			}
			// The restart's first request: s1 and s2, taken there, not
			// before.
			if !carries(reqs[2], "s1") || !carries(reqs[2], "s2") || carries(reqs[2], "s3") {
				t.Fatalf("the restart's first request carries s1: %v, s2: %v, s3: %v; want true, true, false: %q",
					carries(reqs[2], "s1"), carries(reqs[2], "s2"), carries(reqs[2], "s3"), promptOf(reqs[2]))
			}
			// The step after it: s3 too.
			if !carries(reqs[3], "s3") || !carries(reqs[3], "s1") {
				t.Fatalf("the restart's second request carries s3: %v, s1: %v; want both: %q", carries(reqs[3], "s3"), carries(reqs[3], "s1"), promptOf(reqs[3]))
			}
			var reported []string
			for _, e := range of[Steered](ev.list()) {
				reported = append(reported, e.Text)
			}
			want := []string{"s1", "s2", "s3"}
			if preTurn {
				want = []string{"s0", "s1", "s2", "s3"}
			}
			if !slices.Equal(reported, want) {
				t.Fatalf("Steered = %q, want %q: each once, in order", reported, want)
			}
		})
	}
}

// TestAFailedMidTurnCompactionRestartsOnTheSameHistory (plan 028 §3.11 item
// 9): a summarizer that fails writes its failure entry, switches automatic
// compaction off, and the turn goes on — the next segment replays the history
// it had, the failure entry counting for nothing — and no later boundary of
// the turn compacts again.
func TestAFailedMidTurnCompactionRestartsOnTheSameHistory(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", testWindow, 0)
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(toolStep(1, over),
		errorStep(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"}), // the summarizer, fatal at once
		toolStep(2, over), // at the threshold again: suppressed
		answerWith("done"))
	var ev events
	res, err := s.Run(context.Background(), "loop", ev.sink)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("the turn = %+v, %v; want end_turn", res, err)
	}
	reqs := a.requests()
	if len(reqs) != 4 || summarizers(reqs) != 1 || startsFromSummary(reqs[2]) {
		t.Fatalf("%d requests, %d of them the summarizer, the third from a summary: %v; want one failed compaction and the same history",
			len(reqs), summarizers(reqs), len(reqs) > 2 && startsFromSummary(reqs[2]))
	}
	// The restart replays what step 2 would have read anyway: the prompt,
	// step 1's call and its result, and nothing of the failure.
	if got, want := promptOf(reqs[2]), promptOf(reqs[0]); len(got) != len(want)+2 || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("the restart's request:\n%q\nwant the first request's messages, then step 1's call and result", got)
	}
	if got := compactions(t, s); !slices.Equal(got, []string{"auto test/a failed"}) {
		t.Fatalf("compactions = %q, want the one failure", got)
	}
	if !slices.Equal(stepsOf(ev.list()), []int{1, 2, 3}) || res.Usage != (Usage{Input: 2*over + 10, Output: 15, CacheRead: 12}) {
		t.Fatalf("steps %v, usage %+v; want three steps' usage, the failed summarizer's excluded", stepsOf(ev.list()), res.Usage)
	}
}

// TestAMidTurnCompactionThatCannotBeWrittenStopsTheTurn (P5): a compaction
// whose entry cannot be written stops the turn between requests, as a step
// that cannot be saved does — the error, with a steer accepted meanwhile
// unanswered — and the steps so far stay in the transcript, once.
func TestAMidTurnCompactionThatCannotBeWrittenStopsTheTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", testWindow, 0)
	opts := f.options()
	opts.storeOpenFile = func(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
		file, err := os.OpenFile(name, flag, perm)
		if err != nil {
			return nil, err
		}
		return &failWriter{f: file, failAt: 2}, nil // 1st write: the step; 2nd: the compaction entry
	}
	s := f.open(opts)
	a := f.models["test/a"]
	g := newGate()
	a.push(toolStep(1, over), heldSummary(g, "looping."), answerWith("never"))
	var ev events
	out := start(context.Background(), s, "loop", ev.sink)
	await(t, g.reached, "the summarizer's request")
	if err := sendSteer(s, "and this"); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	o := await(t, out, "the turn")
	if o.err == nil || !strings.Contains(o.err.Error(), "saving the compaction") || !errors.As(o.err, new(*errCompactionSaveFailed)) ||
		o.res.StopReason != "" || o.res.Usage != (Usage{}) || !slices.Equal(o.res.Unanswered, []string{"and this"}) {
		t.Fatalf("the turn = %+v, %v; want the compaction's save failure, with the steer unanswered and nothing else", o.res, o.err)
	}
	if reqs := a.requests(); len(reqs) != 2 {
		t.Fatalf("%d requests; want the step's and the summarizer's, and no more", len(reqs))
	}
	if lines := entries(transcript(t, s)); len(lines) != 3 {
		t.Fatalf("transcript:\n%s\nwant the one step, once", strings.Join(lines, "\n"))
	}
}

// lateCancelAgent is a turn's agent that reports a cancel landing after its
// final step was persisted as the stream's error. Fantasy's own loop ends on a
// final step and returns nil, but finish has always defended the case — the
// cancel came too late to stop anything — and this is how a test reaches that
// branch, as TestSynthesisDefence reaches its own. surfaced counts the times
// it did.
type lateCancelAgent struct {
	fantasy.Agent
	surfaced *int
}

func (a lateCancelAgent) Stream(ctx context.Context, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	res, err := a.Agent.Stream(ctx, c)
	if err == nil && ctx.Err() != nil {
		*a.surfaced++
		return nil, ctx.Err()
	}
	return res, err
}

// TestMidTurnCancelAfterTheFinalStepKeepsEverySegmentsUsage (plan 028 §3.11
// item 7, R3-3; astra r1-c11 finding 1): a segmented turn — a tool step at
// the threshold, the compaction, then the final answer — whose cancel lands
// once that answer is persisted, and whose agent returns it as the stream's
// error, ends as finish's too-late branch ends it: end_turn, with nothing
// written after the answer. The turn completed, so Result.Usage is the sum
// over both segments' steps, as a completed turn's always is — not the final
// step's alone. Only a turn that really is cancelled carries none.
func TestMidTurnCancelAfterTheFinalStepKeepsEverySegmentsUsage(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", testWindow, 0)
	s := f.open(f.options())
	surfaced := 0
	s.newAgent = func(lm fantasy.LanguageModel, system string, tools []fantasy.AgentTool) fantasy.Agent {
		return lateCancelAgent{Agent: defaultAgent(lm, system, tools), surfaced: &surfaced}
	}
	a := f.models["test/a"]
	a.push(toolStep(1, over), summaryOf("looping."), answerWith("done"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ev events
	res, err := s.Run(ctx, "loop", func(e Event) {
		ev.sink(e)
		if d, ok := e.(StepDone); ok && d.Step == 2 && d.Saved && d.StopReason == StopEndTurn {
			cancel()
		}
	})
	if surfaced != 1 {
		t.Fatalf("the late cancel was surfaced %d times; want once, by the second segment's stream", surfaced)
	}
	want := Usage{Input: over + 10, Output: 10, CacheRead: 8}
	if err != nil || res.StopReason != StopEndTurn || res.Usage != want {
		t.Fatalf("the turn = %+v, %v; want end_turn with both segments' steps summed, %+v", res, err, want)
	}
	if got := usageOf(ev.list()); got != want {
		t.Fatalf("the StepDones' usage = %+v, want %+v", got, want)
	}
	lines := entries(transcript(t, s))
	if last := lines[len(lines)-1]; last != "assistant test/a high end_turn: done" {
		t.Fatalf("transcript:\n%s\nwant the final answer last, as it was persisted", strings.Join(lines, "\n"))
	}
}

// TestMidTurnACarriedReminderIsReused (plan 028 §3.11 table, R4-1; astra
// r1-c11 finding 2): C12's seam, driven at the reminder's composition, since
// the overflow restart is C12's. A session with a plan already written
// switches from agent to plan mode; the turn's first request carries the
// re-entry notice — the alternation advancing as it goes out — and fails
// before its step finishes, so nothing of it is committed and newSegment
// carries the notice over, as C12's restart will. The replacement request
// carries that notice again and nothing beside it: exactly one reminder, and
// the parity where the failed request left it.
//
// A control pins what a carried notice is told apart from: the same notice
// merely retained — committed with its step, in the rebuilt history — is a
// transition the restart's request is no retry of, so the standing reminder
// is re-sent (sparse, at parity 1, which the re-send does not advance).
//
// And a carried notice for a mode the session has left since is not reused
// (X36, sol r3-c9b-c11a item 5): its request failed, so the model never heard
// it. Switched to ask while the turn compacted, the replacement request
// carries ask's notice alone, composed from the told state, not the plan
// notice beside it; switched back to agent — the mode the model was last
// told — it carries none, and the step that answers it records agent, not
// the plan mode the dropped notice announced.
func TestMidTurnACarriedReminderIsReused(t *testing.T) {
	// failed is the turn once its first request, carrying the re-entry
	// notice, went out: m's alternation is at 1 and nothing is told yet. The
	// modes each of its steps hands the store are logged.
	var logged []string
	failed := func(t *testing.T) (*modes, *turn) {
		t.Helper()
		plan := filepath.Join(t.TempDir(), "s.plan.md")
		if err := os.WriteFile(plan, []byte("## The plan\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := newModes(modeAgent, plan, tool.NewModeGate(modeAgent, nil))
		m.set(modePlan)
		logged = nil
		tn := &turn{ctx: context.Background(), modes: m, steers: &steerbox{},
			logMode:          func(mode string) error { logged = append(logged, mode); return nil },
			turnFirstRequest: true, segmentFirstRequest: true}
		if got := requestReminders(t, tn, []fantasy.Message{fantasy.NewUserMessage("plan it")}); !slices.Equal(got, []string{variantPlanReentry}) || m.turns != 1 {
			t.Fatalf("the turn's first request carries %q at parity %d; want the re-entry notice, the alternation at 1", got, m.turns)
		}
		return m, tn
	}
	summary := []fantasy.Message{fantasy.NewUserMessage("the summary")}

	t.Run("carried and still the mode: reused alone", func(t *testing.T) {
		m, tn := failed(t)
		// The request never finished: nothing committed, the summary
		// retains no reminder, and newSegment keeps the notice.
		tn.newSegment()
		got := requestReminders(t, tn, summary)
		if !slices.Equal(got, []string{variantPlanReentry}) {
			t.Fatalf("the replacement request carries %q; want the carried re-entry notice alone", got)
		}
		if m.turns != 1 {
			t.Fatalf("the alternation is at %d; want 1, where the failed request left it", m.turns)
		}
	})

	t.Run("control: retained, not carried, the standing reminder is re-sent", func(t *testing.T) {
		m, tn := failed(t)
		// The step finished and its append wrote the notice (stepFinished):
		// the model has been told, and the compaction kept it in the tail.
		tn.remWritten = len(tn.reminders)
		tn.modeHeard()
		tn.retainedReminder = variantPlanReentry
		tn.newSegment()
		if got := requestReminders(t, tn, summary); !slices.Equal(got, []string{variantPlanSparse}) || m.turns != 1 {
			t.Fatalf("the restart's request composes %q at parity %d; want the sparse standing text, the alternation still at 1", got, m.turns)
		}
	})

	t.Run("carried for a mode since left: dropped, the transition alone", func(t *testing.T) {
		m, tn := failed(t)
		tn.newSegment()
		m.set(modeAsk) // while the turn compacted
		if got := requestReminders(t, tn, summary); !slices.Equal(got, []string{variantAsk}) {
			t.Fatalf("the replacement request carries %q; want ask's notice alone, the plan notice nobody heard dropped", got)
		}
	})

	t.Run("carried for a mode since left, back in the told one: none", func(t *testing.T) {
		m, tn := failed(t)
		tn.newSegment()
		m.set(modeAgent) // while the turn compacted: what the model was last told
		if got := requestReminders(t, tn, summary); len(got) != 0 {
			t.Fatalf("the replacement request carries %q; want none: the model was never told of plan mode, so there is nothing to leave", got)
		}
		tn.modeChangeHeld() // the replacement's step, about to be appended
		if !slices.Equal(logged, []string{modeAgent}) {
			t.Fatalf("the replacement's step hands the store the mode changes %q; want agent, the told one — not the dropped notice's plan", logged)
		}
	})
}

// requestReminders drives one request of tn as Fantasy does — PrepareStep,
// then OnStepStart — and returns the variants of the reminders the request
// carries, in the order it carries them; and it holds them to the request's
// own messages, so a reminder recorded and not sent fails the test.
func requestReminders(t *testing.T, tn *turn, base []fantasy.Message) []string {
	t.Helper()
	_, prep, err := tn.prepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: base})
	if err != nil {
		t.Fatal(err)
	}
	if err := tn.stepStarted(0); err != nil {
		t.Fatal(err)
	}
	msgs := prep.Messages
	if msgs == nil {
		msgs = base
	}
	var sent []string
	for _, line := range promptOf(fantasy.Call{Prompt: msgs}) {
		if strings.Contains(line, "<"+reminderTag+">") {
			sent = append(sent, line)
		}
	}
	var variants []string
	for _, r := range tn.reminders {
		variants = append(variants, r.variant)
	}
	if len(sent) != len(variants) {
		t.Fatalf("the request carries %d reminders, the turn records %d (%q)", len(sent), len(variants), variants)
	}
	return variants
}

// A multi-byte tool-result character straddling the resultByteCap boundary:
// capResultText must back up to a rune boundary rather than cut at the fixed
// byte offset, so the segment's Markdown stays valid UTF-8, and the omitted
// count must reflect the actual (rune-aligned) cut.
func TestCapResultTextCutsOnRuneBoundaries(t *testing.T) {
	prefix := strings.Repeat("a", resultByteCap-2)
	s := prefix + "测" // a 3-byte rune occupies bytes resultByteCap-2..resultByteCap, straddling the cap
	got := capResultText(s)

	body, notice, ok := strings.Cut(got, "\n[")
	if !ok {
		t.Fatalf("capResultText = %q, want an omitted-bytes marker", got)
	}
	if !utf8.ValidString(body) {
		t.Fatalf("capResultText produced invalid UTF-8: %q", body)
	}
	if body != prefix {
		t.Errorf("capResultText cut at %d bytes (%q), want the straddling rune backed out entirely, keeping %q", len(body), body, prefix)
	}
	wantOmitted := len(s) - len(body)
	wantNotice := fmt.Sprintf("… %d bytes omitted]", wantOmitted)
	if notice != wantNotice {
		t.Errorf("capResultText notice = %q, want %q (omitted count must equal len(s)-cut)", notice, wantNotice)
	}
}
