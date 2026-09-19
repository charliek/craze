package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
)

// The stop reasons a turn ends with. They are ACP's words for the same
// outcomes, so the adapter passes them through.
const (
	// StopEndTurn is a turn the model finished.
	StopEndTurn = "end_turn"
	// StopCancelled is a turn cut short by Run's context or by Close.
	StopCancelled = "cancelled"
	// StopMaxTokens is an answer cut off by the output ceiling ("length").
	StopMaxTokens = "max_tokens"
	// StopRefusal is an answer the provider's content filter stopped
	// ("content-filter"). It is not end_turn, so `craze prompt`, which exits
	// 1 on any stop reason but end_turn, does not pass a filtered answer off
	// as a clean one.
	StopRefusal = "refusal"
)

// maxRetries is Fantasy's retry budget for a step. Retrying has no surface
// in H1, so one retry bounds the worst silent stall to one backoff; and the
// wrapper only lets a step be retried before any of it reached the screen
// (plan 018 §3.5, §3.7).
const maxRetries = 1

// Result is how a turn that did not fail ended. Usage is the turn's token
// counts; it is zero for a cancelled turn, whose provider never reported
// them.
type Result struct {
	StopReason string
	Usage      Usage
}

// Run sends text as the next user turn and blocks until the turn ends. The
// answer's text and thinking stream to sink as they arrive (see Event); sink
// runs synchronously on Fantasy's callbacks, so it must not block for long,
// and it must not call Close. A nil sink discards events.
//
// It returns a Result when the model finished (end_turn, max_tokens,
// refusal) or the turn was cancelled — by ctx, or by Close — and an error,
// with a zero Result, when it failed (see errors.go). A second Run while one
// is live is ErrInTurn; a Run after Close is ErrClosed.
//
// The turn is persisted from Fantasy's callbacks, the way crush does it
// (plan 018 §2.6, §3.7):
//
//   - The user entry is handed to the store at the start; the store holds it
//     and writes it together with the answer, so a turn that produced
//     nothing leaves nothing in the file, and the next turn's prompt
//     replaces it.
//   - A finished step appends the answer (OnStepFinish), with its usage and
//     stop reason, and resets the runner's copy of the streamed deltas. When
//     the step's messages lack text the user saw stream — Fantasy commits a
//     text block only on its end part, which a provider can omit — the
//     answer is built from those deltas instead.
//   - A cancel or failure with text streamed appends what streamed, marked
//     interrupted, once Fantasy has returned; the append uses no context, so
//     the cancel that ended the turn cannot abort it. With no text streamed
//     (thinking alone counts as none) nothing is written.
//   - A cancel that lands after the step was persisted writes nothing more,
//     and the turn keeps the step's stop reason.
//
// A model or effort switch made since the last turn (SetModel, SetEffort) is
// handed to the store first, so it is written just ahead of this turn's
// prompt.
func (s *Session) Run(ctx context.Context, text string, sink func(Event)) (Result, error) {
	if strings.TrimSpace(text) == "" {
		return Result{}, ErrEmptyPrompt
	}
	if sink == nil {
		sink = func(Event) {}
	}
	m, changes, turnCtx, err := s.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer s.end()

	if err := s.record(m, changes); err != nil {
		return Result{}, err
	}
	history := s.store.Context(m.id())
	// A prompt still held is an earlier turn's that produced nothing. It is
	// not in history, so this turn's request never sent it; written ahead of
	// this turn's answer, it would put in the transcript what the model never
	// saw.
	s.store.DiscardHeldUsers()
	if err := s.store.AppendUser(store.MessageEntry{
		Message: fantasy.NewUserMessage(text), // byte for byte what Fantasy sends for Prompt
		Model:   m.id(),
		Effort:  m.effort,
	}); err != nil {
		return Result{}, fmt.Errorf("harness: %w", err)
	}

	t := &turn{store: s.store, model: m, sink: sink}
	agent := fantasy.NewAgent(m.lm, fantasy.WithSystemPrompt(s.system), fantasy.WithMaxRetries(maxRetries))
	res, err := agent.Stream(turnCtx, t.call(text, history))
	if err == nil {
		if t.saveErr != nil {
			return Result{}, fmt.Errorf("harness: saving the answer: %w", t.saveErr)
		}
		stop := StopEndTurn
		if n := len(res.Steps); n > 0 {
			stop = stopReason(res.Steps[n-1].FinishReason)
		}
		return Result{StopReason: stop, Usage: *store.UsageOf(res.TotalUsage)}, nil
	}

	// Fantasy discards everything on a cancel or a failure (plan 018 §2.4);
	// only the deltas this runner kept say what the user saw. A context
	// cancelled for any reason is a cancel, whatever error it surfaced as.
	cancelled := turnCtx.Err() != nil
	partial := t.text.Len() > 0 || t.reasoning.Len() > 0
	saveErr := t.saveInterrupted(cancelled)
	switch {
	case cancelled && saveErr != nil:
		return Result{}, fmt.Errorf("harness: saving the interrupted answer: %w", saveErr)
	case cancelled && t.stepped && !partial:
		return Result{StopReason: t.stop, Usage: t.usage}, nil
	case cancelled:
		return Result{StopReason: StopCancelled}, nil
	case saveErr != nil:
		return Result{}, errors.Join(classify(err, m.id()), fmt.Errorf("harness: saving the interrupted answer: %w", saveErr))
	default:
		return Result{}, classify(err, m.id())
	}
}

