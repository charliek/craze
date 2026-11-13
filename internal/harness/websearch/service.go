// Package websearch is the native harness's web search service (plan 051
// §B): what the websearch and webfetch tools reach the web through, behind
// tool.Web. It calls a search provider's hosted MCP endpoint — Exa's, the
// only provider so far — with one JSON-RPC tools/call per search or fetch
// (the MCP caller, mcp.go), and puts every failure in words the model can be
// shown (tool.WebError).
//
// Of craze it imports only the tool framework (the seam it implements),
// internal/atomicfile, the Messages API client's key pattern
// (messagesapi.KeyPattern, the one definition of how a key a server echoes
// is found) and the redactor's marker: never the parent harness, the agent
// or the TUI (deps_test.go, and the websearch depguard rule).
//
// The key: optional, sent only as the x-api-key header, only to the
// provider's own endpoint — never under the test override — and only when
// the session's redactor could hold it (usable). Every text a reply gives,
// success or error, has the key taken out before anything reads it
// (scrubber), and no error the service returns carries a cause, a request
// URL, a transport error's text or a header.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// TestEndpointEnv names the test override of the provider's endpoint: a
// loopback http URL, honoured for no other value (Override).
const TestEndpointEnv = "CRAZE_TEST_EXA_MCP"

// ErrOverride is a TestEndpointEnv, or a Config.Endpoint, that is not a
// loopback http URL. The value is not echoed.
var ErrOverride = errors.New("websearch: " + TestEndpointEnv + " may name only a loopback http URL (http://127.0.0.1:<port>/<path>)")

// ErrUnknownProvider is a Config.Provider the table does not have.
var ErrUnknownProvider = errors.New("websearch: unknown provider")

// Override is the test override of the provider's endpoint for this process:
// "" when TestEndpointEnv is unset or empty, its value when that is an http
// URL on a loopback IP literal (not a name, which a resolver could send
// anywhere) with no user, query or fragment — a path is kept — and
// ErrOverride for anything else, never ignored, so a test that misnames its
// fake server fails rather than reaching the provider. getenv is the
// environment (nil is os.Getenv); the agent layer reads it once and hands
// the result on, as it does messagesapi.BaseURL's.
func Override(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	v := getenv(TestEndpointEnv)
	if v == "" {
		return "", nil
	}
	return loopbackURL(v)
}

