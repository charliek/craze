package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// OrphanAge is how long a file must have been left before SweepOrphans takes
// it for an orphan (plan 032 §3.9, P11): ten minutes. Under it, a lock may be
// in its host's create-to-flock window — made, not yet taken — and a socket
// between its host's bind and listen, refusing for an instant; past it,
// neither is a host on its way up.
const OrphanAge = 10 * time.Minute

// OrphanReport is what one SweepOrphans removed, for the hub's log.
type OrphanReport struct {
	// Locks is the host ids whose orphan lock, hosts/<id>.lock with no
	// entry, was removed.
	Locks []string
	// Temps is the registry temporaries removed, by their names in hosts/.
	Temps []string
	// Sockets is the refused host sockets removed, by path.
	Sockets []string
}

func (r OrphanReport) String() string {
	return fmt.Sprintf("%d orphan locks, %d registry temporaries and %d refused sockets removed",
		len(r.Locks), len(r.Temps), len(r.Sockets))
}

// SweepOrphans removes what hosts that died left where nothing else ever
// looks, SF-49's residuals (plan 032 §3.9, P11). The hub runs it at start and
// every ten minutes. now is the sweep's clock: every age is now less a file's
// time, so a test ages a file by sweeping later, not by waiting. Scope: env's
// HOME's registry, and the sockets in the runtime base env's namespace
// chooses (socketBase, which validates it and makes it and <ns> where
// missing) — every <ns> in that base, never another base.
//
//  1. Registry orphans. A host that dies between taking its lock and
//     writing its first entry leaves hosts/<id>.lock with no <id>.json, and
//     one that dies mid-write a temporary, .<id>.json.<n>; Hosts visits
//     neither (it walks entries). A lock with no entry, last modified more
//     than OrphanAge before now, is taken with Hosts' own authority: in the
//     registry directory walked and held, flocked without blocking (one held
//     is a live host's, left), still the file at its name (dir.sameFile),
//     and unlinked relative to the held directory, its entry checked absent
//     again under the lock. The age is a grace for a host between its lock's
//     open and its flock, which a sweeper holding the lock would make fail: a
//     host refreshes the time of the lock it opens. It is not what keeps a
//     host findable: a host stopped in that window past OrphanAge finds its
//     lock unlinked after its flock, and takes the name afresh (takeLock).
//     A temporary older than OrphanAge goes when its host's lock is missing
//     (a host takes its lock before it writes anything, and ids are never
//     reused) or old and taken that way; a lock under OrphanAge is never
//     taken, and its host's temporaries wait. A host with an entry is left to
//     Hosts, which sweeps a dead one whole.
//
//  2. Refused sockets. In each <base>/<ns> that validates as a leaf (not a
//     link; the euid's own; 0700) — one that does not is not looked into —
//     each <hostId>.sock (a host id's name: never hub.sock) that lstat finds
//     a socket, whose inode change time is more than OrphanAge before now (a
//     host between bind and listen refuses for an instant, so a young
//     refusing socket proves nothing) and whose hosts/<id>.lock is not held
//     — missing, or old and taken here, then let go at once — is cleared by
//     ClearStaleSocket: removed only when a connect is refused and it is
//     still the socket probed. The registry only ever skips a socket, never
//     authorises its removal (a copied registry cannot steer an unlink); the
//     evidence is the runtime tree's own. This reverses X30 for sockets,
//     which plan 027's sweep never removes.
//
//     Not on Darwin (sweepSockets): there a refused connect is no evidence
//     of death. XNU answers ECONNREFUSED both when nobody listens and when a
//     live listener cannot take another connection (uipc_usrreq.c's connect:
//     sonewconn fails on a full backlog), so a busy live host's socket —
//     another HOME's, sharing this base, whose lock this registry cannot
//     show held — would be unlinked. With no other runtime evidence, Darwin
//     sweeps registry orphans only, and a dead host's socket stays until its
//     directory is cleared, as before plan 032.
//
//  3. Nothing is signalled. A dead host's agents and its .pgids are left to
//     its spawner and the host-log sweep (SweepHostLogs).
//
// A registry that does not validate is an error, and then no socket is
// touched either, since no host's lock can be told; one that does not exist
// yet holds no host's lock. A runtime base that does not validate is an
// error. Each file's failure to go is joined into the error; the report is
// what went, whatever else failed.
func SweepOrphans(env Env, now time.Time) (OrphanReport, error) {
	var r OrphanReport
	cutoff := now.Add(-OrphanAge)
	var errs []error
	hosts, err := env.cacheDir(hostsName, false)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		hosts = nil
	case err != nil:
		return r, err
	default:
		defer func() { _ = hosts.close() }()
		errs = append(errs, sweepRegistryOrphans(hosts, cutoff, &r))
	}
	if sweepSockets {
		errs = append(errs, env.sweepRefusedSockets(hosts, cutoff, &r))
	}
	return r, errors.Join(errs...)
}

