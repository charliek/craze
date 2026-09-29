package rundir

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The host logs (plan 030 §3.3, §3.4): <Home>/.cache/craze/host-logs, beside
// the registry and the session locks and validated as they are. A detached
// host (craze serve) writes its own diagnostics and its agent's stderr to
// <hostId>.log there, rotated once to <hostId>.log.1; the spawner records the
// process groups of a host's agents beside it in <hostId>.pgids. Every host
// sweeps the directory as it starts (SweepHostLogs), so it never outgrows a
// week of hosts.

// hostLogsName is the host logs' directory under the cache tree.
const hostLogsName = "host-logs"

// HostLogDir is the host logs' directory, <Home>/.cache/craze/host-logs,
// validated exactly as the rest of the cache tree is (cacheDir: every component
// walked from "/" by descriptor, never through a link; the craze tree's leaves
// the euid's own and 0700) and made 0700 where it is missing. It answers the
// directory's canonical path, which only this user can write in, so a file a
// host opens there by name is the host's own.
func HostLogDir(env Env) (string, error) {
	d, err := env.cacheDir(hostLogsName, true)
	if err != nil {
		return "", err
	}
	defer func() { _ = d.close() }()
	return d.path, nil
}

// SweepHostLogs removes from the host logs' directory everything a host that
// is gone left there and nobody has touched for maxAge: each regular file whose
// name is <hostId>.log, <hostId>.log.<n> or <hostId>.pgids — a name that is
// not a host id's is left alone — last modified before now less maxAge, and
// whose host is gone by its own host lock (hostGone): a host that has run,
// silent, for longer is still writing to it. keep is a name never removed
// whatever its age — the log the sweeping host is about to open and append to
// (plan 030 C2r) — or "".
//
// A host's death is decided by its lock alone, never by Hosts: Hosts leaves
// out a live host whose registry entry cannot be read or is malformed, and a
// sweep that took that for death would delete the logs of a host still writing
// them (astra r3-c2 2). So a file is removed only when its host's lock is
// missing or can be taken here, and kept whenever it is held or cannot be
// told (an open or a lock that fails).
//
// A directory that does not exist yet is nothing to sweep and is not created;
// one that fails validation, or a registry directory that does (so no host can
// be known to be gone), is an error and nothing is removed — a registry
// directory that does not exist yet holds no host's lock, and every host is
// gone. Every stat and unlink is relative to the held directory, and a symlink
// is never followed: it is not a regular file. It answers how many files it
// removed.
func SweepHostLogs(env Env, now time.Time, maxAge time.Duration, keep string) (int, error) {
	d, err := env.cacheDir(hostLogsName, false)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = d.close() }()
	names, err := d.names()
	if err != nil {
		return 0, err
	}
	hosts, err := env.cacheDir(hostsName, false)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		hosts = nil
	case err != nil:
		return 0, err
	default:
		defer func() { _ = hosts.close() }()
	}
	// The candidates first, by the host they belong to: only a host with a
	// file old enough to go has its lock probed at all.
	cutoff := now.Add(-maxAge)
	var owners []string
	old := map[string][]string{}
	for _, name := range names {
		id, ok := hostLogOwner(name)
		if !ok || name == keep {
			continue
		}
		st, err := d.lstat(name)
		if err != nil || uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if !time.Unix(st.Mtim.Unix()).Before(cutoff) {
			continue
		}
		if _, seen := old[id]; !seen {
			owners = append(owners, id)
		}
		old[id] = append(old[id], name)
	}
	removed := 0
	for _, id := range owners {
		release, gone := hostGone(hosts, id)
		if !gone {
			continue
		}
		for _, name := range old[id] {
			if d.unlink(name) == nil {
				removed++
			}
		}
		release()
	}
	return removed, nil
}

// hostGone reports whether the host id is gone, by its lock in the held
// registry directory hosts (nil: there is none yet) — the lock a live host
// holds from its Bind to its Close, and the one Hosts probes: missing, the host
// never bound, has exited (its Close unlinks the lock) or was swept; taken
// here, its holder is dead, and it is held — release lets it go — while the
// caller removes that host's files, as Hosts' own sweep holds it. Held by
// another, the host is live; an open or a flock that fails is a host that
// cannot be told, and is not gone either.
//
// A lock taken here is never one a host is about to bind: host ids are never
// reused, and only an id with a file old enough to sweep is probed.
func hostGone(hosts *dir, id string) (release func(), gone bool) {
	nothing := func() {}
	if hosts == nil {
		return nothing, true
	}
	lock, err := hosts.openFile(id+".lock", os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nothing, true
	}
	if err != nil {
		return nothing, false
	}
	taken, err := tryLock(lock)
	if err != nil || !taken {
		_ = lock.Close()
		return nothing, false
	}
	return func() {
		_ = unlock(lock)
		_ = lock.Close()
	}, true
}

// hostLogOwner is the host id a host-logs name belongs to — <id>.log,
// <id>.log.<n> or <id>.pgids — or false for any other name.
func hostLogOwner(name string) (string, bool) {
	id, rest, ok := strings.Cut(name, ".")
	if !ok || !ValidHostID(id) {
		return "", false
	}
	if rest == "log" || rest == "pgids" || strings.HasPrefix(rest, "log.") {
		return id, true
	}
	return "", false
}
