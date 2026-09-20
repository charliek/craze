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

// TestNativeInterjectUnansweredComesBackInTheResult (§7.12; plan 021 §3.5): an
// interjection the turn accepted during its final step comes back in
// Result.Unanswered, in the order it was typed. Interject applies no size
// limit of its own: text far over the queue's size cap is accepted just the
// same as an ordinary one.
//
// Where those steers then go is the engine's, above this seam: to the head of
// the queue, last first, in the same locked section that decides the turn's
// successor, so steered text is ahead of whatever was waiting behind the turn
// that took it. The session has no queue of its own any more to put them in.
func TestNativeInterjectUnansweredComesBackInTheResult(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("all done"), finishParts(fantasy.FinishReasonStop)))

	out := startPrompt(s, "go")
	await(t, h.reached, "the held final step")
	// Accepted during the final text step: there is no later step to take it
	// up, so it comes back unanswered.
	huge := strings.Repeat("x", queueTextCap+1)
	for _, text := range []string{nativeSteer, huge} {
		if err := s.Interject(context.Background(), text); err != nil {
			t.Fatalf("Interject: %v", err)
		}
	}
	close(h.release)
	got := await(t, out, "the prompt")
	if got.err != nil {
		t.Fatalf("the prompt failed: %v", got.err)
	}

	if u := got.res.Unanswered; len(u) != 2 || u[0] != nativeSteer || u[1] != huge {
		t.Fatalf("Unanswered holds %d texts, want both interjections in the order they were typed", len(u))
	}
	// Both were shown as interjections too, before the turn's EventDone.
	texts, last := interjected(drained(s))
	if len(texts) != 2 || texts[0] != nativeSteer || texts[1] != huge {
		t.Fatalf("interjections = %d, want both, in order", len(texts))
	}
	_ = last
}

// TestNativeInterjectReportsUnansweredOnTheErrorPathToo (§7.12; plan 021 §3.5): an
// interjection a failed turn could not answer is reported in Result.Unanswered on
// the error path as much as on the clean one, so the engine above the seam puts it
// back and then clears it with the rest. The control is the same turn
// succeeding, where it is reported just the same.
func TestNativeInterjectReportsUnansweredOnTheErrorPathToo(t *testing.T) {
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
			if u := got.res.Unanswered; len(u) != 1 || u[0] != nativeSteer {
				t.Fatalf("Unanswered = %q, want the steer the turn could not answer", u)
			}
			if !fail {
				return
			}
			// The interjection was shown before the error that followed it.
			evs := drained(s)
			_, last := interjected(evs)
			errAt := -1
			for i, ev := range evs {
				if ev.Type == EventError {
					errAt = i
				}
			}
			if last < 0 || errAt < 0 || last >= errAt {
				t.Fatalf("interjection at %d, error at %d; want the interjection first in %+v", last, errAt, evs)
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
	firstRun := await(t, out, "the first prompt")
	if firstRun.err != nil {
		t.Fatal(firstRun.err)
	}
	firstEvents := drained(s)
	// The control landed where it belongs: shown, and reported as the turn's own
	// unanswered steer rather than queued by the session.
	if texts, _ := interjected(firstEvents); len(texts) != 1 || texts[0] != "into the first turn" {
		t.Fatalf("the first turn showed %q, want its own interjection", texts)
	}
	if u := firstRun.res.Unanswered; len(u) != 1 || u[0] != "into the first turn" {
		t.Fatalf("the first turn's Unanswered = %q, want its own steer", u)
	}

	// The second turn opens; Interject's second half now runs with the first
	// turn's token.
	out = startPrompt(s, "second")
	await(t, second.reached, "the second turn")
	if err := s.hs.Steer(stale, nativeSteer); !errors.Is(err, harness.ErrNotInTurn) {
		t.Fatalf("the previous turn's token = %v, want harness.ErrNotInTurn", err)
	}
	close(second.release)
	secondRun := await(t, out, "the second prompt")
	if secondRun.err != nil {
		t.Fatal(secondRun.err)
	}

	evs := drained(s)
	if texts, _ := interjected(evs); len(texts) != 0 {
		t.Fatalf("the second turn showed %q; the text was typed into the first", texts)
	}
	if u := secondRun.res.Unanswered; len(u) != 0 {
		t.Fatalf("the second turn reported %q; the text was typed into the first", u)
	}
	if containsLine(transcriptOf(t, f), nativeSteer) {
		t.Fatal("the stale interjection was written to the second turn")
	}
}

// TestNativeInterjectOnAClosedSession (finding 3): a closed session refuses an
// interjection rather than acknowledging one it could never show. Nothing is
// pushed anywhere for a turn Close cancelled either — the steers come back in
// Result.Unanswered and the engine above the seam decides, and the session has
// no queue of its own to leak into.
//
// The control is the same interjection on the same turn without the Close: it is
// accepted and it does come back as the turn's unanswered steer.
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
			// turn's final step: it has no step left to answer it, so it comes
			// back unanswered.
			if err := s.Interject(context.Background(), nativeSteer); err != nil {
				t.Fatalf("Interject during a live turn = %v", err)
			}
			if !closing {
				close(h.release)
				got := await(t, out, "the prompt")
				if got.err != nil {
					t.Fatal(got.err)
				}
				if u := got.res.Unanswered; len(u) != 1 || u[0] != nativeSteer {
					t.Fatalf("control: Unanswered = %q, want the interjection", u)
				}
				return
			}

			// Close cancels the turn and waits for it, so the turn ends with
			// the session already closed.
			closed := make(chan struct{})
			go func() {
				_ = s.Close()
				close(closed)
			}()
			await(t, closed, "Close")
			await(t, out, "the cancelled prompt")
			// And a fresh interjection is refused rather than acknowledged.
			if err := s.Interject(context.Background(), nativeSteer); err == nil || err.Error() != "agent: session closed" {
				t.Fatalf("Interject after Close = %v, want the session-closed refusal", err)
			}
		})
	}
}

