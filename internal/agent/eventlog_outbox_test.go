package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/journal"
)

// The outbox's tests (plan 021 §3.3, A2 and A3). Everything here is driven by
// the log's own barriers — the hooks, a channel a goroutine closed, a full
// primary nobody reads — and the only clock is the package's watchdog, which
// turns a deadlock into a failure instead of a hung run. Nothing sleeps to let
// something happen.

// drainerFrame is the outbox drainer's goroutine, as runtime.Stack prints it,
// for the leak checks: Close joins it, so a log that has been closed runs none.
const drainerFrame = "github.com/charliek/craze/internal/agent.(*EventLog).drain("

// outboxState is the outbox's accounting, read under its own mutex: what is
// enqueued and not yet committed, the running totals Flush compares, and whether
// the drainer was ever started.
func outboxState(l *EventLog) (events int, bytes int, enqueued, drained uint64, started bool) {
	l.outboxMu.Lock()
	defer l.outboxMu.Unlock()
	return l.outboxEvents, l.outboxBytes, l.enqueued, l.drained, l.drainerStarted
}

// flushIn starts a Flush on a goroutine of its own — never the primary's reader
// — and hands back its answer.
func flushIn(l *EventLog, ctx context.Context) <-chan error {
	out := make(chan error, 1)
	go func() { out <- l.Flush(ctx, nil) }()
	return out
}

// flushNow is Flush from the test's own goroutine, which must return.
func flushNow(t *testing.T, l *EventLog) error {
	t.Helper()
	var err error
	within(t, "Flush", func() { err = l.Flush(context.Background(), nil) })
	return err
}

// sendingAt is an outboxSending hook that reports, once, that the drainer is
// inside the boundary about to offer seq to the primary, and whether it was
// still going to wait for it (false is the at-close commit).
func sendingAt(seq uint64) (func(uint64, bool), <-chan bool) {
	at := make(chan bool, 1)
	var once sync.Once
	return func(s uint64, waiting bool) {
		if s == seq {
			once.Do(func() { at <- waiting })
		}
	}, at
}

// admittingBatch is an outboxAdmitting hook that reports every batch the drainer
// is about to take the boundary for.
func admittingBatch() (func(int), <-chan int) {
	at := make(chan int, 64)
	return func(events int) { at <- events }, at
}

// ringRecords is every record the ring holds, oldest first, read inside the
// boundary: what "committed to the ring" comes to once Close has ended every
// subscription and the primary's buffer is all that is left of the stream.
func ringRecords(t *testing.T, l *EventLog) []Record {
	t.Helper()
	var recs []Record
	within(t, "reading the ring", func() {
		l.sem <- struct{}{}
		defer l.release()
		for i := range l.ring.n {
			recs = append(recs, l.ring.at(i))
		}
	})
	return recs
}

// ringSeqs is ringRecords' seqs.
func ringSeqs(t *testing.T, l *EventLog) []uint64 {
	t.Helper()
	recs := ringRecords(t, l)
	seqs := make([]uint64, len(recs))
	for i, r := range recs {
		seqs[i] = r.Seq
	}
	return seqs
}

// collectRecords reads a subscription until it closes, on a goroutine of its
// own, and hands over what it got: the shape a test needs when the records it
// waits for are only committed while Close runs. count reports what it has so
// far, so a test can wait for a record without reading the slice.
func collectRecords(s *Subscription) (got <-chan []Record, count *atomic.Int64) {
	out := make(chan []Record, 1)
	n := &atomic.Int64{}
	go func() {
		var recs []Record
		for r := range s.Records() {
			recs = append(recs, r)
			n.Add(1)
		}
		out <- recs
	}()
	return out, n
}

// awaitCount waits, up to the watchdog, for n to reach want. It is a wait for
// another goroutine's progress, not for a duration: a correct log satisfies it
// at once, and only a real failure reaches the deadline. It reports rather than
// fails, because it is also called from inside a hook — off the test's own
// goroutine, where a Fatalf would stop the wrong one — and every caller has an
// assertion after it that the missing records fail anyway.
func awaitCount(t *testing.T, n *atomic.Int64, want int64, what string) {
	t.Helper()
	deadline := time.Now().Add(logWatchdog)
	for n.Load() < want {
		if time.Now().After(deadline) {
			t.Errorf("%s: %d of %d after %v", what, n.Load(), want, logWatchdog)
			return
		}
		runtime.Gosched()
	}
}

// waitGroupWithin waits for wg, bounded by the watchdog. It reports rather than
// fails, because it is called from inside a hook — off the test's own goroutine,
// where a Fatalf would stop the wrong one.
func waitGroupWithin(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(logWatchdog):
		t.Errorf("%s: still running after %v", what, logWatchdog)
	}
}

// batchTexts is one batch's texts, "p<producer>-b<batch>-<index>".
func batchTexts(producer, batch, n int) []Event {
	evs := make([]Event, n)
	for i := range evs {
		evs[i] = textEvent(fmt.Sprintf("p%d-b%d-%d", producer, batch, i))
	}
	return evs
}

// TestEventLogEnqueueIsOrderedAndBatchAtomic is A2's ordering half: with several
// producers enqueuing batches and others publishing directly at the same time,
// every batch lands as a contiguous run of seqs in the order it was given, each
// producer's batches land in the order that producer enqueued them, and nothing
// — not another batch, not a direct Publish — ever lands inside one.
func TestEventLogEnqueueIsOrderedAndBatchAtomic(t *testing.T) {
	const producers, batches, each = 4, 25, 3
	const directPublishers, directEach = 2, 50
	const total = producers*batches*each + directPublishers*directEach
	l, _ := newJournaledLog(t, EventLogOptions{})
	keepDrained(t, l)
	sub := mustSubscribe(t, l, SubscribeOptions{MaxItems: total + 1})

	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			for b := range batches {
				l.Enqueue(batchTexts(p, b, each)...)
			}
		})
	}
	for d := range directPublishers {
		wg.Go(func() {
			for i := range directEach {
				if !l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("d%d-%d", d, i))) {
					t.Errorf("a direct publish returned false on an open log")
					return
				}
			}
		})
	}
	waitDone(t, &wg)
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush after every batch was enqueued: %v", err)
	}
	if events, bytes, enq, dr, _ := outboxState(l); events != 0 || bytes != 0 || enq != dr {
		t.Fatalf("after Flush the outbox holds %d events and %d bytes, enqueued %d drained %d", events, bytes, enq, dr)
	}

	recs := readN(t, sub, total)
	assertRun(t, "the subscription", recs, 1, total)
	// Where each batch landed, and in what order inside it.
	type place struct{ first, last uint64 }
	at := map[[2]int]*place{}
	lastBatch := map[int]int{}
	for i, text := range recordTexts(t, recs) {
		seq := recs[i].Seq
		var p, b, n int
		if _, err := fmt.Sscanf(text, "p%d-b%d-%d", &p, &b, &n); err != nil {
			if strings.HasPrefix(text, "d") {
				continue // a direct publish; its place is what the batches' contiguity proves
			}
			t.Fatalf("seq %d is %q, which is neither a batch's event nor a direct publish", seq, text)
		}
		key := [2]int{p, b}
		got, ok := at[key]
		switch {
		case !ok:
			if n != 0 {
				t.Fatalf("producer %d's batch %d starts at its event %d: the batch was split", p, b, n)
			}
			at[key] = &place{first: seq, last: seq}
			if prev, seen := lastBatch[p]; seen && b != prev+1 {
				t.Fatalf("producer %d's batch %d began after its batch %d: its batches are out of order", p, b, prev)
			}
			lastBatch[p] = b
		default:
			if seq != got.last+1 {
				t.Fatalf("producer %d's batch %d has seq %d after %d: something landed inside the batch", p, b, seq, got.last)
			}
			if want := int(seq - got.first); n != want {
				t.Fatalf("producer %d's batch %d holds its event %d at offset %d", p, b, n, want)
			}
			got.last = seq
		}
	}
	if len(at) != producers*batches {
		t.Fatalf("%d batches reached the subscription, want %d", len(at), producers*batches)
	}
	for key, got := range at {
		if n := got.last - got.first + 1; n != each {
			t.Fatalf("producer %d's batch %d spans %d seqs, want %d", key[0], key[1], n, each)
		}
	}
}

