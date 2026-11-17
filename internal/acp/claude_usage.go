package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

// The claude dialect's usage and the overage guard's wire half (plan 052 §
// D1, § D3). claude-agent-acp reports the account's plan limits as
// `_meta["_claude/rateLimit"]` on a usage_update — only when they change
// (v1/acp-agent.js:5018-5036, a rate_limit_event riding the next usage) — and
// as `data.rateLimit` on a prompt's rate_limit failure (result-end.js
// turnErrorData). The guard is containment, not prevention: what trips it has
// already been billed, so the client ends whatever turn runs, stops sending,
// and stops the agent's process group.

// RateLimit is the adapter's `_claude/rateLimit`: the Agent SDK's
// SDKRateLimitInfo (sdk.d.ts), the fields craze reads. The times are epoch
// seconds, 0 for none.
type RateLimit struct {
	// Status is allowed, allowed_warning or rejected.
	Status string `json:"status"`
	// ResetsAt is when the limit RateLimitType names resets.
	ResetsAt float64 `json:"resetsAt"`
	// RateLimitType names the window: five_hour, seven_day,
	// seven_day_opus, seven_day_sonnet, seven_day_overage_included or
	// overage.
	RateLimitType string `json:"rateLimitType"`
	// Utilization is the window's use, 0..1; nil when the adapter sent none.
	Utilization *float64 `json:"utilization"`
	// OverageStatus is extra usage's own status, OverageResetsAt its reset.
	OverageStatus   string  `json:"overageStatus"`
	OverageResetsAt float64 `json:"overageResetsAt"`
	// IsUsingOverage and OverageInUse say a request was billed as extra
	// usage.
	IsUsingOverage bool `json:"isUsingOverage"`
	OverageInUse   bool `json:"overageInUse"`
}

// Trips reports whether r is one of the overage guard's four signals (plan
// 052 § D3): extra usage in use, said either way; a window that is extra
// usage itself; or the plan's limit rejected while extra usage is allowed —
// the next request would be billed as extra usage, so this one trips before
// it. The reading of extra usage as allowed or in use is t3code's
// (apps/server/src/orchestration-v2/Adapters/ClaudeAdapterV2.ts, its
// overageAllowed, at 26f70e72fa), which treats a rejected limit with extra
// usage allowed as not blocked: craze trips on it instead.
func (r RateLimit) Trips() bool {
	switch {
	case r.IsUsingOverage, r.OverageInUse:
		return true
	case r.RateLimitType == "overage", r.RateLimitType == "seven_day_overage_included":
		return true
	case r.Status == "rejected" && (r.OverageStatus == "allowed" || r.OverageStatus == "allowed_warning"):
		return true
	}
	return false
}

// UsageRateLimit is a usage_update's `_meta["_claude/rateLimit"]`, and
// whether it carried one that reads as an object.
func UsageRateLimit(update json.RawMessage) (RateLimit, bool) {
	var w struct {
		Meta struct {
			RateLimit json.RawMessage `json:"_claude/rateLimit"`
		} `json:"_meta"`
	}
	if json.Unmarshal(update, &w) != nil {
		return RateLimit{}, false
	}
	return decodeRateLimit(w.Meta.RateLimit)
}

