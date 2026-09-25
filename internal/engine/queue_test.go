package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// steering scripts a turn that accepts text mid-run and cannot answer it: what
// a native interjection the harness took and no step wrote down comes back as
// (agent.Result.Unanswered).
func steering(sc *script, texts ...string) *script { sc.unanswered = texts; return sc }

// TestTheQueueVerbs is the band's four verbs, each one section, each naming the
// command that caused it, and each leaving the queue in the order a client
// draws.
func TestTheQueueVerbs(t *testing.T) {
	r := newRig(t, Options{})
	client := r.e.NewClientID()
	cmd := func(n int) Command { return Command{Client: client, ID: fmt.Sprint(n)} }

	one, err := r.e.Queue(cmd(1), "one")
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if one.ID == "" || one.Text != "one" || one.Version != 0 {
		t.Fatalf("the queued row is %+v", one)
	}
	two, err := r.e.Queue(cmd(2), "two")
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := r.e.EditQueued(cmd(3), two.ID, "two, edited", nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	three, err := r.e.Queue(cmd(4), "three")
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	gone, err := r.e.Unqueue(cmd(5), one.ID)
	if err != nil {
		t.Fatalf("unqueue: %v", err)
	}
	if gone.ID != one.ID || gone.Text != "one" {
		t.Fatalf("unqueue answered %+v, want the row that went", gone)
	}
	r.wantRows("two, edited", "three")
	rows, err := r.e.ClearQueue(cmd(6))
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != two.ID || rows[0].Text != "two, edited" || rows[0].Version != 1 ||
		rows[1].ID != three.ID || rows[1].Text != "three" {
		t.Fatalf("clear answered %+v, want the two rows it removed, in queue order, as they stood", rows)
	}
	r.wantRows()

	got := r.until(func(ev agent.Event) bool {
		return ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueRemoved && ev.Queue.ID == three.ID
	})
	r.wantShapes(got,
		`queue queued "one"`,
		`queue queued "two"`,
		`queue edited "two, edited"`,
		`queue queued "three"`,
		`queue removed "one"`,
		`queue removed "two, edited"`,
		`queue removed "three"`,
	)
	// Every event names the command that caused it, in order.
	wantCauses := []string{"1", "2", "3", "4", "5", "6", "6"}
	for i, ev := range got {
		if want := client + "/" + wantCauses[i]; ev.Cause != want {
			t.Fatalf("event %d (%s) has cause %q, want %q", i, shape(ev), ev.Cause, want)
		}
	}
	r.wantPrompts()
}

// TestTheQueueVerbsNeverStartATurn: Queue is not a second admission path.
// Nothing is sent however idle the engine is, and the rows go when the next
// turn settles — which is what a client's own send is for.
func TestTheQueueVerbsNeverStartATurn(t *testing.T) {
	r := newRig(t, Options{})
	for _, text := range []string{"one", "two"} {
		r.queue(text)
	}
	// Nothing woke the driver, and Sync proves what was enqueued is in the
	// record: if a verb had started a turn the session would have been handed
	// text by now.
	r.sync()
	r.wantPrompts()
	st := r.e.State()
	if st.Activity != ActivityIdle || st.Turn != "" || st.Prompted || len(st.Queue) != 2 {
		t.Fatalf("state after two queue verbs: %+v", st)
	}
	// A send starts a turn and the rows then drain behind it, in order.
	r.submit("sent")
	r.until(lastEnding)
	r.wantPrompts("sent", "one", "two")
}

// TestQueueRefusesAFullQueueAndOversizedText: both refusals leave the queue as
// it was, so the client still has the draft it tried to queue.
func TestQueueRefusesAFullQueueAndOversizedText(t *testing.T) {
	r := newRig(t, Options{})
	for i := 0; i < 32; i++ {
		r.queue(fmt.Sprintf("row-%d", i))
	}
	if _, err := r.e.Queue(Command{}, "one too many"); !errors.Is(err, agent.ErrQueueFull) || Code(err) != "queue_full" {
		t.Fatalf("a 33rd row: %v (%s)", err, Code(err))
	}
	if n := len(r.e.State().Queue); n != 32 {
		t.Fatalf("the refused row changed the queue: %d rows", n)
	}
	long := strings.Repeat("x", (32<<10)+1)
	if _, err := r.e.Queue(Command{}, long); !errors.Is(err, agent.ErrQueueTextTooLong) || Code(err) != "text_too_long" {
		t.Fatalf("an oversized row: %v (%s)", err, Code(err))
	}
	if err := r.e.EditQueued(Command{}, r.e.State().Queue[0].ID, long, nil); !errors.Is(err, agent.ErrQueueTextTooLong) {
		t.Fatalf("an oversized edit: %v", err)
	}
	if got := r.e.State().Queue[0].Text; got != "row-0" {
		t.Fatalf("the refused edit rewrote the row: %q", got)
	}
}

// TestTheQueueVerbsRefuseAnUnknownRow: a row id the queue does not hold — one
// the drain already sent, one another client removed — is its own error, so a
// client knows to re-read the queue rather than to fix its call.
func TestTheQueueVerbsRefuseAnUnknownRow(t *testing.T) {
	r := newRig(t, Options{})
	row := r.queue("one")
	sent, err := r.e.Unqueue(Command{}, row.ID)
	if err != nil {
		t.Fatalf("unqueue: %v", err)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"edit", r.e.EditQueued(Command{}, sent.ID, "again", nil)},
		{"unqueue", errRow(r.e.Unqueue(Command{}, sent.ID))},
		{"submit from a row", errOf(r.e.Submit(Command{}, "", SubmitQueue, sent.ID))},
	} {
		if !errors.Is(tc.err, ErrUnknownRow) || Code(tc.err) != "unknown_row" {
			t.Fatalf("%s of a row that has gone: %v (%s)", tc.name, tc.err, Code(tc.err))
		}
	}
}

