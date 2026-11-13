package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// The MCP caller (plan 051 §B, C1): replies as JSON and as SSE, what is an
// answer and what is a failure, the body cap, the deadline, cancellation and
// redirects — each through the service, as the tools will see it.

// TestTheRequest: a search and a fetch are each one POST of a JSON-RPC
// tools/call — Exa's tool, its arguments, a fresh id — with the content
// type, the accept list and craze's User-Agent; the key goes in x-api-key
// and nowhere else: not the URL, the body or an Authorization header.
func TestTheRequest(t *testing.T) {
	const key = "exa-key-0123456789abcdef"
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendJSON(w, resultJSON(rec.Call.ID, false, ""))
	})
	s := keyed(t, f, "  "+key+"\n", 0)
	if _, err := search(s, "go 1.27 release"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, err := s.Fetch(context.Background(), "https://go.dev/doc/devel/release"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := f.calls()
	if len(got) != 2 {
		t.Fatalf("%d requests, want 2", len(got))
	}
	for i, rec := range got {
		if rec.Method != http.MethodPost || rec.Path != "/mcp" || rec.RawQuery != "" {
			t.Errorf("request %d: %s %s?%s", i, rec.Method, rec.Path, rec.RawQuery)
		}
		for name, want := range map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json, text/event-stream",
			"User-Agent":   "craze",
			"X-Api-Key":    key, // trimmed
		} {
			if v := rec.Header.Get(name); v != want {
				t.Errorf("request %d: %s = %q, want %q", i, name, v, want)
			}
		}
		if rec.Header.Get("Authorization") != "" || rec.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("request %d carries an Authorization or session header", i)
		}
		if strings.Contains(string(rec.Body), key) {
			t.Errorf("request %d: the body carries the key", i)
		}
		if rec.Call.JSONRPC != "2.0" || rec.Call.Method != "tools/call" || rec.Call.ID != int64(i+1) {
			t.Errorf("request %d: %+v", i, rec.Call)
		}
	}
	search, fetch := got[0].Call.Params, got[1].Call.Params
	if search.Name != "web_search_exa" || fmt.Sprint(search.Arguments) != "map[numResults:8 objective:go 1.27 release query:go 1.27 release]" {
		t.Errorf("search called %s with %v", search.Name, search.Arguments)
	}
	if fetch.Name != "web_fetch_exa" || fmt.Sprint(fetch.Arguments) != "map[maxCharacters:50000 urls:[https://go.dev/doc/devel/release]]" {
		t.Errorf("fetch called %s with %v", fetch.Name, fetch.Arguments)
	}
}

// TestJSONReply: a reply sent as one JSON object, its media type with
// parameters, is read.
func TestJSONReply(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, resultJSON(rec.Call.ID, false, "# Page\nURL: https://a.example/\n\nbody"))
	})
	page, err := overridden(t, f, "").Fetch(context.Background(), "https://a.example/")
	if err != nil || page.Text != "# Page\nURL: https://a.example/\n\nbody" || page.URL != "https://a.example/" {
		t.Fatalf("Fetch = %+v, %v", page, err)
	}
}

// TestSSEReply: in a stream, comment lines are ignored, an event's data lines
// are joined with a newline, LF, CRLF and CR all end a line, and the answer
// is the first message event that answers the call's id — a notification, a
// request of the server's, an answer to another id, an event of another type
// and data that is not JSON are skipped. The stream is not read past the
// answer: a server that keeps it open does not hold the call.
func TestSSEReply(t *testing.T) {
	answer := resultJSON(1, false, "# T\nURL: https://a.example/\n\nline one\nline two")
	// The answer split across data lines at a point JSON allows a newline.
	split := strings.Replace(answer, `,"jsonrpc"`, "\ndata: ,\"jsonrpc\"", 1)
	split = strings.ReplaceAll(split, "\n", "\r\n")
	stream := ": a comment\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"sampling/createMessage\",\"params\":{}}\n\n" +
		"event: message\ndata: " + resultJSON(7, false, "another call's") + "\n\n" +
		"event: other\ndata: " + resultJSON(1, false, "not a message event") + "\n\n" +
		"data: not json\r\r" +
		"event: message\r\n:another comment\r\ndata: " + split + "\r\n\r\n"
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ received) {
		sendSSE(w, stream)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the stream stays open
	})
	s := overridden(t, f, "")
	done := make(chan struct{})
	var page tool.WebPage
	var err error
	go func() {
		page, err = s.Fetch(context.Background(), "https://a.example/")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the call waited for the stream to end after its answer")
	}
	if err != nil || page.Text != "# T\nURL: https://a.example/\n\nline one\nline two" {
		t.Fatalf("Fetch = %q, %v", page.Text, err)
	}
}

