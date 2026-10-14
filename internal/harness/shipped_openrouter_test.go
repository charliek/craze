package harness

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The shipped catalog's OpenRouter models as a session sends them, with no
// real traffic: the openrouter driver's endpoint is fixed, so the client is
// redirected to a local server, which sees where each request was aimed.

// sentRequest is one request as the local server saw it.
type sentRequest struct {
	host, path, auth string
	body             map[string]json.RawMessage
}

// onShippedOpenRouter opens a session on the shipped catalog at model and
// effort ("" for either is the session's own choice), with OpenRouter alone
// funded, by an inline key: a providers.toml naming only that key, and no
// models.toml. It returns the session and what the server has seen; the
// server answers every request with a short text turn.
func onShippedOpenRouter(t *testing.T, model, effort string) (*Session, func() []sentRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []sentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request body: %v", err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("a request body is not JSON: %v", err)
		}
		mu.Lock()
		seen = append(seen, sentRequest{host: r.Header.Get("X-Original-Host"), path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		sseReply(textChunk("ok"), finishChunk("stop", true))(w, r)
	}))
	t.Cleanup(srv.Close)
	home := filepath.Join(t.TempDir(), "native")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	providers := "version = 1\n\n[providers.openrouter]\napi_key = \"" + canary + "\"\n"
	if err := os.WriteFile(filepath.Join(home, modeltable.ProvidersFile), []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := modeltable.Load(home)
	if err != nil {
		t.Fatalf("loading the shipped catalog: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Options{
		Home: home, Workspace: ws, Table: table, Model: model, Effort: effort,
		Getenv: func(string) string { return "" },
		Now:    testNow, Version: "v0.0.0-test",
		NewModel: func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
			return llm.New(r, llm.WithHTTPClient(redirectClient(t, srv.URL)))
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, func() []sentRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]sentRequest(nil), seen...)
	}
}

// checkSent fails unless req went to OpenRouter's Chat Completions endpoint
// with the inline key, for wire at effort in OpenRouter's reasoning object
// (never the top-level reasoning_effort), with ceiling as max_tokens.
func checkSent(t *testing.T, i int, req sentRequest, wire, effort string, ceiling int) {
	t.Helper()
	if req.host != "openrouter.ai" || req.path != "/api/v1/chat/completions" {
		t.Errorf("request %d went to %s%s, want openrouter.ai/api/v1/chat/completions", i, req.host, req.path)
	}
	if req.auth != "Bearer "+canary {
		t.Errorf("request %d's Authorization is not the inline key", i)
	}
	var model string
	var reasoning map[string]string
	var maxTokens int
	for name, into := range map[string]any{"model": &model, "reasoning": &reasoning, "max_tokens": &maxTokens} {
		if err := json.Unmarshal(req.body[name], into); err != nil {
			t.Errorf("request %d's %s = %s: %v", i, name, req.body[name], err)
		}
	}
	if model != wire || len(reasoning) != 1 || reasoning["effort"] != effort || maxTokens != ceiling {
		t.Errorf("request %d sent model %q, reasoning %v, max_tokens %d; want %q, effort %q, %d", i, model, reasoning, maxTokens, wire, effort, ceiling)
	}
	for _, absent := range []string{"reasoning_effort", "max_completion_tokens"} {
		if v, ok := req.body[absent]; ok {
			t.Errorf("request %d sent %s %s", i, absent, v)
		}
	}
}

// TestShippedGeminiArgonReachesTheWire: Gemini 4 Argon goes out on its
// numbered wire id at its default effort, medium, or the one asked for, with
// its 65536 ceiling; and a session on OpenRouter's start, Gemini Flash
// Latest, that switches to Argon at low sends the start's request, then
// Argon's.
func TestShippedGeminiArgonReachesTheWire(t *testing.T) {
	const argon, argonWire = "openrouter/gemini-4-argon", "google/gemini-4-argon"
	for _, tc := range []struct{ effort, want string }{{"", "medium"}, {"high", "high"}} {
		t.Run("at "+tc.want, func(t *testing.T) {
			s, sent := onShippedOpenRouter(t, argon, tc.effort)
			run(t, s, "hello")
			reqs := sent()
			if len(reqs) != 1 {
				t.Fatalf("the server saw %d requests, want 1", len(reqs))
			}
			checkSent(t, 1, reqs[0], argonWire, tc.want, 65536)
		})
	}
	t.Run("switched to from the start", func(t *testing.T) {
		s, sent := onShippedOpenRouter(t, "", "")
		run(t, s, "hello")
		if err := s.SetModelEffort(argon, "low"); err != nil {
			t.Fatalf("SetModelEffort: %v", err)
		}
		run(t, s, "again")
		reqs := sent()
		if len(reqs) != 2 {
			t.Fatalf("the server saw %d requests, want 2", len(reqs))
		}
		checkSent(t, 1, reqs[0], "~google/gemini-flash-latest", "medium", 65536)
		checkSent(t, 2, reqs[1], argonWire, "low", 65536)
	})
}
