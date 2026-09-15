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

	// The queue scripts' per-prompt state (queue.go): every prompt still held
	// with its own cancel channel and its own reply, the turn runner's state,
	// and the interjections waiting for a safe point.
	promptSeq int
	queued    []*promptReq
	running   *promptReq
	draining  bool
	// bmu orders the queue broadcasts: each one snapshots the queue and
	// writes it inside this lock, so no broadcast can publish state older
	// than one already on the wire.
	bmu                  sync.Mutex
	pendingInterjections []string
	merged               []string
	fallbackSeq          int
	fallbackOn           bool
	fallbackID           string
	fallbackText         string
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
	cfg := defaultConfigOptions()
	if grokScript(script) {
		cfg = grokConfigOptions()
	}
	s := &server{conn: conn, script: script, config: cfg}
	conn.SetRequestHandler(s.onRequest)
	conn.SetNotifyHandler(s.onNotify)
	conn.Start()
	<-conn.Done()
	return nil
}

func grokScript(script string) bool {
	return strings.HasPrefix(script, "grok-")
}

// advertiseCommands is the available_commands_update both session/new branches
// send, in one place so the wire shape has one definition. Most scripts keep
// the single `research` entry the older goldens were written against; two do
// not. `commands` swaps in a catalog big enough to overflow the slash menu's
// window, so the band's columns, its scroll marks and its one-row-per-entry
// rule all have a golden to stand on, and `nocommands` sends nothing at all.
func (s *server) advertiseCommands() {
	if s.script == "nocommands" {
		// An agent that never advertises a catalog. Plugin rows stay
		// provisional for the whole session then (plan 010 §3.2), which is the
		// one state a golden cannot otherwise reach: the update lands the
		// instant session/new is answered, so "before the catalog" is a race
		// against the reader goroutine rather than a frame anyone can capture.
		return
	}
	cmds := []acp.AvailableCommand{{Name: "research", Description: "Agent-advertised command"}}
	if s.script == "commands" {
		cmds = commandsCatalog()
	}
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate:     acp.UpdateAvailableCommands,
		AvailableCommands: cmds,
	})
}

// commandsCatalog is 24 entries: enough that eight rows window onto it, with
// descriptions long enough to be clamped at 80 columns. Three names are load
// bearing and the goldens that use them say so — `zulu-tool` is the sentinel a
// frame script waits for to know the catalog has landed (the update arrives
// after session/new replies), `gauntlet-like` is the only entry matching the
// query `gau`, and `multiline-note` is the entry whose advertised description
// carries a newline, which the menu owes one physical row regardless.
func commandsCatalog() []acp.AvailableCommand {
	return []acp.AvailableCommand{
		{Name: "alpha-review", Description: "Review the working tree and report the findings worth acting on before the next commit"},
		{Name: "bravo-search", Description: "Search the whole repository for a symbol and summarise every call site it turns up"},
		{Name: "charlie-build", Description: "Build every binary in the module and report the first compile error in full"},
		{Name: "delta-deploy", Description: "Deploy the current branch to the staging environment and tail the rollout log"},
		{Name: "echo-lint", Description: "Run every configured linter and group the complaints by the file they belong to"},
		{Name: "foxtrot-format", Description: "Format the tree in place and list the files the formatter actually rewrote"},
		{Name: "golf-refactor", Description: "Extract the selected code into a helper and update every caller in the package"},
		{Name: "hotel-migrate", Description: "Generate the next schema migration and dry-run it against a scratch database"},
		{Name: "india-inspect", Description: "Inspect a running process and dump its goroutines, heap profile and open files"},
		{Name: "juliet-bench", Description: "Run the benchmark suite twice and report the deltas that clear the noise floor"},
		{Name: "kilo-profile", Description: "Collect a CPU profile for thirty seconds and render the hottest twenty frames"},
		{Name: "lima-trace", Description: "Capture an execution trace of one request and annotate every blocking wait in it"},
		{Name: "mike-audit", Description: "Audit the dependency tree for advisories and propose the smallest safe upgrade"},
		{Name: "november-scan", Description: "Scan the repository for secrets, tokens and private keys that were committed"},
		{Name: "oscar-package", Description: "Package the release artefacts for every supported platform and checksum them"},
		{Name: "papa-publish", Description: "Publish the built artefacts to the registry once every required check is green"},
		{Name: "quebec-query", Description: "Run a read-only query against the analytics warehouse and tabulate the result"},
		{Name: "romeo-report", Description: "Write the weekly engineering report from the merged pull requests of the week"},
		{Name: "sierra-sync", Description: "Sync the local checkout with upstream and rebase every unpushed local commit"},
		{Name: "tango-test", Description: "Run the full test suite with the race detector and repeat the flaky tests ten times"},
		{Name: "uniform-upgrade", Description: "Upgrade the toolchain pinned in the manifest and re-run the per-commit gate"},
		{Name: "gauntlet-like", Description: "Discover, plan, implement and verify a change end to end, one gated commit at a time"},
		{Name: "multiline-note", Description: "Draws on one line.\nThe newline in this description must not split the row."},
		{Name: "zulu-tool", Description: "The last entry in the catalog, and the sentinel a frame script waits for"},
	}
}

