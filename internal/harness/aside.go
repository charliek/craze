package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
)

// Side questions (/btw, plan 040 §3.4): a question about the session's work,
// asked while it goes on or between turns, answered by one request built from
// the session's own context and never part of it. The request is a turn's as
// far as a provider's prefix cache can tell — the frozen system prompt, the
// same tool definitions (inert), the same effort options, headers and output
// ceiling, and the written history — with the side conversation's earlier
// pairs and the question after it, each question opened by craze's
// instruction in the reminder tags. A running turn's prompt that has had no
// step written yet is never sent as a user message of its own: the model
// would take it for the request to answer, and answer it. It is described
// inside the first side question's instruction instead, as the request the
// main task is working on. Nothing of it is written to the transcript,
// emitted as an Event or steered into a turn: the one trace it leaves is its
// usage, noted as no turn's (asideTurn), which the session's spend counts
// from then on.

// AsidePair is one earlier question of a side conversation and the answer it
// got, as the caller kept them.
type AsidePair struct{ Question, Answer string }

// AsideAnswer is a side question's answer: the text the model streamed, the
// display name of the model that answered (the table's Name), and what every
// request sent for it was billed, attempts summed.
type AsideAnswer struct {
	Text  string
	Model string
	Usage Usage
}

var (
	// ErrAsideEmpty is a side question the model answered with no text, or
	// with nothing but whitespace. The usage is noted all the same.
	ErrAsideEmpty = errors.New("harness: the model gave the side question no answer")
	// ErrAsideToolCall is ErrAsideEmpty when the model asked for a tool
	// instead: none runs for a side question, and there is no next step to
	// read a result in.
	ErrAsideToolCall = fmt.Errorf("%w: the model tried to use a tool, which a side question cannot do", ErrAsideEmpty)
	// errAsideChild refuses a side question to a sub-agent's session. Only
	// top-level sessions are asked; it is a guard, never an answer.
	errAsideChild = errors.New("harness: a sub-agent's session takes no side question")
)

// asideTurn is the turn a side question's usage is noted under: a number no
// turn has, so the session's spend counts it and no turn's does — turn 0
// would count toward a session's spend before its first turn (spend.go).
const asideTurn = -1

// asideRetryWait is the wait before a side question's one retry (Aside).
const asideRetryWait = 500 * time.Millisecond

// asideNoAnswer is what an earlier pair with an empty answer is replayed as:
// an assistant message with no text is one some providers refuse.
const asideNoAnswer = "(no answer)"

// asideInstruction opens every side question, in the reminder tags (plan 040
// §3.4 step 5): craze's own words, which grok-build's text informed.
const asideInstruction = `The user is asking a side question while the main work goes on. Answer it in this one reply, from what this conversation already contains.

- This is an aside. It is not saved to the conversation, and the main task continues without it. Do not resume or continue the main task here.
- Do not call tools. None will run, and there is no next step in which to use a result.
- Do not offer to check, look up or try anything. If the conversation does not contain the answer, say so plainly.`

// asideTaskLead and asideResultsLead open the description of the running
// turn's held prompt (asideTask) inside the first side question's
// instruction: a person's request, or a wake's background results.
const (
	asideTaskLead    = "The main task is working on this request from the user right now:"
	asideResultsLead = "The main task is working right now on these background results:"
)

// asideImage and asideFile stand for an attachment of a held prompt
// described in the instruction (asideTask): the model is told one was there,
// and is sent none of it.
const (
	asideImage = "[image]"
	asideFile  = "[file]"
)

// asideWrap is question as a side question sends it: the instruction in the
// reminder tags, escaped as every reminder is, a blank line, then the
// question as it was asked. task, when not "", is the running turn's held
// prompt as asideTask describes it, and goes inside the tags after the
// instruction, a blank line apart — so the held prompt is never a user
// message of the request, which the model would answer as if it were the
// question. It is a function of its arguments alone, so a follow-up's
// request repeats the earlier one's byte for byte.
func asideWrap(question, task string) string {
	instruction := asideInstruction
	if task != "" {
		instruction += "\n\n" + task
	}
	return reminderText(instruction) + "\n\n" + question
}

