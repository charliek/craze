package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// The web tools (plan 051 §A): websearch and webfetch, opencode's ids and
// result layout (packages/core/src/tool/plugin/websearch.ts and webfetch.ts
// at 6c34e24, NOTICE), over the session's web service (tool.Env.Web), which
// owns the provider, the call on the wire and, later, consent. The tools own
// the input and how a result is written for the model.

// noResultsText is opencode's answer to a search with no results
// (websearch.ts:13).
const noResultsText = "No search results found. Please try a different query."

// noWebText is either tool's answer in a session with no web service
// (Env.Web nil): a session offers the tools only with one, so this is a bug's
// answer, not one a model normally reads.
const noWebText = "Web search is not available in this session"

type webSearchTool struct{ spec tool.Spec }

func newWebSearch() (tool.Tool, error) {
	desc, err := description("websearch", nil)
	if err != nil {
		return nil, err
	}
	// opencode's Input (websearch.ts:24-26), and craze's own two options
	// (issue #140): the sites to search, and how many results.
	return &webSearchTool{spec: tool.Spec{
		ID:          tool.WebSearchTool,
		Description: desc,
		Parameters: map[string]any{
			"query": map[string]any{"type": "string", "description": "Websearch query"},
			"domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Only return results from these sites and their subdomains, as bare hosts, at most 10 (e.g. [\"docs.python.org\"]). Optional"},
			"num_results": map[string]any{"type": "integer", "minimum": 1, "maximum": tool.MaxWebResults,
				"description": "How many results to return, 1 to 10 (default 8). Optional"},
		},
		Required: []string{"query"},
		Kind:     tool.KindSearch,
		ReadOnly: true,
		Parallel: true,
		Truncate: tool.Head,
	}}, nil
}

func (w *webSearchTool) Spec() tool.Spec { return w.spec }

func (w *webSearchTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	query, _, err := a.str("query", true)
	if err != nil {
		return nil, err
	}
	if query = strings.TrimSpace(query); query == "" {
		return nil, errors.New("query must not be empty")
	}
	q := tool.WebQuery{Query: query}
	items, _, err := a.array("domains", false)
	if err != nil {
		return nil, err
	}
	if len(items) > tool.MaxWebDomains {
		return nil, fmt.Errorf("domains takes at most %d sites, not %d", tool.MaxWebDomains, len(items))
	}
	for _, raw := range items {
		var d string
		if json.Unmarshal(raw, &d) != nil {
			return nil, errors.New("domains must be an array of strings")
		}
		host, ok := siteHost(d)
		if !ok {
			return nil, fmt.Errorf("domains: %q is not a site's host name, such as docs.python.org", d)
		}
		if !slices.Contains(q.Domains, host) {
			q.Domains = append(q.Domains, host)
		}
	}
	n, ok, err := a.integer("num_results", 1)
	if err != nil {
		return nil, err
	}
	if ok {
		if n > tool.MaxWebResults {
			return nil, fmt.Errorf("num_results must be at most %d, not %d", tool.MaxWebResults, n)
		}
		q.Results = int(n)
	}
	return &webSearchCall{q: q}, nil
}

// siteHost is d as a bare host name — a scheme, a path and a port dropped,
// lowercased — and whether it is one: no user name or password, at least two
// labels, each 1 to 63 letters, digits or hyphens not starting or ending with
// a hyphen, 253 bytes in all, as a domain filter takes.
func siteHost(d string) (string, bool) {
	d = strings.ToLower(strings.TrimSpace(d))
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if strings.Contains(d, "@") {
		return "", false
	}
	if i := strings.LastIndexByte(d, ':'); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSuffix(d, ".")
	labels := strings.Split(d, ".")
	if len(labels) < 2 || len(d) > 253 {
		return "", false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", false
		}
		for _, r := range l {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			default:
				return "", false
			}
		}
	}
	return d, true
}

type webSearchCall struct{ q tool.WebQuery }

func (c *webSearchCall) Request() tool.Request { return tool.Request{Title: c.q.Query} }

