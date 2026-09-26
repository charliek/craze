package rundir

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestASymlinkedLeafIsRefused(t *testing.T) {
	t.Parallel()
	ns := func(env Env) string {
		n, err := Namespace(env.CrazeDir)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	cache := func(env Env) string { return filepath.Join(env.Home, cacheName) }
	for name, tc := range map[string]struct {
		setup func(t *testing.T, env *Env) string // makes the link; returns it
		claim bool                                // refused by ClaimSession rather than Bind
	}{
		"base": {setup: func(t *testing.T, env *Env) string {
			link := filepath.Join(shortDir(t), "rt")
			symlink(t, env.CrazeRuntimeDir, link)
			env.CrazeRuntimeDir = link
			return link
		}},
		"ns": {setup: func(t *testing.T, env *Env) string {
			link := filepath.Join(env.CrazeRuntimeDir, ns(*env))
			symlink(t, shortDir(t), link)
			return link
		}},
		"craze": {setup: func(t *testing.T, env *Env) string {
			mkdir(t, cache(*env), 0o700)
			link := filepath.Join(cache(*env), crazeName)
			symlink(t, shortDir(t), link)
			return link
		}},
		"hosts": {setup: func(t *testing.T, env *Env) string {
			mkdir(t, filepath.Join(cache(*env), crazeName), 0o700)
			link := filepath.Join(cache(*env), crazeName, hostsName)
			symlink(t, shortDir(t), link)
			return link
		}},
		"locks": {claim: true, setup: func(t *testing.T, env *Env) string {
			mkdir(t, filepath.Join(cache(*env), crazeName), 0o700)
			link := filepath.Join(cache(*env), crazeName, locksName)
			symlink(t, shortDir(t), link)
			return link
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			link := filepath.Base(tc.setup(t, &env))
			var err error
			if tc.claim {
				_, err = ClaimSession(env, "s-1", NewHostID())
			} else {
				err = bindErr(t, env)
			}
			if err == nil || !strings.Contains(err.Error(), link+" is a symlink") {
				t.Fatalf("error %v; want the symlinked %s refused by name", err, name)
			}
		})
	}
}

// A tree reached through a link is validated at the link's canonical target.
// The cache tree accepts a group-writable directory of the euid's own
// (TestTheCacheTreeAcceptsAGroupWritableDirectoryOfTheUsersOwn), so there the
// target is world-writable; the runtime tree refuses a group-writable one.
func TestASymlinkedAncestorIntoAWritableDirectoryIsRefused(t *testing.T) {
	t.Parallel()
	t.Run("cache", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		shared := filepath.Join(shortDir(t), "shared")
		mkdir(t, shared, 0o777)
		symlink(t, shared, filepath.Join(env.Home, cacheName))
		bindErr(t, env, "ancestor "+mustCanonical(t, shared)+" is group- or world-writable without the sticky bit (mode 0777)")
	})
	t.Run("runtime", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		shared := filepath.Join(shortDir(t), "shared")
		mkdir(t, shared, 0o770)
		link := filepath.Join(shortDir(t), "l")
		symlink(t, shared, link)
		env.CrazeRuntimeDir = filepath.Join(link, "rt")
		bindErr(t, env, "CRAZE_RUNTIME_DIR", "ancestor "+mustCanonical(t, shared)+
			" is group- or world-writable without the sticky bit (mode 0770)")
	})
}

func TestAGroupOrWorldWritableAncestorWithoutStickyIsRefused(t *testing.T) {
	t.Parallel()
	for mode, fix := range map[os.FileMode]string{0o770: "g-w", 0o777: "go-w", 0o707: "o-w"} {
		t.Run(strconv.FormatUint(uint64(mode), 8), func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			parent := env.CrazeRuntimeDir
			env.CrazeRuntimeDir = filepath.Join(parent, "rt")
			chmod(t, parent, mode)
			canon := mustCanonical(t, parent)
			bindErr(t, env, "CRAZE_RUNTIME_DIR", "ancestor "+canon+" is group- or world-writable",
				"run: chmod "+fix+" "+canon)
		})
	}
}

func TestAStickyWorldWritableAncestorIsAccepted(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	parent := env.CrazeRuntimeDir
	env.CrazeRuntimeDir = filepath.Join(parent, "rt")
	chmod(t, parent, 0o777|os.ModeSticky)
	if got := perm(t, parent); got != 0o1777 {
		t.Fatalf("setup: %s has mode %04o, want 1777", parent, got)
	}
	bind(t, env)
}

