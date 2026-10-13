package llm

// This file is the Responses drivers' only Fantasy-facing code (plan 033
// §3.9, P17, owner decision 14): it adapts package responsesapi — craze's own
// Responses client, which imports neither Fantasy nor openai-go — to
// fantasy.LanguageModel, mapping Fantasy's messages to Responses input items
// and the stream's events to Fantasy's stream parts, for both of the client's
// routes: the ChatGPT plan's (DriverChatGPT) and the API key's
// (DriverOpenAIResponses, plan 047 D12). It lives here, beside the factory
// that builds it (newSignedIn, newKeyResponses), rather than in responsesapi:
// it returns *FinalError, and package llm imports the driver, so the driver's
// package could not import llm back. Its design follows Fantasy's Apache-2.0
// responses_language_model.go (responsesapi/NOTICE).

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

// responsesModel is a model on one of the Responses drivers: the ChatGPT
// plan's (DriverChatGPT) — the Responses API at OpenAI's fixed endpoint,
// authenticated by an Auth rather than a key — or the API key's
// (DriverOpenAIResponses, plan 047 D12), the same API at the provider's
// base_url with its key. The factory builds it (newSignedIn,
// newKeyResponses), wrapped as every model is (wrap.go). What differs between
// the two is the client's route (responsesapi.Route) and the few choices
// below that are the route's.
//
// Each request is built from the step's call alone, whole, as the route
// keeps nothing between requests (store false):
//
//   - The system prompt is instructions; no system item is ever sent.
//   - The call's effort, from EffortOptions, is reasoning.effort; without one,
//     reasoning asks for the summary alone.
//   - The call's headers go with it — on the ChatGPT plan session-id, the
//     session's id, which the harness sets for that driver alone (P18) — and
//     its User-Agent.
//   - On the key route the call's cache key (WithCacheKey) is
//     prompt_cache_key and its output ceiling max_output_tokens (MR-6,
//     MR-8). On the ChatGPT route neither is ever sent, whatever the call
//     holds: the route refuses max_output_tokens, and D-74's ceiling is the
//     harness's compaction reserve alone there (the client's route decides,
//     responsesapi.Request.Body).
//   - Temperature and the other sampling settings are never sent.
//   - parallel_tool_calls is the model's ParallelToolCalls, nil leaving it to
//     the server.
//   - A reasoning item is replayed only with an id of the route's characters
//     (legalItemID, keyItemID).
type responsesModel struct {
	provider string // the table's provider id: the key a call's options are under
	model    string // the wire model
	parallel *bool
	client   *responsesapi.Client
	// latch is the Auth's usage latch (usageLatch), nil when it has none: the
	// usage-limit FinalError sets it (fail), so the plan's next request — a
	// wake's, a sub-agent's, a summary's — fails at once (P33). The key route
	// has none.
	latch usageLatch
	// route is the client's route: whether a response cut short is a finish
	// (cutShort). Which failures are final is the client's error's to say
	// (responsesapi.Error.Route, responsesError).
	route responsesapi.Route
	// ids is what a replayed reasoning item's id must look like on route.
	ids *regexp.Regexp
	// says is how the driver is named in what it tells a person: "the
	// ChatGPT plan's driver", or the key route's provider's.
	says string
}

