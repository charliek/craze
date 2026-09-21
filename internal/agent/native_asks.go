package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native adapter's side of the asker seam (plan 023 §3.4, §3.5): the two
// harness tools that block on a person — ask_user_question and exit_plan_mode
// — reach craze's ask registry through here, and the harness's todo list
// becomes the snapshot's and one event.
//
// Nothing here emits an ask. The registry publishes an opening and an ending
// itself, under its own mutex and in that order (asks.go's Open and
// Automatic), so an adapter that also emitted would raise every card twice.
// What the adapter adds around the wait is the flush live adds (live.go's
// decideAsk): once when the opening has been enqueued and once before the
// decision goes back, so a question's line is in the record ahead of the text
// the model writes after being answered.
//
// Every string that leaves here for a person has been through two hands. The
// tools redacted what they show with the call's own redactor before the asker
// saw it (plan 023 X9) — the keys belong to the harness, and the asker's side
// of the seam has none — and this file sanitizes, because only the adapter
// knows the text is about to be drawn in a terminal. Both halves run over the
// whole body, so nothing reaches the registry, the snapshot or an event
// carrying either a provider key or an escape sequence.
//
// It all runs on one of Fantasy's tool goroutines, which the turn joins before
// Run returns (agent.go's toolExecutionWg): the token it reads is its own
// turn's, and no ask can outlive the turn that opened it.

// nativeAsker is tool.Asker over the session's registry. It is a type of its
// own rather than methods on nativeSession because it is a seam the harness
// holds for the session's whole life, and naming it says which two methods
// the harness may call back on.
type nativeAsker struct{ s *nativeSession }

var _ tool.Asker = nativeAsker{}

// nativePlanNameBytes caps the name taken from a plan's first heading. The
// plan file itself is bounded by the tool (256 KiB) and a heading is one line,
// but a file that is one long line beginning with "#" would otherwise put all
// of it in the snapshot and in the journal as a card's title.
const nativePlanNameBytes = 120

// AskQuestion opens a question ask and blocks on it. Under the first-option
// policy it opens nothing and answers from the policy instead, exactly as live
// does (live.go's onAskQuestion), so `craze prompt --json` prints the same Auto
// opening for native that it prints for cursor.
//
// The answer comes back as indices, not labels (tool.Answers): what the person
// was shown has been redacted and sanitized, so it is not always the text the
// tool was given, and the tool answers the model in the model's own words.
func (a nativeAsker) AskQuestion(ctx context.Context, qs []tool.Question) (tool.Answers, error) {
	s := a.s
	tok, red, ok := s.askTurn()
	if !ok {
		// No turn of craze's own: nothing opens an ask that no turn could end
		// (asks.go's EndTurn), and the tool answers with its unanswered text.
		return tool.Answers{}, nil
	}
	body := nativeQuestion(qs, red)
	req := AskRequest{Kind: AskQuestion, Body: AskBody{Question: body}}
	if EffectiveApproval(s.opts).Question == ApprovalFirstOption {
		rec := s.automaticAsk(tok, req, AskAnswer{Answers: firstOptionAnswers(body)})
		if rec.Outcome != AskAutomatic {
			return tool.Answers{}, nil
		}
		return tool.Answers{Answered: true, Picked: nativePicks(body, rec.Answer.Answers)}, nil
	}
	rec, opened := s.parkedAsk(ctx, tok, req)
	// A skip is an answer the registry records (the card's Esc sends one), and
	// it is the person declining the whole request rather than choosing
	// nothing in particular: the tool's unanswered text says exactly that, so
	// it is not mapped to a card of empty picks.
	if !opened || rec.Outcome != AskAnswered || rec.Answer.Skip {
		return tool.Answers{}, nil
	}
	return tool.Answers{Answered: true, Picked: nativePicks(body, rec.Answer.Answers)}, nil
}

