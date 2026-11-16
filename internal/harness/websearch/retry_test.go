package websearch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// Rate-limit retries and the in-flight cap (issue #138), and a failed fetch's
// plain words (issue #139).

// withConfig is a service at f's endpoint through the test override, built
// from cfg with that endpoint.
func withConfig(t *testing.T, f *fakeMCP, cfg Config) *Service {
	t.Helper()
	cfg.Endpoint = f.srv.URL + "/mcp"
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// limitedThen answers the first n calls with limit and every later one with a
// result of one search block.
func limitedThen(n int, limit replyFunc) replyFunc {
	var seen atomic.Int64
	return func(w http.ResponseWriter, r *http.Request, rec received) {
		if seen.Add(1) <= int64(n) {
			limit(w, r, rec)
			return
		}
		replyText("Title: T\nURL: https://a.example/\nText: body")(w, r, rec)
	}
}

// http429 is Exa's keyless rate limit: an HTTP 429 with a Retry-After.
func http429(retryAfter string) replyFunc {
	return func(w http.ResponseWriter, _ *http.Request, _ received) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"You've hit Exa's free MCP rate limit."}}`)
	}
}

// result429 is Exa's keyed rate limit: HTTP 200, an error result naming 429,
// no Retry-After.
func result429(w http.ResponseWriter, _ *http.Request, rec received) {
	sendSSE(w, event(resultJSON(rec.Call.ID, true, "web_search_exa error (429): Rate limit exceeded: 10 requests per second for your team. Wait at least 1 second before retrying, and reduce your request rate")))
}

// TestRateLimitIsRetried: both shapes of a rate limit are tried again, each
// attempt a fresh request with a fresh id, and the call succeeds once the
// provider lets it; the wait is told the provider's Retry-After (0 when it
// sent none) and the attempt; the retries are counted.
func TestRateLimitIsRetried(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit replyFunc
		after time.Duration
	}{
		{"HTTP 429 with Retry-After", http429("3"), 3 * time.Second},
		{"HTTP 429 without", http429(""), 0},
		{"an error result naming 429", result429, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, limitedThen(2, tc.limit))
			var mu sync.Mutex
			var waits []string
			s := withConfig(t, f, Config{backoff: func(after time.Duration, attempt int) time.Duration {
				mu.Lock()
				defer mu.Unlock()
				waits = append(waits, after.String()+"/"+string(rune('0'+attempt)))
				return time.Millisecond
			}})
			results, err := search(s, "q")
			if err != nil || len(results) != 1 {
				t.Fatalf("Search = %v, %v", results, err)
			}
			got := f.calls()
			if len(got) != 3 {
				t.Fatalf("%d requests, want 3", len(got))
			}
			for i, rec := range got {
				if rec.Call.ID != int64(i+1) {
					t.Errorf("request %d has id %d", i, rec.Call.ID)
				}
			}
			want := []string{tc.after.String() + "/1", tc.after.String() + "/2"}
			if strings.Join(waits, " ") != strings.Join(want, " ") {
				t.Errorf("waits = %v, want %v", waits, want)
			}
			if s.Retries() != 2 {
				t.Errorf("Retries = %d, want 2", s.Retries())
			}
		})
	}
}

// TestRetriesRunOut: a call still rate limited after maxAttempts reads as the
// rate limit, never no results.
func TestRetriesRunOut(t *testing.T) {
	f := newFake(t, http429("1"))
	_, err := search(withConfig(t, f, Config{}), "q")
	wantErr(t, err, tool.ClassToolError, limitedKeyless)
	if n := len(f.calls()); n != maxAttempts {
		t.Fatalf("%d requests, want %d", n, maxAttempts)
	}
}

// TestOnlyRateLimitsAreRetried: any other failure is returned at once.
func TestOnlyRateLimitsAreRetried(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ received) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := search(withConfig(t, f, Config{}), "q")
	wantErr(t, err, tool.ClassToolError, "Web search request failed (HTTP 503)")
	if n := len(f.calls()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// TestAWaitPastTheDeadlineIsNotTaken: a wait that would leave too little of
// the call's deadline is not waited out: the rate limit comes back at once,
// not a timeout.
func TestAWaitPastTheDeadlineIsNotTaken(t *testing.T) {
	f := newFake(t, http429("30"))
	s := withConfig(t, f, Config{timeout: 5 * time.Second, backoff: backoff})
	start := time.Now()
	_, err := search(s, "q")
	wantErr(t, err, tool.ClassToolError, limitedKeyless)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
	if n := len(f.calls()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// TestAHugeRetryAfterIsNotRetried: a Retry-After too large for a Duration
// is not wrapped into a short wait: the 429 comes back at once (review
// finding, PR 1).
func TestAHugeRetryAfterIsNotRetried(t *testing.T) {
	f := newFake(t, http429("18446744074"))
	s := withConfig(t, f, Config{backoff: backoff})
	_, err := search(s, "q")
	wantErr(t, err, tool.ClassToolError, limitedKeyless)
	if n := len(f.calls()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// TestCancelBeforeTheWaitIsAnAbort: a cancel that lands as the wait begins,
// with the call's deadline over too, is the caller's cancel, never a
// timeout (review finding, PR 1).
func TestCancelBeforeTheWaitIsAnAbort(t *testing.T) {
	f := newFake(t, http429(""))
	ctx, cancel := context.WithCancel(context.Background())
	s := withConfig(t, f, Config{timeout: 3 * time.Second, backoff: func(time.Duration, int) time.Duration {
		cancel()
		return 0
	}})
	_, err := s.Search(ctx, "q")
	wantErr(t, err, tool.ClassAborted, tool.AbortedText)
}

// TestCancelDuringAWait: a cancel while waiting to try again is an abort,
// promptly.
func TestCancelDuringAWait(t *testing.T) {
	arrived := make(chan struct{}, 1)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, rec received) {
		http429("")(w, r, rec)
		arrived <- struct{}{}
	})
	s := withConfig(t, f, Config{backoff: func(time.Duration, int) time.Duration { return 10 * time.Second }})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-arrived
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := s.Search(ctx, "q")
	wantErr(t, err, tool.ClassAborted, tool.AbortedText)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

// TestInFlightCap: however many calls a session makes at once, at most
// MaxInFlight are at the provider together, and every one completes; time
// queued for a slot is not the call's, so a deadline shorter than the queue
// still lets them all through.
func TestInFlightCap(t *testing.T) {
	const calls = 3*MaxInFlight + 1
	var now, peak atomic.Int64
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, rec received) {
		n := now.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		now.Add(-1)
		replyText("Title: T\nURL: https://a.example/\nText: body")(w, r, rec)
	})
	// Four waves of 150 ms: the last call starts about 450 ms after the
	// first, past this deadline, which it does not run into.
	s := withConfig(t, f, Config{timeout: 400 * time.Millisecond})
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = search(s, "q")
			} else {
				_, err = s.Fetch(context.Background(), "https://a.example/")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a call failed: %v", err)
		}
	}
	if p := peak.Load(); p != MaxInFlight {
		t.Fatalf("peak in flight = %d, want %d", p, MaxInFlight)
	}
}

// TestCancelWhileQueued: a call waiting for a slot that is cancelled is an
// abort, and never reaches the provider.
func TestCancelWhileQueued(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, rec received) {
		<-release
		replyText("Title: T\nURL: https://a.example/\nText: body")(w, r, rec)
	})
	defer close(release)
	s := withConfig(t, f, Config{})
	for range MaxInFlight {
		go func() { _, _ = search(s, "q") }()
	}
	for len(f.calls()) < MaxInFlight {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Search(ctx, "queued")
	wantErr(t, err, tool.ClassAborted, tool.AbortedText)
	if n := len(f.calls()); n != MaxInFlight {
		t.Fatalf("%d requests, want %d", n, MaxInFlight)
	}
}

// TestRetryAfterOf: delta-seconds and an HTTP date are read; anything else,
// or a time already past, is none.
func TestRetryAfterOf(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		v        string
		min, max time.Duration
	}{
		{"", 0, 0},
		{"1", time.Second, time.Second},
		{" 12 ", 12 * time.Second, 12 * time.Second},
		{"0", 0, 0},
		{"-3", 0, 0},
		{"soon", 0, 0},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0, 0},
		{"18446744074", time.Hour, time.Hour},
		{"99999999999999999999999", time.Hour, time.Hour},
		{"7200", time.Hour, time.Hour},
		{time.Now().Add(72 * time.Hour).UTC().Format(http.TimeFormat), time.Hour, time.Hour},
		{future, 80 * time.Second, 90 * time.Second},
	} {
		if got := retryAfterOf(tc.v); got < tc.min || got > tc.max {
			t.Errorf("retryAfterOf(%q) = %s, want [%s, %s]", tc.v, got, tc.min, tc.max)
		}
	}
}

// TestBackoff: the wait is the base — Retry-After, else a second — grown by
// jitter in [1, 2) and by the attempt.
func TestBackoff(t *testing.T) {
	for range 100 {
		if d := backoff(0, 1); d < time.Second || d >= 2*time.Second {
			t.Fatalf("backoff(0, 1) = %s", d)
		}
		if d := backoff(3*time.Second, 2); d < 6*time.Second || d >= 12*time.Second {
			t.Fatalf("backoff(3s, 2) = %s", d)
		}
		if d := backoff(maxRetryAfter, 3); d != maxRetryAfter {
			t.Fatalf("backoff(max, 3) = %s, want it capped", d)
		}
	}
}

// TestCrawlFailures: a fetch Exa could not make reads in plain words by its
// crawl code, with the URL asked for — the key scrubbed out of it — and a
// code craze does not know keeps the code; a search's error text that
// happens to look like one is not a fetch's.
func TestCrawlFailures(t *testing.T) {
	const key = "exa-key-0123456789abcdef"
	for _, tc := range []struct {
		code, want string
	}{
		{"CRAWL_NOT_FOUND", "The page was not found (HTTP 404, or it no longer exists): https://a.example/x. Search for its current address."},
		{"CRAWL_UNKNOWN_ERROR", "Exa could not fetch https://a.example/x: the site may be unreachable or block crawlers. Try another source for the same information."},
		{"CRAWL_EMPTY_CONTENT", "Exa fetched https://a.example/x but found no readable text (the page may need JavaScript to render). Try a search: its excerpts may carry the page's text."},
		{"CRAWL_LIVECRAWL_TIMEOUT", "Exa could not fetch https://a.example/x (CRAWL_LIVECRAWL_TIMEOUT)"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
				url := rec.Call.Params.Arguments["urls"].([]any)[0].(string)
				sendSSE(w, event(resultJSON(rec.Call.ID, true, "Error fetching URL(s): "+url+": "+tc.code)))
			})
			s := overridden(t, f, "")
			_, err := s.Fetch(context.Background(), "https://a.example/x")
			wantErr(t, err, tool.ClassToolError, tc.want)
		})
	}
	t.Run("the key in the URL", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
			sendSSE(w, event(resultJSON(rec.Call.ID, true, "Error fetching URL(s): https://a.example/?k=x: CRAWL_NOT_FOUND")))
		})
		_, err := overridden(t, f, key).Fetch(context.Background(), "https://a.example/?k="+key)
		if we := webErr(t, err); strings.Contains(we.Text, key) || !strings.HasPrefix(we.Text, "The page was not found") {
			t.Fatalf("error = %q", we.Text)
		}
	})
	t.Run("not on a search", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
			sendSSE(w, event(resultJSON(rec.Call.ID, true, "Error fetching URL(s): https://a.example/: CRAWL_NOT_FOUND")))
		})
		_, err := search(overridden(t, f, ""), "q")
		wantErr(t, err, tool.ClassToolError, "Unable to search the web for q: Error fetching URL(s): https://a.example/: CRAWL_NOT_FOUND")
	})
}
