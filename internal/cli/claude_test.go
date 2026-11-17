package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// TestClaudeAvailability (plan 052 § A2): claude is ready only when both the
// adapter and Claude Code's `claude` resolve, and a missing one is named with
// its own install command. CLAUDE_CODE_EXECUTABLE points at the companion;
// --agent-bin, CRAZE_AGENT_BIN and [agents].claude are the adapter's.
func TestClaudeAvailability(t *testing.T) {
	claudeDir := t.TempDir()
	claude := filepath.Join(claudeDir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := []agent.Provider{agent.ClaudeProvider()}
	for _, tc := range []struct {
		name    string
		resolve map[string]binResolution
		path    string
		env     map[string]string
		want    availEntry
	}{
		{"both there", nil, claudeDir, nil, availEntry{"claude", "ready", "", ""}},
		{"the adapter missing", map[string]binResolution{"claude": {Source: binFromPath}}, claudeDir, nil,
			availEntry{"claude", "unavailable", "claude-agent-acp not found on PATH",
				"install the adapter: npm i -g @agentclientprotocol/claude-agent-acp (Node ≥22), or set [agents].claude in " + availConfig}},
		{"claude missing", nil, t.TempDir(), nil,
			availEntry{"claude", "unavailable", "claude not found on PATH", "install Claude Code (claude), or set CLAUDE_CODE_EXECUTABLE"}},
		{"claude off PATH, pointed at", nil, t.TempDir(), map[string]string{agent.ClaudeExecutableEnv: claude},
			availEntry{"claude", "ready", "", ""}},
		{"pointed at nothing", nil, claudeDir, map[string]string{agent.ClaudeExecutableEnv: "/no/claude"},
			availEntry{"claude", "unavailable", "CLAUDE_CODE_EXECUTABLE /no/claude not found", "point CLAUDE_CODE_EXECUTABLE at Claude Code's claude, or unset it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			in := availInputs{
				resolve:    resolvesAs(tc.resolve),
				native:     nativeAs(availReady, "", ""),
				gui:        session(true, true),
				configPath: availConfig,
				getenv:     func(k string) string { return tc.env[k] },
			}
			got := availEntries(availability(in, rows))
			if !reflect.DeepEqual(got, []availEntry{tc.want}) {
				t.Fatalf("availability:\n%+v\nwant:\n%+v", got, tc.want)
			}
		})
	}
}

// TestFastRefusedOnClaude (plan 052 § D3, AC15): --fast with claude is a
// usage error, exit 2, on every command that takes it, before anything is
// spawned or dialled; --no-fast passes the check.
func TestFastRefusedOnClaude(t *testing.T) {
	isolateRunEnv(t)
	for _, args := range [][]string{
		{"prompt", "--provider", "claude", "--fast", "hi"},
		{"new", "--provider", "claude", "--fast", "hi"},
		{"new", "--wait", "--provider", "claude", "--fast", "hi"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(err.Error(), agent.FastBillsExtra) {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// The TUI's own refusal, which every provider it may start is asked.
	var ee *exitError
	if err := (&tuiFlags{fast: true}).refuse(agent.ClaudeProvider()); !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("the TUI: %v", err)
	}
	if err := refuseFast("craze", agent.ClaudeProvider(), fastFlags(false, true)); err != nil {
		t.Fatalf("--no-fast: %v", err)
	}
	if err := refuseFast("craze", agent.CursorProvider(), fastFlags(true, false)); err != nil {
		t.Fatalf("cursor --fast: %v", err)
	}
}

// TestPromptAgentGetsTheCallerMarker (plan 052 § A3, § J1): craze prompt's
// agent — every spawn goes through the session's start — is given
// CRAZE_SESSION_PROVIDER, replacing one craze itself inherited.
func TestPromptAgentGetsTheCallerMarker(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "env")
	t.Setenv("CRAZE_FAKE_DUMP_ENV", dump)
	t.Setenv(agent.SessionProviderEnv, "grok")
	runPromptStdout(t, "echo", []string{"--provider", "cursor"}, "hi")
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, agent.SessionProviderEnv) {
			n++
			if l != agent.SessionProviderEnv+"=cursor" {
				t.Fatalf("marker %q", l)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d markers in %q", n, lines)
	}
}

// TestDetachedHostAgentGetsTheCallerMarker: the agent of a detached host —
// the TUI's spawn, and the hub's, start one the same way — is given the
// marker too, its own provider's, over whatever the spawner's environment
// carried.
func TestDetachedHostAgentGetsTheCallerMarker(t *testing.T) {
	env, ws := serveHome(t)
	dump := filepath.Join(t.TempDir(), "env")
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv("CRAZE_FAKE_DUMP_ENV", dump)
	t.Setenv(agent.SessionProviderEnv, "grok")
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	ref, err := spawnNow(t, spawnFor(t, env, ws))
	if err != nil {
		t.Fatalf("spawnHost: %v", err)
	}
	t.Cleanup(func() { endHost(ref) })
	var raw []byte
	deadline := time.Now().Add(serveStep)
	for time.Now().Before(deadline) {
		if raw, err = os.ReadFile(dump); err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(raw), agent.SessionProviderEnv+"=cursor\n") || strings.Contains(string(raw), agent.SessionProviderEnv+"=grok") {
		t.Fatalf("the host's agent environment carries the marker as %q", markerLines(string(raw)))
	}
}

func markerLines(dump string) []string {
	var out []string
	for _, l := range strings.Split(dump, "\n") {
		if strings.HasPrefix(l, agent.SessionProviderEnv) {
			out = append(out, l)
		}
	}
	return out
}

// TestNewFastRefusedOnAClaudeDefault (review F3): craze new --fast with no
// --provider is refused, exit 2, when the provider the create would start is
// claude — the hub's configured default — as an explicit --provider claude is.
func TestNewFastRefusedOnAClaudeDefault(t *testing.T) {
	isolateRunEnv(t)
	writeFileT(t, filepath.Join(os.Getenv("CRAZE_HOME"), "config.toml"), "provider = \"claude\"\n")
	for _, args := range [][]string{{"new", "--fast", "hi"}, {"new", "--json", "--fast", "hi"}} {
		var stdout, stderr bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(err.Error(), agent.FastBillsExtra) {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
