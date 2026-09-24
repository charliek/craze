package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// Two turns: each streams its text, finishes end_turn with the step's usage,
// and is persisted; the second request carries the first turn's pair.
func TestTurnsCarryHistory(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(
		reply(reasoningParts("greet back"), textParts("hel", "lo"), finish(fantasy.FinishReasonStop)),
		answerWith("hello again"),
	)

	var ev events
	res, err := s.Run(context.Background(), "hi", ev.sink)
	if err != nil {
		t.Fatal(err)
	}
	wantUsage := Usage{Input: 10, Output: 5, CacheRead: 4}
	equal(t, "result", res, Result{StopReason: StopEndTurn, Usage: wantUsage})
	equal(t, "events", plain(ev.list()), []Event{
		ThoughtDelta{Text: "greet back"}, TextDelta{Text: "hel"}, TextDelta{Text: "lo"},
		done(1, fantasy.FinishReasonStop, StopEndTurn, 2), // the user entry and the answer
	})
	if d := of[StepDone](ev.list())[0]; d.TimeToFirstToken <= 0 {
		t.Errorf("StepDone's time to first token = %v, want the time the thinking took to start", d.TimeToFirstToken)
	}
	run(t, s, "again")

	calls := a.requests()
	if len(calls) != 2 {
		t.Fatalf("test/a saw %d requests, want 2", len(calls))
	}
	equal(t, "second request", promptOf(calls[1])[1:], []string{
		"user: hi",
		"assistant: (thinking: greet back) hello",
		"user: again",
	})
	for i, call := range calls {
		if call.MaxOutputTokens == nil || *call.MaxOutputTokens != 4096 {
			t.Errorf("request %d's output ceiling = %v, want the table's 4096", i+1, call.MaxOutputTokens)
		}
		if got := effortOf(t, call, "test"); got != "high" {
			t.Errorf("request %d's effort = %q, want high", i+1, got)
		}
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high end_turn: (thinking: greet back) hello",
		"user test/a high: again",
		"assistant test/a high end_turn: hello again",
	})
	if u := transcript(t, s).Entries[1].Usage; u == nil || *u != wantUsage {
		t.Errorf("the answer's usage was recorded as %+v, want %+v", u, wantUsage)
	}
}

// A model with no output ceiling and no effort control sends neither.
func TestNoCeilingNoEffort(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	opts.Model = "other/c"
	s := f.open(opts)
	f.models["other/c"].push(answerWith("ok"))
	run(t, s, "hi")
	call := f.models["other/c"].requests()[0]
	if call.MaxOutputTokens != nil || len(call.ProviderOptions) != 0 {
		t.Fatalf("request carried a ceiling %v and provider options %v; want neither", call.MaxOutputTokens, call.ProviderOptions)
	}
}

// Each finish reason's stop reason, in the result and in the file: "length"
// is max_tokens, "content-filter" is refusal (so `craze prompt` does not
// pass a filtered answer off as clean), and everything else is end_turn —
// "tool-calls" too, for a step that called no tool.
func TestFinishReasonsMapToStopReasons(t *testing.T) {
	cases := []struct {
		finish fantasy.FinishReason
		want   string
	}{
		{fantasy.FinishReasonStop, StopEndTurn},
		{fantasy.FinishReasonLength, StopMaxTokens},
		{fantasy.FinishReasonContentFilter, StopRefusal},
		{fantasy.FinishReasonToolCalls, StopEndTurn},
		{fantasy.FinishReasonError, StopEndTurn},
		{fantasy.FinishReasonOther, StopEndTurn},
		{fantasy.FinishReasonUnknown, StopEndTurn},
	}
	for _, tc := range cases {
		t.Run(string(tc.finish), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.models["test/a"].push(reply(textParts("cut"), finish(tc.finish)))
			if res := run(t, s, "go on"); res.StopReason != tc.want {
				t.Fatalf("stop reason %q, want %q", res.StopReason, tc.want)
			}
			equal(t, "transcript", entries(transcript(t, s)), []string{
				"user test/a high: go on",
				"assistant test/a high " + tc.want + ": cut",
			})
		})
	}
}

