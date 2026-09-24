package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// physical is how a fixture path will be spelled once the resolver has been
// through it: t.TempDir() hands back a path under /var on macOS, which is a
// symlink to /private/var, and the chain is physical paths throughout.
func physical(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return real
}

func physicalAll(t *testing.T, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, physical(t, p))
	}
	return out
}

// nested creates levels directories one inside the next and returns base
// followed by each of them, outermost first — which is exactly the chain the
// resolver owes when base is a repository root.
func nested(t *testing.T, base string, levels int) []string {
	t.Helper()
	dirs := []string{writeTree(t, base, nil)}
	dir := base
	for i := 1; i <= levels; i++ {
		dir = writeTree(t, filepath.Join(dir, fmt.Sprintf("d%02d", i)), nil)
		dirs = append(dirs, dir)
	}
	return dirs
}

func gitDir(t *testing.T, repo string) {
	t.Helper()
	writeTree(t, filepath.Join(repo, ".git"), nil)
}

// TestResolveNativeSources is the seam's own table: everything a loader is
// told, for one workspace and one home. The layout is asserted whole — the
// instruction names and the rules directories are read by the loader C5 adds
// and by nothing in this commit, and a field nothing asserts is a field that
// can quietly be wrong until then.
func TestResolveNativeSources(t *testing.T) {
	base := t.TempDir()
	repo := writeTree(t, filepath.Join(base, "repo"), nil)
	gitDir(t, repo)
	home := writeTree(t, filepath.Join(base, "home"), nil)

	want := contentSources{
		Chain:    []string{physical(t, repo)},
		UserRoot: filepath.Join(home, ".claude"),
		Layout: contentLayout{
			Instructions: []string{
				"AGENTS.override.md",
				"AGENTS.md",
				"CLAUDE.md",
				"CLAUDE.local.md",
				filepath.Join(".claude", "CLAUDE.md"),
				filepath.Join(".claude", "CLAUDE.local.md"),
			},
			Rules:    filepath.Join(".claude", "rules"),
			Commands: filepath.Join(".claude", "commands"),
			SkillRoots: []string{
				filepath.Join(".claude", "skills"),
				filepath.Join(".agents", "skills"),
			},
			UserCommands:     "commands",
			UserSkills:       "skills",
			UserRules:        "rules",
			UserInstructions: []string{"CLAUDE.md"},
			Agents:           filepath.Join(".claude", "agents"),
			UserAgents:       "agents",
			PluginAgents:     "agents",
		},
		Plugins:  PluginScan{ClaudePlugins: true},
		Personas: true,
		Home:     home,
	}
	if got := resolveNativeSources(repo, home); !reflect.DeepEqual(got, want) {
		t.Fatalf("sources %+v, want %+v", got, want)
	}

	// No home is no user content at all, rather than a fallback to whatever
	// home the process happens to have: the caller isolating a test's home is
	// the caller that would have to isolate this.
	for _, home := range []string{"", "   "} {
		got := resolveNativeSources(repo, home)
		if got.UserRoot != "" || got.Home != "" {
			t.Fatalf("home %q gave UserRoot %q Home %q", home, got.UserRoot, got.Home)
		}
	}
}

