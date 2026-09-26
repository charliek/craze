package atomicfile

import (
	"errors"
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

// TestLockWithinAcquiresAFreeLock: nothing holds it, so the first try takes
// it, and a Lock after the unlock is not kept waiting.
func TestLockWithinAcquiresAFreeLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.lock")
	unlock, err := LockWithin(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	again, err := LockWithin(path, 0)
	if err != nil {
		t.Fatalf("a released lock was not free to a single try: %v", err)
	}
	again()
}

// TestLockWithinTimesOutAtItsBound: a holder that keeps the lock past the
// bound is ErrLockBusy — not before the bound, and not long after it: the poll
// never sleeps past the deadline.
func TestLockWithinTimesOutAtItsBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.lock")
	held, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	const bound = 150 * time.Millisecond
	start := time.Now()
	unlock, err := LockWithin(path, bound)
	took := time.Since(start)
	if !errors.Is(err, ErrLockBusy) {
		unlock()
		t.Fatalf("LockWithin on a held lock = %v, want ErrLockBusy", err)
	}
	if unlock == nil {
		t.Fatal("unlock must be non-nil even on error")
	}
	unlock() // a no-op, and must not release the holder's lock
	if took < bound {
		t.Fatalf("gave up after %s, before its bound of %s", took, bound)
	}
	// One poll interval of slack for the last try, plus scheduling.
	if took > bound+lockPoll+time.Second {
		t.Fatalf("took %s, long past its bound of %s", took, bound)
	}
	if _, err := LockWithin(path, 0); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("the failed LockWithin's unlock released the holder's lock: %v", err)
	}
}

// TestLockWithinTakesALockReleasedInsideTheBound: a holder that lets go while
// LockWithin is polling is waited for, not reported busy.
func TestLockWithinTakesALockReleasedInsideTheBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.lock")
	held, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(3 * lockPoll)
		held()
		close(released)
	}()
	unlock, err := LockWithin(path, 10*time.Second)
	if err != nil {
		t.Fatalf("LockWithin = %v, want the lock once its holder let go", err)
	}
	unlock()
	<-released
}

// TestLockWithinReturnsTheOpenError is Lock's contract: a lock file that
// cannot be opened is its own error, not ErrLockBusy.
func TestLockWithinReturnsTheOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "index.lock")
	unlock, err := LockWithin(path, time.Second)
	if err == nil || errors.Is(err, ErrLockBusy) {
		t.Fatalf("LockWithin = %v, want the open error", err)
	}
	unlock()
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

// TestWriteCheckedRefusesBeforeTheRename: a failing check leaves the target
// as it was and no temp file behind, and the check runs after the new
// content is complete (it can see the temp file). A passing check is the
// negative control.
func TestWriteCheckedRefusesBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("changed on disk")
	sawTemp := false
	err := WriteChecked(path, []byte("new"), 0o644, func() error {
		matches, _ := filepath.Glob(filepath.Join(dir, ".notes.txt.*"))
		for _, m := range matches {
			if b, _ := os.ReadFile(m); string(b) == "new" {
				sawTemp = true
			}
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("WriteChecked = %v, want the check's error", err)
	}
	if !sawTemp {
		t.Fatal("the check ran before the temp file held the new content")
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("content = %q after a refused write, want %q", b, "old")
	}
	assertOnlyFile(t, dir, "notes.txt")

	if err := WriteChecked(path, []byte("new"), 0o644, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content = %q after a passing check, want %q", b, "new")
	}
	assertOnlyFile(t, dir, "notes.txt")
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
