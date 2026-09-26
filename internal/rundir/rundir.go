// Package rundir is where a craze host puts what other processes must reach —
// its control socket, and the files a resolver reads to find it — and how
// those paths are made safe to trust (plan 027 §3.8). They live in two trees,
// split by what each must satisfy:
//
//   - The socket lives in a short runtime base, <base>/<ns>/<hostId>.sock,
//     because a Unix socket's path must fit sun_path (about 104 bytes). The
//     base is the first usable of CRAZE_RUNTIME_DIR, $XDG_RUNTIME_DIR/craze,
//     /run/user/<euid>/craze (Linux) and /tmp/craze-<euid>, deduplicated by
//     canonical path, and ns keys one CRAZE_HOME (Namespace). Only a host
//     searches bases.
//   - Everything a resolver must find lives under a fixed per-user path,
//     <HOME>/.cache/craze/: the registry (hosts/<hostId>.json), each host's
//     lifetime lock (hosts/<hostId>.lock) and one lock per craze session
//     (locks/<crazeSessionId>.lock, SQ16). $HOME is the same under an SSH exec
//     as in the tab that started the host, whatever XDG_RUNTIME_DIR, CRAZE_HOME
//     or CRAZE_RUNTIME_DIR held there, so discovery depends on no environment
//     variable: a resolver reads the registry (Hosts), and each entry names
//     its socket's absolute path.
//
// Both trees are validated before anything in them is probed, bound, written
// or removed, by roost's rules (plan 027 §2.10) with two changes:
//
//   - Roost refuses any symlink on the way, which would refuse real systems
//     (Fedora Silverblue's /home, macOS's /tmp, a ~/.cache on another disk).
//     Here the parent of each chain craze owns is canonicalised once
//     (filepath.EvalSymlinks), and every component of the canonical path is
//     then an ancestor: lstat-ed, a directory, not a symlink, owned by root
//     or the euid (with links resolved, the owner check is what stops a path
//     through another user's directory), and not group- or world-writable
//     unless the sticky bit is set — or the directory is the euid's own and
//     only its user-private group can write it (Env.PrivateGID).
//   - Every directory craze names is a leaf: lstat-ed and never followed; a
//     missing one is created 0700 (then chmod-ed 0700 against the umask); a
//     present one must be a directory owned by the euid with mode exactly
//     0700. A leaf is never repaired: 0755 is refused, not chmod-ed.
//
// Every open under either tree carries O_NOFOLLOW. A host's socket and
// registry entry are unlinked at exit only while their (dev, ino) still match
// what the host recorded, and its lock file only while it still holds it. A
// session lock file is never unlinked.
//
// Every function takes an Env rather than reading the process environment, so
// a test can build hostile trees in parallel; ProcessEnv is the real one.
package rundir

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/charliek/craze/internal/paths"
)

// Env is everything this package reads from the process: the environment
// variables it honours, the effective uid, and the system roots a test
// replaces. ProcessEnv fills it from the running process.
type Env struct {
	// Home is the user's home directory (paths.HomeDir); the cache tree,
	// <Home>/.cache/craze, hangs under it. It must be absolute.
	Home string
	// CrazeDir is the craze directory (paths.CrazeDir): it keys the socket's
	// namespace. It may be relative, and is resolved against the working
	// directory when a host binds. "" is refused.
	CrazeDir string
	// CrazeRuntimeDir is $CRAZE_RUNTIME_DIR: when set, the socket base itself,
	// and an error rather than a fall-through when it is unusable.
	CrazeRuntimeDir string
	// XDGRuntimeDir is $XDG_RUNTIME_DIR; the base candidate is its "craze".
	XDGRuntimeDir string
	// EUID is the effective uid every leaf must be owned by (os.Geteuid). A
	// test sets another value to play the wrong owner.
	EUID int
	// RunUserRoot is "/run/user" on Linux and "" elsewhere, where the
	// /run/user/<euid>/craze candidate does not exist. A test points it at a
	// directory of its own.
	RunUserRoot string
	// TmpRoot is "/tmp", the parent of the last candidate, /tmp/craze-<euid>
	// ("" skips it). Never $TMPDIR: on macOS it is about 50 bytes long and
	// would eat sun_path. A test points it at a directory of its own.
	TmpRoot string
	// PrivateGID is the gid of the euid's user-private group, or 0 when it has
	// none (privateGID). A directory owned by the euid, writable by its group
	// but not by others, is an acceptable ancestor when its group is this one:
	// under user-private groups (Debian, Ubuntu, Fedora; umask 002) that group
	// has the user as its only member, so a 0775 ~/.cache is writable by no
	// one else (OpenSSH's Debian user-group-modes rule).
	PrivateGID int
}

// The environment variable names ProcessEnv reads.
const (
	envRuntimeDir    = "CRAZE_RUNTIME_DIR"
	envXDGRuntimeDir = "XDG_RUNTIME_DIR"
)

// ProcessEnv is the running process's Env.
func ProcessEnv() Env {
	euid := os.Geteuid()
	env := Env{
		Home:            paths.HomeDir(),
		CrazeDir:        paths.CrazeDir(),
		CrazeRuntimeDir: os.Getenv(envRuntimeDir),
		XDGRuntimeDir:   os.Getenv(envXDGRuntimeDir),
		EUID:            euid,
		TmpRoot:         "/tmp",
		PrivateGID:      processPrivateGID(euid),
	}
	if runtime.GOOS == "linux" {
		env.RunUserRoot = "/run/user"
	}
	return env
}

// hostIDLen is a host id's length: 12 lowercase hex digits, 48 random bits.
const hostIDLen = 12

// NewHostID mints a host id: 12 random lowercase hex digits (crypto/rand). A
// host mints one at process start; ids are never reused, which is what lets
// a host's lock file be unlinked (Host.Close).
func NewHostID() string {
	b := make([]byte, hostIDLen/2)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

// ValidHostID reports whether id is exactly 12 lowercase hex digits. An id
// that is not is refused wherever one would build a path.
func ValidHostID(id string) bool {
	if len(id) != hostIDLen {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// tokenMax is the longest opaque token: a session id a lock file is named for.
const tokenMax = 128

// ValidToken reports whether s is an opaque token that can name a file
// safely: [A-Za-z0-9._-]{1,128}, and not "." or "..". A craze session id is
// checked with it before a lock path is built from it (ClaimSession), as is
// `craze bridge --session`'s id (plan 027 §3.10).
func ValidToken(s string) bool {
	if s == "" || len(s) > tokenMax || s == "." || s == ".." {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// Namespace is the socket's directory name under the runtime base: the first
// 8 hex digits of sha256 over the absolute, cleaned craze directory. That is
// one namespace per user and CRAZE_HOME; a relative craze directory resolves
// against the working directory now. "" — no home directory, or the removed
// config variable still set (paths.CrazeDir, paths.CheckEnv) — is refused.
func Namespace(crazeDir string) (string, error) {
	if crazeDir == "" {
		return "", errors.New("rundir: no craze directory, so no control socket namespace")
	}
	abs, err := filepath.Abs(crazeDir)
	if err != nil {
		return "", fmt.Errorf("rundir: resolve the craze directory %q: %w", crazeDir, err)
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:8], nil
}
