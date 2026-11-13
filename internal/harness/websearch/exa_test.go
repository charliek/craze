package websearch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// Exa (plan 051 §B, C1): the captured replies (testdata/, real public web
// content as Exa sent it on 2026-11-11) read as results and pages, and the
// search text's edges.

// TestCapturedSearch: the captured search reply reads as its five results in
// order — each block's title, URL and content, its Published date with N/A as
// none, its Author line left out — and the "---" lines between blocks are in
// no result.
func TestCapturedSearch(t *testing.T) {
	f := newFake(t, replay(t, "search.body"))
	results, err := search(overridden(t, f, ""), "go 1.27 release")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []tool.WebResult{
		{Title: "Go 1.27 Release Notes - The Go Programming Language", URL: "https://go.dev/doc/go1.27"},
		{Title: "Go 1.27 is released - The Go Programming Language", URL: "https://go.dev/blog/go1.27", Published: "2026-08-19T00:00:00.000Z"},
		{Title: "Release History - The Go Programming Language", URL: "https://go.dev/doc/devel/release"},
		{Title: "All releases - The Go Programming Language", URL: "https://go.dev/dl/"},
		{Title: "Tags · golang/go · GitHub", URL: "https://github.com/golang/go/tags"},
	}
	if len(results) != len(want) {
		t.Fatalf("%d results, want %d", len(results), len(want))
	}
	for i, r := range results {
		w := want[i]
		if r.Title != w.Title || r.URL != w.URL || r.Published != w.Published {
			t.Errorf("result %d = {%q %q %q}, want {%q %q %q}", i, r.Title, r.URL, r.Published, w.Title, w.URL, w.Published)
		}
		if r.Content == "" || strings.HasPrefix(r.Content, "Highlights:") {
			t.Errorf("result %d: content %.40q", i, r.Content)
		}
		for _, line := range strings.Split(r.Content, "\n") {
			if strings.HasPrefix(line, "Author:") || strings.HasPrefix(line, "Title:") || line == "---" {
				t.Errorf("result %d: content holds the line %q", i, line)
			}
		}
	}
	if !strings.HasPrefix(results[0].Content, "Go 1.27 Release Notes - The Go Programming Language\n\n# Go 1.27 Release Notes") {
		t.Errorf("result 0 starts %.80q", results[0].Content)
	}
	if !strings.HasSuffix(results[4].Content, "Reviewed-by: Dmitri Shuralyov") {
		t.Errorf("result 4 ends %q", results[4].Content[max(0, len(results[4].Content)-60):])
	}
	// The control: the captured text holds what was left out.
	text := capturedText(t, "search.body")
	if seps, authors := strings.Count(text, "\n---\n"), strings.Count("\n"+text, "\nAuthor: "); seps != 4 || authors != 5 {
		t.Fatalf("control: the capture has %d separators and %d Author lines", seps, authors)
	}
}

// TestCapturedFetch: a fetch returns the page's text exactly as Exa sent it —
// title line, URL line, blank line, markdown — for the default size and the
// 50000-character one, and the page's URL is the one fetched.
func TestCapturedFetch(t *testing.T) {
	for _, name := range []string{"fetch.body", "fetch-50000.body"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, replay(t, name))
			page, err := overridden(t, f, "").Fetch(context.Background(), "https://go.dev/x")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if want := capturedText(t, name); page.Text != want {
				t.Fatalf("the page's text (%d bytes) is not the reply's (%d bytes)", len(page.Text), len(want))
			}
			if page.URL != "https://go.dev/x" || !strings.HasPrefix(page.Text, "# ") || !strings.Contains(page.Text, "\nURL: https://go.dev/") {
				t.Fatalf("page %q, text %.80q", page.URL, page.Text)
			}
		})
	}
	// The capture beside the reply is the same text.
	b, err := os.ReadFile(filepath.Join("testdata", "fetch.txt"))
	if err != nil || strings.TrimSuffix(string(b), "\n") != capturedText(t, "fetch.body") {
		t.Fatalf("testdata/fetch.txt is not fetch.body's text (%v)", err)
	}
}

