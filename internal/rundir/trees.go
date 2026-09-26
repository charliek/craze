package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SocketPathMax is the longest socket path a host binds: sun_path is 104
// bytes on Darwin and 108 on Linux, the terminating NUL included, and 100
// leaves a margin on both. A longer path is refused before anything is
// created under its base.
const SocketPathMax = 100

// The cache tree's names: <Home>/.cache/craze/{hosts,locks}.
const (
	cacheName  = ".cache"
	crazeName  = "craze"
	hostsName  = "hosts"
	locksName  = "locks"
	tmpPrefix  = "craze-"
	sockSuffix = ".sock"
)

// cacheSubdir validates the cache tree down to <Home>/.cache/craze/<sub> and
// returns that directory's canonical path. <Home>/.cache is created 0700 when
// it is missing (and create is set), inside a home already validated; it is
// then canonicalised — a ~/.cache symlinked to another disk is fine — and
// every component of it validated as an ancestor. craze and <sub> are leaves.
// With create unset nothing is made, and a missing directory is an error
// wrapping fs.ErrNotExist.
func (env Env) cacheSubdir(sub string, create bool) (string, error) {
	if env.Home == "" {
		return "", errors.New("rundir: no home directory for the craze cache tree")
	}
	if !filepath.IsAbs(env.Home) {
		return "", fmt.Errorf("rundir: the home directory %q is not absolute", env.Home)
	}
	cache := filepath.Join(env.Home, cacheName)
	if _, err := os.Lstat(cache); errors.Is(err, fs.ErrNotExist) {
		if !create {
			return "", fmt.Errorf("rundir: %s: %w", cache, fs.ErrNotExist)
		}
		home, err := canonical(env.Home)
		if err != nil {
			return "", fmt.Errorf("rundir: resolve the home directory: %w", err)
		}
		if err := env.checkAncestors(home); err != nil {
			return "", fmt.Errorf("rundir: the home directory: %w", err)
		}
		if err := mkdirPrivate(filepath.Join(home, cacheName)); err != nil {
			return "", fmt.Errorf("rundir: create %s: %w", cache, err)
		}
	}
	canon, err := canonical(cache)
	if err != nil {
		return "", fmt.Errorf("rundir: resolve %s: %w", cache, err)
	}
	if err := env.checkAncestors(canon); err != nil {
		return "", fmt.Errorf("rundir: the cache directory: %w", err)
	}
	dir := canon
	for _, name := range []string{crazeName, sub} {
		dir = filepath.Join(dir, name)
		if err := env.leaf(dir, create); err != nil {
			return "", fmt.Errorf("rundir: the cache tree: %w", err)
		}
	}
	return dir, nil
}

// candidate is one socket base to try: the leaf name under a parent craze
// does not own.
type candidate struct {
	label    string // how an error names it
	parent   string // absolute; canonicalised before use
	name     string // the base's own name under parent
	explicit bool   // CRAZE_RUNTIME_DIR: a failure is an error, not a fall-through
	skip     string // why it is not tried at all, or ""
}

// candidates is the four socket bases in their fixed order (plan 027 §3.8).
// A candidate that cannot be tried at all carries its reason; the Linux-only
// third is left out where RunUserRoot is "".
func (env Env) candidates() ([]candidate, error) {
	var out []candidate
	if d := env.CrazeRuntimeDir; d != "" {
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("rundir: %s=%q is not an absolute path", envRuntimeDir, d)
		}
		d = filepath.Clean(d)
		if d == "/" {
			return nil, fmt.Errorf("rundir: %s=/ cannot be a craze-owned directory", envRuntimeDir)
		}
		out = append(out, candidate{label: envRuntimeDir, parent: filepath.Dir(d), name: filepath.Base(d), explicit: true})
	}
	xdg := candidate{label: envXDGRuntimeDir, parent: env.XDGRuntimeDir, name: crazeName}
	switch {
	case env.XDGRuntimeDir == "":
		xdg.skip = "not set"
	case !filepath.IsAbs(env.XDGRuntimeDir):
		xdg.skip = fmt.Sprintf("%q is not absolute", env.XDGRuntimeDir)
	}
	out = append(out, xdg)
	if env.RunUserRoot != "" {
		p := filepath.Join(env.RunUserRoot, strconv.Itoa(env.EUID))
		out = append(out, candidate{label: p, parent: p, name: crazeName, skip: env.runUserSkip(p)})
	}
	tmp := candidate{label: filepath.Join(env.TmpRoot, tmpPrefix+strconv.Itoa(env.EUID)), parent: env.TmpRoot,
		name: tmpPrefix + strconv.Itoa(env.EUID)}
	if env.TmpRoot == "" {
		tmp.skip = "no temporary directory"
	}
	return append(out, tmp), nil
}

