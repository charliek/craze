package harness

import (
	"context"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// Long-context price tiers in the spend (plan 047 D10, §7 A10): each request
// classified on its own — an assistant entry when it is priced, a child's
// steps and a compaction's attempts as they arrive — and every usage that
// sums requests carrying its long-context subset, priced at the tier's rates
// beside the rest at base, each token once. Haiku 5.5's shipped price is the
// tier every case here runs on.

// haikuBase and haikuLong are Haiku 5.5's rates, spelled out in picodollars
// per token (LC-1): the base tier for a prompt of 100,000 tokens or fewer,
// and the long one above. haikuCost checks the catalog says the same.
var (
	haikuBase = modeltable.TierRates{Input: 100_000, Output: 500_000, CacheRead: 10_000, CacheWrite: 125_000}
	haikuLong = modeltable.TierRates{Input: 500_000, Output: 2_500_000, CacheRead: 50_000, CacheWrite: 625_000}
)

// haikuCost is Haiku 5.5's shipped cost, tier and all: a copy of the
// catalog's own, after checking it prices at haikuBase and haikuLong above
// 100,000 tokens.
func haikuCost(t *testing.T) *modeltable.Cost {
	t.Helper()
	cat, err := modeltable.ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	c := cat.Models["anthropic/claude-haiku-5-5"].Cost
	tbl := &modeltable.Table{Models: map[string]modeltable.Model{"h": {Provider: "p", WireModel: "w", Cost: c}}}
	r, ok := tbl.Price("p", "w")
	if !ok || r.Base() != haikuBase || r.Long() != haikuLong || r.LongContextAbove != 100_000 {
		t.Fatalf("premise: Haiku 5.5 prices at %+v, %v", r, ok)
	}
	return c.Clone()
}

// withHaiku prices alias in f's table as Haiku 5.5, before a session opens.
func withHaiku(t *testing.T, f *fixture, alias string) {
	t.Helper()
	m := f.table.Models[alias]
	m.Cost = haikuCost(t)
	f.table.Models[alias] = m
}

// pico is u at r, in picodollars: the rule in the test's own terms.
func pico(u Usage, r modeltable.TierRates) int64 {
	return u.Input*r.Input + u.Output*r.Output + u.CacheRead*r.CacheRead + u.CacheCreation*r.CacheWrite
}

// fantasyUsage is u as a provider's finish part reports it.
func fantasyUsage(u Usage) fantasy.Usage {
	return fantasy.Usage{InputTokens: u.Input, OutputTokens: u.Output, ReasoningTokens: u.Reasoning,
		CacheReadTokens: u.CacheRead, CacheCreationTokens: u.CacheCreation, TotalTokens: u.Input + u.Output}
}

// finishBilled ends a step with reason, billed u.
func finishBilled(reason fantasy.FinishReason, u Usage) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: reason, Usage: fantasyUsage(u)}}
}

// answerUsing is answerWith billed u.
func answerUsing(text string, u Usage) step {
	return reply(textParts(text), finishBilled(fantasy.FinishReasonStop, u))
}

// callUsing is callStep billed u.
func callUsing(u Usage, calls ...[]fantasy.StreamPart) step {
	return reply(cat(calls...), finishBilled(fantasy.FinishReasonToolCalls, u))
}

// summaryUsing is summaryReply billed u.
func summaryUsing(what string, u Usage) step {
	return answerUsing(longSummary("1. Request and intent\n"+what), u)
}

// rowOn is the entry on s's transcript that carries subagent_usage rows; the
// test fails unless exactly one does.
func rowOn(t *testing.T, s *Session) []ModelUsage {
	t.Helper()
	var rows []ModelUsage
	n := 0
	for _, e := range transcript(t, s).Entries {
		if len(e.SubagentUsage) > 0 {
			rows, n = e.SubagentUsage, n+1
		}
	}
	if n != 1 {
		t.Fatalf("%d entries carry subagent_usage rows; want one", n)
	}
	return rows
}

