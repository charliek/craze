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

// parseInterjection reads x.ai/session/interjection. A broadcast without text
// is dropped: the whole point of the notification is the user block it makes.
func parseInterjection(params json.RawMessage) (InterjectionNotification, bool) {
	var n InterjectionNotification
	if err := decodeObject(unwrapExtParams(params), &n); err != nil {
		return InterjectionNotification{}, false
	}
	if n.SessionID == "" || n.Text == "" {
		return InterjectionNotification{}, false
	}
	return n, true
}

// parseQueueChanged reads x.ai/queue/changed. Entries may legitimately be
// empty — that is what a drained queue looks like — so only the session id is
// required.
func parseQueueChanged(params json.RawMessage) (QueueChanged, bool) {
	var n QueueChanged
	if err := decodeObject(unwrapExtParams(params), &n); err != nil {
		return QueueChanged{}, false
	}
	if n.SessionID == "" {
		return QueueChanged{}, false
	}
	return n, true
}

// grokTurnCompleted is the turn_completed update carried on
// x.ai/session_notification. Live grok sends one for every turn — craze's own
// and the interject fallback's — and it is the only ending the fallback has.
type grokTurnCompleted struct {
	SessionID  string
	PromptID   string
	StopReason string
}

// parseGrokTurnCompleted returns the event only for a turn_completed update
// that names its prompt. Any other update kind is not one, and says nothing
// about malformed params: handleSubagentNotification still gets its turn.
func parseGrokTurnCompleted(params json.RawMessage) (grokTurnCompleted, bool) {
	var w struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			PromptID      string `json:"prompt_id"`
			StopReason    string `json:"stop_reason"`
		} `json:"update"`
	}
	if err := decodeObject(unwrapExtParams(params), &w); err != nil {
		return grokTurnCompleted{}, false
	}
	if w.Update.SessionUpdate != UpdateTurnCompleted || w.SessionID == "" || w.Update.PromptID == "" {
		return grokTurnCompleted{}, false
	}
	return grokTurnCompleted{
		SessionID:  w.SessionID,
		PromptID:   w.Update.PromptID,
		StopReason: w.Update.StopReason,
	}, true
}
