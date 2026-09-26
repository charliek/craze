package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// openLock opens the lock file name in d read-write, creating it 0600 when
// it is missing (d.openFile: O_NOFOLLOW, regular files only). The create is
// exclusive, so a file this call made is known to be its own, and that one is
// fchmod-ed 0600, as directories and sockets are chmod-ed: the umask may have
// cleared the owner's bits, and a lock file without them could never be
// opened again — by Hosts, probing a live host, or by the session's next
// claim. A file that already exists is opened as it is, and never changed.
func openLock(d *dir, name string) (*os.File, error) {
	f, err := d.openFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return d.openFile(name, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
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
	// A pid_t is 32 bits: a larger number is no pid, and kill would truncate it.
	if err != nil || pid <= 0 || pid > math.MaxInt32 || !ValidHostID(hostID) {
		return Holder{}
	}
	return Holder{PID: pid, HostID: hostID}
}

// alive reports whether a process with this pid exists: kill(pid, 0) sends
// nothing, and fails with ESRCH only when there is none (EPERM is a process
// of another uid, which exists).
func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// unlinkFile removes the non-directory at path; one already gone is not an
// error. Path-based: for the runtime tree's socket (the cache tree's unlinks
// are dir.unlink).
func unlinkFile(path string) error {
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unlink %s: %w", path, err)
	}
	return nil
}

// Holder is who holds a lock: the pid and host id its file names. PID 0 is a
// holder that has not written its line yet (the instant after its flock), or
// whose file still names a process that no longer exists — "pid ?" — and the
// lock is held all the same.
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
// before any path is built. The locks directory is walked and held for the
// claim alone (cacheDir) and the lock file opened relative to it; the Claim
// keeps only the lock file's descriptor.
func ClaimSession(env Env, crazeID, hostID string) (*Claim, error) {
	if !ValidToken(crazeID) {
		return nil, fmt.Errorf("rundir: session id %q is not a token of 1-128 characters from [A-Za-z0-9._-]", crazeID)
	}
	if !ValidHostID(hostID) {
		return nil, fmt.Errorf("rundir: host id %q is not 12 lowercase hex digits", hostID)
	}
	locks, err := env.cacheDir(locksName, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = locks.close() }()
	name := crazeID + ".lock"
	path := locks.join(name)
	f, err := openLock(locks, name)
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
		// A process that died holding the lock leaves its line complete, and
		// the next holder truncates it only after its own flock: a claimer
		// in between reads the dead one's. A pid no process has is nobody's.
		if holder.PID > 0 && !alive(holder.PID) {
			holder = Holder{}
		}
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