// TestHaikuLongContextBoundary (A10): a Haiku 5.5 step whose prompt — input,
// cache reads and cache writes — is exactly 100,000 tokens is priced at the
// base rates, and one of 100,001 at the long rates, every token of it, the
// one token over the threshold a cache write's or a cache read's. Its tokens
// are counted once. The costs are spelled out. Negative controls: a
// threshold the prompt must reach (>=) fails the first case; a prompt that
// leaves out cache writes, or cache reads, fails the second, or the third.
func TestHaikuLongContextBoundary(t *testing.T) {
	f := newFixture(t, "http://unused")
	withHaiku(t, f, "test/a")
	s := f.open(f.options())
	var total int64
	for _, tc := range []struct {
		name string
		u    Usage
		want int64 // p$
	}{
		// 40,000×100,000 + 1,000×500,000 + 50,000×10,000 + 10,000×125,000
		{"exactly 100,000", Usage{Input: 40_000, Output: 1_000, CacheRead: 50_000, CacheCreation: 10_000}, 6_250_000_000},
		// 40,000×500,000 + 1,000×2,500,000 + 50,000×50,000 + 10,001×625,000
		{"100,001 by a cache write", Usage{Input: 40_000, Output: 1_000, CacheRead: 50_000, CacheCreation: 10_001}, 31_250_625_000},
		// 40,000×500,000 + 1,000×2,500,000 + 60,001×50,000
		{"100,001 by a cache read", Usage{Input: 40_000, Output: 1_000, CacheRead: 60_001}, 25_500_050_000},
	} {
		f.models["test/a"].push(answerUsing("ok", tc.u))
		var ev events
		if _, err := s.Run(context.Background(), tc.name, ev.sink); err != nil {
			t.Fatal(err)
		}
		got := lastSpent(t, ev.list()).Turn
		want := Spend{Input: tc.u.Input, Output: tc.u.Output, CacheRead: tc.u.CacheRead, CacheCreation: tc.u.CacheCreation, CostPicoUSD: tc.want}
		if got != want {
			t.Errorf("%s: the turn's spend = %+v, want %+v", tc.name, got, want)
		}
		total += tc.want
	}
	if _, session, _ := s.spend(1); session.CostPicoUSD != total {
		t.Fatalf("the session's cost = %d, want %d", session.CostPicoUSD, total)
	}
}

// TestLongContextSubagentRows (A10): a child's requests are classified one by
// one as it runs — a long one and a short one in one child — and its row
// carries the long one's usage as its subset, persisted, so the parent's spend
// prices that part long and the rest base; two children of one step on one
// model merge into one row whose subset is the sum of theirs — a long child
// and one of exactly 100,000 tokens, which stays base. The parent's own model
// is unpriced, so the cost is the rows' alone. Negative controls: an observer
// that does not classify a child's steps leaves the subset empty (the first
// case's row); a merge that does not add subsets loses the long child's (the
// second's).
func TestLongContextSubagentRows(t *testing.T) {
	open := func(t *testing.T) *routed {
		t.Helper()
		f := newRouted(t)
		withHaiku(t, f.fixture, "test/b")
		return f
	}

	t.Run("one child, a long request and a short one", func(t *testing.T) {
		f := open(t)
		s := f.open(f.options())
		long, short := Usage{Input: 120_000, Output: 200}, Usage{Input: 2_000, Output: 100}
		f.routers["test/a"].route("go", callStep(agentPart(t, "a1", task("look", "look around", "model", "test/b"))), answerWith("done"))
		f.routers["test/b"].route("look around", callUsing(long, globPart("g1")), answerUsing("child done", short))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
			t.Fatal(err)
		}
		want := []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: addUsage(long, short), LongContext: long}}
		if rows := rowOn(t, s); !reflect.DeepEqual(rows, want) {
			t.Fatalf("the row = %+v, want %+v", rows, want)
		}
		// 120,000×500,000 + 200×2,500,000 + 2,000×100,000 + 100×500,000
		if got := lastSpent(t, ev.list()).Session.CostPicoUSD; got != 60_750_000_000 || got != pico(long, haikuLong)+pico(short, haikuBase) {
			t.Fatalf("the session's cost = %d, want 60,750,000,000: the long request at the long rates, the short at base", got)
		}
	})

	t.Run("two children merged into one row", func(t *testing.T) {
		f := open(t)
		s := f.open(f.options())
		long, exact := Usage{Input: 150_000, Output: 300}, Usage{Input: 99_000, CacheRead: 1_000, Output: 50}
		f.routers["test/a"].route("go", callStep(
			agentPart(t, "a1", task("one", "job one", "model", "test/b")),
			agentPart(t, "a2", task("two", "job two", "model", "test/b"))), answerWith("done"))
		f.routers["test/b"].route("job one", answerUsing("one", long))
		f.routers["test/b"].route("job two", answerUsing("two", exact))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
			t.Fatal(err)
		}
		want := []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: addUsage(long, exact), LongContext: long}}
		if rows := rowOn(t, s); !reflect.DeepEqual(rows, want) {
			t.Fatalf("the row = %+v, want %+v", rows, want)
		}
		// 150,000×500,000 + 300×2,500,000 + 99,000×100,000 + 1,000×10,000 + 50×500,000
		if got := lastSpent(t, ev.list()).Session.CostPicoUSD; got != 85_685_000_000 || got != pico(long, haikuLong)+pico(exact, haikuBase) {
			t.Fatalf("the session's cost = %d, want 85,685,000,000", got)
		}
	})
}

