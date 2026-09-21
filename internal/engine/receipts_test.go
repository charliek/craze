package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// A16, row by row (plan 021 §3.8, C11), and r24's "untested schedules". Every
// test here uses barriers; the watchdog is a deadlock backstop, never a timing
// assertion, and every clock-based test uses the table's injected clock rather
// than a real sleep.
//
// The schedules that are the TABLE's own — a panic on the way through a
// command, an open reservation crossing both bounds, five thousand clients,
// ids arriving out of order — drive receiptTable directly, with a run function
// of the test's: putting them through a Control method would pin the method's
// behaviour and leave the mechanism itself asserted only by implication.

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

func (c *testClock) advance(d time.Duration) { c.set(c.now().Add(d)) }

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// epoch is the fixed instant every clocked test starts from.
func epoch() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

// newClockedRig is newRig with the RECEIPTS table's own clock replaced before
// the engine is built: a receipt's lifetime is real elapsed time and not the
// session's event clock (r24 finding 6), so this is the only way to prove the
// age bound without a ten-minute sleep.
func newClockedRig(t *testing.T, opts Options, clock func() time.Time) *rig {
	t.Helper()
	return newRigHooked(t, opts, agent.EventLogOptions{NoPrimary: true},
		&hooks{receipts: &receiptHooks{now: clock}})
}

// okRun and failRun are a table-level command's two shapes, and neverRun is
// one the test says must not be entered at all: a duplicate that re-executes
// is the bug every test in this file is about.
func okRun(res string) func() (string, error) {
	return func() (string, error) { return res, nil }
}

func neverRun(t *testing.T, what string) func() (string, error) {
	return func() (string, error) {
		t.Errorf("%s re-executed", what)
		return "re-executed", nil
	}
}

// overlap counts a command's executions and remembers the most that ever ran
// at once: "at most one execution at a time" is what a reservation buys, and
// counting the runs alone would not show it.
type overlap struct {
	mu   sync.Mutex
	now  int
	most int
	runs int
}

func (o *overlap) enter() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.now++
	o.runs++
	if o.now > o.most {
		o.most = o.now
	}
}

func (o *overlap) exit() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.now--
}

func (o *overlap) seen() (most, runs int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.most, o.runs
}

// awaitHook waits for n firings of a table hook's barrier.
func awaitHook(t *testing.T, ch <-chan struct{}, n int, what string) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(watchdog):
			t.Fatalf("only %d of %d %s after %s", i, n, what, watchdog)
		}
	}
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
	client := r.e.NewClientID()

	// EventTurn{started}: a re-execution would either be refused
	// (ErrPromptInFlight, since a turn is already running) or — far worse —
	// silently start a second one.
	turn := r.s.script(held())
	c1 := Command{Client: client, ID: "1"}
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
	c2 := Command{Client: client, ID: "2"}
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
	c3 := Command{Client: client, ID: "3"}
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
	c := Command{Client: r.e.NewClientID(), ID: "1"}
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
	c := Command{Client: r.e.NewClientID(), ID: "1"}
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
	client := r.e.NewClientID()
	for i := 1; i <= receiptCap+1; i++ {
		c := Command{Client: client, ID: strconv.Itoa(i)}
		if err := r.e.SetTitle(c, fmt.Sprintf("title-%d", i)); err != nil {
			t.Fatalf("SetTitle %d: %v", i, err)
		}
	}
	// The cap holds the moment the one past it is stored, not at the next
	// touch (r24 finding 8).
	r.e.receipts.mu.Lock()
	n := len(r.e.receipts.order)
	r.e.receipts.mu.Unlock()
	if n != receiptCap {
		t.Fatalf("the table holds %d completed receipts, want the cap of %d", n, receiptCap)
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

// The age half of the same bound, with the table's injected clock: a
// wall-clock sleep would make this test slower than the ten minutes it is
// proving, and this repo's tests use barriers and injected clocks instead.
func TestAnEvictedIDByAgeIsErrUnknownCommand(t *testing.T) {
	clock := newTestClock(epoch())
	r := newClockedRig(t, Options{}, clock.now)
	c := Command{Client: r.e.NewClientID(), ID: "1"}
	if err := r.e.SetTitle(c, "old title"); err != nil {
		t.Fatal(err)
	}
	clock.advance(receiptAge + time.Second)
	// Every table touch prunes by age first (evictAgedLocked), so this resend's
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
//
// The barrier is the table's own hook: it fires once a duplicate has FOUND the
// open reservation, and the first is released only after it, so "the duplicate
// waited" is forced rather than hoped for — without it the scheduler could run
// the duplicate after the first had already completed, and the test would pass
// having exercised a replay (r24 finding 9).
func TestADuplicateOfABlockingCommandWaitsForTheFirst(t *testing.T) {
	found := make(chan struct{}, 4)
	r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true},
		&hooks{receipts: &receiptHooks{foundOpen: func() { found <- struct{}{} }}})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	entered, release := r.s.holdNextCancel()
	c := Command{Client: r.e.NewClientID(), ID: "1"}
	firstRes := make(chan CancelResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		res, err := r.e.Cancel(context.Background(), c, "turn-1")
		firstRes <- res
		firstErr <- err
	}()
	await(t, entered, "the first cancel to reach the session")

	// A duplicate whose own context has already ended returns at once, without
	// touching the first's reservation — and what it returns is
	// ErrCommandInProgress wrapping that context's error (r31 finding 2). This
	// invocation ran nothing and the owner's reservation is still open, so the
	// instruction is in_progress's "resend THE SAME id"; a bare context error
	// would code `aborted`, which says the opposite — that the command may
	// already have happened and a NEW id is what another attempt needs — and a
	// new id here would layer a second cancel on top of the one still running.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.e.Cancel(expired, c, "turn-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a duplicate whose own ctx ended: %v, want the context error still matchable", err)
	}
	if !errors.Is(err, ErrCommandInProgress) || Code(err) != "in_progress" {
		t.Fatalf("a duplicate whose own ctx ended: %v (%s), want ErrCommandInProgress coded in_progress", err, Code(err))
	}
	awaitHook(t, found, 1, "duplicates to find the open reservation")
	if st := r.e.State(); st.Turn != "turn-1" {
		t.Fatalf("the first cancel's hold was disturbed: %+v", st)
	}
	if n := r.s.cancelsWritten(); n != 0 {
		t.Fatalf("%d cancels reached the session; the first is still parked at its door", n)
	}

	// A duplicate that waits gets exactly the first's result, whenever the
	// first returns. It is provably parked before the first is released.
	dupRes := make(chan CancelResult, 1)
	dupErr := make(chan error, 1)
	go func() {
		res, err := r.e.Cancel(context.Background(), c, "turn-1")
		dupRes <- res
		dupErr <- err
	}()
	awaitHook(t, found, 1, "the waiting duplicate to find the open reservation")
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