// errOf, errRow and errRows are the error half of a two-result verb, so a
// table can hold one call per line.
func errOf(_ SubmitResult, err error) error           { return err }
func errRow(_ agent.QueuedPrompt, err error) error    { return err }
func errRows(_ []agent.QueuedPrompt, err error) error { return err }

// TestEditQueuedChecksTheVersion is A11's pair of first edits. Two clients read
// the same new row — version 0, which is why nil and not zero is the wildcard —
// and both edit it against that version: one wins and the other is told the row
// changed, rather than silently overwriting text it never saw.
func TestEditQueuedChecksTheVersion(t *testing.T) {
	t.Run("sequentially, which pins which error", func(t *testing.T) {
		r := newRig(t, Options{})
		row := r.queue("draft")
		if row.Version != 0 {
			t.Fatalf("a new row is at version %d, want 0", row.Version)
		}
		zero := 0
		if err := r.e.EditQueued(Command{}, row.ID, "client A's text", &zero); err != nil {
			t.Fatalf("the first edit: %v", err)
		}
		err := r.e.EditQueued(Command{}, row.ID, "client B's text", &zero)
		if !errors.Is(err, ErrStaleVersion) || Code(err) != "stale_version" {
			t.Fatalf("the second edit: %v (%s)", err, Code(err))
		}
		r.wantRows("client A's text")
		// nil is unconditional, which is what a client with no version to
		// offer — the TUI, in this phase — passes.
		if err := r.e.EditQueued(Command{}, row.ID, "unconditional", nil); err != nil {
			t.Fatalf("an unconditional edit: %v", err)
		}
		r.wantRows("unconditional")
	})
	t.Run("concurrently, which pins that the check and the edit are one section", func(t *testing.T) {
		r := newRig(t, Options{})
		row := r.queue("draft")
		zero := 0
		errs := make([]error, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = r.e.EditQueued(Command{}, row.ID, fmt.Sprintf("text %d", i), &zero)
			}(i)
		}
		close(start)
		wg.Wait()
		won, lost := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrStaleVersion):
				lost++
			default:
				t.Fatalf("an edit failed some other way: %v", err)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("%d edits won and %d were stale, want one each", won, lost)
		}
		if got := r.e.State().Queue[0].Version; got != 1 {
			t.Fatalf("the row is at version %d, want 1: exactly one edit landed", got)
		}
	})
}