// TestEventLogEnqueueReturnsWithThePrimaryFullAndTheBoundaryHeld is A2's
// non-blocking half, and the whole reason the outbox exists: with the primary
// full and a direct publisher blocked on it *inside* the boundary, an Enqueue
// made with a state lock held returns at once. Nothing is lost by it — the
// drainer delivers the batch, in order, behind the publisher it waited for, once
// a reader appears.
func TestEventLogEnqueueReturnsWithThePrimaryFullAndTheBoundaryHeld(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	fillPrimary(t, l)

	hook, inside := insideAt(primaryCap + 1)
	admitting, admitted := admittingBatch()
	l.hooks = &logHooks{beforePrimarySend: hook, outboxAdmitting: admitting}
	aResult := make(chan bool, 1)
	go func() { aResult <- l.Publish(context.Background(), nil, textEvent("A")) }()
	await(t, inside, "the direct publisher to block on the full primary inside the boundary")

	// A caller's own state lock, held across the enqueue: the point of the leaf.
	var state sync.Mutex
	within(t, "Enqueue with the primary full and the boundary held", func() {
		state.Lock()
		defer state.Unlock()
		l.Enqueue(textEvent("e1"), textEvent("e2"), textEvent("e3"))
	})
	if n := await(t, admitted, "the drainer to reach the boundary"); n != 3 {
		t.Fatalf("the drainer took a batch of %d, want the 3 enqueued together", n)
	}
	if _, _, _, drained, _ := outboxState(l); drained != 0 {
		t.Fatalf("%d events were committed while the boundary was held by a blocked publisher", drained)
	}
	if !l.OutboxRoom() {
		t.Fatal("OutboxRoom is false with three events in the outbox")
	}

	const want = primaryCap + 4
	primary := collectPrimary(l, want)
	if !await(t, aResult, "the direct publisher, once a reader appeared") {
		t.Fatal("the direct publisher returned false")
	}
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	evs := await(t, primary, "the primary's events")
	seqs := make([]uint64, len(evs))
	for i, ev := range evs {
		seqs[i] = ev.Seq
	}
	if p := runSeqs(seqs, 1); p != "" {
		t.Fatalf("the primary: %s", p)
	}
	tail := []string{evs[want-4].Text, evs[want-3].Text, evs[want-2].Text, evs[want-1].Text}
	if want := []string{"A", "e1", "e2", "e3"}; !equalStrings(tail, want) {
		t.Fatalf("the primary ends %v, want %v: the batch follows the publisher that held the boundary", tail, want)
	}
	closeLog(t, l, w)
	assertRun(t, "the journal", fileRecords(t, w), 1, want)
	if h := l.Health(); h.DroppedAtClose != 0 || h.OutboxSkippedPrimary != 0 {
		t.Fatalf("health is %+v: nothing was dropped or skipped in this run", h)
	}
}

// TestEventLogADirectPublishIsStillBufferedWhenItReturns: "emitted means
// buffered" is unchanged by the outbox, which is the contract the whole of
// internal/cli/prompt.go's non-blocking drains rest on
// (TestEventLogWhatAReturnedPublisherEmittedIsAlreadyInThePrimary pins it with
// no outbox; this pins it with a drainer running beside the publisher, competing
// for the same boundary). What the outbox costs is only that an *enqueued* event
// may still be behind, which is what Flush is for.
func TestEventLogADirectPublishIsStillBufferedWhenItReturns(t *testing.T) {
	const direct = 20
	l, w := newJournaledLog(t, EventLogOptions{})
	mustSubscribe(t, l, SubscribeOptions{MaxItems: 8}) // and a subscriber that never reads
	for b := range 5 {
		l.Enqueue(batchTexts(0, b, 3)...)
	}
	texts := make([]string, direct)
	within(t, "publishing beside a running drainer", func() {
		for i := range direct {
			texts[i] = fmt.Sprintf("direct %d", i)
			if !l.Publish(context.Background(), nil, textEvent(texts[i])) {
				t.Errorf("direct publish %d returned false on an open log", i)
				return
			}
		}
	})
	// A non-blocking drain, on the goroutine that published: every direct event
	// is already there. An enqueued one may not be.
	buffered := map[string]bool{}
	for _, ev := range drainPrimary(l) {
		buffered[ev.Text] = true
	}
	for _, text := range texts {
		if !buffered[text] {
			t.Fatalf("%q was not in the primary's buffer when its Publish returned", text)
		}
	}
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	closeLog(t, l, w)
	assertRun(t, "the journal", fileRecords(t, w), 1, direct+15)
}

