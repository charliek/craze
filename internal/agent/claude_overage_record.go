package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/errkind"
)

// The trip record's file handling (plan 052 § D3; reviews C5 items 2–3, C5b
// items 1–2). The record is read before every claude start, prompt and wake,
// and written by every trip, so it is handled as untrusted input on a
// filesystem that can stall:
//
//   - Every open refuses to follow a symlink and to block (O_NOFOLLOW,
//     O_NONBLOCK) and requires a regular file: a FIFO or a device at the
//     record's name is refused, never waited on.
//   - Reads are bounded (overageReadBound) and fail closed: a check whose
//     read does not answer refuses with quota, naming the file.
//   - A trip starts its write on a goroutine of its own and waits for it at
//     most overageWriteBound before containing the session: a stalled
//     filesystem never holds the stop (tripOverage).
//   - The fallback is only ever a directory the user owns, and no other user
//     can write into: $XDG_RUNTIME_DIR/craze on Linux, the per-user temporary
//     directory on macOS, none elsewhere. It must be a real directory (not a
//     symlink), owned by this user, and is made 0700.

// The record's I/O bounds, vars for tests.
var (
	overageReadBound  = 2 * time.Second
	overageWriteBound = time.Second
)

// recordCap bounds what is read of a record.
const recordCap = 64 << 10

// testOverageIO is a test seam, nil in production: it runs at the start of
// every read and every write of a record, with what is done and where — a
// test blocks it to stand in for a filesystem that stalls.
var testOverageIO atomic.Pointer[func(op, path string)]

// overageIO runs testOverageIO, when a test has set it.
func overageIO(op, path string) {
	if f := testOverageIO.Load(); f != nil {
		(*f)(op, path)
	}
}

// defaultOverageFallback is where the trip record goes when the craze
// directory will not take it (review C5 item 3), "" for nowhere: a directory
// only this user can write into (review C5b item 2), under a name drawn from
// the record's own path, so each craze directory has one fallback.
func defaultOverageFallback(primary string) string {
	var dir string
	switch runtime.GOOS {
	case "linux":
		if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && filepath.IsAbs(rt) {
			dir = filepath.Join(rt, "craze")
		}
	case "darwin":
		// macOS's per-user /var/folders/… directory, 0700 and the user's.
		dir = filepath.Join(os.TempDir(), "craze")
	}
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(primary))
	return filepath.Join(dir, "claude-overage-"+hex.EncodeToString(sum[:6])+".json")
}

// checkFallbackDir checks the fallback's directory before it is written or
// trusted: made 0700 when missing (its parent must exist), and then a real
// directory, not a symlink, owned by this user; a mode that lets anyone else
// in is closed to 0700. Anything else is refused.
func checkFallbackDir(dir string, create bool) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory of its own", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != overageUID() {
		return fmt.Errorf("%s is not this user's", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// openRegular opens path with flags, refusing to follow a symlink or to
// block, and answers it only if it is a regular file.
func openRegular(path string, flags int, perm uint32) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, perm)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink", path)
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
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