// TestStreamWithNoAnswer: a stream that ends without answering the call is a
// failure, never an empty result — whether it ends after other events, with
// nothing, or in the middle of the answer's event, which a blank line never
// finished.
func TestStreamWithNoAnswer(t *testing.T) {
	for name, stream := range map[string]string{
		"only another id":     event(resultJSON(5, false, "x")),
		"nothing":             "",
		"comments only":       ": hi\n\n",
		"an unfinished event": "event: message\ndata: " + resultJSON(1, false, "x") + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ received) { sendSSE(w, stream) })
			_, err := search(overridden(t, f, ""), "q")
			wantErr(t, err, tool.ClassToolError, "Unable to search the web for q")
		})
	}
}

// TestJSONRPCErrorAndMalformedReplies: a JSON-RPC error, an envelope that is
// not one, a missing or null result, a result of the wrong shape, an answer
// to another id and a media type that is neither JSON nor a stream are each
// a failure, distinct from an empty search — which the control, an empty
// result, is. A JSON-RPC error's message is kept, after the words.
func TestJSONRPCErrorAndMalformedReplies(t *testing.T) {
	for _, tc := range []struct {
		name, body, media, want string
	}{
		{name: "a JSON-RPC error", body: errorJSON(1, "Invalid params: query\nmore"), want: "Unable to search the web for q: Invalid params: query"},
		{name: "not JSON", body: `<html>busy</html>`},
		{name: "an array", body: `[` + resultJSON(1, false, "") + `]`},
		{name: "no result", body: `{"jsonrpc":"2.0","id":1}`},
		{name: "not JSON-RPC 2.0", body: `{"id":1,"result":{"content":[]}}`},
		{name: "a null result", body: `{"jsonrpc":"2.0","id":1,"result":null}`},
		{name: "a result that is not an object", body: `{"jsonrpc":"2.0","id":1,"result":"text"}`},
		{name: "content that is not a list", body: `{"jsonrpc":"2.0","id":1,"result":{"content":"text"}}`},
		{name: "another id", body: resultJSON(2, false, "")},
		{name: "a string id", body: `{"jsonrpc":"2.0","id":"1","result":{"content":[]}}`},
		{name: "plain text", body: resultJSON(1, false, ""), media: "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ received) {
				media := tc.media
				if media == "" {
					media = "application/json"
				}
				w.Header().Set("Content-Type", media)
				_, _ = io.WriteString(w, tc.body)
			})
			want := tc.want
			if want == "" {
				want = "Unable to search the web for q"
			}
			_, err := search(overridden(t, f, ""), "q")
			wantErr(t, err, tool.ClassToolError, want)
		})
	}
	// The control: an empty result is no results, not a failure.
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendJSON(w, `{"jsonrpc":"2.0","id":`+fmt.Sprint(rec.Call.ID)+`,"result":{"content":[]}}`)
	})
	if results, err := search(overridden(t, f, ""), "q"); err != nil || results == nil || len(results) != 0 {
		t.Fatalf("an empty result: %v, %v; want no results", results, err)
	}
}