// TestLongContextBackgroundMixtures (A10): a background child's usage is
// classified by the price it carries — the table its parent held when the
// call started it — and delivered with its subset after the parent has
// reloaded a table that prices its model another way (here a tier above
// 50,000 tokens at 2 and 8): by a wake, the row its part alone, priced at the
// opened table's long rates; and read by agent_output beside a foreground
// child on the same model, run under the new table, the two merged into one
// row whose subset is both, each part at its own table's long rates. The
// parent's own model is unpriced. Negative control: a commit that does not
// carry the background usage's subset leaves a rest whose subset is not
// inside it, so the whole row is priced at the new table.
func TestLongContextBackgroundMixtures(t *testing.T) {
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	reloaded := models + `
[models."test/b".cost]
input = 1.0
output = 4.0
long_context_above = 50000

[models."test/b".cost.long_context]
input = 2.0
output = 8.0
`
	newLong := modeltable.TierRates{Input: 2_000_000, Output: 8_000_000}
	bgUsage, fgUsage := Usage{Input: 110_000, Output: 100}, Usage{Input: 60_000, Output: 100}
	open := func(t *testing.T) *bg {
		t.Helper()
		return openBG(t, func(o *Options) {
			m := o.Table.Models["test/b"]
			m.Cost = haikuCost(t)
			o.Table.Models["test/b"] = m
		})
	}
	// spawnOnB starts a background child on test/b, held mid-step, and has
	// the idle parent take the reloaded table.
	spawnOnB := func(t *testing.T, b *bg) (*worker, string) {
		t.Helper()
		w := newWorker()
		b.routers["test/b"].route("child work", w.step(openText("did child work"),
			cat([]fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextEnd, ID: "0"}}, finishBilled(fantasy.FinishReasonStop, bgUsage))))
		b.routers["test/a"].route("go", callStep(bgPart(t, "a1", "job", "child work", "model", "test/b")), answerWith("started"))
		var ev events
		if res, err := b.s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("the spawning turn = %+v, %v", res, err)
		}
		st := startedWith(t, ev.list(), "child work")
		await(t, w.reached, "the child mid-step")
		if err := b.s.SetTable(tableFrom(t, providers, reloaded), nil); err != nil {
			t.Fatalf("SetTable while the child runs: %v", err)
		}
		return w, st.ID
	}

	t.Run("delivered by a wake after a reload", func(t *testing.T) {
		b := open(t)
		w, id := spawnOnB(t, b)
		b.finish(t, w)
		b.routers["test/a"].route("go", answerWith("noted"))
		if res, err := b.s.Wake(context.Background(), nil); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Wake = %+v, %v", res, err)
		}
		tr := transcript(t, b.s)
		e := tr.Entries[entryIndex(tr, resultOf(t, b.s, id).entry)]
		want := []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: bgUsage, LongContext: bgUsage}}
		if !reflect.DeepEqual(e.SubagentUsage, want) {
			t.Fatalf("the wake's entry carries %+v; want %+v", e.SubagentUsage, want)
		}
		_, session, _ := b.s.spend(2)
		// 110,000×500,000 + 100×2,500,000, at the table the child opened under.
		if session.CostPicoUSD != 55_250_000_000 || session.CostPicoUSD != pico(bgUsage, haikuLong) {
			t.Fatalf("the session cost %d; want 55,250,000,000, the child's long request at its own table's long rates", session.CostPicoUSD)
		}
	})

	t.Run("read by agent_output beside a foreground child after a reload", func(t *testing.T) {
		b := open(t)
		w, id := spawnOnB(t, b)
		atStep, ready := make(chan struct{}), make(chan struct{})
		b.routers["test/b"].route("fg work", answerUsing("fg done", fgUsage))
		b.routers["test/a"].route("go",
			func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				close(atStep)
				<-ready
				callStep(outputPart(t, "o1", id, 0), agentPart(t, "f1", task("fg", "fg work", "model", "test/b")))(ctx, yield)
			},
			answerWith("done"))
		out := start(context.Background(), b.s, "next", nil)
		await(t, atStep, "the step after its boundary")
		b.finish(t, w)
		close(ready)
		if got := await(t, out, "the turn"); got.err != nil {
			t.Fatal(got.err)
		}
		tr := transcript(t, b.s)
		e := tr.Entries[entryIndex(tr, resultOf(t, b.s, id).entry)]
		both := addUsage(bgUsage, fgUsage)
		want := []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: both, LongContext: both}}
		if e.Message.Role != fantasy.MessageRoleTool || !reflect.DeepEqual(e.SubagentUsage, want) {
			t.Fatalf("the result's entry (%s) carries %+v; want the tool entry, %+v", e.Message.Role, e.SubagentUsage, want)
		}
		_, session, _ := b.s.spend(2)
		// The background part at the opened table's long rates; the
		// foreground child's 60,000 tokens, base there, long under the new
		// table it ran under: 60,000×2,000,000 + 100×8,000,000.
		if want := pico(bgUsage, haikuLong) + pico(fgUsage, newLong); session.CostPicoUSD != want || want != 55_250_000_000+120_800_000_000 {
			t.Fatalf("the session cost %d; want %d", session.CostPicoUSD, want)
		}
	})
}

