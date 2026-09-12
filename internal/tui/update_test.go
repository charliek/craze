package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

func isolateSkillsHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func sized(t *testing.T) Model {
	t.Helper()
	isolateSkillsHome(t)
	return startSized(t, t.TempDir())
}

func startSized(t *testing.T, ws string) Model {
	t.Helper()
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: ws,
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
	if !strings.Contains(view, "grok  medium  agent  yolo") {
		t.Fatalf("footer should be model effort mode yolo:\n%s", view)
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
	isolateSkillsHome(t)
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
	if m.picking {
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = tm.(Model)
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

func TestEscSlashKeepsComposerText(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/he")
	if !m.slashMenuOpen() {
		t.Fatal("expected slash menu")
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.input.Value() != "/he" {
		t.Fatalf("esc cleared composer: %q", m.input.Value())
	}
	if m.slashMenuOpen() {
		t.Fatal("esc should hide the slash menu")
	}
	if m.quitting {
		t.Fatal("esc on slash must not quit")
	}
}

func TestHelpOverlayFitsTerminal(t *testing.T) {
	m := applyInFlight(t, sized(t), inFlightTools())
	m.input.SetValue("/help")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.help {
		t.Fatal("expected help")
	}
	view := m.View()
	if h := lipgloss.Height(view); h > 24 {
		t.Fatalf("help view is %d rows, crops 24-row terminal:\n%s", h, view)
	}
	if !strings.Contains(view, "yolo") {
		t.Fatalf("footer cropped:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Fatalf("composer cropped:\n%s", view)
	}
	if !strings.Contains(view, "shift+tab") && !strings.Contains(view, "/exit") {
		t.Fatalf("help body missing:\n%s", view)
	}
}

func TestParseSlashFields(t *testing.T) {
	name, args, ok := parseSlashLine("/model\tfast")
	if !ok || name != "model" || args != "fast" {
		t.Fatalf("got %q %q %v", name, args, ok)
	}
}

func flushCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if msg == nil {
		return m
	}
	tm, next := m.Update(msg)
	m = tm.(Model)
	if next != nil {
		return flushCmd(t, m, next)
	}
	return m
}

func TestModelPickerCurrentFastFirst(t *testing.T) {
	m := sized(t)
	m.snap.CurrentModel = "fast"
	m.model = "fast"
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.picking || m.effortStep {
		t.Fatal("expected model picker")
	}
	view := m.View()
	if !strings.Contains(view, "> 1 fast") {
		t.Fatalf("current fast should be first:\n%s", view)
	}
}

func TestModelPickerFitsTerminal(t *testing.T) {
	m := sized(t)
	for i := 0; i < 30; i++ {
		m.snap.Models = append(m.snap.Models, agent.ModelInfo{
			ID: fmt.Sprintf("extra-%02d", i), Name: fmt.Sprintf("Extra %02d", i),
		})
	}
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	view := m.View()
	if h := lipgloss.Height(view); h > 24 {
		t.Fatalf("picker view is %d rows:\n%s", h, view)
	}
	if !strings.Contains(view, "grok") {
		t.Fatalf("current grok cropped:\n%s", view)
	}
	if !strings.Contains(view, "yolo") {
		t.Fatalf("footer cropped:\n%s", view)
	}
}

func TestModelPickerThenEffort(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.picking || m.effortStep {
		t.Fatal("expected model step")
	}
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	m = flushCmd(t, m, cmd)
	if m.snap.CurrentModel != "grok" {
		t.Fatalf("model %q", m.snap.CurrentModel)
	}
	if !m.picking || !m.effortStep {
		t.Fatal("expected effort step after model")
	}
	view := m.View()
	if !strings.Contains(view, "effort") || !strings.Contains(view, "medium") {
		t.Fatalf("effort overlay missing:\n%s", view)
	}
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	m = flushCmd(t, m, cmd)
	if m.picking {
		t.Fatal("picker should close after effort")
	}
	if !strings.Contains(m.View(), "medium") {
		t.Fatalf("footer missing medium:\n%s", m.View())
	}
}

func TestModelSlashSetsModelAndEffort(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model grok high")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	m = flushCmd(t, m, cmd)
	if m.picking {
		t.Fatal("picker should stay closed")
	}
	if m.snap.CurrentModel != "grok" {
		t.Fatalf("model %q", m.snap.CurrentModel)
	}
	if !strings.Contains(m.View(), "high") {
		t.Fatalf("footer missing high:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "grok  high  agent  yolo") {
		t.Fatalf("footer tokens:\n%s", m.View())
	}
}

func TestSetConfigFailAfterModel(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	m = flushCmd(t, m, cmd)
	if !m.effortStep {
		t.Fatal("expected effort step")
	}
	stub.FailNextSetConfig()
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	m = flushCmd(t, m, cmd)
	if m.picking {
		t.Fatal("picker should close")
	}
	if m.snap.CurrentModel != "grok" {
		t.Fatalf("model %q", m.snap.CurrentModel)
	}
	view := m.View()
	if !strings.Contains(view, "medium") {
		t.Fatalf("effort should stay medium:\n%s", view)
	}
	if strings.Contains(view, "grok  high") {
		t.Fatalf("effort should not become high:\n%s", view)
	}
	if len(texts(m, "error")) == 0 {
		t.Fatal("expected error toast")
	}
}

func TestSetModelFailNoEffortOverlay(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.FailNextSetModel()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.input.SetValue("/model")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = tm.(Model)
	if m.snap.CurrentModel != "fast" {
		t.Fatalf("optimistic %q", m.snap.CurrentModel)
	}
	m = flushCmd(t, m, cmd)
	if m.picking || m.effortStep {
		t.Fatal("SetModel fail must not leave the effort overlay open")
	}
	if m.snap.CurrentModel != "grok" {
		t.Fatalf("model should revert, got %q", m.snap.CurrentModel)
	}
	view := m.View()
	if !strings.Contains(view, "grok  medium  agent  yolo") {
		t.Fatalf("footer should be unchanged:\n%s", view)
	}
	if len(texts(m, "error")) == 0 {
		t.Fatal("expected error toast")
	}
}

func TestSetModeFailureKeepsWorkingStatus(t *testing.T) {
	isolateSkillsHome(t)
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

func belowComposer(view string) (string, bool) {
	i := strings.Index(view, "message")
	j := strings.Index(view, "yolo")
	if i < 0 || j < 0 || j <= i {
		return "", false
	}
	return view[i:j], true
}

func inFlightTools() []agent.ToolEvent {
	return []agent.ToolEvent{
		{
			ID:          "task-1",
			Kind:        "other",
			Title:       "Subagent research",
			Status:      "pending",
			ContentText: "scanning workspace",
			RawInput:    "PEEK-TASK-RAW",
		},
		{
			ID:          "sh-1",
			Kind:        "execute",
			Title:       "Shell",
			Status:      "in_progress",
			ContentText: "running",
			RawInput:    "PEEK-SHELL-RAW",
		},
	}
}

func applyInFlight(t *testing.T, m Model, tools []agent.ToolEvent) Model {
	t.Helper()
	stub, ok := m.sess.(*Stub)
	if !ok {
		t.Fatalf("sess is %T, want *Stub", m.sess)
	}
	stub.SetTools(tools)
	for i := range tools {
		tool := tools[i]
		tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &tool}})
		m = tm.(Model)
	}
	return m
}

func hangWorking(t *testing.T) Model {
	t.Helper()
	isolateSkillsHome(t)
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
	return m
}

func TestInPlaceToolLineSameID(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID: "call-1", Kind: "execute", Status: "pending", Title: "Shell",
	}}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID: "call-1", Kind: "execute", Status: "completed", Title: "Shell", RawInput: "echo hi",
	}}})
	m = tm.(Model)
	got := texts(m, "tool")
	if len(got) != 1 {
		t.Fatalf("tool lines %q", got)
	}
	if !strings.Contains(got[0], "completed") {
		t.Fatalf("final status missing: %q", got[0])
	}
	if strings.Contains(got[0], "pending") {
		t.Fatalf("stale pending status: %q", got[0])
	}
	view := m.View()
	if strings.Contains(view, "tool tool") {
		t.Fatalf("renderer prefixed tool twice:\n%s", view)
	}
	if !strings.Contains(view, "tool ") {
		t.Fatalf("missing tool prefix:\n%s", view)
	}
}