// TestBlocks: a result's text blocks are joined in order, and a block of any
// other type is left out.
func TestBlocks(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendSSE(w, event(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[`+
			`{"type":"text","text":"# T\nURL: https://a.example/\n\none "},`+
			`{"type":"image","data":"aGk=","mimeType":"image/png"},`+
			`{"type":"resource","resource":{"uri":"x://y","text":"not this"}},`+
			`{"type":"text","text":"two"}]}}`, rec.Call.ID)))
	})
	page, err := overridden(t, f, "").Fetch(context.Background(), "https://a.example/")
	if err != nil || page.Text != "# T\nURL: https://a.example/\n\none two" {
		t.Fatalf("Fetch = %q, %v", page.Text, err)
	}
}

// TestProviderErrors: an error result names its status in its text, as Exa's
// do, and is read as that status — the captured 401, a 429 with and without
// a key (the hint names EXA_API_KEY only when no key was sent), any other —
// and one that names none is unable, with the provider's first line. An HTTP
// status other than 200 is read the same, and a rate limit is never "no
// results".
func TestProviderErrors(t *testing.T) {
	const key = "exa-key-0123456789abcdef"
	isError := func(text string) replyFunc {
		return func(w http.ResponseWriter, _ *http.Request, rec received) {
			sendSSE(w, event(resultJSON(rec.Call.ID, true, text)))
		}
	}
	status := func(code int) replyFunc {
		return func(w http.ResponseWriter, _ *http.Request, _ received) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":"see https://mcp.exa.ai/mcp x-api-key: `+key+`"}`)
		}
	}
	const (
		limited     = "Web search rate limited (HTTP 429)"
		limitedHint = limited + ". Setting EXA_API_KEY raises the limit"
		auth        = "Web search authentication failed (HTTP 401)"
	)
	for _, tc := range []struct {
		name  string
		reply replyFunc
		key   string
		want  string
	}{
		{"the captured 401", replay(t, "error-401-invalid-key.body"), key, auth},
		{"isError 429, no key", isError("web_search_exa error (429): Too many requests"), "", limitedHint},
		{"isError 429, a key", isError("web_search_exa error (429): Too many requests"), key, limited},
		{"isError 500", isError("web_search_exa error (500): oops"), "", "Web search request failed (HTTP 500)"},
		{"isError, no status", isError("Something broke\nTimestamp: now"), "", "Unable to search the web for q: Something broke"},
		{"HTTP 429, no key", status(http.StatusTooManyRequests), "", limitedHint},
		{"HTTP 429, a key", status(http.StatusTooManyRequests), key, limited},
		{"HTTP 401", status(http.StatusUnauthorized), key, auth},
		{"HTTP 503", status(http.StatusServiceUnavailable), "", "Web search request failed (HTTP 503)"},
		{"HTTP 202", status(http.StatusAccepted), "", "Web search request failed (HTTP 202)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.reply)
			var s *Service
			if tc.key != "" {
				s = keyed(t, f, tc.key, 0)
			} else {
				s = overridden(t, f, "")
			}
			_, err := search(s, "q")
			wantErr(t, err, tool.ClassToolError, tc.want)
			_, err = s.Fetch(context.Background(), "https://a.example/")
			wantErr(t, err, tool.ClassToolError, strings.Replace(tc.want, "Unable to search the web for q", "Unable to fetch https://a.example/", 1))
		})
	}
}

