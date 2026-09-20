package tui

import (
	"errors"
	"strings"
	"testing"

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
// card goes with it (plan 021 §4's second recorded behaviour change). The
// ending is in the record before the turn's own EventDone, because the session
// waits for the outbox between the two.
func TestAStubTurnEndingEndsItsOpenCard(t *testing.T) {
	m, stub := sizedCards(t)
	stub.HangNext()
	m.input.SetValue("go")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if !m.cardOpen() {
		t.Fatal("fixture: no card")
	}
	// The hung turn ends when the session closes, which is also what answers a
	// card nobody got to. A cancel is the ordinary way; here the turn's own end
	// is the subject, so the turn is released by closing.
	if err := m.eng.Close(); err != nil {
		t.Fatal(err)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || !calls[0].Cancelled {
		t.Fatalf("the ask ends exactly once when its turn goes: %+v", calls)
	}
	if open := stub.Asks().Asks(); len(open) != 0 {
		t.Fatalf("something is still parked: %+v", open)
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
