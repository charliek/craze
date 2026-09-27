package harness

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// A session's spend (plan 028 §3.14, PD18; §7 A31): the accounting rule over
// the transcript's path — assistant usage, subagent_usage rows and every
// compaction entry's usage, each at its own model's price — plus what the
// incarnation observed that no entry holds, reported as Spent after every
// step, every compaction and a Replay, by a session that is not a sub-agent.

// unpricedSpend is u as a Spend on a model the table has no price for: its
// tokens, no cost, and Unpriced when it holds anything.
func unpricedSpend(u Usage) Spend {
	return Spend{Input: u.Input, Output: u.Output, Reasoning: u.Reasoning, CacheRead: u.CacheRead,
		CacheCreation: u.CacheCreation, Unpriced: u != (Usage{})}
}

// unpricedSpent is the Spent a session on the test table, which prices no
// model and knows no window, reports: the context's size, and the turn's and
// the session's usage.
func unpricedSpent(context int64, turn, session Usage) Spent {
	return Spent{ContextTokens: context, Turn: unpricedSpend(turn), Session: unpricedSpend(session)}
}

// rateA and rateB are what withPrices makes test/a and test/b cost, in
// picodollars per token: $1, $2, $0.50 and $4 per 1M tokens of input, output,
// cache reads and cache writes on test/a; $3, $15, $0.30 and $3.75 on test/b.
var (
	rateA = modeltable.Rates{Input: 1_000_000, Output: 2_000_000, CacheRead: 500_000, CacheWrite: 4_000_000}
	rateB = modeltable.Rates{Input: 3_000_000, Output: 15_000_000, CacheRead: 300_000, CacheWrite: 3_750_000}
)

// withCost gives alias a cost in f's table, in $ per 1M tokens, as models.toml
// would. It is called before a session opens on the table.
func withCost(f *fixture, alias string, input, output, cacheRead, cacheWrite float64) {
	m := f.table.Models[alias]
	m.Cost = &modeltable.Cost{Input: &input, Output: &output, CacheRead: &cacheRead, CacheWrite: &cacheWrite}
	f.table.Models[alias] = m
}

// withPrices prices test/a at rateA and test/b at rateB; other/c and nokey/d
// stay unpriced.
func withPrices(f *fixture) {
	withCost(f, "test/a", 1, 2, 0.5, 4)
	withCost(f, "test/b", 3, 15, 0.3, 3.75)
}

// scripted is every scripted finish's usage (stepUsage) in the stored shape.
var scriptedUsage = Usage{Input: 10, Output: 5, CacheRead: 4}

// at is u as a Spend priced at r, the rule in the test's own terms: input,
// output — reasoning inside it — and cache reads and writes, each at its rate.
func at(u Usage, r modeltable.Rates) Spend {
	return Spend{Input: u.Input, Output: u.Output, Reasoning: u.Reasoning, CacheRead: u.CacheRead,
		CacheCreation: u.CacheCreation,
		CostPicoUSD:   u.Input*r.Input + u.Output*r.Output + u.CacheRead*r.CacheRead + u.CacheCreation*r.CacheWrite}
}

// plus is sp summed: tokens and cost added, and Unpriced when any is.
func plus(sp ...Spend) Spend {
	var out Spend
	for _, s := range sp {
		out.Input += s.Input
		out.Output += s.Output
		out.Reasoning += s.Reasoning
		out.CacheRead += s.CacheRead
		out.CacheCreation += s.CacheCreation
		out.CostPicoUSD += s.CostPicoUSD
		out.Unpriced = out.Unpriced || s.Unpriced
	}
	return out
}

// times is sp, n times over.
func times(n int, sp Spend) Spend {
	all := make([]Spend, n)
	for i := range all {
		all[i] = sp
	}
	return plus(all...)
}

// lastSpent is the last Spent among evs.
func lastSpent(t *testing.T, evs []Event) Spent {
	t.Helper()
	spents := of[Spent](evs)
	if len(spents) == 0 {
		t.Fatalf("no Spent among %d events", len(evs))
	}
	return spents[len(spents)-1]
}

