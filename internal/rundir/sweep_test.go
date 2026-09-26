package rundir

import (
	"encoding/json"
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
		dead.Socket(),
		filepath.Join(hostsDir(env), dead.ID()+".json"),
		filepath.Join(hostsDir(env), dead.ID()+".lock"),
	} {
		if exists(t, p) {
			t.Errorf("the sweep left %s", p)
		}
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

func TestTheSweepLeavesASocketOutsideAValidDirectory(t *testing.T) {
	t.Parallel()
	for name, spoil := range map[string]func(t *testing.T, dir string){
		"a 0755 directory":       func(t *testing.T, dir string) { chmod(t, dir, 0o755) },
		"a 0770 directory":       func(t *testing.T, dir string) { chmod(t, dir, 0o770) },
		"an open ancestor":       func(t *testing.T, dir string) { chmod(t, filepath.Dir(dir), 0o777) },
		"a symlinked directory":  nil,                         // the entry names the socket through a link
		"a directory left as is": func(*testing.T, string) {}, // the control: it is unlinked
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			dir := filepath.Join(mustCanonical(t, shortDir(t)), "ns")
			mkdir(t, dir, 0o700)
			var sock string
			h := deadWithSocket(t, env, func(id string) string {
				sock = filepath.Join(dir, id+".sock")
				listenAt(t, sock)
				if spoil == nil {
					link := filepath.Join(filepath.Dir(dir), "link")
					symlink(t, dir, link)
					return filepath.Join(link, id+".sock")
				}
				spoil(t, dir)
				return sock
			})
			if _, err := Hosts(env); err != nil {
				t.Fatal(err)
			}
			if kept := exists(t, sock); kept != (name != "a directory left as is") {
				t.Fatalf("socket kept = %v", kept)
			}
			if exists(t, filepath.Join(hostsDir(env), h.ID()+".json")) {
				t.Fatal("the sweep left the stale entry")
			}
		})
	}
}

func TestTheSweepLeavesASocketNotNamedForItsHost(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	other := bind(t, env) // a valid namespace directory, and a live neighbour
	sock := filepath.Join(filepath.Dir(other.Socket()), "stray.sock")
	listenAt(t, sock)
	deadWithSocket(t, env, func(string) string { return sock })
	deadWithSocket(t, env, func(string) string { return other.Socket() }) // a live host's
	got, err := Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if ids := entries(got); !slices.Equal(ids, []string{other.ID()}) {
		t.Fatalf("Hosts = %v, want only %s", ids, other.ID())
	}
	if !exists(t, sock) || !exists(t, other.Socket()) {
		t.Fatal("the sweep unlinked a socket not named for the dead host")
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
	if _, err := env.cacheSubdir(hostsName, true); err != nil {
		t.Fatal(err)
	}
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
