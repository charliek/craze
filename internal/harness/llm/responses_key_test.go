package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm/responsesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The openai-responses driver (plan 047 D12, D13): the adapter on the
// client's key route, built by New, against a loopback server speaking the
// stream in Meta's shapes (the plan's live facts, MR-1…16). The key is the
// package's dummy canary.

// keyRoute is a resolved model on the openai-responses driver at base, as
// the table resolves one: Meta's Wonder, its key the canary.
func keyRoute(base string) modeltable.Resolved {
	return modeltable.Resolved{
		Alias:           "meta/muse-wonder-1.0",
		ProviderID:      "meta",
		Driver:          modeltable.DriverOpenAIResponses,
		BaseURL:         base,
		APIKey:          canary,
		WireModel:       "muse-wonder-1.0",
		Name:            "Muse Wonder 1.0",
		ContextWindow:   262144,
		MaxOutputTokens: 131072,
		Efforts:         []string{"minimal", "low", "medium", "high", "xhigh", "max"},
		DefaultEffort:   "high",
		Vision:          true,
	}
}

// keyModel is keyRoute's model aimed at the server, built by New.
func (s *rxServer) keyModel(t *testing.T) fantasy.LanguageModel {
	t.Helper()
	lm, err := New(keyRoute(s.srv.URL + "/v1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return lm
}

// Meta's shapes: a reasoning item whose summary is empty (MR-2), its
// encrypted content only at its end (MR-10); a message with a phase and an
// rs_ id (MR-5); a call with no namespace; and response.completed carrying
// the whole output again (MR-10).

func metaReasoning(id, sealed string) map[string]any {
	item := map[string]any{"type": "reasoning", "id": id, "summary": []any{}, "encrypted_content": nil}
	if sealed != "" {
		item["encrypted_content"] = sealed
	}
	return item
}

func metaMessage(id, text string) map[string]any {
	m := rxMessage(id, text)
	m["phase"] = "commentary"
	return m
}

func metaCall(id, callID, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": name, "arguments": args}
}

func metaCompleted(input, cached, output, reasoning int, items ...map[string]any) string {
	out := []any{}
	for _, it := range items {
		out = append(out, it)
	}
	return rx(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_1", "status": "completed", "store": false, "output": out, "usage": rxUsage(input, cached, output, reasoning)}})
}

// metaAnswer is a one-message reply in Meta's shapes.
func metaAnswer(text string) http.HandlerFunc {
	return rxEvents(rxAdded(0, metaMessage("rs_ans", "")), rxText(0, "rs_ans", text), rxDone(0, metaMessage("rs_ans", text)),
		metaCompleted(10, 0, 2, 0, metaMessage("rs_ans", text)))
}

// TestKeyRouteToolLoop (A12) runs one turn through Fantasy's agent on the
// openai-responses driver — a reasoning step with a commentary message that
// calls a tool, then the answer — and pins both requests whole
// (testdata/responses_key_tool_loop.golden): every tool a plain function,
// max_output_tokens the call's ceiling, prompt_cache_key the call's key, the
// effort, and on the second request step 1's reasoning replayed with its
// colon id and encrypted content, the message without its phase or id, the
// call without a namespace. Each request goes to <base>/responses with the
// key as its bearer and no session-id header; temperature and top_p are never
// sent; and the effort options the key was added to are as they were.
func TestKeyRouteToolLoop(t *testing.T) {
	rs := "rs_6acd987e3:rs_01a1331bc4"
	step1 := []map[string]any{metaReasoning(rs, "sealed-1426"), metaMessage("rs_01m1", "I'll read README.md."),
		metaCall("fc_01", "call_01", "read_file", `{"path":"README.md"}`)}
	srv := newRxServer(t,
		rxEvents(
			rxAdded(0, metaReasoning(rs, "")),
			rxDone(0, step1[0]),
			rxAdded(1, metaMessage("rs_01m1", "")),
			rxText(1, "rs_01m1", "I'll read README.md."),
			rxDone(1, step1[1]),
			rxAdded(2, metaCall("fc_01", "call_01", "read_file", "")),
			rxArgs(2, "fc_01", `{"path":"README.md"}`),
			rxDone(2, step1[2]),
			metaCompleted(550, 113, 97, 27, step1...),
		),
		rxEvents(
			rxAdded(0, metaMessage("rs_02m1", "")),
			rxText(0, "rs_02m1", "# craze"),
			rxDone(0, metaMessage("rs_02m1", "# craze")),
			metaCompleted(658, 497, 24, 10, metaMessage("rs_02m1", "# craze")),
		),
	)
	r := keyRoute(srv.srv.URL + "/v1")
	lm := srv.keyModel(t)
	var mu sync.Mutex
	var seen []string
	agent := fantasy.NewAgent(lm, fantasy.WithSystemPrompt("You are craze."), fantasy.WithTools(readTool(&seen, &mu)), fantasy.WithMaxRetries(0))
	effort, err := EffortOptions(r, "high")
	if err != nil {
		t.Fatal(err)
	}
	ceiling, temperature, topP := int64(131072), 0.2, 0.9
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:          "Read README.md and give me its first line.",
		ProviderOptions: WithCacheKey(r, effort, "0199c0de-session"),
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
		if req.path != "/v1/responses" {
			t.Errorf("request %d went to %s", i, req.path)
		}
		if req.header.Get("Authorization") != "Bearer "+canary {
			t.Errorf("request %d carries another bearer", i)
		}
		if got := req.header.Get("Session-Id"); got != "" {
			t.Errorf("request %d sent session-id %q", i, got)
		}
		f := fieldsOf(t, req.body)
		for _, name := range []string{"temperature", "top_p"} {
			if _, ok := f[name]; ok {
				t.Errorf("request %d sends %s", i, name)
			}
		}
		if string(f["max_output_tokens"]) != "131072" || string(f["prompt_cache_key"]) != `"0199c0de-session"` {
			t.Errorf("request %d: max_output_tokens %s, prompt_cache_key %s", i, f["max_output_tokens"], f["prompt_cache_key"])
		}
		if bytes.Contains(req.body, []byte("namespace")) {
			t.Errorf("request %d holds a namespace", i)
		}
		if err := json.Indent(&pinned, req.body, "", "  "); err != nil {
			t.Fatal(err)
		}
		pinned.WriteString("\n")
	}
	golden(t, "responses_key_tool_loop.golden", pinned.Bytes())
	items := input(t, reqs[1].body)
	want := []string{"message/user", "reasoning/" + rs, "message/assistant", "function_call/call_01", "function_call_output/call_01"}
	if got := itemTypes(items); !slices.Equal(got, want) {
		t.Fatalf("the second request's input = %v, want %v", got, want)
	}
	if items[1]["encrypted_content"] != "sealed-1426" || items[2]["phase"] != nil || items[2]["id"] != nil {
		t.Fatalf("replayed: reasoning %v, message %v", items[1], items[2])
	}
	if o := effort[r.ProviderID].(*responsesOptions); o.CacheKey != "" || o.Effort != "high" {
		t.Fatalf("the effort options were changed: %+v", o)
	}
	// One usage per response: response.completed's output, Meta's, was
	// never read for items, and its usage counted once.
	wantUsage := fantasy.Usage{InputTokens: 550 - 113 + 658 - 497, OutputTokens: 97 + 24, TotalTokens: 647 + 682, ReasoningTokens: 27 + 10, CacheReadTokens: 113 + 497}
	if res.TotalUsage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", res.TotalUsage, wantUsage)
	}
}

// TestKeyRouteReplaysColonIDs (A12, MR-3): a reasoning part with Meta's
// colon id is replayed on the key route, and still dropped on the ChatGPT
// route (the control, gx's rule). On both, an id with other characters, or
// one past 256 bytes, is dropped.
func TestKeyRouteReplaysColonIDs(t *testing.T) {
	assistant := func(parts ...fantasy.MessagePart) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
	}
	call := func(id string) fantasy.ToolCallPart {
		return fantasy.ToolCallPart{ToolCallID: id, ToolName: "read_file", Input: `{"path":"x"}`}
	}
	result := func(id string) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: "x"}}}}
	}
	long := "rs_" + strings.Repeat("a", 254)
	prompt := fantasy.Prompt{
		fantasy.NewUserMessage("go"),
		assistant(sealedPart("rs_6acd:rs_01a1", "sealed-colon"), call("call_1")),
		result("call_1"),
		assistant(sealedPart("rs bad!", "sealed-bad"), call("call_2")),
		result("call_2"),
		assistant(sealedPart(long, "sealed-long"), call("call_3")),
		result("call_3"),
		fantasy.NewUserMessage("again"),
	}
	replayed := func(body []byte) []string {
		var ids []string
		for _, it := range input(t, body) {
			if it["type"] == "reasoning" {
				ids = append(ids, fmt.Sprint(it["id"]))
			}
		}
		return ids
	}
	key := newRxServer(t, metaAnswer("ok"))
	if got := replayed(streamPrompt(t, key.keyModel(t), key, prompt)); !slices.Equal(got, []string{"rs_6acd:rs_01a1"}) {
		t.Fatalf("the key route replayed %v, want only the colon id", got)
	}
	chat := newRxServer(t, rxAnswer("ok"))
	if got := replayed(streamPrompt(t, chat.model(t, chatgpt(), newStaticAuth(chatgptToken)), chat, prompt)); len(got) != 0 {
		t.Fatalf("the ChatGPT route replayed %v, want none", got)
	}
}