// TestLongContextCompactions (A10): each summarizer attempt is one request,
// classified on its own before the attempts are summed, and the compaction
// entry carries the long ones' sum: two attempts below the threshold whose
// sum is above it stay base; a long degenerate attempt and a base success are
// split; a failed compaction's three attempts, two long and one short, too.
// The entry and Compacted{ended} carry the same subset. Negative control: a
// compaction that classifies the running sum instead of each attempt fails
// the first case.
func TestLongContextCompactions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts []step
		ok       bool
		usage    Usage
		long     Usage
		want     int64 // p$
	}{
		{"two short attempts whose sum is long",
			[]step{answerUsing(shortSummary, Usage{Input: 60_000, Output: 100}), summaryUsing("turn one.", Usage{Input: 60_000, Output: 900})},
			true, Usage{Input: 120_000, Output: 1_000}, Usage{},
			// 120,000×100,000 + 1,000×500,000
			12_500_000_000},
		{"a long attempt and a short one",
			[]step{answerUsing(shortSummary, Usage{Input: 100_001, Output: 100}), summaryUsing("turn one.", Usage{Input: 90_000, CacheCreation: 10_000, Output: 900})},
			true, Usage{Input: 190_001, CacheCreation: 10_000, Output: 1_000}, Usage{Input: 100_001, Output: 100},
			// 100,001×500,000 + 100×2,500,000 + 90,000×100,000 + 10,000×125,000 + 900×500,000
			60_950_500_000},
		{"a failed compaction",
			[]step{answerUsing(shortSummary, Usage{Input: 200_000, Output: 10}), answerUsing(shortSummary, Usage{Input: 5_000, Output: 10}),
				answerUsing(shortSummary, Usage{Input: 100_001, Output: 10})},
			false, Usage{Input: 305_001, Output: 30}, Usage{Input: 300_001, Output: 20},
			// 300,001×500,000 + 20×2,500,000 + 5,000×100,000 + 10×500,000
			150_555_500_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://unused")
			withHaiku(t, f, "test/a")
			s := f.open(f.options())
			s.sleep = func(context.Context, time.Duration) {}
			f.models["test/a"].push(answerWith("hello"))
			if _, err := s.Run(context.Background(), "hi", nil); err != nil {
				t.Fatal(err)
			}
			f.models["test/a"].push(tc.attempts...)
			var ev events
			if _, err := s.Compact(context.Background(), "", "/compact", ev.sink); (err == nil) != tc.ok {
				t.Fatalf("/compact = %v; want success %v", err, tc.ok)
			}
			entries := compactionEntries(t, s)
			e := entries[len(entries)-1]
			if *e.Usage != tc.usage || e.Compaction.LongContext != tc.long {
				t.Fatalf("the compaction entry's usage %+v, subset %+v; want %+v, %+v", *e.Usage, e.Compaction.LongContext, tc.usage, tc.long)
			}
			var ended []Compacted
			for _, c := range of[Compacted](ev.list()) {
				if c.Phase == CompactionEnded {
					ended = append(ended, c)
				}
			}
			if len(ended) != 1 || ended[0].Usage != tc.usage || ended[0].LongContext != tc.long {
				t.Fatalf("Compacted{ended} = %+v; want one with the entry's usage and subset", ended)
			}
			if got := lastSpent(t, ev.list()).Turn.CostPicoUSD; got != tc.want || got != pico(subUsage(tc.usage, tc.long), haikuBase)+pico(tc.long, haikuLong) {
				t.Fatalf("the compaction's cost = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLongContextUnsavedUsage (A10): usage no entry holds keeps its tier: a
// step whose append failed, classified as it is noted; the row of a child
// whose parent's step was not written, with its subset; a compaction whose
// entry could not be written, with its attempts' subset; and a side
// question's request. Negative control: noting a step without classifying it
// prices the first case at base.
func TestLongContextUnsavedUsage(t *testing.T) {
	long, short := Usage{Input: 100_001, Output: 10}, Usage{Input: 1_000, Output: 10}
	longCost, shortCost := pico(long, haikuLong), pico(short, haikuBase) // 50,025,500,000 and 105,000

	t.Run("a step whose append failed", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		withHaiku(t, f, "test/a")
		opts := f.options()
		opts.storeOpenFile = (&failingWrites{fail: map[int]bool{1: true}}).open
		s := f.open(opts)
		f.models["test/a"].push(answerUsing("lost", long))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err == nil {
			t.Fatal("the step was saved despite the injected failure")
		}
		if d := of[StepDone](ev.list()); len(d) != 1 || d[0].Saved {
			t.Fatalf("StepDone = %+v; want one, unsaved", d)
		}
		if got := lastSpent(t, ev.list()).Session.CostPicoUSD; got != longCost {
			t.Fatalf("the session's cost = %d, want %d: the unsaved step at the long rates", got, longCost)
		}
	})

	t.Run("a child's row on a step whose append failed", func(t *testing.T) {
		f := newRouted(t)
		withHaiku(t, f.fixture, "test/b")
		opts := f.options()
		opts.storeOpenFile = (&failingWrites{fail: map[int]bool{1: true}}).open
		s := f.open(opts)
		f.routers["test/a"].route("go", callStep(agentPart(t, "a1", task("look", "look around", "model", "test/b"))))
		f.routers["test/b"].route("look around", callUsing(long, globPart("g1")), answerUsing("child done", short))
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err == nil {
			t.Fatal("the step was saved despite the injected failure")
		}
		d := of[StepDone](ev.list())
		want := []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: addUsage(long, short), LongContext: long}}
		if len(d) != 1 || d[0].Saved || !reflect.DeepEqual(d[0].SubagentUsage, want) {
			t.Fatalf("StepDone = %+v; want one, unsaved, carrying %+v", d, want)
		}
		if got := lastSpent(t, ev.list()).Session.CostPicoUSD; got != longCost+shortCost {
			t.Fatalf("the session's cost = %d, want %d", got, longCost+shortCost)
		}
	})

	t.Run("a compaction whose entry could not be written", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		withHaiku(t, f, "test/a")
		opts := f.options()
		opts.storeOpenFile = (&failingWrites{fail: map[int]bool{2: true}}).open // turn 1 is write 1
		s := f.open(opts)
		s.sleep = func(context.Context, time.Duration) {}
		f.models["test/a"].push(answerUsing("hello", short))
		if _, err := s.Run(context.Background(), "hi", nil); err != nil {
			t.Fatal(err)
		}
		f.models["test/a"].push(answerUsing(shortSummary, long), summaryUsing("turn one.", short))
		var ev events
		if _, err := s.Compact(context.Background(), "", "/compact", ev.sink); !errors.As(err, new(*errCompactionSaveFailed)) {
			t.Fatalf("/compact = %v; want a save failure", err)
		}
		if got := lastSpent(t, ev.list()).Turn.CostPicoUSD; got != longCost+shortCost {
			t.Fatalf("the compaction's cost = %d, want %d", got, longCost+shortCost)
		}
	})

	t.Run("a side question", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		withHaiku(t, f, "test/a")
		s := f.open(f.options())
		f.models["test/a"].push(answerUsing("hello", short))
		if _, err := s.Run(context.Background(), "hi", nil); err != nil {
			t.Fatal(err)
		}
		f.models["test/a"].push(answerUsing("an aside", long))
		if out := ask(s, nil, "q"); out.err != nil {
			t.Fatal(out.err)
		}
		sp, ok := s.IdleSpent()
		if !ok || sp.Session.CostPicoUSD != shortCost+longCost {
			t.Fatalf("IdleSpent = %+v, %v; want the session at %d", sp, ok, shortCost+longCost)
		}
	})
}