// TestNativeChain is the walk up from the workspace: which ancestor a chain
// starts at, in what order, and every shape a .git comes in.
func TestNativeChain(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, base string) (workspace string, want []string)
	}{
		{
			// Deep on purpose: the walk examines the workspace and fifteen
			// ancestors, so a fixture maxChainDirs levels down is one whose
			// every examined directory this test created and none of which
			// holds a .git. A shallower one would be asserting that TMPDIR has
			// no checkout above it, which is a fact about the machine and not
			// about the chain.
			name: "no repository anywhere is the workspace alone",
			build: func(t *testing.T, base string) (string, []string) {
				dirs := nested(t, filepath.Join(base, "plain"), maxChainDirs)
				ws := dirs[len(dirs)-1]
				return ws, []string{physical(t, ws)}
			},
		},
		{
			// A workspace that does not resolve has no ancestors craze may
			// climb: /repo's content is not this session's just because the
			// session was opened on a path spelled under it. The want is the
			// unresolved spelling, which is what the chain keeps.
			name: "a workspace that is not there does not load its repository",
			build: func(t *testing.T, base string) (string, []string) {
				repo := writeTree(t, filepath.Join(base, "repo"), nil)
				gitDir(t, repo)
				missing := filepath.Join(repo, "missing")
				return missing, []string{missing}
			},
		},
		{
			// The same, reached the other way: EvalSymlinks fails on a loop
			// rather than on an absence, and the answer has to be the same.
			name: "a symlink loop does not load its repository",
			build: func(t *testing.T, base string) (string, []string) {
				repo := writeTree(t, filepath.Join(base, "repo"), nil)
				gitDir(t, repo)
				loop := filepath.Join(repo, "loop")
				if err := os.Symlink(loop, loop); err != nil {
					t.Skipf("symlink: %v", err)
				}
				return loop, []string{loop}
			},
		},
		{
			name: "the workspace is the repository root",
			build: func(t *testing.T, base string) (string, []string) {
				repo := writeTree(t, filepath.Join(base, "repo"), nil)
				gitDir(t, repo)
				return repo, []string{physical(t, repo)}
			},
		},
		{
			name: "a .git directory two levels up, outermost first",
			build: func(t *testing.T, base string) (string, []string) {
				dirs := nested(t, filepath.Join(base, "repo"), 2)
				gitDir(t, dirs[0])
				return dirs[len(dirs)-1], physicalAll(t, dirs)
			},
		},
		{
			// What a linked worktree and a submodule leave behind: a .git file
			// holding a gitdir: line. craze's own checkout is one of these.
			name: "a .git file stops the walk",
			build: func(t *testing.T, base string) (string, []string) {
				dirs := nested(t, filepath.Join(base, "worktree"), 1)
				writeTree(t, dirs[0], map[string]string{".git": "gitdir: /elsewhere/.git/worktrees/x\n"})
				return dirs[len(dirs)-1], physicalAll(t, dirs)
			},
		},
		{
			// Lstat, not Stat: the link is the marker its owner meant even
			// when what it points at has been moved away.
			name: "a dangling .git symlink stops the walk",
			build: func(t *testing.T, base string) (string, []string) {
				dirs := nested(t, filepath.Join(base, "linked"), 1)
				if err := os.Symlink(filepath.Join(base, "gone"), filepath.Join(dirs[0], ".git")); err != nil {
					t.Skipf("symlink: %v", err)
				}
				return dirs[len(dirs)-1], physicalAll(t, dirs)
			},
		},
		{
			// A submodule or a nested checkout stops at its own .git, so the
			// superproject's files are never loaded (§4, deliberate).
			name: "the nearest .git wins over an outer repository",
			build: func(t *testing.T, base string) (string, []string) {
				outer := writeTree(t, filepath.Join(base, "outer"), nil)
				gitDir(t, outer)
				dirs := nested(t, filepath.Join(outer, "inner"), 1)
				gitDir(t, dirs[0])
				return dirs[len(dirs)-1], physicalAll(t, dirs)
			},
		},
		{
			name: "a workspace reached through a symlink resolves to its real path",
			build: func(t *testing.T, base string) (string, []string) {
				dirs := nested(t, filepath.Join(base, "repo"), 1)
				gitDir(t, dirs[0])
				link := filepath.Join(base, "link")
				if err := os.Symlink(dirs[len(dirs)-1], link); err != nil {
					t.Skipf("symlink: %v", err)
				}
				return link, physicalAll(t, dirs)
			},
		},
		{
			// The workspace counts as the first of the sixteen, so a root
			// fifteen levels up is the last one the walk examines.
			name: "a repository root at the cap is still found",
			build: func(t *testing.T, base string) (string, []string) {
				dirs := nested(t, filepath.Join(base, "deep"), maxChainDirs-1)
				gitDir(t, dirs[0])
				return dirs[len(dirs)-1], physicalAll(t, dirs)
			},
		},
		{
			name: "a repository root past the cap is the workspace alone",
			build: func(t *testing.T, base string) (string, []string) {
				dirs := nested(t, filepath.Join(base, "deeper"), maxChainDirs)
				gitDir(t, dirs[0])
				ws := dirs[len(dirs)-1]
				return ws, []string{physical(t, ws)}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace, want := tc.build(t, t.TempDir())
			got := resolveNativeSources(workspace, "").Chain
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("chain %v, want %v", got, want)
			}
		})
	}
}

// TestNativeChainWorkspace pins the other half of the chain's contract: the
// innermost element is the session's own directory, which is what the plugin
// readers are given as the workspace.
func TestNativeChainWorkspace(t *testing.T) {
	dirs := nested(t, filepath.Join(t.TempDir(), "repo"), 2)
	gitDir(t, dirs[0])
	src := resolveNativeSources(dirs[len(dirs)-1], "")
	if got, want := src.workspace(), physical(t, dirs[len(dirs)-1]); got != want {
		t.Fatalf("workspace %q, want %q", got, want)
	}
	if (contentSources{}).workspace() != "" {
		t.Fatal("an empty chain must have no workspace")
	}
}
