package tui

import (
	"context"
	"errors"
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
	return startStub(t, NewStub(), ws, 80, 24)
}

// startStub is where every test model is built: the standard config around the
// caller's stub, sized, and started. Isolating the skills home stays with the
// caller, because a test that wants real skills on disk sets one up first.
func startStub(t *testing.T, stub *Stub, ws string, cols, rows int) Model {
	t.Helper()
	m := New(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: ws,
		Model:     "grok",
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
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
	// A turn is over when both of its endings have landed, so the status waits
	// for the stream as well as for the prompt.
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}})
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("status %s before the prompt returned", m.status)
	}
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
		if m.dialog == dialogHelp {
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

// TestEscDuringTheCatalogWaitSettlesTheTurn is Esc while the first prompt is
// still held back for the agent's command catalog. That prompt opened no turn,
// so the session emits nothing at all for it and its error is the whole of the
// ending — and what it has to leave on screen is what every other cancel
// leaves: the draft where the user typed it, the note under it, no error.
//
// Both commands run for real — the one Enter returned, which parks, and the one
// Esc returned, which frees it — so the promptDoneMsg fed back below is the
// session's own and not one written here. What this does not test is the race
// that follows: the session-level TestCancelDuringTheWaitLeavesTheNextPromptAlone
// owns what Cancel may still do once the freed prompt has returned.
func TestEscDuringTheCatalogWaitSettlesTheTurn(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	parked := stub.ParkNext()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.input.SetValue("/probe-echo banana")
	tm, send := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatal("want working")
	}
	// The send runs where the tea runtime runs it, on its own goroutine, so
	// the Esc below lands while it is still parked. Starting that goroutine
	// does not order it against the Esc, though: without the barrier the Esc
	// can run first, and the prompt then takes the buffered cancellation on
	// arrival — a queued cancel, not a cancel of a prompt already in the wait,
	// which is the only thing this test is about.
	done := make(chan tea.Msg, 1)
	go func() { done <- runCmd(send) }()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never parked")
	}

	tm, esc := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if esc == nil {
		t.Fatal("expected cancel cmd")
	}
	if msg := runCmd(esc); msg != nil {
		t.Fatalf("the cancel failed: %+v", msg)
	}
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Esc never freed the parked prompt")
	}
	got, ok := msg.(promptDoneMsg)
	if !ok || !errors.Is(got.err, agent.ErrPromptCancelled) {
		t.Fatalf("the parked prompt returned %#v", msg)
	}
	tm, _ = m.Update(got)
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s", m.status)
	}
	if m.err != "" {
		t.Fatalf("a cancel is not an error: %q", m.err)
	}
	view := plainView(m)
	if !strings.Contains(view, "/probe-echo banana") {
		t.Fatalf("the draft left the transcript:\n%s", view)
	}
	if !strings.Contains(view, stopCancelled) {
		t.Fatalf("the cancel left no note:\n%s", view)
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
	m.refreshViewport()
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
	m.refreshViewport()
	if m.vp.YOffset != scrolled {
		t.Fatalf("new lines jumped the viewport while scrolled up: %d -> %d", scrolled, m.vp.YOffset)
	}
}

