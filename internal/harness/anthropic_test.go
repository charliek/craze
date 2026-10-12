package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool"
)

// Anthropic's driver through the harness (plan 044 §3.4, A3): the real llm
// factory and the Messages adapter behind a fake Messages API, wherever the
// history a session sends is what is under test — after a compaction, after
// a resume, after an interrupted step, across a switch between two Claude
// models, with parallel results, an image and a steer in one step, and the
// summarizer's own request — and the refusal's category and Anthropic's
// 403/404 on their way out. Every session here is built by the default
// NewModel, its Anthropic base the fake's (Options.AnthropicAPIBase, P21), so
// every request a session makes, its sub-agents' and its summarizer's
// included, reaches the fake or fails the test. The key is a canary.

// anKey is the only Anthropic key any test here holds.
const anKey = "sk-ant-canary-not-a-secret-0001"

// The two Claude models of the test table: two aliases on the anthropic
// provider with different wire ids, both thinking adaptively (the table
// resolves an unknown Anthropic model's thinking to adaptive).
const (
	anOpus   = "anthropic/claude-opus-5-5"
	anSonnet = "anthropic/claude-sonnet-5-5"
)

const anthropicProviderTOML = `
[providers.anthropic]
driver = "anthropic"
env_keys = ["ANTHROPIC_API_KEY"]
`

const anthropicModelsTOML = `
[models."anthropic/claude-opus-5-5"]
provider = "anthropic"
wire_model = "claude-opus-5-5"
context_window = 200000
max_output_tokens = 8000
efforts = ["low", "medium", "high"]
default_effort = "medium"
vision = true

[models."anthropic/claude-sonnet-5-5"]
provider = "anthropic"
wire_model = "claude-sonnet-5-5"
context_window = 200000
max_output_tokens = 8000
efforts = ["low", "medium", "high"]
default_effort = "medium"
vision = true
`