// A COMPLETED blocking receipt replays AT ONCE, whatever the duplicate's own
// ctx: withBlockingReceipt only calls waitReceipt for an OPEN reservation,
// never a completed one, so a duplicate whose ctx has ALREADY ended cannot
// race context.Canceled against the stored result in waitReceipt's select
// (before this fix, both r.done and ctx.Done() would be closed, and Go's
// select would nondeterministically return either one). Two hundred
// iterations, so a flake would not survive one lucky run (r26 finding 3).
func TestACompletedBlockingReceiptReplaysWhateverTheDuplicatesCtx(t *testing.T) {
	rt := newReceiptTable(nil)
	c := Command{Client: rt.newClient(), ID: "1"}
	hash := receiptHash("Cancel", "turn-1")
	want, err := withBlockingReceipt(context.Background(), rt, c, hash, okRun("cancelled"))
	if err != nil || want != "cancelled" {
		t.Fatalf("the first call: %q, %v", want, err)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 200; i++ {
		got, err := withBlockingReceipt(expired, rt, c, hash, neverRun(t, "the resend"))
		if err != nil || got != "cancelled" {
			t.Fatalf("iteration %d: %q, %v; want the stored result despite an already-canceled ctx", i, got, err)
		}
	}
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
	c := Command{Client: e.NewClientID(), ID: "1"}
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

// A duplicate of a synchronous command that finds the first still RUNNING is
// refused at once — ErrCommandInProgress, "resend the SAME id" — and never
// waits, which is what keeps "waits on nothing" true once C12 puts an index
// write inside SetTitle and Submit (r24 finding 5). Its code is "in_progress",
// distinct from a gate refusal's "unavailable" without matching text, because
// the two mean different things a client must tell apart (r26 finding 1). Two
// things are proven while the first is parked inside the session: the
// duplicate comes straight back with ErrCommandInProgress, and the table's
// mutex is free, so another client's command — an Answer, on the goroutine
// that would be the primary's reader — completes meanwhile.
func TestASyncDuplicateOfARunningCommandIsRefusedNotBlocked(t *testing.T) {
	found := make(chan struct{}, 4)
	r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true},
		&hooks{receipts: &receiptHooks{foundOpen: func() { found <- struct{}{} }}})
	ask := r.s.openAsk(t)
	entered, release := r.s.holdNextTitle()

	a := Command{Client: r.e.NewClientID(), ID: "1"}
	firstErr := make(chan error, 1)
	go func() { firstErr <- r.e.SetTitle(a, "renamed") }()
	await(t, entered, "the first SetTitle to reach the session")

	dup := make(chan error, 1)
	go func() { dup <- r.e.SetTitle(a, "renamed") }()
	select {
	case err := <-dup:
		if !errors.Is(err, ErrCommandInProgress) || Code(err) != "in_progress" {
			t.Fatalf("a duplicate of a running synchronous command: %v (%s), want ErrCommandInProgress", err, Code(err))
		}
	case <-time.After(watchdog):
		t.Fatal("a duplicate of a running synchronous command waited for it")
	}
	awaitHook(t, found, 1, "the duplicate to find the open reservation")

	// The table's mutex is not held across the first command, so another
	// client's id-carrying command goes through while it is parked.
	b := Command{Client: r.e.NewClientID(), ID: "1"}
	other := make(chan error, 1)
	go func() { other <- r.e.Answer(b, ask.ID(), agent.AskAnswer{Cancel: true}) }()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("another client's Answer while a sync command is parked: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("another client's Answer waited on the receipts table")
	}

	release()
	if err := <-firstErr; err != nil {
		t.Fatalf("the first SetTitle: %v", err)
	}
	// Completed now, so the same id replays rather than being refused, and the
	// session is renamed exactly once.
	if err := r.e.SetTitle(a, "renamed"); err != nil {
		t.Fatalf("the resend once the first completed: %v", err)
	}
	if got := r.e.State().Title; got != "renamed" {
		t.Fatalf("the title is %q, want %q", got, "renamed")
	}
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

// A malformed command id is ErrBadRequest and mutates nothing — including
// every non-canonical spelling of a number that would otherwise parse, which
// would alias two causes onto one entry (r24 finding 7) — and so is a client
// id this engine never minted, because a mark can only be kept for a client
// the table knows (r24 finding 1).
func TestAMalformedCommandIsBadRequestAndMutatesNothing(t *testing.T) {
	r := newRig(t, Options{})
	client := r.e.NewClientID()
	for _, id := range []string{"", "0", "00", "01", "+1", "-1", " 1", "1 ", "1_0", "0x1", "1.0", "abc", "18446744073709551616"} {
		err := r.e.SetTitle(Command{Client: client, ID: id}, "renamed")
		if !errors.Is(err, ErrBadRequest) || Code(err) != "bad_request" {
			t.Fatalf("command id %q: %v (%s), want ErrBadRequest", id, err, Code(err))
		}
	}
	for _, c := range []Command{
		{Client: "not-minted", ID: "1"},
		{Client: clientName(9999), ID: "1"}, // this table's own spelling, never minted
		{Client: "", ID: "1"},
	} {
		if err := r.e.SetTitle(c, "renamed"); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("client %q: %v, want ErrBadRequest", c.Client, err)
		}
	}
	if got := r.e.State().Title; got != "" {
		t.Fatalf("a refused command renamed the session to %q", got)
	}
	r.wantNoMoreAfterSync()
}