// TestTooLargeAReply: a reply over MaxReply, as JSON or as a stream whose
// answer comes after the cap, is an output_limit failure, never a result cut
// short; a reply exactly at the cap is read whole.
func TestTooLargeAReply(t *testing.T) {
	const over = "Web search reply is larger than 256 KiB"
	padTo := func(body string, n int) string { return body + strings.Repeat(" ", n-len(body)) }
	for _, tc := range []struct {
		name  string
		reply func(id int64) (media, body string)
		ok    bool
	}{
		{"JSON over the cap", func(id int64) (string, string) {
			return "application/json", padTo(resultJSON(id, false, ""), MaxReply+1)
		}, false},
		{"JSON with a text over the cap", func(id int64) (string, string) {
			return "application/json", resultJSON(id, false, strings.Repeat("x", MaxReply))
		}, false},
		{"a stream whose answer is past the cap", func(id int64) (string, string) {
			return "text/event-stream", strings.Repeat(": keepalive\n", MaxReply/12+1) + event(resultJSON(id, false, ""))
		}, false},
		{"JSON exactly at the cap", func(id int64) (string, string) {
			return "application/json", padTo(resultJSON(id, false, ""), MaxReply)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
				media, body := tc.reply(rec.Call.ID)
				w.Header().Set("Content-Type", media)
				_, _ = io.WriteString(w, body)
			})
			results, err := search(overridden(t, f, ""), "q")
			if tc.ok {
				if err != nil || len(results) != 0 {
					t.Fatalf("Search = %v, %v; want no results", results, err)
				}
				return
			}
			wantErr(t, err, tool.ClassOutputLimit, over)
		})
	}
}

// TestAnswerEndingPastTheCap: a stream whose answer's last byte, the LF of
// the blank line that ends its event, is the first byte past MaxReply is an
// output_limit failure — the byte past the cap is never read as the reply's —
// while the same stream one byte shorter, its last byte exactly at the cap,
// is the answer.
func TestAnswerEndingPastTheCap(t *testing.T) {
	stream := func(id int64, size int) string {
		ev := event(resultJSON(id, false, ""))
		s := ":" + strings.Repeat("x", size-len(ev)-2) + "\n" + ev
		if len(s) != size || s[size-1] != '\n' || s[size-2] != '\n' {
			t.Fatalf("the stream is %d bytes, want %d ending in a blank line", len(s), size)
		}
		return s
	}
	for _, tc := range []struct {
		name string
		size int
		ok   bool
	}{
		{"its last byte past the cap", MaxReply + 1, false},
		{"its last byte at the cap", MaxReply, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
				sendSSE(w, stream(rec.Call.ID, tc.size))
			})
			s := overridden(t, f, "")
			results, err := search(s, "q")
			_, ferr := s.Fetch(context.Background(), "https://a.example/")
			if tc.ok {
				if err != nil || results == nil || len(results) != 0 || ferr != nil {
					t.Fatalf("Search = %v, %v; Fetch: %v; want no results and a page", results, err, ferr)
				}
				return
			}
			wantErr(t, err, tool.ClassOutputLimit, "Web search reply is larger than 256 KiB")
			wantErr(t, ferr, tool.ClassOutputLimit, "Web search reply is larger than 256 KiB")
		})
	}
}

// TestCROnlyEnding: a stream whose answer's event ends in CR CR, the stream
// kept open after it, is answered at once — a CR ends a line by itself, and
// the reader does not wait for the byte after it to see whether it is a LF.
func TestCROnlyEnding(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, rec received) {
		sendSSE(w, "event: message\rdata: "+resultJSON(rec.Call.ID, false, "# T\nURL: https://a.example/\n\nbody")+"\r\r")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the stream stays open
	})
	s := keyed(t, f, "", 10*time.Second)
	done := make(chan struct{})
	var page tool.WebPage
	var err error
	start := time.Now()
	go func() {
		page, err = s.Fetch(context.Background(), "https://a.example/")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the call waited for the byte after the answer's last CR")
	}
	if err != nil || page.Text != "# T\nURL: https://a.example/\n\nbody" {
		t.Fatalf("Fetch = %q, %v (after %v)", page.Text, err, time.Since(start))
	}
}

// TestCancelInsideAStalledLine: a call cancelled while the stream stalls
// after a CR inside the answer's event is aborted, never the answer.
func TestCancelInsideAStalledLine(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, rec received) {
		sendSSE(w, "event: message\ndata: "+resultJSON(rec.Call.ID, false, "")+"\r")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	})
	s := overridden(t, f, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := s.Search(ctx, "q")
	wantErr(t, err, tool.ClassAborted, tool.AbortedText)
}

