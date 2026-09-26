package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/charliek/craze/internal/sessions"
)

// runTUI's own wiring of the control socket and the session claims (plan 027
// C13), end to end in process.

// TestARunWhoseSocketFailsStillRuns: a runtime directory craze refuses to
// bind in is a warning on craze's own lane, printed after the alt screen, and
// the TUI runs exactly as it would with the socket off — its new session
// claimed all the same, and no registry entry anywhere.
func TestARunWhoseSocketFailsStillRuns(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Skipf("pty resize: %v", err)
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv("CRAZE_JOURNAL", "0")
	crazeHome(t)
	runtimeDir := shortRuntimeDir(t)
	if err := os.Chmod(runtimeDir, 0o770); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_RUNTIME_DIR", runtimeDir)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")

	tail := newPTYTail(ptmx)
	prevIn, prevOut, prevErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = tty, tty, tty
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = prevIn, prevOut, prevErr })

	done := make(chan error, 1)
	ws := t.TempDir()
	go func() {
		done <- runTUI(nil, &tuiFlags{agentBin: fakeAgentPath(t), workspace: ws, force: true}, hostEnv{})
	}()
	if !tail.wait(" craze ─", 10*time.Second) {
		t.Fatalf("craze never drew a frame; got %q", tail.text())
	}
	locks, _ := filepath.Glob(filepath.Join(home, ".cache", "craze", "locks", "*.lock"))
	if len(locks) != 1 {
		t.Fatalf("session locks while running: %q, want the new session's", locks)
	}
	if !lockHeldBy(t, locks[0], os.Getpid()) {
		t.Fatalf("the session lock %s is not held by this process", locks[0])
	}
	if entries, _ := filepath.Glob(filepath.Join(home, ".cache", "craze", "hosts", "*")); len(entries) != 0 {
		t.Fatalf("a refused bind left %q", entries)
	}
	if strings.Contains(tail.text(), "control socket off") {
		t.Fatal("the warning reached the terminal while the TUI owned it")
	}
	if _, err := ptmx.Write([]byte{0x04}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runTUI = %v, want a clean exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("craze did not quit after ctrl+d")
	}
	if !tail.wait("craze: control socket off: ", 5*time.Second) {
		t.Fatalf("no warning after exit; got %q", tail.text())
	}
	if lockHeldBy(t, locks[0], os.Getpid()) {
		t.Fatal("the session lock outlived the run")
	}
}

// lockHeldBy reports whether path is flocked by someone and names pid.
func lockHeldBy(t *testing.T, path string, pid int) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.HasPrefix(string(b), strconv.Itoa(pid)+" ")
}

// TestARefusedContinueBindsNothing: a --continue another craze holds is
// refused before the bind — not even the socket's namespace directory is
// created, and no registry entry was ever written.
func TestARefusedContinueBindsNothing(t *testing.T) {
	ws := indexHome(t)
	runtimeDir := shortRuntimeDir(t)
	t.Setenv("CRAZE_RUNTIME_DIR", runtimeDir)
	seedRow(t, sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, CrazeID: "018f-held", TitleKind: sessions.TitleKindNone}, time.Minute)
	if _, err := anotherCraze(t).claimSession("018f-held"); err != nil {
		t.Fatal(err)
	}
	cmd, f := parseTUIFlags(t, "--continue", "--workspace", ws)
	code, msg := exitCode(t, runTUI(cmd, f, hostEnv{}))
	if code != 1 || !strings.Contains(msg, "that session is open in another craze (pid ") {
		t.Fatalf("exit %d %q, want the refusal", code, msg)
	}
	if left, _ := os.ReadDir(runtimeDir); len(left) != 0 {
		t.Fatalf("a refused --continue bound a socket: %s holds %v", runtimeDir, left)
	}
	if entries, _ := filepath.Glob(filepath.Join(ws, ".cache", "craze", "hosts", "*")); len(entries) != 0 {
		t.Fatalf("a refused --continue registered a host: %q", entries)
	}
}
