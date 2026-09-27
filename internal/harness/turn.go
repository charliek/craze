package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/redact"
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
	// StopMaxTurnRequests is a turn the harness stopped while the model
	// still had tool results to read: at the step limit (maxSteps). `craze
	// prompt` exits 1 for it, as for any stop but end_turn.
	StopMaxTurnRequests = "max_turn_requests"
	// StopToolUse is the stop reason a step whose tool calls ran records in
	// the transcript: the model is not done, the next step reads the
	// results. A turn never ends with it; a turn stopped after such a step
	// ends cancelled or max_turn_requests.
	StopToolUse = "tool_use"
)

// maxRetries is Fantasy's retry budget for a step. Retrying has no surface
// in H1, so one retry bounds the worst silent stall to one backoff; and the
// wrapper only lets a step be retried before any of it reached the screen
// (plan 018 §3.5, §3.7).
const maxRetries = 1

// maxSteps bounds the model requests one turn makes: a model that keeps
// calling tools is stopped after this many steps, with max_turn_requests
// (plan 019 §3.5).
const maxSteps = 200

// Result is how a turn ended. StopReason and Usage — the turn's token counts,
// every step's summed, zero for a cancelled turn — are filled in for a turn
// that did not fail; Unanswered is filled in whatever the turn did.
type Result struct {
	StopReason string
	Usage      Usage
	// Unanswered is the text of every steer Steer accepted that no step
	// persisted, in the order it was accepted: one the turn ended before a
	// step could take it up, and one taken into a step whose append never
	// happened. It is returned alongside an error too — a turn that failed
	// still owes the user whatever they typed into it (plan 019 §3.10). A
	// steer a step did persist is not here: the model read it, and the
	// transcript holds it.
	Unanswered []string
}

// Run sends text as the next user turn and blocks until the turn ends. What
// the turn does streams to sink as it happens (see Event): the answer's text
// and thinking, the tool calls the model makes — which the session runs, a
// step at a time, until the model answers without one — and each step's
// end. A nil sink discards events.
//
// The turn's own events reach sink only while the turn holds its own lock, so
// two of them never arrive at once, though tool events come from Fantasy's
// tool goroutines; the lifecycle of a sub-agent — SubagentStarted and
// SubagentFinished — is the turn's own and arrives the same way. A
// sub-agent's own events do not (plan 026 §3.9): each child's turn hands them,
// wrapped in SubagentEvent, straight to this sink while it holds the child's
// lock and never this turn's, so that nothing on the parent's side ever waits
// on a child. While children run, sink is therefore entered concurrently — by
// this turn under its lock, and by each child under its own — and a consumer
// whose state both touch must guard it. The events of any one child still
// arrive one at a time and in its order. Nothing reaches sink after Run
// returns: a child is joined before its agent call returns, and the turn
// before its calls. It must not block for long, and it must never block on a
// ToolProgress, a child's included: progress is lossy by contract, and a
// snapshot the turn cannot hand over at once (its lock is busy) is dropped,
// never queued — a consumer that may block delivers ToolProgress without
// blocking, or drops it. A consumer that blocks on one child's event holds up
// that child, and neither the parent nor another child. sink may call the
// session's other methods, but not Close, which waits for this Run.
//
// It returns a Result when the model finished (end_turn, max_tokens,
// refusal), when the harness stopped it (max_turn_requests), or when the
// turn was cancelled — by ctx, or by Close — and an error, with a Result
// carrying nothing but Unanswered, when it failed (see errors.go). A second
// Run while one is live is ErrInTurn; a Run after Close is ErrClosed.
//
// Steer merges text into the turn while it runs; Result.Unanswered is
// whatever it accepted that no step wrote down (steer.go).
//
// The turn is persisted from Fantasy's callbacks, a step at a time (plan
// 019 §3.5, §3.6):
//
//   - The user entry is handed to the store at the start; the store holds it
//     and writes it together with the first step that has output, so a turn
//     that produced nothing leaves nothing in the file, and the next turn's
//     prompt replaces it.
//   - A finished step appends its assistant message (text, reasoning and
//     tool calls, as Fantasy recorded them) with its usage and stop reason,
//     and, when it called tools, the tool message holding their results, in
//     one append (store.AppendStepLed), led by what its request carried that
//     no append has written yet: the mode reminder, as its variant (plan 028
//     §3.15), the steers and background results. A step whose calls Fantasy
//     did not run — an abnormal finish — gets a not_executed result per call
//     first. When the step's messages lack text the user saw stream —
//     Fantasy commits a text block only on its end part, which a provider
//     can omit — the answer's text is the streamed deltas, with the step's
//     tool calls.
//   - A step that cannot be saved stops the turn before another request,
//     and Run returns the error. A step whose calls have an empty or repeated
//     provider id runs none of them, is not saved, and fails the turn with
//     ErrBadToolCalls.
//   - A cancel or failure mid-step appends what streamed, marked
//     interrupted and led by no mode reminder (reminders.go), once Fantasy
//     has returned; the append uses no context, so
//     the cancel that ended the turn cannot abort it. With no text streamed
//     (thinking alone counts as none) nothing is written. Tools honour the
//     cancel and report "Tool execution aborted"; their step then finishes
//     and is saved as usual, and the turn stops there, cancelled.
//   - A cancel that lands after the model's final step was persisted writes
//     nothing more, and the turn keeps that step's stop reason, with every
//     step's usage summed as for any completed turn; a cancel after a tool
//     step is a cancelled turn, whatever Fantasy reports.
//   - A request the provider refuses as longer than the window is not saved
//     at all, once per turn: what it streamed is dropped, its calls settled
//     as not run, the context compacted, and the turn goes on with a request
//     that replaces it (overflow recovery, plan 028 §3.12). A second one, or
//     one the compaction cannot help, fails the turn with ErrContextTooLarge.
//
// A model or effort switch made since the last turn (SetModel, SetEffort) is
// handed to the store first, so it is written just ahead of this turn's
// prompt.
//
// In a session with background sub-agents (Options.Background, plan 026
// §3.11), every result waiting to be delivered — and every one a failed wake
// set aside — is taken up at the turn's first step, after the prompt, and one
// that becomes ready while the turn runs at its next step boundary: as a user
// part of their own, written with the step as an entry of results, never a
// steer. A result a step took up and did not write is given back when the
// turn ends, before Run returns; Options.OnPending is then called if one went
// back to waiting.
func (s *Session) Run(ctx context.Context, text string, sink func(Event)) (Result, error) {
	if strings.TrimSpace(text) == "" {
		return Result{}, ErrEmptyPrompt
	}
	return s.run(ctx, text, false, sink)
}

// Wake runs a turn of the session's own (plan 026 §3.11): Run with no prompt
// of the caller's, whose user message is the background sub-agents' results
// waiting to be delivered as it begins — each in its wrapper, in the order
// they finished — so the model reacts to its children without the user
// typing. It takes every pending result (never a suspended one), persists
// them as the turn's user entry — marked as results and carrying their usage
// — and is otherwise Run: the same history, reminders, tools, steers and
// ending, and a result that becomes ready while it runs is taken up at its
// next step boundary. A result it takes and does not write is set aside
// (suspended), whatever ended it — an error, a cancel, or a clean stop that
// persisted nothing — so a wake that keeps failing cannot wake again: the next
// turn a person starts delivers it, or agent_output.
//
// It returns ErrNothingPending, having written and emitted nothing, when no
// result was waiting as it began; ErrInTurn while a turn runs, and ErrClosed
// after Close, as Run does. The caller owns when to call it: typically when
// Options.OnPending has said a result is waiting and no turn of its own is
// running or about to start.
func (s *Session) Wake(ctx context.Context, sink func(Event)) (Result, error) {
	return s.run(ctx, "", true, sink)
}

