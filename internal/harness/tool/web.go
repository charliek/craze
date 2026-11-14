package tool

import "context"

// The web tools' ids (plan 051 §A), beside AgentTool and the jobs' tools: a
// session offers them only when its web state does (the harness withholds
// them otherwise, before anything is built from its profile).
const (
	WebSearchTool = "websearch"
	WebFetchTool  = "webfetch"
)

// IsWebTool reports whether id is one of the web tools.
func IsWebTool(id string) bool { return id == WebSearchTool || id == WebFetchTool }

// Web is how the websearch and webfetch tools reach the web (plan 051 §B): a
// narrow seam of the same kind as TodoStore, Asker and Subagents, since this
// package may not import the service that implements it
// (internal/harness/websearch). The tools own the call's input and how a
// result is written for the model; the service owns the provider, the call
// on the wire, consent and what it remembers.
//
// Every error either method returns is a *WebError, whose Text is already
// fit for the model to read; anything else is read as a tool_error with no
// text of its own. Both must return promptly once ctx is done, with a
// *WebError of class aborted.
type Web interface {
	// Search runs query and returns its results in the provider's order; no
	// results is an empty slice and a nil error, never an error.
	Search(ctx context.Context, query string) ([]WebResult, error)
	// Fetch returns the content of the page at url as the provider gives it.
	Fetch(ctx context.Context, url string) (WebPage, error)
}

// WebResult is one search result. URL is never empty; Title and Published
// may be ("" when the provider gave none), and Published is the provider's
// date as written (an ISO 8601 date or date-time).
type WebResult struct {
	Title     string
	URL       string
	Published string
	Content   string
}

// WebPage is a fetched page: the URL it was fetched for — with the provider
// key taken out, so it is the one a tool may show — and its text, as the
// provider returned it ("" when it returned none).
type WebPage struct {
	URL  string
	Text string
}

// WebError is a failed search or fetch: why, for diagnosis (the result's
// Class), and what the model reads. Text is already safe to show: it never
// holds the request's endpoint, a transport error's text, a header or the
// provider key, and a tool returns it as it is.
type WebError struct {
	Class ErrorClass
	Text  string
}

func (e *WebError) Error() string { return e.Text }

// WebCall is one call the web service handled, reported once its outcome is
// known (plan 051 F.4.1): a search or a fetch (Fetch), whether it returned an
// error (Failed: a provider error, a rate limit, a declined, disabled or
// cancelled consent, a cancellation in flight), and the URLs it contributes —
// a successful search's result URLs in result order, a successful fetch's
// requested URL when the page's text was not empty, and none for a failed
// call — each with the provider key already scrubbed out. A call refused
// before it reached the service (a bad or non-public URL) is never reported.
// The service knows nothing of turns: who counts a report, and in which turn,
// is the session's.
type WebCall struct {
	Fetch  bool
	Failed bool
	URLs   []string
}