func grokConfigOptions() []map[string]any {
	return []map[string]any{
		{
			"id":           "reasoning_effort",
			"name":         "Effort",
			"category":     "model_option",
			"type":         "select",
			"currentValue": "high",
			"options": []map[string]string{
				{"value": "low", "name": "Low"},
				{"value": "high", "name": "High"},
			},
		},
	}
}

func grokModels() map[string]any {
	return map[string]any{
		"currentModelId": "grok-4.6",
		"availableModels": []map[string]string{
			{"modelId": "grok-4.6", "name": "Grok 4.6"},
		},
	}
}

func (s *server) onRequest(msg *acp.Message) {
	switch msg.Method {
	case acp.MethodInitialize:
		auth := []map[string]string{{"id": acp.AuthCursorLogin, "name": "Cursor Login"}}
		if s.script == "noauth" {
			auth = []map[string]string{}
		}
		if grokScript(s.script) {
			auth = []map[string]string{
				{"id": acp.AuthXAIAPIKey, "name": "API Key"},
				{"id": acp.AuthCachedToken, "name": "Cached Token"},
			}
		}
		result := map[string]any{
			"protocolVersion": acp.ProtocolVersion,
			"agentInfo":       map[string]string{"name": "craze-fake-agent", "version": "test"},
			"authMethods":     auth,
			"agentCapabilities": map[string]any{
				"loadSession": false,
			},
		}
		if grokScript(s.script) {
			result["_meta"] = map[string]any{"modelState": grokModels()}
		}
		s.reply(msg.ID, result)
	case acp.MethodAuthenticate:
		if p := os.Getenv("CRAZE_FAKE_DUMP_AUTH"); p != "" {
			_ = os.WriteFile(p, msg.Params, 0o644)
		}
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
		if grokScript(s.script) {
			s.reply(msg.ID, map[string]any{
				"sessionId": fakeSessionID,
				"modes": map[string]any{
					"currentModeId": "default",
					"availableModes": []map[string]string{
						{"id": "default", "name": "Default"},
						{"id": "plan", "name": "Plan"},
						{"id": "ask", "name": "Ask"},
					},
				},
				"models":        grokModels(),
				"configOptions": grokConfigOptions(),
			})
			s.advertiseCommands()
			return
		}
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
		s.advertiseCommands()
	case acp.MethodSessionPrompt:
		s.noteOrder(orderPrompt, "")
		if queueScript(s.script) {
			// The queue scripts hold one record per prompt instead of the
			// single flag, so a second prompt cancelling the first cannot
			// cancel itself. Registration is on the read loop: the cancel is
			// decided by the order the prompts arrived in.
			s.beginPrompt(msg)
			s.kickQueue()
			return
		}
		// A cancel belongs to the turn it interrupts. Clearing the flag here,
		// on the read loop, means a cancel read after this prompt can never be
		// lost and one read before it can never cancel this turn — without
		// which a second prompt to a cancel script cancels itself instantly.
		s.cancelled.Store(false)
		go s.handlePrompt(msg)
	case acp.MethodGrokInterject, acp.MethodGrokInterjectWrapped:
		// Only grok answers it. The cursor scripts fall through to the
		// -32601 every other unknown method gets, which is the live wire.
		if !grokScript(s.script) || !queueScript(s.script) {
			_ = s.conn.ReplyErr(msg.ID, acp.MethodNotFound(msg.Method))
			return
		}
		go s.handleInterject(msg)
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
	if queueScript(s.script) {
		// session/cancel is scoped to the session: every prompt the fake is
		// still holding is cancelled, and each is answered exactly once.
		s.cancelAll()
	}
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
	case "grok-ask":
		s.grokAsk(msg.ID, false)
	case "grok-ask-wrapped":
		s.grokAsk(msg.ID, true)
	case "grok-plan":
		s.grokPlan(msg.ID)
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
	case "grok-subagent":
		s.grokSubagent(msg.ID, grokSubagentSingle)
	case "grok-subagent-fail":
		s.grokSubagent(msg.ID, grokSubagentFail)
	case "grok-subagent-two":
		s.grokSubagentTwo(msg.ID)
	case "grok-subagent-nested":
		s.grokSubagentNested(msg.ID)
	case "grok-subagent-late":
		s.grokSubagent(msg.ID, grokSubagentLate)
	case "grok-subagent-cancel":
		s.grokSubagentCancel(msg.ID, false)
	case "grok-subagent-cancel-early":
		s.grokSubagentCancel(msg.ID, true)
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
	s.finishPrompt(id, acp.StopEndTurn)
}

func (s *server) finishPrompt(id json.RawMessage, stop string) {
	if grokScript(s.script) {
		_ = s.conn.Notify(context.Background(), acp.MethodGrokPromptComplete, map[string]any{
			"sessionId":  fakeSessionID,
			"stopReason": stop,
		})
	}
	s.reply(id, map[string]any{"stopReason": stop})
}

func grokAskParams() map[string]any {
	return map[string]any{
		"sessionId":  fakeSessionID,
		"toolCallId": askToolCallID,
		"mode":       "default",
		"questions": []map[string]any{
			{
				"question": "Pick one",
				"options": []map[string]string{
					{"label": "A"},
					{"label": "B"},
				},
			},
			{
				"question":    "Pick any",
				"multiSelect": true,
				"options": []map[string]string{
					{"label": "X"},
					{"label": "Y"},
					{"label": "Z"},
				},
			},
		},
	}
}

func (s *server) grokAsk(id json.RawMessage, wrapped bool) {
	params := grokAskParams()
	method := acp.MethodGrokAskUserQuestion
	body := any(params)
	if wrapped {
		method = acp.MethodGrokAskUserQuestionWrapped
		body = map[string]any{"method": acp.MethodGrokAskUserQuestion, "params": params}
	}
	var result struct {
		Outcome string              `json:"outcome"`
		Answers map[string][]string `json:"answers"`
	}
	err := s.conn.Call(context.Background(), method, body, &result)
	if err != nil || result.Outcome == "cancelled" {
		s.finishPrompt(id, acp.StopCancelled)
		return
	}
	if result.Outcome != "accepted" && result.Outcome != "skip_interview" {
		s.say("asked:bad-envelope")
		s.finishPrompt(id, acp.StopEndTurn)
		return
	}
	picks := make([]string, 0, 2)
	for _, q := range []string{"Pick one", "Pick any"} {
		picks = append(picks, q+"="+strings.Join(result.Answers[q], ","))
	}
	s.say("asked:" + result.Outcome + ":" + strings.Join(picks, ";"))
	s.finishPrompt(id, acp.StopEndTurn)
}

func (s *server) grokPlan(id json.RawMessage) {
	params := map[string]any{
		"sessionId":   fakeSessionID,
		"toolCallId":  "call-plan-grok",
		"planContent": "## Steps\n\n- read main.go\n- edit main.go\n",
	}
	var result struct {
		Outcome string `json:"outcome"`
	}
	err := s.conn.Call(context.Background(), acp.MethodGrokExitPlanMode, params, &result)
	if err != nil || result.Outcome == "cancelled" {
		s.finishPrompt(id, acp.StopCancelled)
		return
	}
	if result.Outcome != "approved" && result.Outcome != "abandoned" {
		s.say("planned:bad-envelope")
		s.finishPrompt(id, acp.StopEndTurn)
		return
	}
	s.say("planned:" + result.Outcome)
	s.finishPrompt(id, acp.StopEndTurn)
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

// subagentNotify sends one x.ai/session_notification lifecycle event in the
// live snake_case shape, with attempt_id and the child session id.
func (s *server) subagentNotify(outer string, update map[string]any) {
	raw, err := json.Marshal(map[string]any{
		"sessionId": outer,
		"update":    update,
	})
	if err != nil {
		return
	}
	_ = s.conn.Notify(context.Background(), acp.MethodGrokSessionNotificationWrapped, json.RawMessage(raw))
}

type grokSubagentMode int

const (
	grokSubagentSingle grokSubagentMode = iota
	grokSubagentFail
	grokSubagentLate
)

// grokSubagent is the one-child script: parent thought, spawn_subagent
// tool_call with _meta, subagent_spawned, spawn tool completed carrying
// subagent_id in rawOutput and content, two-chunk child prompt, child
// thought, list_dir tool_call with _meta, child answer, wait tool,
// subagent_progress before the golden text, a pause while the child runs,
// subagent_finished, wait tool completed retitled after finished (as live),
// parent text, prompt_complete then the RPC reply.
func (s *server) grokSubagent(id json.RawMessage, mode grokSubagentMode) {
	child := "sub-1"
	desc := "List directory files"
	prompt := "List the files in the current working directory in one line."
	s.thought("Spawning an explore subagent.")
	s.toolMeta(fakeSessionID, "call-spawn-1", "spawn_subagent", "spawn_subagent", map[string]any{
		"description":   desc,
		"prompt":        prompt,
		"subagent_type": "explore",
		"background":    true,
	})
	s.spawnTitle(fakeSessionID, "call-spawn-1", desc, prompt, "explore")
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":     "subagent_spawned",
		"subagent_id":       child,
		"attempt_id":        "at1.test",
		"parent_session_id": fakeSessionID,
		"parent_prompt_id":  "prompt-1",
		"child_session_id":  child,
		"subagent_type":     "explore",
		"description":       desc,
		"capability_mode":   "read-only",
		"role":              "explore",
		"model":             "grok-4.6",
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-spawn-1",
		"status":        "completed",
		"content": []map[string]any{{
			"type":    "content",
			"content": map[string]any{"type": "text", "text": "Subagent started in background.\nsubagent_id: " + child + "\ntype: explore\ndescription: " + desc},
		}},
		"rawOutput": map[string]any{"type": "Text", "text": "Subagent started in background.\nsubagent_id: " + child + "\ntype: explore\ndescription: " + desc},
		"_meta":     map[string]any{"x.ai/tool": map[string]any{"name": "spawn_subagent", "kind": "task"}},
	})
	s.childText(child, "user_message_chunk", "List the files in the")
	s.childText(child, "user_message_chunk", " current working directory in one line.")
	s.childText(child, "agent_thought_chunk", "Listing files.")
	s.toolMeta(child, "call-1", "list_dir", "list_dir", map[string]any{"target_directory": "/tmp/ws"})
	s.update(child, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"status":        "completed",
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "list_dir", "kind": "list"}},
	})
	// The view goldens wait on this tool completion; the pause holds the
	// pre-answer frame open long enough for the capture not to race the text.
	time.Sleep(taskRunFor)
	s.childText(child, "agent_message_chunk", "main.py README.md")
	s.toolMeta(fakeSessionID, "call-wait-1", "get_command_or_subagent_output", "get_command_or_subagent_output", map[string]any{
		"task_ids":   []string{child},
		"timeout_ms": 120000,
	})
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":     "subagent_progress",
		"subagent_id":       child,
		"attempt_id":        "at1.test",
		"parent_session_id": fakeSessionID,
		"child_session_id":  child,
		"duration_ms":       2058,
		"turn_count":        1,
		"tool_call_count":   1,
		"tokens_used":       4740,
		"tools_used":        []string{"list_dir"},
	})
	if mode == grokSubagentLate {
		s.finishPrompt(id, acp.StopEndTurn)
		time.Sleep(400 * time.Millisecond)
		s.childText(child, "agent_message_chunk", " late line")
		s.subagentNotify(fakeSessionID, map[string]any{
			"sessionUpdate":    "subagent_finished",
			"subagent_id":      child,
			"attempt_id":       "at1.test",
			"child_session_id": child,
			"status":           "completed",
			"tool_calls":       1,
			"turns":            1,
			"duration_ms":      2873,
			"tokens_used":      4740,
			"output":           "main.py README.md",
		})
		s.update(fakeSessionID, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "call-wait-1",
			"status":        "completed",
			"title":         "[subagent:explore] " + desc + " (sub-1)",
		})
		return
	}
	time.Sleep(taskRunFor)
	// A failed child reports the error, not an output it never produced.
	status, errText, output := "completed", "", "main.py README.md"
	parentText := "DONE: main.py README.md"
	if mode == grokSubagentFail {
		status, errText, output, parentText = "failed", "boom", "", "subagent failed"
	}
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":    "subagent_finished",
		"subagent_id":      child,
		"attempt_id":       "at1.test",
		"child_session_id": child,
		"status":           status,
		"error":            errText,
		"tool_calls":       1,
		"turns":            1,
		"duration_ms":      2873,
		"tokens_used":      4740,
		"output":           output,
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-wait-1",
		"status":        "completed",
		"title":         "[subagent:explore] " + desc + " (sub-1)",
	})
	s.say(parentText)
	s.finishPrompt(id, acp.StopEndTurn)
}