// run is Run and Wake, one implementation (see both). For a wake text is
// empty until the results it takes are known.
func (s *Session) run(ctx context.Context, text string, wake bool, sink func(Event)) (Result, error) {
	if sink == nil {
		sink = func(Event) {}
	}
	m, changes, turnCtx, number, err := s.begin(ctx, wake)
	if err != nil {
		return Result{}, err
	}
	// The turn's reservations of background results are settled once it has
	// ended, on every path — after the interrupted answer is saved (finish),
	// and on a return before the model was asked or a panic too — and before
	// the session is released; whoever is told a result is waiting again is
	// told once it has been, holding no lock.
	gave := false
	defer func() {
		if gave {
			s.subs.notifyPending(false)
		}
	}()
	defer s.end()
	defer func() { gave = s.subs.restoreTurn(number) }()

	// The entry the turn opens with records the turn's number (plan 028 §3.2,
	// P10): it is how a replay tells a prompt from a steer, and where a
	// resumed session's numbering continues from. A wake's is its results.
	user := store.MessageEntry{Model: m.id(), Effort: m.effort, Turn: number}
	var held []string
	if wake {
		// Taken under the runner's lock now the session is claimed, owned by
		// the wake's first step (P47) before anything is sent.
		b := s.subs.reserve(owner{turn: number, step: 1, wake: true}, false)
		s.mu.Lock()
		if b == nil {
			// Nothing was waiting: no turn was, its number is not spent, and a
			// Replay is still in time. The session is still claimed, so no
			// other turn has begun.
			s.turns--
		} else {
			s.begun = true // a turn from here, as a Run's is from begin
		}
		s.mu.Unlock()
		if b == nil {
			return Result{}, ErrNothingPending
		}
		text, held = b.text, b.ids
		user.SubagentUsage, user.SubagentResults = b.rows, true
	}

	if err := s.record(m, changes); err != nil {
		return Result{}, err
	}
	// The history the request replays, redacted: an entry written before the
	// session knew a key can hold one — a switch resolves keys mid-session,
	// and this turn may be the one that adopted them — and the replay would
	// send it to the model, the newly switched one included. The same places
	// are redacted as when a step is persisted (redactCalls, redactResults): a
	// tool call's arguments and a tool result's text, never the model's own
	// text or reasoning — and the text of an entry of background results,
	// which a child wrote (plan 026 §3.11), and of a compaction's summary
	// message, which the summarizer wrote from tool output among the rest
	// (plan 028 P30), never a person's prompt.
	//
	// The replacer is the session's live union, the keys Session.Redact
	// covers — the parent's, every registered child's, and every background
	// child's whose result is not yet delivered — not the turn's own (astra
	// r15, finding 1): a result committed before anyone knew a key, which a
	// background child learned since and the parent never did, would
	// otherwise go out whole while that child runs or its result waits. It is
	// a superset of the turn's redactor, and redacting more is never a leak.
	//
	// It leaves the transcript on disk as it was written; and with no key in
	// the history — every other turn of every other session — it changes
	// nothing at all, however many keys it covers, so the request's bytes,
	// and the provider's prefix cache, are what they would have been.
	msgs, results := s.store.ContextWithResults(m.id())
	history := redactHistory(s.redactor(), msgs, results)
	// A prompt still held is an earlier turn's that produced nothing. It is
	// not in history, so this turn's request never sent it; written ahead of
	// this turn's answer, it would put in the transcript what the model never
	// saw.
	s.store.DiscardHeldUsers()
	user.Message = fantasy.NewUserMessage(text) // byte for byte what Fantasy sends for Prompt
	if err := s.store.AppendUser(user); err != nil {
		return Result{}, fmt.Errorf("harness: %w", s.tools.redactErr(err))
	}

	t := &turn{
		ctx:     turnCtx,
		store:   s.store,
		model:   m,
		sink:    sink,
		number:  number,
		calls:   calls{tools: s.tools},
		steers:  &s.steers,
		modes:   s.modes,
		logMode: s.recordMode,
		subs:    s.subs,
		wake:    wake,
		held:    held,

		turnFirstRequest:    true,
		segmentFirstRequest: true,
	}
	t.resetCalls(true) // Fantasy opens every step with OnStepStart; this is a defence
	if s.autoCompacts(m) {
		t.compactCheck = func() bool { return s.midTurnDue(m) }
	}
	// Every finished step is followed by what the session has spent (plan 028
	// §3.14) — in a session that is not a sub-agent (P33).
	if !s.child {
		t.spent = func(d StepDone) Spent { return s.stepSpent(number, m, d) }
	}
	// The session's todo store reaches this turn's sink only through here
	// (todos.go, plan 023 §3.4): attach for the turn's whole life, detached
	// once Run returns, the same way modes and the steer box are handed a
	// fixed reference at the start rather than looked up each time. A
	// sub-agent has no todo list (plan 026 §3.2).
	if s.tools.todos != nil {
		defer s.tools.todos.attach(t.emitLocked)()
	}
	// And the session's asker tells this turn, and no other, of a plan the
	// person approved (asker.go).
	if s.tools.asker != nil {
		defer s.tools.asker.attach(t.planWasApproved)()
	}
	// And the session's sub-agent runner reports to this turn, and resolves a
	// child's model against this turn's, for as long as it runs (plan 026
	// §3.6, §3.9): a call's children report their lifecycle through the
	// turn's lock and their own events straight to its sink, and one made
	// after a SetModel mid-turn still starts from the model the turn — and
	// so the call — runs on, not the session's current one (panel CodeRabbit
	// 12). Detached once Run returns, by which time every call, and so every
	// child, has.
	if s.subs != nil {
		defer s.subs.attach(t)()
	}
	// From here Steer is accepted, and only from here: a prompt the store
	// refused above never became a turn, so there was nothing to steer into.
	t.steers.begin()
	// The pre-turn check (plan 028 §3.6) runs with the box open (P36): a
	// compaction can take a while, and what the person types meanwhile is
	// the turn's to take up — in its first request, once one ran, to a
	// summary or to a summarizer failure (compactedBeforeFirst, which
	// preTurnCompaction sets). The compaction is written ahead of the held
	// prompt, which goes out with the first step after it, and after a
	// summary the history is the store's again, from the summary on.
	compacted, err := s.preTurnCompaction(t, user.Message)
	if err != nil {
		return t.stopBeforeRequest(err)
	}
	if compacted {
		history = s.rebuildHistory(t)
	}
	agent := s.newAgent(m.lm, s.system, t.agentTools())

	// The segmented turn (plan 028 §3.11, owner decision 2): one turn is a
	// loop over segments, each one agent.Stream. A segment ends at a step
	// boundary when the step that just finished — persisted, and stopped
	// tool_use — left the context at the threshold (compactionDue, the
	// turn's third stop condition, beside the step limit and halted); the
	// loop, not finish, decides whether the turn goes on: it compacts, then
	// starts the next segment from the store, in the same turn, with an
	// empty prompt — the rebuilt history ends in the last step's tool
	// message, or in the summary message when there is no tail, both of
	// which Fantasy allows a prompt of "" after (agent.go:1247-1275).
	// Nothing is restarted after a segment that also ended by a save
	// failure, unusable call ids, the doom-loop guard, an approved plan, a
	// cancel or the step allowance (restartDue): those end the turn as they
	// always have.
	//
	// Overflow recovery (§3.12, D-08, P2) is decided here too, on the error a
	// segment returns, before finish ever sees it — finish would persist the
	// failed request's partial answer and write the held prompt with it. A
	// request the provider refused as too large for the window, once per
	// turn and never suppressed (overflowRecovers): the failed request is
	// settled and discarded (failedRequest, §3.11 item 5), the context is
	// compacted in the text form (reason overflow, §3.8 item 5, P17), and
	// the next segment starts from the summary, with the turn's prompt sent
	// again iff no append of the turn has written it yet — a first request
	// that overflowed; after a step the prompt is in the summary — and the
	// steers, reminder and results the failed request carried, uncommitted,
	// re-placed after it (§3.11 item 6). A second overflow, a compaction
	// that failed, or a context with nothing stored to compact — a new
	// session's first request, whose prompt alone is too large — fails the
	// turn with ErrContextTooLarge; a cancel during the compaction ends it
	// cancelled, and one that cannot be written stops it (P5), as between
	// any two segments.
	prompt := text
	recovered := false
	for {
		res, err := agent.Stream(turnCtx, t.call(prompt, history))
		switch {
		case t.restartDue(err):
			// Between requests: the completed step's call state is retired and
			// nothing is outstanding, so a cancel here — during the summarizer —
			// persists nothing more (P1), and a compaction that cannot be
			// written stops the turn as a failed step save does (P5).
			if err := s.midTurnCompaction(t); err != nil {
				return t.stopBeforeRequest(err)
			}
			prompt = ""
		case !recovered && s.overflowRecovers(t, err):
			recovered = true
			held, sent := t.failedRequest()
			if cerr := s.overflowCompaction(t, sent); cerr != nil {
				var saveErr *errCompactionSaveFailed
				if errors.As(cerr, &saveErr) || turnCtx.Err() != nil {
					return t.stopBeforeRequest(cerr)
				}
				// The summarizer failed (its failure entry is written): the
				// turn fails with the overflow, between requests — the failed
				// request is settled already, and nothing of it is
				// persisted.
				return t.finish(res, err)
			}
			prompt = ""
			if held {
				prompt = text
			}
		default:
			return t.finish(res, err)
		}
		history = s.rebuildHistory(t)
		t.newSegment()
	}
}

