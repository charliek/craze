package main

// The session/load scripts, together because they are one replay seen five
// ways: `load` is cursor's shape, `grok-load` is grok's (every notification
// tagged _meta.isReplay, a sub-agent lifecycle pair, a turn_completed, and a
// result carrying models alone), and the other three are that replay's edges —
// an agent that refuses the id, one that never answers, and one whose replay
// is longer than any client buffer.

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/charliek/craze/internal/acp"
)

// replayToolCallID is the synthetic id cursor invents for a tool call it
// replays from history: `replay-<turn>-<call>`, with no live counterpart.
const replayToolCallID = "replay-0-1"

// The replayed transcript, in one place so the cursor and grok scripts replay
// the same session and a golden can be read against either.
const (
	replayUserChunk1 = "List the files in the"
	replayUserChunk2 = " current working directory in one line."
	replayThought    = "Recalling the earlier turn."
	replayToolTitle  = "List Directory"
	replayToolOutput = "main.py README.md"
	replayAnswer     = "restored: main.py README.md"
)

// loadScript names the scripts that advertise loadSession, answer session/load,
// and refuse session/new. Every other script keeps loadSession false and
// answers session/load with the -32601 an agent without the capability sends.
func loadScript(script string) bool {
	switch script {
	case "load", "grok-load", "load-missing", "load-hang", "load-long":
		return true
	}
	return false
}

// mainID is the session the scripts stream on: the id session/load was asked
// for, or the fixed fake id when no load has happened. Without it a craze that
// loads any other id would see its own prompts echoed onto a session it is not
// listening to.
func (s *server) mainID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadedID != "" {
		return s.loadedID
	}
	return fakeSessionID
}

// handleLoad answers one session/load. Every replay notification goes out
// before the reply: replay end is the RPC result, which is what the live
// agents do and what craze's LoadSession invariant rests on.
func (s *server) handleLoad(msg *acp.Message) {
	var p acp.LoadSessionParams
	_ = json.Unmarshal(msg.Params, &p)
	sid := p.SessionID
	if sid == "" {
		sid = fakeSessionID
	}
	s.mu.Lock()
	s.loadedID = sid
	s.mu.Unlock()

	switch s.script {
	case "load-hang":
		// Answers nothing, ever. The client's own deadline is the only thing
		// that can end this call.
		return
	case "load-missing":
		// An id the agent does not have. cursor's own shape for it.
		_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: -32602, Message: "Session not found"})
	case "load-long":
		// The deadlock regression: far more replay events than a client's
		// event buffer holds, all of them before the RPC result. A client that
		// only starts draining once the load returns never gets there.
		for i := 0; i < 600; i++ {
			s.replayUpdate(sid, acp.SessionUpdate{
				SessionUpdate: acp.UpdateAgentMessage,
				Content:       &acp.ContentBlock{Type: "text", Text: "line " + strconv.Itoa(i+1) + "\n"},
			}, false)
		}
		s.reply(msg.ID, cursorLoadResult())
	case "grok-load":
		s.grokReplay(sid)
		// grok's load result: models alone — no modes, no configOptions.
		s.reply(msg.ID, map[string]any{"models": grokModels()})
	default: // "load"
		s.cursorReplay(sid)
		s.reply(msg.ID, cursorLoadResult())
	}
}

// cursorLoadResult is `{modes, models}` — cursor's session/load result, which
// is its session/new result without configOptions.
func cursorLoadResult() map[string]any {
	return map[string]any{
		"modes": map[string]any{
			"currentModeId": "agent",
			"availableModes": []map[string]string{
				{"id": "agent", "name": "Agent"},
				{"id": "plan", "name": "Plan"},
				{"id": "ask", "name": "Ask"},
			},
		},
		"models": map[string]any{
			"currentModelId": "default",
			"availableModels": []map[string]string{
				{"modelId": "default", "name": "Default"},
				{"modelId": "composer", "name": "Composer"},
			},
		},
	}
}

