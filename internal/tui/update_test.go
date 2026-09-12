package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

func sized(t *testing.T) Model {
	t.Helper()
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	return tm.(Model)
}

func enter() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

func TestViewFooterAndComposer(t *testing.T) {
	m := sized(t)
	view := m.View()
	if !strings.Contains(view, "idle") {
		t.Fatalf("missing idle in view:\n%s", view)
	}
	if !strings.Contains(view, "yolo") {
		t.Fatalf("missing yolo:\n%s", view)
	}
	if !strings.Contains(view, "grok") {
		t.Fatalf("missing model:\n%s", view)
	}
	if !strings.Contains(view, "agent") {
		t.Fatalf("missing mode:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Fatalf("missing boxed composer placeholder:\n%s", view)
	}
}

func TestFooterStartingBeforeStart(t *testing.T) {
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	view := m.View()
	if !strings.Contains(view, "starting") {
		t.Fatalf("missing starting:\n%s", view)
	}
	if strings.Contains(view, "idle") {
		t.Fatalf("idle before Start:\n%s", view)
	}
}

func TestEnterEmptyDoesNotSend(t *testing.T) {
	m := sized(t)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s", m.status)
	}
	if cmd != nil {
		t.Fatal("empty enter should not start a prompt")
	}
}

func TestEnterSendsAndFollowUp(t *testing.T) {
	m := sized(t)
	m.input.SetValue("hello")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("status %s", m.status)
	}
	if cmd == nil {
		t.Fatal("expected prompt cmd")
	}
	if got := strings.Join(texts(m, "user"), ""); got != "hello" {
		t.Fatalf("user %q", got)
	}
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("enter while working should be ignored")
	}

	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "echo: hello"}})
	m = tm.(Model)
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s after done", m.status)
	}
	if !strings.Contains(m.View(), "echo: hello") {
		t.Fatalf("missing assistant text:\n%s", m.View())
	}

	m.input.SetValue("again")
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("follow-up should send")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "follow-up: again"}})
	m = tm.(Model)
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if !strings.Contains(m.View(), "follow-up: again") {
		t.Fatalf("missing follow-up:\n%s", m.View())
	}
}

func TestQQuitsWhenComposerEmpty(t *testing.T) {
	m := sized(t)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if !m.quitting {
		t.Fatal("expected quit")
	}
	if cmd == nil {
		t.Fatal("expected quit cmd")
	}
	m.input.SetValue("keep")
	m.quitting = false
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if m.quitting {
		t.Fatal("q with composer text must type, not quit")
	}
	if m.input.Value() == "keep" {
		t.Fatal("q should be inserted into composer")
	}
}

func TestEscCancelsWorkingTurn(t *testing.T) {
	stub := NewStub()
	stub.HangNext()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.input.SetValue("wait")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatal("want working")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected cancel cmd")
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "cancelled"}})
	m = tm.(Model)
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "cancelled"}})
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s", m.status)
	}
}

func TestPermissionOverlayKeys(t *testing.T) {
	m := sized(t)
	m.yolo = false
	tm, _ := m.Update(eventMsg{agent.Event{
		Type: agent.EventPermission,
		Permission: &agent.PermissionEvent{
			ID:   "perm-1",
			Tool: "Shell",
			Options: []agent.PermissionOption{
				{OptionID: "opt-once", Kind: "allow_once"},
				{OptionID: "opt-reject", Kind: "reject_once"},
			},
		},
	}})
	m = tm.(Model)
	if m.pending == nil {
		t.Fatal("expected overlay")
	}
	if !strings.Contains(m.View(), "permission Shell") {
		t.Fatalf("missing overlay:\n%s", m.View())
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = tm.(Model)
	if m.pending != nil {
		t.Fatal("overlay should clear")
	}
	if cmd == nil {
		t.Fatal("expected answer cmd")
	}
}

func TestPermissionOverlayPinnedKeys(t *testing.T) {
	t.Run("q quits", func(t *testing.T) {
		m := withOverlay(t)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("q should quit during permission overlay")
		}
		assertQuitCmd(t, cmd)
	})
	t.Run("ctrl+c quits", func(t *testing.T) {
		m := withOverlay(t)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("ctrl+c should quit during permission overlay")
		}
		assertQuitCmd(t, cmd)
	})
	t.Run("ctrl+d quits", func(t *testing.T) {
		m := withOverlay(t)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("ctrl+d should quit during permission overlay")
		}
		assertQuitCmd(t, cmd)
	})
	t.Run("esc cancels overlay", func(t *testing.T) {
		m := withOverlay(t)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = tm.(Model)
		if m.pending != nil {
			t.Fatal("esc should clear the permission overlay")
		}
		if m.quitting {
			t.Fatal("esc should cancel the turn, not quit")
		}
		if cmd == nil {
			t.Fatal("expected cancel/answer cmd")
		}
	})
}

