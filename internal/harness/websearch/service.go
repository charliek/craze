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
	"sync"
	"sync/atomic"
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
	// KeySource is where Key came from, for Status: KeyEnv or KeyStored;
	// anything else, or no usable key, reads as KeyNone.
	KeySource KeySource
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
	id    string
	entry entry
	// overridden is a service at the test override, which sends no key
	// whatever it is given.
	overridden bool
	// slots holds one token per call at the provider (MaxInFlight).
	slots chan struct{}
	// c is the provider's caller: the key it sends now and the scrubber for
	// the words of a failure, which carry the query or the page's URL as the
	// caller gave it, and its retry count.
	c       *caller
	timeout time.Duration
	// mu guards source and serializes SetKey.
	mu     sync.Mutex
	source KeySource
	// The session's totals (Status), over the service's life.
	searches, fetches, failed, rateLimited atomic.Int64
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
	source := KeyNone
	if key != "" && (cfg.KeySource == KeyEnv || cfg.KeySource == KeyStored) {
		source = cfg.KeySource
	} else if key != "" {
		source = KeyStored
	}
	return &Service{
		p: e.build(c), id: id, entry: e, overridden: cfg.Endpoint != "", slots: make(chan struct{}, MaxInFlight), c: c,
		timeout: timeout, source: source, consent: newConsent(cfg, id), report: cfg.Report,
	}, nil
}

// KeySource is where the key a service sends came from.
type KeySource string

const (
	// KeyNone is no key: the provider's keyless endpoint.
	KeyNone KeySource = "keyless"
	// KeyEnv is the provider's variable (EXA_API_KEY).
	KeyEnv KeySource = "env"
	// KeyStored is the key `craze auth login` stored in credentials.toml.
	KeyStored KeySource = "stored"
)

// Status is a service as /websearch shows it: the provider, where the key it
// sends now came from, where the session's consent stands, and the session's
// totals over the service's life, its children's calls included.
type Status struct {
	// Provider is the provider's id ("exa"); Name its name ("Exa").
	Provider, Name string
	// Key is where the key sent now came from; KeyEnvName is the variable
	// the provider's key is read from.
	Key        KeySource
	KeyEnvName string
	// Consent is "allowed", "allowed for this session", "disabled",
	// "declined" or "undecided".
	Consent string
	// Searches and Fetches are the calls the service handled; Failed those
	// that returned an error, RateLimited those of them that were a rate
	// limit after every retry, and Retries the attempts made again after a
	// rate limit.
	Searches, Fetches, Failed, RateLimited, Retries int
}

// Status is the service now.
func (s *Service) Status() Status {
	s.mu.Lock()
	source := s.source
	s.mu.Unlock()
	return Status{
		Provider: s.id, Name: s.entry.name, Key: source, KeyEnvName: s.entry.keyEnv,
		Consent:  s.consent.state(),
		Searches: int(s.searches.Load()), Fetches: int(s.fetches.Load()), Failed: int(s.failed.Load()),
		RateLimited: int(s.rateLimited.Load()), Retries: s.Retries(),
	}
}

// SetKey makes key, from source, the key the service sends from its next
// call on (issue #137): a key stored while the session runs, handed over by
// the session once its redactor holds it — never before, so the key sent is
// always one the redactor holds. The scrubber keeps every key it held, the
// one before included. Calls already at the provider finish with the key
// they were sent with. It reports whether the key now sent changed; a key
// that is not usable, one under the test override, the one already sent, or
// a stored key while the variable's is sent (the variable comes first, as at
// Open) changes nothing (but a source).
func (s *Service) SetKey(key string, source KeySource) bool {
	key = strings.TrimSpace(key)
	if s.overridden || !usable(key) || (source != KeyEnv && source != KeyStored) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.source == KeyEnv && source == KeyStored {
		return false
	}
	ks := s.c.keys()
	s.source = source
	if ks.key == key {
		return false
	}
	s.c.setKey(key, ks.scrub.with(key))
	return true
}