// overflowRecovers reports whether err, the error a segment's agent.Stream
// returned, is an overflow run recovers from by compacting (plan 028 §3.12):
// the provider refused the request as too large for the model's window
// (overflowed), and the context holds something stored to compact — a new
// session whose first request overflowed has nothing, and fails at once, as
// it always has (§3.8 item 5, R2-9). It is asked once per turn at most: run
// recovers once. It holds no lock of the turn's.
func (s *Session) overflowRecovers(t *turn, err error) bool {
	if !t.overflowed(err) {
		return false
	}
	msgs, _ := s.store.ContextWithResults(t.model.id())
	return len(msgs) > 0
}

// overflowCompaction is overflow recovery's compaction (plan 028 §3.12): reason
// overflow — so the text form (§3.8 item 5, P17), the aligned request being
// the one that just overflowed — recorded as the turn it is in, on the
// turn's own model, run with no lock of the turn's held, as the mid-turn
// one is (midTurnCompaction). An unknown window's text form is budgeted by
// sent, the estimate of the turn's request that overflowed (failedRequest,
// review r1-c12), not by the aligned summarizer request, which never went
// out and holds neither the prompt nor what the request carried. It is never
// suppressed, and a summary switches automatic compaction on or off by what
// it left, as any compaction's does (compacted). Once it has run — anything
// but nothing to compact — the turn's overflow error says it compacted
// (overflowCompacted). The error is compact's: a summarizer failure, whose
// failure entry is written, a cancel, or *errCompactionSaveFailed (P5); run
// triages it.
func (s *Session) overflowCompaction(t *turn, sent int64) error {
	res, err := s.compactOn(t.ctx, t.model, t.model.r, true, sent, t.number, store.CompactionOverflow, "", "", t.emitLocked)
	if !errors.Is(err, store.ErrNothingToCompact) {
		t.mu.Lock()
		t.overflowCompacted = true
		t.mu.Unlock()
	}
	if err == nil {
		s.compacted(t.model, res)
	}
	return err
}

// rebuildHistory is the history the next request replays, rebuilt from the
// store — after a compaction, from its summary on — redacted as run's first
// build is (see there). It also fixes, for the segment about to start, what
// the retained history says the model was last reminded of (retainedReminder,
// plan 028 §3.11 table): the variant of the last reminder entry among the
// context's steps, which a restart's first request decides its own reminder
// by. It holds no lock of the turn's; the turn is between requests.
func (s *Session) rebuildHistory(t *turn) []fantasy.Message {
	msgs, results := s.store.ContextWithResults(t.model.id())
	history := redactHistory(s.redactor(), msgs, results)
	retained := lastReminderVariant(s.store.Steps(t.model.id()))
	t.mu.Lock()
	t.retainedReminder = retained
	t.mu.Unlock()
	return history
}

// begin claims the session for one turn: it refuses when closed or busy,
// fixes the model the turn runs on — and the redactor, taking up one a
// switch resolved while the turn before it ran (toolset.adopt), so a turn
// redacts everything it reports and persists with the same one — works out
// which switches the transcript has not been told about, numbers the turn
// (its tool calls' ids start with it), and registers the turn's cancel func
// before the lock is released, so a Close from then on finds it (crush's
// order). A Run's turn has begun from here, so a Replay is too late; a
// wake's only once it has found results to take (run).
func (s *Session) begin(ctx context.Context, wake bool) (model, []func(*store.Store) error, context.Context, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return model{}, nil, nil, 0, ErrClosed
	case s.running:
		return model{}, nil, nil, 0, ErrInTurn
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

	// No turn is running here, so this is the one point at which changing the
	// session's redactor cannot cut across a step.
	s.tools.adopt()

	// A cancel cause, so Close can tell a tool the session is closing
	// (tool.ErrClosing) rather than that the user stopped the turn.
	turnCtx, cancel := context.WithCancelCause(ctx)
	s.turns++
	s.running, s.cancel, s.done = true, cancel, make(chan struct{})
	if !wake {
		s.begun = true
	}
	return m, changes, turnCtx, s.turns, nil
}

