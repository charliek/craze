package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// The Stub's half of the ask seam (plan 021 A13) and the TUI's half of §3.6:
// an ending that is not this client's own removes the card and writes the row.

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
	c := m.nextCmd()

	if err := m.eng.Answer(c, "ask-1", agent.AskAnswer{OptionID: "opt-a"}); !errors.Is(err, agent.ErrBadAnswer) {
		t.Fatalf("a permission's answer must not fit a question: %v", err)
	}
	if engine.Code(errors.New("x")) == "" {
		t.Fatal("unreachable")
	}
	if got := len(stub.Calls()); got != 0 {
		t.Fatalf("a refused answer resolved the ask: %+v", stub.Calls())
	}
	if err := m.eng.Answer(c, "ask-1", agent.AskAnswer{Skip: true}); err != nil {
		t.Fatalf("the ask must still be answerable: %v", err)
	}
	if err := m.eng.Answer(c, "ask-1", agent.AskAnswer{Skip: true}); !errors.Is(err, agent.ErrAlreadyResolved) {
		t.Fatalf("a second answer: %v", err)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || !calls[0].Skip {
		t.Fatalf("one ending, and it is the skip: %+v", calls)
	}
	if err := m.eng.Answer(c, "ask-404", agent.AskAnswer{Skip: true}); !errors.Is(err, agent.ErrUnknownAsk) {
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
	// claimed.
	if m.answerCard(head, "ask-1", agent.AskAnswer{Accept: true}) {
		t.Fatal("a mis-addressed answer must not be taken")
	}
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
// or the turn went — removes the card wherever it is in the queue. What it
// writes is what the local path writes for the same outcome, and as little.
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
			tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventAsk, Ask: tc.ask}})
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
		})
	}
}

// A plan answered elsewhere writes the same verb the local path writes.
func TestAPlanEndingFromAnotherClientWritesItsVerb(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
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
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
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

// This model's own answer is applied in the Update that sent it, so its ending
// is an echo: the row is not written twice.
func TestTheModelSkipsTheEndingOfItsOwnAnswer(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if n := strings.Count(plainView(m), "→ skipped"); n != 1 {
		t.Fatalf("the local path writes one note, got %d:\n%s", n, plainView(m))
	}
	// The ending, with this model's own cause on it.
	cause := m.askEchoes[0]
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventAsk, Cause: cause, Ask: &agent.AskUpdate{
		ID: "ask-1", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, Skip: true,
	}}})
	m = tm.(Model)
	if n := strings.Count(plainView(m), "→ skipped"); n != 1 {
		t.Fatalf("its own echo wrote the note again, got %d:\n%s", n, plainView(m))
	}
	if len(m.askEchoes) != 0 {
		t.Fatalf("the echo is spent once: %v", m.askEchoes)
	}
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
			st := m.eng.State()
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
		tm, _ := m.Update(eventMsg{ev})
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

// The hidden half of finding 5: a question the config shows no card for is
// answered where it stands, so a refusal for room there is a provider parked
// with nothing on screen to retry it. The answer is kept and retried on the
// next event the model applies.
func TestAHiddenAnswerRefusedForRoomIsRetried(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	// A provider with no capabilities at all: questions and plans are hidden,
	// which is the path that answers with no card and no row.
	stub.SetProvider(plantHidden(t))
	m := startStub(t, stub, t.TempDir(), 80, 24)
	if m.showAsk() {
		t.Fatal("the fixture needs a provider whose questions are hidden")
	}

	// The ask is opened while there is still room, and delivered to the model
	// after the log has backed up: the opening was already on its way.
	ev := agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}
	stub.Emit(ev)
	release := saturate(t, stub)

	tm, _ := m.Update(eventMsg{ev})
	m = tm.(Model)
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
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "the agent carries on"}})
	m = tm.(Model)
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

// Finding 7 of review r17: the loser of a two-client answer race must not erase
// the winner's row. The card was on screen, so the winner's ending has not been
// applied yet and is on its way: the card goes back with no error row, and that
// ending removes it and writes the winner's answer through the ordinary path.
func TestTheLoserOfAnAnswerRaceKeepsTheWinnersRow(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})

	// Another client on the same engine answers first.
	other := engine.Command{Client: m.eng.NewClientID(), ID: "1"}
	if err := m.eng.Answer(other, "ask-1", agent.AskAnswer{Answers: map[string][]string{"q1": {"opt-b"}}}); err != nil {
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
	tm, _ := m.Update(eventMsg{awaitStubEvent(t, stub, agent.EventAsk)})
	m = tm.(Model)
	if m.cardOpen() {
		t.Fatalf("the winner's ending removes the card: %+v", m.cards)
	}
	if !strings.Contains(plainView(m), "? Pick one → B") {
		t.Fatalf("the winner's answer is missing from the transcript:\n%s", plainView(m))
	}
}

// An ending for an ask this model never raised a card for writes nothing: the
// hidden paths, a masked card, and every ending that carries its own Body.
func TestAnEndingForNoCardWritesNothing(t *testing.T) {
	m, _ := sizedCards(t)
	before := plainView(m)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
		ID: "ask-9", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, Skip: true,
		Body: &agent.AskBody{Question: stubQuestion()},
	}}})
	if got := plainView(tm.(Model)); got != before {
		t.Fatalf("an ending nobody raised a card for changed the frame:\n%s", got)
	}
}
