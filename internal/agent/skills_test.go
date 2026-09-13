package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverSkillsInspectPluginAndUserInvocable(t *testing.T) {
	raw := `{
		"skills": [
			{"name":"bundled-one","description":"bundled","source":{"type":"bundled","path":"/b/SKILL.md"},"userInvocable":true},
			{"name":"plugin-one","description":"from plugin","source":{"type":"plugin","path":"/p/SKILL.md"}},
			{"name":"hidden","description":"no","source":{"type":"user","path":"/h"},"userInvocable":false},
			{"name":"no-path","description":"missing path","source":{"type":"user"}},
			{"name":"PLUGIN-ONE","description":"dup"}
		]
	}`
	run := func(string, []string, string, time.Duration) ([]byte, error) { return []byte(raw), nil }
	got := discoverSkills(GrokProvider(), t.TempDir(), t.TempDir(), "/bin/grok", run)
	names := skillNames(got)
	if strings.Join(names, ",") != "bundled-one,plugin-one,no-path" {
		t.Fatalf("skills %v", names)
	}
	if got[1].Description != "from plugin" {
		t.Fatalf("plugin description %q", got[1].Description)
	}
}

func TestDiscoverSkillsInspectFailureFallsBackToFilesystem(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".grok", "skills", "disk", "SKILL.md"), `---
name: disk-skill
description: from disk
---
`)
	writeSkill(t, filepath.Join(cwd, ".cursor", "skills", "cursor-only", "SKILL.md"), `---
name: cursor-only
description: must not appear in grok fallback
---
`)
	run := func(string, []string, string, time.Duration) ([]byte, error) {
		return nil, fmt.Errorf("inspect failed")
	}
	got := discoverSkills(GrokProvider(), cwd, home, "/bin/grok", run)
	if len(got) != 1 || got[0].Name != "disk-skill" {
		t.Fatalf("fallback %v", got)
	}
}

func TestDiscoverSkillsInspectDecodeFailureFallsBack(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".grok", "skills", "fb", "SKILL.md"), `---
name: fb
description: fallback
---
`)
	run := func(string, []string, string, time.Duration) ([]byte, error) {
		return []byte("not json"), nil
	}
	got := discoverSkills(GrokProvider(), cwd, "", "/bin/grok", run)
	if len(got) != 1 || got[0].Name != "fb" {
		t.Fatalf("decode fallback %v", got)
	}
}

func TestDiscoverSkillsInspectEmptyCatalogIsSuccess(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".grok", "skills", "disk", "SKILL.md"), `---
name: disk-skill
description: should not appear
---
`)
	run := func(string, []string, string, time.Duration) ([]byte, error) {
		return []byte(`{"skills":[]}`), nil
	}
	got := discoverSkills(GrokProvider(), cwd, "", "/bin/grok", run)
	if got == nil {
		got = []Skill{}
	}
	if len(got) != 0 {
		t.Fatalf("empty inspect must not fall back, got %v", got)
	}
}

func TestDiscoverSkillsInspectCap(t *testing.T) {
	skills := make([]map[string]any, 0, maxSkillFiles+10)
	for i := 0; i < maxSkillFiles+10; i++ {
		skills = append(skills, map[string]any{
			"name":        fmt.Sprintf("s%03d", i),
			"description": "x",
			"source":      map[string]string{"type": "bundled", "path": "/x"},
		})
	}
	raw, err := json.Marshal(map[string]any{"skills": skills})
	if err != nil {
		t.Fatal(err)
	}
	run := func(string, []string, string, time.Duration) ([]byte, error) { return raw, nil }
	got := discoverSkills(GrokProvider(), "", "", "/bin/grok", run)
	if len(got) != maxSkillFiles {
		t.Fatalf("capped %d, want %d", len(got), maxSkillFiles)
	}
}

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
	got := DiscoverSkills(CursorProvider(), cwd, "", "")
	if len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("cursor skills %v", got)
	}
}

func TestDiscoverSkillsFakeAgentInspect(t *testing.T) {
	got := DiscoverSkills(GrokProvider(), t.TempDir(), t.TempDir(), fakeAgentPath(t))
	names := skillNames(got)
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "plugin-one") || !strings.Contains(joined, "bundled-one") {
		t.Fatalf("fake inspect %v", names)
	}
	if strings.Contains(joined, "hidden") {
		t.Fatalf("userInvocable false leaked: %v", names)
	}
	if strings.Contains(joined, "PLUGIN-ONE") {
		t.Fatalf("duplicate not first-wins: %v", names)
	}
	if !strings.Contains(joined, "no-path") {
		t.Fatalf("missing path dropped: %v", names)
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

func skillNames(in []Skill) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Name
	}
	return out
}
