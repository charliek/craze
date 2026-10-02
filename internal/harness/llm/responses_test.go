package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/charliek/craze/internal/harness/llm/responsesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The tests here run the ChatGPT plan's driver (plan 033 §3.9) — the adapter
// over package responsesapi — against a local server speaking the Responses
// stream in the shapes the spike captured, with a static token standing in
// for the sign-in (plan 033 C12: the driver lands inert, tested through this
// seam alone).

var updateGolden = flag.Bool("update", false, "rewrite internal/harness/llm/testdata/*.golden")

// The tokens the tests hand out: dummies, at least 8 bytes, that look like
// nothing real.
const (
	chatgptToken     = "test-chatgpt-token-0001"
	chatgptTokenNext = "test-chatgpt-token-0002"
)

// staticAuth is an Auth over fixed tokens: the first until it is
// invalidated, then the next.
type staticAuth struct {
	mu     sync.Mutex
	tokens []string
	gen    uint64
	err    error // Token's answer, when set
}

func newStaticAuth(tokens ...string) *staticAuth { return &staticAuth{tokens: tokens} }

func (a *staticAuth) Token(context.Context) (string, uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return "", 0, a.err
	}
	return a.tokens[min(int(a.gen), len(a.tokens)-1)], a.gen, nil
}

func (a *staticAuth) Invalidate(_ context.Context, gen uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen == a.gen {
		a.gen++
	}
	return nil
}

func (a *staticAuth) Values() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.tokens)
}

var _ Auth = (*staticAuth)(nil)

// chatgpt is a resolved model on the ChatGPT plan's driver, as the table
// will resolve one (plan 033 §3.11): no key, no base URL.
func chatgpt() modeltable.Resolved {
	return modeltable.Resolved{
		Alias:         "chatgpt/gpt-5.6-luna",
		ProviderID:    "chatgpt",
		Driver:        modeltable.DriverChatGPT,
		WireModel:     "gpt-5.6-luna",
		Name:          "GPT-5.6 Luna (ChatGPT plan)",
		ContextWindow: 272000,
		Efforts:       []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
		DefaultEffort: "medium",
		Vision:        true,
	}
}

// rxRequest is one request as the fake server saw it.
type rxRequest struct {
	host   string // the host the client aimed at
	path   string
	header http.Header
	body   []byte
}

// rxServer answers each request with the next queued reply.
type rxServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	replies []http.HandlerFunc
	reqs    []rxRequest
}

func newRxServer(t *testing.T, replies ...http.HandlerFunc) *rxServer {
	t.Helper()
	s := &rxServer{replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request body: %v", err)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, rxRequest{host: r.Header.Get("X-Original-Host"), path: r.URL.Path, header: r.Header.Clone(), body: body})
		var next http.HandlerFunc
		if len(s.replies) > 0 {
			next, s.replies = s.replies[0], s.replies[1:]
		}
		s.mu.Unlock()
		if next == nil {
			http.Error(w, "the test queued no reply for this request", http.StatusTeapot)
			return
		}
		next(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rxServer) requests() []rxRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// model is r's model on auth, its requests — aimed at api.openai.com —
// redirected to the server.
func (s *rxServer) model(t *testing.T, r modeltable.Resolved, auth Auth) *responsesModel {
	t.Helper()
	m, err := newResponsesModel(r, auth, redirectClient(t, s.srv.URL))
	if err != nil {
		t.Fatalf("newResponsesModel: %v", err)
	}
	return m
}

// rxEvents streams each event as the route does: event and data lines, and
// no Content-Type header.
func rxEvents(evs ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
		for _, e := range evs {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(e), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
		}
	}
}

func rxJSON(status int, body string, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func rx(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func rxAdded(idx int, item map[string]any) string {
	return rx(map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item})
}

func rxDone(idx int, item map[string]any) string {
	return rx(map[string]any{"type": "response.output_item.done", "output_index": idx, "item": item})
}

func rxMessage(id, text string) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
	}
	return map[string]any{"type": "message", "id": id, "role": "assistant", "content": content}
}

func rxReasoning(id, sealed string, summary ...string) map[string]any {
	parts := []any{}
	for _, s := range summary {
		parts = append(parts, map[string]any{"type": "summary_text", "text": s})
	}
	item := map[string]any{"type": "reasoning", "id": id, "summary": parts, "content": []any{}}
	if sealed != "" {
		item["encrypted_content"] = sealed
	}
	return item
}

func rxCall(id, callID, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": name, "namespace": "craze", "arguments": args}
}

func rxText(idx int, id, delta string) string {
	return rx(map[string]any{"type": "response.output_text.delta", "output_index": idx, "item_id": id, "delta": delta})
}

func rxSummary(idx int, id string, part int, delta string) string {
	return rx(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": idx, "item_id": id, "summary_index": part, "delta": delta})
}

func rxArgs(idx int, id, delta string) string {
	return rx(map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx, "item_id": id, "delta": delta})
}

