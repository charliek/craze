package llm

// This file is the ChatGPT plan driver's only Fantasy-facing code (plan 033
// §3.9, P17, owner decision 14): it adapts package responsesapi — craze's own
// Responses client, which imports neither Fantasy nor openai-go — to
// fantasy.LanguageModel, mapping Fantasy's messages to Responses input items
// and the stream's events to Fantasy's stream parts. It lives here, beside
// the factory that will build it (plan 033 C14), rather than in
// responsesapi: it returns *FinalError, and package llm imports the driver,
// so the driver's package could not import llm back. Its design follows
// Fantasy's Apache-2.0 responses_language_model.go (responsesapi/NOTICE).

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/charliek/craze/internal/harness/llm/responsesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// responsesModel is a model on the ChatGPT plan's driver (DriverChatGPT): the
// Responses API at OpenAI's fixed endpoint, authenticated by an Auth rather
// than a key.
//
// It lands inert (plan 033 C12): the model table still refuses the driver,
// and the factory builds it from C14 on, wrapped as every model is (wrap.go).
//
// Each request is built from the step's call alone, whole, as the route
// keeps nothing between requests (store false):
//
//   - The system prompt is instructions; no system item is ever sent.
//   - The call's effort, from EffortOptions, is reasoning.effort; without one,
//     reasoning asks for the summary alone.
//   - The call's headers go with it — session-id, the session's id, which
//     the harness sets for this driver alone (P18) — and its User-Agent.
//   - The call's output ceiling, temperature and other sampling settings are
//     never sent: the route refuses max_output_tokens, and D-74's ceiling is
//     the harness's compaction reserve alone here.
//   - parallel_tool_calls is the model's ParallelToolCalls, nil leaving it to
//     the server.
type responsesModel struct {
	provider string // the table's provider id: the key a call's options are under
	model    string // the wire model
	parallel *bool
	client   *responsesapi.Client
}

// newResponsesModel builds r's model on auth. httpClient, when not nil,
// sends the requests (tests reach a local server through it); the client
// refuses redirects whatever it is.
func newResponsesModel(r modeltable.Resolved, auth Auth, httpClient *http.Client) (*responsesModel, error) {
	if auth == nil {
		return nil, fmt.Errorf("llm: provider %q (model %q): driver %q signs in, and no sign-in was given", r.ProviderID, r.Alias, r.Driver)
	}
	if strings.TrimSpace(r.WireModel) == "" {
		return nil, fmt.Errorf("llm: provider %q (model %q) names no wire model", r.ProviderID, r.Alias)
	}
	c, err := responsesapi.NewClient(responsesapi.Config{HTTPClient: httpClient, Credentials: auth})
	if err != nil {
		return nil, fmt.Errorf("llm: model %q: %w", r.Alias, err)
	}
	var parallel *bool
	if r.ParallelToolCalls != nil {
		v := *r.ParallelToolCalls
		parallel = &v
	}
	return &responsesModel{provider: r.ProviderID, model: r.WireModel, parallel: parallel, client: c}, nil
}

func (m *responsesModel) Provider() string { return m.provider }
func (m *responsesModel) Model() string    { return m.model }

// typeResponsesOptions is responsesOptions' id in Fantasy's provider-data
// registry.
const typeResponsesOptions = "craze.responses.options"

// responsesOptions are a ChatGPT-plan call's provider options
// (EffortOptions): the reasoning effort to send.
type responsesOptions struct {
	Effort string `json:"effort,omitempty"`
}

func init() {
	fantasy.RegisterProviderType(typeResponsesOptions, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var v responsesOptions
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return &v, nil
	})
}

func (*responsesOptions) Options() {}

func (o responsesOptions) MarshalJSON() ([]byte, error) {
	type plain responsesOptions
	return fantasy.MarshalProviderType(typeResponsesOptions, plain(o))
}

func (o *responsesOptions) UnmarshalJSON(data []byte) error {
	type plain responsesOptions
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	*o = responsesOptions(p)
	return nil
}

