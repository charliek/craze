package rundir

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

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
	e, err := readEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Ready || e.ProviderSessionID != "p-1" {
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
