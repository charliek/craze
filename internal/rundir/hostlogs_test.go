package rundir

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestHostLogDirIsMade0700InTheCacheTree: HostLogDir makes the whole missing
// cache tree down to host-logs, each leaf 0700, and answers its canonical
// path; asked again it answers the same. A host-logs directory another mode
// left, or a symlink in its place, is refused as every cache-tree leaf is.
func TestHostLogDirIsMade0700InTheCacheTree(t *testing.T) {
	env := testEnv(t)
	dir, err := HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(mustCanonical(t, env.Home), ".cache", "craze", "host-logs")
	if dir != want {
		t.Fatalf("HostLogDir = %q, want %q", dir, want)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("host-logs is %v, %v; want a 0700 directory", fi.Mode(), err)
	}
	if again, err := HostLogDir(env); err != nil || again != dir {
		t.Fatalf("again: %q, %v", again, err)
	}

	chmod(t, dir, 0o755)
	if _, err := HostLogDir(env); err == nil {
		t.Fatal("a 0755 host-logs directory was accepted")
	}
	chmod(t, dir, 0o700)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	symlink(t, t.TempDir(), dir)
	if _, err := HostLogDir(env); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked host-logs directory: %v", err)
	}
}

// TestSweepHostLogsRemovesOnlyAGoneHostsOldLogs: the sweep removes a host's
// log, its rotation and its pgids file once they are older than the bound —
// and nothing else: a recent file, a live host's old log, a name that is not
// a host's, a symlink (never followed, never a regular file) and a directory
// all stay. A missing directory is nothing to sweep, and is not made.
func TestSweepHostLogsRemovesOnlyAGoneHostsOldLogs(t *testing.T) {
	env := testEnv(t)
	now := time.Now()
	if n, err := SweepHostLogs(env, now, time.Hour, ""); n != 0 || err != nil {
		t.Fatalf("no directory: %d, %v", n, err)
	}
	if _, err := os.Lstat(filepath.Join(env.Home, ".cache")); !os.IsNotExist(err) {
		t.Fatalf("the sweep made the cache tree: %v", err)
	}
	dir, err := HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	// A live host: bound, so its entry is listed and its lock held.
	live := NewHostID()
	h, err := Bind(env, live, Entry{Workspace: "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	gone := NewHostID()
	old := now.Add(-2 * time.Hour)
	target := filepath.Join(t.TempDir(), "outside")
	writeFile(t, target, "keep")
	aged := func(name string) string {
		p := filepath.Join(dir, name)
		writeFile(t, p, "x")
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	removed := []string{aged(gone + ".log"), aged(gone + ".log.1"), aged(gone + ".pgids")}
	kept := []string{aged(live + ".log"), aged("notes.log"), aged(gone + ".txt"), aged("ABCDEF012345.log")}
	recent := filepath.Join(dir, NewHostID()+".log")
	writeFile(t, recent, "x")
	kept = append(kept, recent)
	link := filepath.Join(dir, NewHostID()+".log")
	symlink(t, target, link)
	kept = append(kept, link)
	sub := filepath.Join(dir, NewHostID()+".log.2")
	mkdir(t, sub, 0o700)
	kept = append(kept, sub)

	n, err := SweepHostLogs(env, now, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != len(removed) {
		t.Fatalf("removed %d, want %d", n, len(removed))
	}
	for _, p := range removed {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep: %v", filepath.Base(p), err)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was swept: %v", filepath.Base(p), err)
		}
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep" {
		t.Fatalf("the link's target: %q, %v", b, err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != len(kept) {
		t.Fatalf("left %q, want the %d kept", names, len(kept))
	}
	if !slices.Contains(names, live+".log") {
		t.Fatalf("the live host's log went: %q", names)
	}
}

// TestSweepHostLogsDecidesDeathByTheHostLock (astra r3-c2 2): whether a host is
// gone is its own lock's answer, never the registry listing's. A live host
// whose entry cannot be read, or is not JSON — both of which Hosts leaves out
// — keeps every file while its lock is held; so does a host whose lock cannot
// be opened, which cannot be told. A dead host whose lock is still there
// (killed: nothing unlinked) and a host with no lock at all are gone, and
// their files go; the dead host's lock, taken for the sweep, is let go again.
// The name the sweeping host keeps (its own log, about to be opened) stays
// whatever its age.
func TestSweepHostLogsDecidesDeathByTheHostLock(t *testing.T) {
	env := testEnv(t)
	now := time.Now()
	dir, err := HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	aged := func(id string) []string {
		t.Helper()
		var ps []string
		for _, suffix := range []string{".log", ".log.1", ".pgids"} {
			p := filepath.Join(dir, id+suffix)
			writeFile(t, p, "x")
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
			ps = append(ps, p)
		}
		return ps
	}

	// Two live hosts, their locks held, whose entries Hosts cannot list.
	unreadable, malformed := bind(t, env), bind(t, env)
	chmod(t, filepath.Join(hostsDir(env), unreadable.ID()+".json"), 0)
	writeFile(t, filepath.Join(hostsDir(env), malformed.ID()+".json"), "{")
	if listed, err := Hosts(env); err != nil || len(listed) != 0 {
		t.Fatalf("Hosts lists %q, %v; the test wants both live hosts left out", entries(listed), err)
	}
	var kept, removed []string
	kept = append(kept, aged(unreadable.ID())...)
	kept = append(kept, aged(malformed.ID())...)

	// A host killed: its lock and entry are still there, and nobody holds it.
	killed := bind(t, env)
	killed.die()
	removed = append(removed, aged(killed.ID())...)
	// A host with no lock at all.
	removed = append(removed, aged(NewHostID())...)
	// A host whose lock cannot be opened: not known to be gone.
	if os.Geteuid() != 0 {
		unknown := bind(t, env)
		unknown.die()
		chmod(t, filepath.Join(hostsDir(env), unknown.ID()+".lock"), 0)
		kept = append(kept, aged(unknown.ID())...)
	}
	// The sweeping host's own log, reused: old, and its host has no lock.
	own := NewHostID() + ".log"
	kept = append(kept, filepath.Join(dir, own))
	writeFile(t, kept[len(kept)-1], "x")
	if err := os.Chtimes(kept[len(kept)-1], old, old); err != nil {
		t.Fatal(err)
	}

	n, err := SweepHostLogs(env, now, time.Hour, own)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range removed {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep: %v", filepath.Base(p), err)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was swept: %v", filepath.Base(p), err)
		}
	}
	if n != len(removed) {
		t.Fatalf("removed %d, want %d", n, len(removed))
	}
	lock, err := os.OpenFile(filepath.Join(hostsDir(env), killed.ID()+".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if taken, err := tryLock(lock); err != nil || !taken {
		t.Fatalf("the dead host's lock is still held after the sweep: %v, %v", taken, err)
	}
}
