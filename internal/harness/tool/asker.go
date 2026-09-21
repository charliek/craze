package tool

import "context"

// The asker is how a tool reaches a person (plan 023 §3.4): ask_user_question
// and exit_plan_mode block on it, and whoever opened the session answers — the
// native adapter, over craze's ask registry. It is pure data on both sides, so
// the harness still imports nothing of craze's (D-02), and a session opened
// with none (Env.Asker is nil) has nobody to ask: both tools then answer at
// once with the unanswered text, which is grok-build's migration fallback.
//
// ctx is the call's own. A cancel, a close and the turn's end all reach a
// blocked tool through it or through the asker's own return, never through a
// timeout: an ask has none, by decision (D-52).

// Question is one multiple-choice question.
type Question struct {
	// Question is the full question text; Header is a short label for it, or
	// "" (Claude Code's field, which a card may show as its title).
	Question, Header string
	Options          []QuestionOption
	// MultiSelect lets the person pick more than one option.
	MultiSelect bool
}

// QuestionOption is one choice: a few words, and optionally what picking it
// means.
type QuestionOption struct {
	Label, Description string
}

// Answers is what came back from AskQuestion.
type Answers struct {
	// Answered is false when nobody answered: the ask was cancelled, its turn
	// ended, or the session is closing. Picked is then nil.
	Answered bool
	// Picked holds, per question and in the questions' order, the indices of
	// the options chosen. Indices rather than labels, because what the asker
	// shows a person may not be the text the tool was given — it is redacted
	// and made safe for a terminal on the way — and the tool answers the model
	// in the model's own words.
	Picked [][]int
}

// PlanOffer is a plan put to the person: the plan file and its text.
type PlanOffer struct {
	Path, Text string
}

// PlanOutcome is what the person did with a plan.
type PlanOutcome int

const (
	// PlanUnanswered: nobody decided — the ask was cancelled, its turn ended,
	// or the session is closing. It is the zero value, so an asker that
	// forgets to decide has not approved anything.
	PlanUnanswered PlanOutcome = iota
	// PlanApproved ends the turn: the harness watches for it (plan 023 §3.4).
	PlanApproved
	// PlanRejected keeps the turn going in plan mode. It carries no feedback:
	// the person's next message does.
	PlanRejected
)

// Asker puts a question or a plan to a person and blocks until it is settled.
// Both methods must return promptly once ctx is done. An error is the asker's
// own failure, not a refusal: the tool reports it as a tool error.
type Asker interface {
	AskQuestion(ctx context.Context, questions []Question) (Answers, error)
	PresentPlan(ctx context.Context, plan PlanOffer) (PlanOutcome, error)
}
