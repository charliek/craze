package harness

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

// Compaction (plan 028 §3.8, §3.9, §3.10): summarizing and cutting a
// session's context so it fits the model's window again. compact does the
// work; Session.Compact is /compact's turn of its own (§3.12's harness
// side — the wire and the adapter are C13's). Neither decides WHEN to
// compact: the pre-turn check, the threshold and suppression are
// autocompact.go's (§3.6, §3.7); the segmented turn that restarts after a
// mid-turn compaction is C11's; overflow recovery — the loop that calls
// compact with reason overflow after a request fails too large, and decides
// what happens next — is C12's. The settings are models.toml's [compaction]
// (modeltable.Compaction).

// summarizerAttempts bounds the summarizer's tries: the first and two more
// (plan 028 §3.8 item 5).
const summarizerAttempts = 3

// summarizerBackoff is the delay before the summarizer's 2nd and 3rd
// attempts, in order; compact's own retry, on top of Fantasy's (maxRetries)
// inside any one of them.
var summarizerBackoff = []time.Duration{2 * time.Second, 4 * time.Second}

// minSummaryLen is the shortest cleaned summary compact accepts without
// retrying (plan 028 §3.8 item 5): shorter than this, or empty, is
// degenerate.
const minSummaryLen = 500

// compactionPrompt is craze's own compaction prompt (plan 028 §3.8 item 2,
// golden testdata/compaction_prompt.golden): the seven headings, in order,
// the model writes its summary under, and the two instructions that follow
// them. It is one component of the request's one user message
// (compactionPromptText); the state section and the focus line are appended
// after it, never inside it, so this text alone is what the golden pins.
//
// Regenerate with:
//
//	go test ./internal/harness -run TestCompactionPromptGolden -update
const compactionPrompt = `Summarize this conversation so it can continue after everything above is removed from your context. Write one <summary> block with these seven headings, in this order, each on its own line with its content beneath it. Write "None." under a heading with nothing to report.

1. Request and intent
2. User messages (each, in order; verbatim when short)
3. Decisions and context
4. Files and code
5. Errors and fixes
6. Work state (Done / In progress / Blocked)
7. Next step (quoting the instruction it follows from)

Do not call tools. Do not continue the task.

If an earlier summary appears in the conversation, treat it as accurate and carry forward everything in it that still matters: whatever you leave out is lost. Where it conflicts with later messages, the later messages win. Preserve exact paths, identifiers, commands, error strings and URLs.`

// resultLineCap and resultLadderCap are the text form's per-result caps
// (plan 028 §3.8 item 5): the ordinary one, and the one a turn that still
// does not fit is cut to.
const (
	resultLineCap   = 2000
	resultLadderCap = 500
)

// CompactResult is what a successful compaction leaves (compact,
// Session.Compact): the compaction entry's id, and the context's estimated
// size before and after (plan 028 §3.7).
type CompactResult struct {
	EntryID                   string
	TokensBefore, TokensAfter int64
}

// errCompactionSaveFailed wraps a store error compact could not get past:
// writing the segment, or appending the compaction entry (a success's or a
// failure's). Unlike every other way compact ends without a summary — the
// summarizer never producing one that was any good, or the text form's
// budget refusing to fit anything — this one means nothing was recorded of
// what happened at all, so the transcript itself may now be missing what its
// last write cost (P5): the caller must stop, as it does when a step cannot
// be saved (turn.go's halted, saveErr), not merely note the failure and go
// on. The attempts' usage is unsaved and only ever reaches
// DiagCompactionUnsaved.
type errCompactionSaveFailed struct{ err error }

func (e *errCompactionSaveFailed) Error() string { return e.err.Error() }
func (e *errCompactionSaveFailed) Unwrap() error { return e.err }

// errDegenerateSummary is a cleaned summary compact would not accept: empty,
// or under minSummaryLen bytes (plan 028 §3.8 item 5). It is retried like a
// transient provider failure, and is the failure entry's Error when every
// attempt was one.
var errDegenerateSummary = errors.New("harness: the summarizer's reply was too short to be a real summary")

