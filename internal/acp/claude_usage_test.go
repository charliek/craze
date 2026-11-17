package acp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The overage guard's wire half (plan 052 § D3): the rate limit read, the four
// trip signals, the trip ending the foreign turn before its result can settle
// it, the fence and the severed stop.

// probe2RateLimit is the real `_claude/rateLimit` of verify/L0 probe2's first
// usage_update (adapter 0.107.3): a Max account at 56% of its seven-day window
// with extra usage disabled by its org — no signal.
const probe2RateLimit = `{"status":"allowed_warning","resetsAt":1795399200,"rateLimitType":"seven_day","utilization":0.56,` +
	`"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,` +
	`"unifiedWindows":{"seven_day":{"utilization":0.56,"resetsAt":1795399200,"status":"allowed"}},"overageEnabled":false}`

func usageWith(rl string, cost bool) string {
	u := `{"sessionUpdate":"usage_update","used":28529,"size":1000000,`
	if cost {
		u += `"cost":{"amount":0.0067,"currency":"USD"},`
	}
	return u + `"_meta":{"_claude/rateLimit":` + rl + `,"_claude/model":"claude-haiku-5-5"}}`
}

func TestRateLimitTrips(t *testing.T) {
	real, ok := UsageRateLimit(json.RawMessage(usageWith(probe2RateLimit, false)))
	if !ok || real.Trips() || real.RateLimitType != "seven_day" || real.Utilization == nil || *real.Utilization != 0.56 ||
		real.ResetsAt != 1795399200 || real.Status != "allowed_warning" {
		t.Fatalf("probe2's rate limit read as %+v %v; want no trip", real, ok)
	}
	for _, tc := range []struct {
		rl   string
		trip bool
	}{
		{`{"status":"allowed","isUsingOverage":true}`, true},
		{`{"status":"allowed","overageInUse":true}`, true},
		{`{"status":"allowed","rateLimitType":"overage"}`, true},
		{`{"status":"allowed","rateLimitType":"seven_day_overage_included"}`, true},
		{`{"status":"rejected","overageStatus":"allowed"}`, true},
		{`{"status":"rejected","overageStatus":"allowed_warning"}`, true},
		{`{"status":"rejected","overageStatus":"rejected"}`, false},
		{`{"status":"rejected"}`, false},
		{`{"status":"allowed_warning","overageStatus":"allowed","rateLimitType":"five_hour"}`, false},
	} {
		rl, ok := UsageRateLimit(json.RawMessage(usageWith(tc.rl, false)))
		if !ok || rl.Trips() != tc.trip {
			t.Errorf("%s: trips %v (read %v); want %v", tc.rl, rl.Trips(), ok, tc.trip)
		}
	}
	for _, none := range []string{`{"sessionUpdate":"usage_update","used":1,"size":2}`, `{"_meta":{"_claude/rateLimit":null}}`, `{"_meta":{"_claude/rateLimit":"x"}}`} {
		if _, ok := UsageRateLimit(json.RawMessage(none)); ok {
			t.Errorf("%s: read a rate limit from none", none)
		}
	}
	rl, ok := ErrorRateLimit(json.RawMessage(`{"errorKind":"rate_limit","rateLimit":{"status":"rejected","overageStatus":"allowed","resetsAt":1795399200}}`))
	if !ok || !rl.Trips() || rl.ResetsAt != 1795399200 {
		t.Fatalf("data.rateLimit read as %+v %v", rl, ok)
	}
	if _, ok := ErrorRateLimit(json.RawMessage(`{"errorKind":"billing_error"}`)); ok {
		t.Fatal("a rate limit read from data with none")
	}
}

