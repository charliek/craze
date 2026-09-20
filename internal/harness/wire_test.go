package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The tests here run turns through the real stack — Fantasy's agent, the
// OpenAI-compatible provider, the llm factory and wrapper — against a local
// server speaking Chat Completions SSE.

// wire answers each request with the next queued reply and keeps every
// request body exactly as it arrived.
type wire struct {
	srv *httptest.Server

	mu      sync.Mutex
	replies []http.HandlerFunc
	bodies  [][]byte
}

func newWire(t *testing.T, replies ...http.HandlerFunc) *wire {
	t.Helper()
	w := &wire{replies: replies}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request body: %v", err)
		}
		w.mu.Lock()
		w.bodies = append(w.bodies, body)
		var next http.HandlerFunc
		if len(w.replies) > 0 {
			next, w.replies = w.replies[0], w.replies[1:]
		}
		w.mu.Unlock()
		if next == nil {
			jsonError(rw, http.StatusTeapot, "the test queued no reply for this request")
			return
		}
		next(rw, r)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// requests is every request body the server received.
func (w *wire) requests() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.bodies)
}

// wireFixture is the test table aimed at w, on the default model factory
// (Options.NewModel nil, so llm.New).
func wireFixture(t *testing.T, w *wire) (*fixture, Options) {
	t.Helper()
	f := newFixture(t, w.srv.URL+"/v1")
	opts := f.options()
	opts.NewModel = nil
	return f, opts
}

func sseReply(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func textChunk(text string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text)
}

func reasoningChunk(text string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":%q},"finish_reason":null}]}`, text)
}

