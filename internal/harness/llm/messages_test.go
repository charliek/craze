package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The tests here run Anthropic's driver (plan 044 §3.3) — the adapter over
// package messagesapi — against a local server speaking the Messages API's
// stream in the shapes the live probes saw (LF-3; one test replays the real
// capture in messagesapi/testdata), through Fantasy's real agent wherever the
// step loop matters. The key is a canary; no test reaches Anthropic.

// anthropicKey is the only Anthropic key any test here holds.
const anthropicKey = "sk-ant-canary-not-a-secret-0001"

// claude is a resolved model on Anthropic's driver, as the table resolves
// one: a key, no base URL, adaptive thinking.
func claude() modeltable.Resolved {
	return modeltable.Resolved{
		Alias:           "anthropic/claude-test-5",
		ProviderID:      "anthropic",
		Driver:          modeltable.DriverAnthropic,
		APIKey:          anthropicKey,
		WireModel:       "claude-test-5",
		Name:            "Claude Test 5",
		ContextWindow:   1000000,
		MaxOutputTokens: 64000,
		Efforts:         []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:   "medium",
		Vision:          true,
		Thinking:        modeltable.ThinkingAdaptive,
	}
}

// claudeModel is r's model aimed at the server, unwrapped.
func (s *rxServer) claudeModel(t *testing.T, r modeltable.Resolved) *messagesModel {
	t.Helper()
	m, err := newMessagesModel(r, anthropicKey, s.srv.URL, nil)
	if err != nil {
		t.Fatalf("newMessagesModel: %v", err)
	}
	return m
}

// claudeNew is r's model as New builds it — wrapped — aimed at the server.
func (s *rxServer) claudeNew(t *testing.T, r modeltable.Resolved) fantasy.LanguageModel {
	t.Helper()
	lm, err := New(r, WithAnthropicAPIBase(func() (string, error) { return s.srv.URL, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return lm
}

// The Messages API's events, in its shapes (LF-3).

// an is v as JSON as the API writes it, nothing HTML-escaped: an event, or
// a value in one.
func an(v any) string {
	b, err := encodeJSON(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// anReply streams the events of every group, each as one SSE event, with a
// request id and header's pairs (name, value, …) on the response.
func anReply(header []string, groups ...[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_test_0001")
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		for _, g := range groups {
			for _, e := range g {
				var head struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(e), &head)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
			}
		}
	}
}

// anJSON answers status with body as JSON, and header's pairs.
func anJSON(status int, body string, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_test_0002")
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func anErrorBody(typ, message string) string {
	return an(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}, "request_id": "req_body_1"})
}

func anUsage(input, cacheWrite, cacheRead, output int) map[string]any {
	return map[string]any{
		"input_tokens":                input,
		"cache_creation_input_tokens": cacheWrite,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": cacheWrite, "ephemeral_1h_input_tokens": 0},
		"output_tokens":               output,
	}
}

// anStart is message_start with usage and, when given, the request's input
// transformations.
func anStart(usage map[string]any, transforms ...map[string]any) []string {
	msg := map[string]any{"id": "msg_test_1", "type": "message", "role": "assistant", "model": "claude-test-5",
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage}
	if transforms != nil {
		msg["input_transformations"] = transforms
	}
	return []string{an(map[string]any{"type": "message_start", "message": msg})}
}

func anBlockStart(idx int, block map[string]any) string {
	return an(map[string]any{"type": "content_block_start", "index": idx, "content_block": block})
}

func anDelta(idx int, delta map[string]any) string {
	return an(map[string]any{"type": "content_block_delta", "index": idx, "delta": delta})
}

func anBlockStop(idx int) string {
	return an(map[string]any{"type": "content_block_stop", "index": idx})
}

// anText is a text block at idx, its text in one delta.
func anText(idx int, text string) []string {
	return []string{anBlockStart(idx, map[string]any{"type": "text", "text": ""}),
		anDelta(idx, map[string]any{"type": "text_delta", "text": text}), anBlockStop(idx)}
}

// anThinking is a thinking block at idx, its text in one delta and its
// signature in the fragments given.
func anThinking(idx int, text string, signature ...string) []string {
	evs := []string{anBlockStart(idx, map[string]any{"type": "thinking", "thinking": "", "signature": ""}),
		anDelta(idx, map[string]any{"type": "thinking_delta", "thinking": text})}
	for _, s := range signature {
		evs = append(evs, anDelta(idx, map[string]any{"type": "signature_delta", "signature": s}))
	}
	return append(evs, anBlockStop(idx))
}

func anRedacted(idx int, data string) []string {
	return []string{anBlockStart(idx, map[string]any{"type": "redacted_thinking", "data": data}), anBlockStop(idx)}
}

// anToolUse is a tool_use block at idx, its input in the API's first, empty
// fragment and then whole.
func anToolUse(idx int, id, name, input string) []string {
	return []string{anBlockStart(idx, map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}, "caller": map[string]any{"type": "direct"}}),
		anDelta(idx, map[string]any{"type": "input_json_delta", "partial_json": ""}),
		anDelta(idx, map[string]any{"type": "input_json_delta", "partial_json": input}), anBlockStop(idx)}
}

// anEnd is message_delta with stop and its details (nil for none) and an
// output count, then message_stop.
func anEnd(stop string, details map[string]any, output int) []string {
	return []string{
		an(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil, "stop_details": details},
			"usage": map[string]any{"output_tokens": output}}),
		an(map[string]any{"type": "message_stop"}),
	}
}

func anErrorEvent(typ, message string) []string {
	return []string{an(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}})}
}

// anAnswer is a one-text-block reply.
func anAnswer(text string) http.HandlerFunc {
	return anReply(nil, anStart(anUsage(10, 0, 0, 1)), anText(0, text), anEnd("end_turn", nil, 2))
}

// anMessage is one message of a request body, its blocks raw: exactly the
// bytes the body holds for each.
type anMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// anMessages is a request body's messages.
func anMessages(t *testing.T, body []byte) []anMessage {
	t.Helper()
	var req struct {
		Messages []anMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("a request body is not JSON: %v\n%s", err, body)
	}
	return req.Messages
}

// anBlock is a block decoded.
func anBlock(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("a block is not JSON: %v", err)
	}
	return b
}

// anShape is each message as role:type,type,…
func anShape(t *testing.T, msgs []anMessage) []string {
	t.Helper()
	var out []string
	for _, m := range msgs {
		var types []string
		for _, c := range m.Content {
			types = append(types, fmt.Sprint(anBlock(t, c)["type"]))
		}
		out = append(out, m.Role+":"+strings.Join(types, ","))
	}
	return out
}

// pathTool is a tool named name that reports the path it was asked for.
func pathTool(name string, seen *[]string, mu *sync.Mutex) fantasy.AgentTool {
	type args struct {
		Path string `json:"path" description:"the file to read"`
	}
	return fantasy.NewParallelAgentTool(name, "Read a file.", func(_ context.Context, in args, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		mu.Lock()
		*seen = append(*seen, in.Path)
		mu.Unlock()
		return fantasy.NewTextResponse("contents of " + in.Path), nil
	})
}

