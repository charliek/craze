package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// onChatGPT puts the fixture's default model, test/a, on the ChatGPT plan:
// the provider chatgpt on its driver in providers.toml, test/a on it in
// models.toml, and the fixture's home signed in with plan usage — the
// registration, and a token file that holds no token, since the table only
// stats it (plan 033 §3.11) and the scripted models never ask for one. The
// table is loaded again from the files.
func onChatGPT(f *fixture) {
	f.t.Helper()
	signInFixture(f.t, f.home)
	edit := func(name, old, new string) {
		path := filepath.Join(f.home, name)
		b, err := os.ReadFile(path)
		if err != nil {
			f.t.Fatal(err)
		}
		if !strings.Contains(string(b), old) {
			f.t.Fatalf("%s holds no %q", name, old)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	edit(modeltable.ProvidersFile, "[providers.test]", "[providers.chatgpt]\ndriver = \"chatgpt\"\n\n[providers.test]")
	edit(modeltable.ModelsFile, "[models.\"test/a\"]\nprovider = \"test\"", "[models.\"test/a\"]\nprovider = \"chatgpt\"")
	f.table = f.load()
	f.models["test/a"].provider = modeltable.ChatGPTProvider
}

// signInFixture lays down dir's ChatGPT sign-in as internal/chatgptauth
// leaves it, for the table's funding: a registration with plan usage, and a
// token file that is a placeholder, never a token.
func signInFixture(t *testing.T, dir string) {
	t.Helper()
	auth := filepath.Join(dir, modeltable.ChatGPTAuthDir)
	if err := os.MkdirAll(auth, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		modeltable.ChatGPTClientFile: `{"client_id":"app_client-0001","subject":"user-subject-0001","plan_usage":true}`,
		modeltable.ChatGPTTokenFile:  `{"placeholder":"no token here"}`,
	} {
		if err := os.WriteFile(filepath.Join(auth, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// sessionIDs is the session-id header of each call the model was sent, ""
// where a call had none.
func sessionIDs(calls []fantasy.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Headers["session-id"])
	}
	return out
}

// TestSessionIDGoesToTheChatGPTDriver (plan 033 §3.9, P18): every request a
// session on the ChatGPT plan's driver sends — each step of a turn, and both
// summarizer forms — carries the session's id as the session-id header,
// which the route derives its prompt-cache affinity from. A session on any
// other driver sends none (the control): Fantasy's OpenAI-compatible client
// would send it on, and nothing asked for it there.
func TestSessionIDGoesToTheChatGPTDriver(t *testing.T) {
	t.Run("a turn and the aligned summary", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		onChatGPT(f)
		s := f.open(f.options())
		f.models["test/a"].push(answerWith("hi"))
		run(t, s, "hello")
		f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nhello.")))
		if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		ids := sessionIDs(f.models["test/a"].requests())
		if len(ids) != 2 || ids[0] != s.ID() || ids[1] != s.ID() || s.ID() == "" {
			t.Fatalf("session-id headers = %q, want the session's id %q on both", ids, s.ID())
		}
	})
	t.Run("the text-form summary", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		onChatGPT(f)
		s := f.open(f.options())
		s.sleep = func(context.Context, time.Duration) {}
		f.models["test/a"].push(answerWith("hi"))
		run(t, s, "hello")
		m := s.cur
		m.r.ContextWindow, m.r.MaxOutputTokens = 4000, 100
		f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nFrom the text form.")))
		if _, err := s.compact(context.Background(), m, 2, store.CompactionOverflow, "", "", nil); err != nil {
			t.Fatalf("compact: %v", err)
		}
		calls := f.models["test/a"].requests()
		if last := calls[len(calls)-1]; len(last.Tools) != 0 || last.Headers["session-id"] != s.ID() {
			t.Fatalf("the text form's call: %d tools, session-id %q; want none and %q", len(last.Tools), last.Headers["session-id"], s.ID())
		}
	})
	t.Run("another driver sends none", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		onChatGPT(f)
		s := f.open(f.options())
		if err := s.SetModel("other/c"); err != nil {
			t.Fatal(err)
		}
		f.models["other/c"].push(answerWith("hi"))
		run(t, s, "hello")
		if ids := sessionIDs(f.models["other/c"].requests()); len(ids) != 1 || ids[0] != "" {
			t.Fatalf("an openai-compat call carried session-id %q", ids)
		}
	})
}

// TestRequestHeaders: the header goes to the ChatGPT driver alone, and a
// session with no id yet sends none.
func TestRequestHeaders(t *testing.T) {
	for _, tc := range []struct {
		driver, id string
		want       map[string]string
	}{
		{modeltable.DriverChatGPT, "0199c0de", map[string]string{"session-id": "0199c0de"}},
		{modeltable.DriverChatGPT, "", nil},
		{modeltable.DriverOpenAICompat, "0199c0de", nil},
		{modeltable.DriverOpenRouter, "0199c0de", nil},
	} {
		got := requestHeaders(modeltable.Resolved{Driver: tc.driver}, tc.id)
		if len(got) != len(tc.want) || got["session-id"] != tc.want["session-id"] {
			t.Errorf("%s %q: headers = %v, want %v", tc.driver, tc.id, got, tc.want)
		}
	}
}
