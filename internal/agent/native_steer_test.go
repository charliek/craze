package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
)

// The native adapter's Interject (plan 019 §3.10, §7.12): the live session's
// three refusals, EventUser{Interjection} from the turn's own goroutine, and
// whatever the turn could not answer at the head of the queue.

// nativeSteer is the interjection these tests send.
const nativeSteer = "also look at b.txt"

// interjected is the text of every EventUser an interjection produced, in
// order, and how far through evs the last one was.
func interjected(evs []Event) ([]string, int) {
	var out []string
	last := -1
	for i, ev := range evs {
		if ev.Type == EventUser && ev.Interjection {
			out = append(out, ev.Text)
			last = i
		}
	}
	return out, last
}

// queueTexts is the session's queue in send order.
func queueTexts(s Session) []string {
	var out []string
	for _, p := range s.Snapshot().Queue {
		out = append(out, p.Text)
	}
	return out
}

// TestNativeInterjectRefusals: Interject is refused in the three states the
// live session refuses in — nothing running, the turn's ending already out,
// and a cancel in progress — and nothing is emitted for a refusal. The
// control is the same call during a live turn, which is accepted and does
// emit.
func TestNativeInterjectRefusals(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("thinking"), finishParts(fantasy.FinishReasonStop)), answer("second"))

	if err := s.Interject(context.Background(), nativeSteer); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Interject while idle = %v, want ErrNotInTurn", err)
	}
	if evs := drained(s); len(evs) != 0 {
		t.Fatalf("a refused interjection emitted %+v", evs)
	}

	out := startPrompt(s, "go")
	await(t, h.reached, "the held step")
	// The control: a live turn takes it.
	if err := s.Interject(context.Background(), nativeSteer); err != nil {
		t.Fatalf("control: Interject during a turn = %v, want it accepted", err)
	}
	// A cancel in progress refuses, even though the turn has not ended yet.
	s.mu.Lock()
	s.cancelling = true
	s.mu.Unlock()
	if err := s.Interject(context.Background(), "during a cancel"); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Interject during a cancel = %v, want ErrNotInTurn", err)
	}
	s.mu.Lock()
	s.cancelling = false
	s.mu.Unlock()
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("the prompt failed: %v", got.err)
	}

	// After the turn, the slot is idle again and the refusal is the first one.
	if err := s.Interject(context.Background(), "after the turn"); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Interject after the turn = %v, want ErrNotInTurn", err)
	}
	texts, _ := interjected(drained(s))
	if len(texts) != 1 || texts[0] != nativeSteer {
		t.Fatalf("interjections seen = %q, want only the accepted one", texts)
	}
}