// opusCapture is a tool step exactly as Opus 5.5 streamed it (LF-3), its
// thinking text, and its signature.
func opusCapture(t *testing.T) (capture, thinking, signature string) {
	t.Helper()
	b, err := os.ReadFile("messagesapi/testdata/opus-tool-step.sse")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`"signature_delta","signature":"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("the capture has no signature")
	}
	return string(b), "I'll go ahead and read the go.mod file.\n\n", string(m[1])
}

func rawReply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_capture_1")
		_, _ = io.WriteString(w, body)
	}
}

// TestMessagesToolLoop runs one turn through Fantasy's agent — Opus 5.5's
// real tool step (the capture: summarized thinking, then a call), then an
// answer — and pins both requests whole (testdata/messages_tool_loop.golden):
// the system prompt as a block with its cache breakpoint and the top-level
// automatic one, the tool, adaptive thinking with its display and
// block_binding, the effort, the call's output ceiling as max_tokens and no
// sampling setting. The second request replays step one's thinking byte for
// byte, signature whole, in its place ahead of the call it led to, and the
// call's result by its id. Every request carries the key, the API's version,
// the binding beta and craze's user agent.
func TestMessagesToolLoop(t *testing.T) {
	capture, thinking, signature := opusCapture(t)
	srv := newRxServer(t, rawReply(capture),
		anReply(nil, anStart(anUsage(14, 0, 402, 1)), anText(0, "The module is github.com/charliek/craze."), anEnd("end_turn", nil, 12)))
	r := claude()
	var mu sync.Mutex
	var seen []string
	agent := fantasy.NewAgent(srv.claudeModel(t, r), fantasy.WithSystemPrompt("You are craze."),
		fantasy.WithTools(pathTool("read", &seen, &mu)), fantasy.WithMaxRetries(0))
	effort, err := EffortOptions(r, "medium")
	if err != nil {
		t.Fatal(err)
	}
	ceiling, temperature, topP, topK := int64(32000), 0.2, 0.9, int64(5)
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:          "Which module is this? Read go.mod.",
		ProviderOptions: effort,
		MaxOutputTokens: &ceiling,
		Temperature:     &temperature,
		TopP:            &topP,
		TopK:            &topK,
	})
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if got := res.Response.Content.Text(); got != "The module is github.com/charliek/craze." {
		t.Fatalf("answer = %q", got)
	}
	if !slices.Equal(seen, []string{"go.mod"}) {
		t.Fatalf("the tool was asked for %v", seen)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	var pinned bytes.Buffer
	for i, req := range reqs {
		for h, want := range map[string]string{"X-Api-Key": anthropicKey, "Anthropic-Version": "2023-06-01",
			"Anthropic-Beta": messagesapi.BindingBeta, "User-Agent": "craze", "Accept": "text/event-stream"} {
			if got := req.header.Get(h); got != want {
				t.Errorf("request %d %s = %q, want %q", i, h, got, want)
			}
		}
		if req.path != "/v1/messages" {
			t.Errorf("request %d went to %s", i, req.path)
		}
		fields := fieldsOf(t, req.body)
		for _, f := range []string{"temperature", "top_p", "top_k", "stop_sequences", "metadata", "speed", "fallbacks", "inference_geo"} {
			if _, ok := fields[f]; ok {
				t.Errorf("request %d sends %s", i, f)
			}
		}
		if err := json.Indent(&pinned, req.body, "", "  "); err != nil {
			t.Fatal(err)
		}
		pinned.WriteString("\n")
	}
	golden(t, "messages_tool_loop.golden", pinned.Bytes())

	// In place, byte for byte: the second request's assistant message is
	// the step as it streamed — the thinking block exactly as the model
	// wrote it, then the call.
	msgs := anMessages(t, reqs[1].body)
	if got, want := anShape(t, msgs), []string{"user:text", "assistant:thinking,tool_use", "user:tool_result"}; !slices.Equal(got, want) {
		t.Fatalf("second request = %v, want %v", got, want)
	}
	wantThinking := `{"type":"thinking","thinking":` + an(thinking) + `,"signature":` + an(signature) + `}`
	if got := string(msgs[1].Content[0]); got != wantThinking || len(signature) != 840 {
		t.Fatalf("the replayed thinking is not the streamed one:\n got %s\nwant %s", got, wantThinking)
	}
	if call := anBlock(t, msgs[1].Content[1]); call["id"] != "toolu_01GzUYQdbrz3Y6pjVvjTYLRj" || call["name"] != "read" ||
		string(msgs[1].Content[1]) != `{"type":"tool_use","id":"toolu_01GzUYQdbrz3Y6pjVvjTYLRj","name":"read","input":{"path":"go.mod"}}` {
		t.Fatalf("the replayed call = %s", msgs[1].Content[1])
	}
	if result := anBlock(t, msgs[2].Content[0]); result["tool_use_id"] != "toolu_01GzUYQdbrz3Y6pjVvjTYLRj" {
		t.Fatalf("the result = %v", result)
	}

	// Usage: the capture's step (402 in, 68 out, 19 of them thinking) and
	// the answer's (14 in, 402 read from the cache, 12 out).
	want := fantasy.Usage{InputTokens: 402 + 14, CacheReadTokens: 402, OutputTokens: 68 + 12, ReasoningTokens: 19, TotalTokens: 470 + 428}
	if res.TotalUsage != want {
		t.Fatalf("usage = %+v, want %+v", res.TotalUsage, want)
	}
}

// TestMessagesReplaysInWireOrder (P1): a step's blocks — text, calls and
// thinking interleaved — are replayed in the order the model wrote them, each
// thinking block where it was and with its signature, and the calls' results
// follow in call order. The step's messages survive the transcript's JSON
// round trip as the same request: a request built from the stored step is the
// agent's own, byte for byte.
func TestMessagesReplaysInWireOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks [][]string
		want   string
		// replayed are the reasoning blocks the second request must carry,
		// in order, exactly: each thinking block's text and its signature's
		// fragments joined, a redacted one's data.
		replayed []string
	}{
		{"text, tool_use, thinking, tool_use", [][]string{
			anText(0, "Reading a first."),
			anToolUse(1, "toolu_a", "read_file", `{"path":"a.txt"}`),
			anThinking(2, "Now b.", "sig-b-1", "sig-b-2"),
			anToolUse(3, "toolu_b", "read_file", `{"path":"b.txt"}`),
		}, "assistant:text,tool_use,thinking,tool_use", []string{`{"type":"thinking","thinking":"Now b.","signature":"sig-b-1sig-b-2"}`}},
		{"thinking, tool_use, text, tool_use", [][]string{
			anThinking(0, "First a.", "sig-a"),
			anToolUse(1, "toolu_a", "read_file", `{"path":"a.txt"}`),
			anText(2, "Then b."),
			anToolUse(3, "toolu_b", "read_file", `{"path":"b.txt"}`),
		}, "assistant:thinking,tool_use,text,tool_use", []string{`{"type":"thinking","thinking":"First a.","signature":"sig-a"}`}},
		{"redacted_thinking, tool_use, thinking, tool_use", [][]string{
			anRedacted(0, "EuoBCkgIBRABGAIi-opaque=="),
			anToolUse(1, "toolu_a", "read_file", `{"path":"a.txt"}`),
			anThinking(2, "", "sig-omitted"),
			anToolUse(3, "toolu_b", "read_file", `{"path":"b.txt"}`),
		}, "assistant:redacted_thinking,tool_use,thinking,tool_use", []string{
			`{"type":"redacted_thinking","data":"EuoBCkgIBRABGAIi-opaque=="}`,
			`{"type":"thinking","thinking":"","signature":"sig-omitted"}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := append([][]string{anStart(anUsage(20, 0, 0, 1))}, tc.blocks...)
			step = append(step, anEnd("tool_use", nil, 40))
			srv := newRxServer(t, anReply(nil, step...), anAnswer("both read"), anAnswer("again"))
			lm := srv.claudeModel(t, claude())
			var mu sync.Mutex
			var seen []string
			agent := fantasy.NewAgent(lm, fantasy.WithTools(pathTool("read_file", &seen, &mu)), fantasy.WithMaxRetries(0))
			res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "read a and b"})
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(seen)
			if !slices.Equal(seen, []string{"a.txt", "b.txt"}) {
				t.Fatalf("the tool ran for %v", seen)
			}
			reqs := srv.requests()
			msgs := anMessages(t, reqs[1].body)
			if got, want := anShape(t, msgs), []string{"user:text", tc.want, "user:tool_result,tool_result"}; !slices.Equal(got, want) {
				t.Fatalf("second request = %v, want %v", got, want)
			}
			var replayed []string
			for _, raw := range msgs[1].Content {
				if typ := anBlock(t, raw)["type"]; typ == "thinking" || typ == "redacted_thinking" {
					replayed = append(replayed, string(raw))
				}
			}
			if !slices.Equal(replayed, tc.replayed) {
				t.Fatalf("replayed reasoning =\n%s\nwant\n%s", strings.Join(replayed, "\n"), strings.Join(tc.replayed, "\n"))
			}
			for i, id := range []string{"toolu_a", "toolu_b"} {
				if got := anBlock(t, msgs[2].Content[i])["tool_use_id"]; got != id {
					t.Errorf("result %d answers %v, want %s", i, got, id)
				}
			}

			// The stored form: the step's messages through JSON, as the
			// transcript keeps them, replay as the agent replayed them.
			raw, err := json.Marshal(res.Steps[0].Messages)
			if err != nil {
				t.Fatal(err)
			}
			var stored []fantasy.Message
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			streamPrompt(t, lm, srv, append(fantasy.Prompt{fantasy.NewUserMessage("read a and b")}, stored...))
			again := anMessages(t, srv.requests()[2].body)
			if len(again) != 3 || !slices.EqualFunc(again[1].Content, msgs[1].Content, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) {
				t.Fatalf("the stored step replays as\n%s\nnot as the agent's\n%s", again[1].Content, msgs[1].Content)
			}
		})
	}
}