// Fantasy puts a text block in the step's messages only when the block's end
// part arrives. A provider that finishes without ending it still showed the
// user the whole answer, so the answer is persisted from the deltas —
// complete, with the step's stop reason and usage — and the next turn
// carries it.
func TestUnendedBlocksArePersisted(t *testing.T) {
	openReasoning := []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
		{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: "plan"},
	}
	cases := []struct {
		name  string
		parts []fantasy.StreamPart
		want  string
	}{
		{"text never ended", openText("hel", "lo"), "hello"},
		{"reasoning ended, text not", cat(reasoningParts("plan"), openText("hello")), "(thinking: plan) hello"},
		{"neither ended", cat(openReasoning, openText("hello")), "(thinking: plan) hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.models["test/a"].push(reply(tc.parts, finish(fantasy.FinishReasonLength)), answerWith("next"))
			if res := run(t, s, "hi"); res.StopReason != StopMaxTokens {
				t.Fatalf("stop reason %q, want the step's %q", res.StopReason, StopMaxTokens)
			}
			run(t, s, "again")
			tr := transcript(t, s)
			equal(t, "transcript", entries(tr), []string{
				"user test/a high: hi",
				"assistant test/a high max_tokens: " + tc.want,
				"user test/a high: again",
				"assistant test/a high end_turn: next",
			})
			if u := tr.Entries[1].Usage; u == nil || *u != (Usage{Input: 10, Output: 5, CacheRead: 4}) {
				t.Errorf("the answer's usage was recorded as %+v", u)
			}
			equal(t, "the next request", promptOf(f.models["test/a"].requests()[1])[1:], []string{
				"user: hi", "assistant: " + tc.want, "user: again",
			})
		})
	}
}

// A call to a tool the profile does not have — here with no input parts
// before it, as a provider that does not stream arguments sends one — is not
// run: Fantasy answers it with its own error, the model reads that at the
// next step, and the step persists paired, so the next turn replays it. The
// call is reported like any other (a ToolStarted made for it, since none
// streamed), and its result is the text the model read.
func TestUnknownToolIsAnErrorResult(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(
		reply(textParts("let me look"), bareCall("call-1", "read_file", `{"path":"x"}`), finish(fantasy.FinishReasonToolCalls)),
		answerWith("ok"),
		answerWith("next ok"),
	)
	var ev events
	res, err := s.Run(context.Background(), "read x", ev.sink)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", res, err)
	}
	notFound := "tool not found: read_file. Available tools: bash, read, glob, grep, edit, write, agent, todo_write, ask_user_question, exit_plan_mode"
	equal(t, "events", plain(ev.list()), []Event{
		TextDelta{Text: "let me look"},
		ToolStarted{ID: "t1.1.1", Step: 1, Tool: "read_file"},
		ToolCalled{ID: "t1.1.1", CallID: "call-1", Request: ToolRequest{Tool: "read_file", Input: `{"path":"x"}`}},
		ToolFinished{ID: "t1.1.1", Result: tool.Result{Text: notFound, IsError: true, Class: tool.ClassInvalidInput}},
		done(1, fantasy.FinishReasonToolCalls, StopToolUse, 3), // the user entry, the call, the result
		TextDelta{Text: "ok"},
		done(2, fantasy.FinishReasonStop, StopEndTurn, 1),
	})
	run(t, s, "next")
	equal(t, "the next request", promptOf(f.models["test/a"].requests()[2])[1:], []string{
		"user: read x",
		`assistant: let me look [call call-1 read_file {"path":"x"}]`,
		"tool: [error call-1: " + notFound + "]",
		"assistant: ok",
		"user: next",
	})
	if s.tools.d.Pending() != 0 {
		t.Fatalf("the dispatcher still holds %d prepared calls", s.tools.d.Pending())
	}
}

