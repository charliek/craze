package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frameHome is a frame case's environment: HOME is its own, and CRAZE_CONFIG
// is cleared so the session index is the config file's sibling under the
// runner's *isolated* HOME rather than a path that would escape it.
func frameHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_CONFIG", "")
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", "")
	return home
}

func runFrame(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("craze %v: %v\nstderr:\n%s\nstdout:\n%s", args, err, stderr.String(), stdout.String())
	}
	return stdout.String()
}

// TestFrameContinueLoadsASeededRow is --continue end to end: a row seeded into
// the index the frame runner isolates, resolved inside that isolation, loaded
// over a real session/load, and the replayed transcript on the frame with the
// `restored` note that closes it. The script waits on the note and never on
// <start>, which fires mid-replay (§3.5).
func TestFrameContinueLoadsASeededRow(t *testing.T) {
	frameHome(t)
	got := runFrame(t, "frame", "--cols", "100", "--rows", "30",
		"--agent-bin", fakeAgentPath(t), "--fake-script", "load",
		"--seed-session", "cursor:sess-load-1:fix: the flaky pty test",
		"--continue", "--keys", "<wait:text:restored><wait:idle>", "--timeout", "20s")
	for _, want := range []string{
		"List the files in the current working directory in one line.",
		"the workspace holds main.py and README.md",
		"restored",
		// The stored title is on the composer rule: it was seeded into the
		// session before Start, because no agent replays one (§3.4).
		"fix: the flaky pty test ─",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the restored frame is missing %q:\n%s", want, got)
		}
	}
	// The load script refuses session/new, so a frame that got this far proves
	// craze never fell back to one.
	if strings.Contains(got, "session/new must not be called") {
		t.Fatalf("craze fell back to session/new:\n%s", got)
	}
}

// TestFrameSeedingStaysInsideTheIsolatedHome is the §3.7 regression: the
// seeded rows have to be visible to the model — the case above only passes if
// they are — and invisible to the developer's own home directory. The runner
// enters its isolated HOME only after the caller has built the Config, so
// seeding from the caller would write the real ~/.craze/sessions.jsonl.
func TestFrameSeedingStaysInsideTheIsolatedHome(t *testing.T) {
	outer := frameHome(t)
	runFrame(t, "frame", "--cols", "100", "--rows", "30",
		"--agent-bin", fakeAgentPath(t), "--fake-script", "load",
		"--seed-session", "cursor:sess-load-1:seeded session",
		"--continue", "--keys", "<wait:text:restored>", "--timeout", "20s")

	index := filepath.Join(outer, ".craze", "sessions.jsonl")
	if _, err := os.Stat(index); !errors.Is(err, os.ErrNotExist) {
		body, _ := os.ReadFile(index)
		t.Fatalf("the frame wrote the outer HOME's index (%v):\n%s", err, body)
	}
	if entries, err := os.ReadDir(outer); err == nil && len(entries) != 0 {
		t.Fatalf("the frame left %d entries under the outer HOME", len(entries))
	}
	if os.Getenv("HOME") != outer {
		t.Fatalf("HOME was not restored: %q", os.Getenv("HOME"))
	}
}

// TestFrameResumePicksARowAndLoadsIt drives the picker itself: two seeded
// rows, newest first, and Enter on the second one loads that session. The
// runner's <start> is satisfied by the picker being up, because nothing is
// starting until a row is chosen.
func TestFrameResumePicksARowAndLoadsIt(t *testing.T) {
	frameHome(t)
	got := runFrame(t, "frame", "--cols", "100", "--rows", "30",
		"--agent-bin", fakeAgentPath(t), "--fake-script", "load",
		"--seed-session", "cursor:sess-old:an older session",
		"--seed-session", "cursor:sess-load-1:the newest session",
		"--resume", "--keys", "<down><enter><wait:text:restored><wait:idle>", "--timeout", "20s")
	for _, want := range []string{"the workspace holds main.py and README.md", "restored", "an older session ─"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the picked session is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "enter loads") {
		t.Fatalf("the picker is still up:\n%s", got)
	}
}

// TestFrameResumeWithNoSeededRowRefuses: the picker is never empty, in the
// frame runner as on the command line.
func TestFrameResumeWithNoSeededRowRefuses(t *testing.T) {
	frameHome(t)
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"frame", "--cols", "100", "--rows", "30", "--resume", "--keys", ""})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(ee.msg, "no session to continue in") {
		t.Fatalf("msg %q", ee.msg)
	}
}
