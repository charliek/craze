package llm

// This file is Anthropic's driver's only Fantasy-facing code (plan 044 §3.3):
// it adapts package messagesapi — craze's own Messages API client, which
// imports neither Fantasy nor any SDK — to fantasy.LanguageModel, mapping
// Fantasy's messages to Messages API content blocks and the stream's events
// to Fantasy's stream parts. It lives here, beside the factory that builds
// it (New), as responses_adapter.go does and for the same reason: it returns
// *FinalError, and package llm imports the client, so the client's package
// could not import llm back. Its design follows Fantasy's Apache-2.0
// providers/anthropic (messagesapi/NOTICE).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// messagesModel is a model on Anthropic's driver (DriverAnthropic): the
// Messages API at Anthropic's fixed endpoint, funded by an API key. The
// factory builds it (New), wrapped as every model is (wrap.go).
//
// Each request is built from the step's call alone, whole — the API keeps
// nothing between requests — and deterministically, so the history a later
// step replays encodes to the bytes it encoded to before: the prompt cache
// and the thinking blocks' signatures see the same prefix (messagesPrompt).
//
//   - The system prompt is the system blocks, the last with a cache
//     breakpoint, and the request carries the top-level automatic one (§3.2:
//     two of the four).
//   - Thinking is adaptive, summarized, and bound with drop_block behind its
//     beta, for a model whose thinking is adaptive (Resolved.Thinking); a
//     model with none sends no thinking field.
//   - The call's effort, from EffortOptions, is output_config.effort.
//   - The call's output ceiling is max_tokens, always sent (64000 without
//     one); temperature and the other sampling settings are never sent,
//     which these models refuse.
type messagesModel struct {
	provider string // the table's provider id: the key a call's options are under
	model    string // the wire model
	thinking bool   // ask for adaptive thinking
	client   *messagesapi.Client
}

// newMessagesModel builds r's model on key, sending to the API at baseURL
// (messagesapi.DefaultBaseURL, or a loopback URL; anything else is refused,
// messagesapi.ErrBaseURL). httpClient, when not nil, sends the requests
// (tests reach a local server through it); the client refuses redirects
// whatever it is. No error it returns can carry the key.
func newMessagesModel(r modeltable.Resolved, key, baseURL string, httpClient *http.Client) (*messagesModel, error) {
	if strings.TrimSpace(r.WireModel) == "" {
		return nil, fmt.Errorf("llm: provider %q (model %q) names no wire model", r.ProviderID, r.Alias)
	}
	var thinking bool
	switch r.Thinking {
	case modeltable.ThinkingAdaptive, "":
		// "" is a Resolved built outside the table, which resolves an
		// unknown model's thinking the same way.
		thinking = true
	case modeltable.ThinkingNone:
	default:
		return nil, fmt.Errorf("llm: model %q: unknown thinking %q", r.Alias, r.Thinking)
	}
	c, err := messagesapi.NewClient(messagesapi.Config{BaseURL: baseURL, APIKey: key, HTTPClient: httpClient})
	if err != nil {
		return nil, fmt.Errorf("llm: model %q: %w", r.Alias, err)
	}
	return &messagesModel{provider: r.ProviderID, model: r.WireModel, thinking: thinking, client: c}, nil
}

func (m *messagesModel) Provider() string { return m.provider }
func (m *messagesModel) Model() string    { return m.model }

// The provider-data types this driver keeps in Fantasy's registry, so a
// call's options and a stored step's reasoning survive JSON: the
// transcript's file and its replay (store/golden_test.go
// TestAnthropicThinkingSurvives).
const (
	typeAnthropicOptions  = "craze.anthropic.options"
	typeAnthropicThinking = "craze.anthropic.thinking"
)

// anthropicOptions are an Anthropic call's provider options (EffortOptions):
// the effort to send.
type anthropicOptions struct {
	Effort string `json:"effort,omitempty"`
}

// anthropicThinking is the metadata a thinking block's reasoning part keeps,
// under thinkingKey (§3.3): its signature, or a redacted block's opaque
// data — what the next request must send back, byte for byte, for the model
// to read its own reasoning again (replayThinking). The text is the part's own.
type anthropicThinking struct {
	Signature    string `json:"signature,omitempty"`
	RedactedData string `json:"redacted_data,omitempty"`
}

// thinkingKey is the key a reasoning part's anthropicThinking is under in
// its provider metadata, and so in its options once it is history. It names
// no provider id, so no other driver's metadata is ever read as this one's.
const thinkingKey = typeAnthropicThinking