// TestNativeInterjectDuringAToolStep (§7.12): an interjection sent while a
// tool step runs is answered in the same turn — the model reads it at the next
// step and the transcript holds it — it is shown once as EventUser before the
// turn's EventDone, and it is not queued afterwards. The control is the same
// turn with no interjection: no EventUser, nothing in the transcript, and the
// second request one message shorter.
func TestNativeInterjectDuringAToolStep(t *testing.T) {
	for _, interject := range []bool{true, false} {
		t.Run(fmt.Sprintf("interject=%v", interject), func(t *testing.T) {
			f := newNativeFixture(t)
			ws := t.TempDir()
			if err := os.WriteFile(ws+"/a.txt", []byte("alpha\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			s := f.started(Options{Workspace: ws})
			h := newHeld(t)
			m := f.models["test/a"]
			m.push(
				h.step(nativeCallParts("c1", "read", nativeArgs(t, map[string]any{"filePath": "a.txt"})),
					finishParts(fantasy.FinishReasonToolCalls)),
				answer("done"),
			)

			out := startPrompt(s, "read a.txt")
			await(t, h.reached, "the held tool step")
			if interject {
				if err := s.Interject(context.Background(), nativeSteer); err != nil {
					t.Fatalf("Interject: %v", err)
				}
			}
			close(h.release)
			got := await(t, out, "the prompt")
			if got.err != nil || got.res.StopReason != "end_turn" {
				t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
			}

			evs := drained(s)
			texts, last := interjected(evs)
			done := len(evs) - 1
			if evs[done].Type != EventDone {
				t.Fatalf("the last event is %+v, want EventDone", evs[done])
			}
			// Whatever was answered inside the turn is not queued for the next.
			if q := queueTexts(s); len(q) != 0 {
				t.Fatalf("the queue holds %q; an answered interjection is not re-sent", q)
			}
			calls := m.requests()
			if len(calls) != 2 {
				t.Fatalf("the model saw %d requests, want 2", len(calls))
			}
			second := calls[1].Prompt

			if !interject {
				if len(texts) != 0 {
					t.Fatalf("EventUser interjections with no Interject called: %q", texts)
				}
				if got := len(second); got != 4 {
					t.Fatalf("the second request carried %d messages, want 4", got)
				}
				for _, line := range transcriptOf(t, f) {
					if strings.Contains(line, nativeSteer) {
						t.Fatalf("the control's transcript holds an interjection: %q", line)
					}
				}
				return
			}
			if len(texts) != 1 || texts[0] != nativeSteer {
				t.Fatalf("interjections = %q, want exactly the one sent", texts)
			}
			if last > done {
				t.Fatalf("an EventUser interjection at %d came after the EventDone at %d", last, done)
			}
			// The model really read it, at the index the transcript keeps it.
			if got := len(second); got != 5 {
				t.Fatalf("the second request carried %d messages, want 5", got)
			}
			if text := userTextOf(second[4]); text != nativeSteer {
				t.Fatalf("the second request's message 4 is %q, want the interjection", text)
			}
			if !containsLine(transcriptOf(t, f), nativeSteer) {
				t.Fatalf("the transcript does not hold the interjection:\n%s", strings.Join(transcriptOf(t, f), "\n"))
			}
		})
	}
}

// userTextOf is a message's first text part, or "".
func userTextOf(m fantasy.Message) string {
	for _, p := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			return tp.Text
		}
	}
	return ""
}

