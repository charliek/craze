package messagesapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Request is one Messages request as craze builds it (plan 044 §3.2). Body
// encodes it; the fields it has are the only ones that are ever sent.
type Request struct {
	// Model is the wire model, an id from the account's model list.
	Model string
	// MaxTokens is max_tokens, always sent: the harness's output ceiling
	// (D-74), thinking included.
	MaxTokens int64
	// System is the system prompt's text blocks, in order.
	System []string
	// Messages is the conversation, alternating user and assistant, the
	// first a user message: the API keeps nothing between requests.
	Messages []Message
	// Tools are the client tools.
	Tools []Tool
	// ToolChoice is "" (or "auto"), which leaves tool_choice out — auto is
	// the API's default — or "none".
	ToolChoice string
	// Thinking is the thinking configuration; nil sends none (a model
	// without adaptive thinking, Opus 4.5 and Haiku 4.5).
	Thinking *ThinkingConfig
	// Effort is output_config.effort; "" sends no output_config.
	Effort string
	// CacheSystem puts an ephemeral cache breakpoint on the last system
	// block: the tools and the system prompt, cached together.
	CacheSystem bool
	// CacheAuto sends the top-level cache_control, automatic caching: the
	// API places that breakpoint on the last cacheable block and moves it as
	// the conversation grows (LF-7, LF-10).
	CacheAuto bool
	// Headers are the request's own HTTP headers. The client's own
	// (protectedHeaders, and every anthropic-* header) are never taken from
	// here.
	Headers map[string]string
}

// ThinkingConfig is the request's thinking: adaptive, the only type craze
// sends (§3.3).
type ThinkingConfig struct {
	// Display is thinking.display: "summarized" for craze, so the reasoning
	// pane has text; "" leaves it to the model's default.
	Display string
	// DropMismatched sends block_binding with prefix_mismatch_behavior
	// "drop_block", and the BindingBeta header with it: a signed thinking
	// block bound to an edited prefix is dropped rather than refused with a
	// 400 (LF-4, LF-11).
	DropMismatched bool
}

// Tool is a client tool: its name, its description and its input's JSON
// Schema (nil is an empty object's).
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Role is a message's role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one message of the conversation: its role and its content
// blocks, at least one.
type Message struct {
	Role    Role
	Content []Content
}

// The content blocks' types, as the API names them; ContentBlock.Type is
// one of the last four for every block a stream reports.
const (
	TypeText             = "text"
	TypeImage            = "image"
	TypeToolResult       = "tool_result"
	TypeThinking         = "thinking"
	TypeRedactedThinking = "redacted_thinking"
	TypeToolUse          = "tool_use"
)

// Content is one content block of a message. Build one with Text, Image,
// Thinking, RedactedThinking, ToolUse or ToolResult; each encodes exactly
// the fields its type takes.
type Content struct {
	typ       string
	text      string          // text's text, thinking's thinking
	mediaType string          // image
	data      string          // image's base64 data, redacted thinking's opaque data
	signature string          // thinking
	id, name  string          // tool_use
	input     json.RawMessage // tool_use
	isError   bool            // tool_result
	content   []Content       // tool_result: text and image blocks
}

// Text is a text block.
func Text(text string) Content { return Content{typ: TypeText, text: text} }

// Image is an image block: data, base64-encoded, of mediaType (the adapter
// sends only image/jpeg, png, gif and webp, §3.3).
func Image(mediaType string, data []byte) Content {
	return Content{typ: TypeImage, mediaType: mediaType, data: base64.StdEncoding.EncodeToString(data)}
}

// Thinking is a thinking block replayed to the model that wrote it: its
// text and its signature, byte for byte as they streamed. An empty text is
// sent as "" — a signed block with no text is a block all the same.
func Thinking(thinking, signature string) Content {
	return Content{typ: TypeThinking, text: thinking, signature: signature}
}

// RedactedThinking is a redacted thinking block replayed: its opaque data.
func RedactedThinking(data string) Content { return Content{typ: TypeRedactedThinking, data: data} }

// ToolUse is a call the model made, replayed: its id, its tool's name and
// its input, a JSON object; empty input is sent as {}.
func ToolUse(id, name string, input json.RawMessage) Content {
	return Content{typ: TypeToolUse, id: id, name: name, input: input}
}

// ToolResult is a call's result: content of text and image blocks, left
// out when there is none, and isError for a failed call.
func ToolResult(toolUseID string, isError bool, content ...Content) Content {
	return Content{typ: TypeToolResult, id: toolUseID, isError: isError, content: content}
}

// cacheControl is an ephemeral breakpoint: the 5-minute TTL, the only one
// craze uses (§3.3).
type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

// wireRequest is the body, field for field, in a fixed order: identical
// requests encode to identical bytes. Its tags are the whole of what craze
// sends — never temperature, top_p, top_k, stop_sequences, metadata, speed,
// fallbacks, inference_geo or a tool's eager_input_streaming (§3.2; D2).
type wireRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int64             `json:"max_tokens"`
	Stream       bool              `json:"stream"`
	Thinking     *wireThinking     `json:"thinking,omitempty"`
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
	CacheControl *cacheControl     `json:"cache_control,omitempty"`
	System       []wireSystem      `json:"system,omitempty"`
	Tools        []wireTool        `json:"tools,omitempty"`
	ToolChoice   *wireToolChoice   `json:"tool_choice,omitempty"`
	Messages     []Message         `json:"messages"`
}