// asideTask is the description of a running turn's held prompt that the
// first side question's instruction carries (asideWrap): a lead — the
// person's request, or background results when every held entry is a wake's
// results (marks) — then each held message's text, its parts a line apart
// and the messages a blank line apart, an image as asideImage and any other
// file as asideFile. held is already redacted as a request's history is. It
// is "" when nothing is held, or nothing held has text or an attachment.
func asideTask(held []fantasy.Message, marks []bool) string {
	var msgs []string
	for _, m := range held {
		var parts []string
		for _, p := range m.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
				if strings.TrimSpace(tp.Text) != "" {
					parts = append(parts, tp.Text)
				}
				continue
			}
			if f, ok := fantasy.AsMessagePart[fantasy.FilePart](p); ok {
				if isImage(f.MediaType) {
					parts = append(parts, asideImage)
				} else {
					parts = append(parts, asideFile)
				}
			}
		}
		if len(parts) > 0 {
			msgs = append(msgs, strings.Join(parts, "\n"))
		}
	}
	if len(msgs) == 0 {
		return ""
	}
	lead := asideTaskLead
	if !slices.Contains(marks, false) {
		lead = asideResultsLead
	}
	return lead + "\n\n" + strings.Join(msgs, "\n\n")
}

// Aside answers one side question (plan 040 §3.4): question, after the side
// conversation's earlier pairs in prior, oldest first, from the session's
// context. onText, when not nil, is called with each piece of the answer's
// text as it streams, on the request's goroutine; it must not block. The
// model's reasoning is not passed on.
//
// The context is the running turn's when one is attached (its model, and the
// prompt it sent that has had no step written yet, described in the first
// question's instruction), the session's current model and stored history
// otherwise (asideContext). It is refused, having sent nothing, with
// ErrClosed after Close, and with ErrStoredKeyFrozen in the refusal state,
// whether the session is found in it before the request is built or as a
// request is sent (asideGate). A failure before any text arrived that a
// retry may cure — the compaction summarizer's rule, summarizerFailureKind —
// is retried once, after asideRetryWait; any other failure stands, through
// classify. An answer with no text is ErrAsideEmpty, or ErrAsideToolCall
// when the model asked for a tool. A context that ends returns its error; a
// Close cancels the request and returns ErrClosed, and waits for this to
// return before the transcript closes.
//
// It may run while a turn runs, and beside other side questions. Nothing is
// written to the transcript and no Event is emitted; whatever each request
// was billed is noted as no turn's (asideTurn), success or failure, for the
// session's spend — published by the caller (IdleSpent) when no turn runs,
// and by the next step's Spent otherwise, or, when the turn has no step left
// to report, by the caller once it has ended (OwedSpent).
func (s *Session) Aside(ctx context.Context, prior []AsidePair, question string, onText func(string)) (AsideAnswer, error) {
	if s.child {
		return AsideAnswer{}, errAsideChild
	}
	if onText == nil {
		onText = func(string) {}
	}
	// Fantasy sends a request whatever its context: one already ended sends
	// nothing here.
	if err := ctx.Err(); err != nil {
		return AsideAnswer{}, err
	}
	sideCtx, release, err := s.registerAside(ctx)
	if err != nil {
		return AsideAnswer{}, err
	}
	defer release()
	if s.tools.refusing.Load() {
		return AsideAnswer{}, ErrStoredKeyFrozen
	}

	m, c := s.asideContext()
	red := s.redactor()
	task := asideTask(redactHistory(red, c.held, c.heldMarks), c.heldMarks)
	messages, prompt := asideMessages(requestHistory(red, c.msgs, c.marks, m.r), task, prior, question)
	// The summarizer's agent: no retries of Fantasy's own, which would
	// replay a request whose text the caller already has. Its model is behind
	// the refusal gate (asideGate): the check above refuses before anything
	// is built, and the gate at the moment each request is sent.
	ag := s.newSummarizerAgent(asideGate{LanguageModel: m.lm, refusing: &s.tools.refusing}, s.system, s.inertTools())
	var used Usage
	for attempt := 1; ; attempt++ {
		text, u, delivered, toolCall, err := s.askAside(sideCtx, ag, m, messages, prompt, onText)
		s.noteUnsaved(asideTurn, m.id(), u, nil) // one request
		used = addUsage(used, u)
		switch {
		case err == nil && strings.TrimSpace(text) == "" && toolCall:
			return AsideAnswer{Usage: used}, ErrAsideToolCall
		case err == nil && strings.TrimSpace(text) == "":
			return AsideAnswer{Usage: used}, ErrAsideEmpty
		case err == nil:
			return AsideAnswer{Text: text, Model: m.r.Name, Usage: used}, nil
		case sideCtx.Err() != nil:
			return AsideAnswer{Usage: used}, asideEnded(ctx)
		case errors.Is(err, ErrStoredKeyFrozen):
			// The gate's refusal (asideGate): nothing of this request went
			// out, and classify would turn the sentinel into a provider's
			// failure.
			return AsideAnswer{Usage: used}, ErrStoredKeyFrozen
		}
		err = classify(err, m.r)
		if attempt > 1 || delivered || summarizerFailureKind(err) != "" {
			return AsideAnswer{Usage: used}, err
		}
		if !s.backoff(sideCtx, asideRetryWait) {
			return AsideAnswer{Usage: used}, asideEnded(ctx)
		}
		// A key learned while the first request was out is in the frozen
		// prompt the retry would send.
		if s.tools.refusing.Load() {
			return AsideAnswer{Usage: used}, ErrStoredKeyFrozen
		}
	}
}