// onAnthropic is the fixture's table with the anthropic provider and its two
// models added, and Open's options for a session on Opus built by the
// default NewModel against api, the key in its environment.
func onAnthropic(t *testing.T, api *anAPI) (*fixture, Options) {
	t.Helper()
	f := newFixture(t, "http://127.0.0.1:1/v1")
	appendTo := func(name, body string) {
		file, err := os.OpenFile(filepath.Join(f.home, name), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(body); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	appendTo(modeltable.ProvidersFile, anthropicProviderTOML)
	appendTo(modeltable.ModelsFile, anthropicModelsTOML)
	f.table = f.load()
	opts := f.options()
	opts.Table, opts.Model = f.table, anOpus
	opts.NewModel = nil
	opts.AnthropicAPIBase = api.base
	opts.Getenv = func(k string) string {
		if k == "ANTHROPIC_API_KEY" {
			return anKey
		}
		return testEnv[k]
	}
	return f, opts
}

// anAPI is a fake Messages API: each request is answered by the next queued
// reply, else refused, and recorded as it arrived.
//
// It also stands in for the rule LF-4 found the API enforcing, so a test sees
// what the API would make of the history the harness sends: each signature a
// reply streams is bound to the messages before its block (a digest of them,
// order-insensitive in each object's keys), and a later request that sends
// the block after other messages is refused with LF-4's 400 — unless it asks
// for block_binding's drop_block, when the block is dropped and the reply's
// message_start reports it as input_transformations (LF-4, LF-11). It binds
// only the messages, the part LF-4 saw edited; the API itself may bind more.
type anAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	replies []anReply
	reqs    []anRequest
	bound   map[string]string // a signature → the digest of the messages it was streamed after
}

// anRequest is one request as the fake read it.
type anRequest struct {
	header http.Header
	body   []byte
	msgs   []anMsg
	// stale are the paths of the signed thinking blocks sent after messages
	// other than those they were streamed after; dropped is stale when the
	// request asked for drop_block.
	stale, dropped []string
}

// anMsg is one message of a request body, its blocks raw: the bytes the
// body holds for each.
type anMsg struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// anReply answers one request.
type anReply func(w http.ResponseWriter, r *http.Request, req anRequest)

func newAnAPI(t *testing.T, replies ...anReply) *anAPI {
	t.Helper()
	a := &anAPI{t: t, replies: replies, bound: map[string]string{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request body: %v", err)
		}
		var parsed struct {
			Messages []anMsg `json:"messages"`
			Thinking *struct {
				BlockBinding *struct {
					PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
				} `json:"block_binding"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("a request body is not JSON: %v\n%s", err, body)
		}
		drop := parsed.Thinking != nil && parsed.Thinking.BlockBinding != nil &&
			parsed.Thinking.BlockBinding.PrefixMismatchBehavior == "drop_block"
		req := anRequest{header: r.Header.Clone(), body: body, msgs: parsed.Messages}
		a.mu.Lock()
		req.stale = a.staleLocked(req.msgs)
		if drop {
			req.dropped = req.stale
		}
		a.reqs = append(a.reqs, req)
		var next anReply
		if len(req.stale) == 0 || drop {
			if len(a.replies) > 0 {
				next, a.replies = a.replies[0], a.replies[1:]
			}
		}
		a.mu.Unlock()
		switch {
		case len(req.stale) > 0 && !drop:
			anFail(400, "invalid_request_error", req.stale[0]+": Invalid signature in thinking block. The block is bound to a different conversation.")(w, r, req)
		case next == nil:
			anFail(418, "invalid_request_error", "the test queued no reply for this request")(w, r, req)
		default:
			next(w, r, req)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// base is the fake's origin, as Options.AnthropicAPIBase answers it.
func (a *anAPI) base() (string, error) { return a.srv.URL, nil }

func (a *anAPI) queue(replies ...anReply) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replies = append(a.replies, replies...)
}

func (a *anAPI) requests() []anRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.reqs)
}

// staleLocked is the binding rule's verdict on msgs: the path of every
// signed thinking block sent after messages other than those it was
// streamed after. a.mu is held.
func (a *anAPI) staleLocked(msgs []anMsg) []string {
	var out []string
	for j, m := range msgs {
		for k, raw := range m.Content {
			var b struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			}
			if json.Unmarshal(raw, &b) != nil || b.Type != messagesapi.TypeThinking {
				continue
			}
			if a.bound[b.Signature] != anDigest(a.t, msgs[:j], m.Role, m.Content[:k]) {
				out = append(out, fmt.Sprintf("messages.%d.content.%d", j, k))
			}
		}
	}
	return out
}

// bind binds signature to the messages before its block: msgs, then the
// reply's own blocks before it.
func (a *anAPI) bind(signature string, msgs []anMsg, before []json.RawMessage) {
	d := anDigest(a.t, msgs, "assistant", before)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bound[signature] = d
}

// anDigest is the digest of msgs followed by a message of role holding head,
// every block decoded and encoded again, so the digest is the same whatever
// order a block's keys were written in.
func anDigest(t *testing.T, msgs []anMsg, role string, head []json.RawMessage) string {
	t.Helper()
	var all []any
	for _, m := range append(slices.Clone(msgs), anMsg{Role: role, Content: head}) {
		var blocks []any
		for _, raw := range m.Content {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Errorf("a block is not JSON: %v", err)
			}
			blocks = append(blocks, v)
		}
		all = append(all, map[string]any{"role": m.Role, "content": blocks})
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// anBlock is one content block a reply streams.
type anBlock struct {
	kind            string // a messagesapi type: text, thinking or tool_use
	text, signature string
	id, name, input string
}

func anThought(text, signature string) anBlock {
	return anBlock{kind: messagesapi.TypeThinking, text: text, signature: signature}
}
func anSaid(text string) anBlock { return anBlock{kind: messagesapi.TypeText, text: text} }
func anCall(id, name, input string) anBlock {
	return anBlock{kind: messagesapi.TypeToolUse, id: id, name: name, input: input}
}

// replayed is b as a request sends it back: what the adapter writes for it.
func (b anBlock) replayed() json.RawMessage {
	switch b.kind {
	case messagesapi.TypeThinking:
		return json.RawMessage(an(map[string]any{"type": b.kind, "thinking": b.text, "signature": b.signature}))
	case messagesapi.TypeToolUse:
		return json.RawMessage(an(map[string]any{"type": b.kind, "id": b.id, "name": b.name, "input": json.RawMessage(b.input)}))
	}
	return json.RawMessage(an(map[string]any{"type": b.kind, "text": b.text}))
}

// an is v as JSON.
func an(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// anEvent writes one server-sent event of the Messages stream.
func anEvent(w io.Writer, ev map[string]any) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], an(ev))
}

// anStart writes message_start for req: the usage, and the blocks the binding
// rule dropped as input_transformations.
func anStart(w http.ResponseWriter, req anRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Request-Id", "req_test_0001")
	transforms := []any{}
	for _, p := range req.dropped {
		transforms = append(transforms, map[string]any{"type": "thinking_dropped", "path": p, "reason": "prefix_binding_mismatch"})
	}
	anEvent(w, map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_test_1", "type": "message", "role": "assistant", "model": "claude-test", "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": 10, "cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 0, "output_tokens": 1},
		"input_transformations": transforms,
	}})
}

// anBlockEvents writes block b at index i, whole.
func anBlockEvents(w io.Writer, i int, b anBlock) {
	start := func(block map[string]any) {
		anEvent(w, map[string]any{"type": "content_block_start", "index": i, "content_block": block})
	}
	delta := func(d map[string]any) {
		anEvent(w, map[string]any{"type": "content_block_delta", "index": i, "delta": d})
	}
	switch b.kind {
	case messagesapi.TypeThinking:
		start(map[string]any{"type": b.kind, "thinking": "", "signature": ""})
		delta(map[string]any{"type": "thinking_delta", "thinking": b.text})
		if b.signature != "" {
			delta(map[string]any{"type": "signature_delta", "signature": b.signature})
		}
	case messagesapi.TypeToolUse:
		start(map[string]any{"type": b.kind, "id": b.id, "name": b.name, "input": map[string]any{}})
		delta(map[string]any{"type": "input_json_delta", "partial_json": ""})
		delta(map[string]any{"type": "input_json_delta", "partial_json": b.input})
	default:
		start(map[string]any{"type": b.kind, "text": ""})
		delta(map[string]any{"type": "text_delta", "text": b.text})
	}
	anEvent(w, map[string]any{"type": "content_block_stop", "index": i})
}

// anEnd writes message_delta, stopping for stop with details (nil for none),
// and message_stop.
func anEnd(w io.Writer, stop string, details map[string]any) {
	anEvent(w, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil, "stop_details": details},
		"usage": map[string]any{"output_tokens": 5}})
	anEvent(w, map[string]any{"type": "message_stop"})
}

// reply is a reply streaming blocks and stopping for stop, with details as
// its stop_details: each signature it streams bound to the messages before
// it.
func (a *anAPI) reply(stop string, details map[string]any, blocks ...anBlock) anReply {
	return func(w http.ResponseWriter, _ *http.Request, req anRequest) {
		anStart(w, req)
		var before []json.RawMessage
		for i, b := range blocks {
			if b.kind == messagesapi.TypeThinking && b.signature != "" {
				a.bind(b.signature, req.msgs, before)
			}
			anBlockEvents(w, i, b)
			before = append(before, b.replayed())
		}
		anEnd(w, stop, details)
	}
}

// answer is a reply that ends the turn with blocks.
func (a *anAPI) answer(blocks ...anBlock) anReply { return a.reply("end_turn", nil, blocks...) }

// calls is a reply that calls tools with blocks.
func (a *anAPI) calls(blocks ...anBlock) anReply { return a.reply("tool_use", nil, blocks...) }

// anFail answers status with Anthropic's error body.
func anFail(status int, typ, message string) anReply {
	return func(w http.ResponseWriter, _ *http.Request, _ anRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_test_0002")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, an(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}}))
	}
}

// anSummary is a summarizer's reply: a summary long enough to be kept.
func (a *anAPI) anSummary(what string) anReply {
	return a.answer(anSaid(longSummary("1. Request and intent\n" + what)))
}

// anToolID is the pattern the API holds a tool_use id to (LF-6).
var anToolID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// anImageTypes are the image types the API takes.
var anImageTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// anProblems is what the Messages API would refuse in req, the rules a
// history can break: a body with a model and max_tokens; block_binding only
// with its beta header; messages that start and end with the user's and
// alternate; no empty message and no empty text block (LF-6); thinking only
// in the assistant's messages and only signed; tool_use ids the API's pattern
// (LF-6); every tool_use answered by a tool_result in the next message,
// whose results come before anything else in it, and no result without its
// call in the message before; images of the four types; no other block type.
// nil when it would take the request.
func anProblems(req anRequest) []string {
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Thinking  *struct {
			BlockBinding *json.RawMessage `json:"block_binding"`
		} `json:"thinking"`
		Messages []anMsg `json:"messages"`
	}
	if err := json.Unmarshal(req.body, &body); err != nil {
		return []string{"the body is not JSON: " + err.Error()}
	}
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if body.Model == "" || body.MaxTokens <= 0 {
		bad("no model or no max_tokens")
	}
	if body.Thinking != nil && body.Thinking.BlockBinding != nil && !strings.Contains(req.header.Get("Anthropic-Beta"), messagesapi.BindingBeta) {
		bad("block_binding without the %s beta", messagesapi.BindingBeta)
	}
	msgs := body.Messages
	if len(msgs) == 0 {
		return append(out, "no messages")
	}
	if msgs[0].Role != "user" || msgs[len(msgs)-1].Role != "user" {
		bad("the messages run %s…%s, want user…user", msgs[0].Role, msgs[len(msgs)-1].Role)
	}
	var calls []string // the tool_use ids of the message before
	for i, m := range msgs {
		if i > 0 && msgs[i-1].Role == m.Role {
			bad("messages %d and %d are both %s", i-1, i, m.Role)
		}
		if len(m.Content) == 0 {
			bad("message %d is empty", i)
		}
		var uses, results []string
		other := false
		for k, raw := range m.Content {
			var b struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Signature string `json:"signature"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
				Source    struct {
					MediaType string `json:"media_type"`
				} `json:"source"`
			}
			if err := json.Unmarshal(raw, &b); err != nil {
				bad("messages.%d.content.%d is not JSON", i, k)
				continue
			}
			at := fmt.Sprintf("messages.%d.content.%d", i, k)
			switch {
			case b.Type == messagesapi.TypeText && strings.TrimSpace(b.Text) == "":
				bad("%s: an empty text block", at)
			case b.Type == messagesapi.TypeThinking && (m.Role != "assistant" || b.Signature == ""):
				bad("%s: thinking that is unsigned or not the assistant's", at)
			case b.Type == messagesapi.TypeRedactedThinking && m.Role != "assistant":
				bad("%s: redacted thinking that is not the assistant's", at)
			case b.Type == messagesapi.TypeToolUse:
				if m.Role != "assistant" || !anToolID.MatchString(b.ID) {
					bad("%s: a tool_use that is not the assistant's or whose id is %q", at, b.ID)
				}
				uses = append(uses, b.ID)
			case b.Type == messagesapi.TypeToolResult:
				if m.Role != "user" || other {
					bad("%s: a tool_result that is not the user's or not ahead of the message's other blocks", at)
				}
				results = append(results, b.ToolUseID)
			case b.Type == messagesapi.TypeImage && !slices.Contains(anImageTypes, b.Source.MediaType):
				bad("%s: an image of type %q", at, b.Source.MediaType)
			case !slices.Contains([]string{messagesapi.TypeText, messagesapi.TypeThinking, messagesapi.TypeRedactedThinking,
				messagesapi.TypeImage}, b.Type):
				bad("%s: a block of type %q", at, b.Type)
			}
			if b.Type != messagesapi.TypeToolResult {
				other = true
			}
		}
		for _, id := range calls {
			if !slices.Contains(results, id) {
				bad("message %d does not answer tool_use %s", i, id)
			}
		}
		for _, id := range results {
			if !slices.Contains(calls, id) {
				bad("message %d answers %s, which the message before did not call", i, id)
			}
		}
		calls = uses
	}
	if len(calls) > 0 {
		bad("the last message's tool_use is not answered")
	}
	return out
}

// valid fails the test for every request api saw that the API would refuse.
func valid(t *testing.T, api *anAPI) {
	t.Helper()
	for i, req := range api.requests() {
		if p := anProblems(req); p != nil {
			t.Errorf("request %d would be refused: %s\n%s", i+1, strings.Join(p, "; "), req.body)
		}
	}
}

// blockAt is the raw block at messages.j.content.k of req, "" when there is
// none.
func blockAt(req anRequest, j, k int) string {
	if j < 0 || j >= len(req.msgs) || k >= len(req.msgs[j].Content) {
		return ""
	}
	return string(req.msgs[j].Content[k])
}

// shape is each message of req as role:type,type,…
func shape(t *testing.T, req anRequest) []string {
	t.Helper()
	var out []string
	for _, m := range req.msgs {
		var types []string
		for _, raw := range m.Content {
			var b struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatal(err)
			}
			types = append(types, b.Type)
		}
		out = append(out, m.Role+":"+strings.Join(types, ","))
	}
	return out
}