func texts(m Model, kind entryKind) []string {
	var out []string
	for _, e := range m.main.entries {
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
	for _, e := range m.main.entries {
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
	if m.dialog != dialogHelp {
		t.Fatal("expected the help dialog")
	}
	view := plainView(m)
	// A row of the box itself, not the "shift+tab" hint status row 2 also draws.
	if !strings.Contains(view, "enter             send the draft") {
		t.Fatalf("help missing keys:\n%s", view)
	}
	if strings.Contains(view, "/btw") {
		t.Fatal("help must not hardcode /btw")
	}

	m = m.closeDialog(true)
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
	// Accepting ends the token with a space, which is what closes the menu.
	if m.input.Value() != "/help " {
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
	if m.dialog != dialogHelp {
		t.Fatal("expected help")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if m.dialog != dialogHelp {
		t.Fatal("q must not close help")
	}
	if m.quitting || cmd != nil {
		t.Fatal("q on help must not quit or run a command")
	}
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.dialog == dialogHelp {
		t.Fatal("esc should close help")
	}
	if m.quitting || cmd != nil {
		t.Fatal("esc on help must not quit")
	}
}

func TestEscOnModelDialogClosesNotQuits(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogModel {
		t.Fatal("expected the model dialog")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = tm.(Model)
	if m.dialog != dialogModel {
		t.Fatal("q must not close the dialog")
	}
	if m.quitting {
		t.Fatal("q on the dialog must not quit")
	}
	if m.mdlg.filter.Value() != "q" {
		t.Fatalf("q should have filtered, got %q", m.mdlg.filter.Value())
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.dialog != dialogNone {
		t.Fatal("esc should close the dialog")
	}
	if m.quitting || cmd != nil {
		t.Fatal("esc on the dialog must not quit")
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

// TestHelpDialogFitsTerminal: help is a modal layer now, so it owes the frame
// the same contract the other two dialogs do — the box sits inside the
// transcript region and every band under it keeps its rows — with in-flight
// agent rows on screen, which is the tightest transcript craze draws.
//
// It replaces TestHelpOverlayFitsTerminal and TestHelpOverlayFitsWithInFlightTools,
// which had the same fixture as each other and only checked the frame's height.
func TestHelpDialogFitsTerminal(t *testing.T) {
	m := applyInFlight(t, sized(t), inFlightTools())
	m.input.SetValue("/help")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogHelp {
		t.Fatalf("expected the help dialog, got %v", m.dialog)
	}
	view := plainView(m)
	if h := lipgloss.Height(view); h != 24 {
		t.Fatalf("help view is %d rows, want exactly 24:\n%s", h, view)
	}
	r, tr := m.lay.Dialog, m.lay.Region(regionTranscript)
	if r.Empty() {
		t.Fatalf("no dialog rectangle: %+v", m.lay)
	}
	if r.Y < tr.Top || r.Y+r.H > tr.Bottom {
		t.Fatalf("the box %+v is not inside the transcript %+v:\n%s", r, tr, view)
	}
	if !strings.Contains(view, chipYolo) {
		t.Fatalf("status rows covered:\n%s", view)
	}
	if !strings.Contains(view, "message") {
		t.Fatalf("composer covered:\n%s", view)
	}
	if !strings.Contains(strings.Split(view, "\n")[r.Y+1], helpDialogTitle) {
		t.Fatalf("the box drew no title row:\n%s", view)
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

func TestModelDialogCurrentFirstAndFilters(t *testing.T) {
	m := sized(t)
	m.snap.CurrentModel = "fast"
	m.model = "fast"
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogModel {
		t.Fatal("expected the model dialog")
	}
	view := plainView(m)
	if !strings.Contains(view, "> Fast") || !strings.Contains(view, "current") {
		t.Fatalf("the current model should be first and tagged:\n%s", view)
	}
	// The filter matches the display name or the id, case-insensitively.
	m = typeInto(t, m, "GRO")
	if got := len(m.dialogModelList()); got != 1 {
		t.Fatalf("filter GRO matched %d models", got)
	}
	if view := plainView(m); !strings.Contains(view, "❯ GRO") || strings.Contains(view, "> Fast") {
		t.Fatalf("filter row and list disagree:\n%s", view)
	}
}

func typeInto(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = tm.(Model)
	}
	return m
}

// TestModelDialogFitsTerminal: a long catalogue scrolls inside the box rather
// than growing it past the transcript region.
func TestModelDialogFitsTerminal(t *testing.T) {
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
	if h := lipgloss.Height(view); h != 24 {
		t.Fatalf("frame is %d rows:\n%s", h, view)
	}
	tr := m.lay.Region(regionTranscript)
	r := m.lay.Dialog
	if r.Y < tr.Top || r.Y+r.H > tr.Bottom {
		t.Fatalf("the box %+v escaped the transcript %+v", r, tr)
	}
	if !strings.Contains(view, "▼") {
		t.Fatalf("a clipped list should say so:\n%s", view)
	}
	if !strings.Contains(view, chipYolo) {
		t.Fatalf("the status rows were covered:\n%s", view)
	}
}

// TestModelDialogAppliesModelEffortAndFast is §3.4's apply chain: three steps
// in order, each only when it changed, one note per step that landed.
func TestModelDialogAppliesModelEffortAndFast(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	// Model: the second row. Effort: one right. Fast: one right.
	m = pressKey(t, m, tea.KeyDown)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogNone {
		t.Fatal("enter closes the dialog optimistically")
	}
	m = flushCmd(t, m, cmd)

	if got := texts(m, entryNote); len(got) != 3 ||
		got[0] != "model → fast" || got[1] != "effort → high" || got[2] != "fast → on" {
		t.Fatalf("notes %v", got)
	}
	stub := m.sess.(*Stub)
	snap := stub.Snapshot()
	if snap.CurrentModel != "fast" {
		t.Fatalf("model %q", snap.CurrentModel)
	}
	// "on" is the advertised value, which is the string "true" and not a bool.
	opt := agent.FastOption(snap)
	if opt == nil || opt.Current != "true" {
		t.Fatalf("fast should have been sent the string \"true\": %+v", opt)
	}
	if !strings.Contains(plainView(m), "Fast (high · fast)") {
		t.Fatalf("status row 1 should name the effort and fast:\n%s", plainView(m))
	}
}

func pressKey(t *testing.T, m Model, k tea.KeyType) Model {
	t.Helper()
	tm, _ := m.Update(tea.KeyMsg{Type: k})
	return tm.(Model)
}

// TestModelDialogUnchangedAppliesNothing: Enter with nothing moved sends no
// request and writes no note.
func TestModelDialogUnchangedAppliesNothing(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("nothing changed, so there is nothing to send")
	}
	if got := texts(m, entryNote); len(got) != 0 {
		t.Fatalf("notes %v", got)
	}
}

// TestModelDialogStepFailures fails each of the three steps in turn: the steps
// before it keep their notes, the failing one is named, and nothing after it
// runs.
func TestModelDialogStepFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fail      func(*Stub)
		wantNotes []string
		wantStep  string
	}{
		{"model", (*Stub).FailNextSetModel, nil, "model"},
		{"effort", func(s *Stub) { s.FailNextSetConfigAfter(0) }, []string{"model → fast"}, "effort"},
		{"fast", func(s *Stub) { s.FailNextSetConfigAfter(1) }, []string{"model → fast", "effort → high"}, "fast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(t)
			stub := m.sess.(*Stub)
			m.input.SetValue("/model")
			tm, _ := m.Update(enter())
			m = tm.(Model)
			m = pressKey(t, m, tea.KeyDown)
			m = pressKey(t, m, tea.KeyTab)
			m = pressKey(t, m, tea.KeyRight)
			m = pressKey(t, m, tea.KeyTab)
			m = pressKey(t, m, tea.KeyRight)
			tc.fail(stub)
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			m = flushCmd(t, m, cmd)

			if got := texts(m, entryNote); strings.Join(got, "|") != strings.Join(tc.wantNotes, "|") {
				t.Fatalf("notes %v, want %v", got, tc.wantNotes)
			}
			errs := texts(m, entryError)
			if len(errs) != 1 || !strings.HasPrefix(errs[0], tc.wantStep+": ") {
				t.Fatalf("errors %v, want one naming %q", errs, tc.wantStep)
			}
			// The failure re-reads the snapshot, so the rows show the agent's
			// state and not the dialog's hope.
			snap := stub.Snapshot()
			if m.snap.CurrentModel != snap.CurrentModel {
				t.Fatalf("model %q, agent has %q", m.snap.CurrentModel, snap.CurrentModel)
			}
			if got, want := agent.FastOn(m.snap), agent.FastOn(snap); got != want {
				t.Fatalf("fast %v, agent has %v", got, want)
			}
		})
	}
}

// TestModelDialogStaleFailureKeepsTheNewerChoice: the box closes optimistically,
// so a second apply can be under way before the first one answers. Only the
// newest apply settles the rows — an older one's failure re-read the snapshot
// and put back a value the user had already changed — and a successful apply
// settles them, so the rows cannot end up showing a value the agent does not
// have.
func TestModelDialogStaleFailureKeepsTheNewerChoice(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	if got := agent.EffortOption(m.snap); got == nil || got.Current != "medium" {
		t.Fatalf("effort starts at %+v", got)
	}
	// medium → high, holding its command.
	m = m.openModelDialog()
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight)
	tm, high := m.Update(enter())
	m = tm.(Model)
	// The user reopens the box and picks low, which answers second.
	m = m.openModelDialog()
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyLeft)
	m = pressKey(t, m, tea.KeyLeft)
	if m.mdlg.effort != "low" {
		t.Fatalf("the reopened box is on %q", m.mdlg.effort)
	}
	tm, low := m.Update(enter())
	m = tm.(Model)

	// The first apply fails, late; the second then lands.
	stub.FailNextSetConfig()
	m = flushCmd(t, m, high)
	m = flushCmd(t, m, low)

	if got := agent.EffortOption(stub.Snapshot()); got == nil || got.Current != "low" {
		t.Fatalf("the agent is on %+v, want low", got)
	}
	if got := agent.EffortOption(m.snap); got == nil || got.Current != "low" {
		t.Fatalf("the rows show %+v while the agent is on low", got)
	}
	if !strings.Contains(plainView(m), "(low)") {
		t.Fatalf("status row 1 should name the effort the agent has:\n%s", plainView(m))
	}
	if got := texts(m, entryNote); len(got) != 1 || got[0] != "effort → low" {
		t.Fatalf("notes %v, want only the step that landed", got)
	}
	if errs := texts(m, entryError); len(errs) != 1 || !strings.HasPrefix(errs[0], "effort: ") {
		t.Fatalf("errors %v, want the failure named", errs)
	}
}

// TestModelDialogTabSkipsMissingRows: without a fast option Tab cycles between
// the list and effort only, and the row is not drawn.
func TestModelDialogTabSkipsMissingRows(t *testing.T) {
	m := sized(t)
	m.snap.Config = m.snap.Config[:1]
	m = m.openModelDialog()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	if strings.Contains(plainView(m), "[off]") {
		t.Fatalf("a session with no fast option draws no fast row:\n%s", plainView(m))
	}
	m = pressKey(t, m, tea.KeyTab)
	if m.mdlg.focus != focusEffort {
		t.Fatalf("focus %v", m.mdlg.focus)
	}
	m = pressKey(t, m, tea.KeyTab)
	if m.mdlg.focus != focusList {
		t.Fatalf("tab should wrap back to the list, got %v", m.mdlg.focus)
	}
}

func TestModelSlashSetsModelAndEffort(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model grok high")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	m = flushCmd(t, m, cmd)
	if m.dialog != dialogNone {
		t.Fatal("/model with args opens no dialog")
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

func TestSetModelFailReverts(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.FailNextSetModel()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.input.SetValue("/model fast")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.snap.CurrentModel != "fast" {
		t.Fatalf("optimistic %q", m.snap.CurrentModel)
	}
	m = flushCmd(t, m, cmd)
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
	subs := subagentsFromTools(tools)
	if len(subs) > 0 {
		stub.SetSubagents(subs)
	}
	for i := range tools {
		tool := tools[i]
		tm, _ := m.Update(eventMsg{agent.Event{Type: agent.EventTool, Tool: &tool}})
		m = tm.(Model)
	}
	for i := range subs {
		info := subs[i]
		change := agent.SubagentChangeSpawned
		if subagentTerminal(info) {
			change = agent.SubagentChangeFinished
		}
		tm, _ := m.Update(eventMsg{agent.Event{
			Type:           agent.EventSubagent,
			Subagent:       &info,
			SubagentChange: change,
		}})
		m = tm.(Model)
	}
	return m
}

func subagentsFromTools(tools []agent.ToolEvent) []agent.SubagentInfo {
	var out []agent.SubagentInfo
	for _, t := range tools {
		if !t.IsTask() {
			continue
		}
		info := agent.SubagentInfo{
			ID:         t.ID,
			ToolCallID: t.ID,
			Status:     agent.SubagentRunning,
		}
		if t.Task != nil {
			info.Description = t.Task.Description
			info.Prompt = t.Task.Prompt
			info.Model = t.Task.Model
			info.DurationMs = t.Task.DurationMs
			info.SubagentType = t.Task.SubagentType
			if t.Task.Status != "" {
				info.Status = t.Task.Status
			}
		}
		if info.Description == "" {
			info.Description = strings.TrimPrefix(t.Title, "Task: ")
		}
		if t.Task == nil || t.Task.Status == "" {
			switch t.Status {
			case "failed":
				info.Status = agent.SubagentFailed
			case "cancelled":
				info.Status = agent.SubagentCancelled
			case "completed":
				info.Status = agent.SubagentCompleted
			}
		}
		out = append(out, info)
	}
	return out
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
	t.Run("help", func(t *testing.T) {
		// A builtin never queues, and /help needs nothing from the agent, so
		// it is answered mid-turn rather than held or refused.
		m := hangWorking(t)
		m.input.SetValue("/help")
		tm, _ := m.Update(enter())
		m = tm.(Model)
		if m.dialog != dialogHelp {
			t.Fatal("/help while working opens the help box")
		}
		if m.quitting {
			t.Fatal("/help while working must not quit")
		}
		if m.status != statusWorking {
			t.Fatalf("status %s", m.status)
		}
		if len(m.snap.Queue) != 0 {
			t.Fatalf("a builtin must never queue: %+v", m.snap.Queue)
		}
	})
	t.Run("model", func(t *testing.T) {
		// The builtins that do need the agent keep today's refusal, and
		// still never queue.
		m := hangWorking(t)
		m.input.SetValue("/model")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if m.dialog == dialogModel {
			t.Fatal("/model while working is refused")
		}
		if cmd != nil {
			t.Fatal("/model while working should not run a command")
		}
		if len(m.snap.Queue) != 0 {
			t.Fatalf("a builtin must never queue: %+v", m.snap.Queue)
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
	if len(m.main.entries) != 0 {
		t.Fatalf("clear left entries %+v", m.main.entries)
	}
	if len(m.main.toolLine) != 0 {
		t.Fatalf("clear left toolLine %+v", m.main.toolLine)
	}
	if len(m.main.pathDirs) != 0 || m.main.trimmed {
		t.Fatalf("clear left the path cache %+v (trimmed=%v)", m.main.pathDirs, m.main.trimmed)
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

// --- §3.3 plan-mode exit -------------------------------------------------

// intoPlanMode cycles the session into plan mode the way Shift+Tab does and
// runs the SetMode through, so the stub's own snapshot agrees with the model's
// and a refreshSnap cannot put the mode back.
func intoPlanMode(t *testing.T, m Model) Model {
	t.Helper()
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	if msg := runCmd(cmd); msg != nil {
		t.Fatalf("SetMode returned %+v", msg)
	}
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("mode %q, want plan", m.snap.CurrentMode)
	}
	return m
}

// feed applies events the way the event pump does.
func feed(t *testing.T, m Model, evs ...agent.Event) Model {
	t.Helper()
	for _, ev := range evs {
		tm, _ := m.Update(eventMsg{ev})
		m = tm.(Model)
	}
	return m
}

// startTurn sends a prompt the way Enter does and drops the command it
// returned, so a test can drive the turn's endings by hand without the stub
// answering as well. Every turn state below goes through it: the offer belongs
// to a turn, so a test that skipped the prompt would be holding a state craze
// cannot reach.
func startTurn(t *testing.T, m Model, text string) Model {
	t.Helper()
	m.input.SetValue(text)
	tm, _ := m.Update(enter())
	return tm.(Model)
}

// planTurn is a turn that ended with a reply: the prompt, then both endings in
// the order the live session usually produces them.
func planTurn(t *testing.T, m Model) Model {
	t.Helper()
	m = feed(t, startTurn(t, m, "plan it"),
		agent.Event{Type: agent.EventText, Text: "here is the plan"},
		agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	return tm.(Model)
}

func planOfferModel(t *testing.T) Model {
	t.Helper()
	m := planTurn(t, intoPlanMode(t, sized(t)))
	if !m.planOffering() {
		t.Fatalf("a finished plan-mode turn should offer:\n%s", plainView(m))
	}
	return m
}

// TestPlanOfferWaitsForBothEndings holds the ordering pin: EventDone arms the
// offer and promptDoneMsg settles the status, and neither order may show a
// placeholder Enter would not honour.
func TestPlanOfferWaitsForBothEndings(t *testing.T) {
	done := eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}}
	settled := promptDoneMsg{res: agent.Result{StopReason: "end_turn"}}
	for _, tc := range []struct {
		name string
		msgs []tea.Msg
	}{
		{"done first", []tea.Msg{done, settled}},
		{"status first", []tea.Msg{settled, done}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := startTurn(t, intoPlanMode(t, sized(t)), "plan it")
			m = feed(t, m, agent.Event{Type: agent.EventText, Text: "here is the plan"})
			var tm tea.Model
			for i, msg := range tc.msgs {
				tm, _ = m.Update(msg)
				m = tm.(Model)
				if i == 0 && m.planOffering() {
					t.Fatal("the offer is only actionable once both endings have landed")
				}
			}
			if !m.planOffering() {
				t.Fatal("expected the offer once both endings had landed")
			}
			if !strings.Contains(plainView(m), planOfferPlaceholder) {
				t.Fatalf("the placeholder is missing:\n%s", plainView(m))
			}
		})
	}
}

// TestPlanOfferNeedsAPlanToOffer covers the turns that leave nothing behind.
func TestPlanOfferNeedsAPlanToOffer(t *testing.T) {
	text := agent.Event{Type: agent.EventText, Text: "here is the plan"}
	ended := agent.Event{Type: agent.EventDone, StopReason: "end_turn"}
	for _, tc := range []struct {
		name string
		evs  []agent.Event
	}{
		{"cancelled", []agent.Event{text, {Type: agent.EventDone, StopReason: "cancelled"}}},
		{"error", []agent.Event{text, {Type: agent.EventError, Err: errors.New("boom")}, ended}},
		{"thought only", []agent.Event{{Type: agent.EventThought, Text: "hmm"}, ended}},
		{"tool only", []agent.Event{{Type: agent.EventTool, Tool: &agent.ToolEvent{
			ID: "sh-1", Kind: "execute", Title: "Shell", Status: "completed",
		}}, ended}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := feed(t, startTurn(t, intoPlanMode(t, sized(t)), "plan it"), tc.evs...)
			tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
			m = tm.(Model)
			if m.planArmed() || m.planOffering() {
				t.Fatal("this turn left no plan to implement")
			}
			if strings.Contains(plainView(m), planOfferPlaceholder) {
				t.Fatalf("placeholder drawn anyway:\n%s", plainView(m))
			}
		})
	}
}

// TestPlanOfferNeedsAnImplementMode: an agent that advertises no way to build
// the plan is never offered one.
func TestPlanOfferNeedsAnImplementMode(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.snap.CurrentMode = "plan"
	stub.snap.Modes = []agent.ModeInfo{{ID: "plan", Name: "Plan"}, {ID: "ask", Name: "Ask"}}
	m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = planTurn(t, tm.(Model))
	if m.implementModeID() != "" {
		t.Fatalf("implement mode %q, want none", m.implementModeID())
	}
	if m.planArmed() {
		t.Fatal("nothing to switch to, so nothing to offer")
	}
}

func TestPlanOfferCleared(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(*testing.T, Model) Model
	}{
		{"/clear", func(t *testing.T, m Model) Model {
			m.input.SetValue("/clear")
			tm, _ := m.Update(enter())
			return tm.(Model)
		}},
		{"the agent changes the mode", func(t *testing.T, m Model) Model {
			if err := m.sess.SetMode(context.Background(), "agent"); err != nil {
				t.Fatal(err)
			}
			return feed(t, m, agent.Event{Type: agent.EventMeta, Mode: "agent"})
		}},
		{"the agent changes the mode and changes it back", func(t *testing.T, m Model) Model {
			// The snapshot says "plan" before and after, so only the event
			// itself can say the mode moved at all.
			return feed(t, m, agent.Event{Type: agent.EventMeta, Mode: "plan"})
		}},
		{"the user changes the mode", func(t *testing.T, m Model) Model {
			tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
			return tm.(Model)
		}},
		{"the next send", func(t *testing.T, m Model) Model {
			m.input.SetValue("something else entirely")
			tm, _ := m.Update(enter())
			return tm.(Model)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.do(t, planOfferModel(t))
			if m.planArmed() {
				t.Fatal("the offer should be gone")
			}
			if strings.Contains(plainView(m), planOfferPlaceholder) {
				t.Fatalf("the placeholder survived:\n%s", plainView(m))
			}
		})
	}
}

// TestPlanOfferHiddenWhileTyping: the placeholder keys on Value()=="", the same
// rule the textarea draws any placeholder by, so it comes back on backspace.
func TestPlanOfferHiddenWhileTyping(t *testing.T) {
	m := planOfferModel(t)
	tm, _ := m.Update(runeKey('h'))
	m = tm.(Model)
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("a draft hides the placeholder:\n%s", plainView(m))
	}
	if !m.planArmed() {
		t.Fatal("typing refines the plan, it does not decline the offer")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = tm.(Model)
	if m.input.Value() != "" {
		t.Fatalf("draft %q", m.input.Value())
	}
	if !strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the offer should be back:\n%s", plainView(m))
	}
}

func TestPlanOfferEscKeepsFocus(t *testing.T) {
	m := planOfferModel(t)
	if !m.input.Focused() {
		t.Fatal("the composer starts focused")
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if m.planArmed() {
		t.Fatal("esc declines the offer")
	}
	if !m.input.Focused() {
		t.Fatal("esc on the offer must not blur the composer")
	}
}

// TestPlanOfferBeatsAgentPeek: Enter on an empty composer opens a running
// sub-agent, unless there is a plan on offer.
func TestPlanOfferBeatsAgentPeek(t *testing.T) {
	m := intoPlanMode(t, sized(t))
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = planTurn(t, m)
	if len(m.visibleAgents()) == 0 {
		t.Fatal("expected a lingering agent row")
	}
	if !m.planOffering() {
		t.Fatal("expected the offer")
	}
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.viewing != "" {
		t.Fatal("the offer outranks the sub-agent view")
	}
	if cmd == nil {
		t.Fatal("expected the SetMode command")
	}
}

// TestPlanImplementChainsSetModeThenPrompt is the whole success path: the mode
// lands first, the note and the user entry follow it, and only then is a prompt
// sent.
func TestPlanImplementChainsSetModeThenPrompt(t *testing.T) {
	m := planOfferModel(t)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected the SetMode command")
	}
	if m.snap.CurrentMode != "agent" {
		t.Fatalf("mode %q, want the optimistic agent", m.snap.CurrentMode)
	}
	if m.status != statusIdle || len(texts(m, entryUser)) != 1 {
		t.Fatalf("nothing is written or sent before SetMode comes back: status %s, users %q",
			m.status, texts(m, entryUser))
	}
	msg := runCmd(cmd)
	if _, ok := msg.(planImplementMsg); !ok {
		t.Fatalf("SetMode returned %T, want planImplementMsg", msg)
	}
	tm, cmd = m.Update(msg)
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected the prompt command")
	}
	if m.status != statusWorking {
		t.Fatalf("status %s", m.status)
	}
	// The note is written before the turn it explains.
	note, user := -1, -1
	for i, e := range m.main.entries {
		if e.kind == entryNote && strings.HasPrefix(e.text, "mode → agent") {
			note = i
		}
		if e.kind == entryUser && e.text == "Implement the plan above." {
			user = i
		}
	}
	if note < 0 || user < 0 || note > user {
		t.Fatalf("want the mode note then the user entry, got %d and %d:\n%s", note, user, plainView(m))
	}
	if m.planArmed() {
		t.Fatal("the offer is spent")
	}
	if got := runCmd(cmd); got == nil {
		t.Fatal("the prompt command should answer")
	}
}

func TestPlanImplementSetModeFailureSendsNoPrompt(t *testing.T) {
	m := planOfferModel(t)
	m.sess.(*Stub).FailNextSetMode()
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	msg := runCmd(cmd)
	if _, ok := msg.(planImplementFailedMsg); !ok {
		t.Fatalf("SetMode returned %T, want planImplementFailedMsg", msg)
	}
	tm, cmd = m.Update(msg)
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("a failed mode change sends no prompt")
	}
	if got := texts(m, entryUser); len(got) != 1 || got[0] != "plan it" {
		t.Fatalf("nothing was sent, so nothing was added to the transcript: %q", got)
	}
	if m.status != statusIdle {
		t.Fatalf("status %s", m.status)
	}
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("mode %q, want it reverted", m.snap.CurrentMode)
	}
	if len(texts(m, entryError)) == 0 {
		t.Fatal("expected an error note")
	}
	if !m.planOffering() {
		t.Fatal("the plan is still on screen, so it is still on offer")
	}
}

// TestPlanRefineStaysInPlanMode: typing instead of accepting is an ordinary
// send.
func TestPlanRefineStaysInPlanMode(t *testing.T) {
	m := planOfferModel(t)
	m.input.SetValue("more detail on step two")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected a prompt")
	}
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("mode %q, want plan", m.snap.CurrentMode)
	}
	if m.planArmed() {
		t.Fatal("the send retires the offer")
	}
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "more detail on step two" {
		t.Fatalf("user entries %q", got)
	}
	if m.status != statusWorking {
		t.Fatalf("status %s", m.status)
	}
}