// keyStep streams one step on the server's key-route model, built by New and
// so wrapped, and returns its parts and the error part's error, failing when
// the step ends without one.
func keyStep(t *testing.T, srv *rxServer) ([]fantasy.StreamPart, error) {
	t.Helper()
	stream, err := srv.keyModel(t).Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
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

// keyTurn runs one turn on the server's key-route model with Fantasy's own
// retries on (three), recording each retry's wait, and reports the error.
func keyTurn(t *testing.T, srv *rxServer) ([]time.Duration, error) {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	agent := fantasy.NewAgent(srv.keyModel(t), fantasy.WithMaxRetries(3))
	_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi", OnRetry: func(_ *fantasy.ProviderError, d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
	}})
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(waits), err
}

// TestKeyRouteHTTPFailures (A12, P19): through Fantasy's agent, its retries
// on, a 401 is sent once whatever its headers or body say — x-should-retry,
// a wait, a transient code — an auth failure; a 400, even with
// x-should-retry, a 403 and a 404 are sent once; a redirect is sent once,
// says it was not followed, and its target sees nothing. None is a
// *FinalError. The controls: a 429 and a 500 are retried, after the wait
// their Retry-After-Ms asked for, and the turn succeeds.
func TestKeyRouteHTTPFailures(t *testing.T) {
	retry := []string{"x-should-retry", "true", "retry-after-ms", "1", "retry-after", "1"}
	elsewhere := newRxServer(t, metaAnswer("never"))
	for _, tc := range []struct {
		name   string
		reply  http.HandlerFunc
		status int
		auth   bool
	}{
		{"401", rxJSON(401, `{"error":{"code":"invalid_api_key","message":"Unauthorized","param":null,"type":"authentication_error"}}`, retry...), 401, true},
		{"401 with a transient code", rxJSON(401, `{"error":{"code":"server_error","message":"try again"}}`, retry...), 401, true},
		{"400 with x-should-retry", rxJSON(400, `{"error":{"code":null,"message":"The request contains invalid parameters.","param":null,"type":"invalid_request_error"}}`, retry...), 400, false},
		{"403", rxJSON(403, `{"error":{"code":"permission_denied","message":"no","type":"permission_error"}}`), 403, true},
		{"404", rxJSON(404, `{"error":{"code":"model_not_found","message":"The requested model was not found.","param":null,"type":"invalid_request_error"}}`), 404, false},
		{"a redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.srv.URL+"/v1/responses", http.StatusTemporaryRedirect)
		}, 307, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRxServer(t, tc.reply, metaAnswer("a retry"), metaAnswer("a retry"), metaAnswer("a retry"))
			waits, err := keyTurn(t, srv)
			if n := len(srv.requests()); n != 1 || len(waits) != 0 {
				t.Fatalf("%d requests and %d retries, want 1 and none", n, len(waits))
			}
			var pe *fantasy.ProviderError
			if !errors.As(err, &pe) || pe.StatusCode != tc.status || pe.AuthError != tc.auth || pe.IsRetryable() {
				t.Fatalf("err = %v (%T), want a non-retryable HTTP %d, auth %v", err, err, tc.status, tc.auth)
			}
			var fe *FinalError
			if errors.As(err, &fe) {
				t.Fatalf("the key route returned a *FinalError: %v", fe)
			}
			if tc.status == 307 && !strings.Contains(pe.Message, "redirect (HTTP 307), which craze does not follow") {
				t.Fatalf("the redirect's message = %q", pe.Message)
			}
		})
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("the redirect's target saw %d requests", n)
	}
	for _, status := range []int{429, 500, 503} {
		t.Run(fmt.Sprintf("%d is retried", status), func(t *testing.T) {
			srv := newRxServer(t, rxJSON(status, `{"error":{"code":"rate_limit_exceeded","message":"slow down","type":"requests"}}`, "retry-after-ms", "1"), metaAnswer("ok"))
			waits, err := keyTurn(t, srv)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if n := len(srv.requests()); n != 2 || !slices.Equal(waits, []time.Duration{time.Millisecond}) {
				t.Fatalf("%d requests after waits %v, want 2 after the 1ms asked for", n, waits)
			}
		})
	}
	// The wait in seconds is kept for Fantasy too, and x-should-retry never.
	_, err := keyStep(t, newRxServer(t, rxJSON(429, `{"error":{"message":"slow down"}}`, "retry-after", "7", "x-should-retry", "false")))
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) || pe.ResponseHeaders["retry-after"] != "7" || pe.ResponseHeaders["x-should-retry"] != "" || !pe.IsRetryable() {
		t.Fatalf("err = %v, headers %v", err, pe.ResponseHeaders)
	}
}

