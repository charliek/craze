package harness

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// When a session compacts on its own (plan 028 §3.6, §3.7): the threshold, the
// size of a context, the pre-turn check, the previous-model rule and
// suppression. compact.go does the compacting; the settings are models.toml's
// [compaction] (modeltable.Compaction), one section for every model. A child
// runs the same turn code on its own model's window and its parent's table,
// so it compacts by the same rules (§3.17).
//
// The mid-turn check (midTurnDue, midTurnCompaction) reuses
// compactionThreshold, contextTokens, the suppression and compacted; it is
// asked once per step boundary, as the pre-turn check is asked once before a
// turn's first request, and each makes at most one compaction.

// compactionConfig is the model table's [compaction] section, read under mu
// like every read of the table.
func (s *Session) compactionConfig() modeltable.Compaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.table.Compaction
}

// compactionThreshold is the context size, in tokens, at which a session on
// r compacts on its own (plan 028 §3.6, P32): percent of r's context window,
// rounded down, and never more than the window less r's output ceiling, the
// room a request must leave for its answer. It is 0 — no automatic
// compaction — when the window is unknown (PD12; /compact and the recovery
// from an overflow still work), and when the ceiling takes the whole window,
// which no request could fit either.
func compactionThreshold(r modeltable.Resolved, percent int) int64 {
	if r.ContextWindow <= 0 {
		return 0
	}
	window := int64(r.ContextWindow)
	t := window * int64(percent) / 100
	if r.MaxOutputTokens > 0 {
		t = min(t, window-int64(r.MaxOutputTokens))
	}
	return max(t, 0)
}

// contextTokens is the size, in tokens, of what the next request to m sends,
// its new prompt aside (plan 028 §3.7, P16, P32): the frontier's usage
// (store.Frontier) — input, cache reads and cache writes, and output, since
// Fantasy's OpenAI-family input leaves the cached tokens out — plus the
// estimate of every message the context sends after it: the frontier's own
// tool message, counted once, and the steers, results and reminders after
// it. With no frontier (nothing since the latest compaction carries usage),
// or one whose usage is all zero (a provider that reports none), it is the
// estimate of the whole request instead: the system prompt, the tools, and
// the context as sent (estimateContext).
//
// frontier is the frontier's entry, when there is one — its model is the one
// that holds the context and its prefix cache (the previous-model rule) — and
// nil otherwise.
func (s *Session) contextTokens(m model) (tokens int64, frontier *store.Entry) {
	return s.contextTokensOn(m.r)
}

// contextTokensOn is contextTokens for the model r: the history as a request
// to it would send it — the store's reasoning rule, D-33, is by model, and so
// is the vision strip (stripImages, plan 033 §3.5): to a model that does not
// accept images an image is its placeholder, and to one that does, its
// estimate (messageTokens, P9). The Spent a session reports sizes the next
// request with it (spend.go), for a model it holds the table's entry of.
func (s *Session) contextTokensOn(r modeltable.Resolved) (tokens int64, frontier *store.Entry) {
	id := idOf(r)
	f, ok := s.store.Frontier(id)
	if ok {
		frontier = &f.Entry
		u := f.Entry.Usage
		if n := u.Input + u.CacheRead + u.CacheCreation + u.Output; n > 0 {
			for _, msg := range requestHistory(s.redactor(), f.After, f.Marks, r) {
				n += messageTokens(msg)
			}
			return n, frontier
		}
	}
	msgs, marks := s.store.ContextWithResults(id)
	return s.estimateContext(requestHistory(s.redactor(), msgs, marks, r)), frontier
}

// suppression is automatic compaction switched off (plan 028 §3.6, PD14,
// PD23): after an automatic compaction whose summarizer failed, or after any
// compaction that left the context still at or over the threshold — both of
// which the next boundary would only repeat, paying for it each time. It is
// on again after a compaction that gets the context under the threshold
// (/compact's included: compacted) or a model change. /compact and the
// recovery from an overflow are never suppressed. The zero value is "not
// suppressed".
type suppression struct {
	on bool
	// model is the model of the turn it was switched off in: a turn on any
	// other — another window, another threshold — is a model change.
	model store.Model
}

// autoSuppressed reports whether automatic compaction is off for a turn on
// m, switching it back on first when m is not the model it was switched off
// on: the model has changed.
func (s *Session) autoSuppressed(m model) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.autoOff.on && s.autoOff.model != m.id() {
		s.autoOff = suppression{}
	}
	return s.autoOff.on
}

// suppressAuto switches automatic compaction off, for turns on m.
func (s *Session) suppressAuto(m model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoOff = suppression{on: true, model: m.id()}
}

