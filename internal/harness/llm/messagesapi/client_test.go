package messagesapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestClientSendsTheHeaders: every request carries the key, the API
// version, the JSON body's type, the event stream asked for and the user
// agent craze (P18); the binding beta goes exactly when the request carries
// block_binding (the controls: no thinking, and thinking without it). A
// request's own headers go too, but never over the client's — nor any
// anthropic-* header, an Authorization or a cookie.
func TestClientSendsTheHeaders(t *testing.T) {
	srv := newServer(t, textReply("a"), textReply("b"), textReply("c"))
	cl := srv.client(t)
	bound := minimalRequest()
	bound.Thinking = &ThinkingConfig{Display: "summarized", DropMismatched: true}
	bound.Headers = map[string]string{
		"x-api-key":         "sk-ant-not-the-key",
		"Anthropic-Version": "1999-01-01",
		"anthropic-beta":    "fast-mode-2026-02-01",
		"Anthropic-Other":   "x",
		"content-type":      "text/plain",
		"Accept":            "text/html",
		"User-Agent":        "not-craze",
		"Authorization":     "Bearer not-the-key",
		"Cookie":            "a=b",
		"X-Extra":           "kept",
	}
	unbound := minimalRequest()
	unbound.Thinking = &ThinkingConfig{Display: "summarized"}
	for _, req := range []Request{bound, minimalRequest(), unbound} {
		s, err := cl.Stream(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, s); err != nil {
			t.Fatal(err)
		}
	}
	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	for i, r := range reqs {
		if r.method != http.MethodPost || r.path != "/v1/messages" || r.query != "" {
			t.Errorf("request %d: %s %s?%s", i, r.method, r.path, r.query)
		}
		beta := ""
		if i == 0 {
			beta = BindingBeta
		}
		for name, want := range map[string]string{
			"X-Api-Key":         testKey,
			"Anthropic-Version": APIVersion,
			"Anthropic-Beta":    beta,
			"Content-Type":      "application/json",
			"Accept":            "text/event-stream",
			"User-Agent":        "craze",
		} {
			if got := r.header.Values(name); strings.Join(got, ",") != want {
				t.Errorf("request %d: %s = %q, want %q", i, name, got, want)
			}
		}
	}
	h := reqs[0].header
	for name, want := range map[string]string{"X-Extra": "kept", "Anthropic-Other": "", "Authorization": "", "Cookie": ""} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if want, _ := bound.Body(); string(reqs[0].body) != string(want) {
		t.Errorf("the request sent another body than Body's")
	}
}

// TestClientNeedsAKey: a client without a key sends no stream request, and
// ListModels needs its key given.
func TestClientNeedsAKey(t *testing.T) {
	srv := newServer(t, textReply("a"))
	cl, err := NewClient(Config{BaseURL: srv.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Stream(context.Background(), minimalRequest()); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Stream: %v, want ErrNoKey", err)
	}
	if _, _, err := cl.ListModels(context.Background(), ""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("ListModels: %v, want ErrNoKey", err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}
}

// TestClientNeverFollowsARedirect: a 3xx from either endpoint is a failed
// request; the place it points to never sees the request or its key (P13).
func TestClientNeverFollowsARedirect(t *testing.T) {
	elsewhere := newServer(t, textReply("a"), jsonReply(200, `{"data":[],"has_more":false}`))
	redirect := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.srv.URL+path, http.StatusTemporaryRedirect)
		}
	}
	srv := newServer(t, redirect("/v1/messages"), redirect("/v1/models"))
	_, _, err := streamOf(t, srv)
	if e := asError(t, err); e.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("messages: error = %+v, want the 307 itself", e)
	}
	_, _, err = srv.client(t).ListModels(context.Background(), testKey)
	if e := asError(t, err); e.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("models: error = %+v, want the 307 itself", e)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("the redirects' target saw %d requests", n)
	}
}