// h12 is the first 12 hex digits of id's SHA-256, the suffix a sanitized id
// ends with.
func h12(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:12]
}

// toolStep is an assistant message of calls to name with ids, then the tool
// message answering each.
func toolStep(name string, ids ...string) []fantasy.Message {
	var calls, results []fantasy.MessagePart
	for _, id := range ids {
		calls = append(calls, fantasy.ToolCallPart{ToolCallID: id, ToolName: name, Input: `{"path":"x"}`})
		results = append(results, fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: "x of " + id}})
	}
	return []fantasy.Message{{Role: fantasy.MessageRoleAssistant, Content: calls}, {Role: fantasy.MessageRoleTool, Content: results}}
}

// TestMessagesSanitizesToolIDs (P3): another provider's call ids — with
// characters the API refuses, too long, Unicode, empty — go as valid ids, each
// call's result by its call's, one mapping per request: a valid id is itself
// unless it is taken, a sanitized one that would take a valid id's is told
// apart, and an id a provider reused in a later step maps as it did. A tool
// name the API refuses is sanitized the same way. The same history sends the
// same bytes.
func TestMessagesSanitizesToolIDs(t *testing.T) {
	long := strings.Repeat("x", 130)
	lookalike := "x_y_" + h12("x.y") // valid, and exactly what x.y sanitizes to
	prompt := fantasy.Prompt{fantasy.NewUserMessage("go")}
	prompt = append(prompt, toolStep("read_file", "call_ok", "functions.read:0", long, "呼び出し-1", "", "tool-call-0")...)
	prompt = append(prompt, toolStep("read_file", lookalike, "x.y", "tool-call-0")...)
	prompt = append(prompt, toolStep("functions.read", "call_named")...)
	prompt = append(prompt, fantasy.NewUserMessage("again"))

	srv := newRxServer(t, anAnswer("ok"), anAnswer("ok"))
	lm := srv.claudeModel(t, claude())
	body := streamPrompt(t, lm, srv, prompt)
	want := [][]string{
		{"call_ok", "functions_read_0_" + h12("functions.read:0"), strings.Repeat("x", 100) + "_" + h12(long), "____-1_" + h12("呼び出し-1"), "_" + h12(""), "tool-call-0"},
		{lookalike, lookalike + "_2", "tool-call-0"},
		{"call_named"},
	}
	msgs := anMessages(t, body)
	for step, ids := range want {
		calls, results := msgs[1+2*step], msgs[2+2*step]
		for i, id := range ids {
			call, result := anBlock(t, calls.Content[i]), anBlock(t, results.Content[i])
			if call["id"] != id || result["tool_use_id"] != id {
				t.Errorf("step %d call %d: tool_use %v, tool_result %v; want both %q", step, i, call["id"], result["tool_use_id"], id)
			}
			if !validID.MatchString(id) {
				t.Errorf("step %d call %d: %q is not an id the API takes", step, i, id)
			}
		}
	}
	if name := anBlock(t, msgs[5].Content[0])["name"]; name != "functions_read_"+h12("functions.read") {
		t.Errorf("a refused tool name went as %v", name)
	}
	if again := streamPrompt(t, lm, srv, prompt); !bytes.Equal(again, body) {
		t.Fatal("the same history sent different bytes")
	}
}

// blocksOf is every block in a request body, tool results' inner blocks
// included.
func blocksOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range anMessages(t, body) {
		for _, raw := range m.Content {
			b := anBlock(t, raw)
			out = append(out, b)
			if inner, ok := b["content"].([]any); ok {
				for _, c := range inner {
					out = append(out, c.(map[string]any))
				}
			}
		}
	}
	return out
}

