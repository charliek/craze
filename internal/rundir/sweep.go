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
	live, _, err := HostsScan(env)
	return live, err
}

// HostsScan is Hosts for a reader that must know its scan missed no live host
// (plan 032 §3.10: a hub proving that no live host carries a create's request
// before it spawns one): the live hosts Hosts lists, and beside them the id of
// every host the scan cannot vouch for — alive, or perhaps alive (its lock
// held by another, or a lock it could neither open nor try), and its entry
// unreadable or naming another host. Hosts skips those. An entry whose lock is
// free is a dead host's, swept as Hosts sweeps it; one whose lock is missing is
// mid-exit; neither is either list's. A registry that cannot be read is an
// error, as for Hosts.
func HostsScan(env Env) ([]Entry, []string, error) {
	hosts, err := env.cacheDir(hostsName, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = hosts.close() }()
	names, err := hosts.names()
	if err != nil {
		return nil, nil, err
	}
	var live []Entry
	var unreadable []string
	for _, name := range names {
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !ValidHostID(id) {
			continue
		}
		switch e, st := probe(hosts, id); st {
		case probeLive:
			live = append(live, e)
		case probeUnreadable:
			unreadable = append(unreadable, id)
		}
	}
	return live, unreadable, nil
}

// probeState is what one registry entry's check came to.
type probeState int

const (
	// probeGone: not listed — stale and swept, or mid-exit (no lock).
	probeGone probeState = iota
	// probeLive: its host is alive and its entry read.
	probeLive
	// probeUnreadable: its host is alive (or its lock could not be tried)
	// and its entry cannot be read, or names another host.
	probeUnreadable
)

// probe is one registry entry's check, in the held registry directory: its
// live Entry, or why it is not listed (probeState).
func probe(hosts *dir, id string) (Entry, probeState) {
	lockName := id + ".lock"
	lock, err := hosts.openFile(lockName, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, probeGone
	}
	if err != nil {
		return Entry{}, probeUnreadable
	}
	defer lock.Close()
	taken, err := tryLock(lock)
	if err != nil {
		return Entry{}, probeUnreadable
	}
	if !taken {
		e, err := readEntry(hosts, id+".json")
		if err != nil || e.HostID != id {
			return Entry{}, probeUnreadable
		}
		return e, probeLive
	}
	defer func() { _ = unlock(lock) }() // before the Close deferred above
	// The lock taken must still be the one at the name: a sweeper that got
	// here first unlinked it, and this descriptor holds a lock on a file no
	// longer anyone's.
	if hosts.sameFile(lock, lockName) {
		sweep(hosts, id)
	}
	return Entry{}, probeGone
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
// /tmp is emptied at boot or by systemd-tmpfiles), or until the hub's sweep
// finds it old and refusing (SweepOrphans: the runtime tree's own evidence,
// never this lock's); a host's own Close removes its socket,
// identity-checked.
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