// TestALeftOutTierRateIsPricedAtBase (plan 047 D10): a tier written with
// input and output alone prices a long request's input and output at its
// rates and its cache reads and writes at the model's base rates — never at
// 0, which would under-report the spend. Negative control: a tier rate left
// out priced at 0 drops the cache reads' and writes' cost.
func TestALeftOutTierRateIsPricedAtBase(t *testing.T) {
	f := newFixture(t, "http://unused")
	in, out, read, write, tierIn, tierOut := 1.0, 2.0, 0.5, 4.0, 10.0, 20.0
	m := f.table.Models["test/a"]
	m.Cost = &modeltable.Cost{Input: &in, Output: &out, CacheRead: &read, CacheWrite: &write,
		LongContextAbove: new(int64(1000)), LongContext: &modeltable.LongContextCost{Input: &tierIn, Output: &tierOut}}
	f.table.Models["test/a"] = m
	s := f.open(f.options())
	u := Usage{Input: 2_000, Output: 100, CacheRead: 3_000, CacheCreation: 500} // a prompt of 5,500: long
	f.models["test/a"].push(answerUsing("ok", u))
	var ev events
	if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	// 2,000×10,000,000 + 100×20,000,000 at the tier's rates, and
	// 3,000×500,000 + 500×4,000,000 at the base cache rates.
	if got := lastSpent(t, ev.list()).Turn.CostPicoUSD; got != 25_500_000_000 {
		t.Fatalf("the long request cost %d, want 25,500,000,000: its cache reads and writes at the base rates", got)
	}
}

