package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"charm.land/fantasy"
)

// Every session publishes through its EventLog (plan 020 §3.1, commit C4).
// These tests hold that through the sessions themselves, not the log: the
// value agent.New returns is an EventSource, a subscription taken through it
// sees exactly what the primary reader sees, in sequence order, "emitted means
// buffered" still holds once a Prompt has returned, and Close is the log's
// admission cutoff for an emitter that outlives it. The log's own properties
// are eventlog_test.go's.

// sourceOf is s as an EventSource, failing the test if it is not one.
func sourceOf(t *testing.T, s Session) EventSource {
	t.Helper()
	src, ok := s.(EventSource)
	if !ok {
		t.Fatalf("%T is not an agent.EventSource", s)
	}
	return src
}

// seqsOf is the Seq of each event, in order.
func seqsOf(evs []Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, ev := range evs {
		out[i] = ev.Seq
	}
	return out
}

// assertRecordsArePrimary fails unless recs are evs one for one: the same
// seq, and a body that decodes (Record.Event) to the event the primary
// reader got, under the codec's own equivalence — the decoded event encodes
// back to the body, and the body is the primary event's encoding.
func assertRecordsArePrimary(t *testing.T, what string, recs []Record, evs []Event) {
	t.Helper()
	if len(recs) != len(evs) {
		t.Fatalf("%s: %d records for %d primary events", what, len(recs), len(evs))
	}
	for i, rec := range recs {
		p := evs[i]
		if rec.Seq != p.Seq || rec.Type != p.Type {
			t.Fatalf("%s: record %d is seq %d %s, the primary's is seq %d %s", what, i, rec.Seq, rec.Type, p.Seq, p.Type)
		}
		ev, err := rec.Event()
		if err != nil {
			t.Fatalf("%s: record %d (seq %d): %v", what, i, rec.Seq, err)
		}
		if ev.Seq != p.Seq {
			t.Fatalf("%s: record %d decoded with seq %d, want %d", what, i, ev.Seq, p.Seq)
		}
		want, err := EncodeEvent(p)
		if err != nil {
			t.Fatalf("%s: encoding primary event %d: %v", what, p.Seq, err)
		}
		got, err := EncodeEvent(ev)
		if err != nil {
			t.Fatalf("%s: re-encoding decoded event %d: %v", what, ev.Seq, err)
		}
		if rec.Body != want || got != want {
			t.Fatalf("%s: seq %d differs from the primary's event:\n record  %s\n decoded %s\n primary %s", what, p.Seq, rec.Body, got, want)
		}
	}
}

// runSubscribedTurn subscribes to sess before it starts, starts it, runs one
// prompt, and holds the EventSource contract against what the primary got:
//
//   - after Prompt returns, a non-blocking drain of Events() already holds
//     the turn's ending (emitted means buffered, internal/cli/prompt.go's
//     drainBuffered);
//   - Seq runs 1, 2, 3, … with no hole, since the subscription was taken
//     before anything was published;
//   - the live subscription and a replay from the ring (a cursor at seq 0)
//     each deliver exactly the primary's events, in seq order;
//   - Incarnation never changes;
//   - Close ends the subscription with ErrClosed, having delivered nothing
//     the primary did not get, and a Subscribe after it is refused.
func runSubscribedTurn(t *testing.T, sess Session, text string) {
	t.Helper()
	src := sourceOf(t, sess)
	inc := src.Incarnation()
	if inc == "" {
		t.Fatal("Incarnation is empty")
	}
	sub, err := src.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe before Start: %v", err)
	}
	if err := sess.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res, err := sess.Prompt(t.Context(), text)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	primary := drained(sess)
	if dones := ofType(primary, EventDone); len(dones) != 1 || dones[0].StopReason != res.StopReason {
		t.Fatalf("after Prompt returned, the buffered events hold %d EventDone (want 1, stop %q): %v",
			len(dones), res.StopReason, typesOf(primary))
	}
	if msg := runSeqs(seqsOf(primary), 1); msg != "" {
		t.Fatalf("the primary's seqs %v: %s", seqsOf(primary), msg)
	}
	assertRecordsArePrimary(t, "live subscription", readN(t, sub, len(primary)), primary)

	replay, err := src.Subscribe(SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: 0}})
	if err != nil {
		t.Fatalf("Subscribe from seq 0: %v", err)
	}
	assertRecordsArePrimary(t, "replay from the ring", readN(t, replay, len(primary)), primary)
	replay.Close()

	if got := src.Incarnation(); got != inc {
		t.Fatalf("Incarnation changed during the turn: %q, then %q", inc, got)
	}
	within(t, "Close", func() { _ = sess.Close() })

	// A notification the agent sent after the turn (a sub-agent's trailing
	// progress, say) can have been published between the drain above and
	// Close. Whatever the subscription delivered past the turn is the start
	// of exactly that; the rest was discarded when it ended.
	late := drained(sess)
	rest := readAll(t, sub)
	if len(rest) > len(late) {
		t.Fatalf("after the turn the subscription delivered %d records, the primary got %d events", len(rest), len(late))
	}
	assertRecordsArePrimary(t, "after the turn", rest, late[:len(rest)])
	if err := sub.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("the subscription ended with %v, want ErrClosed", err)
	}
	if got := src.Incarnation(); got != inc {
		t.Fatalf("Incarnation changed at Close: %q, then %q", inc, got)
	}
	if _, err := src.Subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
}

