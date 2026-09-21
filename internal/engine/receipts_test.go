package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// A16, row by row (plan 021 §3.8, C11). Every test here uses barriers; the
// watchdog is a deadlock backstop, never a timing assertion, and the two
// clock-based tests use an injected clock rather than a real sleep.

// testClock is a clock a test can move forward without racing the engine's
// own goroutines, which read it independently of the test's: a plain
// closure over a mutable time.Time would be a data race under -race the
// moment the driver or the settings worker reads it while the test writes.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock(start time.Time) *testClock { return &testClock{t: start} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newClockedRig is newRig with the fakeSession's clock replaced before the
// engine is built, so State().RetryHorizon's age bound can be proven without
// a real sleep.
func newClockedRig(t *testing.T, opts Options, clock func() time.Time) *rig {
	t.Helper()
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	s.clock = clock
	e, err := newEngine(s, opts, nil)
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

// wantNoMoreAfterSync calls Sync, which guarantees anything the most recent
// call enqueued is now in the record's buffer, then asserts nothing new is
// there: a resend's whole point is that it enqueued nothing at all.
func (r *rig) wantNoMoreAfterSync() {
	r.t.Helper()
	r.sync()
	select {
	case rec, ok := <-r.sub.Records():
		if !ok {
			return
		}
		ev, err := rec.Event()
		if err != nil {
			r.t.Fatal(err)
		}
		r.t.Fatalf("a resend enqueued another event: %s", shape(ev))
	default:
	}
}

// Two clients numbering their own commands from 1 never collide: the table
// is keyed by (client, id), never by id alone.
func TestTwoClientsBothNumberingFromOneNeverCollide(t *testing.T) {
	r := newRig(t, Options{})
	a, b := r.e.NewClientID(), r.e.NewClientID()
	if a == b {
		t.Fatalf("NewClientID minted the same id twice: %q", a)
	}
	rowA, err := r.e.Queue(Command{Client: a, ID: "1"}, "from a")
	if err != nil {
		t.Fatal(err)
	}
	rowB, err := r.e.Queue(Command{Client: b, ID: "1"}, "from b")
	if err != nil {
		t.Fatal(err)
	}
	if rowA.ID == rowB.ID {
		t.Fatalf("two different clients' id-1 commands collided into one row: %+v / %+v", rowA, rowB)
	}
	r.wantRows("from a", "from b")
}

// A resent command id returns the first result and does not re-execute, for
// each of a turn's started, a queue row, and a delta: the three shapes A16
// names. Each section uses its own command id so the three do not interfere.
func TestResentCommandReturnsTheFirstResultAndDoesNotReexecute(t *testing.T) {
	r := newRig(t, Options{})

	// EventTurn{started}: a re-execution would either be refused
	// (ErrPromptInFlight, since a turn is already running) or — far worse —
	// silently start a second one.
	turn := r.s.script(held())
	c1 := Command{Client: "c-1", ID: "1"}
	first, err := r.e.Submit(c1, "hello", SubmitQueue, "")
	if err != nil {
		t.Fatal(err)
	}
	r.until(started("turn-1"))
	second, err := r.e.Submit(c1, "hello", SubmitQueue, "")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("a resent Submit returned %+v, want the first's %+v", second, first)
	}
	r.wantNoMoreAfterSync()
	r.wantPrompts("hello")

	// A queue row.
	c2 := Command{Client: "c-1", ID: "2"}
	rowFirst, err := r.e.Queue(c2, "queued text")
	if err != nil {
		t.Fatal(err)
	}
	r.next() // "queue queued"
	rowSecond, err := r.e.Queue(c2, "queued text")
	if err != nil {
		t.Fatal(err)
	}
	if rowSecond != rowFirst {
		t.Fatalf("a resent Queue returned %+v, want the first's %+v", rowSecond, rowFirst)
	}
	r.wantNoMoreAfterSync()
	r.wantRows("queued text")

	// A delta: SetTitle's.
	c3 := Command{Client: "c-1", ID: "3"}
	if err := r.e.SetTitle(c3, "New Title"); err != nil {
		t.Fatal(err)
	}
	r.next() // the title delta
	if err := r.e.SetTitle(c3, "New Title"); err != nil {
		t.Fatalf("the resend: %v", err)
	}
	r.wantNoMoreAfterSync()

	turn.release()
	r.until(lastEnding)
}