func (c *webSearchCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return webAborted()
	}
	if env.Web == nil {
		return tool.Result{Text: noWebText, IsError: true, Class: tool.ClassToolError}
	}
	var results []tool.WebResult
	var err error
	if wq, ok := env.Web.(tool.WebQuerier); ok {
		results, err = wq.SearchQuery(ctx, c.q)
	} else {
		results, err = env.Web.Search(ctx, c.q.Query)
	}
	if err != nil {
		return webErrorResult(ctx, err, "Unable to search the web for "+c.q.Query)
	}
	return tool.Result{Text: withFooter(cutFirstLineAt(searchText(results), env, tool.MaxBytes-len(excerptFooter)-2), len(results))}
}

// searchText is opencode's layout of the results (websearch.ts:151-161): per
// result a "## [title](url)" heading, the URL standing for a missing title,
// then "Published: <date>" when there is one, then a blank line and the
// content when there is any; the results joined by a blank line. No results
// is noResultsText. The date is the provider's as written, where opencode
// rewrites it as an ISO date-time.
func searchText(results []tool.WebResult) string {
	if len(results) == 0 {
		return noResultsText
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		title := r.Title
		if title == "" {
			title = r.URL
		}
		s := "## [" + title + "](" + r.URL + ")"
		if r.Published != "" {
			s += "\nPublished: " + r.Published
		}
		if r.Content != "" {
			s += "\n\n" + r.Content
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

// withFooter is a search's text with excerptFooter: last, where the model
// reads it after the results, unless the dispatcher's head would cut it off
// there (tool.MaxBytes, tool.MaxLines) — then first, so a long search still
// says it. text is already as cutFirstLine left it; no results, no footer.
func withFooter(text string, results int) string {
	if results == 0 {
		return text
	}
	if len(text)+len(excerptFooter)+2 > tool.MaxBytes || strings.Count(text, "\n")+3 > tool.MaxLines {
		return excerptFooter + "\n\n" + text
	}
	return text + "\n\n" + excerptFooter
}

// excerptFooter ends every search with results (issue #131): a model that
// writes from an excerpt misses what is further down the page, and a line at
// the point it reads the results moves it where the description alone did
// not (eval T-WS6).
const excerptFooter = "These results are excerpts, often only the start of each page. Before relying on a detail from one, read the page with webfetch."

// webErrorResult is a failed search's or fetch's result: a *tool.WebError's
// class and text as they are (its text is already fit for the model), a
// cancelled call as aborted, and anything else as fallback, a tool_error.
func webErrorResult(ctx context.Context, err error, fallback string) tool.Result {
	var we *tool.WebError
	switch {
	case errors.As(err, &we):
		class := we.Class
		if class == "" {
			class = tool.ClassToolError
		}
		return tool.Result{Text: we.Text, IsError: true, Class: class}
	case ctx.Err() != nil:
		return webAborted()
	}
	return tool.Result{Text: fallback, IsError: true, Class: tool.ClassToolError}
}

func webAborted() tool.Result {
	return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
}

// cutFirstLine keeps a web result from reaching the model empty. The
// dispatcher's Head truncation keeps whole lines, and a first line longer
// than tool.MaxBytes keeps nothing of itself (tool.Truncate), so a page that
// is one long line would come back as the truncation notice alone (plan 051
// §A). Such a line is broken at the limit — at a character's start — with a
// newline, so the head keeps its first tool.MaxBytes bytes and the spill file
// still holds every byte the provider returned.
//
// The text is redacted first, with the session's keys as they are now
// (tool.Env.CurrentRedactor), not as they were when the call began: a key
// the session learned while the call waited on the web would otherwise be
// halved by the break, leaving two pieces the dispatcher's redaction, which
// runs after, would not recognise. And it is redacted before the line is
// measured: a line that fits as the provider wrote it can outgrow the limit
// once a key in it becomes the longer marker, and would then keep nothing.
// A redacted line does not grow again when the dispatcher redacts it (a
// second pass changes nothing). Text whose first line fits once redacted is
// returned redacted, as the dispatcher would make it.
func cutFirstLine(text string, env tool.Env) string { return cutFirstLineAt(text, env, tool.MaxBytes) }

// cutFirstLineAt is cutFirstLine breaking at limit bytes rather than
// tool.MaxBytes: a search's, which may put excerptFooter in front of it
// (withFooter), keeps that much room for it.
func cutFirstLineAt(text string, env tool.Env, limit int) string {
	text = env.CurrentRedactor().String(text)
	first, _, _ := strings.Cut(text, "\n")
	if len(first) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "\n" + text[cut:]
}
