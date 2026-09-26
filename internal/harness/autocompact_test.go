package harness

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// When a session compacts on its own (plan 028 §3.6, §3.7, §3.17; §7 A20–A22):
// the pre-turn check at the threshold, the frontier's count, suppression, the
// previous-model rule, and a child's compactions in what its parent is told
// it spent. The scripted models report whatever usage a test gives a step, so
// a context's size is set by the usage its last answer reports, and the
// estimate of what follows it.

// windowed gives alias a context window and an output ceiling in f's table,
// before a session opens on it or switches to it (a model's limits are fixed
// when it is built).
func windowed(f *fixture, alias string, window, maxOut int) {
	m := f.table.Models[alias]
	m.ContextWindow, m.MaxOutputTokens = window, maxOut
	f.table.Models[alias] = m
}

// setWindow gives the model s's next turn runs on a context window and an
// output ceiling, as a table entry would: for a test that sizes the window
// by the session's own system prompt and tools, which it knows only once the
// session is open.
func setWindow(s *Session, window, maxOut int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.r.ContextWindow, s.cur.r.MaxOutputTokens = window, maxOut
}

// finishUsing ends a step with reason, reporting input tokens and
// stepUsage's output and cache reads (9 more tokens of context).
func finishUsing(reason fantasy.FinishReason, input int64) []fantasy.StreamPart {
	u := stepUsage
	u.InputTokens, u.TotalTokens = input, input+stepUsage.OutputTokens
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: reason, Usage: u}}
}

// answerSpending is a one-step answer whose request reported input tokens:
// the context after it is input + 9.
func answerSpending(text string, input int64) step {
	return reply(textParts(text), finishUsing(fantasy.FinishReasonStop, input))
}

// summaryReply is a summarizer's answer compact accepts.
func summaryReply(what string) step { return answerWith(longSummary("1. Request and intent\n" + what)) }

// promptTokens is what the pre-turn check adds for a turn's prompt.
func promptTokens(text string) int64 { return messageTokens(fantasy.NewUserMessage(text)) }

// isSummarizer reports whether c is a compaction's request: its last message
// carries the compaction prompt.
func isSummarizer(c fantasy.Call) bool {
	return len(c.Prompt) > 0 && strings.Contains(messageText(c.Prompt[len(c.Prompt)-1]), "1. Request and intent")
}

// startsFromSummary reports whether c's history starts with a compaction's
// summary message: its first user message is one.
func startsFromSummary(c fantasy.Call) bool {
	for _, m := range c.Prompt {
		if m.Role == fantasy.MessageRoleUser {
			return strings.HasPrefix(messageText(m), "<"+compactedTag+">")
		}
	}
	return false
}

// summarizers counts the compaction requests among c.
func summarizers(c []fantasy.Call) int {
	n := 0
	for _, r := range c {
		if isSummarizer(r) {
			n++
		}
	}
	return n
}

// compactions summarizes s's compaction entries, one line each: reason,
// model alias, and "ok" or "failed".
func compactions(t *testing.T, s *Session) []string {
	t.Helper()
	var out []string
	for _, e := range compactionEntries(t, s) {
		outcome := "ok"
		if !e.Compaction.Succeeded() {
			outcome = "failed"
		}
		out = append(out, e.Compaction.Reason+" "+e.Model.Alias+" "+outcome)
	}
	return out
}

