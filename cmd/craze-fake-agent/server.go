package main

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"

	"github.com/charliek/craze/internal/acp"
)

const fakeSessionID = "fake-session-1"

type server struct {
	conn   *acp.Conn
	script string

	mu        sync.Mutex
	promptN   int
	cancelled atomic.Bool
	hangWait  chan struct{}
	config    []map[string]any
}

func defaultConfigOptions() []map[string]any {
	return []map[string]any{
		{
			"id":           "effort",
			"name":         "Effort",
			"category":     "thought_level",
			"type":         "select",
			"currentValue": "medium",
			"options": []map[string]string{
				{"value": "low", "name": "Low"},
				{"value": "medium", "name": "Medium"},
				{"value": "high", "name": "High"},
			},
		},
	}
}

func run(script string) error {
	conn := acp.NewConn(os.Stdin, os.Stdout)
	s := &server{conn: conn, script: script, config: defaultConfigOptions()}
	conn.SetRequestHandler(s.onRequest)
	conn.SetNotifyHandler(s.onNotify)
	conn.Start()
	<-conn.Done()
	return nil
}

func (s *server) onRequest(msg *acp.Message) {
	switch msg.Method {
	case acp.MethodInitialize:
		auth := []map[string]string{{"id": acp.AuthCursorLogin, "name": "Cursor Login"}}
		if s.script == "noauth" {
			auth = []map[string]string{}
		}
		s.reply(msg.ID, map[string]any{
			"protocolVersion": acp.ProtocolVersion,
			"agentInfo":       map[string]string{"name": "craze-fake-agent", "version": "test"},
			"authMethods":     auth,
			"agentCapabilities": map[string]any{
				"loadSession": false,
			},
		})
	case acp.MethodAuthenticate:
		if s.script == "noauth" {
			_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: -32000, Message: "authenticate must not be called"})
			return
		}
		if s.script == "authfail" {
			_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: -32000, Message: "authentication failed"})
			return
		}
		s.reply(msg.ID, map[string]any{})
	case acp.MethodSessionNew:
		s.mu.Lock()
		cfg := s.config
		s.mu.Unlock()
		s.reply(msg.ID, map[string]any{
			"sessionId": fakeSessionID,
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
			"configOptions": cfg,
		})
		s.update(fakeSessionID, acp.SessionUpdate{
			SessionUpdate: acp.UpdateAvailableCommands,
			AvailableCommands: []acp.AvailableCommand{
				{Name: "research", Description: "Agent-advertised command"},
			},
		})
	case acp.MethodSessionPrompt:
		go s.handlePrompt(msg)
	case acp.MethodSessionSetModel:
		s.reply(msg.ID, map[string]any{})
	case acp.MethodSessionSetMode:
		var p acp.SetModeParams
		_ = json.Unmarshal(msg.Params, &p)
		s.reply(msg.ID, map[string]any{})
		if p.ModeID != "" {
			s.update(fakeSessionID, acp.SessionUpdate{
				SessionUpdate: acp.UpdateCurrentMode,
				CurrentModeID: p.ModeID,
			})
		}
	case acp.MethodSessionSetConfig:
		var p struct {
			ConfigID string `json:"configId"`
			Value    string `json:"value"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		for _, opt := range s.config {
			id, _ := opt["id"].(string)
			if id == p.ConfigID {
				opt["currentValue"] = p.Value
			}
		}
		cfg := s.config
		s.mu.Unlock()
		s.update(fakeSessionID, map[string]any{
			"sessionUpdate": acp.UpdateConfigOption,
			"configOptions": cfg,
		})
		s.reply(msg.ID, map[string]any{})
	default:
		_ = s.conn.ReplyErr(msg.ID, acp.MethodNotFound(msg.Method))
	}
}

func (s *server) onNotify(msg *acp.Message) {
	if msg.Method != acp.MethodSessionCancel {
		return
	}
	s.mu.Lock()
	s.cancelled.Store(true)
	if s.hangWait != nil {
		close(s.hangWait)
		s.hangWait = nil
	}
	s.mu.Unlock()
}

func (s *server) handlePrompt(msg *acp.Message) {
	text := promptText(msg.Params)
	s.mu.Lock()
	s.promptN++
	n := s.promptN
	s.mu.Unlock()

	switch s.script {
	case "hang":
		s.hang(msg.ID)
	case "followup":
		s.followup(msg.ID, n)
	case "tool":
		s.tool(msg.ID)
	case "tasks":
		s.tasks(msg.ID)
	case "permission":
		s.permission(msg.ID)
	case "ask":
		s.ask(msg.ID)
	case "plan":
		s.plan(msg.ID)
	default:
		s.echo(msg.ID, text)
	}
}

func (s *server) hang(id json.RawMessage) {
	s.mu.Lock()
	ch := make(chan struct{})
	s.hangWait = ch
	already := s.cancelled.Load()
	if already {
		s.hangWait = nil
	}
	s.mu.Unlock()
	if already {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	<-ch
	s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
}

func (s *server) echo(id json.RawMessage, text string) {
	s.update("other-session", acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "NOPE"},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "echo: "},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: text},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) followup(id json.RawMessage, n int) {
	text := "first reply"
	if n >= 2 {
		text = "second reply"
	}
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: text},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) tool(id json.RawMessage) {
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateToolCall,
		ToolCallID:    "call-1",
		Title:         "Shell",
		Kind:          "execute",
		Status:        "pending",
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "after tool"},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) tasks(id json.RawMessage) {
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    "task-1",
		"kind":          "other",
		"title":         "Subagent research",
		"status":        "pending",
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    "sh-1",
		"kind":          "execute",
		"title":         "Shell",
		"status":        "in_progress",
		"rawInput":      map[string]string{"command": "echo hi"},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    "task-1",
		"content": []map[string]any{
			{"type": "content", "content": map[string]any{"type": "text", "text": "scanning workspace"}},
		},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    "sh-1",
		"status":        "completed",
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    "task-1",
		"status":        "completed",
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "done tasks"},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) permission(id json.RawMessage) {
	params := acp.PermissionRequest{
		SessionID: fakeSessionID,
		ToolCall: acp.ToolCall{
			ToolCallID: "call-perm-1",
			Title:      "Shell",
			Kind:       "execute",
			Status:     "pending",
		},
		Options: []acp.PermissionOption{
			{OptionID: "opt-always", Name: "Allow always", Kind: acp.KindAllowAlways},
			{OptionID: "opt-once", Name: "Allow once", Kind: acp.KindAllowOnce},
			{OptionID: "opt-reject", Name: "Reject once", Kind: acp.KindRejectOnce},
		},
	}
	var result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	err := s.conn.Call(context.Background(), acp.MethodRequestPermission, params, &result)
	if err != nil || result.Outcome.Outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "decision:" + result.Outcome.OptionID},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) ask(id json.RawMessage) {
	params := acp.AskQuestionRequest{
		ToolCallID: "ask-1",
		Title:      "Question",
		Questions: []acp.AskQuestion{{
			ID:     "q1",
			Prompt: "Pick one",
			Options: []acp.AskOption{
				{ID: "opt-a", Label: "A"},
				{ID: "opt-b", Label: "B"},
			},
		}},
	}
	var result struct {
		Outcome struct {
			Outcome string `json:"outcome"`
		} `json:"outcome"`
	}
	err := s.conn.Call(context.Background(), acp.MethodCursorAskQuestion, params, &result)
	if err != nil || result.Outcome.Outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "asked:" + result.Outcome.Outcome},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) plan(id json.RawMessage) {
	params := map[string]any{"title": "Plan", "plan": "do the thing"}
	var result struct {
		Outcome struct {
			Outcome string `json:"outcome"`
		} `json:"outcome"`
	}
	err := s.conn.Call(context.Background(), acp.MethodCursorCreatePlan, params, &result)
	if err != nil || result.Outcome.Outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "planned:" + result.Outcome.Outcome},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) update(sessionID string, upd any) {
	raw, err := json.Marshal(upd)
	if err != nil {
		return
	}
	_ = s.conn.Notify(context.Background(), acp.MethodSessionUpdate, acp.SessionNotification{
		SessionID: sessionID,
		Update:    raw,
	})
}

func (s *server) reply(id json.RawMessage, result any) {
	_ = s.conn.Reply(id, result)
}

func promptText(params json.RawMessage) string {
	var p acp.PromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	var out string
	for _, b := range p.Prompt {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}
