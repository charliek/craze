package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Fake Codex app-server v2 JSON-RPC server (Plan 054 § A5).
// Auto-activated when os.Args contains "app-server" or CRAZE_FAKE_SCRIPT starts
// with "codex-". Intentionally omits `"jsonrpc":"2.0"` on server->client frames
// and starts server request IDs at integer `0` to match real codex-cli 0.172.0.

type codexFrame struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  any              `json:"error,omitempty"`
}

type codexServer struct {
	script string

	encMu  sync.Mutex
	stdout *bufio.Writer

	mu          sync.Mutex
	threadID    string
	turnSeq     int
	activeTurn  string
	turnCancel  map[string]chan struct{}
	nextReqID   int64
	pendingReqs map[string]chan codexFrame
	cwd         string
	lastMode    string
	lastModel   string
	lastEffort  string
	lastSandbox string
	lastApprove string
	steeredText []string
}

func runCodex(script string) error {
	s := &codexServer{
		script:      script,
		stdout:      bufio.NewWriter(os.Stdout),
		turnCancel:  make(map[string]chan struct{}),
		pendingReqs: make(map[string]chan codexFrame),
		lastModel:   "gpt-6.2-sol",
		lastEffort:  "low",
	}
	if line := os.Getenv("CRAZE_FAKE_STDERR"); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg codexFrame
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Method == "" && msg.ID != nil {
			s.deliverReply(msg)
			continue
		}
		if msg.Method != "" {
			s.recordCall(msg.Method)
			s.handleIncoming(msg)
		}
	}
	if os.Getenv("CRAZE_FAKE_LINGER") == "1" {
		time.Sleep(30 * time.Second)
	}
	return scanner.Err()
}

func (s *codexServer) recordCall(method string) {
	p := os.Getenv("CRAZE_FAKE_DUMP_CALLS")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, method)
	_ = f.Close()
}

func (s *codexServer) writeJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.encMu.Lock()
	defer s.encMu.Unlock()
	_, _ = s.stdout.Write(append(b, '\n'))
	_ = s.stdout.Flush()
}

// reply omits "jsonrpc" on purpose to mirror codex-cli 0.172.0.
func (s *codexServer) reply(id *json.RawMessage, result any) {
	if id == nil {
		return
	}
	s.writeJSON(map[string]any{
		"id":     *id,
		"result": result,
	})
}

func (s *codexServer) replyErr(id *json.RawMessage, code int, msg string) {
	if id == nil {
		return
	}
	s.writeJSON(map[string]any{
		"id": *id,
		"error": map[string]any{
			"code":    code,
			"message": msg,
		},
	})
}

func (s *codexServer) notify(method string, params any) {
	s.writeJSON(map[string]any{
		"method":      method,
		"params":      params,
		"emittedAtMs": time.Now().UnixMilli(),
	})
}

func (s *codexServer) sendRequest(cancelCh <-chan struct{}, method string, params any) (codexFrame, bool, bool) {
	s.mu.Lock()
	id := s.nextReqID
	s.nextReqID++
	key := fmt.Sprintf("%d", id)
	ch := make(chan codexFrame, 1)
	s.pendingReqs[key] = ch
	s.mu.Unlock()

	s.writeJSON(map[string]any{
		"method": method,
		"id":     id,
		"params": params,
	})
	select {
	case resp, ok := <-ch:
		if ok {
			s.notify("serverRequest/resolved", map[string]any{
				"threadId":  s.getThreadID(),
				"requestId": id,
			})
		}
		return resp, ok, false
	case <-cancelCh:
		s.mu.Lock()
		_, stillPending := s.pendingReqs[key]
		if stillPending {
			delete(s.pendingReqs, key)
		}
		s.mu.Unlock()
		if !stillPending {
			// Reply raced in and already removed the pending entry; drain it
			// and still emit serverRequest/resolved once.
			resp, ok := <-ch
			if ok {
				s.notify("serverRequest/resolved", map[string]any{
					"threadId":  s.getThreadID(),
					"requestId": id,
				})
				return resp, true, false
			}
			return codexFrame{}, false, true
		}
		s.notify("serverRequest/resolved", map[string]any{
			"threadId":  s.getThreadID(),
			"requestId": id,
		})
		return codexFrame{}, false, true
	}
}

