package rundir

import (
	"io/fs"
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

func TestASymlinkedAncestorIntoAGroupWritableDirectoryIsRefused(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	shared := filepath.Join(shortDir(t), "shared")
	mkdir(t, shared, 0o770)
	symlink(t, shared, filepath.Join(env.Home, cacheName))
	bindErr(t, env, "ancestor "+mustCanonical(t, shared)+" is group- or world-writable without the sticky bit (mode 0770)")
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

// groupWritableParent is a CRAZE_RUNTIME_DIR under a parent of mode, owned by
// the test's uid and its egid, and that gid.
func groupWritableParent(t *testing.T, env *Env, mode os.FileMode) int {
	t.Helper()
	gid := os.Getegid()
	if gid <= 0 {
		t.Skip("needs a non-root egid")
	}
	parent := env.CrazeRuntimeDir
	// A directory made under macOS's /tmp takes /tmp's group (wheel); the
	// user may give it any group of its own.
	if err := os.Chown(parent, -1, gid); err != nil {
		t.Fatal(err)
	}
	chmod(t, parent, mode)
	env.CrazeRuntimeDir = filepath.Join(parent, "rt")
	return gid
}

// localNSSwitch is an nsswitch.conf whose users and groups are all local:
// the owner's own box, where the user-private-group exemption applies.
const localNSSwitch = "# comment\npasswd:         files systemd\ngroup:          files systemd\nhosts: files dns\nnetgroup: nis\n"

func TestAUserPrivateGroupAncestorIsAccepted(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	env.PrivateGID = groupWritableParent(t, &env, 0o775)
	env.NSSwitch = localNSSwitch
	bind(t, env)
}

func TestTheUserPrivateGroupExemptionNeedsLocalAccounts(t *testing.T) {
	t.Parallel()
	for name, nsswitch := range map[string]string{
		"sssd":                     "passwd: files sss\ngroup: files sss\n",
		"ldap for groups":          "passwd: files systemd\ngroup: files ldap\n",
		"compat":                   "passwd: compat\ngroup: compat\n",
		"no nsswitch.conf":         "",
		"no group line":            "passwd: files\n",
		"initgroups from winbind":  localNSSwitch + "initgroups: files winbind\n",
		"a line this cannot parse": "passwd files\ngroup: files\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			env.PrivateGID = groupWritableParent(t, &env, 0o775)
			env.NSSwitch = nsswitch
			parent := mustCanonical(t, filepath.Dir(env.CrazeRuntimeDir))
			bindErr(t, env, "ancestor "+parent+" is group- or world-writable without the sticky bit (mode 0775)",
				"run: chmod g-w "+parent)
		})
	}
}

func TestLocalAccounts(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"passwd: files\ngroup: files\n":                                                 true,
		"passwd: files systemd\ngroup: files systemd\n":                                 true,
		"passwd: files [SUCCESS=merge] systemd\ngroup: files [SUCCESS=merge] systemd\n": true,
		"passwd:\tfiles[NOTFOUND=return]  systemd\r\ngroup :files\n":                    true,
		"passwd: files # ldap\ngroup: systemd files\nshadow: files sss\n":               true,
		localNSSwitch + "initgroups: files\n":                                           true,
		"passwd: compat\ngroup: compat\n":                                               false,
		"passwd: files sss\ngroup: files sss\n":                                         false,
		"passwd: files ldap\ngroup: files\n":                                            false,
		"passwd: files\ngroup: files nis\n":                                             false,
		"passwd: files winbind\ngroup: files\n":                                         false,
		"passwd: Files\ngroup: files\n":                                                 false,
		"passwd: files\n":                                                               false,
		"group: files\n":                                                                false,
		"# passwd: files\n# group: files\n":                                             false,
		"":                                                                              false,
		"passwd:\ngroup: files\n":                                                       false,
		"passwd: [NOTFOUND=return] files\ngroup: files\n":                               false,
		"passwd: files [NOTFOUND=return\ngroup: files\n":                                false,
		"passwd: files [a=[b]]\ngroup: files\n":                                         false,
		"passwd: files\npasswd: ldap\ngroup: files\n":                                   false,
		"passwd: files\nPASSWD: ldap\ngroup: files\n":                                   false,
		"passwd: files\ngroup: files\ninitgroups: ldap\n":                               false,
		"passwd files\ngroup: files\n":                                                  false,
		"passwd: files\ngroup: files\ngarbage\n":                                        false,
		": files\npasswd: files\ngroup: files\n":                                        false,
	} {
		if got := localAccounts(in); got != want {
			t.Errorf("localAccounts(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProcessAccountsReadsLinuxOnly(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		nsswitchPath: localNSSwitch,
		passwdPath:   "u:x:1000:1000::/home/u:/bin/sh\n",
		groupPath:    "u:x:1000:\n",
	}
	read := func(missing string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			s, ok := files[p]
			if !ok || p == missing {
				return nil, fs.ErrNotExist
			}
			return []byte(s), nil
		}
	}
	for name, tc := range map[string]struct {
		goos, missing string
		gid           int
		nsswitch      string
	}{
		"linux":            {"linux", "", 1000, localNSSwitch},
		"darwin, never":    {"darwin", "", 0, ""},
		"no nsswitch.conf": {"linux", nsswitchPath, 1000, ""},
		"no passwd":        {"linux", passwdPath, 0, localNSSwitch},
		"no group":         {"linux", groupPath, 0, localNSSwitch},
	} {
		gid, nsswitch := processAccounts(tc.goos, 1000, read(tc.missing))
		if gid != tc.gid || nsswitch != tc.nsswitch {
			t.Errorf("%s: processAccounts = %d, %q; want %d, %q", name, gid, nsswitch, tc.gid, tc.nsswitch)
		}
		exempt := Env{EUID: 1000, PrivateGID: gid, NSSwitch: nsswitch}.privateGroupWritable(1000, 1000, 0o775)
		if want := name == "linux"; exempt != want {
			t.Errorf("%s: a 0775 directory of the user's private group exempt = %v, want %v", name, exempt, want)
		}
	}
}