// TestChildObserverSumsLongContext: a child's observer classifies each step
// — one request — by the price it was handed, and adds each compaction's
// subset as the child classified it, beside the totals. Negative control: an
// observer that drops a Compacted's subset fails.
func TestChildObserverSumsLongContext(t *testing.T) {
	f := newFixture(t, "http://unused")
	withHaiku(t, f, "test/a")
	r, ok := f.table.Price("test", "wire-a")
	obs := childObserver{price: priced{rates: r, ok: ok}}
	long, short, comp := Usage{Input: 100_001}, Usage{Input: 100_000}, Usage{Input: 300_000, Output: 9}
	obs.observe(StepDone{Usage: long})
	obs.observe(StepDone{Usage: short})
	obs.observe(Compacted{Phase: CompactionEnded, Usage: comp, LongContext: Usage{Input: 200_000, Output: 3}})
	usage, gotLong, _, steps := obs.totals()
	if want := addUsage(addUsage(long, short), comp); usage != want || steps != 2 {
		t.Fatalf("totals = %+v over %d steps, want %+v over 2", usage, steps, want)
	}
	if want := (Usage{Input: 300_001, Output: 3}); gotLong != want {
		t.Fatalf("the subset = %+v, want %+v: the long step and the compaction's own subset", gotLong, want)
	}
}

// TestAChildClassifiesByItsParentsPrice: a child opened after its parent took
// a table that does not price the child's model classifies its requests by
// the price its parent's spend will charge — the parent's opened table's,
// tier and all — both the child's steps (its observer, the runner's) and its
// compactions' attempts (its own priceNow, through the fallback its parent
// hands it). Negative control: a child opened without the parent's fallback
// classifies nothing as long, which the priceNow check catches.
func TestAChildClassifiesByItsParentsPrice(t *testing.T) {
	f := newRouted(t)
	withHaiku(t, f.fixture, "test/b")
	s := f.open(f.options())
	kids := watchKids(s)
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	if err := s.SetTable(tableFrom(t, providers, models), nil); err != nil {
		t.Fatal(err)
	}
	long := Usage{Input: 100_001, Output: 10}
	f.routers["test/a"].route("go", callStep(agentPart(t, "a1", task("look", "look around", "model", "test/b"))), answerWith("done"))
	f.routers["test/b"].route("look around", answerUsing("child done", long))
	var ev events
	if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
		t.Fatal(err)
	}
	child := kids.get(of[SubagentStarted](ev.list())[0].ID)
	wireB := store.Model{Provider: "test", WireModel: "wire-b"}
	if p := child.priceNow(wireB); !p.ok || p.rates.LongContextAbove != 100_000 || p.rates.Long() != haikuLong {
		t.Fatalf("the child prices its model at %+v; want its parent's opened table's Haiku tier", p)
	}
	if rows := rowOn(t, s); rows[0].LongContext != long {
		t.Fatalf("the row's subset = %+v, want the child's long request", rows[0].LongContext)
	}
	if got := lastSpent(t, ev.list()).Session.CostPicoUSD; got != pico(long, haikuLong) {
		t.Fatalf("the session's cost = %d, want %d", got, pico(long, haikuLong))
	}
}

