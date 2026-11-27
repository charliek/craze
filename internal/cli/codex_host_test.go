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
		if !slices.Equal(files, []string{"added.txt", "updated.txt", "moved.txt"}) {
			t.Fatalf("JSON files = %v, want exact [added.txt updated.txt moved.txt] (full result: %v)", files, m)
		}
		waitFor(t, "codex edits host to stop", func() bool {
			hosts, err := rundir.Hosts(env)
			return err == nil && len(hosts) == 0
		})
	})

	t.Run("CODEX_THREAD_ID self-delegation refusal and --allow-same-provider override", func(t *testing.T) {
		env, ws, _ := codexHostHome(t, "codex-basic")
		spawnAsChild(t, nil)
		spawnBounds(t, serveStep, serveStep, serveStep)
		t.Cleanup(func() { stopEveryHost(t, env) })
		t.Setenv(codexThreadEnv, "thr-parent-123")

		_, stderr, code := executeErr([]string{
			"new", "--wait", "--json", "--exit-after-turn",
			"--provider", "codex", "-C", ws, "should be refused",
		})
		if code != 2 || !strings.Contains(stderr, "already runs on codex") || !strings.Contains(stderr, "--allow-same-provider") {
			t.Fatalf("expected exit 2 self-delegation refusal, got code=%d stderr=%q", code, stderr)
		}

		stdout, stderr, code := executeErr([]string{
			"new", "--wait", "--json", "--exit-after-turn", "--allow-same-provider",
			"--provider", "codex", "-C", ws, "allowed override",
		})
		m := decodeResult(t, stdout)
		msg, _ := m["message"].(string)
		if code != 0 || m["status"] != "completed" || !strings.Contains(msg, "allowed override") {
			t.Fatalf("with --allow-same-provider: code=%d result=%v stderr=%q", code, m, stderr)
		}
		waitFor(t, "codex host after override to stop", func() bool {
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

// TestCodexUnattendedNeedsInputAndAutoAnswer (Plan 054 § 5 M2b, L11):
//   - agent --no-force approval without --exit-after-turn returns exit 4
//     (needs_input) and leaves the host alive with the pending ask answerable.
//   - agent --no-force approval WITH --exit-after-turn returns exit 4
//     (needs_input) and stops the host before exit.
//   - unattended requestUserInput with free-text-only (options: null) returns
//     exit 4 (needs_input) instead of fabricating bogus text.
//   - unattended requestUserInput with options auto-answers the first option
//     and returns exit 0 (completed).
func TestCodexUnattendedNeedsInputAndAutoAnswer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		script        string
		extraFlags    []string
		wantCode      int
		wantStatus    string
		wantMsgSubstr string
		wantHostGone  bool
		wantAskOpen   bool
	}{
		{
			name:        "approval --no-force exits 4 needs_input and keeps host alive",
			script:      "codex-approval",
			extraFlags:  []string{"--no-force"},
			wantCode:    4,
			wantStatus:  "needs_input",
			wantAskOpen: true,
		},
		{
			name:         "approval --no-force --exit-after-turn exits 4 needs_input and stops host",
			script:       "codex-approval",
			extraFlags:   []string{"--no-force", "--exit-after-turn"},
			wantCode:     4,
			wantStatus:   "needs_input",
			wantHostGone: true,
		},
		{
			name:        "freetext requestUserInput exits 4 needs_input",
			script:      "codex-ask-freetext",
			extraFlags:  []string{},
			wantCode:    4,
			wantStatus:  "needs_input",
			wantAskOpen: true,
		},
		{
			name:          "options requestUserInput auto-answers first option",
			script:        "codex-ask",
			extraFlags:    []string{"--exit-after-turn"},
			wantCode:      0,
			wantStatus:    "completed",
			wantMsgSubstr: "Red",
			wantHostGone:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, _ := codexHostHome(t, tc.script)
			spawnAsChild(t, nil)
			spawnBounds(t, serveStep, serveStep, serveStep)
			t.Cleanup(func() { stopEveryHost(t, env) })

			args := append([]string{"new", "--wait", "--json", "--provider", "codex", "-C", ws}, tc.extraFlags...)
			args = append(args, "trigger ask")
			stdout, stderr, code := executeErr(args)
			m := decodeResult(t, stdout)
			if code != tc.wantCode || m["status"] != tc.wantStatus {
				t.Fatalf("craze new --wait (%s): exit=%d status=%v want=(%d, %s); result=%v stderr=%q",
					tc.name, code, m["status"], tc.wantCode, tc.wantStatus, m, stderr)
			}
			if tc.wantMsgSubstr != "" {
				msg, _ := m["message"].(string)
				if !strings.Contains(msg, tc.wantMsgSubstr) {
					t.Fatalf("message = %q, want substring %q", msg, tc.wantMsgSubstr)
				}
			}
			if tc.wantHostGone {
				waitFor(t, "codex host to stop after "+tc.name, func() bool {
					hosts, err := rundir.Hosts(env)
					return err == nil && len(hosts) == 0
				})
			}
			if tc.wantAskOpen {
				id, _ := m["session"].(string)
				e, ok := hostServing(env, id)
				if !ok {
					t.Fatalf("expected host for %s to stay alive with pending ask", id)
				}
				if row := listRow(t, e); row.PendingAsks < 1 {
					t.Fatalf("expected PendingAsks >= 1 on live host, got %+v", row)
				}
			}
		})
	}
}
