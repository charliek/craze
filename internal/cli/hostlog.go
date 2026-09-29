package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// craze serve's log (plan 030 §3.3). A headless host has no terminal: the
// spawner (C3) starts it with stdin, stdout and stderr on /dev/null, so
// whatever it has to say — craze's own diagnostics and the agent child's
// stderr, which the TUI defers behind its alt screen (deferredStderr) — goes to
// the file --log names instead, which the spawner sets to
// ~/.cache/craze/host-logs/<hostId>.log. Without --log it is the command's
// stderr, as for any command run by hand.

// hostLogMax is the size a host log is rotated at: 4 MiB, with one rotation
// kept (<log>.1), so a host holds at most twice that however long it runs and
// whatever its agent says.
const hostLogMax = 4 << 20

// hostLogKeep is how long a gone host's logs are kept: every host sweeps the
// host logs older than this as it starts (rundir.SweepHostLogs).
const hostLogKeep = 7 * 24 * time.Hour

// hostLog is a log file capped at max bytes with one rotation: a write that
// would take the file past max first renames it to <path>.1 — replacing the
// rotation before it — and opens a new, empty <path>, 0600. One write is never
// split: a write larger than max on its own lands whole in a fresh file. A
// rotation that fails leaves the writes going to whichever file is open, and
// is tried again at the next write past the cap: a log that cannot rotate must
// not stop the host that writes it. It is safe for concurrent use — craze's own
// lines and the agent's stderr tee arrive from different goroutines — and every
// write is one write(2) under its lock, so lines never interleave.
//
// The file is also where the Go runtime writes a fatal error or an unrecovered
// panic (debug.SetCrashOutput), re-pointed at every rotation: a detached
// host's own stderr is /dev/null, and its crash is the one thing its log must
// not lose.
type hostLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// openHostLog opens path for a host's log, creating it 0600 — appending to one
// already there — after validating its directory (hostLogDir). It must be a
// regular file, never followed through a symlink.
func openHostLog(env rundir.Env, path string, max int64) (*hostLog, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, err := hostLogDir(env, filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	abs = filepath.Join(dir, filepath.Base(abs))
	f, err := openLogFile(abs, os.O_APPEND)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	l := &hostLog{path: abs, max: max, f: f, size: st.Size()}
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	return l, nil
}

// hostLogDir is the directory a host log is opened in. The cache tree's own
// host-logs directory — where the spawner puts every log — is validated as the
// rest of the cache tree is and made 0700 where it is missing
// (rundir.HostLogDir), and answered by its canonical path. Any other directory
// is the user's own choice of --log: made 0700 when it is missing, and used as
// it is when it is not.
func hostLogDir(env rundir.Env, dir string) (string, error) {
	if env.Home != "" && sameDir(dir, filepath.Join(env.Home, ".cache", "craze", "host-logs")) {
		return rundir.HostLogDir(env)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// sameDir reports whether a and b name one directory: the same path, or two
// paths that both resolve to it. A path that does not resolve (missing) is
// only ever the same as itself.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ca, errA := filepath.EvalSymlinks(a)
	cb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ca == cb
}

// openLogFile opens a log file for writing, 0600 when it is made: O_NOFOLLOW,
// so a symlink planted at the name is refused rather than written through, and
// anything but a regular file is refused too (a FIFO would block the host on
// its first line). flag is O_APPEND for the log a host starts on and O_TRUNC
// for the one a rotation starts.
func openLogFile(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|flag, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	// O_NONBLOCK was for the open alone: a regular file's writes block, as a
	// log's should.
	if err := syscall.SetNonblock(int(f.Fd()), false); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Write appends p, rotating first when p would take the file past the cap.
func (l *hostLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		l.rotateLocked()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotateLocked renames the log to <path>.1 and opens a new one in its place.
// The rename comes first, while the old file is still open, and the old file is
// closed only once the new one is: a failure at either step leaves the host
// writing to a file it has open (the renamed one, if only the open failed),
// with its size as it is, so the next write past the cap tries again.
func (l *hostLog) rotateLocked() {
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return
	}
	f, err := openLogFile(l.path, os.O_TRUNC)
	if err != nil {
		return
	}
	_ = l.f.Close()
	l.f, l.size = f, 0
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
}

// Close closes the log, and gives the runtime's crash output back to stderr
// alone. Idempotent.
func (l *hostLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	err := l.f.Close()
	l.f = nil
	return err
}

// openServeLog is where craze serve says what it has to say: --log's file
// (hostLog), or with none stderr. close is the log's own close, a no-op for
// stderr. A --log that cannot be opened is exit 1, said on stderr — there is
// nowhere else yet.
func openServeLog(env rundir.Env, path string, stderr io.Writer) (w io.Writer, close func(), err error) {
	if path == "" {
		return stderr, func() {}, nil
	}
	l, err := openHostLog(env, path, hostLogMax)
	if err != nil {
		return nil, nil, exitf(1, "craze serve: --log %s: %v", path, err)
	}
	return l, func() { _ = l.Close() }, nil
}

// sweepHostLogs is every host's sweep as it starts (plan 030 §3.3): the host
// logs, and the agents' process-group records beside them, of hosts gone for
// longer than hostLogKeep. A sweep that cannot run is one line on the log; the
// host runs either way.
func sweepHostLogs(env rundir.Env, log io.Writer) {
	if _, err := rundir.SweepHostLogs(env, time.Now(), hostLogKeep); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(log, "craze serve: old host logs not swept: %v\n", err)
	}
}
