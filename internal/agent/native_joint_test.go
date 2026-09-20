package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
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

	// One reader, draining as a consumer would, recording what it saw.
	var mu sync.Mutex
	var seqs []uint64
	var lossy, terminal int
	read := make(chan struct{})
	go func() {
		defer close(read)
		for ev := range s.Events() {
			mu.Lock()
			seqs = append(seqs, ev.Seq)
			if ev.Type == EventTool && ev.Tool != nil && ev.Tool.Status == toolInProgress {
				lossy++
			}
			mu.Unlock()
		}
	}()

	jointToolEvents(s, 8, 12)
	// A cancel with nothing claimed is a no-op, but it runs the same locked
	// sections a real one does while the publishers above are still settling.
	if err := s.Cancel(t.Context()); err != nil {
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
	drainPrimaryInto(s, &mu, &seqs)

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

// drainPrimaryInto takes whatever is still buffered in the primary after
// Close, without blocking: the channel is never closed, so a range over it
// would wait for ever.
func drainPrimaryInto(s *nativeSession, mu *sync.Mutex, seqs *[]uint64) {
	for {
		select {
		case ev := <-s.Events():
			mu.Lock()
			*seqs = append(*seqs, ev.Seq)
			mu.Unlock()
		default:
			return
		}
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
