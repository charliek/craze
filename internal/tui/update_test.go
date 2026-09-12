package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

func isolateSkillsHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// plainView is the frame as text. TestMain forces a true-colour profile so the
// theme tests can assert on real escape sequences, which means every other
// assertion has to strip them first.
func plainView(m Model) string { return plain(m.View()) }

func plain(s string) string { return ansi.Strip(s) }

// statusText is a status row as plain text: the rows return their spans too,
// and most assertions only want the characters.
func statusText(row string, _ []segSpan) string { return plain(row) }

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

// runCmd runs the command a handler returned. Update batches the tick chain in
// behind it, so a batch is unwrapped and only its first member runs: executing
// the tick would wait out a real timer.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	if len(batch) == 0 || batch[0] == nil {
		return nil
	}
	return batch[0]()
}

func TestViewStatusRowsAndComposer(t *testing.T) {
	m := sized(t)
	view := plainView(m)
	if !strings.Contains(view, chipYolo) {
		t.Fatalf("missing the permission chip:\n%s", view)
	}
	if !strings.Contains(view, "cursor │ Grok (medium) │ 0m") {
		t.Fatalf("row 1 should be provider, model (effort), elapsed:\n%s", view)
	}
	if !strings.Contains(view, modeChipAgent+statusDot+modeHint+statusDot+chipYolo) {
		t.Fatalf("row 2 leads with the mode chip and its hint:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Fatalf("missing the composer placeholder:\n%s", view)
	}
}

func TestStatusStartingBeforeStart(t *testing.T) {
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	view := plainView(m)
	if !strings.Contains(view, "starting…") {
		t.Fatalf("missing starting:\n%s", view)
	}
	if strings.Contains(view, "cursor") {
		t.Fatalf("the session line fills in only after Start:\n%s", view)
	}
	if !strings.Contains(view, chipYolo) {
		t.Fatalf("the chip is up before Start too:\n%s", view)
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
	if got := strings.Join(texts(m, entryUser), ""); got != "hello" {
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
	if !strings.Contains(plainView(m), "echo: hello") {
		t.Fatalf("missing assistant text:\n%s", plainView(m))
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
	if !strings.Contains(plainView(m), "follow-up: again") {
		t.Fatalf("missing follow-up:\n%s", plainView(m))
	}
}

func TestCtrlDQuits(t *testing.T) {
	m := sized(t)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting {
		t.Fatal("expected quit")
	}
	if cmd == nil {
		t.Fatal("expected quit cmd")
	}
	assertQuitCmd(t, cmd)

	m = sized(t)
	m.input.SetValue("keep")
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting || cmd == nil {
		t.Fatal("ctrl+d should quit with composer text too")
	}
}

func TestTypingQuickDoesNotQuit(t *testing.T) {
	m := sized(t)
	for _, r := range "quick question" {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(Model)
		if m.quitting {
			t.Fatalf("typing %q quit craze", r)
		}
		if m.help {
			t.Fatalf("typing %q opened help", r)
		}
	}
	if m.input.Value() != "quick question" {
		t.Fatalf("composer %q", m.input.Value())
	}
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("quick question should send")
	}
	if got := strings.Join(texts(m, entryUser), ""); got != "quick question" {
		t.Fatalf("user %q", got)
	}
}

func TestCtrlCStateMachine(t *testing.T) {
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	t.Run("idle quits", func(t *testing.T) {
		m := sized(t)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("ctrl+c while idle should quit")
		}
		assertQuitCmd(t, cmd)
	})

	t.Run("error quits", func(t *testing.T) {
		m := sized(t)
		m.status = statusError
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("ctrl+c in the error state should quit")
		}
	})

	t.Run("working cancels then quits", func(t *testing.T) {
		now := base
		m := hangWorking(t)
		m.clock = func() time.Time { return now }
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if m.quitting {
			t.Fatal("first ctrl+c while working must cancel, not quit")
		}
		if cmd == nil {
			t.Fatal("expected cancel cmd")
		}
		if !m.ctrlCDeadline.Equal(now.Add(time.Second)) {
			t.Fatalf("deadline %v", m.ctrlCDeadline)
		}
		now = base.Add(400 * time.Millisecond)
		tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if !m.quitting || cmd == nil {
			t.Fatal("second ctrl+c inside the window should quit")
		}
		assertQuitCmd(t, cmd)
	})

	t.Run("after the window cancels again", func(t *testing.T) {
		now := base
		m := hangWorking(t)
		m.clock = func() time.Time { return now }
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		now = base.Add(2 * time.Second)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if m.quitting {
			t.Fatal("ctrl+c after the window should cancel again")
		}
		if cmd == nil {
			t.Fatal("expected a second cancel cmd")
		}
		if !m.ctrlCDeadline.Equal(now.Add(time.Second)) {
			t.Fatalf("deadline should re-arm, got %v", m.ctrlCDeadline)
		}
	})

	t.Run("working with a pending permission cancels once", func(t *testing.T) {
		m := hangWorking(t)
		m.clock = func() time.Time { return base }
		m = cardEvent(t, m, m.sess.(*Stub), agent.Event{
			Type: agent.EventPermission,
			Permission: &agent.PermissionEvent{
				ID:      "perm-1",
				Tool:    "Shell",
				Options: []agent.PermissionOption{{OptionID: "opt-once", Kind: "allow_once"}},
			},
		})
		if !m.cardOpen() || m.status != statusWorking {
			t.Fatalf("setup: pending=%v status=%s", m.cardOpen(), m.status)
		}
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if m.quitting {
			t.Fatal("ctrl+c with a pending request should cancel, not quit")
		}
		if m.cardOpen() {
			t.Fatal("cancel should clear the pending request")
		}
		if cmd == nil {
			t.Fatal("expected one cancel cmd")
		}
		// The answer and the cancel must run in one command, and a request the
		// agent already withdrew must not paint an error row.
		if msg := cmd(); msg != nil {
			t.Fatalf("cancel cmd returned %T %v", msg, msg)
		}
		if got := texts(m, entryError); len(got) != 0 {
			t.Fatalf("cancel painted error rows %q", got)
		}
	})

	t.Run("another key clears the window", func(t *testing.T) {
		now := base
		m := hangWorking(t)
		m.clock = func() time.Time { return now }
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		m = tm.(Model)
		if !m.ctrlCDeadline.IsZero() {
			t.Fatalf("deadline should be cleared, got %v", m.ctrlCDeadline)
		}
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if m.quitting {
			t.Fatal("ctrl+c after another key should cancel, not quit")
		}
		if cmd == nil {
			t.Fatal("expected cancel cmd")
		}
	})
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
	m = cardEvent(t, m, m.sess.(*Stub), agent.Event{
		Type: agent.EventPermission,
		Permission: &agent.PermissionEvent{
			ID:   "perm-1",
			Tool: "Shell",
			Options: []agent.PermissionOption{
				{OptionID: "opt-once", Kind: "allow_once"},
				{OptionID: "opt-reject", Kind: "reject_once"},
			},
		},
	})
	if !m.cardOpen() {
		t.Fatal("expected overlay")
	}
	if !strings.Contains(plainView(m), "permission Shell") {
		t.Fatalf("missing overlay:\n%s", plainView(m))
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = tm.(Model)
	if m.cardOpen() {
		t.Fatal("overlay should clear")
	}
	// The answer goes out in this same update, not in a command that runs later.
	calls := m.sess.(*Stub).Calls()
	if len(calls) != 1 || calls[0].Method != "permission" || calls[0].Option != "opt-once" {
		t.Fatalf("allow once sent %+v", calls)
	}
}

func TestPermissionOverlayPinnedKeys(t *testing.T) {
	t.Run("q does not quit", func(t *testing.T) {
		m := withOverlay(t)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
		m = tm.(Model)
		if m.quitting || cmd != nil {
			t.Fatal("bare q must not quit during a permission overlay")
		}
		if !m.cardOpen() {
			t.Fatal("overlay should remain")
		}
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
		if m.cardOpen() {
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
		m.appendEntry(entry{kind: entryAssistant, text: fmt.Sprintf("line-%02d padding so the transcript is taller than the viewport", i)})
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
	m.appendEntry(entry{kind: entryAssistant, text: "new-line-while-scrolled-up"})
	if m.vp.YOffset != scrolled {
		t.Fatalf("new lines jumped the viewport while scrolled up: %d -> %d", scrolled, m.vp.YOffset)
	}
}

func texts(m Model, kind entryKind) []string {
	var out []string
	for _, e := range m.entries {
		if e.kind == kind {
			out = append(out, e.text)
		}
	}
	return out
}

// toolRows is what the transcript actually draws for each tool call, one entry
// per element and its rendered rows joined.
func toolRows(m Model) []string {
	var out []string
	for _, e := range m.entries {
		if e.kind == entryTool {
			out = append(out, plain(strings.Join(e.rendered, "\n")))
		}
	}
	return out
}

func withOverlay(t *testing.T) Model {
	t.Helper()
	m := sized(t)
	m.yolo = false
	m = cardEvent(t, m, m.sess.(*Stub), agent.Event{
		Type: agent.EventPermission,
		Permission: &agent.PermissionEvent{
			ID:   "perm-1",
			Tool: "Shell",
			Options: []agent.PermissionOption{
				{OptionID: "opt-once", Kind: "allow_once"},
				{OptionID: "opt-reject", Kind: "reject_once"},
			},
		},
	})
	if !m.cardOpen() {
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
	got := texts(m, entryAssistant)
	if len(got) != 1 || got[0] != "PONG" {
		t.Fatalf("coalesce %q", got)
	}
	if !strings.Contains(plainView(m), "PONG") {
		t.Fatalf("missing PONG:\n%s", plainView(m))
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "next"}})
	m = tm.(Model)
	got = texts(m, entryAssistant)
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
	got := texts(m, entryThought)
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
	if !strings.Contains(plainView(m), "plan") {
		t.Fatalf("footer missing plan:\n%s", plainView(m))
	}
}

func TestShiftTabIgnoredDuringPermission(t *testing.T) {
	m := withOverlay(t)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("shift+tab should be ignored on the permission overlay")
	}
	if !m.cardOpen() {
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
	view := plainView(m)
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
	if got := strings.Join(texts(m, entryUser), ""); got != "/research" {
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
	if got := strings.Join(texts(m, entryUser), ""); got != "/hepl" {
		t.Fatalf("user %q", got)
	}
}

func TestEscOnHelpClosesNotQuits(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/help")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.help {
		t.Fatal("expected help")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if !m.help {
		t.Fatal("q must not close help")
	}
	if m.quitting || cmd != nil {
		t.Fatal("q on help must not quit or run a command")
	}
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.help {
		t.Fatal("esc should close help")
	}
	if m.quitting || cmd != nil {
		t.Fatal("esc on help must not quit")
	}
}

func TestEscOnModelPickerClosesNotQuits(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !m.picking {
		t.Fatal("expected model picker")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if !m.picking {
		t.Fatal("q must not close the picker")
	}
	if m.quitting || cmd != nil {
		t.Fatal("q on the picker must not quit or run a command")
	}
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.picking {
		t.Fatal("esc should close picker")
	}
	if m.quitting || cmd != nil {
		t.Fatal("esc on picker must not quit")
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
	view := plainView(m)
	if h := lipgloss.Height(view); h > 24 {
		t.Fatalf("help view is %d rows, crops 24-row terminal:\n%s", h, view)
	}
	if !strings.Contains(view, chipYolo) {
		t.Fatalf("status rows cropped:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Fatalf("composer cropped:\n%s", view)
	}
	if !strings.Contains(view, "shift+tab") && !strings.Contains(view, "/exit") {
		t.Fatalf("help body missing:\n%s", view)
	}
}

func TestWrapProseHardWrapsAtTinyWidths(t *testing.T) {
	for _, width := range []int{1, 2, 3, 80} {
		limit := max(1, min(width-2, proseMaxWidth))
		got := wrapProse(strings.Repeat("x", 40), width)
		for _, ln := range strings.Split(got, "\n") {
			if lipgloss.Width(ln) > limit {
				t.Fatalf("width %d: line %q is wider than %d", width, ln, limit)
			}
		}
		if strings.Count(got, "x") != 40 {
			t.Fatalf("width %d: wrap lost characters: %q", width, got)
		}
	}
	if got := wrapProse("unchanged", 0); got != "unchanged" {
		t.Fatalf("wrap before the first resize should pass through, got %q", got)
	}
}

func TestToolRowsStayOneRow(t *testing.T) {
	m := sized(t)
	tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID:     "call-abc-0\nfc_123",
		Kind:   "execute\nextra",
		Status: "pending\nextra",
		Title:  "Shell\x1b]0;x\x07 go vet\n./...",
	}}})
	m = tm.(Model)
	got := toolRows(m)
	if len(got) != 1 {
		t.Fatalf("tool lines %q", got)
	}
	if strings.ContainsAny(got[0], "\n\x1b\x07") {
		t.Fatalf("tool row still has control characters: %q", got[0])
	}
	if strings.Contains(got[0], "fc_123") {
		t.Fatalf("tool id must never be rendered: %q", got[0])
	}
	if strings.Contains(got[0], "]0;x") {
		t.Fatalf("injected escape sequence survived: %q", got[0])
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
	view := plainView(m)
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
	view := plainView(m)
	if h := lipgloss.Height(view); h > 24 {
		t.Fatalf("picker view is %d rows:\n%s", h, view)
	}
	if !strings.Contains(view, "grok") {
		t.Fatalf("current grok cropped:\n%s", view)
	}
	if !strings.Contains(view, chipYolo) {
		t.Fatalf("status rows cropped:\n%s", view)
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
	view := plainView(m)
	if !strings.Contains(view, "effort") || !strings.Contains(view, "medium") {
		t.Fatalf("effort overlay missing:\n%s", view)
	}
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	m = flushCmd(t, m, cmd)
	if m.picking {
		t.Fatal("picker should close after effort")
	}
	if !strings.Contains(plainView(m), "medium") {
		t.Fatalf("footer missing medium:\n%s", plainView(m))
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
	if !strings.Contains(plainView(m), "high") {
		t.Fatalf("footer missing high:\n%s", plainView(m))
	}
	if !strings.Contains(plainView(m), "Grok (high) │ 0m") {
		t.Fatalf("status row 1 tokens:\n%s", plainView(m))
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
	view := plainView(m)
	if !strings.Contains(view, "medium") {
		t.Fatalf("effort should stay medium:\n%s", view)
	}
	if strings.Contains(view, "grok  high") {
		t.Fatalf("effort should not become high:\n%s", view)
	}
	if len(texts(m, entryError)) == 0 {
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
	view := plainView(m)
	if !strings.Contains(view, "Grok (medium) │ 0m") {
		t.Fatalf("status row 1 should be unchanged:\n%s", view)
	}
	if len(texts(m, entryError)) == 0 {
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
	msg := runCmd(cmd)
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("status %s after failed SetMode", m.status)
	}
	if m.snap.CurrentMode != "agent" {
		t.Fatalf("mode should revert, got %q", m.snap.CurrentMode)
	}
	if len(texts(m, entryError)) == 0 {
		t.Fatal("expected error toast")
	}
}

// belowComposer is the band between the composer and the status rows: the
// sub-agent peek and the permission line live there. The chip is the anchor
// because status row 2 always draws it.
func belowComposer(view string) (string, bool) {
	i := strings.Index(view, "message")
	j := strings.Index(view, chipYolo)
	if j < 0 {
		j = strings.Index(view, chipPrompt)
	}
	if i < 0 || j < 0 || j <= i {
		return "", false
	}
	return view[i:j], true
}

// belowStatus is everything under the status rows, which since U3b is where
// the sub-agent rows are drawn.
func belowStatus(view string) (string, bool) {
	j := strings.Index(view, chipYolo)
	if j < 0 {
		j = strings.Index(view, chipPrompt)
	}
	if j < 0 {
		return "", false
	}
	rest := view[j:]
	k := strings.Index(rest, "\n")
	if k < 0 {
		// The chip is the last line, so nothing is drawn under it.
		return "", true
	}
	return rest[k+1:], true
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
	got := toolRows(m)
	if len(got) != 1 {
		t.Fatalf("tool lines %q", got)
	}
	if !strings.HasPrefix(got[0], "✓ bash") {
		t.Fatalf("final status missing: %q", got[0])
	}
	if !strings.Contains(got[0], "echo hi") {
		t.Fatalf("command missing: %q", got[0])
	}
	if !strings.Contains(plainView(m), "✓ bash  echo hi") {
		t.Fatalf("row missing from the view:\n%s", plainView(m))
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
	if len(toolRows(m)) != 1 {
		t.Fatalf("setup lines %q", toolRows(m))
	}
	m.input.SetValue("/clear")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if len(m.entries) != 0 {
		t.Fatalf("clear left entries %+v", m.entries)
	}
	if len(m.toolLine) != 0 {
		t.Fatalf("clear left toolLine %+v", m.toolLine)
	}
	if len(m.pathDirs) != 0 || m.trimmed {
		t.Fatalf("clear left the path cache %+v (trimmed=%v)", m.pathDirs, m.trimmed)
	}
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID: "old-1", Kind: "execute", Status: "completed", Title: "Shell",
	}}})
	m = tm.(Model)
	got := toolRows(m)
	if len(got) != 1 {
		t.Fatalf("expected one new tool row, got %q", got)
	}
	if !strings.HasPrefix(got[0], "✓ bash") {
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
	view := plainView(m)
	if h := lipgloss.Height(view); h > 24 {
		t.Fatalf("help+agents view is %d rows, crops 24-row terminal:\n%s", h, view)
	}
	if !strings.Contains(view, chipYolo) {
		t.Fatalf("footer cropped:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Fatalf("composer cropped:\n%s", view)
	}
	if !strings.Contains(view, "shift+tab") && !strings.Contains(view, "/exit") {
		t.Fatalf("help body missing:\n%s", view)
	}
}