// transcriptOf is every line of the fixture's one transcript.
func transcriptOf(t *testing.T, f *nativeFixture) []string {
	t.Helper()
	paths := f.transcripts()
	if len(paths) != 1 {
		t.Fatalf("%d transcripts, want 1", len(paths))
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func containsLine(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// TestNativeInterjectUnansweredGoesToTheQueueFront (§7.12): an interjection
// the turn accepted during its final step is at the head of the queue once the
// turn ends — ahead of rows queued before it — and neither cap refuses it: it
// lands on a queue already full, and text far over the size cap lands too. The
// control is Queue itself, which refuses both.
func TestNativeInterjectUnansweredGoesToTheQueueFront(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("all done"), finishParts(fantasy.FinishReasonStop)))

	for i := range queueCap {
		if _, err := s.Queue(fmt.Sprintf("queued %02d", i)); err != nil {
			t.Fatalf("Queue %d: %v", i, err)
		}
	}
	// The control: the queue is full and the text is oversized, so Queue
	// refuses both, which is exactly what Interject must not do.
	if _, err := s.Queue("one too many"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("control: Queue on a full queue = %v, want ErrQueueFull", err)
	}
	huge := strings.Repeat("x", queueTextCap+1)
	if _, err := s.Queue(huge); !errors.Is(err, ErrQueueTextTooLong) && !errors.Is(err, ErrQueueFull) {
		t.Fatalf("control: Queue of oversized text = %v, want a refusal", err)
	}

	out := startPrompt(s, "go")
	await(t, h.reached, "the held final step")
	// Accepted during the final text step: there is no later step to take it
	// up, so it comes back unanswered.
	for _, text := range []string{nativeSteer, huge} {
		if err := s.Interject(context.Background(), text); err != nil {
			t.Fatalf("Interject onto a full queue: %v", err)
		}
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("the prompt failed: %v", got.err)
	}

	q := queueTexts(s)
	if len(q) != queueCap+2 {
		t.Fatalf("the queue holds %d rows, want the %d queued plus both interjections", len(q), queueCap)
	}
	if q[0] != nativeSteer || q[1] != huge || q[2] != "queued 00" {
		t.Fatalf("the queue's head is %q, %q, %q; want both interjections, in order, ahead of the queued rows", q[0], q[1], q[2])
	}
	// Both were shown as interjections too, before the turn's EventDone.
	texts, last := interjected(drained(s))
	if len(texts) != 2 || texts[0] != nativeSteer || texts[1] != huge {
		t.Fatalf("interjections = %d, want both, in order", len(texts))
	}
	_ = last
}

// TestNativeInterjectClearedWithTheQueueOnAnError (§7.12): an interjection a
// failed turn could not answer reaches the queue like any other row and is
// then cleared with the rest, one EventQueue removal each, after the
// EventError. The control is the same turn succeeding, where it stays queued.
func TestNativeInterjectClearedWithTheQueueOnAnError(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.started(Options{})
			h := newHeld(t)
			after := finishParts(fantasy.FinishReasonStop)
			if fail {
				after = errorParts(errors.New("upstream gone"))
			}
			f.models["test/a"].push(h.step(textParts("partial"), after))

			out := startPrompt(s, "go")
			await(t, h.reached, "the held step")
			if err := s.Interject(context.Background(), nativeSteer); err != nil {
				t.Fatalf("Interject: %v", err)
			}
			close(h.release)
			got := await(t, out, "the prompt")
			if fail != (got.err != nil) {
				t.Fatalf("Prompt = %+v, %v; fail=%v", got.res, got.err, fail)
			}

			evs := drained(s)
			if !fail {
				if q := queueTexts(s); len(q) != 1 || q[0] != nativeSteer {
					t.Fatalf("control: the queue holds %q, want the interjection", q)
				}
				return
			}
			if q := queueTexts(s); len(q) != 0 {
				t.Fatalf("the queue survived the error: %q", q)
			}
			// The interjection was shown, the error came after it, and the
			// removal after the error: a consumer knows why the row went.
			_, last := interjected(evs)
			errAt, removedAt := -1, -1
			for i, ev := range evs {
				switch {
				case ev.Type == EventError:
					errAt = i
				case ev.Type == EventQueue && ev.QueueChange == QueueRemoved && ev.Queue.Text == nativeSteer:
					removedAt = i
				}
			}
			if last < 0 || errAt < 0 || removedAt < 0 || last >= errAt || errAt >= removedAt {
				t.Fatalf("interjection at %d, error at %d, removal at %d; want that order in %+v", last, errAt, removedAt, evs)
			}
		})
	}
}

