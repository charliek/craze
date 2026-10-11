package messagesapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	fakehttp2 "github.com/charliek/craze/internal/harness/llm/messagesapi/internal/http2"
)

// keyEcho is a transport's failure that names the key it was sent — a
// net.Error, a timeout, so a test can tell its classification outlived the
// scrub.
type keyEcho struct{ key string }

func (e *keyEcho) Error() string   { return "proxy refused the request with x-api-key " + e.key }
func (e *keyEcho) Timeout() bool   { return true }
func (e *keyEcho) Temporary() bool { return true }

// failingBody reads what it holds, then fails with err.
type failingBody struct {
	r   io.Reader
	err error
}

func (b *failingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, b.err
	}
	return n, err
}

func (b *failingBody) Close() error { return nil }

// echoTransport is a transport whose failures name the key each request
// carried: the round trip itself fails (keyEcho), or, with midStream, a 200
// whose body breaks after message_start with an error that names the key
// and wraps io.ErrUnexpectedEOF.
type echoTransport struct{ midStream bool }

func (t echoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.Header.Get("X-Api-Key")
	if !t.midStream {
		return nil, &keyEcho{key: key}
	}
	body := &failingBody{
		r:   strings.NewReader("data: " + messageStart() + "\n\n"),
		err: fmt.Errorf("connection reset reading for %s: %w", key, io.ErrUnexpectedEOF),
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: r}, nil
}