// binding is req's thinking.block_binding.prefix_mismatch_behavior, "" when it
// asks for none, and whether it carries the controls beta.
func binding(t *testing.T, req anRequest) (string, bool) {
	t.Helper()
	var body struct {
		Thinking *struct {
			BlockBinding *struct {
				PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
			} `json:"block_binding"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	beta := strings.Contains(req.header.Get("Anthropic-Beta"), messagesapi.BindingBeta)
	if body.Thinking == nil || body.Thinking.BlockBinding == nil {
		return "", beta
	}
	return body.Thinking.BlockBinding.PrefixMismatchBehavior, beta
}

// TestAnProblemsFindsWhatTheAPIRefuses is the checker's control: each body
// breaks one rule and is found out by that rule alone, and a valid one has
// no problem.
func TestAnProblemsFindsWhatTheAPIRefuses(t *testing.T) {
	beta := http.Header{"Anthropic-Beta": []string{messagesapi.BindingBeta}}
	user := func(blocks ...string) string { return `{"role":"user","content":[` + strings.Join(blocks, ",") + `]}` }
	assistant := func(blocks ...string) string {
		return `{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}`
	}
	text := func(s string) string { return `{"type":"text","text":"` + s + `"}` }
	thinking := func(sig string) string { return `{"type":"thinking","thinking":"t","signature":"` + sig + `"}` }
	use := func(id string) string { return `{"type":"tool_use","id":"` + id + `","name":"read","input":{}}` }
	result := func(id string) string { return `{"type":"tool_result","tool_use_id":"` + id + `"}` }
	body := func(binding bool, msgs ...string) []byte {
		thinkingField := ""
		if binding {
			thinkingField = `"thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}},`
		}
		return []byte(`{"model":"m","max_tokens":10,` + thinkingField + `"messages":[` + strings.Join(msgs, ",") + `]}`)
	}
	for _, tc := range []struct {
		name   string
		header http.Header
		body   []byte
		want   string // a word of the one problem, "" for none
	}{
		{"valid", beta, body(true, user(text("a")), assistant(thinking("s"), use("t_1")), user(result("t_1"), text("b"))), ""},
		{"no beta", http.Header{}, body(true, user(text("a"))), "beta"},
		{"an empty text block", beta, body(false, user(text(" "))), "empty text"},
		{"unsigned thinking", beta, body(false, user(text("a")), assistant(thinking(""), text("b")), user(text("c"))), "unsigned"},
		{"an unanswered call", beta, body(false, user(text("a")), assistant(use("t_1")), user(text("b"))), "does not answer"},
		{"a result after text", beta, body(false, user(text("a")), assistant(use("t_1")), user(text("b"), result("t_1"))), "not ahead"},
		{"an orphan result", beta, body(false, user(text("a")), assistant(text("b")), user(result("t_9"))), "did not call"},
		{"two user messages", beta, body(false, user(text("a")), user(text("b"))), "both user"},
		{"assistant first", beta, body(false, assistant(text("a")), user(text("b"))), "want user"},
		{"a bad tool id", beta, body(false, user(text("a")), assistant(use("t.1")), user(result("t.1"))), "whose id"},
	} {
		got := anProblems(anRequest{header: tc.header, body: tc.body})
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: problems %q, want none", tc.name, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
			t.Errorf("%s: problems %q, want one about %q", tc.name, got, tc.want)
		}
	}
}

