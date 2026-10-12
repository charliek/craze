package messagesapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// serverError is the *Error the client makes of an HTTP status whose body is
// an error object of typ and msg: the path every verdict is read on.
func serverError(status int, typ, msg string) *Error {
	body, err := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
	if err != nil {
		panic(err)
	}
	return statusError(&http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil)
}

// TestErrorClassification: every status and error type the API documents
// (WF §6), by HTTP status and as an error event, is transient (may be tried
// again before any output), context-too-large (compact and resend), an
// authentication failure, or none of these. x-should-retry decides either
// way when the server sent it.
func TestErrorClassification(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		e         Error
		transient bool
		context   bool
		auth      bool
	}{
		{name: "400", e: Error{StatusCode: 400, Type: "invalid_request_error"}},
		{name: "400 too long", e: *serverError(400, "invalid_request_error", "prompt is too long: 210000 tokens > 200000 maximum"), context: true},
		{name: "401", e: Error{StatusCode: 401, Type: "authentication_error"}, auth: true},
		{name: "402", e: Error{StatusCode: 402, Type: "billing_error"}},
		{name: "403", e: Error{StatusCode: 403, Type: "permission_error"}},
		{name: "404", e: Error{StatusCode: 404, Type: "not_found_error"}},
		{name: "408", e: Error{StatusCode: 408}, transient: true},
		{name: "409", e: Error{StatusCode: 409}, transient: true},
		{name: "413", e: Error{StatusCode: 413, Type: "request_too_large"}, context: true},
		{name: "429", e: Error{StatusCode: 429, Type: "rate_limit_error"}, transient: true},
		{name: "500", e: Error{StatusCode: 500, Type: "api_error"}, transient: true},
		{name: "501", e: Error{StatusCode: 501}},
		{name: "502", e: Error{StatusCode: 502}, transient: true},
		{name: "503", e: Error{StatusCode: 503}, transient: true},
		{name: "504", e: Error{StatusCode: 504}, transient: true},
		{name: "529", e: Error{StatusCode: 529, Type: "overloaded_error"}, transient: true},
		{name: "a transient type on another status", e: Error{StatusCode: 418, Type: "overloaded_error"}, transient: true},
		{name: "529 the server says not to retry", e: Error{StatusCode: 529, ShouldRetry: &no}},
		{name: "429 the server says not to retry", e: Error{StatusCode: 429, Type: "rate_limit_error", ShouldRetry: &no}},
		{name: "400 the server says to retry", e: Error{StatusCode: 400, ShouldRetry: &yes}, transient: true},
		{name: "429 too long is no overflow", e: *serverError(429, "rate_limit_error", "prompt is too long"), transient: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.Transient(); got != tc.transient {
				t.Errorf("Transient = %v, want %v", got, tc.transient)
			}
			if got := tc.e.ContextTooLarge(); got != tc.context {
				t.Errorf("ContextTooLarge = %v, want %v", got, tc.context)
			}
			if got := tc.e.AuthError(); got != tc.auth {
				t.Errorf("AuthError = %v, want %v", got, tc.auth)
			}
		})
	}
}

// TestErrorEventsTakeTheirTypesStatus: an error event arrives in a 200, so
// its status is its type's — and with it, its classification; a type craze
// does not know has none, and is not transient.
func TestErrorEventsTakeTheirTypesStatus(t *testing.T) {
	for typ, want := range map[string]struct {
		status               int
		transient, ctx, auth bool
	}{
		"invalid_request_error": {status: 400},
		"authentication_error":  {status: 401, auth: true},
		"billing_error":         {status: 402},
		"permission_error":      {status: 403},
		"not_found_error":       {status: 404},
		"request_too_large":     {status: 413, ctx: true},
		"rate_limit_error":      {status: 429, transient: true},
		"api_error":             {status: 500, transient: true},
		"overloaded_error":      {status: 529, transient: true},
		"brand_new_error":       {status: 0},
	} {
		e := streamError(&wireError{Type: []byte(`"` + typ + `"`), Message: []byte(`"m"`)}, Error{RequestID: "req_1"}, nil)
		if e.StatusCode != want.status || e.Transient() != want.transient || e.ContextTooLarge() != want.ctx || e.AuthError() != want.auth ||
			!e.InStream || e.Type != typ || e.RequestID != "req_1" || e.ShouldRetry != nil || e.RetryAfter != 0 {
			t.Errorf("%s: %+v (transient %v, context %v, auth %v), want %+v", typ, e, e.Transient(), e.ContextTooLarge(), e.AuthError(), want)
		}
	}
	e := streamError(&wireError{Type: []byte(`"invalid_request_error"`), Message: []byte(`"prompt is too long: 1050000 tokens > 1000000 maximum"`)}, Error{}, nil)
	if !e.ContextTooLarge() {
		t.Error("an error event's prompt-too-long is not context-too-large")
	}
}