// cursorReplay is cursor's replay: an untagged two-chunk user message, a
// thought, a tool call that arrives pending and is completed by an update, and
// the answer.
func (s *server) cursorReplay(sid string) {
	s.replayChunk(sid, "user_message_chunk", replayUserChunk1, false)
	s.replayChunk(sid, "user_message_chunk", replayUserChunk2, false)
	s.replayChunk(sid, acp.UpdateAgentThought, replayThought, false)
	s.replayUpdate(sid, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    replayToolCallID,
		"title":         replayToolTitle,
		"kind":          "read",
		"status":        "pending",
	}, false)
	s.replayUpdate(sid, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    replayToolCallID,
		"status":        "completed",
		"content":       []map[string]any{replayToolContent()},
	}, false)
	s.replayChunk(sid, acp.UpdateAgentMessage, replayAnswer, false)
}

// grokReplay is grok's: the same transcript with params._meta.isReplay on
// every notification, a tool call that arrives already completed, and the
// sub-agent lifecycle and turn_completed that ride
// _x.ai/session_notification.
func (s *server) grokReplay(sid string) {
	child := "sub-1"
	desc := "List directory files"
	s.replayChunk(sid, "user_message_chunk", replayUserChunk1, true)
	s.replayChunk(sid, "user_message_chunk", replayUserChunk2, true)
	s.replayChunk(sid, acp.UpdateAgentThought, replayThought, true)
	s.replayUpdate(sid, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    replayToolCallID,
		"title":         replayToolTitle,
		"status":        "completed",
		"content":       []map[string]any{replayToolContent()},
		"rawOutput":     map[string]any{"type": "Text", "text": replayToolOutput},
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "list_dir", "kind": "list"}},
	}, true)
	s.subagentNotifyMeta(sid, map[string]any{
		"sessionUpdate":     "subagent_spawned",
		"subagent_id":       child,
		"attempt_id":        "at1.test",
		"parent_session_id": sid,
		"parent_prompt_id":  "prompt-1",
		"child_session_id":  child,
		"subagent_type":     "explore",
		"description":       desc,
		"capability_mode":   "read-only",
		"role":              "explore",
		"model":             "grok-4.6",
	}, true)
	s.subagentNotifyMeta(sid, map[string]any{
		"sessionUpdate":    "subagent_finished",
		"subagent_id":      child,
		"attempt_id":       "at1.test",
		"child_session_id": child,
		"status":           "completed",
		"tool_calls":       1,
		"turns":            1,
		"duration_ms":      2873,
		"tokens_used":      4740,
		"output":           replayToolOutput,
	}, true)
	s.replayChunk(sid, acp.UpdateAgentMessage, replayAnswer, true)
	// The replayed terminator. Live grok sends it on a method craze does not
	// dispatch, but a client must not let a replayed one end a turn it never
	// started, so the fake puts it where it would be noticed.
	s.subagentNotifyMeta(sid, map[string]any{
		"sessionUpdate": acp.UpdateTurnCompleted,
		"prompt_id":     "prompt-1",
		"stop_reason":   acp.StopEndTurn,
	}, true)
}

func replayToolContent() map[string]any {
	return map[string]any{
		"type":    "content",
		"content": map[string]any{"type": "text", "text": replayToolOutput},
	}
}

func (s *server) replayChunk(sid, kind, text string, replay bool) {
	s.replayUpdate(sid, acp.SessionUpdate{
		SessionUpdate: kind,
		Content:       &acp.ContentBlock{Type: "text", Text: text},
	}, replay)
}

// replayUpdate is update() with grok's replay tag: _meta.isReplay sits on the
// notification params, beside sessionId and update, not inside the update.
func (s *server) replayUpdate(sid string, upd any, replay bool) {
	raw, err := json.Marshal(upd)
	if err != nil {
		return
	}
	params := map[string]any{"sessionId": sid, "update": json.RawMessage(raw)}
	if replay {
		params["_meta"] = map[string]any{"isReplay": true}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return
	}
	_ = s.conn.Notify(context.Background(), acp.MethodSessionUpdate, json.RawMessage(body))
}