// A gate refusal — the engine simply not admitting anything right now, this
// command's own arguments aside — is never stored: a synchronous command's
// reservation is forgotten on the way out, so a later call with the same id,
// once the gate has reopened, gets a fresh attempt rather than a cached
// refusal.
func TestASyncCommandsGateRefusalIsNotStored(t *testing.T) {
	s, e, returned := saturableEngine(t)
	turn := s.script(silently(held())) // see the identical note above
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	await(t, turn.opened, "the turn to open")
	saturate(t, e)

	client := e.NewClientID()
	c := Command{Client: client, ID: "1"}
	if _, err := e.Queue(c, "queue me"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("queue with no room: %v, want ErrUnavailable", err)
	}
	e.receipts.mu.Lock()
	_, reserved := e.receipts.byKey[receiptKey{client: client, id: 1}]
	stored := len(e.receipts.order)
	e.receipts.mu.Unlock()
	if reserved {
		t.Fatal("a gate refusal reserved an entry: a resend once room returns would be told the same stale refusal, never a fresh attempt")
	}
	if stored != 0 {
		t.Fatalf("a gate refusal stored %d receipts", stored)
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
	c := Command{Client: r.e.NewClientID(), ID: "1"}
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

// A replayed SubmitResult is the caller's own: the queue row it carries is
// copied on the way in and on every way out, so a client that writes through
// the pointer it was handed changes neither the queue nor the next replay.
func TestAReplayedSubmitResultIsEachCallersOwn(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	c := Command{Client: r.e.NewClientID(), ID: "1"}
	first, err := r.e.Submit(c, "queued", SubmitQueue, "")
	if err != nil || first.Queued == nil {
		t.Fatalf("Submit behind a running turn: %+v, %v; want a queued row", first, err)
	}
	first.Queued.Text = "MUTATED"

	second, err := r.e.Submit(c, "queued", SubmitQueue, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Queued == nil || second.Queued.Text != "queued" {
		t.Fatalf("the replay carries %+v: a caller's mutation reached the table", second.Queued)
	}
	if second.Queued == first.Queued {
		t.Fatal("two callers were handed the same row pointer")
	}
	r.wantRows("queued")

	turn.release()
	r.until(lastEnding)
}

// An OPEN reservation is in no eviction order at all: it is what a duplicate
// has to find, and a gate refusal can only leave the id retryable if the entry
// is still there to forget. This holds one open across BOTH bounds — ten
// minutes and a thousand-and-one other results — and then takes it through the
// whole of its life: the refusal its waiter gets, the fresh attempt the id is
// still good for, and the replay that attempt leaves behind (r24 finding 2).
func TestAnOpenReservationOutlivesBothBounds(t *testing.T) {
	clock := newTestClock(epoch())
	found := make(chan struct{}, 4)
	rt := newReceiptTable(&receiptHooks{now: clock.now, foundOpen: func() { found <- struct{}{} }})
	owner, other := rt.newClient(), rt.newClient()
	c := Command{Client: owner, ID: "1"}
	hash := receiptHash("Cancel", "turn-1")

	reserved, release := make(chan struct{}), make(chan struct{})
	refused := make(chan error, 1)
	go func() {
		_, err := withBlockingReceipt(context.Background(), rt, c, hash, func() (string, error) {
			close(reserved)
			<-release
			return "", ErrNotAccepting // a gate refusal, after a very long call
		})
		refused <- err
	}()
	await(t, reserved, "the reservation")

	// Both bounds pass it by while it is open.
	clock.advance(receiptAge + time.Minute)
	for i := 1; i <= receiptCap+1; i++ {
		id := Command{Client: other, ID: strconv.Itoa(i)}
		if _, err := withSyncReceipt(rt, id, receiptHash("SetTitle", "t"), okRun("titled")); err != nil {
			t.Fatalf("filling the table: %v", err)
		}
	}

	// Still discoverable: a duplicate finds the reservation, not the
	// ErrUnknownCommand an evicted entry's mark would have left.
	dup := make(chan error, 1)
	go func() {
		_, err := withBlockingReceipt(context.Background(), rt, c, hash, neverRun(t, "the duplicate"))
		dup <- err
	}()
	awaitHook(t, found, 1, "the duplicate to find the open reservation")
	close(release)

	if err := <-refused; !errors.Is(err, ErrNotAccepting) {
		t.Fatalf("the owner: %v, want ErrNotAccepting", err)
	}
	select {
	case err := <-dup:
		if !errors.Is(err, ErrNotAccepting) {
			t.Fatalf("the waiting duplicate: %v, want the owner's refusal", err)
		}
	case <-time.After(watchdog):
		t.Fatal("the waiting duplicate was never answered")
	}

	// The refusal left the id exactly as unseen as before, so it is good for a
	// genuine attempt — and that attempt is what every later resend replays.
	got, err := withBlockingReceipt(context.Background(), rt, c, hash, okRun("cancelled"))
	if err != nil || got != "cancelled" {
		t.Fatalf("the fresh attempt: %q, %v", got, err)
	}
	again, err := withBlockingReceipt(context.Background(), rt, c, hash, neverRun(t, "the resend"))
	if err != nil || again != "cancelled" {
		t.Fatalf("the resend: %q, %v; want the attempt's own answer", again, err)
	}
}

// A command that PANICS completes its reservation on the way out: the
// duplicates already parked on it are answered with a stored
// ErrCommandAborted rather than left waiting for a channel nobody will ever
// close, and the panic still reaches the owner's caller. The outcome is
// stored, not forgotten — the command may already have mutated state, so the
// id must never run again (r24 finding 3).
func TestAnOwnersPanicCompletesItsReservation(t *testing.T) {
	t.Run("a blocking command", func(t *testing.T) {
		found := make(chan struct{}, 4)
		rt := newReceiptTable(&receiptHooks{foundOpen: func() { found <- struct{}{} }})
		c := Command{Client: rt.newClient(), ID: "1"}
		hash := receiptHash("Cancel", "turn-1")

		reserved, parked := make(chan struct{}), make(chan struct{})
		panicked := make(chan struct{})
		go func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not reach the owner's caller")
				}
				close(panicked)
			}()
			_, _ = withBlockingReceipt(context.Background(), rt, c, hash, func() (string, error) {
				close(reserved)
				<-parked
				panic("a command that panics")
			})
		}()
		await(t, reserved, "the reservation")

		dups := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				_, err := withBlockingReceipt(context.Background(), rt, c, hash, neverRun(t, "a duplicate"))
				dups <- err
			}()
		}
		awaitHook(t, found, 2, "duplicates to find the open reservation")
		close(parked)
		await(t, panicked, "the panic to reach the owner's caller")

		for i := 0; i < 2; i++ {
			select {
			case err := <-dups:
				if !errors.Is(err, ErrCommandAborted) || Code(err) != "aborted" {
					t.Fatalf("a parked duplicate: %v (%s), want ErrCommandAborted", err, Code(err))
				}
			case <-time.After(watchdog):
				t.Fatal("a duplicate parked on a panicking owner was never answered")
			}
		}
		// Stored, never forgotten: a later resend is told the same thing rather
		// than running a command whose outcome nobody knows.
		if _, err := withBlockingReceipt(context.Background(), rt, c, hash, neverRun(t, "the resend")); !errors.Is(err, ErrCommandAborted) {
			t.Fatalf("the resend after a panic: %v, want ErrCommandAborted", err)
		}
	})

	t.Run("a synchronous command", func(t *testing.T) {
		rt := newReceiptTable(nil)
		c := Command{Client: rt.newClient(), ID: "1"}
		hash := receiptHash("SetTitle", "renamed")
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not reach the owner's caller")
				}
			}()
			_, _ = withSyncReceipt(rt, c, hash, func() (string, error) {
				panic("a command that panics")
			})
		}()
		if _, err := withSyncReceipt(rt, c, hash, neverRun(t, "the resend")); !errors.Is(err, ErrCommandAborted) {
			t.Fatalf("the resend after a panic: %v, want ErrCommandAborted", err)
		}
	})
}

