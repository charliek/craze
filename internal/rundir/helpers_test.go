package rundir

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortDir is a fresh 0700 directory with a short path, for anything a socket
// is bound under: sun_path. It is made under the real /tmp — never
// t.TempDir(), whose path is long, and never $TMPDIR, which is about 50 bytes
// on macOS (plan 027 R5: macOS CI runs these under the real /tmp).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "czr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// testEnv is an isolated Env: its own home (so its own cache tree and
// registry), its own craze directory, and a short CRAZE_RUNTIME_DIR. The
// /run/user and /tmp candidates are off.
func testEnv(t *testing.T) Env {
	t.Helper()
	// t.TempDir makes its directory 0777 less the umask: 0775 under a
	// user-private-group umask, which is a group-writable ancestor.
	home := t.TempDir()
	chmod(t, home, 0o700)
	return Env{
		Home:            home,
		CrazeDir:        filepath.Join(home, ".craze"),
		CrazeRuntimeDir: shortDir(t),
		EUID:            os.Geteuid(),
	}
}

// mustCanonical is p with every symlink resolved.
func mustCanonical(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mkdir makes p with exactly mode (chmod-ed past the umask).
func mkdir(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	chmod(t, p, mode)
}

func chmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// perm is p's st_mode & 07777, lstat-ed.
func perm(t *testing.T, p string) uint32 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return permOf(fi)
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return false
}

// bind is Bind for a fresh host id, closed at cleanup.
func bind(t *testing.T, env Env) *Host {
	t.Helper()
	h, err := Bind(env, NewHostID(), Entry{Provider: "fake", Workspace: "/w", CrazeSessionID: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// bindErr is Bind expected to fail, with a message containing each of want.
func bindErr(t *testing.T, env Env, want ...string) error {
	t.Helper()
	h, err := Bind(env, NewHostID(), Entry{})
	if err == nil {
		_ = h.Close()
		t.Fatalf("Bind succeeded at %s; want an error containing %q", h.Socket(), want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("Bind error %q does not contain %q", err, w)
		}
	}
	return err
}

// die abandons h the way a killed process does: the kernel releases its lock
// and closes its listener, and nothing is unlinked.
func (h *Host) die() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	_ = h.ln.Close()
	_ = unlock(h.lock)
	_ = h.lock.Close()
	h.lock = nil
}

// hostsDir is env's registry directory.
func hostsDir(env Env) string {
	return filepath.Join(env.Home, cacheName, crazeName, hostsName)
}

// listenAt binds a socket at p and closes it without unlinking: a socket file
// with nobody behind it.
func listenAt(t *testing.T, p string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
}

// entries is the entries' host ids.
func entries(es []Entry) []string {
	var ids []string
	for _, e := range es {
		ids = append(ids, e.HostID)
	}
	return ids
}