// TestMessagesNeverSendsEmptyBlocks (P20, LF-6): text that is empty or blank
// — a user's, the assistant's, a tool result's — is never a block (the API
// refuses one forever), an empty result is a tool_result with no content, and
// an error is is_error with its text.
func TestMessagesNeverSendsEmptyBlocks(t *testing.T) {
	user := fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
		fantasy.TextPart{Text: ""}, fantasy.TextPart{Text: " \n"}, fantasy.TextPart{Text: "the question"}}}
	assistant := func(parts ...fantasy.MessagePart) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
	}
	result := func(id string, out fantasy.ToolResultOutputContent) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: id, Output: out}}}
	}
	call := func(id string) fantasy.ToolCallPart {
		return fantasy.ToolCallPart{ToolCallID: id, ToolName: "read_file", Input: `{}`}
	}
	prompt := fantasy.Prompt{
		user,
		assistant(fantasy.TextPart{Text: ""}, fantasy.TextPart{Text: "\t "}, call("call_1")),
		result("call_1", fantasy.ToolResultOutputContentText{Text: ""}),
		assistant(call("call_2")),
		result("call_2", fantasy.ToolResultOutputContentText{Text: "  \n"}),
		assistant(call("call_3")),
		result("call_3", fantasy.ToolResultOutputContentError{Error: errors.New("boom")}),
		fantasy.NewUserMessage("go on"),
	}
	srv := newRxServer(t, anAnswer("ok"))
	body := streamPrompt(t, srv.claudeModel(t, claude()), srv, prompt)
	for _, b := range blocksOf(t, body) {
		if b["type"] == "text" && strings.TrimSpace(fmt.Sprint(b["text"])) == "" {
			t.Fatalf("an empty text block was sent: %s", body)
		}
	}
	msgs := anMessages(t, body)
	if got, want := anShape(t, msgs), []string{"user:text", "assistant:tool_use", "user:tool_result", "assistant:tool_use",
		"user:tool_result", "assistant:tool_use", "user:tool_result,text"}; !slices.Equal(got, want) {
		t.Fatalf("request = %v, want %v", got, want)
	}
	for _, i := range []int{2, 4} {
		if r := anBlock(t, msgs[i].Content[0]); r["content"] != nil || r["is_error"] != nil {
			t.Errorf("an empty result went as %v", r)
		}
	}
	if r := string(msgs[6].Content[0]); r != `{"type":"tool_result","tool_use_id":"call_3","content":[{"type":"text","text":"boom"}],"is_error":true}` {
		t.Errorf("an error result went as %s", r)
	}
}

// anthropicPart is a reasoning part as this driver keeps one.
func anthropicPart(text string, md *anthropicThinking) fantasy.ReasoningPart {
	return fantasy.ReasoningPart{Text: text, ProviderOptions: fantasy.ProviderOptions{thinkingKey: md}}
}

// TestMessagesDropsForeignReasoning (§3.3 item 3): only a reasoning part this
// driver wrote, signed or redacted, is replayed; another provider's (the
// Responses metadata), one with none (an interrupted step's), and one of this
// driver's with nothing to send back are dropped, and the calls they led to
// are sent all the same (LF-9).
func TestMessagesDropsForeignReasoning(t *testing.T) {
	step := func(r fantasy.ReasoningPart, id string) []fantasy.Message {
		return []fantasy.Message{
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{r,
				fantasy.ToolCallPart{ToolCallID: id, ToolName: "read_file", Input: `{"path":"x"}`}}},
			{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: id,
				Output: fantasy.ToolResultOutputContentText{Text: "x"}}}},
		}
	}
	prompt := fantasy.Prompt{fantasy.NewUserMessage("go")}
	prompt = append(prompt, step(sealedPart("rs_1", "sealed-openai", "an OpenAI summary"), "call_1")...)
	prompt = append(prompt, step(fantasy.ReasoningPart{Text: "an interrupted step's reasoning"}, "call_2")...)
	prompt = append(prompt, step(anthropicPart("unsigned", &anthropicThinking{}), "call_3")...)
	prompt = append(prompt, step(anthropicPart("signed", &anthropicThinking{Signature: "sig-kept"}), "call_4")...)
	prompt = append(prompt, step(anthropicPart("", &anthropicThinking{RedactedData: "redacted-kept"}), "call_5")...)
	prompt = append(prompt, fantasy.NewUserMessage("again"))

	srv := newRxServer(t, anAnswer("ok"))
	body := streamPrompt(t, srv.claudeModel(t, claude()), srv, prompt)
	msgs := anMessages(t, body)
	want := []string{"user:text",
		"assistant:tool_use", "user:tool_result", "assistant:tool_use", "user:tool_result", "assistant:tool_use", "user:tool_result",
		"assistant:thinking,tool_use", "user:tool_result", "assistant:redacted_thinking,tool_use", "user:tool_result,text"}
	if got := anShape(t, msgs); !slices.Equal(got, want) {
		t.Fatalf("request = %v, want %v", got, want)
	}
	if got := string(msgs[7].Content[0]); got != `{"type":"thinking","thinking":"signed","signature":"sig-kept"}` {
		t.Errorf("the signed block went as %s", got)
	}
	if got := string(msgs[9].Content[0]); got != `{"type":"redacted_thinking","data":"redacted-kept"}` {
		t.Errorf("the redacted block went as %s", got)
	}
	for _, leak := range []string{"sealed-openai", "an OpenAI summary", "interrupted", "unsigned"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("dropped reasoning was sent: %q", leak)
		}
	}
}

// TestMessagesNeverRunsACutOffCall (P1): a step that ends max_tokens or
// refusal with a complete tool call in it runs no tool and sends no second
// request — its finish is length or content-filter, which Fantasy never
// dispatches on — and a refusal's category, kept only as a short identifier,
// is in the finish's metadata.
func TestMessagesNeverRunsACutOffCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stop     string
		details  map[string]any
		finish   fantasy.FinishReason
		category string
	}{
		{"max_tokens", "max_tokens", nil, fantasy.FinishReasonLength, ""},
		{"refusal", "refusal", map[string]any{"type": "refusal", "category": "cyber", "explanation": "no"}, fantasy.FinishReasonContentFilter, "cyber"},
		{"refusal, no category", "refusal", map[string]any{"type": "refusal", "category": nil}, fantasy.FinishReasonContentFilter, ""},
		{"refusal, not an identifier", "refusal", map[string]any{"category": "Cyber Stuff"}, fantasy.FinishReasonContentFilter, ""},
		{"refusal, too long", "refusal", map[string]any{"category": strings.Repeat("a", 33)}, fantasy.FinishReasonContentFilter, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRxServer(t, anReply(nil, anStart(anUsage(30, 0, 0, 1)), anThinking(0, "hm", "sig"),
				anToolUse(1, "toolu_1", "read_file", `{"path":"a.txt"}`), anEnd(tc.stop, tc.details, 50)), anAnswer("never"))
			var mu sync.Mutex
			var seen []string
			agent := fantasy.NewAgent(srv.claudeNew(t, claude()), fantasy.WithTools(pathTool("read_file", &seen, &mu)), fantasy.WithMaxRetries(0))
			res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "read a"})
			if err != nil {
				t.Fatal(err)
			}
			if len(seen) != 0 || len(srv.requests()) != 1 {
				t.Fatalf("the tool ran for %v, %d requests: a cut-off call was run", seen, len(srv.requests()))
			}
			if res.Response.FinishReason != tc.finish {
				t.Fatalf("finish = %q, want %q", res.Response.FinishReason, tc.finish)
			}
			f, ok := AnthropicFinishOf(res.Steps[0].ProviderMetadata)
			if !ok || f.RefusalCategory != tc.category || f.RequestID != "req_test_0001" {
				t.Fatalf("finish metadata = %+v (%v), want category %q", f, ok, tc.category)
			}
		})
	}
}

// streamParts sends one step on lm with a one-message prompt and returns
// every part.
func streamParts(t *testing.T, lm fantasy.LanguageModel) []fantasy.StreamPart {
	t.Helper()
	stream, err := lm.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		t.Fatal(err)
	}
	var parts []fantasy.StreamPart
	for p := range stream {
		parts = append(parts, p)
	}
	return parts
}

