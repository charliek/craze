package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/journal"
)

// The tests of a subscription's cutoff and of its closing tail (plan 027 §3.4,
// §3.7, A21). Like the rest of the log's tests they are driven by the log's own
// barriers — the hooks, a channel a goroutine closed, a reader that stops — and
// the only clock is the watchdog.

// selfNamed fails unless every record's text is its own seq, which is how
// these tests publish: a record that came from the wrong place, or twice, says
// so.
func selfNamed(t *testing.T, what string, recs []Record) {
	t.Helper()
	for i, text := range recordTexts(t, recs) {
		if text != fmt.Sprint(recs[i].Seq) {
			t.Fatalf("%s: seq %d carries %q", what, recs[i].Seq, text)
		}
	}
}

// publishRun publishes the events from, from+1, …, to, each carrying its own
// seq as its text.
func publishRun(t *testing.T, l *EventLog, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		publishWithin(t, l, textEvent(fmt.Sprint(i)))
	}
}

// sendingAtSeq is a sending hook that closes the returned channel when the
// owner is about to block handing the record with seq to Records.
func sendingAtSeq(seq uint64) (func(uint64), <-chan struct{}) {
	blocked := make(chan struct{})
	var once sync.Once
	return func(s uint64) {
		if s == seq {
			once.Do(func() { close(blocked) })
		}
	}, blocked
}

// liveLen is how many records s's live buffer holds, read under its mutex.
func liveLen(s *Subscription) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// closedRest is s's Rest once its log's Close has returned. By then the owner
// has stopped — Close waits for it — so Records must be closed with nothing on
// it, and Err must be ErrClosed: what the owner did not deliver is Rest's.
func closedRest(t *testing.T, s *Subscription) ([]Record, error) {
	t.Helper()
	select {
	case r, ok := <-s.Records():
		if ok {
			t.Fatalf("seq %d was delivered after the log's Close returned", r.Seq)
		}
	default:
		t.Fatal("Records is still open after the log's Close returned")
	}
	if err := s.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("the subscription ended with %v, want ErrClosed", err)
	}
	return s.Rest()
}

// mustRest is closedRest that must succeed.
func mustRest(t *testing.T, s *Subscription) []Record {
	t.Helper()
	rest, err := closedRest(t, s)
	if err != nil {
		t.Fatalf("Rest after the log's Close: %v", err)
	}
	return rest
}

// TestCutoffIsTheHeadAtRegistration: Cutoff is the last seq committed when the
// subscription registered, read inside the boundary — 0 on a log that had
// committed nothing, the head for a live-only subscription (live delivery begins
// after it) whatever a publisher does next, and the head again for a cursor
// subscription, whose replay runs to it: the record with that seq ends the
// catch-up. A cursor at the head has nothing to replay, and its cutoff says so
// by equalling its start.
func TestCutoffIsTheHeadAtRegistration(t *testing.T) {
	t.Run("a log with nothing published", func(t *testing.T) {
		l := newTestLog(t, EventLogOptions{})
		live := mustSubscribe(t, l, SubscribeOptions{})
		cursor := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
		if live.Cutoff() != 0 || cursor.Cutoff() != 0 {
			t.Fatalf("cutoffs on an empty log are %d (live) and %d (cursor), want 0", live.Cutoff(), cursor.Cutoff())
		}
		publishRun(t, l, 1, 2)
		assertRun(t, "the live-only subscription", readN(t, live, 2), 1, 2)
		if live.Cutoff() != 0 {
			t.Fatalf("the cutoff moved to %d with the stream", live.Cutoff())
		}
	})

	t.Run("live only, with a publisher mid-run", func(t *testing.T) {
		const total, hold = 20, 7
		l := newTestLog(t, EventLogOptions{})
		p := publishHeldAt(t, l, total, hold)
		s := mustSubscribe(t, l, SubscribeOptions{})
		if head := lastSeq(t, l); s.Cutoff() != head || head != hold {
			t.Fatalf("the cutoff is %d, the head at registration %d, want both %d", s.Cutoff(), head, hold)
		}
		p.release()
		recs := readN(t, s, total-hold)
		assertRun(t, "live delivery", recs, s.Cutoff()+1, total-hold)
		if s.Cutoff() != hold {
			t.Fatalf("the cutoff moved to %d with the stream", s.Cutoff())
		}
	})

	t.Run("a cursor", func(t *testing.T) {
		const published, after = 10, 4
		l := newTestLog(t, EventLogOptions{})
		publishRun(t, l, 1, published)
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}})
		if s.Cutoff() != published {
			t.Fatalf("a cursor subscription's cutoff is %d, want the head %d", s.Cutoff(), published)
		}
		replay := readN(t, s, published-after)
		assertRun(t, "the replay", replay, after+1, published-after)
		if last := replay[len(replay)-1].Seq; last != s.Cutoff() {
			t.Fatalf("the replay ends at seq %d, want the cutoff %d", last, s.Cutoff())
		}

		atHead := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: published}})
		if atHead.Cutoff() != published {
			t.Fatalf("a cursor at the head has cutoff %d, want its own start %d", atHead.Cutoff(), published)
		}
		publishWithin(t, l, textEvent(fmt.Sprint(published+1)))
		assertRun(t, "a cursor at the head", readN(t, atHead, 1), published+1, 1)
	})
}