// runtime.Goexit from the command itself — a t.Fatal inside a hook, in
// production terms — unwinds through the deferred completion exactly as a
// panic does: the reservation is completed as ErrCommandAborted, and the
// goroutine's own defer must not also try to close r.done a second time
// (finish already did, for a return; here abort does, for the Goexit).
// Double-closing a channel panics, which — unrecovered, in this untracked
// goroutine — would crash the whole test binary, so the test's silence on
// that point is itself part of what it proves (r26 finding 4, hunt item A).
func TestGoexitFromRunCompletesItsReservationAsAborted(t *testing.T) {
	t.Run("a blocking command", func(t *testing.T) {
		rt := newReceiptTable(nil)
		c := Command{Client: rt.newClient(), ID: "1"}
		hash := receiptHash("Cancel", "turn-1")
		unwound := make(chan struct{})
		go func() {
			defer close(unwound)
			_, _ = withBlockingReceipt(context.Background(), rt, c, hash, func() (string, error) {
				runtime.Goexit()
				return "unreachable", nil
			})
		}()
		select {
		case <-unwound:
		case <-time.After(watchdog):
			t.Fatal("the Goexit never unwound the goroutine")
		}
		if _, err := withBlockingReceipt(context.Background(), rt, c, hash, neverRun(t, "the resend")); !errors.Is(err, ErrCommandAborted) {
			t.Fatalf("the resend after a Goexit: %v, want ErrCommandAborted", err)
		}
	})

	t.Run("a synchronous command", func(t *testing.T) {
		rt := newReceiptTable(nil)
		c := Command{Client: rt.newClient(), ID: "1"}
		hash := receiptHash("SetTitle", "renamed")
		unwound := make(chan struct{})
		go func() {
			defer close(unwound)
			_, _ = withSyncReceipt(rt, c, hash, func() (string, error) {
				runtime.Goexit()
				return "unreachable", nil
			})
		}()
		select {
		case <-unwound:
		case <-time.After(watchdog):
			t.Fatal("the Goexit never unwound the goroutine")
		}
		if _, err := withSyncReceipt(rt, c, hash, neverRun(t, "the resend")); !errors.Is(err, ErrCommandAborted) {
			t.Fatalf("the resend after a Goexit: %v, want ErrCommandAborted", err)
		}
	})
}