// TestPlanImplementDropsAnAnswerForAFinishedTurn is the competing-turn defect:
// the offer is accepted, the user sends a prompt of their own while SetMode is
// still in flight, and the mode change comes back to a turn that no longer
// exists. Writing its note and its user entry then would put a prompt in the
// transcript that the session refuses as already in flight.
func TestPlanImplementDropsAnAnswerForAFinishedTurn(t *testing.T) {
	m := planOfferModel(t)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected the SetMode command")
	}
	// The user does not wait for it.
	m = startTurn(t, m, "something else entirely")
	if got := texts(m, entryUser); len(got) != 2 || got[1] != "something else entirely" {
		t.Fatalf("the user's own prompt should have started: %q", got)
	}
	msg := runCmd(cmd)
	if _, ok := msg.(planImplementMsg); !ok {
		t.Fatalf("SetMode returned %T", msg)
	}
	tm, cmd = m.Update(msg)
	m = tm.(Model)
	if cmd != nil {
		t.Fatal("a mode change for a finished turn sends no prompt")
	}
	for _, u := range texts(m, entryUser) {
		if u == "Implement the plan above." {
			t.Fatalf("the implement prompt was written anyway:\n%s", plainView(m))
		}
	}
	// The mode did change, so it is still noted — it is the entry and the
	// prompt behind it that belonged to the turn that is gone.
	noted := false
	for _, n := range texts(m, entryNote) {
		noted = noted || strings.HasPrefix(n, "mode → agent")
	}
	if !noted {
		t.Fatalf("the mode change should still be noted:\n%s", plainView(m))
	}
}