// TestNativeInterjectLimitIsTheQueuesOwn: a turn takes as many interjections
// as the queue could hold and refuses the next with ErrQueueFull, changing
// nothing — which is what bounds the burst one turn can hand back, and with it the
// number of events putting them back can produce. The control is the last accepted
// one, and Result.Unanswered afterwards, which holds exactly the accepted ones.
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
	got := await(t, out, "the prompt")
	if got.err != nil {
		t.Fatal(got.err)
	}

	u := got.res.Unanswered
	if len(u) != queueCap || u[0] != "steer 00" || u[queueCap-1] != fmt.Sprintf("steer %02d", queueCap-1) {
		t.Fatalf("Unanswered holds %d texts, want exactly the %d accepted, in order", len(u), queueCap)
	}
	for _, text := range u {
		if text == nativeSteer {
			t.Fatal("the refused interjection was accepted after all")
		}
	}
}

// TestNativeInterjectEndsAFullBurstBesideANearlyFullChannel (finding 2, and
// what is left of it): a turn takes a full turn's worth of interjections with
// the event channel already mostly full, and neither the prompt nor the
// interjections wedge.
//
// Half of what this used to guard is gone by construction. The wedge was
// queueTx holding emitMu across the *requeue's* blocking sends, so an
// unbounded burst could fill the channel from inside that lock and leave
// every other queue call waiting behind a send only the blocked consumer
// could free (a concurrent Queue call used to be part of this test for
// exactly that reason). Nothing is requeued here any more, and the session has
// no queue call left to race: the steers come back in Result.Unanswered, and
// the engine above the seam puts them back — into the log's outbox, whose
// mutex is a leaf, with no lock held across a send at all. What remains, and
// is still worth holding, is the EventUser per accepted interjection, which
// the turn's own goroutine publishes with a nearly full channel.
//
// The control is the vacuity check: the channel really was left with a small
// fraction of its capacity free, and the whole burst really came back.
func TestNativeInterjectEndsAFullBurstBesideANearlyFullChannel(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))

	// Nothing drains, and the channel is filled to leave exactly what one
	// capped turn needs: an EventUser per interjection, and a handful for the
	// turn itself.
	const burst = queueCap - 1
	headroom := burst + 8
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
	close(h.release)
	got := await(t, out, "the prompt, which must not wedge behind its own burst")
	if got.err != nil {
		t.Fatal(got.err)
	}
	// The control: the whole burst really came back, in order.
	u := got.res.Unanswered
	if len(u) != burst || u[0] != "steer 00" {
		t.Fatalf("Unanswered holds %d texts headed by %q, want %d headed by the first interjection", len(u), u[0], burst)
	}
}

// TestNativeInterjectRacesTheTurnEnd (run under -race): interjections arrive
// from many goroutines while the turn ends. Not one EventUser interjection
// follows the turn's ending event, every accepted one is shown exactly once,
// and each is either answered inside the turn or reported unanswered — nothing
// an interjection said it took goes missing. The control is the accepted count,
// which must not be zero.
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
	run := await(t, out, "the prompt")
	if run.err != nil {
		t.Fatalf("the prompt failed: %v", run.err)
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
	unanswered := map[string]bool{}
	for _, text := range run.res.Unanswered {
		unanswered[text] = true
	}
	for _, text := range sent {
		if !unanswered[text] && !containsLine(transcriptOf(t, f), text) {
			t.Errorf("%q was accepted but is neither reported unanswered nor written", text)
		}
	}
	if len(sent) == 0 {
		t.Fatalf("control: not one of %d interjections was accepted", senders)
	}
}