// TestEventLogFlushReturnsOnlyAfterItsEventsAreCommitted is A2's barrier: a
// Flush parked behind a drainer that cannot get its batch onto a full primary
// does not return, and returns nil the moment the batch is committed — not
// before. A second Flush, with nothing enqueued since, returns nil at once.
func TestEventLogFlushReturnsOnlyAfterItsEventsAreCommitted(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	fillPrimary(t, l)
	sending, atSend := sendingAt(primaryCap + 1)
	parked := make(chan uint64, 1)
	l.hooks = &logHooks{outboxSending: sending, flushParked: func(target uint64) { parked <- target }}

	l.Enqueue(textEvent("f1"), textEvent("f2"))
	if waiting := await(t, atSend, "the drainer to offer its first event to the primary"); !waiting {
		t.Fatal("the drainer did not wait for the primary on an open log")
	}
	flushed := flushIn(l, context.Background())
	if target := await(t, parked, "the Flush to park"); target != 2 {
		t.Fatalf("the Flush parked for %d events, want the 2 enqueued", target)
	}
	select {
	case err := <-flushed:
		t.Fatalf("Flush returned (%v) with its batch still in the outbox", err)
	default:
	}

	primary := collectPrimary(l, primaryCap+2)
	if err := await(t, flushed, "the Flush once its batch was committed"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Committed means committed everywhere, not merely handed to the primary.
	if n := lastSeq(t, l); n != primaryCap+2 {
		t.Fatalf("the last seq committed is %d when Flush returned, want %d", n, primaryCap+2)
	}
	if err := flushNow(t, l); err != nil {
		t.Fatalf("a second Flush with an empty outbox: %v", err)
	}
	// A19's shape: the barrier ran on a helper goroutine while the reader kept
	// reading, and what it waited for is on the primary as well as in the record.
	evs := await(t, primary, "the primary's events")
	if tail := []string{evs[primaryCap].Text, evs[primaryCap+1].Text}; !equalStrings(tail, []string{"f1", "f2"}) {
		t.Fatalf("the primary ends %v, want the flushed batch", tail)
	}
	closeLog(t, l, w)
	recs := fileRecords(t, w)
	assertRun(t, "the journal", recs, 1, primaryCap+2)
	if texts := recordTexts(t, recs[primaryCap:]); !equalStrings(texts, []string{"f1", "f2"}) {
		t.Fatalf("the journal ends %v, want the flushed batch", texts)
	}
}

// TestEventLogFlushHonoursItsContext: a Flush whose ctx ends before its batch is
// committed returns ctx.Err() and leaves the batch alone — giving up on the
// barrier is not giving up on the record, and the events commit as soon as the
// primary has room.
func TestEventLogFlushHonoursItsContext(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	fillPrimary(t, l)
	sending, atSend := sendingAt(primaryCap + 1)
	parked := make(chan uint64, 1)
	l.hooks = &logHooks{outboxSending: sending, flushParked: func(target uint64) { parked <- target }}

	l.Enqueue(textEvent("c1"))
	await(t, atSend, "the drainer to offer its event to the primary")
	ctx, cancel := context.WithCancel(context.Background())
	flushed := flushIn(l, ctx)
	await(t, parked, "the Flush to park")
	cancel()
	if err := await(t, flushed, "the Flush to give up on its ctx"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush returned %v, want context.Canceled", err)
	}
	if _, _, _, drained, _ := outboxState(l); drained != 0 {
		t.Fatalf("%d events were committed: the ctx released the waiter, not the primary", drained)
	}

	primary := collectPrimary(l, primaryCap+1)
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush once the primary had room: %v", err)
	}
	await(t, primary, "the primary's events")
	closeLog(t, l, w)
	recs := fileRecords(t, w)
	assertRun(t, "the journal", recs, 1, primaryCap+1)
	if texts := recordTexts(t, recs[primaryCap:]); !equalStrings(texts, []string{"c1"}) {
		t.Fatalf("the journal ends %v: the abandoned barrier cost the event", texts)
	}
}

// TestEventLogFlushOnAnEmptyOutboxReturnsAtOnceAndStartsNothing: a log nothing
// has enqueued to has no drainer at all, and Flush on it is nil without parking
// anything. An Enqueue with no events is the same.
func TestEventLogFlushOnAnEmptyOutboxReturnsAtOnceAndStartsNothing(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	parked := make(chan uint64, 1)
	l.hooks = &logHooks{flushParked: func(target uint64) { parked <- target }}
	keepDrained(t, l)

	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush on an empty outbox: %v", err)
	}
	l.Enqueue()
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush after an empty Enqueue: %v", err)
	}
	if _, _, enq, _, started := outboxState(l); enq != 0 || started {
		t.Fatalf("an empty Enqueue counted %d events and started the drainer: %v", enq, started)
	}
	if n := goroutinesRunning(drainerFrame); n != 0 {
		t.Fatalf("%d drainers run for a log nothing was enqueued to", n)
	}
	select {
	case target := <-parked:
		t.Fatalf("a Flush parked for %d with an empty outbox", target)
	default:
	}

	l.Enqueue(textEvent("one"))
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush after one event: %v", err)
	}
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush with the outbox empty again: %v", err)
	}
	closeLog(t, l, w)
	settleGoroutines(t, drainerFrame, 0)
}

// TestEventLogFlushAtCloseAnswersTheParkedAndRefusesTheLate is A2's close leg
// for Flush, and the rule pinned in one place: a Flush already parked when Close
// begins is answered **nil**, because the at-close path commits what is left
// rather than abandoning it, so a nil return always means "your events are in
// the record"; a Flush that arrives once Close has begun is ErrLogClosing at
// once, which always means "you asked too late" and never "we lost them".
func TestEventLogFlushAtCloseAnswersTheParkedAndRefusesTheLate(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	fillPrimary(t, l)
	sending, atSend := sendingAt(primaryCap + 1)
	parked := make(chan uint64, 1)
	l.hooks = &logHooks{outboxSending: sending, flushParked: func(target uint64) { parked <- target }}

	l.Enqueue(textEvent("p1"), textEvent("p2"), textEvent("p3"))
	if waiting := await(t, atSend, "the drainer to block on the full primary"); !waiting {
		t.Fatal("the drainer did not wait for the primary before the cut")
	}
	flushed := flushIn(l, context.Background())
	await(t, parked, "the Flush to park")

	// Nobody is reading the primary and nobody will: Close is the only thing
	// that can release the drainer, and it must do so by committing.
	closeLog(t, l, w)
	if err := await(t, flushed, "the parked Flush"); err != nil {
		t.Fatalf("the parked Flush returned %v, want nil: its target was committed by the at-close path", err)
	}
	if err := l.Flush(context.Background(), nil); !errors.Is(err, ErrLogClosing) {
		t.Fatalf("a Flush after Close returned %v, want ErrLogClosing", err)
	}
	if l.OutboxRoom() {
		t.Fatal("OutboxRoom is true after Close")
	}
	recs := fileRecords(t, w)
	assertRun(t, "the journal", recs, 1, primaryCap+3)
	if texts := recordTexts(t, recs[primaryCap:]); !equalStrings(texts, []string{"p1", "p2", "p3"}) {
		t.Fatalf("the journal ends %v, want the three enqueued events", texts)
	}
	if h := l.Health(); h.OutboxSkippedPrimary != 3 || h.DroppedAtClose != 0 {
		t.Fatalf("health is %+v, want 3 skipped by the primary and nothing dropped", h)
	}
}

