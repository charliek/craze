package harness

import (
	"context"
	"sync"

	"github.com/charliek/craze/internal/harness/tool"
)

// Asker is tool.Asker, named here so that Options can carry one: harness.go
// shapes Fantasy's types, and toolbridge.go is the one file that may import
// both Fantasy and the tool framework (Seam 1, plan 019 §3.1).
type Asker = tool.Asker

// watchedAsker is the caller's asker (Options.Asker) with the session
// watching one outcome over its shoulder: a plan the person approved, which
// ends the turn (plan 023 §3.4, D-51).
//
// The approval is read here, off the asker's typed answer, and not off
// anything the tool returns: a tool cannot end a turn — the dispatcher clears
// Result.StopTurn on whatever one hands back, since only the harness decides
// that — and the words of a result are for the model, not for control flow.
// So the tool's answer and the turn's ending are two readers of one fact, and
// neither can be talked into it by a tool that says "approved".
//
// A turn attaches for its whole life, as it does to the todo list (todos.go):
// the call that asked runs on one of Fantasy's tool goroutines, which the turn
// joins before Run returns, so an approval can only ever reach the turn whose
// call asked for it. Between turns nothing is attached and an approval marks
// nothing. mu guards the one field and is held across no call.
type watchedAsker struct {
	inner tool.Asker

	mu       sync.Mutex
	approved func() // the running turn's planWasApproved; nil between turns
}

// attach makes approved what an approved plan calls while a turn runs, and
// returns the func that detaches it as the turn ends.
func (w *watchedAsker) attach(approved func()) (release func()) {
	w.mu.Lock()
	w.approved = approved
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		w.approved = nil
		w.mu.Unlock()
	}
}

func (w *watchedAsker) AskQuestion(ctx context.Context, questions []tool.Question) (tool.Answers, error) {
	return w.inner.AskQuestion(ctx, questions)
}

// PresentPlan is the inner asker's, and marks the running turn before it
// returns an approval: by the time the tool has its answer the turn already
// knows, so every call the model placed after this one in the step is refused
// (runTool) and no step follows it (halted).
func (w *watchedAsker) PresentPlan(ctx context.Context, plan tool.PlanOffer) (tool.PlanOutcome, error) {
	out, err := w.inner.PresentPlan(ctx, plan)
	if err == nil && out == tool.PlanApproved {
		w.mu.Lock()
		approved := w.approved
		w.mu.Unlock()
		if approved != nil {
			approved()
		}
	}
	return out, err
}