func init() {
	fantasy.RegisterProviderType(typeAnthropicOptions, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var v anthropicOptions
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return &v, nil
	})
	fantasy.RegisterProviderType(typeAnthropicThinking, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var v anthropicThinking
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return &v, nil
	})
}

func (*anthropicOptions) Options() {}

func (o anthropicOptions) MarshalJSON() ([]byte, error) {
	type plain anthropicOptions
	return fantasy.MarshalProviderType(typeAnthropicOptions, plain(o))
}

func (o *anthropicOptions) UnmarshalJSON(data []byte) error {
	type plain anthropicOptions
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	*o = anthropicOptions(p)
	return nil
}

func (*anthropicThinking) Options() {}

func (t anthropicThinking) MarshalJSON() ([]byte, error) {
	type plain anthropicThinking
	return fantasy.MarshalProviderType(typeAnthropicThinking, plain(t))
}

func (t *anthropicThinking) UnmarshalJSON(data []byte) error {
	type plain anthropicThinking
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	*t = anthropicThinking(p)
	return nil
}

// anthropicFinishKey is the provider-metadata key a step's finish carries
// AnthropicFinish under. Like the wrapper's craze.raw_finish it names no
// provider id, rides in the step's metadata, which Fantasy never puts in a
// message, and so reaches neither the transcript nor the wire.
const anthropicFinishKey = "craze.anthropic"

// AnthropicFinish is what a step on Anthropic's driver said beyond its
// finish reason (§3.3), from its finish's provider metadata
// (AnthropicFinishOf).
type AnthropicFinish struct {
	// RequestID is the response's request-id, "" when it sent none.
	RequestID string `json:"request_id,omitempty"`
	// ThinkingDropped is how many thinking blocks the API dropped from the
	// request before running it (input_transformations' thinking_dropped):
	// blocks bound to a prefix that has since been edited (LF-4, LF-11).
	ThinkingDropped int `json:"thinking_dropped"`
	// RefusalCategory is a refusal's category ("cyber", …) as an identifier
	// of at most 32 characters, "" for any other finish, a refusal that named
	// none, or one that named something else.
	RefusalCategory string `json:"refusal_category,omitempty"`
}

func (*AnthropicFinish) Options() {}

func (f *AnthropicFinish) MarshalJSON() ([]byte, error) {
	type plain AnthropicFinish
	return json.Marshal((*plain)(f))
}

func (f *AnthropicFinish) UnmarshalJSON(b []byte) error {
	type plain AnthropicFinish
	return json.Unmarshal(b, (*plain)(f))
}

// AnthropicFinishOf is a step's AnthropicFinish, from the step's provider
// metadata (fantasy.StepResult.ProviderMetadata); ok is false for a step on
// any other driver.
func AnthropicFinishOf(md fantasy.ProviderMetadata) (AnthropicFinish, bool) {
	f, ok := md[anthropicFinishKey].(*AnthropicFinish)
	if !ok || f == nil {
		return AnthropicFinish{}, false
	}
	return *f, true
}

// refusalCategory is what a refusal's category must look like to be kept
// (§3.4): a short lowercase identifier, as every category the API documents
// is.
var refusalCategory = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// thinkingDisplay is the thinking's display (§3.3): summarized, so the
// reasoning pane has text — the newer models' default, omitted, would show an
// empty pane and a long pause.
const thinkingDisplay = "summarized"

// defaultMaxTokens is max_tokens for a call with no output ceiling: the
// harness always sets one (D-74), so this is for a caller that does not.
const defaultMaxTokens = 64000