// TestPlanImplementFailureDoesNotReviveAClearedPlan: /clear while SetMode is in
// flight retires the offer, so the failure that would otherwise put it back has
// nothing to put back — the plan it described is no longer on screen.
func TestPlanImplementFailureDoesNotReviveAClearedPlan(t *testing.T) {
	m := planOfferModel(t)
	m.sess.(*Stub).FailNextSetMode()
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	m.input.SetValue("/clear")
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if len(m.main.entries) != 0 {
		t.Fatalf("/clear should have emptied the transcript: %d entries", len(m.main.entries))
	}
	msg := runCmd(cmd)
	if _, ok := msg.(planImplementFailedMsg); !ok {
		t.Fatalf("SetMode returned %T", msg)
	}
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if m.planArmed() || m.planOffering() {
		t.Fatal("a cleared transcript has no plan above to implement")
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the placeholder came back:\n%s", plainView(m))
	}
}

// TestPlanOfferIgnoresAnAbandonedTurnsLateEvents is the misattribution defect: a
// cancelled turn's buffered text and its own EventDone reach craze after its
// prompt returned, and neither may count for the turn after it. The turn is not
// over until both of its endings have landed, so no next turn can start over the
// events still draining and mistake them for its own.
func TestPlanOfferIgnoresAnAbandonedTurnsLateEvents(t *testing.T) {
	m := startTurn(t, intoPlanMode(t, sized(t)), "plan it")
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("esc while working cancels")
	}
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("the stream has not closed, so the turn is not over: status %s", m.status)
	}
	m = startTurn(t, m, "and now this")
	if got := texts(m, entryUser); len(got) != 1 {
		t.Fatalf("no turn may start while the last one is still draining: %q", got)
	}
	if len(m.snap.Queue) != 1 {
		t.Fatalf("the draft is queued instead: %+v", m.snap.Queue)
	}
	// The cancelled turn's own events land now, against the turn that made them.
	m = feed(t, m,
		agent.Event{Type: agent.EventText, Text: "here is the plan"},
		agent.Event{Type: agent.EventDone, StopReason: stopCancelled})
	if m.planArmed() {
		t.Fatal("a cancelled turn offers nothing")
	}
	// Both of that turn's endings have landed, so the queued message is what
	// starts next — and it is a turn of its own, with its own user entry.
	if m.status != statusWorking {
		t.Fatalf("the queue drains once the turn settles: status %s", m.status)
	}
	if got := texts(m, entryUser); len(got) != 2 {
		t.Fatalf("the queued message is sent as a turn: %q", got)
	}
	if len(m.snap.Queue) != 0 {
		t.Fatalf("the row left the queue: %+v", m.snap.Queue)
	}
	// The drained turn starts clean: the chunk that arrived late was not its
	// own, so its ending has no evidence to arm an offer with.
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.planArmed() || m.planOffering() {
		t.Fatalf("this turn said nothing, so it left no plan:\n%s", plainView(m))
	}
}

