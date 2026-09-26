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
// The registry directory is walked and held (cacheDir), its entries read from
// that descriptor, and every open, read and unlink below is relative to it.
// For each hosts/<id>.json (a name that is not a host id is ignored) it opens
// hosts/<id>.lock without creating it:
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
	hosts, err := env.cacheDir(hostsName, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = hosts.close() }()
	names, err := hosts.names()
	if err != nil {
		return nil, err
	}
	var live []Entry
	for _, name := range names {
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !ValidHostID(id) {
			continue
		}
		if e, ok := env.probe(hosts, id); ok {
			live = append(live, e)
		}
	}
	return live, nil
}

// probe is one registry entry's check, in the held registry directory: its
// live Entry, or false when it is not listed (stale and swept, mid-exit, or
// unreadable).
func (env Env) probe(hosts *dir, id string) (Entry, bool) {
	lockName := id + ".lock"
	lock, err := hosts.openFile(lockName, os.O_RDWR, 0)
	if err != nil {
		return Entry{}, false
	}
	defer lock.Close()
	taken, err := tryLock(lock)
	if err != nil {
		return Entry{}, false
	}
	if !taken {
		e, err := readEntry(hosts, id+".json")
		if err != nil || e.HostID != id {
			return Entry{}, false
		}
		return e, true
	}
	defer func() { _ = unlock(lock) }() // before the Close deferred above
	// The lock taken must still be the one at the name: a sweeper that got
	// here first unlinked it, and this descriptor holds a lock on a file no
	// longer anyone's.
	if hosts.sameFile(lock, lockName) {
		env.sweep(hosts, id)
	}
	return Entry{}, false
}

// sweep removes a dead host's files from the held registry directory; its
// lock is held by the caller.
func (env Env) sweep(hosts *dir, id string) {
	e, err := readEntry(hosts, id+".json")
	_ = hosts.unlink(id + ".json")
	if err == nil {
		env.sweepSocket(id, e.Socket)
	}
	_ = hosts.unlink(id + ".lock")
}

// sweepSocket unlinks a dead host's socket only when it is where a host puts
// one: an absolute, clean path named <id>.sock, in a directory that
// validates as a leaf under ancestors that validate — by path and the strict
// rule, as the runtime tree is (the path is canonical as Bind recorded it, so
// no component may be a symlink) — and a socket.
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
