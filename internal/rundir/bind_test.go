package rundir

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/protocol"
)

func TestBindServesA0600SocketInA0700Namespace(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	started := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	h, err := Bind(env, "0123456789ab", Entry{
		StartedAt: started, CrazeSessionID: "s-1", Provider: "grok", Workspace: "/w",
		// Bind's own members: overwritten.
		Protocol: 99, HostID: "ffffffffffff", PID: 1, Socket: "/elsewhere",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	fi, err := os.Lstat(h.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || permOf(fi) != 0o600 {
		t.Fatalf("%s: type %v mode %04o, want a 0600 socket", h.Socket(), fi.Mode().Type(), permOf(fi))
	}
	if got := perm(t, filepath.Dir(h.Socket())); got != 0o700 {
		t.Fatalf("the namespace directory has mode %04o, want 0700", got)
	}
	if filepath.Base(h.Socket()) != "0123456789ab.sock" || h.ID() != "0123456789ab" {
		t.Fatalf("socket %s, id %s", h.Socket(), h.ID())
	}

	lock, err := os.ReadFile(filepath.Join(hostsDir(env), "0123456789ab.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(os.Getpid()) + " 0123456789ab\n"; string(lock) != want {
		t.Fatalf("the host lock holds %q, want %q", lock, want)
	}

	path := filepath.Join(hostsDir(env), "0123456789ab.json")
	if got := perm(t, path); got != 0o600 {
		t.Fatalf("the registry entry has mode %04o, want 0600", got)
	}
	var e Entry
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	want := Entry{
		Protocol: protocol.ProtocolVersion, HostID: "0123456789ab", PID: os.Getpid(), StartedAt: started,
		Socket: h.Socket(), CrazeSessionID: "s-1", Provider: "grok", Workspace: "/w",
	}
	if e != want || h.Entry() != want {
		t.Fatalf("registry entry %+v (Entry() %+v), want %+v", e, h.Entry(), want)
	}

	conn, err := net.Dial("unix", h.Socket())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestTheRegistryEntryHasExactlyItsMembers(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Entry{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"protocol", "hostId", "pid", "startedAt", "socket", "crazeSessionId",
		"providerSessionId", "incarnation", "provider", "workspace", "ready"}
	if len(m) != len(want) {
		t.Fatalf("the entry has %d members, want %d: %v", len(m), len(want), m)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("the entry has no %q", k)
		}
	}
}

// lockHeldElsewhere reports whether the lock file at p is held by another
// open file description: a fresh open of it cannot be flocked.
func lockHeldElsewhere(t *testing.T, p string) bool {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	taken, err := tryLock(f)
	if err != nil {
		t.Fatal(err)
	}
	if taken {
		_ = unlock(f)
	}
	return !taken
}

// TestBindTakesAFreshLockWhenASweepUnlinksItsOwn is not parallel: it
// replaces hostLockOpened, which every Bind calls. The hub's sweep runs in
// the window between the host's open of its lock and its flock, ten minutes
// on: the lock has no entry yet and nobody holds it, so the sweep takes it
// for an orphan and unlinks it. The host's flock then takes an inode no
// reader can find; it sees that, and takes the name afresh — so it is bound
// under a lock at its name, which it holds and Hosts finds, and which a
// second sweep leaves.
func TestBindTakesAFreshLockWhenASweepUnlinksItsOwn(t *testing.T) {
	env := testEnv(t)
	id := NewHostID()
	opens := 0
	var swept OrphanReport
	hostLockOpened = func(string) {
		opens++
		if opens == 1 {
			r, err := SweepOrphans(env, time.Now().Add(OrphanAge+time.Minute))
			if err != nil {
				t.Errorf("the sweep in the window: %v", err)
			}
			swept = r
		}
	}
	t.Cleanup(func() { hostLockOpened = func(string) {} })
	h, err := Bind(env, id, Entry{Workspace: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if !slices.Equal(swept.Locks, []string{id}) {
		t.Fatalf("setup: the sweep in the window removed %v; want the host's lock %s", swept.Locks, id)
	}
	lock := filepath.Join(hostsDir(env), id+".lock")
	if !exists(t, lock) || !lockHeldElsewhere(t, lock) {
		t.Fatalf("the host was bound under a lock that is not at %s, held", lock)
	}
	if opens != 2 {
		t.Fatalf("the lock was opened %d times; want once more after the sweep", opens)
	}
	if got, err := Hosts(env); err != nil || !slices.Equal(entries(got), []string{id}) {
		t.Fatalf("Hosts = %v, %v; want the host", entries(got), err)
	}
	if r := sweepOrphansAt(t, env, time.Now().Add(OrphanAge+time.Minute)); len(r.Locks) != 0 || !exists(t, lock) {
		t.Fatalf("a second sweep removed the bound host's lock (report %#v)", r)
	}
}

// sweepOrphansAt is SweepOrphans expected to succeed.
func sweepOrphansAt(t *testing.T, env Env, now time.Time) OrphanReport {
	t.Helper()
	r, err := SweepOrphans(env, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestBindGivesUpOnALockUnlinkedOnEveryTry is not parallel: it replaces
// hostLockOpened. A lock unlinked between its open and its flock on every
// try is never taken: Bind fails, bounded, and leaves no socket or entry.
func TestBindGivesUpOnALockUnlinkedOnEveryTry(t *testing.T) {
	env := testEnv(t)
	opens := 0
	hostLockOpened = func(p string) {
		opens++
		_ = os.Remove(p)
	}
	t.Cleanup(func() { hostLockOpened = func(string) {} })
	id := NewHostID()
	if h, err := Bind(env, id, Entry{}); err == nil {
		_ = h.Close()
		t.Fatal("Bind took a lock unlinked under it on every try")
	} else if !strings.Contains(err.Error(), "unlinked or replaced") {
		t.Fatalf("Bind = %v; want the lost lock named", err)
	}
	if opens != lockTries {
		t.Fatalf("the lock was opened %d times, want %d", opens, lockTries)
	}
	if exists(t, filepath.Join(hostsDir(env), id+".json")) {
		t.Fatal("a Bind with no lock wrote an entry")
	}
}

// TestBindRetriesPastASweepHoldingItsUnlinkedLock is not parallel: it
// replaces hostLockOpened and orphanUnlinked. A host reopens an old lock left
// at its id; a sweep that read the lock's time before the host refreshed it
// takes it, unlinks it, and is paused before it lets it go. The host's flock
// is refused — but on a file no longer at its name, so a sweep's, not
// another host's: the host opens the name again, makes the lock afresh and is
// bound under it.
func TestBindRetriesPastASweepHoldingItsUnlinkedLock(t *testing.T) {
	env := testEnv(t)
	id := NewHostID()
	orphanLock(t, env, id, time.Now().Add(-time.Hour))
	paused, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	orphanUnlinked = func(string) { close(paused); <-resume }
	swept := make(chan OrphanReport, 1)
	opens := 0
	hostLockOpened = func(string) {
		opens++
		if opens != 1 {
			return
		}
		go func() {
			r, err := SweepOrphans(env, time.Now().Add(OrphanAge+time.Minute))
			if err != nil {
				t.Errorf("the sweep: %v", err)
			}
			swept <- r
		}()
		select {
		case <-paused:
		case <-time.After(10 * time.Second):
			t.Error("setup: the sweep never took and unlinked the lock")
		}
	}
	t.Cleanup(func() {
		hostLockOpened = func(string) {}
		orphanUnlinked = func(string) {}
	})
	h, err := Bind(env, id, Entry{Workspace: "/w"})
	release.Do(func() { close(resume) })
	r := <-swept
	if err != nil {
		t.Fatalf("Bind while a sweep held its unlinked lock: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if !slices.Equal(r.Locks, []string{id}) {
		t.Fatalf("setup: the sweep removed %v; want the old lock %s", r.Locks, id)
	}
	lock := filepath.Join(hostsDir(env), id+".lock")
	if !exists(t, lock) || !lockHeldElsewhere(t, lock) || opens != 2 {
		t.Fatalf("the host is not bound under a lock at %s, held (opens %d, want 2)", lock, opens)
	}
}

// TestBindRetriesWhenASweepUnlinksItsLockBetweenItsOpens is not parallel: it
// replaces lockFound. A host's exclusive create finds an old lock left at its
// id; before the open that follows, a sweep takes the lock and unlinks it, so
// that open finds nothing. The host opens the name again — the lock made
// afresh — and is bound under it.
func TestBindRetriesWhenASweepUnlinksItsLockBetweenItsOpens(t *testing.T) {
	env := testEnv(t)
	id := NewHostID()
	orphanLock(t, env, id, time.Now().Add(-time.Hour))
	var swept OrphanReport
	found := 0
	lockFound = func(string) {
		found++
		if found == 1 {
			r, err := SweepOrphans(env, time.Now())
			if err != nil {
				t.Errorf("the sweep: %v", err)
			}
			swept = r
		}
	}
	t.Cleanup(func() { lockFound = func(string) {} })
	h, err := Bind(env, id, Entry{Workspace: "/w"})
	if err != nil {
		t.Fatalf("Bind after its lock was unlinked between its opens: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if !slices.Equal(swept.Locks, []string{id}) {
		t.Fatalf("setup: the sweep removed %v; want the old lock %s", swept.Locks, id)
	}
	lock := filepath.Join(hostsDir(env), id+".lock")
	if !exists(t, lock) || !lockHeldElsewhere(t, lock) || found != 1 {
		t.Fatalf("the host is not bound under a lock made afresh at %s (found %d, want 1)", lock, found)
	}
}

// A host's lock another process holds — still the file at its name — is
// another holder of this host's id, never a sweep: Bind refuses at once,
// takes nothing and writes nothing.
func TestBindRefusesAHostLockAnotherHolds(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	id := NewHostID()
	lock := orphanLock(t, env, id, time.Now())
	holdLock(t, lock)
	h, err := Bind(env, id, Entry{})
	if err == nil {
		_ = h.Close()
		t.Fatal("Bind took a host lock another process holds")
	}
	if !strings.Contains(err.Error(), "another process holds it") {
		t.Fatalf("Bind = %v; want the holder named", err)
	}
	if !exists(t, lock) || exists(t, filepath.Join(hostsDir(env), id+".json")) {
		t.Fatal("a refused Bind removed the held lock or wrote an entry")
	}
}

// TestBindRefreshesAReopenedLocksTime is not parallel: it replaces
// hostLockOpened. A host that reopens a lock file left at its id — an old
// one, at a reused --host-id — refreshes its modification time as it opens
// it, before its flock: in that window the sweep's age gate sees a lock as
// young as a new one.
func TestBindRefreshesAReopenedLocksTime(t *testing.T) {
	env := testEnv(t)
	id := NewHostID()
	lock := filepath.Join(cacheSubdir(t, env, hostsName), id+".lock")
	writeFile(t, lock, "")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	var seen time.Time
	hostLockOpened = func(p string) {
		if fi, err := os.Lstat(p); err == nil {
			seen = fi.ModTime()
		}
	}
	t.Cleanup(func() { hostLockOpened = func(string) {} })
	start := time.Now().Add(-time.Second)
	h, err := Bind(env, id, Entry{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if seen.Before(start) {
		t.Fatalf("between the open and the flock the reopened lock was modified at %v; want it refreshed (after %v)", seen, start)
	}
}

// entryV1 is the registry entry as plan 027 shipped it, before plan 032 added
// requestId and requestHash: what an older reader — shed, a craze before
// plan 032 — decodes a newer entry into.
type entryV1 struct {
	Protocol          int       `json:"protocol"`
	HostID            string    `json:"hostId"`
	PID               int       `json:"pid"`
	StartedAt         time.Time `json:"startedAt"`
	Socket            string    `json:"socket"`
	CrazeSessionID    string    `json:"crazeSessionId"`
	ProviderSessionID string    `json:"providerSessionId"`
	Incarnation       string    `json:"incarnation"`
	Provider          string    `json:"provider"`
	Workspace         string    `json:"workspace"`
	Ready             bool      `json:"ready"`
}

// The two members plan 032 adds (§3.10, §3.15): a hub-created host's create
// request, given at Bind, is written as requestId and requestHash, kept by
// every rewrite whatever the rewrite does to them, and listed by Hosts; an
// older reader decodes the same file as before, ignoring them; and a host
// given none writes neither, so its file is byte for byte the older shape.
func TestTheRegistryEntrysRequestMembersRoundTrip(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h, err := Bind(env, NewHostID(), Entry{Workspace: "/w", Provider: "fake", RequestID: "req-1", RequestHash: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := h.Update(func(e *Entry) {
		e.Ready, e.CrazeSessionID = true, "s-1"
		e.RequestID, e.RequestHash = "", "other"
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hostsDir(env), h.ID()+".json")
	got := readEntryFile(t, path)
	if got.RequestID != "req-1" || got.RequestHash != "abc123" || !got.Ready || got.CrazeSessionID != "s-1" {
		t.Fatalf("the rewritten entry is %+v; want the request kept and the rewrite applied", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["requestId"] != "req-1" || m["requestHash"] != "abc123" {
		t.Fatalf("the entry file holds %v; want requestId and requestHash", m)
	}
	var old entryV1
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("an older reader cannot decode the entry: %v", err)
	}
	if want := (entryV1{Protocol: got.Protocol, HostID: got.HostID, PID: got.PID, StartedAt: got.StartedAt,
		Socket: got.Socket, CrazeSessionID: got.CrazeSessionID, ProviderSessionID: got.ProviderSessionID,
		Incarnation: got.Incarnation, Provider: got.Provider, Workspace: got.Workspace, Ready: got.Ready}); old != want {
		t.Fatalf("an older reader decoded %+v, want %+v", old, want)
	}
	listed, err := Hosts(env)
	if err != nil || len(listed) != 1 || listed[0] != h.Entry() || listed[0].RequestID != "req-1" {
		t.Fatalf("Hosts = %+v, %v; want the entry with its request", listed, err)
	}

	plain := bind(t, env)
	b, err = os.ReadFile(filepath.Join(hostsDir(env), plain.ID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	m = nil
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["requestId"]; ok || len(m) != 11 {
		t.Fatalf("a host with no create request wrote %v; want the 11 older members only", m)
	}
}

func TestClosingTheListenerLeavesTheSocket(t *testing.T) {
	t.Parallel()
	h := bind(t, testEnv(t))
	if err := h.Listener().Close(); err != nil {
		t.Fatal(err)
	}
	if !exists(t, h.Socket()) {
		t.Fatal("closing the listener unlinked the socket; only Host.Close may")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(t, h.Socket()) {
		t.Fatal("Host.Close left the socket")
	}
}

func TestCloseUnlinksOnlyWhatItBound(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	entry := filepath.Join(hostsDir(env), h.ID()+".json")
	// Each successor must be another file, not merely another name: ext4
	// hands a freed inode's number straight back to the next file created
	// near it, and a (dev, ino) check cannot tell that file from ours. Our
	// socket's inode is held by the host's open listener, so removing it frees
	// nothing; our entry's is held by an open descriptor until the test ends.
	held, err := os.Open(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for _, p := range []string{h.Socket(), entry} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	listenAt(t, h.Socket()) // a successor's socket at the same path
	writeFile(t, entry, "{}")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !exists(t, h.Socket()) || !exists(t, entry) {
		t.Fatal("Close unlinked a file it did not bind")
	}
	if exists(t, filepath.Join(hostsDir(env), h.ID()+".lock")) {
		t.Fatal("Close left the host's lock file")
	}
}

func TestARewriteRefreshesTheRegistryIdentity(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	if err := h.Update(func(e *Entry) {
		e.Ready = true
		e.ProviderSessionID = "p-1"
	}); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(hostsDir(env), h.ID()+".json")
	if e := readEntryFile(t, entry); !e.Ready || e.ProviderSessionID != "p-1" {
		t.Fatalf("the rewritten entry is %+v", e)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(t, entry) {
		t.Fatal("Close left the rewritten registry entry")
	}
}

func TestUpdateKeepsBindsMembers(t *testing.T) {
	t.Parallel()
	h := bind(t, testEnv(t))
	before := h.Entry()
	if err := h.Update(func(e *Entry) {
		e.Protocol, e.HostID, e.PID, e.Socket = 7, "ffffffffffff", 1, "/x"
		e.Incarnation = "inc"
	}); err != nil {
		t.Fatal(err)
	}
	after := h.Entry()
	if after.Protocol != before.Protocol || after.HostID != before.HostID || after.PID != before.PID ||
		after.Socket != before.Socket || after.Incarnation != "inc" {
		t.Fatalf("after Update: %+v (before %+v)", after, before)
	}
}

func TestUpdateAfterCloseWritesNothing(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := h.Update(func(*Entry) { called = true }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Update after Close = %v, want ErrClosed", err)
	}
	if called || exists(t, filepath.Join(hostsDir(env), h.ID()+".json")) {
		t.Fatal("Update after Close wrote the registry entry")
	}
}

func TestRewritesRacingCloseLeaveNoEntry(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 20 {
				_ = h.Update(func(e *Entry) { e.Incarnation = strconv.Itoa(i*100 + j) })
			}
		})
	}
	wg.Go(func() { _ = h.Close() })
	wg.Wait()
	if exists(t, filepath.Join(hostsDir(env), h.ID()+".json")) {
		t.Fatal("a rewrite racing Close left the registry entry behind")
	}
	if names, _ := os.ReadDir(hostsDir(env)); len(names) != 0 {
		t.Fatalf("the registry holds %d files after Close", len(names))
	}
}

func TestProcessEnvIsThisProcess(t *testing.T) {
	t.Parallel()
	env := ProcessEnv()
	if env.EUID != os.Geteuid() || env.TmpRoot != "/tmp" {
		t.Fatalf("ProcessEnv = %+v", env)
	}
	if wantRunUser := runtime.GOOS == "linux"; (env.RunUserRoot == "/run/user") != wantRunUser {
		t.Fatalf("RunUserRoot = %q on %s", env.RunUserRoot, runtime.GOOS)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	h := bind(t, testEnv(t))
	for i := range 3 {
		if err := h.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
}

func TestAFailedBindUnwinds(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	id := NewHostID()
	ns, err := Namespace(env.CrazeDir)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(env.CrazeRuntimeDir, ns, id+".sock")
	mkdir(t, filepath.Dir(sock), 0o700)
	writeFile(t, sock, "in the way") // bind fails: the path is taken
	if _, err := Bind(env, id, Entry{}); err == nil {
		t.Fatal("Bind over an existing path succeeded")
	}
	if !exists(t, sock) {
		t.Fatal("the unwind removed a file Bind did not create")
	}
	for _, name := range []string{id + ".lock", id + ".json"} {
		if exists(t, filepath.Join(hostsDir(env), name)) {
			t.Errorf("the unwind left %s", name)
		}
	}
}

// TestAFailedStatOfTheBoundSocketUnlinksIt is not parallel: it replaces
// lstatBound, which every Bind calls.
func TestAFailedStatOfTheBoundSocketUnlinksIt(t *testing.T) {
	env := testEnv(t)
	var bound string
	lstatBound = func(p string) (fs.FileInfo, error) {
		if fi, err := os.Lstat(p); err != nil || fi.Mode().Type() != fs.ModeSocket {
			t.Errorf("the stat after bind found %v, %v; want the socket just bound", fi, err)
		}
		bound = p
		return nil, errors.New("injected lstat failure")
	}
	t.Cleanup(func() { lstatBound = os.Lstat })
	id := NewHostID()
	if h, err := Bind(env, id, Entry{}); err == nil {
		_ = h.Close()
		t.Fatal("Bind succeeded although the stat of its socket failed")
	}
	if bound == "" {
		t.Fatal("Bind never stat-ed its socket")
	}
	if exists(t, bound) {
		t.Fatal("a failed stat after bind left the socket, which no registry entry names")
	}
	for _, name := range []string{id + ".lock", id + ".json"} {
		if exists(t, filepath.Join(hostsDir(env), name)) {
			t.Errorf("the unwind left %s", name)
		}
	}
}

// TestLockFilesAre0600UnderAnyUmask is not parallel: the umask is the
// process's, and every file another test made meanwhile would take it.
func TestLockFilesAre0600UnderAnyUmask(t *testing.T) {
	env := testEnv(t) // its directories made before the umask changes
	// So is the cache tree: a cache-tree directory made under umask 0777 is
	// refused, never chmod-ed (TestTheCacheTreeUnderEveryUmask). A file is
	// fchmod-ed through the descriptor its exclusive create returned.
	cacheSubdir(t, env, hostsName)
	cacheSubdir(t, env, locksName)
	h, c := func() (*Host, *Claim) {
		old := syscall.Umask(0o777)
		defer syscall.Umask(old)
		h, err := Bind(env, NewHostID(), Entry{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		c, err := ClaimSession(env, "s-1", h.ID())
		if err != nil {
			t.Fatal(err)
		}
		return h, c
	}()
	for _, p := range []string{filepath.Join(hostsDir(env), h.ID()+".lock"), c.Path()} {
		if got := perm(t, p); got != 0o600 {
			t.Errorf("%s has mode %04o under umask 0777, want 0600", p, got)
		}
	}
	// Each is opened again: the host's lock by a resolver, the session's by
	// its next claim.
	if got, err := Hosts(env); err != nil || len(got) != 1 {
		t.Fatalf("Hosts = %v, %v; want the live host", entries(got), err)
	}
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := ClaimSession(env, "s-1", NewHostID())
	if err != nil {
		t.Fatalf("a session released under umask 0777 could not be claimed again: %v", err)
	}
	_ = again.Release()
}

// TestAFailedEntryWriteLeavesNoTemporary is not parallel: it replaces
// fstatTemp. A registry write's temporary is unlinked on any failure, and
// that unlink is installed the moment the exclusive create returns, so a
// step after it that fails — the fstat, here — leaves no temporary: an
// Update's failure leaves the entry as it was, and a Bind's leaves nothing.
func TestAFailedEntryWriteLeavesNoTemporary(t *testing.T) {
	env := testEnv(t)
	h := bind(t, env)
	entry := filepath.Join(hostsDir(env), h.ID()+".json")
	before, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	old := fstatTemp
	t.Cleanup(func() { fstatTemp = old })
	fstatTemp = func(int, *unix.Stat_t) error { return unix.EIO }

	if err := h.Update(func(e *Entry) { e.Ready = true }); !errors.Is(err, unix.EIO) {
		t.Fatalf("Update with a failing fstat = %v, want EIO", err)
	}
	if after, err := os.ReadFile(entry); err != nil || string(after) != string(before) {
		t.Fatalf("the entry holds %q (%v) after a failed Update, want it unchanged", after, err)
	}
	if got := dirNames(t, hostsDir(env)); !slices.Equal(got, []string{h.ID() + ".json", h.ID() + ".lock"}) {
		t.Fatalf("the registry holds %v after a failed Update, want only the entry and the lock", got)
	}

	if h, err := Bind(env, NewHostID(), Entry{}); !errors.Is(err, unix.EIO) {
		if err == nil {
			_ = h.Close()
		}
		t.Fatalf("Bind with a failing fstat = %v, want EIO", err)
	}
	if got := dirNames(t, hostsDir(env)); !slices.Equal(got, []string{h.ID() + ".json", h.ID() + ".lock"}) {
		t.Fatalf("the registry holds %v after a failed Bind, want only the first host's entry and lock", got)
	}
}

// dirNames is the names in the directory p, sorted.
func dirNames(t *testing.T, p string) []string {
	t.Helper()
	des, err := os.ReadDir(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func TestBindRefusesABadHostID(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	for _, id := range []string{"", "../../x", "0123456789AB", "0123456789abc"} {
		if _, err := Bind(env, id, Entry{}); err == nil {
			t.Fatalf("Bind(%q) succeeded", id)
		}
	}
	if exists(t, filepath.Join(env.Home, cacheName)) {
		t.Fatal("a refused host id built the cache tree")
	}
}

// TestLostNamesAVanishedSocketOrEntry (plan 030 §3.3, SF-66): a bound host is
// not lost; its socket removed, or replaced by another bound at its path, is;
// so is its registry entry removed, written over by another file, or taken
// away with its whole directory — looked up by path, whatever the descriptor
// held since Bind still sees; a rewrite (Update) is the host's own and loses
// nothing; and a closed host reports nothing.
func TestLostNamesAVanishedSocketOrEntry(t *testing.T) {
	t.Parallel()
	fresh := func(t *testing.T) (*Host, string) {
		env := testEnv(t)
		h := bind(t, env)
		if why := h.Lost(); why != "" {
			t.Fatalf("a host just bound is lost: %s", why)
		}
		// Lost names the entry by its canonical path: on macOS the test's
		// /tmp is /private/tmp, so the expectation is resolved the same way.
		dir, err := filepath.EvalSymlinks(hostsDir(env))
		if err != nil {
			t.Fatal(err)
		}
		return h, filepath.Join(dir, h.ID()+".json")
	}
	lost := func(t *testing.T, h *Host, want string) {
		t.Helper()
		if why := h.Lost(); !strings.Contains(why, want) {
			t.Fatalf("Lost = %q, want it to say %q", why, want)
		}
	}
	t.Run("a rewrite is the host's own", func(t *testing.T) {
		h, _ := fresh(t)
		if err := h.Update(func(e *Entry) { e.Ready = true }); err != nil {
			t.Fatal(err)
		}
		if why := h.Lost(); why != "" {
			t.Fatalf("a rewritten entry is lost: %s", why)
		}
	})
	t.Run("the socket removed", func(t *testing.T) {
		h, _ := fresh(t)
		if err := os.Remove(h.Socket()); err != nil {
			t.Fatal(err)
		}
		lost(t, h, "its control socket "+h.Socket()+" is gone")
	})
	t.Run("another socket at its path", func(t *testing.T) {
		h, _ := fresh(t)
		if err := os.Remove(h.Socket()); err != nil {
			t.Fatal(err)
		}
		listenAt(t, h.Socket())
		lost(t, h, "its control socket "+h.Socket()+" is another file now")
	})
	t.Run("the entry removed", func(t *testing.T) {
		h, entry := fresh(t)
		if err := os.Remove(entry); err != nil {
			t.Fatal(err)
		}
		lost(t, h, "its registry entry "+entry+" is gone")
	})
	t.Run("the entry written over", func(t *testing.T) {
		h, entry := fresh(t)
		// Held open, so the replacement cannot reuse its inode (ext4).
		held, err := os.Open(entry)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		if err := os.Remove(entry); err != nil {
			t.Fatal(err)
		}
		writeFile(t, entry, "{}")
		lost(t, h, "its registry entry "+entry+" is another file now")
	})
	t.Run("the registry moved away", func(t *testing.T) {
		h, entry := fresh(t)
		dir := filepath.Dir(entry)
		if err := os.Rename(dir, dir+".moved"); err != nil {
			t.Fatal(err)
		}
		lost(t, h, "its registry entry "+entry+" is gone")
	})
	t.Run("closed", func(t *testing.T) {
		h, _ := fresh(t)
		if err := os.Remove(h.Socket()); err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		if why := h.Lost(); why != "" {
			t.Fatalf("a closed host is lost: %s", why)
		}
	})
}