// TestTheQueueVerbsRefuseAStoppedOrClosedEngine: a queue nothing will ever
// drain is not a queue, so the removals are refused with the additions. Stop
// has emptied it by then in any case.
func TestTheQueueVerbsRefuseAStoppedOrClosedEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		shut func(*testing.T, *rig)
	}{
		{"stopped", func(t *testing.T, r *rig) {
			if err := r.e.Stop(context.Background(), Command{}); err != nil {
				t.Fatalf("stop: %v", err)
			}
		}},
		{"closed", func(t *testing.T, r *rig) {
			if err := r.e.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			row := r.queue("before")
			tc.shut(t, r)
			for what, err := range map[string]error{
				"queue":   errRow(r.e.Queue(Command{}, "after")),
				"edit":    r.e.EditQueued(Command{}, row.ID, "after", nil),
				"unqueue": errRow(r.e.Unqueue(Command{}, row.ID)),
				"clear":   errRows(r.e.ClearQueue(Command{})),
				"disarm":  r.e.Disarm(Command{}),
			} {
				if !errors.Is(err, ErrNotAccepting) || Code(err) != "not_accepting" {
					t.Fatalf("%s on a %s engine: %v (%s)", what, tc.name, err, Code(err))
				}
			}
		})
	}
}

// TestTheQueueVerbsReturnWithAFullPrimary is hazard 1 (A4): the caller of a
// queue verb is, in the TUI, the primary's own reader, and a verb that waited
// for a send it would itself have to read to release would wedge the session.
// Every one of them returns with the primary full and nobody reading, and so do
// the other two commands an Update makes — arming a send-now and disarming it.
func TestTheQueueVerbsReturnWithAFullPrimary(t *testing.T) {
	s := newFake(t, agent.EventLogOptions{})
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const primaryCap = 256
	for i := 0; i < primaryCap; i++ {
		s.emit(agent.Event{Type: agent.EventText, Text: "fill"})
	}
	// A turn to arm a send-now against. Its continuation parks on its own Flush,
	// which the full primary never releases, so the turn is claimed and current
	// throughout — and the cancel the arm asks for withdraws it rather than
	// making the session say anything.
	s.script(held())
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 7)
	go func() {
		row, err := e.Queue(Command{}, "one")
		if err != nil {
			t.Errorf("queue: %v", err)
		}
		done <- "queue"
		if err := e.EditQueued(Command{}, row.ID, "one, edited", nil); err != nil {
			t.Errorf("edit: %v", err)
		}
		done <- "edit"
		if _, err := e.Queue(Command{}, "two"); err != nil {
			t.Errorf("queue: %v", err)
		}
		done <- "queue again"
		if _, err := e.Unqueue(Command{}, row.ID); err != nil {
			t.Errorf("unqueue: %v", err)
		}
		done <- "unqueue"
		if _, err := e.ClearQueue(Command{}); err != nil {
			t.Errorf("clear: %v", err)
		}
		done <- "clear"
		if res, err := e.Submit(Command{}, "now", SubmitSendNow, ""); err != nil || !res.Armed {
			t.Errorf("arm: %+v, %v", res, err)
		}
		done <- "arm"
		if err := e.Disarm(Command{}); err != nil {
			t.Errorf("disarm: %v", err)
		}
		done <- "disarm"
	}()
	for i := 0; i < cap(done); i++ {
		select {
		case <-done:
		case <-time.After(watchdog):
			t.Fatalf("a command is waiting on a primary nobody reads (%d of %d returned)", i, cap(done))
		}
	}
}