// request is call as a Messages request, with warnings for what it could not
// carry, or the error for a call that could only be refused.
func (m *messagesModel) request(call fantasy.Call) (messagesapi.Request, []fantasy.CallWarning, error) {
	p, err := messagesPrompt(call.Prompt)
	if err != nil {
		return messagesapi.Request{}, nil, err
	}
	req := messagesapi.Request{
		Model:       m.model,
		MaxTokens:   defaultMaxTokens,
		System:      p.system,
		Messages:    p.messages,
		CacheSystem: true,
		CacheAuto:   true,
		Headers:     maps.Clone(call.Headers),
	}
	if n := call.MaxOutputTokens; n != nil && *n > 0 {
		req.MaxTokens = *n
	}
	if m.thinking {
		req.Thinking = &messagesapi.ThinkingConfig{Display: thinkingDisplay, DropMismatched: true}
	}
	if o, ok := call.ProviderOptions[m.provider].(*anthropicOptions); ok && o != nil {
		req.Effort = o.Effort
	}
	warnings := p.warnings
	for _, t := range call.Tools {
		ft, ok := t.(fantasy.FunctionTool)
		if !ok {
			warnings = append(warnings, fantasy.CallWarning{Type: fantasy.CallWarningTypeUnsupportedTool, Tool: t,
				Message: "Anthropic's driver sends function tools only"})
			continue
		}
		tool := messagesapi.Tool{Name: ft.Name, Description: ft.Description}
		if ft.InputSchema != nil {
			if tool.InputSchema, err = encodeJSON(ft.InputSchema); err != nil {
				return messagesapi.Request{}, nil, fmt.Errorf("llm: tool %q's input schema: %w", ft.Name, err)
			}
		}
		req.Tools = append(req.Tools, tool)
	}
	if tc := call.ToolChoice; tc != nil {
		switch *tc {
		case "", fantasy.ToolChoiceAuto:
		case fantasy.ToolChoiceNone:
			req.ToolChoice = "none"
		default:
			// A forced tool is a 400 on the adaptive-thinking models (WF §7).
			warnings = append(warnings, fantasy.CallWarning{Type: fantasy.CallWarningTypeUnsupportedSetting, Setting: "tool_choice",
				Message: "Anthropic's driver sends a required or specific tool choice as auto"})
		}
	}
	return req, warnings, nil
}

// encodeJSON is v as JSON with nothing HTML-escaped, as the request's body
// is (messagesapi), and no trailing newline.
func encodeJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// prompt is a call's prompt as a request carries it.
type prompt struct {
	system   []string
	messages []messagesapi.Message
	warnings []fantasy.CallWarning
}

// The prompt's limits and fixed texts (§3.3).
const (
	// maxImages is the most images one request may carry: the API's own
	// limit. The oldest beyond it go as placeholders, the newest kept.
	maxImages = 100
	// continues opens a conversation whose first message is not the user's:
	// the API's first message must be.
	continues = "(conversation continues)"
)

// imageTypes are the image types the API reads; another is sent as a
// placeholder.
var imageTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// messagesPrompt is a prompt as the request's system blocks and messages
// (Fantasy's anthropic toPrompt, adapted to what the API refuses — P20 —
// and deterministic, so identical history encodes identically):
//
//  1. System messages' text, in order, are the system blocks; an empty one
//     is dropped.
//  2. A user message's text and images are text and image blocks: an image
//     of a type the API does not read, or one beyond the request's 100
//     (images), is a text placeholder, and another file a placeholder with a
//     warning. A tool message is a user message of tool_result blocks: its
//     text, an error's text with is_error, or its image (toolResult).
//  3. An assistant message's parts are blocks in their order (assistant): a
//     reasoning part this driver wrote is its thinking block, replayed byte
//     for byte, and any other reasoning is dropped; text, unless blank; a
//     tool call, its input a JSON object.
//  4. Tool call ids, and tool names, are mapped to what the API accepts, one
//     mapping per request (idMap).
//  5. Text that is empty or blank is never a block, a message left with no
//     blocks is dropped, and consecutive messages of one role are merged,
//     tool results first. A conversation whose first message is not the
//     user's opens with a placeholder; one that ends with the assistant's is
//     refused: the API would take it as a prefill, which these models refuse.
func messagesPrompt(fp fantasy.Prompt) (prompt, error) {
	b := promptBuilder{ids: newIDMap(), names: newIDMap()}
	b.skip = max(0, countImages(fp)-maxImages)
	var out prompt
	var msgs []turnMessage
	for _, msg := range fp {
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			for _, part := range msg.Content {
				if t, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && !blank(t.Text) {
					out.system = append(out.system, t.Text)
				}
			}
		case fantasy.MessageRoleUser:
			msgs = append(msgs, turnMessage{messagesapi.RoleUser, b.user(msg)})
		case fantasy.MessageRoleAssistant:
			msgs = append(msgs, turnMessage{messagesapi.RoleAssistant, b.assistant(msg)})
		case fantasy.MessageRoleTool:
			msgs = append(msgs, turnMessage{messagesapi.RoleUser, b.toolResults(msg)})
		}
	}
	out.warnings = b.warnings
	msgs = mergeMessages(msgs)
	switch {
	case len(msgs) == 0:
		return prompt{}, errors.New("llm: the prompt holds nothing a Messages request can carry")
	case msgs[len(msgs)-1].role != messagesapi.RoleUser:
		return prompt{}, errors.New("llm: the prompt ends with the assistant's message, which the Messages API would take as a prefill: refused")
	case msgs[0].role != messagesapi.RoleUser:
		msgs = append([]turnMessage{{messagesapi.RoleUser, []turnBlock{{c: messagesapi.Text(continues)}}}}, msgs...)
	}
	for _, m := range msgs {
		content := make([]messagesapi.Content, len(m.blocks))
		for i, blk := range m.blocks {
			content[i] = blk.c
		}
		out.messages = append(out.messages, messagesapi.Message{Role: m.role, Content: content})
	}
	return out, nil
}

