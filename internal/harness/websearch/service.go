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

	// Undecided is a session that opened with nothing decided (plan 051
	// §C.3): its first call asks the consent question, when CanAsk. False
	// is a session that opened on — by config.toml, the remembered choice or
	// the caller's flag — whose calls never ask.
	Undecided bool
	// CanAsk is the session's question policy, fixed at Open: a person is
	// asked (the ask policy, with an asker wired). An undecided service with
	// it false never asks and never writes StatePath, whatever Asker it
	// holds — an automatic first-option answer is never consent — and
	// answers DeclinedText.
	CanAsk bool
	// Asker is who the consent question goes to.
	Asker tool.Asker
	// StatePath is where the answer is remembered (StateFile in the native
	// directory); "" remembers nothing.
	StatePath string
	// Warn is told, in words that hold no path or value, when the answer
	// cannot be remembered. nil says nothing.
	Warn func(string)
	// Report is told of every call the service handled, exactly once, after
	// its outcome is known (plan 051 F.4.1): from the calling goroutine, with
	// none of the service's own locks held, so it may be called concurrently.
	// A call that failed its consent counts as handled and failed. Every URL
	// it carries has the key scrubbed out. nil reports nothing.
	Report func(tool.WebCall)

	// timeout replaces CallTimeout, for the package's tests.
	timeout time.Duration
	// backoff replaces the rate-limit wait (backoff), for the package's
	// tests.
	backoff func(retryAfter time.Duration, attempt int) time.Duration
}

// MaxInFlight is the most calls one service — a session and the children it
// shares the service with — has at the provider at once (issue #138). A
// model sends searches in parallel batches, and a batch larger than this
// queues for a slot instead of tripping the provider's rate limit; time spent
// queued is not the call's (CallTimeout starts when a slot is taken).
const MaxInFlight = 4

// Service is the web search service: a tool.Web over one provider, behind
// the session's consent (consent.go, plan 051 §C.4). It is safe for
// concurrent use.
type Service struct {
	p     Provider
	entry entry
	keyed bool // a key goes with every call
	// slots holds one token per call at the provider (MaxInFlight).
	slots chan struct{}
	// c is the provider's caller, for its retry count.
	c       *caller
	timeout time.Duration
	// scrub is the caller's scrubber, for the words of a failure, which
	// carry the query or the page's URL as the caller gave it.
	scrub *scrubber
	// consent is what every call passes before the provider is called.
	consent *consent
	// report is Config.Report, nil for none.
	report func(tool.WebCall)
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
	if cfg.backoff != nil {
		c.backoff = cfg.backoff
	}
	return &Service{
		p: e.build(c), entry: e, keyed: key != "", slots: make(chan struct{}, MaxInFlight), c: c,
		timeout: timeout, scrub: scrub, consent: newConsent(cfg, id), report: cfg.Report,
	}, nil
}

// slot waits for a slot at the provider (MaxInFlight) and returns its
// release, or an aborted error when ctx ends first.
func (s *Service) slot(ctx context.Context) (func(), *tool.WebError) {
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, &tool.WebError{Class: tool.ClassAborted, Text: tool.AbortedText}
	}
}

// Retries is how many times the service has tried a call again after a rate
// limit, over its life.
func (s *Service) Retries() int { return int(s.c.retries.Load()) }

// Search runs query on the provider, once the session's consent lets it.
// Every error is a *tool.WebError. The call is reported once, however it
// returns (reportCall).
func (s *Service) Search(ctx context.Context, query string) (results []tool.WebResult, err error) {
	call := tool.WebCall{}
	defer func() { s.reportCall(call, err) }()
	if werr := s.consent.pass(ctx); werr != nil {
		return nil, werr
	}
	release, werr := s.slot(ctx)
	if werr != nil {
		return nil, werr
	}
	results, err = func() ([]tool.WebResult, error) {
		defer release()
		return s.p.Search(ctx, query)
	}()
	if err != nil {
		return nil, s.webError(err, "Unable to search the web for "+query)
	}
	if results == nil {
		results = []tool.WebResult{}
	}
	if s.report != nil {
		for _, r := range results {
			// The provider's text was scrubbed before it was parsed (R1); a
			// second pass over the URL alone is idempotent, and the URL is
			// the one value of the reply the session keeps beyond the call.
			if u := s.scrub.text(r.URL); u != "" {
				call.URLs = append(call.URLs, u)
			}
		}
	}
	return results, nil
}