// compacted records what a successful compaction for a turn on m left
// (§3.6, PD23): a context under m's threshold switches automatic compaction
// back on, and one still at or over it switches it off. With m's window
// unknown there is no threshold to be over, and it is on (for a model with
// no window it never runs anyway).
func (s *Session) compacted(m model, res CompactResult) {
	threshold := compactionThreshold(m.r, s.compactionConfig().ThresholdPercent())
	if threshold > 0 && res.TokensAfter >= threshold {
		s.suppressAuto(m)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoOff = suppression{}
}

// preTurnCompaction is the pre-turn check (plan 028 §3.6). run calls it once
// the turn's steer box is open (P36), so a steer typed while it compacts is
// accepted and goes in the first request, and before that first request,
// with prompt the turn's user message. It compacts — reason auto, recorded
// as the turn it precedes — when automatic compaction is on ([compaction]
// auto, and not suppressed), the turn's model's window is known, and the
// context's size (contextTokens) plus the prompt's estimate reaches that
// model's threshold.
//
// It first switches a suppression back on when the turn's model is not the
// one it was switched off on (autoSuppressed), whatever else the turn is:
// a turn on a model with no window, or with auto off, is a model change as
// much as any (review r1-c10 finding 2), so a suppression on A does not
// outlive a turn on B and last into A's next.
//
// When the turn's model is not the one the frontier's usage was reported on
// — a switch since (PD13) — the compaction runs on that previous model,
// which holds the context and its cache, while its tail and its outcome are
// judged by the turn's threshold; if the previous model no longer resolves,
// it runs on the turn's model in the text form, since the aligned request
// may not fit the smaller window. Either way it makes one compaction, never a
// second on the other model: one per boundary. A compaction that still leaves
// the context over the threshold suppresses the next ones instead.
//
// compacted says a compaction succeeded, so run rebuilds the history. err is
// non-nil only when the turn must stop before its first request: the
// compaction was cancelled (the turn's context), its entry could not be
// written (*errCompactionSaveFailed, P5), which outranks the cancel, or its
// summarizer found the session refusing (ErrStoredKeyFrozen, compactRefused,
// plan 034 C4r2): the turn ends with the refusal, and automatic compaction is
// not switched off for it. A summarizer that failed is not: its failure entry
// is written, automatic compaction is switched off (PD14), and the turn goes
// on with the context it had. Nothing to compact — a new session whose own prompt is over — is no
// compaction and no error: the request goes out as it is.
//
// A compaction that ran and lets the turn go on — to a summary, or to a
// summarizer failure its entry records — sets t.compactedBeforeFirst, so
// the turn's first request takes up the steers accepted while it ran (R2-2,
// review r1-c10 finding 1): the person typed them during it either way, and
// a first request that skipped them would hand them back unanswered when it
// ends the turn.
func (s *Session) preTurnCompaction(t *turn, prompt fantasy.Message) (compacted bool, err error) {
	m := t.model
	suppressed := s.autoSuppressed(m) // the model-change reset: first, on every turn
	cfg := s.compactionConfig()
	if !cfg.Auto() {
		return false, nil
	}
	threshold := compactionThreshold(m.r, cfg.ThresholdPercent())
	if threshold == 0 || suppressed {
		return false, nil
	}
	tokens, frontier := s.contextTokens(m)
	if tokens+messageTokens(prompt) < threshold {
		return false, nil
	}

	on, textForm := m, false
	if frontier != nil && !sameModel(frontier.Model, m.id()) {
		if prev, ok := s.previousModel(frontier.Model, frontier.Effort); ok {
			on = prev
		} else {
			textForm = true
			s.warnf("compaction: the model that holds the context, %s (%s, %s), no longer resolves; compacting on %s in the text form",
				frontier.Model.Alias, frontier.Model.Provider, frontier.Model.WireModel, m.r.Alias)
		}
	}
	res, err := s.compactOn(t.ctx, on, m.r, textForm, 0, t.number, store.CompactionAuto, "", "", t.emitLocked)
	var saveErr *errCompactionSaveFailed
	switch {
	case err == nil:
		s.compacted(m, res)
	case errors.Is(err, store.ErrNothingToCompact):
		return false, nil
	case errors.As(err, &saveErr), t.ctx.Err() != nil, errors.Is(err, ErrStoredKeyFrozen):
		return false, err
	default:
		s.suppressAuto(m)
	}
	t.mu.Lock()
	t.compactedBeforeFirst = true
	t.mu.Unlock()
	return err == nil, nil
}

// autoCompacts reports whether a turn on m can compact on its own at all
// (plan 028 §3.6): [compaction] auto is on and m's window is known. What a
// suppression says is asked at each boundary, not here: it can come and go
// within a turn (a compaction that fails, a /compact cannot run mid-turn, but
// a still-over compaction suppresses the boundaries after it).
func (s *Session) autoCompacts(m model) bool {
	cfg := s.compactionConfig()
	return cfg.Auto() && compactionThreshold(m.r, cfg.ThresholdPercent()) > 0
}

// midTurnDue is the mid-turn check (plan 028 §3.6, §3.11 item 1), asked by
// the turn under its lock at the end of a saved tool_use step: whether the
// context that step left (contextTokens: the step's own assistant entry is
// the frontier, its tool message counted once, §3.7) reaches m's threshold,
// with automatic compaction on and not suppressed. It takes the store lock
// and the redactor's leaf locks under the turn's, the order takeResults
// already takes them in; the session's lock too (compactionConfig,
// autoSuppressed), which recordMode takes under the turn's already.
func (s *Session) midTurnDue(m model) bool {
	suppressed := s.autoSuppressed(m)
	cfg := s.compactionConfig()
	if !cfg.Auto() {
		return false
	}
	threshold := compactionThreshold(m.r, cfg.ThresholdPercent())
	if threshold == 0 || suppressed {
		return false
	}
	tokens, _ := s.contextTokens(m)
	return tokens >= threshold
}

// midTurnCompaction is the compaction between two segments of t (plan 028
// §3.11 items 4, 9), run with no lock of the turn's held (compact streams no
// deltas; its Compacted events go through t.emitLocked): reason auto,
// recorded as the turn it is in, on the turn's own model — the frontier is
// the step that just finished, so there is no previous model to prefer. Its
// outcome is triaged as the pre-turn one's is (preTurnCompaction): a
// summary switches suppression by what it left (compacted); nothing to
// compact — which a turn with a persisted step cannot meet — changes
// nothing; a summarizer failure, its entry written, suppresses automatic
// compaction and the turn goes on with the context it had; and err is
// non-nil only when the turn must stop between requests: the compaction was
// cancelled, its entry could not be written (*errCompactionSaveFailed, P5),
// which outranks the cancel, or its summarizer found the session refusing
// (ErrStoredKeyFrozen, plan 034 C4r2).
func (s *Session) midTurnCompaction(t *turn) error {
	m := t.model
	res, err := s.compact(t.ctx, m, t.number, store.CompactionAuto, "", "", t.emitLocked)
	var saveErr *errCompactionSaveFailed
	switch {
	case err == nil:
		s.compacted(m, res)
		return nil
	case errors.Is(err, store.ErrNothingToCompact):
		return nil
	case errors.As(err, &saveErr), t.ctx.Err() != nil, errors.Is(err, ErrStoredKeyFrozen):
		return err
	default:
		s.suppressAuto(m)
		return nil
	}
}

// sameModel reports whether a and b name one model: the same provider and
// wire model, whatever alias each is reached by (resume's identity, §3.3).
func sameModel(a, b store.Model) bool {
	return a.Provider == b.Provider && a.WireModel == b.WireModel
}

// previousModel is the model a compaction runs on under the previous-model
// rule (plan 028 §3.6, PD13): id's model, reached by an alias of the table
// that still names it — id's own first, then the others in sorted order,
// never one re-pointed at another model (identityAliases) — which the
// session can run: its provider has a key, and it has the session's tool
// profile. It runs at effort when that model offers it, else at its default.
// ok is false when no alias will do.
func (s *Session) previousModel(id store.Model, effort string) (model, bool) {
	s.mu.Lock()
	table := s.table
	s.mu.Unlock()
	for _, alias := range identityAliases(table, id) {
		m, err := s.eligible(table, alias)
		if err != nil {
			continue
		}
		if m, err = withEffort(m, carriedEffort(effort, m.r)); err != nil {
			continue
		}
		return m, true
	}
	return model{}, false
}

// identityAliases are the aliases of table that name id's model — its
// provider and wire model: id's own alias first, when it still does, then
// every other that does, in sorted order. An alias re-pointed at another
// model since is never among them (D-33).
func identityAliases(table *modeltable.Table, id store.Model) []string {
	same := func(alias string) bool {
		m, ok := table.Models[alias]
		return ok && m.Provider == id.Provider && m.WireModel == id.WireModel
	}
	var out []string
	if same(id.Alias) {
		out = append(out, id.Alias)
	}
	for _, alias := range slices.Sorted(maps.Keys(table.Models)) {
		if alias != id.Alias && same(alias) {
			out = append(out, alias)
		}
	}
	return out
}

// warnf hands Options.Warn a line, redacted; nothing without one.
func (s *Session) warnf(format string, args ...any) {
	if s.warn != nil {
		s.warn(s.tools.redactor().String(fmt.Sprintf(format, args...)))
	}
}