// TestKeyRouteStreamFailures (A12, P19): inside the stream a rate limit or a
// server's fault before any output is a retryable *fantasy.ProviderError —
// where the ChatGPT route makes the rate limit final — and after output began
// the step fails once, a *MidStreamError (rule 3); a refusal of another kind,
// and an incomplete response, are not retryable; a stream that stopped short
// is Fantasy's retryable incomplete-stream error. A failed response's usage
// reaches a Finish part before the error. (An incomplete response cut by the
// ceiling or the content filter is a finish: TestKeyRouteCutShort.)
func TestKeyRouteStreamFailures(t *testing.T) {
	failed := func(code, typ string) string {
		return rx(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed",
			"error": map[string]any{"code": code, "type": typ, "message": "boom"}, "usage": rxUsage(5, 0, 0, 0)}})
	}
	for _, tc := range []struct {
		name      string
		reply     http.HandlerFunc
		retryable bool
	}{
		{"a rate limit", rxEvents(rx(map[string]any{"type": "error", "code": "rate_limit_exceeded", "message": "slow down"})), true},
		{"a rate-limit type", rxEvents(rx(map[string]any{"type": "error", "error": map[string]any{"type": "rate_limit_error", "message": "slow down"}})), true},
		{"a server error", rxEvents(failed("server_error", "")), true},
		{"an api error type", rxEvents(failed("", "api_error")), true},
		{"an invalid request", rxEvents(failed("invalid_prompt", "invalid_request_error")), false},
		{"incomplete for a reason craze does not know", rxEvents(rx(map[string]any{"type": "response.incomplete", "response": map[string]any{
			"status": "incomplete", "incomplete_details": map[string]any{"reason": "interrupted"}}})), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := keyStep(t, newRxServer(t, tc.reply))
			var pe *fantasy.ProviderError
			if !errors.As(err, &pe) || pe.IsRetryable() != tc.retryable {
				t.Fatalf("err = %v (%T), want a *fantasy.ProviderError, retryable %v", err, err, tc.retryable)
			}
			if strings.Contains(tc.name, "server") && !slices.ContainsFunc(parts, func(p fantasy.StreamPart) bool {
				return p.Type == fantasy.StreamPartTypeFinish && p.Usage.InputTokens == 5
			}) {
				t.Fatal("the failed response's usage reached no Finish part")
			}
		})
	}
	t.Run("after output", func(t *testing.T) {
		srv := newRxServer(t, rxEvents(rxAdded(0, metaMessage("rs_m", "")), rxText(0, "rs_m", "partial"), failed("server_error", "")),
			metaAnswer("a retry"))
		waits, err := keyTurn(t, srv)
		var me *MidStreamError
		if !errors.As(err, &me) || len(srv.requests()) != 1 || len(waits) != 0 {
			t.Fatalf("err = %v (%T) after %d requests, want one *MidStreamError", err, err, len(srv.requests()))
		}
	})
	t.Run("the stream stops short", func(t *testing.T) {
		created := rx(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}}})
		_, err := keyStep(t, newRxServer(t, rxEvents(created)))
		var pe *fantasy.ProviderError
		if !errors.As(err, &pe) || !pe.IsRetryable() {
			t.Fatalf("err = %v, want Fantasy's retryable incomplete-stream error", err)
		}
	})
}