// record hands the turn's switches to the store and, only once all of them
// are there, marks m — the turn's model and effort, not whatever a SetModel
// since begin made current — as what the transcript was last told. A switch
// the store refused stays unlogged, so the next turn hands it over again.
func (s *Session) record(m model, changes []func(*store.Store) error) error {
	for _, c := range changes {
		if err := c(s.store); err != nil {
			return fmt.Errorf("harness: %w", s.tools.redactErr(err))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The mode is left alone: it is recorded at the step boundary that tells
	// the model about it, which is not this one (recordMode).
	s.logged.model, s.logged.effort = m.id(), m.effort
	return nil
}

// recordMode hands the store a mode_change when mode is not what the
// transcript was last told, and marks it held once the store has taken it —
// held or written, which is what logged means for the model and the effort
// too. The turn calls it as it appends the step whose request announced the
// mode, so the entry is held with that step's output and lands where the
// change became visible in the conversation (plan 023 §3.1). A store that
// refuses it stays untold, as a refused model change does.
func (s *Session) recordMode(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logged.mode == mode {
		return nil
	}
	if err := s.store.AppendModeChange(mode); err != nil {
		return fmt.Errorf("harness: %w", s.tools.redactErr(err))
	}
	s.logged.mode = mode
	return nil
}

// end releases the session after a turn and wakes a Close waiting on it. The
// steer box is shut here too: the turn shuts it as it finishes (settleSteers),
// and this catches a Run that returned before it ever opened one.
func (s *Session) end() {
	s.steers.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel(nil) // releases the turn context's resources
	close(s.done)
	s.running, s.cancel, s.done = false, nil, nil
}

// turn is one Run's state.
//
// Fantasy calls the stream callbacks, OnToolCall and OnStepFinish on the
// goroutine that called Stream, but it runs tools — and calls OnToolResult —
// on goroutines of its own, up to five at once, and a tool's progress
// arrives on the dispatcher's (plan 019 §2.4). So everything a callback
// touches is guarded by mu, and the sink is called only while mu is held:
// the sink keeps H1's single-caller contract. Steps do not overlap: Fantasy
// waits for every tool before it finishes a step.
type turn struct {
	ctx    context.Context // the turn's: its cancel is the turn's
	store  *store.Store
	model  model
	sink   func(Event)
	number int // the turn's number in the session, from 1

	mu sync.Mutex
	// ended is set as Run returns: nothing reaches the sink after it, not
	// even a progress snapshot a tool goroutine delivers late.
	ended bool

	// The deltas streamed since the step began: what the user has seen of an
	// answer that may never finish.
	text, reasoning strings.Builder

	step       int           // the step streaming or last finished, from 1
	stepStart  time.Time     // when its request went out (a retry's, after the backoff)
	firstToken time.Duration // from stepStart to its first text, thinking or tool call; 0 before
	retries    int           // the step's retries so far

	stepped bool   // a step finished
	stop    string // that step's stop reason
	usage   Usage  // that step's usage
	saveErr error  // the first failed append of a finished step
	badIDs  bool   // a step's tool calls had an unusable provider id (ErrBadToolCalls)

	calls // this step's tool calls (toolbridge.go)

	loop doomLoop // the doom-loop guard's count, across the turn's steps (doomloop.go)

	// planApproved is set when the person approved the plan exit_plan_mode
	// presented (planWasApproved): the turn ends with that step, as end_turn,
	// and every call the model placed after the asking one in the step is refused
	// (plan 023 §3.4, D-51).
	planApproved bool
	approvedAt   int // the asking call's place in its step (toolCall.order)

	// compactedBeforeFirst says a pre-turn compaction ran (plan 028 §3.6,
	// R2-2) — to a summary or to a summarizer failure, the turn going on
	// either way (preTurnCompaction): the steers accepted while it did are
	// taken up at the turn's first request, which otherwise takes none
	// (prepareStep).
	compactedBeforeFirst bool

	// The segmented turn's state (plan 028 §3.11; run's loop). Fantasy numbers
	// the steps of one Agent.Stream from 0, and a turn may run several
	// (segments), so what each consumer of that number uses is spelled out in
	// §3.11's table: step is global — stepBase + n + 1, stepBase being the
	// requests started in earlier segments, finished or not (R2-1), so no tool
	// id, StepDone or reservation owner repeats across a restart;
	// turnFirstRequest is true until the turn's first request goes out
	// (stepStarted) — a pre-turn compaction does not clear it — and
	// segmentFirstRequest at each segment's first; between says the turn is
	// between requests — the completed step's call state retired, nothing
	// outstanding — so an ending there persists nothing more (P1).
	//
	// compactCheck is the mid-turn check (midTurnDue), asked under mu at the
	// end of a saved tool_use step; nil when the session never compacts on
	// its own. compactDue is its answer for the step just finished, which the
	// stop condition compactionDue reports; stepStarted clears it. spent is
	// what a finished step reports after its StepDone (Session.stepSpent,
	// spend.go): the step noted when no append wrote it, and the Spent it
	// leaves; nil for a sub-agent, which reports none (plan 028 P33). total is
	// every StepDone's usage summed over every segment, what a completed or
	// limited turn returns (R3-3) — never one call's TotalUsage.
	// retainedReminder is the variant of the last reminder entry in the
	// history a restart replays (rebuildHistory), "" for none, which its
	// first request's reminder is decided by (reminders.go, restartReminder)
	// — apart from a reminder carried uncommitted, which decides differently
	// (carriedReminder, R4-1).
	//
	// userWritten says an append of the turn has written its own user entry,
	// which the store holds from run's AppendUser until the first append,
	// whichever it is (wrote): the overflow restart sends the prompt again
	// only while it has not (§3.12).
	//
	// request is the input of the request prepareStep last prepared, as it
	// went out, which an overflow's compaction is budgeted by (requestTokens,
	// review r1-c12); overflowCompacted says the turn compacted for an
	// overflow (§3.12) — the compaction ran, to a summary or to a failure —
	// so the ErrContextTooLarge it may still end with says "even after
	// compacting" (classify, C9c item 4).
	compactCheck        func() bool
	spent               func(StepDone) Spent
	compactDue          bool
	stepBase            int
	turnFirstRequest    bool
	segmentFirstRequest bool
	between             bool
	total               Usage
	retainedReminder    string
	userWritten         bool
	request             []fantasy.Message
	overflowCompacted   bool

	// Interject's steers, spliced into every step's messages from the one that
	// first saw them (steer.go, plan 019 §3.10). steers is the session's box,
	// which has its own lock; spliced and written are this turn's, under mu.
	steers  *steerbox
	spliced []splice // the steers taken up, in order, each at a fixed index
	written int      // how many of spliced an append has written

	// The background sub-agents' results this turn delivers (delivery.go,
	// plan 026 §3.11): subs is the session's runner, nil for a sub-agent's;
	// wake says the turn is a wake, whose prompt is results; internal are the
	// parts of results its steps took up, a collection of their own and never
	// the steer box's; held are the results a wake was started with, which
	// its user entry carries, and heldWritten says an append has written it.
	// Under mu, but for the two fixed ones.
	subs        *subagents
	wake        bool
	internal    []internalPart
	held        []string
	heldWritten bool

	// The mode's reminders (reminders.go, plan 023 §3.3): modes is the
	// session's box, which has its own lock; the rest is this turn's, under
	// mu. reminders is a collection of its own — never spliced, never
	// emitted — and pending is the one composed for the step about to go out.
	// Each is written, as its variant, with the first finished step's append
	// after it (plan 028 §3.15) — never with an interrupted save
	// (reminders.go); remWritten counts those an append has written.
	//
	// carried and sent are the two halves of announcing a mode. carried is the
	// mode this turn's reminders already speak for, from the moment one is
	// composed: every later request of the turn carries it, so the turn never
	// composes it twice. sent is that mode once a request carrying it has
	// really gone out, and it is what the step that persists output records —
	// in the transcript (modeChangeHeld) and as what the model has been told
	// (modeHeard).
	modes      *modes
	logMode    func(mode string) error
	reminders  []reminder
	remWritten int
	pending    pendingReminder
	hasPending bool
	carried    string
	sent       string
}

// redactor is the session's, as it is now — fixed for the whole turn, since
// only a turn's start adopts a new one (toolset.adopt): everything the turn
// reports and writes down is redacted with the same one. The history it
// replays is redacted with a superset of it, the session's live union (run).
func (t *turn) redactor() *redact.Replacer { return t.tools.redactor() }

// emit hands ev to the sink unless the turn has ended. mu is held.
func (t *turn) emit(ev Event) {
	if !t.ended {
		t.sink(ev)
	}
}

// emitLocked is emit for a caller outside toolbridge.go's callbacks that
// does not already hold mu: the session's todo store (todos.go), whose own
// lock wraps a call to this one so that a mutation and its event are one
// critical section from the store's side too (plan 023 §3.4). It takes and
// releases mu itself, so a caller must not already hold it.
func (t *turn) emitLocked(ev Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emit(ev)
}

// call is the turn's request. MaxOutputTokens, effort and retries are per
// call; the system prompt and the tools are the agent's.
func (t *turn) call(text string, history []fantasy.Message) fantasy.AgentStreamCall {
	c := fantasy.AgentStreamCall{
		Prompt:          text,
		Messages:        history,
		ProviderOptions: t.model.effortOpts,
		// The step allowance is the turn's, not the segment's (plan 028
		// §3.11 item 8): a segment may make what earlier ones left of it.
		StopWhen: []fantasy.StopCondition{fantasy.StepCountIs(maxSteps - t.stepBase), t.halted, t.compactionDue},

		PrepareStep: t.prepareStep,
		OnStepStart: t.stepStarted,
		OnTextDelta: func(_, delta string) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.firstOutput()
			t.text.WriteString(delta)
			t.emit(TextDelta{Text: delta})
			return nil
		},
		// A reasoning block's first part may already carry text.
		OnReasoningStart: func(_ string, r fantasy.ReasoningContent) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.firstOutput()
			if r.Text != "" {
				t.reasoning.WriteString(r.Text)
				t.emit(ThoughtDelta{Text: r.Text})
			}
			return nil
		},
		OnReasoningDelta: func(_, delta string) error {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.firstOutput()
			t.reasoning.WriteString(delta)
			t.emit(ThoughtDelta{Text: delta})
			return nil
		},
		// Every callback returns nil: an error from OnToolCall leaks
		// Fantasy's tool coordinator (agent.go:1744, 1758), and the others'
		// would fail the turn over what is only a report.
		OnToolInputStart: t.toolInputStart,
		OnToolCall:       t.toolCall,
		OnToolResult:     t.toolResult,
		OnRetry:          t.retry,
		OnStepFinish:     t.stepFinished,
	}
	if n := t.model.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		c.MaxOutputTokens = &ceiling
	}
	return c
}

