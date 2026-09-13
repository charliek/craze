package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

func pickerFactory(t *testing.T) func(agent.Provider) agent.Session {
	t.Helper()
	return func(p agent.Provider) agent.Session {
		s := NewStub()
		s.SetProvider(p)
		return s
	}
}

func newPicker(t *testing.T, def agent.Provider) Model {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{
		Theme:      "tokyo-night",
		Workspace:  t.TempDir(),
		Model:      "grok",
		Yolo:       true,
		Provider:   def,
		NewSession: pickerFactory(t),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return tm.(Model)
}

func TestProviderPickerShowsBeforeStart(t *testing.T) {
	m := newPicker(t, agent.CursorProvider())
	if !m.pickingProvider || m.dialog != dialogProvider {
		t.Fatalf("picker dialog=%v picking=%v", m.dialog, m.pickingProvider)
	}
	if m.started || m.sess != nil {
		t.Fatal("session must not exist until a row is chosen")
	}
	view := plainView(m)
	if strings.Contains(view, "starting…") {
		t.Fatalf("starting chip during picker:\n%s", view)
	}
	if !strings.Contains(view, "cursor") || !strings.Contains(view, "grok") {
		t.Fatalf("picker rows missing:\n%s", view)
	}
	if !strings.Contains(view, "default") {
		t.Fatalf("default tag missing:\n%s", view)
	}
}

func TestProviderLockedSkipsPicker(t *testing.T) {
	isolateSkillsHome(t)
	called := 0
	m := New(Config{
		Theme:          "tokyo-night",
		Workspace:      t.TempDir(),
		Yolo:           true,
		Provider:       agent.GrokProvider(),
		ProviderLocked: true,
		NewSession: func(p agent.Provider) agent.Session {
			called++
			s := NewStub()
			s.SetProvider(p)
			return s
		},
	})
	if m.pickingProvider || m.dialog != dialogNone {
		t.Fatalf("locked still picking dialog=%v", m.dialog)
	}
	if called != 1 {
		t.Fatalf("factory called %d times", called)
	}
	if m.sess == nil {
		t.Fatal("locked path must construct immediately")
	}
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("provider %q", m.snap.Provider.Name)
	}
}

func TestProviderPickerEnterStartsSelected(t *testing.T) {
	m := newPicker(t, agent.CursorProvider())
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.providerCursor != 1 {
		t.Fatalf("cursor %d", m.providerCursor)
	}
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.pickingProvider || m.dialog != dialogNone {
		t.Fatal("picker still open after enter")
	}
	msg := runCmd(cmd)
	if _, ok := msg.(startedMsg); !ok {
		t.Fatalf("start cmd %T", msg)
	}
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if !m.started {
		t.Fatal("not started")
	}
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("provider %q", m.snap.Provider.Name)
	}
}

func TestProviderPickerEscStartsDefault(t *testing.T) {
	m := newPicker(t, agent.GrokProvider())
	if m.providerCursor != 1 {
		t.Fatalf("preselect %d", m.providerCursor)
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if m.providerCursor != 0 {
		t.Fatalf("cursor after up %d", m.providerCursor)
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	msg := runCmd(cmd)
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("esc must start the default, got %q", m.snap.Provider.Name)
	}
}

func TestStartedMsgPersistsProvider(t *testing.T) {
	path := writeConfigFile(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(agent.GrokProvider())
	m := New(Config{
		Session:         stub,
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		PersistProvider: true,
		ProviderLocked:  true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	if !tm.(Model).started {
		t.Fatal("not started")
	}
	if got := ConfigProvider(); got != "grok" {
		t.Fatalf("persisted %q", got)
	}
}

func TestProviderPickerCtrlDQuitsWithoutSession(t *testing.T) {
	path := writeConfigFile(t, "provider = \"codex\"\n")
	called := 0
	isolateSkillsHome(t)
	m := New(Config{
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		Provider:        agent.CursorProvider(),
		PersistProvider: true,
		FallbackDefault: true,
		NewSession: func(p agent.Provider) agent.Session {
			called++
			s := NewStub()
			s.SetProvider(p)
			return s
		},
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting {
		t.Fatal("ctrl+d should quit")
	}
	msg := runCmd(cmd)
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("quit cmd %T", msg)
	}
	if called != 0 {
		t.Fatal("factory must not run on picker quit")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "codex") {
		t.Fatalf("picker quit persisted:\n%s", body)
	}
}

func TestFallbackPickerEnterPersistsChosenProvider(t *testing.T) {
	path := writeConfigFile(t, "provider = \"codex\"\n")
	m := New(Config{
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		Provider:        agent.CursorProvider(),
		PersistProvider: true,
		FallbackDefault: true,
		NewSession:      pickerFactory(t),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	tm, _ = m.Update(runCmd(cmd))
	m = tm.(Model)
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("provider %q", m.snap.Provider.Name)
	}
	if got := ConfigProvider(); got != "grok" {
		t.Fatalf("persisted %q, file %s", got, path)
	}
}

func TestFallbackPickerEscDoesNotPersist(t *testing.T) {
	path := writeConfigFile(t, "provider = \"codex\"\n")
	m := New(Config{
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		Provider:        agent.CursorProvider(),
		PersistProvider: true,
		FallbackDefault: true,
		NewSession:      pickerFactory(t),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	tm, _ = m.Update(runCmd(cmd))
	_ = tm
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "codex") {
		t.Fatalf("esc on fallback persisted:\n%s", body)
	}
	if ConfigProvider() == "cursor" {
		t.Fatal("esc on fallback must not write cursor over the unknown id")
	}
}

func TestFailedStartDoesNotPersistProvider(t *testing.T) {
	path := writeConfigFile(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	isolateSkillsHome(t)
	m := New(Config{
		Session:         NewStub(),
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		PersistProvider: true,
		ProviderLocked:  true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(errMsg{err: fmt.Errorf("nope")})
	_ = tm
	if got := ConfigProvider(); got != "" {
		t.Fatalf("failed start persisted %q", got)
	}
}

func TestStartedMsgDoesNotPersistWhenDisabled(t *testing.T) {
	path := writeConfigFile(t, "theme = \"tokyo-night\"\n")
	m := sized(t)
	tm, _ := m.Update(startedMsg{})
	_ = tm
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "provider") {
		t.Fatalf("wrote provider without PersistProvider:\n%s", body)
	}
}

func TestFrameGoldenProviderPicker(t *testing.T) {
	isolateSkillsHome(t)
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		m := New(Config{
			Theme:      "tokyo-night",
			Workspace:  frameWorkspace(t),
			Model:      "grok",
			Yolo:       true,
			Provider:   agent.CursorProvider(),
			NewSession: pickerFactory(t),
		})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: size.cols, Height: size.rows})
		m = tm.(Model)
		name := "provider-picker-100x30"
		if size.cols == 80 {
			name = "provider-picker-80x24"
		}
		assertGolden(t, name, size.cols, size.rows, plainView(m))
	}
}