// keyed says a key goes with the service's calls now.
func (s *Service) keyed() bool { return s.c != nil && s.c.keys().key != "" }

// scrub is the scrubber the service's words are scrubbed with now.
func (s *Service) scrub() *scrubber {
	if s.c == nil {
		return nil
	}
	return s.c.scrubber()
}

// count adds a handled call to the session's totals.
func (s *Service) count(fetch bool, err error) {
	if fetch {
		s.fetches.Add(1)
	} else {
		s.searches.Add(1)
	}
	if err == nil {
		return
	}
	s.failed.Add(1)
	if we, ok := errors.AsType[*tool.WebError](err); ok && we.RateLimited {
		s.rateLimited.Add(1)
	}
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

// Search runs query on the provider with no options (SearchQuery).
func (s *Service) Search(ctx context.Context, query string) ([]tool.WebResult, error) {
	return s.SearchQuery(ctx, tool.WebQuery{Query: query})
}

var _ tool.WebQuerier = (*Service)(nil)

// SearchQuery runs q on the provider, once the session's consent lets it.
// Every error is a *tool.WebError. The call is reported once, however it
// returns (reportCall).
func (s *Service) SearchQuery(ctx context.Context, q tool.WebQuery) (results []tool.WebResult, err error) {
	query := q.Query
	call := tool.WebCall{}
	defer func() { s.count(false, err); s.reportCall(call, err) }()
	if werr := s.consent.pass(ctx); werr != nil {
		return nil, werr
	}
	release, werr := s.slot(ctx)
	if werr != nil {
		return nil, werr
	}
	results, err = func() ([]tool.WebResult, error) {
		defer release()
		return s.p.Search(ctx, q)
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
			if u := s.scrub().text(r.URL); u != "" {
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
	defer func() { s.count(true, err); s.reportCall(call, err) }()
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
			return tool.WebPage{}, &tool.WebError{Class: tool.ClassToolError, Text: s.scrub().text(s.crawlText(f.crawl, pageURL))}
		}
		return tool.WebPage{}, s.webError(err, "Unable to fetch "+pageURL)
	}
	// The requested URL, as the tool accepted it and with the key scrubbed
	// out (F.4.1), never one parsed from the provider's text; a page whose
	// text was empty is a fetch that contributes none.
	if s.report != nil && strings.TrimSpace(page.Text) != "" {
		if u := s.scrub().text(pageURL); u != "" {
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
		if we, ok := errors.AsType[*tool.WebError](err); ok && we.RateLimited {
			call.RateLimited = true
		}
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
			if !s.keyed() {
				text = "Web search rate limited (HTTP 429) on " + s.entry.name + "'s keyless endpoint. Wait a few seconds before searching again. " + s.keyHint("raises the limit") + "."
			}
			return &tool.WebError{Class: tool.ClassToolError, Text: text, RateLimited: true}
		case http.StatusUnauthorized:
			return &tool.WebError{Class: tool.ClassToolError, Text: "Web search authentication failed (HTTP 401)"}
		case http.StatusForbidden:
			if !s.keyed() {
				return &tool.WebError{Class: tool.ClassToolError, Text: "Web search refused (HTTP 403) by " + s.entry.name +
					"'s keyless endpoint, which refuses IPv6 and proxied connections. " + s.keyHint("avoids this") + "."}
			}
		}
		return &tool.WebError{Class: tool.ClassToolError, Text: fmt.Sprintf("Web search request failed (HTTP %d)", f.status)}
	}
	if f.detail != "" {
		unable += ": " + f.detail
	}
	return &tool.WebError{Class: tool.ClassToolError, Text: s.scrub().text(unable)}
}

// keyHint is how a keyless session's user gets a key to the provider, for a
// rate limit or refusal of its keyless endpoint — what the key does is
// effect — storing one, or the variable, and when a key stored now is sent.
func (s *Service) keyHint(effect string) string {
	return "A free " + s.entry.name + " key " + effect + ": store one with 'craze auth login " + s.entry.login +
		"' (or set " + s.entry.keyEnv + "); a key stored now is used from the next turn"
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
