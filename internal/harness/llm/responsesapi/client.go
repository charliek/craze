// Package responsesapi is craze's own client for OpenAI's Responses API as
// the ChatGPT plan reaches it through Sign in with ChatGPT (plan 033 §3.9,
// P17): the request craze sends, the stream it reads back, and the errors it
// tells apart, over net/http and the standard library alone.
//
// It imports neither Fantasy nor openai-go (owner decision 14; deps_test.go
// holds the line). Fantasy's own Responses model mishandles this route —
// gpt-6 taken for a non-reasoning model, max_output_tokens always sent,
// reasoning never replayed, encrypted reasoning read from the wrong event
// (discovery/chatgpt-craze.md §3) — and an SDK's defaults are a liability on
// a route that rejects fields a general API accepts. Package llm adapts this
// package to Fantasy in one file (responses_adapter.go), so leaving Fantasy
// later rewrites that file and nothing here. The design follows Fantasy's
// Apache-2.0 responses_language_model.go; see NOTICE.
//
// What the route needs, verified live (the plan's spike FINDINGS.md, and
// OpenAI's Sign in with ChatGPT docs):
//
//   - The body (Request.Body): store false and stream true on every request;
//     the system prompt in instructions, never a system item (a 400); every
//     function tool inside one namespace (P32); reasoning's encrypted content
//     included, so it can be replayed; and none of the fields the route
//     rejects or ignores — max_output_tokens above all (a 400), so the
//     harness's output ceiling (D-74) is never sent here.
//   - The stream (Stream): no Content-Type header on the reply, so the reader
//     never asks for one; response.completed's output is empty, so items are
//     assembled from each response.output_item.done, by output_index; and
//     success is response.completed alone.
//   - Errors (Error): a {"detail": …} body before the stream, the standard
//     error object after; which failures are final (FinalError in package
//     llm) and which may be tried again before any output.
//   - Credentials: a bearer from a token source (Credentials), renewed once
//     on a 401; the session-id header that gives the server its cache
//     affinity (P18) arrives with the request's headers.
package responsesapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// DefaultBaseURL is the one API the ChatGPT plan's requests go to (P37):
// credentialed HTTP goes only to this fixed HTTPS origin.
const DefaultBaseURL = "https://api.openai.com/v1"

// defaultUserAgent names craze to the server when the request names nothing
// else. The harness may not import internal/version (D-02), so it carries
// no version.
const defaultUserAgent = "craze"

// maxErrorBody bounds how much of a failed response's body is read: enough
// for any error object or detail, and a proxy's error page is cut.
const maxErrorBody = 64 << 10

// Credentials is where a request's bearer comes from (llm.Auth, without the
// values the scrubber reads): Token is the token for the next request and
// gen names it; Invalidate tells the source the server refused gen, so it
// adopts or mints a newer one before Token is asked again. Both may be called
// from several goroutines at once.
type Credentials interface {
	Token(ctx context.Context) (token string, gen uint64, err error)
	Invalidate(ctx context.Context, gen uint64) error
}

// Config is what NewClient builds a client from.
type Config struct {
	// BaseURL is the API's base: "" is DefaultBaseURL, the only one
	// production uses. A loopback http or https URL is accepted too, for
	// tests (the same rule as the sign-in's test overrides, plan 033 §3.10);
	// anything else is refused, so a misconfiguration cannot carry the bearer
	// to another host.
	BaseURL string
	// HTTPClient sends the requests; nil is a fresh client. It is copied with
	// redirects refused (P37): a 3xx is a failed request, never followed, so
	// the Authorization header goes to the endpoint and nowhere else.
	HTTPClient *http.Client
	// Credentials is required.
	Credentials Credentials
	// UserAgent is the default User-Agent; "" is "craze".
	UserAgent string
}

// Client sends Responses requests. It is safe for concurrent use.
type Client struct {
	endpoint  string // <base>/responses
	http      *http.Client
	creds     Credentials
	userAgent string
}

// ErrBaseURL is NewClient's refusal of a base URL that is neither
// DefaultBaseURL's host nor a loopback one. The URL is not echoed.
var ErrBaseURL = errors.New("responsesapi: the base URL must be https://api.openai.com or a loopback URL")

// NewClient builds a client from cfg.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Credentials == nil {
		return nil, errors.New("responsesapi: no credentials")
	}
	endpoint, err := endpointOf(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		hc = &c
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ua := cfg.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return &Client{endpoint: endpoint, http: hc, creds: cfg.Credentials, userAgent: ua}, nil
}

// endpointOf is base's responses endpoint, or ErrBaseURL.
func endpointOf(base string) (string, error) {
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrBaseURL
	}
	switch {
	case u.Scheme == "https" && u.Host == "api.openai.com":
	case (u.Scheme == "http" || u.Scheme == "https") && loopback(u.Hostname()):
	default:
		return "", ErrBaseURL
	}
	return strings.TrimSuffix(u.String(), "/") + "/responses", nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protectedHeaders are the request headers a Request's Headers never set:
// the credential and the framing are the client's own, a cookie has no
// business here, and the OpenAI organization and project headers — which
// OpenAI's SDKs add from the environment — must not reach the plan's API
// with the owner's API identifiers (plan 033 §3.10).
var protectedHeaders = map[string]bool{
	"Authorization":       true,
	"Content-Type":        true,
	"Content-Length":      true,
	"Accept":              true,
	"Host":                true,
	"Cookie":              true,
	"Connection":          true,
	"Transfer-Encoding":   true,
	"Openai-Organization": true,
	"Openai-Project":      true,
}

// Stream sends req and returns its event stream, once the server has
// answered 2xx. A server's refusal before the stream is an *Error carrying
// its status; a 401 first invalidates the token the request carried and
// sends the request again, once, with the source's next token. An error from
// the credential source is returned wrapped, its sentinel intact.
//
// The caller must Close the stream.
func (c *Client) Stream(ctx context.Context, req Request) (*Stream, error) {
	body, err := req.Body()
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, body, req)
	if err != nil {
		return nil, err
	}
	return newStream(ctx, resp), nil
}

func (c *Client) send(ctx context.Context, body []byte, req Request) (*http.Response, error) {
	for retried := false; ; retried = true {
		token, gen, err := c.creds.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("responsesapi: no token for the request: %w", err)
		}
		if token == "" {
			return nil, errors.New("responsesapi: the credential source gave an empty token")
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("responsesapi: building the request: %w", err)
		}
		c.setHeaders(hreq.Header, token, req)
		resp, err := c.http.Do(hreq)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !retried {
			discard(resp)
			if err := c.creds.Invalidate(ctx, gen); err != nil {
				return nil, fmt.Errorf("responsesapi: the server refused the token, and it could not be renewed: %w", err)
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, statusError(resp)
		}
		return resp, nil
	}
}

// setHeaders sets a request's headers: the request's own first (session-id
// among them), then the client's, which win — the bearer, the JSON body and
// the event stream asked for.
func (c *Client) setHeaders(h http.Header, token string, req Request) {
	for k, v := range req.Headers {
		if protectedHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		h.Set(k, v)
	}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	ua := req.UserAgent
	if ua == "" {
		ua = c.userAgent
	}
	h.Set("User-Agent", ua)
}

// discard reads what is left of a response it will not use, within
// maxErrorBody, so its connection can be reused, and closes it.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}