// TestRestKeepsTheRecordTheOwnerWasSending is A21 with the owner blocked in a
// pinned send: the reader takes three of a ten-record replay and stops, the
// owner blocks handing over the fourth, and three live records gather behind
// the pin. The log's Close ends it without a reader, and Rest is the record the
// owner was sending, then the rest of its pin, then its live buffer: 4 to 13,
// exactly the undelivered tail.
func TestRestKeepsTheRecordTheOwnerWasSending(t *testing.T) {
	const published, read, live = 10, 3, 3
	l := newTestLog(t, EventLogOptions{})
	hook, blocked := sendingAtSeq(read + 1)
	l.hooks = &logHooks{sending: hook}
	keepDrained(t, l)
	publishRun(t, l, 1, published)
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
	assertRun(t, "what the reader took", readN(t, s, read), 1, read)
	await(t, blocked, "the owner to block handing over the next pinned record")
	publishRun(t, l, published+1, published+live)
	// The owner is in its pinned phase, so the live records are all buffered.
	if n := liveLen(s); n != live {
		t.Fatalf("the live buffer holds %d records, want %d", n, live)
	}

	closeLog(t, l, nil)
	rest := mustRest(t, s)
	assertRun(t, "the rest", rest, read+1, published+live-read)
	selfNamed(t, "the rest", rest)
	if n := l.Health().SubscribersDropped; n != 0 {
		t.Fatalf("%d subscriptions dropped: this case is about the log's Close", n)
	}
}

// TestRestIsTheWholeUndeliveredTail is A21 with the owner blocked in a live
// send: held after handing over record 1, it takes 2 to 5 as one batch, blocks
// handing over 2, and 6 to 8 gather in its live buffer. Rest is the record it
// was sending, the rest of its batch and its live buffer, in order and
// contiguous after the last record delivered: 2 to 8.
func TestRestIsTheWholeUndeliveredTail(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	handed, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(release) // before the log's Close, which waits for the owner
	hook, blocked := sendingAtSeq(2)
	l.hooks = &logHooks{
		sending: hook,
		delivered: func(seq uint64) {
			if seq == 1 {
				close(handed)
				<-resume
			}
		},
	}
	keepDrained(t, l)
	s := mustSubscribe(t, l, SubscribeOptions{})
	publishRun(t, l, 1, 1)
	assertRun(t, "the first record", readN(t, s, 1), 1, 1)
	await(t, handed, "the owner to hand the first record over")
	publishRun(t, l, 2, 5) // the owner's next batch
	release()
	await(t, blocked, "the owner to block handing over the first of its batch")
	if n := liveLen(s); n != 0 {
		t.Fatalf("the live buffer holds %d records: the owner did not take 2 to 5 as its batch", n)
	}
	publishRun(t, l, 6, 8)
	if n := liveLen(s); n != 3 {
		t.Fatalf("the live buffer holds %d records, want 6 to 8", n)
	}

	closeLog(t, l, nil)
	rest := mustRest(t, s)
	assertRun(t, "the rest", rest, 2, 7)
	selfNamed(t, "the rest", rest)
}