// TestClaudeTripEndsTheForeignTurnBeforeItsResult (§ D3): a trip on the
// foreign turn's own result usage ends that turn with the handler's error at
// once — not after the grace, not completed — and closes the machine; the
// handler sees every live rate limit, tripping or not, and none a replay
// holds.
func TestClaudeTripEndsTheForeignTurnBeforeItsResult(t *testing.T) {
	r := newClaudeRig(t)
	errTrip := errors.New("tripped")
	var seen []bool
	r.client.SetRateLimitHandler(func(rl RateLimit) error {
		seen = append(seen, rl.Trips())
		r.log.add("ratelimit")
		if rl.Trips() {
			return errTrip
		}
		return nil
	})
	var ended *ForeignTurn
	r.client.SetForeignTurnHandler(func(f ForeignTurn) {
		r.log.add(foreignLine(f))
		if !f.Running {
			ended = &f
		}
	})
	r.ready(t)
	r.push(t, usageWith(probe2RateLimit, false), agentText("waking"),
		usageWith(`{"status":"allowed","isUsingOverage":true,"resetsAt":1795399200}`, true), agentText("more"))
	want := []string{"ratelimit", "usage_update", "start claude-1 background", "agent_message_chunk",
		"ratelimit", "end claude-1 background err", "usage_update", "agent_message_chunk"}
	if got := r.log.get(); !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if ended == nil || !errors.Is(ended.Err, errTrip) || ended.Cancelled || ended.Silent {
		t.Fatalf("ending %+v; want failed with the trip", ended)
	}
	if !slices.Equal(seen, []bool{false, true}) || r.client.ForeignPhase() != "closed" {
		t.Fatalf("seen %v, phase %s", seen, r.client.ForeignPhase())
	}
	// Nothing opens again, whatever the agent sends: the session is stopping.
	r.clk.Advance(time.Hour)
	if got := r.log.get(); len(got) != len(want) {
		t.Fatalf("after the trip: %q", got[len(want):])
	}
}

func agentText(s string) string {
	return `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + s + `"}}`
}