// TestNewClientRefusesAnotherBaseURL: the key goes to api.anthropic.com, or
// to a loopback server in a test, and nowhere else; the refusal does not
// echo the URL. The base is required: the caller injects it (P21).
func TestNewClientRefusesAnotherBaseURL(t *testing.T) {
	for base, ok := range map[string]bool{
		"https://api.anthropic.com":         true,
		"https://api.anthropic.com/":        true,
		"http://127.0.0.1:8080":             true,
		"http://127.0.0.1:8080/proxy":       true,
		"https://[::1]:8443":                true,
		"http://localhost:8080":             true,
		"":                                  false,
		"http://api.anthropic.com":          false, // not https
		"https://api.anthropic.com/v1":      false, // the paths are the client's
		"https://api.anthropic.com.evil":    false,
		"https://example.com":               false,
		"https://user:pw@api.anthropic.com": false,
		"https://api.anthropic.com?k=v":     false,
		"https://api.anthropic.com#f":       false,
		"http://192.168.1.10":               false,
		"::not a url":                       false,
	} {
		_, err := NewClient(Config{BaseURL: base, APIKey: testKey})
		if (err == nil) != ok {
			t.Errorf("base %q: err = %v, want accepted %v", base, err, ok)
		}
		if !ok && (!errors.Is(err, ErrBaseURL) || (base != "" && strings.Contains(err.Error(), base))) {
			t.Errorf("base %q: err = %v, want ErrBaseURL, the URL not echoed", base, err)
		}
	}
	srv := newServer(t, textReply("a"))
	cl, err := NewClient(Config{BaseURL: srv.srv.URL + "/proxy/", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	s, err := cl.Stream(context.Background(), minimalRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = collect(t, s)
	if p := srv.requests()[0].path; p != "/proxy/v1/messages" {
		t.Fatalf("path = %q, want the base's path then /v1/messages", p)
	}
}

// TestBaseURLIsLoopbackOnly (P21, P30): CRAZE_TEST_ANTHROPIC_API is honoured
// only for an http URL on a loopback IP literal, its path kept; anything
// else is ErrOverride — never ignored — and the error does not echo the
// value. Unset, the origin is production's.
func TestBaseURLIsLoopbackOnly(t *testing.T) {
	env := func(v string) func(string) string {
		return func(name string) string {
			if name == TestAPIEnv {
				return v
			}
			return ""
		}
	}
	if b, err := BaseURL(env("")); err != nil || b != DefaultBaseURL {
		t.Fatalf("unset: %q, %v", b, err)
	}
	for v, want := range map[string]string{
		"http://127.0.0.1:8080":        "http://127.0.0.1:8080",
		"http://127.0.0.1:8080/":       "http://127.0.0.1:8080",
		"http://[::1]:9":               "http://[::1]:9",
		"http://127.1.2.3:443/a/proxy": "http://127.1.2.3:443/a/proxy",
	} {
		if b, err := BaseURL(env(v)); err != nil || b != want {
			t.Errorf("%q: %q, %v; want %q", v, b, err, want)
		}
	}
	for _, bad := range []string{
		"https://127.0.0.1:8080", "http://localhost:8080", "http://example.com", "http://127.0.0.1.evil.test",
		"http://10.0.0.1:80", "http://user@127.0.0.1:80", "http://127.0.0.1:80?x=1", "http://127.0.0.1:80?",
		"http://127.0.0.1:80#f", "ftp://127.0.0.1", "127.0.0.1:80", "http://0.0.0.0:80", "https://api.anthropic.com",
		"http:127.0.0.1",
	} {
		b, err := BaseURL(env(bad))
		if !errors.Is(err, ErrOverride) || b != "" {
			t.Errorf("%q: %q, %v; want ErrOverride", bad, b, err)
		}
		if err != nil && strings.Contains(err.Error(), bad) {
			t.Errorf("%q: the error %q echoes it", bad, err)
		}
	}
	t.Setenv(TestAPIEnv, "http://127.0.0.1:1")
	if b, err := BaseURL(nil); err != nil || b != "http://127.0.0.1:1" {
		t.Fatalf("nil getenv reads the environment: %q, %v", b, err)
	}
}

// TestNewClientCopiesTheHTTPClient: the client given is copied, never
// changed — its redirect policy stays the caller's — and no overall
// timeout is added; a client formatted with %+v shows no key.
func TestNewClientCopiesTheHTTPClient(t *testing.T) {
	given := &http.Client{}
	cl, err := NewClient(Config{BaseURL: DefaultBaseURL, APIKey: testKey, HTTPClient: given})
	if err != nil {
		t.Fatal(err)
	}
	if given.CheckRedirect != nil || cl.http == given || cl.http.CheckRedirect == nil {
		t.Fatal("the caller's client was changed, or not copied")
	}
	cl, err = NewClient(Config{BaseURL: DefaultBaseURL, APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if cl.http.Timeout != 0 {
		t.Fatalf("an overall timeout of %v: a stream would be cut", cl.http.Timeout)
	}
	if s := fmt.Sprintf("%+v %v %#v", cl, *cl, *cl); strings.Contains(s, testKey) {
		t.Fatalf("formatting the client shows the key: %s", s)
	}
}

// TestClientReturnsTheContextsError: a request whose context is done fails
// with the context's error, not the transport's.
func TestClientReturnsTheContextsError(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := srv.client(t).Stream(ctx, minimalRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
}