// readOverageRecord is the record at path: nil for none, an error for one
// that is there and cannot be read, is not a regular file, or does not parse.
func readOverageRecord(path string) (*overageRecord, error) {
	overageIO("read", path)
	f, err := openRegular(path, syscall.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		// Nothing there, or nothing could be: a path through a file.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, recordCap))
	if err != nil {
		return nil, err
	}
	var rec overageRecord
	if err := json.Unmarshal(b, &rec); err != nil || rec.Until.IsZero() {
		return nil, errRecordUnusable
	}
	return &rec, nil
}

// lockRecord takes the record's exclusive lock, path + ".lock", opened as a
// regular file that is no symlink.
func lockRecord(path string) (unlock func(), err error) {
	f, err := openRegular(path+".lock", syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return nil, err
	}
	release, err := atomicfile.FlockWithin(f, overageLockWait)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { release(); _ = f.Close() }, nil
}

// testOverageAfterRead runs, when a test sets it, in writeOverageRecordAt
// between the read of the record there and the write that replaces it.
var testOverageAfterRead func(rec overageRecord)

// writeOverageRecordAt writes rec at path under an exclusive lock on its
// sibling path + ".lock" (review C5 item 2): the read, the comparison and the
// replacement are one transaction across every craze process on the machine,
// so a record that holds longer is never replaced by a shorter one — the later
// until, and its window, are kept. A record there that cannot be read is
// replaced (the rename replaces the entry, whatever it is).
func writeOverageRecordAt(path string, rec overageRecord) error {
	overageIO("write", path)
	unlock, err := lockRecord(path)
	if err != nil {
		return err
	}
	defer unlock()
	if prev, err := readOverageRecord(path); err == nil && prev != nil && prev.Until.After(rec.Until) {
		rec.Until, rec.Window = prev.Until, prev.Window
	}
	if testOverageAfterRead != nil {
		testOverageAfterRead(rec)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(b, '\n'), 0o600)
}

// writeOverageRecord writes rec as the machine's trip record. The craze
// directory's record is tried twice (its directory made if missing); when it
// will not take it, the fallback is written instead (review C5 item 3), which
// every check reads too. It answers where the record went, and the craze
// directory's failure when the fallback had to take it; err is set only when
// neither took it — the session that tripped still stops, but the hold is not
// recorded.
func writeOverageRecord(rec overageRecord) (where string, primaryErr, err error) {
	path, fallback, _ := overageSeams()
	if path != "" {
		primaryErr = os.MkdirAll(filepath.Dir(path), 0o700)
		if primaryErr == nil {
			if primaryErr = writeOverageRecordAt(path, rec); primaryErr != nil {
				primaryErr = writeOverageRecordAt(path, rec)
			}
		}
		if primaryErr == nil {
			return path, nil, nil
		}
	} else {
		primaryErr = errors.New("craze has no directory to keep the trip record in")
	}
	if fallback == "" {
		return "", primaryErr, fmt.Errorf("%v; and there is no private directory for a fallback", primaryErr)
	}
	ferr := checkFallbackDir(filepath.Dir(fallback), true)
	if ferr == nil {
		ferr = writeOverageRecordAt(fallback, rec)
	}
	if ferr != nil {
		return "", primaryErr, fmt.Errorf("%v; and the fallback %s: %v", primaryErr, fallback, ferr)
	}
	return fallback, primaryErr, nil
}

// readBounded is readOverageRecord bounded by overageReadBound: a read that
// does not answer in time is an error (its goroutine is left to the
// filesystem).
func readBounded(path string) (*overageRecord, error) {
	type answer struct {
		rec *overageRecord
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		rec, err := readOverageRecord(path)
		ch <- answer{rec, err}
	}()
	t := time.NewTimer(overageReadBound)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.rec, a.err
	case <-t.C:
		return nil, fmt.Errorf("its read did not answer within %v", overageReadBound)
	}
}

// overageRefusal is the trip record's word before a claude start, prompt or
// wake (§ D3): the quota refusal while it — the craze directory's, or its
// fallback (review C5 item 3), whichever holds longer — holds, nil once both
// have expired or when there are none. A record that is there and cannot be
// read, that is not a regular file, or whose read does not answer in time
// refuses too, and says which file: the guard fails closed (review C5b item
// 1). A fallback whose directory is not the user's own is not read at all.
func overageRefusal() error {
	path, fallback, now := overageSeams()
	if fallback != "" && checkFallbackDir(filepath.Dir(fallback), false) != nil {
		fallback = ""
	}
	var held *overageRecord
	heldAt := ""
	for _, p := range []string{path, fallback} {
		if p == "" {
			continue
		}
		rec, err := readBounded(p)
		if err != nil {
			return claudeErr(errkind.Quota, "craze cannot read its Claude overage record %s (%v); it refuses Claude sessions until the file is fixed or removed", p, err)
		}
		if rec != nil && rec.Until.After(now) && (held == nil || rec.Until.After(held.Until)) {
			held, heldAt = rec, p
		}
	}
	if held == nil {
		return nil
	}
	return claudeErr(errkind.Quota, "%s. craze refuses Claude sessions on this machine until then (%s)", overageWords(held.Window, held.Until), heldAt)
}
