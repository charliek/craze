package cli

import (
	"os"
	"path/filepath"
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

// TestHostLogRotatesOnceAtTheCap is §3.3's cap: a write that would take the
// log past it first moves the log to <log>.1 — replacing the rotation before —
// and starts a new one, 0600; a write larger than the cap on its own lands
// whole in a fresh file; a log already there is appended to, its size counted.
func TestHostLogRotatesOnceAtTheCap(t *testing.T) {
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

	again, err := openHostLog(env, path, 29)
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

// TestHostLogRefusesWhatIsNotAFile: a symlink at the log's name is refused,
// never written through, and so is a FIFO, which would hold the host on its
// first line.
func TestHostLogRefusesWhatIsNotAFile(t *testing.T) {
	env, _ := serveHome(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if l, err := openHostLog(env, link, hostLogMax); err == nil {
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
	if l, err := openHostLog(env, fifo, hostLogMax); err == nil {
		_ = l.Close()
		t.Fatal("a FIFO was opened as a log")
	}
}