// TestASaturatedOutboxRefusesTheQueueVerbsAndStillRequeuesSteers is A5's queue
// half: over the bound every rejectable verb refuses having changed nothing,
// while the completions a turn owes — its settlement, and the steers it accepted
// and could not answer — are enqueued all the same.
func TestASaturatedOutboxRefusesTheQueueVerbsAndStillRequeuesSteers(t *testing.T) {
	// A primary nobody reads is what makes the outbox undeliverable: the drainer
	// parks on the first send it cannot make and holds the log's publishing
	// boundary while it does, so everything enqueued behind it stays enqueued.
	// The turn is silent for that reason — a turn that published anything would
	// wait for that same boundary and never come back — and the settlement this
	// test is about therefore happens with the outbox provably over its bound,
	// before anything reads a single event.
	s, e, returned := saturableEngine(t)
	turn := s.script(silently(steering(held(), "steered")))
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	row, err := e.Queue(Command{}, "queued before the flood")
	if err != nil {
		t.Fatal(err)
	}
	saturate(t, e)

	before := e.State()
	for what, err := range map[string]error{
		"queue":   errRow(e.Queue(Command{}, "two")),
		"edit":    e.EditQueued(Command{}, row.ID, "rewritten", nil),
		"unqueue": errRow(e.Unqueue(Command{}, row.ID)),
		"clear":   errRows(e.ClearQueue(Command{})),
		"sendNow": errOf(e.Submit(Command{}, "now", SubmitSendNow, "")),
	} {
		if !errors.Is(err, ErrUnavailable) || Code(err) != "unavailable" {
			t.Fatalf("%s with no room in the outbox: %v (%s)", what, err, Code(err))
		}
	}
	after := e.State()
	if len(after.Queue) != len(before.Queue) || after.Queue[0].Text != before.Queue[0].Text ||
		after.Turn != before.Turn || after.SendNow != nil {
		t.Fatalf("a refused command changed the engine: %+v → %+v", before, after)
	}

	// The turn ends while the outbox is still over its bound. Its settlement is a
	// mandatory completion: the steer is requeued, the successor claimed and the
	// ending enqueued regardless, which is asserted on the engine's own state
	// while nothing has yet been read — the record comes afterwards.
	turn.release()
	awaitTurn(t, returned, "turn-1")
	if e.log.OutboxRoom() {
		t.Fatal("the outbox came back under its bound before the settlement was checked")
	}
	st := e.State()
	if st.Turn != "turn-2" || st.Activity != ActivityWorking {
		t.Fatalf("the settlement under a saturated outbox left: %+v", st)
	}
	// The steer was requeued at the head and taken as the successor, ahead of the
	// row queued before the flood, which is all that is left waiting.
	if len(st.Queue) != 1 || st.Queue[0].Text != "queued before the flood" {
		t.Fatalf("the queue after the settlement: %+v", st.Queue)
	}
	if got := s.prompts(); strings.Join(got, "|") != "one|steered" {
		t.Fatalf("the session was handed %q", got)
	}

	// Only now does anyone read, and everything the settlement enqueued over the
	// bound is in the record.
	r := readerOn(t, s, e)
	got := r.until(ended("turn-1"))
	if last := got[len(got)-1].Turn; last.Next != "turn-2" || last.Pending != 1 {
		t.Fatalf("the ending enqueued under a saturated outbox: %+v", last)
	}
	if !hasShape(got, `queue queued "steered"`) {
		t.Fatalf("the requeue is not in the record: %s", describe(got))
	}
	r.until(lastEnding)
	r.wantPrompts("one", "steered", "queued before the flood")
}