// TestAnthropicCompactionKeepsTheTailsSignedThinking (A3, LF-11): after a
// compaction that keeps a tail, the next requests carry block_binding's
// drop_block and its beta, and send the tail's signed thinking back byte for
// byte — the API, not craze, decides what to drop. Through the binding rule's
// stand-in: the tail's block, streamed before the compaction edited what
// precedes it, is dropped from both requests of the turn after, while the
// block that turn's first step streamed is sent back in its second and kept.
// The summarizer's own request is a valid one, carrying every block, and it
// reached the fake (the session's base). The control: without drop_block the
// rule refuses both requests after the compaction, and refuses none before.
func TestAnthropicCompactionKeepsTheTailsSignedThinking(t *testing.T) {
	api := newAnAPI(t)
	f, opts := onAnthropic(t, api)
	s := f.open(opts)
	f.put("a.txt", "alpha\n")
	f.put("b.txt", "beta\n")
	api.queue(
		api.calls(anThought("look first", "sig-a1"), anCall("toolu_01", "read", `{"filePath":"a.txt"}`)),
		api.answer(anThought("read it", "sig-a2"), anSaid("It says alpha.")),
		api.answer(anThought("short", "sig-b1"), anSaid("Nothing more.")),
	)
	run(t, s, "what is in a.txt?")
	run(t, s, "and now?")

	// The tail: the last step alone (testTailBudget's way).
	steps := s.store.Steps(s.cur.id())
	size := 0
	for _, m := range steps[len(steps)-1].Messages {
		size += int(messageTokens(m))
	}
	f.table.Compaction.TailTokensSet = &size
	api.queue(api.anSummary("Read a.txt."))
	if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if c := compactionEntries(t, s); len(c) != 1 || c[0].Compaction.FirstKeptID == "" {
		t.Fatalf("the compaction kept no tail: %+v", c)
	}

	api.queue(
		api.calls(anThought("after", "sig-c1"), anCall("toolu_02", "read", `{"filePath":"b.txt"}`)),
		api.answer(anSaid("It says beta.")),
	)
	run(t, s, "and b.txt?")

	valid(t, api)
	reqs := api.requests()
	if len(reqs) != 6 {
		t.Fatalf("%d requests, want 6 (two turns' three steps, the summary, the third turn's two steps)", len(reqs))
	}
	summary, after, last := reqs[3], reqs[4], reqs[5]
	if !strings.Contains(string(summary.body), "1. Request and intent") || len(summary.dropped) != 0 {
		t.Fatalf("request 4 is not the summarizer's, or dropped %v:\n%s", summary.dropped, summary.body)
	}
	for _, sig := range []string{"sig-a1", "sig-a2", "sig-b1"} {
		if !strings.Contains(string(summary.body), sig) {
			t.Errorf("the summarizer's request left out %s", sig)
		}
	}
	tailBlock := `{"type":"thinking","thinking":"short","signature":"sig-b1"}`
	for i, req := range []anRequest{after, last} {
		if b, beta := binding(t, req); b != "drop_block" || !beta {
			t.Errorf("request %d after the compaction: block_binding %q, beta %v; want drop_block and the beta", i+1, b, beta)
		}
		if strings.Contains(string(req.body), "sig-a1") || strings.Contains(string(req.body), "sig-a2") {
			t.Errorf("request %d after the compaction sends a summarized step's thinking:\n%s", i+1, req.body)
		}
		if !strings.Contains(string(req.body), tailBlock) {
			t.Errorf("request %d after the compaction does not send the tail's thinking byte for byte:\n%s", i+1, req.body)
		}
		if len(req.dropped) != 1 || !strings.Contains(blockAt(req, msgIndex(req.dropped[0]), 0), "sig-b1") {
			t.Errorf("request %d after the compaction: dropped %v, want the tail's block alone", i+1, req.dropped)
		}
		if req.stale == nil {
			t.Errorf("control: the binding rule would take request %d after the compaction without drop_block", i+1)
		}
	}
	if !strings.Contains(string(last.body), `{"type":"thinking","thinking":"after","signature":"sig-c1"}`) {
		t.Errorf("the turn's second request does not send its first step's thinking back:\n%s", last.body)
	}
	for i, req := range reqs[:4] {
		if req.stale != nil {
			t.Errorf("control: request %d, before the compaction edited anything, has stale blocks %v", i+1, req.stale)
		}
	}
}

