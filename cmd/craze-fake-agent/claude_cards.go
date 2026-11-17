package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The claude card scripts (craze plan 052 C3), shaped after claude-agent-acp
// 0.107.3's real frames (internal/acp/testdata/claude-cards). Each is
// claude-basic, with every prompt that carries no claude-basic directive
// answered as below; the answer each request got is said in the turn, so a
// test reads it off the transcript.
//
//	claude-ask       AskUserQuestion: its tool_call, then elicitation/create
//	                 with one single-select question and its free-text
//	                 question_0_custom (MULTI in the prompt: a second,
//	                 multi-select question too); says "ask answer: <reply>"
//	claude-ask-odd   elicitations craze cannot show: a url-mode one, and forms
//	                 of other shapes; says "odd <name>: <reply>" for each
//	claude-plan      ExitPlanMode: its tool_call, the update carrying the plan,
//	                 and the request with the adapter's five options; says
//	                 "plan answer: <option>". exit-plan-default ends the turn
//	                 end_turn in Manual; reject ends it cancelled, as the
//	                 adapter's interrupt does
//	claude-plan-odd  suspected plan exits of every odd shape craze rejects; says
//	                 "odd <name>: <option> (<kind>)" for each, then end_turn
//	claude-steer     holds the turn up to 5 s for a _session/steering, then
//	                 says "steered: <text>" (or "no steer") and ends it
//
// Every claude script answers _session/steering: injected into a turn
// claude-steer holds; outside one, promptRequired when the request's
// _meta.steering.idleBehavior asks for it, else startedNewTurn.

// claudeCardTurn runs the card script's turn for one prompt; false for a
// script that has none.
func (s *server) claudeCardTurn(id json.RawMessage, sid, text string) bool {
	switch s.script {
	case "claude-ask":
		s.claudeAsk(id, sid, strings.Contains(text, "MULTI"))
	case "claude-ask-odd":
		s.claudeAskOdd(id, sid)
	case "claude-plan":
		s.claudePlan(id, sid)
	case "claude-plan-odd":
		s.claudePlanOdd(id, sid)
	case "claude-steer":
		s.claudeSteerTurn(id, sid)
	default:
		return false
	}
	return true
}

func (s *server) claudeSay(sid, text string) {
	s.update(sid, acp.SessionUpdate{SessionUpdate: acp.UpdateAgentMessage, Content: &acp.ContentBlock{Type: "text", Text: text}})
}

func claudeMetaTool(name string) map[string]any {
	return map[string]any{"claudeCode": map[string]any{"toolName": name}}
}

// callRaw makes one request of the client and answers with its raw reply, or
// "error: …".
func (s *server) callRaw(method string, params any) string {
	var raw json.RawMessage
	if err := s.conn.Call(context.Background(), method, params, &raw); err != nil {
		return "error: " + err.Error()
	}
	return string(raw)
}

// claudeAsk is AskUserQuestion as the adapter asks it (elicitation.js).
func (s *server) claudeAsk(id json.RawMessage, sid string, multi bool) {
	const call = "toolu_ask_1"
	meta := claudeMetaTool("AskUserQuestion")
	s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCall, "name": "AskUserQuestion",
		"rawInput": map[string]any{}, "status": "pending", "title": "Asking for your input", "kind": "other", "content": []any{}})
	props := map[string]any{
		"question_0": map[string]any{"type": "string", "title": "Color", "oneOf": []map[string]string{
			{"const": "Red", "title": "Red", "description": "Choose red"},
			{"const": "Blue", "title": "Blue", "description": "Choose blue"},
		}},
		"question_0_custom": map[string]any{"type": "string", "title": "Other",
			"description": "Type your own answer, or add a note to the option you chose above (optional)."},
	}
	message := "Do you prefer red or blue?"
	if multi {
		props["question_0"].(map[string]any)["description"] = message
		props["question_1"] = map[string]any{"type": "array", "title": "Sizes", "description": "Which sizes?", "items": map[string]any{
			"anyOf": []map[string]string{{"const": "S", "title": "Small"}, {"const": "L", "title": "Large"}},
		}}
		props["question_1_custom"] = map[string]any{"type": "string", "title": "Other",
			"description": "Type your own answer to add to your selection above (optional)."}
		message = "Please answer the following questions."
	}
	answer := s.callRaw(acp.MethodElicitationCreate, map[string]any{
		"mode": "form", "sessionId": sid, "toolCallId": call, "message": message,
		"requestedSchema": map[string]any{"type": "object", "properties": props},
	})
	status := "completed"
	if !strings.Contains(answer, `"accept"`) {
		status = "failed"
	}
	s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCallUpd, "status": status})
	s.claudeSay(sid, "ask answer: "+answer)
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// claudeOddForms are elicitations craze must not show: a url-mode request
// and forms of shapes other than AskUserQuestion's.
var claudeOddForms = []struct {
	name   string
	params map[string]any
}{
	{"url", map[string]any{"mode": "url", "message": "Sign in", "url": "https://example.com/auth", "elicitationId": "e1"}},
	{"extra-field", map[string]any{"mode": "form", "message": "Your email?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{
		"email": map[string]any{"type": "string", "format": "email"},
	}}}},
	{"refusal-fallback", map[string]any{"mode": "form", "message": "Retry with opus?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{
		"choice": map[string]any{"type": "string", "oneOf": []map[string]string{{"const": "retry_fallback", "title": "Retry"}, {"const": "cancelled", "title": "Keep"}}},
	}}}},
	{"no-options", map[string]any{"mode": "form", "message": "?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{
		"question_0": map[string]any{"type": "string", "oneOf": []any{}},
	}}}},
}

