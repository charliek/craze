package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// writeRepo lays out a bare-bones .git directory with the given HEAD.
func writeRepo(t *testing.T, root, head string) string {
	t.Helper()
	dir := filepath.Join(root, ".git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if head != "" {
		if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte(head), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGitBranchVariants(t *testing.T) {
	t.Run("branch", func(t *testing.T) {
		ws := t.TempDir()
		writeRepo(t, ws, "ref: refs/heads/feature/status-rows\n")
		if got := discoverGit(ws).branch(); got != "feature/status-rows" {
			t.Fatalf("branch %q", got)
		}
	})

	t.Run("found from a subdirectory", func(t *testing.T) {
		ws := t.TempDir()
		writeRepo(t, ws, "ref: refs/heads/main\n")
		sub := filepath.Join(ws, "internal", "tui")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := discoverGit(sub).branch(); got != "main" {
			t.Fatalf("branch %q", got)
		}
	})

	t.Run("detached", func(t *testing.T) {
		ws := t.TempDir()
		writeRepo(t, ws, "3f7a91c4d2e5b6a7089f1c2d3e4b5a6978c0d1e2\n")
		if got := discoverGit(ws).branch(); got != "3f7a91c" {
			t.Fatalf("detached HEAD should be a 7-char sha, got %q", got)
		}
	})

	t.Run("worktree file", func(t *testing.T) {
		root := t.TempDir()
		main := filepath.Join(root, "repo")
		gitDir := writeRepo(t, main, "ref: refs/heads/main\n")
		wtDir := filepath.Join(gitDir, "worktrees", "wt")
		if err := os.MkdirAll(wtDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wtDir, "HEAD"), []byte("ref: refs/heads/side\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		wt := filepath.Join(root, "wt")
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := discoverGit(wt).branch(); got != "side" {
			t.Fatalf("worktree branch %q", got)
		}
	})

	t.Run("relative gitdir", func(t *testing.T) {
		root := t.TempDir()
		writeRepo(t, filepath.Join(root, "repo"), "ref: refs/heads/main\n")
		sub := filepath.Join(root, "repo", "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := discoverGit(sub).branch(); got != "main" {
			t.Fatalf("relative gitdir branch %q", got)
		}
	})

	t.Run("unreadable HEAD", func(t *testing.T) {
		ws := t.TempDir()
		writeRepo(t, ws, "")
		if got := discoverGit(ws).branch(); got != "" {
			t.Fatalf("a missing HEAD should show nothing, got %q", got)
		}
	})

	t.Run("no repo", func(t *testing.T) {
		// No .git between the workspace and the walk's boundary, the test's
		// own root: what lies above a temporary directory is the machine's
		// (a /tmp/.git that some program left failed this case, plan 037).
		// The parent's planted .git is that, here on purpose, and the control
		// is the walk to the root, which finds it.
		parent := t.TempDir()
		planted := writeRepo(t, parent, "ref: refs/heads/planted\n")
		root := filepath.Join(parent, "root")
		ws := filepath.Join(root, "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if g := discoverGit(ws); g.dir != planted {
			t.Fatalf("control: the walk to the root found %q, not the planted %q", g.dir, planted)
		}
		g := discoverGitUpTo(ws, root)
		if g.dir != "" {
			t.Fatalf("found a repo at %q", g.dir)
		}
		if got := g.branch(); got != "" {
			t.Fatalf("branch %q", got)
		}
	})
}

func TestStatusRowShowsTheBranchAndRefreshesAtTurnEnd(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeRepo(t, ws, "ref: refs/heads/main\n")
	m := startSized(t, ws)
	if !strings.Contains(plainView(m), "main") {
		t.Fatalf("the branch belongs in status row 1:\n%s", plainView(m))
	}

	// The branch is re-read when a turn ends, never polled.
	writeRepo(t, ws, "ref: refs/heads/side\n")
	if strings.Contains(statusText(m.statusRow1()), "side") {
		t.Fatal("the branch must not be re-read on every frame")
	}
	tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	if !strings.Contains(statusText(m.statusRow1()), "side") {
		t.Fatalf("the turn ended, so the branch should refresh:\n%s", statusText(m.statusRow1()))
	}
}

// ownTempRoot is this package's git walk boundary (gitBoundary, set in
// TestMain): a search that starts under base, the machine's temporary
// directory, stops at the test's own one — <base>/<Test><n>/<nnn>, what
// t.TempDir returned — so a `.git` above it, one some program left in /tmp
// among them, gives no model here a branch, and every status row and frame
// built on a temporary workspace shows none (plan 037). A start outside base
// walks to the root, as craze does.
func ownTempRoot(base string) func(start string) string {
	return func(start string) string {
		dir, err := filepath.Abs(start)
		if err != nil {
			return ""
		}
		rel, err := filepath.Rel(base, dir)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		parts := strings.Split(rel, string(filepath.Separator))
		return filepath.Join(append([]string{base}, parts[:min(len(parts), 2)]...)...)
	}
}

func TestOwnTempRoot(t *testing.T) {
	root := ownTempRoot("/tmp")
	for start, want := range map[string]string{
		"/tmp/TestX1/001":       "/tmp/TestX1/001",
		"/tmp/TestX1/001/ws/a":  "/tmp/TestX1/001",
		"/tmp/craze-x1":         "/tmp/craze-x1",
		"/tmp":                  "",
		"/home/u/proj":          "",
		"/tmpfoo/TestX1/001/ws": "",
	} {
		if got := root(start); got != want {
			t.Errorf("ownTempRoot(%q) = %q, want %q", start, got, want)
		}
	}
}

// TestNoRepoDropsTheBranchSegment: a workspace in no repository shows no
// branch segment. The test's own temporary directory bounds the search
// (ownTempRoot): the planted .git, with a HEAD, in the directory above it
// stands for one a program left in /tmp, and the control is the walk to the
// root, which finds it.
func TestNoRepoDropsTheBranchSegment(t *testing.T) {
	ws := t.TempDir()
	planted := writeRepo(t, filepath.Dir(ws), "ref: refs/heads/planted\n")
	if g := discoverGitUpTo(ws, ""); g.dir != planted || g.branch() != "planted" {
		t.Fatalf("control: the walk to the root found %q (branch %q), not the planted %q", g.dir, g.branch(), planted)
	}
	m := startSized(t, ws)
	row := statusText(m.statusRow1())
	if strings.Contains(row, " │  │ ") {
		t.Fatalf("an empty branch must not leave an empty segment: %q", row)
	}
	if !strings.HasPrefix(row, workspaceName(m.cwd)+" │ cursor") {
		t.Fatalf("row 1 without a repo: %q", row)
	}
}

func TestGitWalkHasNoDepthLimit(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, "ref: refs/heads/deep\n")
	deep := root
	for i := 0; i < 80; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Skipf("cannot nest that deep here: %v", err)
	}
	if got := discoverGit(deep).branch(); got != "deep" {
		t.Fatalf("a deep workspace reported %q; the walk must reach the root", got)
	}
}
