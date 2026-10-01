package rundir

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A sweep's clock is its now: every age is now less a file's time. A file's
// modification time is set back with os.Chtimes; its inode change time
// cannot be, so a socket is made old by sweeping later — at swept(), past
// OrphanAge from the moment it is called — and every file a test made before
// then is old to that sweep unless the test sets its mtime to that now.

// swept is a sweep clock past OrphanAge from now.
func swept() time.Time { return time.Now().Add(OrphanAge + time.Minute) }

// sweepOrphans is SweepOrphans expected to succeed.
func sweepOrphans(t *testing.T, env Env, now time.Time) OrphanReport {
	t.Helper()
	r, err := SweepOrphans(env, now)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	return r
}

// setMtime sets p's modification (and access) time to at.
func setMtime(t *testing.T, p string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// orphanLock makes hosts/<id>.lock with no entry beside it — what a host
// leaves that is killed between taking its lock and writing its first
// entry, or one still in that window — modified at mtime; its path.
func orphanLock(t *testing.T, env Env, id string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(cacheSubdir(t, env, hostsName), id+".lock")
	writeFile(t, p, "")
	setMtime(t, p, mtime)
	return p
}

// holdLock flocks the file at p for the rest of the test, as a live host
// holds its lock.
func holdLock(t *testing.T, p string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if taken, err := tryLock(f); err != nil || !taken {
		t.Fatalf("setup: tryLock %s = %v, %v", p, taken, err)
	}
}

// nsDir is env's <base>/<ns>, validated and made as the hub makes it.
func nsDir(t *testing.T, env Env) string {
	t.Helper()
	sock, err := HubSocket(env)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(sock)
}

// pausedSocket binds a socket at p and never listens on it, for the rest of
// the test: a host paused between bind and listen, whose socket refuses.
func pausedSocket(t *testing.T, p string) {
	t.Helper()
	fd := rawSocket(t)
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: p}); err != nil {
		t.Fatal(err)
	}
	if _, err := net.Dial("unix", p); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("setup: a connect to a socket bound and not listening at %s: %v, want ECONNREFUSED", p, err)
	}
}

// sweptSockets is the sockets a sweep that finds want old and refusing
// removes on this OS: want, and none on Darwin, which sweeps no socket
// (sweepSockets).
func sweptSockets(want ...string) []string {
	if !sweepSockets {
		return nil
	}
	return want
}

// checkSocketsSwept checks that each of paths is gone when the sweep removes
// sockets on this OS, and still a socket when it does not (Darwin).
func checkSocketsSwept(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if gone := !exists(t, p); gone != sweepSockets {
			t.Errorf("after the sweep %s is gone: %v; want %v on %s", p, gone, sweepSockets, runtime.GOOS)
		}
	}
}

// A16's positive case: a host killed mid-bind — its lock taken and its
// socket bound, then killed before its first registry entry — and a
// temporary of a write that died leave a lock with no entry, a temporary and
// a refusing socket. Ten minutes on, one sweep removes all three, and says
// so (on Darwin, which sweeps no socket, the first two).
func TestTheSweepRemovesAHostKilledMidBind(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	h.die()
	if err := os.Remove(filepath.Join(hostsDir(env), h.ID()+".json")); err != nil {
		t.Fatal(err)
	}
	tmp := "." + h.ID() + ".json.42"
	writeFile(t, filepath.Join(hostsDir(env), tmp), "{")
	gone := NewHostID() // a temporary whose host's lock is already gone
	goneTmp := "." + gone + ".json.7"
	writeFile(t, filepath.Join(hostsDir(env), goneTmp), "{")

	r := sweepOrphans(t, env, swept())
	for _, p := range []string{
		filepath.Join(hostsDir(env), h.ID()+".lock"),
		filepath.Join(hostsDir(env), tmp),
		filepath.Join(hostsDir(env), goneTmp),
	} {
		if exists(t, p) {
			t.Errorf("the sweep left %s", p)
		}
	}
	checkSocketsSwept(t, h.Socket())
	wantTemps := []string{tmp, goneTmp}
	slices.Sort(wantTemps)
	slices.Sort(r.Temps)
	if !slices.Equal(r.Locks, []string{h.ID()}) || !slices.Equal(r.Temps, wantTemps) ||
		!slices.Equal(r.Sockets, sweptSockets(h.Socket())) {
		t.Fatalf("the report: %#v; want lock %s, temporaries %v, sockets %v", r, h.ID(), wantTemps, sweptSockets(h.Socket()))
	}
	want := fmt.Sprintf("1 orphan locks, 2 registry temporaries and %d refused sockets removed", len(sweptSockets(h.Socket())))
	if r.String() != want {
		t.Fatalf("the report reads %q, want %q", r.String(), want)
	}
}

