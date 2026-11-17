package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The claude usage, overage and error scripts (craze plan 052 C5), shaped
// after the adapter's real frames (verify/L0 probe2): a usage_update carries
// used, size and `_meta["_claude/model"]`, and the first of a turn the
// account's `_claude/rateLimit` when it changed (v1/acp-agent.js:5018-5036);
// the result's usage carries cost and `_claude/origin`; the prompt's reply
// carries `_meta.quota.token_count`; a failed turn is -32603 with
// data.errorKind, and data.rateLimit for a rate_limit (result-end.js).
//
//	claude-usage     a prompt streams two usage reports — the first with the
//	                 plan's rate limit (seven_day, 45%, allowed_warning,
//	                 extra usage rejected: no trip) — and replies with its
//	                 quota. PLAN-SONNET: the reports name claude-sonnet-5-5
//	                 whatever model is picked (plan mode, L2). NOTICE: a
//	                 notice. COMPACT: a compaction, in_progress then completed
//	                 with a summary chunk between.
//	                 CRAZE_FAKE_CLAUDE_SETUP_USAGE (set): a usage report on
//	                 Sonnet before session/new's reply.
//	claude-overage   a prompt names where the trip lands; the signal is
//	                 CRAZE_FAKE_CLAUDE_OVERAGE (below). Directives:
//	                   MID       text, the tripping usage, then work until
//	                             cancelled, the reply cancelled
//	                   BEFORE    the tripping usage before any output, then
//	                             work until cancelled
//	                   TERMINAL  text, then the result's usage carrying cost
//	                             and the trip, and the reply end_turn at once
//	                   BG        the reply end_turn at once; then a wake whose
//	                             result usage trips, which goes on ticking
//	                             whatever it is told
//	                   TRAILING  the reply end_turn at once; then, after
//	                             CRAZE_FAKE_CLAUDE_GAP, the tripping usage —
//	                             a trailing report landing in the next turn
//	                   HOLD      work until cancelled
//	                   STDERR    a line on stderr, then the trip
//	                   STDERR-FLOOD  600 lines on stderr, then work until
//	                             cancelled
//	                   CONFIG-TRIP  a config and a commands update, then the
//	                             trip
//	                   FLOOD-TRIP  600 text chunks, then the trip
//	                   FLOOD-ONLY  2000 text chunks, then work until
//	                             cancelled
//	                   TWO-TRIPS a trip with no reset time, then one a week
//	                             away inside the stop's grace
//	                   ESCAPE    two real children, one in the adapter's
//	                             group and one in a session of its own
//	                             (CRAZE_FAKE_CHILD_PIDS), then the trip
//	                   CHILD     a child that ticks on its own session every
//	                             10 ms and ignores every cancel; the root's
//	                             tripping usage, then the root settling at
//	                             once on its cancel
//	                 The script ignores SIGTERM: only SIGKILL ends it at once.
//	                 CRAZE_FAKE_CLAUDE_IDLE_TRIP (a FIFO's path): once a byte
//	                 arrives on it, the tripping usage with nothing running —
//	                 the test sends it when its Start is done, so the trip
//	                 never lands in the start however slow that is.
//	                 CRAZE_FAKE_CLAUDE_IDLE_WAKE (a duration): that long after
//	                 session/new, a wake that does not trip.
//	                 CRAZE_FAKE_CLAUDE_IDLE_LIMIT (a duration): that long after
//	                 session/new, a changed plan limit that does not trip.
//	                 CRAZE_FAKE_CLAUDE_SETUP_TRIP (set): the trip before
//	                 session/new's reply, and the reply an error. A load
//	                 (loadSession true) replays a turn and answers.
//	claude-errors    `ERR <errorKind>` fails the prompt -32603 with that
//	                 data.errorKind; `ERR rate_limit rejected` adds a rejected
//	                 rate limit with extra usage off, `ERR rate_limit TRIP` the
//	                 CRAZE_FAKE_CLAUDE_OVERAGE signal's; `ERR invalid_request
//	                 TOOLONG` says the prompt is too long; AUTH-REQUIRED fails
//	                 it -32000.
//
// CRAZE_FAKE_CLAUDE_OVERAGE is the trip signal: using (isUsingOverage, the
// default), inuse (overageInUse), overage (rateLimitType overage), included
// (rateLimitType seven_day_overage_included) or rejected (status rejected
// with extra usage allowed_warning). CRAZE_FAKE_CLAUDE_RESETS_IN (a duration,
// default 1h) is how far from now its resetsAt is.