// TestEventLogCloseCommitsItsOutboxWithAStoppedReader is A2's close criterion
// whole: with the reader stopped — frame teardown stops the program before
// closing the session, and craze prompt's deferred Close runs on its own reader
// — and a non-empty outbox, Close returns within its bound, every event the
// outbox held reaches the journal, the ring and a live subscription, the ones
// the primary would not take are counted as skipped rather than dropped, and the
// drainer is joined.
func TestEventLogCloseCommitsItsOutboxWithAStoppedReader(t *testing.T) {
	const batches, each = 3, 2
	const want = primaryCap + batches*each
	l, w := newJournaledLog(t, EventLogOptions{})
	sub := mustSubscribe(t, l, SubscribeOptions{MaxItems: want + 1})
	got, delivered := collectRecords(sub)
	fillPrimary(t, l)
	awaitCount(t, delivered, primaryCap, "the subscription to receive what filled the primary")

	sending, atSend := sendingAt(primaryCap + 1)
	// Close is held after the drainer has been joined until the subscription's
	// reader has every record: Close ends a subscription by discarding what its
	// owner had not delivered, so without this barrier the tail of the assertion
	// would be a race with the scheduler rather than a property of the log.
	l.hooks = &logHooks{outboxSending: sending, outboxDrained: func() {
		awaitCount(t, delivered, want, "the subscription to receive the at-close commits")
	}}
	for b := range batches {
		l.Enqueue(batchTexts(0, b, each)...)
	}
	if waiting := await(t, atSend, "the drainer to block on the full primary"); !waiting {
		t.Fatal("the drainer did not wait for the primary before the cut")
	}

	closeLog(t, l, w)
	recs := await(t, got, "the subscription's records")
	assertRun(t, "the live subscription", recs, 1, want)
	if !errors.Is(sub.Err(), ErrClosed) {
		t.Fatalf("the subscription ended with %v, want ErrClosed", sub.Err())
	}
	assertRun(t, "the journal", fileRecords(t, w), 1, want)
	if seqs := ringSeqs(t, l); len(seqs) != want || runSeqs(seqs, 1) != "" {
		t.Fatalf("the ring holds %d records (%s), want %d from seq 1", len(seqs), runSeqs(seqs, 1), want)
	}
	// The enqueued events are the ones the stopped primary never took.
	if evs := drainPrimary(l); len(evs) != primaryCap {
		t.Fatalf("the primary holds %d events, want the %d that filled it", len(evs), primaryCap)
	}
	h := l.Health()
	if h.OutboxSkippedPrimary != batches*each || h.DroppedAtClose != 0 {
		t.Fatalf("health is %+v, want %d skipped by the primary and nothing dropped", h, batches*each)
	}
	closing := assertClosingIsLast(t, fileLines(t, w))
	if closing["outboxSkippedPrimary"] != float64(batches*each) || closing["droppedAtClose"] != float64(0) {
		t.Fatalf("the closing diag says %v, want outboxSkippedPrimary %d and droppedAtClose 0", closing, batches*each)
	}
	settleGoroutines(t, drainerFrame, 0)
	within(t, "a second Close", func() { l.Close(context.Background()) })
}

// TestEventLogCloseCommitsItsOutboxBehindAPublisherHoldingTheBoundary is the
// second stopped-reader schedule, and the reason the admission cutoff still
// comes first: the drainer is waiting for a boundary a direct publisher holds
// while blocked on the full primary, and only the cutoff can free that
// publisher. Close frees it, the drainer then commits everything behind it, and
// the publisher's own event — abandoned, so numberless — is the one thing
// missing from the record.
func TestEventLogCloseCommitsItsOutboxBehindAPublisherHoldingTheBoundary(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	fillPrimary(t, l)

	hook, inside := insideAt(primaryCap + 1)
	admitting, admitted := admittingBatch()
	l.hooks = &logHooks{beforePrimarySend: hook, outboxAdmitting: admitting}
	aResult := make(chan bool, 1)
	go func() { aResult <- l.Publish(context.Background(), nil, textEvent("abandoned")) }()
	await(t, inside, "the direct publisher to block on the full primary inside the boundary")
	l.Enqueue(textEvent("b1"), textEvent("b2"))
	await(t, admitted, "the drainer to reach the boundary the publisher holds")

	closeLog(t, l, w)
	if await(t, aResult, "the publisher Close released") {
		t.Fatal("the publisher blocked on the full primary returned true")
	}
	const want = primaryCap + 2
	recs := fileRecords(t, w)
	assertRun(t, "the journal", recs, 1, want)
	if texts := recordTexts(t, recs[primaryCap:]); !equalStrings(texts, []string{"b1", "b2"}) {
		t.Fatalf("the journal ends %v, want the enqueued batch: the abandoned publish took a number", texts)
	}
	h := l.Health()
	if h.DroppedAtClose != 1 || h.OutboxSkippedPrimary != 2 {
		t.Fatalf("health is %+v, want the publisher dropped and the batch skipped by the primary", h)
	}
	settleGoroutines(t, drainerFrame, 0)
}

// TestEventLogEnqueueAfterCloseIsCountedAndDropped: an Enqueue that arrives once
// Close has cut the outbox neither blocks nor panics. Its events are counted in
// droppedAtClose, the same counter as an emit a session gave up on its own done,
// and go no further; OutboxRoom has been false since the cut, so the command
// that would have caused one was already refused.
func TestEventLogEnqueueAfterCloseIsCountedAndDropped(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	keepDrained(t, l)
	l.Enqueue(textEvent("in time"))
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	closeLog(t, l, w)

	within(t, "Enqueue after Close", func() {
		l.Enqueue(textEvent("late 1"), textEvent("late 2"))
		l.Enqueue(textEvent("late 3"))
		l.Enqueue()
	})
	if h := l.Health(); h.DroppedAtClose != 3 {
		t.Fatalf("health is %+v, want the 3 late events counted (an empty Enqueue is not an event)", h)
	}
	if err := flushNow(t, l); !errors.Is(err, ErrLogClosing) {
		t.Fatalf("Flush after a late Enqueue returned %v, want ErrLogClosing", err)
	}
	recs := fileRecords(t, w)
	assertRun(t, "the journal", recs, 1, 1)
	if texts := recordTexts(t, recs); !equalStrings(texts, []string{"in time"}) {
		t.Fatalf("the journal holds %v, want only the event enqueued before Close", texts)
	}
	settleGoroutines(t, drainerFrame, 0)
	within(t, "a second Close", func() { l.Close(context.Background()) })
}

