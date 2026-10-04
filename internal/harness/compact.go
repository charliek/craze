package harness

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
// retrying, in characters (degenerateLen), tags excluded (plan 028 §3.8 item
// 5, review r1-c9 finding 16): shorter than this, or empty, is degenerate.
const minSummaryLen = 500

// compactionPrompt is craze's own compaction prompt (plan 028 §3.8 item 2,
// golden testdata/compaction_prompt.golden): the seven headings, in order,
// the model writes its summary under, the sentence excluding this request
// itself from what the headings describe (C9e item 2: heading 2 and 7 must
// not treat the summarization request as a user message or as the next
// step), and the two instructions that follow. It is one component of the
// request's one user message (compactionPromptText); the state section and
// the focus line are appended after it, never inside it, so this text alone
// is what the golden pins.
//
// Regenerate with:
//
//	go test ./internal/harness -run TestCompactionPromptGolden -update
const compactionPrompt = `Summarize this conversation so it can continue after everything above is removed from your context. Write one <summary> block with these seven headings, in this order, each on its own line with its content beneath it. Write "None." under a heading with nothing to report.

1. Request and intent
2. User messages (each, in order; verbatim when short; not this summarization request)
3. Decisions and context
4. Files and code
5. Errors and fixes
6. Work state (Done / In progress / Blocked)
7. Next step (quoting the instruction it follows from)

This request to summarize is not part of the conversation: do not list it as a user message or as the next step.

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
// or under minSummaryLen characters (plan 028 §3.8 item 5). It is retried like a
// transient provider failure, and is the failure entry's Error when every
// attempt was one.
var errDegenerateSummary = errors.New("harness: the summarizer's reply was too short to be a real summary")

// errTextFormOverflow is the text form's own budget refusing to fit even the
// fixed part, or the newest turn alone (plan 028 §3.8 item 5, R2-5): the
// compaction fails. It wraps the sentinel, whose text brings the "harness: "
// prefix, so it adds none of its own (C9c item 5).
var errTextFormOverflow = fmt.Errorf("%w: the compaction's text form does not fit its budget", ErrContextTooLarge)

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
// turns that into a cancelled Result, as Run's finish does. A session found
// in its refusal state before an attempt ends them the same way, with
// ErrStoredKeyFrozen (compactRefused, plan 034 C4r2), and so does one found
// in it once the attempts have failed (C4r3): the key was learned during the
// last attempt, or one that failed fatally.
func (s *Session) compact(ctx context.Context, m model, turn int, reason, focus, command string, emit func(Event)) (CompactResult, error) {
	return s.compactOn(ctx, m, m.r, reason == store.CompactionOverflow, 0, turn, reason, focus, command, emit)
}

// compactOn is compact with the choices the previous-model rule and overflow
// recovery make otherwise (plan 028 §3.6, PD13, §3.12; autocompact.go,
// turn.go): fit is the model whose threshold the compacted context must get
// under — the turn's, when m is the previous model that holds the context —
// which bounds the tail (tailBudget, PD23); textForm starts the summarizer in
// the text form rather than switching to it on an overflow, as every
// overflow compaction does (P17) and a pre-turn one whose previous model no
// longer resolves does on the turn's model, whose window the aligned request
// may not fit; and sent, for an overflow compaction, is the estimate of the
// turn's request the provider refused (turn.requestTokens) — the request
// that overflowed, which an unknown window's text form is budgeted by
// (textFormBudget, review r1-c12) — and 0 for every other compaction, whose
// text form follows the aligned summarizer request it would have sent.
//
// A compaction that ran — every one but nothing to compact, which emits
// nothing — is followed on emit, in a session that is not a sub-agent, by the
// Spent it leaves (spend.go, plan 028 §3.14): turn's spend and the session's,
// and the context the next request sends to fit's model. One whose entry
// could not be written (*errCompactionSaveFailed) first has its attempts'
// usage — its Compacted{ended}'s, every attempt's — noted as observed and
// unsaved on m, so the spend still counts what no entry holds. A panic from
// the summarizer or the sink is propagated, as compactRun propagates it, with
// no Spent after its ended.
func (s *Session) compactOn(ctx context.Context, m model, fit modeltable.Resolved, textForm bool, sent int64, turn int, reason, focus, command string, emit func(Event)) (CompactResult, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	if s.child {
		return s.compactRun(ctx, m, fit, textForm, sent, turn, reason, focus, command, emit)
	}
	var ended *Compacted
	res, err := s.compactRun(ctx, m, fit, textForm, sent, turn, reason, focus, command, func(ev Event) {
		if c, ok := ev.(Compacted); ok && c.Phase == CompactionEnded {
			ended = &c
		}
		emit(ev)
	})
	if ended != nil {
		if errors.As(err, new(*errCompactionSaveFailed)) {
			s.noteUnsaved(turn, m.id(), ended.Usage)
		}
		emit(s.spent(turn, fit))
	}
	return res, err
}

// compactRun is compactOn's compaction itself, every event of it on emit and
// no Spent (see compactOn).
func (s *Session) compactRun(ctx context.Context, m model, fit modeltable.Resolved, textForm bool, sent int64, turn int, reason, focus, command string, emit func(Event)) (CompactResult, error) {
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
	// The history as a request to m sends it: redacted, and stripped of
	// images when m does not accept them (stripImages, plan 033 §3.5) — what
	// before weighs, and what the aligned summarizer replays, so its prefix
	// is the turns' own on such a model.
	history := requestHistory(red, msgs, marks, m.r)
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

	var (
		reply   string
		usage   store.Usage
		lastErr error
	)
	// endedSent is whether Compacted{ended} has gone to the sink: set before
	// the sink is called, so an ended the sink panicked while handling
	// counts as sent (review r2 minor 3).
	endedSent := false
	sink := emit
	emit = func(ev Event) {
		if c, ok := ev.(Compacted); ok && c.Phase == CompactionEnded {
			endedSent = true
		}
		sink(ev)
	}
	// ended is deferred before started is emitted, so it covers every exit
	// path from there on — a panic included, from agent construction or
	// streaming (review r1-c9 finding 4, P34), or from the sink itself while
	// it handles started, which it may already have recorded (review r3
	// minor 3): a recovering caller must not keep an unmatched compaction
	// lifecycle. Every ordinary return path below emits Compacted{ended}
	// itself before it returns, which is not a panic, so recover() there is
	// nil and this defer does nothing; "nothing to compact" (above) returns
	// before started is even emitted, and stays event-free, as it always
	// has. ended goes out exactly once (review r2 minor 3): a panic from the
	// sink while it handled an ended already sent is propagated as it is,
	// not answered with a second ended; and the panic propagated is always
	// the one that ended compact — a sink that panics again on this defer's
	// own ended does not replace it.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if !endedSent {
			func() {
				defer func() { _ = recover() }() // r, not the sink's, is what propagates
				emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before,
					Err: fmt.Sprintf("panic: %v", r), Usage: usage})
			}()
		}
		panic(r)
	}()
	emit(Compacted{Phase: CompactionStarted, Reason: reason})
	priorSummary, hasPrior := priorSummaryText(history, s.store.LeadsWithSummary())

attempts:
	for attempt := 1; attempt <= summarizerAttempts; attempt++ {
		// Every attempt sends the frozen system prompt (s.system), so a key
		// learned since the last one — while its request was outstanding, or
		// during the backoff — that is inside it, the tools or the plan path
		// puts the session in its refusal state, and no attempt follows (plan
		// 034 C4r2, r11 #2b): not after a failure the attempts would retry,
		// nor a degenerate summary, nor the aligned form's overflow, which
		// switches to the text form at once. Neither summarizer agent prepares
		// its step (no PrepareStep), and the turn's boundaries guard only the
		// way into a compaction (restartDue, overflowed), so this is the
		// summarizer's own; a manual /compact meets it too.
		if s.tools.refusing.Load() {
			return s.compactRefused(m, turn, reason, command, usage, before, emit)
		}
		var (
			out string
			u   store.Usage
			err error
		)
		if textForm {
			out, u, err = s.summarizeText(ctx, m, before, sent, steps, priorSummaryIf(hasPrior, priorSummary), focus, red)
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
				if attempt < summarizerAttempts && !s.backoff(ctx, summarizerBackoff[attempt-1]) {
					return s.compactCancelled(ctx, m, turn, reason, command, usage, before, emit)
				}
				continue attempts
			}
			break attempts
		}
		cleaned := cleanSummary(out, red)
		if degenerateLen(cleaned) < minSummaryLen {
			lastErr = errDegenerateSummary
			if attempt < summarizerAttempts && !s.backoff(ctx, summarizerBackoff[attempt-1]) {
				return s.compactCancelled(ctx, m, turn, reason, command, usage, before, emit)
			}
			continue attempts
		}
		reply, lastErr = cleaned, nil
		break attempts
	}

	if reply == "" {
		// The loop's own check guards only the way into an attempt: a key
		// learned while the last attempt's request was outstanding, or
		// while one that then failed fatally — or overflowed the text
		// form's budget — was, leaves the loop here without meeting it
		// (plan 034 C4r3, r13 #3). The failure is the refusal's then, as it
		// would have been had another attempt followed: no failure entry
		// unless an attempt was billed, automatic compaction left as it
		// was, and the caller stopping on ErrStoredKeyFrozen — an overflow
		// recovery's turn included, which would otherwise end with the
		// overflow.
		if s.tools.refusing.Load() {
			return s.compactRefused(m, turn, reason, command, usage, before, emit)
		}
		if lastErr == nil {
			lastErr = errDegenerateSummary
		}
		return s.compactFailed(m, turn, reason, command, usage, before, lastErr, emit)
	}
	return s.compactSucceeded(m, turn, reason, command, usage, before, firstKeptID, k, steps, reply, emit)
}

// backoff is s.sleep(ctx, d), then whether ctx is still live: false ends the
// attempts loop at once rather than waiting out the rest of the delay and
// then failing the next attempt's own way (review r1-c9 finding 10 — a
// cancel during backoff must not be waited out).
func (s *Session) backoff(ctx context.Context, d time.Duration) bool {
	s.sleep(ctx, d)
	return ctx.Err() == nil
}

// compactCancelled is compact's return once ctx has ended (item 8): a
// failure entry only if some attempt was billed, and the caller's own
// cancellation either way.
func (s *Session) compactCancelled(ctx context.Context, m model, turn int, reason, command string, usage store.Usage, before int64, emit func(Event)) (CompactResult, error) {
	if usage != (store.Usage{}) {
		if _, err := s.appendCompactionFailure(m, turn, reason, command, usage, ctx.Err().Error(), emit); err != nil {
			emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: s.redactor().String(err.Error()), Usage: usage})
			return CompactResult{}, &errCompactionSaveFailed{err}
		}
	}
	emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: "cancelled", Usage: usage})
	return CompactResult{}, ctx.Err()
}

// compactRefused is compact's return once the session is refusing before an
// attempt (plan 034 C4r2, r11 #2b), or after attempts that all failed (C4r3,
// r13 #3): no further request is sent, and the compaction
// ends as a cancelled one does (compactCancelled) — a failure entry only if
// some attempt was billed, so what it spent is held by an entry, and
// Compacted{ended} with the refusal's own text, which names no key. It
// returns ErrStoredKeyFrozen, which every caller stops on: the turn ends with
// the refusal (stopBeforeRequest), and a manual /compact returns it.
func (s *Session) compactRefused(m model, turn int, reason, command string, usage store.Usage, before int64, emit func(Event)) (CompactResult, error) {
	msg := cleanErrorText(ErrStoredKeyFrozen)
	if usage != (store.Usage{}) {
		if _, err := s.appendCompactionFailure(m, turn, reason, command, usage, msg, emit); err != nil {
			emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: s.redactor().String(err.Error()), Usage: usage})
			return CompactResult{}, &errCompactionSaveFailed{err}
		}
	}
	emit(Compacted{Phase: CompactionEnded, Reason: reason, TokensBefore: before, Err: msg, Usage: usage})
	return CompactResult{}, ErrStoredKeyFrozen
}

// compactFailed is compact's return once every attempt (or the text form's
// own budget) failed to produce a usable summary: a failure entry (PD20),
// Compacted{ended, Err} by way of the caller (compact itself has no defer:
// every return path here emits it explicitly, since Go has no single exit
// point worth deferring around without hiding what each path carries).
func (s *Session) compactFailed(m model, turn int, reason, command string, usage store.Usage, before int64, cause error, emit func(Event)) (CompactResult, error) {
	msg := s.redactor().String(cleanErrorText(cause))
	if _, err := s.appendCompactionFailure(m, turn, reason, command, usage, msg, emit); err != nil {
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
	// The redactor above is taken fresh, at persistence time, and may know a
	// key the one the attempts ran under did not: a running background child
	// can learn one while the summarizer was retrying (review r1-c9 finding
	// 9). Re-redacting is a no-op for every key already gone from reply, so
	// this only ever removes what only the fresh redactor knows.
	reply = red.String(reply)
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
// here is reported through DiagCompactionUnsaved, on emit — the caller's own
// (review r1-c9 finding 7: a no-op here would silently drop the diagnostic
// on every failure-entry append failure, not only the successful-summary
// one).
func (s *Session) appendCompactionFailure(m model, turn int, reason, command string, usage store.Usage, msg string, emit func(Event)) (string, error) {
	c := store.Compaction{Reason: reason, Command: command, Error: msg}
	id, err := s.store.AppendCompaction(turn, m.id(), usage, c)
	if err != nil {
		s.diagUnsaved(emit, turn, reason, usage, err)
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
// summarizer failure otherwise; ErrStoredKeyFrozen when a key learned while
// it ran put the session in its refusal state before an attempt
// (compactRefused, plan 034 C4r2); or *errCompactionSaveFailed, a save
// failure like any turn's. A cancelled ctx ends the turn cancelled, as any
// turn's does, with a nil error.
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
	default:
		// A save failure must reach the caller as one — stop, as for any
		// other save failure (P5) — even when ctx also ended: cerr being
		// *errCompactionSaveFailed takes priority over turnCtx having ended,
		// or a real append failure a cancellation raced with would be
		// silently turned into an ordinary cancelled result and lost (review
		// r1-c9 finding 3).
		var saveErr *errCompactionSaveFailed
		if !errors.As(cerr, &saveErr) && turnCtx.Err() != nil {
			return Result{StopReason: StopCancelled}, nil
		}
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
// authentication, model-not-found, an account whose quota or credit is gone
// whatever the status says (quotaExhausted — an in-band stream error has
// none, review r3), a failure the provider said not to repeat (the ChatGPT
// plan's Final, plan 033 P33 — a retry would be the same request, every 429
// on that driver among them), the sign-in's own failure (signed out, the
// usage latch: nothing a backoff changes, §3.12), or any other client error
// the provider raised deliberately (a 4xx this craze has no more specific
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
	if !errors.As(err, &pe) {
		return ""
	}
	switch {
	case pe.Final, pe.signIn != nil, quotaExhausted(pe):
		return "fatal"
	case pe.StatusCode >= 400 && pe.StatusCode < 500 && !transientClientStatus(pe):
		return "fatal"
	}
	return ""
}

// quotaExhaustedPattern matches the provider's own text for a 429 that means
// the account has no quota or credit left, rather than an ordinary rate
// limit worth retrying: OpenAI-family "insufficient_quota", and the more
// common human phrasing around it (review r1-c9, decision 4). It is
// quotaExhausted's fallback, for an error with no structured code or type
// that names the failure.
var quotaExhaustedPattern = regexp.MustCompile(
	`(?i)insufficient_quota|insufficient quota|exceeded (?:your |its )?(?:current )?quota|quota exceeded|out of credits?|credit balance`)

// quotaExhaustedCodes are the structured error codes and types (lowercase)
// that name a failure as the account's quota or credit being gone — a 429's
// or an in-band stream error's alike (ProviderError.Code, .Type):
// OpenAI-family "insufficient_quota", and the ChatGPT plan's usage limit
// (plan 033 §3.12), which a retry cannot lift either.
var quotaExhaustedCodes = map[string]bool{"insufficient_quota": true, CodeUsageLimit: true}

// transientClientStatus reports whether a classified 4xx failure is worth
// retrying rather than ending the summarizer at once (plan 028 §3.8 decision
// 4, review r1-c9 finding 5): HTTP 408 (request timeout) and 429 (rate
// limit) are transient — EXCEPT a 429 whose provider error says the
// account's quota or credit is exhausted (quotaExhausted), which a retry
// cannot fix. HTTP 402 (payment required) is exactly that failure by its
// status alone, whatever it says, and every other 4xx (400, 404 already
// handled by its own sentinel above, 422, …) is fatal, as before.
func transientClientStatus(pe *ProviderError) bool {
	switch pe.StatusCode {
	case 408:
		return true
	case 429:
		return !quotaExhausted(pe)
	default:
		return false
	}
}

// quotaExhausted reports whether pe says the account's quota or credit is
// gone rather than that it is merely rate limited (review r2 major 2): by the
// provider's structured error first — its code or type, what the provider
// says the failure IS (quotaExhaustedCodes), kept by classify — whatever the
// status, since an in-band stream error carries none (review r3); and then,
// as a fallback for a provider whose error carries no such name, by the
// phrases of its message (quotaExhaustedPattern), which is display text,
// cleaned and cut to maxMessageBytes — for a 429, or an error with no
// status, alone: a status that says the failure is something else (a 5xx,
// say) outranks a phrase.
func quotaExhausted(pe *ProviderError) bool {
	if quotaExhaustedCodes[strings.ToLower(pe.Code)] || quotaExhaustedCodes[strings.ToLower(pe.Type)] {
		return true
	}
	if pe.StatusCode != 429 && pe.StatusCode != 0 {
		return false
	}
	return quotaExhaustedPattern.MatchString(pe.Message)
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

// observeUsage is the OnStreamFinish callback both summarizer forms give
// their call: a stream can report its usage in a Finish part and only then
// fail — a cancellation after it, say, or a provider error riding in on the
// same stream (review r1-c9 finding 1) — and Fantasy's AgentResult never
// reaches the caller when Stream itself returns an error, so that usage
// would otherwise be lost. *into is set on every call (there is at most one
// Finish part before StopWhen ends the one step both forms run).
func observeUsage(into *store.Usage) func(fantasy.Usage, fantasy.FinishReason, fantasy.ProviderMetadata) error {
	return func(u fantasy.Usage, _ fantasy.FinishReason, _ fantasy.ProviderMetadata) error {
		*into = *store.UsageOf(u)
		return nil
	}
}

// summarizeAligned sends the summarizer's aligned request (plan 028 §3.8
// item 1, P11, PD10): the session's own agent, with the session's tools
// offered inert (s.inertTools, same Info as a turn's) so Fantasy converts
// and normalizes them exactly as a turn's would, one step
// (fantasy.StepCountIs(1)), the same provider options, headers
// (requestHeaders) and output ceiling as a turn. Messages is history,
// exactly the next request's; Prompt is the compaction prompt, the state
// section, and, for a focus, the "Focus this summary on" line (item 1, item
// 2, item 3). Its agent's own retries are off
// (newSummarizerAgent, review r1-c9 "three attempts means three requests"):
// compact's outer attempts are the whole retry budget.
//
// The summarizer is never sent an image (plan 033 §3.5, P8), whatever the
// model: history is the request's as a turn on m sends it — already stripped
// of images when m does not accept them, which keeps it aligned with the
// turns' — and whatever images are left, on a model that does accept them,
// go as placeholders saying the summarizer is not sent them (summarizerOmits)
// rather than that the model cannot see them, which it can, and which the
// summary would otherwise carry into the turns after it.
func (s *Session) summarizeAligned(ctx context.Context, m model, history []fantasy.Message, focus string, red *redact.Replacer) (string, store.Usage, error) {
	agent := s.newSummarizerAgent(m.lm, s.system, s.inertTools())
	var observed store.Usage
	call := fantasy.AgentStreamCall{
		Prompt:          s.compactionPromptText(focus, red),
		Messages:        omitImages(history, summarizerOmits),
		ProviderOptions: m.effortOpts,
		Headers:         requestHeaders(m.r, s.store.ID()),
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
		OnStreamFinish:  observeUsage(&observed),
	}
	if n := m.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		call.MaxOutputTokens = &ceiling
	}
	res, err := agent.Stream(ctx, call)
	if err != nil {
		return "", observed, classify(err, m.r)
	}
	return res.Response.Content.Text(), *store.UsageOf(res.TotalUsage), nil
}

// summarizeText sends the summarizer's text-form request (plan 028 §3.8
// item 5): the context serialized as opencode's lines, budgeted, with the
// same prompt and no tools at all (not even inert ones — there is nothing
// left to answer a call with room for). Its agent's own retries are off, as
// summarizeAligned's are. before and sent are textFormPrompt's.
func (s *Session) summarizeText(ctx context.Context, m model, before, sent int64, steps []store.Step, priorSummary, focus string, red *redact.Replacer) (string, store.Usage, error) {
	prompt, err := s.textFormPrompt(m, before, sent, steps, priorSummary, focus, red)
	if err != nil {
		return "", store.Usage{}, err
	}
	agent := s.newSummarizerAgent(m.lm, s.system, nil)
	var observed store.Usage
	call := fantasy.AgentStreamCall{
		Prompt:          prompt,
		ProviderOptions: m.effortOpts,
		Headers:         requestHeaders(m.r, s.store.ID()),
		StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
		OnStreamFinish:  observeUsage(&observed),
	}
	if n := m.r.MaxOutputTokens; n > 0 {
		ceiling := int64(n)
		call.MaxOutputTokens = &ceiling
	}
	res, err := agent.Stream(ctx, call)
	if err != nil {
		return "", observed, classify(err, m.r)
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
			// c.desc was redacted at launch, under whatever the redactor knew
			// then; red is the one current now, which may know a key the
			// child's own description exposed since (review r1-c9 finding
			// 8) — re-redacting is a no-op otherwise.
			fmt.Fprintf(&b, "- %s (%s): %s", c.id, c.typ, red.String(c.desc))
			if i < len(children)-1 {
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// textFormBudget is the text form's whole-request budget (plan 028 §3.8
// item 5, R2-5): 70% of the window less its output ceiling, or, with an
// unknown window, 60% of overflowed — the estimate of the request that
// ACTUALLY overflowed: for an overflow compaction, the turn's own request
// the provider refused, held prompt and carried splices included (review
// r1-c12); otherwise the aligned summarizer request compact always builds
// first, whether or not it was sent, including the compaction prompt, the
// state section and the focus (review r1-c9 finding 14) — never the raw
// context alone, which never went to the model by itself.
func textFormBudget(r modeltable.Resolved, overflowed int64) int64 {
	if r.ContextWindow <= 0 {
		return overflowed * 60 / 100
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
// results are cut to resultLadderCap and the fit is checked once more.
// Each step's messages are redacted with the live redactor before they are
// rendered (liveRedact, review r1-c9 finding 2): steps carries no per-message
// "results" mark the way redactHistory's callers do, so it redacts every
// message's text unconditionally, as segment.go's own transcript rendering
// does for the same reason. Once assembled, the whole prompt — separators
// included — is checked against budget once more (review r1-c9 finding 15),
// trimming further, oldest first, if the pieces summed short of what
// assembling them actually costs — but never the newest turn (review r2
// major 1): once it is all that is left and the whole is still over, its
// results are cut to resultLadderCap as above, and if the fixed part plus
// that cut turn still exceed the budget — or the fixed part alone does —
// errTextFormOverflow. A context with turns never becomes a prompt that is
// the fixed part alone.
//
// before is the stored context's estimate (estimateContext); sent is an
// overflow compaction's estimate of the turn's request that overflowed, 0
// for any other compaction (compactOn).
func (s *Session) textFormPrompt(m model, before, sent int64, steps []store.Step, priorSummary, focus string, red *redact.Replacer) (string, error) {
	promptCore := s.compactionPromptText(focus, red)
	// What actually overflowed, for the unknown-window fallback
	// (textFormBudget): an overflow compaction's refused turn request, sent
	// (review r1-c12); otherwise the aligned summarizer request's own
	// estimate (finding 14) — before already counts the system prompt, the
	// tools and the history (estimateContext), and promptCore is the one user
	// message compact adds on top of it, weighed as the message it is sent as
	// — its JSON, not its bare text (review r2 minor 6) — exactly as before
	// weighs each message of the history.
	overflowed := sent
	if overflowed <= 0 {
		overflowed = before + messageTokens(fantasy.NewUserMessage(promptCore))
	}
	budget := textFormBudget(m.r, overflowed)
	fixed := textTokens(s.system) + textTokens(promptCore) + textTokens(priorSummary)
	if fixed > budget {
		return "", errTextFormOverflow
	}
	remaining := budget - fixed

	type chosen struct {
		lines string
		size  int64
		cut   bool // the newest turn, its results cut to resultLadderCap
	}
	// newestCut is the newest turn with its results cut to resultLadderCap:
	// what a turn that does not fit at the ordinary cap is tried as, once,
	// before the compaction fails.
	newestCut := func() chosen {
		lines := textLines(redactStepMessages(red, steps[len(steps)-1].Messages), resultLadderCap)
		return chosen{lines, textTokens(lines), true}
	}
	var picked []chosen
	sum := int64(0)
	for i := len(steps) - 1; i >= 0; i-- {
		lines := textLines(redactStepMessages(red, steps[i].Messages), resultLineCap)
		n := textTokens(lines)
		if sum+n > remaining {
			break
		}
		sum += n
		picked = append([]chosen{{lines, n, false}}, picked...)
	}
	if len(picked) == 0 && len(steps) > 0 {
		c := newestCut()
		if c.size > remaining {
			return "", errTextFormOverflow
		}
		picked = []chosen{c}
	}

	assemble := func(picked []chosen) string {
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
		return b.String()
	}

	// The pieces were budgeted separately, against a budget meant to bound
	// the whole request (system prompt included, as fixed above does);
	// assembling them adds separators (the blank line after the prior
	// summary, the newline before the prompt core) that were never counted
	// (review r1-c9 finding 15). Check the assembled whole, system prompt
	// and all, once more, trimming the oldest picked turn at a time until it
	// fits or only the newest is left — picked is always a run of the newest
	// turns, so its last is the newest. That one is never dropped (review r2
	// major 1): it is cut to resultLadderCap instead, unless it already is,
	// and if the whole is still over, the compaction fails.
	total := func(result string) int64 { return textTokens(s.system) + textTokens(result) }
	result := assemble(picked)
	for total(result) > budget && len(picked) > 1 {
		picked = picked[1:]
		result = assemble(picked)
	}
	if total(result) > budget && len(picked) == 1 && !picked[0].cut {
		picked = []chosen{newestCut()}
		result = assemble(picked)
	}
	if total(result) > budget {
		return "", errTextFormOverflow
	}
	return result, nil
}

// redactStepMessages is msgs, live-redacted (liveRedact) message for
// message: a copy, msgs itself untouched.
func redactStepMessages(red *redact.Replacer, msgs []fantasy.Message) []fantasy.Message {
	out := make([]fantasy.Message, len(msgs))
	for i, m := range msgs {
		out[i] = liveRedact(red, m)
	}
	return out
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

// resultText is a tool result part's text: its output, its error's, or an
// image result's text (outputText) — never the image, so the text form, like
// the aligned one, sends the summarizer no pixels (plan 033 §3.5).
func resultText(r fantasy.ToolResultPart) string {
	text, _ := outputText(r.Output)
	return text
}

// capText is s cut to at most limit runes — not bytes (review r1-c9 finding
// 16): a non-ASCII result kept substantially less than the cap, and cutting
// on bytes alone could split a multi-byte rune — with "[truncated]" appended
// when it was cut (limit < 0 is no cap).
func capText(s string, limit int) string {
	if limit < 0 {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i] + "[truncated]"
		}
		n++
	}
	return s
}

// priorSummaryText is the prior compaction's summary message, verbatim, when
// msgs starts with one (plan 028 §3.8 item 5: "the prior summary message
// always kept whole"): hasSummary says so precisely (store.LeadsWithSummary,
// review r1-c9 finding 13) rather than by guessing from msgs[0]'s content —
// a background sub-agent's result can legitimately hold text that reads like
// the summary wrapper, and is never mistaken for it now. msgs is the
// redacted history (redactHistory's own output), so the text returned here
// is already live-redacted.
func priorSummaryText(msgs []fantasy.Message, hasSummary bool) (string, bool) {
	if !hasSummary || len(msgs) == 0 {
		return "", false
	}
	return textOf(msgs[0]), true
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

// cleanSummary is a summarizer reply, cleaned (plan 028 §3.8 item 4, C9e item
// 1): every <analysis>…</analysis> block dropped, then the text strictly
// inside the last <summary>…</summary> block found, trimmed — or, with none,
// the whole reply, trimmed — with </compacted_context> defused
// (escapeCompacted, P39) and redacted. The tags themselves are never stored:
// only the summary's own text is (review r2-c13a-c9d finding 4 was the smoke
// that found them still there).
func cleanSummary(reply string, red *redact.Replacer) string {
	reply = dropAnalysisBlocks(reply)
	text := strings.TrimSpace(reply)
	if block, ok := lastSummaryBlock(reply); ok {
		text = strings.TrimSpace(summaryBlockText(block))
	}
	return escapeCompacted(red.String(text))
}

// summaryBlockText is block's text strictly inside its <summary>…</summary>
// tags: block is always exactly one such span, as lastSummaryBlock returns
// it.
func summaryBlockText(block string) string {
	const open, close = "<summary>", "</summary>"
	return block[len(open) : len(block)-len(close)]
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

// lastSummaryBlock is the last COMPLETE <summary>…</summary> span in s, tags
// included (orchestrator decision X41, review r1-c14a finding 1, replacing
// the plain first-opener rule that finding pinned down):
//
//   - C is the index of the last "</summary>". None → ("", false); the
//     caller falls back to the whole reply, trimmed.
//   - P is the index just past the last "</summary>" before C, or 0 when
//     there is none — the end of the previous complete block, if any.
//   - The opener is the FIRST "<summary>" in [P, C) that begins a line (at
//     P itself, or the start of s, or after a newline — a "\r\n" ending
//     counts — optionally followed by spaces or tabs); if none in [P, C)
//     begins a line, the FIRST "<summary>" in [P, C) of any kind; if
//     [P, C) has no "<summary>" at all, the first line-start "<summary>"
//     before C anywhere (this last fallback uses ordinary line starts, not
//     P, since there is no in-range opener to anchor on).
//
// Restarting the opener search at P (instead of the start of s) is what
// keeps a draft block and its corrected replacement — two complete,
// back-to-back <summary>…</summary> spans — from being joined into one:
// the draft's own opener lives before P and is out of range, so the final
// block's opener wins outright. Within a single real block, a model quoting
// the compaction instruction under heading 2 (or echoing it back on a line
// of its own) puts a second "<summary>" inside the real body, closer to C
// than the real opener is; taking the first line-start opener in range
// still picks the real, first one. A trailing, unfinished opener after C
// (the model started a second block and was cut off) is ignored either
// way, since it never precedes C (review r1-c9 finding 12).
func lastSummaryBlock(s string) (string, bool) {
	const open, close = "<summary>", "</summary>"
	c := strings.LastIndex(s, close)
	if c == -1 {
		return "", false
	}
	p := 0
	if prev := strings.LastIndex(s[:c], close); prev != -1 {
		p = prev + len(close)
	}
	start := firstOpenerInRange(s, open, p, c)
	if start == -1 {
		start = firstLineStartIndex(s[:c], open)
	}
	if start == -1 {
		return "", false
	}
	return s[start : c+len(close)], true
}

// firstOpenerInRange is the index of the first occurrence of sub in
// s[p:c) that begins a line (per beginsLineFrom, treating p itself as a
// line start), or, if none in that range does, the first occurrence of any
// kind in that range; -1 if sub does not occur in [p, c) at all.
func firstOpenerInRange(s, sub string, p, c int) int {
	first := -1
	for i := p; i < c; {
		j := strings.Index(s[i:c], sub)
		if j == -1 {
			break
		}
		idx := i + j
		if first == -1 {
			first = idx
		}
		if beginsLineFrom(s, idx, p) {
			return idx
		}
		i = idx + len(sub)
	}
	return first
}

// firstLineStartIndex is the index of the first occurrence of sub in s that
// begins a line — s[:i] is empty, or ends in a newline optionally followed
// by spaces or tabs — or -1 if none does.
func firstLineStartIndex(s, sub string) int {
	for i := strings.Index(s, sub); i != -1; {
		if beginsLine(s, i) {
			return i
		}
		next := strings.Index(s[i+len(sub):], sub)
		if next == -1 {
			return -1
		}
		i += len(sub) + next
	}
	return -1
}

// beginsLine reports whether s[i:] begins a line: i is 0, or every byte
// back to the previous newline is a space or a tab. A "\r\n" line ending
// still counts: the byte immediately before the run of spaces/tabs is the
// "\n", regardless of the "\r" that precedes it.
func beginsLine(s string, i int) bool {
	return beginsLineFrom(s, i, 0)
}

// beginsLineFrom reports whether s[i:] begins a line, treating position
// from as an implicit line start in addition to the ordinary rule: i ==
// from, or every byte back to from or the previous newline is a space or a
// tab, and the byte immediately before that run (if any, and if it exists
// before reaching from) is a newline. A "\r\n" line ending still counts:
// the byte immediately before the run of spaces/tabs is the "\n",
// regardless of the "\r" that precedes it.
func beginsLineFrom(s string, i, from int) bool {
	for i > from && (s[i-1] == ' ' || s[i-1] == '\t') {
		i--
	}
	return i == from || s[i-1] == '\n'
}

// degenerateLen is the length compact's minSummaryLen check counts (review
// r1-c9 finding 16): runes, not bytes — a non-ASCII summary must not pass
// the check simply for being encoded in more bytes per character — of the
// summary's own text, tags excluded — the retained <summary></summary>
// wrapper must not count toward the minimum either.
func degenerateLen(cleaned string) int {
	const open, close = "<summary>", "</summary>"
	text := cleaned
	if strings.HasPrefix(text, open) && strings.HasSuffix(text, close) {
		text = text[len(open) : len(text)-len(close)]
	}
	return utf8.RuneCountInString(text)
}
