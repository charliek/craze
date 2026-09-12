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
		// A temp dir has no .git anywhere between it and the root.
		g := discoverGit(t.TempDir())
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
	if !strings.Contains(m.View(), "main") {
		t.Fatalf("the branch belongs in status row 1:\n%s", m.View())
	}

	// The branch is re-read when a turn ends, never polled.
	writeRepo(t, ws, "ref: refs/heads/side\n")
	if strings.Contains(m.statusRow1(), "side") {
		t.Fatal("the branch must not be re-read on every frame")
	}
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	if !strings.Contains(m.statusRow1(), "side") {
		t.Fatalf("the turn ended, so the branch should refresh:\n%s", m.statusRow1())
	}
}

func TestNoRepoDropsTheBranchSegment(t *testing.T) {
	m := startSized(t, t.TempDir())
	row := m.statusRow1()
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
