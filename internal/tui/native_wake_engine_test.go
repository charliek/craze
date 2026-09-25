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
)

// The engine over the native session's wake (plan 026 §3.11, §7 A14). It
// lives here rather than in internal/engine because the engine's own tests
// drive it over fakes and may not import the harness (plan 021 §3.1); this
// package is where craze composes an engine over a real native session
// (app.go), and its frame tests already run one. What it pins is the order
// inside native's own s.mu section — the claim released before the ending
// bracket is enqueued — as the engine observes it, through the engine's public
// surface alone: the record a subscription delivers, Submit and State. Every
// wait is a barrier the test owns.

// heldFrameStep yields before, closes reached, waits for release — or the
// context's end, reported as the stream's error — and then yields after.
func heldFrameStep(before, after []fantasy.StreamPart, reached, release chan struct{}) frameStep {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		for _, p := range before {
			if !yield(p) {
				return
			}
		}
		close(reached)
		select {
		case <-ctx.Done():
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		case <-release:
		}
		for _, p := range after {
			if !yield(p) {
				return
			}
		}
	}
}

// wakeWatchdog bounds every wait here; a passing run never comes near it.
const wakeWatchdog = 10 * time.Second

// wakeRecord reads the engine's record through a subscription, in order.
type wakeRecord struct {
	t    *testing.T
	sub  *agent.Subscription
	seen []agent.Event
}

func (r *wakeRecord) next() agent.Event {
	r.t.Helper()
	select {
	case rec, ok := <-r.sub.Records():
		if !ok {
			r.t.Fatalf("the subscription ended: %v\nseen:\n  %s", r.sub.Err(), strings.Join(wakeShapes(r.seen), "\n  "))
		}
		ev, err := rec.Event()
		if err != nil {
			r.t.Fatal(err)
		}
		r.seen = append(r.seen, ev)
		return ev
	case <-time.After(wakeWatchdog):
		r.t.Fatalf("no event in %s\nseen:\n  %s", wakeWatchdog, strings.Join(wakeShapes(r.seen), "\n  "))
	}
	panic("unreachable")
}

// until reads the record up to and including the first event pred accepts.
func (r *wakeRecord) until(pred func(agent.Event) bool) {
	r.t.Helper()
	for !pred(r.next()) {
	}
}

// wakeShape is one event as this test compares it; "" is an event it does not
// read (tool rows, roster events, settings deltas).
func wakeShape(ev agent.Event) string {
	switch ev.Type {
	case agent.EventTurn:
		t := ev.Turn
		if t.Phase == agent.TurnStarted {
			return fmt.Sprintf("started %s %s %q", t.ID, t.Origin, t.Text)
		}
		s := fmt.Sprintf("ended %s stop=%q next=%q pending=%d", t.ID, t.StopReason, t.Next, t.Pending)
		if t.Synthetic {
			s += " synthetic"
		}
		return s
	case agent.EventQueue:
		return fmt.Sprintf("queue %s %q", ev.QueueChange, ev.Queue.Text)
	case agent.EventText:
		return fmt.Sprintf("text %q", ev.Text)
	case agent.EventDone:
		return "done " + ev.StopReason
	case agent.EventError:
		return "error"
	case agent.EventForeignTurn:
		return fmt.Sprintf("foreign running=%v reason=%s", ev.ForeignTurn.Running, ev.ForeignTurn.Reason)
	}
	return ""
}

// wakeShapes is wakeShape over evs, the unread events left out.
func wakeShapes(evs []agent.Event) []string {
	var out []string
	for _, ev := range evs {
		if s := wakeShape(ev); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func wakeAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(wakeWatchdog):
		t.Fatalf("still waiting for %s after %s", what, wakeWatchdog)
	}
}