// claudeModel is the model the scripts' usage reports name.
const claudeModel = "claude-haiku-5-5"

// planRateLimit is a real account's report (probe2): the seven-day window
// at 45%, extra usage rejected — no signal.
func planRateLimit() map[string]any {
	return map[string]any{"status": "allowed_warning", "resetsAt": time.Now().Add(72 * time.Hour).Unix(), "rateLimitType": "seven_day",
		"utilization": 0.45, "overageStatus": "rejected", "overageDisabledReason": "org_level_disabled", "isUsingOverage": false,
		"unifiedWindows": map[string]any{"seven_day": map[string]any{"utilization": 0.45, "status": "allowed"}}, "overageEnabled": false}
}

// tripRateLimit is CRAZE_FAKE_CLAUDE_OVERAGE's signal.
func tripRateLimit() map[string]any {
	resets := time.Now().Add(envDuration("CRAZE_FAKE_CLAUDE_RESETS_IN", time.Hour)).Unix()
	rl := map[string]any{"status": "allowed", "resetsAt": resets, "rateLimitType": "seven_day", "utilization": 1.0,
		"overageStatus": "allowed", "isUsingOverage": false, "overageEnabled": true}
	switch envOr("CRAZE_FAKE_CLAUDE_OVERAGE", "using") {
	case "inuse":
		rl["overageInUse"] = true
	case "overage":
		rl["rateLimitType"] = "overage"
	case "included":
		rl["rateLimitType"] = "seven_day_overage_included"
	case "rejected":
		rl["status"], rl["overageStatus"] = "rejected", "allowed_warning"
	default:
		rl["isUsingOverage"] = true
	}
	return rl
}

// usageReport is a usage_update; rl, when set, rides as `_claude/rateLimit`,
// and cost makes it the result's usage.
func (s *server) usageReport(sid string, used int, model string, rl map[string]any, cost bool) {
	meta := map[string]any{"_claude/model": model}
	if rl != nil {
		meta["_claude/rateLimit"] = rl
	}
	u := map[string]any{"sessionUpdate": "usage_update", "used": used, "size": 1000000, "_meta": meta}
	if cost {
		u["cost"] = map[string]any{"amount": 0.0067, "currency": "USD"}
		meta["_claude/origin"] = map[string]any{"kind": "human"}
	}
	s.update(sid, u)
}

// quotaResult is a prompt's reply with its `_meta.quota` (probe2's write step).
func quotaResult(stop string) map[string]any {
	tc := map[string]any{"totalTokens": 59586, "inputTokens": 4, "cachedInputTokens": 28511, "cachedWriteTokens": 30907,
		"outputTokens": 164, "reasoningOutputTokens": 0}
	return map[string]any{"stopReason": stop,
		"usage": map[string]any{"inputTokens": 4, "outputTokens": 164, "cachedReadTokens": 28511, "cachedWriteTokens": 30907, "totalTokens": 59586},
		"_meta": map[string]any{"quota": map[string]any{"token_count": tc,
			"model_usage": []any{map[string]any{"model": claudeModel, "token_count": tc}}}}}
}

// claudeUsageTurn is the usage, overage and error scripts' prompt; false
// leaves it to the rest of claudePrompt.
func (s *server) claudeUsageTurn(id json.RawMessage, sid, text string) bool {
	switch s.script {
	case "claude-usage":
		s.usageTurn(id, sid, text)
	case "claude-overage":
		s.overageTurn(id, sid, text)
	case "claude-errors":
		s.errorTurn(id, text)
	default:
		return false
	}
	return true
}