// Fetch fetches the page at pageURL through the provider, once the session's
// consent lets it. Every error is a *tool.WebError. The call is reported
// once, however it returns (reportCall).
func (s *Service) Fetch(ctx context.Context, pageURL string) (page tool.WebPage, err error) {
	call := tool.WebCall{Fetch: true}
	defer func() { s.reportCall(call, err) }()
	if werr := s.consent.pass(ctx); werr != nil {
		return tool.WebPage{}, werr
	}
	release, werr := s.slot(ctx)
	if werr != nil {
		return tool.WebPage{}, werr
	}
	page, err = func() (tool.WebPage, error) {
		defer release()
		return s.p.Fetch(ctx, pageURL)
	}()
	if err != nil {
		if f, ok := errors.AsType[*failure](err); ok && f.crawl != "" {
			return tool.WebPage{}, &tool.WebError{Class: tool.ClassToolError, Text: s.scrub.text(s.crawlText(f.crawl, pageURL))}
		}
		return tool.WebPage{}, s.webError(err, "Unable to fetch "+pageURL)
	}
	// The requested URL, as the tool accepted it and with the key scrubbed
	// out (F.4.1), never one parsed from the provider's text; a page whose
	// text was empty is a fetch that contributes none.
	if s.report != nil && strings.TrimSpace(page.Text) != "" {
		if u := s.scrub.text(pageURL); u != "" {
			call.URLs = []string{u}
		}
	}
	return page, nil
}

// reportCall is Config.Report, deferred by Search and Fetch so that every
// handled call is reported exactly once, whichever way it returns — failed,
// with no URLs, when err is set — from the calling goroutine, holding no
// lock of the service's.
func (s *Service) reportCall(call tool.WebCall, err error) {
	if s.report == nil {
		return
	}
	if err != nil {
		call.Failed, call.URLs = true, nil
	}
	s.report(call)
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
			text := "Web search rate limited (HTTP 429). Wait a few seconds before searching again, and send fewer searches at once."
			if !s.keyed {
				text = "Web search rate limited (HTTP 429) on " + s.entry.name + "'s keyless endpoint. Wait a few seconds before searching again. " + s.keyHint("raises the limit") + "."
			}
			return &tool.WebError{Class: tool.ClassToolError, Text: text}
		case http.StatusUnauthorized:
			return &tool.WebError{Class: tool.ClassToolError, Text: "Web search authentication failed (HTTP 401)"}
		case http.StatusForbidden:
			if !s.keyed {
				return &tool.WebError{Class: tool.ClassToolError, Text: "Web search refused (HTTP 403) by " + s.entry.name +
					"'s keyless endpoint, which refuses IPv6 and proxied connections. " + s.keyHint("avoids this") + "."}
			}
		}
		return &tool.WebError{Class: tool.ClassToolError, Text: fmt.Sprintf("Web search request failed (HTTP %d)", f.status)}
	}
	if f.detail != "" {
		unable += ": " + f.detail
	}
	return &tool.WebError{Class: tool.ClassToolError, Text: s.scrub.text(unable)}
}

// keyHint is how a keyless session's user gets a key to the provider, for a
// rate limit or refusal of its keyless endpoint — what the key does is
// effect — storing one, or the variable, and when a key stored now is sent.
func (s *Service) keyHint(effect string) string {
	return "A free " + s.entry.name + " key " + effect + ": store one with 'craze auth login " + s.entry.login +
		"' (or set " + s.entry.keyEnv + "); a key stored now is used from the next session"
}

// crawlText is a failed fetch of pageURL in plain words (issue #139), by the
// crawl code the provider's error ended with: a missing page is the URL's
// fault, not the tool's, and the model is told what to do instead.
func (s *Service) crawlText(code, pageURL string) string {
	name := s.entry.name
	switch code {
	case "CRAWL_NOT_FOUND":
		return "The page was not found (HTTP 404, or it no longer exists): " + pageURL + ". Search for its current address."
	case "CRAWL_UNKNOWN_ERROR":
		return name + " could not fetch " + pageURL + ": the site may be unreachable or block crawlers. Try another source for the same information."
	case "CRAWL_EMPTY_CONTENT":
		return name + " fetched " + pageURL + " but found no readable text (the page may need JavaScript to render). Try a search: its excerpts may carry the page's text."
	}
	return name + " could not fetch " + pageURL + " (" + code + ")"
}