// errTextFormOverflow is the text form's own budget refusing to fit even the
// fixed part, or the newest turn alone (plan 028 §3.8 item 5, R2-5): the
// compaction fails.
var errTextFormOverflow = fmt.Errorf("harness: %w: the compaction's text form does not fit its budget", ErrContextTooLarge)

// compact summarizes the session's context and cuts it (plan 028 §3.8,
// §3.9, §3.10). m is the model the summarizer runs on: a turn's own for a
// pre-turn, mid-turn or overflow compaction (§3.6, §3.11, §3.12), the
// session's current one for a manual /compact (Session.Compact); the
// previous-model rule runs it on another (compactOn). turn is
// the turn number the compaction entry records (§3.2, P10): the turn it
// precedes for an automatic one, or the compaction's own for a manual one —
// the caller's, since only the caller (run's segment loop, or
// Session.Compact's own claim) knows which. reason is store.CompactionAuto,
// CompactionManual or CompactionOverflow; focus and command are set only for
// a manual one — focus is what the person asked the summary to keep,
// command the `/compact …` they typed, recorded on the entry and replayed
// as a Prompted (P10). emit is told Compacted{started} before the first
// summarizer attempt and Compacted{ended} once compact returns, whatever the
// outcome — on every return path (P34) — except when there was nothing to
// compact at all, which emits neither (below). The ended one carries every
// attempt's usage, recorded or not, so a child's observer counts what its
// compactions cost (§3.17, P19).
//
// (The signature adapts §3.8's `(ctx, m, reason, focus, emit)`: turn and
// command are added, since the entry needs both and compact is the one
// place that writes it — see the brief's report for why.)
//
// It returns CompactResult{}, store.ErrNothingToCompact when the context has
// no message at all — a brand new session's first turn, or a path every
// entry of which a previous incarnation already trimmed (§3.8 item 5): the
// caller changed nothing and asked nothing of the model.
//
// On success it returns the compaction entry's id and the estimated token
// counts, nil error. On a summarizer failure — every attempt exhausted, or
// the text form's budget refusing to fit anything — it appends a failure
// entry (no summary, the attempts' usage, the error) and returns
// CompactResult{}, the classified failure: the caller decides what "going
// on" means for its reason (auto: suppress and let the turn's own request
// proceed; overflow: the turn fails; manual: the turn ends with the error,
// Session.Compact's own rule). A store failure — the segment's write, or
// either entry's append — is returned wrapped in *errCompactionSaveFailed:
// every caller must stop, as for any save failure (P5); the attempts' usage
// is reported once, through DiagCompactionUnsaved, since no entry holds it.
//
// A cancelled ctx ends the attempts where they are: a failure entry is
// written only if some attempt was billed (its usage is not all zero), and
// compact returns CompactResult{}, ctx.Err() either way — Session.Compact
// turns that into a cancelled Result, as Run's finish does.
func (s *Session) compact(ctx context.Context, m model, turn int, reason, focus, command string, emit func(Event)) (CompactResult, error) {
	return s.compactOn(ctx, m, m.r, reason == store.CompactionOverflow, turn, reason, focus, command, emit)
}

