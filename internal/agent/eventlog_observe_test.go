package agent

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/journal"
)

// countedError counts every call of its Error, and carries a JSON-RPC error in
// its chain, so the class and code the codec gives it (rpc, 42) are the
// chain-walk's, not anything of its own.
type countedError struct {
	msg   string
	calls *atomic.Int64
}

func (e *countedError) Error() string {
	e.calls.Add(1)
	return e.msg
}

func (e *countedError) Unwrap() error { return &acp.RPCError{Code: 42, Message: "inner"} }

// observing installs an observer that keeps every event it is handed, and
// returns a reader of what it kept.
func observing(t *testing.T, l *EventLog) func() []Event {
	t.Helper()
	var mu sync.Mutex
	var seen []Event
	if err := l.Observe(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev)
	}); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), seen...)
	}
}

// observedErrors is the Err of every observed event that carried one, each
// required to be a *RemoteError.
func observedErrors(t *testing.T, evs []Event) []*RemoteError {
	t.Helper()
	var out []*RemoteError
	for _, ev := range evs {
		if ev.Err == nil {
			continue
		}
		remote, ok := ev.Err.(*RemoteError)
		if !ok || remote == nil {
			t.Fatalf("the observer was handed seq %d's Err as %T (%p), want a *RemoteError", ev.Seq, ev.Err, ev.Err)
		}
		out = append(out, remote)
	}
	return out
}

// TestEventLogTheObserverIsHandedTheCodecsError (r2 findings 1 and 3): on every
// path that commits — Publish, TryPublish, the outbox and its at-close commits —
// the observer's Err is the *RemoteError the encoding built before the
// boundary (the message, the class and the code a subscriber decodes), never
// the publisher's value; the primary still gets the publisher's value; and the
// publisher's Error is called exactly once per publish, by the encoding, so
// nothing reads it under the boundary. The outbox never carries a caller's
// error at all: it substituted its inert sentinel (Enqueue), and that is the
// RemoteError its observer sees.
func TestEventLogTheObserverIsHandedTheCodecsError(t *testing.T) {
	want := &RemoteError{Message: "the turn failed", Class: EventErrRPC, Code: 42}

	for _, path := range []string{"Publish", "TryPublish"} {
		t.Run(path, func(t *testing.T) {
			l, w := newJournaledLog(t, EventLogOptions{})
			observed := observing(t, l)
			var calls atomic.Int64
			orig := &countedError{msg: "the turn failed", calls: &calls}
			ev := Event{Type: EventError, Err: orig, At: logTestTime}
			if path == "Publish" {
				publishWithin(t, l, ev)
			} else if !l.TryPublish(ev) {
				t.Fatal("TryPublish with room returned false")
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("the publisher's Error was called %d times for one %s, want exactly once (the encoding's)", n, path)
			}
			if got := observedErrors(t, observed()); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("the observer was handed %+v, want %+v", got, want)
			}
			if evs := drainPrimary(l); len(evs) != 1 || evs[0].Err != error(orig) {
				t.Fatalf("the primary got %+v, want the publisher's own error value", evs)
			}
			// What the observer holds is what a subscriber decodes from the record.
			recs := ringRecords(t, l)
			if len(recs) != 1 {
				t.Fatalf("the ring holds %d records", len(recs))
			}
			decoded, err := recs[0].Event()
			if err != nil || !reflect.DeepEqual(decoded.Err, error(want)) {
				t.Fatalf("the record decodes to %+v (%v), the observer was handed %+v", decoded.Err, err, want)
			}
			closeLog(t, l, w)
			if n := calls.Load(); n != 1 {
				t.Fatalf("the publisher's Error was called %d times by the end, want once", n)
			}
		})
	}

	t.Run("the outbox and its at-close commits", func(t *testing.T) {
		l, w := newJournaledLog(t, EventLogOptions{})
		observed := observing(t, l)
		var calls atomic.Int64
		errEvent := func(what string) Event {
			return Event{Type: EventError, Text: what, Err: &countedError{msg: "never read", calls: &calls}, At: logTestTime}
		}
		l.Enqueue(errEvent("drained"))
		if err := flushNow(t, l); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		// Fill the primary and enqueue behind it, so the last event is the
		// at-close path's: committed with a primary send it never takes.
		for i := len(observed()); i < primaryCap; i++ {
			publishWithin(t, l, textEvent(fmt.Sprintf("fill %d", i)))
		}
		sending, atSend := sendingAt(primaryCap + 1)
		l.hooks = &logHooks{outboxSending: sending}
		l.Enqueue(errEvent("at close"))
		await(t, atSend, "the drainer to block on the full primary")
		closeLog(t, l, w)

		if n := calls.Load(); n != 0 {
			t.Fatalf("an enqueued event's Err was read %d times", n)
		}
		evs := observed()
		if len(evs) != primaryCap+1 || evs[0].Text != "drained" || evs[primaryCap].Text != "at close" {
			t.Fatalf("the observer saw %d events, want the drained one first and the at-close one last", len(evs))
		}
		sentinel := &RemoteError{Message: errEnqueuedErr.Error(), Class: EventErrOther}
		got := observedErrors(t, evs)
		if len(got) != 2 || !reflect.DeepEqual(got[0], sentinel) || !reflect.DeepEqual(got[1], sentinel) {
			t.Fatalf("the observer was handed %+v, want the outbox sentinel's RemoteError twice", got)
		}
		if first := drainPrimary(l); len(first) == 0 || first[0].Err != errEnqueuedErr {
			t.Fatal("the primary did not get the outbox's own sentinel for the drained event")
		}
	})
}