// spentAfterEach checks that in evs, a sink's events, every StepDone and every
// Compacted{ended} of the session's own is followed at once by a Spent, and
// that every Spent follows one of them. It returns how many Spents there were.
func spentAfterEach(t *testing.T, evs []Event) int {
	t.Helper()
	report := func(ev Event) bool {
		switch e := ev.(type) {
		case StepDone:
			return true
		case Compacted:
			return e.Phase == CompactionEnded
		}
		return false
	}
	n := 0
	for i, ev := range evs {
		if report(ev) {
			if i+1 == len(evs) {
				t.Fatalf("event %d, the last, is a %T with no Spent after it", i, ev)
			}
			if _, ok := evs[i+1].(Spent); !ok {
				t.Fatalf("event %d is a %T followed by a %T; want a Spent", i, ev, evs[i+1])
			}
		}
		if _, ok := ev.(Spent); ok {
			n++
			if i == 0 || !report(evs[i-1]) {
				t.Fatalf("event %d is a Spent that follows no step and no compaction", i)
			}
		}
	}
	return n
}

// failingWrites is a session file for store.Options.OpenFile whose writes
// numbered in fail (from 1: the first is the file's creation, each append one
// more) fail having written nothing, which fails that append alone and leaves
// the store usable (the store's zero-byte rule).
type failingWrites struct {
	mu   sync.Mutex
	f    *os.File
	n    int
	fail map[int]bool
}

func (w *failingWrites) open(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	w.f = f
	return w, nil
}

func (w *failingWrites) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n++
	if w.fail[w.n] {
		return 0, errors.New("injected write failure")
	}
	return w.f.Write(p)
}

func (w *failingWrites) Close() error { return w.f.Close() }

