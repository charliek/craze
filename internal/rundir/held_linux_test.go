//go:build linux

package rundir

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTheWalkPassesASearchOnlyAncestor: on Linux an ancestor is opened O_PATH,
// which needs no read permission, so a search-only ancestor — a root-owned
// 0711 /home, to every user but root — is walked as lstat walked it before
// the cache tree was held by descriptor: the tree under it is made, a session
// claimed, a host bound and listed. The test cannot give a directory to root,
// and 0711 is readable to its own owner, so the ancestor is the euid's own
// and 0111: the same search-only directory, seen from the euid.
func TestTheWalkPassesASearchOnlyAncestor(t *testing.T) {
	t.Parallel()
	root := mustCanonical(t, shortDir(t))
	anc := filepath.Join(root, "anc")
	home := filepath.Join(anc, "home")
	mkdir(t, home, 0o700)
	chmod(t, anc, 0o111)
	// Before shortDir's RemoveAll, which runs after this.
	t.Cleanup(func() { _ = os.Chmod(anc, 0o700) })
	// The case exists only where the euid cannot read the ancestor: root
	// (CAP_DAC_READ_SEARCH) can.
	if fd, err := unix.Open(anc, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); err == nil {
		_ = unix.Close(fd)
		t.Skip("the euid can read a search-only directory: there is nothing to walk past")
	} else if !errors.Is(err, unix.EACCES) {
		t.Fatalf("reading the search-only ancestor: %v, want EACCES", err)
	}

	env := Env{
		Home:            home,
		CrazeDir:        filepath.Join(home, ".craze"),
		CrazeRuntimeDir: shortDir(t),
		EUID:            os.Geteuid(),
	}
	d, err := env.walk(home)
	if err != nil {
		t.Fatalf("the walk refused a search-only ancestor: %v", err)
	}
	_ = d.close()
	c, err := ClaimSession(env, "s-1", NewHostID())
	if err != nil {
		t.Fatalf("a claim under a search-only ancestor: %v", err)
	}
	_ = c.Release()
	h := bind(t, env)
	if got, err := Hosts(env); err != nil || !slices.Equal(entries(got), []string{h.ID()}) {
		t.Fatalf("Hosts = %v, %v; want %s", entries(got), err, h.ID())
	}
	if got := perm(t, anc); got != 0o111 {
		t.Fatalf("the walk changed the ancestor's mode to %04o", got)
	}
}
