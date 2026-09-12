package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/tui"
)

func TestFrameUnknownTokenExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"frame", "--keys", "<nope>"})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(ee.msg, "unknown token") {
		t.Fatalf("msg %q", ee.msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestFrameTimeoutExits3WithDiagnostics(t *testing.T) {
	var stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	err := frameExitError(cmd, &tui.WaitTimeoutError{
		Wait:      "<wait:idle>",
		Timeout:   2 * time.Second,
		LastFrame: "craze  working",
	})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(stderr.String(), "<wait:idle>") || !strings.Contains(stderr.String(), "craze  working") {
		t.Fatalf("diagnostics %q", stderr.String())
	}
}

// The child must inherit the runner's isolated HOME, not the developer's, or
// goldens would depend on ~/.craze/config.toml.
func TestFrameIsolatesHomeForTheChild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAZE_FAKE_SCRIPT", "")
	outer := os.Getenv("HOME")

	dir := t.TempDir()
	record := filepath.Join(dir, "child-env")
	bin := filepath.Join(dir, "home-probe")
	probe := "#!/bin/sh\nprintf '%s\\n%s' \"$HOME\" \"$CRAZE_FAKE_SCRIPT\" > " + record + "\nexit 1\n"
	if err := os.WriteFile(bin, []byte(probe), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"frame", "--cols", "80", "--rows", "24", "--agent-bin", bin,
		"--fake-script", "todos", "--keys", "", "--timeout", "5s"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("frame: %v", err)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("child did not run: %v", err)
	}
	home, script, _ := strings.Cut(string(got), "\n")
	if home == "" || home == outer {
		t.Fatalf("child HOME %q, caller HOME %q", home, outer)
	}
	if script != "todos" {
		t.Fatalf("child CRAZE_FAKE_SCRIPT %q, want todos", script)
	}
	if os.Getenv("HOME") != outer {
		t.Fatalf("HOME was not restored: %q", os.Getenv("HOME"))
	}
}

func TestFrameRejectsZeroSize(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"frame", "--cols", "0"})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("got %v", err)
	}
}