// TestMessagesContextWindowFull (P16): a response that stops
// model_context_window_exceeded fails its step as context too large — after
// output, a MidStreamError that keeps it; before, a ProviderError — with its
// usage in an error finish first, and Fantasy does not retry it: the
// harness's overflow path compacts and sends again instead.
func TestMessagesContextWindowFull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks [][]string
		mid    bool
	}{
		{"after output", [][]string{anText(0, "partial answer")}, true},
		{"before output", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := append([][]string{anStart(anUsage(900000, 0, 0, 1))}, tc.blocks...)
			step = append(step, anEnd("model_context_window_exceeded", nil, 7))
			srv := newRxServer(t, anReply(nil, step...))
			parts := streamParts(t, srv.claudeNew(t, claude()))
			n := len(parts)
			if n < 2 || parts[n-2].Type != fantasy.StreamPartTypeFinish || parts[n-2].FinishReason != fantasy.FinishReasonError ||
				parts[n-2].Usage.InputTokens != 900000 || parts[n-1].Type != fantasy.StreamPartTypeError {
				t.Fatalf("the step ended %+v, want an error finish with the usage, then the error", parts[max(0, n-2):])
			}
			err := parts[n-1].Error
			tooLarge, ok := err.(interface{ IsContextTooLarge() bool })
			if !ok || !tooLarge.IsContextTooLarge() {
				t.Fatalf("err = %v (%T), want context too large", err, err)
			}
			var mse *MidStreamError
			var pe *fantasy.ProviderError
			switch {
			case tc.mid && !errors.As(err, &mse):
				t.Fatalf("err = %T, want a MidStreamError after output", err)
			case !tc.mid && (!errors.As(err, &pe) || pe.IsRetryable()):
				t.Fatalf("err = %v, want a ProviderError that is not retried", err)
			}

			srv = newRxServer(t, anReply(nil, step...), anAnswer("a retry"))
			agent := fantasy.NewAgent(srv.claudeNew(t, claude()), fantasy.WithMaxRetries(3))
			if _, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "go"}); err == nil || len(srv.requests()) != 1 {
				t.Fatalf("err = %v after %d requests, want a failure and no retry", err, len(srv.requests()))
			}
		})
	}
}

// TestMessagesRequestCounts (P2): what reaches Fantasy's agent through New —
// the wrapper included, retries on — is retried exactly when it should be,
// counted in requests the server saw: a 429, a 529, a 500 and an overloaded
// error event before any output are tried again, as is a 500 the server says
// to retry; a 400, a 401, a 403, a 404 and a 413 are not, even when the server
// says to (C2 review 1), nor is any failure the server marked x-should-retry:
// false — a *FinalError, whatever its status — nor a failure after output
// began. Every reply asks for a 1 ms wait, so a
// wrong retry costs nothing but a failed count.
func TestMessagesRequestCounts(t *testing.T) {
	wait := []string{"retry-after-ms", "1"}
	final := []string{"retry-after-ms", "1", "x-should-retry", "false"}
	retry := []string{"retry-after-ms", "1", "x-should-retry", "true"}
	for _, tc := range []struct {
		name     string
		reply    http.HandlerFunc
		requests int
		check    func(error) bool
	}{
		{"429", anJSON(429, anErrorBody("rate_limit_error", "slow down"), wait...), 2, isNil},
		{"529", anJSON(529, anErrorBody("overloaded_error", "Overloaded"), wait...), 2, isNil},
		{"500", anJSON(500, anErrorBody("api_error", "boom"), wait...), 2, isNil},
		{"overloaded before output", anReply(wait, anStart(anUsage(5, 0, 0, 1)), anErrorEvent("overloaded_error", "Overloaded")), 2, isNil},
		{"a 500 the server says to retry", anJSON(500, anErrorBody("api_error", "try again"), retry...), 2, isNil},
		{"a 400 the server says to retry", anJSON(400, anErrorBody("invalid_request_error", "try again"), retry...), 1, status(400)},
		{"a 401 the server says to retry", anJSON(401, anErrorBody("authentication_error", "try again"), retry...), 1, status(401)},
		{"a 403 the server says to retry", anJSON(403, anErrorBody("permission_error", "try again"), retry...), 1, status(403)},
		{"a 404 the server says to retry", anJSON(404, anErrorBody("not_found_error", "model: claude-test-5"), retry...), 1, status(404)},
		{"a 413 the server says to retry", anJSON(413, anErrorBody("request_too_large", "too large"), retry...), 1, status(413)},
		{"a refusal in a stream the server says to retry", anReply(retry, anStart(anUsage(5, 0, 0, 1)), anErrorEvent("permission_error", "no")), 1, status(403)},
		{"400", anJSON(400, anErrorBody("invalid_request_error", "bad"), wait...), 1, status(400)},
		{"401", anJSON(401, anErrorBody("authentication_error", "invalid x-api-key"), wait...), 1, func(err error) bool {
			var pe *fantasy.ProviderError
			return errors.As(err, &pe) && pe.AuthError && pe.StatusCode == 401
		}},
		{"403", anJSON(403, anErrorBody("permission_error", "no access"), wait...), 1, status(403)},
		{"500 marked final", anJSON(500, anErrorBody("api_error", "boom"), final...), 1, isFinal(500)},
		{"429 marked final", anJSON(429, anErrorBody("rate_limit_error", "slow down"), final...), 1, isFinal(429)},
		{"overloaded in a stream marked final", anReply(final, anStart(anUsage(5, 0, 0, 1)), anErrorEvent("overloaded_error", "Overloaded")), 1, isFinal(529)},
		{"overloaded after output", anReply(wait, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"), anErrorEvent("overloaded_error", "Overloaded")), 1, func(err error) bool {
			var mse *MidStreamError
			return errors.As(err, &mse) && mse.StatusCode == 529 && mse.Type == "overloaded_error"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRxServer(t, tc.reply, anAnswer("ok"), anAnswer("ok"), anAnswer("ok"))
			agent := fantasy.NewAgent(srv.claudeNew(t, claude()), fantasy.WithMaxRetries(3))
			_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi"})
			if n := len(srv.requests()); n != tc.requests {
				t.Fatalf("%d requests, want %d (err %v)", n, tc.requests, err)
			}
			if !tc.check(err) {
				t.Fatalf("err = %v (%T)", err, err)
			}
		})
	}
}

// anBlockOpen is a text block at idx begun with text and never stopped: a
// stream cut off inside it.
func anBlockOpen(idx int, text string) []string {
	return []string{anBlockStart(idx, map[string]any{"type": "text", "text": ""}), anDelta(idx, map[string]any{"type": "text_delta", "text": text})}
}

func isNil(err error) bool { return err == nil }

func status(code int) func(error) bool {
	return func(err error) bool {
		var pe *fantasy.ProviderError
		return errors.As(err, &pe) && pe.StatusCode == code && !pe.IsRetryable()
	}
}

func isFinal(code int) func(error) bool {
	return func(err error) bool {
		var fe *FinalError
		var pe *fantasy.ProviderError
		return errors.As(err, &fe) && fe.StatusCode == code && !errors.As(err, &pe) && !fantasy.IsTransportError(err)
	}
}

