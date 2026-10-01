package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
)

// The Stub's half of the ask seam (plan 021 A13) and the TUI's half of §3.6:
// an ending removes the card, and the row its answer earned is the shared
// model's, drawn by the fold (plan 032 C4).

// Stub.Emit routes a card event through the registry with the test's OWN id,
// and the opening is buffered by the time Emit returns — which is what some
// forty test sites rely on when they emit a card and then look.
func TestStubEmitAdoptsTheTestsIDAndBuffersTheOpening(t *testing.T) {
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	stub.Emit(agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})

	open := stub.Asks().Asks()
	if len(open) != 1 || open[0].ID != "ask-1" || open[0].Kind != agent.AskQuestion {
		t.Fatalf("the test's id must survive: %+v", open)
	}
	select {
	case ev := <-stub.Events():
		if ev.Type != agent.EventQuestion || ev.Question.ID != "ask-1" {
			t.Fatalf("the opening is %+v", ev)
		}
	default:
		t.Fatal("emitted means buffered: the opening was not in the primary when Emit returned")
	}
}

// An Auto question or plan is a request craze has already answered: it opens
// nothing, exactly as the live session's automatic path parks nothing.
func TestStubEmitDoesNotOpenAnAutoRequest(t *testing.T) {
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	q := stubQuestion()
	q.Auto = true
	stub.Emit(agent.Event{Type: agent.EventQuestion, Question: q})
	if open := stub.Asks().Asks(); len(open) != 0 {
		t.Fatalf("an auto-answered request opened an ask: %+v", open)
	}
}

// A card id a test re-uses once the first card has been answered is a SECOND
// ask, not a correction of the first. Calls() is rebuilt from the registry's
// terminal records rather than kept beside them, so it must hold every answer
// the stub was given, in the order it was given them, however the tests
// numbered their cards (review r18, finding 1).
func TestStubCallsKeepEveryAnswerWhenACardIDIsReused(t *testing.T) {
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })

	stub.Emit(agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if _, err := stub.Asks().Answer("", "ask-1", agent.AskAnswer{Skip: true}); err != nil {
		t.Fatalf("the first answer: %v", err)
	}
	// The same id again, now that the ask holding it has ended.
	stub.Emit(agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if _, err := stub.Asks().Answer("", "ask-1",
		agent.AskAnswer{Answers: map[string][]string{"q1": {"opt-b"}}}); err != nil {
		t.Fatalf("the second answer: %v", err)
	}

	calls := stub.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls() is %+v, want both answers: one card id is not one card", calls)
	}
	if calls[0].ID != "ask-1" || !calls[0].Skip || calls[0].Cancelled {
		t.Fatalf("the first call is %+v, want the skip", calls[0])
	}
	if calls[1].ID != "ask-1" || calls[1].Skip || calls[1].Answers["q1"][0] != "opt-b" {
		t.Fatalf("the second call is %+v, want the answer that followed it", calls[1])
	}
}

