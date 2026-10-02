package responsesapi

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

// The tokens the tests hand out: dummies of 8 bytes and more that look like
// nothing real and do not overlap the redaction marker.
const (
	tokenOne = "test-token-one-0001"
	tokenTwo = "test-token-two-0002"
)

// creds is a Credentials that hands out tokens in turn: the first until it
// is invalidated, then the next. It records every Invalidate.
type creds struct {
	mu          sync.Mutex
	tokens      []string
	gen         uint64
	invalidated []uint64
	tokenErr    error // Token's answer, when set
	invalidErr  error // Invalidate's answer, when set
}

func newCreds(tokens ...string) *creds { return &creds{tokens: tokens} }

func (c *creds) Token(context.Context) (string, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokenErr != nil {
		return "", 0, c.tokenErr
	}
	i := min(int(c.gen), len(c.tokens)-1)
	return c.tokens[i], c.gen, nil
}

func (c *creds) Invalidate(_ context.Context, gen uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidated = append(c.invalidated, gen)
	if c.invalidErr != nil {
		return c.invalidErr
	}
	if gen == c.gen {
		c.gen++
	}
	return nil
}

func (c *creds) invalidations() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.invalidated)
}

// request is one request as the fake server saw it.
type request struct {
	path   string
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
		s.reqs = append(s.reqs, request{path: r.URL.Path, header: r.Header.Clone(), body: body})
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

func (s *server) requests() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// client is a client aimed at the server with c's tokens.
func (s *server) client(t *testing.T, c Credentials) *Client {
	t.Helper()
	cl, err := NewClient(Config{BaseURL: s.srv.URL + "/v1", Credentials: c})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cl
}

// events is a reply streaming each event as one SSE event, the way the
// route does: an "event:" line naming its type, a "data:" line, a blank line
// — and, like the route, no Content-Type header at all (the nil value keeps
// net/http from sniffing one in).
func events(evs ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
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

// jsonReply is a reply with status and body as JSON.
func jsonReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// The events themselves, in the route's shapes (the spike's captures).

func ev(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func created() string {
	return ev(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}}})
}

func added(idx int, item map[string]any) string {
	return ev(map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item})
}

func itemDone(idx int, item map[string]any) string {
	return ev(map[string]any{"type": "response.output_item.done", "output_index": idx, "item": item})
}

func message(id, text string) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
	}
	return map[string]any{"type": "message", "id": id, "role": "assistant", "content": content}
}

func reasoning(id, encrypted string, summary ...string) map[string]any {
	parts := []any{}
	for _, s := range summary {
		parts = append(parts, map[string]any{"type": "summary_text", "text": s})
	}
	item := map[string]any{"type": "reasoning", "id": id, "summary": parts, "content": []any{}}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}

func call(id, callID, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": name, "namespace": Namespace, "arguments": args}
}

func textDelta(idx int, itemID, delta string) string {
	return ev(map[string]any{"type": "response.output_text.delta", "output_index": idx, "item_id": itemID, "content_index": 0, "delta": delta})
}

func summaryDelta(idx int, itemID string, part int, delta string) string {
	return ev(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": idx, "item_id": itemID, "summary_index": part, "delta": delta})
}

func argsDelta(idx int, itemID, delta string) string {
	return ev(map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx, "item_id": itemID, "delta": delta})
}

func usageObject(input, cached, output, reasoning int) map[string]any {
	return map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": cached, "cache_write_tokens": 0},
		"output_tokens":         output,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          input + output,
	}
}

func completed(input, cached, output, reasoning int) string {
	return ev(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_1", "status": "completed", "output": []any{}, "usage": usageObject(input, cached, output, reasoning)}})
}

func failed(code, message string, usage map[string]any) string {
	r := map[string]any{"id": "resp_1", "status": "failed", "output": []any{},
		"error": map[string]any{"code": code, "message": message}}
	if usage != nil {
		r["usage"] = usage
	}
	return ev(map[string]any{"type": "response.failed", "response": r})
}

func incomplete(reason string, usage map[string]any) string {
	r := map[string]any{"id": "resp_1", "status": "incomplete", "output": []any{},
		"incomplete_details": map[string]any{"reason": reason}}
	if usage != nil {
		r["usage"] = usage
	}
	return ev(map[string]any{"type": "response.incomplete", "response": r})
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
func streamOf(t *testing.T, srv *server) ([]Event, error) {
	t.Helper()
	s, err := srv.client(t, newCreds(tokenOne)).Stream(context.Background(), Request{Model: "gpt-test", Input: []Item{UserMessage(InputText("hi"))}})
	if err != nil {
		return nil, err
	}
	return collect(t, s)
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

// kinds is evs' kinds, each with its index: "added0", "text0", "done1", …
func kinds(evs []Event) []string {
	names := map[EventKind]string{ItemAdded: "added", TextDelta: "text", ReasoningDelta: "reasoning",
		ArgumentsDelta: "args", ItemDone: "done", Completed: "completed"}
	var out []string
	for _, e := range evs {
		n := names[e.Kind]
		if e.Kind != Completed {
			n += fmt.Sprint(e.Index)
		}
		out = append(out, n)
	}
	return out
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v (%T) is not an *Error", err, err)
	}
	return e
}
