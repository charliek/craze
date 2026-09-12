package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/acp"
)

const fakeSessionID = "fake-session-1"

// taskRunFor is how long the sub-agent tool stays in_progress. A real
// sub-agent runs for seconds; without a pause here the running row lives
// for less than one render frame, so no terminal ever paints it and the
// tmux smoke's "● agent then ✓ agent" expectation is vacuous.
const taskRunFor = 250 * time.Millisecond

type server struct {
	conn   *acp.Conn
	script string

	mu        sync.Mutex
	promptN   int
	cancelled atomic.Bool
	hangWait  chan struct{}
	config    []map[string]any
	// order is every session/set_mode and session/prompt in arrival order, with
	// the mode each set_mode asked for. It is what lets the planmode scripts
	// prove craze chained the two rather than racing them; recording it anywhere
	// but the read loop would record the order the handler goroutines happened
	// to wake in.
	order []orderEntry
}

// orderEntry is one recorded call. mode is the mode a set_mode asked for and is
// empty for a prompt. Method names alone cannot answer the question the goldens
// rest on — a prompt whose own mode change arrived first, and to which mode —
// because a set_mode belongs to the prompt that follows it and only the mode it
// asked for says whether that prompt is an implement turn.
type orderEntry struct {
	method string
	mode   string
}