// TestEventLogOutboxRoomFlipsAtItsBoundAndBack is A5's log half: OutboxRoom
// reports the soft bound by count and by bytes, so a rejectable command can
// refuse before it mutates anything; it reads true again as the drainer catches
// up; and Enqueue itself accepts past the bound either way, because what reaches
// it is either a mandatory completion or a command whose caller already checked.
func TestEventLogOutboxRoomFlipsAtItsBoundAndBack(t *testing.T) {
	t.Run("by count", func(t *testing.T) {
		l, w := newJournaledLog(t, EventLogOptions{})
		l.outboxMaxEvents = 4
		sending, atSend := sendingAt(primaryCap + 1)
		l.hooks = &logHooks{outboxSending: sending}
		fillPrimary(t, l)

		l.Enqueue(textEvent("1"), textEvent("2"), textEvent("3"))
		await(t, atSend, "the drainer to block on the full primary")
		if !l.OutboxRoom() {
			t.Fatal("OutboxRoom is false with 3 of 4 events in the outbox")
		}
		l.Enqueue(textEvent("4"))
		if l.OutboxRoom() {
			t.Fatal("OutboxRoom is true at the bound of 4 events")
		}
		// Past the bound, and still accepted: a mandatory completion is bounded
		// by state, not by this.
		l.Enqueue(textEvent("5"), textEvent("6"))
		if events, _, enq, _, _ := outboxState(l); events != 6 || enq != 6 {
			t.Fatalf("the outbox holds %d of %d enqueued events, want all 6 accepted", events, enq)
		}

		const want = primaryCap + 6
		primary := collectPrimary(l, want)
		if err := flushNow(t, l); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if !l.OutboxRoom() {
			t.Fatal("OutboxRoom is still false with the outbox drained")
		}
		await(t, primary, "the primary's events")
		closeLog(t, l, w)
		assertRun(t, "the journal", fileRecords(t, w), 1, want)
	})

	t.Run("by bytes", func(t *testing.T) {
		l, w := newJournaledLog(t, EventLogOptions{})
		big := textEvent(strings.Repeat("x", 40<<10))
		l.outboxMaxBytes = 64 << 10
		sending, atSend := sendingAt(primaryCap + 1)
		l.hooks = &logHooks{outboxSending: sending}
		fillPrimary(t, l)

		l.Enqueue(big)
		await(t, atSend, "the drainer to block on the full primary")
		if !l.OutboxRoom() {
			t.Fatal("OutboxRoom is false with one 40 KiB event under a 64 KiB bound")
		}
		l.Enqueue(big)
		if l.OutboxRoom() {
			t.Fatal("OutboxRoom is true with 80 KiB in the outbox under a 64 KiB bound")
		}
		if _, bytes, _, _, _ := outboxState(l); bytes < 80<<10 {
			t.Fatalf("the outbox counts %d bytes, want the two encoded records", bytes)
		}

		const want = primaryCap + 2
		primary := collectPrimary(l, want)
		if err := flushNow(t, l); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if !l.OutboxRoom() {
			t.Fatal("OutboxRoom is still false with the outbox drained")
		}
		if _, bytes, _, _, _ := outboxState(l); bytes != 0 {
			t.Fatalf("the drained outbox counts %d bytes", bytes)
		}
		await(t, primary, "the primary's events")
		closeLog(t, l, w)
		assertRun(t, "the journal", fileRecords(t, w), 1, want)
	})
}

// TestEventLogNoPrimaryPublishesWithNobodyReading is NoPrimary's criterion: with
// it set, a publisher never blocks however long nobody reads, nothing is ever
// put on Primary(), and every event still reaches the ring, every subscription
// and the journal — which is what Flush and Close mean by "committed" there too.
func TestEventLogNoPrimaryPublishesWithNobodyReading(t *testing.T) {
	const direct, enqueued = 1000, 12
	const total = direct + enqueued
	l, w := newJournaledLog(t, EventLogOptions{NoPrimary: true})
	sub := mustSubscribe(t, l, SubscribeOptions{MaxItems: total + 1})

	within(t, "1000 publishes with nobody reading", func() {
		for i := range direct {
			if !l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("d%d", i))) {
				t.Errorf("publish %d returned false on an open log", i)
				return
			}
		}
	})
	within(t, "TryPublish with nobody reading", func() {
		if !l.TryPublish(textEvent("tried")) {
			t.Error("TryPublish returned false with no primary to be full")
		}
	})
	for b := range enqueued / 3 {
		l.Enqueue(batchTexts(0, b, 3)...)
	}
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush on a NoPrimary log: %v", err)
	}
	if n := len(drainPrimary(l)); n != 0 {
		t.Fatalf("%d events reached Primary() on a NoPrimary log", n)
	}
	recs := readN(t, sub, total+1)
	assertRun(t, "the subscription", recs, 1, total+1)
	closeLog(t, l, w)
	assertRun(t, "the journal", fileRecords(t, w), 1, total+1)
	if h := l.Health(); h.DroppedAtClose != 0 || h.OutboxSkippedPrimary != 0 {
		t.Fatalf("health is %+v: nothing is dropped or skipped when there is no primary", h)
	}
}

// TestSessionLogCarriesOptionsNoPrimary: Options.NoPrimary reaches the log both
// sessions are built with, journal or no journal, and nothing else does. It is
// the whole of the plumbing between agent.Options and EventLogOptions, so it is
// checked where it is written rather than through a session that would have to
// be started to show it.
func TestSessionLogCarriesOptionsNoPrimary(t *testing.T) {
	for _, journaled := range []bool{false, true} {
		t.Run(fmt.Sprintf("journal=%v", journaled), func(t *testing.T) {
			for _, noPrimary := range []bool{false, true} {
				opts := Options{NoPrimary: noPrimary}
				if journaled {
					opts.JournalDir = t.TempDir()
				}
				l := newSessionLog(opts, journalHeader{provider: "test"})
				t.Cleanup(func() { l.Close(context.Background()) })
				if l.noPrimary != noPrimary {
					t.Fatalf("Options.NoPrimary %v built a log with noPrimary %v", noPrimary, l.noPrimary)
				}
				if journaled != (l.journal != nil) {
					t.Fatalf("a log for JournalDir %q has journal %v", opts.JournalDir, l.journal != nil)
				}
			}
		})
	}
}