// msgIndex is the message index of a binding rule's path.
func msgIndex(path string) int {
	var j, k int
	if _, err := fmt.Sscanf(path, "messages.%d.content.%d", &j, &k); err != nil {
		return -1
	}
	return j
}

// TestAnthropicResumeReplaysTheSignedThinking (A3): a resumed session's first
// request carries block_binding and its beta and sends the stored turn's
// signed thinking back byte for byte, in place — the signatures survived the
// transcript. The control: resumed onto the other Claude model, whose wire id
// differs, the same history goes without them (the transcript's rule).
func TestAnthropicResumeReplaysTheSignedThinking(t *testing.T) {
	for _, onto := range []string{anOpus, anSonnet} {
		t.Run(onto, func(t *testing.T) {
			api := newAnAPI(t)
			f, opts := onAnthropic(t, api)
			f.put("a.txt", "alpha\n")
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			api.queue(
				api.calls(anThought("look first", "sig-r1"), anCall("toolu_01", "read", `{"filePath":"a.txt"}`)),
				api.answer(anThought("read it", "sig-r2"), anSaid("It says alpha.")),
			)
			run(t, s, "what is in a.txt?")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			stored := api.requests()[1]

			resumedOpts := resumeOptions(opts, s.ID())
			resumedOpts.Model = onto
			r := resumed(t, resumedOpts)
			api.queue(api.answer(anSaid("Still alpha.")))
			run(t, r, "and now?")

			valid(t, api)
			req := api.requests()[2]
			if b, beta := binding(t, req); b != "drop_block" || !beta {
				t.Fatalf("the resumed request: block_binding %q, beta %v; want drop_block and the beta", b, beta)
			}
			first, second := blockAt(stored, 1, 0), `{"type":"thinking","thinking":"read it","signature":"sig-r2"}`
			sent := blockAt(req, 1, 0) == first && blockAt(req, 3, 0) == second
			if want := onto == anOpus; sent != want {
				t.Fatalf("resumed onto %s: the stored thinking sent in place = %v, want %v\n%s", onto, sent, want, req.body)
			}
			if onto == anSonnet && strings.Contains(string(req.body), "sig-r") {
				t.Fatalf("resumed onto the other model, a signature was sent:\n%s", req.body)
			}
		})
	}
}

// TestAnthropicInterruptedStepsReplayAsValidRequests (A3, LF-9): the two
// shapes an interrupted or rewritten step leaves in the transcript — thinking
// stored without its signature beside a tool call, and thinking and text cut
// short by a cancel — go out as requests the API takes: the unsigned thinking
// left out, the call alone in its message with its result paired, the cut
// answer's text alone. The control in each: the transcript does hold the
// unsigned thinking, so its absence is the adapter's doing.
func TestAnthropicInterruptedStepsReplayAsValidRequests(t *testing.T) {
	t.Run("a rewritten tool step", func(t *testing.T) {
		api := newAnAPI(t)
		f, opts := onAnthropic(t, api)
		s := f.open(opts)
		f.put("a.txt", "alpha\n")
		// A text block of whitespace alone is no answer, so the harness stores
		// the step with what streamed (withStreamed): its thinking unsigned.
		api.queue(
			api.calls(anThought("look first", "sig-w1"), anSaid("  "), anCall("toolu_01", "read", `{"filePath":"a.txt"}`)),
			api.answer(anSaid("It says alpha.")),
			api.answer(anSaid("Yes.")),
		)
		run(t, s, "what is in a.txt?")
		run(t, s, "sure?")

		valid(t, api)
		lines := entries(transcript(t, s))
		if !slices.ContainsFunc(lines, func(l string) bool {
			return strings.Contains(l, "(thinking: look first)") && strings.Contains(l, "[call toolu_01")
		}) {
			t.Fatalf("control: no stored step holds the thinking beside the call:\n%s", strings.Join(lines, "\n"))
		}
		reqs := api.requests()
		if !strings.Contains(blockAt(reqs[1], 1, 0), "sig-w1") {
			t.Fatalf("the turn's own second request did not send its thinking back:\n%s", reqs[1].body)
		}
		next := reqs[2]
		equal(t, "the next turn's request", shape(t, next), []string{
			"user:text", "assistant:tool_use", "user:tool_result", "assistant:text", "user:text",
		})
		if strings.Contains(string(next.body), "look first") {
			t.Fatalf("the unsigned thinking was sent:\n%s", next.body)
		}
	})
	t.Run("a cancelled answer", func(t *testing.T) {
		api := newAnAPI(t)
		f, opts := onAnthropic(t, api)
		s := f.open(opts)
		// The step streams thinking with no signature yet and text, then
		// waits: the cancel ends it.
		api.queue(func(w http.ResponseWriter, r *http.Request, req anRequest) {
			anStart(w, req)
			anEvent(w, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			anEvent(w, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "half a thought"}})
			anEvent(w, map[string]any{"type": "content_block_stop", "index": 0})
			anEvent(w, map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}})
			anEvent(w, map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "Half an"}})
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		texted := make(chan struct{})
		var once sync.Once
		out := start(ctx, s, "say something", func(ev Event) {
			if _, ok := ev.(TextDelta); ok {
				once.Do(func() { close(texted) })
			}
		})
		await(t, texted, "the answer's text")
		cancel()
		if got := await(t, out, "the cancelled turn"); got.err != nil || got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
		}
		api.queue(api.answer(anSaid("Whole.")))
		run(t, s, "go on")

		valid(t, api)
		lines := entries(transcript(t, s))
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "interrupted: (thinking: half a thought) Half an") }) {
			t.Fatalf("control: the transcript does not hold the cut step's thinking:\n%s", strings.Join(lines, "\n"))
		}
		next := api.requests()[1]
		equal(t, "the next turn's request", shape(t, next), []string{"user:text", "assistant:text", "user:text"})
		if strings.Contains(string(next.body), "half a thought") {
			t.Fatalf("the unsigned thinking was sent:\n%s", next.body)
		}
	})
}

