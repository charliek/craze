package tui

import (
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// planEnding is one plan ask's ending as the registry publishes it.
func planEnding(outcome agent.AskOutcome, accepted bool) agent.Event {
	return agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{
		ID: "plan-1", Kind: agent.AskPlan, Outcome: outcome, Accepted: accepted,
	}}
}

// TestPlanOfferArmsOnAnApprovedPlanWithNoText is plan 023 correction 2: the
// offer used to need assistant text, which every cursor plan turn has and a
// native one need not — a turn of write(plan) + exit_plan_mode says nothing in
// words at all, and the plan it left behind is a file. An accepted plan ask is
// the other evidence a turn can leave.
//
// Nothing here emits an EventText, at any point in the turn, which is the whole
// of what the case is for. TestFrameGoldenNativePlanOffer draws the same turn
// end to end on a real native session, including the route where this model
// answers the card itself and the ending comes back as its own echo.
func TestPlanOfferArmsOnAnApprovedPlanWithNoText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   agent.Event
		offer bool
	}{
		{name: "accepted", end: planEnding(agent.AskAnswered, true), offer: true},
		// Rejected leaves the model to revise: there is nothing agreed to
		// implement, and the turn said nothing either.
		{name: "rejected", end: planEnding(agent.AskAnswered, false)},
		{name: "cancelled", end: planEnding(agent.AskCancelled, false)},
		// craze's own headless policy accepted it, which never happens with a
		// frontend attached — and an Auto plan is not written to the transcript
		// (applyEvent's EventPlan arm), so there would be no plan above to
		// implement.
		{name: "automatic", end: planEnding(agent.AskAutomatic, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sess := scriptedModel(t)
			m = intoPlanMode(t, m)
			sc := scriptHeld()
			m = startScripted(t, m, sess, "plan it", sc)
			sess.Emit(tc.end)
			sc.Release()
			m = pumpUntil(t, m, isIdle)
			m = pumpSettled(t, m)

			if m.sawAssistantSeq == m.turnSeq {
				t.Fatal("setup: the turn said something in words")
			}
			if got := m.planArmed(); got != tc.offer {
				t.Fatalf("planArmed = %v, want %v:\n%s", got, tc.offer, plainView(m))
			}
			if got := strings.Contains(plainView(m), planOfferPlaceholder); got != tc.offer {
				t.Fatalf("the placeholder is drawn = %v, want %v:\n%s", got, tc.offer, plainView(m))
			}
		})
	}
}

// TestPlanApprovalBelongsToItsTurn: the record is against the turn, like every
// other piece of offer state, so a plan approved in one turn cannot arm the
// offer of the next one — which is what a turn ending with nothing to implement
// after a plan-mode turn would otherwise do.
func TestPlanApprovalBelongsToItsTurn(t *testing.T) {
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	sc := scriptHeld()
	m = startScripted(t, m, sess, "plan it", sc)
	sess.Emit(planEnding(agent.AskAnswered, true))
	sc.Release()
	m = pumpUntil(t, m, allOf(isIdle, viewHas(planOfferPlaceholder)))
	m = pumpSettled(t, m)
	if !m.planArmed() {
		t.Fatalf("setup: the approved plan did not arm the offer:\n%s", plainView(m))
	}

	next := scriptHeld()
	m = startScripted(t, m, sess, "and now", next)
	next.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if m.planArmed() {
		t.Fatalf("the next turn inherited the approval:\n%s", plainView(m))
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("placeholder drawn for a turn that left nothing:\n%s", plainView(m))
	}
}