// hookTransport answers every request with a JSON answer to its call whose
// body runs hook, with the request, before its first read: the reply reads
// whole and well whatever happens to the call's context meanwhile.
type hookTransport struct{ hook func(*http.Request) }

func (h hookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var call struct {
		ID int64 `json:"id"`
	}
	b, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(b, &call)
	body := resultJSON(call.ID, false, "# T\nURL: https://a.example/\n\nbody")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       &hookBody{r: strings.NewReader(body), hook: func() { h.hook(req) }},
		Request:    req,
	}, nil
}

type hookBody struct {
	r    io.Reader
	hook func()
	once sync.Once
}

func (b *hookBody) Read(p []byte) (int, error) {
	b.once.Do(b.hook)
	return b.r.Read(p)
}

func (b *hookBody) Close() error { return nil }

// TestTheContextDecides: a call whose context ended before it returned is
// aborted or timed out, as the context says, even when the reply came whole:
// a cancel or a deadline is never reported as a success.
func TestTheContextDecides(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, tc := range []struct {
		name string
		hook func(*http.Request)
		want *tool.WebError
	}{
		{"the caller cancels", func(*http.Request) { cancel() }, &tool.WebError{Class: tool.ClassAborted, Text: tool.AbortedText}},
		{"the deadline passes", func(r *http.Request) { <-r.Context().Done() }, &tool.WebError{Class: tool.ClassTimeout, Text: "Web search timed out after 50ms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Config{Endpoint: "http://127.0.0.1:9/mcp", HTTPClient: &http.Client{Transport: hookTransport{tc.hook}}, timeout: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			call := context.Background()
			if tc.want.Class == tool.ClassAborted {
				call = ctx
			}
			page, err := s.Fetch(call, "https://a.example/")
			if err == nil {
				t.Fatalf("Fetch = %+v, nil; want %+v", page, tc.want)
			}
			wantErr(t, err, tc.want.Class, tc.want.Text)
		})
	}
}

// TestMalformedResults: a result that is no object with a content list —
// empty, a null content — or a text block with no text is a failure of
// search and fetch alike, never no results or an empty page; an empty
// content list, the control, is no results and an empty page.
func TestMalformedResults(t *testing.T) {
	for _, tc := range []struct{ name, result string }{
		{"an empty result", `{}`},
		{"a null content", `{"content":null}`},
		{"no content, an error flag", `{"isError":false}`},
		{"a text block with no text", `{"content":[{"type":"text"}]}`},
		{"a text block with a null text", `{"content":[{"type":"text","text":null}]}`},
		{"a text block with a number for text", `{"content":[{"type":"text","text":1}]}`},
		{"a block that is not an object", `{"content":["text"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
				sendJSON(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, rec.Call.ID, tc.result))
			})
			s := overridden(t, f, "")
			results, err := search(s, "q")
			if err == nil {
				t.Fatalf("Search = %v, nil; want a failure", results)
			}
			wantErr(t, err, tool.ClassToolError, "Unable to search the web for q")
			page, err := s.Fetch(context.Background(), "https://a.example/")
			if err == nil {
				t.Fatalf("Fetch = %+v, nil; want a failure", page)
			}
			wantErr(t, err, tool.ClassToolError, "Unable to fetch https://a.example/")
		})
	}
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendJSON(w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[]}}`, rec.Call.ID))
	})
	s := overridden(t, f, "")
	if results, err := search(s, "q"); err != nil || results == nil || len(results) != 0 {
		t.Fatalf("an empty content list: Search = %v, %v; want no results", results, err)
	}
	if page, err := s.Fetch(context.Background(), "https://a.example/"); err != nil || page.Text != "" || page.URL != "https://a.example/" {
		t.Fatalf("an empty content list: Fetch = %+v, %v; want an empty page", page, err)
	}
}

