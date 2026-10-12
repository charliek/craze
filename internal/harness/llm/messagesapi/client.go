// Package messagesapi is craze's own client for Anthropic's Messages API
// (plan 044 §3.2): the request craze sends, the stream it reads back, the
// errors it tells apart, and the account's model list, over net/http and the
// standard library alone.
//
// It imports neither Fantasy nor any SDK, nor package llm (owner decision 2:
// the Anthropic SDK is not linked into craze; D-02; deps_test.go holds the
// line). Package llm adapts this package to Fantasy in one file
// (messages_adapter.go), so leaving Fantasy later rewrites that file and
// nothing here. The design of the content mapping follows Fantasy's
// Apache-2.0 providers/anthropic; see NOTICE.
//
// What the API needs, verified live (plan 044 research/live-facts, LF-n):
//
//   - The body (Request.Body): stream true; adaptive thinking with its
//     display and, when asked, block_binding's drop_block (LF-4), which needs
//     the thinking-binding-controls beta header; effort as
//     output_config.effort; an explicit cache breakpoint on the last system
//     block and the top-level automatic one (LF-7, LF-10); and none of the
//     fields craze never sends (wireRequest). Identical history encodes to
//     identical bytes, so the prompt cache and the thinking signatures see
//     the same prefix.
//   - The stream (Stream): content blocks by index, assembled from their
//     start and deltas (LF-3); the usage of message_start overwritten only
//     field by field; success is message_stop after a stop reason, and any
//     break in the protocol is ErrIncomplete.
//   - Errors (Error): the {"type":"error","error":{type,message}} body before
//     the stream and the error event inside it, classified by status and type,
//     with x-should-retry and retry-after kept for the retry policy.
//   - Credentials: the API key in x-api-key, sent only to the API's origin,
//     or a test's loopback one (BaseURL), never through a redirect, and
//     taken out of every error the client returns (secret, P13).
package messagesapi

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// DefaultBaseURL is the origin every production request goes to:
// credentialed HTTP goes only to this fixed HTTPS origin.
const DefaultBaseURL = "https://api.anthropic.com"

// APIVersion is the anthropic-version header every request carries.
const APIVersion = "2023-06-01"

// BindingBeta is the beta that lets a request set thinking.block_binding
// (LF-4). It is sent exactly when the request carries block_binding.
const BindingBeta = "thinking-binding-controls-2026-08-01"

// TestAPIEnv names the test override of the API's origin (P21, P30): a
// loopback http URL, honoured for no other value.
const TestAPIEnv = "CRAZE_TEST_ANTHROPIC_API"

// defaultUserAgent names craze to the server (P18). The harness may not
// import internal/version (D-02), so it carries no version.
const defaultUserAgent = "craze"

// maxErrorBody bounds how much of a failed response's body is read: enough
// for any error object, and a proxy's error page is cut.
const maxErrorBody = 64 << 10

// ErrOverride is a TestAPIEnv that is not a loopback http URL. The value is
// not echoed.
var ErrOverride = errors.New("messagesapi: " + TestAPIEnv + " may name only a loopback http URL (http://127.0.0.1:<port>)")

// ErrBaseURL is NewClient's refusal of a base URL that is neither
// DefaultBaseURL nor a loopback one. The URL is not echoed.
var ErrBaseURL = errors.New("messagesapi: the base URL must be https://api.anthropic.com or a loopback URL")

// ErrNoKey is a request with no API key to send.
var ErrNoKey = errors.New("messagesapi: no API key")

