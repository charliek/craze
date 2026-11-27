package agent

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/charliek/craze/internal/codexrpc"
)

// codexUsage tracks a Codex session's token usage, context window, and account
// rate-limit state under s.mu (Plan 054 § D4).
type codexUsage struct {
	tokensReported bool
	rateReported   bool
	plan           string
	model          string

	// Rate-limit snapshots tracked by limitId so sparse pushes for one quota
	// bucket never inherit or clobber fields from a different bucket.
	buckets     map[string]*codexrpc.RateLimitsSnapshot
	bucketOrder []string

	// Token accounting:
	// - total is the cumulative thread spend reported by thread/tokenUsage/updated.
	// - turnBase is the cumulative spend at the start of the active turn (seeded
	//   from historical tokenUsage on resume so post-resume turns don't double-count).
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

func (u *codexUsage) hasAny() bool {
	return u.tokensReported || u.rateReported || u.plan != ""
}

// beginTurn snapshots the current cumulative total as the turn baseline so
// UsageState.Turn reports only this turn's delta.
func (u *codexUsage) beginTurn() {
	u.turnBase = u.total
	u.turn = Spend{Unpriced: true}
}

func (u *codexUsage) applyTokenUsage(tu codexrpc.ThreadTokenUsage, model string, asBaseline bool) {
	u.tokensReported = true
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

// mergeRateLimitsRead seeds buckets from rateLimitsByLimitId when present,
// falling back to the single-bucket rateLimits field.
func (u *codexUsage) mergeRateLimitsRead(res *codexrpc.RateLimitsReadResult) bool {
	if res == nil {
		return false
	}
	changed := false
	if len(res.RateLimitsByLimitID) > 0 {
		keys := make([]string, 0, len(res.RateLimitsByLimitID))
		for k := range res.RateLimitsByLimitID {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			snap := res.RateLimitsByLimitID[k]
			if snap == nil {
				continue
			}
			cp := *snap
			if cp.LimitID == "" {
				cp.LimitID = k
			}
			if u.mergeRateLimits(&cp) {
				changed = true
			}
		}
		return changed
	}
	if res.RateLimits != nil {
		return u.mergeRateLimits(res.RateLimits)
	}
	return false
}

// mergeRateLimits folds a full or sparse RateLimitsSnapshot into its bucket
// (keyed by limitId), merging nested fields without overwriting known non-nil
// values with nil.
func (u *codexUsage) mergeRateLimits(in *codexrpc.RateLimitsSnapshot) bool {
	if in == nil {
		return false
	}
	if u.buckets == nil {
		u.buckets = make(map[string]*codexrpc.RateLimitsSnapshot)
	}
	key := strings.TrimSpace(in.LimitID)
	cur, ok := u.buckets[key]
	if !ok {
		cp := cloneCodexRateLimitSnapshot(in)
		cp.LimitID = key
		u.buckets[key] = cp
		u.bucketOrder = append(u.bucketOrder, key)
		if p := strings.TrimSpace(cp.PlanType); p != "" {
			u.plan = sanitizeLine(p)
		}
		u.rateReported = true
		return true
	}
	changed := false
	if in.LimitName != "" && in.LimitName != cur.LimitName {
		cur.LimitName = in.LimitName
		changed = true
	}
	if p := strings.TrimSpace(in.PlanType); p != "" && p != cur.PlanType {
		cur.PlanType = p
		u.plan = sanitizeLine(p)
		changed = true
	}
	if in.Primary != nil {
		if cur.Primary == nil {
			cur.Primary = cloneCodexRateWindow(in.Primary)
			changed = true
		} else if mergeCodexRateWindow(cur.Primary, in.Primary) {
			changed = true
		}
	}
	if in.Secondary != nil {
		if cur.Secondary == nil {
			cur.Secondary = cloneCodexRateWindow(in.Secondary)
			changed = true
		} else if mergeCodexRateWindow(cur.Secondary, in.Secondary) {
			changed = true
		}
	}
	if in.Credits != nil {
		if cur.Credits == nil || *cur.Credits != *in.Credits {
			cp := *in.Credits
			cur.Credits = &cp
			changed = true
		}
	}
	if in.SpendControlReached != nil {
		if cur.SpendControlReached == nil || *cur.SpendControlReached != *in.SpendControlReached {
			v := *in.SpendControlReached
			cur.SpendControlReached = &v
			changed = true
		}
	}
	if changed {
		u.rateReported = true
	}
	return changed
}

func mergeCodexRateWindow(dst, src *codexrpc.RateLimitWindow) bool {
	if dst == nil || src == nil {
		return false
	}
	changed := false
	if src.UsedPercent != nil && (dst.UsedPercent == nil || *dst.UsedPercent != *src.UsedPercent) {
		v := *src.UsedPercent
		dst.UsedPercent = &v
		changed = true
	}
	if src.WindowDurationMins != nil && (dst.WindowDurationMins == nil || *dst.WindowDurationMins != *src.WindowDurationMins) {
		v := *src.WindowDurationMins
		dst.WindowDurationMins = &v
		changed = true
	}
	if src.ResetsAt != nil && (dst.ResetsAt == nil || *dst.ResetsAt != *src.ResetsAt) {
		v := *src.ResetsAt
		dst.ResetsAt = &v
		changed = true
	}
	return changed
}

func cloneCodexRateWindow(w *codexrpc.RateLimitWindow) *codexrpc.RateLimitWindow {
	if w == nil {
		return nil
	}
	cp := *w
	if w.UsedPercent != nil {
		v := *w.UsedPercent
		cp.UsedPercent = &v
	}
	if w.WindowDurationMins != nil {
		v := *w.WindowDurationMins
		cp.WindowDurationMins = &v
	}
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
	if in.SpendControlReached != nil {
		v := *in.SpendControlReached
		cp.SpendControlReached = &v
	}
	return &cp
}

// codexWindowName maps windowDurationMins to Craze's canonical UsageState.Window
// vocabulary (internal/tui/status.go:195-203 expects "five_hour" / "seven_day").
func codexWindowName(mins *int) string {
	if mins == nil || *mins <= 0 {
		return ""
	}
	switch *mins {
	case 300:
		return "five_hour"
	case 10080:
		return "seven_day"
	default:
		if *mins%1440 == 0 {
			return fmt.Sprintf("%dd", *mins/1440)
		}
		if *mins%60 == 0 {
			return fmt.Sprintf("%dh", *mins/60)
		}
		return fmt.Sprintf("%dm", *mins)
	}
}

func codexWindowPercent(w *codexrpc.RateLimitWindow) float64 {
	if w == nil || w.UsedPercent == nil {
		return -1
	}
	return *w.UsedPercent
}

func (u *codexUsage) pickRateWindow(now time.Time) *codexrpc.RateLimitWindow {
	var bestActive *codexrpc.RateLimitWindow
	bestActivePct := -1.0
	consider := func(w *codexrpc.RateLimitWindow) {
		if w == nil {
			return
		}
		// Skip windows whose reset time has already passed so a stale 100%
		// expired bucket never masks a fresh active bucket.
		if w.ResetsAt != nil && *w.ResetsAt > 0 && !time.Unix(*w.ResetsAt, 0).After(now) {
			return
		}
		pct := codexWindowPercent(w)
		if bestActive == nil || pct > bestActivePct {
			bestActive = w
			bestActivePct = pct
		}
	}
	for _, key := range u.bucketOrder {
		rl := u.buckets[key]
		if rl == nil {
			continue
		}
		consider(rl.Primary)
		consider(rl.Secondary)
	}
	return bestActive
}

func (u *codexUsage) state() UsageState {
	return u.stateAt(time.Now())
}

func (u *codexUsage) stateAt(now time.Time) UsageState {
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
	if w := u.pickRateWindow(now); w != nil {
		st.Window = codexWindowName(w.WindowDurationMins)
		if w.UsedPercent != nil {
			util := *w.UsedPercent / 100.0
			if !math.IsNaN(util) && !math.IsInf(util, 0) {
				if util < 0 {
					util = 0
				}
				st.WindowUtilization = util
			}
		}
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			st.ResetsAt = time.Unix(*w.ResetsAt, 0).UTC()
		}
	}
	return st
}