// TestKeyRouteCancellation (A12, P19): a step cancelled while the provider
// holds the request, before any output or after some, ends with the
// context's own error — never a retryable provider error — and a turn
// cancelled so sends nothing again.
func TestKeyRouteCancellation(t *testing.T) {
	// holding answers once the step is cancelled: with some output first
	// when output is set, then nothing until the request is gone.
	holding := func(output bool, reached chan<- struct{}) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			if output {
				fmt.Fprintf(w, "event: x\ndata: %s\n\n", rxAdded(0, metaMessage("rs_m", "")))
				fmt.Fprintf(w, "event: x\ndata: %s\n\n", rxText(0, "rs_m", "partial"))
			}
			w.(http.Flusher).Flush()
			close(reached)
			<-r.Context().Done()
		}
	}
	for _, output := range []bool{false, true} {
		t.Run(fmt.Sprintf("a step, after output %v", output), func(t *testing.T) {
			reached := make(chan struct{})
			srv := newRxServer(t, holding(output, reached))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-reached
				cancel()
			}()
			stream, err := srv.keyModel(t).Stream(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			var got error
			for part := range stream {
				if part.Type == fantasy.StreamPartTypeError {
					got = part.Error
				}
			}
			var pe *fantasy.ProviderError
			if !errors.Is(got, context.Canceled) || errors.As(got, &pe) {
				t.Fatalf("the step's error = %v (%T), want the context's own", got, got)
			}
		})
		t.Run(fmt.Sprintf("a turn, after output %v", output), func(t *testing.T) {
			reached := make(chan struct{})
			srv := newRxServer(t, holding(output, reached), metaAnswer("a retry"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-reached
				cancel()
			}()
			agent := fantasy.NewAgent(srv.keyModel(t), fantasy.WithMaxRetries(3))
			_, err := agent.Stream(ctx, fantasy.AgentStreamCall{Prompt: "hi"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the context's", err)
			}
			if n := len(srv.requests()); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
		})
	}
}

// TestKeyRouteKeepsTheKeyOutOfErrors: a provider that echoes the bearer in its
// error body — before the stream and inside it — never has the key reach
// the error the step ends with.
func TestKeyRouteKeepsTheKeyOutOfErrors(t *testing.T) {
	echo := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rxJSON(status, `{"error":{"message":"bad credentials: `+r.Header.Get("Authorization")+`","type":"authentication_error"}}`)(w, r)
		}
	}
	inStream := func(w http.ResponseWriter, r *http.Request) {
		rxEvents(rx(map[string]any{"type": "error", "code": "invalid_request_error", "message": "echo " + r.Header.Get("Authorization")}))(w, r)
	}
	for name, reply := range map[string]http.HandlerFunc{"401": echo(401), "400": echo(400), "in the stream": inStream} {
		t.Run(name, func(t *testing.T) {
			srv := newRxServer(t, reply)
			_, err := keyStep(t, srv)
			if err == nil || strings.Contains(err.Error(), canary) {
				t.Fatalf("the error holds the key, or there is none: %v", err == nil)
			}
			var pe *fantasy.ProviderError
			if errors.As(err, &pe) && (strings.Contains(pe.Message, canary) || bytes.Contains(pe.ResponseBody, []byte(canary))) {
				t.Fatal("the provider error holds the key")
			}
		})
	}
}