// grokSubagentTwo runs two children with interleaved streams and colliding
// child tool call ids. Both spawned land before either spawn tool's rawOutput;
// a child update for unknown sub-9 and the echo foreign other-session update
// exercise the drop path; a duplicate spawned for sub-1 is a no-op. sub-2
// finishes first; multi-wait retitles after both finished, as live.
func (s *server) grokSubagentTwo(id json.RawMessage) {
	s.thought("Spawning two explore subagents.")
	s.toolMeta(fakeSessionID, "call-spawn-1", "spawn_subagent", "spawn_subagent", map[string]any{
		"description":   "List python files",
		"prompt":        "List the python files in one line.",
		"subagent_type": "explore",
		"background":    true,
	})
	s.spawnTitle(fakeSessionID, "call-spawn-1", "List python files", "List the python files in one line.", "explore")
	s.toolMeta(fakeSessionID, "call-spawn-2", "spawn_subagent", "spawn_subagent", map[string]any{
		"description":   "Report README first line",
		"prompt":        "Report the first line of README.md.",
		"subagent_type": "explore",
		"background":    true,
	})
	s.spawnTitle(fakeSessionID, "call-spawn-2", "Report README first line", "Report the first line of README.md.", "explore")
	s.childText("sub-9", "agent_message_chunk", "must never surface")
	s.update("other-session", acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentMessage,
		Content:       &acp.ContentBlock{Type: "text", Text: "NOPE"},
	})
	for _, sp := range []struct{ child, desc string }{{"sub-1", "List python files"}, {"sub-2", "Report README first line"}} {
		s.subagentNotify(fakeSessionID, map[string]any{
			"sessionUpdate":     "subagent_spawned",
			"subagent_id":       sp.child,
			"attempt_id":        "at1.test",
			"parent_session_id": fakeSessionID,
			"parent_prompt_id":  "prompt-1",
			"child_session_id":  sp.child,
			"subagent_type":     "explore",
			"description":       sp.desc,
			"capability_mode":   "read-only",
			"role":              "explore",
			"model":             "grok-4.6",
		})
	}
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":     "subagent_spawned",
		"subagent_id":       "sub-1",
		"attempt_id":        "at1.test",
		"parent_session_id": fakeSessionID,
		"child_session_id":  "sub-1",
		"subagent_type":     "explore",
		"description":       "List python files",
		"model":             "grok-4.6",
	})
	for _, sp := range []struct{ child, desc string }{{"sub-1", "List python files"}, {"sub-2", "Report README first line"}} {
		s.update(fakeSessionID, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "call-spawn-" + sp.child[len(sp.child)-1:],
			"status":        "completed",
			"rawOutput":     map[string]any{"type": "Text", "text": "Subagent started in background.\nsubagent_id: " + sp.child},
			"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "spawn_subagent", "kind": "task"}},
		})
	}
	s.childText("sub-1", "user_message_chunk", "List the python files.")
	s.childText("sub-2", "user_message_chunk", "Report the first line.")
	s.toolMeta("sub-1", "call-1", "list_dir", "list_dir", map[string]any{"target_directory": "/tmp/ws"})
	s.toolMeta("sub-2", "call-1", "read_file", "read_file", map[string]any{"path": "/tmp/ws/README.md"})
	s.update("sub-1", map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"status":        "completed",
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "list_dir", "kind": "list"}},
	})
	s.childText("sub-1", "agent_message_chunk", "main.py util.py")
	s.update("sub-2", map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"status":        "completed",
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "read_file", "kind": "read"}},
	})
	// The two-view golden waits on sub-2's tool completion and captures
	// before its answer; the pause keeps that frame from racing the text.
	time.Sleep(taskRunFor)
	s.childText("sub-2", "agent_message_chunk", "# hi")
	s.toolMeta(fakeSessionID, "call-wait-1", "multi-wait (wait_all)", "get_command_or_subagent_output", map[string]any{
		"task_ids":   []string{"sub-1", "sub-2"},
		"timeout_ms": 120000,
	})
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":     "subagent_progress",
		"subagent_id":       "sub-1",
		"attempt_id":        "at1.test",
		"parent_session_id": fakeSessionID,
		"child_session_id":  "sub-1",
		"tool_call_count":   1,
		"tokens_used":       4740,
		"tools_used":        []string{"list_dir"},
	})
	time.Sleep(taskRunFor)
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":    "subagent_finished",
		"subagent_id":      "sub-2",
		"attempt_id":       "at1.test",
		"child_session_id": "sub-2",
		"status":           "completed",
		"tool_calls":       1,
		"turns":            1,
		"duration_ms":      4096,
		"tokens_used":      4799,
		"output":           "# hi",
	})
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":    "subagent_finished",
		"subagent_id":      "sub-1",
		"attempt_id":       "at1.test",
		"child_session_id": "sub-1",
		"status":           "completed",
		"tool_calls":       1,
		"turns":            1,
		"duration_ms":      3202,
		"tokens_used":      4934,
		"output":           "main.py util.py",
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-wait-1",
		"status":        "completed",
		"title":         "multi-wait (wait_all)",
	})
	s.say("DONE: main.py util.py / # hi")
	s.finishPrompt(id, acp.StopEndTurn)
}