const (
	orderSetMode = "set_mode"
	orderPrompt  = "prompt"
	// implementModeID is the mode craze switches to on its way out of plan mode,
	// so a prompt preceded by a set_mode to it is the implement turn.
	implementModeID = "agent"
)

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
		{
			// Cursor's fast toggle, verbatim from the live captures: string
			// values, and two U+200B in the on label.
			"id":           "fast",
			"name":         "Fast",
			"category":     "model_config",
			"type":         "select",
			"currentValue": "false",
			"options": []map[string]string{
				{"value": "false", "name": "Off"},
				{"value": "true", "name": "Fast\u200b\u200b"},
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
		// The plan-exit scripts have to start where the offer can be made.
		currentMode := "agent"
		if planExitScript(s.script) {
			currentMode = "plan"
		}
		s.reply(msg.ID, map[string]any{
			"sessionId": fakeSessionID,
			"modes": map[string]any{
				"currentModeId": currentMode,
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
		s.noteOrder(orderPrompt, "")
		go s.handlePrompt(msg)
	case acp.MethodSessionSetModel:
		s.reply(msg.ID, map[string]any{})
	case acp.MethodSessionSetMode:
		var p acp.SetModeParams
		_ = json.Unmarshal(msg.Params, &p)
		// Still on the read loop, so the mode is recorded in arrival order.
		s.noteOrder(orderSetMode, p.ModeID)
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
	case "todos":
		s.todos(msg.ID, true)
	case "todos-notify":
		s.todos(msg.ID, false)
	case "diff":
		s.diff(msg.ID)
	case "bigdiff":
		s.bigdiff(msg.ID)
	case "bash":
		s.bash(msg.ID)
	case "task":
		s.task(msg.ID, false)
	case "task-late":
		s.task(msg.ID, true)
	case "markdown":
		s.markdown(msg.ID)
	case "title":
		s.title(msg.ID, text)
	case "planmode":
		s.planmode(msg.ID, text, n)
	case "planmode-card":
		s.planmodeCard(msg.ID, text, n)
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
		ToolCallID: askToolCallID,
		Title:      "Question",
		Questions: []acp.AskQuestion{
			{
				ID:     "q1",
				Prompt: "Pick one",
				Options: []acp.AskOption{
					{ID: "opt-a", Label: "A"},
					{ID: "opt-b", Label: "B"},
				},
			},
			{
				ID:            "q2",
				Prompt:        "Pick any",
				AllowMultiple: true,
				Options: []acp.AskOption{
					{ID: "opt-x", Label: "X"},
					{ID: "opt-y", Label: "Y"},
					{ID: "opt-z", Label: "Z"},
				},
			},
		},
	}
	var result struct {
		Outcome struct {
			Outcome string `json:"outcome"`
			Answers []struct {
				QuestionID        string   `json:"questionId"`
				SelectedOptionIDs []string `json:"selectedOptionIds"`
			} `json:"answers"`
		} `json:"outcome"`
	}
	err := s.conn.Call(context.Background(), acp.MethodCursorAskQuestion, params, &result)
	if err != nil || result.Outcome.Outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	picks := make([]string, 0, len(result.Outcome.Answers))
	for _, a := range result.Outcome.Answers {
		picks = append(picks, a.QuestionID+"="+strings.Join(a.SelectedOptionIDs, ","))
	}
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content: &acp.ContentBlock{
			Type: "text",
			Text: "asked:" + result.Outcome.Outcome + ":" + strings.Join(picks, ";"),
		},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) plan(id json.RawMessage) {
	outcome, err := s.createPlan(map[string]any{
		"name":     "Fake Plan",
		"overview": "Two steps, then stop.",
		"plan":     "## Steps\n\n- read main.go\n- edit main.go\n",
		"todos": []map[string]string{
			{"id": "1", "content": "Read main.go", "status": "pending"},
			{"id": "2", "content": "Edit main.go", "status": "pending"},
		},
	})
	if err != nil || outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	s.say("planned:" + outcome)
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// createPlan raises a cursor/create_plan card and returns the outcome the
// client answered with. Conn.Call blocks until that answer arrives, which is
// what makes a card a card.
func (s *server) createPlan(params map[string]any) (string, error) {
	var result struct {
		Outcome struct {
			Outcome string `json:"outcome"`
		} `json:"outcome"`
	}
	if err := s.conn.Call(context.Background(), acp.MethodCursorCreatePlan, params, &result); err != nil {
		return "", err
	}
	return result.Outcome.Outcome, nil
}

// say is one agent_message_chunk, the smallest thing a turn can put in the
// transcript.
func (s *server) say(text string) {
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: text},
	})
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

// Real cursor toolCallIds carry a literal newline; the new scripts keep that
// shape so craze is exercised against it.
const (
	askToolCallID  = "call-ask-0\nfc_ask"
	todoToolCallID = "call-todo-0\nfc_todo"
	readToolCallID = "call-read-1\nfc_read"
	editToolCallID = "call-edit-2\nfc_edit"
	bashToolCallID = "call-bash-3\nfc_bash"
	taskToolCallID = "call-task-4\nfc_task"
)

// todos drives the cursor/update_todos stream: a full list, then a merge that
// closes #1 and starts #2. asRequest picks the envelope.
func (s *server) todos(id json.RawMessage, asRequest bool) {
	s.sendTodos(asRequest, false, []map[string]string{
		{"id": "1", "content": "Read main.go", "status": "in_progress"},
		{"id": "2", "content": "Edit main.go", "status": "pending"},
		{"id": "3", "content": "Run go vet", "status": "pending"},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "planning"},
	})
	s.sendTodos(asRequest, true, []map[string]string{
		{"id": "1", "content": "Read main.go", "status": "completed"},
		{"id": "2", "content": "Edit main.go", "status": "in_progress"},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "done todos"},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) sendTodos(asRequest, merge bool, todos []map[string]string) {
	params := map[string]any{
		"toolCallId": todoToolCallID,
		"todos":      todos,
		"merge":      merge,
	}
	if !asRequest {
		_ = s.conn.Notify(context.Background(), acp.MethodCursorUpdateTodos, params)
		return
	}
	var result struct {
		Outcome struct {
			Outcome string `json:"outcome"`
		} `json:"outcome"`
	}
	if err := s.conn.Call(context.Background(), acp.MethodCursorUpdateTodos, params, &result); err != nil {
		fmt.Fprintf(os.Stderr, "craze-fake-agent: update_todos failed: %v\n", err)
		return
	}
	if result.Outcome.Outcome != "accepted" {
		fmt.Fprintf(os.Stderr, "craze-fake-agent: update_todos outcome %q\n", result.Outcome.Outcome)
	}
}

const (
	diffOldText = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	diffNewText = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello, world\")\n}\n"
)

// diff emits a read tool that returns file content and an edit tool that
// completes with a diff item changing one of six lines.
func (s *server) diff(id json.RawMessage) {
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    readToolCallID,
		"title":         "Read File",
		"kind":          "read",
		"status":        "pending",
		"rawInput":      map[string]any{},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    readToolCallID,
		"title":         "Read main.go",
		"rawInput":      map[string]string{"path": "/tmp/ws/main.go"},
		"locations":     []map[string]string{{"path": "/tmp/ws/main.go"}},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    readToolCallID,
		"status":        "completed",
		"rawOutput":     map[string]string{"content": diffOldText},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    editToolCallID,
		"title":         "Edit File",
		"kind":          "edit",
		"status":        "pending",
		"rawInput":      map[string]any{},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    editToolCallID,
		"title":         "Edit `/tmp/ws/main.go`",
		"rawInput":      map[string]string{"path": "/tmp/ws/main.go"},
		"locations":     []map[string]string{{"path": "/tmp/ws/main.go"}},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    editToolCallID,
		"status":        "completed",
		"content": []map[string]any{{
			"type":    "diff",
			"path":    "/tmp/ws/main.go",
			"oldText": diffOldText,
			"newText": diffNewText,
		}},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "done diff"},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// bigdiff changes one line of a 200 KiB file so the counts stay checkable
// while both sides blow past the per-side cap.
func (s *server) bigdiff(id json.RawMessage) {
	oldText := bigFile("line")
	newText := bigFile("line") + "tail changed\n"
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    editToolCallID,
		"title":         "Edit `/tmp/ws/big.txt`",
		"kind":          "edit",
		"status":        "in_progress",
		"rawInput":      map[string]string{"path": "/tmp/ws/big.txt"},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    editToolCallID,
		"status":        "completed",
		"content": []map[string]any{{
			"type":    "diff",
			"path":    "/tmp/ws/big.txt",
			"oldText": oldText,
			"newText": newText,
		}},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "done bigdiff"},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func bigFile(prefix string) string {
	var b strings.Builder
	for i := 0; b.Len() < 200*1024; i++ {
		fmt.Fprintf(&b, "%s %06d %s\n", prefix, i, strings.Repeat("x", 32))
	}
	return b.String()
}

// bash completes an execute tool the way cursor does: output only at the end.
func (s *server) bash(id json.RawMessage) {
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    bashToolCallID,
		"title":         "`go vet ./...`",
		"kind":          "execute",
		"status":        "pending",
		"rawInput":      map[string]string{"command": "go vet ./..."},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    bashToolCallID,
		"status":        "in_progress",
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    bashToolCallID,
		"status":        "completed",
		"rawOutput": map[string]any{
			"exitCode": 127,
			"stdout":   "",
			"stderr":   "Command 'go' not found, but can be installed with:\nsudo apt install golang-go\n",
		},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "done bash"},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// task runs a sub-agent tool with its cursor/task receipt. late sends the
// receipt before the tool_call, the order craze must also survive.
func (s *server) task(id json.RawMessage, late bool) {
	if late {
		s.taskReceipt()
	}
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCall,
		"toolCallId":    taskToolCallID,
		"title":         "Task: Count main.go lines",
		"kind":          "other",
		"status":        "pending",
		"rawInput": map[string]any{
			"_toolName":    "task",
			"prompt":       "Count the number of lines in main.go and report only the number.",
			"description":  "Count main.go lines",
			"subagentType": map[string]any{"unspecified": map[string]any{}},
		},
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    taskToolCallID,
		"status":        "in_progress",
	})
	time.Sleep(taskRunFor)
	if !late {
		s.taskReceipt()
	}
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateToolCallUpd,
		"toolCallId":    taskToolCallID,
		"status":        "completed",
		"rawOutput":     map[string]any{"durationMs": 8010, "isBackground": false},
	})
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "done task"},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) taskReceipt() {
	params := map[string]any{
		"toolCallId":   taskToolCallID,
		"description":  "Count main.go lines",
		"prompt":       "Count the number of lines in main.go and report only the number.",
		"subagentType": map[string]any{"custom": map[string]any{"unspecified": map[string]any{}}},
		"model":        "cursor-grok-4.6-high-fast",
		"agentId":      "agent-1234",
		"durationMs":   8010,
	}
	var result struct {
		Outcome struct {
			Outcome string `json:"outcome"`
		} `json:"outcome"`
	}
	if err := s.conn.Call(context.Background(), acp.MethodCursorTask, params, &result); err != nil {
		fmt.Fprintf(os.Stderr, "craze-fake-agent: cursor/task failed: %v\n", err)
		return
	}
	if result.Outcome.Outcome != "completed" {
		fmt.Fprintf(os.Stderr, "craze-fake-agent: cursor/task outcome %q\n", result.Outcome.Outcome)
	}
}