// TestRestOfASubscriptionThatDeliveredNothing: a subscription whose reader never
// took a record has, as its tail, everything after its start — the cursor for a
// resume, the cutoff for a live-only one — and one with nothing after its start
// has an empty tail, which is not an error. An owner the scheduler had not run
// when the log closed (held before it delivers anything, until the end) is the
// same: its whole pin and its whole live buffer.
func TestRestOfASubscriptionThatDeliveredNothing(t *testing.T) {
	t.Run("a cursor", func(t *testing.T) {
		const published, after = 10, 4
		l := newTestLog(t, EventLogOptions{})
		keepDrained(t, l)
		publishRun(t, l, 1, published)
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}})
		publishRun(t, l, published+1, published+2)
		closeLog(t, l, nil)
		rest := mustRest(t, s)
		assertRun(t, "the rest", rest, after+1, published+2-after)
		selfNamed(t, "the rest", rest)
	})

	t.Run("live only", func(t *testing.T) {
		l := newTestLog(t, EventLogOptions{})
		keepDrained(t, l)
		publishRun(t, l, 1, 3)
		s := mustSubscribe(t, l, SubscribeOptions{})
		publishRun(t, l, 4, 6)
		closeLog(t, l, nil)
		rest := mustRest(t, s)
		assertRun(t, "the rest", rest, s.Cutoff()+1, 3)
		selfNamed(t, "the rest", rest)
	})

	t.Run("live only, nothing after it", func(t *testing.T) {
		l := newTestLog(t, EventLogOptions{})
		keepDrained(t, l)
		publishRun(t, l, 1, 3)
		s := mustSubscribe(t, l, SubscribeOptions{})
		closeLog(t, l, nil)
		if rest := mustRest(t, s); len(rest) != 0 {
			t.Fatalf("an empty tail is %d records", len(rest))
		}
	})

	t.Run("an owner the scheduler had not run", func(t *testing.T) {
		const published, after = 10, 4
		l := newTestLog(t, EventLogOptions{})
		ran := make(chan struct{})
		l.hooks = &logHooks{ownerStarts: func(kill <-chan struct{}) {
			<-kill
			close(ran)
		}}
		keepDrained(t, l)
		publishRun(t, l, 1, published)
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}})
		publishRun(t, l, published+1, published+2)
		closeLog(t, l, nil)
		await(t, ran, "the held owner to run")
		rest := mustRest(t, s)
		assertRun(t, "the rest", rest, after+1, published+2-after)
		selfNamed(t, "the rest", rest)
	})
}

// TestRestBeyondTheRing: Rest answers from what the subscription itself holds,
// never from the ring. With a budget far larger than the ring's bounds — by
// count, and by bytes — a subscription pins the whole ring, blocks handing over
// its first record, and buffers ten times the ring's worth more, while the ring
// evicts what it holds. Rest still starts at seq 1, which the ring no longer
// has.
func TestRestBeyondTheRing(t *testing.T) {
	const ring, total = 8, 100
	body, err := EncodeEvent(textEvent(fmt.Sprint(total)))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		lo   EventLogOptions
		so   SubscribeOptions
	}{
		{"by count", EventLogOptions{RingEvents: ring}, SubscribeOptions{MaxItems: 10 * total}},
		{"by bytes", EventLogOptions{RingBytes: ring * len(body)}, SubscribeOptions{MaxItems: 10 * total, MaxBytes: 100 * total * len(body)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newTestLog(t, c.lo)
			hook, blocked := sendingAtSeq(1)
			l.hooks = &logHooks{sending: hook}
			keepDrained(t, l)
			publishRun(t, l, 1, ring)
			c.so.After = &Cursor{Incarnation: l.Incarnation()}
			s := mustSubscribe(t, l, c.so)
			await(t, blocked, "the owner to block handing over its first pinned record")
			publishRun(t, l, ring+1, total)

			closeLog(t, l, nil)
			held := ringSeqs(t, l)
			if len(held) == 0 || held[0] <= ring || len(held) > ring {
				t.Fatalf("the ring holds %v: it must have evicted the whole pin", held)
			}
			rest := mustRest(t, s)
			assertRun(t, "the rest", rest, 1, total)
			selfNamed(t, "the rest", rest)
		})
	}
}