// A changed payload under the same command id is ErrBadRequest, never a
// re-execution: the row it names is exactly what the first call left it as.
func TestAChangedPayloadIsErrBadRequest(t *testing.T) {
	r := newRig(t, Options{})
	c := Command{Client: "c-1", ID: "1"}
	if _, err := r.e.Queue(c, "first text"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Queue(c, "different text"); !errors.Is(err, ErrBadRequest) || Code(err) != "bad_request" {
		t.Fatalf("a resend with a different payload: %v (%s), want ErrBadRequest", err, Code(err))
	}
	r.wantRows("first text")
}

// EditQueued's expectedVersion is a pointer; two calls with equal VALUES —
// never equal addresses, which change on every call — must hash the same, or
// the "resend the identical edit" case this test drives would spuriously
// collide with "a changed payload" instead.
func TestEditQueuedVersionPointerIdentityDoesNotLeakIntoTheHash(t *testing.T) {
	r := newRig(t, Options{})
	row, err := r.e.Queue(Command{}, "original")
	if err != nil {
		t.Fatal(err)
	}
	c := Command{Client: "c-1", ID: "1"}
	v1 := 0
	if err := r.e.EditQueued(c, row.ID, "edited", &v1); err != nil {
		t.Fatal(err)
	}
	// A fresh *int holding the SAME value, at a different address.
	v2 := 0
	if err := r.e.EditQueued(c, row.ID, "edited", &v2); err != nil {
		t.Fatalf("a resend whose pointer moved but whose value did not: %v", err)
	}
}

// An id the table's count bound has evicted is ErrUnknownCommand — never
// treated as unseen, which would silently run whatever the resend carries —
// and an id above the client's high-water mark that was never seen executes,
// because a client's own pre-minted, never-sent ids are supposed to leave
// gaps (plan 021 X18/X22/X25).
func TestAnEvictedIDByCountIsErrUnknownCommand(t *testing.T) {
	r := newRig(t, Options{})
	client := "c-1"
	for i := 1; i <= receiptCap+1; i++ {
		c := Command{Client: client, ID: strconv.Itoa(i)}
		if err := r.e.SetTitle(c, fmt.Sprintf("title-%d", i)); err != nil {
			t.Fatalf("SetTitle %d: %v", i, err)
		}
	}
	if err := r.e.SetTitle(Command{Client: client, ID: "1"}, "title-1"); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("a resend past the count bound: %v, want ErrUnknownCommand", err)
	}
	// The newest is still answerable from the table: a resend of it with its
	// own payload, unmutated.
	last := Command{Client: client, ID: strconv.Itoa(receiptCap + 1)}
	if err := r.e.SetTitle(last, fmt.Sprintf("title-%d", receiptCap+1)); err != nil {
		t.Fatalf("a resend still inside the bound: %v", err)
	}
	// Never seen, and above the mark: a fresh command, not an unknown one.
	if err := r.e.SetTitle(Command{Client: client, ID: "5000"}, "brand new"); err != nil {
		t.Fatalf("a never-seen id above the mark: %v, want it to execute", err)
	}
}

// The age half of the same bound, with an injected clock: a wall-clock sleep
// would make this test slower than the ten minutes it is proving, and this
// repo's tests use barriers and injected clocks instead.
func TestAnEvictedIDByAgeIsErrUnknownCommand(t *testing.T) {
	clock := newTestClock(time.Unix(1_700_000_000, 0).UTC())
	r := newClockedRig(t, Options{}, clock.now)
	c := Command{Client: "c-1", ID: "1"}
	if err := r.e.SetTitle(c, "old title"); err != nil {
		t.Fatal(err)
	}
	clock.advance(receiptAge + time.Second)
	// Every table touch prunes by age first (evictLocked), so this resend's
	// own lookup is what notices id 1 has aged out.
	if err := r.e.SetTitle(c, "old title"); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("a resend past the age bound: %v, want ErrUnknownCommand", err)
	}
}

// A duplicate of a blocking command (Cancel here; Stop, Set and Interject go
// through the identical mechanism, withBlockingReceipt) parks on the first's
// reservation and gets exactly its result; a duplicate whose OWN context ends
// first returns that context's error and disturbs nothing — the first is
// still parked at the session's door when this is checked.
func TestADuplicateOfABlockingCommandWaitsForTheFirst(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	entered, release := r.s.holdNextCancel()
	c := Command{Client: "c-1", ID: "1"}
	firstRes := make(chan CancelResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		res, err := r.e.Cancel(context.Background(), c, "turn-1")
		firstRes <- res
		firstErr <- err
	}()
	await(t, entered, "the first cancel to reach the session")

	// A duplicate whose own context has already ended returns that error at
	// once, without touching the first's reservation.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.e.Cancel(expired, c, "turn-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a duplicate whose own ctx ended: %v, want context.Canceled", err)
	}
	if st := r.e.State(); st.Turn != "turn-1" {
		t.Fatalf("the first cancel's hold was disturbed: %+v", st)
	}
	if n := r.s.cancelsWritten(); n != 0 {
		t.Fatalf("%d cancels reached the session; the first is still parked at its door", n)
	}

	// A duplicate that waits gets exactly the first's result, whenever the
	// first returns.
	dupRes := make(chan CancelResult, 1)
	dupErr := make(chan error, 1)
	go func() {
		res, err := r.e.Cancel(context.Background(), c, "turn-1")
		dupRes <- res
		dupErr <- err
	}()
	release()

	want, err := <-firstRes, <-firstErr
	if err != nil {
		t.Fatalf("the first cancel: %v", err)
	}
	select {
	case got := <-dupRes:
		if err := <-dupErr; err != nil {
			t.Fatalf("the duplicate: %v", err)
		}
		if got != want {
			t.Fatalf("the duplicate got %+v, want the first's %+v", got, want)
		}
	case <-time.After(watchdog):
		t.Fatal("the duplicate never returned")
	}
	if n := r.s.cancelsWritten(); n != 1 {
		t.Fatalf("the session saw %d cancels, want exactly 1: the duplicate must not re-execute", n)
	}

	turn.release()
	r.until(lastEnding)
}