func (s *codexServer) deliverReply(msg codexFrame) {
	key := strings.TrimSpace(string(*msg.ID))
	s.mu.Lock()
	ch, ok := s.pendingReqs[key]
	if ok {
		delete(s.pendingReqs, key)
	}
	s.mu.Unlock()
	if ok {
		ch <- msg
	}
}

func (s *codexServer) getThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

func (s *codexServer) handleIncoming(msg codexFrame) {
	switch msg.Method {
	case "initialize":
		s.reply(msg.ID, map[string]any{
			"userAgent":      "craze-fake-codex/0.172.0",
			"codexHome":      "/tmp/fake-codex-home",
			"platformFamily": "unix",
			"platformOs":     "linux",
		})
	case "initialized":
		// Notification, no reply.
	case "account/read":
		switch s.script {
		case "codex-loggedout":
			s.reply(msg.ID, map[string]any{
				"account":            nil,
				"requiresOpenaiAuth": true,
			})
		case "codex-apikey", "codex-subscription-refused":
			s.reply(msg.ID, map[string]any{
				"account":            map[string]any{"type": "apiKey"},
				"requiresOpenaiAuth": true,
			})
		case "codex-custom-provider":
			s.reply(msg.ID, map[string]any{
				"account":            nil,
				"requiresOpenaiAuth": false,
			})
		default:
			s.reply(msg.ID, map[string]any{
				"account": map[string]any{
					"type":     "chatgpt",
					"planType": "prolite",
				},
				"requiresOpenaiAuth": true,
			})
		}
	case "config/read":
		provider := "openai"
		if s.script == "codex-paid-endpoint" {
			provider = "custom"
		}
		s.reply(msg.ID, map[string]any{"config": map[string]any{"model_provider": provider}})
	case "account/rateLimits/read":
		if s.script == "codex-slow-ratelimits" {
			// Deliberately never reply to test independent rate-limit startup deadline.
			return
		}
		if s.script == "codex-apikey" || s.script == "codex-custom-provider" || s.script == "codex-loggedout" {
			s.replyErr(msg.ID, -32600, "codex account authentication required to read rate limits")
			return
		}
		s.reply(msg.ID, map[string]any{
			"rateLimits": map[string]any{
				"limitId":  "codex",
				"planType": "prolite",
				"primary": map[string]any{
					"usedPercent":        33,
					"windowDurationMins": 10080,
					"resetsAt":           time.Now().Add(72 * time.Hour).Unix(),
				},
				"secondary": nil,
			},
		})
	case "model/list":
		var mp struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(msg.Params, &mp)
		if s.script == "codex-model-loop-cursor" {
			s.reply(msg.ID, map[string]any{
				"data": []map[string]any{
					{"id": "gpt-6.2-sol", "model": "gpt-6.2-sol", "displayName": "gpt-6.2-sol", "isDefault": true, "defaultReasoningEffort": "low"},
				},
				"nextCursor": "repeat-cur",
			})
			return
		}
		if s.script == "codex-model-multipage" {
			if mp.Cursor == "" {
				s.reply(msg.ID, map[string]any{
					"data": []map[string]any{
						{"id": "gpt-6.2-sol", "model": "gpt-6.2-sol", "displayName": "gpt-6.2-sol", "isDefault": true, "defaultReasoningEffort": "low", "supportedReasoningEfforts": []map[string]any{{"reasoningEffort": "low"}, {"reasoningEffort": "high"}}},
					},
					"nextCursor": "page-2",
				})
				return
			}
			s.reply(msg.ID, map[string]any{
				"data": []map[string]any{
					{"id": "gpt-6.2-astra", "model": "gpt-6.2-astra", "displayName": "gpt-6.2-astra", "isDefault": false, "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []map[string]any{{"reasoningEffort": "medium"}}},
				},
				"nextCursor": nil,
			})
			return
		}
		s.reply(msg.ID, map[string]any{
			"data": []map[string]any{
				{
					"id":                     "gpt-6.2-sol",
					"model":                  "gpt-6.2-sol",
					"displayName":            "gpt-6.2-sol",
					"isDefault":              true,
					"hidden":                 false,
					"defaultReasoningEffort": "low",
					"supportedReasoningEfforts": []map[string]any{
						{"reasoningEffort": "low", "description": "Fast"},
						{"reasoningEffort": "medium", "description": "Balanced"},
						{"reasoningEffort": "high", "description": "Deep"},
						{"reasoningEffort": "ultra", "description": "Ultra"},
					},
				},
				{
					"id":                     "gpt-6.2-astra",
					"model":                  "gpt-6.2-astra",
					"displayName":            "gpt-6.2-astra",
					"isDefault":              false,
					"hidden":                 false,
					"defaultReasoningEffort": "low",
					"supportedReasoningEfforts": []map[string]any{
						{"reasoningEffort": "low", "description": "Fast"},
						{"reasoningEffort": "medium", "description": "Balanced"},
						{"reasoningEffort": "high", "description": "Deep"},
					},
				},
				{
					"id":                     "gpt-6-luna",
					"model":                  "gpt-6-luna",
					"displayName":            "gpt-6-luna",
					"isDefault":              false,
					"hidden":                 false,
					"defaultReasoningEffort": "medium",
					"supportedReasoningEfforts": []map[string]any{
						{"reasoningEffort": "low", "description": "Fast"},
						{"reasoningEffort": "medium", "description": "Balanced"},
						{"reasoningEffort": "high", "description": "Deep"},
					},
				},
				{
					"id":                     "gpt-5-hidden",
					"model":                  "gpt-5-hidden",
					"displayName":            "gpt-5-hidden",
					"isDefault":              false,
					"hidden":                 true,
					"defaultReasoningEffort": "low",
					"supportedReasoningEfforts": []map[string]any{
						{"reasoningEffort": "low", "description": "Fast"},
					},
				},
			},
			"nextCursor": nil,
		})
	case "skills/list":
		var params struct {
			Cwds []string `json:"cwds"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		cwd := ""
		if len(params.Cwds) > 0 {
			cwd = params.Cwds[0]
		}
		skills := []map[string]any{
			{
				"name":             "fake-echo",
				"description":      "Echo via codex skill",
				"shortDescription": "Echo via codex skill",
				"path":             filepath.Join(cwd, ".agents", "skills", "fake-echo", "SKILL.md"),
				"scope":            "repo",
				"enabled":          true,
			},
			{
				"name":        "disabled-skill",
				"description": "Should be filtered out",
				"path":        filepath.Join(cwd, ".agents", "skills", "disabled-skill", "SKILL.md"),
				"scope":       "repo",
				"enabled":     false,
			},
		}
		s.reply(msg.ID, map[string]any{
			"data": []map[string]any{
				{"cwd": cwd, "skills": skills},
			},
		})
	case "collaborationMode/list":
		s.reply(msg.ID, map[string]any{
			"data": []map[string]any{
				{"name": "Plan", "mode": "plan", "model": nil, "reasoning_effort": "medium"},
				{"name": "Default", "mode": "default", "model": nil, "reasoning_effort": nil},
			},
		})
	case "thread/start":
		var p struct {
			Cwd            string `json:"cwd"`
			Sandbox        string `json:"sandbox"`
			ApprovalPolicy string `json:"approvalPolicy"`
			Model          string `json:"model"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		tid := os.Getenv("CRAZE_FAKE_SESSION_ID")
		if tid == "" {
			tid = "codex-thread-1"
		}
		if p.Cwd != "" {
			tid = strings.ReplaceAll(tid, "{dir}", filepath.Base(p.Cwd))
		}
		model := p.Model
		if model == "" {
			model = "gpt-6.2-sol"
		}
		s.mu.Lock()
		s.threadID = tid
		s.cwd = p.Cwd
		s.lastModel = model
		s.lastSandbox = p.Sandbox
		s.lastApprove = p.ApprovalPolicy
		s.mu.Unlock()
		sandboxObj := map[string]any{
			"type":                "workspaceWrite",
			"writableRoots":       []string{},
			"networkAccess":       false,
			"excludeTmpdirEnvVar": false,
			"excludeSlashTmp":     false,
		}
		if p.Sandbox == "read-only" {
			sandboxObj = map[string]any{"type": "readOnly", "networkAccess": false}
		}
		s.reply(msg.ID, map[string]any{
			"thread":          map[string]any{"id": tid, "cwd": p.Cwd, "model": model, "turns": []any{}},
			"model":           model,
			"modelProvider":   "openai",
			"approvalPolicy":  p.ApprovalPolicy,
			"sandbox":         sandboxObj,
			"reasoningEffort": "low",
		})
		s.notify("thread/started", map[string]any{
			"thread": map[string]any{"id": tid, "cwd": p.Cwd, "model": model, "turns": []any{}},
		})
	case "thread/resume":
		var p struct {
			ThreadID       string `json:"threadId"`
			ExcludeTurns   bool   `json:"excludeTurns"`
			Cwd            string `json:"cwd"`
			Sandbox        string `json:"sandbox"`
			ApprovalPolicy string `json:"approvalPolicy"`
			Model          string `json:"model"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if s.script == "codex-resume-missing" || strings.Contains(p.ThreadID, "missing") {
			s.replyErr(msg.ID, -32600, "no rollout found for thread id "+p.ThreadID)
			return
		}
		model := p.Model
		if model == "" {
			model = "gpt-6.2-astra"
		}
		s.mu.Lock()
		s.threadID = p.ThreadID
		s.cwd = p.Cwd
		s.lastModel = model
		s.lastEffort = "medium"
		s.lastSandbox = p.Sandbox
		s.lastApprove = p.ApprovalPolicy
		s.mu.Unlock()
		sandboxObj := map[string]any{
			"type":                "workspaceWrite",
			"writableRoots":       []string{},
			"networkAccess":       false,
			"excludeTmpdirEnvVar": false,
			"excludeSlashTmp":     false,
		}
		if p.Sandbox == "read-only" {
			sandboxObj = map[string]any{"type": "readOnly", "networkAccess": false}
		}
		s.reply(msg.ID, map[string]any{
			"thread":          map[string]any{"id": p.ThreadID, "cwd": p.Cwd, "model": model, "reasoningEffort": "medium", "turns": []any{}},
			"model":           model,
			"modelProvider":   "openai",
			"approvalPolicy":  p.ApprovalPolicy,
			"sandbox":         sandboxObj,
			"reasoningEffort": "medium",
		})
		s.notify("thread/tokenUsage/updated", map[string]any{
			"threadId": p.ThreadID,
			"turnId":   "turn-prev-2",
			"tokenUsage": map[string]any{
				"total":              map[string]any{"totalTokens": 1000, "inputTokens": 900, "cachedInputTokens": 400, "cacheWriteInputTokens": 0, "outputTokens": 100, "reasoningOutputTokens": 20},
				"last":               map[string]any{"totalTokens": 500, "inputTokens": 450, "cachedInputTokens": 200, "cacheWriteInputTokens": 0, "outputTokens": 50, "reasoningOutputTokens": 10},
				"modelContextWindow": 258400,
			},
		})
	case "thread/turns/list":
		var p struct {
			ThreadID string `json:"threadId"`
			Cursor   string `json:"cursor"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Cursor == "" {
			// Page 1 (newest first): turn-2 (interrupted command row)
			next := "page-2"
			s.reply(msg.ID, map[string]any{
				"data": []map[string]any{
					{
						"id":        "turn-prev-2",
						"status":    "interrupted",
						"itemsView": "full",
						"items": []map[string]any{
							{"type": "userMessage", "id": "u-2", "content": []map[string]any{{"type": "text", "text": "second question"}}},
							{"type": "commandExecution", "id": "cmd-2", "command": "sleep 30", "status": "inProgress"},
							{"type": "agentMessage", "id": "a-2", "text": "second answer", "phase": "final_answer"},
						},
					},
				},
				"nextCursor": next,
			})
		} else {
			// Page 2 (oldest): turn-1
			s.reply(msg.ID, map[string]any{
				"data": []map[string]any{
					{
						"id":        "turn-prev-1",
						"status":    "completed",
						"itemsView": "full",
						"items": []map[string]any{
							{"type": "userMessage", "id": "u-1", "content": []map[string]any{{"type": "text", "text": "first question"}}},
							{"type": "reasoning", "id": "r-1", "summary": []string{"thought about first"}},
							{"type": "agentMessage", "id": "a-1", "text": "first answer", "phase": "final_answer"},
						},
					},
				},
				"nextCursor": nil,
			})
		}
	case "thread/name/set":
		s.reply(msg.ID, map[string]any{})
	case "turn/start":
		s.handleTurnStart(msg)
	case "turn/interrupt":
		if s.script == "codex-hang-interrupt" {
			// Intentionally drop turn/interrupt without replying to exercise
			// bounded cancel timeout handling.
			return
		}
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		ch := s.turnCancel[p.TurnID]
		if ch != nil {
			delete(s.turnCancel, p.TurnID)
			close(ch)
		}
		s.mu.Unlock()
		s.reply(msg.ID, map[string]any{})
	case "turn/steer":
		var p struct {
			ThreadID       string `json:"threadId"`
			ExpectedTurnID string `json:"expectedTurnId"`
			Input          []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		active := s.activeTurn
		if active == "" || (p.ExpectedTurnID != "" && p.ExpectedTurnID != active) {
			s.mu.Unlock()
			s.replyErr(msg.ID, -32600, "no active turn to steer")
			return
		}
		var joined []string
		for _, in := range p.Input {
			if in.Text != "" {
				joined = append(joined, in.Text)
			}
		}
		steered := strings.Join(joined, " ")
		s.steeredText = append(s.steeredText, steered)
		s.mu.Unlock()
		s.reply(msg.ID, map[string]any{"turnId": active})
		// Real Codex echoes a userMessage item on steer; adapter suppresses live echo.
		s.notify("item/completed", map[string]any{
			"threadId": p.ThreadID,
			"turnId":   active,
			"item": map[string]any{
				"type":    "userMessage",
				"id":      "u-steer",
				"content": []map[string]any{{"type": "text", "text": steered}},
			},
		})
	default:
		s.replyErr(msg.ID, -32601, "Method not found: "+msg.Method)
	}
}

func (s *codexServer) handleTurnStart(msg codexFrame) {
	var p struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"input"`
		SandboxPolicy *struct {
			Type string `json:"type"`
		} `json:"sandboxPolicy"`
		ApprovalPolicy    string `json:"approvalPolicy"`
		CollaborationMode *struct {
			Mode     string `json:"mode"`
			Settings struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoning_effort"`
			} `json:"settings"`
		} `json:"collaborationMode"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if line := os.Getenv("CRAZE_FAKE_STDERR"); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	if dumpPath := os.Getenv("CRAZE_FAKE_DUMP_PROMPTS"); dumpPath != "" {
		if f, err := os.OpenFile(dumpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			b, _ := json.Marshal(map[string]any{"turnStart": json.RawMessage(msg.Params)})
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}

	s.mu.Lock()
	s.turnSeq++
	turnID := fmt.Sprintf("turn-%d", s.turnSeq)
	cancelCh := make(chan struct{})
	s.turnCancel[turnID] = cancelCh
	s.activeTurn = turnID
	s.steeredText = nil
	if p.CollaborationMode != nil {
		s.lastMode = p.CollaborationMode.Mode
		if p.CollaborationMode.Settings.Model != "" {
			s.lastModel = p.CollaborationMode.Settings.Model
		}
		if p.CollaborationMode.Settings.ReasoningEffort != "" {
			s.lastEffort = p.CollaborationMode.Settings.ReasoningEffort
		}
	}
	if p.SandboxPolicy != nil {
		s.lastSandbox = p.SandboxPolicy.Type
	}
	s.lastApprove = p.ApprovalPolicy
	turnCwd := s.cwd
	s.mu.Unlock()

	var textParts []string
	var skillNames []string
	var imagePaths []string
	for _, in := range p.Input {
		switch in.Type {
		case "text":
			textParts = append(textParts, in.Text)
		case "skill":
			skillNames = append(skillNames, in.Name)
		case "localImage":
			imagePaths = append(imagePaths, in.Path)
		}
	}
	promptText := strings.Join(textParts, "\n")

	if s.script == "codex-completed-before-reply" {
		// Emit turn/started, delta, and turn/completed BEFORE sending the turn/start RPC reply.
		s.emitSimpleTurn(p.ThreadID, turnID, "early: "+promptText)
		s.reply(msg.ID, map[string]any{"turn": map[string]any{"id": turnID, "status": "completed"}})
		s.clearActiveTurn(turnID)
		return
	}

	s.reply(msg.ID, map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}})
	go s.runTurn(p.ThreadID, turnID, turnCwd, promptText, skillNames, imagePaths, p.ApprovalPolicy, p.SandboxPolicy, cancelCh)
}

func (s *codexServer) clearActiveTurn(turnID string) {
	s.mu.Lock()
	if s.activeTurn == turnID {
		s.activeTurn = ""
	}
	delete(s.turnCancel, turnID)
	s.mu.Unlock()
}

func (s *codexServer) emitSimpleTurn(threadID, turnID, replyText string) {
	s.notify("turn/started", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "inProgress"},
	})
	itemID := "msg-" + turnID
	s.notify("item/started", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"item":     map[string]any{"type": "agentMessage", "id": itemID, "text": "", "phase": "final_answer"},
	})
	s.notify("item/agentMessage/delta", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"itemId":   itemID,
		"delta":    replyText,
	})
	s.notify("item/completed", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"item":     map[string]any{"type": "agentMessage", "id": itemID, "text": replyText, "phase": "final_answer"},
	})
	s.notify("thread/tokenUsage/updated", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"tokenUsage": map[string]any{
			"total":              map[string]any{"totalTokens": 1200, "inputTokens": 1100, "cachedInputTokens": 600, "cacheWriteInputTokens": 0, "outputTokens": 100, "reasoningOutputTokens": 25},
			"last":               map[string]any{"totalTokens": 1200, "inputTokens": 1100, "cachedInputTokens": 600, "cacheWriteInputTokens": 0, "outputTokens": 100, "reasoningOutputTokens": 25},
			"modelContextWindow": 475000,
		},
	})
	s.notify("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "completed"},
	})
}

func (s *codexServer) runTurn(threadID, turnID, cwd, promptText string, skills, images []string, approval string, sandbox *struct {
	Type string `json:"type"`
}, cancelCh chan struct{}) {
	defer s.clearActiveTurn(turnID)

	// Trigger foreign turn test after a normal turn when requested.
	if s.script == "codex-foreign" && promptText == "TRIGGER_FOREIGN" {
		s.emitSimpleTurn(threadID, turnID, "done")
		s.clearActiveTurn(turnID)
		time.Sleep(20 * time.Millisecond)
		s.notify("turn/started", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": "foreign-turn-1", "status": "inProgress"},
		})
		time.Sleep(150 * time.Millisecond)
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": "foreign-turn-1", "status": "completed"},
		})
		return
	}

	s.notify("turn/started", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "inProgress"},
	})
	// Echo userMessage (real Codex always emits this; adapter suppresses live echo).
	s.notify("item/completed", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"item": map[string]any{
			"type":    "userMessage",
			"id":      "u-" + turnID,
			"content": []map[string]any{{"type": "text", "text": promptText}},
		},
	})

	switch s.script {
	case "codex-error-401":
		s.notify("error", map[string]any{
			"threadId":  threadID,
			"turnId":    turnID,
			"willRetry": true,
			"error": map[string]any{
				"message":        "unexpected status 401 Unauthorized",
				"codexErrorInfo": map[string]any{"responseStreamDisconnected": map[string]any{"httpStatusCode": 401}},
			},
		})
		return

	case "codex-quota", "codex-dirty-quota":
		if s.script == "codex-dirty-quota" {
			if err := os.WriteFile(filepath.Join(cwd, "review-mutation.txt"), []byte("changed\n"), 0600); err != nil {
				panic(err)
			}
		}
		s.notify("turn/completed", map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "failed", "error": map[string]any{"message": "account quota", "codexErrorInfo": "usageLimitExceeded"}}})
		return
	case "codex-retry-usagelimit":
		s.notify("error", map[string]any{
			"threadId":  threadID,
			"turnId":    turnID,
			"willRetry": true,
			"error": map[string]any{
				"message":        "usage limit reached",
				"codexErrorInfo": "usageLimitExceeded",
			},
		})
		return

	case "codex-retry-context":
		s.notify("error", map[string]any{
			"threadId":  threadID,
			"turnId":    turnID,
			"willRetry": true,
			"error": map[string]any{
				"message":        "context window exceeded",
				"codexErrorInfo": "contextWindowExceeded",
			},
		})
		return

	case "codex-exit-zero-midturn":
		s.emitMessage(threadID, turnID, "msg-"+turnID, "partial before exit 0")
		os.Exit(0)
		return

	case "codex-exit-nonzero-midturn":
		_, _ = fmt.Fprintln(os.Stderr, "fatal crash midturn")
		os.Exit(7)
		return

	case "codex-complete-then-exit":
		pDone := filepath.Join(cwd, "completed-before-eof.txt")
		s.notify("item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item": map[string]any{
				"type":   "fileChange",
				"id":     "fc-eof",
				"status": "completed",
				"changes": []map[string]any{
					{"path": pDone, "kind": map[string]any{"type": "add"}, "diff": "final\n"},
				},
			},
		})
		s.emitMessage(threadID, turnID, "msg-"+turnID, "finished right before exit")
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		os.Exit(0)
		return

	case "codex-permissions-req":
		resp, _, cancelled := s.sendRequest(cancelCh, "item/permissions/requestApproval", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"itemId":   "perm-1",
			"reason":   "need extra permissions",
		})
		if cancelled {
			s.notify("turn/completed", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "interrupted"},
			})
			return
		}
		s.emitMessage(threadID, turnID, "msg-"+turnID, "perm_reply="+string(resp.Result))
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		return

	case "codex-hang-interrupt":
		// Ignore turn/interrupt completely (never emit turn/completed) to test
		// bounded cancel failure.
		select {}

	case "codex-hang":
		s.notify("item/started", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "commandExecution", "id": "cmd-hang", "command": "sleep 60", "status": "inProgress"},
		})
		<-cancelCh
		// Like real codex-cli 0.172.0, omit item/completed for the interrupted command.
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "interrupted"},
		})
		return

	case "codex-steer":
		s.notify("item/started", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "commandExecution", "id": "cmd-steer", "command": "sleep 1", "status": "inProgress"},
		})
		// Wait briefly or until gate/cancel for steer to arrive.
		select {
		case <-cancelCh:
			s.notify("turn/completed", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "interrupted"},
			})
			return
		case <-time.After(150 * time.Millisecond):
		}
		s.notify("item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "commandExecution", "id": "cmd-steer", "command": "sleep 1", "status": "completed", "aggregatedOutput": "ok\n", "exitCode": 0},
		})
		s.mu.Lock()
		steered := append([]string(nil), s.steeredText...)
		s.mu.Unlock()
		msg := "done:" + strings.Join(steered, "|")
		s.emitMessage(threadID, turnID, "msg-"+turnID, msg)
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		return

	case "codex-approval":
		s.notify("item/started", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "commandExecution", "id": "cmd-1", "command": "touch out.txt", "status": "inProgress"},
		})
		resp, ok, cancelled := s.sendRequest(cancelCh, "item/commandExecution/requestApproval", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"itemId":   "cmd-1",
			"reason":   "Allow touch out.txt?",
			"command":  "touch out.txt",
			"availableDecisions": []any{
				"accept",
				map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []string{"touch", "out.txt"}}},
				"decline",
				"cancel",
			},
		})
		if cancelled {
			s.notify("turn/completed", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "interrupted"},
			})
			return
		}
		dec := "decline"
		if ok {
			var r struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal(resp.Result, &r)
			if r.Decision != "" {
				dec = r.Decision
			}
		}
		status := "completed"
		if dec == "decline" || dec == "cancel" {
			status = "declined"
		}
		s.notify("item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "commandExecution", "id": "cmd-1", "command": "touch out.txt", "status": status},
		})
		if dec == "cancel" {
			s.notify("turn/completed", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "interrupted"},
			})
			return
		}
		s.emitMessage(threadID, turnID, "msg-"+turnID, "decision="+dec)
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		return

	case "codex-filechange-approval":
		resp, ok, cancelled := s.sendRequest(cancelCh, "item/fileChange/requestApproval", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"itemId":   "fc-1",
			"reason":   "Allow editing foo.txt?",
		})
		if cancelled {
			s.notify("turn/completed", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "interrupted"},
			})
			return
		}
		dec := "decline"
		if ok {
			var r struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal(resp.Result, &r)
			if r.Decision != "" {
				dec = r.Decision
			}
		}
		s.emitMessage(threadID, turnID, "msg-"+turnID, "fc_decision="+dec)
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		return

	case "codex-ask", "codex-ask-secret", "codex-ask-freetext":
		q := map[string]any{
			"id":       "q1",
			"header":   "Choice",
			"question": "Pick a color?",
			"isOther":  true,
			"isSecret": s.script == "codex-ask-secret",
			"options": []map[string]any{
				{"label": "Red", "description": "Warm"},
				{"label": "Blue", "description": "Cool"},
			},
		}
		if s.script == "codex-ask-freetext" {
			q["options"] = nil
		}
		resp, _, cancelled := s.sendRequest(cancelCh, "item/tool/requestUserInput", map[string]any{
			"threadId":  threadID,
			"turnId":    turnID,
			"itemId":    "rui-1",
			"questions": []map[string]any{q},
		})
		if cancelled {
			s.notify("turn/completed", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "interrupted"},
			})
			return
		}
		s.emitMessage(threadID, turnID, "msg-"+turnID, "answers="+string(resp.Result))
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		return

	case "codex-child-thread":
		// Emit a collabAgentToolCall + child-thread text that must NOT bleed into main text.
		s.notify("item/started", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "collabAgentToolCall", "id": "sub-1", "tool": "spawn_agent", "status": "inProgress", "receiverThreadIds": []string{"child-thread-1"}, "prompt": "inspect code"},
		})
		s.notify("item/agentMessage/delta", map[string]any{
			"threadId": "child-thread-1",
			"turnId":   "child-turn-1",
			"itemId":   "child-msg-1",
			"delta":    "CHILD_SECRET_DO_NOT_BLEED",
		})
		s.notify("subAgentActivity", map[string]any{
			"threadId":       "child-thread-1",
			"parentThreadId": threadID,
			"subAgentId":     "sub-1",
			"status":         "completed",
			"activity":       "inspected",
		})
		s.notify("item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "collabAgentToolCall", "id": "sub-1", "tool": "spawn_agent", "status": "completed", "receiverThreadIds": []string{"child-thread-1"}},
		})
		s.emitMessage(threadID, turnID, "msg-"+turnID, "main-only")
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		return

	case "codex-edits":
		pAdd := filepath.Join(cwd, "added.txt")
		pUpd := filepath.Join(cwd, "updated.txt")
		pMov := filepath.Join(cwd, "moved.txt")
		pFail := filepath.Join(cwd, "failed.txt")
		s.notify("item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item": map[string]any{
				"type":   "fileChange",
				"id":     "fc-ok",
				"status": "completed",
				"changes": []map[string]any{
					{"path": pAdd, "kind": map[string]any{"type": "add"}, "diff": "hello\n"},
					{"path": pUpd, "kind": map[string]any{"type": "update", "move_path": pMov}, "diff": "@@ -1,1 +1,1 @@\n-old\n+new\n"},
				},
			},
		})
		s.notify("item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item": map[string]any{
				"type":   "fileChange",
				"id":     "fc-fail",
				"status": "declined",
				"changes": []map[string]any{
					{"path": pFail, "kind": map[string]any{"type": "add"}, "diff": "nope\n"},
				},
			},
		})
		s.emitMessage(threadID, turnID, "msg-"+turnID, "edited")
		s.notify("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
		return
	}

	// Default codex-basic: echo mode/sandbox/skills/images/text.
	sb := ""
	if sandbox != nil {
		sb = sandbox.Type
	}
	s.mu.Lock()
	mode := s.lastMode
	model := s.lastModel
	effort := s.lastEffort
	s.mu.Unlock()
	if mode == "plan" {
		// Stream a plan item to test plan delta dedup.
		s.notify("item/started", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "plan", "id": "plan-" + turnID, "text": ""},
		})
		planText := "Plan for: " + promptText
		s.notify("item/plan/delta", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"itemId":   "plan-" + turnID,
			"delta":    planText,
		})
		s.notify("item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "plan", "id": "plan-" + turnID, "text": planText},
		})
	} else {
		var meta []string
		meta = append(meta, fmt.Sprintf("mode=%s sandbox=%s approval=%s model=%s effort=%s", mode, sb, approval, model, effort))
		if len(skills) > 0 {
			meta = append(meta, "skills="+strings.Join(skills, ","))
		}
		if len(images) > 0 {
			meta = append(meta, fmt.Sprintf("images=%d", len(images)))
		}
		s.emitMessage(threadID, turnID, "msg-"+turnID, strings.Join(meta, " ")+"\n"+promptText)
	}
	s.notify("thread/tokenUsage/updated", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"tokenUsage": map[string]any{
			"total":              map[string]any{"totalTokens": 1200, "inputTokens": 1100, "cachedInputTokens": 600, "cacheWriteInputTokens": 0, "outputTokens": 100, "reasoningOutputTokens": 25},
			"last":               map[string]any{"totalTokens": 1200, "inputTokens": 1100, "cachedInputTokens": 600, "cacheWriteInputTokens": 0, "outputTokens": 100, "reasoningOutputTokens": 25},
			"modelContextWindow": 475000,
		},
	})
	s.notify("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "completed"},
	})
}

func (s *codexServer) emitMessage(threadID, turnID, itemID, text string) {
	s.notify("item/started", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"item":     map[string]any{"type": "agentMessage", "id": itemID, "text": "", "phase": "final_answer"},
	})
	s.notify("item/agentMessage/delta", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"itemId":   itemID,
		"delta":    text,
	})
	s.notify("item/completed", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
		"item":     map[string]any{"type": "agentMessage", "id": itemID, "text": text, "phase": "final_answer"},
	})
}
