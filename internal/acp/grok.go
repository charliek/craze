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

// unwrapExtParams peels a gateway-wrapped {_x.ai/..., params:{method,params}}
// envelope down to the inner params object. Direct payloads pass through.
func unwrapExtParams(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return raw
	}
	var w struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return raw
	}
	nested := bytes.TrimSpace(w.Params)
	if w.Method == "" || len(nested) == 0 {
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

func parseGrokPromptComplete(params json.RawMessage) (sessionID, stopReason string, ok bool) {
	var n struct {
		SessionID  string `json:"sessionId"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(unwrapExtParams(params), &n); err != nil {
		return "", "", false
	}
	if n.SessionID == "" {
		return "", "", false
	}
	return n.SessionID, n.StopReason, true
}
