package responsesapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
)

func okReply() http.HandlerFunc {
	return events(added(0, message("msg_1", "")), itemDone(0, message("msg_1", "ok")), completed(1, 0, 1, 0))
}

// TestClientReadsADetailBody: a refusal before the stream is {"detail": …},
// not the standard error object (spike: a system item, max_output_tokens),
// and its text is the error's message. Any 400 is final.
func TestClientReadsADetailBody(t *testing.T) {
	srv := newServer(t, jsonReply(http.StatusBadRequest, `{"detail":"Unsupported parameter: max_output_tokens"}`))
	_, err := streamOf(t, srv)
	e := asError(t, err)
	if e.StatusCode != 400 || !e.Detail || e.Message != "Unsupported parameter: max_output_tokens" || e.Code != "" {
		t.Fatalf("error = %+v", e)
	}
	if !e.Final() {
		t.Fatal("a 400 is not final")
	}
	if len(srv.requests()) != 1 {
		t.Fatalf("%d requests, want 1", len(srv.requests()))
	}
}

// TestClientKeepsAStructuredError: the standard error object's code, type,
// param and message are kept (the spike's bogus-effort reply).
func TestClientKeepsAStructuredError(t *testing.T) {
	srv := newServer(t, jsonReply(http.StatusBadRequest, `{"error":{"code":"invalid_value","message":"Invalid value: 'bogus'.","param":"reasoning.effort","type":"invalid_request_error"}}`))
	_, err := streamOf(t, srv)
	e := asError(t, err)
	if e.Code != "invalid_value" || e.Type != "invalid_request_error" || e.Param != "reasoning.effort" || e.Message != "Invalid value: 'bogus'." || e.Detail {
		t.Fatalf("error = %+v", e)
	}
}

// TestClientKeepsABodyThatIsNotJSON: a proxy's page is the message, cut.
func TestClientKeepsABodyThatIsNotJSON(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("  <html>bad gateway</html>\n"))
	})
	_, err := streamOf(t, srv)
	if e := asError(t, err); e.StatusCode != 502 || e.Message != "<html>bad gateway</html>" {
		t.Fatalf("error = %+v", e)
	}
}