func rxUsage(input, cached, output, reasoning int) map[string]any {
	return map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": cached, "cache_write_tokens": 0},
		"output_tokens":         output,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          input + output,
	}
}

func rxCompleted(input, cached, output, reasoning int) string {
	return rx(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_1", "status": "completed", "output": []any{}, "usage": rxUsage(input, cached, output, reasoning)}})
}

func rxFailed(code, message string, usage map[string]any) string {
	r := map[string]any{"id": "resp_1", "status": "failed", "output": []any{}, "error": map[string]any{"code": code, "message": message}}
	if usage != nil {
		r["usage"] = usage
	}
	return rx(map[string]any{"type": "response.failed", "response": r})
}

// rxAnswer is a one-message reply.
func rxAnswer(text string) http.HandlerFunc {
	return rxEvents(rxAdded(0, rxMessage("msg_a", "")), rxText(0, "msg_a", text), rxDone(0, rxMessage("msg_a", text)), rxCompleted(10, 0, 2, 0))
}

// input is a request body's input items, each decoded.
func input(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var req struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("a request body is not JSON: %v\n%s", err, body)
	}
	return req.Input
}

// itemTypes is the input's items as type/role (or type/call_id) strings.
func itemTypes(items []map[string]any) []string {
	var out []string
	for _, it := range items {
		s := fmt.Sprint(it["type"])
		switch {
		case it["role"] != nil:
			s += "/" + fmt.Sprint(it["role"])
		case it["call_id"] != nil:
			s += "/" + fmt.Sprint(it["call_id"])
		case it["id"] != nil:
			s += "/" + fmt.Sprint(it["id"])
		}
		out = append(out, s)
	}
	return out
}

// readTool is a tool that reports the path it was asked for.
func readTool(seen *[]string, mu *sync.Mutex) fantasy.AgentTool {
	type args struct {
		Path string `json:"path" description:"the file to read"`
	}
	return fantasy.NewParallelAgentTool("read_file", "Read a file.", func(_ context.Context, in args, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		mu.Lock()
		*seen = append(*seen, in.Path)
		mu.Unlock()
		return fantasy.NewTextResponse("contents of " + in.Path), nil
	})
}

// golden compares got with testdata/<name>, rewriting it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden: %v (run with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from what was sent:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// TestResponsesToolLoop runs one turn through Fantasy's agent — a reasoning
// step that calls a tool, then the answer — and pins both requests whole
// (testdata/responses_tool_loop.golden): the system prompt as instructions
// (no system item), the tools in the craze namespace, the effort and the
// reasoning summary, the encrypted reasoning included and, on the second
// request, replayed from the item's end with its id, before the call it led
// to; the call and its output by call_id. The output ceiling, temperature and
// top_p the call carries are never sent (plan 033 §3.9; D-74's ceiling is the
// harness's reserve alone here), and every request carries the call's
// session-id header and the bearer.
func TestResponsesToolLoop(t *testing.T) {
	srv := newRxServer(t,
		rxEvents(
			rxAdded(0, rxReasoning("rs_1", "sealed-partial")),
			rxSummary(0, "rs_1", 0, "Reading the file"),
			rxDone(0, rxReasoning("rs_1", "sealed-whole", "Reading the file")),
			rxAdded(1, rxCall("fc_1", "call_1", "read_file", "")),
			rxArgs(1, "fc_1", `{"path":"README.md"}`),
			rxDone(1, rxCall("fc_1", "call_1", "read_file", `{"path":"README.md"}`)),
			rxCompleted(130, 0, 253, 231),
		),
		rxEvents(
			rxAdded(0, rxMessage("msg_1", "")),
			rxText(0, "msg_1", "# craze"),
			rxDone(0, rxMessage("msg_1", "# craze")),
			rxCompleted(404, 128, 6, 0),
		),
	)
	r := chatgpt()
	lm := srv.model(t, r, newStaticAuth(chatgptToken))
	var mu sync.Mutex
	var seen []string
	agent := fantasy.NewAgent(lm, fantasy.WithSystemPrompt("You are craze."), fantasy.WithTools(readTool(&seen, &mu)), fantasy.WithMaxRetries(0))
	effort, err := EffortOptions(r, "high")
	if err != nil {
		t.Fatal(err)
	}
	ceiling, temperature, topP := int64(32000), 0.2, 0.9
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:          "Read README.md and give me its first line.",
		ProviderOptions: effort,
		Headers:         map[string]string{"session-id": "sess-1"},
		MaxOutputTokens: &ceiling,
		Temperature:     &temperature,
		TopP:            &topP,
	})
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if got := res.Response.Content.Text(); got != "# craze" {
		t.Fatalf("answer = %q", got)
	}
	if !slices.Equal(seen, []string{"README.md"}) {
		t.Fatalf("the tool was asked for %v", seen)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	var pinned bytes.Buffer
	for i, req := range reqs {
		if req.host != "api.openai.com" || req.path != "/v1/responses" {
			t.Errorf("request %d went to %s%s, want api.openai.com/v1/responses", i, req.host, req.path)
		}
		if got := req.header.Get("Session-Id"); got != "sess-1" {
			t.Errorf("request %d session-id = %q", i, got)
		}
		if got := req.header.Get("Authorization"); got != "Bearer "+chatgptToken {
			t.Errorf("request %d carries another bearer", i)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(req.body, &fields); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"max_output_tokens", "temperature", "top_p"} {
			if _, ok := fields[f]; ok {
				t.Errorf("request %d sends %s", i, f)
			}
		}
		if err := json.Indent(&pinned, req.body, "", "  "); err != nil {
			t.Fatal(err)
		}
		pinned.WriteString("\n")
	}
	golden(t, "responses_tool_loop.golden", pinned.Bytes())
	if strings.Contains(pinned.String(), "sealed-partial") {
		t.Error("the replay used the reasoning's start blob, not its end's")
	}

	// Usage: cached tokens are CacheRead and not Input as well; output
	// includes reasoning, which is reported too.
	want := fantasy.Usage{InputTokens: 130 + 404 - 128, OutputTokens: 253 + 6, TotalTokens: 383 + 410, ReasoningTokens: 231, CacheReadTokens: 128}
	if res.TotalUsage != want {
		t.Fatalf("usage = %+v, want %+v", res.TotalUsage, want)
	}
}

