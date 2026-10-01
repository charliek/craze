package attach

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// The attachments directory (plan 033 §3.2): <CRAZE_HOME>/attachments, handed
// to every function here as an argument (this package cannot import
// internal/paths). The TUI writes processed copies into it (Save) and sweeps
// it (Sweep); the host reads them back (OpenDir, ReadPath) when it sends a
// message whose envelope names them.
//
// Nothing in it is trusted. The envelope that names a file is forgeable by
// any socket client or a typed draft (P2), and the user — or anything running
// as the user — can put whatever it likes in the directory. So every use goes
// through an os.Root opened on the directory after checking it is a real
// directory, 0700 and the user's own (openDir); every file the host reads is
// checked to be a regular file named the way the store names files, read
// through a limit, and validated from its bytes (Validate).

// ErrNotStored is a path that is not a file the store could have written: not
// directly inside the attachments directory, or not named
// <16 hex digits>.<png|jpg|webp>.
var ErrNotStored = errors.New("not a file in craze's attachments directory")

// hashLen is how many hex digits of the content's SHA-256 name a stored file:
// 64 bits, so a collision between two pastes is not a practical concern, and
// Save compares the bytes on a name hit anyway.
const hashLen = 16

// tempSuffix ends the name of a file Save is still writing:
// .<stored name>.<8 hex digits>.tmp. The leading dot and the suffix keep it
// out of StoredName's pattern, so the host never reads one; Sweep removes a
// crashed write's leftover by age and budget like any stored file.
const tempSuffix = ".tmp"

// lockName is the store's cross-process lock file, inside the directory
// (plan 033 C3r, r1 #12). It is no stored name and no temp name, so the host
// never reads it and the sweep never removes it; it is created on first use
// and stays.
const lockName = ".lock"

// How long Save and Sweep wait for the store's lock. Save, holding a chip
// open, waits a few seconds — a sweep of a full directory takes a moment —
// and then fails the attach (ErrStoreBusy): it never refreshes or replaces a
// file without the lock, since that is the race the lock exists for (plan 033
// C6r, r2 #5). Sweep, a best-effort tidy at TUI start, waits a second and then
// gives up quietly: the next start sweeps. Vars only so tests can shorten
// them; nothing in craze writes them.
var (
	saveLockWait  = 5 * time.Second
	sweepLockWait = time.Second
)

// lockBusy runs when the store's lock was busy at its first try, before the
// wait: the point at which a test knows the other side holds it. A var only
// so tests can set it; nothing in craze writes it.
var lockBusy = func() {}

// flockStore is the cross-process half of the store's lock:
// atomicfile.FlockWithin. A var only so tests can stand in a flock that never
// excludes this process from itself, as one emulated over fcntl does (Linux's
// NFS client); nothing in craze writes it.
var flockStore = atomicfile.FlockWithin

// storeSlots is the in-process half of the store's lock: one slot (a channel
// of one) per attachments directory this process has locked, keyed by its
// clean absolute path (root.Name()). A process uses one directory, two at
// most, so they are never removed.
var storeSlots sync.Map

// storeSlot is the in-process slot for the attachments directory root.
func storeSlot(root *os.Root) chan struct{} {
	slot, _ := storeSlots.LoadOrStore(root.Name(), make(chan struct{}, 1))
	return slot.(chan struct{})
}

// lockStore takes the store's lock in root, waiting at most d for the whole of
// it, and answers ErrStoreBusy when it is still held by then (plan 033 C6r,
// r2 #5). It has two halves, taken in this order and released together:
//
//   - the in-process slot for the directory (storeSlot), so a Save and the
//     Sweep in one craze exclude each other however the filesystem's flock
//     treats two opens by one process — a flock emulated over fcntl, as
//     Linux's NFS client does, grants both;
//   - the flock of lockName (atomicfile.FlockWithin), which excludes every
//     other craze. The file is opened through the Root, so the lock is the
//     one in this directory whatever its path now names.
//
// unlock is never nil; on success it releases both halves and closes the
// file.
func lockStore(root *os.Root, d time.Duration) (unlock func(), err error) {
	noop := func() {}
	deadline := time.Now().Add(d)
	slot := storeSlot(root)
	busy := false
	select {
	case slot <- struct{}{}:
	default:
		busy = true
		lockBusy()
		wait := time.NewTimer(d)
		select {
		case slot <- struct{}{}:
			wait.Stop()
		case <-wait.C:
			return noop, ErrStoreBusy
		}
	}
	leave := func() { <-slot }
	f, err := root.OpenFile(lockName, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		leave()
		return noop, fmt.Errorf("attach: %w", err)
	}
	release, err := flockStore(f, 0)
	if errors.Is(err, atomicfile.ErrLockBusy) {
		if !busy {
			lockBusy()
		}
		release, err = flockStore(f, time.Until(deadline))
	}
	if err != nil {
		_ = f.Close()
		leave()
		if errors.Is(err, atomicfile.ErrLockBusy) {
			return noop, ErrStoreBusy
		}
		return noop, fmt.Errorf("attach: %w", err)
	}
	return func() {
		release()
		_ = f.Close()
		leave()
	}, nil
}

