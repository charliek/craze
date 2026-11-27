package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
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
