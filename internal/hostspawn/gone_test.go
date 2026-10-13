package hostspawn

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// starts is each way a host is started: StartCmd, and StartReparented, whose
// intermediate is craze too.
var starts = []struct {
	name  string
	start func(*exec.Cmd) (*Child, *os.File, error)
}{
	{"StartCmd", func(cmd *exec.Cmd) (*Child, *os.File, error) { return StartCmd(cmd, HostChildEnv, "", "") }},
	{"StartReparented", func(cmd *exec.Cmd) (*Child, *os.File, error) {
		return StartReparented(context.Background(), cmd, HostChildEnv, "", "", ReparentSpawn{})
	}},
}

// TestAGoneExecutableIsTyped (plan 046 U1): a start whose executable is gone
// from its path — the versioned directory an upgrade removed — is an
// *ExecutableGoneError naming that path, in its words, still the start's own
// ENOENT underneath; nothing is started.
func TestAGoneExecutableIsTyped(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "cellar", "0.1.0", "bin", "craze")
	for _, tc := range starts {
		t.Run(tc.name, func(t *testing.T) {
			child, r, err := tc.start(exec.Command(gone, "serve"))
			var ge *ExecutableGoneError
			if child != nil || r != nil || !errors.As(err, &ge) || ge.Path != gone || !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a start of a gone executable = %v, %v, %v; want an *ExecutableGoneError for %s", child, r, err, gone)
			}
			if want := "craze at " + gone + " is gone: it was upgraded or removed after this process started"; err.Error() != want {
				t.Fatalf("its words: %q, want %q", err.Error(), want)
			}
		})
	}
}

// TestAVanishedDirectoryIsNoGoneExecutable is U1's negative control: a start
// whose working directory is gone fails with the very same ENOENT — Setsid
// makes os/exec skip its own check of cmd.Dir, so the child's chdir fails it —
// and is not taken for a gone executable, whose path is still there. Nor is
// a relative path, which cannot be told from a vanished directory.
func TestAVanishedDirectoryIsNoGoneExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range starts {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(exe, "-test.run=^$")
			cmd.Dir = filepath.Join(t.TempDir(), "gone")
			child, r, err := tc.start(cmd)
			var ge *ExecutableGoneError
			if child != nil || r != nil || !errors.Is(err, fs.ErrNotExist) || errors.As(err, &ge) {
				t.Fatalf("a start in a gone directory = %v, %v, %v; want ENOENT, and no *ExecutableGoneError", child, r, err)
			}
		})
		t.Run(tc.name+" relative", func(t *testing.T) {
			child, r, err := tc.start(exec.Command("./no-such-craze", "serve"))
			var ge *ExecutableGoneError
			if child != nil || r != nil || err == nil || errors.As(err, &ge) {
				t.Fatalf("a start of a relative path = %v, %v, %v; want an error, and no *ExecutableGoneError", child, r, err)
			}
		})
	}
}