func TestASymlinkedAncestorIntoAGoodDirectoryIsAccepted(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	real := env.CrazeRuntimeDir
	link := filepath.Join(shortDir(t), "l")
	symlink(t, real, link)
	env.CrazeRuntimeDir = filepath.Join(link, "rt")
	elsewhere := filepath.Join(shortDir(t), "cache")
	mkdir(t, elsewhere, 0o700)
	symlink(t, elsewhere, filepath.Join(env.Home, cacheName)) // ~/.cache on another disk
	h := bind(t, env)
	want := filepath.Join(mustCanonical(t, real), "rt", h.Namespace(), h.ID()+".sock")
	if h.Socket() != want || h.Entry().Socket != want {
		t.Fatalf("socket %s, registry socket %s; want the canonical %s", h.Socket(), h.Entry().Socket, want)
	}
	if !exists(t, filepath.Join(elsewhere, crazeName, hostsName, h.ID()+".json")) {
		t.Fatal("the registry entry is not in the canonical cache tree")
	}
}

func TestALeafWithMode0755IsRefusedNotRepaired(t *testing.T) {
	t.Parallel()
	for name, leaf := range map[string]func(env Env) string{
		"base":  func(env Env) string { return env.CrazeRuntimeDir },
		"ns":    func(env Env) string { n, _ := Namespace(env.CrazeDir); return filepath.Join(env.CrazeRuntimeDir, n) },
		"craze": func(env Env) string { return filepath.Join(env.Home, cacheName, crazeName) },
		"hosts": func(env Env) string { return filepath.Join(env.Home, cacheName, crazeName, hostsName) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			p := leaf(env)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			mkdir(t, p, 0o755)
			bindErr(t, env, filepath.Base(p)+" has mode 0755, and 0700 is required")
			if got := perm(t, p); got != 0o755 {
				t.Fatalf("%s was changed to %04o; a leaf is never repaired", p, got)
			}
		})
	}
}

func TestARegularFileWhereADirectoryShouldBeIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		path func(env Env) string
		want string
	}{
		"base":   {func(env Env) string { return filepath.Join(env.CrazeRuntimeDir, "rt") }, "rt is not a directory"},
		"hosts":  {func(env Env) string { return filepath.Join(env.Home, cacheName, crazeName, hostsName) }, "hosts is not a directory"},
		".cache": {func(env Env) string { return filepath.Join(env.Home, cacheName) }, "ancestor"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			p := tc.path(env)
			if name == "base" {
				env.CrazeRuntimeDir = p
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, p, "x")
			bindErr(t, env, tc.want)
		})
	}
}

func TestTheWrongOwnerIsRefused(t *testing.T) {
	t.Parallel()
	other := os.Geteuid() + 1
	t.Run("ancestor", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		parent := env.CrazeRuntimeDir
		env.CrazeRuntimeDir = filepath.Join(parent, "rt")
		env.EUID = other
		// The first directory of the real uid on the way is an ancestor: the
		// home directory's temp root.
		bindErr(t, env, "ancestor", "is owned by uid "+strconv.Itoa(os.Geteuid())+", not root or uid "+strconv.Itoa(other))
	})
	t.Run("leaf", func(t *testing.T) {
		t.Parallel()
		// CRAZE_RUNTIME_DIR's parent is /tmp, root's, so only the leaf is
		// the real uid's.
		env := testEnv(t)
		env.EUID = other
		_, err := env.socketDir("0123abcd", NewHostID())
		want := mustCanonical(t, env.CrazeRuntimeDir) + " is owned by uid " + strconv.Itoa(os.Geteuid()) + ", not uid " + strconv.Itoa(other)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("socketDir error %v; want %q", err, want)
		}
	})
}

func TestTheCacheDirectoryIsCreated0700WhenMissing(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	bind(t, env)
	for _, p := range []string{
		filepath.Join(env.Home, cacheName),
		filepath.Join(env.Home, cacheName, crazeName),
		hostsDir(env),
	} {
		if got := perm(t, p); got != 0o700 {
			t.Errorf("%s has mode %04o, want 0700", p, got)
		}
	}
}
