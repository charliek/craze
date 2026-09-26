package rundir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// openNoFollow opens path with O_NOFOLLOW and O_CLOEXEC — a symlink at path
// is refused, and no child craze spawns (an agent) inherits the descriptor,
// or a lock with it — and refuses anything but a regular file.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, perm)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return f, nil
}

// tryLock is flock(LOCK_EX|LOCK_NB) on f: true when taken, false when
// another open file description holds it. flock locks belong to the open
// file description, so two opens of one file contend even within a process.
func tryLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return false, err
		}
	}
}

// unlock is an explicit LOCK_UN. Closing is not enough: a forked child that
// inherited the descriptor would keep the lock alive, and LOCK_UN releases it
// on the open file description every such descriptor shares.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// writeHolder replaces a lock file's contents, once its flock is held, with
// "<pid> <hostId>\n". The newline marks the line complete: a reader that
// catches it half-written sees no holder, not a wrong one.
func writeHolder(f *os.File, hostID string) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+" "+hostID+"\n"), 0)
	return err
}

// holderMax bounds a holder read: a pid, a space, a host id and a newline.
const holderMax = 64

// readHolder is who a lock file names; the zero Holder when the line is
// empty, incomplete or malformed.
func readHolder(f *os.File) Holder {
	b := make([]byte, holderMax)
	n, _ := f.ReadAt(b, 0)
	return parseHolder(string(b[:n]))
}

func parseHolder(s string) Holder {
	line, ok := strings.CutSuffix(s, "\n")
	if !ok {
		return Holder{}
	}
	pidText, hostID, ok := strings.Cut(line, " ")
	if !ok {
		return Holder{}
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 || !ValidHostID(hostID) {
		return Holder{}
	}
	return Holder{PID: pid, HostID: hostID}
}

// sameFile reports whether path still names the file f has open: the check
// that a lock taken on a descriptor is still the lock at its name before
// anything is unlinked on its authority.
func sameFile(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return idOf(held) == idOf(named)
}

// unlinkFile removes a non-directory; one already gone is not an error.
func unlinkFile(path string) error {
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unlink %s: %w", path, err)
	}
	return nil
}

// Holder is who holds a lock: the pid and host id its file names. PID 0 is a
// holder that has not written its line yet (the instant after its flock) —
// "pid ?" — and the lock is held all the same.
type Holder struct {
	PID    int
	HostID string
}

// HeldError is ClaimSession's refusal: another process holds the session.
// Find it with errors.As.
type HeldError struct {
	CrazeID string
	Holder  Holder
}

func (e *HeldError) Error() string {
	pid := "?"
	if e.Holder.PID > 0 {
		pid = strconv.Itoa(e.Holder.PID)
	}
	s := "rundir: session " + e.CrazeID + " is open in another craze (pid " + pid
	if e.Holder.HostID != "" {
		s += ", host " + e.Holder.HostID
	}
	return s + ")"
}

// Claim is one process's hold on a craze session (SQ16): the open, flocked
// locks/<crazeId>.lock. It is held for the process's life and handed through,
// never released and taken again on another descriptor.
type Claim struct {
	crazeID string
	path    string

	mu sync.Mutex
	f  *os.File
}

// ClaimSession takes <Home>/.cache/craze/locks/<crazeID>.lock without
// blocking and writes "<pid> <hostId>" into it. It depends on Env.Home alone —
// never on the socket base or CRAZE_HOME — so two crazes contend for one
// session whatever else differs between them, and it is taken even when the
// control socket is off. A session another process holds is a *HeldError
// naming the holder its lock file names. crazeID is checked (ValidToken)
// before any path is built.
func ClaimSession(env Env, crazeID, hostID string) (*Claim, error) {
	if !ValidToken(crazeID) {
		return nil, fmt.Errorf("rundir: session id %q is not a token of 1-128 characters from [A-Za-z0-9._-]", crazeID)
	}
	if !ValidHostID(hostID) {
		return nil, fmt.Errorf("rundir: host id %q is not 12 lowercase hex digits", hostID)
	}
	dir, err := env.cacheSubdir(locksName, true)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, crazeID+".lock")
	f, err := openNoFollow(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("rundir: open the session lock: %w", err)
	}
	taken, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("rundir: lock %s: %w", path, err)
	}
	if !taken {
		holder := readHolder(f)
		_ = f.Close()
		return nil, &HeldError{CrazeID: crazeID, Holder: holder}
	}
	if err := writeHolder(f, hostID); err != nil {
		_ = unlock(f)
		_ = f.Close()
		return nil, fmt.Errorf("rundir: write %s: %w", path, err)
	}
	return &Claim{crazeID: crazeID, path: path, f: f}, nil
}

// CrazeID is the session this claim holds.
func (c *Claim) CrazeID() string { return c.crazeID }

// Path is the claim's lock file.
func (c *Claim) Path() string { return c.path }

// Release lets the session go: its holder line is cleared, then LOCK_UN, then
// close. It is idempotent. The lock file is never unlinked: its name is
// reused by the next claim, and another process may already have it open to
// try it.
func (c *Claim) Release() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.f == nil {
		return nil
	}
	f := c.f
	c.f = nil
	// Cleared under the lock, so the next holder's instant between its flock
	// and its write reads "pid ?", never this process's pid.
	_ = f.Truncate(0)
	return errors.Join(unlock(f), f.Close())
}