// turnMessage is a message being built: its role and its blocks.
type turnMessage struct {
	role   messagesapi.Role
	blocks []turnBlock
}

// turnBlock is one block of a turnMessage, and whether it is a tool result,
// which a merge puts first.
type turnBlock struct {
	c      messagesapi.Content
	result bool
}

// mergeMessages drops every message with no blocks and merges each run of
// messages of one role into one: the API wants the roles to alternate. A
// user run's tool results go first, in order, then its other blocks in
// order: the API wants a call's results at the start of the message after
// it, and the harness puts what it splices in (a steer, a reminder, the
// images a tool returned) after them (store/pairing.go).
func mergeMessages(msgs []turnMessage) []turnMessage {
	var out []turnMessage
	for _, m := range msgs {
		if len(m.blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].role == m.role {
			out[n-1].blocks = append(out[n-1].blocks, m.blocks...)
			continue
		}
		out = append(out, turnMessage{role: m.role, blocks: slices.Clone(m.blocks)})
	}
	for i := range out {
		if out[i].role == messagesapi.RoleUser {
			slices.SortStableFunc(out[i].blocks, func(a, b turnBlock) int {
				switch {
				case a.result == b.result:
					return 0
				case a.result:
					return -1
				}
				return 1
			})
		}
	}
	return out
}

// promptBuilder holds what one request's mapping shares across messages.
type promptBuilder struct {
	ids, names *idMap
	// skip is how many of the request's images, oldest first, go as
	// placeholders (maxImages); seen counts the images met so far.
	skip, seen int
	warnings   []fantasy.CallWarning
}

func (b *promptBuilder) warn(msg string) {
	b.warnings = append(b.warnings, fantasy.CallWarning{Type: fantasy.CallWarningTypeOther, Message: msg})
}

// countImages is how many images the prompt would send, in the order the
// mapping meets them: a user's image parts and the images tool results hold.
func countImages(fp fantasy.Prompt) int {
	n := 0
	for _, msg := range fp {
		for _, part := range msg.Content {
			switch msg.Role {
			case fantasy.MessageRoleUser:
				if f, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok && sendableImage(f.MediaType, f.Data) {
					n++
				}
			case fantasy.MessageRoleTool:
				if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok && !r.ProviderExecuted {
					if _, _, ok := resultImage(r.Output); ok {
						n++
					}
				}
			}
		}
	}
	return n
}

// sendableImage reports whether an image of mediaType with data goes as an
// image block: a type the API reads, and some data.
func sendableImage(mediaType string, data []byte) bool {
	return slices.Contains(imageTypes, mediaType) && len(data) > 0
}

// resultImage is a tool result's image, when it holds one the API reads: its
// type and its bytes.
func resultImage(out fantasy.ToolResultOutputContent) (string, []byte, bool) {
	o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](out)
	if !ok {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(o.Data)
	if err != nil || !sendableImage(o.MediaType, data) {
		return "", nil, false
	}
	return o.MediaType, data, true
}

// image is an image the API reads as its block, or — beyond the request's
// newest maxImages — as a placeholder.
func (b *promptBuilder) image(mediaType string, data []byte) messagesapi.Content {
	b.seen++
	if b.seen <= b.skip {
		return messagesapi.Text(omitted("image", mediaType))
	}
	return messagesapi.Image(mediaType, data)
}

