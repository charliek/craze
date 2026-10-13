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
//
// That is the ChatGPT route (RouteChatGPT), the zero value and the one every
// client was before plan 047. The key route (RouteKey, plan 047 D12) is the
// same API reached with a static API key at a configured base URL — Meta's
// Responses API, verified live (the plan's MR-1…16) — and differs only where
// the route does, each difference a policy of the route's own:
//
//   - The endpoint is the configured base URL: https on any host, or http only
//     to a loopback IP literal (ErrKeyBaseURL), the rule the model table holds
//     a file to as well, since the route sends a static key.
//   - The credential is the key (Config.APIKey): a 401 is the one request,
//     never renewed and never sent again.
//   - The body (Request.body): every function tool a plain top-level
//     function, a replayed call with no namespace (MR-9), the call's output
//     ceiling as max_output_tokens (MR-8), and the session's id as
//     prompt_cache_key, which the route needs to cache at all (MR-6).
//   - Errors (Error.Route): nothing is final everywhere; a rate limit or a
//     server's fault may be tried again before any output, in a status or
//     inside the stream, and every other refusal is left to its status.
//
// What both routes share is the rest: store false and stream true, the
// encrypted reasoning included and replayed, instructions and input_image
// data URLs, items assembled from each output_item.done (Meta's
// response.completed carries the output too, MR-10, and is never read for
// it), the usage, and redirects never followed.
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

// Route is which of the API's two routes a client speaks (see the package
// doc): every policy that differs between them is the route's.
type Route int

const (
	// RouteChatGPT is the ChatGPT plan's route, the zero value: OpenAI's
	// fixed endpoint, a sign-in's token renewed once on a 401, tools in one
	// namespace, no output ceiling and no prompt_cache_key, and the plan's
	// codes final wherever they arrive.
	RouteChatGPT Route = iota
	// RouteKey is the route for an API key (plan 047 D12): the configured
	// endpoint, the key sent once, plain function tools, max_output_tokens
	// and prompt_cache_key, and a rate limit or a server's fault retryable
	// before any output.
	RouteKey
)

// Config is what NewClient builds a client from.
type Config struct {
	// Route is the route the client speaks: the ChatGPT plan's unless set.
	Route Route
	// BaseURL is the API's base. On the ChatGPT route "" is DefaultBaseURL,
	// the only one production uses, and a loopback http or https URL is
	// accepted too, for tests (the same rule as the sign-in's test
	// overrides, plan 033 §3.10). On the key route it is required: an https
	// URL, or an http one to a loopback IP literal (ErrKeyBaseURL). Anything
	// else is refused, so a misconfiguration cannot carry the credential to
	// another host.
	BaseURL string
	// HTTPClient sends the requests; nil is a fresh client. It is copied with
	// redirects refused (P37): a 3xx is a failed request, never followed, so
	// the Authorization header goes to the endpoint and nowhere else.
	HTTPClient *http.Client
	// Credentials is the ChatGPT route's token source, required there and
	// refused on the key route.
	Credentials Credentials
	// APIKey is the key route's static key, required there and refused on the
	// ChatGPT route. It is sent as the bearer of every request, and the
	// client's errors never hold it.
	APIKey string
	// UserAgent is the default User-Agent; "" is "craze".
	UserAgent string
}

// Client sends Responses requests. It is safe for concurrent use.
type Client struct {
	route     Route
	endpoint  string // <base>/responses
	http      *http.Client
	creds     Credentials
	userAgent string
}

// ErrBaseURL is NewClient's refusal, on the ChatGPT route, of a base URL that
// is neither DefaultBaseURL's host nor a loopback one. The URL is not echoed.
var ErrBaseURL = errors.New("responsesapi: the base URL must be https://api.openai.com or a loopback URL")

// ErrKeyBaseURL is NewClient's refusal, on the key route, of a base URL the
// key may not be sent to: none at all, one that is not https — http is
// allowed only to a loopback IP literal, 127.0.0.0/8 or ::1, never a name a
// resolver answers — or one with user info, a query or a fragment. The URL is
// not echoed.
var ErrKeyBaseURL = errors.New("responsesapi: the base URL must be an https URL, or http to a loopback IP address, with no user info, query or fragment")

// NewClient builds a client from cfg.
func NewClient(cfg Config) (*Client, error) {
	var creds Credentials
	var endpoint string
	var err error
	switch cfg.Route {
	case RouteChatGPT:
		if cfg.Credentials == nil {
			return nil, errors.New("responsesapi: no credentials")
		}
		if cfg.APIKey != "" {
			return nil, errors.New("responsesapi: the ChatGPT route takes a token source, not an API key")
		}
		creds = cfg.Credentials
		endpoint, err = endpointOf(cfg.BaseURL)
	case RouteKey:
		key := strings.TrimSpace(cfg.APIKey)
		if key == "" {
			return nil, errors.New("responsesapi: no API key")
		}
		if cfg.Credentials != nil {
			return nil, errors.New("responsesapi: the key route takes an API key, not a token source")
		}
		creds = staticKey(key)
		endpoint, err = keyEndpointOf(cfg.BaseURL)
	default:
		return nil, fmt.Errorf("responsesapi: unknown route %d", cfg.Route)
	}
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
	return &Client{route: cfg.Route, endpoint: endpoint, http: hc, creds: creds, userAgent: ua}, nil
}

// staticKey is the key route's credential: the one key, every request. It is
// never invalidated — a 401 on the key route is the request's answer (send).
type staticKey string

func (k staticKey) Token(context.Context) (string, uint64, error) { return string(k), 0, nil }

func (staticKey) Invalidate(context.Context, uint64) error {
	return errors.New("responsesapi: an API key cannot be renewed")
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
	return loopbackIP(host)
}

// keyEndpointOf is base's responses endpoint on the key route, or
// ErrKeyBaseURL (plan 047 D13, P25): https on any host, or http only to a
// loopback IP literal, and nothing a key or another origin could ride in —
// the rule modeltable's keyEndpointProblem holds a file to, held here again
// for every client built.
func keyEndpointOf(base string) (string, error) {
	u, err := url.Parse(base)
	if base == "" || err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Hostname() == "" {
		return "", ErrKeyBaseURL
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopbackIP(u.Hostname()):
	default:
		return "", ErrKeyBaseURL
	}
	return strings.TrimSuffix(u.String(), "/") + "/responses", nil
}

// loopbackIP reports whether host is an IP literal in 127.0.0.0/8 or ::1.
func loopbackIP(host string) bool {
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
// answered 2xx. The body is the client's route's (Request.body). A server's
// refusal before the stream is an *Error carrying its status; on the ChatGPT
// route a 401 first invalidates the token the request carried and sends the
// request again, once, with the source's next token, and on the key route a
// 401 is the answer, the request sent once. An error from the credential
// source is returned wrapped, its sentinel intact.
//
// The caller must Close the stream.
func (c *Client) Stream(ctx context.Context, req Request) (*Stream, error) {
	body, err := req.body(c.route)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, body, req)
	if err != nil {
		return nil, err
	}
	s := newStream(ctx, resp)
	s.route = c.route
	return s, nil
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
		if resp.StatusCode == http.StatusUnauthorized && c.route == RouteChatGPT && !retried {
			discard(resp)
			if err := c.creds.Invalidate(ctx, gen); err != nil {
				return nil, fmt.Errorf("responsesapi: the server refused the token, and it could not be renewed: %w", err)
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			e := statusError(resp)
			e.Route = c.route
			return nil, e
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