// A-X3, through Control: an ask answered twice yields one ending and one
// ErrAlreadyResolved; an invalid answer returns ErrBadAnswer, emits nothing,
// and the ask is still answerable.
func TestAnswerThroughControl(t *testing.T) {
	m, stub := sizedCards(t)
	stub.Emit(agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})

	// Each call below is a distinct user action against the card — a wrong
	// answer, then a corrected one, then a stale retry — so each mints its own
	// command id, exactly as cards.go's answerCard does on every keypress
	// (plan 021 C11: a resend is the SAME id with the SAME payload, never a
	// second attempt with a different one, which a command id table now
	// answers with ErrBadRequest rather than running).
	if err := m.eng.Answer(context.Background(), m.nextCmd(), "ask-1", agent.AskAnswer{OptionID: "opt-a"}); !errors.Is(err, agent.ErrBadAnswer) {
		t.Fatalf("a permission's answer must not fit a question: %v", err)
	}
	if engine.Code(errors.New("x")) == "" {
		t.Fatal("unreachable")
	}
	if got := len(stub.Calls()); got != 0 {
		t.Fatalf("a refused answer resolved the ask: %+v", stub.Calls())
	}
	if err := m.eng.Answer(context.Background(), m.nextCmd(), "ask-1", agent.AskAnswer{Skip: true}); err != nil {
		t.Fatalf("the ask must still be answerable: %v", err)
	}
	if err := m.eng.Answer(context.Background(), m.nextCmd(), "ask-1", agent.AskAnswer{Skip: true}); !errors.Is(err, agent.ErrAlreadyResolved) {
		t.Fatalf("a second answer: %v", err)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || !calls[0].Skip {
		t.Fatalf("one ending, and it is the skip: %+v", calls)
	}
	if err := m.eng.Answer(context.Background(), m.nextCmd(), "ask-404", agent.AskAnswer{Skip: true}); !errors.Is(err, agent.ErrUnknownAsk) {
		t.Fatalf("an id nobody issued: %v", err)
	}
}

// The protocol codes an ask's refusals answer with (05's closed set).
func TestAskErrorCodes(t *testing.T) {
	for err, want := range map[error]string{
		agent.ErrBadAnswer:       "bad_request",
		agent.ErrAlreadyResolved: "already_resolved",
		agent.ErrUnknownAsk:      "unknown_ask",
		agent.ErrAskUnavailable:  "unavailable",
	} {
		if got := engine.Code(err); got != want {
			t.Fatalf("%v is %q, want %q", err, got, want)
		}
	}
}

// A permission kind the request never offered is the explicit cancel, not an
// empty option id: an empty id is not an option, and must not be able to
// cancel a request by accident (panel CodeRabbit 14).
func TestAMissingPermissionKindIsAnExplicitCancel(t *testing.T) {
	m, stub := sizedCards(t)
	m.yolo = false
	perm := &agent.PermissionEvent{
		ID:      "perm-1",
		Tool:    "Shell",
		Options: []agent.PermissionOption{{OptionID: "opt-always", Kind: kindAllowAlways}},
	}
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: perm})
	// The key is not bound, so the card stays; answerPermission is called
	// directly, which is the call that used to send "".
	tm, _ := m.answerPermission(kindAllowOnce)
	m = tm.(Model)
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "perm-1" || !calls[0].Cancelled || calls[0].Option != "" {
		t.Fatalf("a kind the request never offered is a cancel: %+v", calls)
	}
	if m.cardOpen() {
		t.Fatal("a cancel answers the request, so the card goes")
	}
}

// ErrBadAnswer leaves the ask open, and the card is popped before the answer is
// sent — so the card comes back at the head rather than the agent waiting for
// an Esc (panel CodeRabbit 14).
func TestABadAnswerReRaisesTheCard(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})
	head, ok := m.popCard()
	if !ok {
		t.Fatal("fixture: no card")
	}
	// A plan's answer against a question's id: nothing fits, so nothing is
	// claimed, and the answer's continuation puts the card back (C18c:
	// answerCard goes through the command gate).
	m, _ = m.answerCard(head, "ask-1", agent.AskAnswer{Accept: true})
	if len(m.cards) != 2 || m.cards[0].kind != cardQuestion {
		t.Fatalf("the card must come back at the head: %+v", m.cards)
	}
	if got := len(stub.Calls()); got != 0 {
		t.Fatalf("nothing was resolved: %+v", stub.Calls())
	}
	errs := texts(m, entryError)
	if len(errs) != 1 || !strings.Contains(errs[0], "that answer does not fit") {
		t.Fatalf("the error is still reported: %q", errs)
	}
}

