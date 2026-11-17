package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/rundir"
)

// claudeHostHome is serveHome with the fake adapter as [agents].claude, a
// `claude` that passes the preflight, and script the fake's.
func claudeHostHome(t *testing.T, script string) (rundir.Env, string, string) {
	t.Helper()
	_, ws := serveHome(t)
	fake := fakeAgentPath(t)
	writeCrazeConfig(t, "host_idle_exit = \"30s\"\n\n[agents]\nclaude = \""+fake+"\"\n")
	env := rundir.ProcessEnv()
	t.Setenv("CRAZE_FAKE_SCRIPT", script)
	claude := filepath.Join(t.TempDir(), "claude")
	stub := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *'auth status'*) echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\",\"subscriptionType\":\"max\"}' ;;\n" +
		"  *--version*) echo '2.1.326 (Claude Code)' ;;\nesac\n"
	if err := os.WriteFile(claude, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agent.ClaudeExecutableEnv, claude)
	return env, ws, fake
}

// TestClaudeHeldTurnNewWait (plan 052 AC19): craze new --wait on a task that
// starts a background sub-agent returns only after the sub-agent and its
// hand-back, because the adapter holds the prompt's reply — through a real
// host, against a fake that holds it as the adapter does.
func TestClaudeHeldTurnNewWait(t *testing.T) {
	env, ws, _ := claudeHostHome(t, "claude-held-bg-subagent")
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	stdout, stderr, code := executeErr([]string{"new", "--wait", "--json", "--provider", "claude", "-C", ws, "do the job in the background"})
	m := decodeResult(t, stdout)
	msg, _ := m["message"].(string)
	if code != 0 || m["status"] != "completed" || !strings.Contains(msg, "the child reported: background result") {
		t.Fatalf("craze new --wait on a held claude turn: exit %d, %v, %q", code, m, stderr)
	}
}

// TestClaudeHeldTurnSendWaitExitAfterTurn (plan 052 AC19): craze send --wait
// --exit-after-turn to an interactive claude host returns the whole held turn
// — the background sub-agent's hand-back in its message — and the host is gone
// by the time it exits.
func TestClaudeHeldTurnSendWaitExitAfterTurn(t *testing.T) {
	env, ws, fake := claudeHostHome(t, "claude-held-bg-subagent")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fake, "--workspace", ws, "--provider", "claude")
	e := r.waitServing(t, env, true)
	short := e.CrazeSessionID[len(e.CrazeSessionID)-8:]
	stdout, stderr, code := executeErr([]string{"send", short, "--wait", "--json", "--exit-after-turn", "go"})
	m := decodeResult(t, stdout)
	msg, _ := m["message"].(string)
	if code != 0 || m["status"] != "completed" || m["stopping"] != true || !strings.Contains(msg, "the child reported: background result") {
		t.Fatalf("craze send --wait --exit-after-turn on a held claude turn: exit %d, %v, %q", code, m, stderr)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("the host ended with %v", err)
	}
}

// TestClaudeBackgroundShellDoesNotHoldTheHost (plan 052 AC19, CLD-15a): a
// turn that starts a background shell is answered at once, so craze new
// --wait --exit-after-turn returns its result and the host goes with it,
// shell or no shell.
func TestClaudeBackgroundShellDoesNotHoldTheHost(t *testing.T) {
	env, ws, _ := claudeHostHome(t, "claude-unattended-bg")
	t.Setenv("CRAZE_FAKE_CLAUDE_WAKE_DELAY", "20s")
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	start := time.Now()
	stdout, stderr, code := executeErr([]string{"new", "--wait", "--json", "--exit-after-turn", "--provider", "claude", "-C", ws, "start a background command"})
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["message"] != "started" || m["stopping"] != true {
		t.Fatalf("craze new --wait --exit-after-turn with a background shell: exit %d, %v, %q", code, m, stderr)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("the command took %v: the shell held it", d)
	}
	waitFor(t, "the host to go", func() bool {
		hosts, err := rundir.Hosts(env)
		return err == nil && len(hosts) == 0
	})
}

