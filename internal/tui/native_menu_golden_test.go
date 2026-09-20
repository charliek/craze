package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// nativeContentWorkspace is a frame workspace with content of its own: two
// commands and a skill under .claude, as a real repository ships them. The
// basename stays "ws" (frameWorkspace's rule) so the status line is stable.
func nativeContentWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := frameWorkspace(t)
	for rel, body := range files {
		path := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// TestFrameGoldenNativeMenu is plan 022 §3.2's menu, through the real program:
// a native session finds the project's own commands and skills and the user's,
// and each row is labelled with the pseudo plugin it came from — (project) and
// (user), where a cursor plugin row is labelled with its plugin id. Native
// advertises no catalog, so there is no provisional window to wait out: the
// rows are there from the first frame.
//
// The content home is a directory of this test's own, never HOME: the rows
// would otherwise be whatever the machine running the suite has installed.
func TestFrameGoldenNativeMenu(t *testing.T) {
	ws := nativeContentWorkspace(t, map[string]string{
		".claude/commands/linux-test.md":  "---\ndescription: run the Linux tests\n---\nlinux body\n",
		".claude/commands/popos-test.md":  "---\ndescription: run them on Pop!_OS\n---\npopos body\n",
		".claude/skills/release/SKILL.md": "---\nname: release\ndescription: cut a release\n---\nrelease body\n",
	})
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "commands", "linux-notes.md"),
		[]byte("---\ndescription: the user's own notes\n---\nnotes body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: home},
		nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model))

	// "lin" narrows the menu to the three rows this test is about, so the
	// golden is the labelling rather than a page of craze's own builtins.
	got, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 100, 30, "<wait:idle>/lin", FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-menu-100x30", 100, 30, got,
		[]string{"/linux-test", "(project)", "/linux-notes", "(user)"},
		// The bodies are never in the menu, and nothing here is a disk skill:
		// everything native offers is expandable, so no row is labelled
		// (skill) (§3.2).
		[]string{"linux body", "notes body", "(skill)"})
}