// compactOn is compact with the two choices the previous-model rule makes
// otherwise (plan 028 §3.6, PD13; autocompact.go): fit is the model whose
// threshold the compacted context must get under — the turn's, when m is the
// previous model that holds the context — which bounds the tail (tailBudget,
// PD23); and textForm starts the summarizer in the text form rather than
// switching to it on an overflow, as every overflow compaction does (P17)
// and a pre-turn one whose previous model no longer resolves does on the
// turn's model, whose window the aligned request may not fit.
func (s *Session) compactOn(ctx context.Context, m model, fit modeltable.Resolved, textForm bool, turn int, reason, focus, command string, emit func(Event)) (CompactResult, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	if reason != store.CompactionManual {
		focus, command = "", ""
	}

	msgs, marks := s.store.ContextWithResults(m.id())
	if len(msgs) == 0 {
		return CompactResult{}, store.ErrNothingToCompact
	}
	red := s.redactor()
	history := redactHistory(red, msgs, marks)
	before := s.estimateContext(history)

	steps := s.store.Steps(m.id())
	cfg := s.compactionConfig()
	budget := tailBudget(int64(cfg.TailTokens()), compactionThreshold(fit, cfg.ThresholdPercent()))
	k := store.Cut(steps, budget, messageTokens)
	if k == 0 {
		// X32: a cut that would keep every step is no tail — summarize
		// everything instead, or a short /compact would only grow the
		// context by a summary of nothing.
		k = len(steps)
	}
	firstKeptID := ""
	if k < len(steps) {
		firstKeptID = steps[k].First
	}

	emit(Compacted{Phase: CompactionStarted, Reason: reason})
	var (
		reply   string
		usage   store.Usage
		lastErr error
	)
	priorSummary, hasPrior := priorSummaryText(msgs, marks)

attempts:
	for attempt := 1; attempt <= summarizerAttempts; attempt++ {
		var (
			out string
			u   store.Usage
			err error
		)
		if textForm {
			out, u, err = s.summarizeText(ctx, m, before, steps, priorSummaryIf(hasPrior, priorSummary), focus, red)
		} else {
			out, u, err = s.summarizeAligned(ctx, m, history, focus, red)
		}
		usage = addUsage(usage, u)
		if ctx.Err() != nil {
			return s.compactCancelled(ctx, m, turn, reason, command, usage, before, emit)
		}
		if err != nil {
			switch summarizerFailureKind(err) {
			case "overflow":
				if !textForm {
					// Switch to the text form and try again at once: this
					// still spends one of the summarizerAttempts, but no
					// backoff — the aligned form was never going to fit,
					// waiting would not change that (P17).
					textForm = true
					lastErr = err
					continue attempts
				}
				// The text form's own budget refused to fit anything:
				// retrying identically cannot help (errTextFormOverflow).
				lastErr = err
			case "fatal":
				lastErr = err
			default:
				lastErr = err
				if attempt < summarizerAttempts {
					s.sleep(summarizerBackoff[attempt-1])
				}
				continue attempts
			}
			break attempts
		}
		cleaned := cleanSummary(out, red)
		if len(cleaned) < minSummaryLen {
			lastErr = errDegenerateSummary
			if attempt < summarizerAttempts {
				s.sleep(summarizerBackoff[attempt-1])
			}
			continue attempts
		}
		reply, lastErr = cleaned, nil
		break attempts
	}

	if reply == "" {
		if lastErr == nil {
			lastErr = errDegenerateSummary
		}
		return s.compactFailed(m, turn, reason, command, usage, before, lastErr, emit)
	}
	return s.compactSucceeded(m, turn, reason, command, usage, before, firstKeptID, k, steps, reply, emit)
}

// compactCancelled is compact's return once ctx has ended (item 8): a
// failure entry only if some attempt was billed, and the caller's own
// cancellation either way.
func (s *Session) compactCancelled(ctx context.Context, m model, turn int, reason, command string, usage store.Usage, before int64, emit func(Event)) (CompactResult, error) {
	if usage != (store.Usage{}) {
		if _, err := s.appendCompactionFailure(m, turn, reason, command, usage, ctx.Err().Error()); err != nil {
			emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: s.redactor().String(err.Error()), Usage: usage})
			return CompactResult{}, &errCompactionSaveFailed{err}
		}
	}
	emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: "cancelled", Usage: usage})
	return CompactResult{}, ctx.Err()
}

