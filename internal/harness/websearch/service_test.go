package websearch

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The service (plan 051 §B, C1): when the key is sent, the test override, and
// the provider table. Consent is not here yet (§C).

// TestTheKeyIsSentOnlyWhenUsable: a key the session's redactor could not hold
// — too short, overlapping the marker, blank — is never sent, as if there
// were none (and a rate limit then names the variable that raises it); a
// usable key goes in x-api-key, trimmed.
func TestTheKeyIsSentOnlyWhenUsable(t *testing.T) {
	limited := func(w http.ResponseWriter, _ *http.Request, _ received) { w.WriteHeader(http.StatusTooManyRequests) }
	for _, tc := range []struct {
		name, key, sent string
	}{
		{"too short", "1234567", ""},
		{"too short once trimmed", "  1234567  ", ""},
		{"inside the marker", "redacted-credential", ""},
		{"across the marker's end", "credential]-and-more", ""},
		{"blank", "   ", ""},
		{"none", "", ""},
		{"usable", " exa-0123456789abcdef ", "exa-0123456789abcdef"},
		{"usable at the floor", "12345678", "12345678"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, limited)
			_, err := search(keyed(t, f, tc.key, 0), "q")
			want := "Web search rate limited (HTTP 429)"
			if tc.sent == "" {
				want += ". Setting EXA_API_KEY raises the limit"
			}
			wantErr(t, err, tool.ClassToolError, want)
			got := f.calls()
			if len(got) != 1 {
				t.Fatalf("%d requests", len(got))
			}
			if v, ok := got[0].Header["X-Api-Key"]; tc.sent == "" && ok {
				t.Fatalf("an unusable key was sent: x-api-key present (%d values)", len(v))
			}
			if v := got[0].Header.Get("X-Api-Key"); v != tc.sent {
				t.Fatalf("x-api-key = %q, want %q", v, tc.sent)
			}
		})
	}
}

// TestNoKeyUnderTheOverride: a service at the test override sends no key,
// however usable — the key goes to the provider's own endpoint only — and
// still scrubs it from what comes back.
func TestNoKeyUnderTheOverride(t *testing.T) {
	const key = "exa-0123456789abcdef"
	f := newFake(t, replyText("Title: "+key+"\nURL: https://a.example/\nHighlights:\nx"))
	results, err := search(overridden(t, f, key), "q")
	if err != nil || len(results) != 1 || results[0].Title != redact.Marker {
		t.Fatalf("Search = %+v, %v", results, err)
	}
	for _, rec := range f.calls() {
		if _, ok := rec.Header["X-Api-Key"]; ok || rec.Header.Get("Authorization") != "" || strings.Contains(string(rec.Body), key) {
			t.Fatal("the key went to the override")
		}
	}
}