// TestPreTurnCompactionAtTheThreshold (A20): a turn whose context and prompt
// reach the threshold compacts before its first request — at 85% of the
// window, 84% not — and a turn under it does not; the threshold is capped by
// the window less the output ceiling (P32), and its percentage is
// [compaction]'s. The compaction is recorded as the turn it precedes, is
// written ahead of that turn's prompt, reports its summarizer's usage, and
// the turn's request starts from its summary.
func TestPreTurnCompactionAtTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		name           string
		window, maxOut int
		percent        int   // [compaction] threshold_percent; 0 leaves it out
		at             int64 // the context and the prompt, in tokens
		compacts       bool
	}{
		{"84% of the window does not compact", 100000, 4096, 0, 84000, false},
		{"just under 85% does not compact", 100000, 4096, 0, 84999, false},
		{"85% compacts", 100000, 4096, 0, 85000, true},
		{"just under the output ceiling's cap does not compact", 100000, 20000, 0, 79999, false},
		{"the output ceiling caps the threshold", 100000, 20000, 0, 80000, true},
		{"a configured 50% does not compact under it", 100000, 0, 50, 49999, false},
		{"a configured 50% compacts at it", 100000, 0, 50, 50000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://unused")
			windowed(f, "test/a", tc.window, tc.maxOut)
			if tc.percent != 0 {
				f.table.Compaction.ThresholdPercentSet = &tc.percent
			}
			s := f.open(f.options())
			a := f.models["test/a"]
			const second = "turn two"
			a.push(answerSpending("one", tc.at-9-promptTokens(second)))
			run(t, s, "turn one")
			if got, _ := s.contextTokens(s.cur); got+promptTokens(second) != tc.at {
				t.Fatalf("control: the context and the prompt = %d tokens, want %d", got+promptTokens(second), tc.at)
			}

			if tc.compacts {
				a.push(summaryReply("turn one."))
			}
			a.push(answerWith("two"))
			var ev events
			if res, err := s.Run(context.Background(), second, ev.sink); err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("turn two = %+v, %v; want end_turn", res, err)
			}
			reqs := a.requests()
			if !tc.compacts {
				if len(reqs) != 2 || summarizers(reqs) != 0 || len(compactions(t, s)) != 0 || len(of[Compacted](ev.list())) != 0 {
					t.Fatalf("under the threshold: %d requests, %d compactions; want turn two's own request and no compaction",
						len(reqs), len(compactions(t, s)))
				}
				return
			}
			if len(reqs) != 3 || !isSummarizer(reqs[1]) || isSummarizer(reqs[2]) || !startsFromSummary(reqs[2]) {
				t.Fatalf("at the threshold: requests %d (summarizer at 1: %v); want turn one, the summarizer, then turn two from the summary",
					len(reqs), len(reqs) > 1 && isSummarizer(reqs[1]))
			}
			ces := compactionEntries(t, s)
			if len(ces) != 1 || ces[0].Compaction.Reason != store.CompactionAuto || !ces[0].Compaction.Succeeded() ||
				ces[0].Turn != 2 {
				t.Fatalf("compaction entries = %+v; want one automatic success recorded as turn 2", ces)
			}
			got := entries(transcript(t, s))
			if want := "compaction auto no-tail " + store.SegmentName(1); !slices.Equal(got[2:], []string{want, "user test/a high: turn two", "assistant test/a high end_turn: two"}) {
				t.Fatalf("transcript = %q; want the compaction ahead of turn two's prompt", got)
			}
			evs := ev.list()
			cs := of[Compacted](evs)
			if len(cs) != 2 || cs[0].Phase != CompactionStarted || cs[1].Phase != CompactionEnded || cs[1].Reason != store.CompactionAuto ||
				cs[1].Err != "" || cs[1].Usage != oneStep(1) {
				t.Fatalf("Compacted = %+v; want started, then ended with the summarizer's usage", cs)
			}
			if slices.IndexFunc(evs, func(e Event) bool { _, ok := e.(Compacted); return ok }) >
				slices.IndexFunc(evs, func(e Event) bool { _, ok := e.(StepDone); return ok }) {
				t.Fatal("the compaction was reported after the turn's step")
			}
		})
	}
}

// TestUnknownWindowNeverAutoCompacts (A20, PD12): a model whose window is
// unknown never compacts on its own, however large its context; /compact
// still does. So does a window its output ceiling takes whole: it has no
// threshold either.
func TestUnknownWindowNeverAutoCompacts(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options()) // test/a has no context_window
	a := f.models["test/a"]
	a.push(answerSpending("one", 50_000_000))
	run(t, s, "turn one")
	a.push(answerWith("two"))
	run(t, s, "turn two")
	if reqs := a.requests(); len(reqs) != 2 || len(compactions(t, s)) != 0 {
		t.Fatalf("%d requests, compactions %q; want no compaction", len(reqs), compactions(t, s))
	}

	a.push(summaryReply("both turns."))
	if res, err := s.Compact(context.Background(), "", "/compact", nil); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Compact = %+v, %v; want /compact to work with an unknown window", res, err)
	}
	if got := compactions(t, s); !slices.Equal(got, []string{"manual test/a ok"}) {
		t.Fatalf("compactions = %q; want the manual one", got)
	}

	for _, r := range []modeltable.Resolved{{}, {ContextWindow: 4096, MaxOutputTokens: 4096}, {ContextWindow: 1000, MaxOutputTokens: 5000}} {
		if got := compactionThreshold(r, 85); got != 0 {
			t.Errorf("the threshold of window %d, ceiling %d = %d; want 0: no automatic compaction", r.ContextWindow, r.MaxOutputTokens, got)
		}
	}
}