// A panic from afterForget, AFTER publication, must not re-run the deferred
// abort: the reservation is already completed and its done channel already
// closed by finish/publishLocked, so if completed were still false when
// afterForget panicked, the defer would call abort and close r.done a second
// time — a second panic ("close of closed channel") that would MASK
// afterForget's own, which is exactly what the recovered value below checks
// for (r26 finding 4).
func TestAPanicFromAfterForgetAfterPublicationDoesNotDoubleClose(t *testing.T) {
	rt := newReceiptTable(&receiptHooks{
		afterForget: func() { panic("afterForget panics") },
	})
	c := Command{Client: rt.newClient(), ID: "1"}
	hash := receiptHash("Cancel", "turn-1")

	func() {
		defer func() {
			rec := recover()
			if rec == nil {
				t.Fatal("afterForget's own panic did not reach the caller")
			}
			if msg, ok := rec.(string); !ok || msg != "afterForget panics" {
				t.Fatalf("recovered %v, want afterForget's own panic unmasked (a double-close would mask it)", rec)
			}
		}()
		_, _ = withBlockingReceipt(context.Background(), rt, c, hash, func() (string, error) {
			return "", ErrNotAccepting // a gate refusal: afterForget runs
		})
	}()

	// The id was forgotten by the gate refusal, and nothing double-closed it,
	// so a fresh attempt executes exactly once: proof the entry was published
	// and released cleanly whatever afterForget did afterwards.
	got, err := withBlockingReceipt(context.Background(), rt, c, hash, okRun("cancelled"))
	if err != nil || got != "cancelled" {
		t.Fatalf("the fresh attempt after afterForget panicked: %q, %v", got, err)
	}
}