// saturableEngine is an engine whose log has a primary nobody reads: enqueue
// enough and the outbox can no longer deliver anything, which is the state every
// saturation test is about. It reports its continuations, because with nothing
// reading the record a returned turn is the only barrier there is.
//
// One reader is allowed as far as the first turn's started, because a turn's
// first continuation waits for it (runTurn's Flush) and would otherwise never
// open at all.
func saturableEngine(t *testing.T) (*fakeSession, *Engine, <-chan string) {
	t.Helper()
	s := newFake(t, agent.EventLogOptions{})
	returned := make(chan string, 32)
	e, err := newEngine(s, Options{}, &hooks{turnReturned: func(id string) { returned <- id }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	go func() {
		for ev := range e.Events() {
			if started("turn-1")(ev) {
				return
			}
		}
	}()
	return s, e, returned
}

// saturate fills the outbox past its soft bound.
func saturate(t *testing.T, e *Engine) {
	t.Helper()
	filler := make([]agent.Event, 512)
	for i := range filler {
		filler[i] = agent.Event{Type: agent.EventText, Text: fmt.Sprintf("fill-%d", i)}
	}
	for e.log.OutboxRoom() {
		e.log.Enqueue(filler...)
	}
}

// readerOn starts reading a saturated log: a budgeted subscription for the
// record, and a goroutine on the primary to unwedge the drainer. The
// subscription is opened after that goroutine because Subscribe waits for the
// publishing boundary, which the parked drainer holds.
func readerOn(t *testing.T, s *fakeSession, e *Engine) *rig {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case <-e.Events():
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })
	sub, err := e.Subscribe(agent.SubscribeOptions{MaxItems: 1 << 16, MaxBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	return &rig{t: t, s: s, e: e, sub: sub}
}

// hasShape reports whether one of evs has the given shape.
func hasShape(evs []agent.Event, want string) bool {
	for _, ev := range evs {
		if shape(ev) == want {
			return true
		}
	}
	return false
}

// TestUnansweredSteersAreRequeuedLastFirstAheadOfTheQueue is A11's three rules
// at once: the steers go back through PushFront, ahead of everything already
// waiting; last first, so they end up in the order they were typed and a
// consumer replaying the events lands on the same order; and they are committed
// before the successor is decided, so the first of them is what runs next
// rather than the row that was queued behind the turn.
func TestUnansweredSteersAreRequeuedLastFirstAheadOfTheQueue(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(steering(held(), "steer one", "steer two"))
	// The successor is held too, so the queue can be read while it runs rather
	// than after it has drained the next row.
	successor := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.queue("queued")
	r.sync()
	turn.release()
	got := r.until(started("turn-2"))
	r.wantShapes(got,
		`started turn-1 submit "one"`,
		`queue queued "queued"`,
		`text "echo: one"`,
		`done end_turn`,
		// Last first, each an insert at position 0: the queue ends up
		// [steer one, steer two, queued].
		`queue queued "steer two"`,
		`queue queued "steer one"`,
		`queue sent "steer one"`,
		`ended turn-1 stop="end_turn" next="turn-2" pending=2`,
		`started turn-2 drain "steer one"`,
	)
	await(t, successor.opened, "the successor to open")
	r.wantRows("steer two", "queued")
	successor.release()
	r.until(lastEnding)
	r.wantPrompts("one", "steer one", "steer two", "queued")
}

// TestARequeuedSteerIsNotBoundedByTheQueuesCap: PushFront is the one way in
// that neither cap bounds, because the text is already the user's — a turn
// accepted it — and a cap must not be the reason it vanishes when it has
// nowhere else to go.
func TestARequeuedSteerIsNotBoundedByTheQueuesCap(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(steering(held(), "steered"))
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	for i := 0; i < 32; i++ {
		r.queue(fmt.Sprintf("row-%d", i))
	}
	if _, err := r.e.Queue(Command{}, "one too many"); !errors.Is(err, agent.ErrQueueFull) {
		t.Fatalf("the queue is not full: %v", err)
	}
	r.sync()
	turn.release()
	got := r.until(ended("turn-1"))
	last := got[len(got)-1].Turn
	if last.Next != "turn-2" || last.Pending != 32 {
		t.Fatalf("the ending after a requeue into a full queue: %+v", last)
	}
	if got := r.until(started("turn-2")); got[len(got)-1].Turn.Text != "steered" {
		t.Fatalf("the successor is %q, want the steer", got[len(got)-1].Turn.Text)
	}
}

// TestAFailedTurnRequeuesItsSteersThenClearsThem: the requeue runs on the error
// path too, before the clear, so the removals for the requeued rows are in the
// record — which is what lets a client say the queue was emptied when steers
// were all that was in it.
func TestAFailedTurnRequeuesItsSteersThenClearsThem(t *testing.T) {
	r := newRig(t, Options{})
	boom := errors.New("the turn failed")
	turn := r.s.script(failing(steering(held(), "steered"), boom))
	r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.sync()
	turn.release()
	got := r.until(ended("turn-1"))
	r.wantShapes(got,
		`started turn-1 submit "one"`,
		`error`,
		`queue queued "steered"`,
		`queue removed "steered"`,
		`ended turn-1 stop="" next="" pending=0 class=other`,
	)
	if st := r.e.State(); st.Activity != ActivityError || len(st.Queue) != 0 {
		t.Fatalf("state after a failed turn with a steer: %+v", st)
	}
}