// TestNativeInterjectDoesNotLeakIntoTheNextTurn: Interject reads the running
// turn's token in the same locked section that judges the turn live, and hands
// it to the harness; a turn that ends in the window between the two refuses
// the text rather than letting the next turn take it up.
//
// The two halves are held apart here exactly as Interject holds them together
// — the token is read the way Interject reads it, the turn is then allowed to
// end and the next to begin, and only then is the accept made — because that
// window is not otherwise reachable from outside. The control is the same
// token used while the turn it names is still live, which is accepted.
func TestNativeInterjectDoesNotLeakIntoTheNextTurn(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	first, second := newHeld(t), newHeld(t)
	f.models["test/a"].push(
		first.step(textParts("one"), finishParts(fantasy.FinishReasonStop)),
		second.step(textParts("two"), finishParts(fantasy.FinishReasonStop)),
	)

	out := startPrompt(s, "first")
	await(t, first.reached, "the first turn")
	// Interject's first half, on the first turn.
	s.mu.Lock()
	stale := s.hs.SteerToken()
	s.mu.Unlock()
	if stale == 0 {
		t.Fatal("no token during a live turn")
	}
	// The control: the same token, while the turn it names is still live.
	if err := s.hs.Steer(stale, "into the first turn"); err != nil {
		t.Fatalf("control: the live turn's token = %v, want it accepted", err)
	}
	close(first.release)
	if got := await(t, out, "the first prompt"); got.err != nil {
		t.Fatal(got.err)
	}
	firstEvents := drained(s)
	// The control landed where it belongs: shown, then queued for the next turn.
	if texts, _ := interjected(firstEvents); len(texts) != 1 || texts[0] != "into the first turn" {
		t.Fatalf("the first turn showed %q, want its own interjection", texts)
	}
	if _, err := s.Unqueue(s.Snapshot().Queue[0].ID); !err {
		t.Fatal("clearing the control row")
	}
	drained(s)

	// The second turn opens; Interject's second half now runs with the first
	// turn's token.
	out = startPrompt(s, "second")
	await(t, second.reached, "the second turn")
	if err := s.hs.Steer(stale, nativeSteer); !errors.Is(err, harness.ErrNotInTurn) {
		t.Fatalf("the previous turn's token = %v, want harness.ErrNotInTurn", err)
	}
	close(second.release)
	if got := await(t, out, "the second prompt"); got.err != nil {
		t.Fatal(got.err)
	}

	evs := drained(s)
	if texts, _ := interjected(evs); len(texts) != 0 {
		t.Fatalf("the second turn showed %q; the text was typed into the first", texts)
	}
	if q := queueTexts(s); len(q) != 0 {
		t.Fatalf("the stale interjection was queued: %q", q)
	}
	if containsLine(transcriptOf(t, f), nativeSteer) {
		t.Fatal("the stale interjection was written to the second turn")
	}
}

// TestNativeInterjectOnAClosedSession (finding 3), both halves of it: a closed
// session refuses an interjection rather than acknowledging one it could never
// show, and the requeue of a turn Close cancelled puts nothing in the queue.
// A closed session's events are suppressed, so a row pushed then would sit
// there unseen, never drained and never cleared, with the user told their text
// had been taken.
//
// The control is the same interjection on the same turn without the Close: it
// is accepted and it does reach the head of the queue.
func TestNativeInterjectOnAClosedSession(t *testing.T) {
	for _, closing := range []bool{true, false} {
		t.Run(fmt.Sprintf("closed=%v", closing), func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.started(Options{})
			h := newHeld(t)
			f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))

			out := startPrompt(s, "go")
			await(t, h.reached, "the held step")
			// Taken while the session is open and the turn is live, by the
			// turn's final step: it has no step left to answer it, so it is
			// the requeue's business.
			if err := s.Interject(context.Background(), nativeSteer); err != nil {
				t.Fatalf("Interject during a live turn = %v", err)
			}
			if !closing {
				close(h.release)
				if got := await(t, out, "the prompt"); got.err != nil {
					t.Fatal(got.err)
				}
				if q := queueTexts(s); len(q) != 1 || q[0] != nativeSteer {
					t.Fatalf("control: the queue holds %q, want the interjection at the head", q)
				}
				return
			}

			// Close cancels the turn and waits for it, so the requeue runs
			// with the session already closed.
			closed := make(chan struct{})
			go func() {
				_ = s.Close()
				close(closed)
			}()
			await(t, closed, "Close")
			await(t, out, "the cancelled prompt")
			if q := queueTexts(s); len(q) != 0 {
				t.Fatalf("a closed session queued %q; nothing could ever show, drain or clear it", q)
			}
			// And a fresh interjection is refused rather than acknowledged.
			if err := s.Interject(context.Background(), nativeSteer); err == nil || err.Error() != "agent: session closed" {
				t.Fatalf("Interject after Close = %v, want the session-closed refusal", err)
			}
			if q := queueTexts(s); len(q) != 0 {
				t.Fatalf("a refused interjection queued %q", q)
			}
		})
	}
}