// halted is the turn's own stop condition, checked after every step
// alongside the step limit: a save failed (Fantasy ignores OnStepFinish's
// error, so without it the turn would go on paying for steps, and a tool
// changing files, that the transcript cannot hold), a step's call ids were
// unusable, the doom-loop guard refused a fifth identical call (plan 019
// §3.7), or the turn was cancelled — a cancel during tools lets their step
// finish, and no request may follow it.
//
// The guard's step ends with tool results the model never gets to read, so
// finish turns its tool_use into max_turn_requests, as it does for the step
// limit.
//
// An approved plan stops the turn here too (plan 023 §3.4): Fantasy joins
// every tool of the step, builds the step and runs OnStepFinish before it
// asks this, so by now the approval is recorded, the step — exit_plan_mode's
// call and its result — is persisted, and the only thing left to prevent is
// the next request. It is not done through a result's StopTurn, which the
// dispatcher clears on anything a tool returns and which would leave the
// step's tool_use to be read as a turn that ran out of requests.
func (t *turn) halted([]fantasy.StepResult) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saveErr != nil || t.badIDs || t.loop.stopped || t.planApproved || t.ctx.Err() != nil
}

// compactionDue is the segmented turn's stop condition (plan 028 §3.11 item
// 1), checked after every step beside halted: the step just finished was
// persisted, stopped tool_use, and left the context at the threshold
// (stepFinished, midTurnDue). It ends the segment, not the turn: run's loop
// compacts and starts the next one (restartDue).
func (t *turn) compactionDue([]fantasy.StepResult) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.compactDue
}

// restartDue decides, once a segment's agent.Stream has returned err,
// whether run starts another segment (plan 028 §3.11 items 2, 3): only when
// the segment ended by compactionDue and by nothing else — not a failure of
// the stream, a save failure, unusable call ids, the doom-loop guard, an
// approved plan, a cancel, or the step allowance, each of which ends the turn
// as it always has (finish). When it does, it retires the completed step's
// call state — the step is persisted, and every call of it settled
// (stepResults) — marks the turn between requests, and counts the segment's
// requests into stepBase, so the next segment's numbering carries on from
// the last request started (the consumer table's global step; R2-1 counts a
// request that started and never finished the same way, failedRequest).
func (t *turn) restartDue(err error) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil || !t.compactDue {
		return false
	}
	if t.saveErr != nil || t.badIDs || t.loop.stopped || t.planApproved || t.ctx.Err() != nil || t.step >= maxSteps {
		return false
	}
	t.resetCalls(true)
	t.resetDeltas()
	t.between = true
	t.stepBase = t.step
	return true
}

// overflowed reports whether err, the error a segment's agent.Stream
// returned, is the provider refusing the request as longer than the model's
// context window — classified as finish classifies it (classify), so a
// retried step's last error, a *fantasy.ProviderError and the wrapper's
// *llm.MidStreamError, an overflow after output began, are judged alike
// (IsContextTooLarge) — and nothing else ended the turn: not a cancel, which
// is a cancel whatever error it surfaced as, and none of the conditions that
// forbid a restart (restartDue; they end a segment before a request fails,
// so this is a defence), the step allowance included — the request that
// failed was a request, and one past the allowance is never sent.
func (t *turn) overflowed(err error) bool {
	if err == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx.Err() != nil || t.saveErr != nil || t.badIDs || t.loop.stopped || t.planApproved || t.step >= maxSteps {
		return false
	}
	return errors.Is(classify(err, t.model.id()), ErrContextTooLarge)
}

// classify is the turn's failure as the session returns it: classify on the
// turn's model, and an overflow marked Compacted when the turn compacted for
// one (overflowCompacted, §3.12) — then it failed even after compacting;
// otherwise nothing was compacted for it and the request alone is too large
// (C9c item 4). mu is held.
func (t *turn) classify(err error) error {
	cerr := classify(err, t.model.id())
	var pe *ProviderError
	if t.overflowCompacted && errors.As(cerr, &pe) && pe.kind == ErrContextTooLarge {
		pe.Compacted = true
	}
	return cerr
}

// failedRequest is the failed-request transition (plan 028 §3.11 item 5,
// R2-1, R3-1): what run does, before the overflow's compaction, with a
// request that ended without OnStepFinish. Its announced calls are settled
// as not run, so every ToolStarted has its ToolFinished and the dispatcher
// holds nothing of them; its partial text and reasoning are dropped from the
// deltas — the user saw them stream, and the compaction that follows closes
// that stream (a child's observer drops them there too, childObserver) — so a
// later interrupted save holds the replacement's own alone; it counts toward
// stepBase, as a request started, so no id or StepDone number of it is ever
// used again; and nothing of it is persisted. The turn is between requests
// from here, as at a mid-turn boundary: a cancel during the compaction
// persists nothing, and the steers it drained, the reminder it carried and
// the background results it reserved — none written by an append — are
// carried into the next segment by newSegment, their reservations kept.
//
// held reports whether the turn's own user entry is still held by the store:
// no append of the turn has written it (wrote), so the failed request was
// the turn's first to carry it, and the next segment sends the prompt again.
// sent is the failed request's own estimate (requestTokens): what the
// provider refused, held prompt and carried splices included, which the
// compaction's text form is budgeted by when the window is unknown (§3.8
// item 5, review r1-c12).
func (t *turn) failedRequest() (held bool, sent int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sent = t.requestTokens()
	t.settleCalls(incomplete)
	t.resetCalls(true)
	t.resetDeltas()
	t.between = true
	t.stepBase = t.step
	return !t.userWritten, sent
}

// newSegment opens the next segment, once the compaction — or its failure —
// is recorded and the history rebuilt (plan 028 §3.11 items 4, 6): the
// turn is no longer between requests, the segment's first request is next,
// and the splices are what the rebuilt history does not hold. A steer, a
// reminder or a part of results is committed once a step's append wrote it,
// and a committed one is in the history now, so it is dropped from the
// lists; an uncommitted one — drained or reserved for a request that never
// completed, C12's overflow case — is kept, its reservation with it, to be
// re-placed at the new base's end by the segment's first prepareStep, and
// its Steered is never emitted again (it was, when it was taken up). The mode
// the segment's requests already announce is what the kept reminders say,
// or nothing: everything else is told for good, in the transcript (heard).
func (t *turn) newSegment() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.between = false
	t.segmentFirstRequest = true
	t.spliced, t.written = slices.Clone(t.spliced[t.written:]), 0
	t.reminders, t.remWritten = slices.Clone(t.reminders[t.remWritten:]), 0
	t.internal = slices.DeleteFunc(slices.Clone(t.internal), func(p internalPart) bool { return p.written })
	t.carried = ""
	for _, r := range t.reminders {
		t.carried = reminderVariants[r.variant].mode
	}
}