const markdownReply = "## Heading\n\n" +
	"- first item\n- second item\n- third item\n\n" +
	"```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n\n" +
	"Inline `code` and **bold** together. " +
	"Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor " +
	"incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud " +
	"exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute.\n\n" +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
	"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" +
	"cccccccccccccccccccccccccccccccccccccccccccccccccc\n"

// markdown also opens with a thought run: it is the only script that emits
// agent_thought_chunk, so the collapse/expand path has something to show.
func (s *server) markdown(id json.RawMessage) {
	for _, part := range []string{"Let me think about ", "the shape of this reply."} {
		s.update(fakeSessionID, acp.SessionUpdate{
			SessionUpdate: acp.UpdateAgentThought,
			Content:       &acp.ContentBlock{Type: "text", Text: part},
		})
	}
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: markdownReply},
	})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// noteOrder records one of the two calls the planmode scripts compare. mode is
// the mode a set_mode asked for, and empty for a prompt.
func (s *server) noteOrder(method, mode string) {
	s.mu.Lock()
	s.order = append(s.order, orderEntry{method: method, mode: mode})
	s.mu.Unlock()
}

// promptMode answers the only question the planmode goldens rest on: did the nth
// session/prompt's *own* mode change arrive before it, and which mode did it ask
// for? A set_mode belongs to the prompt that follows it, so only the ones that
// arrived since the previous prompt count — an earlier turn's set_mode says
// nothing about this one, and the last of several is the one in force. mode is
// empty when nothing preceded this prompt. overtaken is the race the chained
// command exists to prevent: the mode change meant for this prompt arrived
// *after* it, with no prompt of its own in between.
func (s *server) promptMode(n int) (mode string, overtaken bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prompts := 0
	last := "" // the newest set_mode since the previous prompt
	for _, e := range s.order {
		if e.method == orderSetMode {
			if prompts == n {
				overtaken = overtaken || (mode == "" && e.mode == implementModeID)
				continue
			}
			last = e.mode
			continue
		}
		prompts++
		if prompts == n {
			mode, last = last, ""
			continue
		}
		if prompts > n {
			// A later prompt owns everything after it.
			break
		}
		last = ""
	}
	return mode, overtaken
}