func (s *server) usageTurn(id json.RawMessage, sid, text string) {
	model := claudeModel
	if strings.Contains(text, "PLAN-SONNET") {
		model = "claude-sonnet-5-5"
	}
	if strings.Contains(text, "NOTICE") {
		s.update(sid, map[string]any{"sessionUpdate": "notice", "severity": "warning", "title": "Stop hook says", "description": "remember the tests"})
	}
	if strings.Contains(text, "COMPACT") {
		s.update(sid, map[string]any{"sessionUpdate": "compaction_update", "compactionId": "cmp-1", "status": "in_progress"})
		s.update(sid, map[string]any{"sessionUpdate": "compaction_summary_chunk", "compactionId": "cmp-1", "content": map[string]string{"type": "text", "text": "the summary"}})
		s.update(sid, map[string]any{"sessionUpdate": "compaction_update", "compactionId": "cmp-1", "status": "completed"})
	}
	s.usageReport(sid, 28529, model, planRateLimit(), false)
	s.claudeSay(sid, "counted")
	s.usageReport(sid, 30938, model, nil, true)
	s.reply(id, quotaResult(acp.StopEndTurn))
}

// overageTurn is claude-overage's prompt: the trip where the text says.
func (s *server) overageTurn(id json.RawMessage, sid, text string) {
	trip := func() { s.usageReport(sid, 30000, claudeModel, tripRateLimit(), false) }
	switch {
	case strings.Contains(text, "MID"):
		s.claudeSay(sid, "before the trip")
		trip()
		s.claudeSay(sid, "after the trip")
		s.hang(id)
	case strings.Contains(text, "BEFORE"):
		trip()
		s.hang(id)
	case strings.Contains(text, "TERMINAL"):
		s.claudeSay(sid, "all done")
		s.usageReport(sid, 30000, claudeModel, tripRateLimit(), true)
		s.reply(id, quotaResult(acp.StopEndTurn))
	case strings.Contains(text, "BG"):
		s.endTurnSaying(id, sid, "started")
		time.Sleep(envDuration("CRAZE_FAKE_CLAUDE_WAKE_DELAY", 100*time.Millisecond))
		s.usageReport(sid, 30000, claudeModel, nil, false)
		s.claudeSay(sid, "waking")
		s.usageReport(sid, 30100, claudeModel, tripRateLimit(), true)
		s.tickForever(sid, "wake")
	case strings.Contains(text, "TRAILING"):
		s.endTurnSaying(id, sid, "done")
		go func() {
			time.Sleep(envDuration("CRAZE_FAKE_CLAUDE_GAP", 50*time.Millisecond))
			trip()
		}()
	case strings.Contains(text, "HOLD"):
		s.hang(id)
	case strings.Contains(text, "FLOOD-ONLY"):
		// More text than a reader that is not reading has room for, and no
		// trip: work until cancelled.
		for i := range 2000 {
			s.claudeSay(sid, fmt.Sprintf("chunk %d ", i))
		}
		s.hang(id)
	case strings.Contains(text, "FLOOD-TRIP"):
		// More text than a reader that is not reading has room for, then
		// the trip behind it.
		for i := range 600 {
			s.claudeSay(sid, fmt.Sprintf("chunk %d ", i))
		}
		trip()
		s.hang(id)
	case strings.Contains(text, "CONFIG-TRIP"):
		// A config update and a commands update the read loop publishes,
		// then the trip behind them.
		s.update(sid, map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": s.claude.catalog()})
		s.update(sid, map[string]any{"sessionUpdate": acp.UpdateAvailableCommands, "availableCommands": []any{}})
		trip()
		s.hang(id)
	case strings.Contains(text, "STDERR-FLOOD"):
		// 600 lines on stderr, a millisecond apart, then work until
		// cancelled.
		for i := range 600 {
			fmt.Fprintf(os.Stderr, "claude-agent-acp: line %d\n", i)
			time.Sleep(time.Millisecond)
		}
		s.hang(id)
	case strings.Contains(text, "STDERR"):
		// A diagnostic on the adapter's stderr, then the trip.
		fmt.Fprintln(os.Stderr, "claude-agent-acp: something to say")
		trip()
		s.hang(id)
	case strings.Contains(text, "TWO-TRIPS"):
		// A first trip with no reset time, then, inside the stop's grace, a
		// second whose reset is a week away.
		first := tripRateLimit()
		delete(first, "resetsAt")
		s.usageReport(sid, 30000, claudeModel, first, false)
		time.Sleep(50 * time.Millisecond)
		second := tripRateLimit()
		second["resetsAt"] = time.Now().Add(7 * 24 * time.Hour).Unix()
		s.usageReport(sid, 30100, claudeModel, second, false)
		s.hang(id)
	case strings.Contains(text, "ESCAPE"):
		// Two real OS children: one in the adapter's own process group, one
		// in a session of its own (setsid) — a background command that left
		// the group — each pid written to CRAZE_FAKE_CHILD_PIDS; then the trip.
		s.spawnChildren()
		trip()
		s.hang(id)
	case strings.Contains(text, "CHILD"):
		s.announce(sid, fakeChild, "Stubborn", "never stop", "toolu_launch_1")
		go s.tickForever(fakeChild, "tick")
		time.Sleep(30 * time.Millisecond)
		trip()
		// The root settles at once on its cancel; the child does not.
		s.hang(id)
	default:
		s.endTurnSaying(id, sid, "echo: "+text)
	}
}

// tickForever sends "<what> N" on sid every 10 ms until the process ends,
// whatever it is told.
func (s *server) tickForever(sid, what string) {
	for n := 1; ; n++ {
		s.claudeSay(sid, what+" "+strconv.Itoa(n))
		time.Sleep(10 * time.Millisecond)
	}
}

// claudeOverageSetup is claude-overage's traffic after session/new's reply:
// an idle trip or an idle wake, when asked for. The script ignores SIGTERM,
// as a Claude Code busy with a request might: only a kill ends it at once.
func (s *server) claudeOverageSetup(sid string) {
	signal.Ignore(syscall.SIGTERM)
	if gate := os.Getenv("CRAZE_FAKE_CLAUDE_IDLE_TRIP"); gate != "" {
		go func() {
			awaitSetGate(gate)
			s.usageReport(sid, 30000, claudeModel, tripRateLimit(), false)
		}()
	}
	if d := envDuration("CRAZE_FAKE_CLAUDE_IDLE_LIMIT", 0); d > 0 {
		// A changed plan limit with nothing running.
		go func() {
			time.Sleep(d)
			s.usageReport(sid, 30000, claudeModel, planRateLimit(), false)
		}()
	}
	if d := envDuration("CRAZE_FAKE_CLAUDE_IDLE_WAKE", 0); d > 0 {
		go func() {
			time.Sleep(d)
			s.usageReport(sid, 30000, claudeModel, nil, false)
			s.claudeSay(sid, "an idle wake")
			s.tickForever(sid, "wake")
		}()
	}
}

// errorTurn is claude-errors' prompt: the failure its text names.
func (s *server) errorTurn(id json.RawMessage, text string) {
	if strings.Contains(text, "AUTH-REQUIRED") {
		_ = s.conn.ReplyErr(id, &acp.RPCError{Code: -32000, Message: "Authentication required"})
		return
	}
	fields := strings.Fields(text)
	kind := "unknown"
	for i, f := range fields {
		if f == "ERR" && i+1 < len(fields) {
			kind = fields[i+1]
		}
	}
	data := map[string]any{"errorKind": kind}
	msg := "Internal error: the turn failed"
	switch {
	case kind == "rate_limit" && strings.Contains(text, "TRIP"):
		data["rateLimit"] = tripRateLimit()
	case kind == "rate_limit" && strings.Contains(text, "rejected"):
		rl := planRateLimit()
		rl["status"] = "rejected"
		data["rateLimit"] = rl
	case kind == "rate_limit":
		rl := planRateLimit()
		rl["status"] = "allowed_warning"
		data["rateLimit"] = rl
	case strings.Contains(text, "TOOLONG"):
		msg = "Internal error: prompt is too long: 1012345 tokens > 1000000 maximum"
	}
	raw, _ := json.Marshal(data)
	_ = s.conn.ReplyErr(id, &acp.RPCError{Code: -32603, Message: msg, Data: raw})
}

// spawnChildren starts `sleep 600` twice — once in the adapter's process
// group, once in a new session — and appends "group <pid>" and "escaped
// <pid>" to CRAZE_FAKE_CHILD_PIDS.
func (s *server) spawnChildren() {
	var lines []string
	for _, c := range []struct {
		name   string
		setsid bool
	}{{"group", false}, {"escaped", true}} {
		cmd := exec.Command("sleep", "600")
		if c.setsid {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if err := cmd.Start(); err != nil {
			continue
		}
		lines = append(lines, c.name+" "+strconv.Itoa(cmd.Process.Pid))
		go func() { _ = cmd.Wait() }()
	}
	if p := os.Getenv("CRAZE_FAKE_CHILD_PIDS"); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	}
}