// TestResponsesKeepsParallelCallsApart: two calls in one step, their
// argument deltas interleaved and the second finishing first, run each with
// its own arguments, and the next request replays them in output_index
// order, each answered by its own call_id.
func TestResponsesKeepsParallelCallsApart(t *testing.T) {
	srv := newRxServer(t,
		rxEvents(
			rxAdded(0, rxCall("fc_a", "call_a", "read_file", "")),
			rxAdded(1, rxCall("fc_b", "call_b", "read_file", "")),
			rxArgs(0, "fc_a", `{"path":`),
			rxArgs(1, "fc_b", `{"path":`),
			rxArgs(1, "fc_b", `"b.txt"}`),
			rxArgs(0, "fc_a", `"a.txt"}`),
			rxDone(1, rxCall("fc_b", "call_b", "read_file", `{"path":"b.txt"}`)),
			rxDone(0, rxCall("fc_a", "call_a", "read_file", `{"path":"a.txt"}`)),
			rxCompleted(50, 0, 30, 0),
		),
		rxAnswer("both read"),
	)
	var mu sync.Mutex
	var seen []string
	agent := fantasy.NewAgent(srv.model(t, chatgpt(), newStaticAuth(chatgptToken)), fantasy.WithTools(readTool(&seen, &mu)), fantasy.WithMaxRetries(0))
	if _, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "read a and b"}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []string{"a.txt", "b.txt"}) {
		t.Fatalf("the tool ran for %v", seen)
	}
	items := input(t, srv.requests()[1].body)
	got := itemTypes(items)
	want := []string{"message/user", "function_call/call_a", "function_call/call_b", "function_call_output/call_a", "function_call_output/call_b"}
	if !slices.Equal(got, want) {
		t.Fatalf("second request's input = %v, want %v", got, want)
	}
	if items[1]["arguments"] != `{"path":"a.txt"}` || items[2]["arguments"] != `{"path":"b.txt"}` ||
		items[3]["output"] != "contents of a.txt" || items[4]["output"] != "contents of b.txt" {
		t.Fatalf("the calls' arguments or outputs crossed: %v", items[1:])
	}
}

// TestResponsesSendsTheCallsHeaders: the call's headers — the session-id
// the harness sets for this driver (P18) — go with the request, and a call
// with none sends none (the control).
func TestResponsesSendsTheCallsHeaders(t *testing.T) {
	srv := newRxServer(t, rxAnswer("ok"), rxAnswer("ok"))
	agent := fantasy.NewAgent(srv.model(t, chatgpt(), newStaticAuth(chatgptToken)), fantasy.WithMaxRetries(0))
	for _, h := range []map[string]string{{"session-id": "0199-session"}, nil} {
		if _, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi", Headers: h}); err != nil {
			t.Fatal(err)
		}
	}
	reqs := srv.requests()
	if got := reqs[0].header.Get("Session-Id"); got != "0199-session" {
		t.Errorf("session-id = %q, want the call's", got)
	}
	if got := reqs[1].header.Get("Session-Id"); got != "" {
		t.Errorf("a call with no headers sent session-id %q", got)
	}
}

// sealedPart is a reasoning part as this driver keeps one.
func sealedPart(id, sealed string, summary ...string) fantasy.ReasoningPart {
	md := &openai.ResponsesReasoningMetadata{ItemID: id, Summary: summary}
	if sealed != "" {
		md.EncryptedContent = &sealed
	}
	return fantasy.ReasoningPart{Text: strings.Join(summary, "\n\n"), ProviderOptions: fantasy.ProviderOptions{openai.Name: md}}
}

