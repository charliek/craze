package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

const watchdog = 10 * time.Second

// The driver's schedules are pinned over a scripted double (fake_test.go). These
// run it over tui.Stub, the session every frame golden is drawn from, so that
// what the goldens will exercise — the Stub's own claim, its withdraw on a
// cancel, its hung turn ending cancelled, its injected clock — is what the
// engine has been seen to drive.

func newStubEngine(t *testing.T) (*tui.Stub, *engine.Engine, *agent.Subscription) {
	t.Helper()
	stub := tui.NewStubNoPrimary()
	e, err := engine.New(stub, engine.Options{})
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
	return stub, e, sub
}

// readUntil reads the record up to the first event pred accepts.
func readUntil(t *testing.T, sub *agent.Subscription, pred func(agent.Event) bool) []agent.Event {
	t.Helper()
	var got []agent.Event
	for {
		select {
		case rec, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the subscription ended: %v", sub.Err())
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, ev)
			if pred(ev) {
				return got
			}
		case <-time.After(watchdog):
			t.Fatalf("no event in %s; read %d so far", watchdog, len(got))
		}
	}
}

func chainOver(ev agent.Event) bool {
	return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded && ev.Turn.Next == "" && ev.Turn.Pending == 0
}

func TestTheStubRunsAChainThroughTheEngine(t *testing.T) {
	stub, e, sub := newStubEngine(t)
	// ParkNext and not HangNext: it hands back a barrier, so the cancel below
	// provably lands on a prompt that is parked. A hung turn has no such
	// barrier, and a cancel that beat it to the opening would make it withdraw
	// and leave the Stub's hang armed for the turn after.
	parked := stub.ParkNext()
	res, err := e.Submit(engine.Command{}, "one", engine.SubmitQueue, "")
	if err != nil || res.Turn == "" {
		t.Fatalf("submit: %+v, %v", res, err)
	}
	if res, err := e.Submit(engine.Command{}, "two", engine.SubmitQueue, ""); err != nil || res.Queued == nil {
		t.Fatalf("a submit behind the parked prompt: %+v, %v", res, err)
	}
	select {
	case <-parked:
	case <-time.After(watchdog):
		t.Fatal("the prompt never parked")
	}
	// The parked prompt ends only when it is cancelled, as Esc ends it: it
	// withdraws, the session says nothing at all, and the engine authors its
	// ending. The row behind it carries on: the TUI's chain policy.
	if _, err := e.Cancel(context.Background(), engine.Command{}, res.Turn); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := readUntil(t, sub, chainOver)
	var texts, ends []string
	for _, ev := range got {
		switch {
		case ev.Type == agent.EventText:
			texts = append(texts, ev.Text)
		case ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded:
			ends = append(ends, ev.Turn.ID+":"+ev.Turn.StopReason+"→"+ev.Turn.Next)
		}
	}
	if strings.Join(ends, " ") != "turn-1:cancelled→turn-2 turn-2:end_turn→" {
		t.Fatalf("endings %q", ends)
	}
	if strings.Join(texts, "|") != "echo: two" {
		t.Fatalf("the Stub said %q", texts)
	}
	if got := stub.Prompts(); strings.Join(got, "|") != "one|two" {
		t.Fatalf("the Stub was handed %q", got)
	}
	if st := e.State(); st.Activity != engine.ActivityIdle || st.SessionID == "" {
		t.Fatalf("state: %+v", st)
	}
}

