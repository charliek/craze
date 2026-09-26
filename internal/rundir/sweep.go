package rundir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Hosts is every live host in the registry, in host-id order: what `craze
// bridge` and `craze attach` resolve a session against. It reads files and
// never connects.
//
// For each hosts/<id>.json (a name that is not a host id is ignored) it
// opens hosts/<id>.lock without creating it:
//   - missing: the host is mid-exit (it unlinks its entry before its lock) or
//     a sweeper is at work; skipped, nothing created;
//   - held by another: live; the entry is read (O_NOFOLLOW) and listed, or
//     skipped when it cannot be read;
//   - taken: the holder is dead and its entry stale. Holding that lock — the
//     only authority to unlink anything of another host's; a failed connect
//     is none — the sweep removes the entry, the socket it names (only when
//     that socket's directory validates, its name is <id>.sock and it is a
//     socket), and the lock file itself, then LOCK_UN. Host ids are never
//     reused, so a dead host's lock file can go.
//
// A registry tree that does not exist yet is no hosts, and is not created;
// one that fails validation is an error.
func Hosts(env Env) ([]Entry, error) {
	dir, err := env.cacheSubdir(hostsName, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var live []Entry
	for _, de := range names {
		id, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || !ValidHostID(id) {
			continue
		}
		if e, ok := env.probe(dir, id); ok {
			live = append(live, e)
		}
	}
	return live, nil
}

// probe is one registry entry's check: its live Entry, or false when it is
// not listed (stale and swept, mid-exit, or unreadable).
func (env Env) probe(dir, id string) (Entry, bool) {
	entryPath := filepath.Join(dir, id+".json")
	lockPath := filepath.Join(dir, id+".lock")
	lock, err := openNoFollow(lockPath, os.O_RDWR, 0)
	if err != nil {
		return Entry{}, false
	}
	defer lock.Close()
	taken, err := tryLock(lock)
	if err != nil {
		return Entry{}, false
	}
	if !taken {
		e, err := readEntry(entryPath)
		if err != nil || e.HostID != id {
			return Entry{}, false
		}
		return e, true
	}
	defer func() { _ = unlock(lock) }() // before the Close deferred above
	// The lock taken must still be the one at the name: a sweeper that got
	// here first unlinked it, and this descriptor holds a lock on a file no
	// longer anyone's.
	if sameFile(lock, lockPath) {
		env.sweep(id, entryPath, lockPath)
	}
	return Entry{}, false
}

// sweep removes a dead host's files; its lock is held by the caller.
func (env Env) sweep(id, entryPath, lockPath string) {
	e, err := readEntry(entryPath)
	_ = unlinkFile(entryPath)
	if err == nil {
		env.sweepSocket(id, e.Socket)
	}
	_ = unlinkFile(lockPath)
}

// sweepSocket unlinks a dead host's socket only when it is where a host puts
// one: an absolute, clean path named <id>.sock, in a directory that
// validates as a leaf under ancestors that validate (the path is canonical
// as Bind recorded it, so no component may be a symlink), and a socket.
func (env Env) sweepSocket(id, sock string) {
	if !filepath.IsAbs(sock) || filepath.Clean(sock) != sock || filepath.Base(sock) != id+sockSuffix {
		return
	}
	dir := filepath.Dir(sock)
	if env.checkAncestors(filepath.Dir(dir)) != nil || env.leaf(dir, false) != nil {
		return
	}
	fi, err := os.Lstat(sock)
	if err != nil || fi.Mode().Type() != fs.ModeSocket {
		return
	}
	_ = unlinkFile(sock)
}