// TestMessagesCancellationStops: a turn cancelled while the model writes ends
// with the context's error, and nothing tries the request again.
func TestMessagesCancellationStops(t *testing.T) {
	done := make(chan struct{})
	blocking := func(w http.ResponseWriter, r *http.Request) {
		anReply([]string{"retry-after-ms", "1"}, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"))(w, r)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}
	srv := newRxServer(t, blocking, anAnswer("never"), anAnswer("never"))
	// Registered after the server's, so it runs first: a handler still
	// waiting is released before the server waits for it.
	t.Cleanup(func() { close(done) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := fantasy.NewAgent(srv.claudeNew(t, claude()), fantasy.WithMaxRetries(3))
	_, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:      "hi",
		OnTextDelta: func(string, string) error { cancel(); return nil },
	})
	if !errors.Is(err, context.Canceled) || len(srv.requests()) != 1 {
		t.Fatalf("err = %v after %d requests, want context.Canceled after 1", err, len(srv.requests()))
	}
}

// TestMessagesTransportAndTruncation: a stream that stops before it says how
// it ended, and a connection the server drops, are Fantasy's retryable
// failures before any output (counted retries would wait Fantasy's default
// backoff, so the verdict is read off the error).
func TestMessagesTransportAndTruncation(t *testing.T) {
	truncated := anReply(nil, anStart(anUsage(5, 0, 0, 1)))
	dropped := func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	for name, reply := range map[string]http.HandlerFunc{"truncated": truncated, "dropped": dropped} {
		t.Run(name, func(t *testing.T) {
			parts := streamParts(t, newRxServer(t, reply).claudeModel(t, claude()))
			err := parts[len(parts)-1].Error
			var pe *fantasy.ProviderError
			if !errors.As(err, &pe) || !pe.IsRetryable() {
				t.Fatalf("err = %v (%T), want a retryable ProviderError", err, err)
			}
		})
	}
}

// TestMessagesUsage (§3.3): input_tokens leaves the cache out, so Input,
// CacheRead and CacheCreation are each the API's own count, Output includes
// thinking, which Reasoning reports, and Total is their sum; through the
// agent, a turn's usage is its steps' summed, cache writes included.
func TestMessagesUsage(t *testing.T) {
	u := messagesapi.Usage{InputTokens: 120, CacheCreationInputTokens: 2000, CacheReadInputTokens: 3000, OutputTokens: 50, ThinkingTokens: 20}
	want := fantasy.Usage{InputTokens: 120, CacheCreationTokens: 2000, CacheReadTokens: 3000, OutputTokens: 50, ReasoningTokens: 20, TotalTokens: 5170}
	if got := messagesUsage(u); got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	// message_delta's usage repeats the input counts and carries the
	// thinking (LF-3); the reply here sends them there.
	end := []string{
		an(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_details": nil},
			"usage": map[string]any{"input_tokens": 120, "cache_creation_input_tokens": 2000, "cache_read_input_tokens": 3000,
				"output_tokens": 50, "output_tokens_details": map[string]any{"thinking_tokens": 20}}}),
		an(map[string]any{"type": "message_stop"}),
	}
	srv := newRxServer(t, anReply(nil, anStart(anUsage(120, 2000, 3000, 1)), anText(0, "hi"), end))
	agent := fantasy.NewAgent(srv.claudeNew(t, claude()), fantasy.WithMaxRetries(0))
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalUsage != want {
		t.Fatalf("the turn's usage = %+v, want %+v", res.TotalUsage, want)
	}
}

// TestMessagesSendsImages (§3.3 item 2): a user's image of a type the API
// reads is an image block; of another type, a placeholder; another file, a
// placeholder and a warning; past the request's 100 images, the oldest are
// placeholders and the newest 100 go. A tool's image goes, through the
// wrapper, after the run of results in the same user message — results first
// — and, without it, inside its result after the result's text.
func TestMessagesSendsImages(t *testing.T) {
	png := []byte("\x89PNG-test-bytes")
	b64 := base64.StdEncoding.EncodeToString(png)
	t.Run("types", func(t *testing.T) {
		srv := newRxServer(t, anAnswer("ok"))
		stream, err := srv.claudeModel(t, claude()).Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{
			fantasy.NewUserMessage("look", fantasy.FilePart{Data: png, MediaType: "image/png"}, fantasy.FilePart{Data: png, MediaType: "image/bmp"},
				fantasy.FilePart{Data: []byte("%PDF"), MediaType: "application/pdf"}, fantasy.FilePart{Data: png, MediaType: "bad\ntype"}),
		}})
		if err != nil {
			t.Fatal(err)
		}
		var warnings []fantasy.CallWarning
		for part := range stream {
			warnings = append(warnings, part.Warnings...)
		}
		content := anMessages(t, srv.requests()[0].body)[0].Content
		want := []string{
			`{"type":"text","text":"look"}`,
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}`,
			`{"type":"text","text":"[image omitted: image/bmp]"}`,
			`{"type":"text","text":"[file omitted: application/pdf]"}`,
			`{"type":"text","text":"[file omitted: an unknown type]"}`,
		}
		if got := rawStrings(content); !slices.Equal(got, want) {
			t.Fatalf("content =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if len(warnings) != 2 || !strings.Contains(warnings[0].Message, "application/pdf") {
			t.Fatalf("warnings = %+v, want one for each file", warnings)
		}
	})
	t.Run("over 100", func(t *testing.T) {
		parts := []fantasy.FilePart{}
		for i := range 101 {
			parts = append(parts, fantasy.FilePart{Data: fmt.Appendf(nil, "png-%03d", i), MediaType: "image/png"})
		}
		srv := newRxServer(t, anAnswer("ok"))
		body := streamPrompt(t, srv.claudeModel(t, claude()), srv, fantasy.Prompt{fantasy.NewUserMessage("many", parts...)})
		content := anMessages(t, body)[0].Content
		if got := string(content[1]); got != `{"type":"text","text":"[image omitted: image/png]"}` {
			t.Fatalf("the oldest image went as %s", got)
		}
		images := 0
		for _, raw := range content {
			if anBlock(t, raw)["type"] == "image" {
				images++
			}
		}
		last := anBlock(t, content[len(content)-1])["source"].(map[string]any)["data"]
		if images != 100 || last != base64.StdEncoding.EncodeToString([]byte("png-100")) {
			t.Fatalf("%d images sent, the last %v; want the newest 100", images, last)
		}
	})
	toolTurn := fantasy.Prompt{
		fantasy.NewUserMessage("read the screenshot"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "read_file", Input: `{}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call_1",
			Output: fantasy.ToolResultOutputContentMedia{Data: b64, MediaType: "image/png", Text: "Read image file: shot.png"}}}},
	}
	image := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}`
	t.Run("a tool's, wrapped", func(t *testing.T) {
		srv := newRxServer(t, anAnswer("ok"))
		body := streamPrompt(t, srv.claudeNew(t, claude()), srv, toolTurn)
		content := anMessages(t, body)[2].Content
		want := []string{
			`{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"Read image file: shot.png"}]}`,
			`{"type":"text","text":"Images returned by tool call(s) call_1:"}`,
			image,
		}
		if got := rawStrings(content); !slices.Equal(got, want) {
			t.Fatalf("content =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("a tool's, unwrapped", func(t *testing.T) {
		srv := newRxServer(t, anAnswer("ok"))
		body := streamPrompt(t, srv.claudeModel(t, claude()), srv, toolTurn)
		want := `{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"Read image file: shot.png"},` + image + `]}`
		if got := string(anMessages(t, body)[2].Content[0]); got != want {
			t.Fatalf("result = %s\nwant %s", got, want)
		}
	})
}

func rawStrings(raw []json.RawMessage) []string {
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = string(r)
	}
	return out
}

// TestMessagesShapesTheConversation (§3.3 item 5): system messages are the
// system blocks, blank ones dropped, the last with the cache breakpoint;
// messages of one role in a row are merged, a call's results first; a
// conversation that starts with the assistant's message opens with a
// placeholder of the user's; and one that ends with the assistant's is
// refused before anything is sent, as is one with nothing to send.
func TestMessagesShapesTheConversation(t *testing.T) {
	srv := newRxServer(t, anAnswer("ok"), anAnswer("ok"))
	lm := srv.claudeModel(t, claude())
	body := streamPrompt(t, lm, srv, fantasy.Prompt{
		fantasy.NewSystemMessage("You are craze."),
		fantasy.NewSystemMessage(" "),
		fantasy.NewSystemMessage("Be brief."),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Earlier, I said this."}}},
		fantasy.NewUserMessage("first"),
		fantasy.NewUserMessage("second"),
	})
	fields := fieldsOf(t, body)
	if got := string(fields["system"]); got != `[{"type":"text","text":"You are craze."},{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}}]` {
		t.Errorf("system = %s", got)
	}
	if got := string(fields["cache_control"]); got != `{"type":"ephemeral"}` {
		t.Errorf("cache_control = %s", got)
	}
	msgs := anMessages(t, body)
	if got, want := rawStrings(msgs[0].Content), []string{`{"type":"text","text":"(conversation continues)"}`}; msgs[0].Role != "user" || !slices.Equal(got, want) {
		t.Errorf("the first message = %s %v", msgs[0].Role, got)
	}
	if got, want := anShape(t, msgs), []string{"user:text", "assistant:text", "user:text,text"}; !slices.Equal(got, want) {
		t.Errorf("messages = %v, want %v", got, want)
	}

	// A user message held ahead of the results it shares a turn with goes
	// after them: a call's results open the message after it.
	body = streamPrompt(t, lm, srv, fantasy.Prompt{
		fantasy.NewUserMessage("q"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "read_file", Input: `{}`}}},
		fantasy.NewUserMessage("a steer"),
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call_1", Output: fantasy.ToolResultOutputContentText{Text: "x"}}}},
	})
	want := []string{`{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"x"}]}`, `{"type":"text","text":"a steer"}`}
	if got := rawStrings(anMessages(t, body)[2].Content); !slices.Equal(got, want) {
		t.Errorf("the merged message = %v, want %v", got, want)
	}

	for name, prompt := range map[string]fantasy.Prompt{
		"ends with the assistant's": {fantasy.NewUserMessage("q"), {Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "a prefill"}}}},
		"nothing to send":           {fantasy.NewSystemMessage("You are craze."), fantasy.NewUserMessage(" ")},
	} {
		if _, err := lm.Stream(context.Background(), fantasy.Call{Prompt: prompt}); err == nil {
			t.Errorf("%s: the prompt was not refused", name)
		}
	}
	if n := len(srv.requests()); n != 2 {
		t.Fatalf("%d requests, want the first two alone: a refused prompt was sent", n)
	}
}

// TestMessagesThinking (§3.3): a model whose thinking is adaptive asks for it
// with summarized display and drop_block, behind the binding beta; one with
// none sends neither the field nor the beta; an unknown thinking, a Resolved
// built outside the table, asks for adaptive; and any other value is refused
// when the model is built.
func TestMessagesThinking(t *testing.T) {
	const adaptive = `{"type":"adaptive","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`
	for _, tc := range []struct {
		thinking string
		want     string // the thinking field, "" for none
	}{
		{modeltable.ThinkingAdaptive, adaptive},
		{"", adaptive},
		{modeltable.ThinkingNone, ""},
	} {
		srv := newRxServer(t, anAnswer("ok"))
		r := claude()
		r.Thinking = tc.thinking
		req := streamPromptReq(t, srv.claudeModel(t, r), srv, fantasy.Prompt{fantasy.NewUserMessage("hi")})
		got := string(fieldsOf(t, req.body)["thinking"])
		beta := req.header.Get("Anthropic-Beta")
		if got != tc.want || (beta == messagesapi.BindingBeta) != (tc.want != "") {
			t.Errorf("thinking %q: field %s, beta %q; want %s", tc.thinking, got, beta, tc.want)
		}
	}
	r := claude()
	r.Thinking = "enabled"
	if _, err := newMessagesModel(r, anthropicKey, messagesapi.DefaultBaseURL, nil); err == nil {
		t.Fatal("a model with an unknown thinking was built")
	}
}