// registerAside counts a side question in (§3.4 step 2): its context, which
// ends with ctx's or with a Close's cancel, and what counts it out again.
// Both are refused after Close: the check and the count are one section of
// mu, as begin's are, so Close's join never misses one.
func (s *Session) registerAside(ctx context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrClosed
	}
	sideCtx, cancel := context.WithCancelCause(ctx)
	id := s.sideNext
	s.sideNext++
	if s.sides == nil {
		s.sides = make(map[uint64]context.CancelCauseFunc)
	}
	s.sides[id] = cancel
	s.sideCalls.Add(1)
	return sideCtx, func() {
		s.mu.Lock()
		delete(s.sides, id)
		s.mu.Unlock()
		cancel(nil)
		s.sideCalls.Done()
	}, nil
}

// asideHistory is what a side question knows of the session (§3.4 step 4,
// store.ContextWithHeld): the written history with its marks, and apart from
// it the held prompt of the turn it was asked during, with its marks.
type asideHistory struct {
	msgs, held       []fantasy.Message
	marks, heldMarks []bool
}

// asideContext is the model a side question asks and what it knows of the
// session (§3.4 steps 3-4). The basis is the attached turn's, from the turn
// link — its model, which the history's reasoning and the cache prefix are
// the turn's for, and its number, whose held prompt the store read takes.
// With none — idle, or the instant between begin and attach — it is the idle
// basis: the session's current model and turn 0, which takes no held prompt.
// s.running is not read: begin sets it before a dead turn's held prompt is
// discarded and the new one appended.
//
// The link is read again after the store read. A turn that has ended
// meanwhile — detached, whether or not another has attached since — may have
// left its prompt held, cancelled before it wrote any output, until the next
// turn begins: the read would have taken a dead prompt. The history is then
// read again on the idle basis, which takes no held prompt and so needs no
// check of its own; the next turn's held prompt is never taken either, as the
// link read first never named that turn. A link still attached was attached
// throughout the read, so what was held for it was a live turn's.
func (s *Session) asideContext() (model, asideHistory) {
	var link *turnLink
	if s.subs != nil {
		link = s.subs.turn.Load()
	}
	m, turn := s.idleModel(), 0
	if link != nil {
		m, turn = link.model, link.number
	}
	if seam := s.asideRead; seam != nil {
		seam()
	}
	var c asideHistory
	c.msgs, c.marks, c.held, c.heldMarks = s.store.ContextWithHeld(m.id(), turn)
	if link == nil || s.subs.turn.Load() == link {
		return m, c
	}
	m = s.idleModel()
	c.msgs, c.marks, c.held, c.heldMarks = s.store.ContextWithHeld(m.id(), 0)
	return m, c
}

