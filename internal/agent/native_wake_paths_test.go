package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness"
)

// The wake's schedules astra r19 asked for (plan 026 PR 3, C9's fix round):
// a prompt that could overtake the wake's ending, the observer's instant, F5's
// re-stamp, and the wake's untested endings. Each is forced with a barrier —
// the log's own hooks, its observer, the sink's hooks, the worker's seams —
// never a sleep.

// TestNativeWakeEndingIsNotOvertakenByAPrompt (astra r19 #1): the wake's
// ending is enqueued in the section that releases the claim, so a direct
// Prompt can claim before the outbox has committed it — and a turn's own
// publishes never wait for queued events. The continuation flushes the outbox
// before it says anything, so the record reads opening → wake text → ending
// → the prompt's text; without the barrier the prompt's text comes first and
// the ending closes the wrong stream. The schedule: the drainer is parked
// before it commits the ending (outboxAdmitting), the wake's own flush parks
// behind it (flushParked), the prompt is started, and its own park is what
// says it waited — where, without the barrier, it publishes its text
// (admitting) and its request reaches the model. A park alone cannot tell the
// two apart: a publish's commit wakes the wake's flush, which finds its
// target still queued and parks again, so the publish is the signal.
func TestNativeWakeEndingIsNotOvertakenByAPrompt(t *testing.T) {
	var holdDrainer atomic.Bool
	drainerHeld, drainerRelease := make(chan struct{}), make(chan struct{})
	var heldOnce, publishedOnce sync.Once
	parked := make(chan uint64, 8)
	published := make(chan struct{})
	rig := newWakeRigWith(t, Options{}, wakeSeams{log: &logHooks{
		outboxAdmitting: func(int) {
			if holdDrainer.Load() {
				heldOnce.Do(func() { close(drainerHeld) })
				<-drainerRelease
			}
		},
		// Only the parks after the hold: every earlier flush — the roster's
		// barriers, the opening bracket's — parks for a moment too.
		flushParked: func(target uint64) {
			if holdDrainer.Load() {
				parked <- target
			}
		},
		admitting: func(k admitKind) {
			if k == admitPublish && holdDrainer.Load() {
				publishedOnce.Do(func() { close(published) })
			}
		},
	}})
	t.Cleanup(func() { closeOnce(drainerRelease) })
	s, w := rig.s, rig.w
	child, wake, later := newHeld(t), newHeld(t), newHeld(t)
	id := rig.spawnOne(child, wake.step(openTextParts("noted"), closeTextParts()),
		later.step(openTextParts("echo later"), closeTextParts()))
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(true, 1, "wake-1")
	await(t, wake.reached, "the wake's step")
	w.wait("the wake's text", func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" && ev.Text == "noted" })

	// The next batch the drainer takes is the ending's: parked there, the
	// ending is enqueued and uncommitted, the claim released.
	holdDrainer.Store(true)
	close(wake.release)
	await(t, drainerHeld, "the drainer parked before the ending")
	if got := await(t, parked, "the wake's own flush to park"); got == 0 {
		t.Fatalf("the wake's flush parked for seq %d", got)
	}
	out := startPrompt(s, "later")
	select {
	case <-parked:
		// A park is the prompt's barrier only if nothing has been published
		// since the drainer parked: a publish's commit wakes the wake's own
		// flush, which parks again, so the publish — which precedes that
		// commit — is checked after the park, never raced against it.
		select {
		case <-published:
			t.Fatal("the prompt published before the wake's ending was committed")
		default:
		}
	case <-published:
		t.Fatal("the prompt published before the wake's ending was committed")
	case <-later.reached:
		t.Fatal("the prompt reached the model before the wake's ending was committed")
	case <-time.After(nativeWait):
		t.Fatal("the prompt neither parked nor published")
	}
	close(drainerRelease)
	await(t, later.reached, "the prompt's step")
	close(later.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	w.waitTerminals(2)

	evs := w.events()
	opening := indexWhere(evs, 0, isBracket(true))
	noted := indexWhere(evs, 0, func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" && ev.Text == "noted" })
	ending := indexWhere(evs, 0, isBracket(false))
	echoed := indexWhere(evs, 0, func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" && ev.Text == "echo later" })
	if !ascending(opening, noted, ending, echoed) {
		t.Fatalf("opening %d, wake text %d, ending %d, the prompt's text %d; want that order", opening, noted, ending, echoed)
	}
}

// TestWakeEndingReleasedWhenTheObserverSeesIt (astra r19 #3, P22): at the
// instant the log's observer — the engine's own hook, the one its driver runs
// from — sees the ending bracket, the wake's claim is already released and
// ForeignTurn already false, because both happen in the section that enqueued
// it. The schedule is forced: the worker is held right after that section
// (wakeEnded) until the observer has seen the ending, so the observer reads
// native's state between the section and everything that follows it. A
// regression that enqueued the ending in one section and released the claim
// in a later one fails here: the observer would find the claim held.
func TestWakeEndingReleasedWhenTheObserverSeesIt(t *testing.T) {
	observed := make(chan struct{})
	var foreign, claimed atomic.Bool
	var rig *wakeRig
	var once sync.Once
	rig = newWakeRigWith(t, Options{}, wakeSeams{
		observe: func(ev Event) {
			if !isBracket(false)(ev) {
				return
			}
			s := rig.s
			foreign.Store(s.ForeignTurn())
			s.mu.Lock()
			claimed.Store(s.claimed || s.inPrompt)
			s.mu.Unlock()
			once.Do(func() { close(observed) })
		},
		ended: func() { <-observed },
	})
	s, w := rig.s, rig.w
	child, wake := newHeld(t), newHeld(t)
	id := rig.spawnOne(child, wake.step(openTextParts("noted"), closeTextParts()), answer("echo later"))
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(true, 1, "wake-1")
	await(t, wake.reached, "the wake's step")
	close(wake.release)
	await(t, observed, "the observer to see the ending")
	if foreign.Load() || claimed.Load() {
		t.Fatalf("when the observer saw the ending, ForeignTurn=%v claimed=%v; want both false", foreign.Load(), claimed.Load())
	}
	rig.bracket(false, 1, "wake-1")
	// What the driver does on that instant: it claims, and the turn runs
	// behind the ending in the record.
	if _, err := s.Prompt(context.Background(), "later"); err != nil {
		t.Fatalf("a prompt at the ending: %v", err)
	}
	w.waitTerminals(2)
	evs := w.events()
	ending := indexWhere(evs, 0, isBracket(false))
	echoed := indexWhere(evs, 0, func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" && ev.Text == "echo later" })
	if !ascending(ending, echoed) {
		t.Fatalf("the ending is at %d and the next prompt's text at %d", ending, echoed)
	}
}

// TestBackgroundFinishNeverRestampsTheCallRow (astra r19 #4; F5, X33): the
// schedule the guard exists for — the background child finishes while its
// call's acknowledgement is still held in the sink, so the call's row is
// still in flight when subagentFinished runs. With the guard the row is never
// stamped with the child's outcome: it closes with the acknowledgement's own
// (completed, no duration) though the child failed. Without the guard the
// stamp would merge the child's failure into the running row and publish it.
func TestBackgroundFinishNeverRestampsTheCallRow(t *testing.T) {
	rig := newWakeRig(t, Options{}, nil)
	s, w := rig.s, rig.w
	ackReached, ackRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	rig.hookBefore(func(ev harness.Event) {
		if fin, ok := ev.(harness.ToolFinished); ok && fin.ID == "t1.1.1" {
			once.Do(func() { close(ackReached) })
			<-ackRelease
		}
	})
	t.Cleanup(func() { closeOnce(ackRelease) })
	child := newHeld(t)
	rig.a.route("go", callsStep(bgCall(t, "a1", "job", "child work")), answer("started"), answer("noted"))
	rig.a.route("child work", child.step(openTextParts("half"), errorParts(errors.New("provider down"))))

	out := startPrompt(s, "go")
	await(t, ackReached, "the call's acknowledgement, held")
	id := w.wait("the child's spawned row", func(ev Event) bool {
		return ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeSpawned && ev.Subagent.Prompt == "child work"
	}).Subagent.ID
	rig.finish(child, id) // the child fails and its result is published, the acknowledgement still held
	close(ackRelease)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	// The result, pending before the turn's next step, is delivered into
	// that step (prepareStep): no wake follows, and the turn's own ending is
	// the barrier behind every row of the call.
	w.waitType(EventDone)
	if rig.s.hs.HasPending() || w.count(EventForeignTurn) != 0 {
		t.Fatal("the result was not delivered into the running turn")
	}

	var rows []ToolEvent
	for _, ev := range w.events() {
		if ev.Type == EventTool && ev.Agent == "" && ev.Tool.ID == "t1.1.1" {
			rows = append(rows, *ev.Tool)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no row for the call")
	}
	for _, row := range rows {
		if row.Task != nil && (row.Task.Status == SubagentFailed || row.Task.DurationMs != 0) {
			t.Fatalf("the call's row carried the child's finish: %+v", row.Task)
		}
	}
	last := rows[len(rows)-1]
	if last.Task == nil || last.Task.Status != SubagentCompleted || !last.Task.Receipt || !last.Task.Background || last.Task.AgentID != id {
		t.Fatalf("the call's final row is %+v (task %+v); want the acknowledgement's", last, last.Task)
	}
	fin := w.wait("the child's finished row", func(ev Event) bool { return isRoster(ev, id, SubagentChangeFinished) }).Subagent
	if fin.Status != SubagentFailed {
		t.Fatalf("the child's roster row is %+v; want failed", fin)
	}
}

// journaledWakeRig is a wake rig with a journal, and the reads of it.
func journaledWakeRig(t *testing.T) (*wakeRig, func() []map[string]any) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "journal")
	rig := newWakeRig(t, Options{JournalDir: dir}, nil)
	jw := journalOf(t, rig.s.log)
	inc := rig.s.Incarnation()
	return rig, func() []map[string]any {
		closeJournaled(t, rig.s, jw)
		return diags(assertOneJournal(t, dir, jw, inc), diagSubagentWake)
	}
}

// oneWakeEnded checks one wake ran and ended — a balanced pair of brackets,
// no EventDone or EventError after the user's turn's, the claim released
// (ForeignTurn false, nothing claimed), the ending's recheck stood down.
func oneWakeEnded(t *testing.T, rig *wakeRig) {
	t.Helper()
	rig.bracket(true, 1, "wake-1")
	rig.bracket(false, 1, "wake-1")
	rig.awaitDecided(false, "the wake's ending")
	rig.noTerminalAfterTheFirst(1)
	if n := rig.w.count(EventForeignTurn); n != 2 {
		t.Fatalf("%d brackets, want one wake's two", n)
	}
	s := rig.s
	s.mu.Lock()
	held := s.claimed || s.inPrompt || s.wake || s.foreign
	s.mu.Unlock()
	if held || s.ForeignTurn() {
		t.Fatal("the wake's claim is still held after its ending")
	}
}

// wantWakeNote checks the journal's one subagent_wake note.
func wantWakeNote(t *testing.T, notes []map[string]any, outcome, errText string) {
	t.Helper()
	if len(notes) != 1 || notes[0]["wake"] != "wake-1" || notes[0]["outcome"] != outcome {
		t.Fatalf("the wake's notes are %v; want wake-1 %s", notes, outcome)
	}
	if e, _ := notes[0]["error"].(string); !strings.Contains(e, errText) {
		t.Fatalf("the note's error is %q, want it to mention %q", e, errText)
	}
}

// TestNativeWakeEndsOnEveryPath (astra r19's untested paths): a wake whose
// provider fails, one that ends cleanly having said nothing, one whose turn
// panics, and one that opens an ask — each ends through the one ending:
// balanced brackets, no EventDone or EventError, the claim released, the
// journal's outcome right, and the next turn a person starts runs.
func TestNativeWakeEndsOnEveryPath(t *testing.T) {
	t.Run("a provider failure", func(t *testing.T) {
		rig, notes := journaledWakeRig(t)
		child := newHeld(t)
		id := rig.spawnOne(child, reply(errorParts(errors.New("the provider is down"))), answer("later ok"))
		rig.finish(child, id)
		rig.awaitDecided(true, "the result pending")
		oneWakeEnded(t, rig)
		if rig.s.hs.HasPending() {
			t.Fatal("a failed wake's result is suspended, never pending")
		}
		if _, err := rig.s.Prompt(context.Background(), "later"); err != nil {
			t.Fatalf("a prompt after the failed wake: %v", err)
		}
		rig.w.waitTerminals(2)
		wantWakeNote(t, notes(), "failed", "the provider is down")
	})

	t.Run("a clean empty answer", func(t *testing.T) {
		rig, notes := journaledWakeRig(t)
		child := newHeld(t)
		// An empty response with usage is a clean stop that persists nothing
		// (harness: TestCleanEmptyWakeSuspends): the harness sets the result
		// aside, and the wake's own record is a clean end.
		id := rig.spawnOne(child, reply(finishParts(fantasy.FinishReasonStop)), answer("later ok"))
		rig.finish(child, id)
		rig.awaitDecided(true, "the result pending")
		oneWakeEnded(t, rig)
		if rig.s.hs.HasPending() {
			t.Fatal("an empty wake's result is suspended, never pending")
		}
		if got := mainText(rig.w.events()); got != "started" {
			t.Fatalf("the session's text is %q; want the user's turn's alone", got)
		}
		if _, err := rig.s.Prompt(context.Background(), "later"); err != nil {
			t.Fatalf("a prompt after the empty wake: %v", err)
		}
		rig.w.waitTerminals(2)
		n := notes()
		wantWakeNote(t, n, "delivered", "")
		if n[0]["stop_reason"] != "end_turn" {
			t.Fatalf("the note is %v; want stop_reason end_turn", n[0])
		}
	})

	t.Run("a recovered panic", func(t *testing.T) {
		rig, notes := journaledWakeRig(t)
		child := newHeld(t)
		id := rig.spawnOne(child, answer("boom"), answer("later ok"))
		// Armed after the user's turn, whose own text would otherwise trip
		// it: the wake's first text delta panics inside the sink, on the
		// harness's goroutine, from where nothing but the worker's recover
		// would catch it.
		var armed atomic.Bool
		rig.hookBefore(func(ev harness.Event) {
			if _, ok := ev.(harness.TextDelta); ok && armed.CompareAndSwap(true, false) {
				panic("the sink panicked")
			}
		})
		armed.Store(true)
		rig.finish(child, id)
		rig.awaitDecided(true, "the result pending")
		oneWakeEnded(t, rig)
		rig.hookBefore(nil)
		if _, err := rig.s.Prompt(context.Background(), "later"); err != nil {
			t.Fatalf("a prompt after the panicked wake: %v", err)
		}
		rig.w.waitTerminals(2)
		wantWakeNote(t, notes(), "failed", "the sink panicked")
	})

	t.Run("an ask the cancel answers", func(t *testing.T) {
		rig, notes := journaledWakeRig(t)
		s, w := rig.s, rig.w
		child := newHeld(t)
		id := rig.spawnOne(child, nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which?", "alpha", "beta")),
			answer("never asked"), answer("later ok"))
		rig.finish(child, id)
		rig.awaitDecided(true, "the result pending")
		rig.bracket(true, 1, "wake-1")
		q := w.waitType(EventQuestion).Question
		if rec, ok := s.Asks().Record(q.ID); !ok || rec.Status != AskOpen || rec.Token != turnTokenOf(s) {
			t.Fatalf("the ask's record is %+v; want open on the wake's token", rec)
		}
		if out, err := s.Cancel(context.Background()); err != nil || !out.Wrote || !out.Settled {
			t.Fatalf("Cancel during the wake's ask = %+v, %v", out, err)
		}
		oneWakeEnded(t, rig)
		asks := w.asksOf()
		if len(asks) != 1 || asks[0].ID != q.ID || asks[0].Outcome != AskCancelled {
			t.Fatalf("the ask's endings are %+v; want the one, cancelled", asks)
		}
		if _, err := s.Prompt(context.Background(), "later"); err != nil {
			t.Fatalf("a prompt after the cancelled wake: %v", err)
		}
		w.waitTerminals(2)
		wantWakeNote(t, notes(), "cancelled", "")
	})

	t.Run("an ask the user answers", func(t *testing.T) {
		rig, notes := journaledWakeRig(t)
		s, w := rig.s, rig.w
		child := newHeld(t)
		id := rig.spawnOne(child, nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which?", "alpha", "beta")),
			answer("went with beta"))
		rig.finish(child, id)
		rig.awaitDecided(true, "the result pending")
		rig.bracket(true, 1, "wake-1")
		q := w.waitType(EventQuestion).Question
		token := turnTokenOf(s)
		answerQuestion(t, s, q, 1)
		oneWakeEnded(t, rig)
		asks := w.asksOf()
		if len(asks) != 1 || asks[0].ID != q.ID || asks[0].Outcome != AskAnswered {
			t.Fatalf("the ask's endings are %+v; want the one, answered", asks)
		}
		if rec, ok := s.Asks().Record(q.ID); !ok || rec.Token != token || rec.Status == AskOpen {
			t.Fatalf("the ask's record is %+v; want ended on the wake's token", rec)
		}
		if got := mainText(w.events()); !strings.HasSuffix(got, "went with beta") {
			t.Fatalf("the wake's text is %q", got)
		}
		wantWakeNote(t, notes(), "delivered", "")
	})
}

// mainText is the main session's text, the children's left out.
func mainText(evs []Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Type == EventText && ev.Agent == "" {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}
