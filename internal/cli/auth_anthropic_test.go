package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/anthropicmodels"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// `craze auth login anthropic` fetches the account's model list (plan 044
// §3.6 a, A5): through the real fetcher (internal/anthropicmodels) and the
// Messages API client against a fake Anthropic API reached by the test
// override, or a stand-in for the fetch (fetchAnthropicModels) where a case
// is about what login does around it. The keys are this file's dummies, and
// runAuth fails a case whose output holds one.

// fakeAnthropic is a loopback stand-in for Anthropic's model list: each
// GET /v1/models is answered by reply and the key it carried recorded.
type fakeAnthropic struct {
	mu   sync.Mutex
	keys []string
}

// useFakeAnthropic starts a fake answering with reply and points the test
// override at it for the rest of the test.
func useFakeAnthropic(t *testing.T, reply http.HandlerFunc) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("X-Api-Key"))
		f.mu.Unlock()
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(messagesapi.TestAPIEnv, srv.URL)
	return f
}

// requests is whether each request the fake saw carried key, in order.
func (f *fakeAnthropic) requests(key string) []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(f.keys))
	for i, k := range f.keys {
		out[i] = k == key
	}
	return out
}

// twoModels answers with an account list of two models, complete.
func twoModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"data":[`+
		`{"type":"model","id":"claude-opus-6","display_name":"Claude Opus 6","created_at":"2026-10-10T09:00:00Z","line":"opus","max_input_tokens":1000000,"max_tokens":128000},`+
		`{"type":"model","id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","created_at":"2026-10-07T18:00:00Z","line":"haiku","max_input_tokens":1000000,"max_tokens":128000}`+
		`],"has_more":false,"last_id":"claude-haiku-5-5"}`)
}

// echoesTheKey answers with a 401 whose message repeats the key the request
// carried (P13).
func echoesTheKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key: %s"}}`, r.Header.Get("X-Api-Key"))
}

// listOf is native's account list, as a load reads it.
func listOf(t *testing.T, native string) *modeltable.AnthropicModels {
	t.Helper()
	doc, err := modeltable.ReadAnthropicModels(native)
	if err != nil {
		t.Fatalf("the account's list: %v", err)
	}
	return doc
}

// TestAuthLoginAnthropicFetchesTheList (plan 044 §3.6 a, A5): a key stored
// for Anthropic is followed by the account's model list, fetched with it and
// written bound to it, and one line saying how many models it holds; a key
// exported ahead of the stored one is the one fetched with, as it is the one
// a session binds the list to. `craze auth logout anthropic` keeps the list.
// The control: a key stored for another provider makes no request and says
// nothing of a list.
func TestAuthLoginAnthropicFetchesTheList(t *testing.T) {
	t.Run("the saved key", func(t *testing.T) {
		native := authNative(t)
		api := useFakeAnthropic(t, twoModels)
		stdout, stderr, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "fireworks")
		if err != nil || len(api.requests(authKey)) != 0 || strings.Contains(stdout, "Fetched") || stderr != "" {
			t.Fatalf("control: a Fireworks login: %v, %d requests, stdout %q, stderr %q", err, len(api.requests(authKey)), maskKeys(stdout), maskKeys(stderr))
		}
		stdout, stderr, err = runAuth(t, strings.NewReader(authKey2+"\n"), "auth", "login", "anthropic")
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		want := "Saved the Anthropic key in " + filepath.Join(native, modeltable.ProvidersFile) + ".\nFetched 2 Anthropic models.\n"
		if stdout != want || stderr != "" {
			t.Fatalf("stdout %q, stderr %q; want %q and nothing", maskKeys(stdout), maskKeys(stderr), want)
		}
		if got := api.requests(authKey2); len(got) != 1 || !got[0] {
			t.Fatalf("requests with the saved key: %v; want one", got)
		}
		if doc := listOf(t, native); doc.KeyID != modeltable.AnthropicKeyID(authKey2) || len(doc.Models) != 2 {
			t.Fatalf("the list: %d models, bound to the saved key %t", len(doc.Models), doc.KeyID == modeltable.AnthropicKeyID(authKey2))
		}
		if _, _, err := runAuth(t, strings.NewReader(""), "auth", "logout", "anthropic"); err != nil {
			t.Fatalf("logout: %v", err)
		}
		if storedKey(t, native, modeltable.AnthropicProvider) != "" || len(listOf(t, native).Models) != 2 {
			t.Fatal("logout did not remove the key, or did not keep the list")
		}
	})

	t.Run("an exported key ahead of it", func(t *testing.T) {
		native := authNative(t)
		api := useFakeAnthropic(t, twoModels)
		t.Setenv("ANTHROPIC_API_KEY", authEnvK)
		stdout, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "anthropic")
		if err != nil || !strings.HasSuffix(stdout, "ANTHROPIC_API_KEY is set in this environment; craze uses it before the stored key.\nFetched 2 Anthropic models.\n") {
			t.Fatalf("login: %v, stdout %q", err, maskKeys(stdout))
		}
		if got := api.requests(authEnvK); len(got) != 1 || !got[0] {
			t.Fatalf("requests with the exported key: %v; want one", got)
		}
		if listOf(t, native).KeyID != modeltable.AnthropicKeyID(authEnvK) {
			t.Fatal("the list is not bound to the exported key")
		}
	})
}