// request is call as a Responses request, with warnings for what it could
// not carry.
func (m *responsesModel) request(call fantasy.Call) (responsesapi.Request, []fantasy.CallWarning) {
	instructions, input, warnings := responsesInput(call.Prompt)
	req := responsesapi.Request{
		Model:             m.model,
		Instructions:      instructions,
		Input:             input,
		ParallelToolCalls: m.parallel,
		Headers:           maps.Clone(call.Headers),
		UserAgent:         call.UserAgent,
	}
	if o, ok := call.ProviderOptions[m.provider].(*responsesOptions); ok && o != nil {
		req.Effort = o.Effort
	}
	for _, t := range call.Tools {
		ft, ok := t.(fantasy.FunctionTool)
		if !ok {
			warnings = append(warnings, fantasy.CallWarning{Type: fantasy.CallWarningTypeUnsupportedTool, Tool: t,
				Message: "the ChatGPT plan's driver sends function tools only"})
			continue
		}
		req.Tools = append(req.Tools, responsesapi.Tool{Name: ft.Name, Description: ft.Description, Parameters: ft.InputSchema})
	}
	if call.ToolChoice != nil {
		req.ToolChoice = string(*call.ToolChoice)
	}
	return req, warnings
}

// responsesInput is a prompt as the request's instructions and input
// (Fantasy's toResponsesPrompt, adapted):
//
//   - System messages' text, joined, is the instructions.
//   - A user message's text and images are input_text and input_image parts;
//     an empty one is dropped.
//   - An assistant message's text is an assistant message; its tool calls are
//     function calls with their call ids; and its reasoning is replayed as a
//     reasoning item (D-33: the store keeps reasoning only for the model that
//     produced it) when it can be — see replayable.
//   - A tool result is a function call output with its call id: text as a
//     string, an image as content parts (the API's function_call_output takes
//     either). The wrapper (wrap.go rule 5) has normally moved images out of
//     tool results into one user message after the run already, as it does
//     for every driver, so the image goes as user input_image, the shape the
//     route was seen to take (spike); the content form is for a caller that
//     did not wrap.
func responsesInput(prompt fantasy.Prompt) (string, []responsesapi.Item, []fantasy.CallWarning) {
	var system []string
	var items []responsesapi.Item
	var warnings []fantasy.CallWarning
	warn := func(msg string) {
		warnings = append(warnings, fantasy.CallWarning{Type: fantasy.CallWarningTypeOther, Message: msg})
	}
	for _, msg := range prompt {
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			for _, part := range msg.Content {
				if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && strings.TrimSpace(p.Text) != "" {
					system = append(system, p.Text)
				}
			}
		case fantasy.MessageRoleUser:
			var content []responsesapi.Content
			for _, part := range msg.Content {
				if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
					content = append(content, responsesapi.InputText(p.Text))
				} else if p, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
					if !strings.HasPrefix(p.MediaType, "image/") {
						warn(fmt.Sprintf("a file of type %s is not sent: the driver sends images only", p.MediaType))
						continue
					}
					content = append(content, responsesapi.InputImage(p.MediaType, p.Data))
				}
			}
			if len(content) > 0 {
				items = append(items, responsesapi.UserMessage(content...))
			}
		case fantasy.MessageRoleAssistant:
			items = append(items, assistantItems(msg)...)
		case fantasy.MessageRoleTool:
			for _, part := range msg.Content {
				r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok || r.ProviderExecuted {
					continue
				}
				item, w := toolOutput(r)
				items = append(items, item)
				if w != "" {
					warn(w)
				}
			}
		}
	}
	return strings.Join(system, "\n\n"), items, warnings
}

