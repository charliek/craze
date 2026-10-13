package responsesapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Namespace is the namespace every function tool is sent in (P32): the
// route asks for function tools grouped in namespaces (preview-limitations
// in OpenAI's Sign in with ChatGPT docs), and the spike saw the same call
// shape either way — a call names the tool as it is, with "namespace":
// "craze" beside it. A replayed call carries it too.
const Namespace = "craze"

// namespaceDescription is the namespace's own description.
const namespaceDescription = "craze tools"

// Request is one Responses request as craze builds it. Body encodes it; the
// fields it has are the only ones that are ever sent.
type Request struct {
	// Model is the wire model, a slug from the account's model list.
	Model string
	// Instructions is the system prompt, sent as instructions: the route
	// refuses a system item with a 400 (spike).
	Instructions string
	// Input is the conversation, in order: the route keeps nothing between
	// requests (store false), so every request carries all of it.
	Input []Item
	// Tools are the function tools, sent inside one namespace (Namespace) on
	// the ChatGPT route and as plain functions on the key route. With none,
	// tool_choice and parallel_tool_calls are not sent either.
	Tools []Tool
	// ToolChoice is "auto" (and "" means it), "none", "required", or a tool's
	// name.
	ToolChoice string
	// ParallelToolCalls is parallel_tool_calls: nil leaves it to the server.
	ParallelToolCalls *bool
	// Effort is reasoning.effort; "" sends reasoning with its summary alone,
	// for a model with no effort control.
	Effort string
	// MaxOutputTokens is max_output_tokens, the call's output ceiling (D-74),
	// sent on the key route alone (MR-8), and there only when above 0. The
	// ChatGPT route refuses the field with a 400, so it never goes there.
	MaxOutputTokens int64
	// CacheKey is prompt_cache_key, the session's id, sent on the key route
	// alone, and there only when not "": the route caches nothing without it
	// (MR-6). The ChatGPT route's cache affinity is its session-id header
	// instead, and the server replaces a prompt_cache_key there.
	CacheKey string
	// Headers are the request's own HTTP headers — on the ChatGPT route
	// session-id, the session's id, which gives the server its cache affinity
	// (P18). The client's own headers (protectedHeaders) are never taken
	// from here.
	Headers map[string]string
	// UserAgent overrides the client's User-Agent for this request.
	UserAgent string
}

// Tool is a function tool: its name, its description and its parameters'
// JSON Schema.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// wireRequest is the body, field for field, on either route. Its tags are the
// whole of what craze sends: never temperature, top_p, top_logprobs,
// service_tier (not applied on the ChatGPT route), previous_response_id
// (nothing is stored), prompt_cache_retention, metadata, user, truncation,
// background, conversation, max_tool_calls, moderation, multi_agent, prompt
// or safety_identifier — the fields the ChatGPT route's preview limitations
// list as unsupported, or that it ignores (plan 033 §3.9).
//
// What differs is the route's (body). Tools is the ChatGPT route's one
// namespace ([]wireNamespace), or the key route's plain top-level functions
// ([]wireFunction; MR-9: Meta prefixes a namespace onto the call's name).
// MaxOutputTokens, the call's output ceiling (MR-8), and PromptCacheKey, the
// session's id (MR-6), are the key route's alone: the ChatGPT route refuses
// max_output_tokens with a 400 (D-74's ceiling is the harness's compaction
// reserve alone there) and replaces prompt_cache_key (session-id does its
// work), so they stay empty and unsent there.
type wireRequest struct {
	Model             string        `json:"model"`
	Instructions      string        `json:"instructions,omitempty"`
	Input             []Item        `json:"input"`
	Store             bool          `json:"store"`
	Stream            bool          `json:"stream"`
	Include           []string      `json:"include"`
	Reasoning         wireReasoning `json:"reasoning"`
	Tools             any           `json:"tools,omitempty"`
	ToolChoice        any           `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   int64         `json:"max_output_tokens,omitempty"`
	PromptCacheKey    string        `json:"prompt_cache_key,omitempty"`
}

// wireReasoning asks for the reasoning summary on every request ("auto": the
// server answers "detailed", spike), and for the effort when there is one.
type wireReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary"`
}

