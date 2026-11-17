package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The claude background and children scripts (craze plan 052 C4), shaped
// after the adapter's real frames (internal/acp/testdata/claude-wake and
// claude-subagent): a wake is a model cycle of the adapter's own with no prompt
// in flight — a usage_update, the cycle's content, and the result's usage
// carrying cost and _claude/origin task-notification — and a child is
// announced with subagent_update on its immediate parent, given its prompt by
// session_message on both sessions, and streamed on its own id.
//
// Each is claude-basic, plus:
//
//	claude-wake                   a prompt with BG answers "started"; a wake
//	                              follows after CRAZE_FAKE_CLAUDE_WAKE_DELAY
//	claude-wake-early             a chunk before session/new's reply (opens
//	                              nothing) and a wake right after it
//	claude-wake-placeholder       BG: a wake whose num_turns:0 placeholder
//	                              usage is followed CRAZE_FAKE_CLAUDE_GAP later
//	                              by its follow-up and the follow-up's usage
//	claude-wake-placeholder-late  the same, its gap meant to outlast the grace
//	claude-wake-opentool          BG: a wake that opens a tool call, sends its
//	                              cost usage, and closes the call
//	                              CRAZE_FAKE_CLAUDE_GAP later
//	claude-wake-nousage           BG: a wake with content and no usage at all
//	claude-wake-trailing          BG: the turn's tool call is closed after the
//	                              reply, a trailing tool_call_update
//	claude-replay-load            claude-load with a replay holding a whole
//	                              cycle: content, a tool call, a cost usage
//	claude-notice-idle            after session/new: a notice, a plan, a cost
//	                              usage, a compaction summary, a commands update
//	claude-compaction-idle        after session/new: idle compaction
//	                              (compaction_update in_progress, its usage,
//	                              completed)
//	claude-subagent               AGENT: a child that reads and reports back;
//	                              AGENT-HOLD: one that works until cancelled;
//	                              AGENT-EXIT: one still working when the
//	                              adapter exits
//	                              (its own session/cancel)
//	claude-subagent-nested        AGENT: root → child → grandchild, each asking
//	                              nothing, each idle in turn
//	claude-subagent-early-child   AGENT: the child's first update before its
//	                              announce
//	claude-subagent-reuse         AGENT twice: the second prompt reuses the
//	                              first's idle child
//	claude-subagent-unknown       AGENT: two children, one of which becomes
//	                              unknown
//	claude-child-ask              AGENT: a child that asks a permission, the
//	                              root's turn ending while the card is open,
//	                              then the child going on once answered
//	claude-held-bg-subagent       any prompt: a background child; the reply is
//	                              held until the child and its hand-back are
//	                              done, as the adapter holds it
//	claude-unattended-bg          any prompt: a background shell; the reply
//	                              goes at once, and the shell's wake later
//
// CRAZE_FAKE_CLAUDE_WAKE_AFTER_LOAD=1 makes a load's reply followed at once by
// a wake (claude-replay-load), live content right behind the replay.
//
// Environment: CRAZE_FAKE_CLAUDE_WAKE_DELAY (default 100ms), CRAZE_FAKE_CLAUDE_GAP
// (default 50ms), CRAZE_FAKE_CLAUDE_CHILD_RUN (default 200ms), and
// CRAZE_FAKE_CLAUDE_STEER_STARTS=1, under which an idle _session/steering is
// answered startedNewTurn and its text run as a turn of the adapter's own.

// Child ids.
const (
	fakeChild      = "child-1"
	fakeChild2     = "child-2"
	fakeGrandchild = "grandchild-1"
)

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

// claudeChildren is the scripts' child state: each child's cancel channel.
type claudeChildren struct {
	mu     sync.Mutex
	cancel map[string]chan struct{}
}

func (c *claudeChildren) chanFor(id string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel == nil {
		c.cancel = make(map[string]chan struct{})
	}
	ch, ok := c.cancel[id]
	if !ok {
		ch = make(chan struct{})
		c.cancel[id] = ch
	}
	return ch
}

