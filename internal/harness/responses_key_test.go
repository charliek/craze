package harness

import (
	"context"
	"encoding/json"
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
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// The openai-responses driver in the harness (plan 047 D12, P20, D15, P23):
// the session's id as each request's prompt cache key wherever the ChatGPT
// plan's session-id is set, and the driver change in both directions on a
// resumed session.

// keyProvidersTOML is the fixture's providers.toml with test and other on the
// openai-responses driver at %[1]s, and nokey left on openai-compat.
const keyProvidersTOML = `version = 1

[providers.test]
driver = "openai-responses"
base_url = "%[1]s"
env_keys = ["TEST_API_KEY"]

[providers.other]
driver = "openai-responses"
base_url = "%[1]s"
env_keys = ["OTHER_API_KEY"]

[providers.nokey]
driver = "openai-compat"
base_url = "%[1]s"
env_keys = ["NOKEY_API_KEY"]
`

// writeProviders replaces the fixture's providers.toml with body and loads
// the table again.
func writeProviders(f *fixture, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.home, modeltable.ProvidersFile), []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.table = f.load()
}

// onKeyRoute puts the fixture's test and other providers on the
// openai-responses driver at base.
func onKeyRoute(f *fixture, base string) { writeProviders(f, fmt.Sprintf(keyProvidersTOML, base)) }