// TestClaudeOverageStopsTheHostAndResumes (plan 052 § D3, review C5 item 8):
// a trip stops the host itself — not only the adapter — so the session is no
// one's; a reload is refused with quota while the trip record holds; and once
// it has expired an ordinary reload (craze send) starts a fresh adapter and
// the session answers again.
func TestClaudeOverageStopsTheHostAndResumes(t *testing.T) {
	env, ws, _ := claudeHostHome(t, "claude-overage")
	// The limit resets in a few seconds: the record expires on its own.
	t.Setenv("CRAZE_FAKE_CLAUDE_RESETS_IN", "4s")
	spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	stdout, stderr, code := executeErr([]string{"new", "--wait", "--json", "--provider", "claude", "-C", ws, "MID"})
	m := decodeResult(t, stdout)
	errObj, _ := m["error"].(map[string]any)
	if code != 3 || m["status"] != "failed" || errObj["kind"] != "quota" {
		t.Fatalf("the tripping turn: exit %d, %v, %q", code, m, stderr)
	}
	short, _ := m["short"].(string)
	waitFor(t, "the host to stop", func() bool {
		hosts, err := rundir.Hosts(env)
		return err == nil && len(hosts) == 0
	})
	record := filepath.Join(os.Getenv("CRAZE_HOME"), "claude-overage.json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the trip record: %v", err)
	}
	stdout, stderr, code = executeErr([]string{"send", short, "--wait", "--json", "hello"})
	if m := decodeResult(t, stdout); code == 0 || !strings.Contains(stdout+stderr, "extra usage") {
		t.Fatalf("a reload while the record holds: exit %d, %v, %q", code, m, stderr)
	}
	time.Sleep(4500 * time.Millisecond)
	waitFor(t, "a refused reload's host to go", func() bool {
		hosts, err := rundir.Hosts(env)
		return err == nil && len(hosts) == 0
	})
	stdout, stderr, code = executeErr([]string{"send", short, "--wait", "--json", "hello"})
	if m := decodeResult(t, stdout); code != 0 || m["status"] != "completed" || m["message"] != "echo: hello" {
		t.Fatalf("the reload after the record expired: exit %d, %v, %q", code, m, stderr)
	}
}

// TestClaudeRequeuedLastPromptKeepsItsWait (plan 052 C8 S1): craze send
// --wait --exit-after-turn whose prompt the engine took straight to a turn,
// and which a wake then refused at claude's prompt gate — the wake on the
// wire while the prompt waited there for a fast-mode reset — waits for the
// prompt's real turn, the row its text became: the result is that turn's,
// and the host stops after it, not before.
func TestClaudeRequeuedLastPromptKeepsItsWait(t *testing.T) {
	env, ws, fake := claudeHostHome(t, "claude-basic")
	gate := filepath.Join(t.TempDir(), "set")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SET_GATE", gate)
	t.Setenv("CRAZE_FAKE_CLAUDE_WAKE_BEFORE_SET", "a wake in the gate window")
	// The host is this process, so it is never gone: the wait for it is cut
	// short, as exit_after_turn_test.go's are.
	prev := exitAfterTurnWait
	exitAfterTurnWait = 300 * time.Millisecond
	t.Cleanup(func() { exitAfterTurnWait = prev })
	r := runServeIn(t, hostEnv{}, "--agent-bin", fake, "--workspace", ws, "--provider", "claude")
	e := r.waitServing(t, env, true)
	short := e.CrazeSessionID[len(e.CrazeSessionID)-8:]
	// Fast mode turned on: the reset's set is held at the gate.
	if stdout, stderr, code := executeErr([]string{"send", short, "--wait", "--json", "FAST-ON"}); code != 0 {
		t.Fatalf("the fast-on turn: exit %d, %s, %q", code, stdout, stderr)
	}
	last := goCraze("send", short, "--wait", "--json", "--exit-after-turn", "the last")
	// The engine has taken it to a turn: the session's prompt waits at the
	// gate for the reset, which answers behind a wake.
	waitFor(t, "the last prompt's turn", func() bool { return serveTurnRunning(t, e) })
	if err := releaseGate(gate, serveStep); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := last.within(t, serveStep)
	m := decodeResult(t, stdout)
	msg, _ := m["message"].(string)
	if code != 0 || m["status"] != "completed" || m["stopping"] != true || !strings.Contains(msg, "echo: the last") {
		t.Fatalf("the requeued last prompt: exit %d, %v, %q", code, m, stderr)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("the host ended with %v", err)
	}
}
