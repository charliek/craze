package websearch

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// Status and SetKey (issues #137, #141): the session's totals, the key
// source, and a key stored while the session runs.

// TestStatusCounts: the totals count every handled call, search or fetch,
// its failures and, of those, the rate limits that outlasted the retries,
// and the retries themselves.
func TestStatusCounts(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, rec received) {
		switch q, _ := rec.Call.Params.Arguments["query"].(string); q {
		case "limited":
			http429("")(w, r, rec)
		case "broken":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			replyText("Title: T\nURL: https://a.example/\nText: body")(w, r, rec)
		}
	})
	s := withConfig(t, f, Config{})
	for _, q := range []string{"ok", "limited", "broken", "ok"} {
		_, _ = search(s, q)
	}
	_, _ = s.Fetch(context.Background(), "https://a.example/")
	got := s.Status()
	want := Status{Provider: "exa", Name: "Exa", Key: KeyNone, KeyEnvName: "EXA_API_KEY", Consent: "allowed",
		Searches: 4, Fetches: 1, Failed: 2, RateLimited: 1, Retries: maxAttempts - 1}
	if got != want {
		t.Fatalf("Status = %+v,\nwant %+v", got, want)
	}
}

// TestStatusKeySource: the source a key was resolved from is the status's,
// a usable key with none named reads as stored, and no usable key is
// keyless whatever source is named.
func TestStatusKeySource(t *testing.T) {
	f := newFake(t, replyText(oneResult))
	useEndpoint(t, f.srv.URL+"/mcp")
	for _, tc := range []struct {
		key    string
		source KeySource
		want   KeySource
	}{
		{"exa-0123456789abcdef", KeyEnv, KeyEnv},
		{"exa-0123456789abcdef", KeyStored, KeyStored},
		{"exa-0123456789abcdef", "", KeyStored},
		{"", KeyEnv, KeyNone},
		{"short", KeyStored, KeyNone},
	} {
		s, err := New(Config{Key: tc.key, KeySource: tc.source})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Status().Key; got != tc.want {
			t.Errorf("key %q from %q: Status().Key = %q, want %q", tc.key, tc.source, got, tc.want)
		}
	}
}

// TestStatusConsent: where an undecided session's consent stands, before
// and after each answer.
func TestStatusConsent(t *testing.T) {
	for _, tc := range []struct {
		pick []int
		want string
	}{
		{[]int{0}, "allowed"},
		{[]int{1}, "allowed for this session"},
		{[]int{2}, "disabled"},
		{nil, "declined"},
	} {
		f := newFake(t, replyText(oneResult))
		answer := picks()
		if tc.pick != nil {
			answer = picks(tc.pick)
		}
		s := consented(t, f, &fakeAsker{answer: answer}, true, statePath(t), nil)
		if got := s.Status().Consent; got != "undecided" {
			t.Fatalf("before: %q", got)
		}
		_, _ = search(s, "q")
		if got := s.Status().Consent; got != tc.want {
			t.Errorf("after %v: %q, want %q", tc.pick, got, tc.want)
		}
	}
}

// TestSetKey: a key handed over mid-session is sent from the next call on,
// its source is the status's, a keyless rate limit stops naming how to get a
// key, and both keys — the one before and the new one — stay scrubbed from
// what comes back.
func TestSetKey(t *testing.T) {
	const (
		old    = "exa-old-0123456789abcdef"
		stored = "exa-new-0123456789abcdef"
	)
	limited := false
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, rec received) {
		if limited {
			http429("")(w, r, rec)
			return
		}
		replyText("Title: "+old+" "+stored+"\nURL: https://a.example/\nText: body")(w, r, rec)
	})
	useEndpoint(t, f.srv.URL+"/mcp")
	s, err := New(Config{Key: old, KeySource: KeyStored})
	if err != nil {
		t.Fatal(err)
	}
	if !s.SetKey("  "+stored+"\n", KeyStored) {
		t.Fatal("SetKey changed nothing")
	}
	results, err := search(s, "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(results[0].Title, old) || strings.Contains(results[0].Title, stored) {
		t.Fatalf("a key came back: %q", results[0].Title)
	}
	if got := f.calls()[0].Header.Get("X-Api-Key"); got != stored {
		t.Fatalf("x-api-key = %q", got)
	}
	if s.Status().Key != KeyStored {
		t.Fatalf("source = %q", s.Status().Key)
	}
	if s.SetKey(stored, KeyStored) {
		t.Fatal("the same key again reported a change")
	}
	limited = true
	_, err = search(s, "q")
	wantErr(t, err, tool.ClassToolError, limitedKeyed)
}