func TestTranscriptPageUpStaysPut(t *testing.T) {
	m := sized(t)
	for i := 0; i < 60; i++ {
		m.addLine("assistant", fmt.Sprintf("line-%02d padding so the transcript is taller than the viewport", i))
	}
	if m.vp.YOffset == 0 {
		t.Fatal("expected stick-to-bottom to leave a non-zero YOffset")
	}
	bottom := m.vp.YOffset
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = tm.(Model)
	if m.vp.YOffset >= bottom {
		t.Fatalf("page up did not scroll: YOffset %d (bottom %d)", m.vp.YOffset, bottom)
	}
	scrolled := m.vp.YOffset
	m.addLine("assistant", "new-line-while-scrolled-up")
	if m.vp.YOffset != scrolled {
		t.Fatalf("new lines jumped the viewport while scrolled up: %d -> %d", scrolled, m.vp.YOffset)
	}
}

func TestPresets(t *testing.T) {
	if Preset("tokyo-night").Name != "tokyo-night" {
		t.Fatal("default")
	}
	if Preset("dark").Name != "dark" || Preset("light").Name != "light" {
		t.Fatal("named presets")
	}
	if Preset("unknown").Name != "tokyo-night" {
		t.Fatal("unknown should fall back")
	}
}

func texts(m Model, kind string) []string {
	var out []string
	for _, ln := range m.lines {
		if ln.kind == kind {
			out = append(out, ln.text)
		}
	}
	return out
}

func withOverlay(t *testing.T) Model {
	t.Helper()
	m := sized(t)
	m.yolo = false
	tm, _ := m.Update(eventMsg{agent.Event{
		Type: agent.EventPermission,
		Permission: &agent.PermissionEvent{
			ID:   "perm-1",
			Tool: "Shell",
			Options: []agent.PermissionOption{
				{OptionID: "opt-once", Kind: "allow_once"},
				{OptionID: "opt-reject", Kind: "reject_once"},
			},
		},
	}})
	m = tm.(Model)
	if m.pending == nil {
		t.Fatal("expected overlay")
	}
	return m
}

func assertQuitCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("quit cmd returned %T, want tea.QuitMsg", msg)
	}
}

func TestCoalesceStreamChunks(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "P"}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "ONG"}})
	m = tm.(Model)
	got := texts(m, "assistant")
	if len(got) != 1 || got[0] != "PONG" {
		t.Fatalf("coalesce %q", got)
	}
	if !strings.Contains(m.View(), "PONG") {
		t.Fatalf("missing PONG:\n%s", m.View())
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "next"}})
	m = tm.(Model)
	got = texts(m, "assistant")
	if len(got) != 2 || got[1] != "next" {
		t.Fatalf("EventDone should break stream, got %q", got)
	}
}

func TestThoughtsCoalesceSeparately(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "th"}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventThought, Text: "ink"}})
	m = tm.(Model)
	got := texts(m, "thought")
	if len(got) != 1 || got[0] != "think" {
		t.Fatalf("thoughts %q", got)
	}
}