// idleModel is what the next turn runs on (s.cur), read under mu.
func (s *Session) idleModel() model {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// asideGate is a side question's model as its agent holds it: m.lm, whose
// Stream first asks the refusal state and, with the session refusing, sends
// nothing and fails with ErrStoredKeyFrozen — refusalGate's check (turn.go),
// for a request no turn owns. Aside asks the state before it builds anything;
// a key learned after that and before a request goes out — the first one's or
// the retry's — is inside the frozen system prompt or the tool definitions
// that request carries, so the gate asks again at the moment Fantasy sends.
// Every other method is m.lm's. A thin adapter over Fantasy's LanguageModel,
// as refusalGate is.
type asideGate struct {
	fantasy.LanguageModel
	refusing *atomic.Bool
}

func (g asideGate) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if g.refusing.Load() {
		return nil, ErrStoredKeyFrozen
	}
	return g.LanguageModel.Stream(ctx, call)
}

// asideEnded is the error a side question whose request context ended
// returns: ctx's own when the caller's ended, ErrClosed otherwise — the only
// other cancel is Close's.
func asideEnded(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrClosed
}

// asideMessages is a side question's request after the system prompt (§3.4
// step 5): history — the written one, which ends with an answer (a step's,
// or its tool results), or with a compaction's summary as the next turn's
// prompt would follow it, or is empty — then each earlier pair as the
// wrapped question and the answer as an assistant message, and the wrapped
// question itself as the prompt. task, a running turn's held prompt as
// asideTask describes it, goes inside the first wrapped question's
// instruction — the first pair's, else the question's — and no other: the
// held prompt is never a user message here, so the model never takes it for
// the request to answer; and past history no two user messages follow each
// other. history is not changed.
func asideMessages(history []fantasy.Message, task string, prior []AsidePair, question string) ([]fantasy.Message, string) {
	msgs := slices.Clone(history)
	for _, p := range prior {
		msgs = append(msgs, fantasy.NewUserMessage(asideWrap(p.Question, task)))
		task = ""
		answer := p.Answer
		if strings.TrimSpace(answer) == "" {
			answer = asideNoAnswer
		}
		msgs = append(msgs, streamed("", answer))
	}
	return msgs, asideWrap(question, task)
}

// askAside sends one request of a side question (§3.4 step 6) and returns the
// text it streamed, what it was billed — observed as the stream finished, so
// a request that then failed still reports it — whether any text was passed
// to onText, and whether the step ended asking for a tool. Reasoning is not
// passed on, and the answer is the streamed text, not the response's first
// text part.
func (s *Session) askAside(ctx context.Context, ag fantasy.Agent, m model, messages []fantasy.Message, prompt string, onText func(string)) (text string, used Usage, delivered, toolCall bool, err error) {
	var b strings.Builder
	call := fantasy.AgentStreamCall{
		Prompt:          prompt,
		Messages:        messages,
		ProviderOptions: m.effortOpts,
		Headers:         requestHeaders(m.r, s.store.ID()),
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
		OnStreamFinish:  observeUsage(&used),
		OnTextDelta: func(_, delta string) error {
			if delta == "" {
				return nil
			}
			b.WriteString(delta)
			delivered = true
			onText(delta)
			return nil
		},
	}
	if n := m.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		call.MaxOutputTokens = &ceiling
	}
	res, err := ag.Stream(ctx, call)
	if err != nil {
		return b.String(), used, delivered, false, err
	}
	return b.String(), *store.UsageOf(res.TotalUsage), delivered, len(res.Response.Content.ToolCalls()) > 0, nil
}