// TestEventLogObserverRunsOncePerCommittedEventInSeqOrder is A3: the observer
// sees every committed event exactly once, in Seq order, with Seq set, whichever
// path committed it — Publish, TryPublish, the outbox, or the at-close commits —
// and never sees an abandoned publish or a TryPublish that found no room.
func TestEventLogObserverRunsOncePerCommittedEventInSeqOrder(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	var mu sync.Mutex
	var seen []Event
	if err := l.Observe(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev)
	}); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	observed := func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), seen...)
	}

	// Under the primary's cap, so no reader is needed for any of this.
	publishWithin(t, l, textEvent("a"))
	publishWithin(t, l, textEvent("b"))
	if !l.TryPublish(textEvent("c")) {
		t.Fatal("TryPublish with room returned false")
	}
	l.Enqueue(textEvent("d"), textEvent("e"))
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// An abandoned publish: its session's done is already closed.
	gone := make(chan struct{})
	close(gone)
	if l.Publish(context.Background(), gone, textEvent("abandoned")) {
		t.Fatal("a publish on a closed done returned true")
	}
	// A TryPublish that finds the boundary held: the test holds it itself.
	within(t, "TryPublish past a held boundary", func() {
		l.sem <- struct{}{}
		defer l.release()
		if l.TryPublish(textEvent("dropped")) {
			t.Error("TryPublish past a held boundary returned true")
		}
	})
	publishWithin(t, l, textEvent("f"))

	// Fill the primary and enqueue behind it, so the last two events are the
	// at-close path's: committed with a primary send it never takes.
	for i := len(observed()); i < primaryCap; i++ {
		publishWithin(t, l, textEvent(fmt.Sprintf("fill %d", i)))
	}
	sending, atSend := sendingAt(primaryCap + 1)
	l.hooks = &logHooks{outboxSending: sending}
	l.Enqueue(textEvent("x"), textEvent("y"))
	await(t, atSend, "the drainer to block on the full primary")
	closeLog(t, l, w)

	const want = primaryCap + 2
	got := observed()
	if len(got) != want {
		t.Fatalf("the observer ran %d times, want %d: once per committed event", len(got), want)
	}
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("the observer's call %d carried seq %d: out of order, or a seq twice", i, ev.Seq)
		}
		if ev.Text == "abandoned" || ev.Text == "dropped" {
			t.Fatalf("the observer saw %q, which was never committed", ev.Text)
		}
	}
	head := []string{got[0].Text, got[1].Text, got[2].Text, got[3].Text, got[4].Text, got[5].Text}
	if w := []string{"a", "b", "c", "d", "e", "f"}; !equalStrings(head, w) {
		t.Fatalf("the observer's first events are %v, want %v", head, w)
	}
	if tail := []string{got[want-2].Text, got[want-1].Text}; !equalStrings(tail, []string{"x", "y"}) {
		t.Fatalf("the observer's last events are %v, want the at-close commits", tail)
	}
	// The same sequence the journal recorded, event for event.
	recs := fileRecords(t, w)
	assertRun(t, "the journal", recs, 1, want)
	for i, text := range recordTexts(t, recs) {
		if text != got[i].Text {
			t.Fatalf("seq %d is %q in the journal and %q to the observer", recs[i].Seq, text, got[i].Text)
		}
	}
}

// TestEventLogAnObserverPanicLeavesTheLogUsable: a panic in the observer is a bug
// in craze's own code and the log does not recover it — it reaches the publisher
// — but it never leaves the log wedged. The boundary is released by a defer in
// every scope that holds it, so a caller that recovers finds a log that still
// publishes, still drains its outbox, and still closes. Without that defer the
// next publisher, the drainer and Close would all wait on a boundary nobody would
// ever give back.
func TestEventLogAnObserverPanicLeavesTheLogUsable(t *testing.T) {
	for _, path := range []string{"Publish", "TryPublish"} {
		t.Run(path, func(t *testing.T) {
			l, w := newJournaledLog(t, EventLogOptions{})
			var ran atomic.Int64
			if err := l.Observe(func(ev Event) {
				ran.Add(1)
				if ev.Text == "poison" {
					panic("agent test: an observer that panics")
				}
			}); err != nil {
				t.Fatalf("Observe: %v", err)
			}
			func() {
				defer func() {
					if r := recover(); r == nil {
						t.Fatal("the observer's panic did not reach the publisher: the log swallowed it")
					}
				}()
				if path == "Publish" {
					l.Publish(context.Background(), nil, textEvent("poison"))
					return
				}
				l.TryPublish(textEvent("poison"))
			}()

			// The event that panicked was committed — the observer runs last of
			// all — and everything after it works: a direct publish, the outbox,
			// and Close.
			publishWithin(t, l, textEvent("after"))
			l.Enqueue(textEvent("enqueued"))
			if err := flushNow(t, l); err != nil {
				t.Fatalf("Flush after the observer panicked: %v", err)
			}
			closeLog(t, l, w)
			recs := fileRecords(t, w)
			assertRun(t, "the journal", recs, 1, 3)
			if texts := recordTexts(t, recs); !equalStrings(texts, []string{"poison", "after", "enqueued"}) {
				t.Fatalf("the journal holds %v, want the panicking event and the two after it", texts)
			}
			if n := ran.Load(); n != 3 {
				t.Fatalf("the observer ran %d times, want once per committed event", n)
			}
			settleGoroutines(t, drainerFrame, 0)
		})
	}
}

// TestEventLogEnqueueNeverTouchesAnEventsErr: Enqueue's promise is that nothing
// it does under the caller's state lock runs foreign code, and Err is the one
// field whose encoding would. An event that arrives with Err set has it replaced
// by an inert sentinel without a single method being called on it, counted in
// Health and in the closing diag; the caller's own value keeps its error. After
// Close the same event is a counted drop, and the cut is read before anything is
// encoded, so the error is not touched then either.
func TestEventLogEnqueueNeverTouchesAnEventsErr(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	keepDrained(t, l)
	var touched atomic.Int64
	hostile := &hostileError{touched: &touched}
	ev := Event{Type: EventError, Text: "the turn failed", At: logTestTime, Err: hostile}
	within(t, "Enqueue with a hostile Err", func() { l.Enqueue(ev) })
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if n := touched.Load(); n != 0 {
		t.Fatalf("the enqueued event's Err was used %d times: Enqueue ran the caller's code", n)
	}
	if ev.Err != error(hostile) {
		t.Fatalf("Enqueue replaced the caller's own Err with %v", ev.Err)
	}
	if h := l.Health(); h.OutboxErrReplaced != 1 {
		t.Fatalf("health is %+v, want one replaced Err", h)
	}
	flushThrough(t, w, 1)
	recs := fileRecords(t, w)
	assertRun(t, "the journal", recs, 1, 1)
	got, err := recs[0].Event()
	if err != nil {
		t.Fatal(err)
	}
	if got.Err == nil || got.Err.Error() != errEnqueuedErr.Error() {
		t.Fatalf("the recorded event's Err is %v, want the inert sentinel's message", got.Err)
	}
	if got.Text != ev.Text {
		t.Fatalf("the recorded event's Text is %q, want %q: only Err is replaced", got.Text, ev.Text)
	}

	closeLog(t, l, w)
	late := Event{Type: EventError, At: logTestTime, Err: &hostileError{touched: &touched}}
	within(t, "Enqueue with a hostile Err after Close", func() { l.Enqueue(late) })
	if n := touched.Load(); n != 0 {
		t.Fatalf("a dropped event's Err was used %d times: the cut is read after the encoding", n)
	}
	if h := l.Health(); h.DroppedAtClose != 1 || h.OutboxErrReplaced != 1 {
		t.Fatalf("health is %+v, want the late event counted as dropped and not as replaced", h)
	}
	closing := assertClosingIsLast(t, fileLines(t, w))
	if closing["outboxErrReplaced"] != float64(1) {
		t.Fatalf("the closing diag says %v, want outboxErrReplaced 1", closing)
	}
}