// TestKeyRouteTwoMessagesAndAFullCompletedEvent (A12, P24): a response with
// two message items and a call between them, whose response.completed
// carries all three again (MR-10), is collected once each, in order — the
// call once, each text once — with the usage counted once.
func TestKeyRouteTwoMessagesAndAFullCompletedEvent(t *testing.T) {
	out := []map[string]any{metaMessage("rs_m1", "First."), metaCall("fc_1", "call_1", "read_file", `{"path":"a"}`), metaMessage("rs_m2", "Second.")}
	srv := newRxServer(t, rxEvents(
		rxAdded(0, metaMessage("rs_m1", "")), rxText(0, "rs_m1", "First."), rxDone(0, out[0]),
		rxAdded(1, metaCall("fc_1", "call_1", "read_file", "")), rxDone(1, out[1]),
		rxAdded(2, metaMessage("rs_m2", "")), rxText(2, "rs_m2", "Second."), rxDone(2, out[2]),
		metaCompleted(20, 0, 7, 2, out...),
	))
	resp, err := srv.keyModel(t).Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range resp.Content {
		switch v := c.(type) {
		case fantasy.TextContent:
			got = append(got, "text:"+v.Text)
		case fantasy.ToolCallContent:
			got = append(got, "call:"+v.ToolCallID)
		default:
			got = append(got, string(c.GetType()))
		}
	}
	if want := []string{"text:First.", "call:call_1", "text:Second."}; !slices.Equal(got, want) {
		t.Fatalf("content = %v, want %v", got, want)
	}
	if resp.Usage.TotalTokens != 27 || resp.Usage.OutputTokens != 7 || resp.FinishReason != fantasy.FinishReasonToolCalls {
		t.Fatalf("usage %+v, finish %q", resp.Usage, resp.FinishReason)
	}
}