// sweepSockets is whether SweepOrphans sweeps refused sockets at all: not on
// Darwin, where a refusal does not prove a listener gone. A test sets it
// (never in parallel) to run the Darwin sweep on Linux.
var sweepSockets = runtime.GOOS != "darwin"

// orphanCandidate is one host id's files that may be orphans: its lock with
// no entry beside it (whose age is told once it is open), and its
// temporaries old enough to be.
type orphanCandidate struct {
	lock  bool
	temps []string
}

// sweepRegistryOrphans is SweepOrphans' first part, in the held registry
// directory hosts.
func sweepRegistryOrphans(hosts *dir, cutoff time.Time, r *OrphanReport) error {
	names, err := hosts.names()
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	cands := map[string]*orphanCandidate{}
	var ids []string
	candidate := func(id string) *orphanCandidate {
		c, ok := cands[id]
		if !ok {
			c = &orphanCandidate{}
			cands[id] = c
			ids = append(ids, id)
		}
		return c
	}
	for _, n := range names {
		if id, ok := strings.CutSuffix(n, ".lock"); ok && ValidHostID(id) {
			// Its age is decided on the file opened to be taken (sweepOrphan).
			if !present[id+".json"] {
				candidate(id).lock = true
			}
			continue
		}
		if id, ok := tempOwner(n); ok && oldRegular(hosts, n, cutoff) {
			c := candidate(id)
			c.temps = append(c.temps, n)
		}
	}
	slices.Sort(ids)
	var errs []error
	for _, id := range ids {
		errs = append(errs, sweepOrphan(hosts, id, cands[id], cutoff, r))
	}
	return errors.Join(errs...)
}

// sweepOrphan removes one host id's orphans c from the held registry
// directory hosts, under its lock when it has one.
func sweepOrphan(hosts *dir, id string, c *orphanCandidate, cutoff time.Time, r *OrphanReport) error {
	var errs []error
	removeTemps := func() {
		for _, n := range c.temps {
			if err := hosts.unlink(n); err != nil {
				errs = append(errs, err)
				continue
			}
			r.Temps = append(r.Temps, n)
		}
	}
	lockName := id + ".lock"
	lock, err := hosts.openFile(lockName, os.O_RDWR, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No lock, so no host: a host takes its lock before it writes
		// anything, and its id is never another's. Its temporaries are a
		// write that died.
		removeTemps()
		return errors.Join(errs...)
	case err != nil:
		return nil // cannot be told: left
	}
	defer lock.Close()
	// Never take a lock modified under OrphanAge ago: it may be a host's,
	// made and not yet flocked, and the host's own flock would fail on it.
	var st unix.Stat_t
	if err := unix.Fstat(int(lock.Fd()), &st); err != nil || !modTime(&st).Before(cutoff) {
		return nil
	}
	taken, err := tryLock(lock)
	if err != nil || !taken {
		return nil // a live host's, or cannot be told
	}
	defer func() { _ = unlock(lock) }() // before the Close deferred above
	// The lock taken must still be the one at the name: a sweeper that got
	// here first unlinked it.
	if !hosts.sameFile(lock, lockName) {
		return nil
	}
	removeTemps()
	if c.lock {
		// Under the lock, the entry is checked absent again: a host that
		// wrote one before it died is Hosts' to sweep, whole.
		if _, err := hosts.lstat(id + ".json"); errors.Is(err, unix.ENOENT) {
			if err := hosts.unlink(lockName); err != nil {
				errs = append(errs, err)
			} else {
				r.Locks = append(r.Locks, id)
				orphanUnlinked(id)
			}
		}
	}
	return errors.Join(errs...)
}

// orphanUnlinked runs in SweepOrphans once an orphan lock is unlinked and
// before it is let go, with the host id: nothing in production, and in a test
// (never in parallel) the pause in which a host's flock meets the sweep's.
var orphanUnlinked = func(string) {}

