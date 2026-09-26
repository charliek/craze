package rundir

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestTheAncestorRule(t *testing.T) {
	t.Parallel()
	const me, other = 1000, 1001
	env := Env{EUID: me}
	for _, tc := range []struct {
		uid            int
		perm           uint32
		cache, runtime bool // accepted as an ancestor there
	}{
		{me, 0o755, true, true},
		{me, 0o700, true, true},
		{0, 0o755, true, true},
		{me, 0o775, true, false}, // a user-private group's ~/.cache, umask 002
		{me, 0o770, true, false},
		{0, 0o775, false, false}, // root's, group-writable: refused in both
		{0, 0o770, false, false},
		{me, 0o777, false, false},
		{me, 0o757, false, false},
		{me, 0o707, false, false},
		{0, 0o1777, true, true}, // /tmp
		{0, 0o1775, true, true},
		{me, 0o1777, true, true},
		{other, 0o755, false, false},
		{other, 0o775, false, false},
	} {
		for tree, want := range map[bool]bool{true: tc.cache, false: tc.runtime} {
			err := env.ancestorFault("/d", true, tc.uid, tc.perm, tree)
			if (err == nil) != want {
				t.Errorf("uid %d mode %04o in the cache tree = %v: error %v, want accepted %v", tc.uid, tc.perm, tree, err, want)
			}
		}
	}
	for _, tree := range []bool{true, false} {
		if env.ancestorFault("/d", false, me, 0o700, tree) == nil {
			t.Errorf("a non-directory passed as an ancestor (cache tree %v)", tree)
		}
	}
	for _, tc := range []struct {
		isDir bool
		uid   int
		perm  uint32
		want  bool
	}{
		{true, me, 0o700, true},
		{true, other, 0o700, false},
		{true, 0, 0o700, false},
		{true, me, 0o755, false},
		{true, me, 0o2700, false},
		{false, me, 0o700, false},
	} {
		if err := env.leafFault("/d", tc.isDir, tc.uid, tc.perm); (err == nil) != tc.want {
			t.Errorf("leaf dir %v uid %d mode %04o: error %v, want accepted %v", tc.isDir, tc.uid, tc.perm, err, tc.want)
		}
	}
}

// The owner's own box: umask 002 under user-private groups leaves the home
// directory and ~/.cache 0775. The cache tree, held by descriptor, takes them;
// the runtime tree, used by path, refuses the same directory.
func TestAGroupWritableDirectoryOfTheUsersOwn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	chmod(t, env.Home, 0o775)
	mkdir(t, filepath.Join(env.Home, cacheName), 0o775)
	h := bind(t, env)
	if got, err := Hosts(env); err != nil || !slices.Equal(entries(got), []string{h.ID()}) {
		t.Fatalf("Hosts = %v, %v; want %s", entries(got), err, h.ID())
	}
	c, err := ClaimSession(env, "s-1", h.ID())
	if err != nil {
		t.Fatalf("ClaimSession under a 0775 ~/.cache: %v", err)
	}
	_ = c.Release()

	env.CrazeRuntimeDir = filepath.Join(env.Home, "rt")
	home := mustCanonical(t, env.Home)
	bindErr(t, env, "CRAZE_RUNTIME_DIR",
		"ancestor "+home+" is group- or world-writable without the sticky bit (mode 0775)",
		"run: chmod g-w "+home)
	if exists(t, env.CrazeRuntimeDir) {
		t.Fatal("the refused runtime directory was created")
	}
}

func TestTheCacheTreeRefusesAnAncestorOthersCanWrite(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mode  os.FileMode
		other bool // the Env.EUID seam: every directory of the real uid is another's
		want  []string
	}{
		"world-writable":  {mode: 0o777, want: []string{"(mode 0777)", "run: chmod go-w "}},
		"others-writable": {mode: 0o757, want: []string{"(mode 0757)", "run: chmod o-w "}},
		"group-writable, another uid's": {mode: 0o775, other: true,
			want: []string{"is owned by uid " + strconv.Itoa(os.Geteuid())}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			cache := filepath.Join(env.Home, cacheName)
			mkdir(t, cache, tc.mode)
			if tc.other {
				env.EUID = os.Geteuid() + 1
			} else {
				tc.want = append(tc.want, "ancestor "+mustCanonical(t, cache)+" is group- or world-writable")
			}
			c, err := ClaimSession(env, "s-1", NewHostID())
			if err == nil {
				_ = c.Release()
				t.Fatal("ClaimSession succeeded")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("ClaimSession error %q does not contain %q", err, w)
				}
			}
			if exists(t, filepath.Join(cache, crazeName)) {
				t.Fatal("the cache tree was built under a refused ancestor")
			}
		})
	}
}

