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
	modeSetgid     = 0o2000
	modeGroupWrite = 0o020
	modeOtherWrite = 0o002
	modeLeaf       = 0o700
)

// fileID is a file's identity: two files at one path across a replacement
// differ here though the path is byte-identical.
type fileID struct{ dev, ino uint64 }

// statOf is fi's raw stat. Every FileInfo this package reads comes from
// os.Lstat on Linux or Darwin, where it is a *syscall.Stat_t.
func statOf(fi fs.FileInfo) *syscall.Stat_t {
	return fi.Sys().(*syscall.Stat_t)
}

// idOf is fi's (dev, ino). Stat_t.Dev is an int32 on Darwin and a uint64 on
// Linux; the conversion is the same on both, and the same as idOfStat's.
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
// before each of its components is validated as an ancestor. In the cache
// tree it only says where to walk (walk); the walk refuses a link.
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

// ancestorFault is the ancestor rule on one directory's stat — why p cannot
// be a directory a craze path runs through, or nil. It must be a directory
// owned by root or the euid (with links resolved, the owner check is what
// stops a path through another user's directory), and not group- or
// world-writable unless the sticky bit is set. ownGroupWritable admits one
// more: a directory of the euid's own that its group, and not others, can
// write (a user-private group's 0775 ~/.cache under umask 002). Only the cache
// tree passes it, because only the cache tree is held by descriptor (walk): a
// group member who renames what craze validated there changes nothing craze
// touches. The runtime tree is used by path, so there a group-writable
// directory is refused whoever owns it, and a root-owned one is refused in
// both. A writable one is refused with the chmod that would make it
// acceptable.
func (env Env) ancestorFault(p string, isDir bool, uid int, perm uint32, ownGroupWritable bool) error {
	if !isDir {
		return fmt.Errorf("ancestor %s is not a directory", p)
	}
	if uid != 0 && uid != env.EUID {
		return fmt.Errorf("ancestor %s is owned by uid %d, not root or uid %d", p, uid, env.EUID)
	}
	writable := perm & (modeGroupWrite | modeOtherWrite)
	if writable == 0 || perm&modeSticky != 0 ||
		(ownGroupWritable && writable == modeGroupWrite && uid == env.EUID) {
		return nil
	}
	who := "go"
	switch writable {
	case modeGroupWrite:
		who = "g"
	case modeOtherWrite:
		who = "o"
	}
	return fmt.Errorf("ancestor %s is group- or world-writable without the sticky bit (mode %04o), "+
		"so another user could replace what craze puts under it; if nobody else should write to it, run: chmod %s-w %s",
		p, perm, who, p)
}

// leafFault is the leaf rule on one directory's stat — why p, a directory
// craze names, cannot be used, or nil: it must be a directory owned by the
// euid whose permission bits are exactly 0700. The setgid bit is allowed: on
// a directory it only decides which group new entries take, and with no
// group permission bits it grants nothing — and Linux sets it on every
// directory made in a setgid one, so refusing it would refuse craze under a
// setgid ~/.cache or home. Setuid and sticky are refused. The rule is the
// same in both trees. It is never repaired: a leaf with any other mode is
// refused, and its mode is left as it was. (A symlink never reaches here: a
// leaf is lstat-ed, or opened O_NOFOLLOW.)
func (env Env) leafFault(p string, isDir bool, uid int, perm uint32) error {
	if !isDir {
		return fmt.Errorf("%s is not a directory (remove it)", p)
	}
	if uid != env.EUID {
		return fmt.Errorf("%s is owned by uid %d, not uid %d", p, uid, env.EUID)
	}
	if perm&^modeSetgid != modeLeaf {
		return fmt.Errorf("%s has mode %04o, and 0700 is required; craze never changes it (chmod 700 it or remove it)", p, perm)
	}
	return nil
}

// The runtime tree — the socket's — is validated by path, because bind(2)
// takes one: checkAncestors, leaf and mkdirPrivate below. Nothing holds it
// between the check and the use, so its ancestors take the strict rule.

// checkAncestors validates every component of the canonical path dir, dir
// included, as a runtime-tree ancestor (checkAncestor).
func (env Env) checkAncestors(dir string) error {
	for _, p := range components(dir) {
		if err := env.checkAncestor(p); err != nil {
			return err
		}
	}
	return nil
}

// checkAncestor is the strict ancestor rule (ancestorFault, with no
// group-writable exemption) on p, lstat-ed: a symlink swapped in since the
// path was canonicalised shows here.
func (env Env) checkAncestor(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("ancestor %s: %w", p, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("ancestor %s is a symlink", p)
	}
	return env.ancestorFault(p, fi.IsDir(), int(statOf(fi).Uid), permOf(fi), false)
}

// mkdirPrivate makes p 0700: mkdir 0700 (the umask can only clear bits), then
// an explicit chmod 0700 that restores owner bits a hostile umask stripped. A
// p that already exists — a peer won the race — is left as it is, for the
// caller to validate like any other. Path-based: for the runtime tree, whose
// parents take the strict rule, so nobody else can rename a directory to p
// between the mkdir and the chmod. (The cache tree, whose parents may be the
// euid's own group-writable ones, chmods nothing it makes: mkdirAt.)
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

// leaf validates a runtime-tree directory craze names — p, whose parent is
// already validated — lstat-ed, never followed. Missing: made 0700
// (mkdirPrivate). Present, or just made: not a symlink, and the leaf rule
// (leafFault).
func (env Env) leaf(p string) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
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
	return env.leafFault(p, fi.IsDir(), int(statOf(fi).Uid), permOf(fi))
}