// TestNativeInterjectLimitIsTheQueuesOwn: a turn takes as many interjections
// as the queue could hold and refuses the next with ErrQueueFull, changing
// nothing — which is what bounds the requeue burst, and with it the number of
// events one turn can push through a consumer's channel while queueTx holds
// emitMu. The control is the last accepted one, and the queue afterwards,
// which holds exactly the accepted ones.
func TestNativeInterjectLimitIsTheQueuesOwn(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))

	out := startPrompt(s, "go")
	await(t, h.reached, "the held step")
	for i := range queueCap {
		// The control is the last of these: the cap is reached, not passed.
		if err := s.Interject(context.Background(), fmt.Sprintf("steer %02d", i)); err != nil {
			t.Fatalf("interjection %d of %d = %v, want it accepted", i+1, queueCap, err)
		}
	}
	if err := s.Interject(context.Background(), nativeSteer); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("interjection %d = %v, want ErrQueueFull", queueCap+1, err)
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatal(got.err)
	}

	q := queueTexts(s)
	if len(q) != queueCap || q[0] != "steer 00" || q[queueCap-1] != fmt.Sprintf("steer %02d", queueCap-1) {
		t.Fatalf("the queue holds %d rows, want exactly the %d accepted, in order", len(q), queueCap)
	}
	for _, text := range q {
		if text == nativeSteer {
			t.Fatal("the refused interjection was queued")
		}
	}
}

// TestNativeInterjectRequeueDoesNotWedgeAConcurrentQueue (finding 2): a turn
// requeues a full turn's worth of interjections into an event channel that is
// already mostly full, while another goroutine queues a message of its own.
// queueTx holds emitMu across its sends, so a requeue that could grow without
// limit would fill the channel from inside that lock and leave every other
// queue call waiting behind a send only the blocked consumer could unblock.
//
// What makes it safe is arithmetic, not luck: one turn accepts at most
// queueCap interjections, so its whole burst — an EventUser for each and an
// EventQueue for each it could not answer — is bounded by twice that plus the
// turn's own few, well inside a 256-deep channel. An unbounded interjector
// would need hundreds of slots and would wedge here.
//
// The burst is one short of the cap on purpose. PushFront is exempt from
// queueCap (a requeue has nowhere else to put the text), so a full cap's
// worth of requeued rows leaves an ordinary Queue refusing with
// ErrQueueFull — and whether it does would then depend on which side of the
// requeue it lands on, which is the race this test creates. Leaving one
// ordinary slot free makes the concurrent Queue succeed in either order,
// while the burst it races is the same size in event terms. (CI caught this:
// two runs of the same commit, one green, one "the concurrent Queue = agent:
// queue is full".)
//
// What this does not do is force the concurrent Queue to overlap the requeue
// (CodeRabbit, round 3): the goroutine may run before it, during it, or after
// it, and the test asserts what must hold in every one of those orders — the
// prompt returns, the Queue returns, and the burst is at the head. Forcing the
// overlap would need a test-only barrier inside queueUnanswered, and this
// package keeps no hooks in production code; the test that actually creates
// contention on queueOp and emitMu is TestNativeInterjectRacesTheTurnEnd,
// which runs 24 interjecting goroutines against the turn's end under -race.
//
// The control is the vacuity check: the channel really was left with a small
// fraction of its capacity free, and the whole burst really was requeued.
func TestNativeInterjectRequeueDoesNotWedgeAConcurrentQueue(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))

	// Nothing drains, and the channel is filled to leave exactly what one
	// capped turn needs: an EventUser and an EventQueue per interjection, and
	// a handful for the turn itself and the concurrent Queue.
	const burst = queueCap - 1
	headroom := 2*burst + 8
	// Filled through the session's own emit: the primary belongs to the event
	// log now, and the field is receive-only precisely so nothing enters the
	// stream without a sequence number (plan 020 §3.1). Each of these has room
	// by the loop's own condition, so none of them blocks.
	for len(s.events) < cap(s.events)-headroom {
		s.emit(Event{Type: EventText, Text: "filler"})
	}
	if free := cap(s.events) - len(s.events); free != headroom || free > cap(s.events)/2 {
		t.Fatalf("control: the channel has %d of %d slots free, want %d and well under half", free, cap(s.events), headroom)
	}

	out := startPrompt(s, "go")
	await(t, h.reached, "the held step")
	for i := range burst {
		if err := s.Interject(context.Background(), fmt.Sprintf("steer %02d", i)); err != nil {
			t.Fatalf("interjection %d: %v", i+1, err)
		}
	}
	// A queue call from another goroutine, racing the turn's requeue for
	// queueOp and emitMu.
	queued := make(chan error, 1)
	go func() {
		_, err := s.Queue("from elsewhere")
		queued <- err
	}()
	close(h.release)
	if got := await(t, out, "the prompt, which must not wedge behind its own requeue"); got.err != nil {
		t.Fatal(got.err)
	}
	if err := await(t, queued, "the concurrent Queue"); err != nil {
		t.Fatalf("the concurrent Queue = %v", err)
	}
	// The control: the whole burst really was requeued, at the head.
	q := queueTexts(s)
	if len(q) != burst+1 || q[0] != "steer 00" {
		t.Fatalf("the queue holds %d rows headed by %q, want %d headed by the first interjection", len(q), q[0], burst+1)
	}
}