// childTranscript is the transcript of the child session id, read from the
// harness home f's sessions are filed under.
func childTranscript(t *testing.T, f *fixture, id string) *store.Transcript {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.home, "sessions", "*", "*_"+id+".jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("the child %s's transcript: %v, %v", id, paths, err)
	}
	tr, err := store.Load(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// TestSessionSpendFollowsTheRule (A31): a session's spend is its assistant
// entries' usage, the subagent_usage rows its entries carry and every
// compaction entry's usage — a success's and a failure's, all its attempts —
// each priced by its own model; plus the step whose append failed and the
// compaction whose entry could not be written, which no entry holds. A
// child's own transcript is never added: it is the row, once. Usage with no
// price counts its tokens and marks the spend Unpriced. Every step and every
// compaction is followed by the Spent it leaves, whose turn part is that
// turn's alone.
func TestSessionSpendFollowsTheRule(t *testing.T) {
	f := newFixture(t, "http://unused")
	withPrices(f)
	opts := f.options()
	// Write 5 is turn 4's step and write 6 turn 5's compaction entry (below).
	w := &failingWrites{fail: map[int]bool{5: true, 6: true}}
	opts.storeOpenFile = w.open
	s := f.open(opts)
	s.sleep = func(context.Context, time.Duration) {}
	ctx := context.Background()
	a, b := at(scriptedUsage, rateA), at(scriptedUsage, rateB)
	check := func(what string, evs []Event, turn, session Spend) {
		t.Helper()
		spentAfterEach(t, evs)
		got := lastSpent(t, evs)
		equal(t, what+": the turn's spend", got.Turn, turn)
		equal(t, what+": the session's spend", got.Session, session)
	}

	// Turn 1 (writes 1, 2): an agent call whose child runs on test/b, then the
	// answer — two steps on test/a, and the child's step as the row its tool
	// entry carries, at test/b's price.
	f.models["test/a"].push(callStep(agentPart(t, "a1", task("look", "look around", "model", "test/b"))), answerWith("ok"))
	f.models["test/b"].push(answerWith("child done"))
	var one events
	if res, err := s.Run(ctx, "go", one.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("turn 1 = %+v, %v", res, err)
	}
	session := plus(a, a, b)
	check("turn 1", one.list(), session, session)
	if n := spentAfterEach(t, one.list()); n != 2 {
		t.Fatalf("turn 1 reported %d Spents; want one per step, 2", n)
	}
	rows := transcript(t, s).Entries[2].SubagentUsage
	if len(rows) != 1 || rows[0].Model != "test/b" || rows[0].Usage != scriptedUsage {
		t.Fatalf("the step's tool entry carries %+v; want the child's row on test/b", rows)
	}
	// The child's own transcript holds that usage too, on its own assistant
	// entry: it is never added in again.
	var childSpent Usage
	for _, e := range childTranscript(t, f, of[SubagentStarted](one.list())[0].ID).Entries {
		if e.Usage != nil {
			childSpent = addUsage(childSpent, *e.Usage)
		}
	}
	if childSpent != scriptedUsage {
		t.Fatalf("the child's transcript records %+v; want its step's %+v", childSpent, scriptedUsage)
	}

	// Turn 2 (write 3): /compact on test/b — the compaction entry's usage at
	// the price of the model its summarizer ran on.
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	f.models["test/b"].push(summaryReply("turn one."))
	var two events
	if _, err := s.Compact(ctx, "", "/compact", two.sink); err != nil {
		t.Fatalf("turn 2's /compact: %v", err)
	}
	session = plus(session, b)
	check("the manual compaction", two.list(), b, session)

	// Turn 3 (write 4): a /compact on test/a whose three attempts are all too
	// short — the failure entry's usage, every attempt's.
	if err := s.SetModel("test/a"); err != nil {
		t.Fatal(err)
	}
	f.models["test/a"].push(answerWith(shortSummary), answerWith(shortSummary), answerWith(shortSummary))
	var three events
	if _, err := s.Compact(ctx, "", "/compact", three.sink); err == nil {
		t.Fatal("turn 3's /compact succeeded on three degenerate summaries")
	}
	session = plus(session, times(3, a))
	check("the failed compaction", three.list(), times(3, a), session)

	// Turn 4 (write 5, failing): a step whose append fails — billed, in no
	// entry, and spent all the same.
	f.models["test/a"].push(answerWith("lost"))
	var four events
	if _, err := s.Run(ctx, "unsaved", four.sink); err == nil {
		t.Fatal("turn 4 saved its step despite the injected failure")
	}
	if d := of[StepDone](four.list()); len(d) != 1 || d[0].Saved || d[0].SaveError == "" {
		t.Fatalf("turn 4's StepDone = %+v; want one whose save failed", d)
	}
	session = plus(session, a)
	check("the unsaved step", four.list(), a, session)

	// Turn 5 (write 6, failing): a /compact whose entry cannot be written —
	// its attempt's usage, in no entry, spent all the same.
	f.models["test/a"].push(summaryReply("again."))
	var five events
	if _, err := s.Compact(ctx, "", "/compact", five.sink); !errors.As(err, new(*errCompactionSaveFailed)) {
		t.Fatalf("turn 5's /compact = %v; want a save failure", err)
	}
	session = plus(session, a)
	check("the unwritten compaction", five.list(), a, session)

	// Turn 6 (write 7): a step on other/c, which has no price — its tokens
	// count, its cost cannot, and the spend says so.
	if err := s.SetModel("other/c"); err != nil {
		t.Fatal(err)
	}
	f.models["other/c"].push(answerWith("free?"))
	var six events
	if _, err := s.Run(ctx, "unpriced", six.sink); err != nil {
		t.Fatal(err)
	}
	c := unpricedSpend(scriptedUsage)
	session = plus(session, c)
	check("the unpriced step", six.list(), c, session)
	// The same sum by hand: test/a's seven records (turn 1's two steps, the
	// three failed attempts, the unsaved step, the unwritten compaction) at
	// 10×1,000,000 + 5×2,000,000 + 4×500,000 = 22,000,000 p$ each, and
	// test/b's two (the child's row, the manual compaction) at 10×3,000,000 +
	// 5×15,000,000 + 4×300,000 = 106,200,000 p$ each; other/c's step is
	// tokens alone.
	if want := int64(7*22_000_000 + 2*106_200_000); !session.Unpriced || session.CostPicoUSD != want {
		t.Fatalf("the session's spend = %+v; want %d p$, and unpriced", session, want)
	}
}

// TestReasoningIsPricedAsOutput (A31): reasoning tokens are inside a step's
// output, as providers bill them — reported for what they are, priced once,
// at the output rate.
func TestReasoningIsPricedAsOutput(t *testing.T) {
	f := newFixture(t, "http://unused")
	withCost(f, "test/a", 1, 2, 0.5, 4)
	s := f.open(f.options())
	billed := fantasy.Usage{InputTokens: 100, OutputTokens: 40, ReasoningTokens: 30, TotalTokens: 140}
	f.models["test/a"].push(reply(reasoningParts("think"), textParts("answer"),
		[]fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: billed}}))
	var ev events
	if _, err := s.Run(context.Background(), "hi", ev.sink); err != nil {
		t.Fatal(err)
	}
	// 100 input tokens at $1/M, 40 output tokens at $2/M: the 30 of reasoning
	// are among the 40, never priced a second time.
	want := Spend{Input: 100, Output: 40, Reasoning: 30, CostPicoUSD: 100*1_000_000 + 40*2_000_000}
	got := lastSpent(t, ev.list())
	equal(t, "the turn's spend", got.Turn, want)
	equal(t, "the session's spend", got.Session, want)
}

