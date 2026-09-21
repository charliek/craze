package tool

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// maxLinks bounds the dangling symlinks RealPath follows, as the kernel's own
// ELOOP limit does.
const maxLinks = 40

// RealPath returns abs with every symlink resolved, as far as abs exists. A
// path that exists is filepath.EvalSymlinks's answer. For one that does not,
// the deepest ancestor that exists is resolved and the rest joined on, and a
// dangling symlink is followed to where its target would be: so a write
// through a link creates the link's target and leaves the link, and a check
// made before the open sees the file a call would really touch.
//
// It is the one walk the file tools and the mode gate share (plan 023 §3.1):
// a relative spelling, an alternative spelling of a directory, a symlink and a
// path that does not exist yet all normalize to what the eventual open lands
// on, so a gate judging Request.Targets judges the same file the call writes.
// abs must already be absolute (Env.Resolve).
func RealPath(abs string) (string, error) {
	p := abs
	for range maxLinks {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return r, nil
		}
		if !pathMissing(err) {
			return "", err
		}
		dir, err := RealPath(filepath.Dir(p))
		if err != nil {
			return "", err
		}
		q := filepath.Join(dir, filepath.Base(p))
		info, err := os.Lstat(q)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			// Absent, or not there to resolve: the open that follows
			// reports whatever is wrong with it.
			return q, nil
		}
		link, err := os.Readlink(q)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(dir, link)
		}
		p = link
	}
	return "", &fs.PathError{Op: "resolve", Path: abs, Err: syscall.ELOOP}
}

// pathMissing reports whether err says a path does not exist: it, or a
// directory on the way to it, is absent, or that directory is a file.
func pathMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