// BaseURL is the API's origin for this process: DefaultBaseURL, or, only
// when TestAPIEnv is set, its value if that is a loopback http URL — an
// http URL on a loopback IP literal (not a name, which a resolver could send
// anywhere), with no user, query or fragment; a path is kept. Any other
// value is ErrOverride, never ignored, the chatgptauth rule (plan 033
// §3.10): a test that misnames its fake server must fail, not reach
// Anthropic with a key. getenv is the environment (nil is os.Getenv); the
// caller reads it once and injects the result (P21).
func BaseURL(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	v := getenv(TestAPIEnv)
	if v == "" {
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || !bare(u) {
		return "", ErrOverride
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return "", ErrOverride
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// Config is what NewClient builds a client from.
type Config struct {
	// BaseURL is the API's origin, required: DefaultBaseURL in production,
	// injected by the caller (BaseURL, P21). A loopback http or https URL is
	// accepted too, for tests; anything else is refused (ErrBaseURL), so a
	// misconfiguration cannot carry the key to another host.
	BaseURL string
	// APIKey is the key Stream sends in x-api-key. ListModels takes its key
	// as an argument instead, so a client without one can list.
	APIKey string
	// HTTPClient sends the requests; nil is a fresh client. It is copied
	// with redirects refused: a 3xx is a failed request, never followed, so
	// the key goes to the endpoint and nowhere else. No overall timeout is
	// added: a stream runs as long as the model writes, and the caller's
	// context bounds it.
	HTTPClient *http.Client
}

// Client sends Messages API requests. It is safe for concurrent use.
type Client struct {
	messages string // <base>/v1/messages
	models   string // <base>/v1/models
	http     *http.Client
	// key is held behind a pointer, so formatting a client with %+v prints
	// an address, never the key.
	key *apiKey
}

// apiKey is the key and the secret that takes it out of errors.
type apiKey struct {
	v   string
	sec *secret
}

// NewClient builds a client from cfg.
func NewClient(cfg Config) (*Client, error) {
	base, err := baseOf(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		hc = &c
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &Client{messages: base + "/v1/messages", models: base + "/v1/models", http: hc}
	if cfg.APIKey != "" {
		c.key = &apiKey{v: cfg.APIKey, sec: newSecret(cfg.APIKey)}
	}
	return c, nil
}

// baseOf is base without its trailing slash, or ErrBaseURL: production's
// origin exactly, or an http or https URL on a loopback host, which may
// carry a path (a test's proxy).
func baseOf(base string) (string, error) {
	u, err := url.Parse(base)
	if base == "" || err != nil || !bare(u) {
		return "", ErrBaseURL
	}
	switch {
	case u.Scheme == "https" && u.Host == "api.anthropic.com" && (u.Path == "" || u.Path == "/"):
	case (u.Scheme == "http" || u.Scheme == "https") && loopback(u.Hostname()):
	default:
		return "", ErrBaseURL
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// bare reports whether u is a scheme, a host and a path alone: no user, no
// query (not even a bare "?"), no fragment, and not opaque ("http:host").
func bare(u *url.URL) bool {
	return u.Opaque == "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protectedHeaders are the request headers a Request's Headers never set:
// the credential, the API's version and betas (every anthropic-* header,
// protectedHeader), the framing, and what the client says about itself. An
// Authorization header — an OAuth bearer — has no business on a key's
// request either.
var protectedHeaders = map[string]bool{
	"X-Api-Key":         true,
	"Authorization":     true,
	"Content-Type":      true,
	"Content-Length":    true,
	"Accept":            true,
	"User-Agent":        true,
	"Host":              true,
	"Cookie":            true,
	"Connection":        true,
	"Transfer-Encoding": true,
}

func protectedHeader(name string) bool {
	k := http.CanonicalHeaderKey(name)
	return protectedHeaders[k] || strings.HasPrefix(k, "Anthropic-")
}

// Stream sends req and returns its event stream, once the server has
// answered 2xx. A refusal before the stream is an *Error carrying its
// status; a transport failure is returned with the key taken out of its
// text, or the context's error once that is done. No error the request or
// its stream returns carries the key (P13).
//
// The caller must Close the stream.
func (c *Client) Stream(ctx context.Context, req Request) (*Stream, error) {
	if c.key == nil {
		return nil, ErrNoKey
	}
	body, err := req.Body()
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messages, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(hreq.Header, c.key.v, "text/event-stream", req.Headers)
	hreq.Header.Set("Content-Type", "application/json")
	if req.binding() {
		hreq.Header.Set("Anthropic-Beta", BindingBeta)
	}
	resp, err := c.do(ctx, hreq, c.key.sec)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(resp, c.key.sec)
	}
	return newStream(ctx, resp, c.key.sec), nil
}

// do sends hreq; a failure once ctx is done is ctx's error, and any other
// has the key taken out of its text (sec.err), its cause intact.
func (c *Client) do(ctx context.Context, hreq *http.Request, sec *secret) (*http.Response, error) {
	resp, err := c.http.Do(hreq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, sec.err(err)
	}
	return resp, nil
}

// setHeaders sets a request's headers: the request's own first, less the
// protected ones, then the client's — the key, the API version, what the
// reply should be, and the user agent.
func (c *Client) setHeaders(h http.Header, key, accept string, extra map[string]string) {
	for k, v := range extra {
		if !protectedHeader(k) {
			h.Set(k, v)
		}
	}
	h.Set("X-Api-Key", key)
	h.Set("Anthropic-Version", APIVersion)
	h.Set("Accept", accept)
	h.Set("User-Agent", defaultUserAgent)
}