// replaceSplices puts every splice the segment carries over at the end of
// the segment's first base, at, keeping their order among themselves
// (spliceInto's, at a shared index). mu is held.
func (t *turn) replaceSplices(at int) {
	for i := range t.spliced {
		t.spliced[i].at = at
	}
	for i := range t.reminders {
		t.reminders[i].at = at
	}
	for i := range t.internal {
		t.internal[i].at = at
	}
}

// planWasApproved records that the person approved the plan. The session's
// asker calls it, on the tool goroutine that asked, before exit_plan_mode has
// its answer (asker.go).
//
// It also fixes which call asked, as a place in the step: the last of the
// calls running now. The asking call is one of them, and it is not Parallel,
// so Fantasy dispatches nothing after it until it returns — whatever else is
// running was placed before it. Every call placed after it is refused
// (runTool).
func (t *turn) planWasApproved() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.planApproved {
		return
	}
	t.planApproved = true
	for _, c := range t.list {
		if c.running && c.order > t.approvedAt {
			t.approvedAt = c.order
		}
	}
}

// stepStarted opens step n (from 0, the segment's own numbering): its
// number in the turn — global across segments (plan 028 §3.11 table) — its
// clock, and an empty set of tool calls. The step's request goes out next,
// so this is also where the reminder prepareStep composed for it becomes one
// the model will read (reminderSent) — for a turn whose context is still
// live, since Fantasy opens a step whether or not the request can be sent —
// and where the turn's, and the segment's, first request has gone out.
func (t *turn) stepStarted(n int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.step = t.stepBase + n + 1
	t.stepStart, t.firstToken, t.retries = time.Now(), 0, 0
	t.compactDue = false
	t.turnFirstRequest, t.segmentFirstRequest = false, false
	t.resetCalls(true)
	t.reminderSent()
	return nil
}

// firstOutput stamps the step's time to first token. mu is held.
func (t *turn) firstOutput() {
	if t.firstToken == 0 && !t.stepStart.IsZero() {
		t.firstToken = max(time.Since(t.stepStart), time.Nanosecond)
	}
}

// retry is OnRetry. A retry replays the step from the start and every
// callback fires again (plan 018 §2.4): what the failed attempt streamed is
// not part of the answer, and a call it began never arrives.
func (t *turn) retry(err *fantasy.ProviderError, delay time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetDeltas()
	t.settleCalls(incomplete)
	t.resetCalls(false)
	t.retries++
	t.stepStart, t.firstToken = time.Now().Add(delay), 0
	t.emit(Retrying{Delay: delay, Attempt: t.retries, Reason: t.redactor().String(retryReason(err))})
}

// retryReason is a retried failure on one line.
func retryReason(err *fantasy.ProviderError) string {
	if err == nil {
		return "the request failed before the provider answered"
	}
	msg := err.Message
	if msg == "" {
		msg = err.Title
	}
	msg = oneLine(msg, maxMessageBytes)
	switch {
	case err.StatusCode == 0:
		return msg
	case msg == "":
		return "HTTP " + strconv.Itoa(err.StatusCode)
	}
	return "HTTP " + strconv.Itoa(err.StatusCode) + ": " + msg
}

// stepFinished persists a finished step (the store writes the held user
// entries with the first) and forgets the deltas, which the step now holds;
// see Run. Fantasy ignores a callback's error here, so a failed append is
// kept for halted and Run.
func (t *turn) stepFinished(step fantasy.StepResult) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	text, reasoning := t.text.String(), t.reasoning.String()
	t.resetDeltas()
	t.stepped = true

	assistant, ok := assistantOf(step.Messages)
	if (!ok || !hasText(assistant)) && text != "" {
		assistant, ok = withStreamed(assistant, reasoning, text), true
	}
	open := openCalls(assistant)
	t.stop = stepStop(step.FinishReason, len(open))
	t.usage = *store.UsageOf(step.Usage)
	t.total = addUsage(t.total, t.usage)
	done := t.stepDone(step)
	// What the step's sub-agents spent, a row per model (plan 026 §3.7): on
	// the step's tool entry below, and on its StepDone whatever becomes of the
	// append — a step refused for its ids or whose save fails is persisted
	// nowhere, and the report is then the only record (panel P40). A
	// background result an agent_output call read is not among them: it is
	// given back unless this append writes the call's result, and would then
	// be counted again by whatever delivers it (§3.11).
	done.SubagentUsage = subagentUsage(t.list, nil)

	results, bad, answered := t.stepResults(step, open)
	if bad {
		t.badIDs = true
		t.emit(Diag{Kind: DiagBadToolCalls, Fields: map[string]string{
			"step": strconv.Itoa(t.step), "calls": strconv.Itoa(len(open)),
		}})
		t.stepReport(done)
		return nil
	}
	if ok {
		entry := store.MessageEntry{
			Message:    redactCalls(t.redactor(), assistant),
			Model:      t.model.id(),
			Effort:     t.model.effort,
			Usage:      store.UsageOf(step.Usage),
			StopReason: t.stop,
		}
		var toolEntry *store.MessageEntry
		var todos todoMark
		if results != nil {
			// The children's usage rides on the entry that holds their results,
			// each row priced by its own model; the entry itself is stamped with
			// the parent's model and carries no plain usage, which that model's
			// rate would price (plan 026 §3.7). A background result's rides on
			// it when an agent_output call's own result here delivers it
			// (§3.11).
			writes := func(c *toolCall) bool { return slices.Contains(answered, c) }
			toolEntry = &store.MessageEntry{Message: redactResults(t.redactor(), *results), Model: t.model.id(), Effort: t.model.effort,
				SubagentUsage: subagentUsage(t.list, writes)}
			todos = t.todosOn(toolEntry)
		}
		// The mode this step's request announced is handed over first, so the
		// store writes the mode_change ahead of this step's own entries
		// (reminders.go), and it is only ever handed over for a step there is
		// something to append with.
		t.modeChangeHeld()
		// The steers no step has written yet lead the append: this is the
		// first step that could write them, and the transcript then holds
		// them exactly where this step's request had them — and so do the
		// background results the step's requests carried (§3.11), and the
		// reminders, as their variants (plan 028 §3.15).
		leading, lead := t.leadingEntries()
		ids, err := t.store.AppendStepLed(leading, entry, toolEntry)
		switch {
		case err == nil:
			t.written, t.remWritten = len(t.spliced), len(t.reminders)
			t.wrote(ids, lead, toolEntry != nil, outputCalls(answered))
			t.todosWritten(todos)
			done.Saved, done.Entries = true, ids
			// The conversation now holds the step the model read the notice
			// in, so the mode is told for good (plan 023 §3.3).
			t.modeHeard()
			// The mid-turn check (plan 028 §3.6, §3.11 item 1), once the step
			// is persisted and only for one the model is not done with: a
			// final step never compacts mid-turn. Fantasy asks the stop
			// conditions right after this callback, and compactionDue answers
			// with this.
			if t.stop == StopToolUse && t.compactCheck != nil {
				t.compactDue = t.compactCheck()
			}
		case errors.Is(err, store.ErrNoOutput): // thinking alone: nothing to persist
		default:
			if t.saveErr == nil {
				t.saveErr = err
			}
			done.SaveError = t.redactor().String(err.Error())
			t.emit(Diag{Kind: DiagSaveFailed, Fields: map[string]string{"step": strconv.Itoa(t.step), "error": done.SaveError}})
		}
	}
	t.stepReport(done)
	return nil
}

// stepReport emits a finished step's StepDone and, in a session that is not
// a sub-agent, the Spent it leaves right after it (plan 028 §3.14): a step no
// append wrote — a failed save, ids refused, nothing to write — is noted as
// observed and unsaved first, so the session's spend still counts it. mu is
// held; spent takes the session's and the store's locks under it, as the
// mid-turn check does (midTurnDue).
func (t *turn) stepReport(done StepDone) {
	t.emit(done)
	if t.spent != nil {
		t.emit(t.spent(done))
	}
}

