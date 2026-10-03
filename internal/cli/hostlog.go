package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/caplog"
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
// whatever its agent says. A variable only so that a test's craze serve child
// can rotate at a few bytes (serve_child_test.go).
var hostLogMax int64 = 4 << 20

// hostLogKeep is how long a gone host's logs are kept: every host sweeps the
// host logs older than this as it starts (rundir.SweepHostLogs).
const hostLogKeep = 7 * 24 * time.Hour

// hostLog is a host's log: a capped log with one rotation (caplog.Log) that is
// also where the Go runtime writes a fatal error or an unrecovered panic
// (debug.SetCrashOutput), re-pointed at every rotation: a detached host's own
// stderr is /dev/null, and its crash is the one thing its log must not lose.
// The crash output is hostLog's own, installed through caplog's hooks —
// caplog itself never touches it (plan 034 §3.3). Close gives the crash output
// back to stderr alone, so craze serve closes its log only on a clean return
// (runServe), never on a panic's way out.
type hostLog = caplog.Log

// openHostLog opens path for a host's log, creating it 0600 — appending to one
// already there — after validating its directory (hostLogDir). It must be a
// regular file, never followed through a symlink.
//
// Its modification time is set to now as it is opened: a log reused from a
// host gone a week (a --log naming an old file) would otherwise keep its old
// time until the first line, and another host's start-up sweep could remove
// it from under this one (astra r3-c2 3). Once the host is bound, its own host
// lock keeps it (rundir.SweepHostLogs); before that, its age does.
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
	f, err := caplog.OpenFile(abs, os.O_APPEND)
	if err != nil {
		return nil, err
	}
	// The name, not followed: openLogFile has just refused a link at it.
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, abs, nil, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = f.Close()
		return nil, &os.PathError{Op: "utimensat", Path: abs, Err: err}
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return caplog.New(abs, f, st.Size(), max, caplog.Options{
		OnOpen:  func(f *os.File) { _ = debug.SetCrashOutput(f, debug.CrashOptions{}) },
		OnClose: func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) },
	}), nil
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

// openServeLog is where craze serve says what it has to say: --log's file
// (hostLog), or with none stderr — after the start-up sweep (sweepHostLogs),
// which runs first and never takes the log about to be opened: a sweep after
// the open would remove a reused log a week old, the host's own, before its
// first line (astra r3-c2 3). close is the log's own close, a no-op for
// stderr. A --log that cannot be opened is exit 1, said on stderr — there is
// nowhere else yet.
func openServeLog(env rundir.Env, path string, stderr io.Writer) (w io.Writer, close func(), err error) {
	swept := sweepHostLogs(env, activeHostLog(env, path))
	if path == "" {
		reportSweep(stderr, swept)
		return stderr, func() {}, nil
	}
	l, err := openHostLog(env, path, hostLogMax)
	if err != nil {
		return nil, nil, exitf(1, "craze serve: --log %s: %v", path, err)
	}
	reportSweep(l, swept)
	return l, func() { _ = l.Close() }, nil
}

// activeHostLog is the name of the log path in the host logs' directory —
// the one name this host's sweep keeps — or "" when path is elsewhere.
func activeHostLog(env rundir.Env, path string) string {
	if path == "" || env.Home == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil || !sameDir(filepath.Dir(abs), filepath.Join(env.Home, ".cache", "craze", "host-logs")) {
		return ""
	}
	return filepath.Base(abs)
}

// sweepHostLogs is every host's sweep as it starts (plan 030 §3.3): the host
// logs, and the agents' process-group records beside them, of hosts gone for
// longer than hostLogKeep, but never keep, the log this host is about to
// open. It answers why it could not run, or nil.
func sweepHostLogs(env rundir.Env, keep string) error {
	if _, err := rundir.SweepHostLogs(env, time.Now(), hostLogKeep, keep); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// reportSweep says on the log, in one line, that the start-up sweep could not
// run; the host runs either way.
func reportSweep(log io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(log, "craze serve: old host logs not swept: %v\n", err)
	}
}