// A gate refusal hands the id back, and the next caller gets a genuine
// attempt — with never two executions of one id at once. Two duplicates are
// parked on the reservation when the owner is refused; they are answered with
// the refusal in the same locked section that takes the entry out of the
// table, and a new caller arriving in that very window (the owner is held in
// afterForget until it is done) executes, exactly once, after the first
// attempt has returned (r24 finding 5, hunt item B).
func TestAGateRefusalHandsTheIDBackWithoutOverlappingExecutions(t *testing.T) {
	var runs overlap
	found := make(chan struct{}, 4)
	forgotten, retried := make(chan struct{}), make(chan struct{})
	var once sync.Once
	rt := newReceiptTable(&receiptHooks{
		foundOpen: func() { found <- struct{}{} },
		afterForget: func() {
			// The window: the id is retryable and the first attempt has not
			// come back to its caller yet.
			once.Do(func() {
				close(forgotten)
				<-retried
			})
		},
	})
	c := Command{Client: rt.newClient(), ID: "1"}
	hash := receiptHash("Set", "mode", "", "plan")

	reserved, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := withBlockingReceipt(context.Background(), rt, c, hash, func() (string, error) {
			runs.enter()
			defer runs.exit()
			close(reserved)
			<-release
			return "", ErrUnavailable
		})
		first <- err
	}()
	await(t, reserved, "the reservation")

	dups := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := withBlockingReceipt(context.Background(), rt, c, hash, neverRun(t, "a duplicate"))
			dups <- err
		}()
	}
	awaitHook(t, found, 2, "duplicates to find the open reservation")
	close(release)

	await(t, forgotten, "the refusal to forget its reservation")
	for i := 0; i < 2; i++ {
		select {
		case err := <-dups:
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("a parked duplicate: %v, want the owner's gate refusal", err)
			}
		case <-time.After(watchdog):
			t.Fatal("a duplicate parked on a refused owner was never answered")
		}
	}
	// In the window, with the first attempt still inside its entry hook.
	got, err := withBlockingReceipt(context.Background(), rt, c, hash, func() (string, error) {
		runs.enter()
		defer runs.exit()
		return "set", nil
	})
	if err != nil || got != "set" {
		t.Fatalf("the new caller: %q, %v; want a genuine attempt", got, err)
	}
	close(retried)
	if err := <-first; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("the first attempt: %v, want ErrUnavailable", err)
	}
	if most, n := runs.seen(); most != 1 || n != 2 {
		t.Fatalf("%d executions, %d of them at once; want 2 and never more than 1 at a time", n, most)
	}
	// And the attempt that succeeded is what the id now replays.
	again, err := withBlockingReceipt(context.Background(), rt, c, hash, neverRun(t, "the resend"))
	if err != nil || again != "set" {
		t.Fatalf("the resend: %q, %v", again, err)
	}
}

// Closing the engine while duplicates are parked on a blocking reservation
// answers them: the session's close frees the call the owner is parked in, the
// owner completes its reservation as it would have anyway, and nothing is left
// waiting on a channel that will never close.
func TestCloseAnswersDuplicatesParkedOnAReservation(t *testing.T) {
	found := make(chan struct{}, 4)
	r := newRigHooked(t, Options{}, agent.EventLogOptions{NoPrimary: true},
		&hooks{receipts: &receiptHooks{foundOpen: func() { found <- struct{}{} }}})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	entered, release := r.s.holdNextCancel()
	defer release() // the session's close frees it; this is belt and braces
	c := Command{Client: r.e.NewClientID(), ID: "1"}
	go func() { _, _ = r.e.Cancel(context.Background(), c, "turn-1") }()
	await(t, entered, "the cancel to reach the session")

	dups := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := r.e.Cancel(context.Background(), c, "turn-1")
			dups <- err
		}()
	}
	awaitHook(t, found, 2, "duplicates to find the open reservation")

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close never returned with duplicates parked on a reservation")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-dups:
		case <-time.After(watchdog):
			t.Fatal("a duplicate was left parked after the engine closed")
		}
	}
}

// Ids arrive out of order and with gaps — the TUI pre-mints them for closures
// that may make several calls — so the mark is the highest EVICTED id and the
// entry is looked at first: a retained LOW id still replays after a higher one
// has been evicted, an id that was never seen is unknowable below the mark and
// a fresh command above it.
func TestOutOfOrderIDsKeepTheirOwnAnswers(t *testing.T) {
	rt := newReceiptTable(nil)
	// Two results at a time, so eviction can be driven in three commands
	// instead of a thousand and three.
	rt.cap = 2
	client := rt.newClient()
	hash := receiptHash("SetTitle", "t")
	cmd := func(id uint64) Command { return Command{Client: client, ID: strconv.FormatUint(id, 10)} }

	for _, at := range []struct {
		id  uint64
		res string
	}{{100, "hundred"}, {1, "one"}, {5, "five"}} {
		if _, err := withSyncReceipt(rt, cmd(at.id), hash, okRun(at.res)); err != nil {
			t.Fatalf("id %d: %v", at.id, err)
		}
	}

	// 100 was the oldest completed, so it went first and took the mark with it.
	if got, err := withSyncReceipt(rt, cmd(1), hash, neverRun(t, "the retained low id")); err != nil || got != "one" {
		t.Fatalf("the retained low id: %q, %v; want its own answer replayed", got, err)
	}
	if _, err := withSyncReceipt(rt, cmd(100), hash, neverRun(t, "the evicted high id")); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("the evicted high id: %v, want ErrUnknownCommand", err)
	}
	if _, err := withSyncReceipt(rt, cmd(50), hash, neverRun(t, "an unseen id below the mark")); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("an unseen id below the mark: %v, want ErrUnknownCommand", err)
	}
	if got, err := withSyncReceipt(rt, cmd(101), hash, okRun("above")); err != nil || got != "above" {
		t.Fatalf("an unseen id above the mark: %q, %v; want it to execute", got, err)
	}
}