// TestAutoFalseNeverAutoCompacts: [compaction] auto = false turns automatic
// compaction off for every model, over the threshold or not.
func TestAutoFalseNeverAutoCompacts(t *testing.T) {
	f := newFixture(t, "http://unused")
	windowed(f, "test/a", 100000, 0)
	off := false
	f.table.Compaction.AutoSet = &off
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerSpending("one", 99_000))
	run(t, s, "turn one")
	a.push(answerWith("two"))
	run(t, s, "turn two")
	if reqs := a.requests(); len(reqs) != 2 || len(compactions(t, s)) != 0 {
		t.Fatalf("%d requests, compactions %q; want no compaction with auto off", len(reqs), compactions(t, s))
	}
}

// TestTheToolMessageIsCountedOnce (A20, P16): a context's size is its last
// usage-carrying answer's usage — which covers everything that request sent,
// the tool message before it included — plus the estimate of what follows
// it: after a turn that finished, nothing; after a tool step whose next
// request never happened, that step's own tool message, once.
func TestTheToolMessageIsCountedOnce(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(reply(globPart("g1"), finishUsing(fantasy.FinishReasonToolCalls, 1000)),
		answerSpending("done", 2000))
	run(t, s, "turn one")
	if got, fr := s.contextTokens(s.cur); got != 2000+9 || fr == nil || fr.Usage.Input != 2000 {
		t.Fatalf("after a finished tool turn: %d tokens (frontier %+v); want the answer's usage alone, 2009: "+
			"its request already sent the tool message", got, fr)
	}

	g := newGate()
	a.push(reply(globPart("g2"), finishUsing(fantasy.FinishReasonToolCalls, 3000)), g.hold(nil, textParts("never")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := start(ctx, s, "turn two", nil)
	await(t, g.reached, "turn two's second request")
	cancel()
	if o := await(t, out, "turn two"); o.err != nil || o.res.StopReason != StopCancelled {
		t.Fatalf("turn two = %+v, %v; want cancelled", o.res, o.err)
	}
	msgs := s.store.Context(s.cur.id())
	toolMsg := msgs[len(msgs)-1]
	if toolMsg.Role != fantasy.MessageRoleTool {
		t.Fatalf("control: the context ends with a %s message, want the tool step's results", toolMsg.Role)
	}
	want := 3000 + 9 + messageTokens(toolMsg)
	if got, _ := s.contextTokens(s.cur); got != want {
		t.Fatalf("after a tool step: %d tokens; want its usage and its tool message once: %d (twice would be %d)",
			got, want, want+messageTokens(toolMsg))
	}
}

// TestAStillOverCompactionSuppresses (A20, PD23): a compaction that leaves
// the context at or over the threshold — here the system prompt and the tools
// alone are — switches automatic compaction off, so the next turn does not
// pay for another; a model change switches it back on, and the compaction
// there, still over, switches it off again for the new model. Each boundary
// compacts once at most.
func TestAStillOverCompactionSuppresses(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	base := s.estimateContext(nil)
	window := int(base) // its threshold, 85% of it, is under the prompt and tools alone
	setWindow(s, window, 0)
	big := 2 * base
	a := f.models["test/a"]

	a.push(answerSpending("one", big))
	run(t, s, "turn one") // nothing stored yet: nothing to compact, no compaction
	a.push(summaryReply("turn one."), answerSpending("two", big))
	run(t, s, "turn two")
	ces := compactionEntries(t, s)
	threshold := compactionThreshold(s.cur.r, 85)
	if len(ces) != 1 || !ces[0].Compaction.Succeeded() || ces[0].Compaction.TokensAfter < threshold {
		t.Fatalf("compaction entries = %+v; want one success still at or over the threshold %d", ces, threshold)
	}

	a.push(answerSpending("three", big))
	run(t, s, "turn three")
	if reqs := a.requests(); len(reqs) != 4 || summarizers(reqs) != 1 {
		t.Fatalf("%d requests, %d of them compactions; want turn three to go out without one (suppressed)", len(reqs), summarizers(reqs))
	}

	// A model change: test/b, with the same small window, compacts again —
	// on test/a, the model that holds the context (PD13) — and, still over,
	// is suppressed in turn.
	windowed(f, "test/b", window, 0)
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	a.push(summaryReply("turns one to three."))
	b := f.models["test/b"]
	b.push(answerSpending("four", big))
	run(t, s, "turn four")
	b.push(answerWith("five"))
	run(t, s, "turn five")
	if got, want := compactions(t, s), []string{"auto test/a ok", "auto test/a ok"}; !slices.Equal(got, want) {
		t.Fatalf("compactions = %q, want %q: one after the model change, none on turn five", got, want)
	}
	if reqs := b.requests(); len(reqs) != 2 || summarizers(reqs) != 0 {
		t.Fatalf("test/b's requests: %d, %d compactions; want turns four and five alone", len(reqs), summarizers(reqs))
	}
}

// TestAFailedAutoCompactionSuppressesUntilSuccess (A20, PD14): an automatic
// compaction whose summarizer fails writes its failure entry and lets the
// turn go on with the context it had, and switches automatic compaction off;
// a /compact that gets the context under the threshold switches it back on.
func TestAFailedAutoCompactionSuppressesUntilSuccess(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	s.sleep = func(time.Duration) {}
	base := s.estimateContext(nil)
	setWindow(s, int(4*base), 0) // a summary and the prompt and tools fit well under it
	big := 5 * base
	a := f.models["test/a"]

	a.push(answerSpending("one", big))
	run(t, s, "turn one")
	a.push(errorStep(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"}), answerSpending("two", big))
	if res, err := s.Run(context.Background(), "turn two", nil); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("turn two = %+v, %v; want the turn to go on after the failed compaction", res, err)
	}
	reqs := a.requests()
	if len(reqs) != 3 || startsFromSummary(reqs[2]) || firstUserText(reqs[2]) != "turn one" {
		t.Fatalf("turn two's request = %q; want the context it had", promptOf(reqs[len(reqs)-1]))
	}

	a.push(answerSpending("three", big))
	run(t, s, "turn three")
	if reqs := a.requests(); len(reqs) != 4 {
		t.Fatalf("%d requests; want turn three's alone: automatic compaction is off", len(reqs))
	}

	a.push(summaryReply("turns one to three."))
	if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	a.push(answerSpending("four", big))
	run(t, s, "turn four") // under the threshold after the summary
	a.push(summaryReply("all four."), answerWith("five"))
	run(t, s, "turn five")
	want := []string{"auto test/a failed", "manual test/a ok", "auto test/a ok"}
	if got := compactions(t, s); !slices.Equal(got, want) {
		t.Fatalf("compactions = %q, want %q: on again after the /compact", got, want)
	}
}

// TestSwitchToASmallerWindowCompactsWithThePreviousModel (A21, PD13): a
// switch to a model whose window the context is over compacts before the
// turn on the previous model — the one holding the context and its cache —
// through the aligned request; when that model no longer resolves, on the
// turn's model in the text form, with a warning. One compaction either way.
func TestSwitchToASmallerWindowCompactsWithThePreviousModel(t *testing.T) {
	for _, gone := range []bool{false, true} {
		name := "the previous model"
		if gone {
			name = "the previous model no longer resolves"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "http://unused")
			windowed(f, "test/a", 1_000_000, 4096)
			windowed(f, "test/b", 100_000, 0)
			var warned []string
			opts := f.options()
			opts.Warn = func(line string) { warned = append(warned, line) }
			s := f.open(opts)
			a, b := f.models["test/a"], f.models["test/b"]
			a.push(answerSpending("one", 90_000)) // well under test/a's threshold, over test/b's
			run(t, s, "turn one")
			if gone {
				// Re-pointed: no alias names (test, wire-a) any more.
				m := f.table.Models["test/a"]
				m.WireModel = "wire-a2"
				f.table.Models["test/a"] = m
			}
			if err := s.SetModel("test/b"); err != nil {
				t.Fatal(err)
			}
			if gone {
				b.push(summaryReply("turn one, from the text form."))
			} else {
				a.push(summaryReply("turn one, on a."))
			}
			b.push(answerWith("two"))
			run(t, s, "turn two")

			ces := compactionEntries(t, s)
			if len(ces) != 1 || !ces[0].Compaction.Succeeded() || ces[0].Turn != 2 {
				t.Fatalf("compaction entries = %+v; want one success, recorded as turn 2", ces)
			}
			tr := entries(transcript(t, s))
			if !slices.Equal(tr[2:4], []string{"compaction auto no-tail " + store.SegmentName(1), "model_change test/b"}) {
				t.Fatalf("transcript = %q; want the compaction, then the switch and turn two", tr)
			}
			ar, br := a.requests(), b.requests()
			if !gone {
				if ces[0].Model.Alias != "test/a" || len(ar) != 2 || !isSummarizer(ar[1]) || len(ar[1].Tools) == 0 {
					t.Fatalf("the compaction ran on %s (test/a's requests %d); want test/a's aligned request, tools and all",
						ces[0].Model.Alias, len(ar))
				}
				if len(br) != 1 || !startsFromSummary(br[0]) || len(warned) != 0 {
					t.Fatalf("test/b's requests = %d, warnings %q; want turn two alone, from the summary", len(br), warned)
				}
				return
			}
			if ces[0].Model.Alias != "test/b" || len(ar) != 1 || len(br) != 2 || !isSummarizer(br[0]) {
				t.Fatalf("the compaction ran on %s (requests: test/a %d, test/b %d); want test/b's", ces[0].Model.Alias, len(ar), len(br))
			}
			if len(br[0].Tools) != 0 || len(br[0].Prompt) != 2 {
				t.Fatalf("test/b's compaction sent %d tools and %d messages; want the text form: none, and the system prompt and one message",
					len(br[0].Tools), len(br[0].Prompt))
			}
			if !startsFromSummary(br[1]) {
				t.Fatal("turn two did not start from the summary")
			}
			if len(warned) != 1 || !strings.Contains(warned[0], "no longer resolves") {
				t.Fatalf("warnings = %q; want one saying test/a no longer resolves", warned)
			}
		})
	}
}

// seedChildren makes s's runner open every child with a turn of its own
// already run, so its one turn from the parent has a conversation to
// compact: a new session has none, and a child's only automatic compactions
// otherwise come mid-turn (C11) or on an overflow (C12).
func seedChildren(t *testing.T, s *Session) {
	s.subs.seams.open = func(o Options) (*Session, error) {
		c, err := Open(o)
		if err != nil {
			return nil, err
		}
		if _, err := c.Run(context.Background(), "seed", nil); err != nil {
			_ = c.Close()
			t.Errorf("the child's seed turn: %v", err)
			return nil, err
		}
		return c, nil
	}
}

// TestAChildsCompactionIsInBothUsageFeeds (A22, §3.17, P19): a child compacts
// by the same rules on its own model's window, and what its summarizer was
// billed reaches the parent through both feeds of a child's usage — the
// SubagentFinished and the call's result (a foreground call's Result.Child,
// which its step's tool entry records per model; a background result's
// ChildUsage) — beside its steps', and its Compacted events reach the parent
// wrapped as the child's own.
func TestAChildsCompactionIsInBothUsageFeeds(t *testing.T) {
	// The child's seed step is not observed (the seam ran it); what it spent
	// in the call is its compaction's one attempt and its one step.
	spent := oneStep(2)
	row := tool.Usage{Input: spent.Input, Output: spent.Output, CacheRead: spent.CacheRead}
	childFeed := func(t *testing.T, evs []Event, id string) {
		t.Helper()
		var phases []string
		for _, se := range of[SubagentEvent](evs) {
			if c, ok := se.Event.(Compacted); ok && se.ID == id {
				phases = append(phases, c.Phase)
				if c.Phase == CompactionEnded && (c.Err != "" || c.Usage != oneStep(1)) {
					t.Fatalf("the child's Compacted{ended} = %+v; want a success with its summarizer's usage", c)
				}
			}
		}
		if !slices.Equal(phases, []string{CompactionStarted, CompactionEnded}) {
			t.Fatalf("the child's compaction events = %q; want started, ended", phases)
		}
	}

	t.Run("foreground", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		windowed(f, "test/b", 1000, 0)
		s := f.open(f.options())
		seedChildren(t, s)
		f.models["test/a"].push(callStep(agentPart(t, "a1", task("spend", "spend tokens", "model", "test/b"))), answerWith("ok"))
		f.models["test/b"].push(answerSpending("seeded", 2000), summaryReply("the seed."), answerWith("child done"))
		var ev events
		if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		if reqs := f.models["test/b"].requests(); len(reqs) != 3 || !isSummarizer(reqs[1]) {
			t.Fatalf("the child's requests = %d; want the seed, its compaction, and its turn", len(reqs))
		}
		fins := of[SubagentFinished](ev.list())
		if len(fins) != 1 || fins[0].Status != SubagentCompleted || fins[0].Usage != spent || fins[0].Steps != 1 {
			t.Fatalf("SubagentFinished = %+v; want completed, one step, and the compaction's usage with the step's: %+v", fins, spent)
		}
		childFeed(t, ev.list(), fins[0].ID)
		if res := callResult(t, ev.list(), "t1.1.1"); res.Child == nil || res.Child.Usage != row {
			t.Fatalf("the call's result carries %+v; want %+v", res.Child, row)
		}
		toolEntry := transcript(t, s).Entries[2]
		if len(toolEntry.SubagentUsage) != 1 || toolEntry.SubagentUsage[0].Usage != spent {
			t.Fatalf("the step's tool entry records %+v; want the child's row with its compaction: %+v", toolEntry.SubagentUsage, spent)
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
		windowed(b.fixture, "test/b", 1000, 0)
		seedChildren(t, b.s)
		b.routers["test/a"].route("go", callStep(bgPart(t, "a1", "job", "spend tokens", "model", "test/b")), answerWith("started"))
		b.models["test/b"].push(answerSpending("seeded", 2000), summaryReply("the seed."), answerWith("child done"))
		var ev events
		if res, err := b.s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		if !await(t, b.pending, "the child's result waiting to be delivered") {
			t.Fatal("OnPending's handler found nothing pending")
		}
		id := startedWith(t, ev.list(), "spend tokens").ID
		if fin := finishedOf(t, b.own.list(), id); fin.Status != SubagentCompleted || fin.Usage != spent {
			t.Fatalf("SubagentFinished = %+v; want completed with the compaction's usage and the step's: %+v", fin, spent)
		}
		childFeed(t, b.own.list(), id)
		if res := resultOf(t, b.s, id); res.usage == nil || res.usage.Usage != row {
			t.Fatalf("the background result's ChildUsage = %+v; want %+v", res.usage, row)
		}
	})
}

// TestASteerDuringThePreTurnCompactionGoesInTheFirstRequest (P36, R2-2): the
// steer box is open while a pre-turn compaction runs, and what it accepts
// then is taken up by the turn's first request — after the prompt — and
// written with its step, never left unanswered because the model answered in
// one step.
func TestASteerDuringThePreTurnCompactionGoesInTheFirstRequest(t *testing.T) {
	f := newFixture(t, "http://unused")
	windowed(f, "test/a", 100000, 0)
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerSpending("one", 90000))
	run(t, s, "turn one")

	g := newGate()
	a.push(g.hold(nil, cat(textParts(longSummary("1. Request and intent\nturn one.")), finish(fantasy.FinishReasonStop))),
		answerWith("two"))
	var ev events
	out := start(context.Background(), s, "turn two", ev.sink)
	await(t, g.reached, "the summarizer's request")
	if err := sendSteer(s, "and this"); err != nil {
		t.Fatalf("a steer during the pre-turn compaction was refused: %v", err)
	}
	close(g.release)
	o := await(t, out, "turn two")
	if o.err != nil || o.res.StopReason != StopEndTurn || len(o.res.Unanswered) != 0 {
		t.Fatalf("turn two = %+v, %v; want end_turn with the steer answered", o.res, o.err)
	}
	reqs := a.requests()
	if got := promptOf(reqs[len(reqs)-1]); !slices.Equal(got[len(got)-2:], []string{"user: turn two", "user: and this"}) {
		t.Fatalf("turn two's request = %q; want the prompt, then the steer", got)
	}
	if st := of[Steered](ev.list()); len(st) != 1 || st[0].Text != "and this" {
		t.Fatalf("Steered = %+v; want the one steer", st)
	}
	tr := entries(transcript(t, s))
	if want := []string{"user test/a high: turn two", "user test/a high: and this", "assistant test/a high end_turn: two"}; !slices.Equal(tr[len(tr)-3:], want) {
		t.Fatalf("transcript = %q; want the steer written after the prompt, with the step", tr)
	}
}

