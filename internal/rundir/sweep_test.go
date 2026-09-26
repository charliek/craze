package rundir

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestHostsListsALiveHost(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	// Bind holds the lock on its own open file description; Hosts opens the
	// file again, and flock contends across the two within one process.
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != h.Entry() {
		t.Fatalf("Hosts = %+v, want [%+v]", got, h.Entry())
	}
	if !exists(t, h.Socket()) || !exists(t, filepath.Join(hostsDir(env), h.ID()+".lock")) {
		t.Fatal("listing a live host removed its files")
	}
}

func TestHostsSweepsADeadHost(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	live := bind(t, env)
	dead := bind(t, env)
	dead.die()
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if ids := entries(got); !slices.Equal(ids, []string{live.ID()}) {
		t.Fatalf("Hosts = %v, want only the live %s", ids, live.ID())
	}
	for _, p := range []string{
		filepath.Join(hostsDir(env), dead.ID()+".json"),
		filepath.Join(hostsDir(env), dead.ID()+".lock"),
	} {
		if exists(t, p) {
			t.Errorf("the sweep left %s", p)
		}
	}
	// The sweep removes no socket: the dead host's stays in its runtime
	// directory, under a name never reused.
	if !isSocket(t, dead.Socket()) {
		t.Errorf("the sweep removed the dead host's socket %s", dead.Socket())
	}
}

func TestHostsSkipsAnEntryWithNoLockAndCreatesNone(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	h.die()
	lock := filepath.Join(hostsDir(env), h.ID()+".lock")
	if err := unlinkFile(lock); err != nil {
		t.Fatal(err)
	}
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Hosts = %+v, want none", got)
	}
	if exists(t, lock) {
		t.Fatal("Hosts created a host's lock file")
	}
	if !exists(t, filepath.Join(hostsDir(env), h.ID()+".json")) || !exists(t, h.Socket()) {
		t.Fatal("Hosts removed files it held no lock for")
	}
}

// deadWithSocket is a dead host whose registry entry names sock(its id)
// instead of its own socket.
func deadWithSocket(t *testing.T, env Env, sock func(id string) string) *Host {
	t.Helper()
	h := bind(t, env)
	h.die()
	e := h.Entry()
	e.Socket = sock(h.ID())
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(hostsDir(env), h.ID()+".json"), string(b))
	return h
}

// The sweep removes no socket, wherever the stale entry points: not the dead
// host's own, in the directory it bound in; not a stray one; not a live
// host's. The stale entries and their locks go.
func TestTheSweepRemovesNoSocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	live := bind(t, env) // a valid namespace directory, and a live neighbour
	stray := filepath.Join(filepath.Dir(live.Socket()), "stray.sock")
	listenAt(t, stray)
	own := bind(t, env) // its entry names its own socket, where it bound it
	own.die()
	dead := []*Host{
		own,
		deadWithSocket(t, env, func(string) string { return stray }),
		deadWithSocket(t, env, func(string) string { return live.Socket() }),
	}
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if ids := entries(got); !slices.Equal(ids, []string{live.ID()}) {
		t.Fatalf("Hosts = %v, want only %s", ids, live.ID())
	}
	for _, sock := range []string{own.Socket(), stray, live.Socket()} {
		if !isSocket(t, sock) {
			t.Errorf("the sweep unlinked %s", sock)
		}
	}
	for _, h := range dead {
		for _, n := range []string{h.ID() + ".json", h.ID() + ".lock"} {
			if exists(t, filepath.Join(hostsDir(env), n)) {
				t.Errorf("the sweep left %s", n)
			}
		}
	}
}

// The copy attack (r31, item 3): a sweep's lock is authority over its own
// registry, never over a socket. A copy of the registry the victim owns —
// under another name, its <id>.lock a copy, another inode, so not the live
// host's lock — renamed into ~/.cache/craze by a writer of ~/.cache while the
// host lives hands Hosts a lock it can take. The copy's entry and lock are
// swept; the live host's socket survives, still accepting, and the live
// registry, renamed aside, is untouched.
func TestASweepOfACopiedRegistryLeavesTheLiveSocket(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	cache := filepath.Join(env.Home, cacheName)
	crazeDir := filepath.Join(cache, crazeName)
	backup := filepath.Join(cache, "craze-backup")
	mkdir(t, filepath.Join(backup, hostsName), 0o700)
	chmod(t, backup, 0o700)
	names := []string{h.ID() + ".json", h.ID() + ".lock"}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(crazeDir, hostsName, n))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(backup, hostsName, n), string(b))
	}
	aside := filepath.Join(cache, "craze-live")
	if err := os.Rename(crazeDir, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, crazeDir); err != nil {
		t.Fatal(err)
	}

	got, err := Hosts(env)
	if err != nil || len(got) != 0 {
		t.Fatalf("Hosts = %v, %v; want none (the copied lock is nobody's)", entries(got), err)
	}
	if !isSocket(t, h.Socket()) {
		t.Fatalf("a sweep of a copied registry unlinked the live host's socket %s", h.Socket())
	}
	conn, err := net.Dial("unix", h.Socket())
	if err != nil {
		t.Fatalf("the live host's socket no longer accepts: %v", err)
	}
	_ = conn.Close()
	if left, err := os.ReadDir(filepath.Join(crazeDir, hostsName)); err != nil || len(left) != 0 {
		t.Fatalf("the copy holds %v (%v); want its stale entry and lock swept", left, err)
	}
	for _, n := range names {
		if !exists(t, filepath.Join(aside, hostsName, n)) {
			t.Errorf("the live registry lost %s", n)
		}
	}
}