// TestDeadline: a provider that does not answer within the call's deadline
// is a timeout, and its words name neither the endpoint nor the transport's
// error.
func TestDeadline(t *testing.T) {
	for name, stall := range map[string]func(w http.ResponseWriter, r *http.Request){
		"before the headers": func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		"inside the stream": func(w http.ResponseWriter, r *http.Request) {
			sendSSE(w, ": working\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ received) { stall(w, r) })
			s := keyed(t, f, "", 50*time.Millisecond)
			_, err := search(s, "q")
			wantErr(t, err, tool.ClassTimeout, "Web search timed out after 50ms")
		})
	}
	s, err := New(Config{})
	if err != nil || s.timeout != 25*time.Second {
		t.Fatalf("the default deadline is %v (%v), want 25s", s.timeout, err)
	}
}

// TestCancellation: a call whose context ends while it waits is aborted with
// the tool framework's words for it, and a context already done sends
// nothing.
func TestCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newFake(t, func(_ http.ResponseWriter, r *http.Request, _ received) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	s := overridden(t, f, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := s.Search(ctx, "q")
	wantErr(t, err, tool.ClassAborted, tool.AbortedText)

	_, err = s.Fetch(ctx, "https://a.example/")
	wantErr(t, err, tool.ClassAborted, tool.AbortedText)
	if n := len(f.calls()); n != 1 {
		t.Fatalf("%d requests, want only the first: a done context sends nothing", n)
	}
}

// TestRedirectsAreRefused: a 3xx is a failed call, never followed — the key
// goes to the endpoint and nowhere else.
func TestRedirectsAreRefused(t *testing.T) {
	var hits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		sendJSON(w, resultJSON(1, false, ""))
	}))
	defer elsewhere.Close()
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ received) {
			http.Redirect(w, r, elsewhere.URL+"/mcp", code)
		})
		_, err := search(keyed(t, f, "exa-key-0123456789abcdef", 0), "q")
		wantErr(t, err, tool.ClassToolError, fmt.Sprintf("Web search request failed (HTTP %d)", code))
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("a redirect was followed %d times", n)
	}
}

// TestTransportErrorsCarryNoURL: a call that cannot reach the endpoint says
// it was unable, and never the transport's error, whose text names the
// endpoint's address.
func TestTransportErrorsCarryNoURL(t *testing.T) {
	f := newFake(t, func(http.ResponseWriter, *http.Request, received) {})
	addr := f.srv.Listener.Addr().String()
	s := overridden(t, f, "")
	f.srv.Close()
	_, err := search(s, "q")
	wantErr(t, err, tool.ClassToolError, "Unable to search the web for q")
	_, err = s.Fetch(context.Background(), "https://a.example/x")
	wantErr(t, err, tool.ClassToolError, "Unable to fetch https://a.example/x")
	if strings.Contains(err.Error(), addr) || strings.Contains(err.Error(), "dial") {
		t.Fatalf("the error %q names the transport", err)
	}
}

// TestFailureWordsAreFixed: the internal failure's own text names no status
// detail beyond its kind, so a failure that escapes a provider unworded
// still carries nothing of the reply but its scrubbed detail.
func TestFailureWordsAreFixed(t *testing.T) {
	for _, tc := range []struct {
		f    failure
		want string
	}{
		{failure{kind: failStatus, status: 418}, "websearch: HTTP 418"},
		{failure{kind: failTimeout}, "websearch: timed out"},
		{failure{kind: failAborted}, "websearch: aborted"},
		{failure{kind: failTooLarge}, "websearch: reply too large"},
		{failure{kind: failOther}, "websearch: call failed"},
		{failure{kind: failOther, detail: "d"}, "websearch: call failed: d"},
	} {
		if got := tc.f.Error(); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.f, got, tc.want)
		}
	}
	if !isWebError((&Service{timeout: time.Second}).webError(io.EOF, "u")) {
		t.Fatal("an unknown error is not worded")
	}
}