// TestASendNowOnTheStubFiresWhenTheCancelledTurnSettles is the path a golden
// will take: Ctrl+L on a provider that cannot interject arms the send, the
// engine cancels the parked prompt — which withdraws, so the Stub says nothing
// at all about it — and the settlement fires the send as the successor, ahead of
// the row that was already queued.
func TestASendNowOnTheStubFiresWhenTheCancelledTurnSettles(t *testing.T) {
	stub, e, sub := newStubEngine(t)
	parked := stub.ParkNext()
	if _, err := e.Submit(engine.Command{}, "one", engine.SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Queue(engine.Command{}, "queued"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(watchdog):
		t.Fatal("the prompt never parked")
	}
	res, err := e.Submit(engine.Command{}, "now", engine.SubmitSendNow, "")
	if err != nil || !res.Armed {
		t.Fatalf("arming a send-now on the Stub: %+v, %v", res, err)
	}
	var ends, starts []string
	for _, ev := range readUntil(t, sub, chainOver) {
		if ev.Type != agent.EventTurn {
			continue
		}
		switch ev.Turn.Phase {
		case agent.TurnStarted:
			starts = append(starts, ev.Turn.ID+":"+ev.Turn.Origin+":"+ev.Turn.Text)
		case agent.TurnEnded:
			ends = append(ends, ev.Turn.ID+":"+ev.Turn.StopReason+"→"+ev.Turn.Next)
		}
	}
	// turn-1 was withdrawn by the cancel the arm asked for and names the armed
	// send as its successor; the armed send goes before the queued row.
	if got := strings.Join(ends, " "); got != "turn-1:cancelled→turn-2 turn-2:end_turn→turn-3 turn-3:end_turn→" {
		t.Fatalf("endings %q", got)
	}
	if got := strings.Join(starts, " "); got != "turn-1:submit:one turn-2:send_now:now turn-3:drain:queued" {
		t.Fatalf("the starts: %q", got)
	}
	if got := stub.Prompts(); strings.Join(got, "|") != "one|now|queued" {
		t.Fatalf("the Stub was handed %q", got)
	}
	if st := e.State(); st.SendNow != nil || len(st.Queue) != 0 {
		t.Fatalf("state after the send fired: %+v", st)
	}
}

// TestEngineEventsCarryTheStubsClock: engine-authored events are stamped from
// the session's clock, not the wall's, so a golden that shows a time shows the
// injected one.
func TestEngineEventsCarryTheStubsClock(t *testing.T) {
	stub := tui.NewStubNoPrimary()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stub.Clock = func() time.Time { return at }
	e, err := engine.New(stub, engine.Options{})
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
	if _, err := e.Submit(engine.Command{}, "when", engine.SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	for _, ev := range readUntil(t, sub, chainOver) {
		if ev.Type == agent.EventTurn && !ev.At.Equal(at) {
			t.Fatalf("%s %s is stamped %s, want the Stub's clock %s", ev.Turn.Phase, ev.Turn.ID, ev.At, at)
		}
	}
}

// TestClosingTheEngineEndsTheHungTurnOnTheRecord is the live smoke's finding A1
// over the session every golden runs on: a quit while a turn is open on the wire
// leaves that turn a started and an ended and nothing else, where before it left
// a turn that never closed.
//
// askEngine is the rig, because its Stub's log has a PRIMARY and the primary is
// the one reader that survives a close (committed says why): a budgeted
// subscription's owner stops delivering the instant the log ends, so the very
// event this test is about would be the one it could not see.
//
// HangNext's barrier is received from first: a turn that has not opened yet is
// still withdrawable, and the ending a withdrawn prompt gets is the cancelled
// one — a different claim from this one.
func TestClosingTheEngineEndsTheHungTurnOnTheRecord(t *testing.T) {
	stub, e := askEngine(t)
	open := stub.HangNext()
	res, err := e.Submit(engine.Command{}, "one", engine.SubmitQueue, "")
	if err != nil || res.Turn == "" {
		t.Fatalf("submit: %+v, %v", res, err)
	}
	select {
	case <-open:
	case <-time.After(watchdog):
		t.Fatal("the turn never opened")
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Read once, after Close has returned — which is after its wait on the
	// turn's own goroutine, so the continuation has already come back and had
	// its chance to author a second ending.
	var phases []string
	var ending *agent.TurnInfo
	for _, ev := range committed(t, e) {
		if ev.Type != agent.EventTurn || ev.Turn == nil || ev.Turn.ID != res.Turn {
			continue
		}
		phases = append(phases, ev.Turn.Phase)
		if ev.Turn.Phase == agent.TurnEnded {
			ending = ev.Turn
		}
	}
	if strings.Join(phases, "|") != "started|ended" {
		t.Fatalf("%s left the record %q, want exactly one started and one ended", res.Turn, phases)
	}
	if !ending.Synthetic || ending.StopReason != "closing" || ending.Next != "" || ending.Pending != 0 {
		t.Fatalf("the ending is %+v, want a synthetic closing with no successor", ending)
	}
	if ending.Err != "" || ending.ErrClass != "" {
		t.Fatalf("the ending carries a failure %q/%q: a close is not one", ending.Err, ending.ErrClass)
	}
	if st := e.State(); st.Turn != "" || st.Activity != engine.ActivityClosing {
		t.Fatalf("state after the close: %+v", st)
	}
}

// TestClosingTheEngineClosesTheStub: the session owner will hold the engine and
// close it, and that has to be the whole of shutdown.
func TestClosingTheEngineClosesTheStub(t *testing.T) {
	stub, e, _ := newStubEngine(t)
	stub.HangNext()
	if _, err := e.Submit(engine.Command{}, "one", engine.SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close did not return with a hung turn on the Stub")
	}
	if _, err := e.Submit(engine.Command{}, "late", engine.SubmitQueue, ""); err == nil {
		t.Fatal("a closed engine admitted a prompt")
	}
}