// runUserSkip is why /run/user/<euid> is not a candidate: it must exist, be
// owned by the euid and be 0700 (logind's own shape); "" when it is.
func (env Env) runUserSkip(p string) string {
	fi, err := os.Lstat(p)
	if err != nil {
		return "does not exist"
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return "is not a directory"
	}
	if uid := int(statOf(fi).Uid); uid != env.EUID {
		return fmt.Sprintf("is owned by uid %d", uid)
	}
	if mode := permOf(fi); mode != modeLeaf {
		return fmt.Sprintf("has mode %04o, not 0700", mode)
	}
	return ""
}

// socketDir chooses the socket base — the first usable candidate, each
// canonical base tried once — and returns the validated <base>/<ns>, both
// leaves created as needed. A candidate is usable when its parent resolves,
// the socket path it would give fits SocketPathMax (checked before anything is
// created), every component of its canonical parent validates as an ancestor,
// and the base and <ns> validate as leaves. An explicit CRAZE_RUNTIME_DIR that
// is not usable is an error; any other candidate falls through, and when none
// is usable the error names each and why.
func (env Env) socketDir(ns, hostID string) (string, error) {
	cands, err := env.candidates()
	if err != nil {
		return "", err
	}
	tried := map[string]string{} // canonical base → the label that tried it
	var reasons []string
	for _, c := range cands {
		if c.skip != "" {
			reasons = append(reasons, c.label+": "+c.skip)
			continue
		}
		base, reason := env.tryBase(c, ns, hostID, tried)
		if reason == "" {
			return filepath.Join(base, ns), nil
		}
		if c.explicit {
			return "", fmt.Errorf("rundir: %s: %s", c.label, reason)
		}
		reasons = append(reasons, c.label+": "+reason)
	}
	return "", fmt.Errorf("rundir: no usable directory for the control socket (%s); set %s to a short absolute path you own",
		strings.Join(reasons, "; "), envRuntimeDir)
}

// tryBase validates one candidate, recording its canonical base in tried; it
// returns the canonical base, or why the candidate is not usable.
func (env Env) tryBase(c candidate, ns, hostID string, tried map[string]string) (string, string) {
	parent, err := canonical(c.parent)
	if err != nil {
		return "", err.Error()
	}
	base := filepath.Join(parent, c.name)
	if by, ok := tried[base]; ok {
		return "", fmt.Sprintf("%s is the directory %s already named", base, by)
	}
	tried[base] = c.label
	sock := filepath.Join(base, ns, hostID+sockSuffix)
	if len(sock) > SocketPathMax {
		return "", fmt.Sprintf("the socket path %s is %d bytes, over the %d-byte limit; set %s to a shorter absolute path",
			sock, len(sock), SocketPathMax, envRuntimeDir)
	}
	if err := env.checkAncestors(parent); err != nil {
		return "", err.Error()
	}
	for _, dir := range []string{base, filepath.Join(base, ns)} {
		if err := env.leaf(dir, true); err != nil {
			return "", err.Error()
		}
	}
	return base, ""
}
