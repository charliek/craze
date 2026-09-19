package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// captured is one request as the test server saw it.
type captured struct {
	host   string // the host the client aimed at, before any redirect
	path   string
	header http.Header
	body   map[string]any
}

// recorder answers every request with a short text turn and keeps what it
// was sent.
type recorder struct {
	mu   sync.Mutex
	reqs []captured
}

func (rec *recorder) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		host := r.Header.Get("X-Original-Host")
		if host == "" {
			host = r.Host
		}
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, captured{host: host, path: r.URL.Path, header: r.Header.Clone(), body: body})
		rec.mu.Unlock()
		sse(w, textChunk("ok"), finishChunk("stop", true))
	}
}

func (rec *recorder) only(t *testing.T) captured {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) != 1 {
		t.Fatalf("the server saw %d requests, want 1", len(rec.reqs))
	}
	return rec.reqs[0]
}

// redirect sends every request to target, noting the host it was aimed at.
// It is how a test reaches a local server from the openrouter driver, whose
// endpoint is fixed.
type redirect struct{ target *url.URL }

func (rt redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Original-Host", req.URL.Host)
	r.URL.Scheme, r.URL.Host, r.Host = rt.target.Scheme, rt.target.Host, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

func redirectClient(t *testing.T, to string) *http.Client {
	t.Helper()
	u, err := url.Parse(to)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: redirect{target: u}}
}

// openRouter is resolved's entry on an openrouter-driver provider.
func openRouter() modeltable.Resolved {
	r := resolved("")
	r.ProviderID, r.Driver, r.Alias = "or", modeltable.DriverOpenRouter, "or/model"
	return r
}

// turn runs one agent turn on lm with the given provider options.
func turn(t *testing.T, lm fantasy.LanguageModel, opts fantasy.ProviderOptions) {
	t.Helper()
	agent := fantasy.NewAgent(lm, fantasy.WithMaxRetries(0))
	if _, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "hi", ProviderOptions: opts}); err != nil {
		t.Fatalf("turn: %v", err)
	}
}

func TestNewAimsTheRightEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		r        func(srvURL string) modeltable.Resolved
		redirect bool
		wantHost string // "" = the test server itself
		wantPath string
	}{
		{"openai-compat", func(u string) modeltable.Resolved { return resolved(u + "/v1") }, false, "", "/v1/chat/completions"},
		{"openrouter", func(string) modeltable.Resolved { return openRouter() }, true, "openrouter.ai", "/api/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			srv := newServer(t, rec.handler(t))
			r := tc.r(srv.URL)
			var opts []Option
			if tc.redirect {
				opts = append(opts, WithHTTPClient(redirectClient(t, srv.URL)))
			}
			lm, err := New(r, opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if lm.Provider() != r.ProviderID || lm.Model() != r.WireModel {
				t.Errorf("Provider(), Model() = %q, %q; want %q, %q", lm.Provider(), lm.Model(), r.ProviderID, r.WireModel)
			}
			turn(t, lm, nil)
			got := rec.only(t)
			wantHost := tc.wantHost
			if wantHost == "" {
				wantHost = strings.TrimPrefix(srv.URL, "http://")
			}
			if got.host != wantHost || got.path != tc.wantPath {
				t.Errorf("request went to %s%s, want %s%s", got.host, got.path, wantHost, tc.wantPath)
			}
			if auth := got.header.Get("Authorization"); auth != "Bearer "+canary {
				t.Errorf("Authorization = %q, want the resolved key", auth)
			}
			if got.body["model"] != r.WireModel {
				t.Errorf("model = %v, want the wire id %q", got.body["model"], r.WireModel)
			}
			// The output ceiling is per call (fantasy.Call.MaxOutputTokens),
			// the runner's to set; the factory never sends one.
			if _, ok := got.body["max_tokens"]; ok {
				t.Errorf("the factory sent max_tokens = %v", got.body["max_tokens"])
			}
		})
	}
}

func TestEffortReachesTheWire(t *testing.T) {
	noEfforts := resolved("")
	noEfforts.Efforts = nil
	cases := []struct {
		name          string
		r             modeltable.Resolved
		effort        string
		wantEffort    any // body["reasoning_effort"]; nil = absent
		wantReasoning any // body["reasoning"]; nil = absent
	}{
		{"openai-compat", resolved(""), "high", "high", nil},
		{"openai-compat, unset", resolved(""), "", nil, nil},
		{"openai-compat, no effort control", noEfforts, "", nil, nil},
		{"openrouter", openRouter(), "low", nil, map[string]any{"effort": "low"}},
		{"openrouter, unset", openRouter(), "", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			srv := newServer(t, rec.handler(t))
			r := tc.r
			if r.Driver == modeltable.DriverOpenAICompat {
				r.BaseURL = srv.URL
			}
			lm, err := New(r, WithHTTPClient(redirectClient(t, srv.URL)))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			opts, err := EffortOptions(r, tc.effort)
			if err != nil {
				t.Fatalf("EffortOptions: %v", err)
			}
			if tc.effort == "" && opts != nil {
				t.Errorf("EffortOptions(%q) = %v, want nil", tc.effort, opts)
			}
			turn(t, lm, opts)
			body := rec.only(t).body
			if got := body["reasoning_effort"]; !equalJSON(got, tc.wantEffort) {
				t.Errorf("reasoning_effort = %v, want %v", got, tc.wantEffort)
			}
			if got := body["reasoning"]; !equalJSON(got, tc.wantReasoning) {
				t.Errorf("reasoning = %v, want %v", got, tc.wantReasoning)
			}
		})
	}
}

func equalJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestEffortOptionsRefusesWhatCannotBeSent(t *testing.T) {
	noEfforts := resolved("")
	noEfforts.Efforts = nil
	odd := resolved("")
	odd.Efforts = []string{"turbo"}
	oddRouter := openRouter()
	oddRouter.Efforts = []string{"turbo"}
	unknown := resolved("")
	unknown.Driver = "grpc"

	for _, tc := range []struct {
		name   string
		r      modeltable.Resolved
		effort string
		ok     bool
	}{
		{"an effort the model does not list", resolved(""), "max", false},
		{"any effort on a model without effort control", noEfforts, "high", false},
		{"an effort the OpenAI-compatible request cannot carry", odd, "turbo", false},
		{"OpenRouter validates its own values", oddRouter, "turbo", true},
		{"an unknown driver", unknown, "high", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := EffortOptions(tc.r, tc.effort)
			if (err == nil) != tc.ok {
				t.Fatalf("EffortOptions(%q) = %v, %v; want ok=%v", tc.effort, opts, err, tc.ok)
			}
		})
	}
}

func TestNewRefusesWhatCannotBeBuilt(t *testing.T) {
	// An empty key would let the OpenAI SDK fall back to OPENAI_API_KEY and
	// send it to another provider.
	t.Setenv("OPENAI_API_KEY", canary+"-ambient")
	noKey := resolved("http://127.0.0.1:1")
	noKey.APIKey = ""
	if _, err := New(noKey); !errors.Is(err, modeltable.ErrNoAPIKey) {
		t.Errorf("New with no key: err = %v, want ErrNoAPIKey", err)
	}

	// A key short enough to be ordinary text would shred every scrubbed
	// message; the refusal names the provider but never the key.
	for _, key := range []string{"a", "error", "zq-1234", "  zq-1234  "} {
		short := resolved("http://127.0.0.1:1")
		short.APIKey = modeltable.Secret(key)
		_, err := New(short)
		if !errors.Is(err, ErrAPIKeyTooShort) {
			t.Errorf("New with key %q: err = %v, want ErrAPIKeyTooShort", key, err)
			continue
		}
		// "zq-1234" is text no message would hold by accident.
		if msg := err.Error(); strings.Contains(msg, "zq-1234") || !strings.Contains(msg, `"test"`) {
			t.Errorf("New with key %q: error %q must name the provider and not the key", key, msg)
		}
	}
	eight := resolved("http://127.0.0.1:1")
	eight.APIKey = "sk-12345"
	if _, err := New(eight); err != nil {
		t.Errorf("New with an 8-byte key: %v", err)
	}

	// With no base URL Fantasy would fall back to api.openai.com and send
	// this provider's key to OpenAI.
	for _, baseURL := range []string{"", "   "} {
		if _, err := New(resolved(baseURL)); err == nil || !strings.Contains(err.Error(), "needs a base URL") {
			t.Errorf("New with base URL %q: err = %v, want a missing-base-URL error", baseURL, err)
		}
	}

	unknown := resolved("http://127.0.0.1:1")
	unknown.Driver = "grpc"
	if _, err := New(unknown); err == nil || !strings.Contains(err.Error(), `unknown driver "grpc"`) {
		t.Errorf("New with driver grpc: err = %v, want an unknown-driver error", err)
	}
}

// TestNewIgnoresTheOpenAIEnvironment: the OpenAI SDK reads OPENAI_* variables
// into every client it builds. None of them may redirect a request, replace
// its key, or put the owner's OpenAI identifiers in front of another
// provider.
func TestNewIgnoresTheOpenAIEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-ambient-openai-key")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("OPENAI_ORG_ID", "org-ambient")
	t.Setenv("OPENAI_PROJECT_ID", "proj-ambient")

	var rec recorder
	srv := newServer(t, rec.handler(t))
	turn(t, wrapped(t, srv.URL), nil)
	got := rec.only(t)
	if auth := got.header.Get("Authorization"); auth != "Bearer "+canary {
		t.Errorf("Authorization = %q, want the resolved key", auth)
	}
	for _, h := range []string{"OpenAI-Organization", "OpenAI-Project"} {
		if v := got.header.Get(h); v != "" {
			t.Errorf("%s = %q reached the provider", h, v)
		}
	}
}