// TestAuthLoginAnthropicFetchFailureIsANote (plan 044 §3.6 a, P13, A5): a
// list the account refuses — with a 401 whose message repeats the key — is
// one note naming the status and never the key (runAuth fails the case on a
// key in any stream), the key stays saved, the login exits 0, and no list is
// written.
func TestAuthLoginAnthropicFetchFailureIsANote(t *testing.T) {
	native := authNative(t)
	api := useFakeAnthropic(t, echoesTheKey)
	stdout, stderr, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "anthropic")
	if err != nil {
		t.Fatalf("a refused list failed the login: %v", err)
	}
	if strings.Contains(stdout, "Fetched") || storedKey(t, native, modeltable.AnthropicProvider) != authKey {
		t.Fatalf("stdout %q; want the key saved and no list", maskKeys(stdout))
	}
	if got := api.requests(authKey); len(got) != 1 || !got[0] {
		t.Fatalf("control: requests with the key %v; want the one refused", got)
	}
	if !strings.HasPrefix(stderr, "note: the Anthropic model list could not be fetched (") || !strings.Contains(stderr, "401") ||
		!strings.HasSuffix(stderr, "; a native session fetches it when it opens\n") || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("stderr %q; want one note naming the status", maskKeys(stderr))
	}
	if _, err := os.Stat(filepath.Join(native, modeltable.AnthropicModelsFile)); !os.IsNotExist(err) {
		t.Fatalf("a refused list wrote %s (%v)", modeltable.AnthropicModelsFile, err)
	}
}

// TestAuthLoginAnthropicFetchIsBounded (plan 044 §3.6 a): login's fetch runs
// under a deadline of at most anthropicListWait — a fetch that never ends is
// cut there, a note, and the key stays saved. The control: the stand-in was
// called, with the saved key.
func TestAuthLoginAnthropicFetchIsBounded(t *testing.T) {
	native := authNative(t)
	var deadline time.Time
	var called bool
	old := fetchAnthropicModels
	fetchAnthropicModels = func(ctx context.Context, dir, key string) (int, error) {
		called = dir == native && key == authKey
		deadline, _ = ctx.Deadline()
		return 0, fmt.Errorf("anthropicmodels: %w", context.DeadlineExceeded)
	}
	t.Cleanup(func() { fetchAnthropicModels = old })
	_, stderr, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "anthropic")
	end := time.Now()
	if err != nil || !called {
		t.Fatalf("login: %v; the stand-in called with the saved key %t", err, called)
	}
	if deadline.IsZero() || deadline.After(end.Add(anthropicListWait)) {
		t.Fatalf("the fetch's deadline is %v after the login ended; want none past %v", deadline.Sub(end), anthropicListWait)
	}
	if !strings.Contains(stderr, "could not be fetched (context deadline exceeded)") || storedKey(t, native, modeltable.AnthropicProvider) != authKey {
		t.Fatalf("stderr %q; want the cut fetch's note, and the key saved", maskKeys(stderr))
	}
}

// TestAuthLoginAnthropicOfTheUsersOwn (plan 044 X1): with the catalog off, a
// provider of the user's own named anthropic — a gateway — is no Anthropic
// account: storing its key fetches no list and says nothing of one. The
// control: the login saved the key.
func TestAuthLoginAnthropicOfTheUsersOwn(t *testing.T) {
	native := authNative(t)
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		modeltable.ProvidersFile: "version = 1\n\n[providers.anthropic]\ndriver = \"openai-compat\"\nbase_url = \"https://gateway.example/v1\"\nenv_keys = [\"MY_GATEWAY_KEY\"]\n",
		modeltable.ModelsFile:    "version = 1\ncatalog = false\ndefault_model = \"claude\"\n\n[models.claude]\nprovider = \"anthropic\"\nwire_model = \"claude-opus-5-5\"\n",
	} {
		if err := os.WriteFile(filepath.Join(native, name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var called bool
	old := fetchAnthropicModels
	fetchAnthropicModels = func(context.Context, string, string) (int, error) { called = true; return 0, nil }
	t.Cleanup(func() { fetchAnthropicModels = old })
	stdout, stderr, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "anthropic")
	if err != nil || storedKey(t, native, modeltable.AnthropicProvider) != authKey {
		t.Fatalf("control: login: %v (stdout %q)", err, maskKeys(stdout))
	}
	if called || strings.Contains(stdout+stderr, "Anthropic model") {
		t.Fatalf("a list was fetched for the user's own provider: stdout %q, stderr %q", maskKeys(stdout), maskKeys(stderr))
	}
}

// TestAnthropicListFetchIsTheForcedOne pins the seam's production value to
// anthropicmodels.Fetch, the forced form that never overwrites a newer list
// (P8), by its behaviour: it fetches a list that is current, where the
// background Refresh would not. The control: Refresh fetches nothing then.
func TestAnthropicListFetchIsTheForcedOne(t *testing.T) {
	native := authNative(t)
	api := useFakeAnthropic(t, twoModels)
	if _, err := fetchAnthropicModels(context.Background(), native, authKey); err != nil {
		t.Fatal(err)
	}
	if fetched, err := anthropicmodels.Refresh(context.Background(), native, authKey, anthropicmodels.MaxAge); err != nil || fetched {
		t.Fatalf("control: Refresh of a current list = %t, %v", fetched, err)
	}
	if n, err := fetchAnthropicModels(context.Background(), native, authKey); err != nil || n != 2 {
		t.Fatalf("the second fetch = %d, %v", n, err)
	}
	if got := api.requests(authKey); len(got) != 2 {
		t.Fatalf("%d requests; want the two forced fetches'", len(got))
	}
	b, err := os.ReadFile(filepath.Join(native, modeltable.AnthropicModelsFile))
	if err != nil || !json.Valid(b) {
		t.Fatalf("the list file: %v", err)
	}
}
