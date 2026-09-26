package rundir

import (
	"errors"
	"io/fs"
	"os"
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
//     is none — the sweep removes the entry, the host's temporaries and the
//     lock file itself, then LOCK_UN (sweep). Host ids are never reused, so a
//     dead host's lock file can go. The sweep never removes a socket.
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
		if e, ok := probe(hosts, id); ok {
			live = append(live, e)
		}
	}
	return live, nil
}

// probe is one registry entry's check, in the held registry directory: its
// live Entry, or false when it is not listed (stale and swept, mid-exit, or
// unreadable).
func probe(hosts *dir, id string) (Entry, bool) {
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
		sweep(hosts, id)
	}
	return Entry{}, false
}

// sweep removes a dead host's files from the held registry directory, whose
// <id>.lock the caller holds: the entry, the entry's temporaries (isTempOf:
// writes that died before their rename; listed here, under the lock, when
// the host can make no more) and, last, the lock file. Each is unlinked
// relative to the held directory — the directory whose lock was taken.
//
// It never removes a socket. The lock is authority over this registry's own
// entry and lock, not over a file in the runtime tree an entry names: a copy
// of the registry the victim owns (under another name, its <id>.lock a copy —
// another inode, which no live host holds) renamed into ~/.cache/craze by a
// writer of ~/.cache hands a sweep a lock it can take while the host its
// copied entry names is alive, and that host's socket must survive it. A dead
// host's socket stays in its runtime directory, under a name never reused,
// until that directory is cleared (/run/user is a tmpfs emptied at logout,
// /tmp is emptied at boot or by systemd-tmpfiles); a host's own Close removes
// its socket, identity-checked.
func sweep(hosts *dir, id string) {
	entry := id + ".json"
	_ = hosts.unlink(entry)
	if names, err := hosts.names(); err == nil {
		for _, n := range names {
			if isTempOf(n, entry) {
				_ = hosts.unlink(n)
			}
		}
	}
	_ = hosts.unlink(id + ".lock")
}
