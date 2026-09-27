package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// TestATurnsUsageReachesTheStreamBeforeItsEnd (plan 028 §3.14; the S2 seam),
// on the engine over a real native session: a turn's usage — the report its
// last step leaves, or a /compact's — is in the engine's stream BEFORE the
// engine's EventTurn{ended} for that turn, so a client that folds up to the
// turn's end and draws the idle frame there reads the turn's own spend,
// never the one before it. The session enqueues each report on the harness's
// callbacks — a step's, a compaction's — inside Prompt, into the log's one
// FIFO outbox, and flushes that outbox before its ending and before Prompt
// returns (agent/native.go's prompt); the engine enqueues the turn's ending
// into the same outbox only after Prompt has returned. What the stream
// carried last before the end is exactly what the session reports at idle.
//
// It lives here, not in internal/engine, because the engine's tests may not
// import the harness (.golangci.yml's engine-test rule), and a native session
// needs its table and model seam (nativeSessionTweak).
func TestATurnsUsageReachesTheStreamBeforeItsEnd(t *testing.T) {
	isolateSkillsHome(t)
	table := nativeOneModelTable()
	echo := table.Models["test/echo"]
	echo.ContextWindow = 100_000
	in, out := 1.0, 2.0
	echo.Cost = &modeltable.Cost{Input: &in, Output: &out}
	table.Models["test/echo"] = echo
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{
		cat(nativeTextParts("hi there"), nativeFinishParts()),
		nativeSummaryParts("The user said hello.", 0),
	}
	sess := agent.NewNative(agent.Options{Workspace: frameWorkspace(t), ContentHome: t.TempDir()},
		nativeSessionTweak(t.TempDir(), table, model))
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var before agent.Spend // the session's spend before each turn
	for _, tc := range []struct {
		name, text string
		after      agent.EventType // the turn's last event of its own before the report
	}{
		{"a turn of one step", "hello", agent.EventText},
		{"a /compact", "/compact", agent.EventCompaction},
	} {
		res, err := e.Submit(engine.Command{}, tc.text, engine.SubmitQueue, "")
		if err != nil || res.Turn == "" {
			t.Fatalf("%s: submit = %+v, %v; want a turn", tc.name, res, err)
		}
		evs := turnEvents(t, e, res.Turn)
		end := len(evs) - 1
		if evs[end].Turn.Err != "" {
			t.Fatalf("%s: the turn failed: %s%s", tc.name, evs[end].Turn.Err, describeUsageOrder(evs))
		}
		final := sess.Snapshot().Usage // the session's report at idle
		if final == nil {
			t.Fatalf("%s: the session reports no usage after the turn", tc.name)
		}
		got, at := lastUsage(evs[:end])
		if got == nil || *got != *final {
			t.Fatalf("%s: before the turn's end the stream's last usage is %+v (at %d; the end at %d); the session reports %+v at idle%s",
				tc.name, got, at, end, final, describeUsageOrder(evs))
		}
		if last := lastOfType(evs, tc.after); last < 0 || last > at {
			t.Fatalf("%s: the usage (at %d) is not the turn's own: its last %s is at %d%s", tc.name, at, tc.after, last, describeUsageOrder(evs))
		}
		if final.Session.Input <= before.Input || final.Session.CostPicoUSD <= before.CostPicoUSD {
			t.Fatalf("%s: the session's spend %+v did not grow past %+v", tc.name, final.Session, before)
		}
		before = final.Session
	}
	if model.calls != 2 {
		t.Fatalf("the session sent %d requests; want the turn's and the summarizer's", model.calls)
	}
}

// turnEvents reads the engine's stream from turn's start up to and including
// its EventTurn{ended}.
func turnEvents(t *testing.T, e *engine.Engine, turn string) []agent.Event {
	t.Helper()
	var evs []agent.Event
	started := false
	deadline := time.After(wakeWatchdog)
	for {
		select {
		case ev := <-e.Events():
			if ev.Type == agent.EventTurn && ev.Turn.ID == turn && ev.Turn.Phase == agent.TurnStarted {
				started = true
			}
			if !started {
				continue
			}
			evs = append(evs, ev)
			if ev.Type == agent.EventTurn && ev.Turn.ID == turn && ev.Turn.Phase == agent.TurnEnded {
				return evs
			}
		case <-deadline:
			t.Fatalf("turn %s did not end in %s%s", turn, wakeWatchdog, describeUsageOrder(evs))
		}
	}
}

// lastUsage is the usage section of the last state delta in evs that carries
// one, and its index; nil and -1 for none.
func lastUsage(evs []agent.Event) (*agent.UsageState, int) {
	for i := len(evs) - 1; i >= 0; i-- {
		if ev := evs[i]; ev.Type == agent.EventMeta && ev.State != nil && ev.State.Usage != nil {
			return ev.State.Usage, i
		}
	}
	return nil, -1
}

// lastOfType is the index of the last event of typ in evs, -1 for none.
func lastOfType(evs []agent.Event, typ agent.EventType) int {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == typ {
			return i
		}
	}
	return -1
}

// describeUsageOrder is evs one per line, in order, each usage delta's
// section spelled out: what a failure above needs to show the order.
func describeUsageOrder(evs []agent.Event) string {
	var b strings.Builder
	for i, ev := range evs {
		fmt.Fprintf(&b, "\n  %d %s", i, ev.Type)
		switch {
		case ev.Type == agent.EventTurn:
			fmt.Fprintf(&b, " %s %s stop=%q", ev.Turn.ID, ev.Turn.Phase, ev.Turn.StopReason)
		case ev.Type == agent.EventMeta && ev.State != nil && ev.State.Usage != nil:
			fmt.Fprintf(&b, " usage %+v", *ev.State.Usage)
		case ev.Type == agent.EventCompaction && ev.Compaction != nil:
			fmt.Fprintf(&b, " %s", ev.Compaction.Phase)
		case ev.Type == agent.EventText:
			fmt.Fprintf(&b, " %q", ev.Text)
		}
	}
	return b.String()
}
