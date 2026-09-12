package tui

import (
	"os"
	"path/filepath"
	"strings"
)

// gitShortSHA is how much of a detached HEAD the status row shows.
const gitShortSHA = 7

// gitInfo is the repository craze found when it started. dir is whatever
// directory holds HEAD — the `.git` directory itself, or the directory a
// `.git` file points at for a linked worktree — and is empty when the
// workspace is not in a repository at all.
type gitInfo struct{ dir string }

// discoverGit walks up from start looking for `.git`. It runs once, at start:
// the status row re-reads HEAD from the directory this found, and never
// repeats the search.
func discoverGit(start string) gitInfo {
	dir, err := filepath.Abs(start)
	if err != nil {
		return gitInfo{}
	}
	// filepath.Dir strictly shortens a cleaned path until it reaches the root,
	// where it returns the root itself, so this always terminates. There is no
	// depth cap: one would only turn a deep workspace into a missing branch.
	for {
		if d, ok := gitDirAt(dir); ok {
			return gitInfo{dir: d}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitInfo{}
		}
		dir = parent
	}
}

// gitDirAt resolves one directory's `.git` entry: a directory is the git dir
// itself, a file carries `gitdir: <path>` (a linked worktree or a submodule),
// relative to the directory the file is in.
func gitDirAt(dir string) (string, bool) {
	p := filepath.Join(dir, ".git")
	st, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	if st.IsDir() {
		return p, true
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(ln), "gitdir:")
		if !ok {
			continue
		}
		target := strings.TrimSpace(rest)
		if target == "" {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		if st, err := os.Stat(target); err != nil || !st.IsDir() {
			return "", false
		}
		return target, true
	}
	return "", false
}

// branch reads HEAD: a symbolic ref is the branch name, anything else is a
// detached commit shown as a short SHA. An unreadable HEAD shows nothing, so
// the status row simply drops the segment.
func (g gitInfo) branch() string {
	if g.dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(g.dir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(b))
	if rest, ok := strings.CutPrefix(head, "ref:"); ok {
		ref := strings.TrimSpace(rest)
		return sanitizeLine(strings.TrimPrefix(ref, "refs/heads/"))
	}
	if len(head) > gitShortSHA {
		head = head[:gitShortSHA]
	}
	return sanitizeLine(head)
}