// TestPlanOfferIgnoresEmptyAssistantChunks: appendStream drops an empty chunk,
// so it is not on the screen and cannot be a plan — and the live adapter does
// emit them for content it cannot read as text.
func TestPlanOfferIgnoresEmptyAssistantChunks(t *testing.T) {
	m := startTurn(t, intoPlanMode(t, sized(t)), "plan it")
	m = feed(t, m,
		agent.Event{Type: agent.EventText, Text: ""},
		agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.planArmed() || m.planOffering() {
		t.Fatal("an empty reply left nothing to implement")
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the placeholder was drawn for an empty reply:\n%s", plainView(m))
	}
}

// TestPlanOfferRetiredByACardInEitherOrder: an action between the turn's two
// endings kills the offer whichever ending it landed between, so the two
// orderings stay indistinguishable.
func TestPlanOfferSurvivesAnAnsweredCardInEitherOrder(t *testing.T) {
	done := eventMsg{agent.Event{Type: agent.EventDone, StopReason: "end_turn"}}
	settled := promptDoneMsg{res: agent.Result{StopReason: "end_turn"}}
	card := eventMsg{agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{
		ID:      "perm-1",
		Tool:    "Shell",
		Options: []agent.PermissionOption{{OptionID: "ok", Name: "Allow once", Kind: "allow_once"}},
	}}}
	for _, tc := range []struct {
		name string
		msgs []tea.Msg
	}{
		{"done, card, status", []tea.Msg{done, card, settled}},
		{"status, card, done", []tea.Msg{settled, card, done}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := startTurn(t, intoPlanMode(t, sized(t)), "plan it")
			m = feed(t, m, agent.Event{Type: agent.EventText, Text: "here is the plan"})
			for _, msg := range tc.msgs {
				tm, _ := m.Update(msg)
				m = tm.(Model)
			}
			// The card owns Enter, so nothing is offered while it is up.
			if !m.cardOpen() {
				t.Fatal("this case needs the card still open")
			}
			if m.planOffering() {
				t.Fatal("the card is up and owns Enter; the offer must not compete for it")
			}
			// Answering it must leave the offer standing, in either order: a
			// live plan-mode turn always produces a cursor/create_plan card
			// before the ending that arms the offer.
			m, _ = press(m, runeKey('a'))
			if m.cardOpen() {
				t.Fatal("'a' should have answered the permission card")
			}
			if !m.planOffering() {
				t.Fatal("the card was answered and the turn still earns the offer, so it must show")
			}
		})
	}
}

