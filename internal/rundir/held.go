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
// component opened O_NOFOLLOW (O_PATH too on Linux, so that an ancestor needs
// only search permission) relative to the one before and validated
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
// O_CLOEXEC (openat's) so that no child craze spawns inherits it. It is how
// every leaf is opened; an ancestor on the walk is opened ancestorFlags
// (held_linux.go, held_other.go), which on Linux needs no read permission.
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

// openDir opens name in d as a leaf directory (dirFlags: never through a
// link) and returns it held, with the fstat of the descriptor it holds.
func (d *dir) openDir(name string) (*dir, unix.Stat_t, error) {
	return d.openDirWith(name, dirFlags)
}

// openDirWith is openDir with the open's flags given: dirFlags for a leaf,
// ancestorFlags for an ancestor on the walk.
func (d *dir) openDirWith(name string, flags int) (*dir, unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := openat(d.fd, name, flags, 0)
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
// It returns p held. Every component, p included, is opened ancestorFlags:
// on Linux O_PATH, so a search-only ancestor is walked; what is done relative
// to p — the leaves opened and made in it — needs no more.
func (env Env) walk(p string) (*dir, error) {
	d, st, err := (&dir{fd: unix.AT_FDCWD}).openDirWith("/", ancestorFlags)
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
		next, nst, err := d.openDirWith(name, ancestorFlags)
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
// fs.ErrNotExist. Then opened and validated as it is (openLeaf): never
// repaired, even when this call has just made it.
func (env Env) leafAt(d *dir, name string, create bool) (*dir, error) {
	made := false
	if create {
		var err error
		if made, err = mkdirAt(d, name); err != nil {
			return nil, fmt.Errorf("create %s: %w", d.join(name), err)
		}
	}
	return env.openLeaf(d, name, made)
}

// openLeaf opens name in d O_NOFOLLOW (dirFlags) and applies the leaf rule
// (leafFault) to the fstat of the descriptor it holds: a directory, owned by
// the euid, permission bits exactly 0700 (setgid allowed). made is whether
// mkdirAt has just made name; when it has, a refusal the umask explains says
// so (madeFault), since what failed is then the process's setting, not a
// directory someone left.
func (env Env) openLeaf(d *dir, name string, made bool) (*dir, error) {
	p := d.join(name)
	leaf, st, err := d.openDir(name)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	case made && errors.Is(err, unix.EACCES):
		// Made 0700 and not readable: the umask took the owner's read bit.
		return nil, fmt.Errorf("%s was just made and cannot be opened: %s", p, umaskFix(p))
	case err != nil:
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
		if made {
			if merr := env.madeFault(p, &st); merr != nil {
				return nil, merr
			}
		}
		return nil, err
	}
	return leaf, nil
}

// afterMkdirat runs between a cache-tree directory's mkdirat and the open
// that validates it (mkdirAt), with the new directory's path: nothing in
// production, and in a test (never in parallel) a writer of the parent
// renaming another directory to the name.
var afterMkdirat = func(string) {}

// mkdirAt makes name in d, mode 0700, when it is missing, and reports whether
// this call made it; a name already there is left as it is, for the caller
// to validate like any other.
//
// Nothing is chmod-ed afterwards, not even the directory this call made. In a
// parent its group can write — the ~/.cache this tree accepts
// (ancestorFault) — a member of the group can rename another directory of
// the euid's to the name the instant after the mkdirat, so no step by name
// after it (an open and fchmod, a chmod that follows no link) can prove it
// acts on the directory made. The name is opened and validated as it is,
// like any other leaf (openLeaf). The umask only clears bits, and every
// ordinary one (002, 022, 027, 077) leaves mkdir's 0700 as it is (2700 in a
// setgid parent on Linux, which the leaf rule allows); one that removes
// owner permissions (277, 777) leaves a directory the leaf rule refuses, and
// the refusal says so (madeFault).
func mkdirAt(d *dir, name string) (bool, error) {
	switch err := unix.Mkdirat(d.fd, name, modeLeaf); {
	case errors.Is(err, unix.EEXIST):
		return false, nil
	case err != nil:
		return false, err
	}
	afterMkdirat(d.join(name))
	return true, nil
}

// madeFault is why a directory mkdirAt has just made at p, with stat st,
// fails the leaf rule, when its shape is what the umask leaves: the euid's
// own, with no group or other bits, and short of an owner bit. nil
// otherwise, and leafFault's message stands. p is refused, not chmod-ed: by
// now the name may be someone else's directory. (A setgid bit, which Linux
// passes on to a directory made in a setgid one, is no fault: leafFault
// allows it.)
func (env Env) madeFault(p string, st *unix.Stat_t) error {
	perm := permOfStat(st)
	if !isDirStat(st) || int(st.Uid) != env.EUID || perm&0o077 != 0 || perm&modeLeaf == modeLeaf {
		return nil
	}
	return fmt.Errorf("%s was just made mode %04o, not 0700: %s", p, perm, umaskFix(p))
}

// umaskFix names the cause and the fix of a cache-tree directory the umask
// left without owner permissions.
func umaskFix(p string) string {
	return "the umask removed owner permissions, and craze never chmods a directory it makes " +
		"(set a umask that keeps them, such as 022, then remove " + p + ")"
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

// tempName is a fresh temporary name for name, in name's directory:
// ".<name>.<random decimal>" (replace).
func tempName(name string) string {
	return "." + name + "." + strconv.FormatUint(uint64(rand.Uint32()), 10)
}

// isTempOf reports whether n is one of name's temporaries (tempName): the
// debris of a write that died before its rename.
func isTempOf(n, name string) bool {
	rest, ok := strings.CutPrefix(n, "."+name+".")
	if !ok || rest == "" {
		return false
	}
	for i := range len(rest) {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

// fstatTemp is replace's fstat of the temporary it has just created:
// unix.Fstat, which a test replaces (never in parallel) to fail it.
var fstatTemp = unix.Fstat

// replace makes name in d a new file holding b, mode perm, atomically: a
// temporary (tempName) is created O_CREAT|O_EXCL|O_NOFOLLOW in d — a regular
// file, and the very one this call made — then fstat-ed, written, fchmod-ed
// perm (past the umask) and closed, then renameat-ed onto name, so a reader
// never sees a half-written file and a failed write never truncates name.
// The temporary's unlink is installed the moment the exclusive create
// returns, before any step that can fail, so no failure leaves it. It returns
// the new file's identity, fstat-ed on the descriptor it was written through:
// there is no stat after the rename to fail.
func (d *dir) replace(name string, b []byte, perm uint32) (fileID, error) {
	var f *os.File
	var tmp string
	for range tempTries {
		tmp = tempName(name)
		fd, err := openat(d.fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return fileID{}, &fs.PathError{Op: "create", Path: d.join(tmp), Err: err}
		}
		f = os.NewFile(uintptr(fd), d.join(tmp))
		break
	}
	if f == nil {
		return fileID{}, fmt.Errorf("no unused temporary name for %s in %d tries", d.join(name), tempTries)
	}
	// Unlinked by name, which is safe here: d is the euid's own 0700 leaf,
	// held by descriptor, so only the euid (or root) can have put anything
	// else at tmp since the exclusive create made it.
	renamed := false
	defer func() {
		if !renamed {
			_ = unix.Unlinkat(d.fd, tmp, 0)
		}
	}()
	var st unix.Stat_t
	err := fstatTemp(int(f.Fd()), &st)
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = unix.Fchmod(int(f.Fd()), perm)
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
