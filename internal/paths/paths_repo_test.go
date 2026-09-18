package paths

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// removedEnvAllowed are the only places the removed variable's name may
// appear, relative to the module root: this package (the tripwire and its
// tests), the discovery record (history), and the changelog (written at
// release time).
var removedEnvAllowed = []string{"internal/paths/", "discovery/", "CHANGELOG.md"}

// removedEnvSkipNames are directories never walked, wherever they sit: VCS
// metadata, virtualenvs and tool caches. Nothing a commit adds lives there.
var removedEnvSkipNames = map[string]bool{
	".git": true, ".venv": true, "venv": true, "node_modules": true,
	"__pycache__": true, ".pytest_cache": true, ".ruff_cache": true, ".mypy_cache": true,
	".cache": true,
}

// removedEnvSkipPaths are root-relative outputs .gitignore names — build,
// scratch and capture directories, the docs build's output, and agent
// worktrees (another checkout). Nothing tracked is skipped.
var removedEnvSkipPaths = map[string]bool{
	"bin": true, "dist": true, "scratch": true, "site-build": true,
	"smoke-captures": true, ".claude/worktrees": true,
}

// TestRemovedConfigEnvAppearsNowhereElse is the standing guard behind the
// tripwire: a branch that merges later and still isolates itself with the
// removed variable fails here, in CI, instead of silently reading and writing
// the developer's real ~/.craze.
func TestRemovedConfigEnvAppearsNowhereElse(t *testing.T) {
	root := moduleRoot(t)
	needle := []byte(removedConfigEnv)
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path != root && (removedEnvSkipNames[d.Name()] || removedEnvSkipPaths[rel]) {
				return filepath.SkipDir
			}
			return nil
		}
		// A worktree's .git is a file, not a directory.
		if !d.Type().IsRegular() || d.Name() == ".git" || removedEnvIsAllowed(rel) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, needle) {
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("%s was replaced by CRAZE_HOME and may appear only under %s; found in:\n  %s",
			removedConfigEnv, strings.Join(removedEnvAllowed, ", "), strings.Join(hits, "\n  "))
	}
}

func removedEnvIsAllowed(rel string) bool {
	for _, allowed := range removedEnvAllowed {
		if rel == allowed || (strings.HasSuffix(allowed, "/") && strings.HasPrefix(rel, allowed)) {
			return true
		}
	}
	return false
}

// moduleRoot finds go.mod upward from the test's working directory, which
// `go test` sets to this package's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