// TestRestAfterAPartialJournalReplayIsUnavailable: a subscription whose replay
// was reading its head from the journal file when the log closed has no whole
// tail to give — the rest of its head is in a file it no longer reads — so Rest
// is ErrRestUnavailable, never the suffix it does hold presented as the tail.
// That holds part-way through the file, while it waits for a stalled writer,
// and before the leg has begun.
func TestRestAfterAPartialJournalReplayIsUnavailable(t *testing.T) {
	// The ring keeps the last four of the sixty published, so the head of a
	// replay from the start is 56 records read from the file.
	const published, ring, read = 60, 4, 3

	t.Run("part-way through the file", func(t *testing.T) {
		l, w := newJournaledLog(t, EventLogOptions{RingEvents: ring})
		hook, blocked := sendingAtSeq(read + 1)
		l.hooks = &logHooks{sending: hook}
		keepDrained(t, l)
		publishRun(t, l, 1, published)
		flushThrough(t, w, published)
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
		assertRun(t, "what the file leg delivered", readN(t, s, read), 1, read)
		await(t, blocked, "the owner to block handing over the next record from the file")
		publishRun(t, l, published+1, published+2)

		closeLog(t, l, w)
		if rest, err := closedRest(t, s); !errors.Is(err, ErrRestUnavailable) || rest != nil {
			t.Fatalf("Rest after a partial journal replay: %d records, %v; want ErrRestUnavailable", len(rest), err)
		}
	})

	t.Run("waiting for a stalled writer", func(t *testing.T) {
		l, w := newJournaledLog(t, EventLogOptions{RingEvents: ring})
		keepDrained(t, l)
		stalled := holdJournal(t, l, true)
		publishRun(t, l, 1, published)
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
		await(t, stalled.waiting, "the owner to wait for the stalled journal")

		closeLog(t, l, w)
		if rest, err := closedRest(t, s); !errors.Is(err, ErrRestUnavailable) || rest != nil {
			t.Fatalf("Rest with the journal wait outstanding: %d records, %v; want ErrRestUnavailable", len(rest), err)
		}
	})

	t.Run("before the leg began", func(t *testing.T) {
		l, w := newJournaledLog(t, EventLogOptions{RingEvents: ring})
		l.hooks = &logHooks{ownerStarts: func(kill <-chan struct{}) { <-kill }}
		keepDrained(t, l)
		publishRun(t, l, 1, published)
		flushThrough(t, w, published)
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})

		closeLog(t, l, w)
		if rest, err := closedRest(t, s); !errors.Is(err, ErrRestUnavailable) || rest != nil {
			t.Fatalf("Rest with the journal leg not begun: %d records, %v; want ErrRestUnavailable", len(rest), err)
		}
	})
}