// TestErrorContextTooLarge: the API's words for an overflow, in a 400 or an
// invalid_request_error event, read once from the message as the server
// sent it (review c1r3 1) — so a U+FFFD between two words, which the shown
// message drops, never joins them into the words of an overflow. The
// control on "read once": an Error built by hand with those words in its
// Message is not one; its verdict was never read.
func TestErrorContextTooLarge(t *testing.T) {
	joined := "Unsupported enum literal: 'prompt is too " + string(utf8.RuneError) + "long'"
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"prompt is too long: 210000 tokens > 200000 maximum", true},
		{"Prompt is too long", true},
		{"input length and `max_tokens` exceed context limit: 190000 + 64000 > 200000, decrease input length or `max_tokens` and try again", true},
		{"messages.1.content.0: Invalid `signature` in `thinking` block", false},
		{"max_tokens: Field required", false},
		{joined, false},
	} {
		if got := serverError(400, "invalid_request_error", tc.msg).ContextTooLarge(); got != tc.want {
			t.Errorf("HTTP 400 %q: ContextTooLarge = %v, want %v", tc.msg, got, tc.want)
		}
		event := ev(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": tc.msg}})
		_, _, err := streamOf(t, newServer(t, sse(event)))
		if got := asError(t, err).ContextTooLarge(); got != tc.want {
			t.Errorf("error event %q: ContextTooLarge = %v, want %v", tc.msg, got, tc.want)
		}
	}
	if e := serverError(400, "invalid_request_error", joined); !strings.Contains(e.Message, "prompt is too long") {
		t.Fatalf("message %q: the shown text keeps the U+FFFD, so the case proves nothing", e.Message)
	}
	if e := (Error{StatusCode: 400, Message: "prompt is too long"}); e.ContextTooLarge() {
		t.Fatal("a verdict was read from a hand-built Message")
	}
}

