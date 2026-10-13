package responsesapi

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The key route (plan 047 D12): the Responses API with a static key at a
// configured base URL, as Meta serves it (the plan's live facts, MR-1…16).

// keyOne is the key the tests send: a dummy, 8 bytes and more.
const keyOne = "test-api-key-0001"

// keyClient is a key-route client aimed at the server.
func (s *server) keyClient(t *testing.T) *Client {
	t.Helper()
	cl, err := NewClient(Config{Route: RouteKey, BaseURL: s.srv.URL + "/v1", APIKey: keyOne})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cl
}

// keyRequest is fullRequest with what the key route adds: a replayed
// reasoning item and call, the call's output ceiling and the session's cache
// key.
func keyRequest() Request {
	r := fullRequest()
	r.Input = append(r.Input,
		Reasoning("rs_6acd:rs_01a1", nil, "sealed-1"),
		AssistantMessage("I'll read it."),
		FunctionCall("call_1", "read", `{"path":"a"}`),
		FunctionCallOutput("call_1", "alpha"),
	)
	r.MaxOutputTokens = 131072
	r.CacheKey = "sess-1"
	return r
}

// TestKeyRouteBody: the key route's body holds exactly the ChatGPT route's
// fields plus max_output_tokens and prompt_cache_key; every tool is a plain
// top-level function (MR-9) and a replayed call carries no namespace; the
// store, stream, include and reasoning fields are the ChatGPT route's. The
// control: the same request on the ChatGPT route (Body) sends neither new
// field and keeps the namespace — they are the route's — and the caller's
// items are as they were after the key route's body was built.
func TestKeyRouteBody(t *testing.T) {
	r := keyRequest()
	body, err := r.body(RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	m := topLevel(t, body)
	want := []string{"include", "input", "instructions", "max_output_tokens", "model", "parallel_tool_calls", "prompt_cache_key", "reasoning", "store", "stream", "tool_choice", "tools"}
	if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %v, want exactly %v", got, want)
	}
	for key, want := range map[string]string{
		"store":             "false",
		"stream":            "true",
		"include":           `["reasoning.encrypted_content"]`,
		"instructions":      `"You are craze."`,
		"reasoning":         `{"effort":"high","summary":"auto"}`,
		"max_output_tokens": "131072",
		"prompt_cache_key":  `"sess-1"`,
		"tool_choice":       `"auto"`,
		"tools":             `[{"type":"function","name":"read","description":"Read a file.","parameters":{"type":"object"},"strict":false}]`,
	} {
		if got := string(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	var input []map[string]any
	if err := json.Unmarshal(m["input"], &input); err != nil {
		t.Fatal(err)
	}
	if call := input[3]; call["type"] != "function_call" || call["namespace"] != nil || call["call_id"] != "call_1" {
		t.Errorf("the replayed call = %v, want call_1 with no namespace", call)
	}
	if rs := input[1]; rs["id"] != "rs_6acd:rs_01a1" || rs["encrypted_content"] != "sealed-1" {
		t.Errorf("the replayed reasoning = %v", rs)
	}
	if msg := input[2]; msg["id"] != nil || msg["phase"] != nil {
		t.Errorf("the replayed message = %v, want neither an id nor a phase (MR-5)", msg)
	}
	if strings.Contains(string(body), "namespace") || strings.Contains(string(body), "session-id") {
		t.Errorf("the key route's body holds a namespace or the session header:\n%s", body)
	}

	chat, err := r.Body()
	if err != nil {
		t.Fatal(err)
	}
	cm := topLevel(t, chat)
	for _, f := range []string{"max_output_tokens", "prompt_cache_key"} {
		if _, ok := cm[f]; ok {
			t.Errorf("the ChatGPT route's body sends %s", f)
		}
	}
	if !strings.Contains(string(cm["tools"]), `"type":"namespace"`) || !strings.Contains(string(cm["input"]), `"namespace":"craze"`) {
		t.Errorf("the ChatGPT route lost its namespace after the key route's body was built:\n%s", chat)
	}

	bare := fullRequest()
	bare.Tools, bare.Instructions, bare.Effort = nil, "", ""
	body, err = bare.body(RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Sorted(maps.Keys(topLevel(t, body))), []string{"include", "input", "model", "reasoning", "store", "stream"}; !slices.Equal(got, want) {
		t.Fatalf("with no tools, ceiling or key: top-level keys = %v, want %v", got, want)
	}
	if _, err := (Request{}).body(RouteKey); err == nil {
		t.Error("a request with no model built a body")
	}
	if _, err := fullRequest().body(Route(9)); err == nil {
		t.Error("an unknown route built a body")
	}
}

// TestKeyRouteClientConfig: the key route needs its key and takes no token
// source; the ChatGPT route takes no key. The base URL is required, https on
// any host or http to a loopback IP literal, with no user info, query or
// fragment (P25), the model table's rule (modeltable keyEndpointProblem).
// The control: the ChatGPT route keeps its own rule — localhost and
// api.openai.com, not another https host.
func TestKeyRouteClientConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no key", Config{Route: RouteKey, BaseURL: "https://api.meta.ai/v1"}},
		{"a blank key", Config{Route: RouteKey, BaseURL: "https://api.meta.ai/v1", APIKey: "  "}},
		{"a token source", Config{Route: RouteKey, BaseURL: "https://api.meta.ai/v1", APIKey: keyOne, Credentials: newCreds(tokenOne)}},
		{"a key on the ChatGPT route", Config{Credentials: newCreds(tokenOne), APIKey: keyOne}},
		{"an unknown route", Config{Route: Route(9), BaseURL: "https://api.meta.ai/v1", APIKey: keyOne}},
	} {
		if _, err := NewClient(tc.cfg); err == nil {
			t.Errorf("%s: NewClient accepted it", tc.name)
		} else if strings.Contains(err.Error(), keyOne) {
			t.Errorf("%s: the error holds the key", tc.name)
		}
	}
	for base, ok := range map[string]bool{
		"https://api.meta.ai/v1":                 true,
		"https://localhost:8443/v1":              true,
		"https://10.0.0.7/v1":                    true,
		"http://127.0.0.1:8080/v1":               true,
		"http://127.9.8.7/v1":                    true,
		"http://[::1]:8080/v1":                   true,
		"":                                       false,
		"http://localhost:8080/v1":               false,
		"http://10.0.0.7/v1":                     false,
		"http://api.meta.ai/v1":                  false,
		"http://[::2]/v1":                        false,
		"https://user:pw@api.example/v1":         false,
		"https://api.example/v1?key=v":           false,
		"https://api.example/v1?":                false,
		"https://api.example/v1#f":               false,
		"api.meta.ai/v1":                         false,
		"::not a url":                            false,
		"https:///v1":                            false,
		"ftp://api.meta.ai/v1":                   false,
		"https://api.openai.com.evil.example/v1": true, // any https host: the configured one is the user's
	} {
		_, err := NewClient(Config{Route: RouteKey, BaseURL: base, APIKey: keyOne})
		if (err == nil) != ok {
			t.Errorf("base %q: err = %v, want accepted %v", base, err, ok)
		}
		if !ok && !errors.Is(err, ErrKeyBaseURL) {
			t.Errorf("base %q: err = %v, want ErrKeyBaseURL", base, err)
		}
	}
	for base, ok := range map[string]bool{"http://localhost:8080/v1": true, "https://api.meta.ai/v1": false} {
		if _, err := NewClient(Config{BaseURL: base, Credentials: newCreds(tokenOne)}); (err == nil) != ok {
			t.Errorf("the ChatGPT route at %q: err = %v, want accepted %v", base, err, ok)
		}
	}
}

// TestKeyRouteSendsItsKeyOnce: the key is the bearer of every request, to
// <base>/responses, and a 401 is the request's answer — sent once, never
// renewed, whatever its headers say — an *Error the key route reports. The
// control is the ChatGPT route's TestClientRenewsARefusedTokenOnce, which
// sends a refused request twice.
func TestKeyRouteSendsItsKeyOnce(t *testing.T) {
	srv := newServer(t, okReply())
	s, err := srv.keyClient(t).Stream(context.Background(), Request{Model: "muse-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(t, s); err != nil {
		t.Fatal(err)
	}
	req := srv.requests()[0]
	if req.path != "/v1/responses" || req.header.Get("Authorization") != "Bearer "+keyOne {
		t.Fatalf("the request went to %s with another bearer", req.path)
	}

	refused := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Should-Retry", "true")
		w.Header().Set("Retry-After-Ms", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"Unauthorized","param":null,"type":"authentication_error"}}`))
	}
	srv = newServer(t, refused, okReply())
	_, err = srv.keyClient(t).Stream(context.Background(), Request{Model: "muse-test"})
	e := asError(t, err)
	if e.StatusCode != 401 || e.Code != "invalid_api_key" || e.Type != "authentication_error" || e.Route != RouteKey || e.Final() || e.Transient() {
		t.Fatalf("error = %+v (final %v, transient %v)", e, e.Final(), e.Transient())
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("%d requests, want 1: a refused key was sent again", n)
	}
}

// TestKeyRouteNeverFollowsARedirect: a 3xx is a failed request naming its
// status; the place it points to never sees the request or the key.
func TestKeyRouteNeverFollowsARedirect(t *testing.T) {
	elsewhere := newServer(t, okReply())
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.srv.URL+"/v1/responses", status)
		})
		_, err := srv.keyClient(t).Stream(context.Background(), Request{Model: "muse-test"})
		if e := asError(t, err); e.StatusCode != status || e.Route != RouteKey || e.Transient() {
			t.Fatalf("a %d: error = %+v, want the redirect itself, not retryable", status, e)
		}
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("the redirect's target saw %d requests", n)
	}
}

// TestKeyRouteErrorClassification: on the key route nothing is final in the
// ChatGPT plan's sense; a 429 and a 5xx may be tried again before any
// output, as may a rate limit or a server's fault inside the stream; a 400,
// 401, 403 and 404 never, whatever code they carry. The control: each error
// judged on the ChatGPT route keeps that route's answer (a 429 final, the
// in-stream rate limit final, a 500 left to the caller).
func TestKeyRouteErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status, kind      string
		e                 Error
		transient         bool
		chatFinal, chatTr bool
	}{
		{"429", "", Error{StatusCode: 429}, true, true, false},
		{"429", "rate_limit_exceeded", Error{StatusCode: 429, Code: "rate_limit_exceeded"}, true, true, false},
		{"500", "", Error{StatusCode: 500}, true, false, false},
		{"502", "", Error{StatusCode: 502}, true, false, false},
		{"503", "", Error{StatusCode: 503}, true, false, true},
		{"400", "", Error{StatusCode: 400, Code: "invalid_request_error"}, false, true, false},
		{"401", "server_error", Error{StatusCode: 401, Code: "server_error"}, false, false, true},
		{"403", "", Error{StatusCode: 403}, false, false, false},
		{"404", "model_not_found", Error{StatusCode: 404, Code: "model_not_found"}, false, false, false},
		{"307", "", Error{StatusCode: 307}, false, false, false},
		{"stream", "rate_limit_exceeded", Error{Event: "error", Code: "rate_limit_exceeded"}, true, true, false},
		{"stream", "rate_limit_error type", Error{Event: "error", Type: "rate_limit_error"}, true, false, false},
		{"stream", "server_error", Error{Event: "response.failed", Code: "server_error"}, true, false, true},
		{"stream", "overloaded_error type", Error{Event: "error", Type: "overloaded_error"}, true, false, true},
		{"stream", "invalid_request_error", Error{Event: "error", Code: "invalid_request_error"}, false, false, false},
		{"stream", "an incomplete response", Error{Event: "response.incomplete", Reason: "max_output_tokens"}, false, false, false},
		{"stream", "the plan's usage limit", Error{Event: "response.failed", Code: "subscription_sharing_usage_limit_exceeded"}, false, true, false},
	} {
		key := tc.e
		key.Route = RouteKey
		if key.Final() || key.Transient() != tc.transient {
			t.Errorf("key route, %s %s: final %v transient %v, want false %v", tc.status, tc.kind, key.Final(), key.Transient(), tc.transient)
		}
		chat := tc.e
		if chat.Final() != tc.chatFinal || chat.Transient() != tc.chatTr {
			t.Errorf("ChatGPT route, %s %s: final %v transient %v, want %v %v", tc.status, tc.kind, chat.Final(), chat.Transient(), tc.chatFinal, tc.chatTr)
		}
	}
}

// TestKeyRouteStreamErrorsAreTheRoutes: a failure the stream reports on a
// key-route client is judged by the key route — an in-stream rate limit is
// transient there — and on a ChatGPT-route client by its own (final), the
// same event either way.
func TestKeyRouteStreamErrorsAreTheRoutes(t *testing.T) {
	limited := events(ev(map[string]any{"type": "error", "code": "rate_limit_exceeded", "message": "slow down"}))
	srv := newServer(t, limited, limited)
	s, err := srv.keyClient(t).Stream(context.Background(), Request{Model: "muse-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = collect(t, s)
	if e := asError(t, err); e.Route != RouteKey || !e.Transient() || e.Final() {
		t.Fatalf("on the key route: %+v", e)
	}
	s, err = srv.client(t, newCreds(tokenOne)).Stream(context.Background(), Request{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = collect(t, s)
	if e := asError(t, err); e.Route != RouteChatGPT || !e.Final() {
		t.Fatalf("on the ChatGPT route: %+v", e)
	}
}

// TestKeyRouteReadsMetasStream: a step as Meta streams it (MR-10, MR-3,
// MR-5) — a reasoning item with a colon id whose encrypted content is null
// as it begins and whole at its end, a commentary message with a phase and
// an rs_ id, a call, a second message, and response.completed carrying the
// whole output again — is each item once, in order: the sealed reasoning
// from its end, each message's text once, the call's arguments, and the
// usage, with nothing read twice from the completed event's output.
func TestKeyRouteReadsMetasStream(t *testing.T) {
	rs := map[string]any{"type": "reasoning", "id": "rs_6acd:rs_01a1", "summary": []any{}, "encrypted_content": nil, "status": "in_progress"}
	rsDone := map[string]any{"type": "reasoning", "id": "rs_6acd:rs_01a1", "summary": []any{}, "encrypted_content": "sealed-whole", "status": "completed"}
	msg := func(id, text string) map[string]any {
		m := message(id, text)
		m["phase"] = "commentary"
		return m
	}
	fc := map[string]any{"type": "function_call", "id": "fc_01", "call_id": "call_01", "name": "read_file", "arguments": `{"path":"notes.txt"}`, "status": "completed"}
	output := []any{rsDone, msg("rs_m1", "I'll read it."), fc, msg("rs_m2", "Then answer.")}
	completedWithOutput := ev(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_1", "status": "completed", "output": output, "usage": usageObject(550, 113, 97, 27)}})
	srv := newServer(t, events(
		added(0, rs),
		itemDone(0, rsDone),
		added(1, msg("rs_m1", "")),
		textDelta(1, "rs_m1", "I'll read it."),
		itemDone(1, msg("rs_m1", "I'll read it.")),
		added(2, map[string]any{"type": "function_call", "id": "fc_01", "call_id": "call_01", "name": "read_file", "arguments": ""}),
		argsDelta(2, "fc_01", `{"path":"notes.txt"}`),
		itemDone(2, fc),
		added(3, msg("rs_m2", "")),
		itemDone(3, msg("rs_m2", "Then answer.")),
		completedWithOutput,
	))
	s, err := srv.keyClient(t).Stream(context.Background(), Request{Model: "muse-test"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(evs), []string{"added0", "done0", "added1", "text1", "done1", "added2", "args2", "done2", "added3", "text3", "done3", "completed"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if got := deltas(evs, TextDelta, 1) + "|" + deltas(evs, TextDelta, 3); got != "I'll read it.|Then answer." {
		t.Fatalf("text = %q", got)
	}
	var done []OutputItem
	for _, e := range evs {
		if e.Kind == ItemDone {
			done = append(done, e.Item)
		}
	}
	if done[0].ID != "rs_6acd:rs_01a1" || done[0].EncryptedContent != "sealed-whole" || done[2].CallID != "call_01" || done[2].Namespace != "" {
		t.Fatalf("items = %+v", done)
	}
	if u := evs[len(evs)-1].Usage; u.InputTokens != 550 || u.CachedTokens != 113 || u.OutputTokens != 97 || u.ReasoningTokens != 27 {
		t.Fatalf("usage = %+v", u)
	}
}
