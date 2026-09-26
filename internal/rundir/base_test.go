package rundir

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// base is the runtime base h's socket went under: <base>/<ns>/<id>.sock.
func base(h *Host) string { return filepath.Dir(filepath.Dir(h.Socket())) }

func TestTheExplicitRuntimeDirWins(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	env.XDGRuntimeDir = shortDir(t)
	env.TmpRoot = shortDir(t)
	h := bind(t, env)
	if want := mustCanonical(t, env.CrazeRuntimeDir); base(h) != want {
		t.Fatalf("socket %s is not under CRAZE_RUNTIME_DIR %s", h.Socket(), want)
	}
	if exists(t, filepath.Join(env.XDGRuntimeDir, crazeName)) {
		t.Error("the XDG candidate was built although CRAZE_RUNTIME_DIR won")
	}
}

func TestARelativeExplicitRuntimeDirIsAnError(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	env.CrazeRuntimeDir = "rel/run"
	env.TmpRoot = shortDir(t)
	bindErr(t, env, "CRAZE_RUNTIME_DIR", "not an absolute path")
}

func TestAnUnusableExplicitRuntimeDirDoesNotFallThrough(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	chmod(t, env.CrazeRuntimeDir, 0o755)
	env.XDGRuntimeDir = shortDir(t)
	env.TmpRoot = shortDir(t)
	bindErr(t, env, "CRAZE_RUNTIME_DIR", "mode 0755")
	if exists(t, filepath.Join(env.XDGRuntimeDir, crazeName)) {
		t.Error("an unusable CRAZE_RUNTIME_DIR fell through to XDG_RUNTIME_DIR")
	}
}

func TestALongSocketPathIsRefusedBeforeBind(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	parent := env.CrazeRuntimeDir
	env.CrazeRuntimeDir = filepath.Join(parent, strings.Repeat("x", 80))
	env.TmpRoot = shortDir(t)
	bindErr(t, env, "CRAZE_RUNTIME_DIR", "over the 100-byte limit", "set CRAZE_RUNTIME_DIR to a shorter absolute path")
	if names, _ := os.ReadDir(parent); len(names) != 0 {
		t.Fatalf("%s holds %d entries after the refusal; nothing may be created", parent, len(names))
	}
}

func TestAnUnusableXDGRuntimeDirFallsThrough(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T, env *Env){
		"relative": func(t *testing.T, env *Env) { env.XDGRuntimeDir = "run/user" },
		"a 0755 leaf": func(t *testing.T, env *Env) {
			env.XDGRuntimeDir = shortDir(t)
			mkdir(t, filepath.Join(env.XDGRuntimeDir, crazeName), 0o755)
		},
		"a symlinked leaf": func(t *testing.T, env *Env) {
			env.XDGRuntimeDir = shortDir(t)
			symlink(t, shortDir(t), filepath.Join(env.XDGRuntimeDir, crazeName))
		},
		"too long": func(t *testing.T, env *Env) {
			env.XDGRuntimeDir = filepath.Join(shortDir(t), strings.Repeat("x", 80))
			mkdir(t, env.XDGRuntimeDir, 0o700)
		},
		"missing": func(t *testing.T, env *Env) {
			env.XDGRuntimeDir = filepath.Join(shortDir(t), "gone")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			env.CrazeRuntimeDir = ""
			setup(t, &env)
			env.TmpRoot = shortDir(t)
			h := bind(t, env)
			want := filepath.Join(mustCanonical(t, env.TmpRoot), "craze-"+strconv.Itoa(env.EUID))
			if base(h) != want {
				t.Fatalf("socket %s; want it under the /tmp candidate %s", h.Socket(), want)
			}
			if name == "too long" && exists(t, filepath.Join(env.XDGRuntimeDir, crazeName)) {
				t.Error("an overlong XDG candidate was built before it was refused")
			}
		})
	}
}

func TestAValidXDGRuntimeDirIsTheBase(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	env.CrazeRuntimeDir = ""
	env.XDGRuntimeDir = shortDir(t)
	env.TmpRoot = shortDir(t)
	h := bind(t, env)
	if want := filepath.Join(mustCanonical(t, env.XDGRuntimeDir), crazeName); base(h) != want {
		t.Fatalf("socket %s; want it under %s", h.Socket(), want)
	}
	if got := perm(t, base(h)); got != 0o700 {
		t.Fatalf("the base was created with mode %04o, want 0700", got)
	}
}

func TestTheRunUserCandidateNeedsA0700DirectoryOwnedByTheUser(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mode os.FileMode // 0: the directory is missing
		euid int         // 0: the real euid
		used bool
	}{
		"0700":        {mode: 0o700, used: true},
		"0755":        {mode: 0o755},
		"missing":     {},
		"wrong owner": {mode: 0o700, euid: os.Geteuid() + 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			env.CrazeRuntimeDir = ""
			env.RunUserRoot = shortDir(t)
			env.TmpRoot = shortDir(t)
			if tc.euid != 0 {
				env.EUID = tc.euid
			}
			runUser := filepath.Join(env.RunUserRoot, strconv.Itoa(env.EUID))
			if tc.mode != 0 {
				mkdir(t, runUser, tc.mode)
			}
			if tc.euid != 0 {
				// Every directory of the real uid is refused for another, the
				// cache tree's included, so the base search is asked
				// directly: the candidate is skipped for its owner.
				_, err := env.socketDir("0123abcd", NewHostID())
				if err == nil || !strings.Contains(err.Error(), runUser+": is owned by uid") {
					t.Fatalf("socketDir error %v; want the /run/user candidate skipped for its owner", err)
				}
				return
			}
			h := bind(t, env)
			want := filepath.Join(mustCanonical(t, env.TmpRoot), "craze-"+strconv.Itoa(env.EUID))
			if tc.used {
				want = filepath.Join(mustCanonical(t, runUser), crazeName)
			}
			if base(h) != want {
				t.Fatalf("socket %s; want it under %s", h.Socket(), want)
			}
		})
	}
}

func TestXDGAtRunUserIsTriedOnce(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	env.CrazeRuntimeDir = ""
	env.RunUserRoot = shortDir(t)
	runUser := filepath.Join(env.RunUserRoot, strconv.Itoa(env.EUID))
	mkdir(t, runUser, 0o700)
	env.XDGRuntimeDir = runUser
	mkdir(t, filepath.Join(runUser, crazeName), 0o755) // XDG's leaf fails
	err := bindErr(t, env, "XDG_RUNTIME_DIR: ", "mode 0755", "already named")
	if n := strings.Count(err.Error(), "mode 0755"); n != 1 {
		t.Fatalf("the shared base was validated %d times, want once: %v", n, err)
	}
}

func TestNoUsableBaseNamesEachCandidate(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	env.CrazeRuntimeDir = ""
	env.RunUserRoot = shortDir(t) // no <euid> under it
	env.TmpRoot = shortDir(t)
	mkdir(t, filepath.Join(env.TmpRoot, "craze-"+strconv.Itoa(env.EUID)), 0o755)
	bindErr(t, env,
		"XDG_RUNTIME_DIR: not set",
		filepath.Join(env.RunUserRoot, strconv.Itoa(env.EUID))+": does not exist",
		"craze-"+strconv.Itoa(env.EUID)+": ", "mode 0755",
		"set CRAZE_RUNTIME_DIR")
}
