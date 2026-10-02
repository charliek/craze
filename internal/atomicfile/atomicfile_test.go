package atomicfile

import (
	"context"
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

// TestLockWithinNeverAcquiresPastItsBound (astra r30 7): every try after the
// first is made inside the bound, so a lock held for the whole of it is
// ErrLockBusy even when its holder lets go just past it. The clock is the
// test's: each poll's sleep overshoots by a nanosecond, as a real one does by
// scheduling, and the holder lets go the instant the clock passes the
// deadline — during the last sleep, before any try could see it.
func TestLockWithinNeverAcquiresPastItsBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.lock")
	held, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			held()
		}
	}
	defer release()
	const bound = 5 * lockPoll / 2 // two whole polls, then half of one
	clock := time.Unix(1_000_000, 0)
	deadline := clock.Add(bound)
	now = func() time.Time { return clock }
	sleep = func(d time.Duration) {
		clock = clock.Add(d + time.Nanosecond)
		if clock.After(deadline) {
			release()
		}
	}
	t.Cleanup(func() { now, sleep = time.Now, time.Sleep })

	unlock, err := LockWithin(path, bound)
	if !errors.Is(err, ErrLockBusy) {
		unlock()
		t.Fatalf("LockWithin = %v, its holder gone only %s past the bound; want ErrLockBusy", err, clock.Sub(deadline))
	}
	if !released {
		t.Fatal("the holder never let go: the poll never slept past its deadline")
	}
	unlock()
	// The holder really did let go: the next try takes it.
	again, err := LockWithin(path, 0)
	if err != nil {
		t.Fatalf("the released lock is still busy: %v", err)
	}
	again()
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

// countFsyncs replaces the fsync seam for one test: every call is counted,
// and fail, when it returns an error, makes that call fail.
func countFsyncs(t *testing.T, fail func(n int) error) *int {
	t.Helper()
	n := 0
	fsync = func(f *os.File) error {
		n++
		if fail != nil {
			if err := fail(n); err != nil {
				return err
			}
		}
		return f.Sync()
	}
	t.Cleanup(func() { fsync = (*os.File).Sync })
	return &n
}

// TestWriteSyncFlushesTheFileAndTheDirectory (plan 033 §3.10): WriteSync
// lands the contents at perm with no temp file left, and syncs twice — the
// file before the rename, the directory after it. Write is the control: the
// same write, and no sync at all.
func TestWriteSyncFlushesTheFileAndTheDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatgpt.json")
	syncs := countFsyncs(t, nil)
	if err := WriteSync(path, []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if *syncs != 2 {
		t.Fatalf("WriteSync synced %d times, want 2 (the file, then its directory)", *syncs)
	}
	if b, _ := os.ReadFile(path); string(b) != "durable" {
		t.Fatalf("content = %q", b)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat = %v, %v; want mode 0600", info, err)
	}
	assertOnlyFile(t, dir, "chatgpt.json")

	*syncs = 0
	if err := Write(path, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if *syncs != 0 {
		t.Fatalf("Write synced %d times; only the WriteSync family syncs", *syncs)
	}
}

// TestWriteSyncFailsOnAFailedFsync: a file whose contents could not be made
// durable is not renamed into place — the target keeps its old contents and
// no temp file is left. A directory fsync that fails after the rename is the
// write's failure too, with the new contents in place.
func TestWriteSyncFailsOnAFailedFsync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatgpt.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskFull := errors.New("no space left on device")
	countFsyncs(t, func(n int) error {
		if n == 1 {
			return diskFull
		}
		return nil
	})
	if err := WriteSync(path, []byte("new"), 0o600); !errors.Is(err, diskFull) {
		t.Fatalf("WriteSync = %v, want the file's fsync error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("content = %q after a failed fsync, want the old contents", b)
	}
	assertOnlyFile(t, dir, "chatgpt.json")

	countFsyncs(t, func(n int) error {
		if n == 2 {
			return diskFull
		}
		return nil
	})
	if err := WriteSync(path, []byte("new"), 0o600); !errors.Is(err, diskFull) {
		t.Fatalf("WriteSync = %v, want the directory's fsync error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content = %q after the rename, want the new contents", b)
	}
}

// TestWriteSyncCheckedRefusesBeforeTheRename: the check runs before the
// rename, and a refusal leaves the target as it was; a passing check is the
// control.
func TestWriteSyncCheckedRefusesBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatgpt.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := errors.New("the file was removed")
	if err := WriteSyncChecked(path, []byte("new"), 0o600, func() error { return gone }); !errors.Is(err, gone) {
		t.Fatalf("WriteSyncChecked = %v, want the check's error", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("content = %q after a refused write", b)
	}
	assertOnlyFile(t, dir, "chatgpt.json")
	if err := WriteSyncChecked(path, []byte("new"), 0o600, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content = %q after a passing check", b)
	}
}

// TestRemoveSyncSyncsTheDirectory: a removal is synced once (the
// directory); a path already gone is no error and syncs nothing.
func TestRemoveSyncSyncsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatgpt.json")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncs := countFsyncs(t, nil)
	if err := RemoveSync(path); err != nil {
		t.Fatal(err)
	}
	if *syncs != 1 {
		t.Fatalf("RemoveSync synced %d times, want 1", *syncs)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still there: %v", err)
	}
	*syncs = 0
	if err := RemoveSync(path); err != nil {
		t.Fatalf("RemoveSync of a missing file = %v, want nil", err)
	}
	if *syncs != 0 {
		t.Fatalf("RemoveSync of a missing file synced %d times", *syncs)
	}
}

// TestLockContextGivesUpWhenItsContextIsDone (plan 033 §2.3): a caller
// waiting on a held lock with a minute's bound returns at once when its
// context is cancelled, with the context's error; an already-done context
// tries nothing. The controls: a free lock is taken, and a held one with a
// short bound and a live context is ErrLockBusy.
func TestLockContextGivesUpWhenItsContextIsDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chatgpt.json.lock")

	unlock, err := LockContext(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("LockContext on a free lock = %v", err)
	}
	unlock()

	held, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	if unlock, err := LockContext(context.Background(), path, 3*lockPoll); !errors.Is(err, ErrLockBusy) {
		unlock()
		t.Fatalf("LockContext on a held lock = %v, want ErrLockBusy at its bound", err)
	}

	done, cancel := context.WithCancel(context.Background())
	cancel()
	if unlock, err := LockContext(done, path, time.Minute); !errors.Is(err, context.Canceled) {
		unlock()
		t.Fatalf("LockContext with a done context = %v, want context.Canceled", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		unlock, err := LockContext(ctx, path, time.Minute)
		unlock()
		got <- err
	}()
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LockContext = %v after its context was cancelled, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("LockContext kept waiting for a minute's bound after its context was cancelled")
	}
	if unlock, err := LockWithin(path, 0); !errors.Is(err, ErrLockBusy) {
		unlock()
		t.Fatalf("a cancelled LockContext released the holder's lock: %v", err)
	}
}
