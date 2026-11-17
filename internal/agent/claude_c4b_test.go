package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The C4b fixes (plan 052, review C4 and L4): each test fails on the code
// before its fix.

func claudeUnit(t *testing.T) (*session, *eventLog) {
	t.Helper()
	p := ClaudeProvider()
	s := newSession(Options{Provider: &p})
	s.sessionID = "root"
	t.Cleanup(func() { _ = s.Close() })
	return s, collect(t, s)
}

func claudeChildUpdate(child, state string, extra map[string]any) acp.SubagentNotification {
	can := true
	n := acp.SubagentNotification{Kind: acp.SubagentUpdate, SubagentID: child, ChildSessionID: child, Title: "t-" + child, SubagentType: "general-purpose", State: state, Cancel: &can}
	if v, ok := extra["stop"].(string); ok {
		n.StopReason = v
	}
	if v, ok := extra["error"].(string); ok {
		n.Error = v
	}
	if v, ok := extra["parent"].(string); ok {
		n.ParentSessionID = v
	}
	return n
}

func claudeRowOf(s *session, id string) (SubagentInfo, bool) {
	for _, r := range s.Snapshot().Subagents {
		if r.ID == id {
			return r, true
		}
	}
	return SubagentInfo{}, false
}

// TestClaudeChildRetiredWhenTheAgentStops (review C4 item 3): a child still
// running when the session stops for a reason of its own, or when the adapter
// exits under it, ends failed — "the agent stopped" — so nothing holds the
// session busy for a child that will never report again.
func TestClaudeChildRetiredWhenTheAgentStops(t *testing.T) {
	for _, tc := range []struct{ name, prompt string }{
		{"stopped", "AGENT-HOLD"},
		{"exited", "AGENT-EXIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, log := startClaude(t, "claude-subagent", Options{Interactive: true})
			go func() { _, _ = r.s.Prompt(context.Background(), tc.prompt) }()
			log.waitRow(t, "child-1", func(SubagentInfo) bool { return true })
			if tc.name == "stopped" {
				r.s.stopFor(errNoLoginStatus())
			}
			row := log.waitRow(t, "child-1", rowDone)
			if row.Status != SubagentFailed || row.Error != agentStopped || !row.NoCancel {
				t.Fatalf("the child under a stopped agent: %+v", row)
			}
		})
	}
}

// TestClaudeChildALateErrorIsKept (review C4 item 8): a failed child's second
// idle/error report, carrying the error text the first lacked, updates the row.
func TestClaudeChildALateErrorIsKept(t *testing.T) {
	s, log := claudeUnit(t)
	s.onSubagent(claudeChildUpdate("c1", acp.ChildRunning, nil))
	s.onSubagent(claudeChildUpdate("c1", acp.ChildIdle, map[string]any{"stop": "error"}))
	if r, _ := claudeRowOf(s, "c1"); r.Status != SubagentFailed || r.Error != "the sub-agent failed" {
		t.Fatalf("the first failure %+v", r)
	}
	s.onSubagent(claudeChildUpdate("c1", acp.ChildIdle, map[string]any{"stop": "error", "error": "API Error: 529 overloaded"}))
	if r, _ := claudeRowOf(s, "c1"); r.Status != SubagentFailed || r.Error != "API Error: 529 overloaded" {
		t.Fatalf("the late error was lost: %+v", r)
	}
	log.waitRow(t, "c1", func(r SubagentInfo) bool { return r.Error == "API Error: 529 overloaded" })
}