// TestParseExa: the search text's edges. A "---" that is not followed by a
// Title: line is content; Text: stands for Highlights:; a header line Exa did
// not write starts the content; a block with no URL is dropped; text before
// the first block is no result; CRLF reads as LF. Text with no block, or
// with blocks none of which has a URL, is a failure — never no results —
// and empty text is no results.
func TestParseExa(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []tool.WebResult
		ok   bool
	}{
		{
			name: "a --- inside content",
			text: "Title: A\nURL: https://a.example/\nPublished: 2026-01-02\nAuthor: Someone\nHighlights:\nabove\n---\nbelow, still A\n---\n\n\nTitle: B\nURL: https://b.example/\nPublished: N/A\nAuthor: N/A\nText: inline start\nmore\n",
			want: []tool.WebResult{
				{Title: "A", URL: "https://a.example/", Published: "2026-01-02", Content: "above\n---\nbelow, still A"},
				{Title: "B", URL: "https://b.example/", Content: "inline start\nmore"},
			},
			ok: true,
		},
		{
			name: "a block with no URL is dropped",
			text: "Title: no url\nHighlights:\nx\n---\nTitle: C\nURL: https://c.example/\nHighlights:\ny",
			want: []tool.WebResult{{Title: "C", URL: "https://c.example/", Content: "y"}},
			ok:   true,
		},
		{
			name: "a header line Exa did not write starts the content",
			text: "Title: D\nURL: https://d.example/\nScore: 0.9\nbody",
			want: []tool.WebResult{{Title: "D", URL: "https://d.example/", Content: "Score: 0.9\nbody"}},
			ok:   true,
		},
		{
			name: "text before the first block, and CRLF",
			text: "Results:\r\n---\r\nTitle: E\r\nURL: https://e.example/\r\nHighlights:\r\nz\r\n",
			want: []tool.WebResult{{Title: "E", URL: "https://e.example/", Content: "z"}},
			ok:   true,
		},
		{
			name: "a Title: line inside content starts nothing",
			text: "Title: F\nURL: https://f.example/\nHighlights:\nTitle: not a block\nURL: https://g.example/",
			want: []tool.WebResult{{Title: "F", URL: "https://f.example/", Content: "Title: not a block\nURL: https://g.example/"}},
			ok:   true,
		},
		{name: "empty text is no results", text: " \n\n", ok: true},
		{name: "text with no block", text: "No results found for your query."},
		{name: "blocks none of which has a URL", text: "Title: x\nHighlights:\ny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseExa(tc.text)
			if ok != tc.ok || !slices.Equal(got, tc.want) {
				t.Fatalf("parseExa = %+v, %t; want %+v, %t", got, ok, tc.want, tc.ok)
			}
		})
	}
	// Through the service: a failure is unable, empty text is no results.
	for text, wantErr := range map[string]bool{"No results found for your query.": true, "": false} {
		f := newFake(t, replyText(text))
		results, err := search(overridden(t, f, ""), "q")
		if (err != nil) != wantErr || (err == nil && (results == nil || len(results) != 0)) {
			t.Errorf("Search over %q = %v, %v", text, results, err)
		}
	}
}

// TestExaToolSchemas: what a search and a fetch send meets the input schemas
// Exa's tools/list reply declares (testdata/list.body): every required
// argument, and none it does not declare (its schemas forbid others).
func TestExaToolSchemas(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("testdata", "list.body"))
	if err != nil {
		t.Fatal(err)
	}
	var data string
	for _, line := range strings.Split(string(text), "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			data = d
		}
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties           map[string]any `json:"properties"`
					Required             []string       `json:"required"`
					AdditionalProperties bool           `json:"additionalProperties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(data), &list); err != nil || len(list.Result.Tools) != 2 {
		t.Fatalf("list.body: %v, %d tools", err, len(list.Result.Tools))
	}
	f := newFake(t, replyText(""))
	s := overridden(t, f, "")
	_, _ = search(s, "q")
	_, _ = s.Fetch(context.Background(), "https://a.example/")
	sent := map[string]map[string]any{}
	for _, rec := range f.calls() {
		sent[rec.Call.Params.Name] = rec.Call.Params.Arguments
	}
	for _, tl := range list.Result.Tools {
		args, ok := sent[tl.Name]
		if !ok {
			t.Errorf("nothing called %s", tl.Name)
			continue
		}
		for _, r := range tl.InputSchema.Required {
			if _, ok := args[r]; !ok {
				t.Errorf("%s: required %q not sent", tl.Name, r)
			}
		}
		for a := range args {
			if _, ok := tl.InputSchema.Properties[a]; !ok && !tl.InputSchema.AdditionalProperties {
				t.Errorf("%s: %q is not in its schema", tl.Name, a)
			}
		}
	}
}