// TestLiveAndResumedSpendAgree (A31, V8's automated half): what a session
// reports at close — its last Spent — is what the Spent its Replay ends with
// reports once it is resumed: the same session and last turn, the same context
// and window. Modulo the usage the live session observed that no entry holds:
// a step whose append failed is spent, and a resume, which reads the
// transcript, never sees it.
func TestLiveAndResumedSpendAgree(t *testing.T) {
	t.Run("all saved", func(t *testing.T) { testLiveAndResumedSpend(t, false) })
	t.Run("an unsaved step", func(t *testing.T) { testLiveAndResumedSpend(t, true) })
}

func testLiveAndResumedSpend(t *testing.T, unsaved bool) {
	f := newFixture(t, "http://unused")
	withPrices(f)
	windowed(f, "test/a", 1_000_000, 0) // a known window, far from compacting on its own
	opts := f.options()
	w := &failingWrites{}
	if unsaved {
		w.fail = map[int]bool{4: true} // turn 3's step
	}
	opts.storeOpenFile = w.open
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var live events

	// Turn 1 (writes 1, 2): a glob and a child on test/b, then the answer.
	f.models["test/a"].push(callStep(globPart("g1"), agentPart(t, "a1", task("look", "look around", "model", "test/b"))),
		answerWith("found nothing"))
	f.models["test/b"].push(answerWith("child done"))
	if _, err := s.Run(ctx, "look", live.sink); err != nil {
		t.Fatal(err)
	}
	// Turn 2 (write 3): /compact.
	f.models["test/a"].push(summaryReply("the search."))
	if _, err := s.Compact(ctx, "", "/compact", live.sink); err != nil {
		t.Fatal(err)
	}
	// Turn 3 (write 4): an answer — whose append fails, in the second run.
	f.models["test/a"].push(answerWith("third"))
	if _, err := s.Run(ctx, "and now", live.sink); (err != nil) != unsaved {
		t.Fatalf("turn 3 = %v; want a failure iff its append failed (%v)", err, unsaved)
	}
	// Turn 4 (write 5): the last answer.
	f.models["test/a"].push(answerWith("fourth"))
	if _, err := s.Run(ctx, "last", live.sink); err != nil {
		t.Fatal(err)
	}
	spentAfterEach(t, live.list())
	final := lastSpent(t, live.list())
	a, b := at(scriptedUsage, rateA), at(scriptedUsage, rateB)
	equal(t, "the live session's spend at close", final.Session, plus(times(5, a), b))
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r := resumed(t, resumeOptions(f.options(), id))
	var replay events
	if err := r.Replay(replay.sink); err != nil {
		t.Fatal(err)
	}
	evs := replay.list()
	got, ok := evs[len(evs)-1].(Spent)
	if !ok {
		t.Fatalf("the replay ends with a %T; want the Spent", evs[len(evs)-1])
	}
	if n := len(of[Spent](evs)); n != 1 {
		t.Fatalf("the replay sent %d Spents; want one, at its end", n)
	}
	want := final
	if unsaved {
		// The failed append's step is the one thing the transcript never held.
		want.Session = plus(times(4, a), b)
	}
	equal(t, "the resumed session's Spent", got, want)
	if got.ContextWindow != 1_000_000 || got.ContextTokens == 0 {
		t.Fatalf("the resumed Spent sizes the context %d of %d; want the next request's, of test/a's window", got.ContextTokens, got.ContextWindow)
	}
}