// assistantItems is an assistant message's parts as input items, in order.
// A reasoning part goes only when replayable says it can, and only when an
// output item of the same response — text or a tool call — follows it in
// the message: the route pairs a reasoning item with what it led to, and one
// that led to nothing (a step cut off after it) is dropped.
func assistantItems(msg fantasy.Message) []responsesapi.Item {
	last := -1 // the last output part's index
	for i, part := range msg.Content {
		if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && p.Text != "" {
			last = i
		} else if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && !p.ProviderExecuted {
			last = i
		}
	}
	var items []responsesapi.Item
	for i, part := range msg.Content {
		if p, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
			if md, ok := replayable(p); ok && i < last {
				items = append(items, responsesapi.Reasoning(md.ItemID, md.Summary, *md.EncryptedContent))
			}
		} else if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && p.Text != "" {
			items = append(items, responsesapi.AssistantMessage(p.Text))
		} else if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && !p.ProviderExecuted {
			items = append(items, responsesapi.FunctionCall(p.ToolCallID, p.ToolName, p.Input))
		}
	}
	return items
}

// legalItemID is what a replayed reasoning item's id must look like: gx's
// rule (prior-art-chatgpt-auth.md), the characters the server's own ids
// (rs_…) are made of.
var legalItemID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// replayable is a reasoning part's metadata when the part can be replayed:
// it carries the Responses reasoning metadata this driver writes
// (reasoningMetadata) with encrypted content — without it the route has
// nothing to replay — and an id of legal characters (legalItemID). Anything
// else is dropped: a reasoning part from another driver, or one rebuilt from
// an interrupted step, which keeps its text and no metadata (turn.go).
func replayable(p fantasy.ReasoningPart) (*openai.ResponsesReasoningMetadata, bool) {
	md := openai.GetReasoningMetadata(p.ProviderOptions)
	if md == nil || md.EncryptedContent == nil || *md.EncryptedContent == "" || !legalItemID.MatchString(md.ItemID) {
		return nil, false
	}
	return md, true
}

// toolOutput is a tool result as a function call output, with a warning
// when part of it could not be sent.
func toolOutput(r fantasy.ToolResultPart) (responsesapi.Item, string) {
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](r.Output); ok {
		return responsesapi.FunctionCallOutput(r.ToolCallID, o.Text), ""
	}
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](r.Output); ok {
		text := ""
		if o.Error != nil {
			text = o.Error.Error()
		}
		return responsesapi.FunctionCallOutput(r.ToolCallID, text), ""
	}
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](r.Output); ok {
		data, err := base64.StdEncoding.DecodeString(o.Data)
		if err != nil || !strings.HasPrefix(o.MediaType, "image/") {
			text := cmp.Or(o.Text, "The tool returned "+o.MediaType+" content, which is not sent.")
			return responsesapi.FunctionCallOutput(r.ToolCallID, text),
				fmt.Sprintf("a tool result of type %s is sent as text only", o.MediaType)
		}
		var content []responsesapi.Content
		if o.Text != "" {
			content = append(content, responsesapi.InputText(o.Text))
		}
		content = append(content, responsesapi.InputImage(o.MediaType, data))
		return responsesapi.FunctionCallOutputContent(r.ToolCallID, content...), ""
	}
	kind := "none"
	if r.Output != nil {
		kind = string(r.Output.GetType())
	}
	return responsesapi.FunctionCallOutput(r.ToolCallID, ""), fmt.Sprintf("a tool result of kind %q is sent empty", kind)
}

// Stream sends one step. The request is sent when the stream is first read,
// and the response's body is closed when the read ends, however it ends.
func (m *responsesModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	req, warnings := m.request(call)
	return func(yield func(fantasy.StreamPart) bool) {
		if len(warnings) > 0 && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeWarnings, Warnings: warnings}) {
			return
		}
		s, err := m.client.Stream(ctx, req)
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: responsesError(err)})
			return
		}
		defer func() { _ = s.Close() }()
		st := partState{ids: map[int]string{}}
		for s.Next() {
			if !st.parts(s.Event(), yield) {
				return
			}
		}
		err = s.Err()
		if err == nil {
			return
		}
		// A failed or incomplete response's usage was spent all the same:
		// a Finish part carries it first, for OnStreamFinish (the
		// summarizer counts it, compact.go observeUsage).
		var re *responsesapi.Error
		if errors.As(err, &re) && re.Usage != nil {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, Usage: fantasyUsage(*re.Usage), FinishReason: fantasy.FinishReasonError}) {
				return
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: responsesError(err)})
	}, nil
}