// compactFailed is compact's return once every attempt (or the text form's
// own budget) failed to produce a usable summary: a failure entry (PD20),
// Compacted{ended, Err} by way of the caller (compact itself has no defer:
// every return path here emits it explicitly, since Go has no single exit
// point worth deferring around without hiding what each path carries).
func (s *Session) compactFailed(m model, turn int, reason, command string, usage store.Usage, before int64, cause error, emit func(Event)) (CompactResult, error) {
	msg := s.redactor().String(cleanErrorText(cause))
	if _, err := s.appendCompactionFailure(m, turn, reason, command, usage, msg); err != nil {
		emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: s.redactor().String(err.Error()), Usage: usage})
		return CompactResult{}, &errCompactionSaveFailed{err}
	}
	emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: msg, Usage: usage})
	return CompactResult{}, cause
}

// compactSucceeded writes the segment then the compaction entry (§3.10,
// §3.2) and emits Compacted{ended} with the estimated token counts.
func (s *Session) compactSucceeded(m model, turn int, reason, command string, usage store.Usage, before int64, firstKeptID string, k int, steps []store.Step, reply string, emit func(Event)) (CompactResult, error) {
	n, err := s.nextSegmentNumber()
	if err != nil {
		emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: s.redactor().String(err.Error()), Usage: usage})
		return CompactResult{}, &errCompactionSaveFailed{err}
	}
	segName := store.SegmentName(n)
	red := s.redactor()
	content := s.segmentContent(s.store.ID(), n, steps[:k], reply, red)
	if err := s.store.WriteSegment(segName, content); err != nil {
		// A segment write failure is a summarizer-class failure (§3.10): the
		// store is untouched, so a failure entry is still written, and the
		// caller goes on.
		return s.compactFailed(m, turn, reason, command, usage, before, fmt.Errorf("harness: writing the segment: %w", err), emit)
	}

	c := store.Compaction{
		Summary:     reply,
		FirstKeptID: firstKeptID,
		Reason:      reason,
		Command:     command,
		Segment:     segName,
	}
	c.TokensBefore = before
	tail := tailMessages(steps, k)
	summaryMsg := summaryMessage(store.SegmentDir(s.store.Path()), c)
	c.TokensAfter = s.estimateContext(append([]fantasy.Message{summaryMsg}, tail...))

	id, err := s.store.AppendCompaction(turn, m.id(), usage, c)
	if err != nil {
		s.diagUnsaved(emit, turn, reason, usage, err)
		emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: s.redactor().String(err.Error()), Usage: usage})
		return CompactResult{}, &errCompactionSaveFailed{err}
	}
	emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: c.TokensBefore, TokensAfter: c.TokensAfter, Usage: usage})
	return CompactResult{EntryID: id, TokensBefore: c.TokensBefore, TokensAfter: c.TokensAfter}, nil
}

// appendCompactionFailure writes a failure entry: no summary, no tail, no
// segment, the attempts' usage and msg as the error (PD20). A store failure
// here is reported through DiagCompactionUnsaved by the caller.
func (s *Session) appendCompactionFailure(m model, turn int, reason, command string, usage store.Usage, msg string) (string, error) {
	c := store.Compaction{Reason: reason, Command: command, Error: msg}
	id, err := s.store.AppendCompaction(turn, m.id(), usage, c)
	if err != nil {
		s.diagUnsaved(func(Event) {}, turn, reason, usage, err)
		return "", err
	}
	return id, nil
}

// diagUnsaved journals a compaction's usage that no entry will ever hold
// (§3.8 item 6, §3.14): an append that failed. It is not a Compacted event —
// callers emit that themselves — but Diag, the harness's plain journal
// channel (DiagSaveFailed's sibling).
func (s *Session) diagUnsaved(emit func(Event), turn int, reason string, usage store.Usage, cause error) {
	emit(Diag{Kind: DiagCompactionUnsaved, Fields: map[string]string{
		"turn":   strconv.Itoa(turn),
		"reason": reason,
		"error":  s.redactor().String(cause.Error()),
		"usage": fmt.Sprintf("input=%d output=%d reasoning=%d cache_read=%d cache_creation=%d",
			usage.Input, usage.Output, usage.Reasoning, usage.CacheRead, usage.CacheCreation),
	}})
}

