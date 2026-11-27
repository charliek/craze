package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// codexHostHome sets up an isolated CRAZE_HOME + workspace with a fake `codex`
// binary on PATH (pointing at craze-fake-agent) running the named script.
func codexHostHome(t *testing.T, script string) (rundir.Env, string, string) {
	t.Helper()
	_, ws := serveHome(t)
	fake := fakeAgentPath(t)
	binDir := t.TempDir()
	codexLink := filepath.Join(binDir, "codex")
	if err := os.Symlink(fake, codexLink); err != nil {
		t.Fatalf("symlink fake codex: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeCrazeConfig(t, "host_idle_exit = \"30s\"\nhost_idle_exit_unattended = \"30s\"\n")
	env := rundir.ProcessEnv()
	t.Setenv("CRAZE_FAKE_SCRIPT", script)
	return env, ws, fake
}

// TestCodexDetachedNewWaitExitAfterTurn (Plan 054 § 5 M2a): a detached host
// spawned via `craze new --wait --exit-after-turn --json --provider codex`
// drives the fake codex app-server through `codexSession`, streams the turn
// result, stamps `files` on apply_patch edits, and stops cleanly after the turn.
func TestCodexDetachedNewWaitExitAfterTurn(t *testing.T) {
	t.Run("basic turn and exit-after-turn", func(t *testing.T) {
		env, ws, _ := codexHostHome(t, "codex-basic")
		spawnAsChild(t, nil)
		spawnBounds(t, serveStep, serveStep, serveStep)
		t.Cleanup(func() { stopEveryHost(t, env) })

		stdout, stderr, code := executeErr([]string{
			"new", "--wait", "--json", "--exit-after-turn",
			"--provider", "codex", "-C", ws, "hello from detached host",
		})
		m := decodeResult(t, stdout)
		msg, _ := m["message"].(string)
		if code != 0 || m["status"] != "completed" || m["stopping"] != true || !strings.Contains(msg, "hello from detached host") {
			t.Fatalf("craze new --wait --exit-after-turn codex: exit %d, %v, %q", code, m, stderr)
		}
		waitFor(t, "codex host to stop after exit-after-turn", func() bool {
			hosts, err := rundir.Hosts(env)
			return err == nil && len(hosts) == 0
		})
	})

	t.Run("edits turn stamps files in JSON result", func(t *testing.T) {
		env, ws, _ := codexHostHome(t, "codex-edits")
		spawnAsChild(t, nil)
		spawnBounds(t, serveStep, serveStep, serveStep)
		t.Cleanup(func() { stopEveryHost(t, env) })

		stdout, stderr, code := executeErr([]string{
			"new", "--wait", "--json", "--exit-after-turn",
			"--provider", "codex", "-C", ws, "apply edits",
		})
		m := decodeResult(t, stdout)
		if code != 0 || m["status"] != "completed" || m["stopping"] != true {
			t.Fatalf("craze new --wait codex-edits: exit %d, %v, %q", code, m, stderr)
		}
		rawFiles, _ := m["files"].([]any)
		var files []string
		for _, f := range rawFiles {
			if s, ok := f.(string); ok {
				files = append(files, s)
			}
		}
		if !slices.Contains(files, "added.txt") || !slices.Contains(files, "updated.txt") || !slices.Contains(files, "moved.txt") {
			t.Fatalf("JSON files = %v, want added.txt, updated.txt, moved.txt (full result: %v)", files, m)
		}
		waitFor(t, "codex edits host to stop", func() bool {
			hosts, err := rundir.Hosts(env)
			return err == nil && len(hosts) == 0
		})
	})
}

// TestCodexServeAndSendWait exercises an interactive detached host started via
// `craze serve --provider codex` receiving a follow-up `craze send --wait`.
func TestCodexServeAndSendWait(t *testing.T) {
	env, ws, fake := codexHostHome(t, "codex-basic")
	prev := exitAfterTurnWait
	exitAfterTurnWait = 300 * time.Millisecond
	t.Cleanup(func() { exitAfterTurnWait = prev })
	r := runServeIn(t, hostEnv{}, "--agent-bin", fake, "--workspace", ws, "--provider", "codex")
	e := r.waitServing(t, env, true)
	short := e.CrazeSessionID[len(e.CrazeSessionID)-8:]

	stdout, stderr, code := executeErr([]string{"send", short, "--wait", "--json", "--exit-after-turn", "ping codex"})
	m := decodeResult(t, stdout)
	msg, _ := m["message"].(string)
	if code != 0 || m["status"] != "completed" || m["stopping"] != true || !strings.Contains(msg, "ping codex") {
		t.Fatalf("craze send --wait codex: exit %d, %v, %q", code, m, stderr)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("host ended with %v", err)
	}
}