// hostileError is the canary for Enqueue's callback-free promise: every method the
// codec could reach on an Event.Err — Error, and the chain-walking Is, As and
// Unwrap that classification uses — counts itself instead of doing anything, so a
// single touch is visible. It stands for the real hazard: an Error method that
// publishes, takes the caller's own lock, blocks or panics.
type hostileError struct{ touched *atomic.Int64 }

func (e *hostileError) Error() string {
	e.touched.Add(1)
	return "agent test: an error whose Error must never be called"
}
func (e *hostileError) Is(error) bool { e.touched.Add(1); return false }
func (e *hostileError) As(any) bool   { e.touched.Add(1); return false }
func (e *hostileError) Unwrap() error { e.touched.Add(1); return nil }

// TestEventLogASecondObserveIsAnError: the observer is one per log. A nil one is
// refused rather than read as "unset", a second is ErrObserverSet and does not
// replace the first, and one offered to a closed log is ErrClosed.
func TestEventLogASecondObserveIsAnError(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	var first, second atomic.Int64
	if err := l.Observe(nil); !errors.Is(err, errNilObserver) {
		t.Fatalf("Observe(nil) returned %v", err)
	}
	if err := l.Observe(func(Event) { first.Add(1) }); err != nil {
		t.Fatalf("the first Observe: %v", err)
	}
	if err := l.Observe(func(Event) { second.Add(1) }); !errors.Is(err, ErrObserverSet) {
		t.Fatalf("the second Observe returned %v, want ErrObserverSet", err)
	}
	if err := l.Observe(nil); !errors.Is(err, errNilObserver) {
		t.Fatalf("Observe(nil) after one was set returned %v", err)
	}
	publishWithin(t, l, textEvent("one"))
	if first.Load() != 1 || second.Load() != 0 {
		t.Fatalf("the first observer ran %d times and the second %d, want 1 and 0", first.Load(), second.Load())
	}
	closeLog(t, l, w)
	if err := l.Observe(func(Event) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Observe on a closed log returned %v, want ErrClosed", err)
	}
}