// streamPromptReq is streamPrompt's request whole.
func streamPromptReq(t *testing.T, lm fantasy.LanguageModel, srv *rxServer, prompt fantasy.Prompt) rxRequest {
	t.Helper()
	streamPrompt(t, lm, srv, prompt)
	reqs := srv.requests()
	return reqs[len(reqs)-1]
}

// TestMessagesEffortAndToolChoice (§3.3): each effort the API takes goes as
// output_config.effort, and none sends no output_config; a tool choice of
// none goes as itself, auto as nothing, and a forced one — a 400 on these
// models — as auto with a warning.
func TestMessagesEffortAndToolChoice(t *testing.T) {
	r := claude()
	for _, e := range append(slices.Clone(r.Efforts), "") {
		opts, err := EffortOptions(r, e)
		if err != nil {
			t.Fatalf("effort %q: %v", e, err)
		}
		srv := newRxServer(t, anAnswer("ok"))
		stream, err := srv.claudeModel(t, r).Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}, ProviderOptions: opts})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
		want := ""
		if e != "" {
			want = `{"effort":"` + e + `"}`
		}
		if got := string(fieldsOf(t, srv.requests()[0].body)["output_config"]); got != want {
			t.Errorf("effort %q: output_config = %s, want %s", e, got, want)
		}
	}
	tool := fantasy.FunctionTool{Name: "read_file", Description: "Read <a> file & more.", InputSchema: map[string]any{"type": "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}
	for _, tc := range []struct {
		choice  fantasy.ToolChoice
		want    string
		warning bool
	}{
		{fantasy.ToolChoiceAuto, "", false},
		{fantasy.ToolChoiceNone, `{"type":"none"}`, false},
		{fantasy.ToolChoiceRequired, "", true},
		{fantasy.SpecificToolChoice("read_file"), "", true},
	} {
		srv := newRxServer(t, anAnswer("ok"))
		choice := tc.choice
		stream, err := srv.claudeModel(t, r).Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")},
			Tools: []fantasy.Tool{tool}, ToolChoice: &choice})
		if err != nil {
			t.Fatal(err)
		}
		warned := false
		for part := range stream {
			warned = warned || part.Type == fantasy.StreamPartTypeWarnings
		}
		fields := fieldsOf(t, srv.requests()[0].body)
		if got := string(fields["tool_choice"]); got != tc.want || warned != tc.warning {
			t.Errorf("tool choice %q: tool_choice %s, warned %v; want %s, %v", tc.choice, got, warned, tc.want, tc.warning)
		}
		if got := string(fields["tools"]); got != `[{"name":"read_file","description":"Read <a> file & more.","input_schema":{"properties":{"path":{"type":"string"}},"required":["path"],"type":"object"}}]` {
			t.Errorf("tools = %s", got)
		}
	}
}