// A dead host's temporaries — writes that died before their rename — are
// swept with it, under its lock: only its own, never a live host's, and
// never a name that only looks like one.
func TestTheSweepRemovesTheDeadHostsTemporaries(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	live := bind(t, env)
	dead := bind(t, env)
	dead.die()
	hosts := hostsDir(env)
	gone := []string{"." + dead.ID() + ".json.1", "." + dead.ID() + ".json.4294967295"}
	kept := []string{
		"." + live.ID() + ".json.7",       // a live host's, mid-write
		"." + dead.ID() + ".json.x1",      // not a temporary's name
		"." + dead.ID() + ".json.",        // nor this
		"." + dead.ID() + ".lock.1",       // nor this
		"." + dead.ID() + ".json.1.extra", // nor this
	}
	for _, n := range append(append([]string(nil), gone...), kept...) {
		writeFile(t, filepath.Join(hosts, n), "{")
	}
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if ids := entries(got); !slices.Equal(ids, []string{live.ID()}) {
		t.Fatalf("Hosts = %v, want only %s", ids, live.ID())
	}
	for _, n := range gone {
		if exists(t, filepath.Join(hosts, n)) {
			t.Errorf("the sweep left the dead host's temporary %s", n)
		}
	}
	for _, n := range kept {
		if !exists(t, filepath.Join(hosts, n)) {
			t.Errorf("the sweep removed %s", n)
		}
	}
}

func TestAFIFOEntryDoesNotHangHosts(t *testing.T) {
	t.Parallel()
	for name, held := range map[string]bool{"its lock held": true, "its lock free": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			h := bind(t, env)
			if !held {
				h.die()
			}
			entry := filepath.Join(hostsDir(env), h.ID()+".json")
			if err := os.Remove(entry); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(entry, 0o600); err != nil {
				t.Fatal(err)
			}
			type result struct {
				got []Entry
				err error
			}
			done := make(chan result, 1)
			go func() {
				got, err := Hosts(env)
				done <- result{got, err}
			}()
			var r result
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Hosts is still opening a FIFO registry entry, waiting for a writer")
			}
			if r.err != nil || len(r.got) != 0 {
				t.Fatalf("Hosts = %v, %v; want no host (the entry is no regular file)", entries(r.got), r.err)
			}
			// A live host's unreadable entry is skipped and left; a dead one's
			// is swept with its lock.
			if exists(t, entry) != held || exists(t, filepath.Join(hostsDir(env), h.ID()+".lock")) != held {
				t.Fatalf("after Hosts: entry kept %v, lock kept %v; want both %v",
					exists(t, entry), exists(t, filepath.Join(hostsDir(env), h.ID()+".lock")), held)
			}
		})
	}
}

func TestHostsIgnoresNamesThatAreNotHostIDs(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	for _, name := range []string{"notanid.json", "0123456789AB.json", ".0123456789ab.json.123", "README"} {
		writeFile(t, filepath.Join(hostsDir(env), name), "{}")
	}
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if ids := entries(got); !slices.Equal(ids, []string{h.ID()}) {
		t.Fatalf("Hosts = %v, want only %s", ids, h.ID())
	}
	if !exists(t, filepath.Join(hostsDir(env), "notanid.json")) {
		t.Fatal("Hosts touched a file that is not a registry entry")
	}
}

func TestHostsWithNoTreeIsEmptyAndCreatesNothing(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	got, err := Hosts(env)
	if err != nil || len(got) != 0 {
		t.Fatalf("Hosts = %v, %v; want none", got, err)
	}
	if exists(t, filepath.Join(env.Home, cacheName)) {
		t.Fatal("Hosts built the cache tree")
	}
}

func TestHostsRefusesAHostileRegistry(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	bind(t, env)
	chmod(t, hostsDir(env), 0o755)
	if _, err := Hosts(env); err == nil {
		t.Fatal("Hosts read a registry directory of mode 0755")
	}
}

func TestHostsFindsEveryHostWhateverItsRuntimeBase(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	a := bind(t, env)
	other := env
	other.CrazeDir = filepath.Join(env.Home, "elsewhere")
	other.CrazeRuntimeDir = ""
	other.XDGRuntimeDir = shortDir(t)
	b := bind(t, other)
	// A resolver with neither host's environment, only the same home.
	got, err := Hosts(Env{Home: env.Home, EUID: env.EUID})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{a.ID(), b.ID()}
	slices.Sort(want)
	if ids := entries(got); !slices.Equal(ids, want) {
		t.Fatalf("Hosts = %v, want %v", ids, want)
	}
}

func TestHostsFollowsNoSymlinkedEntry(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	entry := filepath.Join(hostsDir(env), h.ID()+".json")
	b, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(shortDir(t), "entry.json")
	writeFile(t, target, string(b))
	if err := os.Remove(entry); err != nil {
		t.Fatal(err)
	}
	symlink(t, target, entry)
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Hosts listed %v through a symlinked entry", entries(got))
	}
}

func TestBindRefusesASymlinkedHostLock(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	cacheSubdir(t, env, hostsName)
	id := NewHostID()
	lock := filepath.Join(hostsDir(env), id+".lock")
	target := filepath.Join(shortDir(t), "elsewhere")
	writeFile(t, target, "keep")
	symlink(t, target, lock)
	if h, err := Bind(env, id, Entry{}); err == nil {
		_ = h.Close()
		t.Fatal("Bind followed a symlinked host lock")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the symlink's target was written: %q", b)
	}
	if !exists(t, lock) {
		t.Fatal("the unwind removed a lock file Bind never held")
	}
}