// TestNativeInterjectRacesTheTurnEnd (run under -race): interjections arrive
// from many goroutines while the turn ends. Not one EventUser interjection
// follows the turn's ending event, every accepted one is shown exactly once,
// and each is either answered inside the turn or waiting at the head of the
// queue. The control is the accepted count, which must not be zero.
func TestNativeInterjectRacesTheTurnEnd(t *testing.T) {
	const senders = 24
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("done"), finishParts(fantasy.FinishReasonStop)))

	out := startPrompt(s, "go")
	await(t, h.reached, "the held step")
	accepted := make(chan string, senders)
	done := make(chan struct{})
	for i := range senders {
		go func() {
			text := fmt.Sprintf("steer %02d", i)
			err := s.Interject(context.Background(), text)
			switch {
			case err == nil:
				accepted <- text
			case errors.Is(err, ErrNotInTurn):
			default:
				t.Errorf("Interject(%q) = %v, want nil or ErrNotInTurn", text, err)
			}
			done <- struct{}{}
		}()
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("the prompt failed: %v", got.err)
	}
	for range senders {
		await(t, done, "an interjecting goroutine")
	}
	close(accepted)
	var sent []string
	for text := range accepted {
		sent = append(sent, text)
	}

	evs := drained(s)
	shown, last := interjected(evs)
	endAt := -1
	for i, ev := range evs {
		if ev.Type == EventDone || ev.Type == EventError {
			endAt = i
		}
	}
	if endAt < 0 || last > endAt {
		t.Fatalf("an EventUser interjection at %d followed the turn's ending at %d", last, endAt)
	}
	if len(shown) != len(sent) {
		t.Fatalf("%d interjections shown, %d accepted", len(shown), len(sent))
	}
	queued := map[string]bool{}
	for _, text := range queueTexts(s) {
		queued[text] = true
	}
	for _, text := range sent {
		if !queued[text] && !containsLine(transcriptOf(t, f), text) {
			t.Errorf("%q was accepted but is neither queued nor written", text)
		}
	}
	if len(sent) == 0 {
		t.Fatalf("control: not one of %d interjections was accepted", senders)
	}
}