// longContextSession runs a session of every kind of usage that carries a
// tier on Haiku-priced models — a parent's long and short steps, a child's
// row with a long and a short request, a compaction of a long attempt and a
// short one, and a long step after it — and returns it with its last Spent,
// still open.
func longContextSession(t *testing.T, f *fixture, opts Options) (*Session, Spent) {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.sleep = func(context.Context, time.Duration) {}
	ctx := context.Background()
	var live events
	f.models["test/a"].push(callUsing(Usage{Input: 100_500, Output: 40}, agentPart(t, "a1", task("look", "look around", "model", "test/b"))),
		answerUsing("found", Usage{Input: 3_000, CacheRead: 97_000, Output: 20}))
	f.models["test/b"].push(callUsing(Usage{Input: 120_000, Output: 200}, globPart("g1")), answerUsing("child done", Usage{Input: 2_000, Output: 100}))
	if _, err := s.Run(ctx, "look", live.sink); err != nil {
		t.Fatal(err)
	}
	f.models["test/a"].push(answerUsing(shortSummary, Usage{Input: 104_000, Output: 30}), summaryUsing("the search.", Usage{Input: 4_000, Output: 900}))
	if _, err := s.Compact(ctx, "", "/compact", live.sink); err != nil {
		t.Fatal(err)
	}
	f.models["test/a"].push(answerUsing("last", Usage{Input: 1_000, CacheCreation: 99_001, Output: 5}))
	if _, err := s.Run(ctx, "last", live.sink); err != nil {
		t.Fatal(err)
	}
	spentAfterEach(t, live.list())
	return s, lastSpent(t, live.list())
}