// TestOverride: CRAZE_TEST_EXA_MCP is honoured only as an http URL on a
// loopback IP literal with nothing but a path besides, which is kept (less a
// trailing slash); unset is no override; anything else is ErrOverride, whose
// text does not echo the value — a name (localhost included, which a
// resolver could send anywhere), https, a public or private address, a user,
// a query, a fragment, an opaque URL, text that is no URL.
func TestOverride(t *testing.T) {
	env := func(v string) func(string) string {
		return func(name string) string {
			if name == TestEndpointEnv {
				return v
			}
			return ""
		}
	}
	for v, want := range map[string]string{
		"":                            "",
		"http://127.0.0.1:8080/mcp":   "http://127.0.0.1:8080/mcp",
		"http://127.0.0.1:8080/":      "http://127.0.0.1:8080",
		"http://127.0.0.2:9":          "http://127.0.0.2:9",
		"http://[::1]:8080/r/tok/mcp": "http://[::1]:8080/r/tok/mcp",
	} {
		got, err := Override(env(v))
		if err != nil || got != want {
			t.Errorf("Override(%q) = %q, %v; want %q", v, got, err, want)
		}
	}
	for _, v := range []string{
		"http://localhost:8080/mcp",
		"https://127.0.0.1:8080/mcp",
		"http://10.0.0.1/mcp",
		"http://192.168.1.2/mcp",
		"http://mcp.exa.ai/mcp",
		"https://mcp.exa.ai/mcp",
		"http://user:pw@127.0.0.1/mcp",
		"http://127.0.0.1/mcp?x=1",
		"http://127.0.0.1/mcp?",
		"http://127.0.0.1/mcp#f",
		"http:127.0.0.1",
		"127.0.0.1:8080",
		"::not a url::",
		"http://[::ffff:8.8.8.8]/mcp",
	} {
		got, err := Override(env(v))
		if !errors.Is(err, ErrOverride) || got != "" {
			t.Errorf("Override(%q) = %q, %v; want ErrOverride", v, got, err)
			continue
		}
		if strings.Contains(err.Error(), v) {
			t.Errorf("the refusal echoes %q", v)
		}
	}
	// A nil getenv is the process's environment.
	t.Setenv(TestEndpointEnv, "http://127.0.0.1:1/x")
	if got, err := Override(nil); err != nil || got != "http://127.0.0.1:1/x" {
		t.Fatalf("Override(nil) = %q, %v", got, err)
	}
}

// TestNewRefuses: New refuses a provider the table does not have, and an
// endpoint override that is not a loopback http URL — so a misconfigured
// override never reaches anywhere else — and builds the default provider's
// service from an empty Config.
func TestNewRefuses(t *testing.T) {
	if _, err := New(Config{Provider: "bing"}); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("an unknown provider: %v", err)
	}
	for _, ep := range []string{"https://mcp.exa.ai/mcp", "http://localhost:1/mcp", "http://10.1.2.3/mcp"} {
		if _, err := New(Config{Endpoint: ep, Key: "exa-0123456789abcdef"}); !errors.Is(err, ErrOverride) {
			t.Errorf("Endpoint %q: %v", ep, err)
		}
	}
	for _, id := range []string{"", "exa"} {
		if s, err := New(Config{Provider: id}); err != nil || s == nil {
			t.Errorf("New(%q) = %v, %v", id, s, err)
		}
	}
	if !slices.Equal(Providers(), []string{"exa"}) || DefaultProvider != "exa" || ExaEndpoint != "https://mcp.exa.ai/mcp" {
		t.Errorf("the table: %q, default %q, Exa at %q", Providers(), DefaultProvider, ExaEndpoint)
	}
	if MaxReply != 256<<10 || CallTimeout.Seconds() != 25 {
		t.Errorf("MaxReply %d, CallTimeout %v", MaxReply, CallTimeout)
	}
}

// TestTheKeyRuleIsModeltables: usable is modeltable's rule for a key craze
// redacts (KeyPattern's floor and the marker), which this package may not
// import: a key KeyProblem accepts is usable and one it refuses is not, a
// blank one being no key for either; and the variable an Exa key is read
// from is the one the model table registers as a credential.
func TestTheKeyRuleIsModeltables(t *testing.T) {
	if minKeyLen != modeltable.MinKeyLen {
		t.Fatalf("minKeyLen %d, modeltable.MinKeyLen %d", minKeyLen, modeltable.MinKeyLen)
	}
	for _, k := range []string{"", " ", "1234567", "12345678", " 12345678 ", "\t1234567\n", "redacted", "credential]x-0001",
		"x-0001[craze:", redact.Marker, "exa-0123456789abcdef", weirdKey, "éééé"} {
		blank := strings.TrimSpace(k) == ""
		if want := !blank && modeltable.KeyProblem(k) == nil; usable(k) != want {
			t.Errorf("usable(%q) = %t, KeyProblem says %t", k, usable(k), want)
		}
	}
	if ExaKeyEnv != modeltable.ExaKeyEnv {
		t.Fatalf("ExaKeyEnv %q, modeltable.ExaKeyEnv %q", ExaKeyEnv, modeltable.ExaKeyEnv)
	}
}