// An ending this model did not cause — another client answered, or a cancel,
// or the turn went — removes the card wherever it is in the queue. The row an
// answer earns is the fold's (plan 032 C4): a shared row, the same entry every
// client folding the ending holds, and nothing for an ending that decided
// nothing.
func TestAnEndingFromAnotherClientRemovesTheCardAndWritesTheRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  *agent.AskUpdate
		want string
		gone bool
	}{
		{
			name: "question answered",
			ask: &agent.AskUpdate{
				ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered,
				Answers: map[string][]string{"q1": {"opt-b"}},
			},
			want: "? Pick one → B",
			gone: true,
		},
		{
			name: "question skipped",
			ask: &agent.AskUpdate{
				ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, Skip: true,
			},
			want: "? Question → skipped",
			gone: true,
		},
		{
			name: "question cancelled",
			ask: &agent.AskUpdate{
				ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskCancelled, By: agent.AskByCancel,
			},
			gone: true,
		},
		{
			name: "question ended with its turn",
			ask: &agent.AskUpdate{
				ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskTurnEnded, By: agent.AskByTurn,
			},
			gone: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := sizedCards(t)
			m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
			if !m.cardOpen() {
				t.Fatal("fixture: no card")
			}
			tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventAsk, Ask: tc.ask}})
			m = tm.(Model)
			if m.cardOpen() != !tc.gone {
				t.Fatalf("card open = %v, want %v", m.cardOpen(), !tc.gone)
			}
			view := plainView(m)
			if tc.want != "" && !strings.Contains(view, tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, view)
			}
			if tc.want == "" && (strings.Contains(view, "→") || strings.Contains(view, "skipped")) {
				t.Fatalf("an ending that decided nothing wrote a row:\n%s", view)
			}
			if tc.want != "" {
				// The fold's row, not this client's: it shows an entry of the
				// shared model, which holds the note.
				r := noteRow(t, m, tc.want)
				if r.local || r.id.IsZero() {
					t.Fatalf("the outcome row is this client's own (local %v, id %v), not the shared model's", r.local, r.id)
				}
				if !slices.Contains(sharedNotes(m), tc.want) {
					t.Fatalf("the shared model holds notes %q, not %q", sharedNotes(m), tc.want)
				}
			}
		})
	}
}

// A plan answered elsewhere draws its verb: the fold's note, the same one this
// client's own answer draws.
func TestAPlanEndingFromAnotherClientWritesItsVerb(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})
	tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
		ID: "plan-1", Kind: agent.AskPlan, Outcome: agent.AskAnswered, Accepted: true,
	}}})
	m = tm.(Model)
	if m.cardOpen() {
		t.Fatal("the card is removed")
	}
	if !strings.Contains(plainView(m), "plan Fake Plan → accepted") {
		t.Fatalf("missing the note:\n%s", plainView(m))
	}
}

// A permission answered elsewhere removes the card and writes nothing: a
// permission answer has never written a row.
func TestAPermissionEndingWritesNoRow(t *testing.T) {
	m, stub := sizedCards(t)
	m.yolo = false
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
	tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
		ID: "perm-1", Kind: agent.AskPermission, Outcome: agent.AskAnswered,
		OptionID: "opt-once", Label: "Allow once",
	}}})
	m = tm.(Model)
	if m.cardOpen() {
		t.Fatal("the card is removed")
	}
	if strings.Contains(plainView(m), "Allow once") {
		t.Fatalf("a permission answer writes no row:\n%s", plainView(m))
	}
}