// stepDone is the StepDone for step, with its persistence outcome still to
// fill in. mu is held.
func (t *turn) stepDone(step fantasy.StepResult) StepDone {
	id := t.model.id()
	raw, ok := llm.RawFinish(step.ProviderMetadata)
	if !ok {
		raw = step.FinishReason // nothing normalized it
	}
	return StepDone{
		Step:             t.step,
		Provider:         id.Provider,
		Model:            id.Alias,
		WireModel:        id.WireModel,
		Finish:           string(step.FinishReason),
		FinishRaw:        string(raw),
		StopReason:       t.stop,
		TimeToFirstToken: t.firstToken,
		Usage:            t.usage,
	}
}

// todosOn puts on e, a step's tool entry about to be appended, the session's
// todo list when it is not the list a written tool entry last carried — the
// list after the step: Fantasy has joined every call of the step, the
// parallel todo_write calls among them, before the step finishes (plan 028
// §3.2, P7). It is redacted with the turn's redactor, as the step's calls
// and results are (redactCalls, redactResults), so the transcript never
// holds a key the session knows in plaintext. It returns the mark
// todosWritten records once e is written: none when nothing was put on it,
// and for a sub-agent, which has no list. mu is held; the list's record has
// a leaf lock of its own (todos.go).
func (t *turn) todosOn(e *store.MessageEntry) todoMark {
	if t.tools.todos == nil {
		return todoMark{}
	}
	list, ok := t.tools.todos.unrecorded()
	if !ok {
		return todoMark{}
	}
	stored := storeTodos(redactTodos(t.redactor(), list))
	e.Todos = &stored
	return todoMark{list: list, set: true}
}

// todosWritten records that the tool entry todosOn put mark's list on has
// been written. mu is held.
func (t *turn) todosWritten(mark todoMark) {
	if mark.set {
		t.tools.todos.recordedAt(mark.list)
	}
}

// finish is how Run ends once Fantasy has returned: the turn's steers are
// settled, the step a cancel or a failure cut short is persisted as far as it
// streamed, every tool call still open is settled, and the result is worked
// out (see Run).
func (t *turn) finish(res *fantasy.AgentResult, err error) (out Result, ferr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer func() { t.ended = true }()
	// The steers are settled before anything else, and their text is put on
	// the Result by a defer rather than by each return below: a path that
	// forgot it would drop text the user typed. settleSteers shuts the box, so
	// from here no accept can land in a turn that has already reported.
	unanswered := t.settleSteers()
	defer func() { out.Unanswered = unanswered }()
	cancelled := t.ctx.Err() != nil

	if err == nil {
		// Fantasy finishes every step it starts, so there is nothing to
		// settle; this is a defence, not a path.
		t.settleCalls(incomplete)
		switch {
		case t.saveErr != nil:
			return Result{}, fmt.Errorf("harness: saving the answer: %w", t.tools.redactErr(t.saveErr))
		case t.badIDs:
			return Result{}, badToolCalls(t.model.id())
		}
		stop := StopEndTurn
		if t.stepped {
			stop = t.stop
		}
		if stop == StopToolUse || t.loop.stopped {
			// The model had results to read and the turn stopped anyway: the
			// user did, the step limit did, or the doom-loop guard did. The
			// guard is asked separately because a call it refused can arrive
			// under a finish that leaves its step's own stop reason end_turn
			// — Fantasy announces a call on any finish but the abnormal four,
			// and dispatches it only on "tool-calls" (plan 019 §3.7).
			if cancelled {
				return Result{StopReason: StopCancelled}, nil
			}
			stop = StopMaxTurnRequests
			// Unless what stopped it was the person approving the plan: that
			// turn is finished, not cut off, and `craze prompt` exits non-zero
			// on anything but end_turn. The transcript's step keeps its own
			// tool_use. A cancel (above), a save failure and unusable ids
			// (earlier) and the doom-loop guard all outrank it (plan 023 §3.4).
			if t.planApproved && !t.loop.stopped {
				stop = StopEndTurn
			}
		}
		// The turn's own sum over every segment's steps (plan 028 §3.11 item
		// 7): res.TotalUsage covers the last segment's call alone, and a
		// compaction's usage is on its entry, never here.
		return Result{StopReason: stop, Usage: t.total}, nil
	}

	// Fantasy discards everything on a cancel or a failure (plan 018 §2.4);
	// only what this runner kept says what the user saw. A context
	// cancelled for any reason is a cancel, whatever error it surfaced as.
	// Between requests (plan 028 §3.11 item 3) nothing is outstanding — the
	// completed step is persisted and its call state retired, or the request
	// that failed is settled and discarded (failedRequest) — so nothing is
	// saved: an ending there is cancelled, or the error, with the steers
	// alone. run ends a turn here between requests only for an overflow whose
	// compaction produced no summary (§3.12): the turn fails with the
	// overflow, and the steers the failed request drained come back
	// unanswered.
	partial := !t.between && (t.text.Len() > 0 || t.reasoning.Len() > 0 || t.announced() > 0)
	var saveErr error
	if !t.between {
		saveErr = t.saveInterrupted(cancelled)
	}
	why := incomplete
	if cancelled {
		why = aborted
	}
	t.settleCalls(why)
	switch {
	case cancelled && saveErr != nil:
		return Result{}, fmt.Errorf("harness: saving the interrupted answer: %w", t.tools.redactErr(saveErr))
	case cancelled && t.stepped && !partial && t.stop != StopToolUse:
		// The model's last step was final and is persisted: the cancel came
		// too late to stop anything. The turn completed, so it carries what
		// every segment's steps spent (plan 028 §3.11 item 7, R3-3) — the
		// last step's alone would drop the steps before a compaction (astra
		// r1-c11, finding 1).
		return Result{StopReason: t.stop, Usage: t.total}, nil
	case cancelled:
		return Result{StopReason: StopCancelled}, nil
	case saveErr != nil:
		return Result{}, errors.Join(t.classify(err),
			fmt.Errorf("harness: saving the interrupted answer: %w", t.tools.redactErr(saveErr)))
	default:
		return Result{}, t.classify(err)
	}
}

// stopBeforeRequest is how run ends a turn that stopped before its next
// request: before its first (plan 028 §3.6), or between two segments' (§3.11
// item 3), when the compaction there was cancelled or could not be written
// (P5). Nothing is outstanding — no request streamed, or the last step is
// persisted and its calls settled — so there is nothing to persist or settle
// but the steers: the box is shut and every steer it accepted — while the
// compaction ran, say — comes back unanswered, as on any ending
// (settleSteers). Before the first request the prompt stays held, like the
// prompt of a turn that produced nothing, and the next turn's replaces it;
// between requests the steps so far are in the transcript and stay there,
// written once. A save failure fails the turn; a cancel is a cancelled turn,
// with no usage, as a cancel after a tool step has always been (R3-3).
func (t *turn) stopBeforeRequest(err error) (Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer func() { t.ended = true }()
	unanswered := t.settleSteers()
	if errors.As(err, new(*errCompactionSaveFailed)) {
		return Result{Unanswered: unanswered}, fmt.Errorf("harness: saving the compaction: %w", t.tools.redactErr(err))
	}
	return Result{StopReason: StopCancelled, Unanswered: unanswered}, nil
}