// streamPrompt sends one step with prompt and returns the request body.
func streamPrompt(t *testing.T, lm fantasy.LanguageModel, srv *rxServer, prompt fantasy.Prompt) []byte {
	t.Helper()
	stream, err := lm.Stream(context.Background(), fantasy.Call{Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			t.Fatalf("the step failed: %v", part.Error)
		}
	}
	reqs := srv.requests()
	return reqs[len(reqs)-1].body
}

// TestResponsesReplaysOnlySafeReasoning: a reasoning part is replayed only
// with its encrypted content and a legal id, and only when an output item of
// the same response follows it; every other one is dropped — with no blob
// the route has nothing to replay, an illegal id is gx's rule, and one that
// led to nothing (a step cut off after it) has nothing to pair with. The
// control: the one replayable part is replayed, with its summary.
func TestResponsesReplaysOnlySafeReasoning(t *testing.T) {
	srv := newRxServer(t, rxAnswer("ok"))
	lm := srv.model(t, chatgpt(), newStaticAuth(chatgptToken))
	call := func(id string) fantasy.ToolCallPart {
		return fantasy.ToolCallPart{ToolCallID: id, ToolName: "read_file", Input: `{"path":"x"}`}
	}
	result := func(id string) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: "x"}}}}
	}
	assistant := func(parts ...fantasy.MessagePart) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
	}
	prompt := fantasy.Prompt{
		fantasy.NewSystemMessage("You are craze."),
		fantasy.NewUserMessage("go"),
		assistant(sealedPart("rs_good", "sealed-good", "kept summary"), call("call_1")),
		result("call_1"),
		assistant(sealedPart("rs_bare", ""), call("call_2")),
		result("call_2"),
		assistant(sealedPart("rs bad!", "sealed-bad-id"), call("call_3")),
		result("call_3"),
		assistant(fantasy.ReasoningPart{Text: "an interrupted step's reasoning"}, call("call_4")),
		result("call_4"),
		assistant(fantasy.TextPart{Text: "a reply"}, sealedPart("rs_orphan", "sealed-orphan")),
		fantasy.NewUserMessage("again"),
	}
	items := input(t, streamPrompt(t, lm, srv, prompt))
	var replayed []string
	for _, it := range items {
		if it["type"] == "reasoning" {
			replayed = append(replayed, fmt.Sprint(it["id"]))
			if it["encrypted_content"] != "sealed-good" {
				t.Errorf("the replayed reasoning carries %v", it["encrypted_content"])
			}
			if s, _ := json.Marshal(it["summary"]); string(s) != `[{"text":"kept summary","type":"summary_text"}]` {
				t.Errorf("summary = %s", s)
			}
		}
		if it["type"] == "message" && it["role"] == "system" {
			t.Error("a system item was sent")
		}
	}
	if !slices.Equal(replayed, []string{"rs_good"}) {
		t.Fatalf("replayed reasoning %v, want only rs_good", replayed)
	}
	want := []string{"message/user", "reasoning/rs_good", "function_call/call_1", "function_call_output/call_1",
		"function_call/call_2", "function_call_output/call_2", "function_call/call_3", "function_call_output/call_3",
		"function_call/call_4", "function_call_output/call_4", "message/assistant", "message/user"}
	if got := itemTypes(items); !slices.Equal(got, want) {
		t.Fatalf("input = %v, want %v", got, want)
	}
}

// TestResponsesSendsImages: a user's image goes as an input_image data URL.
// A tool's image goes, through the wrapper as every model is built (rule 5),
// as user input_image after the run of outputs — the shape the route was
// seen to take — and, without the wrapper, as the output's own content parts.
func TestResponsesSendsImages(t *testing.T) {
	png := []byte("\x89PNG-test-bytes")
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	toolTurn := func() fantasy.Prompt {
		return fantasy.Prompt{
			fantasy.NewUserMessage("look at this", fantasy.FilePart{Data: png, MediaType: "image/png"}),
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call_1", ToolName: "read_file", Input: `{}`}}},
			{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call_1",
				Output: fantasy.ToolResultOutputContentMedia{Data: base64.StdEncoding.EncodeToString(png), MediaType: "image/png", Text: "Read image file: shot.png"}}}},
		}
	}
	t.Run("wrapped", func(t *testing.T) {
		srv := newRxServer(t, rxAnswer("ok"))
		lm := wrap(srv.model(t, chatgpt(), newStaticAuth(chatgptToken)), newScrubber(""))
		items := input(t, streamPrompt(t, lm, srv, toolTurn()))
		if got, want := itemTypes(items), []string{"message/user", "function_call/call_1", "function_call_output/call_1", "message/user"}; !slices.Equal(got, want) {
			t.Fatalf("input = %v, want %v", got, want)
		}
		user, _ := json.Marshal(items[0]["content"])
		if !strings.Contains(string(user), `"image_url":"`+dataURL+`"`) {
			t.Errorf("the user's image is not an input_image data URL: %s", user)
		}
		if out, ok := items[2]["output"].(string); !ok || out != "Read image file: shot.png" {
			t.Errorf("the tool's output = %v, want its text", items[2]["output"])
		}
		moved, _ := json.Marshal(items[3]["content"])
		if !strings.Contains(string(moved), `"image_url":"`+dataURL+`"`) || !strings.Contains(string(moved), "call_1") {
			t.Errorf("the tool's image did not follow the outputs as user input: %s", moved)
		}
	})
	t.Run("unwrapped", func(t *testing.T) {
		srv := newRxServer(t, rxAnswer("ok"))
		items := input(t, streamPrompt(t, srv.model(t, chatgpt(), newStaticAuth(chatgptToken)), srv, toolTurn()))
		out, _ := json.Marshal(items[2]["output"])
		want := `[{"text":"Read image file: shot.png","type":"input_text"},{"image_url":"` + dataURL + `","type":"input_image"}]`
		if string(out) != want {
			t.Fatalf("output = %s, want %s", out, want)
		}
	})
}

