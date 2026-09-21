package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

// isolateSkillsHome points HOME at an empty directory, which isolates the
// skills and plugin caches, and clears CRAZE_HOME so the craze directory sits
// under that HOME too: a test that saves a theme or a provider must not write
// into a CRAZE_HOME the developer exported.
func isolateSkillsHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRAZE_HOME", "")
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
	return startSession(t, stub, ws, cols, rows)
}

// startSession is startStub for a session that is not a bare Stub — the
// scripted decorator — so the standard config stays in one place.
func startSession(t *testing.T, sess agent.Session, ws string, cols, rows int) Model {
	t.Helper()
	m := New(Config{
		Session:   sess,
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

// TestEnterSendsAndFollowUp is two whole turns driven the way a user drives
// them: Enter, the stub answers on its own goroutine, and the model is waited
// on until the reply is on screen and the turn is over. Nothing here says how a
// turn ends — the status going idle is what "the turn is over" means to a user,
// and it is the only claim the test makes about it.
func TestEnterSendsAndFollowUp(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m = pumpEnter(t, m, "hello")
	if m.status != statusWorking {
		t.Fatalf("status %s", m.status)
	}
	if got := strings.Join(texts(m, entryUser), ""); got != "hello" {
		t.Fatalf("user %q", got)
	}
	// Enter with nothing in the composer — send() cleared it — starts nothing.
	m = pumpKey(t, m, enter())
	if n := turnsStarted(stub); n != 1 {
		t.Fatalf("an empty Enter while working started %d turns", n)
	}

	m = pumpUntil(t, m, allOf(isIdle, viewHas("echo: hello")))
	// Nothing of the first turn is left to report, so the follow-up below cannot
	// be disturbed by it.
	m = pumpSettled(t, m)

	m = pumpEnter(t, m, "again")
	if m.status != statusWorking {
		t.Fatalf("the follow-up should send: status %s", m.status)
	}
	m = pumpUntil(t, m, allOf(isIdle, viewHas("follow-up: again")))
	assertPrompts(t, stub, "hello", "again")
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

// TestEscCancelsWorkingTurn is Esc on a turn the agent is running: the turn
// ends, the model goes idle, and the acknowledgement is the one note a cancel
// leaves. The turn is a hung one, so which of the two real cancel routes the
// scheduler takes — the hung prompt released by the cancel, or a claim the
// cancel reaches first, which withdraws — is not fixed; both are the session's
// own and both leave exactly this, which is the whole of what a user sees.
func TestEscCancelsWorkingTurn(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.HangNext()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = pumpEnter(t, tm.(Model), "wait")
	if m.status != statusWorking {
		t.Fatal("want working")
	}
	m = pumpEsc(t, m)
	m = pumpUntil(t, m, isIdle)
	// Nothing of the turn is left to report, so "exactly one note" is a claim
	// about a finished turn.
	m = pumpSettled(t, m)
	if notes := texts(m, entryNote); len(notes) != 1 || notes[0] != stopCancelled {
		t.Fatalf("a cancel leaves exactly one note: %q", notes)
	}
	if rows := texts(m, entryError); len(rows) != 0 {
		t.Fatalf("a cancel is not an error: %q", rows)
	}
	assertPrompts(t, stub, "wait")
}

// TestEscDuringTheCatalogWaitSettlesTheTurn is Esc while the first prompt is
// still held back for the agent's command catalog. That prompt opened no turn,
// so the session emits nothing at all for it and its error is the whole of the
// ending — and what it has to leave on screen is what every other cancel
// leaves: the draft where the user typed it, the note under it, no error.
//
// Both commands run for real — the one Enter returned, which parks, and the one
// Esc returned, which frees it — so the ending the model settles on is the
// session's own and not one written here. What this does not test is the race
// that follows: the session-level TestCancelDuringTheWaitLeavesTheNextPromptAlone
// owns what Cancel may still do once the freed prompt has returned.
func TestEscDuringTheCatalogWaitSettlesTheTurn(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	m := New(Config{Session: stub, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	// heldTurn is the barrier: the prompt runs where the runtime runs it, and
	// without waiting for it to reach the wait the Esc could land first — a
	// cancel of a claimed prompt, not of a prompt already parked, which is the
	// only thing this test is about.
	m = heldTurn(t, tm.(Model), stub, "/probe-echo banana")

	m = pumpEsc(t, m)
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if m.err != "" {
		t.Fatalf("a cancel is not an error: %q", m.err)
	}
	if rows := texts(m, entryError); len(rows) != 0 {
		t.Fatalf("the cancel failed: %q", rows)
	}
	view := plainView(m)
	if !strings.Contains(view, "/probe-echo banana") {
		t.Fatalf("the draft left the transcript:\n%s", view)
	}
	if !strings.Contains(view, stopCancelled) {
		t.Fatalf("the cancel left no note:\n%s", view)
	}
}

// TestEscRightAfterEnterWithdrawsTheClaimedPrompt is #18's pre-open window at
// the TUI: Enter and then Esc in consecutive Updates, with the prompt claimed and
// nothing on the wire when the cancel lands. Enter claims the turn inside Update
// — the engine's Submit calls the session's Begin in the same locked section that
// reserves it, on the caller's goroutine — so the cancel is this prompt's: the
// prompt withdraws when its continuation goes on, and the cancel reaches nothing.
// What is left is what every cancel leaves: the row, the note, no error, and no
// failed cancel.
//
// Two barriers hold the window open, because the continuation runs on a goroutine
// of the engine's and no command of the model's can be withheld to delay it: the
// script's, which stops the continuation before it has looked at anything, and
// the session's, which says the cancel has arrived.
func TestEscRightAfterEnterWithdrawsTheClaimedPrompt(t *testing.T) {
	m, sess := scriptedModel(t)
	sc := scriptWithheld()
	sess.Script(sc)
	m = pumpEnter(t, m, "stop me")
	if m.status != statusWorking {
		t.Fatal("want working")
	}
	awaitBarrier(t, sc.claimed, "the prompt being claimed with nothing on the wire")

	m = pumpEsc(t, m)
	awaitBarrier(t, sess.Cancels(), "the cancel reaching the session")
	// Only now does the claimed prompt's continuation go on, and it finds itself
	// cancelled before it opened anything.
	sc.Open()

	m = pumpUntil(t, m, isIdle)
	// The claims below are all "nothing else happened", so nothing may still be
	// on its way when they are made.
	m = pumpSettled(t, m)
	if m.err != "" {
		t.Fatalf("a cancel is not an error: %q", m.err)
	}
	if rows := texts(m, entryError); len(rows) != 0 {
		t.Fatalf("error rows %q", rows)
	}
	view := plainView(m)
	if !strings.Contains(view, "stop me") {
		t.Fatalf("the prompt left the transcript:\n%s", view)
	}
	// Exactly one note, and no assistant row, is how the transcript says the
	// withdrawn prompt emitted nothing at all: an EventDone of its own would
	// have written a second cancelled note.
	if notes := texts(m, entryNote); len(notes) != 1 || notes[0] != stopCancelled {
		t.Fatalf("notes %q, want one %q", notes, stopCancelled)
	}
	if got := texts(m, entryAssistant); len(got) != 0 {
		t.Fatalf("a withdrawn prompt said %q", got)
	}
	assertPrompts(t, sess, "stop me")
	if n := sess.CancelsSent(); n != 0 {
		t.Fatalf("%d cancels reached the agent for a prompt that never did", n)
	}
}

// TestEnterDuringAForeignTurnTheModelHasNotSeenQueuesInstead is a RECORDED
// BEHAVIOUR CHANGE, and this is what it changed to.
//
// The agent is running a turn of its own and the event that says so is still in
// the channel, so the model's own mirror would let Enter send. At the baseline it
// did: the prompt was claimed, its row drawn, the model went working, and an Esc
// in the next Update withdrew that claim — the text was never sent
// (TestEscAfterEnterDuringAForeignTurnWritesNoCancel, retired with this).
//
// It queues now, because admission is the engine's and the engine reads the
// session's own flag in the section that would claim the turn (plan 021 §3.4: a
// turn is admitted only with "no foreign turn reported"). The model's view may lag
// that flag; the engine's never does. And a queued row is exactly what a foreign
// turn the model DOES know about has always produced
// (TestEnterDuringAForeignTurnQueues), so the two windows now agree instead of
// behaving differently depending on which events had been delivered.
//
// What the user is owed is a way out, and they have the ordinary one: the row is in
// the band, visible, and removable with the band's own verbs — asserted below —
// and it drains by itself when the agent's turn ends.
func TestEnterDuringAForeignTurnTheModelHasNotSeenQueuesInstead(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	m := startStub(t, stub, t.TempDir(), 80, 24)
	// The flag is set on the session; the event that would tell the model is still
	// in the channel, unread, so the model's mirror still says idle.
	stub.SetForeignTurn(agent.ForeignTurnInfo{Running: true})
	if m.snap.ForeignTurn {
		t.Fatal("setup: the model has already heard about the foreign turn")
	}
	m = pumpEnter(t, m, "mine")
	if m.status == statusWorking {
		t.Fatal("nothing may be claimed into a turn the session knows is the agent's")
	}
	if got := queueTexts(m); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("the draft should be queued: %q", got)
	}
	if n := turnsStarted(stub); n != 0 {
		t.Fatalf("%d prompts reached the session during a foreign turn", n)
	}
	// Visible, and the band's own Backspace takes it back: the way out Esc used to
	// be.
	if !strings.Contains(plainView(m), "#1 mine") {
		t.Fatalf("the row is not on screen:\n%s", plainView(m))
	}
	withdrawn := pumpKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if !withdrawn.queueFocus {
		t.Fatalf("↑ did not reach the band:\n%s", plainView(withdrawn))
	}
	withdrawn = pumpKey(t, withdrawn, tea.KeyMsg{Type: tea.KeyBackspace})
	if !queueEmpty(withdrawn) {
		t.Fatalf("the row could not be taken back: %q", queueTexts(withdrawn))
	}
	// The rest of the case is the row left in place, so it is put back.
	enqueueRow(t, m, "mine")

	// Esc has nothing of craze's own to stop, so the agent's turn is left alone.
	m = pumpEsc(t, m)
	m = pumpSettled(t, m)
	if n := stub.CancelsSent(); n != 0 {
		t.Fatalf("Esc wrote %d cancels for a turn craze never started", n)
	}
	if !stub.Snapshot().ForeignTurn {
		t.Fatal("the foreign turn was stopped")
	}

	// It ends, and the row behind it drains.
	stub.SetForeignTurn(agent.ForeignTurnInfo{Running: false})
	m = pumpUntil(t, m, allOf(turnsReached(stub, 1), isIdle))
	m = pumpSettled(t, m)
	if m.err != "" {
		t.Fatalf("err %q", m.err)
	}
	if !queueEmpty(m) {
		t.Fatalf("the row did not drain: %q", queueTexts(m))
	}
	if got := texts(m, entryUser); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("the drained row draws its own user entry: %q", got)
	}
	assertPrompts(t, stub, "mine")
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
	// The claim is taken inside Update, so a send would already be recorded.
	if n := turnsStarted(m.sess.(*Stub)); n != 0 {
		t.Fatalf("alt+enter started %d turns", n)
	}
	_ = cmd
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
	if msg := cmd(); msg != (modeAppliedMsg{gen: m.modeGen, id: "plan"}) {
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

// --- mode round trips ----------------------------------------------------
//
// A mode change is a round trip. The chip flips the moment the user asks for
// it, and the session's own snapshot only catches up when the agent answers —
// so for the length of that trip refreshSnap draws the requested mode instead
// of the session's. That mask is what stops an unrelated update flickering the
// chip back, and it is also what makes a wrong answer expensive: a mask that is
// never taken down, or taken down by the wrong answer, leaves the chip lying
// for the life of the process rather than for a frame. Model.modeGen is what
// tells the answers apart. These are the interleavings it has to survive.

// askMode is `/plan`, `/ask` or `/agent` typed into the composer. The command
// it produced comes back unrun, so a test can decide separately when the RPC
// happens and when its answer reaches Update — which is the whole subject
// here.
func askMode(t *testing.T, m Model, id string) (Model, tea.Cmd) {
	t.Helper()
	m.input.SetValue("/" + id)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd == nil {
		t.Fatalf("/%s produced no SetMode command", id)
	}
	if m.snap.CurrentMode != id {
		t.Fatalf("/%s left the chip on %q: the flip is optimistic", id, m.snap.CurrentMode)
	}
	return m, cmd
}

// deliver hands a message the model's own command produced back to Update, the
// way the program loop does.
func deliver(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	if msg == nil {
		t.Fatal("nothing to deliver")
	}
	tm, _ := m.Update(msg)
	return tm.(Model)
}

// sessionMode is the mode the stub session is really in: the truth the chip has
// to agree with once nothing of craze's own is in flight.
func sessionMode(s *Stub) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.CurrentMode
}

// agentSetsMode is the agent changing the mode by itself — the session records
// it, and announces it with an EventMeta the test feeds where it wants it.
func agentSetsMode(s *Stub, id string) {
	s.mu.Lock()
	s.snap.CurrentMode = id
	s.mu.Unlock()
}

// unrelatedUpdate is the shape the mask exists for: an available_commands_update
// arrives as a mode-less EventMeta, refreshSnap re-reads the whole snapshot, and
// the session's answer for the mode is still the one the user just left.
func unrelatedUpdate(t *testing.T, m Model) Model {
	t.Helper()
	return feed(t, m, agent.Event{Type: agent.EventMeta})
}

// TestModeAnswersSettleInAnyOrder drives two mode changes that are on the wire
// at once. The commands run independently and their answers can reach Update in
// any order, including one that contradicts the order the session applied them
// in. Only one thing has to hold at the end of every interleaving: nothing is
// left in flight, and the chip says what the session says.
func TestModeAnswersSettleInAnyOrder(t *testing.T) {
	// req 0 is `/plan`, req 1 is `/ask`; run puts that request's RPC on the
	// session, and an op that is neither run nor refresh delivers the answer it
	// gave. fail arms the stub to refuse the run it is on.
	type op struct {
		req     int
		run     bool
		fail    bool
		refresh bool
	}
	for _, tc := range []struct {
		name string
		ops  []op
		want string
		errs int
	}{
		{
			name: "answered in the order they were asked",
			ops:  []op{{req: 0, run: true}, {req: 0}, {refresh: true}, {req: 1, run: true}, {req: 1}},
			want: "ask",
		},
		{
			// The second request reaches the session first, so the first one's
			// RPC is what the session ends up holding. Matching the chip to the
			// newest *request* would leave it on `ask` for ever.
			name: "the second is answered first",
			ops:  []op{{req: 1, run: true}, {req: 1}, {req: 0, run: true}, {req: 0}},
			want: "plan",
		},
		{
			// The refusal settles the chip, an unrelated update then reads the
			// session — and the acceptance that lands afterwards has to be what
			// puts the mode it made real back on the chip.
			name: "the second is refused, and answered first",
			ops:  []op{{req: 1, run: true, fail: true}, {req: 1}, {refresh: true}, {req: 0, run: true}, {req: 0}},
			want: "plan",
			errs: 1,
		},
		{
			// A refusal for a request the chip has moved on from may not roll
			// the chip back to where that request started.
			name: "the first is refused, and answered last",
			ops:  []op{{req: 1, run: true}, {req: 1}, {req: 0, run: true, fail: true}, {req: 0}},
			want: "ask",
			errs: 1,
		},
		{
			// Same, with the stale refusal arriving while the newer request is
			// still out: it may not take the newer one's protection down with
			// it either.
			name: "the first is refused, and answered first",
			ops:  []op{{req: 0, run: true, fail: true}, {req: 0}, {req: 1, run: true}, {req: 1}},
			want: "ask",
			errs: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(t)
			stub := m.sess.(*Stub)
			var cmds [2]tea.Cmd
			var answers [2]tea.Msg
			m, cmds[0] = askMode(t, m, "plan")
			m, cmds[1] = askMode(t, m, "ask")
			for i, o := range tc.ops {
				switch {
				case o.refresh:
					m = unrelatedUpdate(t, m)
				case o.run:
					if o.fail {
						stub.FailNextSetMode()
					}
					answers[o.req] = runCmd(cmds[o.req])
					if answers[o.req] == nil {
						t.Fatalf("op %d: request %d answered with nothing", i, o.req)
					}
				default:
					m = deliver(t, m, answers[o.req])
				}
			}
			if m.modeInFlight != "" {
				t.Fatalf("every answer is in: %q is still masking the chip", m.modeInFlight)
			}
			if got := sessionMode(stub); got != tc.want {
				t.Fatalf("the session ended at %q, the case is written for %q", got, tc.want)
			}
			if m.snap.CurrentMode != tc.want {
				t.Fatalf("the chip says %q, the session says %q", m.snap.CurrentMode, tc.want)
			}
			if got := len(texts(m, entryError)); got != tc.errs {
				t.Fatalf("%d errors on screen, want %d: %q", got, tc.errs, texts(m, entryError))
			}
		})
	}
}

// TestStaleModeRefusalLeavesTheNewerRequestAlone is the refusal case on its
// own, asserted mid-flight: the chip has moved two changes on from the request
// being refused, so the refusal may neither roll it back nor take down the
// protection the newer change is relying on — while still saying out loud that
// the agent refused something the user asked for.
func TestStaleModeRefusalLeavesTheNewerRequestAlone(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m, planCmd := askMode(t, m, "plan")
	m, askCmd := askMode(t, m, "ask")

	stub.FailNextSetMode()
	refusal := runCmd(planCmd)
	if _, ok := refusal.(revertModeMsg); !ok {
		t.Fatalf("a refused SetMode returned %T", refusal)
	}
	m = deliver(t, m, refusal)
	if m.snap.CurrentMode != "ask" {
		t.Fatalf("chip %q: a stale refusal put a mode two changes old back", m.snap.CurrentMode)
	}
	if m.modeInFlight != "ask" {
		t.Fatalf("modeInFlight %q: `ask` is still on the wire and still needs its mask", m.modeInFlight)
	}
	if got := texts(m, entryError); len(got) != 1 {
		t.Fatalf("the refusal is still the user's to see: %q", got)
	}
	// The mask is still doing its job, so the older mode cannot flicker back.
	m = unrelatedUpdate(t, m)
	if m.snap.CurrentMode != "ask" {
		t.Fatalf("chip %q after an unrelated update", m.snap.CurrentMode)
	}

	m = deliver(t, m, runCmd(askCmd))
	if m.modeInFlight != "" || m.snap.CurrentMode != "ask" || sessionMode(stub) != "ask" {
		t.Fatalf("settled with chip %q, session %q, in flight %q",
			m.snap.CurrentMode, sessionMode(stub), m.modeInFlight)
	}
}

// TestModeAnswerForARepeatedModeIsNotTheNewerRequests: mode ids repeat, so
// `plan` → `ask` → `plan` puts the same id on the wire twice. The first
// request's answer names `plan` and so does the third request's mask — matching
// on the id hands the late answer the newer request's protection, and the next
// unrelated update then flicks the chip to a mode the user has already left.
func TestModeAnswerForARepeatedModeIsNotTheNewerRequests(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m, first := askMode(t, m, "plan")
	m, second := askMode(t, m, "ask")
	m, third := askMode(t, m, "plan")

	// The first two reach the session; the third is still on the wire.
	firstMsg, secondMsg := runCmd(first), runCmd(second)
	if got := sessionMode(stub); got != "ask" {
		t.Fatalf("session %q, want the second request's mode", got)
	}
	m = deliver(t, m, firstMsg)
	if m.modeInFlight != "plan" {
		t.Fatalf("modeInFlight %q: the third request is unanswered, so its mask stands", m.modeInFlight)
	}
	m = unrelatedUpdate(t, m)
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("chip %q: an unrelated update flicked it to the session's older mode", m.snap.CurrentMode)
	}
	m = deliver(t, m, secondMsg)
	if m.modeInFlight != "plan" || m.snap.CurrentMode != "plan" {
		t.Fatalf("chip %q, in flight %q, after the second answer", m.snap.CurrentMode, m.modeInFlight)
	}

	m = deliver(t, m, runCmd(third))
	if m.modeInFlight != "" || m.snap.CurrentMode != "plan" || sessionMode(stub) != "plan" {
		t.Fatalf("settled with chip %q, session %q, in flight %q",
			m.snap.CurrentMode, sessionMode(stub), m.modeInFlight)
	}
}

// TestAgentModeArrivingBeforeTheAnswerIsNotMasked: the RPC succeeding and its
// answer being handled are two different moments, and the agent can change the
// mode by itself in between. The mask hides that change for as long as the
// answer is outstanding, so the answer has to read the session back — otherwise
// the chip keeps the mode craze asked for while the session is in another one,
// and no later event is owed to correct it.
func TestAgentModeArrivingBeforeTheAnswerIsNotMasked(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m, cmd := askMode(t, m, "plan")
	applied := runCmd(cmd)
	if got := sessionMode(stub); got != "plan" {
		t.Fatalf("session %q: the RPC was accepted", got)
	}
	// The agent moves the mode again, of its own accord, and its update is
	// handled before craze's own answer is.
	agentSetsMode(stub, "ask")
	m = feed(t, m, agent.Event{Type: agent.EventMeta, Mode: "ask"})
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("chip %q: the mask is what stops the round trip flickering", m.snap.CurrentMode)
	}

	m = deliver(t, m, applied)
	if m.modeInFlight != "" {
		t.Fatalf("modeInFlight %q: the answer for this request is in", m.modeInFlight)
	}
	if m.snap.CurrentMode != "ask" {
		t.Fatalf("chip %q, session %q: taking the mask down has to read the session back",
			m.snap.CurrentMode, sessionMode(stub))
	}
}

// TestStalePlanImplementAnswerLeavesTheNewerRequestAlone is the same ABA on the
// plan-offer path, which asks for a mode of its own: accepting the offer asks
// for `agent`, and Shift+Tab can cycle all the way back round to `agent` before
// the offer's answer lands.
func TestStalePlanImplementAnswerLeavesTheNewerRequestAlone(t *testing.T) {
	m := planOfferModel(t)
	tm, implement := m.Update(enter())
	m = tm.(Model)
	if implement == nil {
		t.Fatal("expected the SetMode command")
	}
	// The user's own turn makes the offer's answer too late to send anything,
	// so this test is about the mask and nothing else.
	m = startTurn(t, m, "something else entirely")
	// agent → plan → ask → agent: the third cycle asks for the same mode the
	// offer did.
	var cycle tea.Cmd
	for range 3 {
		tm, cycle = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		m = tm.(Model)
	}
	if m.snap.CurrentMode != "agent" || m.modeInFlight != "agent" {
		t.Fatalf("chip %q, in flight %q: the cycle should be back on agent",
			m.snap.CurrentMode, m.modeInFlight)
	}

	answer := runCmd(implement)
	if _, ok := answer.(planImplementMsg); !ok {
		t.Fatalf("the offer's SetMode returned %T", answer)
	}
	m = deliver(t, m, answer)
	if m.modeInFlight != "agent" {
		t.Fatalf("modeInFlight %q: the cycle's own request is still unanswered", m.modeInFlight)
	}
	m = deliver(t, m, runCmd(cycle))
	if m.modeInFlight != "" || m.snap.CurrentMode != "agent" {
		t.Fatalf("settled with chip %q, in flight %q", m.snap.CurrentMode, m.modeInFlight)
	}
}

// wedgedMode is a session that is alive but never answers session/set_mode. It
// is the case that used to pin the chip for the life of the process: Conn.Call
// blocks until its context says otherwise, and nothing else ever produces the
// message that takes the mask down.
type wedgedMode struct{ *Stub }

func (wedgedMode) SetMode(ctx context.Context, _ string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		// Nothing bounded this call, so in the program it would never have
		// come back at all. Say so rather than hang the suite out to the
		// package timeout.
		return errors.New("set_mode was never bounded")
	}
}

// TestModeChangeTimesOutInsteadOfPinningTheChip: an agent that does not answer
// has to produce the ordinary revert, because the revert is what takes the mask
// down. Without a deadline there is no answer at all and the chip keeps a mode
// the session was never in.
func TestModeChangeTimesOutInsteadOfPinningTheChip(t *testing.T) {
	defer func(d time.Duration) { modeCallTimeout = d }(modeCallTimeout)
	modeCallTimeout = 20 * time.Millisecond

	isolateSkillsHome(t)
	m := New(Config{Session: wedgedMode{NewStub()}, Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)

	m, cmd := askMode(t, m, "plan")
	answer := runCmd(cmd)
	revert, ok := answer.(revertModeMsg)
	if !ok {
		t.Fatalf("a wedged agent answered with %T, want the ordinary revert", answer)
	}
	if !errors.Is(revert.err, context.DeadlineExceeded) {
		t.Fatalf("revert error %v, want the call's own deadline", revert.err)
	}
	m = deliver(t, m, revert)
	if m.modeInFlight != "" {
		t.Fatalf("modeInFlight %q: a timeout clears the mask like any other refusal", m.modeInFlight)
	}
	if m.snap.CurrentMode != "agent" {
		t.Fatalf("chip %q, want it back where it started", m.snap.CurrentMode)
	}
	if got := texts(m, entryError); len(got) != 1 {
		t.Fatalf("the timeout is the user's to see: %q", got)
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
	// statusWorking is the model's own answer, and the claim behind it becomes a
	// turn on the driver's goroutine a moment later. A test that then cancels is
	// asking what reached the agent, and a cancel that arrives in that gap
	// withdraws the prompt instead of writing one (Stub.Cancel) — so the model
	// is not handed back until the turn it calls working is open on the wire.
	waitInTurn(t, stub)
	return m
}

// waitInTurn blocks until the stub's claimed prompt has opened its turn.
func waitInTurn(t *testing.T, s *Stub) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !s.InTurn() {
		if time.Now().After(deadline) {
			t.Fatal("the stub's prompt never opened its turn")
		}
		time.Sleep(time.Millisecond)
	}
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
		if got := queuedRows(m); len(got) != 0 {
			t.Fatalf("a builtin must never queue: %+v", got)
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
		if got := queuedRows(m); len(got) != 0 {
			t.Fatalf("a builtin must never queue: %+v", got)
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
// runs the SetMode through — answer included, the way the program loop would —
// so the stub's own snapshot agrees with the model's and the mode is no longer
// in flight.
func intoPlanMode(t *testing.T, m Model) Model {
	t.Helper()
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	msg := runCmd(cmd)
	// The generation is whatever this model is up to, so the assertion is on
	// the answer's shape and its mode, not on the counter.
	if applied, ok := msg.(modeAppliedMsg); !ok || applied.id != "plan" {
		t.Fatalf("SetMode returned %+v", msg)
	}
	tm, _ = m.Update(msg)
	m = tm.(Model)
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

// planTurn is a whole plan-mode turn, run for real: the stub answers every
// prompt with assistant text, which is what earns a plan-mode turn its offer,
// and the turn is waited out rather than ended by hand.
func planTurn(t *testing.T, m Model) Model {
	t.Helper()
	m = pumpEnter(t, m, "plan it")
	m = pumpUntil(t, m, allOf(isIdle, viewHas("echo: plan it")))
	// Every caller goes on to act on the offer the finished turn left, so the
	// turn has to be completely over: nothing of it still to report.
	return pumpSettled(t, m)
}

func planOfferModel(t *testing.T) Model {
	t.Helper()
	m := planTurn(t, intoPlanMode(t, sized(t)))
	if !m.planOffering() {
		t.Fatalf("a finished plan-mode turn should offer:\n%s", plainView(m))
	}
	return m
}

// TestPlanOfferAppearsOnlyOnceTheTurnIsOver is the user-visible half of
// TestPlanOfferWaitsForBothEndings, driven through a real turn: while the turn
// is working there is no placeholder to press Enter on, and once the turn is
// over there is. The ordering pin below owns the mechanism that guarantees it;
// this owns what the mechanism is for, which is why it survives the driver
// moving out of the model.
func TestPlanOfferAppearsOnlyOnceTheTurnIsOver(t *testing.T) {
	m := intoPlanMode(t, sized(t))
	m = pumpEnter(t, m, "plan it")
	if m.status != statusWorking {
		t.Fatalf("setup: status %s", m.status)
	}
	if m.planOffering() {
		t.Fatal("a running turn has no finished plan to offer")
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the placeholder is drawn mid-turn:\n%s", plainView(m))
	}
	m = pumpUntil(t, m, allOf(isIdle, viewHas(planOfferPlaceholder)))
	m = pumpSettled(t, m)
	if !m.planOffering() {
		t.Fatalf("the finished turn should offer:\n%s", plainView(m))
	}
}

// TestPlanOfferWaitsForBothEndings is retired with the two-ending race (plan 021
// C4): there is one ending now, so there is no permutation of two to walk. Its
// property has a new shape, which is what this replaces it with — the wire's
// EventDone arms the offer and the engine's ending is what makes it actionable,
// and in between the model is still working, so no placeholder Enter would not
// honour is ever on screen. It is driven for real rather than hand-fed: the
// script publishes its one terminal event and then holds the continuation short
// of returning, which is exactly the window.
//
// The property is also pinned end to end by
// TestPlanOfferAppearsOnlyOnceTheTurnIsOver.
func TestPlanOfferTheWiresEndingArmsTheOfferAndTheTurnsEndingShowsIt(t *testing.T) {
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	sc := scriptHeld().endsThenWaits()
	m = startScripted(t, m, sess, "plan it", sc)
	sess.Emit(agent.Event{Type: agent.EventText, Text: "here is the plan"})
	sc.Release()
	awaitBarrier(t, sc.ended, "the turn publishing its ending")

	// EventDone has armed the offer; the engine has not settled the turn,
	// because the continuation has not returned.
	m = pumpUntil(t, m, func(m Model) bool { return m.planArmed() })
	if m.status != statusWorking {
		t.Fatalf("the engine has not ended the turn yet, so the model is still working: %s", m.status)
	}
	if m.planOffering() {
		t.Fatal("the offer is only actionable once the turn has ended")
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the placeholder is drawn before the turn ended:\n%s", plainView(m))
	}

	sc.Return()
	m = pumpUntil(t, m, allOf(isIdle, viewHas(planOfferPlaceholder)))
	if !m.planOffering() {
		t.Fatalf("the ended turn should offer:\n%s", plainView(m))
	}
}

// TestPlanOfferNeedsAPlanToOffer covers the turns that leave nothing behind:
// each one is a real turn, held open while it says its piece and then ended the
// way the case names — cancelled, failed, or cleanly with nothing implementable
// in it.
func TestPlanOfferNeedsAPlanToOffer(t *testing.T) {
	text := agent.Event{Type: agent.EventText, Text: "here is the plan"}
	for _, tc := range []struct {
		name string
		// said is what the turn puts on the stream before it ends.
		said []agent.Event
		// fail ends the turn with an error; cancelled ends it with Esc.
		fail      error
		cancelled bool
	}{
		{name: "cancelled", said: []agent.Event{text}, cancelled: true},
		{name: "error", said: []agent.Event{text}, fail: errors.New("boom")},
		{name: "thought only", said: []agent.Event{{Type: agent.EventThought, Text: "hmm"}}},
		{name: "tool only", said: []agent.Event{{Type: agent.EventTool, Tool: &agent.ToolEvent{
			ID: "sh-1", Kind: "execute", Title: "Shell", Status: "completed",
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sess := scriptedModel(t)
			m = intoPlanMode(t, m)
			sc := scriptHeld()
			sc.fail = tc.fail
			m = startScripted(t, m, sess, "plan it", sc)
			for _, ev := range tc.said {
				sess.Emit(ev)
			}
			switch {
			case tc.cancelled:
				m = pumpEsc(t, m)
				m = pumpUntil(t, m, isIdle)
			case tc.fail != nil:
				sc.Release()
				m = pumpUntil(t, m, allOf(isErrored, errorRows(1)))
				m = pumpSettled(t, m)
				// A stream ending after the error must not arm an offer an
				// errored turn cannot honour. The chunk behind it is the marker
				// that says it was applied.
				sess.Emit(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
				sess.Emit(agent.Event{Type: agent.EventText, Text: "after the error"})
				m = pumpUntil(t, m, viewHas("after the error"))
			default:
				sc.Release()
				m = pumpUntil(t, m, isIdle)
				m = pumpSettled(t, m)
			}
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

// TestPlanOfferIgnoresAnAbandonedTurnsLateEvents is retired with the window it
// needed (plan 021 C4): the misattribution it guarded against was a prompt that
// had returned while its stream was still open, and the engine settles a turn only
// once its continuation has come back — after which the session has published
// everything it was going to — so a turn's events can no longer arrive inside the
// turn after it.
//
// The property it protected has a new shape, which is what this replaces it with:
// the offer belongs to the turn that earned it, keyed to that turn, so the plan a
// cancelled turn left on screen does not arm the turn drained behind it. That the
// queued row does drain, exactly once, is
// TestEveryEndingDrainsTheNextRowExactlyOnce.
func TestPlanOfferBelongsToTheTurnThatEarnedIt(t *testing.T) {
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	// Two scripted turns, in order: the one that plans and is cancelled, and the
	// one the drain starts behind it, which says nothing at all.
	planning, drained := scriptHeld(), scriptHeld()
	m = startScripted(t, m, sess, "plan it", planning)
	sess.Script(drained)
	sess.Emit(agent.Event{Type: agent.EventText, Text: "here is the plan"})
	m = pumpUntil(t, m, viewHas("here is the plan"))

	m = pumpEnter(t, m, "and now this")
	if got := queueTexts(m); len(got) != 1 {
		t.Fatalf("setup: the follow-up should be queued: %q", got)
	}
	// Esc cancels the planning turn; its settlement drains the row behind it, so
	// the model never leaves working.
	m = pumpEsc(t, m)
	awaitBarrier(t, drained.opened, "the drained turn opening")
	drained.Release()
	m = pumpUntil(t, m, allOf(isIdle, turnsReached(sess, 2)))
	m = pumpSettled(t, m)

	if got := texts(m, entryUser); len(got) != 2 {
		t.Fatalf("one user entry per turn: %q", got)
	}
	if m.planArmed() || m.planOffering() {
		t.Fatalf("the drained turn said nothing, so it left no plan of its own:\n%s", plainView(m))
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the placeholder came back for another turn's plan:\n%s", plainView(m))
	}
	assertPrompts(t, sess, "plan it", "and now this")
}

// TestPlanOfferIgnoresEmptyAssistantChunks: appendStream drops an empty chunk,
// so it is not on the screen and cannot be a plan — and the live adapter does
// emit them for content it cannot read as text.
func TestPlanOfferIgnoresEmptyAssistantChunks(t *testing.T) {
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	sc := scriptHeld()
	m = startScripted(t, m, sess, "plan it", sc)
	// The log orders it ahead of the ending, so waiting for the turn to be over
	// is waiting for the chunk to have been applied.
	sess.Emit(agent.Event{Type: agent.EventText, Text: ""})
	sc.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if m.planArmed() || m.planOffering() {
		t.Fatal("an empty reply left nothing to implement")
	}
	if strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the placeholder was drawn for an empty reply:\n%s", plainView(m))
	}
}

// TestPlanOfferSurvivesAnAnsweredCardInEitherOrder is retired with the two-ending
// race it permuted (plan 021 C4). This is the same scenario driven for real: the
// turn plans, raises a card, and ends; the card owns Enter while it is up, so
// nothing is offered; answering it leaves the offer standing, because a live
// plan-mode turn always produces a cursor/create_plan card before the ending that
// arms the offer. The card-plus-offer flow is also driven against the wire by the
// plan-mode frame goldens.
func TestPlanOfferSurvivesAnAnsweredCard(t *testing.T) {
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	sc := scriptHeld()
	m = startScripted(t, m, sess, "plan it", sc)
	sess.Emit(agent.Event{Type: agent.EventText, Text: "here is the plan"})
	sess.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{
		ID:      "perm-1",
		Tool:    "Shell",
		Options: []agent.PermissionOption{{OptionID: "ok", Name: "Allow once", Kind: "allow_once"}},
	}})
	m = pumpUntil(t, m, hasCard)

	// The card is answered while its own turn is still running, which is the
	// only order the wire produces — cursor blocks on the request and ends the
	// turn once it has its answer — and, since plan 021 §4, the only order
	// there is: a card whose turn ended is removed with the turn (the ask
	// registry ends it), so a card cannot be left standing to be answered
	// afterwards. The offer-hidden-behind-a-card case has its own test, where
	// the card belongs to no turn: TestPlanOfferSurvivesACardAfterTheWiresEnding.
	if m.planOffering() {
		t.Fatal("the turn has not ended; there is nothing to offer yet")
	}
	m, _ = press(m, runeKey('a'))
	if m.cardOpen() {
		t.Fatal("'a' should have answered the permission card")
	}
	sc.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if !m.planOffering() {
		t.Fatalf("the card was answered and the turn still earns the offer, so it must show:\n%s", plainView(m))
	}
}

// TestPlanOfferSurvivesACardAfterTheWiresEnding is the other order the retired
// "either order" test walked, and the one the engine's two endings make a real
// window rather than a message race: the wire's own done has landed — arming the
// offer — and the engine has not settled the turn, and the card arrives in between.
// It is raised, it owns Enter while it is up, and answering it leaves the offer
// standing exactly as it does when the card comes first.
func TestPlanOfferSurvivesACardAfterTheWiresEnding(t *testing.T) {
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	sc := scriptHeld().endsThenWaits()
	m = startScripted(t, m, sess, "plan it", sc)
	sess.Emit(agent.Event{Type: agent.EventText, Text: "here is the plan"})
	sc.Release()
	awaitBarrier(t, sc.ended, "the wire's own ending")
	// Between the two endings: the done has been published, the engine cannot have
	// settled because the continuation is held short of returning.
	m = pumpUntil(t, m, func(m Model) bool { return m.planArmed() })
	if m.status != statusWorking {
		t.Fatalf("the engine has not ended the turn yet: status %s", m.status)
	}
	sess.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{
		ID:      "perm-1",
		Tool:    "Shell",
		Options: []agent.PermissionOption{{OptionID: "ok", Name: "Allow once", Kind: "allow_once"}},
	}})
	m = pumpUntil(t, m, hasCard)
	if m.planOffering() {
		t.Fatal("the card is up and owns Enter; the offer must not compete for it")
	}

	sc.Return()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if !m.cardOpen() {
		t.Fatalf("the card the turn's ending did not answer is still up:\n%s", plainView(m))
	}
	if m.planOffering() {
		t.Fatal("still the card's Enter, even once the turn has ended")
	}
	m, _ = press(m, runeKey('a'))
	if m.cardOpen() {
		t.Fatal("'a' should have answered the permission card")
	}
	if !m.planOffering() {
		t.Fatalf("the card was answered and the turn still earns the offer:\n%s", plainView(m))
	}
}

// TestPlanOfferEscWhileWorkingRetiresIt: the offer is armed by EventDone while
// the prompt has yet to return, so Esc is still a cancel — and a cancel declines
// the offer as surely as an Esc on the composer does.
func TestPlanOfferEscWhileWorkingRetiresIt(t *testing.T) {
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	// The turn publishes its one ending — which arms the offer — and then stops
	// short of returning, so the prompt is still out and Esc is still a cancel.
	sc := scriptHeld().endsThenWaits()
	m = startScripted(t, m, sess, "plan it", sc)
	sess.Emit(agent.Event{Type: agent.EventText, Text: "here is the plan"})
	sc.Release()
	awaitBarrier(t, sc.ended, "the turn's ending")
	m = pumpUntil(t, m, func(m Model) bool { return m.planArmed() })
	if m.status != statusWorking {
		t.Fatalf("the prompt has not returned: status %s", m.status)
	}
	m = pumpEsc(t, m)
	sc.Return()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
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
	m, sess := scriptedModel(t)
	m = intoPlanMode(t, m)
	sc := scriptHeld()
	m = startScripted(t, m, sess, "plan it", sc)
	// The order cursor really sends: some text, the plan card, more text, done.
	sess.Emit(agent.Event{Type: agent.EventText, Text: "I'll look at main.go first."})
	sess.Emit(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{
		ID:       "plan-1",
		Name:     "Print current time",
		Overview: "Change main.go so it prints the current time.",
		Plan:     "In main.go, replace the hi print with the current time.",
	}})
	m = pumpUntil(t, m, hasCard)
	sess.Emit(agent.Event{Type: agent.EventText, Text: "That is the whole plan."})
	m = pumpUntil(t, m, viewHas("That is the whole plan."))
	if m.planOffering() {
		t.Fatal("the card is still up and owns Enter")
	}
	// Answered while its own turn is still running, which is the order cursor
	// sends and, since plan 021 §4, the only one there is: a card whose turn
	// ended is removed with it.
	m, _ = press(m, runeKey('a'))
	if m.cardOpen() {
		t.Fatal("'a' should have accepted the plan card")
	}
	sc.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if !m.planOffering() {
		t.Fatal("the plan was accepted and the turn earned the offer, so it must show")
	}
	if !strings.Contains(plainView(m), planOfferPlaceholder) {
		t.Fatalf("the offer placeholder is not on screen:\n%s", plainView(m))
	}
}

// windowTitleMsgs walks a batched Cmd for every tea.SetWindowTitle it carries,
// rendered with fmt.Sprint. bubbletea's setWindowTitleMsg is an unexported
// string type, so identifying it by its reflected type name and reading its
// value through fmt.Sprint is the only way in from outside the package
// (§3.10).
//
// The tick chain rides in the same batch (Update arms it last) and a real
// tea.Tick command sleeps for its own duration before returning — up to a
// minute, when the model is idle — so it is skipped by function name via
// runtime.FuncForPC rather than called: this walk must stay instant however
// far from a minute boundary the wall clock happens to be when it runs.
func windowTitleMsgs(cmd tea.Cmd) []string {
	var out []string
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		if name := runtime.FuncForPC(reflect.ValueOf(c).Pointer()).Name(); strings.Contains(name, "bubbletea.Tick") {
			return
		}
		msg := c()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				walk(sub)
			}
			return
		}
		if strings.Contains(fmt.Sprintf("%T", msg), "WindowTitleMsg") {
			out = append(out, fmt.Sprint(msg))
		}
	}
	walk(cmd)
	return out
}

// titledStub is startStub with the tab title switched on (every other test
// model leaves Config.TerminalTitle false, the tested default), sized but not
// yet started, so the caller can watch the very first title alongside every
// later transition.
func titledStub(t *testing.T, stub *Stub, ws string) Model {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{
		Session:       stub,
		Theme:         "tokyo-night",
		Workspace:     ws,
		Model:         "grok",
		Yolo:          true,
		TerminalTitle: true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return tm.(Model)
}

// TestWindowTitleCmdOnSizing is the pre-start moment: New alone sets no
// title (windowTitle is pure and the wrapper never ran), but the very first
// Update — sizing the frame, exactly as Run's first WindowSizeMsg does —
// already carries the pre-title "✦ craze", because no handler sets the title
// itself and this is the one comparison every transition passes through.
func TestWindowTitleCmdOnSizing(t *testing.T) {
	isolateSkillsHome(t)
	m := New(Config{
		Session:       NewStub(),
		Theme:         "tokyo-night",
		Workspace:     t.TempDir(),
		Yolo:          true,
		TerminalTitle: true,
	})
	tm, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	if got := windowTitleMsgs(cmd); len(got) != 1 || got[0] != "✦ craze" {
		t.Fatalf("first sizing = %v, want one %q", got, "✦ craze")
	}
	if m.lastTitle != "✦ craze" {
		t.Fatalf("lastTitle = %q, want %q", m.lastTitle, "✦ craze")
	}
}

// TestWindowTitleCmdFiresOnceOnChangeOnly is the write-only-on-change
// contract: a status change re-fires exactly once, and a message that leaves
// every input to windowTitle() untouched fires it not at all, however many
// times Update runs.
func TestWindowTitleCmdFiresOnceOnChangeOnly(t *testing.T) {
	m := titledStub(t, NewStub(), t.TempDir())

	tm, cmd := m.Update(startedMsg{})
	m = tm.(Model)
	if got := windowTitleMsgs(cmd); len(got) != 0 {
		t.Fatalf("startedMsg while still idle should not re-fire the title: %v", got)
	}

	m.input.SetValue("go")
	tm, cmd = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("the send should have started a turn: status %v", m.status)
	}
	if got := windowTitleMsgs(cmd); len(got) != 1 || got[0] != "❖ craze" {
		t.Fatalf("status -> working = %v, want one %q", got, "❖ craze")
	}

	// Unrelated to the title: still working, no card, same text. Run it twice
	// so a bug that fires on every Update rather than on change shows up
	// however many times the assertion below is repeated.
	for i := 0; i < 2; i++ {
		tm, cmd = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = tm.(Model)
		if got := windowTitleMsgs(cmd); len(got) != 0 {
			t.Fatalf("an unrelated resize should not re-fire the title: %v", got)
		}
	}
}

// TestWindowTitleOffSwitchEmitsNothing is Config.TerminalTitle's false path:
// every existing test model leaves it off, and this pins that the reason is a
// gate the Update wrapper actually honours, not an accident of the zero
// value never being exercised.
func TestWindowTitleOffSwitchEmitsNothing(t *testing.T) {
	isolateSkillsHome(t)
	m := startStub(t, NewStub(), t.TempDir(), 80, 24)
	if m.terminalTitle {
		t.Fatal("startStub's Config never sets TerminalTitle, so this must be false")
	}
	m.input.SetValue("go")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if got := windowTitleMsgs(cmd); len(got) != 0 {
		t.Fatalf("terminal_title off should never emit a title: %v", got)
	}
	if m.lastTitle != "" {
		t.Fatalf("lastTitle should stay empty with the title off, got %q", m.lastTitle)
	}
}