// PresentPlan opens a plan ask and blocks on it. Accept is the only outcome
// that approves — the registry's plan answer is exactly one of accept and
// reject (asks.go's validateAnswer) — and everything else, a cancel, the
// turn's end and the close among them, is unanswered: the plan is not
// approved by the absence of a refusal.
func (a nativeAsker) PresentPlan(ctx context.Context, p tool.PlanOffer) (tool.PlanOutcome, error) {
	s := a.s
	tok, red, ok := s.askTurn()
	if !ok {
		return tool.PlanUnanswered, nil
	}
	// The name is taken from the sanitized text, not from the file: a heading
	// that carried an escape would otherwise reach the card as the card's own
	// title, which is the one string a transcript prints without a body.
	text := sanitizeText(red(p.Text))
	req := AskRequest{Kind: AskPlan, Body: AskBody{Plan: &PlanEvent{Name: nativePlanName(text), Plan: text}}}
	if EffectiveApproval(s.opts).Plan == ApprovalAccept {
		rec := s.automaticAsk(tok, req, AskAnswer{Accept: true})
		if rec.Outcome != AskAutomatic || !rec.Answer.Accept {
			return tool.PlanUnanswered, nil
		}
		return tool.PlanApproved, nil
	}
	rec, opened := s.parkedAsk(ctx, tok, req)
	if !opened || rec.Outcome != AskAnswered {
		return tool.PlanUnanswered, nil
	}
	switch {
	case rec.Answer.Accept:
		return tool.PlanApproved, nil
	case rec.Answer.Reject:
		return tool.PlanRejected, nil
	}
	return tool.PlanUnanswered, nil
}

// askTurn is the running turn's token and the session's redactor, read
// together under s.mu and with no registry call made under it (plan 023 §3.5).
//
// It answers false between turns. An ask opened against the no-turn token
// belongs to no turn, so no EndTurn could ever resolve it (asks.go's EndTurn)
// and it would sit until a cancel or the close; the tools' own answer to
// "nobody to ask" — the unanswered text — is the right one for a call that
// somehow reached here outside a turn.
func (s *nativeSession) askTurn() (TurnToken, func(string) string, bool) {
	s.mu.Lock()
	tok, hs := s.turnToken, s.hs
	s.mu.Unlock()
	if tok.NoTurn() || hs == nil {
		return TurnToken{}, nil, false
	}
	// The session's widest redactor, which covers keys learned after Open as
	// well as the ones the call's own redactor holds.
	return tok, hs.Redact, true
}

// parkedAsk is open, flush, wait, flush: live's decideAsk without ACP's reply
// (live.go). The ask is opened against the call's own context, so a cancel
// that ends the turn ends the wait too, and a lifecycle refusal — the turn
// cancelled or ended between the token being read and the insert, the session
// closed, the outbox full — comes back as an ask that is already resolved and
// is read through the same Wait as any other.
//
// It reports false only for ErrAskIDInUse, the one error Open has, which
// nothing here can provoke because nothing here adopts an id.
func (s *nativeSession) parkedAsk(ctx context.Context, tok TurnToken, req AskRequest) (AskRecord, bool) {
	parked, err := s.asks.Open(ctx, tok, req)
	if err != nil {
		return AskRecord{}, false
	}
	s.flushAsks()
	rec := parked.Wait()
	s.flushAsks()
	return rec, true
}

// automaticAsk records a resolution craze's own policy made and waits for the
// outbox, so the opening and its ending are both in the record before the text
// the model writes once it has been answered.
func (s *nativeSession) automaticAsk(tok TurnToken, req AskRequest, a AskAnswer) AskRecord {
	rec := s.asks.Automatic(tok, req, a)
	s.flushAsks()
	return rec
}

// flushAsks waits for everything the registry has enqueued so far, bounded by
// the session's own done (plan 021 X43). It runs on a tool goroutine the turn
// joins, and Close waits for that turn: a flush that could outlive the session
// would be a Close waiting for a goroutine waiting for the log that same Close
// has yet to free (X42's lesson).
func (s *nativeSession) flushAsks() { _ = s.log.Flush(context.Background(), s.done) }

// nativeQuestion is the registry's body for one ask_user_question call: the
// ids everything downstream answers by, and every string redacted and
// sanitized.
//
// The ids are craze's, not the model's: q1… for the questions and o1… for each
// question's own options, which is all the registry validates against (an
// option id is checked inside the question that offered it, asks.go's
// checkQuestionAnswers). The model never sees them — it is answered by index
// — so they exist only to name a choice between the card, the registry and
// this file.
func nativeQuestion(qs []tool.Question, red func(string) string) *QuestionEvent {
	line := func(text string) string { return sanitizeLine(red(text)) }
	out := &QuestionEvent{Questions: make([]Question, 0, len(qs))}
	for i, q := range qs {
		opts := make([]Option, 0, len(q.Options))
		for j, o := range q.Options {
			opts = append(opts, Option{
				ID:          "o" + strconv.Itoa(j+1),
				Label:       line(o.Label),
				Description: line(o.Description),
			})
		}
		out.Questions = append(out.Questions, Question{
			ID: "q" + strconv.Itoa(i+1),
			// The question is prose and keeps its shape; the card folds it
			// onto one line itself when it draws it.
			Prompt:        sanitizeText(red(q.Question)),
			Options:       opts,
			AllowMultiple: q.MultiSelect,
		})
	}
	// The card's heading: Claude Code's short header when the model gave one,
	// and the first question otherwise, which is what a card with no heading
	// would have said anyway.
	if len(qs) > 0 {
		out.Title = line(qs[0].Header)
		if out.Title == "" {
			out.Title = line(qs[0].Question)
		}
	}
	return out
}