// TestResponsesEffort: EffortOptions carries each of the route's efforts to
// reasoning.effort, and refuses ultra (codex's multi-agent mode, which some
// models list) and anything the model does not list; a model with no effort
// control sends the summary alone.
func TestResponsesEffort(t *testing.T) {
	r := chatgpt()
	for _, e := range r.Efforts {
		srv := newRxServer(t, rxAnswer("ok"))
		opts, err := EffortOptions(r, e)
		if err != nil {
			t.Fatalf("effort %q: %v", e, err)
		}
		stream, err := srv.model(t, r, newStaticAuth(chatgptToken)).Stream(context.Background(),
			fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}, ProviderOptions: opts})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
		if got := string(fieldsOf(t, srv.requests()[0].body)["reasoning"]); got != `{"effort":"`+e+`","summary":"auto"}` {
			t.Errorf("effort %q: reasoning = %s", e, got)
		}
	}
	withUltra := r
	withUltra.Efforts = append(slices.Clone(r.Efforts), "ultra")
	if _, err := EffortOptions(withUltra, "ultra"); err == nil {
		t.Error("ultra was accepted")
	}
	if _, err := EffortOptions(r, "turbo"); err == nil {
		t.Error("an effort the model does not list was accepted")
	}

	none := r
	none.Efforts, none.DefaultEffort = nil, ""
	opts, err := EffortOptions(none, "")
	if err != nil || opts != nil {
		t.Fatalf("no effort: options %v, err %v", opts, err)
	}
	srv := newRxServer(t, rxAnswer("ok"))
	stream, err := srv.model(t, none, newStaticAuth(chatgptToken)).Stream(context.Background(),
		fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}, ProviderOptions: opts})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if got := string(fieldsOf(t, srv.requests()[0].body)["reasoning"]); got != `{"summary":"auto"}` {
		t.Errorf("no effort: reasoning = %s", got)
	}
}

func fieldsOf(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("a request body is not JSON: %v", err)
	}
	return m
}

// TestResponsesParallelToolCalls: parallel_tool_calls is the model's
// setting; nil leaves it to the server.
func TestResponsesParallelToolCalls(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	for _, tc := range []struct {
		set  *bool
		want string
	}{{nil, ""}, {boolPtr(true), "true"}, {boolPtr(false), "false"}} {
		srv := newRxServer(t, rxAnswer("ok"))
		r := chatgpt()
		r.ParallelToolCalls = tc.set
		agent := fantasy.NewAgent(srv.model(t, r, newStaticAuth(chatgptToken)), fantasy.WithTools(readTool(&seen, &mu)), fantasy.WithMaxRetries(0))
		if _, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi"}); err != nil {
			t.Fatal(err)
		}
		if got := string(fieldsOf(t, srv.requests()[0].body)["parallel_tool_calls"]); got != tc.want {
			t.Errorf("ParallelToolCalls %v: parallel_tool_calls = %q, want %q", tc.set, got, tc.want)
		}
	}
}

// countingAgent runs one turn with Fantasy's own retries on (three), the
// way a 429 would be retried if the driver let it, and reports the error.
func countingAgent(t *testing.T, srv *rxServer) error {
	t.Helper()
	agent := fantasy.NewAgent(srv.model(t, chatgpt(), newStaticAuth(chatgptToken)), fantasy.WithMaxRetries(3))
	_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi"})
	return err
}

