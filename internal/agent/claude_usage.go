package agent

import (
	"encoding/json"
	"strings"

	"github.com/charliek/craze/internal/acp"
)

// claude's usage section (plan 052 § D1, § D2): the context meter from each
// root usage_update's used and size, the turn's and the session's tokens from
// each prompt result's `_meta.quota` — unpriced, never dollars: the adapter's
// cost is an API-equivalent estimate and the plan is what pays — and the
// plan's limit from the latest `_claude/rateLimit`, which the adapter sends
// only when it changes, so it is kept until the next.

// updateUsageKind is the session/update kind of a usage report.
const updateUsageKind = "usage_update"

// claudeUsage is a claude session's usage, under s.mu.
type claudeUsage struct {
	// reported: something has been reported, so the section is published.
	reported bool
	// plan is the subscription the preflight read ("max").
	plan string
	// rl is the latest rate limit the adapter sent, nil before the first.
	rl *acp.RateLimit
	// used and size are the last root usage_update's; model is the model it
	// named (`_claude/model`).
	used, size int64
	model      string
	// underMode and underModel are the mode and model option the in-use
	// model was reported under (L3 F1): once either changes, the report says
	// nothing of the model the session now runs, and model is cleared.
	underMode, underModel string
	// turn is the last prompted turn's tokens, session the sum of every
	// prompted turn's this incarnation.
	turn, session Spend
}

// state is u as the session's usage section.
func (u *claudeUsage) state() UsageState {
	st := UsageState{
		ContextTokens: u.used,
		ContextWindow: u.size,
		Turn:          u.turn,
		Session:       u.session,
		Plan:          u.plan,
		Model:         u.model,
	}
	// Never priced on a plan: the adapter's cost is what the API would have
	// charged, which is not what the plan pays.
	st.Turn.Unpriced, st.Session.Unpriced = true, true
	if rl := u.rl; rl != nil {
		st.Window = rl.RateLimitType
		if rl.Utilization != nil {
			st.WindowUtilization = *rl.Utilization
		}
		st.ResetsAt = epochTime(rl.ResetsAt)
	}
	return st
}

// publishUsageLocked publishes the usage section as it now stands; s.mu is
// held. A load's replay publishes nothing: what it reports was spent before.
func (s *session) publishUsageLocked() {
	if s.replaying.Load() {
		return
	}
	s.usage.reported = true
	u := s.usage.state()
	s.snap.Usage = &u
	d := u
	s.enqueueDeltaLocked("", Event{}, &StateDelta{Usage: &d})
}

// noteRateLimit keeps the adapter's latest rate limit and publishes it (§ D2).
func (s *session) noteRateLimit(rl acp.RateLimit) {
	if !s.provider().isClaude() {
		return
	}
	s.mu.Lock()
	if prev := s.usage.rl; prev != nil && sameRateLimit(*prev, rl) {
		s.mu.Unlock()
		return
	}
	cp := rl
	if rl.Utilization != nil {
		v := *rl.Utilization
		cp.Utilization = &v
	}
	s.usage.rl = &cp
	s.publishUsageLocked()
	s.mu.Unlock()
	// Enqueued, not flushed (review C4d finding 1): this runs on the read
	// loop, under the dispatch, and a flush would hold the dispatch for as
	// long as a reader stalls; the outbox keeps the delta's order.
}

func sameRateLimit(a, b acp.RateLimit) bool {
	au, bu := a.Utilization, b.Utilization
	a.Utilization, b.Utilization = nil, nil
	if a != b {
		return false
	}
	if au == nil || bu == nil {
		return au == bu
	}
	return *au == *bu
}

// onClaudeUsage is a root usage_update (§ D1): the context meter and the
// model in use. The cost it may carry is not read.
func (s *session) onClaudeUsage(raw json.RawMessage) {
	if !s.provider().isClaude() || s.replaying.Load() {
		return
	}
	var w struct {
		Used *int64 `json:"used"`
		Size *int64 `json:"size"`
		Meta struct {
			Model string `json:"_claude/model"`
		} `json:"_meta"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return
	}
	s.mu.Lock()
	before := s.usage
	if w.Used != nil && *w.Used >= 0 {
		s.usage.used = *w.Used
	}
	if w.Size != nil && *w.Size >= 0 {
		s.usage.size = *w.Size
	}
	if m := sanitizeLine(strings.TrimSpace(w.Meta.Model)); m != "" {
		s.usage.model = m
		s.usage.underMode, s.usage.underModel = s.claudeOptionsLocked()
	}
	if before.reported && before.used == s.usage.used && before.size == s.usage.size && before.model == s.usage.model {
		s.mu.Unlock()
		return
	}
	s.publishUsageLocked()
	s.mu.Unlock()
	// Enqueued, not flushed: the read loop's, as noteRateLimit's.
}

// claudeTurnUsage is a prompted turn's tokens, from its result's
// `_meta.quota.token_count` (§ D1): the turn's, and added to the session's.
func (s *session) claudeTurnUsage(res *acp.PromptResult) {
	if res == nil || !s.provider().isClaude() {
		return
	}
	q, ok := res.Quota()
	if !ok {
		return
	}
	turn := Spend{
		Input:         max(q.InputTokens, 0),
		Output:        max(q.OutputTokens, 0),
		Reasoning:     max(q.ReasoningOutputTokens, 0),
		CacheRead:     max(q.CachedInputTokens, 0),
		CacheCreation: max(q.CachedWriteTokens, 0),
	}
	s.mu.Lock()
	s.usage.turn = turn
	s.usage.session.Input += turn.Input
	s.usage.session.Output += turn.Output
	s.usage.session.Reasoning += turn.Reasoning
	s.usage.session.CacheRead += turn.CacheRead
	s.usage.session.CacheCreation += turn.CacheCreation
	s.publishUsageLocked()
	s.mu.Unlock()
	s.flushDelta()
}

// claudeFamilies are the model families a claude model id or name is read as.
var claudeFamilies = []string{"opus", "sonnet", "haiku", "fable"}

// ClaudeModelFamily is the family a claude model id or display name names —
// opus, sonnet, haiku or fable — "" for none (the adapter's "default").
func ClaudeModelFamily(s string) string {
	low := strings.ToLower(s)
	for _, f := range claudeFamilies {
		if strings.Contains(low, f) {
			return f
		}
	}
	return ""
}

// claudeOptionsLocked is the session's mode and model as their options say
// now; s.mu is held.
func (s *session) claudeOptionsLocked() (mode, model string) {
	mode = claudeEffectiveMode(s.snap.Config, s.snap.CurrentMode)
	model = s.snap.CurrentModel
	if opt := ModelConfigOptionIn(s.snap.Config); opt != nil && opt.Current != "" {
		model = opt.Current
	}
	return mode, model
}

// inUseCheckLocked clears the model in use once the mode or the model option
// has changed since it was reported (plan 052 L3 F1): entering plan mode, the
// adapter moves the model option to Sonnet and the last report's model says
// nothing of the next turn; a report under the new options says again. The
// usage section is published when it changes. s.mu is held.
func (s *session) inUseCheckLocked() {
	if !s.provider().isClaude() || s.usage.model == "" {
		return
	}
	if mode, model := s.claudeOptionsLocked(); mode == s.usage.underMode && model == s.usage.underModel {
		return
	}
	s.usage.model = ""
	if s.usage.reported {
		s.publishUsageLocked()
	}
}
