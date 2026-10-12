package llm

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// What craze's own drivers share, whose APIs only stream (the ChatGPT plan's
// Responses route, responses_adapter.go, and Anthropic's Messages API,
// messages_adapter.go).

// collect is a step's stream, as Stream returned it, gathered into the
// response Generate returns: the content in the order its parts ended — the
// order the step's messages keep — with the warnings, the usage and the
// finish; or the error Stream or the stream ended with.
func collect(stream fantasy.StreamResponse, err error) (*fantasy.Response, error) {
	if err != nil {
		return nil, err
	}
	resp := &fantasy.Response{}
	text := map[string]*strings.Builder{}
	reasoning := map[string]*strings.Builder{}
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeWarnings:
			resp.Warnings = append(resp.Warnings, part.Warnings...)
		case fantasy.StreamPartTypeTextStart:
			text[part.ID] = &strings.Builder{}
		case fantasy.StreamPartTypeTextDelta:
			if b := text[part.ID]; b != nil {
				b.WriteString(part.Delta)
			}
		case fantasy.StreamPartTypeTextEnd:
			if b := text[part.ID]; b != nil {
				resp.Content = append(resp.Content, fantasy.TextContent{Text: b.String()})
			}
		case fantasy.StreamPartTypeReasoningStart:
			reasoning[part.ID] = &strings.Builder{}
		case fantasy.StreamPartTypeReasoningDelta:
			if b := reasoning[part.ID]; b != nil {
				b.WriteString(part.Delta)
			}
		case fantasy.StreamPartTypeReasoningEnd:
			if b := reasoning[part.ID]; b != nil {
				resp.Content = append(resp.Content, fantasy.ReasoningContent{Text: b.String(), ProviderMetadata: part.ProviderMetadata})
			}
		case fantasy.StreamPartTypeToolCall:
			resp.Content = append(resp.Content, fantasy.ToolCallContent{ToolCallID: part.ID, ToolName: part.ToolCallName, Input: part.ToolCallInput})
		case fantasy.StreamPartTypeFinish:
			resp.Usage, resp.FinishReason, resp.ProviderMetadata = part.Usage, part.FinishReason, part.ProviderMetadata
		case fantasy.StreamPartTypeError:
			return nil, part.Error
		}
	}
	return resp, nil
}

// resultErrorText is a failed tool result's text: its error's, "" when it
// has none.
func resultErrorText(o fantasy.ToolResultOutputContentError) string {
	if o.Error == nil {
		return ""
	}
	return o.Error.Error()
}

// unsentResult is the warning for a tool result of a kind the drivers do not
// send, which goes empty.
func unsentResult(out fantasy.ToolResultOutputContent) string {
	kind := "none"
	if out != nil {
		kind = string(out.GetType())
	}
	return fmt.Sprintf("a tool result of kind %q is sent empty", kind)
}
