package rundir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCatalogDirIsTheCacheTreesUnderHome (plan 030 §3.14): the catalog cache
// is <Home>/.cache/craze/catalogs — keyed by HOME as the registry is, never
// by the craze directory CRAZE_HOME relocates — validated as a cache-tree
// leaf: made 0700 with create, not made without it (a reader finds nothing
// cached), a 0755 or symlinked directory refused, and no home refused.
func TestCatalogDirIsTheCacheTreesUnderHome(t *testing.T) {
	env := testEnv(t)
	// A relocated craze directory changes nothing: the cache is HOME's.
	env.CrazeDir = filepath.Join(t.TempDir(), "elsewhere")

	if _, err := CatalogDir(env, false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no cache tree yet, without create: %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(filepath.Join(env.Home, ".cache")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a read made the cache tree: %v", err)
	}

	dir, err := CatalogDir(env, true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(mustCanonical(t, env.Home), ".cache", "craze", "catalogs")
	if dir != want {
		t.Fatalf("CatalogDir = %q, want %q", dir, want)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("catalogs is %v, %v; want a 0700 directory", fi.Mode(), err)
	}
	if again, err := CatalogDir(env, false); err != nil || again != dir {
		t.Fatalf("read back without create: %q, %v", again, err)
	}
	if _, err := os.Lstat(filepath.Join(env.CrazeDir, "catalogs")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the craze directory holds a catalog cache: %v", err)
	}

	chmod(t, dir, 0o755)
	if _, err := CatalogDir(env, false); err == nil {
		t.Fatal("a 0755 catalogs directory was accepted")
	}
	chmod(t, dir, 0o700)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	symlink(t, t.TempDir(), dir)
	if _, err := CatalogDir(env, true); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked catalogs directory: %v", err)
	}

	if _, err := CatalogDir(Env{}, true); err == nil {
		t.Fatal("no home directory was accepted")
	}
}