// finishChunk ends a stream, with usage the way OpenAI-compatible providers
// report it when usage is set.
func finishChunk(reason string, usage bool) string {
	u := ""
	if usage {
		u = `,"usage":{"prompt_tokens":120,"completion_tokens":8,"total_tokens":128,"prompt_tokens_details":{"cached_tokens":64}}`
	}
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":%q}]%s}`, reason, u)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error"}}`, message)
}

// echoKey is a provider error whose message echoes the Authorization header,
// the realistic way a key ends up in an error.
func echoKey(status int, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("retry-after-ms", "1") // a retried status must not wait Fantasy's 5 s
		jsonError(w, status, prefix+r.Header.Get("Authorization"))
	}
}

// messages is a request's messages array, each element the raw bytes as
// sent: the prefix test compares what went over the wire, not a re-encoding.
func messages(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("a request body is not JSON: %v\n%s", err, body)
	}
	return req.Messages
}

// fields is a request's top-level fields, each value's raw bytes.
func fields(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("a request body is not JSON: %v\n%s", err, body)
	}
	return req
}

// TestPromptCachePrefixIsStable is the prompt-cache property (plan 018
// §3.7, owner decision 6), checked on the bytes: over three turns, each
// request's messages begin with the previous request's messages exactly, so
// a provider's prefix cache can hit. The first turn's answer carries
// reasoning, which the next two requests replay; and between the second and
// third turns the model table is reloaded — read again from the same files
// into a fresh Table and installed in the session, the way a later phase's
// reload would, then SetModel to the current alias rebuilds the client from
// it. That catches map-ordering nondeterminism in what the table feeds a
// request, the reasoning filter changing history it should leave alone, and
// a timestamp or anything else per-request leaking into the prompt.
func TestPromptCachePrefixIsStable(t *testing.T) {
	w := newWire(t,
		sseReply(reasoningChunk("Simple arithmetic: "), reasoningChunk("2 plus 2 is 4."), textChunk("Four"), textChunk("."), finishChunk("stop", true)),
		sseReply(textChunk("Six."), finishChunk("stop", true)),
		sseReply(textChunk("You're welcome."), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	var builds atomic.Int32
	opts.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		builds.Add(1)
		return llm.New(r) // the default factory, counted
	}
	opts.Now = time.Now // a real clock: nothing may reach the prompt
	s := f.open(opts)

	run(t, s, "What is 2+2?")
	run(t, s, "And 3+3?")
	reloaded := f.load()
	s.mu.Lock()
	s.table = reloaded
	s.mu.Unlock()
	if err := s.SetModel("test/a"); err != nil {
		t.Fatalf("SetModel after the reload: %v", err)
	}
	run(t, s, "Thanks.")
	if n := builds.Load(); n != 2 {
		t.Fatalf("the model's client was built %d times, want 2 (Open, and the reload)", n)
	}

	bodies := w.requests()
	if len(bodies) != 3 {
		t.Fatalf("the server saw %d requests, want 3", len(bodies))
	}
	for i := 1; i < len(bodies); i++ {
		prev, next := messages(t, bodies[i-1]), messages(t, bodies[i])
		if len(next) != len(prev)+2 {
			t.Fatalf("request %d has %d messages, want request %d's %d plus an answer and a prompt", i+1, len(next), i, len(prev))
		}
		for k := range prev {
			if !bytes.Equal(prev[k], next[k]) {
				t.Fatalf("request %d's message %d differs from request %d's:\n%s\n%s", i+1, k, i, next[k], prev[k])
			}
		}
	}
	// The reasoning really was replayed, so the comparison covered it.
	if a1 := messages(t, bodies[2])[2]; !bytes.Contains(a1, []byte(`"reasoning_content":"Simple arithmetic: 2 plus 2 is 4."`)) {
		t.Fatalf("the reasoning turn's answer was not replayed with its reasoning: %s", a1)
	}
	// Everything else in the request is the same bytes every time too.
	first := fields(t, bodies[0])
	for i, body := range bodies[1:] {
		got := fields(t, body)
		for k, v := range first {
			if k != "messages" && !bytes.Equal(v, got[k]) {
				t.Errorf("request %d's %q is %s, request 1's was %s", i+2, k, got[k], v)
			}
		}
		if len(got) != len(first) {
			t.Errorf("request %d has fields %v, request 1 had %v", i+2, keys(got), keys(first))
		}
	}
	if string(first["max_tokens"]) != "4096" || string(first["reasoning_effort"]) != `"high"` || string(first["model"]) != `"wire-a"` {
		t.Errorf("request 1 sent max_tokens %s, reasoning_effort %s, model %s; want 4096, high, wire-a",
			first["max_tokens"], first["reasoning_effort"], first["model"])
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: What is 2+2?",
		"assistant test/a high end_turn: (thinking: Simple arithmetic: 2 plus 2 is 4.) Four.",
		"user test/a high: And 3+3?",
		"assistant test/a high end_turn: Six.",
		"user test/a high: Thanks.",
		"assistant test/a high end_turn: You're welcome.",
	})
	if u := transcript(t, s).Entries[1].Usage; u == nil || *u != (Usage{Input: 56, Output: 8, CacheRead: 64}) {
		t.Errorf("the first answer's usage was recorded as %+v", u)
	}
}

func keys(m map[string]json.RawMessage) []string { return slices.Sorted(maps.Keys(m)) }

// toolCallChunk is one whole tool call in a Chat Completions stream.
func toolCallChunk(id, name, args string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, id, name, args)
}

// prefixBreak is the index of the first of prev's messages that next does
// not begin with byte for byte, or -1 when next extends prev.
func prefixBreak(prev, next []json.RawMessage) int {
	for k := range prev {
		if k >= len(next) || !bytes.Equal(prev[k], next[k]) {
			return k
		}
	}
	return -1
}

// TestToolLoopPrefixIsStable is plan 019 §3.6's cache property on the wire,
// through Fantasy's and the provider's own encoding: across the steps of a
// tool turn and across turns, every request begins with the whole of the
// request before it — the system prompt, the history, each tool step's
// assistant message (reasoning, as the provider replays it, included) and
// its results — and carries the same tools, and everything else, byte for
// byte. A tool step persisted and replayed next turn is the same bytes the
// turn itself sent at its next step. The control is prefixBreak itself: a
// request with one byte changed in one message breaks the prefix there.
//
// The second turn's call comes with a "stop" finish, which the wrapper turns
// into "tool-calls" (D-21): its StepDone reports both.
func TestToolLoopPrefixIsStable(t *testing.T) {
	read := `{"filePath":"a.txt"}`
	w := newWire(t,
		sseReply(reasoningChunk("I should read it."), toolCallChunk("call_1", "read", read), finishChunk("tool_calls", true)),
		sseReply(textChunk("It says alpha."), finishChunk("stop", true)),
		sseReply(toolCallChunk("call_1", "read", read), finishChunk("stop", true)),
		sseReply(textChunk("Still alpha."), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	opts.Now = time.Now // a real clock: nothing may reach the prompt
	s := f.open(opts)
	f.put("a.txt", "alpha\n")

	run(t, s, "What does a.txt say?")
	var ev events
	if _, err := s.Run(context.Background(), "And now?", ev.sink); err != nil {
		t.Fatal(err)
	}
	if d := of[StepDone](ev.list())[0]; d.Finish != "tool-calls" || d.FinishRaw != "stop" {
		t.Errorf("the normalized step's StepDone says finish %q, raw %q; want tool-calls, stop", d.Finish, d.FinishRaw)
	}

	bodies := w.requests()
	if len(bodies) != 4 {
		t.Fatalf("the server saw %d requests, want 4", len(bodies))
	}
	grow := []int{2, 2, 2} // step 1's call and result; turn 1's answer and turn 2's prompt; turn 2's call and result
	for i := 1; i < len(bodies); i++ {
		prev, next := messages(t, bodies[i-1]), messages(t, bodies[i])
		if k := prefixBreak(prev, next); k >= 0 {
			t.Fatalf("request %d does not begin with request %d: message %d differs\n%s\n%s", i+1, i, k, prev[k], next[min(k, len(next)-1)])
		}
		if len(next) != len(prev)+grow[i-1] {
			t.Fatalf("request %d has %d messages, want request %d's %d plus %d", i+1, len(next), i, len(prev), grow[i-1])
		}
	}
	first := fields(t, bodies[0])
	for i, body := range bodies[1:] {
		got := fields(t, body)
		for k, v := range first {
			if k != "messages" && !bytes.Equal(v, got[k]) {
				t.Errorf("request %d's %q differs from request 1's:\n%s\n%s", i+2, k, got[k], v)
			}
		}
	}
	if !bytes.Contains(first["tools"], []byte(`"name":"read"`)) || bytes.Contains(first["tools"], []byte(`"required":null`)) {
		t.Fatalf("request 1's tools: %s", first["tools"])
	}
	// The reasoning of the tool step was replayed, so the comparison covered it.
	if a := messages(t, bodies[3])[2]; !bytes.Contains(a, []byte(`"reasoning_content":"I should read it."`)) || !bytes.Contains(a, []byte(`"call_1"`)) {
		t.Fatalf("the tool step replayed without its reasoning or its call: %s", a)
	}

	// The control.
	prev := messages(t, bodies[2])
	next := slices.Clone(messages(t, bodies[3]))
	next[2] = bytes.Replace(slices.Clone(next[2]), []byte("I should"), []byte("I shoulD"), 1)
	if k := prefixBreak(prev, next); k != 2 {
		t.Fatalf("control: a changed byte in message 2 broke the prefix at %d, want 2", k)
	}
}

// TestSteerPrefixIsStable extends the property above to an interjection (plan
// 019 §3.6, §3.10): a steer accepted while a turn's first step streams sits at
// one index from the step that takes it up onwards — the step after it, and
// the next turn's replay of the transcript — and every request still begins
// with the whole of the request before it, byte for byte, so the provider's
// prefix cache keeps hitting across an interjection.
//
// That is the reason the runner rebuilds the step's messages from its splices
// every step instead of appending in place: Fantasy composes a step's input
// from initialPrompt + responseMessages and applies PrepareStep's list to that
// one step (agent.go:943-970), so an appended steer would vanish at the next
// step and move the prefix under the cache. The control is the same run with
// no Steer at all: the steer's bytes are in no request, and each request is
// one message shorter.
func TestSteerPrefixIsStable(t *testing.T) {
	// The index the steer lands at: the system prompt, the turn's prompt, and
	// the first tool step's assistant message and its results are ahead of it.
	const at = 4
	for _, steer := range []bool{true, false} {
		t.Run(fmt.Sprintf("steer=%v", steer), func(t *testing.T) {
			read := `{"filePath":"a.txt"}`
			// Two tool steps and an answer, then a second turn that replays
			// the lot: the steer must be at the same index in the last three
			// requests.
			w := newWire(t,
				sseReply(toolCallChunk("call_1", "read", read), finishChunk("tool_calls", true)),
				sseReply(toolCallChunk("call_2", "read", read), finishChunk("tool_calls", true)),
				sseReply(textChunk("It says alpha."), finishChunk("stop", true)),
				sseReply(textChunk("You're welcome."), finishChunk("stop", true)),
			)
			f, opts := wireFixture(t, w)
			opts.Now = time.Now // a real clock: nothing may reach the prompt
			s := f.open(opts)
			f.put("a.txt", "alpha\n")

			// The steer is accepted from inside the sink, once the first step's
			// tool call has been announced: the turn is live, its first
			// request is already on the wire, and the second has not been
			// built. Steer takes no lock the turn holds, so calling it from
			// there is safe, and this is the test that says so.
			var sent atomic.Bool
			_, err := s.Run(context.Background(), "What does a.txt say?", func(ev Event) {
				if _, ok := ev.(ToolCalled); !ok || !steer || !sent.CompareAndSwap(false, true) {
					return
				}
				if err := sendSteer(s, steerText); err != nil {
					t.Errorf("Steer from the sink: %v", err)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if sent.Load() != steer {
				t.Fatalf("the steer was sent = %v, want %v", sent.Load(), steer)
			}
			run(t, s, "Thanks.")

			bodies := w.requests()
			if len(bodies) != 4 {
				t.Fatalf("the server saw %d requests, want 4", len(bodies))
			}
			grow := []int{2, 2, 2} // each step's assistant message and results, then the answer and the next prompt
			if steer {
				grow[0] = 3 // and the steer, the first time it goes out
			}
			for i := 1; i < len(bodies); i++ {
				prev, next := messages(t, bodies[i-1]), messages(t, bodies[i])
				if k := prefixBreak(prev, next); k >= 0 {
					t.Fatalf("request %d does not begin with request %d: message %d differs\n%s\n%s",
						i+1, i, k, prev[k], next[min(k, len(next)-1)])
				}
				if len(next) != len(prev)+grow[i-1] {
					t.Fatalf("request %d has %d messages, want request %d's %d plus %d", i+1, len(next), i, len(prev), grow[i-1])
				}
			}
			// The steer is at one index in every request from the step that
			// took it up on, the next turn's replay included, and in no other
			// message of any of them. The control has it in none at all.
			for i, body := range bodies {
				msgs := messages(t, body)
				want := steer && i > 0
				for k, m := range msgs {
					if holds := bytes.Contains(m, []byte(steerText)); holds != (want && k == at) {
						t.Fatalf("request %d's message %d holds the steer = %v, want %v:\n%s", i+1, k, holds, want && k == at, m)
					}
				}
			}
		})
	}
}

// TestWireErrors maps each provider failure onto the harness's errors, and
// checks that no key reaches a returned error or an emitted event even when
// the provider echoes it back.
func TestWireErrors(t *testing.T) {
	cases := []struct {
		name         string
		replies      []http.HandlerFunc
		kind         error // nil = a ProviderError of no kind
		status       int
		message      string // the exact message; "" = only checked for shape
		requests     int
		retries      int
		wantEntries  []string // nil = no transcript
		wantTextSeen string
	}{
		{name: "401 echoing the key", replies: []http.HandlerFunc{echoKey(401, "invalid credentials: ")},
			kind: ErrAuth, status: 401, message: "invalid credentials: Bearer [redacted]", requests: 1},
		{name: "403", replies: []http.HandlerFunc{echoKey(403, "not allowed: ")},
			kind: ErrAuth, status: 403, message: "not allowed: Bearer [redacted]", requests: 1},
		{name: "404", replies: []http.HandlerFunc{func(w http.ResponseWriter, _ *http.Request) {
			jsonError(w, 404, "The model `wire-a` does not exist")
		}}, kind: ErrModelNotFound, status: 404, message: "The model `wire-a` does not exist", requests: 1},
		{name: "context too large", replies: []http.HandlerFunc{func(w http.ResponseWriter, _ *http.Request) {
			jsonError(w, 400, "This model's maximum context length is 1000 tokens. However, your messages resulted in 2000 tokens.")
		}}, kind: ErrContextTooLarge, status: 400, requests: 1},
		{name: "500 twice, retried once", replies: []http.HandlerFunc{echoKey(500, "overloaded: "), echoKey(500, "still overloaded: ")},
			status: 500, message: "still overloaded: Bearer [redacted]", requests: 2, retries: 1},
		{name: "stream error after text", replies: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", textChunk("partial"))
			fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"server_error\"}}\n\n", "upstream overloaded: "+r.Header.Get("Authorization"))
		}}, message: "stream error: upstream overloaded: Bearer [redacted]", requests: 1,
			wantEntries: []string{"user test/a high: hi", "assistant test/a high interrupted: partial"}, wantTextSeen: "partial"},
		{name: "a raw body", replies: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(400)
			fmt.Fprintf(w, "<html>\n<h1>Bad Request</h1>\n<pre>Authorization: %s</pre>\n%s\n</html>\n", r.Header.Get("Authorization"), strings.Repeat("x", 2000))
		}}, status: 400, requests: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWire(t, tc.replies...)
			f, opts := wireFixture(t, w)
			s := f.open(opts)
			var ev events
			res, err := s.Run(context.Background(), "hi", ev.sink)
			if !empty(res) {
				t.Errorf("a failed turn returned %+v, want a zero Result", res)
			}
			var pe *ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("Run = %#v, want a *ProviderError", err)
			}
			for _, kind := range []error{ErrAuth, ErrModelNotFound, ErrContextTooLarge, ErrEmptyStep} {
				if is := errors.Is(err, kind); is != (kind == tc.kind) {
					t.Errorf("errors.Is(err, %v) = %v", kind, is)
				}
			}
			if pe.StatusCode != tc.status || pe.Provider != "test" || pe.Model != "test/a" {
				t.Errorf("ProviderError = %+v; want status %d, provider test, model test/a", pe, tc.status)
			}
			if tc.message != "" && pe.Message != tc.message {
				t.Errorf("Message = %q, want %q", pe.Message, tc.message)
			}
			if pe.Message == "" || len(pe.Message) > maxMessageBytes || strings.ContainsAny(pe.Message, "\r\n\x1b") {
				t.Errorf("Message %q is not one non-empty line of at most %d bytes", pe.Message, maxMessageBytes)
			}
			if n := len(w.requests()); n != tc.requests {
				t.Errorf("the server saw %d requests, want %d", n, tc.requests)
			}
			var retries int
			var seen strings.Builder
			for _, e := range ev.list() {
				switch e := e.(type) {
				case Retrying:
					retries++
				case TextDelta:
					seen.WriteString(e.Text)
				}
			}
			if retries != tc.retries || seen.String() != tc.wantTextSeen {
				t.Errorf("events: %d Retrying and text %q; want %d and %q", retries, seen.String(), tc.retries, tc.wantTextSeen)
			}
			if found := leaks(err, canary); len(found) > 0 {
				t.Errorf("the key is reachable from the error at %v", found)
			}
			if found := leaks(ev.list(), canary); len(found) > 0 {
				t.Errorf("the key reached an event at %v", found)
			}
			if tc.wantEntries == nil {
				noTranscript(t, s)
			} else {
				equal(t, "transcript", entries(transcript(t, s)), tc.wantEntries)
			}
		})
	}
}

// A provider that finishes with nothing at all is ErrEmptyStep (D-25), a
// failed turn that writes nothing.
func TestWireEmptyStep(t *testing.T) {
	w := newWire(t, sseReply(finishChunk("stop", false)))
	f, opts := wireFixture(t, w)
	s := f.open(opts)
	res, err := s.Run(context.Background(), "hi", nil)
	var pe *ProviderError
	if !errors.Is(err, ErrEmptyStep) || errors.As(err, &pe) || !empty(res) {
		t.Fatalf("Run = %+v, %#v; want a zero Result and ErrEmptyStep", res, err)
	}
	noTranscript(t, s)
}

// A failure before any output is retried once; the retry is reported, and
// the answer is the retry's alone.
func TestWireRetryBeforeOutput(t *testing.T) {
	w := newWire(t,
		echoKey(503, "busy: "),
		sseReply(textChunk("recovered"), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	s := f.open(opts)
	var ev events
	res, err := s.Run(context.Background(), "hi", ev.sink)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", res, err)
	}
	recovered := done(1, fantasy.FinishReasonStop, StopEndTurn, 2)
	recovered.Usage = Usage{Input: 56, Output: 8, CacheRead: 64}
	equal(t, "events", plain(ev.list()), []Event{
		Retrying{Delay: time.Millisecond, Attempt: 1, Reason: "HTTP 503: busy: Bearer [redacted]"},
		TextDelta{Text: "recovered"},
		recovered,
	})
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high end_turn: recovered",
	})
}

// A cancel mid-stream on the real provider ends the turn cancelled, with the
// streamed text persisted.
func TestWireCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	w := newWire(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(rw, "data: %s\n\n", reasoningChunk("thinking"))
		fmt.Fprintf(rw, "data: %s\n\n", textChunk("partial"))
		rw.(http.Flusher).Flush()
		select { // hold the stream open until the client goes away
		case <-r.Context().Done():
		case <-release:
		}
	})
	f, opts := wireFixture(t, w)
	s := f.open(opts)
	// Registered after the session, so it runs first: a regression that
	// leaves the stream open fails on the bound below, and then the
	// session's Close is not left waiting on a server that never lets go.
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := start(ctx, s, "hi", func(e Event) {
		if _, ok := e.(TextDelta); ok {
			cancel()
		}
	})
	if got := await(t, out, "the turn cancelled mid-stream to return"); got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high cancelled interrupted: (thinking: thinking) partial",
	})
}