func TestAGroupWritableAncestorOfAnotherGroupIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mode    os.FileMode
		private func(gid int) int
	}{
		"no private group":       {0o775, func(int) int { return 0 }},
		"another private group":  {0o775, func(gid int) int { return gid + 1 }},
		"world-writable as well": {0o777, func(gid int) int { return gid }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			gid := groupWritableParent(t, &env, tc.mode)
			env.PrivateGID = tc.private(gid)
			env.NSSwitch = localNSSwitch
			bindErr(t, env, "is group- or world-writable without the sticky bit")
		})
	}
	// Another owner's directory never qualifies, whatever its group: the
	// exemption is for the user's own directories.
	env := Env{EUID: 1000, PrivateGID: 1000, NSSwitch: localNSSwitch}
	if env.privateGroupWritable(1001, 1000, 0o775) || env.privateGroupWritable(0, 1000, 0o775) {
		t.Fatal("a group-writable directory of another owner passed as the user's own")
	}
	if !env.privateGroupWritable(1000, 1000, 0o775) {
		t.Fatal("the user's own directory writable by its private group was refused")
	}
}

func TestPrivateGIDFollowsTheUserGroupModesRule(t *testing.T) {
	t.Parallel()
	const passwd = "root:x:0:0:root:/root:/bin/bash\n# a comment\nu:x:1000:1000:U:/home/u:/bin/sh\nv:x:1001:1001::/home/v:/bin/sh\nw:x:1002:100::/home/w:/bin/sh\n"
	for name, tc := range map[string]struct {
		uid           int
		passwd, group string
		want          int
	}{
		"a private group":           {1000, passwd, "root:x:0:\nu:x:1000:\nv:x:1001:\n", 1000},
		"the user as its member":    {1000, passwd, "u:x:1000:u\n", 1000},
		"another member":            {1000, passwd, "u:x:1000:u,v\n", 0},
		"another name":              {1000, passwd, "staff:x:1000:\n", 0},
		"a shared primary group":    {1002, passwd, "users:x:100:\n", 0},
		"another account's primary": {1000, passwd + "x:x:1003:1000::/:/bin/sh\n", "u:x:1000:\n", 0},
		"no group entry":            {1000, passwd, "v:x:1001:\n", 0},
		"no passwd entry":           {4242, passwd, "u:x:1000:\n", 0},
		"root":                      {0, passwd, "root:x:0:\n", 0},
		"two entries for the gid":   {1000, passwd, "u:x:1000:\nu2:x:1000:\n", 0},
	} {
		if got := privateGID(tc.uid, tc.passwd, tc.group); got != tc.want {
			t.Errorf("%s: privateGID = %d, want %d", name, got, tc.want)
		}
	}
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