// A duplicate of a synchronous command never blocks, even called from the
// one goroutine that would otherwise have to drain a full primary to unblock
// anything (readerOn's own doc names this hazard): it is answered from the
// table with no Enqueue and no wait of any kind.
func TestADuplicateOfASynchronousCommandNeverBlocksWithTheOutboxFull(t *testing.T) {
	s, e, returned := saturableEngine(t)
	// Silent: a turn that published anything would itself wait on the same
	// saturated primary and never come back (queue_test.go's saturableEngine
	// doc makes the same point).
	turn := s.script(silently(held()))
	c := Command{Client: "c-1", ID: "1"}
	first, err := e.Submit(c, "one", SubmitQueue, "")
	if err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	saturate(t, e)

	done := make(chan struct{})
	var second SubmitResult
	var err2 error
	go func() {
		// Standing in for "the primary's reader, inside the call": were this
		// resend to Enqueue anything, or wait on the outbox in any way, this
		// is the only goroutine that could ever have drained it.
		second, err2 = e.Submit(c, "one", SubmitQueue, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatal("a duplicate of a synchronous command blocked with the outbox full")
	}
	if err2 != nil || second != first {
		t.Fatalf("the duplicate returned %+v, %v, want the first's %+v", second, err2, first)
	}

	turn.release()
	awaitTurn(t, returned, "turn-1")
}

// The zero Command skips the table entirely: two calls that carry it can
// never collide, resend, or wait on one another, exactly craze prompt's
// behaviour today (it never mints a client id at all).
func TestTheZeroCommandSkipsTheTable(t *testing.T) {
	r := newRig(t, Options{})
	if _, err := r.e.Queue(Command{}, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Queue(Command{}, "second"); err != nil {
		t.Fatal(err)
	}
	r.wantRows("first", "second")
}

// State's RetryHorizon is exactly the table's own bound.
func TestRetryHorizonInState(t *testing.T) {
	r := newRig(t, Options{})
	got := r.e.State().RetryHorizon
	want := RetryHorizon{Commands: receiptCap, Age: receiptAge}
	if got != want {
		t.Fatalf("RetryHorizon is %+v, want %+v", got, want)
	}
}

// A gate refusal — the engine simply not admitting anything right now, this
// command's own arguments aside — is never stored: a synchronous command's
// never reserves an entry for one, so a later call with the same id, once
// the gate has reopened, gets a fresh attempt rather than a cached refusal.
func TestASyncCommandsGateRefusalIsNotStored(t *testing.T) {
	s, e, returned := saturableEngine(t)
	turn := s.script(silently(held())) // see the identical note above
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	saturate(t, e)

	c := Command{Client: "c-1", ID: "1"}
	if _, err := e.Queue(c, "queue me"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("queue with no room: %v, want ErrUnavailable", err)
	}
	e.receipts.mu.Lock()
	_, reserved := e.receipts.byKey[receiptKey{client: "c-1", id: 1}]
	e.receipts.mu.Unlock()
	if reserved {
		t.Fatal("a gate refusal reserved an entry: a resend once room returns would be told the same stale refusal, never a fresh attempt")
	}

	turn.release()
	awaitTurn(t, returned, "turn-1")
}

// The blocking path's identical rule: Cancel's own "nothing to cancel" is
// ErrNotAccepting too (the same gate sentinel — see ErrNotAccepting's own
// doc), and the SAME id with the SAME payload, resent once there is
// something to cancel, goes ahead rather than replaying the refusal.
func TestABlockingCommandsGateRefusalIsNotStored(t *testing.T) {
	r := newRig(t, Options{})
	c := Command{Client: "c-1", ID: "1"}
	if _, err := r.e.Cancel(context.Background(), c, ""); !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("a cancel with nothing to cancel: %v, want ErrNotAccepting", err)
	}
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	res, err := r.e.Cancel(context.Background(), c, "")
	if err != nil {
		t.Fatalf("the resend, now that there is something to cancel: %v", err)
	}
	if res.Turn != "turn-1" {
		t.Fatalf("the resend cancelled %q, want turn-1", res.Turn)
	}

	turn.release()
	r.until(lastEnding)
}