// TestNewBuildsTheAnthropicDriver (§3.3, P21): New builds Anthropic's driver
// on a key — the Messages model, wrapped — and its request goes to the API's
// own origin with nothing to say otherwise, and to the base the option
// answers when it does, with the key, the API's version and craze's user
// agent. The refusals: no key, a key too short to be real, a base that cannot
// be had (its own error, the test override's among them), and a base that is
// neither Anthropic's nor a loopback one.
func TestNewBuildsTheAnthropicDriver(t *testing.T) {
	srv := newRxServer(t, anAnswer("hi"), anAnswer("hi"))
	lm, err := New(claude(), WithHTTPClient(redirectClient(t, srv.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := lm.(*model); !ok {
		t.Fatalf("New = %T, want the wrapper", lm)
	} else if _, ok := w.inner.(*messagesModel); !ok {
		t.Fatalf("the wrapped model is %T, want the Messages model", w.inner)
	}
	if lm.Provider() != "anthropic" || lm.Model() != "claude-test-5" {
		t.Fatalf("Provider(), Model() = %q, %q", lm.Provider(), lm.Model())
	}
	resp, err := lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil || resp.Content.Text() != "hi" || resp.FinishReason != fantasy.FinishReasonStop {
		t.Fatalf("Generate = %+v, %v", resp, err)
	}
	if _, ok := AnthropicFinishOf(resp.ProviderMetadata); !ok {
		t.Fatal("Generate dropped the finish's metadata")
	}
	req := srv.requests()[0]
	if req.host != "api.anthropic.com" || req.path != "/v1/messages" || req.header.Get("X-Api-Key") != anthropicKey ||
		req.header.Get("Anthropic-Version") != "2023-06-01" || req.header.Get("User-Agent") != "craze" || req.header.Get("Authorization") != "" {
		t.Fatalf("request = %s%s %v", req.host, req.path, req.header)
	}
	lm = srv.claudeNew(t, claude())
	if _, err := lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}}); err != nil {
		t.Fatal(err)
	}
	if req := srv.requests()[1]; req.host != "" || req.path != "/v1/messages" {
		t.Fatalf("the option's base was not used: %s%s", req.host, req.path)
	}

	noKey := claude()
	noKey.APIKey = " "
	if _, err := New(noKey); !errors.Is(err, modeltable.ErrNoAPIKey) {
		t.Fatalf("no key: %v", err)
	}
	short := claude()
	short.APIKey = "sk-ant1"
	if _, err := New(short); !errors.Is(err, ErrAPIKeyTooShort) || strings.Contains(err.Error(), "sk-ant1") {
		t.Fatalf("a short key: %v", err)
	}
	broken := errors.New("test: the override is not a loopback URL")
	if _, err := New(claude(), WithAnthropicAPIBase(func() (string, error) { return "", broken })); !errors.Is(err, broken) {
		t.Fatalf("a base that cannot be had: %v", err)
	}
	override := func() (string, error) {
		return messagesapi.BaseURL(func(string) string { return "https://api.example.com" })
	}
	if _, err := New(claude(), WithAnthropicAPIBase(override)); !errors.Is(err, messagesapi.ErrOverride) {
		t.Fatalf("a test override for another host: %v", err)
	}
	if _, err := New(claude(), WithAnthropicAPIBase(func() (string, error) { return "https://api.example.com", nil })); !errors.Is(err, messagesapi.ErrBaseURL) {
		t.Fatalf("another host's base: %v, want ErrBaseURL", err)
	}
	m, err := newMessagesModel(claude(), anthropicKey, messagesapi.DefaultBaseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.GenerateObject(context.Background(), fantasy.ObjectCall{}); err == nil {
		t.Error("GenerateObject did something")
	}
	if _, err := m.StreamObject(context.Background(), fantasy.ObjectCall{}); err == nil {
		t.Error("StreamObject did something")
	}
	noWire := claude()
	noWire.WireModel = " "
	if _, err := newMessagesModel(noWire, anthropicKey, messagesapi.DefaultBaseURL, nil); err == nil {
		t.Error("a model with no wire model was built")
	}
}

// TestAnthropicEffortsMatchTheTable: the efforts EffortOptions sends on
// Anthropic's driver are the table's, and the levels the client reads from a
// model's capabilities.
func TestAnthropicEffortsMatchTheTable(t *testing.T) {
	if !slices.Equal(anthropicEfforts, modeltable.AnthropicEfforts()) || !slices.Equal(anthropicEfforts, messagesapi.EffortLevels()) {
		t.Fatalf("llm %q, modeltable %q, messagesapi %q", anthropicEfforts, modeltable.AnthropicEfforts(), messagesapi.EffortLevels())
	}
}

// TestAnthropicEffortOptions: an effort the model lists is the driver's
// options, under the provider id; one it does not list, or one the API
// cannot carry, is refused.
func TestAnthropicEffortOptions(t *testing.T) {
	r := claude()
	opts, err := EffortOptions(r, "xhigh")
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := opts["anthropic"].(*anthropicOptions); !ok || o.Effort != "xhigh" || len(opts) != 1 {
		t.Fatalf("options = %#v", opts)
	}
	low := r
	low.Efforts = []string{"low"}
	if _, err := EffortOptions(low, "high"); err == nil {
		t.Error("an effort the model does not list was accepted")
	}
	odd := r
	odd.Efforts = []string{"turbo"}
	if _, err := EffortOptions(odd, "turbo"); err == nil {
		t.Error("an effort no Anthropic request can carry was accepted")
	}
}

// TestAnthropicKeyNeverLeaks (P13): a server that echoes the key — in an HTTP
// error's message, JSON-escaped, in an error event before output and after
// it — never gets it into the error a turn ends with, by any reachable path;
// the echo did happen, and was scrubbed rather than lost (the marker is
// there).
func TestAnthropicKeyNeverLeaks(t *testing.T) {
	echo := "your key " + anthropicKey + " is not allowed"
	escaped := `{"type":"error","error":{"type":"authentication_error","message":"bad key ` + escapedEveryOther(anthropicKey) + `"}}`
	for _, tc := range []struct {
		name  string
		reply http.HandlerFunc
	}{
		{"HTTP 400", anJSON(400, anErrorBody("invalid_request_error", echo))},
		{"HTTP 401, escaped", anJSON(401, escaped)},
		{"an error event", anReply(nil, anStart(anUsage(5, 0, 0, 1)), anErrorEvent("invalid_request_error", echo))},
		{"an error event after output", anReply(nil, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"), anErrorEvent("invalid_request_error", echo))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRxServer(t, tc.reply)
			agent := fantasy.NewAgent(srv.claudeNew(t, claude()), fantasy.WithMaxRetries(0))
			_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi"})
			if err == nil {
				t.Fatal("the turn did not fail")
			}
			if l := leaks(err, anthropicKey); len(l) > 0 {
				t.Fatalf("the key is reachable from the error: %v", l)
			}
			if l := decodedLeaks(err, anthropicKey); len(l) > 0 {
				t.Fatalf("the key, decoded, is reachable from the error: %v", l)
			}
			if !strings.Contains(err.Error(), redacted) {
				t.Fatalf("err = %v, want the echo scrubbed in place", err)
			}
		})
	}
}
