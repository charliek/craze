package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/routing"
)

func TestResolveOffline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRAZE_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	t.Setenv("CRAZE_AGENT_BIN", "/missing/ignore-me")
	for _, k := range []string{"CODEX_THREAD_ID", "CODEX_API_KEY", "CRAZE_SESSION_PROVIDER", "CLAUDECODE", "ANTIGRAVITY_AGENT", "CURSOR_AGENT"} {
		t.Setenv(k, "")
	}
	for _, name := range []string{"codex", "claude", "claude-agent-acp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	nativeDir := filepath.Join(dir, "native")
	if err := os.Mkdir(nativeDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{modeltable.ProvidersFile, modeltable.CredentialsFile} {
		path := filepath.Join(nativeDir, file)
		if err := os.WriteFile(path, []byte("version = 1\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
	}
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"resolve", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var res resolveResultJSON
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil || res.Kind != "overview" || len(res.Targets) != 5 {
		t.Fatalf("%s %v", buf.String(), err)
	}
	probe := offlineRouteProbe()
	if f := probe("codex", "x"); !f.Provisional || f.Reason != "" {
		t.Fatal(f)
	}
	t.Setenv("CODEX_API_KEY", "sk-test")
	if f := probe("codex", "x"); f.Provisional || f.Billing != routing.APIKey {
		t.Fatal(f)
	}
	t.Setenv("OPENAI_API_KEY", "sk-environment-key-of-sufficient-length")
	if f := offlineRouteProbe()("native", "openai/gpt-6.2-sol"); f.Reason != routing.HeldEnvKey {
		t.Fatal(f)
	}
	for _, file := range []string{modeltable.ProvidersFile, modeltable.CredentialsFile} {
		s, err := os.Stat(filepath.Join(nativeDir, file))
		if err != nil || s.Mode().Perm() != 0644 {
			t.Fatalf("resolver changed %s", file)
		}
	}
	for _, absent := range []string{".codex", "sessions.jsonl", "config.toml.lock"} {
		if _, err := os.Stat(filepath.Join(dir, absent)); !os.IsNotExist(err) {
			t.Fatal(absent, err)
		}
	}
}

func TestRouteHonorsClaudeOverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	defer agent.SetClaudeOverageForTest(path, nil, 0)()
	if err := os.WriteFile(path, []byte(`{"until":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("CRAZE_HOME", dir)
	t.Setenv("PATH", dir)
	t.Setenv("CLAUDE_CODE_EXECUTABLE", "")
	for _, name := range []string{"claude", "claude-agent-acp"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if f := offlineRouteProbe()("claude", "opus"); f.Reason != routing.OverageHold {
		t.Fatal(f)
	}
}

func TestResolveReportsSkipsAndRejectsEmptySelectors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRAZE_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	for _, k := range []string{"CRAZE_SESSION_PROVIDER", "CLAUDECODE", "ANTIGRAVITY_AGENT", "CURSOR_AGENT", "CODEX_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("CODEX_THREAD_ID", "1")
	for _, bin := range []string{"codex", "agy"} {
		if err := os.WriteFile(filepath.Join(dir, bin), nil, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, flags := range [][]string{{"--target", "sol@6.2", "--version="}, {"--version="}, {"--target=", "--role", "review"}} {
		_, _, code := executeErr(append([]string{"resolve"}, flags...))
		if code != 2 {
			t.Fatalf("%v exit %d", flags, code)
		}
	}
	out, stderr, code := executeErr([]string{"resolve", "--target", "sol", "--json"})
	var doc routing.Result
	if code != 2 || json.Unmarshal([]byte(out), &doc) != nil || doc.Schema != 1 || doc.Routes == nil || len(doc.Routes) != 0 || len(doc.Skipped) != 4 || doc.Skipped[0].Reason != routing.SelfDelegation || doc.Skipped[1].Reason != routing.NotSignedIn || doc.Skipped[2].Reason != routing.PaidBlocked || doc.Skipped[3].Reason != routing.PaidBlocked {
		t.Fatalf("exit %d %s %s", code, out, stderr)
	}
	out, stderr, code = executeErr([]string{"resolve", "--target", "neon", "--effort", "max", "--json"})
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Routes) == 0 || doc.Routes[0].Effort != "xhigh" || strings.Count(stderr, "adjusted") != 1 {
		t.Fatalf("exit %d %s %s", code, out, stderr)
	}
}

func TestNativeRouteChecksAccountListEvenWithUserModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	if err := os.MkdirAll(chatgptauth.AuthDir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON := func(path string, v any) {
		t.Helper()
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		write(path, b)
	}
	writeJSON(chatgptauth.ClientFile(dir), chatgptauth.Client{ClientID: "app_test", Subject: "test-user", PlanUsage: true, NoticeShown: true})
	write(chatgptauth.TokenFile(dir), []byte("test-token"))
	write(filepath.Join(dir, modeltable.ModelsFile), []byte(`version = 1
[models."chatgpt/custom"]
provider = "chatgpt"
wire_model = "gpt-6.2-sol"
`))
	list := chatgptauth.Models{Version: 1, Subject: "test-user", ClientID: "app_test", ClientVersion: modeltable.ChatGPTModelsClientVersion(), FetchedAt: time.Now().UTC(), Models: []chatgptauth.Model{{Slug: "gpt-other"}}}
	writeJSON(chatgptauth.ModelsFile(dir), list)
	if f := loadNativeProbe()("chatgpt/custom"); f.Reason != routing.ModelUnavailable {
		t.Fatalf("fresh list %+v", f)
	}
	list.FetchedAt = time.Now().Add(-48 * time.Hour)
	writeJSON(chatgptauth.ModelsFile(dir), list)
	if f := loadNativeProbe()("chatgpt/custom"); f.Reason != "" || !f.Provisional {
		t.Fatalf("stale list %+v", f)
	}
}

func TestCustomAntigravityRouteEffort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRAZE_HOME", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "agy"), nil, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := routing.Defaults()
	for _, id := range []string{"claude-opus-5-5", "claude-sonnet-5-5-high", "gemini-4-argon"} {
		cfg.Targets["custom"] = routing.Target{Maker: "test", Effort: "max", Cap: "1m", Routes: []routing.Route{{Provider: "antigravity", Model: id, Billing: routing.Subscription}}}
		r, ok, err := cfg.Resolve(routing.Request{Target: "custom"}, offlineRouteProbe())
		if err != nil || !ok || r.Routes[0].Effort != "high" || len(r.Warnings) != 1 {
			t.Fatalf("%s %+v %v", id, r, err)
		}
	}
}
