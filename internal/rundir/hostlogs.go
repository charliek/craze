package rundir

import (
	"errors"
	"io/fs"
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
// whose host is not live (Hosts: a host that has run, silent, for longer is
// still writing to it). A directory that does not exist yet is nothing to
// sweep and is not created; one that fails validation, or a registry that
// cannot be read (so no host can be known not to be live), is an error and
// nothing is removed. Every stat and unlink is relative to the held
// directory, and a symlink is never followed: it is not a regular file. It
// answers how many files it removed.
func SweepHostLogs(env Env, now time.Time, maxAge time.Duration) (int, error) {
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
	live := map[string]bool{}
	entries, err := Hosts(env)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		live[e.HostID] = true
	}
	cutoff := now.Add(-maxAge)
	removed := 0
	for _, name := range names {
		id, ok := hostLogOwner(name)
		if !ok || live[id] {
			continue
		}
		st, err := d.lstat(name)
		if err != nil || uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if !time.Unix(st.Mtim.Unix()).Before(cutoff) {
			continue
		}
		if d.unlink(name) == nil {
			removed++
		}
	}
	return removed, nil
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
