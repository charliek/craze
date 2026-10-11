package messagesapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// testKey is the key the tests send: a canary that looks like a real key's
// prefix and is not one.
const testKey = "sk-ant-canary-not-a-secret-0001"

// testRequestID is the request-id header every fake reply carries.
const testRequestID = "req_test_0001"

// request is one request as the fake server saw it.
type request struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// server answers each request with the next queued reply and keeps every
// request as it arrived.
type server struct {
	srv *httptest.Server

	mu      sync.Mutex
	replies []http.HandlerFunc
	reqs    []request
}

func newServer(t *testing.T, replies ...http.HandlerFunc) *server {
	t.Helper()
	s := &server{replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request body: %v", err)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, request{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
		var next http.HandlerFunc
		if len(s.replies) > 0 {
			next, s.replies = s.replies[0], s.replies[1:]
		}
		s.mu.Unlock()
		if next == nil {
			http.Error(w, "the test queued no reply for this request", http.StatusTeapot)
			return
		}
		w.Header().Set("Request-Id", testRequestID)
		next(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) requests() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// client is a client aimed at the server, with testKey.
func (s *server) client(t *testing.T) *Client {
	t.Helper()
	cl, err := NewClient(Config{BaseURL: s.srv.URL, APIKey: testKey})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cl
}

// sse is a reply streaming each event as one SSE event, the way the API
// does: an "event:" line naming its type, a "data:" line, a blank line.
func sse(evs ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range evs {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(e), &head)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
		}
	}
}

// withHeaders is reply with header's pairs (name, value, name, value, …)
// set on its response.
func withHeaders(reply http.HandlerFunc, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		reply(w, r)
	}
}

// raw is a reply streaming body exactly as given.
func raw(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

// jsonReply is a reply with status and body as JSON, and header's pairs
// (name, value, name, value, …) set.
func jsonReply(status int, body string, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// The events themselves, in the API's shapes (LF-3, the real capture in
// testdata/opus-tool-step.sse).

func ev(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func usageOf(input, cacheWrite, cacheRead, output int) map[string]any {
	return map[string]any{
		"input_tokens":                input,
		"cache_creation_input_tokens": cacheWrite,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": cacheWrite, "ephemeral_1h_input_tokens": 0},
		"output_tokens":               output,
		"service_tier":                "standard",
	}
}

func messageStartWith(msg map[string]any) string {
	m := map[string]any{"id": "msg_test_1", "type": "message", "role": "assistant", "model": "claude-test-1",
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usageOf(10, 0, 0, 1)}
	for k, v := range msg {
		m[k] = v
	}
	return ev(map[string]any{"type": "message_start", "message": m})
}

func messageStart() string { return messageStartWith(nil) }

func blockStart(idx int, block map[string]any) string {
	return ev(map[string]any{"type": "content_block_start", "index": idx, "content_block": block})
}

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func thinkingBlock(thinking, signature string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": thinking, "signature": signature}
}

func redactedBlock(data string) map[string]any {
	return map[string]any{"type": "redacted_thinking", "data": data}
}

func toolUseBlock(id, name string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}, "caller": map[string]any{"type": "direct"}}
}

func blockDelta(idx int, delta map[string]any) string {
	return ev(map[string]any{"type": "content_block_delta", "index": idx, "delta": delta})
}

func textDelta(idx int, text string) string {
	return blockDelta(idx, map[string]any{"type": "text_delta", "text": text})
}

func thinkingDelta(idx int, thinking string) string {
	return blockDelta(idx, map[string]any{"type": "thinking_delta", "thinking": thinking})
}

func signatureDelta(idx int, signature string) string {
	return blockDelta(idx, map[string]any{"type": "signature_delta", "signature": signature})
}

func inputDelta(idx int, partial string) string {
	return blockDelta(idx, map[string]any{"type": "input_json_delta", "partial_json": partial})
}

func blockStop(idx int) string { return ev(map[string]any{"type": "content_block_stop", "index": idx}) }

// messageDelta is a message_delta with stop as its stop reason (nil for
// null) and usage (nil for none).
func messageDelta(stop any, usage map[string]any) string {
	m := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil, "stop_details": nil}}
	if usage != nil {
		m["usage"] = usage
	}
	return ev(m)
}

func messageStop() string { return ev(map[string]any{"type": "message_stop"}) }

func ping() string { return `{"type": "ping"}` }

func errorEvent(typ, message string) string {
	return ev(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}})
}

// textReply is a whole message of one text block.
func textReply(text string) http.HandlerFunc {
	return sse(messageStart(), blockStart(0, textBlock("")), textDelta(0, text), blockStop(0),
		messageDelta("end_turn", map[string]any{"output_tokens": 2}), messageStop())
}

// minimalRequest is the smallest request Body accepts.
func minimalRequest() Request {
	return Request{Model: "claude-test-1", MaxTokens: 1024, Messages: []Message{{Role: RoleUser, Content: []Content{Text("hi")}}}}
}

// collect reads a stream to its end: every event, and the error it ended
// with.
func collect(t *testing.T, s *Stream) ([]Event, error) {
	t.Helper()
	defer s.Close()
	var out []Event
	for s.Next() {
		out = append(out, s.Event())
	}
	return out, s.Err()
}

// streamOf sends a minimal request to the server and reads its reply.
func streamOf(t *testing.T, srv *server) ([]Event, *Stream, error) {
	t.Helper()
	s, err := srv.client(t).Stream(context.Background(), minimalRequest())
	if err != nil {
		return nil, nil, err
	}
	evs, err := collect(t, s)
	return evs, s, err
}

// deltas is the text the events of kind at idx add up to.
func deltas(evs []Event, kind EventKind, idx int) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Kind == kind && e.Index == idx {
			b.WriteString(e.Delta)
		}
	}
	return b.String()
}

// kinds is evs' kinds, each block event with its index: "start0", "text0",
// "stop1", "delta", "completed", …
func kinds(evs []Event) []string {
	names := map[EventKind]string{BlockStart: "start", TextDelta: "text", ThinkingDelta: "thinking",
		InputDelta: "input", BlockStop: "stop", MessageDelta: "delta", Completed: "completed"}
	var out []string
	for _, e := range evs {
		n := names[e.Kind]
		if e.Kind != MessageDelta && e.Kind != Completed {
			n += fmt.Sprint(e.Index)
		}
		out = append(out, n)
	}
	return out
}

// stops is the blocks evs report finished, in order.
func stops(evs []Event) []ContentBlock {
	var out []ContentBlock
	for _, e := range evs {
		if e.Kind == BlockStop {
			out = append(out, e.Block)
		}
	}
	return out
}

func last(evs []Event) Event {
	if len(evs) == 0 {
		return Event{}
	}
	return evs[len(evs)-1]
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v (%T) is not an *Error", err, err)
	}
	return e
}