// TestEventLogTheObserversErrorSurvivesAnOmittedRecord: the observer is handed
// a *RemoteError even when no subscriber could decode one — a body over
// MaxRecordBytes, a type too long to record, or no type at all — and the
// publisher's Error is still called exactly once: by the encoding when it ran,
// and by record in its place when it did not.
func TestEventLogTheObserversErrorSurvivesAnOmittedRecord(t *testing.T) {
	long := strings.Repeat("m", 4096) + " and a bad byte \xff"
	for _, tc := range []struct {
		name   string
		typ    EventType
		msg    string
		reason string
	}{
		{name: "a body over MaxRecordBytes", typ: EventError, msg: long, reason: journal.OmittedOversized},
		{name: "a type too long to record", typ: EventType(strings.Repeat("x", journal.MaxEventTypeBytes+1)), msg: "overlong", reason: journal.OmittedEncodeError},
		{name: "no type at all", typ: "", msg: "untyped", reason: journal.OmittedEncodeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLog(t, EventLogOptions{MaxRecordBytes: 1024})
			observed := observing(t, l)
			var calls atomic.Int64
			publishWithin(t, l, Event{Type: tc.typ, Err: &countedError{msg: tc.msg, calls: &calls}, At: logTestTime})
			if n := calls.Load(); n != 1 {
				t.Fatalf("the publisher's Error was called %d times, want exactly once", n)
			}
			recs := ringRecords(t, l)
			if len(recs) != 1 || recs[0].Omitted == nil || recs[0].Omitted.Reason != tc.reason {
				t.Fatalf("the record is %+v, want one omitted as %s", recs, tc.reason)
			}
			want := &RemoteError{Message: asJSONCarriesIt(tc.msg), Class: EventErrRPC, Code: 42}
			if got := observedErrors(t, observed()); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("the observer was handed %+v, want %+v", got, want)
			}
		})
	}
}

// TestEventLogTheObserversMessageIsWhatASubscriberDecodes: a message that is
// not valid UTF-8 reaches a subscriber with U+FFFD in place of each bad byte,
// because that is what the JSON carries; the observer's copy has the same
// replacement, so a model folded from either accounts the same bytes.
func TestEventLogTheObserversMessageIsWhatASubscriberDecodes(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	observed := observing(t, l)
	s := mustSubscribe(t, l, SubscribeOptions{})
	var calls atomic.Int64
	publishWithin(t, l, Event{Type: EventError, Err: &countedError{msg: "bad \xff\xfe bytes", calls: &calls}, At: logTestTime})
	rec := readN(t, s, 1)[0]
	decoded, err := rec.Event()
	if err != nil {
		t.Fatal(err)
	}
	got := observedErrors(t, observed())
	if len(got) != 1 || got[0].Message != "bad \uFFFD\uFFFD bytes" || !reflect.DeepEqual(decoded.Err, error(got[0])) {
		t.Fatalf("the observer holds %+v, a subscriber decodes %+v", got, decoded.Err)
	}
}