type wireThinking struct {
	Type         string            `json:"type"`
	Display      string            `json:"display,omitempty"`
	BlockBinding *wireBlockBinding `json:"block_binding,omitempty"`
}

type wireBlockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

type wireOutputConfig struct {
	Effort string `json:"effort"`
}

type wireSystem struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireToolChoice struct {
	Type string `json:"type"`
}

// emptySchema is the input schema of a tool that gives none.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// binding reports whether the request carries block_binding, and so the
// BindingBeta header.
func (r Request) binding() bool { return r.Thinking != nil && r.Thinking.DropMismatched }

// Body is the request's JSON body: stream true, max_tokens always, and only
// the fields wireRequest has. A request that could only be refused — no
// model, no max_tokens, no messages, a message with no blocks or another
// role, a block built without a constructor, a tool result holding anything
// but text and images, a tool choice craze does not send — is an error here,
// before anything is sent.
func (r Request) Body() ([]byte, error) {
	if strings.TrimSpace(r.Model) == "" {
		return nil, errors.New("messagesapi: the request names no model")
	}
	if r.MaxTokens <= 0 {
		return nil, errors.New("messagesapi: the request's max_tokens is not positive")
	}
	if len(r.Messages) == 0 {
		return nil, errors.New("messagesapi: the request has no messages")
	}
	for i, m := range r.Messages {
		if err := m.check(); err != nil {
			return nil, fmt.Errorf("messagesapi: message %d: %w", i, err)
		}
	}
	w := wireRequest{Model: r.Model, MaxTokens: r.MaxTokens, Stream: true, Messages: r.Messages}
	if t := r.Thinking; t != nil {
		w.Thinking = &wireThinking{Type: "adaptive", Display: t.Display}
		if t.DropMismatched {
			w.Thinking.BlockBinding = &wireBlockBinding{PrefixMismatchBehavior: "drop_block"}
		}
	}
	if r.Effort != "" {
		w.OutputConfig = &wireOutputConfig{Effort: r.Effort}
	}
	if r.CacheAuto {
		w.CacheControl = ephemeral
	}
	for i, s := range r.System {
		block := wireSystem{Type: TypeText, Text: s}
		if r.CacheSystem && i == len(r.System)-1 {
			block.CacheControl = ephemeral
		}
		w.System = append(w.System, block)
	}
	for _, t := range r.Tools {
		schema := t.InputSchema
		if len(bytes.TrimSpace(schema)) == 0 {
			schema = emptySchema
		}
		w.Tools = append(w.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	switch r.ToolChoice {
	case "", "auto":
	case "none":
		w.ToolChoice = &wireToolChoice{Type: "none"}
	default:
		return nil, errors.New("messagesapi: the request's tool choice is neither auto nor none")
	}
	return encode(w)
}

// encode is v as JSON with nothing HTML-escaped — a prompt's < and & go as
// themselves, as the user wrote them — and no trailing newline. Every value
// in a body, the blocks' own encodings included, goes through it: a
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

func (m Message) check() error {
	if m.Role != RoleUser && m.Role != RoleAssistant {
		return errors.New("it is neither a user nor an assistant message")
	}
	if len(m.Content) == 0 {
		return errors.New("it has no content blocks")
	}
	for _, c := range m.Content {
		if c.typ == "" {
			return errors.New("a content block built without a constructor")
		}
		if c.typ != TypeToolResult {
			continue
		}
		for _, inner := range c.content {
			if inner.typ != TypeText && inner.typ != TypeImage {
				return errors.New("a tool result holds a block that is neither text nor an image")
			}
		}
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	return encode(struct {
		Role    Role      `json:"role"`
		Content []Content `json:"content"`
	}{m.Role, m.Content})
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func (c Content) MarshalJSON() ([]byte, error) {
	switch c.typ {
	case TypeText:
		return encode(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{c.typ, c.text})
	case TypeImage:
		return encode(struct {
			Type   string      `json:"type"`
			Source imageSource `json:"source"`
		}{c.typ, imageSource{Type: "base64", MediaType: c.mediaType, Data: c.data}})
	case TypeThinking:
		// Neither field is omitempty: the API wants a signed block back
		// exactly as it streamed, an empty thinking text included.
		return encode(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{c.typ, c.text, c.signature})
	case TypeRedactedThinking:
		return encode(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{c.typ, c.data})
	case TypeToolUse:
		input := c.input
		if len(bytes.TrimSpace(input)) == 0 {
			input = json.RawMessage("{}")
		}
		return encode(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{c.typ, c.id, c.name, input})
	case TypeToolResult:
		return encode(struct {
			Type      string    `json:"type"`
			ToolUseID string    `json:"tool_use_id"`
			Content   []Content `json:"content,omitempty"`
			IsError   bool      `json:"is_error,omitempty"`
		}{c.typ, c.id, c.content, c.isError})
	}
	return nil, errors.New("messagesapi: a content block built without a constructor")
}