// This model's own answer writes no row of its own (plan 032 C4): its ending,
// carrying this model's cause, is what draws the note — through the fold, once,
// as a shared row — and the same ending delivered again finds the ask gone
// and draws nothing more.
func TestTheModelsOwnAnswerDrawsItsRowFromItsEnding(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.cardOpen() {
		t.Fatal("fixture: the skip did not take the card")
	}
	if n := strings.Count(plainView(m), "→ skipped"); n != 0 {
		t.Fatalf("the answer wrote a row of its own before its ending, %d:\n%s", n, plainView(m))
	}
	end := awaitStubEvent(t, stub, agent.EventAsk)
	if end.Cause == "" || end.Ask == nil || !end.Ask.Skip {
		t.Fatalf("fixture: the ending is %+v", end)
	}
	m = feed(t, m, end)
	if n := strings.Count(plainView(m), "→ skipped"); n != 1 {
		t.Fatalf("its ending drew %d skip notes, want one:\n%s", n, plainView(m))
	}
	if r := noteRow(t, m, "? Question → skipped"); r.local || r.id.IsZero() {
		t.Fatalf("the skip note is this client's own (local %v, id %v), not the shared model's", r.local, r.id)
	}
	// The same ending again: the ask is no longer open, so no second note.
	again := end
	again.Seq = 0
	m = feed(t, m, again)
	if n := strings.Count(plainView(m), "→ skipped"); n != 1 {
		t.Fatalf("a re-delivered ending doubled the note, %d:\n%s", n, plainView(m))
	}
}

// noteRow is the main pane's note row reading text; it fails the test when
// there is none.
func noteRow(t *testing.T, m Model, text string) *entry {
	t.Helper()
	for _, r := range m.main.rows {
		if r.kind == entryNote && r.text == text {
			return r
		}
	}
	t.Fatalf("no note row %q in %v", text, texts(m, entryNote))
	return nil
}

// sharedNotes is the text of every note the shared model's main transcript
// holds, oldest first.
func sharedNotes(m Model) []string {
	var out []string
	for _, e := range m.shared.History().Main.Entries {
		if e.Kind == transcript.KindNote {
			out = append(out, e.Text)
		}
	}
	return out
}

// State.PendingAsks and State.HeadAsk are what a host with no TUI publishes
// instead of the model's own head card, so the two must read the same: one
// derivation (agent.AskLabel), used by both.
func TestStateHeadAskMatchesTheCardTheModelDraws(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   agent.Event
		kind agent.AskKind
	}{
		{"permission", agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)}, agent.AskPermission},
		{"question", agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}, agent.AskQuestion},
		{"plan", agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()}, agent.AskPlan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := sizedCards(t)
			m.yolo = false
			m = cardEvent(t, m, stub, tc.ev)
			st := engineOf(t, m).State()
			if st.PendingAsks != 1 || st.HeadAsk.Kind != tc.kind {
				t.Fatalf("state %+v", st.HeadAsk)
			}
			c, ok := m.headCard()
			if !ok {
				t.Fatal("no card")
			}
			_, label := hostCard(c)
			if st.HeadAsk.Label != label {
				t.Fatalf("the engine says %q and the model draws %q", st.HeadAsk.Label, label)
			}
		})
	}
}