// cancelAt runs a turn whose step holds at g, cancels it there, and returns
// what Run returned.
func cancelAt(t *testing.T, s *Session, g *gate, sink func(Event)) outcome {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := start(ctx, s, "tell me", sink)
	await(t, g.reached, "the scripted step to reach its gate")
	cancel()
	return await(t, out, "the cancelled Run to return")
}

// A cancel mid-answer is a clean cancelled turn, and what streamed is
// persisted, interrupted — thinking included.
func TestCancelKeepsThePartialAnswer(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(cat(reasoningParts("think"), openText("par", "tial")), nil))

	var ev events
	got := cancelAt(t, s, g, ev.sink)
	if got.err != nil || !only(got.res, StopCancelled) {
		t.Fatalf("Run = %+v, %v; want a cancelled result and no error", got.res, got.err)
	}
	equal(t, "events", ev.list(), []Event{ThoughtDelta{Text: "think"}, TextDelta{Text: "par"}, TextDelta{Text: "tial"}})
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: tell me",
		"assistant test/a high cancelled interrupted: (thinking: think) partial",
	})
}

// A cancel before any output writes nothing: no file, and the cancelled
// prompt never appears — the next turn's replaces it.
func TestCancelBeforeOutputWritesNothing(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(nil, nil), answerWith("ok"))
	if got := cancelAt(t, s, g, nil); got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	noTranscript(t, s)
	run(t, s, "second")
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: second",
		"assistant test/a high end_turn: ok",
	})
}

// Thinking alone is not an answer: a cancel while the model only reasoned
// writes nothing (plan 018 §3.6).
func TestCancelWhileOnlyThinkingWritesNothing(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(reasoningParts("hmm", " let me see"), nil))
	var ev events
	if got := cancelAt(t, s, g, ev.sink); got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	equal(t, "events", ev.list(), []Event{ThoughtDelta{Text: "hmm"}, ThoughtDelta{Text: " let me see"}})
	noTranscript(t, s)
}

// A cancel that lands once the step is persisted is too late: the turn ends
// with the step's stop reason and the file holds one, complete answer.
func TestCancelAfterTheStepIsPersisted(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("done"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := s.Run(ctx, "hi", func(ev Event) {
		if _, ok := ev.(StepDone); ok {
			cancel()
		}
	})
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", res, err)
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high end_turn: done",
	})
}

// A failure after output began returns the classified error and a zero
// Result, and keeps what streamed, interrupted, with no stop reason.
func TestFailureKeepsThePartialAnswer(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(reply(openText("half an"), errorPart(&llm.MidStreamError{Message: "stream error - upstream gone"})))
	res, err := s.Run(context.Background(), "hi", nil)
	var pe *ProviderError
	if !empty(res) || !errors.As(err, &pe) || pe.Message != "stream error - upstream gone" {
		t.Fatalf("Run = %+v, %#v; want a zero Result and the provider's error", res, err)
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high interrupted: half an",
	})
}

// Fantasy replays a retried step from the start. What the failed attempt
// streamed must not survive into the answer: here the retried attempt is
// then cancelled, and only its own text is persisted.
func TestRetryResetsTheDeltas(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	busy := &fantasy.ProviderError{Message: "overloaded", StatusCode: 503, ResponseHeaders: map[string]string{"retry-after-ms": "1"}}
	f.models["test/a"].push(
		reply(reasoningParts("stale thought"), openText("stale "), errorPart(busy)),
		g.hold(openText("fresh"), nil),
	)
	var ev events
	if got := cancelAt(t, s, g, ev.sink); got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	equal(t, "events", ev.list(), []Event{
		ThoughtDelta{Text: "stale thought"}, TextDelta{Text: "stale "},
		Retrying{Delay: time.Millisecond, Attempt: 1, Reason: "HTTP 503: overloaded"},
		TextDelta{Text: "fresh"},
	})
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: tell me",
		"assistant test/a high cancelled interrupted: fresh",
	})
}