// TestResponsesFinalFailuresAreNeverRetried: every 429 — a usage limit, any
// other, with or without a Retry-After — every 400, the codes OpenAI's docs
// say not to repeat, and a usage limit arriving inside the stream after
// output began are a *FinalError, and Fantasy's agent, whose retries are on
// and which retries every 429 it sees as one, sends the request once. The
// controls: a 503 and an incomplete stream before any output are retried.
func TestResponsesFinalFailuresAreNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  http.HandlerFunc
		status int
		code   string
	}{
		{"429 usage limit", rxJSON(429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"You've hit your usage limit."}}`, "retry-after-ms", "1"),
			429, "subscription_sharing_usage_limit_exceeded"},
		{"429 with a detail", rxJSON(429, `{"detail":"Too Many Requests"}`, "retry-after", "0"), 429, ""},
		{"429 rate limit", rxJSON(429, `{"error":{"code":"rate_limit_exceeded","message":"slow down","type":"requests"}}`), 429, "rate_limit_exceeded"},
		{"400 detail", rxJSON(400, `{"detail":"System messages are not allowed"}`), 400, ""},
		{"400 unsupported capability", rxJSON(400, `{"error":{"code":"subscription_sharing_unsupported_capability","param":"tools","message":"not supported"}}`),
			400, "subscription_sharing_unsupported_capability"},
		{"403 not eligible", rxJSON(403, `{"error":{"code":"subscription_sharing_user_not_eligible","message":"not eligible"}}`), 403, "subscription_sharing_user_not_eligible"},
		{"403 chatpass", rxJSON(403, `{"error":{"code":"chatpass_v2_scope_not_authorized","message":"scope"}}`), 403, "chatpass_v2_scope_not_authorized"},
		{"usage limit inside the stream", rxEvents(rxAdded(0, rxMessage("msg_1", "")), rxText(0, "msg_1", "partial"),
			rxFailed("subscription_sharing_usage_limit_exceeded", "You've hit your usage limit.", rxUsage(100, 0, 3, 0))),
			0, "subscription_sharing_usage_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRxServer(t, tc.reply, rxAnswer("a retry"), rxAnswer("a retry"), rxAnswer("a retry"))
			err := countingAgent(t, srv)
			var fe *FinalError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v (%T), want a *FinalError", err, err)
			}
			if fe.StatusCode != tc.status || fe.Code != tc.code {
				t.Fatalf("final error = %+v, want status %d code %q", fe, tc.status, tc.code)
			}
			if n := len(srv.requests()); n != 1 {
				t.Fatalf("%d requests, want 1: a final failure was retried", n)
			}
			var pe *fantasy.ProviderError
			if errors.As(err, &pe) || fantasy.IsTransportError(err) {
				t.Fatalf("the final error reads as retryable: %v", err)
			}
		})
	}
	t.Run("detail text", func(t *testing.T) {
		srv := newRxServer(t, rxJSON(400, `{"detail":"Unsupported parameter: max_output_tokens"}`))
		var fe *FinalError
		if err := countingAgent(t, srv); !errors.As(err, &fe) || fe.Message != "Unsupported parameter: max_output_tokens" ||
			!strings.Contains(fe.Error(), "Unsupported parameter: max_output_tokens") {
			t.Fatalf("err = %v, want the detail's text", err)
		}
	})
	t.Run("the transport phrases are defused", func(t *testing.T) {
		fe := &FinalError{StatusCode: 429, Message: "stream error: connection error: x"}
		if fantasy.IsTransportError(fe) {
			t.Fatalf("%q reads as a transport error", fe.Error())
		}
	})
	t.Run("503 is retried", func(t *testing.T) {
		srv := newRxServer(t, rxJSON(503, `{"detail":"unavailable"}`, "retry-after-ms", "1"), rxAnswer("ok"))
		if err := countingAgent(t, srv); err != nil {
			t.Fatalf("err = %v", err)
		}
		if n := len(srv.requests()); n != 2 {
			t.Fatalf("%d requests, want 2", n)
		}
	})
}

// stepError streams one step on a fresh model and returns its parts and the
// error part's error, failing when the step ends without one.
func stepError(t *testing.T, srv *rxServer) ([]fantasy.StreamPart, error) {
	t.Helper()
	stream, err := srv.model(t, chatgpt(), newStaticAuth(chatgptToken)).Stream(context.Background(),
		fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
		if part.Type == fantasy.StreamPartTypeError {
			return parts, part.Error
		}
	}
	t.Fatal("the step ended without an error")
	return nil, nil
}

// TestResponsesRetryableFailures: what may be tried again before any output
// (D-32) reaches Fantasy as a retryable *fantasy.ProviderError — a usage or
// account check that could not run, a server error inside the stream, a
// stream that stopped before saying how it ended — and an incomplete
// response does not: the same request would stop the same way. Each keeps
// what the scrubber reads its code from (the error object in the body), and
// a failed or incomplete response's usage reaches OnStreamFinish in a Finish
// part before the error.
func TestResponsesRetryableFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     http.HandlerFunc
		retryable bool
		code      string
		usage     bool
	}{
		{"usage unavailable", rxEvents(rxFailed("subscription_sharing_usage_unavailable", "try later", nil)), true, "subscription_sharing_usage_unavailable", false},
		{"user unavailable", rxEvents(rxFailed("subscription_sharing_user_unavailable", "try later", rxUsage(5, 0, 0, 0))), true, "subscription_sharing_user_unavailable", true},
		{"server error", rxEvents(rx(map[string]any{"type": "error", "code": "server_error", "message": "boom"})), true, "server_error", false},
		{"incomplete", rxEvents(rxAdded(0, rxMessage("msg_1", "")), rx(map[string]any{"type": "response.incomplete", "response": map[string]any{
			"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "usage": rxUsage(100, 0, 4000, 3990)}})),
			false, "max_output_tokens", true},
		{"a 403 for a region", rxJSON(403, `{"detail":"Country, region, or territory not supported"}`), false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := stepError(t, newRxServer(t, tc.reply))
			var pe *fantasy.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want a *fantasy.ProviderError", err, err)
			}
			if pe.IsRetryable() != tc.retryable {
				t.Fatalf("retryable = %v, want %v (%+v)", pe.IsRetryable(), tc.retryable, pe)
			}
			if code, _ := structuredError(pe.ResponseBody); code != tc.code {
				t.Fatalf("code in the body = %q, want %q (%s)", code, tc.code, pe.ResponseBody)
			}
			hasUsage := slices.ContainsFunc(parts, func(p fantasy.StreamPart) bool {
				return p.Type == fantasy.StreamPartTypeFinish && p.FinishReason == fantasy.FinishReasonError && p.Usage.TotalTokens > 0
			})
			if hasUsage != tc.usage {
				t.Fatalf("usage kept in a Finish part: %v, want %v", hasUsage, tc.usage)
			}
		})
	}
	t.Run("the stream stops short", func(t *testing.T) {
		_, err := stepError(t, newRxServer(t, rxEvents(rxAdded(0, rxMessage("msg_1", "")), rxText(0, "msg_1", "half"))))
		var pe *fantasy.ProviderError
		if !errors.As(err, &pe) || !pe.IsRetryable() || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want Fantasy's retryable incomplete-stream error", err)
		}
	})
	t.Run("a 401 after its renewal", func(t *testing.T) {
		_, err := stepError(t, newRxServer(t, rxJSON(401, `{"detail":"Unauthorized"}`), rxJSON(401, `{"detail":"Unauthorized"}`)))
		var pe *fantasy.ProviderError
		if !errors.As(err, &pe) || pe.StatusCode != 401 || !pe.AuthError || pe.IsRetryable() {
			t.Fatalf("err = %v, want a non-retryable auth failure", err)
		}
	})
}

// TestResponsesCredentialErrorsKeepTheirSentinel: the sign-in's own errors
// (signed out, sign in again) reach the harness as themselves.
func TestResponsesCredentialErrorsKeepTheirSentinel(t *testing.T) {
	signedOut := errors.New("chatgptauth: signed out")
	srv := newRxServer(t, rxAnswer("never sent"))
	auth := newStaticAuth(chatgptToken)
	auth.err = signedOut
	stream, err := srv.model(t, chatgpt(), auth).Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			got = part.Error
		}
	}
	if !errors.Is(got, signedOut) || len(srv.requests()) != 0 {
		t.Fatalf("err = %v after %d requests, want the sign-in's error and none", got, len(srv.requests()))
	}
}

// TestResponsesUsage: the API's input_tokens includes the cached tokens, so
// Input is the rest and CacheRead the cached — the context sum counts each
// once (autocompact.go) — while Output includes reasoning, which is also
// reported. A server's cached count above its input is held to it.
func TestResponsesUsage(t *testing.T) {
	for _, tc := range []struct {
		in   responsesapi.Usage
		want fantasy.Usage
	}{
		{responsesapi.Usage{InputTokens: 3054, CachedTokens: 2816, OutputTokens: 5, TotalTokens: 3059},
			fantasy.Usage{InputTokens: 238, CacheReadTokens: 2816, OutputTokens: 5, TotalTokens: 3059}},
		{responsesapi.Usage{InputTokens: 130, OutputTokens: 253, ReasoningTokens: 231, TotalTokens: 383},
			fantasy.Usage{InputTokens: 130, OutputTokens: 253, ReasoningTokens: 231, TotalTokens: 383}},
		{responsesapi.Usage{InputTokens: 10, CachedTokens: 50, CacheWriteTokens: 4, OutputTokens: 1},
			fantasy.Usage{InputTokens: 0, CacheReadTokens: 10, OutputTokens: 1, TotalTokens: 11}},
	} {
		if got := fantasyUsage(tc.in); got != tc.want {
			t.Errorf("%+v: usage = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// TestResponsesReasoningKeepsItsStoredShape: the reasoning part a step
// leaves in history carries Fantasy's own openai.ResponsesReasoningMetadata
// under openai.Name — the shape every craze transcript can read back
// (store/golden_test.go TestReasoningMetadataSurvives) — and survives the
// JSON round trip the transcript makes, blob and all, so the next request
// can replay it.
func TestResponsesReasoningKeepsItsStoredShape(t *testing.T) {
	srv := newRxServer(t, rxEvents(
		rxAdded(0, rxReasoning("rs_1", "partial")),
		rxSummary(0, "rs_1", 0, "Thinking"),
		rxDone(0, rxReasoning("rs_1", "sealed-whole", "Thinking")),
		rxAdded(1, rxMessage("msg_1", "")),
		rxText(1, "msg_1", "done"),
		rxDone(1, rxMessage("msg_1", "done")),
		rxCompleted(10, 0, 5, 3),
	))
	agent := fantasy.NewAgent(srv.model(t, chatgpt(), newStaticAuth(chatgptToken)), fantasy.WithMaxRetries(0))
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "think"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := res.Steps[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("the step left %d messages", len(msgs))
	}
	raw, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"openai.responses.reasoning_metadata"`) {
		t.Fatalf("the stored reasoning is not Fantasy's Responses metadata:\n%s", raw)
	}
	var back fantasy.Message
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	r, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](back.Content[0])
	if !ok || r.Text != "Thinking" {
		t.Fatalf("first part = %#v, want the reasoning", back.Content[0])
	}
	md := openai.GetReasoningMetadata(r.ProviderOptions)
	if md == nil || md.ItemID != "rs_1" || md.EncryptedContent == nil || *md.EncryptedContent != "sealed-whole" || !slices.Equal(md.Summary, []string{"Thinking"}) {
		t.Fatalf("metadata = %+v", md)
	}
	if _, ok := replayable(r); !ok {
		t.Fatal("the stored reasoning is not replayable")
	}
}

