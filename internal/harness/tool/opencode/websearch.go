package opencode

import (
	"context"
	"errors"
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
	// opencode's Input (websearch.ts:24-26).
	return &webSearchTool{spec: tool.Spec{
		ID:          tool.WebSearchTool,
		Description: desc,
		Parameters: map[string]any{
			"query": map[string]any{"type": "string", "description": "Websearch query"},
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
	return &webSearchCall{query: query}, nil
}

type webSearchCall struct{ query string }

func (c *webSearchCall) Request() tool.Request { return tool.Request{Title: c.query} }

func (c *webSearchCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return webAborted()
	}
	if env.Web == nil {
		return tool.Result{Text: noWebText, IsError: true, Class: tool.ClassToolError}
	}
	results, err := env.Web.Search(ctx, c.query)
	if err != nil {
		return webErrorResult(ctx, err, "Unable to search the web for "+c.query)
	}
	return tool.Result{Text: cutFirstLine(searchText(results), env)}
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
func cutFirstLine(text string, env tool.Env) string {
	text = env.CurrentRedactor().String(text)
	first, _, _ := strings.Cut(text, "\n")
	if len(first) <= tool.MaxBytes {
		return text
	}
	cut := tool.MaxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "\n" + text[cut:]
}
