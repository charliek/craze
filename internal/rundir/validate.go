package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// The mode bits the rules read (st_mode & 07777).
const (
	modeSticky     = 0o1000
	modeGroupWrite = 0o020
	modeOtherWrite = 0o002
	modeLeaf       = 0o700
)

// fileID is a file's identity: two files at one path across a replacement
// differ here though the path is byte-identical.
type fileID struct{ dev, ino uint64 }

// statOf is fi's raw stat. Every FileInfo this package reads comes from
// os.Lstat or (*os.File).Stat on Linux or Darwin, where it is a
// *syscall.Stat_t.
func statOf(fi fs.FileInfo) *syscall.Stat_t {
	return fi.Sys().(*syscall.Stat_t)
}

// idOf is fi's (dev, ino). Stat_t.Dev is an int32 on Darwin and a uint64 on
// Linux; the conversion is the same on both.
func idOf(fi fs.FileInfo) fileID {
	st := statOf(fi)
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

// permOf is fi's st_mode & 07777: permissions plus setuid, setgid and sticky.
func permOf(fi fs.FileInfo) uint32 {
	return uint32(statOf(fi).Mode) & 0o7777
}

// canonical resolves every symlink in the absolute path p. It is how a
// chain's parent — the part of a path craze does not own — is made real
// before each of its components is validated as an ancestor.
func canonical(p string) (string, error) {
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(c) {
		return "", fmt.Errorf("%s resolves to the relative path %s", p, c)
	}
	return c, nil
}

// components is every directory from "/" down to p, p included; p is
// absolute and clean.
func components(p string) []string {
	var out []string
	for {
		out = append(out, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// checkAncestors validates every component of the canonical path dir, dir
// included, as an ancestor (checkAncestor).
func (env Env) checkAncestors(dir string) error {
	for _, p := range components(dir) {
		if err := env.checkAncestor(p); err != nil {
			return err
		}
	}
	return nil
}

// checkAncestor refuses a directory a craze path runs through unless nobody
// but root and the euid can replace what is under it: it is lstat-ed (so a
// symlink swapped in since the path was canonicalised shows here), must be a
// directory owned by root or the euid, and must not be group- or
// world-writable unless the sticky bit is set, or it is the euid's own and
// only its user-private group can write it (privateGroupWritable). A writable
// one is refused with the chmod that would make it acceptable.
func (env Env) checkAncestor(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("ancestor %s: %w", p, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("ancestor %s is a symlink", p)
	}
	if !fi.IsDir() {
		return fmt.Errorf("ancestor %s is not a directory", p)
	}
	st := statOf(fi)
	if uid := int(st.Uid); uid != 0 && uid != env.EUID {
		return fmt.Errorf("ancestor %s is owned by uid %d, not root or uid %d", p, uid, env.EUID)
	}
	mode := permOf(fi)
	if mode&(modeGroupWrite|modeOtherWrite) != 0 && mode&modeSticky == 0 &&
		!env.privateGroupWritable(int(st.Uid), int(st.Gid), mode) {
		who := "go"
		switch mode & (modeGroupWrite | modeOtherWrite) {
		case modeGroupWrite:
			who = "g"
		case modeOtherWrite:
			who = "o"
		}
		return fmt.Errorf("ancestor %s is group- or world-writable without the sticky bit (mode %04o), "+
			"so another user could replace what craze puts under it; if nobody else should write to it, run: chmod %s-w %s",
			p, mode, who, p)
	}
	return nil
}

// privateGroupWritable is the one writable-ancestor exemption besides the
// sticky bit: the directory is the euid's, others cannot write it, the group
// that can is the euid's user-private group, and every account is local
// (localAccounts), so that group's membership is all in the files privateGID
// read.
func (env Env) privateGroupWritable(uid, gid int, mode uint32) bool {
	return mode&modeOtherWrite == 0 && uid == env.EUID && env.PrivateGID > 0 && gid == env.PrivateGID &&
		localAccounts(env.NSSwitch)
}

// mkdirPrivate makes p 0700: mkdir 0700 (the umask can only clear bits), then
// an explicit chmod 0700 that restores owner bits a hostile umask stripped. A
// p that already exists — a peer won the race — is left as it is, for the
// caller to validate like any other.
func mkdirPrivate(p string) error {
	switch err := os.Mkdir(p, modeLeaf); {
	case err == nil:
		return os.Chmod(p, modeLeaf)
	case errors.Is(err, fs.ErrExist):
		return nil
	default:
		return err
	}
}

// leaf validates a directory craze names — p, whose parent is already
// validated — lstat-ed, never followed. Missing: made 0700 (mkdirPrivate)
// when create is set, else an error wrapping fs.ErrNotExist (Hosts reads a
// tree it never builds). Present, or just made: a directory, not a symlink,
// owned by the euid, mode exactly 0700. Never repaired: a leaf with any other
// mode is refused, and its mode is left as it was.
func (env Env) leaf(p string, create bool) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
		if err := mkdirPrivate(p); err != nil {
			return fmt.Errorf("create %s: %w", p, err)
		}
		fi, err = os.Lstat(p)
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", p, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; craze follows no link to a directory it owns (remove it)", p)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (remove it)", p)
	}
	if uid := int(statOf(fi).Uid); uid != env.EUID {
		return fmt.Errorf("%s is owned by uid %d, not uid %d", p, uid, env.EUID)
	}
	if mode := permOf(fi); mode != modeLeaf {
		return fmt.Errorf("%s has mode %04o, and 0700 is required; craze never changes it (chmod 700 it or remove it)", p, mode)
	}
	return nil
}