// TestClaudeChildReplyIsTheSendersOutput (review C4 item 9): a grandchild's
// `<launch>:reply` to its parent child is the grandchild's report, its row's
// output — never the child's prompt, and never the child's user input.
func TestClaudeChildReplyIsTheSendersOutput(t *testing.T) {
	s, log := claudeUnit(t)
	s.onSubagent(claudeChildUpdate("c1", acp.ChildRunning, nil))
	msg := func(id, from, to, text string) json.RawMessage {
		return mustJSON(map[string]any{"sessionUpdate": "session_message", "messageId": id, "senderSessionId": from, "recipientSessionId": to,
			"content": []map[string]string{{"type": "text", "text": text}}})
	}
	s.claudeSessionMessage(msg("toolu_1", "root", "c1", "delegate it"))
	s.onSubagent(claudeChildUpdate("g1", acp.ChildRunning, map[string]any{"parent": "c1"}))
	s.claudeSessionMessage(msg("toolu_2", "c1", "g1", "read deep.txt"))
	s.claudeSessionMessage(msg("toolu_2:reply", "g1", "c1", "deep"))
	c, _ := claudeRowOf(s, "c1")
	g, _ := claudeRowOf(s, "g1")
	if c.Prompt != "delegate it" || g.Prompt != "read deep.txt" || g.Output != "deep" {
		t.Fatalf("child %+v, grandchild %+v", c, g)
	}
	log.waitRow(t, "g1", func(r SubagentInfo) bool { return r.Output == "deep" })
	for _, ev := range log.snapshot() {
		if ev.Type == EventUser && ev.Agent == "c1" && ev.Text == "deep" {
			t.Fatal("the grandchild's reply was drawn as the child's input")
		}
	}
}

// TestClaudeChildFinishedRowsOutliveTheGenericCap (review C4 item 7): with
// more than 32 claude children finished, the first one's row is still held —
// its late transcript lands on it and its reuse is attempt 2 with its labels
// and cancel — as long as the ACP registry still routes it.
func TestClaudeChildFinishedRowsOutliveTheGenericCap(t *testing.T) {
	s, log := claudeUnit(t)
	for i := range 40 {
		id := fmt.Sprintf("c%d", i)
		s.onSubagent(claudeChildUpdate(id, acp.ChildRunning, nil))
		s.onSubagent(claudeChildUpdate(id, acp.ChildIdle, map[string]any{"stop": acp.StopEndTurn}))
	}
	if _, ok := claudeRowOf(s, "c0"); !ok {
		t.Fatal("the first finished child's row was evicted while the registry still holds it")
	}
	s.onChildUpdate("c0", "", sessionUpdateWire{SessionUpdate: acp.UpdateAgentMessage, Content: mustJSON(map[string]any{"type": "text", "text": "late"})}, nil)
	waitFor(t, "the late text", func() bool {
		for _, ev := range log.snapshot() {
			if ev.Type == EventText && ev.Agent == "c0" && ev.Text == "late" {
				return true
			}
		}
		return false
	})
	bare := acp.SubagentNotification{Kind: acp.SubagentUpdate, SubagentID: "c0", ChildSessionID: "c0", State: acp.ChildRunning}
	s.onSubagent(bare)
	if r, _ := claudeRowOf(s, "c0"); r.AttemptID != "2" || r.Description != "t-c0" || r.NoCancel {
		t.Fatalf("the reused row %+v; want attempt 2 with its labels and cancel", r)
	}
}

// TestClaudeChildStopOfAnUnroutedChild (review C4 item 6): a stop of a row the
// registry no longer routes says so — ErrNoSession — and leaves no stop
// pending, instead of discarding the failure.
func TestClaudeChildStopOfAnUnroutedChild(t *testing.T) {
	s, _ := claudeUnit(t)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	t.Cleanup(func() { _ = cr.Close(); _ = cw.Close(); _ = sr.Close(); _ = sw.Close() })
	client := acp.DialWithDialect(cr, cw, acp.DialectClaude)
	t.Cleanup(func() { _ = client.Close() })
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	s.onSubagent(claudeChildUpdate("c1", acp.ChildRunning, nil))
	if err := s.CancelSubagent("c1"); !errors.Is(err, acp.ErrNoSession) || !errors.Is(err, ErrNoSuchSubagent) {
		t.Fatalf("a stop of an unrouted child = %v; want ErrNoSession", err)
	}
	s.mu.Lock()
	asked := s.subagents["c1"].stopAsked
	s.mu.Unlock()
	if asked {
		t.Fatal("a stop that never went is left pending")
	}
}