// TestLongContextReplayAgrees (A10): a resumed session's Replay prices the
// subsets its transcript holds — a row's, a compaction's — and classifies its
// assistant entries with the table it loaded, the same table here, so its
// Spent is the live session's at close. The premise: the session's spend has
// long parts (its cost is not the all-base one). Negative control: a store
// that does not write the compaction's subset resumes at a lower cost.
func TestLongContextReplayAgrees(t *testing.T) {
	f := newFixture(t, "http://unused")
	withHaiku(t, f, "test/a")
	withHaiku(t, f, "test/b")
	s, final := longContextSession(t, f, f.options())
	var all Usage
	for _, u := range []Usage{{Input: 100_500, Output: 40}, {Input: 3_000, CacheRead: 97_000, Output: 20}, {Input: 120_000, Output: 200},
		{Input: 2_000, Output: 100}, {Input: 104_000, Output: 30}, {Input: 4_000, Output: 900}, {Input: 1_000, CacheCreation: 99_001, Output: 5}} {
		all = addUsage(all, u)
	}
	if final.Session.CostPicoUSD <= pico(all, haikuBase) {
		t.Fatalf("premise: the live session cost %d, no more than all of it at base, %d", final.Session.CostPicoUSD, pico(all, haikuBase))
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := resumed(t, resumeOptions(f.options(), id))
	var replay events
	if err := r.Replay(replay.sink); err != nil {
		t.Fatal(err)
	}
	got := lastSpent(t, replay.list())
	want := final
	want.Seq = got.Seq
	equal(t, "the resumed session's Spent", got, want)
	if diags := of[Diag](replay.list()); len(diags) != 0 {
		t.Fatalf("the replay reported %+v; want nothing distrusted", diags)
	}
}

// TestALongContextSubsetOutsideItsUsageIsPricedAtBase (A10): a transcript
// whose row subset is above the row's usage, and whose compaction subset has
// a field below 0, resumes with both usages priced at base, every token
// counted, and each reported once as DiagLongContextMalformed — by the first
// report that counts it: the Replay's, or, with no Replay, an idle report
// through the session's sink — and by no later one. Everything else on the
// path keeps its tier. Negative control: a spend that trusts any subset
// prices the row's too-large subset at the long rates.
func TestALongContextSubsetOutsideItsUsageIsPricedAtBase(t *testing.T) {
	f := newFixture(t, "http://unused")
	withHaiku(t, f, "test/a")
	withHaiku(t, f, "test/b")
	s, live := longContextSession(t, f, f.options())
	id, path := s.ID(), s.store.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	subset := regexp.MustCompile(`"long_context":\{[^}]*\}`)
	lines := strings.Split(string(b), "\n")
	var edited int
	for i, line := range lines {
		switch {
		case strings.Contains(line, `"subagent_usage"`):
			lines[i] = subset.ReplaceAllString(line, `"long_context":{"input":999999,"output":0,"reasoning":0,"cache_read":0,"cache_creation":0}`)
			edited++
		case strings.Contains(line, `"type":"compaction"`):
			lines[i] = subset.ReplaceAllString(line, `"long_context":{"input":-1,"output":0,"reasoning":0,"cache_read":0,"cache_creation":0}`)
			edited++
		}
	}
	if edited != 2 {
		t.Fatalf("premise: edited %d lines, want the row's and the compaction's", edited)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	// The live session priced the row's long request (120,000 + 200 out) and
	// the compaction's (104,000 + 30 out) at the long rates; the resumed one
	// prices both at base, the rest as before.
	rowLong, compLong := Usage{Input: 120_000, Output: 200}, Usage{Input: 104_000, Output: 30}
	want := live.Session
	want.CostPicoUSD -= pico(rowLong, haikuLong) - pico(rowLong, haikuBase) + pico(compLong, haikuLong) - pico(compLong, haikuBase)
	malformed := func(t *testing.T, evs []Event) []Diag {
		t.Helper()
		var out []Diag
		for _, d := range of[Diag](evs) {
			if d.Kind != DiagLongContextMalformed {
				t.Fatalf("an unexpected diag %+v", d)
			}
			out = append(out, d)
		}
		return out
	}

	// The idle case first: it writes nothing, and the replayed one runs a
	// turn that extends the transcript.
	t.Run("an idle report first", func(t *testing.T) {
		var own events
		opts := resumeOptions(f.options(), id)
		opts.Sink = own.sink
		r := resumed(t, opts)
		sp, ok := r.IdleSpent()
		if !ok || sp.Session != want {
			t.Fatalf("IdleSpent = %+v, %v; want the session at %+v", sp, ok, want)
		}
		if d := malformed(t, own.list()); len(d) != 2 {
			t.Fatalf("the session's sink took %+v; want the two diags", d)
		}
		if _, ok := r.IdleSpent(); !ok {
			t.Fatal("a second IdleSpent was refused")
		}
		if d := malformed(t, own.list()); len(d) != 2 {
			t.Fatalf("a second idle report said more: %+v", d)
		}
	})
	t.Run("replayed", func(t *testing.T) {
		r := resumed(t, resumeOptions(f.options(), id))
		var replay events
		if err := r.Replay(replay.sink); err != nil {
			t.Fatal(err)
		}
		evs := replay.list()
		if got := lastSpent(t, evs).Session; got != want {
			t.Fatalf("the resumed session's spend = %+v, want %+v", got, want)
		}
		diags := malformed(t, evs)
		if len(diags) != 2 || diags[0].Fields["usage_of"] != "subagent_usage" || diags[1].Fields["usage_of"] != "compaction" ||
			!strings.Contains(diags[0].Fields["long_context"], "input=999999") || !strings.Contains(diags[1].Fields["long_context"], "input=-1") {
			t.Fatalf("the replay reported %+v; want the row's and the compaction's, once each", diags)
		}
		if _, isSpent := evs[len(evs)-3].(Spent); !isSpent {
			t.Fatalf("the diags follow a %T; want them right after the Spent", evs[len(evs)-3])
		}
		// A later report counts both again, at base, and says nothing more.
		f.models["test/a"].push(answerUsing("again", Usage{Input: 10, Output: 1}))
		var next events
		if _, err := r.Run(context.Background(), "again", next.sink); err != nil {
			t.Fatal(err)
		}
		if d := malformed(t, next.list()); len(d) != 0 {
			t.Fatalf("a later report said %+v again", d)
		}
		if got, again := lastSpent(t, next.list()).Session.CostPicoUSD, want.CostPicoUSD+pico(Usage{Input: 10, Output: 1}, haikuBase); got != again {
			t.Fatalf("the session's cost after a turn = %d, want %d", got, again)
		}
	})

}
