package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/journal"
)

// The joint test Plan 020 §2.7 and R7 owe Plan 019: the native adapter's tool
// events and the event log's ordering boundary, driven together under -race.
// Whoever landed second owed it, and S1a landed second — H2's runner (PR #35)
// was on main before this branch merged.
//
// What it pins is the seam between the two designs. The harness calls the
// sink from its own tool goroutines as well as its stream one, so several
// publishers reach the boundary at once; a ToolProgress is lossy by contract
// and goes out through TryPublish, which drops for everyone and spends no
// sequence number; and Cancel and Close run on other goroutines again, while
// those publishers are still in flight. The invariants below are the ones a
// reconnecting client depends on: numbers that only ever go up with no hole,
// a primary that is never left holding an event the log did not commit, and a
// Close that ends the session without wedging behind any of it.

// jointToolEvents drives the sink the way the harness does: one goroutine per
// call, each opening a row, streaming progress, and finishing it.
func jointToolEvents(s *nativeSession, calls, progress int) {
	var wg sync.WaitGroup
	for c := range calls {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			id := jointToolID(c)
			s.sink(harness.ToolStarted{ID: id, Step: 1, Tool: "bash", Kind: tool.KindExecute})
			for p := range progress {
				s.sink(harness.ToolProgress{ID: id, Output: strings.Repeat("o", p+1)})
			}
			s.sink(harness.ToolFinished{ID: id, Result: tool.Result{Text: "done"}})
		}(c)
	}
	wg.Wait()
}

func jointToolID(c int) string { return "t1.1." + string(rune('a'+c%26)) + string(rune('0'+c/26)) }

// TestNativeToolEventsAndTheBoundaryAgreeUnderRace is the joint -race test:
// concurrent tool publishers, a reader, a cancel and a close, with the log's
// own guarantees asserted afterwards.
func TestNativeToolEventsAndTheBoundaryAgreeUnderRace(t *testing.T) {
	s := newNative(Options{}, nil)
	closeAtCleanup(t, s)

	// ONE reader, draining as a consumer would, recording what it saw. It has
	// to be the only one: two goroutines taking from the same channel would
	// record the two halves in whatever order they were scheduled, which says
	// nothing about the order the log published in. So the reader also does
	// the final drain, once stop is closed, rather than leaving it to the
	// test's own goroutine.
	var mu sync.Mutex
	var seqs []uint64
	var lossy, terminal int
	record := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		seqs = append(seqs, ev.Seq)
		if ev.Type == EventTool && ev.Tool != nil && ev.Tool.Status == toolInProgress {
			lossy++
		}
	}
	stop := make(chan struct{})
	read := make(chan struct{})
	go func() {
		defer close(read)
		for {
			select {
			case ev := <-s.Events():
				record(ev)
			case <-stop:
				// Close has returned, so nothing more can be published; what
				// is still buffered is the whole of what is left.
				for {
					select {
					case ev := <-s.Events():
						record(ev)
					default:
						return
					}
				}
			}
		}
	}()

	jointToolEvents(s, 8, 12)
	// A cancel with nothing claimed is a no-op, but it runs the same locked
	// sections a real one does while the publishers above are still settling.
	if _, err := s.Cancel(t.Context()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	s.settleTools()

	// Every row is terminal, whatever order the goroutines finished in.
	for _, row := range s.Snapshot().Tools {
		if row.Status == toolInProgress || row.Status == toolPending {
			t.Fatalf("a row is still in flight after settling: %+v", row)
		}
		terminal++
	}
	if terminal != 8 {
		t.Fatalf("%d rows, want one per call", terminal)
	}

	// Close ends it: the reader's channel is the log's primary, which Close
	// does not close, so the test stops reading by closing the session and
	// then draining what is buffered.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	close(stop)
	<-read

	mu.Lock()
	defer mu.Unlock()
	if len(seqs) == 0 {
		t.Fatal("the reader saw nothing")
	}
	// The boundary's contract: strictly increasing, no hole, no zero. A
	// TryPublish that dropped spends no number, so gaps in what the READER
	// saw are impossible — a dropped event never reached it at all.
	prev := uint64(0)
	for _, got := range seqs {
		if got == 0 {
			t.Fatalf("an event reached the primary unnumbered: %v", seqs)
		}
		if got <= prev {
			t.Fatalf("seq %d after %d: %v", got, prev, seqs)
		}
		prev = got
	}
	// The log's own count agrees with the highest number it handed out, so no
	// sequence number was spent on an event nobody received.
	if h := s.log.Health(); h.Omitted != 0 {
		t.Fatalf("omitted records in a run of small events: %+v", h)
	}
}