// begin claims the session for one turn: it refuses when closed or busy,
// fixes the model the turn runs on, works out which switches the transcript
// has not been told about, and registers the turn's cancel func before the
// lock is released, so a Close from then on finds it (crush's order).
func (s *Session) begin(ctx context.Context) (model, []func(*store.Store) error, context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return model{}, nil, nil, ErrClosed
	case s.running:
		return model{}, nil, nil, ErrInTurn
	}
	m := s.cur
	// A switch and a switch back before this turn leave nothing to record;
	// several switches record only the last. A change held for a turn that
	// then produced nothing stays held in the store, and a later change of
	// the same kind replaces it there.
	var changes []func(*store.Store) error
	if id := m.id(); id != s.logged.model {
		changes = append(changes, func(st *store.Store) error { return st.AppendModelChange(id) })
	}
	if effort := m.effort; effort != s.logged.effort {
		changes = append(changes, func(st *store.Store) error { return st.AppendEffortChange(effort) })
	}

	turnCtx, cancel := context.WithCancel(ctx)
	s.running, s.cancel, s.done = true, cancel, make(chan struct{})
	return m, changes, turnCtx, nil
}

// record hands the turn's switches to the store and, only once all of them
// are there, marks m — the turn's model and effort, not whatever a SetModel
// since begin made current — as what the transcript was last told. A switch
// the store refused stays unlogged, so the next turn hands it over again.
func (s *Session) record(m model, changes []func(*store.Store) error) error {
	for _, c := range changes {
		if err := c(s.store); err != nil {
			return fmt.Errorf("harness: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logged = logged{model: m.id(), effort: m.effort}
	return nil
}

// end releases the session after a turn and wakes a Close waiting on it.
func (s *Session) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel() // releases the turn context's resources
	close(s.done)
	s.running, s.cancel, s.done = false, nil, nil
}

// turn is one Run's state. Fantasy calls every callback on the goroutine
// that called Stream, so none of it needs a lock.
type turn struct {
	store *store.Store
	model model
	sink  func(Event)

	// The deltas streamed since the step began: what the user has seen of an
	// answer that may never finish.
	text, reasoning strings.Builder

	stepped bool   // a step finished (and was persisted, unless it had no text)
	stop    string // that step's stop reason
	usage   Usage  // that step's usage
	saveErr error  // the first failed append of a finished step
}

// call is the turn's request. MaxOutputTokens, effort and retries are per
// call; the system prompt is the agent's.
func (t *turn) call(text string, history []fantasy.Message) fantasy.AgentStreamCall {
	c := fantasy.AgentStreamCall{
		Prompt:          text,
		Messages:        history,
		ProviderOptions: t.model.effortOpts,
		// No tools are offered, so a turn is one step. If a model answers
		// with a tool call anyway, Fantasy records an error result for it;
		// this keeps it from sending that back for another, unasked-for,
		// paid step.
		StopWhen: []fantasy.StopCondition{fantasy.StepCountIs(1)},

		OnTextDelta: func(_, delta string) error {
			t.text.WriteString(delta)
			t.sink(TextDelta{Text: delta})
			return nil
		},
		// A reasoning block's first part may already carry text.
		OnReasoningStart: func(_ string, r fantasy.ReasoningContent) error {
			if r.Text != "" {
				t.reasoning.WriteString(r.Text)
				t.sink(ThoughtDelta{Text: r.Text})
			}
			return nil
		},
		OnReasoningDelta: func(_, delta string) error {
			t.reasoning.WriteString(delta)
			t.sink(ThoughtDelta{Text: delta})
			return nil
		},
		// A retry replays the step from the start and every callback fires
		// again (plan 018 §2.4); what the failed attempt streamed is not
		// part of the answer.
		OnRetry: func(_ *fantasy.ProviderError, delay time.Duration) {
			t.resetDeltas()
			t.sink(Retrying{Delay: delay})
		},
		OnStepFinish: t.stepFinished,
	}
	if n := t.model.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		c.MaxOutputTokens = &ceiling
	}
	return c
}

// stepFinished persists a finished step's answer (the store writes the held
// user entry with it) and forgets the deltas, which the answer now holds.
// A step with no text persists nothing (store.ErrNoOutput). Fantasy ignores
// a callback's error here, so a failed append is kept for Run to return.
//
// Fantasy puts a text or reasoning block in the step's messages only when
// the block's end part arrives (agent.go processStepStream). A provider that
// streams text and then finishes without ending the block leaves the step's
// messages with no text, though the user saw it all; the answer is then the
// streamed deltas, as saveInterrupted would build it, but complete.
func (t *turn) stepFinished(step fantasy.StepResult) error {
	text, reasoning := t.text.String(), t.reasoning.String()
	t.resetDeltas()
	t.stepped = true
	t.stop = stopReason(step.FinishReason)
	t.usage = *store.UsageOf(step.Usage)
	msg, ok := answer(step.Messages)
	if (!ok || !hasText(msg)) && text != "" {
		msg, ok = streamed(reasoning, text), true
	}
	if ok {
		err := t.store.AppendAssistant(store.MessageEntry{
			Message:    msg,
			Model:      t.model.id(),
			Effort:     t.model.effort,
			Usage:      store.UsageOf(step.Usage),
			StopReason: t.stop,
		})
		if err != nil && !errors.Is(err, store.ErrNoOutput) && t.saveErr == nil {
			t.saveErr = err
		}
	}
	t.sink(StepDone{Usage: t.usage})
	return nil
}

// saveInterrupted appends the answer the deltas hold, marked interrupted,
// with the stop reason "cancelled" for a cancel and none for a failure.
// Thinking with no text is not persisted (the store's ErrNoOutput), and
// neither is nothing.
func (t *turn) saveInterrupted(cancelled bool) error {
	if t.text.Len() == 0 && t.reasoning.Len() == 0 {
		return nil
	}
	stop := ""
	if cancelled {
		stop = StopCancelled
	}
	err := t.store.AppendAssistant(store.MessageEntry{
		Message:     streamed(t.reasoning.String(), t.text.String()),
		Model:       t.model.id(),
		Effort:      t.model.effort,
		StopReason:  stop,
		Interrupted: true,
	})
	if errors.Is(err, store.ErrNoOutput) {
		return nil
	}
	return err
}

func (t *turn) resetDeltas() {
	t.text.Reset()
	t.reasoning.Reset()
}

// streamed is an assistant message built from streamed deltas: the
// reasoning, when there was any, then the text.
func streamed(reasoning, text string) fantasy.Message {
	var parts []fantasy.MessagePart
	if reasoning != "" {
		parts = append(parts, fantasy.ReasoningPart{Text: reasoning})
	}
	parts = append(parts, fantasy.TextPart{Text: text})
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
}

// hasText reports whether m has a text part with something besides
// whitespace in it: the store's test for an answer worth persisting.
func hasText(m fantasy.Message) bool {
	for _, p := range m.Content {
		if t, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && strings.TrimSpace(t.Text) != "" {
			return true
		}
	}
	return false
}

// answer is the assistant message of a step's messages, as the transcript
// keeps it: its text and reasoning parts. With no tools offered, a tool call
// in an answer has nothing to pair with; replayed, it would be a call with
// no result, which strict providers refuse on every later request. H2
// persists tool calls with their results.
func answer(msgs []fantasy.Message) (fantasy.Message, bool) {
	for _, m := range msgs {
		if m.Role != fantasy.MessageRoleAssistant {
			continue
		}
		kept := make([]fantasy.MessagePart, 0, len(m.Content))
		for _, p := range m.Content {
			switch p.GetType() {
			case fantasy.ContentTypeText, fantasy.ContentTypeReasoning:
				kept = append(kept, p)
			}
		}
		m.Content = kept
		return m, true
	}
	return fantasy.Message{}, false
}

// stopReason maps a step's finish reason onto the turn's: "length" is
// max_tokens and "content-filter" is refusal; every other finish, "error"
// and "unknown" included, is a turn the model ended.
func stopReason(r fantasy.FinishReason) string {
	switch r {
	case fantasy.FinishReasonLength:
		return StopMaxTokens
	case fantasy.FinishReasonContentFilter:
		return StopRefusal
	}
	return StopEndTurn
}