// plantDecoy puts a registry directory in crazeDir holding id's entry and
// lock, each "planted": what a path-based rewrite or unlink would reach.
func plantDecoy(t *testing.T, crazeDir, id string) {
	t.Helper()
	hosts := filepath.Join(crazeDir, hostsName)
	mkdir(t, hosts, 0o700)
	for _, n := range []string{id + ".json", id + ".lock"} {
		writeFile(t, filepath.Join(hosts, n), "planted")
	}
}

// checkDecoy fails unless crazeDir's registry is exactly what plantDecoy put.
func checkDecoy(t *testing.T, crazeDir, id string) {
	t.Helper()
	hosts := filepath.Join(crazeDir, hostsName)
	names, err := os.ReadDir(hosts)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, de := range names {
		got = append(got, de.Name())
		if b, err := os.ReadFile(filepath.Join(hosts, de.Name())); err != nil || string(b) != "planted" {
			t.Errorf("the planted %s holds %q (%v): craze wrote to it", de.Name(), b, err)
		}
	}
	if want := []string{id + ".json", id + ".lock"}; !slices.Equal(got, want) {
		t.Errorf("the planted registry holds %v, want %v: craze wrote to or unlinked from it", got, want)
	}
}

// plants are what a writer of ~/.cache could put at a name after renaming
// craze's validated directory away. Each returns the directory the plant
// leads to.
var plants = map[string]func(t *testing.T, at string) string{
	"a symlink to a directory another controls": func(t *testing.T, at string) string {
		target := filepath.Join(shortDir(t), "decoy")
		mkdir(t, target, 0o700)
		symlink(t, target, at)
		return target
	},
	"another 0700 directory of the user's": func(t *testing.T, at string) string {
		mkdir(t, at, 0o700)
		return at
	},
}

// The attack a group-writable ancestor allows, and why it is harmless: after
// Bind validated ~/.cache/craze, a writer of ~/.cache renames it away and
// plants something at its name. The host holds the registry directory by
// descriptor, so its rewrite and its unlinks reach the directory it
// validated, wherever that now is, and never the plant.
func TestARenamedCacheTreeIsStillTheOneTheHostUses(t *testing.T) {
	t.Parallel()
	for name, plant := range plants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			h := bind(t, env)
			cache := filepath.Join(env.Home, cacheName)
			real := filepath.Join(cache, "moved")
			if err := os.Rename(filepath.Join(cache, crazeName), real); err != nil {
				t.Fatal(err)
			}
			decoy := plant(t, filepath.Join(cache, crazeName))
			plantDecoy(t, decoy, h.ID())

			if err := h.Update(func(e *Entry) { e.Incarnation = "after-the-swap" }); err != nil {
				t.Fatal(err)
			}
			realHosts := filepath.Join(real, hostsName)
			if e := readEntryFile(t, filepath.Join(realHosts, h.ID()+".json")); e.Incarnation != "after-the-swap" {
				t.Fatalf("the validated registry holds %+v; the rewrite went elsewhere", e)
			}
			checkDecoy(t, decoy, h.ID())

			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if names, err := os.ReadDir(realHosts); err != nil || len(names) != 0 {
				t.Fatalf("the validated registry holds %v (%v) after Close; want its entry and lock gone", names, err)
			}
			checkDecoy(t, decoy, h.ID())
		})
	}
}

// A claim holds its lock file's descriptor and nothing else, so the next
// claim walks the tree again: a planted symlink is refused there, and a
// directory that validates is used.
func TestAClaimAfterTheLocksDirectoryIsSwappedWalksAgain(t *testing.T) {
	t.Parallel()
	for name, plant := range plants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			first, err := ClaimSession(env, "s-1", NewHostID())
			if err != nil {
				t.Fatal(err)
			}
			crazeDir := filepath.Join(env.Home, cacheName, crazeName)
			locks := filepath.Join(crazeDir, locksName)
			moved := filepath.Join(crazeDir, "locks-moved")
			if err := os.Rename(locks, moved); err != nil {
				t.Fatal(err)
			}
			planted := plant(t, locks)

			second, err := ClaimSession(env, "s-2", NewHostID())
			isLink := planted != locks
			switch {
			case isLink && err == nil:
				_ = second.Release()
				t.Fatal("a claim followed a symlink planted at the locks directory")
			case isLink && !strings.Contains(err.Error(), locksName+" is a symlink"):
				t.Fatalf("ClaimSession error %v; want the symlinked locks directory refused by name", err)
			case !isLink && err != nil:
				t.Fatalf("a claim refused a 0700 locks directory of the user's: %v", err)
			case !isLink:
				if want := filepath.Join(mustCanonical(t, crazeDir), locksName, "s-2.lock"); second.Path() != want || !exists(t, want) {
					t.Fatalf("the claim is at %s; want the validated %s", second.Path(), want)
				}
				_ = second.Release()
			}
			if isLink {
				if names, _ := os.ReadDir(planted); len(names) != 0 {
					t.Fatalf("the symlink's target holds %d files", len(names))
				}
			}
			// The first claim's lock moved with its directory; it is released
			// there, through its descriptor.
			if err := first.Release(); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(moved, "s-1.lock")); err != nil || len(b) != 0 {
				t.Fatalf("the moved lock holds %q (%v); want its holder line cleared", b, err)
			}
		})
	}
}