// StoredName reports whether name is a name Save gives a file:
// <hashLen lowercase hex digits>.<png|jpg|webp>.
func StoredName(name string) bool {
	digits, ext, ok := strings.Cut(name, ".")
	return ok && len(digits) == hashLen && extMIME[ext] != "" && isLowerHex(digits)
}

// tempName reports whether name is one of Save's temp files.
func tempName(name string) bool {
	rest, ok := strings.CutPrefix(name, ".")
	if !ok {
		return false
	}
	rest, ok = strings.CutSuffix(rest, tempSuffix)
	if !ok || len(rest) < 9 || rest[len(rest)-9] != '.' {
		return false
	}
	return isLowerHex(rest[len(rest)-8:]) && StoredName(rest[:len(rest)-9])
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sweepStatted runs between Sweep's listing stat of a file and its re-stat of
// it, and sweepRemoving between that re-stat and the removal: the windows a
// Save in another craze would use. Tests run one there.
var (
	sweepStatted  = func(string) {}
	sweepRemoving = func(string) {}
)

// dirChecked runs between openDir's check of the directory and its open of
// it: the window a swap would use. Tests make the swap there.
var dirChecked = func() {}

// nameChecked runs between ReadFile's Lstat of a name and its open of it: the
// window in which a regular file can be swapped for a symlink or a FIFO. Tests
// make the swap there.
var nameChecked = func() {}

// sizeChecked runs between ReadFile's fstat and its read: the window in which
// a file can grow past what the fstat said. Tests grow it there.
var sizeChecked = func() {}

// OpenDir opens the attachments directory dir for reading, confined: see
// openDir. A missing directory is an error matching fs.ErrNotExist; anything
// else wrong with it is ErrUnsafeDir.
func OpenDir(dir string) (*os.Root, error) {
	return openDir(dir, false)
}

// openDir returns dir as an os.Root, creating it (and its parents) first when
// create is set. Every later operation on a stored file goes through the
// Root, relative to the directory's own descriptor, so none can be redirected
// outside it by a rename or a symlink appearing in its place.
//
// It is tool.openSpillDir's check, open, compare (truncate.go), plus an owner
// check. The entry must be a real directory, not a symlink (Lstat, relative
// to the parent). It is then opened as a Root, and the directory that opened
// must be the one checked (os.SameFile), and owned by the effective user.
// Its mode must be 0700: with create (the store's open) a wider mode is
// tightened through the opened descriptor, never by path; without it (the
// host's read and the sweep) it is refused, since a directory others can
// write into is one whose files nobody vouches for.
//
// dir must be absolute. Its parent (CRAZE_HOME) is trusted as configured and
// may be a symlink.
func openDir(dir string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("attach: the attachments directory must be an absolute path, not %q", dir)
	}
	dir = filepath.Clean(dir)
	parent, base := filepath.Dir(dir), filepath.Base(dir)
	if parent == dir {
		return nil, fmt.Errorf("attach: %q cannot be the attachments directory", dir)
	}
	if create {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, fmt.Errorf("attach: %w", err)
		}
	}
	pr, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	defer pr.Close()
	if create {
		if err := pr.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("attach: %w", err)
		}
	}
	checked, err := pr.Lstat(base)
	if err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	if !checked.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafeDir, dir)
	}
	dirChecked()
	root, err := pr.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeDir, err)
	}
	if err := private(root, checked, create); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// private checks that root is the directory checked describes and the
