package acp

import (
	"bytes"
	"encoding/json"
	"strings"
)

func isGrokAskMethod(method string) bool {
	return method == MethodGrokAskUserQuestion || method == MethodGrokAskUserQuestionWrapped
}

func isGrokPlanMethod(method string) bool {
	return method == MethodGrokExitPlanMode || method == MethodGrokExitPlanModeWrapped
}

// unwrapExtParams peels an ext-method envelope down to the inner params
// object. Live grok 1.0.30 sends `_x.ai/...` params flat; the
// agent-client-protocol crate's own encoding nests them as
// {"params":{...}}, and relays may add the inner {"method":..., "params":...}
// pair. Any of those shapes ends at the same object; a flat payload passes
// through untouched.
func unwrapExtParams(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return raw
	}
	var w struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return raw
	}
	nested := bytes.TrimSpace(w.Params)
	if len(nested) == 0 || nested[0] != '{' {
		return raw
	}
	return nested
}

func parseGrokAsk(params json.RawMessage) (AskQuestionRequest, error) {
	var w struct {
		ToolCallID string `json:"toolCallId"`
		Questions  []struct {
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := decodeObject(unwrapExtParams(params), &w); err != nil {
		return AskQuestionRequest{}, err
	}
	req := AskQuestionRequest{ToolCallID: w.ToolCallID}
	for _, q := range w.Questions {
		text := strings.TrimSpace(q.Question)
		aq := AskQuestion{
			ID:            text,
			Prompt:        text,
			AllowMultiple: q.MultiSelect,
		}
		for _, o := range q.Options {
			label := o.Label
			if label == "" {
				continue
			}
			aq.Options = append(aq.Options, AskOption{ID: label, Label: label})
		}
		req.Questions = append(req.Questions, aq)
	}
	return req, nil
}

func parseGrokPlan(params json.RawMessage) (CreatePlanRequest, error) {
	var w struct {
		ToolCallID  string `json:"toolCallId"`
		PlanContent string `json:"planContent"`
	}
	if err := decodeObject(unwrapExtParams(params), &w); err != nil {
		return CreatePlanRequest{}, err
	}
	content := w.PlanContent
	if strings.TrimSpace(content) == "" {
		content = GrokEmptyPlanMarkdown
	}
	plan, err := json.Marshal(content)
	if err != nil {
		return CreatePlanRequest{}, err
	}
	return CreatePlanRequest{Plan: plan}, nil
}

// grokPromptComplete is the x.ai/session/prompt_complete payload craze reads.
type grokPromptComplete struct {
	SessionID  string `json:"sessionId"`
	PromptID   string `json:"promptId"`
	StopReason string `json:"stopReason"`
}

func parseGrokPromptComplete(params json.RawMessage) (grokPromptComplete, bool) {
	var n grokPromptComplete
	if err := json.Unmarshal(unwrapExtParams(params), &n); err != nil {
		return grokPromptComplete{}, false
	}
	if n.SessionID == "" {
		return grokPromptComplete{}, false
	}
	return n, true
}

// parseSubagentNotification returns a lifecycle event. ok is true only for a
// well-formed spawned/progress/finished. drop is true when the params are
// malformed (unparseable, or a lifecycle kind missing required ids) so the
// caller can count DroppedUpdates. Unknown sessionUpdate kinds return
// ok=false, drop=false and are ignored.
func parseSubagentNotification(params json.RawMessage) (n SubagentNotification, ok, drop bool) {
	var w struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate   string   `json:"sessionUpdate"`
			SubagentID      string   `json:"subagent_id"`
			AttemptID       string   `json:"attempt_id"`
			ChildSessionID  string   `json:"child_session_id"`
			ParentSessionID string   `json:"parent_session_id"`
			ParentPromptID  string   `json:"parent_prompt_id"`
			SubagentType    string   `json:"subagent_type"`
			Description     string   `json:"description"`
			Model           string   `json:"model"`
			Role            string   `json:"role"`
			CapabilityMode  string   `json:"capability_mode"`
			ResumedFrom     string   `json:"resumed_from"`
			Status          string   `json:"status"`
			Error           string   `json:"error"`
			Output          string   `json:"output"`
			DurationMs      int      `json:"duration_ms"`
			ToolCalls       int      `json:"tool_calls"`
			ToolCallCount   int      `json:"tool_call_count"`
			Turns           int      `json:"turns"`
			TurnCount       int      `json:"turn_count"`
			TokensUsed      int      `json:"tokens_used"`
			ToolsUsed       []string `json:"tools_used"`
			WillWake        bool     `json:"will_wake"`
		} `json:"update"`
	}
	if err := decodeObject(unwrapExtParams(params), &w); err != nil {
		return SubagentNotification{}, false, true
	}
	switch w.Update.SessionUpdate {
	case SubagentSpawned, SubagentProgress, SubagentFinished:
	default:
		return SubagentNotification{}, false, false
	}
	if w.SessionID == "" || w.Update.SubagentID == "" || w.Update.ChildSessionID == "" {
		return SubagentNotification{Kind: w.Update.SessionUpdate}, false, true
	}
	toolCalls := w.Update.ToolCalls
	if toolCalls == 0 {
		toolCalls = w.Update.ToolCallCount
	}
	turns := w.Update.Turns
	if turns == 0 {
		turns = w.Update.TurnCount
	}
	return SubagentNotification{
		SessionID:       w.SessionID,
		Kind:            w.Update.SessionUpdate,
		SubagentID:      w.Update.SubagentID,
		AttemptID:       w.Update.AttemptID,
		ChildSessionID:  w.Update.ChildSessionID,
		ParentSessionID: w.Update.ParentSessionID,
		SubagentType:    w.Update.SubagentType,
		Description:     w.Update.Description,
		Model:           w.Update.Model,
		Role:            w.Update.Role,
		CapabilityMode:  w.Update.CapabilityMode,
		ResumedFrom:     w.Update.ResumedFrom,
		Status:          w.Update.Status,
		Error:           w.Update.Error,
		Output:          w.Update.Output,
		DurationMs:      w.Update.DurationMs,
		ToolCalls:       toolCalls,
		Turns:           turns,
		TokensUsed:      w.Update.TokensUsed,
		ToolsUsed:       w.Update.ToolsUsed,
		WillWake:        w.Update.WillWake,
	}, true, false
}

// grokToolName reads update._meta["x.ai/tool"].name, the only grok _meta read
// in the client. The notification-level _meta (eventId, agentTimestampMs) is
// a sibling of update and irrelevant here.
func grokToolName(update json.RawMessage) string {
	var w struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(update, &w); err != nil {
		return ""
	}
	raw, ok := w.Meta["x.ai/tool"]
	if !ok {
		return ""
	}
	var tool struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		return ""
	}
	return tool.Name
}