// TestEventLogOutboxUnderConcurrentPublishersFlushesAndClose is the outbox's
// race test: direct publishes, lossy publishes, batches enqueued under a state
// lock, Flushes, a reader that keeps the primary moving, and a Close in the
// middle of all of it.
//
// The middle is made exact rather than hoped for. Every producer runs its first
// phase freely, parks on a barrier, and runs its second phase only once Close has
// begun — released from inside Close, which then waits for them all — so the
// accepted set is exactly phase one and the refused set is exactly phase two, and
// Close provably overlaps the whole of the second. That makes every assertion an
// equality: every phase-one batch is committed **whole**, every phase-one publish
// is committed, nothing from phase two is in the record, and droppedAtClose is
// exactly phase two's events. A drainer that committed only a batch's first
// member, or an accepted batch it forgot, fails here.
func TestEventLogOutboxUnderConcurrentPublishersFlushesAndClose(t *testing.T) {
	const publishers, pubFirst, pubAfter = 3, 20, 10
	const enqueuers, enqFirst, enqAfter, batch = 3, 8, 4, 3
	const lossy, lossyFirst, lossyAfter = 2, 15, 10
	const afterCut = publishers*pubAfter + enqueuers*enqAfter*batch + lossy*lossyAfter
	l, w := newJournaledLog(t, EventLogOptions{})
	sub := mustSubscribe(t, l, SubscribeOptions{MaxItems: 4096})
	records, delivered := collectRecords(sub)

	// A reader that keeps reading but never runs ahead: the primary fills and
	// drains under the producers rather than being empty for all of them.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			select {
			case <-l.Primary():
				runtime.Gosched()
			case <-stop:
				return
			}
		}
	})

	// A state lock the enqueuers hold across their Enqueue, as the engine and
	// the registry will: the outbox mutex is a leaf under it.
	var state sync.Mutex
	var atBarrier, wg sync.WaitGroup
	release := make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) }) // a failure must strand nobody
	// Installed before anything publishes, as every hook must be: the drainer
	// reads l.hooks from its own goroutine. Close runs this once it has joined the
	// drainer — the cut is behind it, the outbox is committed — so the producers'
	// second phase runs entirely inside Close and entirely after the cut.
	l.hooks = &logHooks{outboxDrained: func() {
		released.Do(func() { close(release) })
		waitGroupWithin(t, &wg, "the producers' second phase, while Close ran")
	}}
	// producer runs first ops, parks until Close has begun, then runs after more.
	producer := func(first, after int, op func(i int)) {
		atBarrier.Add(1)
		wg.Go(func() {
			for i := range first {
				op(i)
			}
			atBarrier.Done()
			<-release
			for i := range after {
				op(first + i)
			}
		})
	}
	for p := range publishers {
		producer(pubFirst, pubAfter, func(i int) {
			l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("pub%d-%d", p, i)))
		})
	}
	for e := range enqueuers {
		producer(enqFirst, enqAfter, func(b int) {
			state.Lock()
			defer state.Unlock()
			l.Enqueue(batchTexts(e, b, batch)...)
		})
	}
	for k := range lossy {
		producer(lossyFirst, lossyAfter, func(i int) {
			l.TryPublish(textEvent(fmt.Sprintf("try%d-%d", k, i)))
		})
	}
	// The flushers are outside the barrier on purpose, so some of them are parked
	// on a batch when Close begins and answer from the at-close commits.
	var flushErrs sync.Map
	for f := range 2 {
		wg.Go(func() {
			for range 12 {
				err := l.Flush(context.Background(), nil)
				if err != nil && !errors.Is(err, ErrLogClosing) {
					flushErrs.Store(f, err)
					return
				}
			}
		})
	}

	// Every producer is parked with its second phase still to run; the outbox
	// holds whatever the drainer has not reached. Close now, and let them go from
	// inside it.
	waitDone(t, &atBarrier)
	within(t, "Close with every producer still working", func() { l.Close(context.Background()) })
	waitDone(t, &wg)
	close(stop)
	waitDone(t, &readers)
	flushErrs.Range(func(k, v any) bool {
		t.Fatalf("Flush %v returned %v, want nil or ErrLogClosing", k, v)
		return false
	})

	// The ring is the whole sequence here — far fewer events than it holds — and
	// Close leaves it alone, so it is what completeness is asserted against. A
	// subscription's records are a prefix of it: Close ends a subscription by
	// discarding what its owner had not delivered.
	ring := ringRecords(t, l)
	assertRun(t, "the ring", ring, 1, -1)
	byseq := map[uint64]string{}
	lastIndex := map[string]int{}
	type span struct{ first, members uint64 }
	batches := map[string]*span{}
	lastBatch := map[int]int{}
	for i, text := range recordTexts(t, ring) {
		seq := ring[i].Seq
		byseq[seq] = text
		var e, b, n int
		if _, err := fmt.Sscanf(text, "p%d-b%d-%d", &e, &b, &n); err == nil {
			if b >= enqFirst {
				t.Fatalf("enqueuer %d's batch %d reached seq %d: it was enqueued after the cut", e, b, seq)
			}
			// A batch stays whole and contiguous under every other producer.
			key := fmt.Sprintf("p%d-b%d", e, b)
			got := batches[key]
			if got == nil {
				if n != 0 {
					t.Fatalf("enqueuer %d's batch %d starts at its event %d", e, b, n)
				}
				batches[key] = &span{first: seq, members: 1}
				if prev, ok := lastBatch[e]; ok && b != prev+1 {
					t.Fatalf("enqueuer %d's batch %d began after its batch %d", e, b, prev)
				}
				lastBatch[e] = b
				continue
			}
			if want := got.first + uint64(n); seq != want {
				t.Fatalf("enqueuer %d's batch %d has its event %d at seq %d, want %d: something landed inside the batch",
					e, b, n, seq, want)
			}
			got.members++
			continue
		}
		who, n := text, -1
		if j := strings.LastIndexByte(text, '-'); j >= 0 {
			who = text[:j]
			if _, err := fmt.Sscanf(text[j+1:], "%d", &n); err != nil {
				t.Fatalf("seq %d is %q, which no producer wrote", seq, text)
			}
		}
		if prev, seen := lastIndex[who]; seen && n <= prev {
			t.Fatalf("%s's event %d reached seq %d after its event %d: a producer's order was broken", who, n, seq, prev)
		}
		lastIndex[who] = n
	}

	// Accepted == committed, exactly. Every batch enqueued before the cut is
	// there with every one of its members; every direct publish before the cut is
	// there; and nothing from after it is, so what the cut refused is precisely
	// what droppedAtClose counts.
	if len(batches) != enqueuers*enqFirst {
		t.Fatalf("%d batches reached the sequence, want the %d enqueued before the cut", len(batches), enqueuers*enqFirst)
	}
	for e := range enqueuers {
		for b := range enqFirst {
			key := fmt.Sprintf("p%d-b%d", e, b)
			got := batches[key]
			if got == nil {
				t.Fatalf("batch %s was accepted before the cut and never committed", key)
			}
			if got.members != batch {
				t.Fatalf("batch %s committed %d of its %d events: an accepted batch was truncated", key, got.members, batch)
			}
		}
	}
	committed := map[string]bool{}
	for _, text := range byseq {
		committed[text] = true
	}
	for p := range publishers {
		for i := range pubFirst {
			if text := fmt.Sprintf("pub%d-%d", p, i); !committed[text] {
				t.Fatalf("%q returned from Publish on an open log and is not in the record", text)
			}
		}
		for i := pubFirst; i < pubFirst+pubAfter; i++ {
			if text := fmt.Sprintf("pub%d-%d", p, i); committed[text] {
				t.Fatalf("%q was published after the cutoff and reached the record", text)
			}
		}
	}
	for k := range lossy {
		for i := lossyFirst; i < lossyFirst+lossyAfter; i++ {
			if text := fmt.Sprintf("try%d-%d", k, i); committed[text] {
				t.Fatalf("%q was tried after the cutoff and reached the record", text)
			}
		}
	}
	if got := l.Health().DroppedAtClose; got != afterCut {
		t.Fatalf("droppedAtClose is %d, want the %d events every producer issued after the cut", got, afterCut)
	}

	recs := await(t, records, "the subscription's records")
	assertRun(t, "the subscription", recs, 1, -1)
	for i, text := range recordTexts(t, recs) {
		if want := byseq[recs[i].Seq]; text != want {
			t.Fatalf("seq %d is %q in the subscription and %q in the ring", recs[i].Seq, text, want)
		}
	}
	if n := delivered.Load(); n != int64(len(recs)) {
		t.Fatalf("the collector counted %d records and returned %d", n, len(recs))
	}

	// The journal recorded the same events in the same order, as far as it got:
	// a writer that cannot keep up sheds into gap lines rather than reorder.
	if err := writerExited(w); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("waiting for the journal writer: %v", err)
	}
	file := fileRecords(t, w)
	var prev uint64
	for i, text := range recordTexts(t, file) {
		seq := file[i].Seq
		if seq <= prev {
			t.Fatalf("the journal has seq %d after %d: its order is not the ring's", seq, prev)
		}
		prev = seq
		if want, ok := byseq[seq]; ok && text != want {
			t.Fatalf("seq %d is %q in the journal and %q in the ring", seq, text, want)
		}
	}
	settleGoroutines(t, drainerFrame, 0)
}

// BenchmarkEventLogPublishObserved is V7: S1a's publish benchmark with an
// observer set, so the two runs side by side say what the observer costs on the
// hot path. Budget, as in V7: a text delta with the journal attached under 5 µs.
func BenchmarkEventLogPublishObserved(b *testing.B) {
	ev := Event{Type: EventText, Text: "Here is the next chunk of the answer, ", At: time.Now()}
	for _, journaled := range []bool{false, true} {
		b.Run(fmt.Sprintf("TextDelta/journal=%v", journaled), func(b *testing.B) {
			var w *journal.Writer
			inc := ""
			if journaled {
				w, inc = newTestJournal(b)
			}
			l := newTestLog(b, EventLogOptions{Journal: w, Incarnation: inc})
			var seen uint64
			if err := l.Observe(func(ev Event) { seen = ev.Seq }); err != nil {
				b.Fatal(err)
			}
			keepDrained(b, l)
			b.ReportAllocs()
			for b.Loop() {
				if !l.Publish(context.Background(), nil, ev) {
					b.Fatal("Publish returned false")
				}
			}
			b.StopTimer()
			if seen == 0 {
				b.Fatal("the observer never ran")
			}
			closeLog(b, l, w)
			if w != nil {
				b.ReportMetric(float64(w.Health().DroppedEvents)/float64(b.N), "journal-drops/op")
			}
		})
	}
}