// TestPlanOfferEscWhileWorkingRetiresIt: the offer is armed by EventDone while
// the prompt has yet to return, so Esc is still a cancel — and a cancel declines
// the offer as surely as an Esc on the composer does.
func TestPlanOfferEscWhileWorkingRetiresIt(t *testing.T) {
	m := startTurn(t, intoPlanMode(t, sized(t)), "plan it")
	m = feed(t, m,
		agent.Event{Type: agent.EventText, Text: "here is the plan"},
		agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	if !m.planArmed() {
		t.Fatal("EventDone arms the offer")
	}
	if m.status != statusWorking {
		t.Fatalf("the prompt has not returned: status %s", m.status)
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("esc while working cancels the turn")
	}
	tm, _ = m.Update(promptDoneMsg{res: agent.Result{StopReason: stopCancelled}})
	m = tm.(Model)
	if m.planArmed() || m.planOffering() {
		t.Fatalf("the cancel declined the offer:\n%s", plainView(m))
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the placeholder survived the cancel:\n%s", plainView(m))
	}
}

// TestPlanOfferSurvivesACreatePlanCard is the live scenario, reduced. cursor
// answers a plan-mode turn with a cursor/create_plan card *and* assistant text,
// in that order, so a card arrival that retired the offer meant the offer could
// never appear in a real session — which is exactly what the live run at 120x40
// found. The card suppresses while it is up; answering it leaves the offer.
func TestPlanOfferSurvivesACreatePlanCard(t *testing.T) {
	m := startTurn(t, intoPlanMode(t, sized(t)), "plan it")
	// The order cursor really sends: some text, the plan card, more text, done.
	m = feed(t, m, agent.Event{Type: agent.EventText, Text: "I'll look at main.go first."})
	m = feed(t, m, agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{
		ID:       "plan-1",
		Name:     "Print current time",
		Overview: "Change main.go so it prints the current time.",
		Plan:     "In main.go, replace the hi print with the current time.",
	}})
	if !m.cardOpen() {
		t.Fatal("the create_plan card should be up")
	}
	m = feed(t, m, agent.Event{Type: agent.EventText, Text: "That is the whole plan."})
	if m.planOffering() {
		t.Fatal("the card is still up and owns Enter")
	}
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	tm, _ := m.Update(promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})
	m = tm.(Model)
	if m.planOffering() {
		t.Fatal("still unanswered, so still no offer")
	}
	m, _ = press(m, runeKey('a'))
	if m.cardOpen() {
		t.Fatal("'a' should have accepted the plan card")
	}
	if !m.planOffering() {
		t.Fatal("the plan was accepted and the turn earned the offer, so it must show")
	}
	if !strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the offer placeholder is not on screen:\n%s", plainView(m))
	}
}