// TestAnthropicSwitchingClaudeModelsKeepsEachModelsOwnThinking (A3): Opus,
// then Sonnet, then Opus again. Sonnet's request goes without Opus's
// thinking (another wire model's reasoning, the transcript's rule), and the
// return to Opus sends Opus's own signed block back byte for byte, in place,
// and none of Sonnet's — and the binding rule's stand-in keeps it: nothing
// before it changed.
func TestAnthropicSwitchingClaudeModelsKeepsEachModelsOwnThinking(t *testing.T) {
	api := newAnAPI(t)
	f, opts := onAnthropic(t, api)
	s := f.open(opts)
	api.queue(
		api.answer(anThought("opus thinks", "sig-o1"), anSaid("One.")),
		api.answer(anThought("sonnet thinks", "sig-s1"), anSaid("Two.")),
		api.answer(anSaid("Three.")),
	)
	run(t, s, "first")
	if err := s.SetModel(anSonnet); err != nil {
		t.Fatal(err)
	}
	run(t, s, "second")
	if err := s.SetModel(anOpus); err != nil {
		t.Fatal(err)
	}
	run(t, s, "third")

	valid(t, api)
	reqs := api.requests()
	sonnet, opus := reqs[1], reqs[2]
	if !strings.Contains(string(sonnet.body), `"model":"claude-sonnet-5-5"`) || !strings.Contains(string(opus.body), `"model":"claude-opus-5-5"`) {
		t.Fatalf("the requests went to the wrong models:\n%s\n%s", sonnet.body, opus.body)
	}
	if strings.Contains(string(sonnet.body), "sig-o1") || strings.Contains(string(sonnet.body), "opus thinks") {
		t.Fatalf("Sonnet was sent Opus's thinking:\n%s", sonnet.body)
	}
	if got := blockAt(opus, 1, 0); got != `{"type":"thinking","thinking":"opus thinks","signature":"sig-o1"}` {
		t.Fatalf("the return to Opus sends %s where its own thinking was", got)
	}
	if strings.Contains(string(opus.body), "sig-s1") {
		t.Fatalf("Opus was sent Sonnet's thinking:\n%s", opus.body)
	}
	if opus.stale != nil {
		t.Fatalf("the binding rule finds Opus's own block stale: %v", opus.stale)
	}
}

// TestAnthropicParallelResultsImageAndSteerAreOneUserMessage (A3, §3.3 item
// 5): a step's two parallel reads — one of them an image, which the wrapper
// moves out of its result into a user message of its own (regroupToolImages)
// — and a steer accepted while they ran reach the next request as ONE user
// message, the tool results first, then the image and the steer. The control:
// the prompt the wrapped model was handed holds the results (the image inside
// one) and the steer as two messages, a tool message and a user message.
func TestAnthropicParallelResultsImageAndSteerAreOneUserMessage(t *testing.T) {
	api := newAnAPI(t)
	f, opts := onAnthropic(t, api)
	var handed []fantasy.Call
	var mu sync.Mutex
	opts.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		lm, err := llm.New(r, llm.WithAnthropicAPIBase(api.base))
		return &callRecorder{LanguageModel: lm, mu: &mu, calls: &handed}, err
	}
	s := f.open(opts)
	f.put("a.txt", "alpha\n")
	f.put("pic.png", string(testPNG(t, 20, 10)))
	api.queue(
		api.calls(anCall("toolu_01", "read", `{"filePath":"a.txt"}`), anCall("toolu_02", "read", `{"filePath":"pic.png"}`)),
		api.answer(anSaid("Alpha and a grey picture.")),
	)
	var once sync.Once
	if _, err := s.Run(context.Background(), "read a.txt and pic.png", func(ev Event) {
		if _, ok := ev.(ToolFinished); ok {
			once.Do(func() {
				if err := sendSteer(s, "and say what colour"); err != nil {
					t.Errorf("Steer: %v", err)
				}
			})
		}
	}); err != nil {
		t.Fatal(err)
	}

	valid(t, api)
	req := api.requests()[1]
	got := shape(t, req)
	if len(got) != 3 || got[0] != "user:text" || got[1] != "assistant:tool_use,tool_use" {
		t.Fatalf("the second request = %q", got)
	}
	last := strings.Split(strings.TrimPrefix(got[2], "user:"), ",")
	if len(last) < 4 || last[0] != "tool_result" || last[1] != "tool_result" || !slices.Contains(last, "image") || slices.Contains(last[2:], "tool_result") {
		t.Fatalf("the user message after the calls = %q, want both results first, then the image and the steer", got[2])
	}
	if steer := blockAt(req, 2, len(last)-1); !strings.Contains(steer, "and say what colour") {
		t.Fatalf("the user message's last block is %s, want the steer", steer)
	}
	mu.Lock()
	prompt := handed[1].Prompt
	mu.Unlock()
	var roles []string
	for _, m := range prompt {
		roles = append(roles, string(m.Role))
	}
	equal(t, "control: the roles of the prompt the wrapped model was handed", roles, []string{"system", "user", "assistant", "tool", "user"})
}