// mediaTypeShape is what a media type must look like to be named in a
// placeholder.
var mediaTypeShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]{0,63}/[A-Za-z0-9][A-Za-z0-9.+-]{0,63}$`)

// omitted is the placeholder for an image or another file that is not sent:
// "[image omitted: image/bmp]".
func omitted(kind, mediaType string) string {
	return "[" + kind + " omitted: " + shownMediaType(mediaType) + "]"
}

// shownMediaType is mediaType when it looks like one, to name in a
// placeholder or a warning, and a fixed phrase otherwise.
func shownMediaType(mediaType string) string {
	if !mediaTypeShape.MatchString(mediaType) {
		return "an unknown type"
	}
	return mediaType
}

// blank reports whether s is text the API would refuse as a block's: empty,
// or white space alone.
func blank(s string) bool { return strings.TrimSpace(s) == "" }

// user is a user message's blocks: its text and images.
func (b *promptBuilder) user(msg fantasy.Message) []turnBlock {
	var out []turnBlock
	for _, part := range msg.Content {
		if t, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			if !blank(t.Text) {
				out = append(out, turnBlock{c: messagesapi.Text(t.Text)})
			}
		} else if f, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			switch {
			case sendableImage(f.MediaType, f.Data):
				out = append(out, turnBlock{c: b.image(f.MediaType, f.Data)})
			case strings.HasPrefix(f.MediaType, "image/"):
				out = append(out, turnBlock{c: messagesapi.Text(omitted("image", f.MediaType))})
			default:
				b.warn(fmt.Sprintf("a file (%s) is sent as a placeholder: the driver sends images only", shownMediaType(f.MediaType)))
				out = append(out, turnBlock{c: messagesapi.Text(omitted("file", f.MediaType))})
			}
		}
	}
	return out
}

// toolResults is a tool message's blocks: a tool_result for each result,
// answering its call by the call's mapped id.
func (b *promptBuilder) toolResults(msg fantasy.Message) []turnBlock {
	var out []turnBlock
	for _, part := range msg.Content {
		r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok || r.ProviderExecuted {
			continue
		}
		out = append(out, turnBlock{c: b.toolResult(r), result: true})
	}
	return out
}

// toolResult is one result's block: its text, or no content when it has
// none; an error's text, with is_error; or an image the tool returned (after
// the wrapper's regroup there normally is none, rule 5), with its text.
func (b *promptBuilder) toolResult(r fantasy.ToolResultPart) messagesapi.Content {
	id := b.ids.get(r.ToolCallID)
	text := func(s string) []messagesapi.Content {
		if blank(s) {
			return nil
		}
		return []messagesapi.Content{messagesapi.Text(s)}
	}
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](r.Output); ok {
		return messagesapi.ToolResult(id, false, text(o.Text)...)
	}
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](r.Output); ok {
		return messagesapi.ToolResult(id, true, text(resultErrorText(o))...)
	}
	if o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](r.Output); ok {
		content := text(o.Text)
		if mediaType, data, ok := resultImage(r.Output); ok {
			content = append(content, b.image(mediaType, data))
		} else {
			content = append(content, messagesapi.Text(omitted("image", o.MediaType)))
		}
		return messagesapi.ToolResult(id, false, content...)
	}
	b.warn(unsentResult(r.Output))
	return messagesapi.ToolResult(id, false)
}

// assistant is an assistant message's blocks, in the order of its parts —
// the order the response streamed them (P1) — so a thinking block is
// replayed where it was, ahead of what it led to.
func (b *promptBuilder) assistant(msg fantasy.Message) []turnBlock {
	var out []turnBlock
	for _, part := range msg.Content {
		if p, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
			if c, ok := replayThinking(p); ok {
				out = append(out, turnBlock{c: c})
			}
		} else if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			if !blank(p.Text) {
				out = append(out, turnBlock{c: messagesapi.Text(p.Text)})
			}
		} else if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && !p.ProviderExecuted {
			out = append(out, turnBlock{c: messagesapi.ToolUse(b.ids.get(p.ToolCallID), b.names.get(p.ToolName), toolInput(p.Input))})
		}
	}
	return out
}

// replayThinking is a reasoning part's block, when it can be sent back: a
// thinking block this driver wrote, its text and its signature as they
// streamed, or a redacted one's data (anthropicThinking). Anything else is
// dropped (§3.3 item 3): another provider's reasoning, or one rebuilt from an
// interrupted or rewritten step, which keeps its text and no signature (LF-9:
// a step without its thinking is history the API accepts).
func replayThinking(p fantasy.ReasoningPart) (messagesapi.Content, bool) {
	md, ok := p.ProviderOptions[thinkingKey].(*anthropicThinking)
	switch {
	case !ok || md == nil:
		return messagesapi.Content{}, false
	case md.Signature != "":
		return messagesapi.Thinking(p.Text, md.Signature), true
	case md.RedactedData != "":
		return messagesapi.RedactedThinking(md.RedactedData), true
	}
	return messagesapi.Content{}, false
}

// toolInput is a call's input as the tool_use block's: the JSON object it
// is, compacted — its keys in the order the model wrote them — or {} for an
// input that is not one (another provider's history may hold anything).
// Compacting is safe for the thinking binding: the API canonicalizes tool
// input for the thinking binding — verified live, plan 044 LF-4/LF-11 probes —
// and the same input always compacts to the same bytes, so the cache hits.
func toolInput(in string) json.RawMessage {
	raw := bytes.TrimSpace([]byte(in))
	var buf bytes.Buffer
	if len(raw) == 0 || raw[0] != '{' || json.Compact(&buf, raw) != nil {
		return json.RawMessage("{}")
	}
	return buf.Bytes()
}

// The API's rule for a tool_use's id and a tool's name (LF-6): an id or a
// name another provider wrote — functions.read:0 — is a 400 forever.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// idMap maps the ids (or the names) of one request to ones the API accepts
// (P3): in the order the request meets them, an id that is valid and not
// yet taken is itself, and any other is sanitizeID's, with _2, _3, …
// appended while that is taken — by a valid id or an earlier mapping. The
// same id always maps the same way, so a call and its result agree, and an
// id a provider reused across steps (LF-6) stays one id; and the mapping of
// an earlier id never depends on a later one, so a history replayed with
// more after it maps as it did.
type idMap struct {
	of    map[string]string
	taken map[string]bool
}

func newIDMap() *idMap { return &idMap{of: map[string]string{}, taken: map[string]bool{}} }

func (m *idMap) get(id string) string {
	if v, ok := m.of[id]; ok {
		return v
	}
	v := id
	if !validID.MatchString(id) || m.taken[id] {
		base := sanitizeID(id)
		v = base
		for n := 2; m.taken[v]; n++ {
			v = base + "_" + strconv.Itoa(n)
		}
	}
	m.of[id], m.taken[v] = v, true
	return v
}

// sanitizeID is id made valid, and distinct from every other id it could be
// made from: each rune the API does not take replaced by _, cut to 100
// bytes, then _ and the first 12 hex digits of the SHA-256 of id as it was.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if idRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 100 {
		s = s[:100]
	}
	sum := sha256.Sum256([]byte(id))
	return s + "_" + hex.EncodeToString(sum[:])[:12]
}

// idRune reports whether r is one of the characters validID takes.
func idRune(r rune) bool {
	return r == '_' || r == '-' || '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
}

// Stream sends one step. The request is built at once — a prompt the API
// could only refuse is refused here, before anything is sent — and sent when
// the stream is first read; the response's body is closed when the read
// ends, however it ends.
func (m *messagesModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	req, warnings, err := m.request(call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if len(warnings) > 0 && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeWarnings, Warnings: warnings}) {
			return
		}
		s, err := m.client.Stream(ctx, req)
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: messagesError(err)})
			return
		}
		defer func() { _ = s.Close() }()
		st := messagesParts{stream: s, ids: map[int]string{}}
		for s.Next() {
			if !st.parts(s.Event(), yield) {
				return
			}
		}
		err = s.Err()
		if err == nil {
			return
		}
		// A response that failed after message_start spent its usage all
		// the same: a Finish part carries it first, for OnStreamFinish (the
		// summarizer counts it, compact.go observeUsage).
		if u := s.Usage(); u.Present != 0 {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonError,
				Usage: messagesUsage(u), ProviderMetadata: st.finishMetadata("", messagesapi.StopDetails{})}) {
				return
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: messagesError(err)})
	}, nil
}

// messagesParts maps one response's events to Fantasy's stream parts.
type messagesParts struct {
	stream *messagesapi.Stream
	ids    map[int]string // each block's part id, by index
}

// parts yields ev as Fantasy stream parts, false when the consumer stopped.
// A text or thinking block's part id is its index's; a tool use's is its
// id, which is what Fantasy and the transcript know the call by.
//
// A tool call is reported whole at its block's stop (P1): the stream stops
// its blocks in wire order, so every part Fantasy keeps lands in the step in
// the order the model wrote them, and a call's reasoning, before it, is
// replayed before it. Fantasy holds the calls until the finish is known and
// runs none on a finish other than tool-calls (agent.go:1599-1607,
// 1707-1733): a call cut short by max_tokens, or in a refused response, is
// never run.
func (st *messagesParts) parts(ev messagesapi.Event, yield func(fantasy.StreamPart) bool) bool {
	id := st.ids[ev.Index]
	switch ev.Kind {
	case messagesapi.BlockStart:
		switch ev.Block.Type {
		case messagesapi.TypeText:
			st.ids[ev.Index] = blockID(ev.Index)
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: st.ids[ev.Index]})
		case messagesapi.TypeThinking, messagesapi.TypeRedactedThinking:
			st.ids[ev.Index] = blockID(ev.Index)
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: st.ids[ev.Index]})
		case messagesapi.TypeToolUse:
			st.ids[ev.Index] = ev.Block.ID
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: ev.Block.ID, ToolCallName: ev.Block.Name})
		}
	case messagesapi.TextDelta:
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Delta})
	case messagesapi.ThinkingDelta:
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: id, Delta: ev.Delta})
	case messagesapi.InputDelta:
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: ev.Delta})
	case messagesapi.BlockStop:
		b := ev.Block
		switch b.Type {
		case messagesapi.TypeText:
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: id})
		case messagesapi.TypeThinking:
			end := fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id}
			if b.Signature != "" {
				end.ProviderMetadata = fantasy.ProviderMetadata{thinkingKey: &anthropicThinking{Signature: b.Signature}}
			}
			return yield(end)
		case messagesapi.TypeRedactedThinking:
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id,
				ProviderMetadata: fantasy.ProviderMetadata{thinkingKey: &anthropicThinking{RedactedData: b.Data}}})
		case messagesapi.TypeToolUse:
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: b.ID}) &&
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: b.ID, ToolCallName: b.Name, ToolCallInput: b.Input})
		}
	case messagesapi.Completed:
		usage, md := messagesUsage(ev.Usage), st.finishMetadata(ev.StopReason, ev.StopDetails)
		if ev.StopReason == "model_context_window_exceeded" {
			// The context window filled while the model wrote (P16): not a
			// length finish, which would end the turn, but a failure the
			// harness's overflow path answers by compacting and sending again
			// (errors.go, turn.go). The usage is reported first.
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonError, Usage: usage, ProviderMetadata: md}) &&
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: contextWindowFull(st.stream.RequestID())})
		}
		return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: finishReason(ev.StopReason), Usage: usage, ProviderMetadata: md})
	}
	return true
}

func blockID(index int) string { return "block-" + strconv.Itoa(index) }

// finishMetadata is a finish's provider metadata, its AnthropicFinish, for
// a response that stopped for stop with details ("" for one that failed): the
// request's id, the thinking blocks the API dropped from it, and a refusal's
// category, kept only as the identifier it should be.
func (st *messagesParts) finishMetadata(stop string, details messagesapi.StopDetails) fantasy.ProviderMetadata {
	f := &AnthropicFinish{RequestID: st.stream.RequestID()}
	for _, t := range st.stream.InputTransformations() {
		if t.Type == "thinking_dropped" {
			f.ThinkingDropped++
		}
	}
	if stop == "refusal" && refusalCategory.MatchString(details.Category) {
		f.RefusalCategory = details.Category
	}
	return fantasy.ProviderMetadata{anthropicFinishKey: f}
}

// finishReason is a stop reason as Fantasy's finish reason (§3.3): a
// pause_turn, which only a server tool's loop sends, ends the step as a stop.
func finishReason(stop string) fantasy.FinishReason {
	switch stop {
	case "end_turn", "stop_sequence", "pause_turn":
		return fantasy.FinishReasonStop
	case "tool_use":
		return fantasy.FinishReasonToolCalls
	case "max_tokens":
		return fantasy.FinishReasonLength
	case "refusal":
		return fantasy.FinishReasonContentFilter
	}
	return fantasy.FinishReasonUnknown
}

// messagesUsage is a response's usage as Fantasy counts it (§3.3): the API's
// input_tokens already leaves the cache out, so Input, CacheRead and
// CacheCreation are each its own count — the harness's context sum adds the
// three with Output and counts each token once (autocompact.go), and spend
// prices cache writes apart (spend.go) — and Output includes the thinking,
// which Reasoning reports.
func messagesUsage(u messagesapi.Usage) fantasy.Usage {
	return fantasy.Usage{
		InputTokens:         u.InputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
		OutputTokens:        u.OutputTokens,
		ReasoningTokens:     u.ThinkingTokens,
		TotalTokens:         u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens,
	}
}

// contextWindowFull is the failure a model_context_window_exceeded stop
// ends its step with (P16): context too large, and not retried as it is.
func contextWindowFull(requestID string) *fantasy.ProviderError {
	return &fantasy.ProviderError{
		Title:              "context window exceeded",
		Message:            withRequestID("the model's context window filled before it finished (model_context_window_exceeded)", requestID),
		ContextTooLargeErr: true,
	}
}

// messagesError is err, from the driver's core, as the error Fantasy and the
// harness classify (P2):
//
//   - The context's own error goes as it is: never retried.
//   - A failure the server marked x-should-retry: false is a *FinalError,
//     which nothing retries — Fantasy retries every 408, 409, 429 and 5xx
//     whatever a ProviderError's TransientError says — with its context flag.
//   - Any other failure the server reported is a *fantasy.ProviderError
//     (messagesProviderError).
//   - A stream that ended before it said how, or broke the protocol, is
//     Fantasy's retryable incomplete-stream error, a broken protocol under a title
//     of its own, which the wrapper's rule 3 makes final after output; a
//     transport's failure — a
//     network error, an HTTP/2 reset or GOAWAY, which the client reports
//     scrubbed and without its cause (messagesapi.IsTransport) — a retryable
//     transport error. After output, the wrapper's rule 3 decides what is
//     tried again.
//   - Anything else — a request the client could not build — goes as it is,
//     in Fantasy's transport wrapper when it is one.
func messagesError(err error) error {
	var me *messagesapi.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &me):
		if me.ShouldRetry != nil && !*me.ShouldRetry {
			return &FinalError{
				StatusCode:      me.StatusCode,
				Type:            me.Type,
				Message:         messagesMessage(me),
				contextTooLarge: me.ContextTooLarge(),
			}
		}
		return messagesProviderError(me)
	case messagesapi.IsViolation(err):
		pe := fantasy.NewIncompleteStreamError()
		pe.Title, pe.Message = protocolTitle, err.Error()
		return pe
	case errors.Is(err, messagesapi.ErrIncomplete):
		return fantasy.NewIncompleteStreamError()
	case messagesapi.IsTransport(err):
		// Fantasy retries a bare net.Error, but not one inside a
		// ProviderError whose cause it cannot read as an HTTP/2 failure: the
		// verdict is the client's, so it is said outright.
		pe := fantasy.NewTransportError(err)
		pe.TransientError = true
		return pe
	}
	return fantasy.WrapTransportError(err)
}

// permanentStatuses are the statuses the same request can never get past,
// whatever a header says (C2 review 1): a request refused (400), a key
// refused (401), access refused (403), a model the account cannot use (404),
// a request too large (413), which the harness answers by compacting.
var permanentStatuses = map[int]bool{
	http.StatusBadRequest:            true,
	http.StatusUnauthorized:          true,
	http.StatusForbidden:             true,
	http.StatusNotFound:              true,
	http.StatusRequestEntityTooLarge: true,
}

// messagesProviderError is a failure the server reported, as the error
// Fantasy retries or not: its status (an error event's, the one its type
// stands for), its auth and context flags, retryable when the client calls it
// transient and its status is not a permanent one — an x-should-retry: true
// on a 403 or a 404 is not followed — and a body holding the standard error object, from which the
// scrubber reads the type back (ErrorNames). Of its headers only the wait it
// asked for is kept, as retry-after-ms (P2): never x-should-retry, which
// Fantasy would honour over the client's verdict.
func messagesProviderError(e *messagesapi.Error) *fantasy.ProviderError {
	pe := &fantasy.ProviderError{
		Title:              messagesTitle(e),
		Message:            messagesMessage(e),
		StatusCode:         e.StatusCode,
		ResponseBody:       errorBody("", e.Type, "", e.Message),
		AuthError:          e.AuthError(),
		ContextTooLargeErr: e.ContextTooLarge(),
		TransientError:     e.Transient() && !permanentStatuses[e.StatusCode],
	}
	if e.RetryAfter > 0 {
		ms := float64(e.RetryAfter) / float64(time.Millisecond)
		pe.ResponseHeaders = map[string]string{"retry-after-ms": strconv.FormatFloat(ms, 'f', -1, 64)}
	}
	return pe
}

func messagesTitle(e *messagesapi.Error) string {
	switch e.Type {
	case "overloaded_error":
		return "provider overloaded"
	case "rate_limit_error":
		return "rate limit exceeded"
	}
	if t := fantasy.ErrorTitleForStatusCode(e.StatusCode); t != "" {
		return t
	}
	if e.InStream {
		return "provider stream error"
	}
	return "provider request failed"
}

// messagesMessage is the failure's display text: the server's message, or
// its type when it sent none, with the request's id to quote to Anthropic.
func messagesMessage(e *messagesapi.Error) string {
	msg := e.Message
	switch {
	case msg != "":
	case e.Type != "":
		msg = e.Type
	default:
		msg = "the request failed"
	}
	return withRequestID(msg, e.RequestID)
}

func withRequestID(msg, requestID string) string {
	if requestID == "" {
		return msg
	}
	return msg + " (request " + requestID + ")"
}

// Generate is one step, collected from its stream: the driver only streams.
func (m *messagesModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return collect(m.Stream(ctx, call))
}

// errNoMessagesObjects is the answer to the object calls, which nothing in
// craze makes.
var errNoMessagesObjects = errors.New("llm: Anthropic's driver does not generate objects")

func (m *messagesModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errNoMessagesObjects
}

func (m *messagesModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errNoMessagesObjects
}