// saveInterrupted appends the step a cancel or a failure cut short, marked
// interrupted, with the stop reason "cancelled" for a cancel and none for a
// failure: the tool calls it announced with their results, when there were
// any (synthesizeStep), otherwise the answer the deltas hold. Thinking with
// no text and no call is not persisted (the store's ErrNoOutput), and
// neither is nothing. mu is held.
func (t *turn) saveInterrupted(cancelled bool) error {
	stop := ""
	if cancelled {
		stop = StopCancelled
	}
	if done, err := t.synthesizeStep(stop); done {
		return err
	}
	if t.text.Len() == 0 && t.reasoning.Len() == 0 {
		return nil
	}
	// The background results the cut step's request carried lead the answer,
	// as they would a finished step's, and commit when it is written (plan 026
	// §3.11); its steers do not, and go back to the user, and neither does its
	// reminder, which the model was not told for good (reminders.go).
	leading, lead := t.internalEntries()
	ids, err := t.store.AppendAnswerLed(leading, store.MessageEntry{
		Message:     streamed(t.reasoning.String(), t.text.String()),
		Model:       t.model.id(),
		Effort:      t.model.effort,
		StopReason:  stop,
		Interrupted: true,
	})
	if err == nil {
		t.wrote(ids, lead, false, nil)
	}
	if errors.Is(err, store.ErrNoOutput) {
		return nil
	}
	return err
}

func (t *turn) resetDeltas() {
	t.text.Reset()
	t.reasoning.Reset()
}

// stepStop is a finished step's stop reason: "length" is max_tokens and
// "content-filter" is refusal; a "tool-calls" finish with calls to answer is
// tool_use, the turn going on; every other finish, "error" and "unknown"
// included, is a turn the model ended.
func stepStop(r fantasy.FinishReason, calls int) string {
	switch {
	case r == fantasy.FinishReasonLength:
		return StopMaxTokens
	case r == fantasy.FinishReasonContentFilter:
		return StopRefusal
	case r == fantasy.FinishReasonToolCalls && calls > 0:
		return StopToolUse
	}
	return StopEndTurn
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

// withStreamed is m, the step's assistant message (or the zero Message), with
// its text and reasoning replaced by the streamed deltas' — text the user
// saw that Fantasy never committed — and its other parts, the tool calls
// among them, kept after them in order.
func withStreamed(m fantasy.Message, reasoning, text string) fantasy.Message {
	out := streamed(reasoning, text)
	for _, p := range m.Content {
		switch p.GetType() {
		case fantasy.ContentTypeText, fantasy.ContentTypeReasoning:
		default:
			out.Content = append(out.Content, p)
		}
	}
	return out
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

// assistantOf is the assistant message of a step's messages, whole: its
// text, reasoning and tool calls, as Fantasy recorded them.
func assistantOf(msgs []fantasy.Message) (fantasy.Message, bool) {
	for _, m := range msgs {
		if m.Role == fantasy.MessageRoleAssistant {
			return m, true
		}
	}
	return fantasy.Message{}, false
}

// toolMessageOf is the tool message of a step's messages, holding the
// results of the calls Fantasy ran, or nil.
func toolMessageOf(msgs []fantasy.Message) *fantasy.Message {
	for i := range msgs {
		if msgs[i].Role == fantasy.MessageRoleTool {
			return &msgs[i]
		}
	}
	return nil
}

// openCalls are the tool calls in m the next message must answer: all but
// the ones the provider executed itself, which carry their own results.
func openCalls(m fantasy.Message) []fantasy.ToolCallPart {
	var calls []fantasy.ToolCallPart
	for _, p := range m.Content {
		if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok && !c.ProviderExecuted {
			calls = append(calls, c)
		}
	}
	return calls
}

// redactCalls is m with every provider key redacted from its tool calls'
// arguments: the model's own output, but what the transcript keeps must not
// hold a key verbatim (plan 019 §3.8). A message holding none is returned as
// it is, so its bytes do not change.
//
// Only the stored copy is redacted. Fantasy keeps the step's own messages
// and sends them again at the turn's next step (agent.go:943-944,
// 1072-1074), so an argument the model wrote goes back to the model as the
// model wrote it, and a later turn, replaying the transcript, sends the
// marker in its place. That is deliberate: what returns within the turn is
// what the model itself sent, so no key can be learned from it, while
// nothing craze keeps holds one. (The canary test checks what the model is
// sent: every tool-role message of every request.)
func redactCalls(red *redact.Replacer, m fantasy.Message) fantasy.Message {
	return mapParts(m, func(p fantasy.MessagePart) (fantasy.MessagePart, bool) {
		if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok {
			if in := red.String(c.Input); in != c.Input {
				c.Input = in
				return c, true
			}
		}
		return p, false
	})
}

// redactResults is m with every provider key redacted from its results'
// text. The results a tool produced are redacted already; the ones Fantasy
// wrote itself — its refusal of an invalid call — are not. A message holding
// none is returned as it is.
func redactResults(red *redact.Replacer, m fantasy.Message) fantasy.Message {
	return mapParts(m, func(p fantasy.MessagePart) (fantasy.MessagePart, bool) {
		r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
		if !ok {
			return p, false
		}
		if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](r.Output); ok {
			if s := red.String(o.Text); s != o.Text {
				r.Output = fantasy.ToolResultOutputContentText{Text: s}
				return r, true
			}
		}
		if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](r.Output); ok && o.Error != nil {
			if s := red.String(o.Error.Error()); s != o.Error.Error() {
				r.Output = fantasy.ToolResultOutputContentError{Error: errors.New(s)}
				return r, true
			}
		}
		return p, false
	})
}

// redactHistory is msgs with every provider key gone from the tool calls'
// arguments and the tool results' text (see Run), and from the text of every
// message results marks: an entry of background sub-agents' results (plan
// 026 §3.11, astra r14), or a compaction's summary message (plan 028 P30). A
// child wrote the one and the summarizer the other, as a tool wrote a
// result, so a key the session learned after it was committed must not go
// out in a later request. results is msgs' marks, message for message
// (store.ContextWithResults). A message holding none is carried over as it
// is, parts and all, so the bytes a provider sees do not change — a person's
// prompt among them, which is never marked; msgs itself, which the store
// owns, is never written to.
func redactHistory(red *redact.Replacer, msgs []fantasy.Message, results []bool) []fantasy.Message {
	out := slices.Clone(msgs)
	for i, m := range out {
		m = redactResults(red, redactCalls(red, m))
		if i < len(results) && results[i] {
			m = redactText(red, m)
		}
		out[i] = m
	}
	return out
}

// redactText is m with every provider key redacted from its text parts; m
// itself when they hold none.
func redactText(red *redact.Replacer, m fantasy.Message) fantasy.Message {
	return mapParts(m, func(p fantasy.MessagePart) (fantasy.MessagePart, bool) {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			if s := red.String(tp.Text); s != tp.Text {
				tp.Text = s
				return tp, true
			}
		}
		return p, false
	})
}

// liveRedact is m with every provider key gone from its calls, its results
// and its own text, unconditionally — segment.go's own rule for a step past
// the frontier, which has no per-message "results" mark to consult the way
// redactHistory does: text-redacting every message is harmless where none is
// present (a no-op), and needed for the two kinds a step's stored form does
// not otherwise cover, a reminder and a compaction's own summary message
// (P30). compact.go's text form reuses it for the same reason (review r1-c9
// finding 2).
func liveRedact(red *redact.Replacer, m fantasy.Message) fantasy.Message {
	return redactText(red, redactResults(red, redactCalls(red, m)))
}

// mapParts is m with f applied to each part, f reporting whether it changed
// one; m itself, its parts untouched, when f changes none. (Parts are not
// compared: some hold maps, and an interface holding one panics on ==.)
func mapParts(m fantasy.Message, f func(fantasy.MessagePart) (fantasy.MessagePart, bool)) fantasy.Message {
	var out []fantasy.MessagePart
	for i, p := range m.Content {
		q, changed := f(p)
		if out == nil && !changed {
			continue
		}
		if out == nil {
			out = append(make([]fantasy.MessagePart, 0, len(m.Content)), m.Content[:i]...)
		}
		out = append(out, q)
	}
	if out == nil {
		return m
	}
	m.Content = out
	return m
}