// tempOwner is the host id whose registry temporary n is — .<id>.json.<n>
// (tempName, isTempOf) — or false for any other name.
func tempOwner(n string) (string, bool) {
	rest, ok := strings.CutPrefix(n, ".")
	if !ok || len(rest) < hostIDLen {
		return "", false
	}
	id := rest[:hostIDLen]
	if !ValidHostID(id) || !isTempOf(n, id+".json") {
		return "", false
	}
	return id, true
}

// oldRegular reports whether name in d is a regular file last modified
// before cutoff (lstat-ed: a link is not one).
func oldRegular(d *dir, name string, cutoff time.Time) bool {
	st, err := d.lstat(name)
	return err == nil && uint32(st.Mode)&unix.S_IFMT == unix.S_IFREG && modTime(&st).Before(cutoff)
}

func modTime(st *unix.Stat_t) time.Time { return time.Unix(st.Mtim.Unix()) }

// changeTime is a stat's inode change time: when the file was made, or its
// inode last changed (a chmod, a link). Nothing sets it back, which is why
// the sweep's clock, not the file, is what a test moves.
func changeTime(st *unix.Stat_t) time.Time { return time.Unix(st.Ctim.Unix()) }

// sweepRefusedSockets is SweepOrphans' second part: the runtime base env's
// namespace chooses, each of its <ns> directories, each old host socket in
// one whose lock the registry directory hosts (nil: none exists) does not
// show held. The runtime tree is used by path, as bind(2) uses it. Residual
// (accepted; SD-04: the boundary is the same uid on this machine): a process
// of this uid swapping a <ns> directory between the stats and the probe can
// point the probe at another socket — and could unlink the socket itself.
func (env Env) sweepRefusedSockets(hosts *dir, cutoff time.Time, r *OrphanReport) error {
	ns, err := Namespace(env.CrazeDir)
	if err != nil {
		return err
	}
	base, err := env.socketBase(ns)
	if err != nil {
		return err
	}
	spaces, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range spaces {
		if !validNamespace(s.Name()) {
			continue
		}
		dirPath := filepath.Join(base, s.Name())
		if env.existingLeaf(dirPath) != nil {
			continue
		}
		socks, err := os.ReadDir(dirPath)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range socks {
			id, ok := strings.CutSuffix(f.Name(), sockSuffix)
			if !ok || !ValidHostID(id) {
				continue
			}
			path := filepath.Join(dirPath, f.Name())
			var st unix.Stat_t
			if err := unix.Lstat(path, &st); err != nil || !isSocketStat(&st) {
				continue
			}
			if !changeTime(&st).Before(cutoff) || lockHeld(hosts, id, cutoff) {
				continue
			}
			fate, err := clearRefused(path, idOfStat(&st))
			if err != nil {
				errs = append(errs, err)
			}
			if fate == SocketRemoved {
				r.Sockets = append(r.Sockets, path)
			}
		}
	}
	return errors.Join(errs...)
}

// lockHeld reports whether the sweep must leave host id's socket for its
// lock, in the held registry directory hosts (nil: there is none): true when
// the lock is held by another, or was modified under OrphanAge before cutoff
// (it is never taken then), or cannot be told; false when it is missing or
// is taken here — and let go at once: the lock only ever skips a socket.
func lockHeld(hosts *dir, id string, cutoff time.Time) bool {
	if hosts == nil {
		return false
	}
	lock, err := hosts.openFile(id+".lock", os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer lock.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(lock.Fd()), &st); err != nil || !modTime(&st).Before(cutoff) {
		return true
	}
	taken, err := tryLock(lock)
	if err != nil || !taken {
		return true
	}
	_ = unlock(lock)
	return false
}

// nsLen is a namespace's length: 8 lowercase hex digits (Namespace).
const nsLen = 8

// validNamespace reports whether name is a namespace's (Namespace): 8
// lowercase hex digits.
func validNamespace(name string) bool {
	if len(name) != nsLen {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// existingLeaf validates the runtime-tree directory p as a leaf without
// making it: lstat-ed (never followed), not a symlink, and the leaf rule
// (leafFault).
func (env Env) existingLeaf(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", p)
	}
	return env.leafFault(p, fi.IsDir(), int(statOf(fi).Uid), permOf(fi))
}