// A client's high-water mark is never silently forgotten, however many
// clients are minted after it: forgetting one would make that client's old
// ids look unseen and run a second time, which is the one thing this table
// exists to prevent. No client is EVER retired (r26 finding 2, closing r24
// finding 1's own gap): the FIRST client minted stays exactly as answerable
// after 5,000 later clients as before them — its own unseen ids still
// execute, its evicted ids still stay ErrUnknownCommand rather than running
// again, and an open reservation of its still finds its duplicate rather than
// being torn out from under it. The reviewer's own schedule proves the last
// of those: c-1 holds an OPEN blocking reservation while all 5,000 clients
// are minted around it, and its duplicate — arriving only once they all
// exist — still waits and gets the owner's result.
func TestNoMarkIsEverLostHoweverManyClientsAreMinted(t *testing.T) {
	found := make(chan struct{}, 4)
	rt := newReceiptTable(&receiptHooks{foundOpen: func() { found <- struct{}{} }})
	rt.cap = 1 // one result at a time, so a mark can be set in two commands
	hash := receiptHash("SetTitle", "t")
	cmd := func(client string, id uint64) Command {
		return Command{Client: client, ID: strconv.FormatUint(id, 10)}
	}
	// mark leaves client with id 1 evicted and id 2 retained.
	mark := func(client string) {
		t.Helper()
		for id := uint64(1); id <= 2; id++ {
			if _, err := withSyncReceipt(rt, cmd(client, id), hash, okRun("titled")); err != nil {
				t.Fatalf("%s/%d: %v", client, id, err)
			}
		}
	}

	first := rt.newClient()
	mark(first)

	// c-1 (first) holds an open blocking reservation, under a THIRD id of its
	// own, for the whole of the 5,000-client mint below.
	blockHash := receiptHash("Cancel", "turn-1")
	held := Command{Client: first, ID: "3"}
	reserved, release := make(chan struct{}), make(chan struct{})
	ownerRes, ownerErr := make(chan string, 1), make(chan error, 1)
	go func() {
		res, err := withBlockingReceipt(context.Background(), rt, held, blockHash, func() (string, error) {
			close(reserved)
			<-release
			return "cancelled", nil
		})
		ownerRes <- res
		ownerErr <- err
	}()
	await(t, reserved, "c-1's own reservation")

	// A client minted late enough to survive any bound a smaller table would
	// have had, so what is proven for it is the MARK's survival, same as
	// r24's own version of this test proved.
	var late string
	for i := 0; i < 5000; i++ {
		if i == 2500 {
			late = rt.newClient()
			mark(late)
			continue
		}
		rt.newClient()
	}

	// A duplicate arriving only now — after all 5,000 later clients exist —
	// still finds c-1's reservation open and waits on it, rather than being
	// told the id is unknown because its client was torn out from under it.
	dupRes, dupErr := make(chan string, 1), make(chan error, 1)
	go func() {
		res, err := withBlockingReceipt(context.Background(), rt, held, blockHash, neverRun(t, "the duplicate"))
		dupRes <- res
		dupErr <- err
	}()
	awaitHook(t, found, 1, "the duplicate to find c-1's still-open reservation")
	close(release)

	if err := <-ownerErr; err != nil {
		t.Fatalf("c-1's own reservation: %v", err)
	}
	select {
	case got := <-dupRes:
		if err := <-dupErr; err != nil {
			t.Fatalf("the duplicate: %v", err)
		}
		if got != "cancelled" {
			t.Fatalf("the duplicate got %q, want the owner's %q", got, "cancelled")
		}
	case <-time.After(watchdog):
		t.Fatal("the duplicate never returned")
	}

	for _, c := range []Command{cmd(first, 1), cmd(late, 1)} {
		if _, err := withSyncReceipt(rt, c, hash, neverRun(t, "an evicted id after 5,000 clients")); !errors.Is(err, ErrUnknownCommand) {
			t.Fatalf("%s: %v, want ErrUnknownCommand", c.Cause(), err)
		}
	}
	// The FIRST client minted is never retired: its own never-seen ids still
	// execute after 5,000 later clients, exactly as the late client's do.
	if got, err := withSyncReceipt(rt, cmd(first, 4242), hash, okRun("fresh")); err != nil || got != "fresh" {
		t.Fatalf("the first client's unseen id: %q, %v; want it to execute, not ErrUnknownCommand", got, err)
	}
	if got, err := withSyncReceipt(rt, cmd(late, 4242), hash, okRun("fresh")); err != nil || got != "fresh" {
		t.Fatalf("a live client's unseen id: %q, %v; want it to execute", got, err)
	}
	// And minting never stops working, whatever the count.
	if id := rt.newClient(); id == "" || id == first {
		t.Fatalf("NewClientID minted %q", id)
	}
}

