package caplog

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
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

// openLog opens path as a Log with no hooks, appending as a process starting
// on an existing log does.
func openLog(path string, max int64) (*Log, error) {
	f, err := OpenFile(path, os.O_APPEND)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return New(path, f, st.Size(), max, Options{}), nil
}

// TestLogRotatesOnceAtTheCap is plan 030 §3.3's cap: a write that would take the
// log past it first moves the log to <log>.1 — replacing the rotation before —
// and starts a new one, 0600; a write larger than the cap on its own lands
// whole in a fresh file; a log already there is appended to, its size counted.
func TestLogRotatesOnceAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.log")
	l, err := openLog(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	write := func(s string) {
		t.Helper()
		if n, err := l.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write %q: %d, %v", s, n, err)
		}
	}
	want := func(cur, rotated string) {
		t.Helper()
		if got := readLog(t, path); got != cur {
			t.Fatalf("the log holds %q, want %q", got, cur)
		}
		if got := readLog(t, path+".1"); got != rotated {
			t.Fatalf("the rotation holds %q, want %q", got, rotated)
		}
	}
	write("aaaa\n")
	write("bbbb\n")
	want("aaaa\nbbbb\n", "")
	write("cccc\n")
	want("cccc\n", "aaaa\nbbbb\n")
	write("dddddddd\n")
	want("dddddddd\n", "cccc\n")
	write(strings.Repeat("e", 25))
	want(strings.Repeat("e", 25), "dddddddd\n")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("a rotated log is %v, %v; want 0600", fi.Mode(), err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("x")); err == nil {
		t.Fatal("a closed log took a write")
	}

	again, err := openLog(path, 29)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if _, err := again.Write([]byte("ffff\n")); err != nil {
		t.Fatal(err)
	}
	// 25 bytes were there already, counted: five more pass a cap of 29.
	want("ffff\n", strings.Repeat("e", 25))
}

// TestLogRefusesWhatIsNotAFile: a symlink at the log's name is refused,
// never written through, and so is a FIFO, which would hold the host on its
// first line.
func TestLogRefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if l, err := openLog(link, 1<<20); err == nil {
		_ = l.Close()
		t.Fatal("a symlinked log was opened")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the link's target was written: %q", b)
	}
	fifo := filepath.Join(dir, "fifo.log")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if l, err := openLog(fifo, 1<<20); err == nil {
		_ = l.Close()
		t.Fatal("a FIFO was opened as a log")
	}
}

// TestLogFinishesARotationWhoseOpenFailed (astra r3-c2 5): a rotation
// whose rename succeeded and whose new log could not be opened goes on
// writing to the renamed file, and finishes — the open retried before each
// write, never the rename, whose source is gone — as soon as the open works
// again: the new log takes the next write, the rotation keeps what was written
// meanwhile, and rotating at the cap resumes as before.
func TestLogFinishesARotationWhoseOpenFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.log")
	l, err := openLog(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	write := func(s string) {
		t.Helper()
		if n, err := l.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write %q: %d, %v", s, n, err)
		}
	}
	want := func(cur, rotated string) {
		t.Helper()
		if got := readLog(t, path); got != cur {
			t.Fatalf("the log holds %q, want %q", got, cur)
		}
		if got := readLog(t, path+".1"); got != rotated {
			t.Fatalf("the rotation holds %q, want %q", got, rotated)
		}
	}
	write("aaaa\n")
	write("bbbb\n")
	// The open fails exactly between the rename and the open: the rename is
	// the real one, and so is every open after the failure clears.
	failing := true
	opens := 0
	l.open = func(p string, flag int) (*os.File, error) {
		opens++
		if failing {
			return nil, syscall.EMFILE
		}
		return OpenFile(p, flag)
	}
	write("cccc\n")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the log was not renamed away: %v", err)
	}
	want("", "aaaa\nbbbb\ncccc\n")
	write("dddd\n")
	want("", "aaaa\nbbbb\ncccc\ndddd\n")
	if opens != 2 {
		t.Fatalf("the open was tried %d times, want once per write", opens)
	}
	failing = false
	write("eeee\n")
	want("eeee\n", "aaaa\nbbbb\ncccc\ndddd\n")
	write("ffff\n")
	want("eeee\nffff\n", "aaaa\nbbbb\ncccc\ndddd\n")
	write("gggg\n")
	want("gggg\n", "eeee\nffff\n")
	if opens != 4 {
		t.Fatalf("%d opens, want the retry and one rotation's", opens)
	}
}

// TestLogTruncateIfOver: a log already past the cap is emptied and counted
// empty, so the next write lands in it whole; one within the cap is left.
func TestLogTruncateIfOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.log")
	if err := os.WriteFile(path, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := openLog(path, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.TruncateIfOver(); err != nil || readLog(t, path) == "" {
		t.Fatalf("a log within the cap was truncated: %v", err)
	}
	_ = l.Close()
	l, err = openLog(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.TruncateIfOver(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, path); got != "new\n" || readLog(t, path+".1") != "" {
		t.Fatalf("the log holds %q after the truncate", got)
	}
}

// crashChildEnv names the test-binary re-exec that TestLogLeavesCrashOutputAlone
// runs: its value is the directory the child works in.
const crashChildEnv = "CRAZE_CAPLOG_CRASH_CHILD"

// crashMarker is the text the child panics with.
const crashMarker = "caplog-crash-marker-7f3a"

// TestLogLeavesCrashOutputAlone (plan 034 §3.3, panel: hostlog's crash output
// must not move into the shared writer): a caplog user that installs no hook
// leaves the runtime's crash output where the process put it, across the
// open, a rotation and Close. A child sets the crash output to a known file,
// opens a Log, rotates it, closes it, and panics; the panic's text must be in
// the known file and in none of the log's files.
func TestLogLeavesCrashOutputAlone(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLogCrashChild$")
	cmd.Env = append(os.Environ(), crashChildEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("the child was meant to die of its panic: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), crashMarker) {
		t.Fatalf("the child did not panic with the marker:\n%s", out)
	}
	if got := readLog(t, filepath.Join(dir, "known.crash")); !strings.Contains(got, crashMarker) {
		t.Fatalf("the crash output left the file the process set: %q", got)
	}
	for _, p := range []string{"h.log", "h.log.1"} {
		if got := readLog(t, filepath.Join(dir, p)); strings.Contains(got, crashMarker) {
			t.Fatalf("the crash output went to the caplog file %s: %q", p, got)
		}
	}
	if got := readLog(t, filepath.Join(dir, "h.log.1")); got == "" {
		t.Fatal("the child never rotated, so the test proved nothing")
	}
}

// TestLogCrashChild is the child of TestLogLeavesCrashOutputAlone; run
// directly it does nothing.
func TestLogCrashChild(t *testing.T) {
	dir := os.Getenv(crashChildEnv)
	if dir == "" {
		t.Skip("only runs as TestLogLeavesCrashOutputAlone's child")
	}
	known, err := os.Create(filepath.Join(dir, "known.crash"))
	if err != nil {
		t.Fatal(err)
	}
	if err := debug.SetCrashOutput(known, debug.CrashOptions{}); err != nil {
		t.Fatal(err)
	}
	l, err := openLog(filepath.Join(dir, "h.log"), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaa\n", "bbbb\n", "cccc\n"} {
		if _, err := l.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	panic(crashMarker)
}