func (s *server) claudeAskOdd(id json.RawMessage, sid string) {
	for _, f := range claudeOddForms {
		params := map[string]any{"sessionId": sid}
		for k, v := range f.params {
			params[k] = v
		}
		s.claudeSay(sid, fmt.Sprintf("odd %s: %s\n", f.name, s.callRaw(acp.MethodElicitationCreate, params)))
	}
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// claudePlanText is the plan claude-plan presents.
const claudePlanText = "# Plan: create plan.txt\n\n1. Write plan.txt containing x.\n"

// claudeExitOptions are ExitPlanMode's options as the adapter offers them
// (permissions/options/tools.js) in a session that was in Manual.
func claudeExitOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionID: "exit-plan-default", Name: "Yes, manually approve edits", Kind: acp.KindAllowOnce},
		{OptionID: "exit-plan-clear-auto", Name: "Yes, clear context (3% used) and use auto mode", Kind: acp.KindAllowAlways},
		{OptionID: "exit-plan-auto", Name: "Yes, and use auto mode", Kind: acp.KindAllowAlways},
		{OptionID: "exit-plan-bypass", Name: "Yes, and bypass permissions", Kind: acp.KindAllowAlways},
		{OptionID: "reject", Name: "No, keep planning", Kind: acp.KindRejectOnce},
	}
}

// claudeExitToolCall is the request's toolCall for ExitPlanMode.
func claudeExitToolCall(call, name, kind, plan string) map[string]any {
	tc := map[string]any{"toolCallId": call, "status": "pending", "title": "Approve Plan",
		"rawInput": map[string]any{"plan": plan, "planFilePath": "/home/user/.claude/plans/plan.md"},
		"content":  []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": plan}}}}
	if name != "" {
		tc["name"] = name
	}
	if kind != "" {
		tc["kind"] = kind
	}
	return tc
}

type permResult struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId"`
	} `json:"outcome"`
}

// claudePlan is an ExitPlanMode exchange as probe2's plan step had it.
func (s *server) claudePlan(id json.RawMessage, sid string) {
	const call = "toolu_plan_1"
	meta := claudeMetaTool("ExitPlanMode")
	s.claudeSay(sid, "I'll write the plan and submit it.")
	s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCall, "name": "ExitPlanMode",
		"rawInput": map[string]any{}, "status": "pending", "title": "Approve Plan", "kind": "switch_mode", "content": []any{}})
	s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCallUpd,
		"rawInput": map[string]any{"plan": claudePlanText, "planFilePath": "/home/user/.claude/plans/plan.md"},
		"content":  []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": claudePlanText}}}})
	var r permResult
	err := s.conn.Call(context.Background(), acp.MethodRequestPermission, map[string]any{
		"sessionId": sid, "toolCall": claudeExitToolCall(call, "ExitPlanMode", "switch_mode", claudePlanText), "options": claudeExitOptions(),
	}, &r)
	if err != nil || r.Outcome.Outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	s.claudeSay(sid, "plan answer: "+r.Outcome.OptionID)
	switch r.Outcome.OptionID {
	case "exit-plan-default":
		s.claude.set("mode", "default")
		s.update(sid, acp.SessionUpdate{SessionUpdate: acp.UpdateCurrentMode, CurrentModeID: "default"})
		s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCallUpd, "status": "completed",
			"title": "Exited Plan Mode", "rawOutput": "User has approved your plan."})
		s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
	case "reject":
		// Keep planning: the adapter denies with an interrupt, which ends
		// the turn cancelled (permissions/effects.js).
		s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCallUpd, "status": "failed"})
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
	default:
		s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
	}
}

// claudeOddExit is one suspected plan exit of a shape craze must reject: its
// request's toolCall pieces, its options, and the name a preceding tool_call
// was stamped with ("" sends none).
type claudeOddExit struct {
	name, stamped, toolName, kind, plan string
	options                             []acp.PermissionOption
}