// TestStatusErrorReadsTheReply: a refusal before the stream keeps the
// body's type and message, the request id (the header's, else the body's),
// the wait asked for and x-should-retry; a type that is no identifier is
// dropped; a body that is no error object is the message, cut.
func TestStatusErrorReadsTheReply(t *testing.T) {
	anthropicBody := `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"},"request_id":"req_body_1"}`
	t.Run("the error object", func(t *testing.T) {
		srv := newServer(t, jsonReply(429, anthropicBody, "Retry-After", "7", "X-Should-Retry", "true"))
		_, _, err := streamOf(t, srv)
		e := asError(t, err)
		if e.StatusCode != 429 || e.InStream || e.Type != "rate_limit_error" || !strings.HasPrefix(e.Message, "Number of request tokens") ||
			e.RequestID != testRequestID || e.RetryAfter != 7*time.Second || e.ShouldRetry == nil || !*e.ShouldRetry {
			t.Fatalf("error = %+v", e)
		}
	})
	t.Run("the body's request id", func(t *testing.T) {
		srv := newServer(t, jsonReply(529, anthropicBody, "Request-Id", "req with spaces"))
		_, _, err := streamOf(t, srv)
		if e := asError(t, err); e.RequestID != "req_body_1" {
			t.Fatalf("request id = %q, want the body's (the header's is no id)", e.RequestID)
		}
	})
	t.Run("the server's wait", func(t *testing.T) {
		for _, tc := range []struct {
			header []string
			want   time.Duration
		}{
			{[]string{"Retry-After-Ms", "1500", "Retry-After", "9"}, 1500 * time.Millisecond},
			{[]string{"Retry-After", "2.5"}, 2500 * time.Millisecond},
			{[]string{"Retry-After", "0"}, 0},
			{[]string{"Retry-After", "-3"}, 0},
			{[]string{"Retry-After", "soon"}, 0},
			{[]string{"Retry-After", "1e300"}, maxRetryAfter},
			{[]string{"Retry-After-Ms", "NaN"}, 0},
		} {
			h := http.Header{}
			for i := 0; i < len(tc.header); i += 2 {
				h.Set(tc.header[i], tc.header[i+1])
			}
			if got := retryAfter(h); got != tc.want {
				t.Errorf("%v: RetryAfter = %v, want %v", tc.header, got, tc.want)
			}
		}
		h := http.Header{}
		h.Set("Retry-After", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
		if got := retryAfter(h); got <= 0 || got > time.Minute {
			t.Errorf("an HTTP date a minute away = %v", got)
		}
	})
	t.Run("x-should-retry", func(t *testing.T) {
		for v, want := range map[string]string{"true": "true", "false": "false", "": "nil", "maybe": "nil"} {
			h := http.Header{}
			h.Set("X-Should-Retry", v)
			got := "nil"
			if b := shouldRetry(h); b != nil {
				got = map[bool]string{true: "true", false: "false"}[*b]
			}
			if got != want {
				t.Errorf("x-should-retry %q = %s, want %s", v, got, want)
			}
		}
	})
	t.Run("a type that is no identifier", func(t *testing.T) {
		srv := newServer(t, jsonReply(401, `{"type":"error","error":{"type":"x-api-key `+testKey+`","message":"invalid x-api-key"}}`))
		_, _, err := streamOf(t, srv)
		if e := asError(t, err); e.Type != "" || e.Message != "invalid x-api-key" || !e.AuthError() {
			t.Fatalf("error = %+v", e)
		}
	})
	t.Run("not an error object", func(t *testing.T) {
		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("  <html>bad gateway</html>\n"))
		})
		_, _, err := streamOf(t, srv)
		if e := asError(t, err); e.StatusCode != 502 || e.Message != "<html>bad gateway</html>" || e.Type != "" || !e.Transient() {
			t.Fatalf("error = %+v", e)
		}
	})
	t.Run("a long message", func(t *testing.T) {
		long := strings.Repeat("é", maxMessage)
		srv := newServer(t, jsonReply(400, `{"type":"error","error":{"type":"invalid_request_error","message":"`+long+`"}}`))
		_, _, err := streamOf(t, srv)
		e := asError(t, err)
		if len(e.Message) > maxMessage+len("…") || !strings.HasSuffix(e.Message, "…") || !strings.HasPrefix(e.Message, "éé") {
			t.Fatalf("message is %d bytes, want it cut at %d", len(e.Message), maxMessage)
		}
	})
	t.Run("a body past the read bound", func(t *testing.T) {
		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(strings.Repeat("z", maxErrorBody*2)))
		})
		_, _, err := streamOf(t, srv)
		if e := asError(t, err); len(e.Message) > maxMessage+len("…") {
			t.Fatalf("message is %d bytes", len(e.Message))
		}
	})
}

// TestErrorText: what an error reads as, for an HTTP status and an error
// event.
func TestErrorText(t *testing.T) {
	for _, tc := range []struct {
		e    Error
		want string
	}{
		{Error{StatusCode: 429, Type: "rate_limit_error", Message: "slow down"}, "messagesapi: HTTP 429 (rate_limit_error): slow down"},
		{Error{StatusCode: 502}, "messagesapi: HTTP 502"},
		{Error{InStream: true, StatusCode: 529, Type: "overloaded_error", Message: "Overloaded"}, "messagesapi: the stream failed (overloaded_error): Overloaded"},
	} {
		if got := tc.e.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// TestErrIncompleteIsATruncatedStream: ErrIncomplete, and every protocol
// violation, unwrap to io.ErrUnexpectedEOF — what a retry policy takes for
// a stream cut short.
func TestErrIncompleteIsATruncatedStream(t *testing.T) {
	v := broke("a delta for block %d, which is not open", 3)
	if !errors.Is(ErrIncomplete, io.ErrUnexpectedEOF) || !errors.Is(v, ErrIncomplete) || !errors.Is(v, io.ErrUnexpectedEOF) {
		t.Fatal("ErrIncomplete or a violation does not unwrap to io.ErrUnexpectedEOF")
	}
	if v.Error() != "messagesapi: the stream broke the protocol: a delta for block 3, which is not open" {
		t.Fatalf("violation reads %q", v)
	}
}
