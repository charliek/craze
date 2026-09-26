package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The cache tree, <Home>/.cache/craze/{hosts,locks}, is held by descriptor.
// Its canonical path is walked from "/" one component at a time, each
// component opened O_NOFOLLOW relative to the one before and validated
// through the descriptor that open returned (fstat), never by its path again;
// and everything below a leaf — lock files, the registry's atomic write, the
// identity-checked unlinks, the sweep's listing and its reads — is an *at
// call relative to the leaf's descriptor. So a rename after validation, by
// anyone who can write a directory on the way, changes nothing craze
// touches: what craze holds is what it validated, wherever it has been moved
// to. The worst a writer of an ancestor can do is make the next walk refuse
// (a symlink or a directory of theirs at a name), which is why the cache tree
// accepts an ancestor of the euid's own that its group can write
// (ancestorFault), and the runtime tree, used by path, does not.

// dirFlags opens a directory to hold: read-only (fstat, the *at calls and
// its entries need no more), never through a symlink at its own name,
// O_NONBLOCK so that a FIFO planted at the name cannot stall the open, and
// O_CLOEXEC so that no child craze spawns inherits it.
const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK

// dir is a directory held open: every operation below it is relative to fd.
type dir struct {
	fd   int
	path string // canonical; what messages and the paths craze reports name
}

func (d *dir) close() error { return unix.Close(d.fd) }

// join is the path of name in d, for messages.
func (d *dir) join(name string) string { return filepath.Join(d.path, name) }

// openat is openat(2), O_CLOEXEC always, retried on EINTR.
func openat(dirfd int, name string, flags int, perm uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flags|unix.O_CLOEXEC, perm)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

func isDirStat(st *unix.Stat_t) bool { return uint32(st.Mode)&unix.S_IFMT == unix.S_IFDIR }

func permOfStat(st *unix.Stat_t) uint32 { return uint32(st.Mode) & 0o7777 }

// idOfStat is st's (dev, ino), converted as idOf converts a FileInfo's.
func idOfStat(st *unix.Stat_t) fileID { return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)} }

// lstat is fstatat(d, name, AT_SYMLINK_NOFOLLOW).
func (d *dir) lstat(name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return st, err
}

// openDir opens name in d as a directory (dirFlags: never through a link)
// and returns it held, with the fstat of the descriptor it holds.
func (d *dir) openDir(name string) (*dir, unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := openat(d.fd, name, dirFlags, 0)
	if err != nil {
		return nil, st, err
	}
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, st, err
	}
	return &dir{fd: fd, path: d.join(name)}, st, nil
}

// openFault names what is at name in d when a directory open of it failed
// with err: O_NOFOLLOW|O_DIRECTORY reports a symlink as ELOOP on Darwin and
// ENOTDIR on Linux, and anything else not a directory as ENOTDIR, so an
// fstatat tells them apart. "" when it cannot.
func (d *dir) openFault(name string, err error) string {
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		return ""
	}
	st, serr := d.lstat(name)
	switch {
	case serr != nil:
		return ""
	case uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK:
		return "is a symlink"
	case !isDirStat(&st):
		return "is not a directory"
	}
	return ""
}

// walk opens the canonical path p from "/", one component at a time, each
// opened relative to the one before (never through a link: a component
// replaced by a symlink since p was canonicalised is refused here) and
// validated as a cache-tree ancestor on its own descriptor's fstat
// (ancestorFault, with the euid's own group-writable directories admitted).
// It returns p held.
func (env Env) walk(p string) (*dir, error) {
	d, st, err := (&dir{fd: unix.AT_FDCWD}).openDir("/")
	if err != nil {
		return nil, fmt.Errorf("ancestor /: %w", err)
	}
	rest := strings.TrimPrefix(p, "/")
	for {
		// Each component is validated before the next is opened through it.
		if err := env.ancestorFault(d.path, isDirStat(&st), int(st.Uid), permOfStat(&st), true); err != nil {
			_ = d.close()
			return nil, err
		}
		if rest == "" {
			return d, nil
		}
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		next, nst, err := d.openDir(name)
		if err != nil {
			if what := d.openFault(name, err); what != "" {
				err = fmt.Errorf("ancestor %s %s", d.join(name), what)
			} else {
				err = fmt.Errorf("ancestor %s: %w", d.join(name), err)
			}
			_ = d.close()
			return nil, err
		}
		_ = d.close()
		d, st = next, nst
	}
}