// TestAChildEmitsNoSpent (A31, P33): a sub-agent reports no Spent — not after
// its steps, not after its compactions — whether its events reach the turn's
// sink (a foreground child) or the session's (a background one); what it
// spent reaches its parent's spend as a row. The parent reports one after
// each of its own steps.
func TestAChildEmitsNoSpent(t *testing.T) {
	childSpent := func(t *testing.T, evs []Event) (spents, steps, compactions int) {
		t.Helper()
		for _, se := range of[SubagentEvent](evs) {
			switch e := se.Event.(type) {
			case Spent:
				spents++
			case StepDone:
				steps++
			case Compacted:
				if e.Phase == CompactionEnded {
					compactions++
				}
			}
		}
		return spents, steps, compactions
	}

	t.Run("foreground", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		windowed(f, "test/b", 1000, 0)
		s := f.open(f.options())
		seedChildren(t, s)
		f.models["test/a"].push(callStep(agentPart(t, "a1", task("spend", "spend tokens", "model", "test/b"))), answerWith("ok"))
		// The child's seed (run by the seam), its pre-turn compaction, a glob
		// step and its answer.
		f.models["test/b"].push(answerSpending("seeded", 2000), summaryReply("the seed."), callStep(globPart("g1")), answerWith("child done"))
		var ev events
		if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		evs := ev.list()
		spents, steps, compactions := childSpent(t, evs)
		if steps != 2 || compactions != 1 {
			t.Fatalf("control: the child's events reached the sink with %d steps and %d compactions; want 2 and 1", steps, compactions)
		}
		if spents != 0 {
			t.Fatalf("the child reported %d Spents; want none", spents)
		}
		if n := spentAfterEach(t, evs); n != 2 {
			t.Fatalf("the parent reported %d Spents; want one per step of its own, 2", n)
		}
	})

	t.Run("background", func(t *testing.T) {
		b := openBG(t)
		ws, _ := b.spawn(t, "child one")
		b.finish(t, ws[0])
		spents, steps, _ := childSpent(t, b.own.list())
		if steps != 1 {
			t.Fatalf("control: the background child's events reached the session's sink with %d steps; want 1", steps)
		}
		if spents != 0 {
			t.Fatalf("the background child reported %d Spents; want none", spents)
		}
		if n := len(of[Spent](b.own.list())); n != 0 {
			t.Fatalf("the session's sink was sent %d Spents; want none: a background child's are its own", n)
		}
	})
}