// grokSubagentNested has sub-1 spawn sub-1a: the spawn tool lives on sub-1's
// session and the spawned notification's outer id is sub-1, so the grandchild
// registers flat like any other child.
func (s *server) grokSubagentNested(id json.RawMessage) {
	s.thought("Spawning a nested subagent.")
	s.toolMeta(fakeSessionID, "call-spawn-1", "spawn_subagent", "spawn_subagent", map[string]any{
		"description":   "Outer task",
		"prompt":        "Spawn a nested subagent and report.",
		"subagent_type": "explore",
		"background":    true,
	})
	s.spawnTitle(fakeSessionID, "call-spawn-1", "Outer task", "Spawn a nested subagent and report.", "explore")
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":     "subagent_spawned",
		"subagent_id":       "sub-1",
		"attempt_id":        "at1.test",
		"parent_session_id": fakeSessionID,
		"parent_prompt_id":  "prompt-1",
		"child_session_id":  "sub-1",
		"subagent_type":     "explore",
		"description":       "Outer task",
		"model":             "grok-4.6",
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-spawn-1",
		"status":        "completed",
		"rawOutput":     map[string]any{"type": "Text", "text": "Subagent started in background.\nsubagent_id: sub-1"},
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "spawn_subagent", "kind": "task"}},
	})
	s.childText("sub-1", "user_message_chunk", "Spawn a nested subagent.")
	s.toolMeta("sub-1", "call-nested-spawn", "spawn_subagent", "spawn_subagent", map[string]any{
		"description":   "Inner task",
		"prompt":        "List files.",
		"subagent_type": "explore",
		"background":    true,
	})
	s.spawnTitle("sub-1", "call-nested-spawn", "Inner task", "List files.", "explore")
	s.subagentNotify("sub-1", map[string]any{
		"sessionUpdate":     "subagent_spawned",
		"subagent_id":       "sub-1a",
		"attempt_id":        "at1.test",
		"parent_session_id": "sub-1",
		"parent_prompt_id":  "prompt-2",
		"child_session_id":  "sub-1a",
		"subagent_type":     "explore",
		"description":       "Inner task",
		"model":             "grok-4.6",
	})
	s.childText("sub-1a", "user_message_chunk", "List files.")
	s.toolMeta("sub-1a", "call-1", "list_dir", "list_dir", map[string]any{"target_directory": "/tmp/ws"})
	s.update("sub-1a", map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"status":        "completed",
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "list_dir", "kind": "list"}},
	})
	s.childText("sub-1a", "agent_message_chunk", "main.py")
	time.Sleep(taskRunFor)
	s.subagentNotify("sub-1", map[string]any{
		"sessionUpdate":    "subagent_finished",
		"subagent_id":      "sub-1a",
		"attempt_id":       "at1.test",
		"child_session_id": "sub-1a",
		"status":           "completed",
		"tool_calls":       1,
		"turns":            1,
		"duration_ms":      1200,
		"tokens_used":      900,
		"output":           "main.py",
	})
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":    "subagent_finished",
		"subagent_id":      "sub-1",
		"attempt_id":       "at1.test",
		"child_session_id": "sub-1",
		"status":           "completed",
		"tool_calls":       1,
		"turns":            1,
		"duration_ms":      2400,
		"tokens_used":      1800,
		"output":           "main.py",
	})
	s.say("DONE: main.py")
	s.finishPrompt(id, acp.StopEndTurn)
}