// TestNewReturnsAnEventSource: agent.New's value is an EventSource for either
// kind of session it builds, each with an incarnation of its own. Neither is
// started: the log is built with the session.
func TestNewReturnsAnEventSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAZE_HOME", t.TempDir())
	native := NativeProvider()
	acpSess, nativeSess := New(Options{}), New(Options{Provider: &native})
	closeAtCleanup(t, acpSess)
	closeAtCleanup(t, nativeSess)
	a, b := sourceOf(t, acpSess), sourceOf(t, nativeSess)
	if a.Incarnation() == "" || a.Incarnation() == b.Incarnation() {
		t.Fatalf("incarnations %q and %q: want two distinct ids", a.Incarnation(), b.Incarnation())
	}
}

// TestACPSessionSubscriptionIsThePrimary is runSubscribedTurn against the
// fake agent, through agent.New, on cursor with a tool call and on grok with
// a sub-agent: the turns whose payloads are pointers (cloned tools and
// sub-agent records) that Publish now encodes on the emitting goroutine.
func TestACPSessionSubscriptionIsThePrimary(t *testing.T) {
	grok := GrokProvider()
	for _, tc := range []struct {
		name, script string
		provider     *Provider
	}{
		{"cursor tool", "tool", nil},
		{"grok subagent", "grok-subagent", &grok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XAI_API_KEY", "")
			t.Setenv("GROK_CODE_XAI_API_KEY", "")
			sess := New(Options{
				Binary:    fakeAgentPath(t),
				ExtraArgs: []string{"-script=" + tc.script},
				Workspace: t.TempDir(),
				Force:     true,
				Provider:  tc.provider,
				Stderr:    io.Discard,
			})
			closeAtCleanup(t, sess)
			runSubscribedTurn(t, sess, "run")
		})
	}
}

// TestNativeSessionSubscriptionIsThePrimary is runSubscribedTurn against the
// native adapter, on a scripted model.
func TestNativeSessionSubscriptionIsThePrimary(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(reply(thoughtParts("thinking"), textParts("hello ", "there"), finishParts(fantasy.FinishReasonStop)))
	sess := NewNative(Options{Workspace: t.TempDir()}, f.tweak)
	closeAtCleanup(t, sess)
	runSubscribedTurn(t, sess, "hi")
}

// TestSessionCloseCutsOffALateEmitter: Close runs the session's teardown and
// then closes its log, and an emitter that outlives it — a handler goroutine
// Close does not join, blocked on a full primary when Close began, or one
// that only gets to emit afterwards — is refused and reaches no subscriber.
// The blocked one is known to be inside the log, about to wait on the
// primary, from the log's hook rather than from a sleep.
func TestSessionCloseCutsOffALateEmitter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() (sess Session, log *EventLog, done <-chan struct{}, emit func(Event) bool)
	}{
		{"acp", func() (Session, *EventLog, <-chan struct{}, func(Event) bool) {
			s := newSession(Options{})
			return s, s.log, s.done, func(ev Event) bool { return s.emitCtx(context.Background(), ev) }
		}},
		{"native", func() (Session, *EventLog, <-chan struct{}, func(Event) bool) {
			s := newNative(Options{}, nil)
			// The native emit reports nothing; what it did is read off the
			// primary and the subscription below.
			return s, s.log, s.done, func(ev Event) bool { s.emit(ev); return false }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, log, done, emit := tc.build()
			closeAtCleanup(t, sess)
			hook, inside := insideAt(primaryCap + 1)
			log.hooks = &logHooks{beforePrimarySend: hook}
			sub, err := sourceOf(t, sess).Subscribe(SubscribeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for i := range primaryCap {
				emit(textEvent(fmt.Sprintf("fill-%d", i)))
			}
			assertRun(t, "the fill, on the subscription", readN(t, sub, primaryCap), 1, primaryCap)

			// Nobody reads the primary, so this one waits inside the log.
			blocked := make(chan bool, 1)
			go func() { blocked <- emit(textEvent("late")) }()
			select {
			case <-inside:
			case <-blocked:
				t.Fatal("an emit onto a full primary returned before Close")
			}
			within(t, "Close with an emitter blocked on the primary", func() { _ = sess.Close() })
			if await(t, blocked, "the blocked emitter") {
				t.Fatal("the emitter blocked across Close reported its event delivered")
			}
			if emit(textEvent("after")) {
				t.Fatal("an emit after Close reported its event delivered")
			}
			// Past the session's own done check, the log refuses on its own:
			// Close closed it. The primary is still full, so a log left open
			// would hold this one forever; the watchdog turns that into a
			// failure.
			for _, p := range []struct {
				what string
				done <-chan struct{}
			}{{"with no done", nil}, {"with the session's done", done}} {
				published := true
				within(t, "a Publish after Close "+p.what, func() {
					published = log.Publish(context.Background(), p.done, textEvent("after, past done"))
				})
				if published {
					t.Fatalf("a Publish after Close %s returned true", p.what)
				}
			}

			primary := drained(sess)
			if msg := runSeqs(seqsOf(primary), 1); msg != "" || len(primary) != primaryCap {
				t.Fatalf("the primary holds %d events (want the %d of the fill): %s", len(primary), primaryCap, msg)
			}
			for _, ev := range primary {
				if ev.Text == "late" || ev.Text == "after" || ev.Text == "after, past done" {
					t.Fatalf("an event emitted across or after Close reached the primary: %q (seq %d)", ev.Text, ev.Seq)
				}
			}
			if rest := readAll(t, sub); len(rest) != 0 {
				t.Fatalf("the subscription got %d records after Close: %v", len(rest), recordTexts(t, rest))
			}
			if err := sub.Err(); !errors.Is(err, ErrClosed) {
				t.Fatalf("the subscription ended with %v, want ErrClosed", err)
			}
		})
	}
}