// keyOptions is a call's Responses options as their JSON says them (the
// options type is package llm's own): its effort and its cache key; ok is
// false when the call has none for provider.
func keyOptions(t *testing.T, call fantasy.Call, provider string) (effort, cacheKey string, ok bool) {
	t.Helper()
	v, ok := call.ProviderOptions[provider]
	if !ok {
		return "", "", false
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Type string `json:"type"`
		Data struct {
			Effort   string `json:"effort"`
			CacheKey string `json:"cache_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Type != "craze.responses.options" {
		t.Fatalf("provider options for %q = %s, want the Responses options", provider, b)
	}
	return env.Data.Effort, env.Data.CacheKey, true
}

// TestCacheKeyGoesToTheResponsesDriver (A12, P20): every request a session on
// the openai-responses driver sends — each step of a turn, a side question,
// and both summarizer forms — carries the session's id as its cache key, with
// its effort, and no session-id header; a model with no effort carries the
// key all the same; and the model's own options never gain it. The control:
// a session on openai-compat sends its model's options as they are, with no
// cache key.
func TestCacheKeyGoesToTheResponsesDriver(t *testing.T) {
	t.Run("a turn, a side question and the aligned summary", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		onKeyRoute(f, "https://responses.example/v1")
		s := f.open(f.options())
		a := f.models["test/a"]
		a.push(answerWith("hi"), answerWith("an aside"), answerWith(longSummary("1. Request and intent\nhello.")))
		run(t, s, "hello")
		if o := ask(s, nil, "what did I say?"); o.err != nil {
			t.Fatalf("Aside: %v", o.err)
		}
		if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		calls := a.requests()
		if len(calls) != 3 || s.ID() == "" {
			t.Fatalf("%d calls, want the turn's, the side question's and the summary's", len(calls))
		}
		for i, c := range calls {
			effort, key, ok := keyOptions(t, c, "test")
			if !ok || effort != "high" || key != s.ID() {
				t.Errorf("call %d: effort %q, cache key %q; want high and the session's id %q", i, effort, key, s.ID())
			}
			if len(c.Headers) != 0 {
				t.Errorf("call %d sent headers %v", i, c.Headers)
			}
		}
		if _, key, _ := keyOptions(t, fantasy.Call{ProviderOptions: s.cur.effortOpts}, "test"); key != "" {
			t.Fatalf("the model's own options gained the cache key %q", key)
		}
	})
	t.Run("the text-form summary", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		onKeyRoute(f, "https://responses.example/v1")
		s := f.open(f.options())
		s.sleep = func(context.Context, time.Duration) {}
		f.models["test/a"].push(answerWith("hi"))
		run(t, s, "hello")
		m := s.cur
		m.r.ContextWindow, m.r.MaxOutputTokens = 4000, 100
		f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nFrom the text form.")))
		if _, err := s.compact(context.Background(), m, 2, store.CompactionOverflow, "", "", nil); err != nil {
			t.Fatalf("compact: %v", err)
		}
		calls := f.models["test/a"].requests()
		last := calls[len(calls)-1]
		if _, key, _ := keyOptions(t, last, "test"); len(last.Tools) != 0 || key != s.ID() {
			t.Fatalf("the text form's call: %d tools, cache key %q; want none and %q", len(last.Tools), key, s.ID())
		}
	})
	t.Run("a model with no effort", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		onKeyRoute(f, "https://responses.example/v1")
		s := f.open(f.options())
		if err := s.SetModel("other/c"); err != nil {
			t.Fatal(err)
		}
		if s.cur.effortOpts != nil {
			t.Fatalf("other/c has options %v, want none: it has no effort", s.cur.effortOpts)
		}
		f.models["other/c"].push(answerWith("hi"))
		run(t, s, "hello")
		calls := f.models["other/c"].requests()
		if effort, key, ok := keyOptions(t, calls[0], "other"); !ok || effort != "" || key != s.ID() {
			t.Fatalf("the call's options: effort %q, cache key %q (present %v); want the key alone", effort, key, ok)
		}
		if s.cur.effortOpts != nil {
			t.Fatal("the model gained options")
		}
	})
	t.Run("another driver sends none", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		f.models["test/a"].push(answerWith("hi"))
		run(t, s, "hello")
		c := f.models["test/a"].requests()[0]
		// Fantasy copies a call's map; the value in it is the model's own.
		if effortOf(t, c, "test") != "high" || len(c.ProviderOptions) != 1 || c.ProviderOptions["test"] != s.cur.effortOpts["test"] {
			t.Fatalf("an openai-compat call's options = %v, want the model's own", c.ProviderOptions)
		}
		if b, _ := json.Marshal(c.ProviderOptions); strings.Contains(string(b), "cache_key") || strings.Contains(string(b), s.ID()) {
			t.Fatalf("an openai-compat call carried the session's id: %s", b)
		}
	})
}

// TestCacheKeyIsEachSessionsOwn (A12, P20): a sub-agent child on the
// openai-responses driver sends its own session's id as its cache key, and
// its parent's requests keep the parent's, before the child ran and after —
// neither sees the other's value.
func TestCacheKeyIsEachSessionsOwn(t *testing.T) {
	f := newRouted(t)
	onKeyRoute(f.fixture, "https://responses.example/v1")
	s := f.open(f.options())
	k := watchKids(s)
	a := f.routers["test/a"]
	a.route("fan out", callStep(agentPart(t, "a1", task("first", "child one"))), answerWith("done"))
	a.route("child one", answerWith("child answer"))
	run(t, s, "fan out")
	kids := k.all()
	if len(kids) != 1 {
		t.Fatalf("%d children, want 1", len(kids))
	}
	child := kids[0].ID()
	if child == "" || child == s.ID() {
		t.Fatalf("the child's id %q is not its own (the parent's is %q)", child, s.ID())
	}
	for prompt, want := range map[string]string{"fan out": s.ID(), "child one": child} {
		calls := a.requests(prompt)
		if len(calls) == 0 {
			t.Fatalf("no calls for %q", prompt)
		}
		for i, c := range calls {
			if _, key, _ := keyOptions(t, c, "test"); key != want {
				t.Errorf("%q call %d: cache key %q, want %q", prompt, i, key, want)
			}
		}
	}
}

// rsAPI is a fake Responses API for the real stack: each request is answered
// by the next queued reply, and recorded as it arrived.
type rsAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	replies []http.HandlerFunc
	bodies  [][]byte
	headers []http.Header
}

func newRsAPI(t *testing.T, replies ...http.HandlerFunc) *rsAPI {
	t.Helper()
	a := &rsAPI{replies: replies}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request body: %v", err)
		}
		a.mu.Lock()
		a.bodies = append(a.bodies, body)
		a.headers = append(a.headers, r.Header.Clone())
		var next http.HandlerFunc
		if len(a.replies) > 0 {
			next, a.replies = a.replies[0], a.replies[1:]
		}
		a.mu.Unlock()
		if r.URL.Path != "/v1/responses" || next == nil {
			jsonError(w, http.StatusTeapot, "the test queued no reply for "+r.URL.Path)
			return
		}
		next(w, r)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *rsAPI) base() string { return a.srv.URL + "/v1" }

func (a *rsAPI) requests() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.bodies)
}

// rsReply streams items as Meta does (MR-10): each item's added and done
// events, a message's text as one delta, and response.completed carrying the
// whole output again with the usage.
func rsReply(items ...map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		send := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], b)
		}
		out := []any{}
		for i, it := range items {
			start := map[string]any{}
			for k, v := range it {
				start[k] = v
			}
			switch it["type"] {
			case "reasoning":
				start["encrypted_content"] = nil
			case "message":
				start["content"] = []any{}
			case "function_call":
				start["arguments"] = ""
			}
			send(map[string]any{"type": "response.output_item.added", "output_index": i, "item": start})
			if it["type"] == "message" {
				text := it["content"].([]any)[0].(map[string]any)["text"]
				send(map[string]any{"type": "response.output_text.delta", "output_index": i, "item_id": it["id"], "delta": text})
			}
			send(map[string]any{"type": "response.output_item.done", "output_index": i, "item": it})
			out = append(out, it)
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": out,
			"usage": map[string]any{"input_tokens": 120, "input_tokens_details": map[string]any{"cached_tokens": 64},
				"output_tokens": 8, "output_tokens_details": map[string]any{"reasoning_tokens": 3}, "total_tokens": 128}}})
	}
}

func rsReasoning(id, sealed string) map[string]any {
	return map[string]any{"type": "reasoning", "id": id, "summary": []any{}, "encrypted_content": sealed}
}

func rsMessage(id, text string) map[string]any {
	return map[string]any{"type": "message", "id": id, "role": "assistant", "phase": "commentary",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func rsCall(callID, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": args}
}

// rsInput is a Responses request's input items, each decoded.
func rsInput(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var req struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("a request body is not JSON: %v\n%s", err, body)
	}
	return req.Input
}

// rsShape is the input's items as type/role or type/call_id or type/id.
func rsShape(items []map[string]any) []string {
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

// realOptions are the fixture's options with every model built by the real
// factory (llm.New).
func realOptions(f *fixture) Options {
	o := f.options()
	o.NewModel = nil
	return o
}

// TestResponsesSessionResumesOnChatCompletions (A14, D15, P21): a session
// recorded on the openai-responses driver — a tool step whose reasoning has
// Meta's colon id, encrypted content and no summary — resumed after its
// provider goes back to openai-compat sends its textless reasoning as the
// assistant message's reasoning_content "", the shape Fantasy sends on
// purpose and Meta accepts (MR-16), with the message's text and its tool
// call intact and the call's result paired after it. The control: the
// session's own second Responses request replayed that reasoning, so the
// transcript does hold it.
func TestResponsesSessionResumesOnChatCompletions(t *testing.T) {
	api := newRsAPI(t,
		rsReply(rsReasoning("rs_6acd:rs_01a1", "sealed-1426"), rsMessage("rs_m1", "Reading a.txt."), rsCall("call_01", "read", `{"filePath":"a.txt"}`)),
		rsReply(rsMessage("rs_m2", "It says alpha.")),
	)
	f := newFixture(t, api.base())
	onKeyRoute(f, api.base())
	f.put("a.txt", "alpha\n")
	opts := realOptions(f)
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	run(t, s, "what is in a.txt?")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d Responses requests, want 2", len(reqs))
	}
	if got := rsShape(rsInput(t, reqs[1])); !slices.Contains(got, "reasoning/rs_6acd:rs_01a1") {
		t.Fatalf("the session's own second request did not replay its reasoning: %v", got)
	}

	w := newWire(t, sseReply(textChunk("Still alpha."), finishChunk("stop", true)))
	writeProviders(f, fmt.Sprintf(providersTOML, w.srv.URL+"/v1"))
	ropts := resumeOptions(realOptions(f), s.ID())
	r := resumed(t, ropts)
	run(t, r, "and now?")

	msgs := messages(t, w.requests()[0])
	var shapes []string
	var assistant, toolMsg json.RawMessage
	for i, raw := range msgs {
		var m struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(raw, &m)
		shapes = append(shapes, m.Role)
		if m.Role == "assistant" && assistant == nil {
			assistant, toolMsg = raw, msgs[i+1]
		}
	}
	if want := []string{"system", "user", "assistant", "tool", "assistant", "user"}; !slices.Equal(shapes, want) {
		t.Fatalf("messages = %v, want %v", shapes, want)
	}
	// The bytes as openai-go writes them: the textless reasoning part is
	// reasoning_content "", beside the text and the call it led to.
	want := `{"content":"Reading a.txt.","tool_calls":[{"id":"call_01","function":{"arguments":"{\"filePath\":\"a.txt\"}","name":"read"},"type":"function"}],"role":"assistant","reasoning_content":""}`
	if string(assistant) != want {
		t.Fatalf("the resumed request's tool-call message =\n%s\nwant\n%s", assistant, want)
	}
	var tm struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
	}
	if err := json.Unmarshal(toolMsg, &tm); err != nil || tm.Role != "tool" || tm.ToolCallID != "call_01" {
		t.Fatalf("the call's result = %s", toolMsg)
	}
	if strings.Contains(string(w.requests()[0]), "sealed-1426") || strings.Contains(string(w.requests()[0]), "rs_6acd") {
		t.Fatal("the encrypted reasoning reached Chat Completions")
	}
}

// TestChatCompletionsSessionResumesOnResponses (A12, P23): a session
// recorded on openai-compat — a step with reasoning text, Meta's redacted
// kind, and a tool call — resumed after its provider moves to the
// openai-responses driver sends no reasoning item (it carries no Responses
// metadata to replay), keeps the call and its result paired by call_id, and
// sends nothing the route refuses: no system item, no namespace, no message
// without content, and the session's id as prompt_cache_key. The control:
// the transcript does hold the reasoning, which a request on openai-compat
// sends back as reasoning_content.
func TestChatCompletionsSessionResumesOnResponses(t *testing.T) {
	w := newWire(t,
		sseReply(reasoningChunk("thinking it over"), toolCallChunk("call_01", "read", `{"filePath":"a.txt"}`), finishChunk("tool_calls", true)),
		sseReply(textChunk("It says alpha."), finishChunk("stop", true)),
	)
	f := newFixture(t, w.srv.URL+"/v1")
	f.put("a.txt", "alpha\n")
	s, err := Open(realOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	run(t, s, "what is in a.txt?")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if second := string(w.requests()[1]); !strings.Contains(second, `"reasoning_content":"thinking it over"`) {
		t.Fatalf("the session's own second request did not send its reasoning back:\n%s", second)
	}

	api := newRsAPI(t, rsReply(rsMessage("rs_m3", "Still alpha.")))
	onKeyRoute(f, api.base())
	r := resumed(t, resumeOptions(realOptions(f), s.ID()))
	run(t, r, "and now?")

	body := api.requests()[0]
	items := rsInput(t, body)
	want := []string{"message/user", "function_call/call_01", "function_call_output/call_01", "message/assistant", "message/user"}
	if got := rsShape(items); !slices.Equal(got, want) {
		t.Fatalf("the resumed request's input = %v, want %v", got, want)
	}
	for i, it := range items {
		if it["type"] == "message" {
			if c, _ := it["content"].([]any); len(c) == 0 {
				t.Errorf("item %d is a message with no content", i)
			}
			if it["role"] == "system" {
				t.Errorf("item %d is a system item", i)
			}
		}
	}
	f2 := fields(t, body)
	if string(f2["prompt_cache_key"]) != `"`+s.ID()+`"` {
		t.Fatalf("prompt_cache_key = %s, want the session's id", f2["prompt_cache_key"])
	}
	if strings.Contains(string(body), "namespace") || strings.Contains(string(body), "thinking it over") {
		t.Fatalf("the request holds a namespace or the Chat Completions reasoning:\n%s", body)
	}
}

// TestKeyRouteCutResponseReplaysInOrder (plan 047 C6, review F1): a turn on
// the openai-responses driver whose response is cut by max_output_tokens
// while message 0 still streams, after message 1 was done, ends max_tokens,
// and the next turn's request replays the stored answer as it was said:
// "First." then "Second.".
func TestKeyRouteCutResponseReplaysInOrder(t *testing.T) {
	cut := func(w http.ResponseWriter, _ *http.Request) {
		send := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], b)
		}
		begun := func(id string) map[string]any {
			return map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{}}
		}
		send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": begun("rs_m0")})
		send(map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "rs_m0", "delta": "First."})
		send(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": begun("rs_m1")})
		send(map[string]any{"type": "response.output_text.delta", "output_index": 1, "item_id": "rs_m1", "delta": "Second."})
		send(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": rsMessage("rs_m1", "Second.")})
		send(map[string]any{"type": "response.incomplete", "response": map[string]any{"status": "incomplete",
			"incomplete_details": map[string]any{"reason": "max_output_tokens"},
			"usage":              map[string]any{"input_tokens": 30, "output_tokens": 9, "total_tokens": 39}}})
	}
	api := newRsAPI(t, cut, rsReply(rsMessage("rs_m2", "Going on.")))
	f := newFixture(t, api.base())
	onKeyRoute(f, api.base())
	s, err := Open(realOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if res := run(t, s, "go"); res.StopReason != StopMaxTokens {
		t.Fatalf("the cut turn stopped %q, want %q", res.StopReason, StopMaxTokens)
	}
	run(t, s, "go on")
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	var said []string
	for _, it := range rsInput(t, reqs[1]) {
		if it["type"] == "message" && it["role"] == "assistant" {
			for _, c := range it["content"].([]any) {
				said = append(said, fmt.Sprint(c.(map[string]any)["text"]))
			}
		}
	}
	if !slices.Equal(said, []string{"First.", "Second."}) {
		t.Fatalf("the next request replays %q, want First. then Second.", said)
	}
}

// TestKeyRouteIncompleteCallIsNotReplayed (plan 047 C6, review F2): a turn
// on the openai-responses driver whose response is cut by max_output_tokens
// after a call's end said status "incomplete", its arguments partial, ends
// max_tokens without running the call, and the next turn's request, built
// from the stored session, replays the text before it and no call at all.
func TestKeyRouteIncompleteCallIsNotReplayed(t *testing.T) {
	partial := `{"filePath":`
	cut := func(w http.ResponseWriter, _ *http.Request) {
		send := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], b)
		}
		begun := rsCall("call_01", "read", "")
		ended := rsCall("call_01", "read", partial)
		ended["status"] = "incomplete"
		send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": rsMessage("rs_m0", "Reading it.")})
		send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": rsMessage("rs_m0", "Reading it.")})
		send(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": begun})
		send(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": begun["id"], "delta": partial})
		send(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": ended})
		send(map[string]any{"type": "response.incomplete", "response": map[string]any{"status": "incomplete",
			"incomplete_details": map[string]any{"reason": "max_output_tokens"},
			"usage":              map[string]any{"input_tokens": 30, "output_tokens": 9, "total_tokens": 39}}})
	}
	api := newRsAPI(t, cut, rsReply(rsMessage("rs_m2", "Going on.")))
	f := newFixture(t, api.base())
	onKeyRoute(f, api.base())
	f.put("a.txt", "alpha\n")
	s, err := Open(realOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if res := run(t, s, "go"); res.StopReason != StopMaxTokens {
		t.Fatalf("the cut turn stopped %q, want %q", res.StopReason, StopMaxTokens)
	}
	run(t, s, "go on")
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	got := rsShape(rsInput(t, reqs[1]))
	// The tools' schemas name filePath too: what must be absent is a call
	// item, and the partial arguments as a call's string would hold them.
	body := string(reqs[1])
	if want := []string{"message/user", "message/assistant", "message/user"}; !slices.Equal(got, want) ||
		strings.Contains(body, `"function_call"`) || strings.Contains(body, `{\"filePath\":"`) {
		t.Fatalf("the next request's input = %v, want %v and no call\n%s", got, want, reqs[1])
	}
}