// callRecorder is a model that records the calls it is handed.
type callRecorder struct {
	fantasy.LanguageModel
	mu    *sync.Mutex
	calls *[]fantasy.Call
}

func (c *callRecorder) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	c.mu.Lock()
	*c.calls = append(*c.calls, call)
	c.mu.Unlock()
	return c.LanguageModel.Stream(ctx, call)
}

// TestAnthropicSummarizerRequestIsValid (A3, compact.go's aligned
// summarizer): the summary's request on a Claude model — the session's
// history with the compaction prompt after it, an image in it, the tools
// offered inert — is one the API takes: the image left out as the
// summarizer's placeholder, the call and its result paired, the prompt the
// last user message's text, thinking with its binding. The control: the
// turn's own request did carry the image.
func TestAnthropicSummarizerRequestIsValid(t *testing.T) {
	api := newAnAPI(t)
	f, opts := onAnthropic(t, api)
	s := f.open(opts)
	f.put("pic.png", string(testPNG(t, 20, 10)))
	api.queue(
		api.calls(anThought("look", "sig-p1"), anCall("toolu_01", "read", `{"filePath":"pic.png"}`)),
		api.answer(anSaid("A grey picture.")),
		api.anSummary("Looked at pic.png."),
	)
	run(t, s, "what is pic.png?")
	if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	valid(t, api)
	reqs := api.requests()
	if !strings.Contains(string(reqs[1].body), `"type":"image"`) {
		t.Fatalf("control: the turn's own request carried no image:\n%s", reqs[1].body)
	}
	summary := reqs[2]
	if strings.Contains(string(summary.body), `"type":"image"`) {
		t.Fatalf("the summarizer was sent the image:\n%s", summary.body)
	}
	if b, beta := binding(t, summary); b != "drop_block" || !beta {
		t.Fatalf("the summarizer's request: block_binding %q, beta %v", b, beta)
	}
	got := shape(t, summary)
	if n := len(got); n < 3 || !strings.HasPrefix(got[n-1], "user:") || !strings.Contains(got[1], "thinking,tool_use") {
		t.Fatalf("the summarizer's request = %q", got)
	}
	if last := summary.msgs[len(summary.msgs)-1]; !strings.Contains(string(last.Content[len(last.Content)-1]), "1. Request and intent") {
		t.Fatalf("the summarizer's last block is not the compaction prompt: %s", last.Content[len(last.Content)-1])
	}
	if !strings.Contains(string(summary.body), `"tools":[`) {
		t.Fatalf("the summarizer's request offers no tools:\n%s", summary.body)
	}
}

// TestAnthropicRefusalCategoryReachesTheResult (§3.4, P6): a turn the model
// refuses ends StopRefusal with the category the API gave in stop_details,
// as Result.RefusalCategory — none when it gave none, and none when what it
// gave is not a short identifier (the adapter keeps no other). The model's
// partial text stays the turn's. The control: a model on another driver
// (scripted, so no Anthropic finish) gives none.
func TestAnthropicRefusalCategoryReachesTheResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details map[string]any
		want    string
	}{
		{"a category", map[string]any{"type": "refusal", "category": "cyber"}, "cyber"},
		{"none", nil, ""},
		{"not an identifier", map[string]any{"type": "refusal", "category": "Cyber \x1b[31m"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newAnAPI(t)
			f, opts := onAnthropic(t, api)
			s := f.open(opts)
			api.queue(api.reply("refusal", tc.details, anSaid("I can't help")))
			res := run(t, s, "do the thing")
			if res.StopReason != StopRefusal || res.RefusalCategory != tc.want {
				t.Fatalf("Result = %+v; want refusal, category %q", res, tc.want)
			}
			if lines := entries(transcript(t, s)); !strings.HasSuffix(lines[len(lines)-1], "refusal: I can't help") {
				t.Fatalf("the transcript's last entry = %q", lines[len(lines)-1])
			}
		})
	}
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(reply(textParts("no"), finish(fantasy.FinishReasonContentFilter)))
	if res := run(t, s, "do the thing"); res.StopReason != StopRefusal || res.RefusalCategory != "" {
		t.Fatalf("control: another driver's refusal = %+v", res)
	}
}