// ErrorRateLimit is a prompt error's `data.rateLimit`, and whether it carried
// one that reads as an object.
func ErrorRateLimit(data json.RawMessage) (RateLimit, bool) {
	raw := bytes.TrimSpace(data)
	if len(raw) == 0 || raw[0] != '{' {
		return RateLimit{}, false
	}
	var w struct {
		RateLimit json.RawMessage `json:"rateLimit"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return RateLimit{}, false
	}
	return decodeRateLimit(w.RateLimit)
}

func decodeRateLimit(raw json.RawMessage) (RateLimit, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return RateLimit{}, false
	}
	var r RateLimit
	if json.Unmarshal(raw, &r) != nil {
		return RateLimit{}, false
	}
	return r, true
}

// SetRateLimitHandler installs the claude rate-limit handler (plan 052 § D2,
// § D3). It runs on the read loop, in dispatch order, for every usage_update
// that carries `_claude/rateLimit` — the root's, a child's, before the session
// has returned or between turns alike; never one a session/load replays — and
// BEFORE the foreign-turn state machine reads that update, so a trip is
// processed before the same update could settle a turn. A non-nil answer is
// the error the trip ends the running turn with: a foreign turn ends with it
// at once, failed, and the machine is closed, so nothing opens again.
func (c *Client) SetRateLimitHandler(h func(RateLimit) error) {
	c.mu.Lock()
	c.rateLimitHandler = h
	c.mu.Unlock()
}

// noteRateLimit is routeClaude's first step for one update: a usage_update's
// rate limit is handed to the handler, and a trip it answers ends the foreign
// turn now. The caller holds dispatchMu and not c.mu.
func (c *Client) noteRateLimit(kind string, n SessionNotification) {
	if kind != updateUsage {
		return
	}
	rl, ok := UsageRateLimit(n.Update)
	if !ok {
		return
	}
	c.mu.Lock()
	// A replay is history: what it says of a limit was said, and acted on,
	// when it was live. Live updates held behind the load's reply are read
	// when ReplayDone routes them, and what came before session/new's reply
	// when claudeSetup does. (An update for a child not yet announced is read
	// now, and again if the early buffer replays it: the handler is
	// idempotent, and a child's report that expired there is still read.)
	later := c.sessionID == "" || c.cf.phase == phaseReplaying
	h := c.rateLimitHandler
	c.mu.Unlock()
	if later || h == nil {
		return
	}
	if err := h(rl); err != nil {
		c.endForeignForTrip(err, true)
	}
}

// EndForeignWith is the overage guard's end of a claude foreign turn from
// outside the read loop's dispatch (plan 052 § D3): one opened while the trip
// record holds, a trip a prompt's error reported. The turn open now, if any,
// ends failed with err, decided now, and the machine is closed, so nothing
// opens again; the ending is delivered in dispatch order on a goroutine of its
// own, so a caller already on the read loop — a foreign-turn handler — never
// waits on itself.
func (c *Client) EndForeignWith(err error) {
	if c.dialect != DialectClaude {
		return
	}
	c.endForeignForTrip(err, false)
}

// endForeignForTrip ends the open foreign turn with err and closes the
// machine. dispatching says the caller holds dispatchMu: the ending is then
// delivered inline, in order; otherwise on a goroutine that takes it.
func (c *Client) endForeignForTrip(err error, dispatching bool) {
	c.mu.Lock()
	if c.cf.failErr == nil {
		c.cf.failErr = err
	}
	var end *ForeignTurn
	if c.cf.phase == phaseForeign {
		end = c.endForeignTurnLocked(false, err)
	}
	c.closeMachineLocked()
	h := c.foreignHandler
	c.mu.Unlock()
	if end == nil || h == nil {
		return
	}
	if dispatching {
		h(*end)
		return
	}
	go func() {
		locked := c.lockDispatchWithin(closeWait())
		defer func() {
			if locked {
				c.dispatchMu.Unlock()
			}
		}()
		h(*end)
	}()
}

// Fence is the overage guard's first step on the wire (plan 052 § D3): from
// here nothing but a session/cancel is written to the agent — no prompt, no
// steer, no answer, no setting — whoever is about to write it. It only sets a
// flag, so it is safe under any lock and on the read loop.
func (c *Client) Fence() { c.conn.enc.fence() }

// StopWithin is the overage guard's stop (plan 052 § D3): session/cancel to
// the root, then, once grace has passed or the agent has exited on its own,
// nothing more read from the agent — its frames are dropped unread from here
// (Conn.sever) — and its process group killed outright, which takes Claude
// Code and every child it runs with it; then the client closes. A child that
// ignores the cancel cannot outlast it. It blocks until the client is closed;
// callers run it on a goroutine of their own.
func (c *Client) StopWithin(grace time.Duration) {
	if sid := c.SessionID(); sid != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), grace)
			defer cancel()
			_ = c.conn.Notify(ctx, MethodSessionCancel, CancelParams{SessionID: sid})
		}()
	}
	timer := time.NewTimer(grace)
	select {
	case <-timer.C:
	case <-c.Exited():
	}
	timer.Stop()
	c.conn.sever()
	if testAfterSever != nil {
		testAfterSever()
	}
	if c.child != nil {
		c.child.Kill()
	}
	_ = c.Close()
}

// testAfterSever is a test seam, nil in production: it runs in StopWithin
// right after the connection is severed, before the kill.
var testAfterSever func()

// PromptQuota is a claude prompt result's `_meta.quota.token_count`: the
// turn's own tokens as the adapter counts them, and whether it carried them.
type PromptQuota struct {
	TotalTokens, InputTokens, CachedInputTokens, CachedWriteTokens, OutputTokens, ReasoningOutputTokens int64
}

// Quota is r's `_meta.quota.token_count` (claude), and whether there was one.
func (r PromptResult) Quota() (PromptQuota, bool) {
	raw := bytes.TrimSpace(r.Meta)
	if len(raw) == 0 || raw[0] != '{' {
		return PromptQuota{}, false
	}
	var w struct {
		Quota struct {
			TokenCount *PromptQuota `json:"token_count"`
		} `json:"quota"`
	}
	if json.Unmarshal(raw, &w) != nil || w.Quota.TokenCount == nil {
		return PromptQuota{}, false
	}
	return *w.Quota.TokenCount, true
}