// TestAPreTurnCompactionThatStopsEndsTheTurn (P5, §3.8 item 8): a pre-turn
// compaction whose entry cannot be written fails the turn before its first
// request, and one that is cancelled ends it cancelled; either way a steer
// accepted meanwhile comes back unanswered, and no request follows.
func TestAPreTurnCompactionThatStopsEndsTheTurn(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "a save failure"
		if cancelled {
			name = "a cancel"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "http://unused")
			windowed(f, "test/a", 100000, 0)
			opts := f.options()
			opts.storeOpenFile = func(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
				file, err := os.OpenFile(name, flag, perm)
				if err != nil {
					return nil, err
				}
				// 1st write: turn one's step; 2nd: the compaction entry.
				return &failWriter{f: file, failAt: 2}, nil
			}
			s := f.open(opts)
			a := f.models["test/a"]
			a.push(answerSpending("one", 90000))
			run(t, s, "turn one")

			g := newGate()
			a.push(g.hold(nil, cat(textParts(longSummary("1. Request and intent\nturn one.")), finish(fantasy.FinishReasonStop))))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ev events
			out := start(ctx, s, "turn two", ev.sink)
			await(t, g.reached, "the summarizer's request")
			if err := sendSteer(s, "and this"); err != nil {
				t.Fatal(err)
			}
			if cancelled {
				cancel()
			} else {
				close(g.release)
			}
			o := await(t, out, "turn two")
			if !slices.Equal(o.res.Unanswered, []string{"and this"}) {
				t.Fatalf("Unanswered = %q; want the steer back", o.res.Unanswered)
			}
			if cancelled {
				if o.err != nil || o.res.StopReason != StopCancelled {
					t.Fatalf("turn two = %+v, %v; want cancelled", o.res, o.err)
				}
			} else if o.err == nil || !strings.Contains(o.err.Error(), "saving the compaction") || !errors.As(o.err, new(*errCompactionSaveFailed)) {
				t.Fatalf("turn two's error = %v; want the compaction's save failure", o.err)
			}
			if reqs := a.requests(); len(reqs) != 2 {
				t.Fatalf("%d requests; want turn one's and the summarizer's, and no more", len(reqs))
			}
			if len(of[StepDone](ev.list())) != 0 {
				t.Fatal("turn two reported a step")
			}
		})
	}
}

// TestTheTailBudgetIsTheConfiguredOne: a compaction keeps the newest steps
// that fit [compaction] tail_tokens, and none when the newest alone does not.
func TestTheTailBudgetIsTheConfiguredOne(t *testing.T) {
	for _, fits := range []bool{true, false} {
		t.Run(map[bool]string{true: "the newest step fits", false: "the newest step does not fit"}[fits], func(t *testing.T) {
			testTailBudget(t, fits)
		})
	}
}

func testTailBudget(t *testing.T, fits bool) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("one"))
	run(t, s, "turn one")
	a.push(answerWith("two"))
	run(t, s, "turn two")
	steps := s.store.Steps(s.cur.id())
	size := 0
	for _, m := range steps[len(steps)-1].Messages {
		size += int(messageTokens(m))
	}
	if !fits {
		size--
	}
	f.table.Compaction.TailTokensSet = &size
	a.push(summaryReply("both turns."))
	if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
		t.Fatal(err)
	}
	want := ""
	if fits {
		want = steps[len(steps)-1].First
	}
	if got := compactionEntries(t, s)[0].Compaction.FirstKeptID; got != want {
		t.Fatalf("tail_tokens %d (fits %v): the tail starts at %q, want %q", size, fits, got, want)
	}
}