// The age bound is measured on the table's own clock, and the pass that
// applies it looks at every entry. A clock that never advances keeps every
// receipt (the old code read the session's clock, which a Stub freezes, and
// advertised ten minutes it never enforced); a clock that goes backwards
// leaves a future-dated entry at the front of the order, and entries behind it
// must still expire (r24 finding 6).
func TestTheAgeBoundIsMeasuredOnTheTablesOwnClock(t *testing.T) {
	t.Run("a clock that never advances keeps everything", func(t *testing.T) {
		clock := newTestClock(epoch())
		rt := newReceiptTable(&receiptHooks{now: clock.now})
		c := Command{Client: rt.newClient(), ID: "1"}
		hash := receiptHash("SetTitle", "t")
		if _, err := withSyncReceipt(rt, c, hash, okRun("titled")); err != nil {
			t.Fatal(err)
		}
		if got, err := withSyncReceipt(rt, c, hash, neverRun(t, "the resend")); err != nil || got != "titled" {
			t.Fatalf("the resend on a frozen clock: %q, %v", got, err)
		}
	})

	t.Run("a backward clock does not park expired entries behind a future one", func(t *testing.T) {
		clock := newTestClock(epoch().Add(time.Hour))
		rt := newReceiptTable(&receiptHooks{now: clock.now})
		client := rt.newClient()
		hash := receiptHash("SetTitle", "t")
		future := Command{Client: client, ID: "1"}
		if _, err := withSyncReceipt(rt, future, hash, okRun("future")); err != nil {
			t.Fatal(err)
		}
		// Back an hour: the entry above is now dated in the future and sits at
		// the front of the completion order.
		clock.set(epoch())
		past := Command{Client: client, ID: "2"}
		if _, err := withSyncReceipt(rt, past, hash, okRun("past")); err != nil {
			t.Fatal(err)
		}
		clock.advance(receiptAge + time.Minute)
		if _, err := withSyncReceipt(rt, past, hash, neverRun(t, "the expired entry")); !errors.Is(err, ErrUnknownCommand) {
			t.Fatalf("an expired entry behind a future-dated one: %v, want ErrUnknownCommand", err)
		}
		if got, err := withSyncReceipt(rt, future, hash, neverRun(t, "the future-dated entry")); err != nil || got != "future" {
			t.Fatalf("the future-dated entry: %q, %v; want it kept", got, err)
		}
	})
}

// The payload hash is INJECTIVE: no two different requests spell themselves
// the same way. A separator alone is not enough — a NUL can fall inside a
// field as easily as between two — and a flattened slice loses where its
// elements end, which would let one answer to an ask replay for another (r24
// finding 4).
func TestThePayloadHashIsInjective(t *testing.T) {
	answers := func(m map[string][]string) string {
		return answerSpelling(agent.AskAnswer{Answers: m})
	}
	for _, tc := range []struct{ name, a, b string }{
		{
			name: "one answer with a space against two answers",
			a:    answers(map[string][]string{"q": {"a b"}}),
			b:    answers(map[string][]string{"q": {"a", "b"}}),
		},
		{
			name: "a key that ends where another's value begins",
			a:    answers(map[string][]string{"a": {"b"}}),
			b:    answers(map[string][]string{"ab": {""}}),
		},
		{
			name: "no answers against one empty answer",
			a:    answers(nil),
			b:    answers(map[string][]string{"": {}}),
		},
		{
			name: "a NUL inside a field against one between two fields",
			a:    receiptHash("Set", "config", "a", "b\x00c"),
			b:    receiptHash("Set", "config", "a\x00b", "c"),
		},
		{
			name: "a missing argument against an empty one",
			a:    receiptHash("ClearQueue"),
			b:    receiptHash("ClearQueue", ""),
		},
		{
			name: "an unconditional edit against one on a version",
			a:    receiptHash("EditQueued", "row-1", "t", versionSpelling(nil)),
			b:    receiptHash("EditQueued", "row-1", "t", versionSpelling(new(int))),
		},
		{
			name: "two methods with the same arguments",
			a:    receiptHash("Queue", "text"),
			b:    receiptHash("Interject", "text"),
		},
	} {
		if tc.a == tc.b {
			t.Errorf("%s: both spell %q", tc.name, tc.a)
		}
	}

	// nil and empty are EQUIVALENT, in both directions and at both levels: a
	// resend that went through a codec may turn one into the other, and a
	// client that changed nothing must not be told ErrBadRequest.
	for _, tc := range []struct {
		name string
		a, b agent.AskAnswer
	}{
		{
			name: "a nil map against an empty one",
			a:    agent.AskAnswer{Skip: true},
			b:    agent.AskAnswer{Skip: true, Answers: map[string][]string{}},
		},
		{
			name: "a nil slice against an empty one",
			a:    agent.AskAnswer{Answers: map[string][]string{"q": nil}},
			b:    agent.AskAnswer{Answers: map[string][]string{"q": {}}},
		},
	} {
		if answerSpelling(tc.a) != answerSpelling(tc.b) {
			t.Errorf("%s: %q against %q", tc.name, answerSpelling(tc.a), answerSpelling(tc.b))
		}
		// Both ways round the table: whichever spelling came first, the other
		// is the same command and replays it rather than re-executing.
		for _, order := range [][2]agent.AskAnswer{{tc.a, tc.b}, {tc.b, tc.a}} {
			rt := newReceiptTable(nil)
			c := Command{Client: rt.newClient(), ID: "1"}
			hashOf := func(a agent.AskAnswer) string { return receiptHash("Answer", "ask-1", answerSpelling(a)) }
			if _, err := withSyncReceipt(rt, c, hashOf(order[0]), okRun("answered")); err != nil {
				t.Fatalf("%s: the first call: %v", tc.name, err)
			}
			got, err := withSyncReceipt(rt, c, hashOf(order[1]), neverRun(t, "the resend"))
			if err != nil || got != "answered" {
				t.Fatalf("%s: the resend: %q, %v", tc.name, got, err)
			}
		}
	}
}