type wireNamespace struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Tools       []wireFunction `json:"tools"`
}

// wireFunction is a function tool. strict is false: craze's tool schemas are
// not written for strict mode, which requires every property and refuses
// additional ones.
type wireFunction struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

// Body is the request's JSON body on the ChatGPT route: store false, stream
// true, the encrypted reasoning included, and nothing the route rejects
// (wireRequest) — MaxOutputTokens and CacheKey among them, whatever they
// hold.
func (r Request) Body() ([]byte, error) { return r.body(RouteChatGPT) }

// body is the request's JSON body on route: the ChatGPT route's (Body), or
// the key route's — the same body but for its tools, which are plain
// functions, its replayed calls, which carry no namespace, and its output
// ceiling and cache key (wireRequest).
func (r Request) body(route Route) ([]byte, error) {
	if strings.TrimSpace(r.Model) == "" {
		return nil, errors.New("responsesapi: the request names no model")
	}
	w := wireRequest{
		Model:        r.Model,
		Instructions: r.Instructions,
		Input:        r.Input,
		Store:        false,
		Stream:       true,
		Include:      []string{"reasoning.encrypted_content"},
		Reasoning:    wireReasoning{Effort: r.Effort, Summary: "auto"},
	}
	if w.Input == nil {
		w.Input = []Item{}
	}
	fns := make([]wireFunction, 0, len(r.Tools))
	for _, t := range r.Tools {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		fns = append(fns, wireFunction{Type: "function", Name: t.Name, Description: t.Description, Parameters: params})
	}
	switch route {
	case RouteChatGPT:
		if len(fns) > 0 {
			w.Tools = []wireNamespace{{Type: "namespace", Name: Namespace, Description: namespaceDescription, Tools: fns}}
		}
	case RouteKey:
		w.Input = plainCalls(w.Input)
		w.PromptCacheKey = r.CacheKey
		if r.MaxOutputTokens > 0 {
			w.MaxOutputTokens = r.MaxOutputTokens
		}
		if len(fns) > 0 {
			w.Tools = fns
		}
	default:
		return nil, fmt.Errorf("responsesapi: unknown route %d", route)
	}
	if len(fns) > 0 {
		w.ToolChoice = toolChoice(r.ToolChoice)
		w.ParallelToolCalls = r.ParallelToolCalls
	}
	return encode(w)
}

// plainCalls is input with each function call marked to go without a
// namespace, as the key route's tools have none (MR-9): a copy, so the
// caller's items are as they were.
func plainCalls(input []Item) []Item {
	out := slices.Clone(input)
	for i := range out {
		if out[i].kind == functionCall {
			out[i].plain = true
		}
	}
	return out
}

// toolChoice is tool_choice's value: a mode as itself, a tool's name as the
// function choice that names it.
func toolChoice(c string) any {
	switch c {
	case "", "auto":
		return "auto"
	case "none", "required":
		return c
	}
	return struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}{"function", c}
}

// encode is v as JSON with nothing HTML-escaped — a prompt's < and & go as
// themselves, as the user wrote them — and no trailing newline. Every value
// in a body, the items' own encodings included, goes through it: a
// MarshalJSON that escaped would be kept escaped by the encoder around it.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Item is one element of a request's input: a message, a reasoning item, a
// function call or a function call's output. Build one with the
// constructors; each encodes exactly the fields its type takes.
type Item struct {
	kind      itemKind
	content   []Content // a user message's parts, or a function output's
	text      string    // an assistant message's text, or a function output's
	id        string    // a reasoning item's id
	summary   []string  // a reasoning item's summary parts
	encrypted string    // a reasoning item's encrypted content
	callID    string
	name      string
	arguments string
	plain     bool // a function call sent with no namespace: the key route's (plainCalls)
}

type itemKind int

const (
	userMessage itemKind = iota + 1
	assistantMessage
	reasoningItem
	functionCall
	functionCallOutput
	functionCallOutputContent
)

// UserMessage is a user message of content: text and images.
func UserMessage(content ...Content) Item { return Item{kind: userMessage, content: content} }

