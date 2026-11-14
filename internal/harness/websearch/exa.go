package websearch

import (
	"context"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// Exa (plan 051 §B): its hosted MCP endpoint, usable with no key. Search is
// web_search_exa and fetch web_fetch_exa, both over the MCP caller; a key,
// when there is one, raises Exa's limits.
const (
	// ExaEndpoint is Exa's hosted MCP endpoint, the one place its key goes.
	ExaEndpoint = "https://mcp.exa.ai/mcp"
	// ExaKeyEnv is the variable an Exa key is read from. The model table
	// registers the same name as a credential (modeltable.ExaKeyEnv), so its
	// value is redacted and kept from commands whether or not search is on.
	ExaKeyEnv = "EXA_API_KEY"
	// exaResults is how many results a search asks for: opencode's number.
	exaResults = 8
	// exaMaxCharacters is the most of a page a fetch asks for; the
	// dispatcher cuts what the model reads after it.
	exaMaxCharacters = 50000
	// exaNoResultsText is the whole text web_search_exa answers a search that
	// found nothing with (exa-mcp-server's webSearch.ts): no results, not a
	// failure. Only exactly this sentence; any other text with no result in it
	// is still a failure.
	exaNoResultsText = "No search results found. Please try a different query."
)

// exa is the Exa provider.
type exa struct{ c *caller }

func newExa(c *caller) Provider { return &exa{c: c} }

// Search asks web_search_exa for query, which is also the search's objective
// (a field Exa requires), and parses the blocks of its reply (parseExa).
func (e *exa) Search(ctx context.Context, query string) ([]tool.WebResult, error) {
	text, err := e.c.call(ctx, "web_search_exa", map[string]any{"query": query, "objective": query, "numResults": exaResults})
	if err != nil {
		return nil, err
	}
	results, ok := parseExa(text)
	if !ok {
		return nil, &failure{kind: failOther}
	}
	return results, nil
}

// Fetch asks web_fetch_exa for url, and returns its text as it is: a "# "
// title line, a URL: line, a blank line, the page as markdown. The page's URL
// is url with the key scrubbed out, as the text is: it is text the tool
// returns too.
func (e *exa) Fetch(ctx context.Context, url string) (tool.WebPage, error) {
	text, err := e.c.call(ctx, "web_fetch_exa", map[string]any{"urls": []string{url}, "maxCharacters": exaMaxCharacters})
	if err != nil {
		return tool.WebPage{}, err
	}
	return tool.WebPage{URL: e.c.scrub.text(url), Text: text}, nil
}

// parseExa reads web_search_exa's text: blocks separated by a "---" line,
// each a run of header lines — Title:, URL:, Published: (N/A is none),
// Author: (ignored) — then Highlights: or Text: and the content to the end
// of the block. A block starts at a Title: line that begins the text or is
// the next non-blank line after a "---" line; a "---" any other line
// follows is content. A header line Exa did not write ends the headers and
// starts the content. A block with no URL is dropped.
//
// ok is false for text that holds something but no result: no block parses,
// or none has a URL — a failure, never "no results". Empty text is no
// results, and so is exaNoResultsText, trimmed, exactly.
func parseExa(text string) (results []tool.WebResult, ok bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if t := strings.TrimSpace(text); t == "" || t == exaNoResultsText {
		return nil, true
	}
	lines := strings.Split(text, "\n")
	// starts are the lines blocks begin at.
	var starts []int
	for i, l := range lines {
		if !strings.HasPrefix(l, "Title:") {
			continue
		}
		j := i - 1
		for j >= 0 && strings.TrimSpace(lines[j]) == "" {
			j--
		}
		if j < 0 || strings.TrimSpace(lines[j]) == "---" {
			starts = append(starts, i)
		}
	}
	for n, s := range starts {
		end := len(lines)
		if n+1 < len(starts) {
			// Back over the blank lines and the "---" before the next block.
			end = starts[n+1] - 1
			for end > s && strings.TrimSpace(lines[end]) == "" {
				end--
			}
		}
		if r, ok := exaBlock(lines[s:end]); ok {
			results = append(results, r)
		}
	}
	return results, len(results) > 0
}

// exaBlock is one block's result, and whether it has a URL.
func exaBlock(lines []string) (tool.WebResult, bool) {
	var r tool.WebResult
	i := 0
headers:
	for ; i < len(lines); i++ {
		name, value, _ := strings.Cut(lines[i], ":")
		value = strings.TrimSpace(value)
		switch name {
		case "Title":
			r.Title = value
		case "URL":
			r.URL = value
		case "Published":
			if value != "N/A" {
				r.Published = value
			}
		case "Author":
		case "Highlights", "Text":
			rest := lines[i+1:]
			if value != "" {
				rest = append([]string{value}, rest...)
			}
			r.Content = strings.TrimSpace(strings.Join(rest, "\n"))
			break headers
		default:
			r.Content = strings.TrimSpace(strings.Join(lines[i:], "\n"))
			break headers
		}
	}
	return r, r.URL != ""
}