// TestNativeToolProgressDropsForEveryoneAtOnce is the TryPublish half of the
// contract: a progress snapshot the primary has no room for is dropped for
// the primary, for every subscription and for the journal alike, and spends
// no sequence number — so a client resuming from a cursor never has to
// explain a hole that a lossy frame left behind.
func TestNativeToolProgressDropsForEveryoneAtOnce(t *testing.T) {
	s := newNative(Options{}, nil)
	closeAtCleanup(t, s)
	sub, err := s.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: "bash", Kind: tool.KindExecute})
	// Fill the primary through the session's own emit, so everything in it
	// carries a number, then take the log's count.
	for len(s.events) < cap(s.events) {
		s.emit(Event{Type: EventText, Text: "filler"})
	}
	// It must return rather than wait, and leave the primary as it found it.
	s.sink(harness.ToolProgress{ID: "t1.1.1", Output: "dropped"})
	if len(s.events) != cap(s.events) {
		t.Fatalf("the dropped snapshot took a slot: %d of %d", len(s.events), cap(s.events))
	}

	// Drained, the last number in the primary is the last one the log spent,
	// and the next event takes exactly the one after it: that is the whole of
	// what "consumes no sequence number" means.
	before := lastPrimarySeq(t, s)
	s.emit(Event{Type: EventText, Text: "after"})
	after := lastPrimarySeq(t, s)
	if after != before+1 {
		t.Fatalf("the dropped progress spent a number: %d then %d", before, after)
	}
	// The row still took the output, which is why dropping the frame is safe.
	if rows := s.Snapshot().Tools; len(rows) != 1 || rows[0].Output == nil || rows[0].Output.Stdout != "dropped" {
		t.Fatalf("the merge did not happen: %+v", rows)
	}
}

// lastPrimarySeq drains the primary and returns the highest number in it.
func lastPrimarySeq(t *testing.T, s *nativeSession) uint64 {
	t.Helper()
	var last uint64
	for {
		select {
		case ev := <-s.Events():
			last = ev.Seq
		default:
			if last == 0 {
				t.Fatal("the primary held nothing numbered")
			}
			return last
		}
	}
}

// TestNativeCloseEndsATurnFullOfToolPublishers is the third leg: Close while
// tool goroutines are still publishing into a primary nobody is draining. It
// must return — the publishers escape on done, the log's cutoff refuses the
// rest — and it must leave no subscription hanging.
func TestNativeCloseEndsATurnFullOfToolPublishers(t *testing.T) {
	s := newNative(Options{}, nil)
	sub, err := s.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Nobody reads the primary, so these block inside the boundary until
	// Close releases them.
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		jointToolEvents(s, 4, 400)
	}()
	<-started

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	wg.Wait() // the publishers must all have been released

	// The subscription ended, and its records are contiguous as far as they
	// go: a closed log ends a subscription rather than truncating it silently.
	prev := uint64(0)
	for rec := range sub.Records() {
		if prev != 0 && rec.Seq != prev+1 {
			t.Fatalf("subscription seq %d after %d", rec.Seq, prev)
		}
		prev = rec.Seq
	}
	if err := sub.Err(); err == nil {
		t.Fatal("the subscription ended with no error")
	}
	// A publish after Close is refused, not queued.
	if s.log.Publish(context.Background(), nil, Event{Type: EventText, Text: "late"}) {
		t.Fatal("a publish after Close was accepted")
	}
}