// TestClaudeForeignSessionALateStartIsNoTurn (review C4 item 2): a start
// delivered after its own ending — a close that could not wait for the read
// loop — does not leave the session in a foreign turn.
func TestClaudeForeignSessionALateStartIsNoTurn(t *testing.T) {
	s, _ := claudeUnit(t)
	s.onForeignTurn(ForeignTurnInfo{ID: "claude-1", Reason: ReasonBackground, Err: &TurnError{Kind: "auth", Message: "stopped"}})
	s.onForeignTurn(ForeignTurnInfo{ID: "claude-1", Reason: ReasonBackground, Running: true})
	if s.ForeignTurn() {
		t.Fatal("a start after its own ending left the session foreign")
	}
	s.onForeignTurn(ForeignTurnInfo{ID: "claude-2", Reason: ReasonBackground, Running: true})
	if !s.ForeignTurn() {
		t.Fatal("the next turn did not open")
	}
}

// TestClaudeForeignSessionLiveAfterLoad (review C4 item 5): a wake right
// behind a load's reply is live: its foreign turn opens after the replay's end
// bracket, nothing of it is stamped replayed, and the session sees the turn.
func TestClaudeForeignSessionLiveAfterLoad(t *testing.T) {
	shortForeign(t, 5*time.Second)
	_, log := startClaude(t, "claude-replay-load", Options{Interactive: true, LoadSessionID: "s-loaded"}, "CRAZE_FAKE_CLAUDE_WAKE_AFTER_LOAD=1")
	log.waitBrackets(t, 2)
	evs := log.snapshot()
	end := slices.IndexFunc(evs, func(ev Event) bool { return ev.Type == EventReplay && ev.Replay != nil && ev.Replay.Phase == ReplayEnd })
	start := slices.IndexFunc(evs, isForeign(true))
	text := slices.IndexFunc(evs, isText("a live wake"))
	if end < 0 || start < end || text < start || evs[start].Replayed || evs[text].Replayed {
		t.Fatalf("replay end %d, start %d, text %d (replayed %v/%v)", end, start, text, start >= 0 && evs[start].Replayed, text >= 0 && evs[text].Replayed)
	}
}

// TestClaudeHeldTurnTextParagraphs (L4 F1): in the real held turn (L4's
// capture, redacted), the launch text and the hand-back are separate
// messages; the hand-back starts a paragraph of its own instead of running on
// from the launch text. Grok's text goes as it came.
func TestClaudeHeldTurnTextParagraphs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "claude-held-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := func(p Provider) string {
		s := newSession(Options{Provider: &p})
		t.Cleanup(func() { _ = s.Close() })
		log := collect(t, s)
		var want int
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var m struct {
				Params acp.SessionNotification `json:"params"`
			}
			if json.Unmarshal([]byte(line), &m) != nil || m.Params.SessionID != "SESSION" {
				continue
			}
			if strings.Contains(string(m.Params.Update), `"agent_message_chunk"`) {
				want++
			}
			s.onUpdate(m.Params)
		}
		var b strings.Builder
		waitFor(t, "the turn's text", func() bool {
			b.Reset()
			n := 0
			for _, ev := range log.snapshot() {
				if ev.Type == EventText && ev.Agent == "" {
					b.WriteString(ev.Text)
					n++
				}
			}
			return n == want
		})
		return b.String()
	}
	if got := text(ClaudeProvider()); !strings.Contains(got, "first word.\n\nPineapple") {
		t.Fatalf("claude's held turn reads %q; want the hand-back in a paragraph of its own", got)
	}
	if got := text(GrokProvider()); !strings.Contains(got, "first word.Pineapple") {
		t.Fatalf("grok's text was changed: %q", got)
	}
}