// TestResponsesGenerateCollectsTheStream: the route only streams, so
// Generate is one streamed step collected — in output order, the usage and
// the finish with it.
func TestResponsesGenerateCollectsTheStream(t *testing.T) {
	srv := newRxServer(t, rxEvents(
		rxAdded(0, rxReasoning("rs_1", "")),
		rxSummary(0, "rs_1", 0, "hm"),
		rxDone(0, rxReasoning("rs_1", "sealed", "hm")),
		rxAdded(1, rxMessage("msg_1", "")),
		rxText(1, "msg_1", "calling"),
		rxDone(1, rxMessage("msg_1", "calling")),
		rxAdded(2, rxCall("fc_1", "call_1", "read_file", "")),
		rxDone(2, rxCall("fc_1", "call_1", "read_file", `{"path":"a"}`)),
		rxCompleted(20, 0, 7, 2),
	), rxJSON(429, `{"detail":"Too Many Requests"}`))
	lm := srv.model(t, chatgpt(), newStaticAuth(chatgptToken))
	resp, err := lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, c := range resp.Content {
		kinds = append(kinds, string(c.GetType()))
	}
	if !slices.Equal(kinds, []string{"reasoning", "text", "tool-call"}) || resp.Content.Text() != "calling" {
		t.Fatalf("content = %v (%q)", kinds, resp.Content.Text())
	}
	if resp.FinishReason != fantasy.FinishReasonToolCalls || resp.Usage.TotalTokens != 27 {
		t.Fatalf("finish %q usage %+v", resp.FinishReason, resp.Usage)
	}
	var fe *FinalError
	if _, err := lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}}); !errors.As(err, &fe) {
		t.Fatalf("a 429 in Generate: err = %v, want a *FinalError", err)
	}
}