// firstOptionAnswers is the headless policy's answer: each question's first
// option (live.go's onAskQuestion sends the same for cursor). It is built from
// the body the registry will validate against, so the answer and what was
// offered cannot disagree.
func firstOptionAnswers(body *QuestionEvent) map[string][]string {
	out := make(map[string][]string, len(body.Questions))
	for _, q := range body.Questions {
		if len(q.Options) == 0 {
			continue
		}
		out[q.ID] = []string{q.Options[0].ID}
	}
	return out
}

// nativePicks maps an answer back to the indices the tool answers the model
// with, per question and in the questions' own order. An option id a question
// did not offer cannot get this far — the registry validates an answer before
// it claims the ask — and is dropped rather than turned into an index.
func nativePicks(body *QuestionEvent, answers map[string][]string) [][]int {
	picked := make([][]int, len(body.Questions))
	for i, q := range body.Questions {
		for _, id := range answers[q.ID] {
			if n := optionIndex(q, id); n >= 0 {
				picked[i] = append(picked[i], n)
			}
		}
	}
	return picked
}

func optionIndex(q Question, id string) int {
	for i, o := range q.Options {
		if o.ID == id {
			return i
		}
	}
	return -1
}

// nativePlanName is the plan's first markdown heading, capped, or "Plan". The
// plan is the model's own document and nothing in it is a title, so the
// heading is the closest thing to one; a plan with none still needs a word on
// the card.
func nativePlanName(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		if name := strings.TrimSpace(strings.TrimLeft(t, "#")); name != "" {
			return truncateUTF8(sanitizeLine(name), nativePlanNameBytes)
		}
	}
	return "Plan"
}

// applyTodos projects the harness's list after a todo_write (plan 023 §3.4).
//
// The snapshot is written first and the event published second, and that order
// is load-bearing: the TUI reads the event's list and falls back to the
// snapshot when it is empty (app.go's todosOf), so a list cleared to nothing
// clears the panel only because the snapshot it falls back to was cleared
// first.
//
// The harness owns the list and sends the whole of it every time, under the
// lock that also makes the emit (todos.go), so two parallel todo_write calls
// reach this in the order their writes took effect and an adapter that only
// ever applies the latest one it received cannot drift from the store.
// It is two locked sections rather than one because the redaction between
// them is a call into the harness, and s.mu is not held across those.
func (s *nativeSession) applyTodos(e harness.Todos) {
	s.mu.Lock()
	hs := s.hs
	s.mu.Unlock()
	// A todo is the model's own words, so the harness's redaction of tool text
	// does not cover it (events.go: "text from the model is raw").
	red := func(text string) string { return text }
	if hs != nil {
		red = hs.Redact
	}
	todos := nativeTodos(e.Items, red)
	s.mu.Lock()
	s.snap.Todos = todos
	s.snap.TodosUpdatedAt = s.Now()
	s.mu.Unlock()
	// Its own copy, never the snapshot's array: a consumer encodes the event
	// on a goroutine of its own while Snapshot is handing the list out again.
	s.emit(Event{Type: EventTodos, Todos: append([]Todo(nil), todos...)})
}

// nativeTodos is one list as a consumer sees it: the four statuses pass
// through as they are (the harness validates them to the same four the TUI
// and `--json` know), the text is redacted and sanitized, and an empty list is
// nil — which is what a cleared list has to be for the snapshot to clear.
func nativeTodos(items []tool.Todo, red func(string) string) []Todo {
	if len(items) == 0 {
		return nil
	}
	out := make([]Todo, 0, len(items))
	for _, it := range items {
		out = append(out, Todo{
			ID:      sanitizeText(red(it.ID)),
			Content: sanitizeText(red(it.Content)),
			Status:  string(it.Status),
		})
	}
	return out
}
