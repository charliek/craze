package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

func writeSkillMD(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func catalogByName(m Model, name string) (slashItem, bool) {
	want := strings.ToLower(name)
	for _, it := range m.slashCatalog() {
		if strings.ToLower(it.Name) == want {
			return it, true
		}
	}
	return slashItem{}, false
}

func TestHelpOverlayFitsWithManyDiskSkills(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	long := strings.Repeat("word ", 40)
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("skill%02d", i)
		writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", name, "SKILL.md"), fmt.Sprintf(`---
name: %s
description: %s
---
`, name, long))
	}
	m := startSized(t, ws)
	m.input.SetValue("/help")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	view := plainView(m)
	if h := lipgloss.Height(view); h != 24 {
		t.Fatalf("help view is %d rows with disk skills:\n%s", h, view)
	}
	// 30 skills with 200-character descriptions cannot push the box past the
	// transcript region: it scrolls instead.
	if r, tr := m.lay.Dialog, m.lay.Region(regionTranscript); r.Y < tr.Top || r.Y+r.H > tr.Bottom {
		t.Fatalf("the box %+v escaped the transcript %+v:\n%s", r, tr, view)
	}
	if !strings.Contains(view, "send the draft") {
		t.Fatalf("the top of the help box is missing:\n%s", view)
	}
}

func TestDiskSkillTabCompleteAndHelp(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "demo", "SKILL.md"), `---
name: demo
description: Disk demo skill
---

body must not be injected
`)
	m := startSized(t, ws)
	// The help box is cropped, not scrolled, and the two status rows leave one
	// row less of it at 24; a taller terminal keeps the whole catalog visible.
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = tm.(Model)
	m.input.SetValue("/demo")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = tm.(Model)
	if m.input.Value() != "/demo " {
		t.Fatalf("complete %q", m.input.Value())
	}

	m.input.SetValue("/help")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogHelp {
		t.Fatal("expected the help dialog")
	}
	// The catalog is the last section of a box that scrolls, so the skill row
	// is only on screen once the box has been paged to the bottom.
	for i := 0; i < 10; i++ {
		m = pressKey(t, m, tea.KeyPgDown)
	}
	view := plainView(m)
	if !strings.Contains(view, "/demo") {
		t.Fatalf("help missing /demo:\n%s", view)
	}
	if !strings.Contains(view, "(skill)") {
		t.Fatalf("help missing (skill):\n%s", view)
	}
	it, ok := catalogByName(m, "demo")
	if !ok || !it.Skill {
		t.Fatalf("demo catalog %#v ok=%v", it, ok)
	}
	if it.labeledDesc() != "Disk demo skill (skill)" {
		t.Fatalf("labeled %q", it.labeledDesc())
	}
}

func TestDiskSkillEnterSendsPrompt(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "demo", "SKILL.md"), `---
name: demo
description: Disk demo skill
---

<skill>do not wrap</skill>
`)
	m := startSized(t, ws)
	m.input.SetValue("/demo")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("/demo should send a prompt")
	}
	got := strings.Join(texts(m, entryUser), "")
	if got != "/demo" {
		t.Fatalf("user %q", got)
	}
	if strings.Contains(got, "<skill") || strings.Contains(got, "<loaded_skill") {
		t.Fatalf("must not wrap skill body: %q", got)
	}

	m = startSized(t, ws)
	m.input.SetValue("/demo args")
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("/demo args should send a prompt")
	}
	if got := strings.Join(texts(m, entryUser), ""); got != "/demo args" {
		t.Fatalf("user %q", got)
	}
}

func TestDiskSkillACPWinsOverDisk(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "research", "SKILL.md"), `---
name: research
description: disk research skill
---
`)
	m := startSized(t, ws)
	it, ok := catalogByName(m, "research")
	if !ok {
		t.Fatal("research missing from catalog")
	}
	if it.Skill {
		t.Fatal("ACP research must win over disk")
	}
	if it.Desc != "Agent-advertised command" {
		t.Fatalf("desc %q", it.Desc)
	}
	if strings.Contains(it.labeledDesc(), "(skill)") {
		t.Fatalf("advertised desc must not be (skill): %q", it.labeledDesc())
	}

	m.input.SetValue("/research")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("/research should send a prompt")
	}
	if got := strings.Join(texts(m, entryUser), ""); got != "/research" {
		t.Fatalf("user %q", got)
	}
}

func TestDiskSkillMalformedSkipped(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "nope", "SKILL.md"), `---
name: nope
description: never closed
`)
	m := startSized(t, ws)
	if _, ok := catalogByName(m, "nope"); ok {
		t.Fatal("malformed SKILL.md must not appear in catalog")
	}
}

func TestDiskSkillDirectoryNameFallback(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".agents", "skills", "fromdir", "SKILL.md"), `---
description: named by directory
---
`)
	m := startSized(t, ws)
	it, ok := catalogByName(m, "fromdir")
	if !ok || !it.Skill {
		t.Fatalf("fromdir catalog %#v ok=%v", it, ok)
	}
	m.input.SetValue("/fromdir")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = tm.(Model)
	if m.input.Value() != "/fromdir " {
		t.Fatalf("complete %q", m.input.Value())
	}
}

func TestDiskSkillRescanOnBareSlash(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	m := startSized(t, ws)
	if _, ok := catalogByName(m, "demo"); ok {
		t.Fatal("demo should be absent before the file exists")
	}
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "demo", "SKILL.md"), `---
name: demo
description: after start
---
`)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = tm.(Model)
	if m.input.Value() != "/" {
		t.Fatalf("composer %q", m.input.Value())
	}
	it, ok := catalogByName(m, "demo")
	if !ok || !it.Skill {
		t.Fatalf("rescan on / missed demo %#v ok=%v", it, ok)
	}
}

func TestScanSkillsProjectBeatsHome(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "demo", "SKILL.md"), `---
name: demo
description: project
---
`)
	writeSkillMD(t, filepath.Join(home, ".cursor", "skills", "demo", "SKILL.md"), `---
name: demo
description: home
---
`)
	items := scanDiskSkills(ws, home)
	if len(items) != 1 || items[0].Desc != "project" {
		t.Fatalf("project should win: %#v", items)
	}
}

func TestScanSkillsSkipsPluginsAndSymlinks(t *testing.T) {
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "plugins", "plug", "SKILL.md"), `---
name: plug
description: plugin cache
---
`)
	real := filepath.Join(ws, "real-skill", "SKILL.md")
	writeSkillMD(t, real, `---
name: linked
description: symlink target
---
`)
	linkDir := filepath.Join(ws, ".cursor", "skills", "linked")
	if err := os.MkdirAll(filepath.Dir(linkDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(real), linkDir); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(ws, ".cursor", "skills", "filelink", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(fileLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, fileLink); err != nil {
		t.Fatal(err)
	}
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "ok", "SKILL.md"), `---
name: ok
description: real
---
`)
	items := scanDiskSkills(ws, "")
	if _, ok := catalogItems(items, "plug"); ok {
		t.Fatal("must not scan .cursor/plugins")
	}
	if _, ok := catalogItems(items, "linked"); ok {
		t.Fatal("must skip symlink skills")
	}
	if _, ok := catalogItems(items, "ok"); !ok {
		t.Fatalf("missing real skill: %#v", items)
	}
}

func TestScanSkillsSkipsOversize(t *testing.T) {
	ws := t.TempDir()
	big := strings.Repeat("a", (1<<20)+1)
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "huge", "SKILL.md"), "---\nname: huge\ndescription: too big\n---\n"+big)
	items := scanDiskSkills(ws, "")
	if _, ok := catalogItems(items, "huge"); ok {
		t.Fatal("oversize SKILL.md must be skipped")
	}
}

func catalogItems(items []slashItem, name string) (slashItem, bool) {
	want := strings.ToLower(name)
	for _, it := range items {
		if strings.ToLower(it.Name) == want {
			return it, true
		}
	}
	return slashItem{}, false
}

// TestSlashCatalogCursorNamesByDirectory pins cursor's verified rule through
// the slash catalog: a skill directory's basename is offered, never the
// frontmatter name (plan 009 §3.6, C3).
func TestSlashCatalogCursorNamesByDirectory(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "dir-alpha", "SKILL.md"), `---
name: fm-alpha
description: alpha
---
`)
	m := startSized(t, ws)
	if _, ok := catalogByName(m, "fm-alpha"); ok {
		t.Fatal("cursor must not offer the frontmatter name")
	}
	it, ok := catalogByName(m, "dir-alpha")
	if !ok || !it.Skill {
		t.Fatalf("cursor should offer the directory name: %#v ok=%v", it, ok)
	}
}

// TestSlashCatalogGrokKeepsFrontmatterName is the opposite rule for grok,
// exercised through the same catalog path.
func TestSlashCatalogGrokKeepsFrontmatterName(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".grok", "skills", "dir-alpha", "SKILL.md"), `---
name: fm-alpha
description: alpha
---
`)
	m := startSized(t, ws)
	m.snap.Provider = agent.GrokProvider().Info()
	m.rescanSkills()
	if _, ok := catalogByName(m, "dir-alpha"); ok {
		t.Fatal("grok must not offer the directory name when frontmatter has one")
	}
	it, ok := catalogByName(m, "fm-alpha")
	if !ok || !it.Skill {
		t.Fatalf("grok should keep the frontmatter name: %#v ok=%v", it, ok)
	}
}

// TestSlashCatalogHiddenSkillExcluded checks user-invocable: false (cursor's
// spelling) and user_invocable: no (grok's) both hide the skill from the
// catalog the picker reads.
func TestSlashCatalogHiddenSkillExcluded(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "hidden", "SKILL.md"), `---
name: hidden
description: hidden skill
user-invocable: false
---
`)
	writeSkillMD(t, filepath.Join(ws, ".grok", "skills", "hidden", "SKILL.md"), `---
name: hidden
description: hidden skill
user_invocable: no
---
`)
	m := startSized(t, ws)
	if _, ok := catalogByName(m, "hidden"); ok {
		t.Fatal("cursor must hide a user-invocable: false skill")
	}
	m.snap.Provider = agent.GrokProvider().Info()
	m.rescanSkills()
	if _, ok := catalogByName(m, "hidden"); ok {
		t.Fatal("grok must hide a user_invocable: no skill")
	}
}

// TestSlashCatalogGrokWalksAgentsSkills pins grok's second root reaching the
// picker: .agents/skills, not only .grok/skills.
func TestSlashCatalogGrokWalksAgentsSkills(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".agents", "skills", "agentskill", "SKILL.md"), `---
name: agentskill
description: from .agents
---
`)
	m := startSized(t, ws)
	m.snap.Provider = agent.GrokProvider().Info()
	m.rescanSkills()
	it, ok := catalogByName(m, "agentskill")
	if !ok || !it.Skill {
		t.Fatalf("grok should walk .agents/skills: %#v ok=%v", it, ok)
	}
}