// TestWithCacheKey (A12, P20): on the openai-responses driver each call's
// options are a fresh map holding a fresh value — the model's effort and the
// key — built for a model with no effort as well, and the model's own
// options are never changed, so two keys never meet. The controls: every
// other driver, and no key, get the model's options as they are.
func TestWithCacheKey(t *testing.T) {
	r := keyRoute("https://api.meta.ai/v1")
	shared, err := EffortOptions(r, "max")
	if err != nil {
		t.Fatal(err)
	}
	parent := WithCacheKey(r, shared, "parent-id")
	child := WithCacheKey(r, shared, "child-id")
	for name, tc := range map[string]struct {
		opts fantasy.ProviderOptions
		key  string
	}{"parent": {parent, "parent-id"}, "child": {child, "child-id"}} {
		o, ok := tc.opts[r.ProviderID].(*responsesOptions)
		if !ok || o.Effort != "max" || o.CacheKey != tc.key {
			t.Fatalf("%s: options = %#v", name, tc.opts[r.ProviderID])
		}
	}
	if parent[r.ProviderID] == child[r.ProviderID] || reflect.ValueOf(parent).Pointer() == reflect.ValueOf(shared).Pointer() {
		t.Fatal("two calls share one options value, or a call's map is the model's")
	}
	if o := shared[r.ProviderID].(*responsesOptions); o.CacheKey != "" || len(shared) != 1 {
		t.Fatalf("the model's own options were changed: %+v", o)
	}

	none := r
	none.Efforts, none.DefaultEffort = nil, ""
	noEffort, err := EffortOptions(none, "")
	if err != nil || noEffort != nil {
		t.Fatalf("no effort: %v, %v", noEffort, err)
	}
	if o, ok := WithCacheKey(none, noEffort, "sess")[r.ProviderID].(*responsesOptions); !ok || o.CacheKey != "sess" || o.Effort != "" {
		t.Fatalf("a model with no effort got %#v", o)
	}
	if got := WithCacheKey(r, shared, ""); reflect.ValueOf(got).Pointer() != reflect.ValueOf(shared).Pointer() {
		t.Fatal("with no key, the options were rebuilt")
	}

	for _, driver := range []string{modeltable.DriverOpenAICompat, modeltable.DriverOpenRouter, modeltable.DriverChatGPT, modeltable.DriverAnthropic} {
		other := resolved("https://x.example/v1")
		other.Driver = driver
		opts := fantasy.ProviderOptions{other.ProviderID: &responsesOptions{Effort: "low"}}
		if got := WithCacheKey(other, opts, "sess"); reflect.ValueOf(got).Pointer() != reflect.ValueOf(opts).Pointer() || len(got) != 1 {
			t.Errorf("%s: the options were rebuilt", driver)
		}
		if got := WithCacheKey(other, nil, "sess"); got != nil {
			t.Errorf("%s: no options became %v", driver, got)
		}
	}
}