// TestNewResponsesModelRefusesWhatCannotBeBuilt: no sign-in, no wire model.
func TestNewResponsesModelRefusesWhatCannotBeBuilt(t *testing.T) {
	if _, err := newResponsesModel(chatgpt(), nil, nil); err == nil {
		t.Error("a model with no Auth was built")
	}
	r := chatgpt()
	r.WireModel = " "
	if _, err := newResponsesModel(r, newStaticAuth(chatgptToken), nil); err == nil {
		t.Error("a model with no wire model was built")
	}
	m, err := newResponsesModel(chatgpt(), newStaticAuth(chatgptToken), nil)
	if err != nil || m.Provider() != "chatgpt" || m.Model() != "gpt-5.6-luna" {
		t.Fatalf("model %v, err %v", m, err)
	}
	if _, err := m.GenerateObject(context.Background(), fantasy.ObjectCall{}); err == nil {
		t.Error("GenerateObject did something")
	}
	if _, err := m.StreamObject(context.Background(), fantasy.ObjectCall{}); err == nil {
		t.Error("StreamObject did something")
	}
}

// The factory does not build the driver yet: it lands inert (plan 033 C12).
func TestNewDoesNotBuildTheChatGPTDriverYet(t *testing.T) {
	if _, err := New(chatgpt()); err == nil || !strings.Contains(err.Error(), "unknown driver") {
		t.Fatalf("New(chatgpt) = %v, want the unknown-driver refusal until the sign-in lands", err)
	}
}

func boolPtr(v bool) *bool { return &v }
