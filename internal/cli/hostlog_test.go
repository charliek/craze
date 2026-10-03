package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// readLog is a log file's content, "" when it does not exist.
func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// TestHostLogIsOpenedFresh (astra r3-c2 3): a log reused from a host gone a
// week and a day is opened with its time set to now — before its first line,
// which would set it too — so a sweep by another host starting in between,
// which sees no lock for the name's host id, still keeps it; and the host's
// lines land in that same file, after what it held.
func TestHostLogIsOpenedFresh(t *testing.T) {
	env, _ := serveHome(t)
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, rundir.NewHostID()+".log")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	l, err := openHostLog(env, path, hostLogMax)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) > time.Hour {
		t.Fatalf("the reused log was opened with its old time: %v, %v", fi.ModTime(), err)
	}
	if _, err := rundir.SweepHostLogs(env, time.Now(), hostLogKeep, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, path); got != "old\nnew\n" {
		t.Fatalf("the log holds %q: a sweep took it from under its host", got)
	}
}

// TestHostLogMakesItsDirectory: a missing log directory is made 0700, and a
// write past the cap rotates through the wrapper as through caplog.
func TestHostLogMakesItsDirectory(t *testing.T) {
	env, _ := serveHome(t)
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "h.log")
	l, err := openHostLog(env, path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if di, err := os.Stat(dir); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("a missing log directory is made 0700: %v, %v", di.Mode(), err)
	}
	for _, s := range []string{"aaaa\n", "bbbb\n", "cccc\n"} {
		if _, err := l.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if readLog(t, path) != "cccc\n" || readLog(t, path+".1") != "aaaa\nbbbb\n" {
		t.Fatalf("the wrapper did not rotate at the cap: %q, %q", readLog(t, path), readLog(t, path+".1"))
	}
}

// hostLogCrashEnv names the re-exec that TestHostLogCrashOutputFollowsRotations
// runs: its value is the directory the child works in.
const hostLogCrashEnv = "CRAZE_HOSTLOG_CRASH_CHILD"

// hostLogCrashMarker is the text the child panics with.
const hostLogCrashMarker = "hostlog-crash-marker-5c1d"

// TestHostLogCrashOutputFollowsRotations (plan 034 §3.3): the host log keeps
// the runtime's crash output pointed at its current file, through the opener's
// hook, across a rotation — a panic after a rotation is in the new log, which
// is what a detached host's /dev/null stderr depends on — and Close gives it
// back to stderr, so a panic after Close is in no log.
func TestHostLogCrashOutputFollowsRotations(t *testing.T) {
	for _, closed := range []bool{false, true} {
		dir := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHostLogCrashChild$")
		cmd.Env = append(os.Environ(), hostLogCrashEnv+"="+dir)
		if closed {
			cmd.Env = append(cmd.Env, "CRAZE_HOSTLOG_CRASH_CLOSE=1")
		}
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("closed=%v: the child was meant to die of its panic: %v\n%s", closed, err, out)
		}
		if !strings.Contains(string(out), hostLogCrashMarker) {
			t.Fatalf("closed=%v: the child did not panic with the marker:\n%s", closed, out)
		}
		cur, rotated := readLog(t, filepath.Join(dir, "h.log")), readLog(t, filepath.Join(dir, "h.log.1"))
		if rotated == "" {
			t.Fatalf("closed=%v: the child never rotated", closed)
		}
		if got := strings.Contains(cur, hostLogCrashMarker); got == closed {
			t.Fatalf("closed=%v: the panic is in the current log: %v (%q)", closed, got, cur)
		}
		if strings.Contains(rotated, hostLogCrashMarker) {
			t.Fatalf("closed=%v: the panic went to the rotated-away log", closed)
		}
	}
}

// TestHostLogCrashChild is the child of
// TestHostLogCrashOutputFollowsRotations; run directly it does nothing.
func TestHostLogCrashChild(t *testing.T) {
	dir := os.Getenv(hostLogCrashEnv)
	if dir == "" {
		t.Skip("only runs as TestHostLogCrashOutputFollowsRotations's child")
	}
	env, _ := serveHome(t)
	l, err := openHostLog(env, filepath.Join(dir, "h.log"), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaa\n", "bbbb\n", "cccc\n"} {
		if _, err := l.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("CRAZE_HOSTLOG_CRASH_CLOSE") != "" {
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
	panic(hostLogCrashMarker)
}