// TestNewBuildsTheKeyRoute (A12, D13): New builds the openai-responses
// driver as the Responses model on the key route, wrapped, and refuses it
// with no key, a key too short to be real, no base URL, or a base URL the
// key may not go to — the client's own rule, held whatever the table let
// through — never naming the key. EffortOptions sends the Responses efforts
// for it, and refuses ultra. Its warnings and object refusals name the
// provider, not the ChatGPT plan.
func TestNewBuildsTheKeyRoute(t *testing.T) {
	lm, err := New(keyRoute("https://api.meta.ai/v1"))
	if err != nil {
		t.Fatal(err)
	}
	w, ok := lm.(*model)
	if !ok {
		t.Fatalf("New = %T, want the wrapper", lm)
	}
	inner, ok := w.inner.(*responsesModel)
	if !ok || inner.route != responsesapi.RouteKey || lm.Provider() != "meta" || lm.Model() != "muse-wonder-1.0" {
		t.Fatalf("the wrapped model is %T %+v", w.inner, inner)
	}
	for _, tc := range []struct {
		name   string
		edit   func(*modeltable.Resolved)
		target error
	}{
		{"no key", func(r *modeltable.Resolved) { r.APIKey = "" }, modeltable.ErrNoAPIKey},
		{"a short key", func(r *modeltable.Resolved) { r.APIKey = "short" }, ErrAPIKeyTooShort},
		{"no base URL", func(r *modeltable.Resolved) { r.BaseURL = " " }, nil},
		{"plain http to another host", func(r *modeltable.Resolved) { r.BaseURL = "http://203.0.113.9/v1" }, responsesapi.ErrKeyBaseURL},
		{"plain http to a name", func(r *modeltable.Resolved) { r.BaseURL = "http://localhost:8080/v1" }, responsesapi.ErrKeyBaseURL},
		{"a query", func(r *modeltable.Resolved) { r.BaseURL = "https://api.meta.ai/v1?k=" + canary }, responsesapi.ErrKeyBaseURL},
		{"no wire model", func(r *modeltable.Resolved) { r.WireModel = " " }, nil},
	} {
		r := keyRoute("https://api.meta.ai/v1")
		tc.edit(&r)
		_, err := New(r)
		if err == nil || (tc.target != nil && !errors.Is(err, tc.target)) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.target)
		} else if strings.Contains(err.Error(), canary) {
			t.Errorf("%s: the error names the key", tc.name)
		}
	}

	r := keyRoute("https://api.meta.ai/v1")
	for _, e := range r.Efforts {
		opts, err := EffortOptions(r, e)
		if o, ok := opts[r.ProviderID].(*responsesOptions); err != nil || !ok || o.Effort != e {
			t.Errorf("effort %q: %v, %v", e, opts, err)
		}
	}
	withUltra := r
	withUltra.Efforts = append(slices.Clone(r.Efforts), "ultra")
	if _, err := EffortOptions(withUltra, "ultra"); err == nil || !strings.Contains(err.Error(), "Responses request") {
		t.Errorf("ultra: err = %v", err)
	}

	_, warnings := inner.request(fantasy.Call{Tools: []fantasy.Tool{fantasy.ProviderDefinedTool{ID: "web", Name: "web"}}})
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, `provider "meta"`) || strings.Contains(warnings[0].Message, "ChatGPT") {
		t.Errorf("warnings = %+v", warnings)
	}
	if _, err := lm.GenerateObject(context.Background(), fantasy.ObjectCall{}); err == nil || !strings.Contains(err.Error(), `provider "meta"`) {
		t.Errorf("GenerateObject: %v", err)
	}
}
