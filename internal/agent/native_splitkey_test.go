package agent

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// TestNativeSplitKeyNeverShows (plan 050 review): a provider that echoes the
// session's key split by a format character — a zero-width space, a word
// joiner, a byte-order mark, a soft hyphen — has it redacted whole by the
// model's scrubber, before the harness cuts the message to its bound and
// before anything sanitizes it, on every status a message is shown for (a
// 403, a 429, a 500) and on both the openai-compat and the Anthropic driver.
// The cut cannot leave the key's front part either: a key longer than the
// bound, split after every character, still never shows. The canary appears
// nowhere in the phrased error or the session's events.
func TestNativeSplitKeyNeverShows(t *testing.T) {
	long := "sk-long-canary-" + strings.Repeat("0123456789abcdef", 24) // 399 bytes: longer than the 300-byte cut
	split := func(key, sep string, every bool) string {
		if !every {
			h := len(key) / 2
			return key[:h] + sep + key[h:]
		}
		var b strings.Builder
		for i, r := range key {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	type driver struct {
		name    string
		key     string
		session func(t *testing.T, key string, replies []http.HandlerFunc) Session
		reply   func(status int, msg string) http.HandlerFunc
	}
	drivers := []driver{
		{"openai-compat", nativeCanary, splitKeyWireSession, func(status int, msg string) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("retry-after-ms", "1")
				jsonError(w, status, msg)
			}
		}},
		{"anthropic", nativeAnthropicKey, splitKeyAnthropicSession, func(status int, msg string) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("retry-after-ms", "1")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				typ := map[int]string{403: "permission_error", 429: "rate_limit_error", 500: "api_error"}[status]
				fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
			}
		}},
	}
	for _, d := range drivers {
		for _, status := range []int{403, 429, 500} {
			for _, sep := range []string{"\u200b", "\u2060", "\ufeff", "\u00ad"} {
				for _, tc := range []struct {
					name  string
					key   string
					every bool
				}{{"the session's key", d.key, false}, {"a key longer than the cut", long, true}} {
					name := fmt.Sprintf("%s/%d/U+%04X/%s", d.name, status, []rune(sep)[0], tc.name)
					t.Run(name, func(t *testing.T) {
						replies := make([]http.HandlerFunc, 8) // a retried status is asked again
						for i := range replies {
							replies[i] = d.reply(status, "denied: "+split(tc.key, sep, tc.every))
						}
						s := d.session(t, tc.key, replies)
						_, err := s.Prompt(context.Background(), "hi")
						evs := drained(s)
						if err == nil {
							t.Fatal("Prompt succeeded; want the provider's failure")
						}
						if !strings.Contains(err.Error(), "denied: ") {
							t.Fatalf("error %q does not show the provider's message", err)
						}
						for _, v := range []any{err, evs} {
							if leaks := nativeLeaks(v, tc.key); len(leaks) > 0 {
								t.Fatalf("the key leaked at %v in %q", leaks, err)
							}
						}
						// Nor a front part long enough to be one: the first
						// sixteen bytes of the key, with the separators gone.
						if shown := dropFormat(err.Error()); strings.Contains(shown, tc.key[:16]) {
							t.Fatalf("the key's front part shows in %q", err)
						}
					})
				}
			}
		}
	}
}

// splitKeyWireSession is wireSession with key in place of the canary.
func splitKeyWireSession(t *testing.T, key string, replies []http.HandlerFunc) Session {
	t.Helper()
	api := newFakeEndpoint(t)
	api.replies = replies
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	table := &modeltable.Table{
		NoCatalog:    true,
		DefaultModel: "wire/m",
		Providers: map[string]modeltable.Provider{
			"wire": {Driver: modeltable.DriverOpenAICompat, BaseURL: api.srv.URL + "/v1", APIKey: modeltable.Secret(key)},
		},
		Models: map[string]modeltable.Model{"wire/m": {Provider: "wire", WireModel: "wire-model"}},
	}
	if err := modeltable.Save(filepath.Join(home, "native"), table); err != nil {
		t.Fatal(err)
	}
	s := NewNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" }
	})
	closeAtCleanup(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// splitKeyAnthropicSession is a started session on a Claude model funded by
// key, built by the real llm factory, whose requests go to a loopback fake
// answering replies (anthropicNative, which shows the override is honoured).
func splitKeyAnthropicSession(t *testing.T, key string, replies []http.HandlerFunc) Session {
	t.Helper()
	api := newFakeEndpoint(t)
	api.replies = replies
	f := newNativeFixture(t)
	table := nativeTestTable("http://127.0.0.1:9/v1")
	table.Providers[modeltable.AnthropicProvider] = modeltable.Provider{Driver: modeltable.DriverAnthropic, EnvKeys: []string{"NATIVE_ANTHROPIC_KEY"}}
	table.Models[nativeAnthropicModel] = modeltable.Model{Provider: modeltable.AnthropicProvider, WireModel: "claude-test-5", Name: "Claude Test 5",
		ContextWindow: 200000, MaxOutputTokens: 8000}
	table.DefaultModel = nativeAnthropicModel
	if err := modeltable.Save(f.dir, table); err != nil {
		t.Fatal(err)
	}
	f.env["NATIVE_ANTHROPIC_KEY"] = key
	f.edit = func(o *harness.Options) { o.NewModel = nil }
	t.Setenv(messagesapi.TestAPIEnv, api.srv.URL)
	return f.started(Options{})
}