// TestDarwinSweepsNoSocket is not parallel: it sets sweepSockets, which every
// SweepOrphans reads, to what Darwin has. There a live listener with a full
// backlog refuses a connect as a dead one does, so the sweep removes no
// socket — an old refusing one stays — while it still removes a registry
// orphan.
func TestDarwinSweepsNoSocket(t *testing.T) {
	was := sweepSockets
	sweepSockets = false
	t.Cleanup(func() { sweepSockets = was })
	env := testEnv(t)
	p := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	listenAt(t, p)
	id := NewHostID()
	lock := orphanLock(t, env, id, time.Now())
	r := sweepOrphans(t, env, swept())
	if !isSocket(t, p) || len(r.Sockets) != 0 {
		t.Fatalf("the Darwin sweep removed a socket (report %#v)", r)
	}
	if exists(t, lock) || !slices.Equal(r.Locks, []string{id}) {
		t.Fatalf("the Darwin sweep left the orphan lock %s (report %#v)", lock, r)
	}
}

// A lock with no entry modified under OrphanAge ago may be a host's between
// its create and its flock: it is never taken, so neither it nor its host's
// old temporaries go.
func TestTheSweepLeavesALockInItsCreateToFlockWindow(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	now := time.Now()
	id := NewHostID()
	lock := orphanLock(t, env, id, now)
	tmp := filepath.Join(hostsDir(env), "."+id+".json.1")
	writeFile(t, tmp, "{")
	setMtime(t, tmp, now.Add(-time.Hour))
	r := sweepOrphans(t, env, now)
	if !exists(t, lock) || !exists(t, tmp) {
		t.Fatalf("the sweep removed a fresh lock (%v) or its host's temporary (%v)", !exists(t, lock), !exists(t, tmp))
	}
	if len(r.Locks)+len(r.Temps)+len(r.Sockets) != 0 {
		t.Fatalf("the report: %#v; want nothing removed", r)
	}
}

// An old lock with no entry that another process holds — a host paused past
// OrphanAge before its first entry, or one whose entry was removed — is a
// live host's, and stays.
func TestTheSweepLeavesAHeldOrphanLock(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	now := time.Now()
	lock := orphanLock(t, env, NewHostID(), now.Add(-time.Hour))
	holdLock(t, lock)
	if r := sweepOrphans(t, env, now); !exists(t, lock) || len(r.Locks) != 0 {
		t.Fatalf("the sweep removed a held lock (report %#v)", r)
	}
}

// Temporaries: an old one goes when its host's lock is missing or old and
// taken; one whose lock is held, one younger than OrphanAge and a name that
// only looks like one stay; a dead host with an entry keeps its entry and
// lock for Hosts, which sweeps it whole.
func TestTheSweepsTemporaries(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	now := time.Now()
	old := now.Add(-time.Hour)
	hosts := cacheSubdir(t, env, hostsName)
	temp := func(id, suffix string, mtime time.Time) string {
		n := "." + id + suffix
		writeFile(t, filepath.Join(hosts, n), "{")
		setMtime(t, filepath.Join(hosts, n), mtime)
		return n
	}
	noLock, takeable, held, withEntry := NewHostID(), NewHostID(), NewHostID(), NewHostID()
	orphanLock(t, env, takeable, old)
	holdLock(t, orphanLock(t, env, held, old))
	orphanLock(t, env, withEntry, old)
	writeFile(t, filepath.Join(hosts, withEntry+".json"), "{}")
	gone := []string{
		temp(noLock, ".json.1", old),
		temp(takeable, ".json.2", old),
		temp(withEntry, ".json.3", old),
	}
	kept := []string{
		temp(held, ".json.4", old),
		temp(NewHostID(), ".json.5", now.Add(-OrphanAge+time.Minute)),
		temp(noLock, ".json.x1", old),
		temp(noLock, ".lock.1", old),
		temp(noLock, ".json.", old),
		withEntry + ".json",
		withEntry + ".lock",
		held + ".lock",
	}
	r := sweepOrphans(t, env, now)
	for _, n := range gone {
		if exists(t, filepath.Join(hosts, n)) {
			t.Errorf("the sweep left %s", n)
		}
	}
	for _, n := range kept {
		if !exists(t, filepath.Join(hosts, n)) {
			t.Errorf("the sweep removed %s", n)
		}
	}
	slices.Sort(gone)
	slices.Sort(r.Temps)
	if !slices.Equal(r.Temps, gone) || !slices.Equal(r.Locks, []string{takeable}) {
		t.Fatalf("the report: %#v; want temporaries %v and lock %s", r, gone, takeable)
	}
}