// TestTurnSpendAttribution (A31, R2-4): a Spent's turn part is the spend of
// the turn it reports on — an entry belongs to the turn of the latest record
// at or before it that numbers one, and a compaction entry to its own turn:
// one before a turn's first request counts toward that turn; a /compact is a
// turn of its own; a wake is a turn of its own, its results' rows with it;
// after a load the turn is the path's last, and the next one starts afresh;
// and a transcript written before turns were recorded is read as its prompts
// open them.
func TestTurnSpendAttribution(t *testing.T) {
	a := at(scriptedUsage, rateA)
	ctx := context.Background()

	t.Run("a pre-turn compaction counts toward the turn it preceded", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		withPrices(f)
		windowed(f, "test/a", 1000, 0)
		s := f.open(f.options())
		f.models["test/a"].push(answerSpending("big", 2000)) // 2009 tokens of context: over 850
		run(t, s, "one")
		big := at(Usage{Input: 2000, Output: 5, CacheRead: 4}, rateA)
		f.models["test/a"].push(summaryReply("turn one."), answerWith("two"))
		var ev events
		if _, err := s.Run(ctx, "two", ev.sink); err != nil {
			t.Fatal(err)
		}
		evs := ev.list()
		if n := spentAfterEach(t, evs); n != 2 {
			t.Fatalf("turn 2 reported %d Spents; want one after its compaction, one after its step", n)
		}
		if c := compactionEntries(t, s); len(c) != 1 || c[0].Turn != 2 {
			t.Fatalf("the compaction entries: %+v; want one, recorded as turn 2", c)
		}
		spents := of[Spent](evs)
		equal(t, "after the compaction: the turn", spents[0].Turn, a)
		equal(t, "after the compaction: the session", spents[0].Session, plus(big, a))
		equal(t, "after the step: the turn", spents[1].Turn, plus(a, a))
		equal(t, "after the step: the session", spents[1].Session, plus(big, a, a))
	})

	t.Run("a manual compaction is a turn of its own", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		withPrices(f)
		s := f.open(f.options())
		f.models["test/a"].push(answerWith("one"))
		run(t, s, "one")
		f.models["test/a"].push(summaryReply("turn one."))
		var compact events
		if _, err := s.Compact(ctx, "", "/compact", compact.sink); err != nil {
			t.Fatal(err)
		}
		got := lastSpent(t, compact.list())
		equal(t, "after /compact: the turn", got.Turn, a)
		equal(t, "after /compact: the session", got.Session, plus(a, a))
		f.models["test/a"].push(answerWith("three"))
		var ev events
		if _, err := s.Run(ctx, "three", ev.sink); err != nil {
			t.Fatal(err)
		}
		got = lastSpent(t, ev.list())
		equal(t, "after turn 3: the turn", got.Turn, a)
		equal(t, "after turn 3: the session", got.Session, times(3, a))
	})

	t.Run("a wake is a turn of its own, its results' rows with it", func(t *testing.T) {
		b := openBG(t)
		ws, _ := b.spawn(t, "child one") // turn 1: two steps; the spawning call carries no usage
		b.finish(t, ws[0])
		b.routers["test/a"].route("go", answerWith("reacting"))
		var ev events
		if _, err := b.s.Wake(ctx, ev.sink); err != nil {
			t.Fatal(err)
		}
		// The wake's step, and its results entry's row: the child's step.
		got := lastSpent(t, ev.list())
		equal(t, "the wake's turn", got.Turn, unpricedSpend(addUsage(scriptedUsage, scriptedUsage)))
		equal(t, "the session", got.Session, unpricedSpend(addUsage(addUsage(scriptedUsage, scriptedUsage), addUsage(scriptedUsage, scriptedUsage))))
	})

	t.Run("after a load the turn is the path's last", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		withPrices(f)
		s, err := Open(f.options())
		if err != nil {
			t.Fatal(err)
		}
		f.models["test/a"].push(answerWith("one"))
		run(t, s, "one")
		f.models["test/a"].push(summaryReply("turn one."))
		if _, err := s.Compact(ctx, "", "/compact", nil); err != nil {
			t.Fatal(err)
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
		equal(t, "after the load: the turn, the /compact's", got.Turn, a)
		equal(t, "after the load: the session", got.Session, plus(a, a))
		f.models["test/a"].push(answerWith("three"))
		var ev events
		if _, err := r.Run(ctx, "three", ev.sink); err != nil {
			t.Fatal(err)
		}
		got = lastSpent(t, ev.list())
		equal(t, "the next turn", got.Turn, a)
		equal(t, "the session after it", got.Session, times(3, a))
	})

	t.Run("an older transcript's turns are inferred", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		withPrices(f)
		s, err := Open(f.options())
		if err != nil {
			t.Fatal(err)
		}
		f.models["test/a"].push(answerWith("one"), callStep(globPart("g1")), answerWith("two"))
		run(t, s, "one")
		run(t, s, "two")
		id, path := s.ID(), s.store.Path()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// As a craze before plan 028 wrote it: no turn on any entry.
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, regexp.MustCompile(`,"turn":[0-9]+`).ReplaceAll(raw, nil), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, e := range transcript(t, s).Entries {
			if e.Turn != 0 {
				t.Fatalf("control: an entry still records turn %d", e.Turn)
			}
		}
		r := resumed(t, resumeOptions(f.options(), id))
		var replay events
		if err := r.Replay(replay.sink); err != nil {
			t.Fatal(err)
		}
		got := lastSpent(t, replay.list())
		equal(t, "the inferred last turn: its two steps", got.Turn, plus(a, a))
		equal(t, "the session", got.Session, times(3, a))
	})
}