// TestTheVariableComesFirst: a session that opened with EXA_API_KEY's key
// keeps it when a key is stored: the variable comes first, as at Open.
func TestTheVariableComesFirst(t *testing.T) {
	f := newFake(t, replyText(oneResult))
	useEndpoint(t, f.srv.URL+"/mcp")
	s, err := New(Config{Key: "exa-env-0123456789abcdef", KeySource: KeyEnv})
	if err != nil {
		t.Fatal(err)
	}
	if s.SetKey("exa-stored-0123456789abcdef", KeyStored) {
		t.Fatal("a stored key replaced the variable's")
	}
	_, _ = search(s, "q")
	if v := f.calls()[0].Header.Get("X-Api-Key"); v != "exa-env-0123456789abcdef" || s.Status().Key != KeyEnv {
		t.Fatalf("x-api-key = %q, source %q", v, s.Status().Key)
	}
}

// TestSetKeyFromKeyless: a keyless session handed a key is keyed from then
// on, and one handed an unusable key, or none, stays keyless.
func TestSetKeyFromKeyless(t *testing.T) {
	f := newFake(t, replyText(oneResult))
	useEndpoint(t, f.srv.URL+"/mcp")
	s, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "short", "redacted-credential"} {
		if s.SetKey(bad, KeyStored) {
			t.Fatalf("SetKey(%q) changed the key", bad)
		}
	}
	if s.SetKey("exa-0123456789abcdef", "elsewhere") {
		t.Fatal("an unknown source was taken")
	}
	_, _ = search(s, "q")
	if s.SetKey("exa-0123456789abcdef", KeyStored) != true {
		t.Fatal("SetKey changed nothing")
	}
	_, _ = search(s, "q")
	got := f.calls()
	if got[0].Header.Get("X-Api-Key") != "" || got[1].Header.Get("X-Api-Key") != "exa-0123456789abcdef" {
		t.Fatalf("keys sent: %q then %q", got[0].Header.Get("X-Api-Key"), got[1].Header.Get("X-Api-Key"))
	}
}

// TestSetKeyUnderTheOverride: a service at the test override never sends a
// key, a key handed over included.
func TestSetKeyUnderTheOverride(t *testing.T) {
	f := newFake(t, replyText(oneResult))
	s := overridden(t, f, "")
	if s.SetKey("exa-0123456789abcdef", KeyStored) {
		t.Fatal("SetKey changed the key under the override")
	}
	_, _ = search(s, "q")
	if v := f.calls()[0].Header.Get("X-Api-Key"); v != "" {
		t.Fatalf("x-api-key = %q", v)
	}
}

// TestScrubberWith: a scrubber widened by a key takes out both keys, and
// one widened by a key it holds, or by none usable, is itself.
func TestScrubberWith(t *testing.T) {
	a, b := "exa-aaaa-0123456789", "exa-bbbb-0123456789"
	s := newScrubber(a)
	w := s.with(b)
	if got := w.text(a + " " + b); strings.Contains(got, a) || strings.Contains(got, b) {
		t.Fatalf("with: %q", got)
	}
	if got := s.text(b); got != b {
		t.Fatalf("the original changed: %q", got)
	}
	if s.with(a) != s || s.with("short") != s {
		t.Fatal("a key held, or none usable, made a new scrubber")
	}
	var none *scrubber
	if got := none.with(b).text(b); strings.Contains(got, b) {
		t.Fatalf("nil.with: %q", got)
	}
}
