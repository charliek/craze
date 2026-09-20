package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// watchdog is how long a test waits before it calls a wait a deadlock. It is
// not a timing assertion: nothing in a passing run waits this long.
const watchdog = 10 * time.Second

// rig is an engine over a fakeSession, read through a budgeted subscription:
// the ordered record every client would fold, with nobody stealing from the
// primary. Most rigs run the log with no primary at all, which is also what a
// session with no client is.
type rig struct {
	t    *testing.T
	s    *fakeSession
	e    *Engine
	sub  *agent.Subscription
	seen []agent.Event
}

func newRig(t *testing.T, opts Options) *rig {
	t.Helper()
	return newRigOn(t, opts, agent.EventLogOptions{NoPrimary: true})
}

func newRigOn(t *testing.T, opts Options, lo agent.EventLogOptions) *rig {
	t.Helper()
	s := newFake(t, lo)
	e, err := New(s, opts)
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
	return &rig{t: t, s: s, e: e, sub: sub}
}

// next is the next event of the record, in order.
func (r *rig) next() agent.Event {
	r.t.Helper()
	select {
	case rec, ok := <-r.sub.Records():
		if !ok {
			r.t.Fatalf("the subscription ended: %v\nseen: %s", r.sub.Err(), describe(r.seen))
		}
		ev, err := rec.Event()
		if err != nil {
			r.t.Fatal(err)
		}
		r.seen = append(r.seen, ev)
		return ev
	case <-time.After(watchdog):
		r.t.Fatalf("no event in %s\nseen: %s", watchdog, describe(r.seen))
	}
	panic("unreachable")
}

// until reads the record up to and including the first event pred accepts, and
// returns what it read.
func (r *rig) until(pred func(agent.Event) bool) []agent.Event {
	r.t.Helper()
	var got []agent.Event
	for {
		ev := r.next()
		got = append(got, ev)
		if pred(ev) {
			return got
		}
	}
}

// ended accepts the ending of the turn named id; "" is any turn's.
func ended(id string) func(agent.Event) bool {
	return func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded && (id == "" || ev.Turn.ID == id)
	}
}

// started accepts the start of the turn named id; "" is any turn's.
func started(id string) func(agent.Event) bool {
	return func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnStarted && (id == "" || ev.Turn.ID == id)
	}
}

// lastEnding accepts the ending that closes a chain: no successor, nothing
// queued.
func lastEnding(ev agent.Event) bool {
	return ended("")(ev) && ev.Turn.Next == "" && ev.Turn.Pending == 0
}

// shape is one event as a test compares it: enough to pin order and meaning,
// nothing a clock or a counter decides.
func shape(ev agent.Event) string {
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
		if t.ErrClass != "" {
			s += " class=" + string(t.ErrClass)
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
		return fmt.Sprintf("foreign running=%v", ev.ForeignTurn.Running)
	}
	return string(ev.Type)
}

func shapes(evs []agent.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = shape(ev)
	}
	return out
}

func describe(evs []agent.Event) string { return "\n  " + strings.Join(shapes(evs), "\n  ") }

func (r *rig) wantShapes(got []agent.Event, want ...string) {
	r.t.Helper()
	g := shapes(got)
	if len(g) != len(want) {
		r.t.Fatalf("got %d events, want %d\ngot: %s\nwant:\n  %s", len(g), len(want), describe(got), strings.Join(want, "\n  "))
	}
	for i := range want {
		if g[i] != want[i] {
			r.t.Fatalf("event %d is %q, want %q\ngot: %s", i, g[i], want[i], describe(got))
		}
	}
}

func (r *rig) submit(text string) SubmitResult {
	r.t.Helper()
	res, err := r.e.Submit(Command{}, text, SubmitQueue, "")
	if err != nil {
		r.t.Fatalf("submit %q: %v", text, err)
	}
	return res
}

func (r *rig) wantPrompts(want ...string) {
	r.t.Helper()
	got := r.s.prompts()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		r.t.Fatalf("the session was handed %q, want %q", got, want)
	}
}

// await waits on a barrier, with the watchdog.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(watchdog):
		t.Fatalf("still waiting for %s after %s", what, watchdog)
	}
}

// sync is Sync with the watchdog, for a test that has to know the engine's
// events are in the record before it reads State.
func (r *rig) sync() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := r.e.Sync(ctx); err != nil {
		r.t.Fatalf("sync: %v", err)
	}
}