// TestClaudeFenceLetsOnlyTheCancelThrough (§ D3): once fenced, a prompt, a
// reply and any other notification are refused before the wire, and a
// session/cancel still goes.
func TestClaudeFenceLetsOnlyTheCancelThrough(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	frames := make(chan string, 16)
	go func() {
		for {
			msg, err := r.dec.ReadMessage()
			if err != nil {
				return
			}
			frames <- msg.Method
		}
	}()
	r.client.Fence()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := r.client.PromptBlocks(ctx, []ContentBlock{{Type: "text", Text: "hi"}}, nil, nil); !errors.Is(err, ErrFenced) {
		t.Fatalf("a prompt after the fence: %v", err)
	}
	if err := r.client.Conn().Reply(json.RawMessage(`7`), map[string]string{"outcome": "allow"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("a reply after the fence: %v", err)
	}
	if err := r.client.Conn().Notify(t.Context(), "craze/other", nil); !errors.Is(err, ErrFenced) {
		t.Fatalf("a notification after the fence: %v", err)
	}
	if err := r.client.Conn().Notify(t.Context(), MethodSessionCancel, CancelParams{SessionID: rigRoot}); err != nil {
		t.Fatalf("the cancel after the fence: %v", err)
	}
	select {
	case m := <-frames:
		if m != MethodSessionCancel {
			t.Fatalf("got %q on the wire; want only the cancel", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the cancel never reached the wire")
	}
}

// TestClaudeStopWithinSevers (§ D3): the stop cancels the root, waits its
// grace, and from the sever on nothing the agent writes reaches anyone.
func TestClaudeStopWithinSevers(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.client.Fence()
	var at []string
	prev := testAfterSever
	testAfterSever = func() {
		at = r.log.get()
		// The agent goes on talking after the sever.
		for i := 0; i < 5; i++ {
			params := `{"sessionId":"` + rigRoot + `","update":` + agentText("after") + `}`
			_ = r.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, Method: MethodSessionUpdate, Params: json.RawMessage(params)})
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Cleanup(func() { testAfterSever = prev })
	done := make(chan struct{})
	start := time.Now()
	go func() { defer close(done); r.client.StopWithin(150 * time.Millisecond) }()
	if msg := r.readWithin(t, 3*time.Second, "the cancel"); msg.Method != MethodSessionCancel {
		t.Fatalf("got %s first; want the cancel", msg.Method)
	}
	r.pushUpdate(t, rigRoot, `{"sessionUpdate":"available_commands_update","availableCommands":[]}`)
	<-done
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Fatalf("stopped after %s; want the grace first", took)
	}
	if got := r.log.get(); !slices.Equal(got, at) || !slices.Contains(at, "available_commands_update") {
		t.Fatalf("handed on after the sever: %q (at the sever %q)", got[len(at):], at)
	}
}

// TestClaudeReplayedRateLimitIsHistory (§ D3): a rate limit a session/load
// replays was acted on when it was live; only the live ones held behind the
// load's reply reach the handler, when ReplayDone routes them.
func TestClaudeReplayedRateLimitIsHistory(t *testing.T) {
	r := newClaudeRig(t)
	var trips []bool
	r.client.SetRateLimitHandler(func(rl RateLimit) error {
		trips = append(trips, rl.Trips())
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.client.LoadSession(context.Background(), rigRoot, t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/load")
	r.pushUpdate(t, rigRoot, usageWith(`{"status":"allowed","isUsingOverage":true}`, true))
	r.answerWith(t, req, `{}`)
	r.pushUpdate(t, rigRoot, usageWith(probe2RateLimit, false))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.roundTrip(t)
	r.client.ReplayDone()
	r.roundTrip(t)
	if !slices.Equal(trips, []bool{false}) {
		t.Fatalf("the handler saw %v; want the live report alone", trips)
	}
}

// TestClaudeUsageFrames reads a real session's usage (verify/L0 probe2,
// adapter 0.107.3, redacted: testdata/claude-usage): the turn's first report
// carries the account's rate limit — a Max plan at 56% of seven days, extra
// usage disabled by its org: no signal — the rest name the model alone, the
// result's report carries cost and its origin, and each prompt result's
// _meta.quota is the turn's tokens (all zero for a cancelled one). Routed
// through a claude client, the handler hears the one rate limit and nothing
// opens a turn.
func TestClaudeUsageFrames(t *testing.T) {
	frames := fixtureFrames(t, "claude-usage")
	r := newClaudeRig(t)
	r.root = "SESSION"
	var limits []RateLimit
	r.client.SetRateLimitHandler(func(rl RateLimit) error {
		limits = append(limits, rl)
		return nil
	})
	r.ready(t)
	var quotas []PromptQuota
	costs := 0
	for _, f := range frames {
		m := f.Msg
		if m.Method == "" {
			var res PromptResult
			if err := json.Unmarshal(m.Result, &res); err != nil {
				t.Fatal(err)
			}
			q, ok := res.Quota()
			if !ok {
				t.Fatalf("result %s carries no quota", m.ID)
			}
			quotas = append(quotas, q)
			continue
		}
		var n SessionNotification
		if err := json.Unmarshal(m.Params, &n); err != nil {
			t.Fatal(err)
		}
		if hasCost(n.Update) {
			costs++
		}
		r.pushOn(t, n.SessionID, string(n.Update))
	}
	if len(limits) != 1 || limits[0].Trips() || limits[0].RateLimitType != "seven_day" || *limits[0].Utilization != 0.56 {
		t.Fatalf("rate limits heard %+v; want the one real report, no trip", limits)
	}
	want := []PromptQuota{
		{TotalTokens: 59586, InputTokens: 4, CachedInputTokens: 28511, CachedWriteTokens: 30907, OutputTokens: 164},
		{TotalTokens: 72791, InputTokens: 4, CachedInputTokens: 71733, CachedWriteTokens: 768, OutputTokens: 286},
		{},
	}
	if !slices.Equal(quotas, want) || costs < 3 {
		t.Fatalf("quotas %+v (%d cost reports)", quotas, costs)
	}
	for _, line := range r.log.get() {
		if strings.HasPrefix(line, "start ") {
			t.Fatalf("usage opened a turn: %q", r.log.get())
		}
	}
}