func TestAltEnterInsertsNewline(t *testing.T) {
	m := sized(t)
	m.input.SetValue("hello")
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("alt+enter sent a prompt, status %s", m.status)
	}
	if cmd != nil {
		if _, ok := cmd().(promptDoneMsg); ok {
			t.Fatal("alt+enter must not send")
		}
	}
	if !strings.Contains(m.input.Value(), "\n") {
		t.Fatalf("expected newline in composer, got %q", m.input.Value())
	}
}

func TestShiftTabCyclesMode(t *testing.T) {
	m := sized(t)
	if m.snap.CurrentMode != "agent" {
		t.Fatalf("start mode %q", m.snap.CurrentMode)
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("optimistic mode %q", m.snap.CurrentMode)
	}
	if cmd == nil {
		t.Fatal("expected SetMode cmd")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("stub SetMode returned %T %v", msg, msg)
	}
	if !strings.Contains(m.View(), "plan") {
		t.Fatalf("footer missing plan:\n%s", m.View())
	}
}

func TestShiftTabIgnoredDuringPermission(t *testing.T) {
	m := withOverlay(t)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("shift+tab should be ignored on the permission overlay")
	}
	if m.pending == nil {
		t.Fatal("overlay should remain")
	}
	if m.snap.CurrentMode != "agent" {
		t.Fatalf("mode %q", m.snap.CurrentMode)
	}
}

func TestSlashHelpExitAndModel(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/help")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.help {
		t.Fatal("expected help overlay")
	}
	view := m.View()
	if !strings.Contains(view, "shift+tab") && !strings.Contains(view, "/exit") {
		t.Fatalf("help missing keys:\n%s", view)
	}
	if strings.Contains(view, "/btw") {
		t.Fatal("help must not hardcode /btw")
	}

	m.help = false
	m.input.SetValue("/model fast")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.snap.CurrentModel != "fast" {
		t.Fatalf("model %q", m.snap.CurrentModel)
	}
	if cmd == nil {
		t.Fatal("expected SetModel cmd")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("stub SetModel returned %v", msg)
	}

	m.input.SetValue("/exit")
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	if !m.quitting || cmd == nil {
		t.Fatal("/exit should quit")
	}
	assertQuitCmd(t, cmd)
}

func TestTabCompletesSlash(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/he")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = tm.(Model)
	if m.input.Value() != "/help" {
		t.Fatalf("complete %q", m.input.Value())
	}
}

func TestAdvertisedSlashSendsPrompt(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/research")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("/research should send a prompt")
	}
	if got := strings.Join(texts(m, "user"), ""); got != "/research" {
		t.Fatalf("user %q", got)
	}
}

func TestUnknownSlashSendsAsPrompt(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/hepl")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("unknown slash should send as prompt")
	}
	if got := strings.Join(texts(m, "user"), ""); got != "/hepl" {
		t.Fatalf("user %q", got)
	}
}

func TestQOnHelpClosesNotQuits(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = tm.(Model)
	if !m.help {
		t.Fatal("expected help")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if m.help {
		t.Fatal("q should close help")
	}
	if m.quitting {
		t.Fatal("q on help must not quit")
	}
	if cmd != nil {
		t.Fatal("q on help should not return a quit cmd")
	}
}

func TestQOnModelPickerClosesNotQuits(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.picking {
		t.Fatal("expected model picker")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if m.picking {
		t.Fatal("q should close picker")
	}
	if m.quitting || cmd != nil {
		t.Fatal("q on picker must not quit")
	}
}

func TestSetModeFailureKeepsWorkingStatus(t *testing.T) {
	stub := NewStub()
	stub.FailNextSetMode()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.status = statusWorking
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("optimistic %q", m.snap.CurrentMode)
	}
	if m.status != statusWorking {
		t.Fatal("optimistic SetMode must not change working status")
	}
	if cmd == nil {
		t.Fatal("expected SetMode cmd")
	}
	msg := cmd()
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("status %s after failed SetMode", m.status)
	}
	if m.snap.CurrentMode != "agent" {
		t.Fatalf("mode should revert, got %q", m.snap.CurrentMode)
	}
	if len(texts(m, "error")) == 0 {
		t.Fatal("expected error toast")
	}
}