// A-X4 over the Stub: a turn that ends with a card open ends that ask, and the
// card goes with it (plan 021 §4's second recorded behaviour change).
//
// The turn ends **on its own** — a held script released, not the engine closing
// — because a close would satisfy every assertion here by a different route
// (asks.Close ends everything too) and prove none of them. What is asserted is
// what the name says: the ending is turn_ended, it is in the record BEFORE the
// turn's own terminal event, and the model that applies it loses the card.
func TestAStubTurnEndingEndsItsOpenCard(t *testing.T) {
	m, sess := scriptedModel(t)
	sc := sess.Script(scriptHeld())
	// The turn is driven straight from the seam, so this test owns the primary
	// and can assert on the order the log committed things in. sc.opened is the
	// barrier: the turn is open, so the card below belongs to it.
	run := sess.Begin("go")
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_, _ = run(context.Background())
	}()
	awaitBarrier(t, sc.opened, "the scripted turn opening")

	// Through the registry alone: what the model is given is what the log
	// delivered, replayed below in order, so nothing here is applied twice.
	sess.Emit(agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if open := sess.Asks().Asks(); len(open) != 1 || open[0].ID != "ask-1" {
		t.Fatalf("fixture: no parked ask: %+v", open)
	}

	sc.Release()
	// The producer is joined before anything is counted: the continuation has
	// published everything it will ever publish.
	awaitBarrier(t, returned, "the turn's continuation returning")
	// Everything the turn published, in the order the log committed it: the
	// session ends the turn for the registry and waits for the outbox before it
	// publishes its terminal event, so the ask's ending cannot be behind it.
	evs := stubEventsUntil(t, sess.Stub, agent.EventDone)
	var ending *agent.AskUpdate
	for _, ev := range evs {
		switch {
		case ev.Type == agent.EventAsk && ev.Ask != nil && ev.Ask.ID == "ask-1":
			if ending != nil {
				t.Fatalf("ask-1 ended twice: %+v then %+v", ending, ev.Ask)
			}
			ending = ev.Ask
		case ev.Type == agent.EventDone && ending == nil:
			t.Fatalf("the turn's own ending came first: %+v", evs)
		}
	}
	if ending == nil {
		t.Fatalf("the turn ended with no ending for the card it held: %+v", evs)
	}
	if ending.Outcome != agent.AskTurnEnded || ending.By != agent.AskByTurn {
		t.Fatalf("ending %+v, want turn_ended by the turn", ending)
	}
	if open := sess.Asks().Asks(); len(open) != 0 {
		t.Fatalf("something is still parked: %+v", open)
	}

	// And the model, applying what the log delivered, raises the card on the
	// opening and loses it on the ending.
	raised := false
	for _, ev := range evs {
		tm, _ := m.Update(eventMsg{ev: ev})
		m = tm.(Model)
		if ev.Type == agent.EventQuestion {
			raised = m.cardOpen()
		}
	}
	if !raised {
		t.Fatal("the opening raised no card, so losing it proves nothing")
	}
	if m.cardOpen() {
		t.Fatalf("the card outlived its turn: %+v", m.cards)
	}
	calls := sess.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || !calls[0].Cancelled {
		t.Fatalf("the ask ends exactly once when its turn goes: %+v", calls)
	}
}

// A-X4 over the Stub: an ask opened between turns is exactly the one no turn's
// ending may take away.
func TestAStubCardBetweenTurnsSurvivesATurn(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if _, err := stub.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := stub.Calls(); len(got) != 0 {
		t.Fatalf("a turn ended an ask that belonged to no turn: %+v", got)
	}
	if open := stub.Asks().Asks(); len(open) != 1 || open[0].ID != "ask-1" {
		t.Fatalf("the ask must still be open: %+v", open)
	}
	_ = m
}

// outboxFillCap bounds the fill below: the soft bound is thousands of events,
// never millions, so a loop that has enqueued this many and still found room
// is a broken bound rather than a slow one.
const outboxFillCap = 1 << 16