// effective user's, and that its mode is 0700 — making it so through its own
// descriptor when tighten is set.
func private(root *os.Root, checked fs.FileInfo, tighten bool) error {
	d, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	defer d.Close()
	info, err := d.Stat()
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	if !os.SameFile(checked, info) {
		return fmt.Errorf("%w: %s changed while it was being opened", ErrUnsafeDir, root.Name())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is not owned by this user", ErrUnsafeDir, root.Name())
	}
	if info.Mode().Perm() == 0o700 {
		return nil
	}
	if !tighten {
		return fmt.Errorf("%w: %s has mode %v, not 0700", ErrUnsafeDir, root.Name(), info.Mode().Perm())
	}
	if err := d.Chmod(0o700); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	return nil
}

// Save stores data, an image of type mime that Process produced, in the
// attachments directory dir and returns its absolute path:
// dir/<first 16 hex digits of its SHA-256>.<ext>, mode 0600, in a 0700
// directory created if absent (plan 033 §3.2).
//
// The write is atomic: a temp file created with O_EXCL beside it, written,
// closed and renamed over the name, all through the Root. A name that already
// holds these exact bytes — the same screenshot pasted twice — is not
// rewritten: its modification time is refreshed instead, so the sweep, which
// goes by age, keeps the file a draft has just pointed at again.
//
// Data over MaxBytes is refused (ErrTooLarge): the host would refuse to read
// it, and the store keeps nothing the host would not send.
//
// The dedupe's refresh and the publish hold the store's lock (lockStore), so
// a Sweep — in another craze or in this one — cannot decide on a file's age,
// let this refresh it, and then remove it anyway, leaving a chip whose file
// is gone (r1 #12). Nothing is refreshed or replaced without it: a lock still
// busy after saveLockWait fails the Save with ErrStoreBusy, and any other
// failure to take it fails it too (plan 033 C6r, r2 #5) — the composer's
// usual note, and the paste stays text.
func Save(dir string, data []byte, mime string) (string, error) {
	ext := Ext(mime)
	if ext == "" {
		return "", fmt.Errorf("attach: %q is not a type the store keeps", mime)
	}
	if len(data) > MaxBytes {
		return "", ErrTooLarge
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:hashLen/2]) + "." + ext
	root, err := openDir(dir, true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	unlock, err := lockStore(root, saveLockWait)
	if err != nil {
		return "", err
	}
	defer unlock()
	path := filepath.Join(dir, name)
	if old, err := ReadFile(root, name); err == nil && bytes.Equal(old, data) {
		now := time.Now()
		if err := root.Chtimes(name, now, now); err != nil {
			return "", fmt.Errorf("attach: %w", err)
		}
		return path, nil
	}
	f, tmp, err := createTemp(root, name)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = root.Remove(tmp)
		return "", fmt.Errorf("attach: %w", err)
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return "", fmt.Errorf("attach: %w", err)
	}
	return path, nil
}

// tempAttempts is how many random temp names createTemp tries.
const tempAttempts = 8

// createTemp creates Save's temp file for name in root, 0600, with O_EXCL and
// O_NOFOLLOW so nothing already at the name — a symlink included — is ever
// opened, and returns it with its name.
func createTemp(root *os.Root, name string) (*os.File, string, error) {
	for range tempAttempts {
		var b [4]byte
		_, _ = rand.Read(b[:]) // never fails (crypto/rand)
		tmp := "." + name + "." + hex.EncodeToString(b[:]) + tempSuffix
		f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("attach: %w", err)
		}
		// The mode a umask cannot narrow further: 0600 even under a umask
		// that would take the owner's own bits.
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			_ = root.Remove(tmp)
			return nil, "", fmt.Errorf("attach: %w", err)
		}
		return f, tmp, nil
	}
	return nil, "", fmt.Errorf("attach: no free temp file name for %s in %s", name, root.Name())
}

