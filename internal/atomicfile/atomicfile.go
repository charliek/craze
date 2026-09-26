// Package atomicfile holds the two primitives craze's on-disk stores build
// on: an exclusive lock that serialises a read-modify-write, and a write that
// can never leave a half-written file behind. Both were lifted out of
// internal/tui/config.go so internal/sessions could reuse them without
// depending on the tui package.
package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrLockBusy is LockWithin's answer when another holder kept the lock for the
// whole bound.
var ErrLockBusy = errors.New("atomicfile: the lock is busy")

// lockPoll is how often LockWithin tries a busy lock again.
const lockPoll = 20 * time.Millisecond

// Lock takes an exclusive lock on path, creating it if it does not exist.
// syscall.Flock exists on both Linux and Darwin (the two platforms this repo
// pins), so this works unchanged on both.
//
// Unlike internal/tui's old lockConfig, Lock returns the open/flock error
// instead of swallowing it -- callers that must not silently skip the lock
// (internal/sessions) can treat a failure as fatal, while callers that want
// config.go's old "never block a save on the lock" policy can ignore it
// themselves at the call site.
//
// The returned unlock is always non-nil, even on error, so callers can
// `defer unlock()` unconditionally.
func Lock(path string) (unlock func(), err error) {
	noop := func() {}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return noop, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return noop, err
	}
	return unlocker(f), nil
}

// LockWithin is Lock with a bound: it tries LOCK_EX|LOCK_NB, again every
// lockPoll, until it holds the lock or d has passed, and then answers
// ErrLockBusy. It never waits past d (a flock cannot be interrupted, so a
// caller that must not block — a picker's claim, --continue — cannot use Lock),
// and it tries at least once, so d <= 0 is a single attempt. Any other open or
// flock error is returned as it is.
//
// The returned unlock is always non-nil, as Lock's is.
func LockWithin(path string, d time.Duration) (unlock func(), err error) {
	noop := func() {}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return noop, err
	}
	deadline := time.Now().Add(d)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return unlocker(f), nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			_ = f.Close()
			return noop, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			_ = f.Close()
			return noop, ErrLockBusy
		}
		time.Sleep(min(lockPoll, left))
	}
}

// unlocker releases a lock Lock or LockWithin took, and closes its file.
func unlocker(f *os.File) func() {
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// Write replaces the contents of path with b atomically: a temp file named
// from path's own base name is created in path's directory, written, chmod'd
// to perm and renamed over path. A reader can never observe a half-written
// file, and an interrupted write can never truncate the target. The temp
// file is removed on any error path; once the rename succeeds, removing it
// is a no-op.
func Write(path string, b []byte, perm os.FileMode) error {
	return WriteChecked(path, b, perm, nil)
}

// WriteChecked is Write with a last check before the rename: check, when it
// is not nil, runs once the temp file is written and chmod'd, and an error
// from it is returned as it is, with path untouched and the temp file
// removed. It is for a caller that must not replace a file someone else
// changed while it was preparing the new content: the check sits as close to
// the rename as a rename-based write allows.
func WriteChecked(path string, b []byte, perm os.FileMode, check func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	return os.Rename(tmpName, path)
}