// TestNativeCloseReturnsWithATurnsFlushBlockedOnTheOutbox is review r17's
// finding 1, as the reviewer scheduled it: the primary is full and its reader
// has stopped, an event of a component above the seam is still in the outbox
// with the drainer blocked on its send, and a turn ends — reaching the flush
// every session owes before its terminal event (plan 021 §3.6).
//
// Only the log's own Close frees that drainer, and Close waits for the turn's
// continuation (rel) BEFORE it closes the log, so a flush that could not escape
// would be a Close waiting on a goroutine waiting on that same Close. It escapes
// on the session's done, exactly as the terminal emit behind it does, so Close
// returns.
func TestNativeCloseReturnsWithATurnsFlushBlockedOnTheOutbox(t *testing.T) {
	f := newNativeFixture(t)
	s := f.session(Options{})
	// Set before Start, so nothing is publishing while the field is written —
	// and armed only once the fill is in place, because Start's own flush (the
	// install delta it publishes) parks on the drainer too, and taking THAT as
	// "the turn's flush" would close the barrier before the turn had reached
	// one.
	parked := make(chan uint64, 1)
	var armed atomic.Bool
	var once sync.Once
	s.log.hooks = &logHooks{flushParked: func(target uint64) {
		if !armed.Load() {
			return
		}
		once.Do(func() { parked <- target })
	}}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start's install delta, off the primary before the fill: without it the
	// fill would find one slot already spent and park inside it.
	takeStartDelta(t, s.log)
	fillPrimary(t, s.log)
	armed.Store(true)
	// One event from above the seam, stuck: the drainer is inside the boundary
	// offering it to a primary nobody will read again.
	s.log.Enqueue(textEvent("from above the seam"))

	// No step is queued, so the model fails the moment it is asked: the turn
	// publishes nothing and goes straight to the flush it owes.
	out := startPrompt(s, "go")
	if target := await(t, parked, "the turn's flush to park on the outbox"); target == 0 {
		t.Fatal("the flush parked for nothing")
	}

	within(t, "Close", func() { _ = s.Close() })
	if got := await(t, out, "the prompt"); got.err == nil {
		t.Fatalf("the prompt returned %+v, want the failure its model gave it", got.res)
	}
}

// TestNativeInterjectIsJournaledWhateverBecameOfIt is R7's interject leg,
// reachable now that H2's PR 4 gave the native session a real Interject.
// Whatever the turn state does with the text — takes it, refuses it, or has
// already ended — the attempt is on the record with the text as typed and a
// class for the outcome, under an id of the journal wrapper's own rather than
// any the harness mints. That is what lets a reader of the file account for
// an interjection that never reached a transcript.
func TestNativeInterjectIsJournaledWhateverBecameOfIt(t *testing.T) {
	s := newNative(Options{JournalDir: filepath.Join(t.TempDir(), "journal")}, nil)
	w := journalOf(t, s.log)

	// Nothing is running, so this takes the refusal path — the one an
	// interjection typed a moment too late also takes.
	err := s.Interject(t.Context(), "BANANA")
	if err == nil {
		t.Fatal("an interjection with no turn was accepted")
	}

	attempts, lines := journaledAttemptsOf(t, s, w)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want the interjection's own", len(attempts))
	}
	a := attempts[0]
	if got := jsonString(a.prompt, "kind"); got != string(journal.PromptKindInterject) {
		t.Fatalf("kind %q, want %q", got, journal.PromptKindInterject)
	}
	if got := jsonString(a.prompt, "text"); got != "BANANA" {
		t.Fatalf("text %q, want the text as typed", got)
	}
	if got := jsonString(a.end, "errClass"); got == "" {
		t.Fatalf("the refusal was journaled with no class: %v", a.end)
	}
	if id := jsonString(a.prompt, "attempt"); !strings.HasPrefix(id, "interject-") {
		t.Fatalf("attempt id %q, want the wrapper's own", id)
	}
	assertClosingIsLast(t, lines)
}