// TestADetachHasNoRest: a subscription its own Close ended — a detach — ends
// with ErrClosed exactly as one its log's Close ended, and Rest tells the two
// apart: a reader that detached asked for nothing more, so there is no tail,
// ErrNoRest — and the log closing afterwards does not make one, since the
// detach was the first cause.
func TestADetachHasNoRest(t *testing.T) {
	const published, read = 10, 2
	l := newTestLog(t, EventLogOptions{})
	hook, blocked := sendingAtSeq(read + 1)
	l.hooks = &logHooks{sending: hook}
	keepDrained(t, l)
	publishRun(t, l, 1, published)
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
	assertRun(t, "what the reader took", readN(t, s, read), 1, read)
	await(t, blocked, "the owner to block handing over the next record")
	publishRun(t, l, published+1, published+2)

	s.Close()
	readAll(t, s)
	if err := s.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("a detach ended with %v, want ErrClosed", err)
	}
	if rest, err := s.Rest(); !errors.Is(err, ErrNoRest) || rest != nil {
		t.Fatalf("Rest after a detach: %d records, %v; want ErrNoRest", len(rest), err)
	}
	closeLog(t, l, nil)
	if rest, err := s.Rest(); !errors.Is(err, ErrNoRest) || rest != nil {
		t.Fatalf("Rest after a detach and then the log's Close: %d records, %v; want ErrNoRest", len(rest), err)
	}
}

// TestAnyOtherEndingHasNoRest: only the log's Close leaves a tail. A slow
// consumer, a replay the journal could not serve, and a hole in what the owner
// was handed each end the subscription with their own error and Rest is
// ErrNoRest — before the log closes and after, since the first cause stands.
func TestAnyOtherEndingHasNoRest(t *testing.T) {
	noRest := func(t *testing.T, l *EventLog, s *Subscription, want error) {
		t.Helper()
		readAll(t, s)
		if err := s.Err(); !errors.Is(err, want) {
			t.Fatalf("the subscription ended with %v, want %v", err, want)
		}
		if rest, err := s.Rest(); !errors.Is(err, ErrNoRest) || rest != nil {
			t.Fatalf("Rest: %d records, %v; want ErrNoRest", len(rest), err)
		}
		closeLog(t, l, nil)
		if rest, err := s.Rest(); !errors.Is(err, ErrNoRest) || rest != nil {
			t.Fatalf("Rest after the log's Close: %d records, %v; want ErrNoRest", len(rest), err)
		}
	}

	t.Run("a slow consumer", func(t *testing.T) {
		l := newTestLog(t, EventLogOptions{})
		keepDrained(t, l)
		s := mustSubscribe(t, l, SubscribeOptions{MaxItems: 2})
		publishRun(t, l, 1, 6)
		if n := l.Health().SubscribersDropped; n != 1 {
			t.Fatalf("%d subscriptions dropped, want the one that never read", n)
		}
		noRest(t, l, s, ErrSlowConsumer)
	})

	t.Run("a replay the journal could not serve", func(t *testing.T) {
		const published, ring = 60, 4
		l, w := newJournaledLog(t, EventLogOptions{RingEvents: ring})
		keepDrained(t, l)
		publishRun(t, l, 1, published)
		flushThrough(t, w, published)
		l.file = &answeringJournal{journalFile: l.file, readErr: fmt.Errorf("%w: no header", journal.ErrMalformed), deliver: 3}
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
		readAll(t, s)
		assertUnresolvable(t, "the file leg", s.Err(), CursorEvicted)
		noRest(t, l, s, s.Err())
	})

	t.Run("a hole", func(t *testing.T) {
		l := newTestLog(t, EventLogOptions{})
		body := func(n int) string { return fmt.Sprintf(`{"type":"text","text":"%d"}`, n) }
		pinned := []Record{{Seq: 11, Type: EventText, Body: body(11)}, {Seq: 13, Type: EventText, Body: body(13)}}
		s := l.newSubscription(defaultSubscribeItems, defaultSubscribeBytes, 0)
		l.startOwner(s, nil, pinned, 10)
		noRest(t, l, s, errNotContiguous)
	})
}