// TestClientRenewsARefusedTokenOnce: a 401 invalidates the token the request
// carried and sends the request again with the source's next one; a second
// 401 gives up. The control: a 403 is not retried and invalidates nothing.
func TestClientRenewsARefusedTokenOnce(t *testing.T) {
	t.Run("renewed", func(t *testing.T) {
		srv := newServer(t, jsonReply(401, `{"detail":"Unauthorized"}`), okReply())
		c := newCreds(tokenOne, tokenTwo)
		s, err := srv.client(t, c).Stream(context.Background(), Request{Model: "gpt-test"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, s); err != nil {
			t.Fatal(err)
		}
		reqs := srv.requests()
		if len(reqs) != 2 || reqs[0].header.Get("Authorization") != "Bearer "+tokenOne || reqs[1].header.Get("Authorization") != "Bearer "+tokenTwo {
			t.Fatalf("%d requests, want two: the first token, then the next", len(reqs))
		}
		if string(reqs[0].body) != string(reqs[1].body) {
			t.Fatal("the retry sent another body")
		}
		if got := c.invalidations(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("invalidated %v, want [0]", got)
		}
	})
	t.Run("refused twice", func(t *testing.T) {
		srv := newServer(t, jsonReply(401, `{"detail":"Unauthorized"}`), jsonReply(401, `{"detail":"Unauthorized"}`), okReply())
		c := newCreds(tokenOne, tokenTwo)
		_, err := srv.client(t, c).Stream(context.Background(), Request{Model: "gpt-test"})
		if e := asError(t, err); e.StatusCode != 401 {
			t.Fatalf("error = %+v, want the 401", e)
		}
		if n := len(srv.requests()); n != 2 {
			t.Fatalf("%d requests, want 2", n)
		}
		if got := c.invalidations(); !slices.Equal(got, []uint64{0}) {
			t.Fatalf("invalidated %v, want [0] only", got)
		}
	})
	t.Run("renewal fails", func(t *testing.T) {
		signIn := errors.New("sign in again")
		srv := newServer(t, jsonReply(401, `{"detail":"Unauthorized"}`), okReply())
		c := newCreds(tokenOne, tokenTwo)
		c.invalidErr = signIn
		_, err := srv.client(t, c).Stream(context.Background(), Request{Model: "gpt-test"})
		if !errors.Is(err, signIn) {
			t.Fatalf("err = %v, want the source's error", err)
		}
		if n := len(srv.requests()); n != 1 {
			t.Fatalf("%d requests, want 1", n)
		}
	})
	t.Run("a 403 is not a 401", func(t *testing.T) {
		srv := newServer(t, jsonReply(403, `{"error":{"code":"subscription_sharing_user_not_eligible","message":"not eligible"}}`), okReply())
		c := newCreds(tokenOne, tokenTwo)
		_, err := srv.client(t, c).Stream(context.Background(), Request{Model: "gpt-test"})
		if e := asError(t, err); e.StatusCode != 403 || !e.Final() {
			t.Fatalf("error = %+v", e)
		}
		if n, inv := len(srv.requests()), c.invalidations(); n != 1 || len(inv) != 0 {
			t.Fatalf("%d requests and invalidations %v, want 1 and none", n, inv)
		}
	})
}

// TestClientNeedsAToken: a source that has no token fails the request before
// anything is sent, its error intact.
func TestClientNeedsAToken(t *testing.T) {
	signedOut := errors.New("signed out")
	srv := newServer(t, okReply())
	c := newCreds(tokenOne)
	c.tokenErr = signedOut
	_, err := srv.client(t, c).Stream(context.Background(), Request{Model: "gpt-test"})
	if !errors.Is(err, signedOut) || len(srv.requests()) != 0 {
		t.Fatalf("err = %v after %d requests, want the source's error and none", err, len(srv.requests()))
	}
}

// TestClientSendsTheRequestsHeaders: session-id (the session's id, P18)
// goes as the request gives it; the client's own headers — the bearer, the
// body's type, the stream asked for — win over anything the request says,
// and the OpenAI organization and project headers never go. The control: a
// request with no headers sends no session-id.
func TestClientSendsTheRequestsHeaders(t *testing.T) {
	srv := newServer(t, okReply(), okReply())
	cl := srv.client(t, newCreds(tokenOne))
	for _, req := range []Request{
		{Model: "gpt-test", Headers: map[string]string{
			"session-id":          "sess-123",
			"Authorization":       "Bearer not-the-token",
			"content-type":        "text/plain",
			"OpenAI-Organization": "org-owner",
			"OpenAI-Project":      "proj-owner",
			"X-Extra":             "kept",
		}},
		{Model: "gpt-test"},
	} {
		s, err := cl.Stream(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, s); err != nil {
			t.Fatal(err)
		}
	}
	reqs := srv.requests()
	h := reqs[0].header
	for name, want := range map[string]string{
		"Session-Id":          "sess-123",
		"Authorization":       "Bearer " + tokenOne,
		"Content-Type":        "application/json",
		"Accept":              "text/event-stream",
		"User-Agent":          "craze",
		"X-Extra":             "kept",
		"Openai-Organization": "",
		"Openai-Project":      "",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if reqs[0].path != "/v1/responses" {
		t.Errorf("path = %q", reqs[0].path)
	}
	if got := reqs[1].header.Get("Session-Id"); got != "" {
		t.Errorf("a request with no headers sent session-id %q", got)
	}
}

// TestClientRefusesAnotherBaseURL: the bearer goes to api.openai.com, or to
// a loopback server in a test, and nowhere else (P37).
func TestClientRefusesAnotherBaseURL(t *testing.T) {
	for base, ok := range map[string]bool{
		"":                               true,
		"https://api.openai.com/v1":      true,
		"http://127.0.0.1:8080/v1":       true,
		"http://[::1]:8080/v1":           true,
		"http://localhost:8080/v1":       true,
		"http://api.openai.com/v1":       false, // not https
		"https://api.openai.com.evil/v1": false,
		"https://example.com/v1":         false,
		"https://user:pw@api.openai.com": false,
		"https://api.openai.com/v1?k=v":  false,
		"http://192.168.1.10/v1":         false,
		"::not a url":                    false,
	} {
		_, err := NewClient(Config{BaseURL: base, Credentials: newCreds(tokenOne)})
		if (err == nil) != ok {
			t.Errorf("base %q: err = %v, want accepted %v", base, err, ok)
		}
		if !ok && !errors.Is(err, ErrBaseURL) {
			t.Errorf("base %q: err = %v, want ErrBaseURL", base, err)
		}
	}
}

// TestClientNeverFollowsARedirect: a 3xx is a failed request; the place it
// points to never sees the request or its bearer.
func TestClientNeverFollowsARedirect(t *testing.T) {
	elsewhere := newServer(t, okReply())
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/v1/responses", http.StatusTemporaryRedirect)
	})
	_, err := streamOf(t, srv)
	if e := asError(t, err); e.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("error = %+v, want the 307 itself", e)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("the redirect's target saw %d requests", n)
	}
}