// cancelChild is a session/cancel naming a child: that child stops.
func (c *claudeChildren) cancelChild(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.cancel[id]
	if !ok {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

var fakeKids claudeChildren

func (s *server) usage(sid string, cost bool) {
	u := map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 1000000}
	if cost {
		u["cost"] = map[string]any{"amount": 0.01, "currency": "USD"}
		u["_meta"] = map[string]any{"_claude/origin": map[string]any{"kind": "task-notification", "producer": "session-task"}}
	}
	s.update(sid, u)
}

func (s *server) toolOn(sid, id, kind, status string) {
	u := map[string]any{"sessionUpdate": kind, "toolCallId": id, "status": status,
		"_meta": claudeMetaTool("Bash")}
	if kind == acp.UpdateToolCall {
		u["title"], u["kind"] = "Terminal", "execute"
	}
	s.update(sid, u)
}

// wake is one model cycle of the adapter's own: its first usage, content,
// and the result's usage with cost.
func (s *server) wake(sid, text string) {
	s.usage(sid, false)
	s.claudeSay(sid, text)
	s.usage(sid, true)
}

// claudeSetupTraffic is what the setup scripts send around session/new's
// reply: before it (pre) and right after it (post).
func (s *server) claudeSetupTraffic(sid string, post bool) {
	switch s.script {
	case "claude-wake-early":
		if !post {
			s.claudeSay(sid, "before the reply")
			return
		}
		s.wake(sid, "an early wake")
	case "claude-notice-idle":
		if !post {
			return
		}
		s.update(sid, map[string]any{"sessionUpdate": "notice", "severity": "info", "title": "a notice"})
		s.update(sid, map[string]any{"sessionUpdate": "plan", "entries": []any{}})
		s.usage(sid, true)
		s.update(sid, map[string]any{"sessionUpdate": "compaction_summary_chunk", "content": map[string]string{"type": "text", "text": "summary"}})
		s.update(sid, map[string]any{"sessionUpdate": acp.UpdateAvailableCommands, "availableCommands": []any{}})
	case "claude-overage":
		if post {
			s.claudeOverageSetup(sid)
		}
	case "claude-compaction-idle":
		if !post {
			return
		}
		s.update(sid, map[string]any{"sessionUpdate": "compaction_update", "status": "in_progress"})
		s.usage(sid, true)
		s.update(sid, map[string]any{"sessionUpdate": "compaction_update", "status": "completed"})
	}
}

// claudeBGTurn is a background script's prompt; false leaves it to the rest
// of claudePrompt.
func (s *server) claudeBGTurn(id json.RawMessage, sid, text string) bool {
	endTurn := func(say string) { s.endTurnSaying(id, sid, say) }
	delay := envDuration("CRAZE_FAKE_CLAUDE_WAKE_DELAY", 100*time.Millisecond)
	gap := envDuration("CRAZE_FAKE_CLAUDE_GAP", 50*time.Millisecond)
	bg := strings.Contains(text, "BG")
	switch {
	case s.script == "claude-wake" && bg:
		endTurn("started")
		time.Sleep(delay)
		s.wake(sid, "bg-done")
	case (s.script == "claude-wake-placeholder" || s.script == "claude-wake-placeholder-late") && bg:
		endTurn("started")
		time.Sleep(delay)
		// The cycle opens, and its num_turns:0 placeholder reports its usage
		// before the shared follow-up runs (v1/acp-agent.js:4165).
		s.claudeSay(sid, "working on it")
		s.usage(sid, true)
		time.Sleep(gap)
		s.wake(sid, "the follow-up")
	case s.script == "claude-wake-opentool" && bg:
		endTurn("started")
		time.Sleep(delay)
		s.claudeSay(sid, "reading the output")
		s.toolOn(sid, "toolu_wake", acp.UpdateToolCall, "in_progress")
		s.usage(sid, true)
		time.Sleep(gap)
		s.toolOn(sid, "toolu_wake", acp.UpdateToolCallUpd, "completed")
	case s.script == "claude-wake-nousage" && bg:
		endTurn("started")
		time.Sleep(delay)
		s.claudeSay(sid, "a wake that never reports usage")
	case s.script == "claude-wake-trailing" && bg:
		s.toolOn(sid, "toolu_turn", acp.UpdateToolCall, "in_progress")
		endTurn("started")
		s.toolOn(sid, "toolu_turn", acp.UpdateToolCallUpd, "completed")
	case s.script == "claude-held-bg-subagent":
		s.heldBackgroundChild(id, sid)
	case s.script == "claude-unattended-bg":
		s.toolOn(sid, "toolu_bg", acp.UpdateToolCall, "in_progress")
		s.update(sid, map[string]any{"sessionUpdate": acp.UpdateToolCallUpd, "toolCallId": "toolu_bg", "status": "completed",
			"rawOutput": "Command running in background with ID: b1.", "_meta": claudeMetaTool("Bash")})
		endTurn("started")
		go func() {
			time.Sleep(delay)
			s.wake(sid, "bg-done")
		}()
	case strings.Contains(text, "AGENT"):
		s.childTurn(id, sid, text)
	default:
		return false
	}
	return true
}

// endTurnSaying ends prompt id's turn on sid with say as its last text.
func (s *server) endTurnSaying(id json.RawMessage, sid, say string) {
	s.claudeSay(sid, say)
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// sessionMessage is a session_message update: a prompt sender sent recipient.
func sessionMessage(msgID, sender, recipient, text string) map[string]any {
	return map[string]any{"sessionUpdate": "session_message", "messageId": msgID, "senderSessionId": sender, "recipientSessionId": recipient,
		"content": []map[string]string{{"type": "text", "text": text}}}
}

// announce is a child's subagent_update running, on its immediate parent, and
// its prompt by session_message on both sessions (native-subagents.js).
func (s *server) announce(parent, child, title, prompt, call string) {
	s.update(parent, map[string]any{"sessionUpdate": "subagent_update", "sessionId": child, "title": title, "description": "general-purpose",
		"capabilities": map[string]any{"cancel": map[string]any{}}, "state": map[string]any{"state": "running"}})
	msg := sessionMessage(call, parent, child, prompt)
	s.update(parent, msg)
	s.update(child, msg)
}

func (s *server) childState(parent, child string, state map[string]any) {
	s.update(parent, map[string]any{"sessionUpdate": "subagent_update", "sessionId": child, "state": state})
}

func idle(stop string) map[string]any {
	return map[string]any{"state": "idle", "stopReason": stop}
}

// childWork is a child reading a file and saying so.
func (s *server) childWork(child, what string) {
	s.toolOn(child, "toolu_"+child, acp.UpdateToolCall, "in_progress")
	s.toolOn(child, "toolu_"+child, acp.UpdateToolCallUpd, "completed")
	s.claudeSay(child, what)
}

// childTurn is an AGENT prompt of the children scripts.
func (s *server) childTurn(id json.RawMessage, sid, text string) {
	end := func(say string) { s.endTurnSaying(id, sid, say) }
	switch s.script {
	case "claude-subagent":
		if strings.Contains(text, "AGENT-EXIT") {
			s.announce(sid, fakeChild, "Doomed", "work until the end", "toolu_launch_1")
			s.claudeSay(fakeChild, "working")
			// The adapter dies with its child running: nothing more of it.
			time.Sleep(50 * time.Millisecond)
			os.Exit(3)
		}
		if strings.Contains(text, "AGENT-HOLD") {
			ch := fakeKids.chanFor(fakeChild)
			s.announce(sid, fakeChild, "Hold on", "work until stopped", "toolu_launch_1")
			s.claudeSay(fakeChild, "working")
			s.mu.Lock()
			hw := make(chan struct{})
			s.hangWait = hw
			s.mu.Unlock()
			select {
			case <-ch:
				s.childState(sid, fakeChild, idle(acp.StopCancelled))
				end("the child was stopped")
			case <-hw:
				s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
			}
			return
		}
		s.announce(sid, fakeChild, "Read note.txt contents", "read note.txt", "toolu_launch_1")
		s.childWork(fakeChild, "hello")
		s.childState(sid, fakeChild, idle(acp.StopEndTurn))
		end("the child said hello")
	case "claude-subagent-nested":
		s.announce(sid, fakeChild, "Outer", "delegate", "toolu_launch_1")
		s.announce(fakeChild, fakeGrandchild, "Inner", "read deep.txt", "toolu_launch_2")
		s.childWork(fakeGrandchild, "deep")
		s.childState(fakeChild, fakeGrandchild, idle(acp.StopEndTurn))
		s.claudeSay(fakeChild, "the grandchild said deep")
		s.childState(sid, fakeChild, idle(acp.StopEndTurn))
		end("nested done")
	case "claude-subagent-early-child":
		s.claudeSay(fakeChild, "before my announce")
		s.announce(sid, fakeChild, "Early", "be early", "toolu_launch_1")
		s.childWork(fakeChild, "after my announce")
		s.childState(sid, fakeChild, idle(acp.StopEndTurn))
		end("early done")
	case "claude-subagent-reuse":
		s.mu.Lock()
		s.promptN++
		n := s.promptN
		s.mu.Unlock()
		if n == 1 {
			s.announce(sid, fakeChild, "Reusable", "first job", "toolu_launch_1")
			s.childWork(fakeChild, "first")
		} else {
			// SendMessage resumes the idle child (native-subagents.js
			// resumeChild): running again, its new prompt.
			s.childState(sid, fakeChild, map[string]any{"state": "running"})
			s.update(fakeChild, sessionMessage("toolu_send_2", sid, fakeChild, "second job"))
			s.childWork(fakeChild, "second")
		}
		s.childState(sid, fakeChild, map[string]any{"state": "idle"})
		end("reuse done")
	case "claude-subagent-unknown":
		s.announce(sid, fakeChild, "Lost", "get lost", "toolu_launch_1")
		s.announce(sid, fakeChild2, "Kept", "stay", "toolu_launch_2")
		s.update(sid, map[string]any{"sessionUpdate": "subagent_update", "sessionId": fakeChild, "capabilities": nil, "state": map[string]any{"state": "unknown"}})
		s.childWork(fakeChild2, "kept")
		s.childState(sid, fakeChild2, idle(acp.StopEndTurn))
		end("unknown done")
	case "claude-child-ask":
		s.announce(sid, fakeChild, "Asker", "write a file", "toolu_launch_1")
		s.childState(sid, fakeChild, map[string]any{"state": "requires_action"})
		answered := make(chan string, 1)
		go func() {
			var result struct {
				Outcome struct {
					Outcome  string `json:"outcome"`
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			}
			params := map[string]any{"sessionId": fakeChild,
				"toolCall": map[string]any{"toolCallId": "toolu_child_write", "title": "Write child.txt", "kind": "edit"},
				"options": []acp.PermissionOption{
					{OptionID: "allow-once", Name: "Yes", Kind: acp.KindAllowOnce},
					{OptionID: "reject", Name: "No", Kind: acp.KindRejectOnce},
				}}
			_ = s.conn.Call(context.Background(), acp.MethodRequestPermission, params, &result)
			answered <- result.Outcome.OptionID
		}()
		// The root's turn ends with the child's card still open.
		end("the child is asking")
		opt := <-answered
		s.childState(sid, fakeChild, map[string]any{"state": "running"})
		s.claudeSay(fakeChild, "permission: "+opt)
		s.childState(sid, fakeChild, idle(acp.StopEndTurn))
	default:
		end("no children here")
	}
}

// heldBackgroundChild is the adapter's held turn (v1/acp-agent.js:418,
// deferredSettle): the prompt starts a background child and says so, the
// child runs, goes idle, the task notification's follow-up streams — and only
// after its result usage is the prompt answered.
func (s *server) heldBackgroundChild(id json.RawMessage, sid string) {
	s.usage(sid, false)
	s.announce(sid, fakeChild, "Background job", "run in the background", "toolu_launch_1")
	s.update(sid, sessionMessage("toolu_launch_1:reply", fakeChild, sid, "Async agent launched successfully."))
	s.claudeSay(sid, "the child is running in the background")
	s.usage(sid, true)
	time.Sleep(envDuration("CRAZE_FAKE_CLAUDE_CHILD_RUN", 200*time.Millisecond))
	s.childWork(fakeChild, "background result")
	s.childState(sid, fakeChild, idle(acp.StopEndTurn))
	s.wake(sid, "the child reported: background result")
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// claudeCancelChild is session/cancel as the claude scripts read it: one that
// names a child stops that child alone; anything else is the root's.
func (s *server) claudeCancelChild(params json.RawMessage) bool {
	var p acp.CancelParams
	_ = json.Unmarshal(params, &p)
	if s.claude == nil || p.SessionID == "" || p.SessionID == s.claude.sid {
		return false
	}
	fakeKids.cancelChild(p.SessionID)
	return true
}

// claudeStartedTurn is CRAZE_FAKE_CLAUDE_STEER_STARTS's idle steer: answered
// startedNewTurn, its text run as a turn of the adapter's own.
func (s *server) claudeStartedTurn(msg *acp.Message, text string) {
	s.reply(msg.ID, map[string]any{"outcome": "startedNewTurn"})
	s.wake(s.claude.sid, "steered: "+text)
}