func claudeOddExits() []claudeOddExit {
	all := claudeExitOptions()
	return []claudeOddExit{
		{name: "missing-default", toolName: "ExitPlanMode", kind: "switch_mode", plan: claudePlanText, options: all[1:]},
		{name: "default-always", toolName: "ExitPlanMode", kind: "switch_mode", plan: claudePlanText, options: []acp.PermissionOption{
			{OptionID: "exit-plan-default", Kind: acp.KindAllowAlways}, {OptionID: "exit-plan-auto", Kind: acp.KindAllowAlways}, {OptionID: "reject", Kind: acp.KindRejectOnce}}},
		{name: "duplicate-default", toolName: "ExitPlanMode", kind: "switch_mode", plan: claudePlanText, options: append([]acp.PermissionOption{all[0]}, all...)},
		{name: "named-write", toolName: "Write", kind: "switch_mode", plan: claudePlanText, options: all},
		{name: "stamped-bash", stamped: "Bash", toolName: "ExitPlanMode", kind: "switch_mode", plan: claudePlanText, options: all},
		{name: "kind-edit", toolName: "ExitPlanMode", kind: "edit", plan: claudePlanText, options: all},
		{name: "no-plan", toolName: "ExitPlanMode", kind: "switch_mode", options: all},
		{name: "no-reject", toolName: "ExitPlanMode", kind: "switch_mode", plan: claudePlanText, options: all[:4]},
		{name: "allow-only", kind: "execute", plan: claudePlanText, options: []acp.PermissionOption{
			{OptionID: "exit-plan-bypass", Kind: acp.KindAllowAlways}, {OptionID: "allow-once", Kind: acp.KindAllowOnce}}},
	}
}

// claudePlanOdd sends every odd plan exit in one turn and says what each got.
func (s *server) claudePlanOdd(id json.RawMessage, sid string) {
	for i, e := range claudeOddExits() {
		call := fmt.Sprintf("toolu_odd_%d", i)
		if e.stamped != "" {
			s.update(sid, map[string]any{"_meta": claudeMetaTool(e.stamped), "toolCallId": call, "sessionUpdate": acp.UpdateToolCall, "status": "pending"})
		}
		var r permResult
		err := s.conn.Call(context.Background(), acp.MethodRequestPermission, map[string]any{
			"sessionId": sid, "toolCall": claudeExitToolCall(call, e.toolName, e.kind, e.plan), "options": e.options,
		}, &r)
		got := r.Outcome.Outcome
		if err != nil {
			got = "error"
		}
		if r.Outcome.OptionID != "" {
			kind := "unoffered"
			for _, o := range e.options {
				if o.OptionID == r.Outcome.OptionID {
					kind = o.Kind
				}
			}
			got = r.Outcome.OptionID + " (" + kind + ")"
		}
		s.claudeSay(sid, fmt.Sprintf("odd %s: %s\n", e.name, got))
	}
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// claudeSteerTurn holds the turn for a steer, a cancel, or 5 s.
func (s *server) claudeSteerTurn(id json.RawMessage, sid string) {
	steer := make(chan string, 1)
	cancel := make(chan struct{})
	s.mu.Lock()
	s.claude.steer = steer
	if s.cancelled.Load() {
		close(cancel)
	} else {
		s.hangWait = cancel
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.claude.steer = nil
		s.mu.Unlock()
	}()
	s.claudeSay(sid, "waiting")
	select {
	case text := <-steer:
		s.claudeSay(sid, "steered: "+text)
		s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
	case <-cancel:
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
	case <-time.After(5 * time.Second):
		s.claudeSay(sid, "no steer")
		s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
	}
}

// onClaudeSteer answers _session/steering (v1/acp-agent.js steer).
func (s *server) onClaudeSteer(msg *acp.Message) {
	var p struct {
		Prompt []acp.ContentBlock `json:"prompt"`
		Meta   struct {
			Steering struct {
				IdleBehavior string `json:"idleBehavior"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	text := ""
	if len(p.Prompt) > 0 {
		text = p.Prompt[0].Text
	}
	s.mu.Lock()
	steer := s.claude.steer
	s.claude.steer = nil
	race := s.claude.race
	s.claude.race = nil
	s.mu.Unlock()
	switch {
	case race != nil:
		// The turn ends just ahead of the steer, which the adapter then runs
		// as a turn of its own (the stray startedNewTurn of plan 052 § B7).
		s.reply(race, map[string]any{"stopReason": acp.StopEndTurn})
		s.claudeStartedTurn(msg, text)
	case steer != nil:
		s.reply(msg.ID, map[string]any{"outcome": "injected"})
		steer <- text
	case os.Getenv("CRAZE_FAKE_CLAUDE_STEER_STARTS") == "1":
		s.claudeStartedTurn(msg, text)
	case p.Meta.Steering.IdleBehavior == "promptRequired":
		s.reply(msg.ID, map[string]any{"outcome": "promptRequired", "reason": "noRunningTurn"})
	default:
		s.reply(msg.ID, map[string]any{"outcome": "startedNewTurn"})
	}
}
