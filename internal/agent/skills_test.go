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

func writeSkill(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