// newResponsesModel builds r's model on auth, sending to the API at baseURL
// ("" is OpenAI's own; anything but it or a loopback URL is refused,
// responsesapi.ErrBaseURL). httpClient, when not nil, sends the requests
// (tests reach a local server through it); the client refuses redirects
// whatever it is.
func newResponsesModel(r modeltable.Resolved, auth Auth, baseURL string, httpClient *http.Client) (*responsesModel, error) {
	if auth == nil {
		return nil, fmt.Errorf("llm: provider %q (model %q): driver %q signs in, and no sign-in was given", r.ProviderID, r.Alias, r.Driver)
	}
	if strings.TrimSpace(r.WireModel) == "" {
		return nil, fmt.Errorf("llm: provider %q (model %q) names no wire model", r.ProviderID, r.Alias)
	}
	var creds responsesapi.Credentials = auth
	if r.Account != (modeltable.Account{}) {
		at, ok := auth.(accountTokens)
		if !ok {
			return nil, fmt.Errorf("llm: model %q is bound to the ChatGPT account whose list it came from, and the sign-in cannot say whose token it hands out", r.Alias)
		}
		creds = &boundCredentials{auth: auth, tokens: at, want: r.Account, alias: r.Alias}
	}
	c, err := responsesapi.NewClient(responsesapi.Config{BaseURL: baseURL, HTTPClient: httpClient, Credentials: creds})
	if err != nil {
		return nil, fmt.Errorf("llm: model %q: %w", r.Alias, err)
	}
	m := responsesModelOn(r, c, responsesapi.RouteChatGPT)
	m.latch, _ = auth.(usageLatch)
	return m, nil
}

// newKeyResponsesModel builds r's model on the key route (plan 047 D12): the
// Responses API at baseURL, which the client holds to the route's rule
// (responsesapi.ErrKeyBaseURL), with key as the bearer of every request.
// httpClient, when not nil, sends the requests; the client refuses redirects
// whatever it is. Its errors name the provider and the model, never the key.
func newKeyResponsesModel(r modeltable.Resolved, key, baseURL string, httpClient *http.Client) (*responsesModel, error) {
	if strings.TrimSpace(r.WireModel) == "" {
		return nil, fmt.Errorf("llm: provider %q (model %q) names no wire model", r.ProviderID, r.Alias)
	}
	c, err := responsesapi.NewClient(responsesapi.Config{Route: responsesapi.RouteKey, BaseURL: baseURL, HTTPClient: httpClient, APIKey: key})
	if err != nil {
		return nil, fmt.Errorf("llm: provider %q (model %q): %w", r.ProviderID, r.Alias, err)
	}
	return responsesModelOn(r, c, responsesapi.RouteKey), nil
}

// responsesModelOn is r's model on c, a client of route, with the choices
// that are the route's: the ChatGPT route's reasoning ids (legalItemID) and
// name, or the key route's (keyItemID), named for its provider.
func responsesModelOn(r modeltable.Resolved, c *responsesapi.Client, route responsesapi.Route) *responsesModel {
	m := &responsesModel{provider: r.ProviderID, model: r.WireModel, client: c,
		route: route, ids: legalItemID, says: "the ChatGPT plan's driver"}
	if r.ParallelToolCalls != nil {
		v := *r.ParallelToolCalls
		m.parallel = &v
	}
	if route == responsesapi.RouteKey {
		m.ids, m.says = keyItemID, fmt.Sprintf("provider %q's Responses driver", r.ProviderID)
	}
	return m
}

// boundCredentials are a sign-in's credentials for a model bound to the
// account whose list it came from (modeltable.Resolved.Account; plan 033
// C14r2, review r13 d): each token is handed to the client only when the
// token source's account at that moment — read with the token — is the
// model's, and is ErrOtherAccount otherwise, before any request carries it.
// Every request on the model asks here — each step of a turn, a summary, a
// wake, a retry after a 401's renewal — whichever session or sub-agent sends
// it, so another account's sign-in in the same process never pays for, or
// sees, a conversation begun on the first's model. Resolve refuses the model
// to anything opened after the switch; this is what holds one opened before
// it. Signing in to the first account again lets the model's requests go
// through once more.
type boundCredentials struct {
	auth   Auth
	tokens accountTokens
	want   modeltable.Account
	alias  string
}

func (b *boundCredentials) Token(ctx context.Context) (string, uint64, error) {
	tok, gen, subject, clientID, err := b.tokens.TokenAccount(ctx)
	if err != nil {
		return "", 0, err
	}
	if (modeltable.Account{Subject: subject, ClientID: clientID}) != b.want {
		return "", 0, fmt.Errorf("%w (model %q)", modeltable.ErrOtherAccount, b.alias)
	}
	return tok, gen, nil
}