// grokSubagentCancel runs a general-purpose child whose shell tool stays
// in_progress while the parent hangs. On session/cancel the parent completes
// cancelled, then — after 400 ms, so the drain must be state-aware — the
// child finishes cancelled (cancel2.out). early is the cancel.out order
// instead: the child's own turn ends first, its finish says completed with no
// error, and only then does the parent turn end cancelled.
func (s *server) grokSubagentCancel(id json.RawMessage, early bool) {
	s.thought("Spawning a general-purpose subagent.")
	s.toolMeta(fakeSessionID, "call-spawn-1", "spawn_subagent", "spawn_subagent", map[string]any{
		"description":   "Sleep 45 then finish",
		"prompt":        "Run sleep 45 and report finished.",
		"subagent_type": "general-purpose",
		"background":    true,
	})
	s.spawnTitle(fakeSessionID, "call-spawn-1", "Sleep 45 then finish", "Run sleep 45 and report finished.", "general-purpose")
	s.subagentNotify(fakeSessionID, map[string]any{
		"sessionUpdate":     "subagent_spawned",
		"subagent_id":       "sub-1",
		"attempt_id":        "at1.test",
		"parent_session_id": fakeSessionID,
		"parent_prompt_id":  "prompt-1",
		"child_session_id":  "sub-1",
		"subagent_type":     "general-purpose",
		"description":       "Sleep 45 then finish",
		"capability_mode":   "general",
		"role":              "general",
		"model":             "grok-4.6",
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-spawn-1",
		"status":        "completed",
		"rawOutput":     map[string]any{"type": "Text", "text": "Subagent started in background.\nsubagent_id: sub-1"},
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "spawn_subagent", "kind": "task"}},
	})
	s.childText("sub-1", "user_message_chunk", "Run sleep 45 and report finished.")
	s.toolMeta("sub-1", "call-1", "Execute sleep 45 && echo finished", "run_terminal_command", map[string]any{"command": "sleep 45 && echo finished"})
	s.update("sub-1", map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"status":        "in_progress",
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "run_terminal_command", "kind": "execute"}},
	})
	s.toolMeta(fakeSessionID, "call-wait-1", "get_command_or_subagent_output", "get_command_or_subagent_output", map[string]any{
		"task_ids":   []string{"sub-1"},
		"timeout_ms": 120000,
	})
	if !s.waitCancelled(id) {
		return
	}
	finish := func(status, errText string) {
		s.subagentNotify(fakeSessionID, map[string]any{
			"sessionUpdate":    "subagent_finished",
			"subagent_id":      "sub-1",
			"attempt_id":       "at1.test",
			"child_session_id": "sub-1",
			"status":           status,
			"error":            errText,
			"tool_calls":       0,
			"turns":            1,
			"duration_ms":      6608,
			"tokens_used":      0,
		})
	}
	if early {
		// cancel.out:63,65,70,71 — the child had already ended its own turn
		// end_turn and reported completed before the cancel reached it.
		s.turnCompleted("sub-1", acp.StopEndTurn)
		finish("completed", "")
		s.turnCompleted(fakeSessionID, acp.StopCancelled)
		s.finishPrompt(id, acp.StopCancelled)
		return
	}
	// cancel2.out:70-77 — parent turn_completed, then the child's on the
	// child session id, then prompt_complete and the reply; the child's
	// finish trails by 400 ms.
	s.turnCompleted(fakeSessionID, acp.StopCancelled)
	s.turnCompleted("sub-1", acp.StopCancelled)
	s.finishPrompt(id, acp.StopCancelled)
	time.Sleep(400 * time.Millisecond)
	finish("cancelled", "Subagent was cancelled")
}