// planTurn is what a planmode prompt is, decided by the mode change that came
// with it.
type planTurn int

const (
	// planTurnPlan has no mode change of its own: the first plan, or a
	// refinement typed while still in plan mode.
	planTurnPlan planTurn = iota
	// planTurnImplement is the plan exit: its own set_mode to the implement mode
	// arrived first, which is what the chained command guarantees.
	planTurnImplement
	// planTurnRaced is a prompt that overtook the mode change meant for it.
	planTurnRaced
)

// planTurnFor classifies the nth prompt. A set_mode that arrived first but asked
// for some other mode — plan again, or ask — leaves the session planning, so the
// turn is a plan turn and not an implement one.
func (s *server) planTurnFor(n int) planTurn {
	mode, overtaken := s.promptMode(n)
	switch {
	case mode == implementModeID:
		return planTurnImplement
	case overtaken:
		return planTurnRaced
	default:
		return planTurnPlan
	}
}

// planmode is the plan-exit script. Leaving plan mode must be a set_mode to the
// implement mode followed by a prompt, so a turn that arrives after one answers
// "implementing", a turn with no mode change of its own is a refinement and
// answers "planned", and a turn that overtook its own set_mode — the race the
// chained command exists to prevent — answers WRONG ORDER and fails the golden.
func (s *server) planmode(id json.RawMessage, text string, n int) {
	reply := "planned: " + text
	switch s.planTurnFor(n) {
	case planTurnImplement:
		reply = "implementing: " + text
	case planTurnRaced:
		reply = "WRONG ORDER"
	}
	s.say(reply)
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// planExitScript reports whether a script's session starts in plan mode, which
// is where the plan-exit offer can be reached at all.
func planExitScript(script string) bool {
	switch script {
	case "planmode", "planmode-card":
		return true
	}
	return false
}

// planmodeCard is the plan-mode turn cursor actually sends, which `planmode`
// alone does not model. Driving a real cursor-agent (005's
// live/{acp.out,step2-plan-offer.txt}) showed a plan turn answering with
// assistant text, then a cursor/create_plan card, then more assistant text, and
// only then the turn's ending — so the card always lands *before* the events
// that arm the plan offer. That order is what made craze retire the offer
// before it could ever be made, and this script is the wire-level guard on the
// fix: the offer survives the card and stands once the card has been answered.
//
// One honest difference from the live turn: Conn.Call blocks until the card is
// answered, so the chunk after the card follows it on the wire (as live) but
// reaches craze once the card is gone rather than while it is still up. The
// ordering the offer depends on — card before the ending — is the same either
// way.
//
// The implement turn gets no card, because cursor sent create_plan for the plan
// turn only; the ordering rules are `planmode`'s, so WRONG ORDER still catches a
// prompt that overtook its own set_mode.
func (s *server) planmodeCard(id json.RawMessage, text string, n int) {
	switch s.planTurnFor(n) {
	case planTurnImplement:
		s.say("implementing: " + text)
		s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
		return
	case planTurnRaced:
		s.say("WRONG ORDER")
		s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
		return
	}
	s.say("drafting: " + text)
	// Name, overview and body, the three fields the live card carried; todos is
	// empty there too, so the plan turn draws a card and not a task panel.
	outcome, err := s.createPlan(map[string]any{
		"name":     "Print current time",
		"overview": "Change main.go so it prints the current time instead of hi.",
		"plan":     "# Print current time\n\nReplace the `hi` print with the current time. No other files.\n",
		"todos":    []map[string]string{},
	})
	if err != nil || outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	s.say("planned: " + text)
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

func (s *server) title(id json.RawMessage, text string) {
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": acp.UpdateSessionInfo,
		"title":         "Fake Title",
	})
	s.echo(id, text)
}