// echoClient is a client on t, with key as its own (none for "").
func echoClient(t *testing.T, tr http.RoundTripper, key string) *Client {
	t.Helper()
	cl, err := NewClient(Config{BaseURL: "http://127.0.0.1:1", APIKey: key, HTTPClient: &http.Client{Transport: tr}})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// noKey fails the test when text holds key, or its first ten bytes — what a
// cut through the key would leave.
func noKey(t *testing.T, what, text, key string) {
	t.Helper()
	if strings.Contains(text, key[:10]) {
		t.Errorf("%s carries the key: %q", what, text)
	}
}

// TestErrorsNeverCarryTheKey (P13, review c1 F1): a server or a transport
// that echoes the key — in an HTTP error's message, plain or escaped, in a
// body that is no error object, in a request id, in an error event, in a
// transport failure, in a body that breaks mid-stream — never gets it into
// an error the client returns: each text is scrubbed, and where an error is
// wrapped its cause is intact, so it classifies as before. The controls:
// each echo really arrived (the message reads [redacted] where the key
// was, the cause still holds it).
func TestErrorsNeverCarryTheKey(t *testing.T) {
	escaped := strings.ReplaceAll(testKey, "-", uEscape('-'))
	percent := strings.ReplaceAll(testKey, "-", "%2d")
	if escaped == testKey || percent == testKey {
		t.Fatal("the escaped spellings are the key itself; the test proves nothing")
	}
	for _, tc := range []struct {
		name  string
		reply http.HandlerFunc
	}{
		{"in the message", jsonReply(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key: `+testKey+`"}}`)},
		{"JSON-escaped in the message", jsonReply(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key: `+escaped+`"}}`)},
		{"in a body that is no error object", jsonReply(502, `<html>upstream said `+testKey+`</html>`)},
		{"JSON-escaped in a body that is no error object", jsonReply(502, `<html>upstream said `+escaped+`</html>`)},
		{"percent-encoded in a body", jsonReply(502, `<html>GET /?key=`+percent+`</html>`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := streamOf(t, newServer(t, tc.reply))
			e := asError(t, err)
			noKey(t, "the error", err.Error(), testKey)
			noKey(t, "the message", e.Message, testKey)
			if !strings.Contains(e.Message, redacted) {
				t.Errorf("message %q: the echo never arrived (the control)", e.Message)
			}
		})
	}

	t.Run("cut through the key", func(t *testing.T) {
		msg := strings.Repeat("x", maxMessage-4) + testKey // a cut first would keep "sk-a"
		_, _, err := streamOf(t, newServer(t, jsonReply(400, `{"type":"error","error":{"type":"invalid_request_error","message":"`+msg+`"}}`)))
		if e := asError(t, err); strings.Contains(e.Message, testKey[:4]) || !strings.HasSuffix(e.Message, "…") {
			t.Fatalf("a cut message keeps %q of the key", e.Message[len(e.Message)-20:])
		}
	})

	t.Run("in a request id", func(t *testing.T) {
		if !requestIDPattern.MatchString("req_" + testKey) {
			t.Fatal("the key would not pass for a request id; the test proves nothing")
		}
		_, _, err := streamOf(t, newServer(t, jsonReply(429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"},"request_id":"req_`+testKey+`"}`,
			"Request-Id", "req_"+testKey)))
		if e := asError(t, err); e.RequestID != "" {
			t.Fatalf("request id = %q", e.RequestID)
		}
	})

	t.Run("in an error event and the stream's request id", func(t *testing.T) {
		srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Request-Id", "req_"+testKey)
			sse(messageStart(), errorEvent("overloaded_error", "overloaded for "+testKey))(w, r)
		})
		_, s, err := streamOf(t, srv)
		e := asError(t, err)
		noKey(t, "the error", err.Error(), testKey)
		if e.RequestID != "" || s.RequestID() != "" || !strings.Contains(e.Message, redacted) || !e.Transient() {
			t.Fatalf("error = %+v, stream request id %q", e, s.RequestID())
		}
	})

	t.Run("in a transport failure", func(t *testing.T) {
		_, err := echoClient(t, echoTransport{}, testKey).Stream(context.Background(), minimalRequest())
		noKey(t, "the error", err.Error(), testKey)
		var ne net.Error
		var echo *keyEcho
		var ue *url.Error
		if errors.As(err, &echo) || errors.As(err, &ue) {
			t.Fatalf("err = %v (%T): its cause, which holds the key, is reachable", err, err)
		}
		if !errors.As(err, &ne) || !ne.Timeout() || !strings.Contains(err.Error(), redacted) {
			t.Fatalf("err = %v (%T): no longer a timeout, or the echo never arrived", err, err)
		}
	})

	t.Run("in the URL", func(t *testing.T) {
		refused := roundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })
		cl, err := NewClient(Config{BaseURL: "http://127.0.0.1:1/" + testKey, APIKey: testKey, HTTPClient: &http.Client{Transport: refused}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = cl.Stream(context.Background(), minimalRequest())
		noKey(t, "the error", err.Error(), testKey)
		var ue *url.Error
		if errors.As(err, &ue) || !strings.Contains(err.Error(), redacted) || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("err = %v: the *url.Error holding the key is reachable, or the URL never was in the text", err)
		}
	})

	bad := testKey[:7] + string([]byte{0xff}) + testKey[7:] // an invalid byte inside the key
	t.Run("an invalid byte inside the key, in a body", func(t *testing.T) {
		_, _, err := streamOf(t, newServer(t, jsonReply(502, "<html>upstream said "+bad+"</html>")))
		e := asError(t, err)
		noKey(t, "the message", e.Message, testKey)
		if !strings.Contains(e.Message, redacted) {
			t.Fatalf("message %q: the key was not found whole", e.Message)
		}
	})
	t.Run("an invalid byte inside the key, in a JSON message", func(t *testing.T) {
		_, _, err := streamOf(t, newServer(t, jsonReply(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key: `+bad+`"}}`)))
		if e := asError(t, err); !strings.Contains(e.Message, redacted) || strings.Contains(e.Message, testKey[10:]) {
			t.Fatalf("message %q: the key's rest is there, or it was never found whole", e.Message)
		}
	})
	t.Run("an invalid byte inside the key, in an error event", func(t *testing.T) {
		event := `{"type":"error","error":{"type":"overloaded_error","message":"overloaded for ` + bad + `"}}`
		_, _, err := streamOf(t, newServer(t, sse(messageStart(), event)))
		if e := asError(t, err); !strings.Contains(e.Message, redacted) || strings.Contains(e.Message, testKey[10:]) || e.StatusCode != 529 {
			t.Fatalf("error = %+v", e)
		}
	})

	t.Run("in a body that breaks mid-stream", func(t *testing.T) {
		s, err := echoClient(t, echoTransport{midStream: true}, testKey).Stream(context.Background(), minimalRequest())
		if err != nil {
			t.Fatal(err)
		}
		_, err = collect(t, s)
		noKey(t, "the error", err.Error(), testKey)
		if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), redacted) {
			t.Fatalf("err = %v: not a truncated stream any more, or the echo never arrived", err)
		}
	})
}

// TestListModelsNeverCarriesItsKey (P13, review c1 F1): ListModels runs
// outside the model wrapper, so it scrubs the key it was given — not the
// client's, which it may not have — from a status's message, a transport
// failure and a body that breaks; the transport failure keeps its cause.
func TestListModelsNeverCarriesItsKey(t *testing.T) {
	const listKey = "sk-ant-canary-list-key-0002"
	t.Run("a status", func(t *testing.T) {
		srv := newServer(t, jsonReply(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key `+listKey+`"}}`, "Request-Id", listKey))
		cl, err := NewClient(Config{BaseURL: srv.srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = cl.ListModels(context.Background(), listKey)
		e := asError(t, err)
		noKey(t, "the error", err.Error(), listKey)
		if e.RequestID != "" || !strings.Contains(e.Message, redacted) {
			t.Fatalf("error = %+v", e)
		}
	})
	t.Run("a transport failure", func(t *testing.T) {
		_, _, err := echoClient(t, echoTransport{}, "").ListModels(context.Background(), listKey)
		noKey(t, "the error", err.Error(), listKey)
		var echo *keyEcho
		var ne net.Error
		if errors.As(err, &echo) || !errors.As(err, &ne) || !ne.Timeout() ||
			!strings.Contains(err.Error(), "listing the models") || !strings.Contains(err.Error(), redacted) {
			t.Fatalf("err = %v: its cause is reachable, it is no longer a timeout, or the echo never arrived", err)
		}
	})
	t.Run("a cursor that is the key", func(t *testing.T) {
		if !modelIDPattern.MatchString(listKey) {
			t.Fatal("the key would not pass for a cursor; the test proves nothing")
		}
		srv := newServer(t, page(true, listKey, model("m-a")), page(false, "", model("m-b")))
		ms, complete, err := srv.client(t).ListModels(context.Background(), listKey)
		if err != nil || complete || len(ms) != 1 || len(srv.requests()) != 1 {
			t.Fatalf("%d models, complete %v, err %v, %d requests; want the key never sent as a cursor", len(ms), complete, err, len(srv.requests()))
		}
	})
	t.Run("a body that breaks", func(t *testing.T) {
		tr := roundTripper(func(r *http.Request) (*http.Response, error) {
			body := &failingBody{r: strings.NewReader(`{"data":[`), err: fmt.Errorf("reset for %s: %w", r.Header.Get("X-Api-Key"), io.ErrUnexpectedEOF)}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: r}, nil
		})
		_, _, err := echoClient(t, tr, "").ListModels(context.Background(), listKey)
		noKey(t, "the error", err.Error(), listKey)
		if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), redacted) {
			t.Fatalf("err = %v", err)
		}
	})
}

// uEscape is r as a JSON \u escape, upper-case hex. Built at run time: an
// escape written out in the source is one an editor or a tool may decode.
func uEscape(r rune) string { return fmt.Sprintf("%cu%04X", '\\', r) }

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestKeyPattern: the spellings a key is found in — itself, any of its
// characters as a JSON \u escape (either case) or short escape, or
// percent-encoded (either case) — and what is not it: another key, and a
// part of it alone.
func TestKeyPattern(t *testing.T) {
	const key = "sk-ant/k1"
	sec := newSecret(key)
	var allEscaped string
	for _, r := range key {
		allEscaped += uEscape(r)
	}
	for _, v := range []string{
		key,
		"sk" + uEscape('-') + `ant\/k1`,
		allEscaped,
		strings.ToLower(allEscaped),
		"sk%2Dant%2fk1",
		"sk%2d" + "ant" + strings.ToLower(uEscape('/')) + "k1",
	} {
		if v != key && strings.Contains(v, key) {
			t.Fatalf("%q holds the key plain; the case proves nothing", v)
		}
		if got := sec.text("<" + v + ">"); got != "<"+redacted+">" {
			t.Errorf("%q scrubs to %q", v, got)
		}
	}
	for _, v := range []string{"sk-ant/k2", "sk-ant", "ant/k1"} {
		if got := sec.text(v); got != v {
			t.Errorf("%q, which is not the key, scrubs to %q", v, got)
		}
	}
	if newSecret("") != nil || (*secret)(nil).text(key) != key || (*secret)(nil).err(io.EOF) != io.EOF {
		t.Error("no key must hide nothing")
	}
	if err := sec.err(io.ErrUnexpectedEOF); err != io.ErrUnexpectedEOF {
		t.Errorf("a class itself was wrapped: %T", err)
	}
}

// TestScrubbedErrorsClassify (review c1r2 2): a scrubbed error never
// reaches its cause — not even one whose text never held the key, whose
// fields still might — yet classifies as it did: it unwraps to the class its
// cause is (a context's error, a stream cut short or ended) and to nothing
// else, and a net.Error's stays one, its Timeout kept.
func TestScrubbedErrorsClassify(t *testing.T) {
	sec := newSecret(testKey)
	timeout := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, tc := range []struct {
		name    string
		cause   error
		class   error
		net     bool
		timeout bool
	}{
		{"cut short", fmt.Errorf("reading for %s: %w", testKey, io.ErrUnexpectedEOF), io.ErrUnexpectedEOF, false, false},
		{"ended", fmt.Errorf("closed: %w", io.EOF), io.EOF, false, false},
		{"cancelled", fmt.Errorf("dial: %w", context.Canceled), context.Canceled, false, false},
		{"past its deadline", fmt.Errorf("dial: %w", context.DeadlineExceeded), context.DeadlineExceeded, true, true},
		{"a network timeout", timeout, nil, true, true},
		{"a network failure", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, nil, true, false},
		{"a plain failure without the key", errors.New("tls: handshake failure"), nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sec.err(tc.cause)
			if err == tc.cause || errors.Unwrap(err) != tc.class {
				t.Fatalf("err %T unwraps to %v, want only %v", err, errors.Unwrap(err), tc.class)
			}
			var ne net.Error
			if isNet := errors.As(err, &ne); isNet != tc.net || (isNet && ne.Timeout() != tc.timeout) {
				t.Fatalf("net.Error %v (timeout %v), want %v (%v)", isNet, isNet && ne.Timeout(), tc.net, tc.timeout)
			}
			var op *net.OpError
			if errors.As(err, &op) {
				t.Fatal("the cause's own type is reachable")
			}
			noKey(t, "the error", err.Error(), testKey)
			if want := strings.ReplaceAll(tc.cause.Error(), testKey, redacted); err.Error() != want {
				t.Fatalf("text %q, want %q: only the key may change", err.Error(), want)
			}
		})
	}
}

// TestStreamCloseNeverCarriesTheKey (review c1r2 3): closing a stream whose
// body fails to close with the key in its words returns the failure
// scrubbed, its cause out of reach.
func TestStreamCloseNeverCarriesTheKey(t *testing.T) {
	tr := roundTripper(func(r *http.Request) (*http.Response, error) {
		body := &closeFails{Reader: strings.NewReader(""), err: errors.New("closing the connection for " + r.Header.Get("X-Api-Key"))}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: r}, nil
	})
	s, err := echoClient(t, tr, testKey).Stream(context.Background(), minimalRequest())
	if err != nil {
		t.Fatal(err)
	}
	err = s.Close()
	if err == nil || !strings.Contains(err.Error(), redacted) {
		t.Fatalf("Close = %v, want the failure, scrubbed", err)
	}
	noKey(t, "Close's error", err.Error(), testKey)
	if errors.Unwrap(err) != nil {
		t.Fatalf("Close's error unwraps to %v", errors.Unwrap(err))
	}
}

// closeFails reads its Reader and fails to close.
type closeFails struct {
	*strings.Reader
	err error
}

func (c *closeFails) Close() error { return c.err }

// TestClassificationIgnoresTheScrub (review c1r2 4): a key that is also a
// word the classification reads — an error type, the context limit's
// max_tokens — changes nothing it decides: the status, the type, Transient,
// AuthError and ContextTooLarge come from the server's data as it came; only
// what is shown is scrubbed.
func TestClassificationIgnoresTheScrub(t *testing.T) {
	client := func(t *testing.T, srv *server, key string) *Client {
		t.Helper()
		cl, err := NewClient(Config{BaseURL: srv.srv.URL, APIKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	run := func(t *testing.T, cl *Client) *Error {
		t.Helper()
		s, err := cl.Stream(context.Background(), minimalRequest())
		if err == nil {
			_, err = collect(t, s)
		}
		return asError(t, err)
	}
	t.Run("a key that is an error type", func(t *testing.T) {
		const key = "overloaded_error"
		srv := newServer(t, sse(messageStart(), errorEvent("overloaded_error", "busy")))
		e := run(t, client(t, srv, key))
		if e.Type != "overloaded_error" || e.StatusCode != 529 || !e.Transient() {
			t.Fatalf("error = %+v (transient %v)", e, e.Transient())
		}
		if strings.Contains(e.Error(), key) || !strings.Contains(e.Error(), redacted) {
			t.Fatalf("Error() = %q shows the key", e.Error())
		}
	})
	t.Run("a key that is the authentication type", func(t *testing.T) {
		const key = "authentication_error"
		srv := newServer(t, sse(errorEvent("authentication_error", "invalid x-api-key")))
		if e := run(t, client(t, srv, key)); e.StatusCode != 401 || !e.AuthError() || strings.Contains(e.Error(), key) {
			t.Fatalf("error = %+v", e)
		}
	})
	t.Run("a key that is max_tokens", func(t *testing.T) {
		const key = "max_tokens"
		srv := newServer(t, jsonReply(400, `{"type":"error","error":{"type":"invalid_request_error","message":"input length and `+"`max_tokens`"+` exceed context limit: 190000 + 64000 > 200000"}}`))
		e := run(t, client(t, srv, key))
		if !e.ContextTooLarge() || strings.Contains(e.Message, key) {
			t.Fatalf("error = %+v (context too large %v)", e, e.ContextTooLarge())
		}
	})
}

// plainReset is a failure with ResetError's text in a package that is no
// HTTP/2 package: the control on the type rule.
type plainReset struct{ Code uint32 }

func (e plainReset) Error() string { return fmt.Sprintf("peer reset the exchange (code %d)", e.Code) }

// TestScrubbedErrorsKeepTheirTransportClass (review c1r3 2): the cause a
// scrubbed error drops told a caller whether it was the transport's failure
// — and Fantasy retries those — so the wrapper says so itself, read from the
// cause: a net.Error anywhere in its chain, an HTTP/2 package's type anywhere
// in it (fakehttp2.ResetError, whose text names nothing), or the words of a
// stream reset, a connection error or a GOAWAY — even with a key that is
// one of those words, which the scrub takes out of the text. The controls:
// the same text in another package's type, a TLS failure, and the classes
// returned as they are, are none.
func TestScrubbedErrorsKeepTheirTransportClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		cause error
		want  bool
	}{
		{"a network error", testKey, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, true},
		{"a network error, wrapped", testKey, fmt.Errorf("reading: %w", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("broken pipe")}), true},
		{"a stream reset's words", testKey, errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer"), true},
		{"a connection error's words", testKey, errors.New("connection error: PROTOCOL_ERROR"), true},
		{"a GOAWAY's words", testKey, errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=""`), true},
		{"a key that is the words", "stream", errors.New("stream error: stream ID 1; INTERNAL_ERROR"), true},
		{"an http2 package's type", testKey, fakehttp2.ResetError{Code: 2}, true},
		{"an http2 package's type, wrapped twice", testKey, fmt.Errorf("a: %w", fmt.Errorf("b: %w", fakehttp2.ResetError{Code: 2})), true},
		{"an http2 package's type, joined, by pointer", testKey, errors.Join(errors.New("first"), &fakehttp2.ResetError{Code: 2}), true},
		{"the same text, another package's type", testKey, plainReset{Code: 2}, false},
		{"a TLS failure", testKey, errors.New("tls: failed to verify certificate"), false},
		{"cut short", testKey, fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := newSecret(tc.key).err(tc.cause)
			if got := IsTransport(err); got != tc.want {
				t.Fatalf("IsTransport(%q) = %v, want %v", err, got, tc.want)
			}
			if got := IsTransport(fmt.Errorf("messagesapi: listing the models: %w", err)); got != tc.want {
				t.Fatalf("wrapped once more: IsTransport = %v, want %v", got, tc.want)
			}
			var reset fakehttp2.ResetError
			if errors.As(err, &reset) {
				t.Fatal("the cause's own type is reachable")
			}
		})
	}
	for _, c := range classes {
		if IsTransport(newSecret(testKey).err(c)) {
			t.Errorf("%v, returned as it is, is a transport failure", c)
		}
	}
}

// TestIsHTTP2Type: any package whose path ends in /http2 — the standard
// library's own (net/http/internal/http2 since Go 1.27), x/net's — and the
// copy older standard libraries bundle into net/http, unexported there and
// named http2…, are an HTTP/2 package's; net/http's other types, and a path
// that only ends in the letters, are not. The standard library's types are
// internal and cannot be made in a test without a live HTTP/2 exchange, so
// the rule is tested on the names here and end to end in
// TestStreamResetIsATransportFailure.
func TestIsHTTP2Type(t *testing.T) {
	for _, tc := range []struct {
		pkg, name string
		want      bool
	}{
		{"net/http/internal/http2", "StreamError", true},
		{"net/http", "http2StreamError", true},
		{"net/http", "http2GoAwayError", true},
		{"net/http", "http2ConnectionError", true},
		{"golang.org/x/net/http2", "GoAwayError", true},
		{"github.com/charliek/craze/internal/harness/llm/messagesapi/internal/http2", "ResetError", true},
		{"net/http", "ProtocolError", false},
		{"example.com/nothttp2", "StreamError", false},
		{"", "http2StreamError", false},
	} {
		if got := isHTTP2Type(tc.pkg, tc.name); got != tc.want {
			t.Errorf("isHTTP2Type(%q, %q) = %v, want %v", tc.pkg, tc.name, got, tc.want)
		}
	}
}

// TestStreamResetIsATransportFailure (review c1r3 2): a real HTTP/2
// exchange whose server resets the stream mid-body — the standard
// library's own bundled stream error, which no test can build by hand —
// ends the stream with a scrubbed error that is a transport failure, and
// is not an incomplete stream (it never reached EOF).
func TestStreamResetIsATransportFailure(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+messageStart()+"\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // resets the stream: RST_STREAM on HTTP/2
	}))
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cl, err := NewClient(Config{BaseURL: srv.URL, APIKey: testKey, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := cl.Stream(context.Background(), minimalRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = collect(t, s)
	if err == nil || !IsTransport(err) || errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v (%T): want a transport failure", err, err)
	}
	if !strings.Contains(err.Error(), "stream error:") {
		t.Fatalf("err = %v: not an HTTP/2 stream reset; the test proves nothing", err)
	}
}