// TestRestNeverHandsOverAHole: the tail is checked as send checks a record, one
// seq after another from the last delivered, and a hole in it — which nothing
// in the log produces — is an error, never a partial tail. The owner here is
// handed a pin with a hole and held before it delivers anything, so the log's
// Close ends it with the hole still in hand.
func TestRestNeverHandsOverAHole(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	l.hooks = &logHooks{ownerStarts: func(kill <-chan struct{}) { <-kill }}
	body := func(n int) string { return fmt.Sprintf(`{"type":"text","text":"%d"}`, n) }
	pinned := []Record{{Seq: 11, Type: EventText, Body: body(11)}, {Seq: 13, Type: EventText, Body: body(13)}}
	s := l.newSubscription(defaultSubscribeItems, defaultSubscribeBytes, 0)
	within(t, "registering the subscription", func() {
		l.sem <- struct{}{}
		defer l.release()
		l.subs = append(l.subs, s)
		l.startOwner(s, nil, pinned, 10)
	})

	closeLog(t, l, nil)
	if rest, err := closedRest(t, s); !errors.Is(err, errNotContiguous) || rest != nil {
		t.Fatalf("Rest of a tail with a hole: %d records, %v; want the contiguity error", len(rest), err)
	}
}

// TestRestIsErrNoRestUntilTheOwnerHasFinished: Rest is meaningful once Records
// has closed. A running subscription has none, and neither does one the log's
// Close has already ended whose owner has not stopped yet — held here, at its
// start, until the test lets it go. Once it has, Rest is the tail.
func TestRestIsErrNoRestUntilTheOwnerHasFinished(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	killed, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(release) // before the log's Close, which waits for the owner
	l.hooks = &logHooks{ownerStarts: func(kill <-chan struct{}) {
		<-kill
		close(killed)
		<-resume
	}}
	keepDrained(t, l)
	s := mustSubscribe(t, l, SubscribeOptions{})
	publishRun(t, l, 1, 3)
	if rest, err := s.Rest(); !errors.Is(err, ErrNoRest) || rest != nil {
		t.Fatalf("Rest of a running subscription: %d records, %v; want ErrNoRest", len(rest), err)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		l.Close(context.Background())
	}()
	await(t, killed, "the log's Close to end the subscription")
	if rest, err := s.Rest(); !errors.Is(err, ErrNoRest) || rest != nil {
		t.Fatalf("Rest with the owner still running: %d records, %v; want ErrNoRest", len(rest), err)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err is %v before the owner has stopped", err)
	}
	release()
	await(t, closed, "the log's Close")
	rest := mustRest(t, s)
	assertRun(t, "the rest", rest, 1, 3)
	selfNamed(t, "the rest", rest)
}

// TestRestIsACopyEveryTime: Rest may be called more than once and from any
// goroutine, and each call hands out records of the caller's own — the slice
// and every Omitted — so what one caller does to its answer reaches no other.
// The records here are all omitted (a body cap nothing fits), which is where a
// shared pointer would show.
func TestRestIsACopyEveryTime(t *testing.T) {
	const n, callers = 4, 8
	l := newTestLog(t, EventLogOptions{MaxRecordBytes: 8})
	keepDrained(t, l)
	s := mustSubscribe(t, l, SubscribeOptions{})
	publishRun(t, l, 1, n)
	closeLog(t, l, nil)

	answers := make([][]Record, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			rest, err := s.Rest()
			if err != nil {
				t.Errorf("caller %d: Rest: %v", i, err)
			}
			answers[i] = rest
		})
	}
	waitDone(t, &wg)
	for i, rest := range answers {
		assertRun(t, fmt.Sprintf("caller %d's rest", i), rest, 1, n)
		for _, r := range rest {
			if r.Omitted == nil {
				t.Fatalf("caller %d: seq %d has a body; this case needs omitted records", i, r.Seq)
			}
		}
	}
	// One caller spoils its answer every way it can.
	for i := range answers[0] {
		answers[0][i].Omitted.Bytes = -1
		answers[0][i].Omitted.Reason = "spoiled"
	}
	answers[0][0] = Record{}
	again := mustRest(t, s)
	assertRun(t, "a later rest", again, 1, n)
	for _, rest := range [][]Record{again, answers[1]} {
		for _, r := range rest {
			if r.Omitted.Bytes <= 0 || r.Omitted.Reason == "spoiled" {
				t.Fatalf("seq %d's Omitted is %+v: another caller's answer reached it", r.Seq, *r.Omitted)
			}
		}
	}
}
