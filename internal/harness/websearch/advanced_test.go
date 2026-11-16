package websearch

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// A search with domains goes to web_search_advanced_exa, whose reply is JSON
// (issue #140, discovery E8); num_results sets numResults either way.

// TestSearchWithDomains: the call names the advanced tool with the query,
// the domains as includeDomains, the result count and highlights; its JSON
// is read into results — highlights joined, cut to exaHighlightMax, a result
// with no URL dropped — and the key a careless reply echoes is scrubbed.
func TestSearchWithDomains(t *testing.T) {
	const key = "exa-key-0123456789abcdef"
	long := strings.Repeat("h", exaHighlightMax+500)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		body := fmt.Sprintf(`{"requestId":"r","results":[`+
			`{"id":"u1","url":"https://docs.python.org/3/library/asyncio-task.html","title":"Coroutines and Tasks","publishedDate":"2026-10-01","highlights":["TaskGroup %s","second"]},`+
			`{"id":"u2","url":"","title":"no url","highlights":["dropped"]},`+
			`{"id":"u3","url":"https://peps.python.org/pep-0654/","title":"PEP 654","text":"%s"}],"costDollars":{"total":0.005}}`, key, long)
		sendSSE(w, event(resultJSON(rec.Call.ID, false, body)))
	})
	s := keyed(t, f, key, 0)
	results, err := s.SearchQuery(context.Background(), tool.WebQuery{Query: "taskgroup", Domains: []string{"docs.python.org", "peps.python.org"}, Results: 3})
	if err != nil {
		t.Fatal(err)
	}
	call := f.calls()[0].Call.Params
	if call.Name != "web_search_advanced_exa" ||
		fmt.Sprint(call.Arguments) != "map[enableHighlights:true highlightsMaxCharacters:3000 includeDomains:[docs.python.org peps.python.org] numResults:3 query:taskgroup]" {
		t.Fatalf("called %s with %v", call.Name, call.Arguments)
	}
	if len(results) != 2 {
		t.Fatalf("%d results: %+v", len(results), results)
	}
	r := results[0]
	if r.URL != "https://docs.python.org/3/library/asyncio-task.html" || r.Title != "Coroutines and Tasks" || r.Published != "2026-10-01" ||
		strings.Contains(r.Content, key) || !strings.HasSuffix(r.Content, "\n\nsecond") {
		t.Fatalf("the first result: %+v", r)
	}
	if c := results[1].Content; len(c) > exaHighlightMax+len("…") || !strings.HasSuffix(c, "…") {
		t.Fatalf("the second result's content is %d bytes, uncut", len(c))
	}
}

// TestSearchWithDomainsFindingNothing: a reply with no results member is no
// results; one that is not JSON is a failure, never no results.
func TestSearchWithDomainsFindingNothing(t *testing.T) {
	for _, tc := range []struct {
		body string
		fail bool
	}{
		{`{"requestId":"r","searchTime":1,"costDollars":{"total":0}}`, false},
		{`{"requestId":"r","results":[]}`, false},
		{`Title: not json`, true},
		{`{"results":[{"url":""}]}`, true},
	} {
		f := newFake(t, replyText(tc.body))
		results, err := overridden(t, f, "").SearchQuery(context.Background(), tool.WebQuery{Query: "q", Domains: []string{"go.dev"}})
		switch {
		case tc.fail:
			wantErr(t, err, tool.ClassToolError, "Unable to search the web for q")
		case err != nil || len(results) != 0:
			t.Errorf("%s: %+v, %v; want no results", tc.body, results, err)
		}
	}
}

// TestNumResultsWithoutDomains: a count with no domains stays on
// web_search_exa, as numResults; none is exaResults.
func TestNumResultsWithoutDomains(t *testing.T) {
	f := newFake(t, replyText(oneResult))
	s := overridden(t, f, "")
	if _, err := s.SearchQuery(context.Background(), tool.WebQuery{Query: "q", Results: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := search(s, "q"); err != nil {
		t.Fatal(err)
	}
	got := f.calls()
	if got[0].Call.Params.Name != "web_search_exa" || fmt.Sprint(got[0].Call.Params.Arguments["numResults"]) != "2" ||
		fmt.Sprint(got[1].Call.Params.Arguments["numResults"]) != "8" {
		t.Fatalf("calls %+v / %+v", got[0].Call.Params, got[1].Call.Params)
	}
}