// Compact runs /compact as a turn of its own (plan 028 §3.12's harness side;
// the wire and the prompt-path interception are C13's): it claims the
// session exactly as Run does (ErrInTurn while one is already running,
// ErrClosed after Close), on the session's current model, and no model turn
// follows it — nothing is sent to the model but the summarizer's own
// request, inside compact.
//
// focus is what the person asked the summary to keep (`/compact <focus>`,
// "" for a bare `/compact`); command is what they typed, recorded on the
// entry (Compaction.Command) so a replay shows the row (P10, A19). sink is
// told Compacted{started…ended} as compact reports it; nil discards.
//
// It returns Result{StopReason: StopEndTurn} on a successful compaction
// (whether or not the summarizer itself succeeded: a failed summarizer is
// recorded and the turn still ends cleanly — item 6's "manual: the turn
// ends with the error" is the *error* return below, not this one), and,
// with an error, whatever compact's own would mean for a plain Run:
// store.ErrNothingToCompact when the context has no message (P35's own
// wording, "nothing to compact yet", is the adapter's, C13's); a classified
// summarizer failure otherwise; or *errCompactionSaveFailed, a save failure
// like any turn's. A cancelled ctx ends the turn cancelled, as any turn's
// does, with a nil error.
func (s *Session) Compact(ctx context.Context, focus, command string, sink func(Event)) (Result, error) {
	if sink == nil {
		sink = func(Event) {}
	}
	m, changes, turnCtx, number, err := s.begin(ctx, false)
	if err != nil {
		return Result{}, err
	}
	defer s.end()
	if err := s.record(m, changes); err != nil {
		return Result{}, err
	}
	res, cerr := s.compact(turnCtx, m, number, store.CompactionManual, focus, command, sink)
	switch {
	case cerr == nil:
		// A /compact is never suppressed, and it is one of the ways back:
		// one that got the context under the threshold turns automatic
		// compaction on again, and one that did not turns it off, as any
		// compaction still over does (§3.6, PD23).
		s.compacted(m, res)
		return Result{StopReason: StopEndTurn}, nil
	case errors.Is(cerr, store.ErrNothingToCompact):
		// Nothing was recorded: the turn number this claim reserved is given
		// back, so a manual /compact that found nothing to do leaves no gap
		// either (P10, mirroring Wake's ErrNothingPending, turn.go's run).
		s.mu.Lock()
		s.turns--
		s.mu.Unlock()
		return Result{}, cerr
	case turnCtx.Err() != nil:
		return Result{StopReason: StopCancelled}, nil
	default:
		return Result{}, cerr
	}
}

// estimateContext is the bytes/4 estimate of a request that sends msgs with
// this session's fixed system prompt and tools (plan 028 §3.7's "no
// frontier" rule, tokens.go): the system prompt's text, the tools' JSON, and
// every message. It takes no model: the prompt and the tools are the
// session's, fixed at Open, whichever model runs a turn.
func (s *Session) estimateContext(msgs []fantasy.Message) int64 {
	n := textTokens(s.system) + tokensOf(len(s.tools.wire))
	for _, m := range msgs {
		n += messageTokens(m)
	}
	return n
}

// summarizerFailureKind classifies a failed summarizer attempt (plan 028
// §3.8 item 5): "overflow" for the provider refusing the request as too
// large (switches every later attempt to the text form); "fatal" for
// authentication, model-not-found, or any other client error the provider
// raised deliberately (quota included: a 4xx this craze has no more specific
// name for) — these end the summarizer at once; "" for anything else — a
// 5xx, a timeout, a stream error — which is retried.
func summarizerFailureKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrContextTooLarge):
		return "overflow"
	case errors.Is(err, ErrAuth), errors.Is(err, ErrModelNotFound):
		return "fatal"
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe.StatusCode >= 400 && pe.StatusCode < 500 {
		return "fatal"
	}
	return ""
}