// AssistantMessage is a message the model wrote, replayed as output_text.
func AssistantMessage(text string) Item { return Item{kind: assistantMessage, text: text} }

// Reasoning is a reasoning item replayed to the model that produced it
// (D-33): its id, its summary parts and its encrypted content, without which
// the route has nothing to replay (the adapter drops such a part).
func Reasoning(id string, summary []string, encrypted string) Item {
	return Item{kind: reasoningItem, id: id, summary: summary, encrypted: encrypted}
}

// FunctionCall is a call the model made, replayed with its call_id, in
// Namespace on the ChatGPT route and with none on the key route; arguments ""
// is sent as "{}".
func FunctionCall(callID, name, arguments string) Item {
	return Item{kind: functionCall, callID: callID, name: name, arguments: arguments}
}

// FunctionCallOutput is a call's result as text.
func FunctionCallOutput(callID, output string) Item {
	return Item{kind: functionCallOutput, callID: callID, text: output}
}

// FunctionCallOutputContent is a call's result as content parts — text and
// images — which the API's function_call_output takes as well as a string.
func FunctionCallOutputContent(callID string, content ...Content) Item {
	return Item{kind: functionCallOutputContent, callID: callID, content: content}
}

func (it Item) MarshalJSON() ([]byte, error) {
	switch it.kind {
	case userMessage:
		return encode(struct {
			Type    string    `json:"type"`
			Role    string    `json:"role"`
			Content []Content `json:"content"`
		}{"message", "user", nonNil(it.content)})
	case assistantMessage:
		return encode(struct {
			Type    string    `json:"type"`
			Role    string    `json:"role"`
			Content []Content `json:"content"`
		}{"message", "assistant", []Content{{kind: outputText, text: it.text}}})
	case reasoningItem:
		summary := make([]summaryText, 0, len(it.summary))
		for _, s := range it.summary {
			summary = append(summary, summaryText{Type: "summary_text", Text: s})
		}
		return encode(struct {
			Type             string        `json:"type"`
			ID               string        `json:"id,omitempty"`
			Summary          []summaryText `json:"summary"`
			EncryptedContent string        `json:"encrypted_content,omitempty"`
		}{"reasoning", it.id, summary, it.encrypted})
	case functionCall:
		args := it.arguments
		if args == "" {
			args = "{}"
		}
		ns := Namespace
		if it.plain {
			ns = "" // the key route's: no namespace at all (plainCalls)
		}
		return encode(struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Namespace string `json:"namespace,omitempty"`
			Arguments string `json:"arguments"`
		}{"function_call", it.callID, it.name, ns, args})
	case functionCallOutput:
		return encode(struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}{"function_call_output", it.callID, it.text})
	case functionCallOutputContent:
		return encode(struct {
			Type   string    `json:"type"`
			CallID string    `json:"call_id"`
			Output []Content `json:"output"`
		}{"function_call_output", it.callID, nonNil(it.content)})
	}
	return nil, errors.New("responsesapi: an input item built without a constructor")
}

type summaryText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func nonNil(c []Content) []Content {
	if c == nil {
		return []Content{}
	}
	return c
}

// Content is one part of a message or a function output: text, or an image
// as a data URL. Build one with InputText or InputImage.
type Content struct {
	kind     contentKind
	text     string
	imageURL string
}

type contentKind int

const (
	inputText contentKind = iota + 1
	inputImage
	outputText
)

// InputText is a text part.
func InputText(text string) Content { return Content{kind: inputText, text: text} }

// InputImage is an image part: data as a base64 data URL of mediaType,
// which the route accepts for a model that takes images (spike).
func InputImage(mediaType string, data []byte) Content {
	return Content{kind: inputImage, imageURL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)}
}

func (c Content) MarshalJSON() ([]byte, error) {
	switch c.kind {
	case inputText, outputText:
		typ := "input_text"
		if c.kind == outputText {
			typ = "output_text"
		}
		return encode(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{typ, c.text})
	case inputImage:
		return encode(struct {
			Type     string `json:"type"`
			ImageURL string `json:"image_url"`
		}{"input_image", c.imageURL})
	}
	return nil, errors.New("responsesapi: a content part built without a constructor")
}