// ReadFile reads the stored file name from root, an attachments directory
// OpenDir opened. It is the host's confined read (plan 033 §3.1, P2):
//
//   - name must be one the store writes (StoredName), else ErrNotStored;
//   - it must be a regular file — not a symlink, a FIFO, a device or a
//     directory — else ErrNotRegular. os.Root resolves a symlink inside the
//     root itself whatever O_NOFOLLOW says (it adds the flag and follows the
//     link on ELOOP), so the refusal is check, open, compare: Lstat says
//     regular, the file is opened with O_NONBLOCK (a FIFO swapped in between
//     must not block the open) and its fstat must be that same regular file;
//   - it is read through a limit of MaxBytes+1, so a file that is over the
//     cap, or grows past it after the fstat, is ErrTooLarge rather than read
//     whole.
//
// A missing file is an error matching fs.ErrNotExist.
func ReadFile(root *os.Root, name string) ([]byte, error) {
	if !StoredName(name) {
		return nil, ErrNotStored
	}
	checked, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !checked.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	nameChecked()
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, ErrNotRegular
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(checked, info) {
		return nil, fmt.Errorf("%w: it changed while it was being opened", ErrNotRegular)
	}
	if info.Size() > MaxBytes {
		return nil, ErrTooLarge
	}
	sizeChecked()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// ReadPath is ReadFile for a stored file named by its absolute path, as an
// envelope names it (plan 033 §3.1, X3): the path must be clean and directly
// inside root's directory, else ErrNotStored, and its base name is then read
// as ReadFile reads one. root is an attachments directory OpenDir opened, so
// root.Name() is that directory's clean absolute path — the comparison is of
// two strings, and the read itself goes through the Root whatever they say.
func ReadPath(root *os.Root, path string) ([]byte, error) {
	if filepath.Clean(path) != path || filepath.Dir(path) != root.Name() {
		return nil, ErrNotStored
	}
	return ReadFile(root, filepath.Base(path))
}

// Sweep removes old files from the attachments directory dir, best effort,
// and returns how many it removed (plan 033 §3.2, P29). First every file
// older than MaxAge; then, while the files left add up to more than
// DirBudget, the oldest first. The TUI runs it once at start.
//
// It opens the directory as the host does (openDir), so a symlinked, swapped,
// foreign or widened directory is an error and nothing in it is listed or
// removed; a missing one is nothing to do. It removes only regular files
// named like the store's own (stored files and a crashed write's temp
// files), never follows a symlink, never removes the directory itself, and
// treats a file that vanishes under it — another craze sweeping too — as no
// error. The first other error is returned after every file has been tried.
//
// The listing, every decision and every removal hold the store's lock
// (lockStore), so no Save refreshes or replaces a file between the stat that
// condemned it and its removal (r1 #12; a Save never goes on without the
// lock, r2 #5); a lock still busy after sweepLockWait is a sweep given up,
// quietly — the next start sweeps. Each file is stat'ed again just before it
// goes, and one that has changed since the listing — anything that writes the
// directory without the lock: the user, an older craze — is kept.
func Sweep(dir string, now time.Time) (int, error) {
	return sweep(dir, now, DirBudget)
}

// sweep is Sweep with the budget as a parameter, so a test can exercise it
// with a few small files.
func sweep(dir string, now time.Time, budget int64) (int, error) {
	root, err := openDir(dir, false)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, err
	}
	defer root.Close()
	unlock, err := lockStore(root, sweepLockWait)
	defer unlock()
	if err != nil {
		return 0, nil
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, fmt.Errorf("attach: %w", err)
	}
	removed := 0
	var first error
	// remove takes fi's file, the one the listing stat'ed, unless it has
	// changed since; it reports whether the file is gone.
	remove := func(fi fs.FileInfo) bool {
		name := fi.Name()
		sweepStatted(name)
		again, err := root.Lstat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return true
		case err != nil || !os.SameFile(fi, again) || !again.ModTime().Equal(fi.ModTime()) || again.Size() != fi.Size():
			return false
		}
		sweepRemoving(name)
		err = root.Remove(name)
		switch {
		case err == nil:
			removed++
			return true
		case errors.Is(err, fs.ErrNotExist):
			return true
		case first == nil:
			first = fmt.Errorf("attach: %w", err)
		}
		return false
	}
	var left []fs.FileInfo
	var total int64
	cutoff := now.Add(-MaxAge)
	for _, e := range entries {
		if !StoredName(e.Name()) && !tempName(e.Name()) {
			continue
		}
		// Through the Root, not the DirEntry: its Info would lstat by path.
		fi, err := root.Lstat(e.Name())
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			if remove(fi) {
				continue
			}
		}
		left = append(left, fi)
		total += fi.Size()
	}
	if total <= budget {
		return removed, first
	}
	slices.SortFunc(left, func(a, b fs.FileInfo) int {
		return cmp.Or(a.ModTime().Compare(b.ModTime()), strings.Compare(a.Name(), b.Name()))
	})
	for _, fi := range left {
		if total <= budget {
			break
		}
		if remove(fi) {
			total -= fi.Size()
		}
	}
	return removed, first
}
