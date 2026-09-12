package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	if m.input.Value() != "/demo" {
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
	if m.input.Value() != "/fromdir" {
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
	big := strings.Repeat("a", maxSkillBytes+1)
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

// TestSkillWarningsGoToTheDiagSeam: the scan runs on the Update goroutine while
// the TUI owns the alt screen, so a warning written to the real stderr is
// painted over the frame with none of the renderer's locks taken. It goes to the
// writer Run installed instead, which the caller flushes afterwards.
func TestSkillWarningsGoToTheDiagSeam(t *testing.T) {
	ws := t.TempDir()
	// No closing fence: the parse fails and the skill is named as skipped.
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "broken", "SKILL.md"), "---\nname: broken\n")

	var buf bytes.Buffer
	prev := setDiag(&buf)
	t.Cleanup(func() { setDiag(prev) })
	// os.Stderr is the terminal under the alt screen. Nothing may reach it.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevErr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = prevErr; _ = r.Close() })

	if items := scanDiskSkills(ws, t.TempDir()); len(items) != 0 {
		t.Fatalf("a skill that does not parse must not appear: %#v", items)
	}
	if got := buf.String(); !strings.Contains(got, "skip skill") || !strings.Contains(got, "broken") {
		t.Fatalf("the warning did not reach the seam: %q", got)
	}
	_ = w.Close()
	leaked, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaked) != 0 {
		t.Fatalf("%q reached the terminal", leaked)
	}
}