// One Run at a time; the refusal is immediate and leaves the live turn be.
func TestOneTurnAtATime(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(openText("first"), finishText()))
	out := start(context.Background(), s, "one", nil)
	await(t, g.reached, "the first turn to stream")
	if _, err := s.Run(context.Background(), "two", nil); !errors.Is(err, ErrInTurn) {
		t.Fatalf("a second Run = %v, want ErrInTurn", err)
	}
	close(g.release)
	if got := await(t, out, "the first turn to finish"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("the first turn = %+v, %v; want end_turn", got.res, got.err)
	}
	if n := len(f.models["test/a"].requests()); n != 1 {
		t.Fatalf("test/a saw %d requests, want 1: the refused turn was sent", n)
	}
}

// Close during a turn cancels it and waits: by the time Close returns, the
// partial answer is in the file.
func TestCloseDuringATurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(openText("unfinished"), nil))
	out := start(context.Background(), s, "hi", nil)
	await(t, g.reached, "the turn to stream")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Read before Run's result: Close alone must have waited for the save.
	equal(t, "transcript at Close", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high cancelled interrupted: unfinished",
	})
	if got := await(t, out, "Run to return"); got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Two Closes at once during a turn: both wait for it.
func TestConcurrentCloses(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(openText("unfinished"), nil))
	out := start(context.Background(), s, "hi", nil)
	await(t, g.reached, "the turn to stream")
	closed := make(chan error, 2)
	for range 2 {
		go func() { closed <- s.Close() }()
	}
	for i := range 2 {
		if err := await(t, closed, "a Close to return"); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	if n := len(transcript(t, s).Entries); n != 2 {
		t.Fatalf("the transcript has %d entries after both Closes, want the turn's 2", n)
	}
	await(t, out, "Run to return")
}

// Close racing a turn that is finishing on its own: whichever wins, the
// outcome is one of the legal ones and nothing is left running. Under -race
// with -count this is the interleaving check.
func TestCloseRacesAFinishingTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("quick"))
	out := start(context.Background(), s, "hi", nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := await(t, out, "Run to return")
	switch {
	case errors.Is(got.err, ErrClosed): // Close won: the turn never started
	case got.err == nil && (got.res.StopReason == StopEndTurn || got.res.StopReason == StopCancelled):
	default:
		t.Fatalf("Run = %+v, %v; want end_turn, cancelled, or ErrClosed", got.res, got.err)
	}
}

// A switch during a turn takes effect on the next one: the running turn
// finishes on its model, Current changes at once, and the change entry is
// written ahead of the next turn's prompt.
func TestModelSwitchDuringATurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(openText("from a"), finishText()))
	f.models["other/c"].push(answerWith("from c"))

	out := start(context.Background(), s, "one", nil)
	await(t, g.reached, "the first turn to stream")
	if err := s.SetModel("other/c"); err != nil {
		t.Fatal(err)
	}
	if m, e := s.Current(); m != "other/c" || e != "" {
		t.Fatalf("Current() = %q, %q during the turn; want other/c, none", m, e)
	}
	close(g.release)
	if got := await(t, out, "the first turn to finish"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("the first turn = %+v, %v; want end_turn", got.res, got.err)
	}
	run(t, s, "two")

	if n := len(f.models["test/a"].requests()); n != 1 {
		t.Fatalf("test/a saw %d requests, want 1", n)
	}
	c := f.models["other/c"].requests()
	if len(c) != 1 {
		t.Fatalf("other/c saw %d requests, want 1", len(c))
	}
	// test/a's answer had no reasoning, so the filter has nothing to drop.
	equal(t, "other/c's request", promptOf(c[0])[1:], []string{"user: one", "assistant: from a", "user: two"})
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: one",
		"assistant test/a high end_turn: from a",
		"model_change other/c",
		"effort_change ",
		"user other/c: two",
		"assistant other/c end_turn: from c",
	})
}