// TestAnthropicSubagentReachesTheBaseAndCarriesTheRefusal (§3.4, P21, P6): a
// sub-agent of a session on Claude is built at the session's Anthropic base —
// its request reaches the fake — and its refusal's category reaches the
// parent's model in the call's result, after the child's answer: after a
// short one, and after the head of one past the result's cap, whose cut must
// not take the note with it (C3-1). The controls: the long answer was cut —
// its last line is not in the result, and is in the spill file, which holds
// the answer alone — and a base that refuses every build after the parent's
// (a URL that is no loopback, which the client refuses before any request)
// fails the sub-agent's build, so the child's model is built through the
// option, never around it.
func TestAnthropicSubagentReachesTheBaseAndCarriesTheRefusal(t *testing.T) {
	const note = "(The sub-agent's model refused to continue; refusal category: cyber.)"
	var long []string
	for i := 1; i <= tool.MaxLines+1; i++ {
		long = append(long, fmt.Sprintf("line %d", i))
	}
	for _, tc := range []struct {
		name   string
		said   string // the child's answer before it is refused; "" when its build is refused
		refuse bool   // the base refuses every build after the parent's
	}{
		{name: "a short answer", said: "I won't"},
		{name: "an answer past the cap", said: strings.Join(long, "\n")},
		{name: "a base that refuses the child's build", refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newAnAPI(t)
			f, opts := onAnthropic(t, api)
			var mu sync.Mutex
			builds := 0
			opts.AnthropicAPIBase = func() (string, error) {
				mu.Lock()
				defer mu.Unlock()
				builds++
				if tc.refuse && builds > 1 {
					return "https://api.example.com", nil
				}
				return api.base()
			}
			s := f.open(opts)
			api.queue(api.calls(anCall("toolu_01", "agent", an(task("look", "the child's task")))))
			if !tc.refuse {
				api.queue(api.reply("refusal", map[string]any{"type": "refusal", "category": "cyber"}, anSaid(tc.said)))
			}
			api.queue(api.answer(anSaid("Understood.")))
			var ev events
			if _, err := s.Run(context.Background(), "delegate it", ev.sink); err != nil {
				t.Fatal(err)
			}
			valid(t, api)
			reqs := api.requests()
			result := callResult(t, ev.list(), "t1.1.1")
			if tc.refuse {
				if len(reqs) != 2 || !result.IsError || !strings.Contains(result.Text, messagesapi.ErrBaseURL.Error()) {
					t.Fatalf("control: %d requests, the call = %+v; want the child's build refused and no request of its own", len(reqs), result)
				}
				return
			}
			if len(reqs) != 3 || !strings.Contains(string(reqs[1].body), "the child's task") {
				t.Fatalf("%d requests; the second should be the child's:\n%s", len(reqs), reqs[1].body)
			}
			if !strings.Contains(string(reqs[2].body), "refusal category: cyber") {
				t.Fatalf("the parent's model was not told the category:\n%s", reqs[2].body)
			}
			cut := strings.Count(tc.said, "\n") >= tool.MaxLines
			if (result.Trunc.Spill != "") != cut {
				t.Fatalf("the call's result was cut: %v, want %v (spill %q)", result.Trunc.Spill != "", cut, result.Trunc.Spill)
			}
			if !cut {
				if want := tc.said + "\n\n" + note; result.Text != want {
					t.Fatalf("the call's result = %q, want %q", result.Text, want)
				}
				return
			}
			last := long[len(long)-1]
			if !strings.HasPrefix(result.Text, "line 1\n") || strings.Contains(result.Text, last+"\n") ||
				!strings.Contains(result.Text, "...1 lines truncated...") {
				t.Fatalf("control: the long answer was not cut to its head:\n%s", result.Text[max(0, len(result.Text)-600):])
			}
			if !strings.HasSuffix(result.Text, "\n\n"+note) {
				t.Fatalf("the cut result does not end with the refusal's note:\n%s", result.Text[max(0, len(result.Text)-600):])
			}
			spilled, err := os.ReadFile(result.Trunc.Spill)
			if err != nil {
				t.Fatal(err)
			}
			if string(spilled) != tc.said {
				t.Fatalf("control: the spill file holds %d bytes ending %q, want the child's whole answer and no note",
					len(spilled), spilled[max(0, len(spilled)-80):])
			}
		})
	}
}

// TestCompletedResultNamesTheRefusalCategory: a child's refusal note says the
// category when its Result has one, and is §3.7's fixed text otherwise; it
// comes back apart from the answer, and withNote puts it after the answer (or
// alone, for an answer with nothing in it).
func TestCompletedResultNamesTheRefusalCategory(t *testing.T) {
	for _, tc := range []struct {
		res  Result
		text string
		want string
	}{
		{Result{StopReason: StopRefusal, RefusalCategory: "bio"}, "no", "no\n\n(The sub-agent's model refused to continue; refusal category: bio.)"},
		{Result{StopReason: StopRefusal, RefusalCategory: "bio"}, "", "(The sub-agent's model refused to continue; refusal category: bio.)"},
		{Result{StopReason: StopRefusal}, "no", "no\n\n" + subagentRefused},
		{Result{StopReason: StopEndTurn, RefusalCategory: "bio"}, "yes", "yes"},
		{Result{StopReason: StopEndTurn}, " ", subagentNoMessage},
	} {
		res, note := completedResult(tc.res, tc.text, false)
		if strings.Contains(res.Text, "refused") {
			t.Errorf("completedResult(%+v, %q): the answer %q holds the note", tc.res, tc.text, res.Text)
		}
		if got := withNote(res.Text, note); got != tc.want {
			t.Errorf("completedResult(%+v, %q) with its note = %q, want %q", tc.res, tc.text, got, tc.want)
		}
	}
}

// TestAnthropicRefusedAccessAndUnknownModelAreWorded (P27): Anthropic's 403
// is a model the key may not use and its 404 a wire id the account cannot
// use, each said so with Anthropic's own reason, while their kinds stay
// ErrAuth and ErrModelNotFound; its 401 is still the key's. The control is
// errors_test.go's: another driver's 403 and 404 keep their words.
func TestAnthropicRefusedAccessAndUnknownModelAreWorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  anReply
		is     error
		prefix string
	}{
		{"403", anFail(403, "permission_error", "Your organization cannot use this model."), ErrAuth,
			`harness: Anthropic refused access to model "anthropic/claude-opus-5-5" (provider "anthropic", model "anthropic/claude-opus-5-5", HTTP 403): Your organization cannot use this model.`},
		{"404", anFail(404, "not_found_error", "model: claude-opus-5-5"), ErrModelNotFound,
			`harness: the Anthropic account cannot use "claude-opus-5-5" (provider "anthropic", model "anthropic/claude-opus-5-5", HTTP 404): model: claude-opus-5-5`},
		{"401", anFail(401, "authentication_error", "invalid x-api-key"), ErrAuth,
			`harness: the provider rejected the API key (provider "anthropic", model "anthropic/claude-opus-5-5", HTTP 401): invalid x-api-key`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newAnAPI(t)
			f, opts := onAnthropic(t, api)
			s := f.open(opts)
			api.queue(tc.reply)
			_, err := s.Run(context.Background(), "hi", nil)
			var pe *ProviderError
			if !errors.As(err, &pe) || !errors.Is(err, tc.is) || pe.WireModel != "claude-opus-5-5" {
				t.Fatalf("Run = %v; want a ProviderError of kind %v naming the wire model", err, tc.is)
			}
			if !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Fatalf("error %q\nwant it to begin %q", err, tc.prefix)
			}
		})
	}
}