func (b *boundCredentials) Invalidate(ctx context.Context, gen uint64) error {
	return b.auth.Invalidate(ctx, gen)
}

// fail is err, from the driver's core, as the error a step ends with
// (responsesError) — and, for the plan's usage limit, the latch set first,
// before anything above sees the failure (P33): the usage-limit code is final
// wherever it arrives, before the stream or inside it. The key route has no
// latch, and nothing final.
func (m *responsesModel) fail(err error) error {
	out := responsesError(err)
	var fe *FinalError
	if m.latch != nil && errors.As(out, &fe) && fe.Code == UsageLimitCode {
		m.latch.LatchUsageLimit()
	}
	return out
}

func (m *responsesModel) Provider() string { return m.provider }
func (m *responsesModel) Model() string    { return m.model }

// typeResponsesOptions is responsesOptions' id in Fantasy's provider-data
// registry.
const typeResponsesOptions = "craze.responses.options"

// responsesOptions are a Responses call's provider options: the reasoning
// effort to send (EffortOptions), and on the key route the prompt cache key
// (WithCacheKey).
type responsesOptions struct {
	Effort   string `json:"effort,omitempty"`
	CacheKey string `json:"cache_key,omitempty"`
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

// WithCacheKey is the provider options one request on r's model carries,
// given opts, the model's own (EffortOptions), and key, the id of the
// session that sends it (plan 047 D12, P20). For the openai-responses driver
// it is a fresh map holding a fresh responsesOptions — opts' effort, if any,
// and key as the request's prompt_cache_key, which that route needs to cache
// at all (MR-6) — built for a model with no effort as well. opts itself, the
// model's shared options, is never changed, so a session, its sub-agents and
// its side questions, each sending its own key, never see each other's.
//
// For every other driver, or with no key, it is opts as it is: no new field
// and no new map — the ChatGPT plan's affinity is its session-id header
// (harness requestHeaders), and a Chat Completions provider is sent nothing
// it did not ask for.
func WithCacheKey(r modeltable.Resolved, opts fantasy.ProviderOptions, key string) fantasy.ProviderOptions {
	if r.Driver != modeltable.DriverOpenAIResponses || key == "" {
		return opts
	}
	o := &responsesOptions{CacheKey: key}
	if cur, ok := opts[r.ProviderID].(*responsesOptions); ok && cur != nil {
		o.Effort = cur.Effort
	}
	out := make(fantasy.ProviderOptions, len(opts)+1)
	maps.Copy(out, opts)
	out[r.ProviderID] = o
	return out
}

// request is call as a Responses request, with warnings for what it could
// not carry. The output ceiling and the cache key are set whatever the
// route: the client's route decides whether they are sent
// (responsesapi.Request.Body), and the ChatGPT route never sends them.
func (m *responsesModel) request(call fantasy.Call) (responsesapi.Request, []fantasy.CallWarning) {
	instructions, input, warnings := responsesInput(call.Prompt, m.ids)
	req := responsesapi.Request{
		Model:             m.model,
		Instructions:      instructions,
		Input:             input,
		ParallelToolCalls: m.parallel,
		Headers:           maps.Clone(call.Headers),
		UserAgent:         call.UserAgent,
	}
	if o, ok := call.ProviderOptions[m.provider].(*responsesOptions); ok && o != nil {
		req.Effort, req.CacheKey = o.Effort, o.CacheKey
	}
	if call.MaxOutputTokens != nil {
		req.MaxOutputTokens = *call.MaxOutputTokens
	}
	for _, t := range call.Tools {
		ft, ok := t.(fantasy.FunctionTool)
		if !ok {
			warnings = append(warnings, fantasy.CallWarning{Type: fantasy.CallWarningTypeUnsupportedTool, Tool: t,
				Message: m.says + " sends function tools only"})
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
//     produced it) when it can be — see replayable — with an id ids
//     matches, the route's characters.
//   - A tool result is a function call output with its call id: text as a
//     string, an image as content parts (the API's function_call_output takes
//     either). The wrapper (wrap.go rule 5) has normally moved images out of
//     tool results into one user message after the run already, as it does
//     for every driver, so the image goes as user input_image, the shape the
//     route was seen to take (spike); the content form is for a caller that
//     did not wrap.
func responsesInput(prompt fantasy.Prompt, ids *regexp.Regexp) (string, []responsesapi.Item, []fantasy.CallWarning) {
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
			items = append(items, assistantItems(msg, ids)...)
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
// A reasoning part goes only when replayableWith says it can, its id matching
// ids, and only when an output item of the same response — text or a tool
// call — follows it in the message: the route pairs a reasoning item with
// what it led to, and one that led to nothing (a step cut off after it) is
// dropped.
func assistantItems(msg fantasy.Message, ids *regexp.Regexp) []responsesapi.Item {
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
			if md, ok := replayableWith(p, ids); ok && i < last {
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

// legalItemID is what a replayed reasoning item's id must look like on the
// ChatGPT route: gx's rule (prior-art-chatgpt-auth.md), the characters the
// server's own ids (rs_…) are made of.
var legalItemID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// keyItemID is what a replayed reasoning item's id must look like on the key
// route (plan 047 D12): legalItemID's characters and the colon, which Meta's
// own reasoning ids hold (rs_6acd…:rs_01a1…, MR-3).
var keyItemID = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,256}$`)

// replayable is replayableWith on the ChatGPT route's ids.
func replayable(p fantasy.ReasoningPart) (*openai.ResponsesReasoningMetadata, bool) {
	return replayableWith(p, legalItemID)
}

// replayableWith is a reasoning part's metadata when the part can be
// replayed: it carries the Responses reasoning metadata these drivers write
// (reasoningMetadata) with encrypted content — without it the route has
// nothing to replay — and an id ids matches (legalItemID, keyItemID).
// Anything else is dropped: a reasoning part from another driver, or one
// rebuilt from an interrupted step, which keeps its text and no metadata
// (turn.go).
func replayableWith(p fantasy.ReasoningPart, ids *regexp.Regexp) (*openai.ResponsesReasoningMetadata, bool) {
	md := openai.GetReasoningMetadata(p.ProviderOptions)
	if md == nil || md.EncryptedContent == nil || *md.EncryptedContent == "" || !ids.MatchString(md.ItemID) {
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
		return responsesapi.FunctionCallOutput(r.ToolCallID, resultErrorText(o)), ""
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
	return responsesapi.FunctionCallOutput(r.ToolCallID, ""), unsentResult(r.Output)
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
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: m.fail(err)})
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
		var re *responsesapi.Error
		if errors.As(err, &re) {
			if reason, ok := m.cutShort(re); ok {
				st.cut(reason, re.Usage, yield)
				return
			}
		}
		// A failed or incomplete response's usage was spent all the same:
		// a Finish part carries it first, for OnStreamFinish (the
		// summarizer counts it, compact.go observeUsage).
		if re != nil && re.Usage != nil {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, Usage: fantasyUsage(*re.Usage), FinishReason: fantasy.FinishReasonError}) {
				return
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: m.fail(err)})
	}, nil
}

// cutShort is the finish a response the key route ended short stands for
// (responsesapi.Error.CutShort, plan 047 C6): max_output_tokens is the output
// ceiling, Chat Completions' "length", and content_filter is its
// "content-filter" — a step that ends, keeping what it said, rather than one
// that failed. Any other reason, and every one on the ChatGPT route, whose
// requests carry no ceiling, is a failure as before (ok false).
func (m *responsesModel) cutShort(re *responsesapi.Error) (fantasy.FinishReason, bool) {
	if !re.CutShort() {
		return "", false
	}
	if re.Reason == "content_filter" {
		return fantasy.FinishReasonContentFilter, true
	}
	return fantasy.FinishReasonLength, true
}

// partState maps one response's events to Fantasy's stream parts.
type partState struct {
	ids      map[int]string // each item's part id, by output_index
	calls    bool           // a function call finished
	cutCalls int            // function calls the response was cut in (ItemCut)
}

// cut ends a response that stopped short (cutShort) the way Fantasy's
// OpenAI-compatible client ends a "length" or "content-filter" stream. The
// items it was cut in were closed already, in output_index order with those
// done before them (parts' ItemCut): a message's text so far kept, a call
// cut mid-arguments closed and never sent as a call. A warning says so for
// such a call. Then one Finish part with reason and the response's usage,
// counted once, and no error.
func (st *partState) cut(reason fantasy.FinishReason, usage *responsesapi.Usage, yield func(fantasy.StreamPart) bool) {
	if st.cutCalls > 0 && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeWarnings, Warnings: []fantasy.CallWarning{{
		Type:    fantasy.CallWarningTypeOther,
		Message: "a tool call was cut off before its arguments were complete, so it was not sent to run",
	}}}) {
		return
	}
	var u fantasy.Usage
	if usage != nil {
		u = fantasyUsage(*usage)
	}
	yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, Usage: u, FinishReason: reason})
}

// parts yields ev as Fantasy stream parts, false when the consumer stopped.
// A message's and a reasoning item's part id is its item id; a function
// call's is its call id, which is what Fantasy and the transcript know the
// call by — the stream announces a call only with its call id, its argument
// deltas held until then (responsesapi.ItemAdded), so each of a call's parts
// carries it.
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
	case responsesapi.ItemCut:
		// An item the key route's response was cut in (plan 047 C6): a
		// message keeps its text so far; a call cut mid-arguments is closed
		// and never sent as a call, as Fantasy's OpenAI-compatible client
		// closes one on a "length" finish; a reasoning item is left open,
		// with no encrypted content to replay, and Fantasy drops it.
		switch ev.Item.Type {
		case "message":
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: id})
		case "function_call":
			st.cutCalls++
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: id})
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
//     which the wrapper's rule 3 enforces (D-32). A 401, the token refused
//     after its one renewal, is an auth failure nothing retries, whatever
//     its body or headers say.
//   - A stream that stopped before saying how it ended is Fantasy's
//     retryable incomplete-stream error.
//   - Anything else — the context's error, the credential source's (its
//     sentinel intact), a transport failure — goes as it is, a transport
//     failure in Fantasy's retryable wrapper when it is one.
//
// On the key route (the failure's Route; plan 047 D12, P19) nothing is final
// — the ChatGPT plan's codes mean nothing there — and a failure the server
// reported is keyProviderError's: the classification the Chat Completions
// drivers get, which the harness already words and retries. Whatever is
// retryable on either route is so before any output only: after it, the
// wrapper's rule 3 fails the step (D-32).
func responsesError(err error) error {
	var re *responsesapi.Error
	switch {
	case errors.As(err, &re):
		switch {
		case re.Final():
			return &FinalError{
				StatusCode:      re.StatusCode,
				Code:            cmp.Or(re.Code, re.Reason),
				Type:            re.Type,
				Param:           re.Param,
				Message:         responsesMessage(re),
				contextTooLarge: re.ContextTooLarge(),
			}
		case re.Route == responsesapi.RouteKey:
			return keyProviderError(re)
		}
		return responsesProviderError(re)
	case errors.Is(err, responsesapi.ErrIncomplete):
		return fantasy.NewIncompleteStreamError()
	}
	return fantasy.WrapTransportError(err)
}

// responsesTitle is a failure's title: its status's, or, inside the stream,
// how the response ended.
func responsesTitle(re *responsesapi.Error) string {
	switch {
	case re.StatusCode != 0:
		return fantasy.ErrorTitleForStatusCode(re.StatusCode)
	case re.Event == "response.incomplete":
		return "response incomplete"
	}
	return "response failed"
}

func responsesProviderError(re *responsesapi.Error) *fantasy.ProviderError {
	// A 401 here is the token refused after its one renewal (Client.send
	// renews on the first), so it is never retried, whatever it says (review
	// r9 item 4): each retry would be a fresh send with a renewal of its own,
	// up to four invalidations for Fantasy's three retries. Not a transient
	// code in its body, and none of its headers either — Fantasy retries any
	// status whose x-should-retry says to (ProviderError.IsRetryable), and
	// nothing reads a 401's headers otherwise.
	refused := re.StatusCode == http.StatusUnauthorized
	pe := &fantasy.ProviderError{
		Title:              responsesTitle(re),
		Message:            responsesMessage(re),
		StatusCode:         re.StatusCode,
		ResponseBody:       errorBody(cmp.Or(re.Code, re.Reason), re.Type, re.Param, re.Message), // an incomplete response's reason as its code
		AuthError:          refused,
		ContextTooLargeErr: re.ContextTooLarge(),
		TransientError:     re.Transient() && !refused,
	}
	if len(re.Header) > 0 && !refused {
		pe.ResponseHeaders = make(map[string]string, len(re.Header))
		for k, v := range re.Header {
			if len(v) > 0 {
				pe.ResponseHeaders[strings.ToLower(k)] = v[len(v)-1]
			}
		}
	}
	return pe
}

// keyProviderError is a failure the key route's server reported, as the
// error Fantasy retries or not:
//
//   - Its status: 401 and 403 are an auth failure (the harness's ErrAuth),
//     404 a model the provider does not serve (ErrModelNotFound), and a 400
//     is refused for good, as on Chat Completions — Fantasy retries none of
//     them, by status.
//   - Retryable when the client calls it transient (responsesapi.Error.Transient
//     on the key route): a 429 or a 5xx, and inside the stream a rate limit or
//     a server's fault.
//   - Of its headers only the wait a retryable failure asked for is kept,
//     retry-after and retry-after-ms, which Fantasy honours; never
//     x-should-retry, which would make Fantasy retry whatever the status, and
//     none at all for a 401, which is the key's answer, sent once.
//   - A redirect is never followed (the client's rule): it says so, and is
//     not retried.
//   - Its body holds the standard error object, from which the scrubber reads
//     the code and type back (ErrorNames).
func keyProviderError(re *responsesapi.Error) *fantasy.ProviderError {
	title, msg := responsesTitle(re), responsesMessage(re)
	if re.StatusCode >= 300 && re.StatusCode < 400 {
		title = "redirect refused"
		msg = fmt.Sprintf("the endpoint answered with a redirect (HTTP %d), which craze does not follow", re.StatusCode)
	}
	transient := re.Transient()
	pe := &fantasy.ProviderError{
		Title:              title,
		Message:            msg,
		StatusCode:         re.StatusCode,
		ResponseBody:       errorBody(cmp.Or(re.Code, re.Reason), re.Type, re.Param, re.Message),
		AuthError:          re.StatusCode == http.StatusUnauthorized || re.StatusCode == http.StatusForbidden,
		ContextTooLargeErr: re.ContextTooLarge(),
		TransientError:     transient,
	}
	if transient {
		for _, name := range []string{"Retry-After", "Retry-After-Ms"} {
			if v := re.Header.Get(name); v != "" {
				if pe.ResponseHeaders == nil {
					pe.ResponseHeaders = map[string]string{}
				}
				pe.ResponseHeaders[strings.ToLower(name)] = v
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

// Generate is one step, collected from its stream: the route only streams.
func (m *responsesModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return collect(m.Stream(ctx, call))
}

// noObjects is the model's answer to the object calls, which nothing in craze
// makes.
func (m *responsesModel) noObjects() error {
	return errors.New("llm: " + m.says + " does not generate objects")
}

func (m *responsesModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, m.noObjects()
}

func (m *responsesModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, m.noObjects()
}
