package websearch

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// Exa (plan 051 §B): its hosted MCP endpoint, usable with no key. Search is
// web_search_exa and fetch web_fetch_exa, both over the MCP caller; a key,
// when there is one, raises Exa's limits.
const (
	// ExaEndpoint is Exa's hosted MCP endpoint, the one place its key goes.
	// Its tools parameter names every tool craze calls: naming one replaces
	// Exa's default list, and web_search_advanced_exa — a search with a
	// domain filter — is not in that list (discovery/web-search E2).
	ExaEndpoint = "https://mcp.exa.ai/mcp?tools=web_search_exa,web_fetch_exa,web_search_advanced_exa"
	// ExaKeyEnv is the variable an Exa key is read from. The model table
	// registers the same name as a credential (modeltable.ExaKeyEnv), so its
	// value is redacted and kept from commands whether or not search is on.
	ExaKeyEnv = "EXA_API_KEY"
	// exaResults is how many results a search asks for when the model names
	// no number: opencode's.
	exaResults = 8
	// exaHighlightMax is the most of a domain-filtered search's result craze
	// keeps, in bytes: web_search_advanced_exa's highlightsMaxCharacters is
	// asked for too, but Exa does not hold to it (discovery E8).
	exaHighlightMax = 3000
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

// Search asks web_search_exa for q's query, which is also the search's
// objective (a field Exa requires), with q's number of results (exaResults
// when it names none), and parses the blocks of its reply (parseExa). A q
// with domains goes to web_search_advanced_exa instead, as includeDomains,
// with highlights, and its JSON reply is parsed (parseExaAdvanced).
func (e *exa) Search(ctx context.Context, q tool.WebQuery) ([]tool.WebResult, error) {
	n := q.Results
	if n <= 0 {
		n = exaResults
	}
	if len(q.Domains) > 0 {
		text, err := e.c.call(ctx, "web_search_advanced_exa", map[string]any{
			"query": q.Query, "numResults": n, "includeDomains": q.Domains,
			"enableHighlights": true, "highlightsMaxCharacters": exaHighlightMax,
		})
		if err != nil {
			return nil, err
		}
		results, ok := parseExaAdvanced(text)
		if !ok {
			return nil, &failure{kind: failOther}
		}
		return results, nil
	}
	text, err := e.c.call(ctx, "web_search_exa", map[string]any{"query": q.Query, "objective": q.Query, "numResults": n})
	if err != nil {
		return nil, err
	}
	results, ok := parseExa(text)
	if !ok {
		return nil, &failure{kind: failOther}
	}
	return results, nil
}

// exaAdvancedReply is web_search_advanced_exa's reply: JSON, unlike
// web_search_exa's blocks of text (discovery E8). Only what a result needs is
// read; requestId, costDollars, image and the rest are ignored.
type exaAdvancedReply struct {
	Results *[]struct {
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		PublishedDate string   `json:"publishedDate"`
		Highlights    []string `json:"highlights"`
		Text          string   `json:"text"`
	} `json:"results"`
}

// parseExaAdvanced reads web_search_advanced_exa's JSON: each result with a
// URL, its highlights joined by a blank line (its text when it has none) and
// cut to exaHighlightMax bytes. A reply with no results member, or an empty
// one, is no results (Exa's answer to a search that found nothing); one that
// is not JSON, or whose results hold none with a URL, is a failure.
func parseExaAdvanced(text string) (results []tool.WebResult, ok bool) {
	var r exaAdvancedReply
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return nil, false
	}
	if r.Results == nil || len(*r.Results) == 0 {
		return nil, true
	}
	for _, x := range *r.Results {
		if strings.TrimSpace(x.URL) == "" {
			continue
		}
		content := strings.TrimSpace(strings.Join(x.Highlights, "\n\n"))
		if content == "" {
			content = strings.TrimSpace(x.Text)
		}
		results = append(results, tool.WebResult{
			Title: strings.TrimSpace(x.Title), URL: strings.TrimSpace(x.URL),
			Published: strings.TrimSpace(x.PublishedDate), Content: cut(content, exaHighlightMax),
		})
	}
	return results, len(results) > 0
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
	return tool.WebPage{URL: e.c.scrubber().text(url), Text: text}, nil
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