// cleanErrorText is a failed summarizer's error, on one line, for the
// failure entry and the ended event: classify's own text for a provider
// failure, cause's own message otherwise (a degenerate summary, a text-form
// budget refusal).
func cleanErrorText(cause error) string {
	return oneLine(cause.Error(), maxMessageBytes)
}

// addUsage is a plus b, field by field: the summed usage of every
// summarizer attempt (plan 028 §3.8 item 6, PD19).
func addUsage(a, b store.Usage) store.Usage {
	return store.Usage{
		Input:         a.Input + b.Input,
		Output:        a.Output + b.Output,
		Reasoning:     a.Reasoning + b.Reasoning,
		CacheRead:     a.CacheRead + b.CacheRead,
		CacheCreation: a.CacheCreation + b.CacheCreation,
	}
}

// summarizeAligned sends the summarizer's aligned request (plan 028 §3.8
// item 1, P11, PD10): the session's own agent, with the session's tools
// offered inert (s.inertTools, same Info as a turn's) so Fantasy converts
// and normalizes them exactly as a turn's would, one step
// (fantasy.StepCountIs(1)), the same provider options and output ceiling as
// a turn. Messages is history, exactly the next request's; Prompt is the
// compaction prompt, the state section, and, for a focus, the "Focus this
// summary on" line (item 1, item 2, item 3).
func (s *Session) summarizeAligned(ctx context.Context, m model, history []fantasy.Message, focus string, red *redact.Replacer) (string, store.Usage, error) {
	agent := s.newAgent(m.lm, s.system, s.inertTools())
	call := fantasy.AgentStreamCall{
		Prompt:          s.compactionPromptText(focus, red),
		Messages:        history,
		ProviderOptions: m.effortOpts,
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
	}
	if n := m.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		call.MaxOutputTokens = &ceiling
	}
	res, err := agent.Stream(ctx, call)
	if err != nil {
		return "", store.Usage{}, classify(err, m.id())
	}
	return res.Response.Content.Text(), *store.UsageOf(res.TotalUsage), nil
}

// summarizeText sends the summarizer's text-form request (plan 028 §3.8
// item 5): the context serialized as opencode's lines, budgeted, with the
// same prompt and no tools at all (not even inert ones — there is nothing
// left to answer a call with room for).
func (s *Session) summarizeText(ctx context.Context, m model, before int64, steps []store.Step, priorSummary, focus string, red *redact.Replacer) (string, store.Usage, error) {
	prompt, err := s.textFormPrompt(m, before, steps, priorSummary, focus, red)
	if err != nil {
		return "", store.Usage{}, err
	}
	agent := s.newAgent(m.lm, s.system, nil)
	call := fantasy.AgentStreamCall{
		Prompt:          prompt,
		ProviderOptions: m.effortOpts,
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
	}
	if n := m.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		call.MaxOutputTokens = &ceiling
	}
	res, err := agent.Stream(ctx, call)
	if err != nil {
		return "", store.Usage{}, classify(err, m.id())
	}
	return res.Response.Content.Text(), *store.UsageOf(res.TotalUsage), nil
}

// compactionPromptText is the summarizer's request's one user message, aligned
// or text form alike (plan 028 §3.8 items 1-3): the compaction prompt, the
// state section, then, for a focus, the line that names it.
func (s *Session) compactionPromptText(focus string, red *redact.Replacer) string {
	text := compactionPrompt + "\n\n" + s.stateSection(red)
	if focus != "" {
		text += "\n\nFocus this summary on: " + focus
	}
	return text
}