// Reasoning goes back only to the model that produced it: after a switch to
// another wire model, even on the same provider, the history drops it.
func TestSwitchDropsForeignReasoning(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(reply(reasoningParts("a's thought"), textParts("a says"), finish(fantasy.FinishReasonStop)))
	f.models["test/b"].push(answerWith("b says"))
	run(t, s, "one")
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	run(t, s, "two")
	equal(t, "test/b's request", promptOf(f.models["test/b"].requests()[0])[1:], []string{
		"user: one", "assistant: a says", "user: two",
	})
	// The file keeps it.
	if got := entries(transcript(t, s))[1]; got != "assistant test/a high end_turn: (thinking: a's thought) a says" {
		t.Fatalf("the first answer reads back as %q", got)
	}
}

// Switches between turns collapse: several record only the last, a switch
// and back record nothing, and a recorded switch is not recorded again.
func TestSwitchesCollapse(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("a1"), answerWith("a2"))
	f.models["test/b"].push(answerWith("b1"), answerWith("b2"))
	run(t, s, "one")
	for _, alias := range []string{"test/b", "other/c", "test/a"} { // and back
		if err := s.SetModel(alias); err != nil {
			t.Fatal(err)
		}
	}
	run(t, s, "two")
	if err := s.SetEffort("low"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffort("high"); err != nil { // and back
		t.Fatal(err)
	}
	for _, alias := range []string{"other/c", "test/b"} {
		if err := s.SetModel(alias); err != nil {
			t.Fatal(err)
		}
	}
	run(t, s, "three")
	run(t, s, "four")
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: one",
		"assistant test/a high end_turn: a1",
		"user test/a high: two",
		"assistant test/a high end_turn: a2",
		"model_change test/b",
		"effort_change medium", // other/c had none; test/b's default
		"user test/b medium: three",
		"assistant test/b medium end_turn: b1",
		"user test/b medium: four",
		"assistant test/b medium end_turn: b2",
	})
}

// An effort switch applies to the next request and is recorded ahead of it.
func TestEffortSwitchBetweenTurns(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("a1"), answerWith("a2"))
	run(t, s, "one")
	if err := s.SetEffort("low"); err != nil {
		t.Fatal(err)
	}
	run(t, s, "two")
	calls := f.models["test/a"].requests()
	if a, b := effortOf(t, calls[0], "test"), effortOf(t, calls[1], "test"); a != "high" || b != "low" {
		t.Fatalf("efforts sent = %q, %q; want high, low", a, b)
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: one",
		"assistant test/a high end_turn: a1",
		"effort_change low",
		"user test/a low: two",
		"assistant test/a low end_turn: a2",
	})
}

// A switch made before a turn that then produced nothing is still recorded,
// with the next turn that does; switching back in between records the way
// back, since the store may be holding the first.
func TestSwitchSurvivesAnEmptyTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(answerWith("a1"), answerWith("a2"))
	f.models["test/b"].push(g.hold(nil, nil))
	run(t, s, "one")
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	cancelAt(t, s, g, nil)
	if err := s.SetModel("test/a"); err != nil {
		t.Fatal(err)
	}
	run(t, s, "three")
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: one",
		"assistant test/a high end_turn: a1",
		"model_change test/a",
		"user test/a high: three",
		"assistant test/a high end_turn: a2",
	})
}