// partState maps one response's events to Fantasy's stream parts.
type partState struct {
	ids   map[int]string // each item's part id, by output_index
	calls bool           // a function call finished
}

// parts yields ev as Fantasy stream parts, false when the consumer stopped.
// A message's and a reasoning item's part id is its item id; a function
// call's is its call id, which is what Fantasy and the transcript know the
// call by.
func (st *partState) parts(ev responsesapi.Event, yield func(fantasy.StreamPart) bool) bool {
	id := st.ids[ev.Index]
	switch ev.Kind {
	case responsesapi.ItemAdded:
		switch ev.Item.Type {
		case "message":
			st.ids[ev.Index] = cmp.Or(ev.Item.ID, fmt.Sprintf("item-%d", ev.Index))
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: st.ids[ev.Index]})
		case "reasoning":
			st.ids[ev.Index] = cmp.Or(ev.Item.ID, fmt.Sprintf("item-%d", ev.Index))
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: st.ids[ev.Index]})
		case "function_call":
			st.ids[ev.Index] = ev.Item.CallID
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: ev.Item.CallID, ToolCallName: ev.Item.Name})
		}
	case responsesapi.TextDelta:
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Delta})
	case responsesapi.ReasoningDelta:
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: id, Delta: ev.Delta})
	case responsesapi.ArgumentsDelta:
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: ev.Delta})
	case responsesapi.ItemDone:
		switch ev.Item.Type {
		case "message":
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: id})
		case "reasoning":
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id,
				ProviderMetadata: fantasy.ProviderMetadata{openai.Name: reasoningMetadata(ev.Item)}})
		case "function_call":
			st.calls = true
			callID := cmp.Or(id, ev.Item.CallID)
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: callID}) &&
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: callID, ToolCallName: ev.Item.Name, ToolCallInput: ev.Item.Arguments})
		}
	case responsesapi.Completed:
		reason := fantasy.FinishReasonStop
		if st.calls {
			reason = fantasy.FinishReasonToolCalls
		}
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, Usage: fantasyUsage(ev.Usage), FinishReason: reason})
	}
	return true
}

// reasoningMetadata is a finished reasoning item as the metadata its part
// keeps: Fantasy's own openai.ResponsesReasoningMetadata, under openai.Name,
// the JSON every craze transcript since H1 can read back
// (store/golden_test.go TestReasoningMetadataSurvives) — the item id, its
// summary parts and its encrypted content, taken from the item's end.
func reasoningMetadata(item responsesapi.OutputItem) *openai.ResponsesReasoningMetadata {
	md := &openai.ResponsesReasoningMetadata{ItemID: item.ID, Summary: item.Summary}
	if md.Summary == nil {
		md.Summary = []string{}
	}
	if item.EncryptedContent != "" {
		sealed := item.EncryptedContent
		md.EncryptedContent = &sealed
	}
	return md
}

// fantasyUsage is a response's usage as Fantasy counts it (plan 033 §3.9):
// cached tokens are CacheRead and not also Input, which the API's
// input_tokens includes them in, so the harness's context sum (Input +
// CacheRead + CacheCreation + Output, autocompact.go) counts each token once;
// Output includes reasoning, as the API's does, and Reasoning reports it.
// cache_write_tokens is not mapped: the spike only ever saw 0, and how it
// relates to input_tokens is unknown, so CacheCreation could count it twice.
func fantasyUsage(u responsesapi.Usage) fantasy.Usage {
	cached := min(max(u.CachedTokens, 0), u.InputTokens)
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return fantasy.Usage{
		InputTokens:     u.InputTokens - cached,
		OutputTokens:    u.OutputTokens,
		TotalTokens:     total,
		ReasoningTokens: u.ReasoningTokens,
		CacheReadTokens: cached,
	}
}

