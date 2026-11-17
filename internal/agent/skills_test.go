package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDiscoverSkillsCursorSkipsPlugins(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "ok", "SKILL.md"), `---
name: ok
description: keep
---
`)
	writeSkill(t, filepath.Join(cwd, ".cursor", "plugins", "p", "skills", "plug", "SKILL.md"), `---
name: plug
description: skip
---
`)
	got := DiscoverSkills(CursorProvider(), cwd, "")
	if len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("cursor skills %v", got)
	}
}

func TestDiscoverSkillsSkipsNonRegular(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, ".cursor", "skills", "fifo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	got := DiscoverSkills(CursorProvider(), cwd, "")
	for _, sk := range got {
		if sk.Name == "fifo" {
			t.Fatal("fifo SKILL.md must be skipped")
		}
	}
}

// TestCursorNameFromDirIgnoresFrontmatterName pins cursor-agent's verified
// rule (plan 009 §3.6): the directory basename is the identity outright, the
// frontmatter name is not even a fallback.
func TestCursorNameFromDirIgnoresFrontmatterName(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "dir-alpha", "SKILL.md"), `---
name: fm-alpha
description: alpha
---
`)
	got := DiscoverSkills(CursorProvider(), cwd, "")
	if len(got) != 1 || got[0].Name != "dir-alpha" {
		t.Fatalf("cursor should name by directory: %v", got)
	}
}

// TestGrokFrontmatterNameWins pins the opposite rule for grok: the
// frontmatter name is the identity, the directory is only the fallback.
func TestGrokFrontmatterNameWins(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".grok", "skills", "dir-alpha", "SKILL.md"), `---
name: fm-alpha
description: alpha
---
`)
	got := DiscoverSkills(GrokProvider(), cwd, "")
	if len(got) != 1 || got[0].Name != "fm-alpha" {
		t.Fatalf("grok should keep the frontmatter name: %v", got)
	}
}