// A host paused between bind and listen refuses connections: a socket
// younger than OrphanAge proves nothing, and stays — here with no lock at
// all, so only its age keeps it.
func TestTheSweepLeavesAFreshRefusingSocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	p := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	pausedSocket(t, p)
	if r := sweepOrphans(t, env, time.Now()); !isSocket(t, p) || len(r.Sockets) != 0 {
		t.Fatalf("the sweep removed a socket younger than OrphanAge (report %#v)", r)
	}
}

// An old refusing socket whose host's lock is held — a host between bind and
// listen, or one whose listener has gone, while it lives — is skipped for
// its lock, without a probe deciding anything.
func TestTheSweepLeavesAnOldSocketWhoseLockIsHeld(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	id := NewHostID()
	p := filepath.Join(nsDir(t, env), id+sockSuffix)
	pausedSocket(t, p)
	holdLock(t, orphanLock(t, env, id, time.Now()))
	if r := sweepOrphans(t, env, swept()); !isSocket(t, p) || len(r.Sockets) != 0 {
		t.Fatalf("the sweep removed a socket whose host's lock is held (report %#v)", r)
	}
}

// A live host is untouched by a sweep ten minutes on: its lock, its entry,
// its socket — still accepting — and its agents' record. A dead host's agent
// record stays too: the sweep signals nothing and reads no .pgids; the dead
// host's entry and lock are Hosts' to sweep; its old refusing socket goes.
func TestTheSweepLeavesALiveHost(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	live := bind(t, env)
	dead := bind(t, env)
	dead.die()
	logs, err := HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []*Host{live, dead} {
		writeFile(t, filepath.Join(logs, h.ID()+".pgids"), "123 456\n")
	}
	r := sweepOrphans(t, env, swept())
	for _, p := range []string{
		filepath.Join(hostsDir(env), live.ID()+".lock"),
		filepath.Join(hostsDir(env), live.ID()+".json"),
		filepath.Join(logs, live.ID()+".pgids"),
		filepath.Join(logs, dead.ID()+".pgids"),
		filepath.Join(hostsDir(env), dead.ID()+".lock"),
		filepath.Join(hostsDir(env), dead.ID()+".json"),
	} {
		if !exists(t, p) {
			t.Errorf("the sweep removed %s", p)
		}
	}
	conn, err := net.Dial("unix", live.Socket())
	if err != nil {
		t.Fatalf("the live host's socket no longer accepts: %v", err)
	}
	_ = conn.Close()
	checkSocketsSwept(t, dead.Socket())
	if !slices.Equal(r.Sockets, sweptSockets(dead.Socket())) || len(r.Locks) != 0 {
		t.Fatalf("the report: %#v; want only the dead host's socket %v removed", r, sweptSockets(dead.Socket()))
	}
}

// An old socket whose lock is not held — another HOME's host, a host whose
// registry was copied over or removed — is probed, and a live one stays:
// one that accepts, and on Linux one whose backlog is full (EAGAIN).
func TestTheSweepLeavesALiveSocketItHasNoLockFor(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	accepting := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	listening(t, accepting)
	kept := []string{accepting}
	if runtime.GOOS == "linux" {
		full := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
		fullBacklog(t, full)
		kept = append(kept, full)
	}
	r := sweepOrphans(t, env, swept())
	for _, p := range kept {
		if !isSocket(t, p) {
			t.Errorf("the sweep removed the live socket %s", p)
		}
	}
	if len(r.Sockets) != 0 {
		t.Fatalf("the report: %#v; want nothing removed", r)
	}
}

// Only a socket goes: a regular file, a directory and a symlink at a host
// socket's name stay (the symlink's target too), whatever their age.
func TestTheSweepLeavesWhatIsNotASocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	dir := nsDir(t, env)
	file := filepath.Join(dir, NewHostID()+sockSuffix)
	writeFile(t, file, "x")
	sub := filepath.Join(dir, NewHostID()+sockSuffix)
	mkdir(t, sub, 0o700)
	link := filepath.Join(dir, NewHostID()+sockSuffix)
	target := filepath.Join(shortDir(t), "t.sock")
	listenAt(t, target)
	symlink(t, target, link)
	r, err := SweepOrphans(env, swept())
	for _, p := range []string{file, sub, link, target} {
		if !exists(t, p) {
			t.Errorf("the sweep removed %s", p)
		}
	}
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if len(r.Sockets) != 0 {
		t.Fatalf("the report: %#v; want nothing removed", r)
	}
}