func TestWorkStripPlacementAndSubagentLabel(t *testing.T) {
	m := applyInFlight(t, sized(t), inFlightTools())
	view := m.View()
	below, ok := belowComposer(view)
	if !ok {
		t.Fatalf("composer/footer missing:\n%s", view)
	}
	if !strings.Contains(below, "Shell") {
		t.Fatalf("Shell missing below composer:\n%s", below)
	}
	if !strings.Contains(below, "Subagent research") {
		t.Fatalf("Subagent research missing below composer:\n%s", below)
	}
	if strings.Contains(below, "subagent Shell") {
		t.Fatalf("Shell must not be labeled subagent:\n%s", below)
	}
	if !strings.Contains(below, "subagent") {
		t.Fatalf("Subagent research should be labeled subagent:\n%s", below)
	}

	done := inFlightTools()
	for i := range done {
		done[i].Status = "completed"
	}
	m = applyInFlight(t, m, done)
	view = m.View()
	below, ok = belowComposer(view)
	if !ok {
		t.Fatalf("composer/footer missing after complete:\n%s", view)
	}
	if strings.Contains(below, "Shell") || strings.Contains(below, "Subagent research") {
		t.Fatalf("strip should be gone after completed:\n%s", below)
	}
}

func TestStripPeekEnterEsc(t *testing.T) {
	m := applyInFlight(t, sized(t), inFlightTools())
	m.status = statusWorking
	m.input.SetValue("")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("peek enter must not send")
	}
	if !m.stripPeek {
		t.Fatal("expected peek")
	}
	view := m.View()
	below, ok := belowComposer(view)
	if !ok {
		t.Fatalf("composer/footer missing:\n%s", view)
	}
	if !strings.Contains(below, "PEEK-TASK-RAW") && !strings.Contains(below, "PEEK-SHELL-RAW") {
		t.Fatalf("peek missing content snippet:\n%s", below)
	}
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.stripPeek {
		t.Fatal("esc should collapse peek")
	}
	if m.status != statusWorking {
		t.Fatalf("status %s, want working", m.status)
	}
	if m.quitting {
		t.Fatal("esc on peek must not quit")
	}
	if cmd != nil {
		t.Fatal("esc on peek must not cancelTurn")
	}
	below, _ = belowComposer(m.View())
	if strings.Contains(below, "PEEK-TASK-RAW") || strings.Contains(below, "PEEK-SHELL-RAW") {
		t.Fatalf("peek snippet still visible:\n%s", below)
	}
}

