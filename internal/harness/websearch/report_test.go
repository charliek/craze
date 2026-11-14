package websearch

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// The service's report (plan 051 F.4.1): exactly one tool.WebCall per call it
// handled, after the outcome is known, whatever the outcome; the URLs of a
// successful result only, scrubbed of the key.

// reports records every report the service makes.
type reports struct {
	mu  sync.Mutex
	got []tool.WebCall
}

func (r *reports) report(c tool.WebCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, c)
}

func (r *reports) all() []tool.WebCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// one says exactly one report was made since the last look, and returns it.
func (r *reports) one(t *testing.T, what string) tool.WebCall {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) != 1 {
		t.Fatalf("%s: %d reports %+v, want exactly one", what, len(r.got), r.got)
	}
	c := r.got[0]
	r.got = nil
	return c
}

// reported is a service at f's endpoint through the override that reports to
// a fresh recorder; cfg's other fields are taken as they are.
func reported(t *testing.T, f *fakeMCP, cfg Config) (*Service, *reports) {
	t.Helper()
	r := &reports{}
	cfg.Endpoint = f.srv.URL + "/mcp"
	cfg.Report = r.report
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, r
}

// TestOneReportPerHandledCall: a search and a fetch each report once — a
// success, a provider error, a rate limit, a declined, a disabled and a
// cancelled consent, and a cancellation in flight — the failures failed and
// carrying no URL.
func TestOneReportPerHandledCall(t *testing.T) {
	isError := func(text string) replyFunc {
		return func(w http.ResponseWriter, _ *http.Request, rec received) {
			sendSSE(w, event(resultJSON(rec.Call.ID, true, text)))
		}
	}
	for _, tc := range []struct {
		name   string
		reply  replyFunc
		failed bool
	}{
		{"success", replyText(oneResult), false},
		{"provider error", isError("web_search_exa error (500): oops"), true},
		{"429", isError("web_search_exa error (429): Too many requests"), true},
		{"HTTP 429", func(w http.ResponseWriter, _ *http.Request, _ received) { w.WriteHeader(http.StatusTooManyRequests) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := reported(t, newFake(t, tc.reply), Config{})
			_, _ = search(s, "q")
			if c := r.one(t, "search"); c.Fetch || c.Failed != tc.failed || (tc.failed && c.URLs != nil) {
				t.Fatalf("search reported %+v; want a search, failed=%v", c, tc.failed)
			}
			_, _ = s.Fetch(context.Background(), "https://a.example/")
			if c := r.one(t, "fetch"); !c.Fetch || c.Failed != tc.failed || (tc.failed && c.URLs != nil) {
				t.Fatalf("fetch reported %+v; want a fetch, failed=%v", c, tc.failed)
			}
		})
	}

	t.Run("declined consent", func(t *testing.T) {
		f := newFake(t, replyText(oneResult))
		// Undecided with nobody to ask: declined, the provider never called.
		s, r := reported(t, f, Config{Undecided: true, CanAsk: false})
		_, err := search(s, "q")
		wantErr(t, err, tool.ClassToolError, DeclinedText)
		if c := r.one(t, "declined search"); c.Fetch || !c.Failed || c.URLs != nil {
			t.Fatalf("reported %+v", c)
		}
		_, err = s.Fetch(context.Background(), "https://a.example/")
		wantErr(t, err, tool.ClassToolError, DeclinedText)
		if c := r.one(t, "declined fetch"); !c.Fetch || !c.Failed {
			t.Fatalf("reported %+v", c)
		}
		// A card answered with something else declines too: the asking call
		// is reported once.
		s2, r2 := reported(t, f, Config{Undecided: true, CanAsk: true, Asker: &fakeAsker{answer: picks()}})
		_, err = search(s2, "q")
		wantErr(t, err, tool.ClassToolError, DeclinedText)
		if c := r2.one(t, "declined by the card"); !c.Failed {
			t.Fatalf("reported %+v", c)
		}
		if n := len(f.calls()); n != 0 {
			t.Fatalf("%d provider calls", n)
		}
	})

	t.Run("disabled consent", func(t *testing.T) {
		f := newFake(t, replyText(oneResult))
		s, r := reported(t, f, Config{Undecided: true, CanAsk: true, Asker: &fakeAsker{answer: picks([]int{1})}})
		_, err := search(s, "q")
		wantErr(t, err, tool.ClassToolError, DisabledText)
		if c := r.one(t, "the disabling call"); !c.Failed {
			t.Fatalf("reported %+v", c)
		}
		_, err = s.Fetch(context.Background(), "https://a.example/")
		wantErr(t, err, tool.ClassToolError, DisabledText)
		if c := r.one(t, "a later call"); !c.Fetch || !c.Failed {
			t.Fatalf("reported %+v", c)
		}
	})

	t.Run("cancelled consent", func(t *testing.T) {
		f := newFake(t, replyText(oneResult))
		a := &fakeAsker{answer: picks([]int{0}), release: make(chan struct{}), opened: make(chan struct{}, 1)}
		s, r := reported(t, f, Config{Undecided: true, CanAsk: true, Asker: a})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := s.Search(ctx, "q"); done <- err }()
		<-a.opened
		if got := r.all(); len(got) != 0 {
			t.Fatalf("a call waiting on the card was reported before its outcome: %+v", got)
		}
		cancel()
		wantErr(t, recv(t, done), tool.ClassAborted, tool.AbortedText)
		if c := r.one(t, "the cancelled call"); !c.Failed || c.URLs != nil {
			t.Fatalf("reported %+v", c)
		}
	})

	t.Run("cancellation in flight", func(t *testing.T) {
		started := make(chan struct{}, 1)
		f := newFake(t, func(_ http.ResponseWriter, r *http.Request, _ received) {
			started <- struct{}{}
			<-r.Context().Done()
		})
		s, r := reported(t, f, Config{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, err := s.Search(ctx, "q")
		wantErr(t, err, tool.ClassAborted, tool.AbortedText)
		if c := r.one(t, "the cancelled search"); !c.Failed || c.URLs != nil {
			t.Fatalf("reported %+v", c)
		}
	})

	t.Run("no report function", func(t *testing.T) {
		// nil reports nothing, and the call is answered as ever.
		s := overridden(t, newFake(t, replyText(oneResult)), "")
		if _, err := search(s, "q"); err != nil {
			t.Fatal(err)
		}
	})
}

// TestReportedURLs: a search's result URLs in result order, none for a
// search with no results; a successful fetch's requested URL — never one the
// provider's text names — and none for a page whose text was empty, which is
// a fetch and not a failure; none for a failed call.
func TestReportedURLs(t *testing.T) {
	const two = "Title: One\nURL: https://one.example/\nHighlights:\nx\n---\nTitle: Two\nURL: https://two.example/b\nText: y\n---\nTitle: One again\nURL: https://one.example/\nText: z"
	s, r := reported(t, newFake(t, replyText(two)), Config{})
	if _, err := search(s, "q"); err != nil {
		t.Fatal(err)
	}
	// The service reports what the provider returned, in order; distinctness
	// is the session's (internal/agent's tally).
	if c := r.one(t, "search"); !slices.Equal(c.URLs, []string{"https://one.example/", "https://two.example/b", "https://one.example/"}) || c.Failed {
		t.Fatalf("search reported %+v", c)
	}

	empty, r := reported(t, newFake(t, replyText("")), Config{})
	if res, err := search(empty, "q"); err != nil || len(res) != 0 {
		t.Fatalf("an empty search = %v, %v", res, err)
	}
	if c := r.one(t, "empty search"); c.Failed || len(c.URLs) != 0 {
		t.Fatalf("an empty search reported %+v", c)
	}

	page, r := reported(t, newFake(t, replyText("# Page\nURL: https://elsewhere.example/\n\nbody")), Config{})
	if _, err := page.Fetch(context.Background(), "https://asked.example/p?q=1"); err != nil {
		t.Fatal(err)
	}
	if c := r.one(t, "fetch"); !c.Fetch || c.Failed || !slices.Equal(c.URLs, []string{"https://asked.example/p?q=1"}) {
		t.Fatalf("a fetch reported %+v; want the requested URL alone", c)
	}

	blank, r := reported(t, newFake(t, replyText("")), Config{})
	if _, err := blank.Fetch(context.Background(), "https://asked.example/"); err != nil {
		t.Fatal(err)
	}
	if c := r.one(t, "empty fetch"); !c.Fetch || c.Failed || len(c.URLs) != 0 {
		t.Fatalf("an empty-text fetch reported %+v; want a fetch, not failed, no URL", c)
	}

	failing, r := reported(t, newFake(t, func(w http.ResponseWriter, _ *http.Request, _ received) {
		w.WriteHeader(http.StatusInternalServerError)
	}), Config{})
	_, _ = search(failing, "q")
	_, _ = failing.Fetch(context.Background(), "https://asked.example/")
	for _, c := range r.all() {
		if !c.Failed || len(c.URLs) != 0 {
			t.Fatalf("a failed call reported %+v", c)
		}
	}
}

// TestReportedURLsCarryNoKey: a key in a result's URL, or in a fetch's
// requested URL — as itself, percent-encoded or with a space as "+" — is
// scrubbed before it is reported (R1).
func TestReportedURLsCarryNoKey(t *testing.T) {
	const key = "exa key 0123456789abcdef"
	for name, form := range map[string]string{
		"raw":     key,
		"percent": url.PathEscape(key),
		"plus":    url.QueryEscape(key),
	} {
		t.Run(name, func(t *testing.T) {
			u := "https://leak.example/p?k=" + form
			f := newFake(t, replyText("Title: Leak\nURL: "+u+"\nText: x"))
			s, r := reported(t, f, Config{Key: key})
			if _, err := search(s, "q"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Fetch(context.Background(), u); err != nil {
				t.Fatal(err)
			}
			for _, c := range r.all() {
				for _, got := range c.URLs {
					for _, sp := range []string{key, url.PathEscape(key), url.QueryEscape(key), "0123456789abcdef"} {
						if strings.Contains(got, sp) {
							t.Fatalf("a reported URL %q carries the key", got)
						}
					}
				}
			}
			if n := len(r.all()); n != 2 {
				t.Fatalf("%d reports, want 2", n)
			}
		})
	}
}
