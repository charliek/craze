package opencode

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// websearch's options (issue #140): domains and num_results.

// queryWeb is a fakeWeb that takes a search's options too (tool.WebQuerier).
type queryWeb struct {
	fakeWeb
	got []tool.WebQuery
}

func (w *queryWeb) SearchQuery(_ context.Context, q tool.WebQuery) ([]tool.WebResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, q)
	return w.results, w.err
}

// TestWebSearchOptionsReachTheService: domains are read as bare hosts — a
// scheme, a path and a port dropped, lowercased, each once — and with
// num_results reach a service that takes them; none sent is the zero
// options.
func TestWebSearchOptionsReachTheService(t *testing.T) {
	web := &queryWeb{fakeWeb: fakeWeb{results: []tool.WebResult{{URL: "https://docs.python.org/3/", Content: "x"}}}}
	f := newWebFixture(t, web, nil)
	_, res := f.call(t, "websearch", map[string]any{"query": "asyncio taskgroup",
		"domains": []string{"https://Docs.Python.org/3/library", "docs.python.org", "peps.python.org:443"}, "num_results": 3})
	ok(t, res)
	_, res = f.call(t, "websearch", map[string]any{"query": "plain"})
	ok(t, res)
	want := []tool.WebQuery{
		{Query: "asyncio taskgroup", Domains: []string{"docs.python.org", "peps.python.org"}, Results: 3},
		{Query: "plain"},
	}
	if len(web.got) != 2 || web.got[0].Query != want[0].Query || !slices.Equal(web.got[0].Domains, want[0].Domains) ||
		web.got[0].Results != 3 || web.got[1].Query != "plain" || web.got[1].Domains != nil || web.got[1].Results != 0 {
		t.Fatalf("the service was asked %+v, want %+v", web.got, want)
	}
	if len(web.queries) != 0 {
		t.Fatalf("Search was called beside SearchQuery: %q", web.queries)
	}
}

// TestWebSearchOptionsAreValidated: a domain that is no host, too many
// domains, a domains that is not an array of strings, and a num_results out
// of range are refused before the service is asked.
func TestWebSearchOptionsAreValidated(t *testing.T) {
	web := &queryWeb{}
	f := newWebFixture(t, web, nil)
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = "a" + strings.Repeat("b", i) + ".example"
	}
	for _, tc := range []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"domains": []string{"localhost"}}, "is not a site's host name"},
		{map[string]any{"domains": []string{"docs python.org"}}, "is not a site's host name"},
		{map[string]any{"domains": eleven}, "at most 10 sites"},
		{map[string]any{"domains": "docs.python.org"}, "must be an array"},
		{map[string]any{"domains": []any{3}}, "array of strings"},
		{map[string]any{"num_results": 0}, "at least 1"},
		{map[string]any{"num_results": 11}, "at most 10"},
		{map[string]any{"num_results": 2.5}, "must be an integer"},
	} {
		tc.in["query"] = "q"
		_, res := f.call(t, "websearch", tc.in)
		if !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%v: %+v, want an error with %q", tc.in, res, tc.want)
		}
	}
	if len(web.got) != 0 {
		t.Fatalf("a refused call reached the service: %+v", web.got)
	}
}

// TestWebSearchOptionsWithAPlainWeb: a Web that takes no options is asked
// the query alone.
func TestWebSearchOptionsWithAPlainWeb(t *testing.T) {
	web := &fakeWeb{}
	f := newWebFixture(t, web, nil)
	_, res := f.call(t, "websearch", map[string]any{"query": "q", "domains": []string{"go.dev"}, "num_results": 2})
	ok(t, res)
	if !slices.Equal(web.queries, []string{"q"}) {
		t.Fatalf("queries %q", web.queries)
	}
}
