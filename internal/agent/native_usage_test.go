package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// What a native session has spent, on the wire (plan 028 §3.14, seam 8; §7
// A32): the harness's Spent after every step, every compaction and a load's
// replay becomes the snapshot's usage section and a usage delta, and a load's
// install delta carries the replay's.

// withPrices gives the fixture's test/a a context window and a price — $1 per
// million input tokens and $2 per million output, so a scripted step's 10 in
// and 5 out cost 20,000,000 picodollars — in the table every session the
// fixture opens loads.
func withPrices(t *testing.T, f *nativeFixture, window int) {
	t.Helper()
	table := nativeTestTable("http://127.0.0.1:9/v1")
	m := table.Models["test/a"]
	m.ContextWindow = window
	in, out := 1.0, 2.0
	m.Cost = &modeltable.Cost{Input: &in, Output: &out}
	table.Models["test/a"] = m
	if err := modeltable.Save(f.dir, table); err != nil {
		t.Fatalf("saving the priced table: %v", err)
	}
}

// usageDeltas is the index in evs of every EventMeta whose delta carries the
// usage section.
func usageDeltas(evs []Event) []int {
	var out []int
	for i, ev := range evs {
		if ev.Type == EventMeta && ev.State != nil && ev.State.Usage != nil {
			out = append(out, i)
		}
	}
	return out
}

// indexOf is the index of the last event of typ in evs, -1 for none.
func indexOf(evs []Event, typ EventType) int {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == typ {
			return i
		}
	}
	return -1
}

// oneUsageDelta is the one usage delta a turn published: a delta carrying
// the usage section alone — no other section, no Text or Mode beside it, so
// `craze prompt --json` prints no line for it — after every event but the
// turn's ending, which it precedes (the prompt flushes the outbox before the
// ending, native.go).
func oneUsageDelta(t *testing.T, evs []Event, after EventType) UsageState {
	t.Helper()
	idx := usageDeltas(evs)
	if len(idx) != 1 {
		t.Fatalf("the turn published %d usage deltas, want 1:\n%s", len(idx), strings.Join(loadLines(evs), "\n"))
	}
	ev := evs[idx[0]]
	if want := (StateDelta{Usage: ev.State.Usage}); !reflect.DeepEqual(*ev.State, want) || ev.Text != "" || ev.Mode != "" || ev.Replayed {
		t.Fatalf("the usage delta carries more than its section: %+v", ev)
	}
	if a, done := indexOf(evs, after), indexOf(evs, EventDone); a < 0 || done < 0 || a > idx[0] || idx[0] > done {
		t.Fatalf("the usage delta is at %d, the last %s at %d and the ending at %d:\n%s", idx[0], after, a, done,
			strings.Join(loadLines(evs), "\n"))
	}
	return *ev.State.Usage
}