// loopbackURL is v when it is an http URL on a loopback IP literal with
// nothing but a path besides, without a trailing slash; else ErrOverride.
func loopbackURL(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", ErrOverride
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return "", ErrOverride
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// Config is what New builds a service from.
type Config struct {
	// Provider is the provider's id in the table (Providers); "" is
	// DefaultProvider.
	Provider string
	// Key is the provider key, optional: sent in the x-api-key header, only
	// to the provider's own endpoint, and only when usable — at least the
	// shortest key craze accepts and clear of the redaction marker, so one
	// the session's redactor holds (modeltable.KeyProblem's rule). An
	// unusable key is never sent, as if there were none. It is trimmed.
	Key string
	// Endpoint is the test override (Override's value): a loopback http URL
	// the provider is called at instead of its own. The key is never sent
	// to it. "" is the provider's own endpoint.
	Endpoint string
	// HTTPClient sends the calls; nil is a fresh client. It is copied with
	// redirects refused. CallTimeout bounds each call whatever it sets.
	HTTPClient *http.Client

	// timeout replaces CallTimeout, for the package's tests.
	timeout time.Duration
}

// Service is the web search service: a tool.Web over one provider. It is
// safe for concurrent use. It asks no one anything yet: consent comes with
// the tools being offered (plan 051 §C).
type Service struct {
	p       Provider
	keyEnv  string
	keyed   bool // a key goes with every call
	timeout time.Duration
	// scrub is the caller's scrubber, for the words of a failure, which
	// carry the query or the page's URL as the caller gave it.
	scrub *scrubber
}

var _ tool.Web = (*Service)(nil)

// New builds the service cfg describes: ErrUnknownProvider for a provider
// the table does not have, ErrOverride for an Endpoint that is not a
// loopback http URL.
func New(cfg Config) (*Service, error) {
	id := cfg.Provider
	if id == "" {
		id = DefaultProvider
	}
	e, ok := providers[id]
	if !ok {
		return nil, ErrUnknownProvider
	}
	endpoint, configured, key := e.endpoint, strings.TrimSpace(cfg.Key), ""
	if cfg.Endpoint != "" {
		u, err := loopbackURL(cfg.Endpoint)
		if err != nil {
			return nil, err
		}
		endpoint = u
	} else if usable(configured) {
		key = configured
	}
	timeout := cfg.timeout
	if timeout <= 0 {
		timeout = CallTimeout
	}
	// The scrubber holds the configured key whether or not it is sent: a
	// server has no business echoing it, and one that does is scrubbed.
	scrub := newScrubber(configured)
	c := newCaller(endpoint, key, scrub, cfg.HTTPClient, timeout)
	return &Service{p: e.build(c), keyEnv: e.keyEnv, keyed: key != "", timeout: timeout, scrub: scrub}, nil
}

// Search runs query on the provider. Every error is a *tool.WebError.
func (s *Service) Search(ctx context.Context, query string) ([]tool.WebResult, error) {
	results, err := s.p.Search(ctx, query)
	if err != nil {
		return nil, s.webError(err, "Unable to search the web for "+query)
	}
	if results == nil {
		results = []tool.WebResult{}
	}
	return results, nil
}

// Fetch fetches the page at pageURL through the provider. Every error is a
// *tool.WebError.
func (s *Service) Fetch(ctx context.Context, pageURL string) (tool.WebPage, error) {
	page, err := s.p.Fetch(ctx, pageURL)
	if err != nil {
		return tool.WebPage{}, s.webError(err, "Unable to fetch "+pageURL)
	}
	return page, nil
}

// webError is err in the model's words (plan 051 §B): a rate limit, an
// authentication failure and any other status by their HTTP status; the
// call's deadline as a timeout, a reply over MaxReply as an output limit, a
// cancel as an abort; and anything else as unable, the words the caller
// passes, followed by the provider's own (scrubbed and cut) when it gave
// any, the whole scrubbed again: the caller's words carry the query or URL
// the caller gave, which may hold the key. A rate limit is never presented
// as no results. Nothing of err's cause is kept.
func (s *Service) webError(err error, unable string) *tool.WebError {
	var f *failure
	if !errors.As(err, &f) {
		f = &failure{kind: failOther}
	}
	switch f.kind {
	case failAborted:
		return &tool.WebError{Class: tool.ClassAborted, Text: tool.AbortedText}
	case failTimeout:
		return &tool.WebError{Class: tool.ClassTimeout, Text: fmt.Sprintf("Web search timed out after %s", s.timeout)}
	case failTooLarge:
		return &tool.WebError{Class: tool.ClassOutputLimit, Text: fmt.Sprintf("Web search reply is larger than %d KiB", MaxReply>>10)}
	case failStatus:
		switch f.status {
		case http.StatusTooManyRequests:
			text := "Web search rate limited (HTTP 429)"
			if !s.keyed {
				text += ". Setting " + s.keyEnv + " raises the limit"
			}
			return &tool.WebError{Class: tool.ClassToolError, Text: text}
		case http.StatusUnauthorized:
			return &tool.WebError{Class: tool.ClassToolError, Text: "Web search authentication failed (HTTP 401)"}
		}
		return &tool.WebError{Class: tool.ClassToolError, Text: fmt.Sprintf("Web search request failed (HTTP %d)", f.status)}
	}
	if f.detail != "" {
		unable += ": " + f.detail
	}
	return &tool.WebError{Class: tool.ClassToolError, Text: s.scrub.text(unable)}
}