// TestUserInvocableExcludesOnBothProviders covers both field spellings
// (cursor's user-invocable, grok's user_invocable), both falsy values (false,
// no), a quoted value, and a case-insensitive key, on both providers.
func TestUserInvocableExcludesOnBothProviders(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"bare false", "user-invocable: false"},
		{"quoted false", `user-invocable: "false"`},
		{"no", "user-invocable: no"},
		{"grok spelling", "user_invocable: false"},
		{"key case-insensitive", "User-Invocable: false"},
		// codex review: a trailing YAML comment ends the plain scalar, so the
		// value is still false and the skill is still hidden.
		{"trailing comment", "user-invocable: false # hidden from the picker"},
		// codex review: a quoted key is the same key.
		{"quoted key", `"user-invocable": false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			body := "---\nname: hidden\ndescription: hidden skill\n" + tc.line + "\n---\n"
			writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "hidden", "SKILL.md"), body)
			writeSkill(t, filepath.Join(cwd, ".grok", "skills", "hidden", "SKILL.md"), body)
			if got := DiscoverSkills(CursorProvider(), cwd, ""); len(got) != 0 {
				t.Fatalf("cursor: hidden skill must be excluded: %v", got)
			}
			if got := DiscoverSkills(GrokProvider(), cwd, ""); len(got) != 0 {
				t.Fatalf("grok: hidden skill must be excluded: %v", got)
			}
		})
	}
}

// TestNestedUserInvocableIsNotTheDocumentsOwn: cursor's frontmatter nests keys
// under `metadata:`, and the agents read only the top-level user-invocable. A
// nested one belongs to the block, so the skill stays visible — the parser must
// not flatten the indentation away and hide a skill the agent shows.
func TestNestedUserInvocableIsNotTheDocumentsOwn(t *testing.T) {
	cwd := t.TempDir()
	body := "---\nname: nested\ndescription: still visible\nmetadata:\n  user-invocable: false\n---\n"
	writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "nested", "SKILL.md"), body)
	writeSkill(t, filepath.Join(cwd, ".grok", "skills", "nested", "SKILL.md"), body)
	for _, p := range []Provider{CursorProvider(), GrokProvider()} {
		got := DiscoverSkills(p, cwd, "")
		if len(got) != 1 || got[0].Name != "nested" {
			t.Fatalf("%s: a nested user-invocable must not hide the skill: %v", p.Name(), got)
		}
	}
}

// TestDirNameWhitespaceIsNotTrimmedAway: the token check runs on the directory
// basename as it is. A trailing space would survive a trim as a perfectly good
// name, and a non-breaking space is whitespace the old ContainsAny never saw.
func TestDirNameWhitespaceIsNotTrimmedAway(t *testing.T) {
	for _, dir := range []string{"trailing ", "non\u00a0breaking"} {
		t.Run(dir, func(t *testing.T) {
			cwd := t.TempDir()
			writeSkill(t, filepath.Join(cwd, ".cursor", "skills", dir, "SKILL.md"), "---\nname: fm-name\n---\n")
			if got := DiscoverSkills(CursorProvider(), cwd, ""); len(got) != 0 {
				t.Fatalf("a directory name with whitespace must be dropped: %#v", got)
			}
		})
	}
}

// TestFrontmatterPresentNoDescriptionEmpty: a frontmatter block with no
// description key leaves the description empty (today's "(skill)" label
// downstream), rather than falling back to the body.
func TestFrontmatterPresentNoDescriptionEmpty(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "nodesc", "SKILL.md"), `---
name: nodesc
---

this body line must not leak into the description
`)
	got := DiscoverSkills(CursorProvider(), cwd, "")
	if len(got) != 1 || got[0].Description != "" {
		t.Fatalf("frontmatter without description must be empty: %v", got)
	}
}

// TestNoFrontmatterFirstBodyLineLiteral: a SKILL.md with no frontmatter block
// at all takes its description from the first non-empty body line, verbatim
// — a "# Heading" keeps its "#" rather than being read as markdown.
func TestNoFrontmatterFirstBodyLineLiteral(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "nofm", "SKILL.md"),
		"\n\n# Heading kept literally\n\nmore body\n")
	got := DiscoverSkills(CursorProvider(), cwd, "")
	if len(got) != 1 || got[0].Description != "# Heading kept literally" {
		t.Fatalf("no-frontmatter description must be the literal first body line: %v", got)
	}
}

// TestDirNameWithSpaceDroppedUnderNameFromDir: cursor names a skill by its
// directory, and a directory name containing whitespace fails the existing
// single-token check, so the skill is dropped rather than offered malformed.
func TestDirNameWithSpaceDroppedUnderNameFromDir(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "dir alpha", "SKILL.md"), `---
description: has a space in its directory name
---
`)
	got := DiscoverSkills(CursorProvider(), cwd, "")
	if len(got) != 0 {
		t.Fatalf("a directory name with a space must be dropped: %v", got)
	}
}

// TestGrokWalksAgentsSkills pins grok's second root: grok-build always scans
// .agents/skills alongside .grok/skills.
func TestGrokWalksAgentsSkills(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".agents", "skills", "agentskill", "SKILL.md"), `---
name: agentskill
description: from .agents
---
`)
	got := DiscoverSkills(GrokProvider(), cwd, "")
	if len(got) != 1 || got[0].Name != "agentskill" {
		t.Fatalf("grok should walk .agents/skills: %v", got)
	}
}

// TestGrokSkillsRootPrecedence: .grok/skills is scanned before .agents/skills,
// so a name in both keeps the .grok/skills.
func TestGrokSkillsRootPrecedence(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".grok", "skills", "dup", "SKILL.md"), `---
name: dup
description: from .grok
---
`)
	writeSkill(t, filepath.Join(cwd, ".agents", "skills", "dup", "SKILL.md"), `---
name: dup
description: from .agents
---
`)
	got := DiscoverSkills(GrokProvider(), cwd, "")
	if len(got) != 1 || got[0].Description != "from .grok" {
		t.Fatalf(".grok/skills should win over .agents/skills: %v", got)
	}
}

func writeSkill(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
