package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if unlock == nil {
		t.Fatal("unlock must not be nil")
	}
	unlock()
}

// TestLockReturnsTheOpenError is the property that sets Lock apart from
// config.go's old lockConfig: a failure to even open the lock file (here, a
// parent directory that does not exist) surfaces as a real error, not a
// silently swallowed no-op.
func TestLockReturnsTheOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "index.lock")
	unlock, err := Lock(path)
	if err == nil {
		t.Fatal("want an error when the lock file cannot be opened")
	}
	if unlock == nil {
		t.Fatal("unlock must be non-nil even on error, so callers can defer it unconditionally")
	}
	unlock() // must not panic
}

// TestLockSerialisesTwoCallers pins the actual mutual-exclusion property:
// a second Lock call on the same path blocks until the first unlock.
func TestLockSerialisesTwoCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.lock")
	unlock1, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan struct{})
	go func() {
		unlock2, err := Lock(path)
		if err != nil {
			t.Error(err)
			return
		}
		defer unlock2()
		close(got)
	}()

	select {
	case <-got:
		t.Fatal("second Lock succeeded while the first was still held")
	case <-time.After(100 * time.Millisecond):
	}

	unlock1()

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second Lock never acquired the lock after the first released it")
	}
}

func TestWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")
	if err := Write(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("content = %q", b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	assertOnlyFile(t, dir, "sessions.jsonl")
}

// TestWriteReplacesExistingContent checks the rename actually lands over a
// pre-existing file rather than erroring or appending.
func TestWriteReplacesExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("content = %q, want %q", b, "new")
	}
	assertOnlyFile(t, dir, "sessions.jsonl")
}

// TestWriteCleansUpTempFileOnError forces the rename step to fail (the
// target is a directory, which cannot be renamed over) and checks the temp
// file created along the way does not survive.
func TestWriteCleansUpTempFileOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), 0o600); err == nil {
		t.Fatal("want an error when the target is a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sessions.jsonl" {
		t.Fatalf("directory contents = %v, want only the target directory itself", entries)
	}
}

func assertOnlyFile(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != want {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory contents = %v, want only %q", names, want)
	}
}