// responsesError is err, from the driver's core, as the error Fantasy and the
// harness classify:
//
//   - A final failure (responsesapi.Error.Final: every 429, every 400, and
//     the codes not to repeat) is a *FinalError, which nothing retries.
//   - Any other failure the server reported is a *fantasy.ProviderError with
//     its status, its headers (a Retry-After), its auth and context flags,
//     and a body holding the standard error object, from which the scrubber
//     reads the code and type back (ErrorNames). One inside the stream is
//     retryable when the core calls it transient (usage or account
//     information unavailable, a server error) — before any output only,
//     which the wrapper's rule 3 enforces (D-32).
//   - A stream that stopped before saying how it ended is Fantasy's
//     retryable incomplete-stream error.
//   - Anything else — the context's error, the credential source's (its
//     sentinel intact), a transport failure — goes as it is, a transport
//     failure in Fantasy's retryable wrapper when it is one.
func responsesError(err error) error {
	var re *responsesapi.Error
	switch {
	case errors.As(err, &re):
		if re.Final() {
			return &FinalError{
				StatusCode:      re.StatusCode,
				Code:            cmp.Or(re.Code, re.Reason),
				Type:            re.Type,
				Param:           re.Param,
				Message:         responsesMessage(re),
				contextTooLarge: re.ContextTooLarge(),
			}
		}
		return responsesProviderError(re)
	case errors.Is(err, responsesapi.ErrIncomplete):
		return fantasy.NewIncompleteStreamError()
	}
	return fantasy.WrapTransportError(err)
}

func responsesProviderError(re *responsesapi.Error) *fantasy.ProviderError {
	title := fantasy.ErrorTitleForStatusCode(re.StatusCode)
	switch {
	case re.StatusCode != 0:
	case re.Event == "response.incomplete":
		title = "response incomplete"
	default:
		title = "response failed"
	}
	pe := &fantasy.ProviderError{
		Title:              title,
		Message:            responsesMessage(re),
		StatusCode:         re.StatusCode,
		ResponseBody:       responsesErrorBody(re),
		AuthError:          re.StatusCode == http.StatusUnauthorized,
		ContextTooLargeErr: re.ContextTooLarge(),
		TransientError:     re.Transient(),
	}
	if len(re.Header) > 0 {
		pe.ResponseHeaders = make(map[string]string, len(re.Header))
		for k, v := range re.Header {
			if len(v) > 0 {
				pe.ResponseHeaders[strings.ToLower(k)] = v[len(v)-1]
			}
		}
	}
	return pe
}

// responsesMessage is the failure's display text: the server's message, or
// what the failure was when it sent none.
func responsesMessage(re *responsesapi.Error) string {
	switch {
	case re.Message != "":
		return re.Message
	case re.Event == "response.incomplete" && re.Reason != "":
		return "the response ended incomplete: " + re.Reason
	case re.Event == "response.incomplete":
		return "the response ended incomplete"
	case re.Code != "":
		return re.Code
	}
	return "the request failed"
}

// responsesErrorBody is the failure as the standard error object,
// {"error": {"code", "type", "param", "message"}} — the body ErrorNames reads
// a failure's code and type from (scrub.go structuredError). An incomplete
// response's reason stands in its code.
func responsesErrorBody(re *responsesapi.Error) []byte {
	type object struct {
		Code    string `json:"code,omitempty"`
		Type    string `json:"type,omitempty"`
		Param   string `json:"param,omitempty"`
		Message string `json:"message,omitempty"`
	}
	b, err := json.Marshal(struct {
		Error object `json:"error"`
	}{object{Code: cmp.Or(re.Code, re.Reason), Type: re.Type, Param: re.Param, Message: re.Message}})
	if err != nil {
		return nil
	}
	return b
}

// Generate is one step, collected from its stream: the route only streams.
func (m *responsesModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	stream, err := m.Stream(ctx, call)
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
			resp.Usage, resp.FinishReason = part.Usage, part.FinishReason
		case fantasy.StreamPartTypeError:
			return nil, part.Error
		}
	}
	return resp, nil
}

// errNoObjects is the answer to the object calls, which nothing in craze
// makes.
var errNoObjects = errors.New("llm: the ChatGPT plan's driver does not generate objects")

func (m *responsesModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errNoObjects
}

func (m *responsesModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errNoObjects
}