// leafAt opens name in d as a cache-tree leaf and returns it held. Missing:
// made 0700 (mkdirAt) when create is set, else an error wrapping
// fs.ErrNotExist. Then opened O_NOFOLLOW, and the leaf rule (leafFault)
// applied to the descriptor's fstat: never repaired.
func (env Env) leafAt(d *dir, name string, create bool) (*dir, error) {
	p := d.join(name)
	if create {
		if err := env.mkdirAt(d, name); err != nil {
			return nil, fmt.Errorf("create %s: %w", p, err)
		}
	}
	leaf, st, err := d.openDir(name)
	if errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	if err != nil {
		switch d.openFault(name, err) {
		case "is a symlink":
			return nil, fmt.Errorf("%s is a symlink; craze follows no link to a directory it owns (remove it)", p)
		case "is not a directory":
			return nil, fmt.Errorf("%s is not a directory (remove it)", p)
		}
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	if err := env.leafFault(p, isDirStat(&st), int(st.Uid), permOfStat(&st)); err != nil {
		_ = leaf.close()
		return nil, err
	}
	return leaf, nil
}

// mkdirAt makes name in d, 0700, when it is missing; a name already there is
// left as it is, for the caller to validate like any other. The umask can
// only clear bits, so the directory made is 0700 or less (or 2700: Linux
// passes a setgid parent's bit on), and it is chmod-ed 0700 to restore owner
// bits a hostile umask stripped — only a directory this call made. Under a
// parent its group can write, a group member could swap another directory
// of the euid's in at the name between the mkdirat and the chmod, so the
// chmod is also only for a directory owned by the euid with no group or
// other bits: one with any is left, to be refused. It is fchmod-ed through
// an O_NOFOLLOW open, or, when the umask took the owner's read bit too so
// that it cannot be opened, chmod-ed by name without following a link.
func (env Env) mkdirAt(d *dir, name string) error {
	switch err := unix.Mkdirat(d.fd, name, modeLeaf); {
	case errors.Is(err, unix.EEXIST):
		return nil
	case err != nil:
		return err
	}
	restore := func(st *unix.Stat_t) bool {
		perm := permOfStat(st)
		return isDirStat(st) && int(st.Uid) == env.EUID && perm != modeLeaf && perm&0o077 == 0
	}
	fd, err := openat(d.fd, name, dirFlags, 0)
	if errors.Is(err, unix.EACCES) {
		st, err := d.lstat(name)
		if err != nil || !restore(&st) {
			return err
		}
		// fchmodat2 on Linux (6.6 and later); earlier kernels answer
		// EOPNOTSUPP, and the umask must leave the owner's bits.
		if err := unix.Fchmodat(d.fd, name, modeLeaf, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("restore the owner's bits the umask cleared (use a umask that keeps them, such as 022): %w", err)
		}
		return nil
	}
	if err != nil {
		return nil // something else is at the name now: the leaf's own open names it
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || !restore(&st) {
		return err
	}
	return unix.Fchmod(fd, modeLeaf)
}

// openFile opens the regular file name in d: openat O_NOFOLLOW (a symlink at
// name is refused), O_NONBLOCK (a FIFO planted at name is refused at once
// instead of waiting forever for a writer) and O_CLOEXEC (no child craze
// spawns — an agent — inherits the descriptor, or a lock with it). The
// descriptor is fstat-ed, and anything but a regular file refused, before a
// byte is read or written; the file returned is an ordinary blocking one.
func (d *dir) openFile(name string, flag int, perm uint32) (*os.File, error) {
	p := d.join(name)
	fd, err := openat(d.fd, name, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK, perm)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: p, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "fcntl", Path: p, Err: err}
	}
	return os.NewFile(uintptr(fd), p), nil
}

// names is d's entries, sorted, read from a fresh descriptor opened on d
// itself (".", relative to its descriptor): d's own offset is never moved.
func (d *dir) names() ([]string, error) {
	fd, err := openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: d.path, Err: err}
	}
	f := os.NewFile(uintptr(fd), d.path)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// tempTries bounds replace's search for an unused temporary name.
const tempTries = 10000

// replace makes name in d a new file holding b, mode perm, atomically: a
// temporary ".<name>.<random>" is created O_CREAT|O_EXCL|O_NOFOLLOW in d,
// written, fchmod-ed perm (past the umask) and closed, then renameat-ed onto
// name, so a reader never sees a half-written file and a failed write never
// truncates name; the temporary is unlinked on any failure. It returns the
// new file's identity, fstat-ed on the descriptor it was written through:
// there is no stat after the rename to fail.
func (d *dir) replace(name string, b []byte, perm uint32) (fileID, error) {
	var f *os.File
	var tmp string
	for range tempTries {
		tmp = "." + name + "." + strconv.FormatUint(uint64(rand.Uint32()), 10)
		var err error
		f, err = d.openFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fileID{}, err
		}
		break
	}
	if f == nil {
		return fileID{}, fmt.Errorf("no unused temporary name for %s in %d tries", d.join(name), tempTries)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = unix.Unlinkat(d.fd, tmp, 0)
		}
	}()
	var st unix.Stat_t
	_, err := f.Write(b)
	if err == nil {
		err = unix.Fchmod(int(f.Fd()), perm)
	}
	if err == nil {
		err = unix.Fstat(int(f.Fd()), &st)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fileID{}, fmt.Errorf("write %s: %w", d.join(tmp), err)
	}
	if err := unix.Renameat(d.fd, tmp, d.fd, name); err != nil {
		return fileID{}, fmt.Errorf("rename %s to %s: %w", d.join(tmp), name, err)
	}
	renamed = true
	return idOfStat(&st), nil
}

// unlink removes the non-directory name in d; one already gone is not an
// error.
func (d *dir) unlink(name string) error {
	if err := unix.Unlinkat(d.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unlink %s: %w", d.join(name), err)
	}
	return nil
}

// unlinkIfOurs removes name in d only while it is still the file whose
// identity was recorded (fstatat, then unlinkat): a file put in its place
// since is left alone, as is a name already gone.
func (d *dir) unlinkIfOurs(name string, want fileID) error {
	st, err := d.lstat(name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", d.join(name), err)
	}
	if idOfStat(&st) != want {
		return nil
	}
	return d.unlink(name)
}

// sameFile reports whether name in d still names the file f has open: the
// check that a lock taken on a descriptor is still the lock at its name
// before anything is unlinked on its authority.
func (d *dir) sameFile(f *os.File, name string) bool {
	var held unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &held); err != nil {
		return false
	}
	named, err := d.lstat(name)
	if err != nil {
		return false
	}
	return idOfStat(&held) == idOfStat(&named)
}