// userTexts is every text part of a message, joined.
func userTexts(m fantasy.Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

// TestWakeEndingReleasesBeforeObserver (§3.11, X30; A14): a background child
// finishes while the engine is idle, and the native session wakes; a prompt
// submitted during the wake is queued behind it — the engine reads
// ForeignTurn under the fence it raised first, so the wake never refuses an
// engine-driven Begin (F2: the engine-level outcome of "wake first" is
// queued) — and the moment the engine's observer sees the ending bracket and
// runs its driver, the queued row drains and runs: the wake's ending was
// enqueued in the same s.mu section that released the claim, so the driver
// finds the session free, with nothing between the ending and the drain. The
// drained turn carries the result to nobody a second time: the wake committed
// it.
func TestWakeEndingReleasesBeforeObserver(t *testing.T) {
	ws := frameWorkspace(t)
	echo := newFrameRouter("test", "wire-echo")
	childReached, childRelease := make(chan struct{}), make(chan struct{})
	wakeReached, wakeRelease := make(chan struct{}), make(chan struct{})
	const task = "child work"
	echo.route("go", frameCalls(frameBackgroundCall("a1", "job", task)),
		frameParts(nativeTextParts("started"), nativeFinishParts()))
	echo.route(task, heldFrameStep(nil, cat(nativeTextParts("did work"), nativeFinishParts()), childReached, childRelease))
	echo.routeWake(heldFrameStep(nativeTextParts("noted"), nativeFinishParts(), wakeReached, wakeRelease))
	echo.route("later", frameParts(nativeTextParts("echo later"), nativeFinishParts()))
	sess := frameBackground(t, ws, echo, &frameClock{})

	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := &wakeRecord{t: t, sub: sub}
	submit := func(text string) engine.SubmitResult {
		t.Helper()
		res, err := e.Submit(engine.Command{}, text, engine.SubmitQueue, "")
		if err != nil {
			t.Fatalf("submit %q: %v", text, err)
		}
		return res
	}
	turnEnded := func(id string) func(agent.Event) bool {
		return func(ev agent.Event) bool {
			return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded && ev.Turn.ID == id
		}
	}
	bracket := func(running bool) func(agent.Event) bool {
		return func(ev agent.Event) bool {
			return ev.Type == agent.EventForeignTurn && ev.ForeignTurn != nil && ev.ForeignTurn.Running == running
		}
	}

	if res := submit("go"); res.Turn != "turn-1" {
		t.Fatalf("the first submit answered %+v", res)
	}
	rec.until(turnEnded("turn-1"))
	wakeAwait(t, childReached, "the child's step")
	close(childRelease)
	// The wake opens once the child's result is pending; its text follows the
	// opening bracket, and the submit below lands after both.
	rec.until(bracket(true))
	wakeAwait(t, wakeReached, "the wake's step")
	rec.until(func(ev agent.Event) bool { return ev.Type == agent.EventText && ev.Text == "noted" })

	res := submit("later")
	if res.Queued == nil || res.Turn != "" || res.Armed {
		t.Fatalf("a submit during the wake answered %+v; want the row queued behind it", res)
	}
	if st := e.State(); st.Activity != engine.ActivityIdle || len(st.Queue) != 1 || !st.ForeignTurn {
		t.Fatalf("the engine during the wake: activity %v, %d queued, foreign %v", st.Activity, len(st.Queue), st.ForeignTurn)
	}
	// The queued row's event is in the record before the wake goes on.
	rec.until(func(ev agent.Event) bool { return ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueQueued })
	close(wakeRelease)
	rec.until(turnEnded("turn-2"))

	from := 0
	for i, ev := range rec.seen {
		if bracket(true)(ev) {
			from = i
			break
		}
	}
	want := []string{
		`foreign running=true reason=subagent_wake`,
		`text "noted"`,
		`queue queued "later"`,
		`foreign running=false reason=subagent_wake`,
		`queue sent "later"`,
		`started turn-2 drain "later"`,
		`text "echo later"`,
		`done end_turn`,
		`ended turn-2 stop="end_turn" next="" pending=0`,
	}
	if got := wakeShapes(rec.seen[from:]); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the record from the wake's opening:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if st := e.State(); st.Activity != engine.ActivityIdle || len(st.Queue) != 0 || st.ForeignTurn {
		t.Fatalf("the engine after the drain: activity %v, %d queued, foreign %v", st.Activity, len(st.Queue), st.ForeignTurn)
	}

	// The wake's request carried the result; the drained turn's carried
	// nothing of it beyond the wake's own entry in its history.
	reqs := echo.requests()
	if n := len(reqs); n != 5 {
		t.Fatalf("%d requests, want the parent's two, the child's, the wake's and the drained turn's", n)
	}
	wake, drained := reqs[3], reqs[4]
	if last := userTexts(wake.Prompt[len(wake.Prompt)-1]); !strings.Contains(last, `<subagent_result id="`) || !strings.Contains(last, "did work") {
		t.Fatalf("the wake's user message is %q", last)
	}
	for _, m := range drained.Prompt[len(wake.Prompt):] {
		if m.Role == fantasy.MessageRoleUser && strings.Contains(userTexts(m), "<subagent_result") {
			t.Fatalf("the drained turn carried the result a second time: %q", userTexts(m))
		}
	}
}