// saturate backs the session's log up until every *rejectable* admission —
// an Answer among them — is refused for want of room (plan 021 §3.3). Nothing
// is closed and nothing is lost: the primary's buffer is filled so the outbox's
// drainer blocks on its send, and batches are enqueued behind it until the
// outbox is over its own soft bound.
//
// The release frees exactly one slot of the primary, which lets the drainer
// commit the one batch it was blocked on and takes the outbox back under the
// bound — the backlog draining, as a reader that starts reading again drains it.
func saturate(t *testing.T, stub *Stub) (release func()) {
	t.Helper()
	log := stub.EventLog()
	// Only the room that is left: whatever the test emitted before this is
	// already in the buffer, and one publish too many would block for ever on a
	// reader that is not coming.
	for range cap(stub.Events()) - len(stub.Events()) {
		if !log.Publish(context.Background(), nil, agent.Event{Type: agent.EventText, Text: "fill"}) {
			t.Fatal("filling the primary: a publish was refused")
		}
	}
	for n := 0; log.OutboxRoom(); n++ {
		if n > outboxFillCap {
			t.Fatalf("the outbox still had room after %d enqueued events", n)
		}
		log.Enqueue(agent.Event{Type: agent.EventText, Text: "behind a reader that stopped"})
	}
	return func() {
		t.Helper()
		<-stub.Events()
		// One slot is enough: the drainer commits the batch it was blocked on
		// and the outbox is under its bound again. The commit is on the
		// drainer's own goroutine, so this waits for that progress with the
		// watchdog as its only clock — never for a duration.
		deadline := time.Now().Add(stubEventWait)
		for !log.OutboxRoom() {
			if time.Now().After(deadline) {
				t.Fatalf("the outbox was still over its bound %v after a slot was freed", stubEventWait)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// Finding 5 of review r17: an answer the log had no room for must not cost the
// card. Control.Answer refuses ErrAskUnavailable **without resolving the ask**,
// and no second opening is ever published, so a popped card that is not put
// back is a provider parked for ever on a question nobody can see.
func TestAnAnswerRefusedForRoomKeepsTheCard(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	release := saturate(t, stub)

	// Esc skips the question: the card is popped, and the answer is refused.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cardOpen() {
		t.Fatal("the card was popped and never put back: nothing would ever raise it again")
	}
	if open := stub.Asks().Asks(); len(open) != 1 || open[0].ID != "ask-1" {
		t.Fatalf("a refusal for room must leave the ask untouched: %+v", open)
	}
	if got := len(stub.Calls()); got != 0 {
		t.Fatalf("nothing was resolved: %+v", stub.Calls())
	}
	errs := texts(m, entryError)
	if len(errs) != 1 || !strings.Contains(errs[0], "backed up") {
		t.Fatalf("the user is told to press again: %q", errs)
	}

	// Once the backlog drains, the same key answers it.
	release()
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.cardOpen() {
		t.Fatalf("the retry did not take: %+v", m.cards)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || !calls[0].Skip {
		t.Fatalf("one answer, and it is the skip: %+v", calls)
	}
}

// hiddenAnswered runs the hidden answers an Update handed back — commands of
// their own, fire-and-forget (plan 027 §3.12, C18c) — and delivers what each
// answered, as the program would: the refusals for room go back on the retry
// list. Every other command is left alone.
func hiddenAnswered(t *testing.T) func(tea.Model, tea.Cmd) Model {
	return func(tm tea.Model, cmd tea.Cmd) Model {
		t.Helper()
		m := tm.(Model)
		for _, c := range hiddenAnswerCmds(cmd) {
			if msg := runWatched(t, c); msg != nil {
				tm, _ := m.Update(msg)
				m = tm.(Model)
			}
		}
		return m
	}
}

// hiddenAnswerCmds is the hidden answers among cmd's commands, in order.
func hiddenAnswerCmds(cmd tea.Cmd) []tea.Cmd {
	return cmdsNamed(cmd, tuiPkg+"(*Model).answerHidden")
}

// hiddenAsksModel is a started model over a Stub whose provider has no
// capabilities at all: questions and plans are hidden, which is the path that
// answers with no card and no row.
func hiddenAsksModel(t *testing.T) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(plantHidden(t))
	m := startStub(t, stub, t.TempDir(), 80, 24)
	if m.showAsk() {
		t.Fatal("the fixture needs a provider whose questions are hidden")
	}
	return m, stub
}

// TestAHiddenAsksEndingDrawsNoRowOnAnyFolder (plan 032 C4): an ask the
// provider's capabilities hide is answered unseen — a question skipped, a plan
// rejected — and its ending draws no outcome note on either folder in process:
// the engine's model, built with the provider's capabilities, and this
// client's, built with its backend's (adopt). Their notes would otherwise
// claim the user skipped or rejected what they were never shown.
func TestAHiddenAsksEndingDrawsNoRowOnAnyFolder(t *testing.T) {
	m, stub := hiddenAsksModel(t)
	if want := (transcript.HiddenAsks{Questions: true, Plans: true}); m.shared.Hidden() != want {
		t.Fatalf("the client's model hides %+v, want %+v: its backend's capabilities", m.shared.Hidden(), want)
	}
	before := plainView(m)
	for _, ev := range []agent.Event{
		{Type: agent.EventQuestion, Question: stubQuestion()},
		{Type: agent.EventPlan, Plan: stubPlanEvent()},
	} {
		stub.Emit(ev)
		m = hiddenAnswered(t)(m.Update(eventMsg{ev: ev}))
		m = feed(t, m, awaitStubEvent(t, stub, agent.EventAsk))
	}
	calls := stub.Calls()
	if len(calls) != 2 || !calls[0].Skip || calls[1].Accept {
		t.Fatalf("the hidden asks were answered %+v, want a skip and a reject", calls)
	}
	if m.cardOpen() || len(texts(m, entryNote)) != 0 {
		t.Fatalf("a hidden ask drew a card (%v) or a note %q:\n%s\nbefore:\n%s", m.cardOpen(), texts(m, entryNote), plainView(m), before)
	}
	snap, err := engineOf(t, m).TranscriptSnapshot("", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snap.Main.Entries {
		if e.Kind == transcript.KindNote {
			t.Fatalf("the engine's model drew %q for a hidden ask", e.Text)
		}
	}
}

// The hidden half of finding 5: a question the config shows no card for is
// answered where it stands, so a refusal for room there is a provider parked
// with nothing on screen to retry it. The answer is kept and retried on the
// next event the model applies — the fast path; the beat below is what carries
// the retry when no event ever follows.
func TestAHiddenAnswerRefusedForRoomIsRetried(t *testing.T) {
	m, stub := hiddenAsksModel(t)

	// The ask is opened while there is still room, and delivered to the model
	// after the log has backed up: the opening was already on its way.
	ev := agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}
	stub.Emit(ev)
	release := saturate(t, stub)

	m = hiddenAnswered(t)(m.Update(eventMsg{ev: ev}))
	if m.cardOpen() {
		t.Fatalf("a hidden question raised a card: %+v", m.cards)
	}
	if len(m.hiddenRetry) != 1 {
		t.Fatalf("the unanswered hidden ask must be kept: %+v", m.hiddenRetry)
	}
	if got := len(stub.Calls()); got != 0 {
		t.Fatalf("a refusal for room resolved something: %+v", stub.Calls())
	}

	// The backlog drains, and the next event the model applies carries the
	// retry with it: no timer, and nothing to press.
	release()
	m = hiddenAnswered(t)(m.Update(eventMsg{ev: agent.Event{Type: agent.EventText, Text: "the agent carries on"}}))
	if len(m.hiddenRetry) != 0 {
		t.Fatalf("the retry is spent once taken: %+v", m.hiddenRetry)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || !calls[0].Skip {
		t.Fatalf("the hidden question is skipped, once: %+v", calls)
	}
	if open := stub.Asks().Asks(); len(open) != 0 {
		t.Fatalf("the provider is still parked: %+v", open)
	}
}

// Finding 1 of review r19: the event-driven retry is only the fast path, and
// the schedule that has no fast path is the one the reviewer traced. The final
// batch's LAST event is what gives the outbox its room back, so the retry that
// event carries runs before the room is there and is refused — and then the
// drainer goes idle and no event ever follows. Nothing here delivers one: the
// answer's own beat is the only thing that can take it, and there is exactly
// one of those in flight at a time and none once the list is empty.
func TestAHiddenAnswerRefusedForRoomIsRetriedByItsOwnBeat(t *testing.T) {
	m, stub := hiddenAsksModel(t)
	if cmd := m.armHiddenRetry(); cmd != nil {
		t.Fatal("a beat was armed with nothing waiting for room")
	}

	ev := agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}
	stub.Emit(ev)
	release := saturate(t, stub)

	m = hiddenAnswered(t)(m.Update(eventMsg{ev: ev}))
	if len(m.hiddenRetry) != 1 || !m.hiddenRetryLive {
		t.Fatalf("a refusal for room must keep the answer and arm its beat: %+v live=%v",
			m.hiddenRetry, m.hiddenRetryLive)
	}
	// Arming is the only place a beat is made, so this is "never more than one
	// in flight" said in full.
	if cmd := m.armHiddenRetry(); cmd != nil {
		t.Fatal("a second beat was armed while one was still in flight")
	}

	// A beat while the log is still backed up: refused again, kept again, and
	// the next beat is armed by the same one-in-flight rule.
	m = hiddenAnswered(t)(m.Update(hiddenRetryMsg{}))
	if len(m.hiddenRetry) != 1 || !m.hiddenRetryLive {
		t.Fatalf("a beat refused for room must keep going: %+v live=%v", m.hiddenRetry, m.hiddenRetryLive)
	}
	if got := len(stub.Calls()); got != 0 {
		t.Fatalf("a refusal for room resolved something: %+v", stub.Calls())
	}

	// The backlog drains, with no event left for the model to apply. The beat
	// alone takes the answer.
	release()
	m = hiddenAnswered(t)(m.Update(hiddenRetryMsg{}))
	if len(m.hiddenRetry) != 0 || m.hiddenRetryLive {
		t.Fatalf("the retry is spent once taken: %+v live=%v", m.hiddenRetry, m.hiddenRetryLive)
	}
	if cmd := m.armHiddenRetry(); cmd != nil {
		t.Fatal("a beat was armed with nothing left waiting for room")
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || !calls[0].Skip {
		t.Fatalf("the hidden question is skipped, once: %+v", calls)
	}
	if open := stub.Asks().Asks(); len(open) != 0 {
		t.Fatalf("the provider is still parked: %+v", open)
	}
}

// Finding 7 of review r17: the loser of a two-client answer race must not erase
// the winner's row. The card was on screen, so the winner's ending has not been
// applied yet and is on its way: the card goes back with no error row, and that
// ending removes it and writes the winner's answer through the ordinary path.
func TestTheLoserOfAnAnswerRaceKeepsTheWinnersRow(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})

	// Another client on the same engine answers first.
	other := engine.Command{Client: engineOf(t, m).NewClientID(), ID: "1"}
	if err := engineOf(t, m).Answer(other, "ask-1", agent.AskAnswer{Answers: map[string][]string{"q1": {"opt-b"}}}); err != nil {
		t.Fatalf("the other client's answer: %v", err)
	}

	// This client answers the card it is still showing, and loses.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cardOpen() {
		t.Fatal("the card must stay until the winner's ending removes it")
	}
	if errs := texts(m, entryError); len(errs) != 0 {
		t.Fatalf("losing a race is not an error the user can do anything about: %q", errs)
	}
	if strings.Contains(plainView(m), "→ skipped") {
		t.Fatalf("the losing answer wrote its own row:\n%s", plainView(m))
	}

	// The winner's ending, as the log published it.
	tm, _ := m.Update(eventMsg{ev: awaitStubEvent(t, stub, agent.EventAsk)})
	m = tm.(Model)
	if m.cardOpen() {
		t.Fatalf("the winner's ending removes the card: %+v", m.cards)
	}
	if !strings.Contains(plainView(m), "? Pick one → B") {
		t.Fatalf("the winner's answer is missing from the transcript:\n%s", plainView(m))
	}
}

// An ending whose opening the fold never saw draws nothing: every ending that
// carries its own Body, which by definition had none published.
func TestAnEndingForNoCardWritesNothing(t *testing.T) {
	m, _ := sizedCards(t)
	before := plainView(m)
	tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
		ID: "ask-9", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, Skip: true,
		Body: &agent.AskBody{Question: stubQuestion()},
	}}})
	if got := plainView(tm.(Model)); got != before {
		t.Fatalf("an ending nobody raised a card for changed the frame:\n%s", got)
	}
}