// turnCompleted is the x.ai turn_completed a session sends as its turn ends.
// Live grok sends one per session — parent and child both — ahead of the
// parent's prompt_complete; craze ignores the kind, and that is the point.
func (s *server) turnCompleted(sessionID, stop string) {
	s.subagentNotify(sessionID, map[string]any{
		"sessionUpdate": "turn_completed",
		"prompt_id":     "prompt-1",
		"stop_reason":   stop,
	})
}

// waitCancelled blocks until session/cancel arrives. On its 30 s deadline it
// ends the prompt itself rather than leaking the pending request, and reports
// false so the script stops.
func (s *server) waitCancelled(id json.RawMessage) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s.cancelled.Load() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.finishPrompt(id, acp.StopEndTurn)
	return false
}

func (s *server) thought(text string) {
	s.update(fakeSessionID, acp.SessionUpdate{
		SessionUpdate: acp.UpdateAgentThought,
		Content:       &acp.ContentBlock{Type: "text", Text: text},
	})
}

// spawnTitle is the tool_call_update grok sends between a spawn_subagent
// tool_call and its subagent_spawned (two.out:45): the row is retitled to the
// description and rawInput is rewritten into the Task variant. Skipping it
// left the fakes showing a row title no live wire ever produces.
func (s *server) spawnTitle(sessionID, callID, desc, prompt, subagentType string) {
	s.update(sessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    callID,
		"kind":          "other",
		"title":         desc,
		"locations":     []any{},
		"rawInput": map[string]any{
			"variant":           "Task",
			"prompt":            prompt,
			"description":       desc,
			"subagent_type":     subagentType,
			"run_in_background": true,
			"task_id":           nil,
		},
		"_meta": map[string]any{"x.ai/tool": map[string]any{
			"version":   1,
			"name":      "spawn_subagent",
			"kind":      "task",
			"namespace": "grok_build",
			"label":     "Subagent",
			"read_only": false,
		}},
	})
}

// toolMeta is a tool_call with update._meta["x.ai/tool"].name set, the shape
// the ACP ToolName normalization reads.
func (s *server) toolMeta(sessionID, callID, title, toolName string, rawInput map[string]any) {
	s.update(sessionID, map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    callID,
		"title":         title,
		"rawInput":      rawInput,
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": toolName}},
	})
}

func (s *server) childText(sessionID, kind, text string) {
	s.update(sessionID, acp.SessionUpdate{
		SessionUpdate: kind,
		Content:       &acp.ContentBlock{Type: "text", Text: text},
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
	var parts []string
	for _, b := range p.Prompt {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	// A newline between blocks, not nothing: craze sends the draft and then one
	// block per plugin expansion, and an echo that ran them together would hide
	// exactly the boundary a golden is there to show.
	return strings.Join(parts, "\n")
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