// TestNativeUsageOnTheWire: a new session's install carries no usage and its
// snapshot has none; each turn's step then publishes one usage delta — the
// turn's spend and the session's, priced, with the context and the window —
// and the snapshot says the same; a /compact, a turn of its own, reports its
// summarizer's spend the same way; and a load of the stored session carries
// the replay's report in its install delta, stamped replayed, with no usage
// delta of its own inside the bracket.
func TestNativeUsageOnTheWire(t *testing.T) {
	ctx := context.Background()
	perStep := Spend{Input: 10, Output: 5, CostPicoUSD: 10*1_000_000 + 5*2_000_000}
	times := func(n int64) Spend {
		return Spend{Input: n * perStep.Input, Output: n * perStep.Output, CostPicoUSD: n * perStep.CostPicoUSD}
	}

	f := newNativeFixture(t)
	withPrices(t, f, 100_000)
	// One workspace for both sessions: a stored session is found under the
	// workspace it ran in.
	ws := t.TempDir()
	s := f.session(Options{Workspace: ws})
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if install := takeStartDelta(t, s.log); install.State.Usage != nil {
		t.Fatalf("a new session's install carries usage %+v: it has spent nothing", install.State.Usage)
	}
	if u := s.Snapshot().Usage; u != nil {
		t.Fatalf("a new session's snapshot has usage %+v", u)
	}

	a := f.models["test/a"]
	a.push(answer("hi there"))
	if _, err := s.Prompt(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	u := oneUsageDelta(t, deltaSettled(t, s), EventText)
	if u.Turn != perStep || u.Session != perStep || u.ContextWindow != 100_000 || u.ContextTokens <= 0 {
		t.Fatalf("after the first turn the usage is %+v; want turn and session %+v, window 100000 and a context", u, perStep)
	}
	if got := s.Snapshot().Usage; got == nil || *got != u {
		t.Fatalf("the snapshot's usage is %+v, the delta's %+v", got, u)
	}

	a.push(answer("again"))
	if _, err := s.Prompt(ctx, "more"); err != nil {
		t.Fatal(err)
	}
	if u = oneUsageDelta(t, deltaSettled(t, s), EventText); u.Turn != perStep || u.Session != times(2) {
		t.Fatalf("after the second turn the usage is %+v; want turn %+v and session %+v", u, perStep, times(2))
	}

	// A /compact is a turn of its own (R2-4): its summarizer's usage is its
	// turn's spend, and the session's grows by it.
	a.push(nativeSummary("The user said hello, then more."))
	if _, err := s.Prompt(ctx, "/compact"); err != nil {
		t.Fatal(err)
	}
	live := oneUsageDelta(t, deltaSettled(t, s), EventCompaction)
	if live.Turn != perStep || live.Session != times(3) {
		t.Fatalf("after /compact the usage is %+v; want turn %+v and session %+v", live, perStep, times(3))
	}
	id := s.Snapshot().SessionID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	loaded := f.session(Options{Workspace: ws, LoadSessionID: id})
	evs, err := startLoad(t, loaded)
	if err != nil {
		t.Fatalf("the load: %v", err)
	}
	idx := usageDeltas(evs)
	if len(idx) != 1 {
		t.Fatalf("the load published %d usage deltas, want the install's alone:\n%s", len(idx), strings.Join(loadLines(evs), "\n"))
	}
	install := evs[idx[0]]
	end := indexOf(evs, EventReplay)
	if !install.Replayed || install.State.Model == nil || install.State.Commands == nil || end < idx[0] ||
		evs[end].Replay.Phase != ReplayEnd {
		t.Fatalf("the usage is not the install delta's, inside the bracket:\n%s", strings.Join(loadLines(evs), "\n"))
	}
	// What the transcript records is what the live session reported: every
	// step and the compaction were saved, so nothing observed-unsaved is
	// missing (X43), and "the turn" after a load is the last one recorded —
	// the /compact's.
	got := *install.State.Usage
	if got.Session != live.Session || got.Turn != live.Turn || got.ContextWindow != live.ContextWindow || got.ContextTokens <= 0 {
		t.Fatalf("the load's install carries usage %+v; the live session last reported %+v", got, live)
	}
	if snap := loaded.Snapshot().Usage; snap == nil || *snap != got {
		t.Fatalf("the loaded snapshot's usage is %+v, its install's %+v", snap, got)
	}
}

// TestNativeUsageOfAnUnpricedModel: a model with no cost in the table has its
// tokens counted and no cost — Unpriced, in the turn's spend and the
// session's alike — and a model the table gives no context window reports 0
// for it, which a client reads as unknown.
func TestNativeUsageOfAnUnpricedModel(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	f.models["test/a"].push(answer("hi there"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	u := oneUsageDelta(t, deltaSettled(t, s), EventText)
	want := Spend{Input: 10, Output: 5, Unpriced: true}
	if u.Turn != want || u.Session != want || u.ContextWindow != 0 || u.ContextTokens <= 0 {
		t.Fatalf("an unpriced model's usage is %+v; want turn and session %+v, no window and a context", u, want)
	}
}

// TestNativeUsageOfALoadThatOpensEmpty: a load whose transcript is missing
// opens empty under the same id (X25) and has spent nothing — its replay
// reports no Spent, so its install carries no usage section and its snapshot
// none, as a new session's.
func TestNativeUsageOfALoadThatOpensEmpty(t *testing.T) {
	f := newNativeFixture(t)
	loaded := f.session(Options{LoadSessionID: "20260918T120000-abcdef"})
	evs, err := startLoad(t, loaded)
	if err != nil {
		t.Fatalf("the load: %v", err)
	}
	if idx := usageDeltas(evs); len(idx) != 0 {
		t.Fatalf("an empty load published usage:\n%s", strings.Join(loadLines(evs), "\n"))
	}
	if u := loaded.Snapshot().Usage; u != nil {
		t.Fatalf("an empty load's snapshot has usage %+v", u)
	}
}

// TestNativeUsageSnapshotIsACopy: the snapshot's usage is the caller's own —
// writing through it changes neither the session's nor the next snapshot's.
func TestNativeUsageSnapshotIsACopy(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	f.models["test/a"].push(answer("hi there"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	deltaSettled(t, s)
	first := s.Snapshot().Usage
	if first == nil {
		t.Fatal("no usage after a turn")
	}
	was := *first
	first.Session.Input = 1 << 40
	if again := s.Snapshot().Usage; again == nil || *again != was {
		t.Fatalf("a caller's write reached the session: %+v, want %+v", again, was)
	}
}