// handedOver is what begin's change entries write: it applies them to a
// fresh store, writes one turn, and returns the change lines that precede it.
func handedOver(t *testing.T, f *fixture, changes []func(*store.Store) error) []string {
	t.Helper()
	st, err := store.New(store.Options{Home: t.TempDir(), Workspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, c := range changes {
		if err := c(st); err != nil {
			t.Fatal(err)
		}
	}
	m := store.Model{Provider: "test", Alias: "test/a", WireModel: "wire-a"}
	if err := st.AppendUser(store.MessageEntry{Message: fantasy.NewUserMessage("q"), Model: m}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAssistant(store.MessageEntry{Message: streamed("", "a"), Model: m}); err != nil {
		t.Fatal(err)
	}
	tr, err := store.Load(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := entries(tr)
	return lines[:len(lines)-2]
}

// A switch the store refused is not marked as recorded: the next turn hands
// it over again rather than losing it.
func TestRefusedSwitchIsHandedOverAgain(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Close(); err != nil { // every append now fails
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), "hi", nil); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Run = %v, want the store's refusal", err)
	}
	_, changes, _, _, err := s.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.end()
	equal(t, "changes the next turn hands over", handedOver(t, f, changes), []string{"model_change test/b"})
}

// What a turn marks as recorded is its own model and effort, not whatever a
// SetModel made current after the turn began: that later switch is still
// handed over by the turn after.
func TestRecordedIsTheTurnsModel(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	m, changes, _, _, err := s.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("other/c"); err != nil { // during the turn, before its handover
		t.Fatal(err)
	}
	if err := s.record(m, changes); err != nil {
		t.Fatal(err)
	}
	s.end()
	_, next, _, _, err := s.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.end()
	equal(t, "changes the next turn hands over", handedOver(t, f, next), []string{"model_change other/c", "effort_change "})
}

// The sink runs on the turn and may call back into the session: the runner
// holds no lock while it runs. A switch made there affects the next turn.
func TestSinkMayCallTheSession(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("one"), answerWith("two"))
	switched := false
	sink := func(ev Event) {
		s.Current()
		s.Models()
		if _, ok := ev.(TextDelta); ok && !switched {
			switched = true
			if err := s.SetEffort("low"); err != nil {
				t.Errorf("SetEffort from the sink: %v", err)
			}
			if _, e := s.Current(); e != "low" {
				t.Errorf("Current() from the sink = %q, want low", e)
			}
		}
	}
	out := make(chan outcome, 1)
	go func() {
		res, err := s.Run(context.Background(), "hi", sink)
		out <- outcome{res, err}
	}()
	if got := await(t, out, "a turn whose sink calls the session"); got.err != nil {
		t.Fatal(got.err)
	}
	run(t, s, "again")
	calls := f.models["test/a"].requests()
	if a, b := effortOf(t, calls[0], "test"), effortOf(t, calls[1], "test"); a != "high" || b != "low" {
		t.Fatalf("efforts sent = %q, %q; want high (the running turn's), then low", a, b)
	}
}

// A finished answer that cannot be saved fails the turn: the user saw the
// whole answer, but the transcript no longer has it, and they should hear
// about that. (Fantasy ignores OnStepFinish's error; the runner keeps it.)
func TestFailedSaveFailsTheTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("lost"))
	var ev events
	res, err := s.Run(context.Background(), "hi", func(e Event) {
		ev.sink(e)
		if _, ok := e.(TextDelta); ok {
			_ = s.store.Close() // the user entry is held; the answer's save will fail
		}
	})
	if !errors.Is(err, store.ErrClosed) || !empty(res) {
		t.Fatalf("Run = %+v, %v; want a zero Result and the store's error", res, err)
	}
	// The step reports its failed save: a Diag, and a StepDone that says it
	// was not saved, and why, with no entry ids.
	evs := plain(ev.list())
	failed := done(1, fantasy.FinishReasonStop, StopEndTurn, 0)
	failed.SaveError = store.ErrClosed.Error()
	equal(t, "events", evs, []Event{
		TextDelta{Text: "lost"},
		Diag{Kind: DiagSaveFailed, Fields: map[string]string{"step": "1", "error": store.ErrClosed.Error()}},
		failed,
	})
	noTranscript(t, s)
}
