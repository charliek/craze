package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstructionConfinementByAnotherRoute is a second, deliberately
// independent pass at §3.4's confinement rule. instructions_test.go's
// TestInstructionConfinement covers the cases the rule is written from; these
// were written afterwards, against the implementation rather than the rule,
// to try to reach a file it was supposed to refuse by a route the first set
// does not take. They are kept because each one failed to escape for a
// different reason, so each pins a different part of confinedPath: that the
// decision is made on the physical path rather than the name that reached it,
// that EvalSymlinks is given the raw spelling, that a resolution which fails
// is a refusal rather than a fallback, and that a non-regular target is never
// inlined.
//
// The first case is the one worth keeping above all: when the workspace is
// reached through a symlink, the link's parent and the physical root's parent
// are different directories, and an import of "../" means the second. A check
// that confined against the name craze was given would admit it.
func TestInstructionConfinementByAnotherRoute(t *testing.T) {
	secret := "PROBE-SECRET-MUST-NOT-APPEAR"

	t.Run("a chain root reached through a symlink cannot import its physical sibling", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real-repo")
		if err := os.MkdirAll(filepath.Join(real, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "outside.md"), []byte(secret), 0o644); err != nil {
			t.Fatal(err)
		}
		// CLAUDE.md climbs out of the PHYSICAL root, not the link's parent.
		if err := os.WriteFile(filepath.Join(real, "CLAUDE.md"), []byte("keep\n@../outside.md\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "via-link")
		skipUnsupported(t, "symlink", os.Symlink(real, link))
		var lines []string
		docs := loadInstructions(resolveNativeSources(link, ""), func(m string) { lines = append(lines, m) })
		all := ""
		for _, d := range docs {
			all += d.Text
		}
		if strings.Contains(all, secret) {
			t.Fatalf("the sibling of the physical root was read:\n%s", all)
		}
		if !strings.Contains(all, "keep") {
			t.Fatalf("the instruction file itself was lost:\n%s", all)
		}
		if len(lines) != 1 {
			t.Fatalf("want one diagnostic, got %q", lines)
		}
	})

	t.Run("a dangling symlink import is refused without panicking", func(t *testing.T) {
		base := t.TempDir()
		repo := filepath.Join(base, "repo")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		skipUnsupported(t, "symlink", os.Symlink(filepath.Join(base, "nowhere.md"), filepath.Join(repo, "dangling.md")))
		if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("keep\n@dangling.md\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		docs := loadInstructions(resolveNativeSources(repo, ""), nil)
		if len(docs) != 1 || !strings.Contains(docs[0].Text, "@dangling.md") {
			t.Fatalf("the dangling import should stay literal, got %+v", docs)
		}
	})

	t.Run("an import of a directory is refused", func(t *testing.T) {
		base := t.TempDir()
		repo := filepath.Join(base, "repo")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, "adir"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("keep\n@adir\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		docs := loadInstructions(resolveNativeSources(repo, ""), nil)
		if len(docs) != 1 || !strings.Contains(docs[0].Text, "@adir") {
			t.Fatalf("a directory import should stay literal, got %+v", docs)
		}
	})

	t.Run("a symlink chain that lands outside after several hops is refused", func(t *testing.T) {
		base := t.TempDir()
		repo := filepath.Join(base, "repo")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "creds.md"), []byte(secret), 0o644); err != nil {
			t.Fatal(err)
		}
		// hop1 -> hop2 -> ../creds.md, all inside the repo until the last hop.
		skipUnsupported(t, "symlink", os.Symlink(filepath.Join(repo, "hop2.md"), filepath.Join(repo, "hop1.md")))
		skipUnsupported(t, "symlink", os.Symlink(filepath.Join(base, "creds.md"), filepath.Join(repo, "hop2.md")))
		if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("keep\n@hop1.md\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var lines []string
		docs := loadInstructions(resolveNativeSources(repo, ""), func(m string) { lines = append(lines, m) })
		all := ""
		for _, d := range docs {
			all += d.Text
		}
		if strings.Contains(all, secret) {
			t.Fatalf("a multi-hop symlink escaped confinement:\n%s", all)
		}
		if len(lines) != 1 {
			t.Fatalf("want one diagnostic, got %q", lines)
		}
	})

	t.Run("a rules file reached through a symlinked chain directory stays confined", func(t *testing.T) {
		base := t.TempDir()
		repo := filepath.Join(base, "repo")
		sub := filepath.Join(repo, "svc")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sub, ".claude", "rules"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "evil.md"), []byte(secret), 0o644); err != nil {
			t.Fatal(err)
		}
		skipUnsupported(t, "symlink", os.Symlink(filepath.Join(base, "evil.md"), filepath.Join(sub, ".claude", "rules", "a.md")))
		var lines []string
		docs := loadInstructions(resolveNativeSources(sub, ""), func(m string) { lines = append(lines, m) })
		all := ""
		for _, d := range docs {
			all += d.Text
		}
		if strings.Contains(all, secret) {
			t.Fatalf("a symlinked rule escaped confinement:\n%s", all)
		}
		if len(lines) != 1 {
			t.Fatalf("want one diagnostic, got %q", lines)
		}
	})
}
