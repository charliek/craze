package agent

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charliek/craze/internal/codexrpc"
)

// codexUsage tracks a Codex session's token usage, context window, and account
// rate-limit state under s.mu (Plan 054 § D4).
type codexUsage struct {
	reported bool
	plan     string
	model    string

	// Rate-limit snapshot merged across sparse account/rateLimits/updated pushes.
	rl *codexrpc.RateLimitsSnapshot

	// Token accounting:
	// - total is the cumulative thread spend reported by thread/tokenUsage/updated.
	// - turnBase is the cumulative spend at the start of the active turn (seeded
	//   from thread/resume's initial tokenUsage so post-resume turns don't double-count).
	// - turn is total - turnBase (clamped >= 0).
	total    Spend
	turnBase Spend
	turn     Spend

	contextTokens int64
	contextWindow int64
}

func codexSpendFromBreakdown(b codexrpc.TokenUsageBreakdown) Spend {
	uncachedInput := max(b.InputTokens-b.CachedInputTokens, 0)
	return Spend{
		Input:         uncachedInput,
		Output:        max(b.OutputTokens, 0),
		Reasoning:     max(b.ReasoningOutputTokens, 0),
		CacheRead:     max(b.CachedInputTokens, 0),
		CacheCreation: max(b.CacheWriteInputTokens, 0),
		Unpriced:      true,
	}
}

func spendSub(cur, base Spend) Spend {
	return Spend{
		Input:         max(cur.Input-base.Input, 0),
		Output:        max(cur.Output-base.Output, 0),
		Reasoning:     max(cur.Reasoning-base.Reasoning, 0),
		CacheRead:     max(cur.CacheRead-base.CacheRead, 0),
		CacheCreation: max(cur.CacheCreation-base.CacheCreation, 0),
		Unpriced:      true,
	}
}

// beginTurn snapshots the current cumulative total as the turn baseline so
// UsageState.Turn reports only this turn's delta.
func (u *codexUsage) beginTurn() {
	u.turnBase = u.total
	u.turn = Spend{Unpriced: true}
}

func (u *codexUsage) applyTokenUsage(tu codexrpc.ThreadTokenUsage, model string, asBaseline bool) {
	u.reported = true
	u.total = codexSpendFromBreakdown(tu.Total)
	if asBaseline {
		u.turnBase = u.total
		u.turn = Spend{Unpriced: true}
	} else {
		u.turn = spendSub(u.total, u.turnBase)
	}
	if tu.Last.TotalTokens > 0 {
		u.contextTokens = tu.Last.TotalTokens
	} else if tu.Last.InputTokens > 0 || tu.Last.OutputTokens > 0 {
		u.contextTokens = tu.Last.InputTokens + tu.Last.OutputTokens
	}
	if tu.ModelContextWindow > 0 {
		u.contextWindow = tu.ModelContextWindow
	}
	if model != "" {
		u.model = model
	}
}

// mergeRateLimits folds a full or sparse RateLimitsSnapshot into u.rl without
// overwriting known non-nil fields with nil.
func (u *codexUsage) mergeRateLimits(in *codexrpc.RateLimitsSnapshot) bool {
	if in == nil {
		return false
	}
	if u.rl == nil {
		cp := cloneCodexRateLimitSnapshot(in)
		u.rl = cp
		if p := strings.TrimSpace(cp.PlanType); p != "" {
			u.plan = sanitizeLine(p)
		}
		u.reported = true
		return true
	}
	changed := false
	if in.LimitID != "" && in.LimitID != u.rl.LimitID {
		u.rl.LimitID = in.LimitID
		changed = true
	}
	if in.LimitName != "" && in.LimitName != u.rl.LimitName {
		u.rl.LimitName = in.LimitName
		changed = true
	}
	if p := strings.TrimSpace(in.PlanType); p != "" && p != u.rl.PlanType {
		u.rl.PlanType = p
		u.plan = sanitizeLine(p)
		changed = true
	}
	if in.Primary != nil && !sameCodexRateWindow(u.rl.Primary, in.Primary) {
		u.rl.Primary = cloneCodexRateWindow(in.Primary)
		changed = true
	}
	if in.Secondary != nil && !sameCodexRateWindow(u.rl.Secondary, in.Secondary) {
		u.rl.Secondary = cloneCodexRateWindow(in.Secondary)
		changed = true
	}
	if in.Credits != nil {
		cp := *in.Credits
		u.rl.Credits = &cp
		changed = true
	}
	if changed {
		u.reported = true
	}
	return changed
}

func cloneCodexRateWindow(w *codexrpc.RateLimitWindow) *codexrpc.RateLimitWindow {
	if w == nil {
		return nil
	}
	cp := *w
	if w.ResetsAt != nil {
		v := *w.ResetsAt
		cp.ResetsAt = &v
	}
	return &cp
}

func cloneCodexRateLimitSnapshot(in *codexrpc.RateLimitsSnapshot) *codexrpc.RateLimitsSnapshot {
	if in == nil {
		return nil
	}
	cp := *in
	cp.Primary = cloneCodexRateWindow(in.Primary)
	cp.Secondary = cloneCodexRateWindow(in.Secondary)
	if in.Credits != nil {
		cr := *in.Credits
		cp.Credits = &cr
	}
	return &cp
}

func sameCodexRateWindow(a, b *codexrpc.RateLimitWindow) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.UsedPercent != b.UsedPercent || a.WindowDurationMins != b.WindowDurationMins {
		return false
	}
	if (a.ResetsAt == nil) != (b.ResetsAt == nil) {
		return false
	}
	if a.ResetsAt != nil && *a.ResetsAt != *b.ResetsAt {
		return false
	}
	return true
}

// codexWindowName maps windowDurationMins to Craze's canonical UsageState.Window
// vocabulary (internal/tui/status.go:195-203 expects "five_hour" / "seven_day").
func codexWindowName(mins int) string {
	if mins <= 0 {
		return ""
	}
	switch mins {
	case 300:
		return "five_hour"
	case 10080:
		return "seven_day"
	default:
		if mins%1440 == 0 {
			return fmt.Sprintf("%dd", mins/1440)
		}
		if mins%60 == 0 {
			return fmt.Sprintf("%dh", mins/60)
		}
		return fmt.Sprintf("%dm", mins)
	}
}

func pickCodexRateWindow(rl *codexrpc.RateLimitsSnapshot) *codexrpc.RateLimitWindow {
	if rl == nil {
		return nil
	}
	switch {
	case rl.Primary != nil && rl.Secondary != nil:
		if rl.Secondary.UsedPercent > rl.Primary.UsedPercent {
			return rl.Secondary
		}
		return rl.Primary
	case rl.Primary != nil:
		return rl.Primary
	default:
		return rl.Secondary
	}
}

func (u *codexUsage) state() UsageState {
	st := UsageState{
		ContextTokens: u.contextTokens,
		ContextWindow: u.contextWindow,
		Turn:          u.turn,
		Session:       u.total,
		Plan:          u.plan,
		Model:         u.model,
	}
	st.Turn.Unpriced = true
	st.Session.Unpriced = true
	if w := pickCodexRateWindow(u.rl); w != nil {
		st.Window = codexWindowName(w.WindowDurationMins)
		util := float64(w.UsedPercent) / 100.0
		if !math.IsNaN(util) && !math.IsInf(util, 0) {
			if util < 0 {
				util = 0
			}
			st.WindowUtilization = util
		}
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			st.ResetsAt = time.Unix(*w.ResetsAt, 0).UTC()
		}
	}
	return st
}