// Only a host's socket name is the sweep's: hub.sock, and any other name,
// stays, refusing and old.
func TestTheSweepLeavesHubSockAndOtherNames(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	dir := nsDir(t, env)
	var kept []string
	for _, n := range []string{"hub.sock", "stray.sock", "0123456789AB.sock", "0123456789ab.sock.1", "0123456789ab"} {
		p := filepath.Join(dir, n)
		listenAt(t, p)
		kept = append(kept, p)
	}
	r := sweepOrphans(t, env, swept())
	for _, p := range kept {
		if !isSocket(t, p) {
			t.Errorf("the sweep removed %s", p)
		}
	}
	if len(r.Sockets) != 0 {
		t.Fatalf("the report: %#v; want nothing removed", r)
	}
}

// TestTheSweepLeavesASocketReplacedAfterItsProbe is not parallel: it
// replaces socketProbed, which every probe that finds a socket refusing
// calls. The socket probed is replaced at its path before the second lstat;
// the replacement is not the socket probed, and stays.
func TestTheSweepLeavesASocketReplacedAfterItsProbe(t *testing.T) {
	if !sweepSockets {
		t.Skip("the sweep probes no socket on " + runtime.GOOS + " (sweepSockets)")
	}
	env := testEnv(t)
	p := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	listenAt(t, p)
	var replacement FileID
	socketProbed = func(probed string) {
		if probed == p {
			replacement = replaceSocket(t, p)
		}
	}
	t.Cleanup(func() { socketProbed = func(string) {} })
	r := sweepOrphans(t, env, swept())
	if replacement == (FileID{}) {
		t.Fatal("the socket was never probed")
	}
	if id, err := SocketID(p); err != nil || id != replacement {
		t.Fatalf("after the sweep %s is %+v (%v); want the replacement %+v left", p, id, err, replacement)
	}
	if len(r.Sockets) != 0 {
		t.Fatalf("the report: %#v; want nothing removed", r)
	}
}

// Every <ns> directory in the base is swept, and only one that validates as
// a leaf: a symlinked one and a 0755 one are not looked into, nor is a
// directory whose name is no namespace's.
func TestTheSweepOnlyLooksIntoValidatedNamespaces(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	base := filepath.Dir(nsDir(t, env))
	stale := func(dir string) string {
		p := filepath.Join(dir, NewHostID()+sockSuffix)
		listenAt(t, p)
		return p
	}
	other := filepath.Join(base, "0badc0de")
	mkdir(t, other, 0o700)
	gone := stale(other)
	open := filepath.Join(base, "0000beef")
	mkdir(t, open, 0o755)
	elsewhere := shortDir(t)
	symlink(t, elsewhere, filepath.Join(base, "feedf00d"))
	named := filepath.Join(base, "not-a-ns")
	mkdir(t, named, 0o700)
	kept := []string{stale(open), stale(elsewhere), stale(named)}
	r := sweepOrphans(t, env, swept())
	for _, p := range kept {
		if !isSocket(t, p) {
			t.Errorf("the sweep removed %s", p)
		}
	}
	checkSocketsSwept(t, gone)
	if !slices.Equal(r.Sockets, sweptSockets(gone)) {
		t.Fatalf("the report: %#v; want %s removed from another valid namespace", r, gone)
	}
}

// With no registry yet, no host's lock can be held: an old refusing socket
// goes, and the cache tree is not built.
func TestTheSweepWithNoRegistry(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	p := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	listenAt(t, p)
	r := sweepOrphans(t, env, swept())
	checkSocketsSwept(t, p)
	if !slices.Equal(r.Sockets, sweptSockets(p)) {
		t.Fatalf("the report: %#v; want %s removed", r, p)
	}
	if exists(t, filepath.Join(env.Home, cacheName)) {
		t.Fatal("the sweep built the cache tree")
	}
}

// A registry that does not validate is an error, and no socket is touched:
// without it, no host's lock can be told held.
func TestTheSweepRefusesAHostileRegistryAndTouchesNoSocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	bind(t, env)
	p := filepath.Join(nsDir(t, env), NewHostID()+sockSuffix)
	listenAt(t, p)
	chmod(t, hostsDir(env), 0o755)
	if _, err := SweepOrphans(env, swept()); err == nil {
		t.Fatal("SweepOrphans read a registry directory of mode 0755")
	}
	if !isSocket(t, p) {
		t.Fatal("a sweep with no valid registry removed a socket")
	}
}