func TestStripNavUpDownAndJTypes(t *testing.T) {
	m := applyInFlight(t, sized(t), inFlightTools())
	if m.stripID != "task-1" {
		t.Fatalf("sel %q idx=%d", m.stripID, m.stripSel)
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.stripID != "sh-1" {
		t.Fatalf("down sel %q", m.stripID)
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if m.stripID != "task-1" {
		t.Fatalf("up sel %q", m.stripID)
	}

	m.input.SetValue("hey")
	sel := m.stripID
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = tm.(Model)
	if !strings.Contains(m.input.Value(), "j") {
		t.Fatalf("j should type, got %q", m.input.Value())
	}
	if m.stripID != sel {
		t.Fatal("j must not move strip selection")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.stripID != sel {
		t.Fatal("down with composer text must not move strip")
	}
}

func TestStripKeepsSelectionByID(t *testing.T) {
	m := applyInFlight(t, sized(t), inFlightTools())
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.stripID != "sh-1" {
		t.Fatalf("want sh-1 selected, got %q sel=%d", m.stripID, m.stripSel)
	}
	stub := m.sess.(*Stub)
	tools := inFlightTools()
	tools[0].ContentText = "still scanning"
	stub.SetTools(tools)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &tools[0]}})
	m = tm.(Model)
	if m.stripID != "sh-1" {
		t.Fatalf("selection jumped to %q sel=%d", m.stripID, m.stripSel)
	}
	items := m.stripItems()
	if len(items) < 2 || items[0].ID != "task-1" {
		t.Fatalf("expected task-1 first after update, got %+v", items)
	}
	if items[m.stripSel].ID != "sh-1" {
		t.Fatalf("peek target %+v sel=%d", items, m.stripSel)
	}
}

func TestExitWhileWorkingQuitsHelpDoesNot(t *testing.T) {
	t.Run("exit", func(t *testing.T) {
		m := hangWorking(t)
		m.input.SetValue("/exit")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("/exit while working should quit")
		}
		assertQuitCmd(t, cmd)
	})
	t.Run("quit", func(t *testing.T) {
		m := hangWorking(t)
		m.input.SetValue("/quit")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("/quit while working should quit")
		}
		assertQuitCmd(t, cmd)
	})
	t.Run("help", func(t *testing.T) {
		m := hangWorking(t)
		m.input.SetValue("/help")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if m.help {
			t.Fatal("/help while working should be ignored")
		}
		if m.quitting {
			t.Fatal("/help while working must not quit")
		}
		if cmd != nil {
			t.Fatal("/help while working should not run a command")
		}
		if m.status != statusWorking {
			t.Fatalf("status %s", m.status)
		}
	})
}

func TestClearThenToolUpdateAppends(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID: "old-1", Kind: "execute", Status: "pending", Title: "Shell",
	}}})
	m = tm.(Model)
	if len(texts(m, "tool")) != 1 {
		t.Fatalf("setup lines %q", texts(m, "tool"))
	}
	m.input.SetValue("/clear")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if len(m.lines) != 0 {
		t.Fatalf("clear left lines %+v", m.lines)
	}
	if len(m.toolLine) != 0 {
		t.Fatalf("clear left toolLine %+v", m.toolLine)
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID: "old-1", Kind: "execute", Status: "completed", Title: "Shell",
	}}})
	m = tm.(Model)
	got := texts(m, "tool")
	if len(got) != 1 {
		t.Fatalf("expected one new tool row, got %q", got)
	}
	if !strings.Contains(got[0], "completed") {
		t.Fatalf("new row %q", got[0])
	}
}

func TestHelpOverlayFitsWithInFlightTools(t *testing.T) {
	m := applyInFlight(t, sized(t), inFlightTools())
	m.input.SetValue("/help")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.help {
		t.Fatal("expected help")
	}
	view := m.View()
	if h := lipgloss.Height(view); h > 24 {
		t.Fatalf("help+strip view is %d rows, crops 24-row terminal:\n%s", h, view)
	}
	if !strings.Contains(view, "yolo") {
		t.Fatalf("footer cropped:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Fatalf("composer cropped:\n%s", view)
	}
	if !strings.Contains(view, "shift+tab") && !strings.Contains(view, "/exit") {
		t.Fatalf("help body missing:\n%s", view)
	}
}