// Hosts walks again too: a symlinked craze or hosts directory is refused
// even when it links to the very tree the host validated, and so is a tree
// of another uid's.
func TestHostsRefusesASwappedTree(t *testing.T) {
	t.Parallel()
	for name, leaf := range map[string]func(env Env) string{
		crazeName: func(env Env) string { return filepath.Join(env.Home, cacheName, crazeName) },
		hostsName: hostsDir,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			bind(t, env)
			p := leaf(env)
			if err := os.Rename(p, p+"-moved"); err != nil {
				t.Fatal(err)
			}
			symlink(t, p+"-moved", p)
			got, err := Hosts(env)
			if err == nil || !strings.Contains(err.Error(), name+" is a symlink") {
				t.Fatalf("Hosts = %v, %v; want the symlinked %s refused by name", entries(got), err, name)
			}
		})
	}
	t.Run("another uid's", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		bind(t, env)
		env.EUID = os.Geteuid() + 1
		if got, err := Hosts(env); err == nil || !strings.Contains(err.Error(), "is owned by uid "+strconv.Itoa(os.Geteuid())) {
			t.Fatalf("Hosts = %v, %v; want the tree of another uid refused", entries(got), err)
		}
	})
}

// The walk is given the canonical path, and every component of it is opened
// O_NOFOLLOW: one replaced by a symlink since canonicalisation is refused, at
// whatever depth, even when it links to a directory that would validate.
func TestTheWalkRefusesAComponentReplacedByASymlink(t *testing.T) {
	t.Parallel()
	for depth := range 4 { // the test's root, a, a/b, a/b/c
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			t.Parallel()
			root := mustCanonical(t, shortDir(t))
			p := filepath.Join(root, "a", "b", "c")
			mkdir(t, p, 0o700)
			env := Env{EUID: os.Geteuid()}
			canon, err := canonical(p)
			if err != nil {
				t.Fatal(err)
			}
			d, err := env.walk(canon)
			if err != nil {
				t.Fatalf("setup: the walk refused %s: %v", canon, err)
			}
			_ = d.close()

			comps := components(canon)
			swapped := comps[len(comps)-4+depth]
			t.Cleanup(func() { _ = os.RemoveAll(swapped + "-real") })
			if err := os.Rename(swapped, swapped+"-real"); err != nil {
				t.Fatal(err)
			}
			symlink(t, swapped+"-real", swapped)
			if d, err := env.walk(canon); err == nil {
				_ = d.close()
				t.Fatalf("the walk followed %s, a symlink", swapped)
			} else if want := "ancestor " + swapped + " is a symlink"; !strings.Contains(err.Error(), want) {
				t.Fatalf("walk error %v; want %q", err, want)
			}
		})
	}
}

// TestTheCacheTreeIsMade0700UnderAnyUmask is not parallel: the umask is the
// process's. Each umask takes a different road to 0700: none needed (077),
// an fchmod through the descriptor of a directory made 0500 (200), and a
// chmod by name, never following a link, of one made 0000, which cannot be
// opened (777).
func TestTheCacheTreeIsMade0700UnderAnyUmask(t *testing.T) {
	for _, mask := range []int{0o077, 0o200, 0o777} {
		t.Run(strconv.FormatInt(int64(mask), 8), func(t *testing.T) {
			env := testEnv(t)
			c, err := func() (*Claim, error) {
				old := syscall.Umask(mask)
				defer syscall.Umask(old)
				return ClaimSession(env, "s-1", NewHostID())
			}()
			if err != nil {
				t.Fatal(err)
			}
			_ = c.Release()
			for _, p := range []string{
				filepath.Join(env.Home, cacheName),
				filepath.Join(env.Home, cacheName, crazeName),
				filepath.Join(env.Home, cacheName, crazeName, locksName),
			} {
				if got := perm(t, p); got != 0o700 {
					t.Errorf("%s has mode %04o under umask %04o, want 0700", p, got, mask)
				}
			}
		})
	}
}