// stateSection is the compaction request's state section (plan 028 §3.8
// item 3, PD24): the current todo list and every running background child,
// rendered deterministically, redacted, so the summary can carry them.
func (s *Session) stateSection(red *redact.Replacer) string {
	var b strings.Builder
	b.WriteString("Current state:\n\nTodos:")
	if todos := s.todoLines(red); len(todos) == 0 {
		b.WriteString(" None.\n")
	} else {
		b.WriteString("\n")
		for _, l := range todos {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nRunning background tasks:")
	if children := s.subs.running(); len(children) == 0 {
		b.WriteString(" None.")
	} else {
		b.WriteString("\n")
		for i, c := range children {
			fmt.Fprintf(&b, "- %s (%s): %s", c.id, c.typ, c.desc)
			if i < len(children)-1 {
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// textFormBudget is the text form's whole-request budget (plan 028 §3.8
// item 5, R2-5): 70% of the window less its output ceiling, or, with an
// unknown window, 60% of before — the estimate of the request that
// overflowed (the aligned request compact always builds first, whether or
// not it was sent).
func textFormBudget(r modeltable.Resolved, before int64) int64 {
	if r.ContextWindow <= 0 {
		return before * 60 / 100
	}
	window := int64(r.ContextWindow)
	limit := window * 70 / 100
	if r.MaxOutputTokens > 0 {
		limit -= int64(r.MaxOutputTokens)
	}
	if limit < 0 {
		limit = 0
	}
	return limit
}

// textFormPrompt builds the text form's one prompt (plan 028 §3.8 item 5):
// the fixed part — the system prompt, the compaction prompt, the state
// section, the focus, the prior summary kept whole — counted first; ordinary
// turns (steps) added newest-first while the assembled request stays within
// budget; ordinary turns that do not fit are simply left out (oldest
// dropped). If even the newest turn does not fit at the ordinary cap, its
// results are cut to resultLadderCap and the fit is checked once more; if the
// fixed part alone, or still the newest turn, exceeds the budget,
// errTextFormOverflow.
func (s *Session) textFormPrompt(m model, before int64, steps []store.Step, priorSummary, focus string, red *redact.Replacer) (string, error) {
	budget := textFormBudget(m.r, before)
	promptCore := s.compactionPromptText(focus, red)
	fixed := textTokens(s.system) + textTokens(promptCore) + textTokens(priorSummary)
	if fixed > budget {
		return "", errTextFormOverflow
	}
	remaining := budget - fixed

	type chosen struct {
		lines string
		size  int64
	}
	var picked []chosen
	sum := int64(0)
	for i := len(steps) - 1; i >= 0; i-- {
		lines := textLines(steps[i].Messages, resultLineCap)
		n := textTokens(lines)
		if sum+n > remaining {
			break
		}
		sum += n
		picked = append([]chosen{{lines, n}}, picked...)
	}
	if len(picked) == 0 && len(steps) > 0 {
		last := steps[len(steps)-1]
		lines := textLines(last.Messages, resultLadderCap)
		n := textTokens(lines)
		if n > remaining {
			return "", errTextFormOverflow
		}
		picked = []chosen{{lines, n}}
	}

	var b strings.Builder
	if priorSummary != "" {
		b.WriteString(priorSummary)
		b.WriteString("\n\n")
	}
	for _, c := range picked {
		b.WriteString(c.lines)
	}
	b.WriteString("\n")
	b.WriteString(promptCore)
	return b.String(), nil
}

// textLines renders msgs (one step's, or a segment's) as opencode's lines
// (plan 028 §3.8 item 5): "[User]: text", "[Assistant]: text",
// "[Assistant tool call]: name(json)", "[Tool result]: text", each result
// capped at limit bytes plus "[truncated]" (limit < 0 is no cap).
func textLines(msgs []fantasy.Message, limit int) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case fantasy.MessageRoleUser:
			if t := textOf(m); t != "" {
				b.WriteString("[User]: " + t + "\n")
			}
		case fantasy.MessageRoleAssistant:
			if t := textOf(m); t != "" {
				b.WriteString("[Assistant]: " + t + "\n")
			}
			for _, p := range m.Content {
				if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok && !c.ProviderExecuted {
					fmt.Fprintf(&b, "[Assistant tool call]: %s(%s)\n", c.ToolName, c.Input)
				}
			}
		case fantasy.MessageRoleTool:
			for _, p := range m.Content {
				if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && !r.ProviderExecuted {
					b.WriteString("[Tool result]: " + capText(resultText(r), limit) + "\n")
				}
			}
		}
	}
	return b.String()
}

// resultText is a tool result part's text: its output, or its error's.
func resultText(r fantasy.ToolResultPart) string {
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](r.Output); ok {
		return o.Text
	}
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](r.Output); ok && o.Error != nil {
		return o.Error.Error()
	}
	return ""
}

// capText is s cut to at most limit bytes, "[truncated]" appended when it
// was (limit < 0 is no cap).
func capText(s string, limit int) string {
	if limit < 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "[truncated]"
}

// priorSummaryText is the prior compaction's summary message, verbatim, when
// msgs starts with one (plan 028 §3.8 item 5: "the prior summary message
// always kept whole"): msgs[0] is a compaction's summary rather than a
// results entry when it is marked (marks[0]) and wraps compactedTag —
// exactly what summaryMessage writes and nothing a sub-agent's results ever
// would.
func priorSummaryText(msgs []fantasy.Message, marks []bool) (string, bool) {
	if len(msgs) == 0 || len(marks) == 0 || !marks[0] {
		return "", false
	}
	t := textOf(msgs[0])
	if !strings.Contains(t, "<"+compactedTag+">") {
		return "", false
	}
	return t, true
}

// priorSummaryIf is text form's convenience for priorSummaryText's result:
// text when ok, "" otherwise.
func priorSummaryIf(ok bool, text string) string {
	if !ok {
		return ""
	}
	return text
}

// tailMessages is the messages of steps[k:], concatenated: the tail a
// compaction keeps, for estimating the context after it (§3.7).
func tailMessages(steps []store.Step, k int) []fantasy.Message {
	var out []fantasy.Message
	for _, st := range steps[k:] {
		out = append(out, st.Messages...)
	}
	return out
}

// nextSegmentNumber is the number this session's next successful compaction
// on the current path gets (plan 028 §3.10): the count of successful
// compactions already on it, plus 1.
func (s *Session) nextSegmentNumber() (int, error) {
	tr := s.store.Transcript()
	path, err := tr.Branch(tr.Leaf())
	if err != nil { // the leaf is always known
		return 0, err
	}
	n := 0
	for _, e := range path {
		if e.Type == store.TypeCompaction && e.Compaction.Succeeded() {
			n++
		}
	}
	return n + 1, nil
}

// cleanSummary is a summarizer reply, cleaned (plan 028 §3.8 item 4): every
// <analysis>…</analysis> block dropped, then the text of the last
// <summary>…</summary> block found (tags included) — or, with none, the
// whole reply, trimmed — with </compacted_context> defused (escapeCompacted,
// P39) and redacted.
func cleanSummary(reply string, red *redact.Replacer) string {
	reply = dropAnalysisBlocks(reply)
	text := strings.TrimSpace(reply)
	if block, ok := lastSummaryBlock(reply); ok {
		text = block
	}
	return escapeCompacted(red.String(text))
}

// dropAnalysisBlocks removes every <analysis>…</analysis> span from s,
// including one left unterminated (dropped to the end of s).
func dropAnalysisBlocks(s string) string {
	const open, close = "<analysis>", "</analysis>"
	for {
		i := strings.Index(s, open)
		if i == -1 {
			return s
		}
		rest := s[i+len(open):]
		j := strings.Index(rest, close)
		if j == -1 {
			return s[:i]
		}
		s = s[:i] + rest[j+len(close):]
	}
}

// lastSummaryBlock is the last <summary>…</summary> span in s, tags
// included.
func lastSummaryBlock(s string) (string, bool) {
	const open, close = "<summary>", "</summary>"
	start := strings.LastIndex(s, open)
	if start == -1 {
		return "", false
	}
	rest := s[start:]
	end := strings.Index(rest, close)
	if end == -1 {
		return "", false
	}
	return rest[:end+len(close)], true
}
