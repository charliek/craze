package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// The mode and the keys (plan 022 A15, A16).

// runningShell starts a command that lives until something kills it, and hands
// back the model running it, the marker its child carries — the only honest way
// to ask whether the process group is still there — and the channel the run's
// own message arrives on.
func runningShell(t *testing.T, m Model) (Model, string, chan tea.Msg) {
	t.Helper()
	marker := shellMarker(t)
	m.input.SetValue("!" + sleeperScript(marker))
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("enter in shell mode returned no command to run")
	}
	if !m.shellRunning() {
		t.Fatal("enter in shell mode did not start a command")
	}
	// Buffered and then closed: the test reads the run's own message, and the
	// cleanup below reads the closed channel rather than competing for it.
	done := make(chan tea.Msg, 1)
	go func() {
		done <- runCmd(cmd)
		close(done)
	}()
	// The grandchild the script starts, not the shell craze started: waiting for
	// the deepest of them is what makes "the marker is gone" below mean the
	// whole group and not only the process craze holds a pid for.
	if !waitMarker(t, leafMarker(marker), true, 10*time.Second) {
		t.Fatal("the command never started")
	}
	ctl := m.shell
	t.Cleanup(func() {
		ctl.shutdown()
		select {
		case <-done:
		case <-time.After(shellShutdownWait + 10*time.Second):
			t.Error("the run's goroutine never returned")
		}
	})
	return m, marker, done
}

// runShellThrough types a draft, runs the command Enter returned and applies
// its result, so the caller holds the model a real session would have once the
// command was over.
func runShellThrough(t *testing.T, m Model, draft string) Model {
	t.Helper()
	m.input.SetValue(draft)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	msg := runCmd(cmd)
	done, ok := msg.(shellDoneMsg)
	if !ok {
		t.Fatalf("running the command returned %T, want a shellDoneMsg", msg)
	}
	tm, _ = m.Update(done)
	return tm.(Model)
}

// shellRows is what the transcript draws for each `!` row, one element per
// entry with its rows joined.
func shellRowsDrawn(m Model) []string {
	var out []string
	for _, e := range m.main.entries {
		if e.kind == entryShell {
			out = append(out, plain(strings.Join(e.rendered, "\n")))
		}
	}
	return out
}

func shellEntries(m Model) int {
	n := 0
	for _, e := range m.main.entries {
		if e.kind == entryShell {
			n++
		}
	}
	return n
}

// TestShellModeIsTheDraftAndTheQueueEditor is the whole definition: the first
// byte of the draft and whether a queued row is being edited. Nothing else, and
// no flag — which is why the leading-space escape hatch works at all.
func TestShellModeIsTheDraftAndTheQueueEditor(t *testing.T) {
	for _, tc := range []struct {
		name, draft, edit string
		want              bool
	}{
		{"a command", "!ls", "", true},
		{"a bare bang", "!", "", true},
		{"a multi-line script", "!cd /tmp\nls", "", true},
		{"the escape hatch", " !important", "", false},
		{"a bang later on", "tell me about !ls", "", false},
		{"an empty draft", "", "", false},
		{"an ordinary prompt", "ls", "", false},
		{"a slash command", "/help", "", false},
		{"the queue editor holds a bang row", "!ls", "q-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(t)
			m.input.SetValue(tc.draft)
			m.queueEdit = tc.edit
			if got := m.shellMode(); got != tc.want {
				t.Fatalf("shellMode() = %v for draft %q (edit %q), want %v", got, tc.draft, tc.edit, tc.want)
			}
		})
	}
}

// TestShellLeadingSpaceGoesToTheAgent is the escape hatch, end to end: send
// trims the draft, so the space that took it out of shell mode never reaches
// the wire either.
func TestShellLeadingSpaceGoesToTheAgent(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m = typeEnter(t, m, " !important")
	if got := stub.Prompts(); len(got) != 1 || got[0] != "!important" {
		t.Fatalf("prompts %q, want one \"!important\"", got)
	}
	if n := shellEntries(m); n != 0 {
		t.Fatalf("%d commands ran; a leading space is a prompt", n)
	}
}

// TestShellModeNeverOpensTheSlashMenu: `!ls /usr` must not offer /usr. The menu
// is suppressed at its own gate, so the band and every key that reads it agree.
func TestShellModeNeverOpensTheSlashMenu(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/usr")
	if !m.slashMenuOpen() {
		t.Fatal("setup: a bare /usr token is exactly what opens the menu")
	}
	m.input.SetValue("!ls /usr")
	if m.slashMenuOpen() {
		t.Fatal("shell mode must close the menu")
	}
	if rows := m.slashRows(); rows != 0 {
		t.Fatalf("slashRows = %d in shell mode", rows)
	}
	if m.slashActive() {
		t.Fatal("slashActive in shell mode")
	}
	if rows := m.overlayRows(); rows != 0 {
		t.Fatalf("the band asked the layout for %d rows in shell mode", rows)
	}
}

// TestShellEnterRefusalsKeepTheDraft is every way Enter says no: the draft is
// still there afterwards, because a refusal the user has to retype is a refusal
// that loses their work.
func TestShellEnterRefusalsKeepTheDraft(t *testing.T) {
	t.Run("the session is not ready", func(t *testing.T) {
		isolateSkillsHome(t)
		m := New(Config{Session: NewStub(), Workspace: t.TempDir(), Yolo: true})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = tm.(Model)
		if m.sessionReady() {
			t.Fatal("setup: the session must still be starting")
		}
		m.input.SetValue("!echo nope")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if got := runCmd(cmd); got != nil {
			t.Fatalf("a refused Enter returned %T", got)
		}
		if m.input.Value() != "!echo nope" || shellEntries(m) != 0 {
			t.Fatalf("draft %q, %d rows", m.input.Value(), shellEntries(m))
		}
	})

	t.Run("a card is open", func(t *testing.T) {
		m := sized(t)
		m.yolo = false
		m = cardEvent(t, m, m.sess.(*Stub), agent.Event{
			Type: agent.EventPermission,
			Permission: &agent.PermissionEvent{
				ID: "perm-1", Tool: "Shell",
				Options: []agent.PermissionOption{{OptionID: "opt-once", Kind: "allow_once"}},
			},
		})
		if !m.cardOpen() {
			t.Fatal("setup: no card")
		}
		m.input.SetValue("!echo nope")
		tm, _ := m.Update(enter())
		m = tm.(Model)
		if m.input.Value() != "!echo nope" || shellEntries(m) != 0 {
			t.Fatalf("draft %q, %d rows", m.input.Value(), shellEntries(m))
		}
	})

	t.Run("a command is already running", func(t *testing.T) {
		m, _, _ := runningShell(t, sized(t))
		m.input.SetValue("!echo second")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if got := runCmd(cmd); got != nil {
			t.Fatalf("a refused Enter returned %T", got)
		}
		if m.input.Value() != "!echo second" {
			t.Fatalf("draft %q", m.input.Value())
		}
		if n := shellEntries(m); n != 1 {
			t.Fatalf("%d rows, want only the running one", n)
		}
	})
}

// TestShellBareBangRunsNothing: `!` alone is the mode and not a command.
func TestShellBareBangRunsNothing(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m.input.SetValue("!")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if got := runCmd(cmd); got != nil {
		t.Fatalf("a bare ! returned %T", got)
	}
	if m.input.Value() != "!" {
		t.Fatalf("draft %q, want the ! kept", m.input.Value())
	}
	if shellEntries(m) != 0 || len(stub.Prompts()) != 0 {
		t.Fatalf("%d rows, %d prompts", shellEntries(m), len(stub.Prompts()))
	}
}

// TestShellCtrlLIsRefused: Ctrl+L is the one send that bypasses handleEnter, so
// it has a refusal of its own. The draft stays where it is.
func TestShellCtrlLIsRefused(t *testing.T) {
	m, stub := queueWorking(t)
	m.input.SetValue("!echo hi")
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	if got := runCmd(cmd); got != nil {
		t.Fatalf("ctrl+l in shell mode returned %T", got)
	}
	if m.input.Value() != "!echo hi" {
		t.Fatalf("draft %q", m.input.Value())
	}
	if m.confirm != nil {
		t.Fatal("ctrl+l must not raise the send-now confirm in shell mode")
	}
	if shellEntries(m) != 0 {
		t.Fatal("ctrl+l ran a command")
	}
	if got := stub.Prompts(); len(got) != 1 || got[0] != "go" {
		t.Fatalf("the draft reached the agent: %q", got)
	}
	if len(queueTexts(m)) != 0 {
		t.Fatalf("the draft was queued: %q", queueTexts(m))
	}
}

// TestShellQueueEditorEnterSavesAndRunsNothing: the composer holds a queued
// message there, not a draft, so a `!` in it is text about a command.
func TestShellQueueEditorEnterSavesAndRunsNothing(t *testing.T) {
	m, _ := queueWorking(t)
	m = typeEnter(t, m, "the original row")
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.queueEdit == "" {
		t.Fatal("setup: the row is not being edited")
	}
	m.input.SetValue("!rm -rf nothing")
	if m.shellMode() {
		t.Fatal("the queue editor is never shell mode")
	}
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if got := runCmd(cmd); got != nil {
		t.Fatalf("saving an edit returned %T", got)
	}
	if got := queueTexts(m); len(got) != 1 || got[0] != "!rm -rf nothing" {
		t.Fatalf("queue %q, want the edited text saved", got)
	}
	if shellEntries(m) != 0 {
		t.Fatal("the queue editor ran a command")
	}
}

// TestShellRunsWhileTheAgentIsWorking: a command needs nothing of the session
// but its workspace, so a running turn is no reason to refuse one.
func TestShellRunsWhileTheAgentIsWorking(t *testing.T) {
	m, stub := queueWorking(t)
	m = runShellThrough(t, m, "!echo CRAZE_MIDTURN")
	if m.status != statusWorking {
		t.Fatalf("status %s: a command must not disturb the turn", m.status)
	}
	rows := shellRowsDrawn(m)
	if len(rows) != 1 || !strings.Contains(rows[0], "CRAZE_MIDTURN") {
		t.Fatalf("rows %q", rows)
	}
	if got := stub.Prompts(); len(got) != 1 || got[0] != "go" {
		t.Fatalf("the command reached the agent: %q", got)
	}
	if len(queueTexts(m)) != 0 {
		t.Fatalf("the command was queued: %q", queueTexts(m))
	}
}

// TestShellCtrlCKillsOnlyTheCommand (A16): one press, one kill. It does not
// quit, it does not cancel the agent's turn, and it arms nothing.
func TestShellCtrlCKillsOnlyTheCommand(t *testing.T) {
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	m := hangWorking(t)
	m.clock = func() time.Time { return base }
	stub := m.sess.(*Stub)
	m, marker, done := runningShell(t, m)

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(Model)
	if got := runCmd(cmd); got != nil {
		t.Fatalf("ctrl+c on a running command returned %T", got)
	}
	if !waitMarker(t, marker, false, 15*time.Second) {
		t.Fatal("ctrl+c did not kill the command's group")
	}
	if m.quitting {
		t.Fatal("ctrl+c on a running command must not quit")
	}
	if !m.ctrlCDeadline.IsZero() {
		t.Fatalf("ctrl+c armed the double-press window: %v", m.ctrlCDeadline)
	}
	if m.status != statusWorking {
		t.Fatalf("status %s, want the turn untouched", m.status)
	}
	if n := stub.CancelsSent(); n != 0 {
		t.Fatalf("%d cancels went to the agent", n)
	}

	// The row settles as killed, and the mode is free again.
	msg := <-done
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if m.shellRunning() {
		t.Fatal("the controller still reports a command")
	}
	if rows := shellRowsDrawn(m); len(rows) != 1 || !strings.Contains(rows[0], "killed") {
		t.Fatalf("rows %q, want one killed row", rows)
	}
}

// TestShellEscKillsFromTheFirstRung (A16): Esc reaches the command before the
// queue edit, the slash menu, a pending send-now and the turn itself.
func TestShellEscKillsFromTheFirstRung(t *testing.T) {
	m := hangWorking(t)
	stub := m.sess.(*Stub)
	m, marker, done := runningShell(t, m)

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if got := runCmd(cmd); got != nil {
		t.Fatalf("esc on a running command returned %T", got)
	}
	if !waitMarker(t, marker, false, 15*time.Second) {
		t.Fatal("esc did not kill the command's group")
	}
	if m.status != statusWorking {
		t.Fatalf("status %s, want the turn untouched", m.status)
	}
	if n := stub.CancelsSent(); n != 0 {
		t.Fatalf("esc cancelled the turn as well: %d cancels", n)
	}
	<-done
}

// TestShellEscUnderALayerKeepsItsEarlierMeaning: a card and a dialog take Esc
// before the ladder, and they keep it. Ctrl+C is how a command is killed there.
func TestShellEscUnderALayerKeepsItsEarlierMeaning(t *testing.T) {
	t.Run("a dialog", func(t *testing.T) {
		m, marker, done := runningShell(t, sized(t))
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
		m = tm.(Model)
		if m.dialog != dialogTheme {
			t.Fatal("setup: no theme picker")
		}
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = tm.(Model)
		if m.dialog != dialogNone {
			t.Fatal("esc must still close the dialog")
		}
		if !markerAlive(t, marker) {
			t.Fatal("esc on a dialog killed the command")
		}
		// Ctrl+C is the way out from under a layer.
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if !waitMarker(t, marker, false, 15*time.Second) {
			t.Fatal("ctrl+c did not kill the command")
		}
		if m.quitting {
			t.Fatal("ctrl+c quit instead of killing")
		}
		<-done
	})

	t.Run("a card", func(t *testing.T) {
		// The command has to start before the card: a card owns the keyboard,
		// so Enter never reaches handleEnter under one. That is exactly why
		// Ctrl+C is the way out here.
		m, marker, done := runningShell(t, sized(t))
		m.yolo = false
		m = cardEvent(t, m, m.sess.(*Stub), agent.Event{
			Type: agent.EventQuestion, Question: stubQuestion(),
		})
		if !m.cardOpen() {
			t.Fatal("setup: no card")
		}
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = tm.(Model)
		if m.cardOpen() {
			t.Fatal("esc must still skip the question")
		}
		if !markerAlive(t, marker) {
			t.Fatal("esc on a card killed the command")
		}
		tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if !waitMarker(t, marker, false, 15*time.Second) {
			t.Fatal("ctrl+c did not kill the command from under a card")
		}
		<-done
	})
}

// TestShellEscClearsTheDraftAndNeverCancelsTheTurn: with nothing running, Esc
// in shell mode is "leave the mode", which is "clear the draft" — and it stops
// there rather than falling through to the turn's cancel.
func TestShellEscClearsTheDraftAndNeverCancelsTheTurn(t *testing.T) {
	m := hangWorking(t)
	stub := m.sess.(*Stub)
	m.input.SetValue("!rm -rf nothing")
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if got := runCmd(cmd); got != nil {
		t.Fatalf("esc in shell mode returned %T", got)
	}
	if m.input.Value() != "" {
		t.Fatalf("draft %q, want it cleared", m.input.Value())
	}
	if m.status != statusWorking {
		t.Fatalf("status %s", m.status)
	}
	if n := stub.CancelsSent(); n != 0 {
		t.Fatalf("esc in shell mode cancelled the turn: %d cancels", n)
	}
	// The mode is gone with the draft, so the next Esc is the turn's again.
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	runCmd(cmd)
	if n := stub.CancelsSent(); n != 1 {
		t.Fatalf("the second esc should cancel the turn, %d cancels", n)
	}
}

// TestShellEveryQuitPathKillsTheCommand (A17): nothing the composer started
// outlives the program that started it, whichever way the program ends.
func TestShellEveryQuitPathKillsTheCommand(t *testing.T) {
	t.Run("ctrl+d", func(t *testing.T) {
		m, marker, done := runningShell(t, sized(t))
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
		m = tm.(Model)
		if !m.quitting {
			t.Fatal("ctrl+d must quit")
		}
		runCmd(cmd)
		if markerAlive(t, marker) {
			t.Fatal("the quit returned with the command still running")
		}
		<-done
	})

	t.Run("/exit", func(t *testing.T) {
		m, marker, done := runningShell(t, sized(t))
		m.input.SetValue("/exit")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if !m.quitting {
			t.Fatal("/exit must quit")
		}
		runCmd(cmd)
		if markerAlive(t, marker) {
			t.Fatal("the quit returned with the command still running")
		}
		<-done
	})

	t.Run("the exit tail, which SIGTERM, SIGHUP and a panic all reach", func(t *testing.T) {
		// bubbletea turns SIGTERM (and SIGHUP, and a recovered panic) into a
		// stop that runs no Update at all: finishRun is the only code that
		// sees it, and it holds the model Run *started* with, which is why the
		// controller is a shared pointer.
		//
		// What this covers is the tail itself — called here as Run calls it,
		// with a nil final and the model Run began with — and not bubbletea's
		// routing into it, which no test in this package can drive: Run writes
		// to os.Stdout, so there is no program to signal. The routing is Run's
		// own two lines around p.Run (see finishRun's comment), and the name
		// says as much so a green run is not read as more than it is.
		m, marker, done := runningShell(t, sized(t))
		finishRun(io.Discard, nil, m, nil)
		if markerAlive(t, marker) {
			t.Fatal("the exit tail left the command running")
		}
		<-done
	})

	t.Run("a session change", func(t *testing.T) {
		// The one path that does not wait: setSession runs inside Update, so
		// it starts the kill and hands the waiting to the run's own goroutine.
		// The command still dies — that is the whole assertion — it just dies
		// after the frame was drawn rather than before it.
		m, marker, done := runningShell(t, sized(t))
		m.setSession(NewStub())
		if !waitMarker(t, marker, false, 15*time.Second) {
			t.Fatal("the new session left the old one's command running")
		}
		<-done
	})

	t.Run("a double ctrl+c cannot reach a running command", func(t *testing.T) {
		// The second press of a double Ctrl+C is requestQuit, which the two
		// cases above cover. What this pins is the precedence in front of it: a
		// command running when Ctrl+C lands takes the press, armed window or
		// not, so the quit never happens with one still going.
		base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
		m := hangWorking(t)
		m.clock = func() time.Time { return base }
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if m.ctrlCDeadline.IsZero() {
			t.Fatal("setup: the first press should arm the window")
		}
		m, marker, done := runningShell(t, m)
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		m = tm.(Model)
		if m.quitting {
			t.Fatal("a press inside the window quit with a command running")
		}
		runCmd(cmd)
		if !waitMarker(t, marker, false, 15*time.Second) {
			t.Fatal("the press did not kill the command")
		}
		<-done
	})
}

// TestShellGlyphNamesHowItEnded pins each mark to the ending it stands for.
//
// The one worth a test of its own is the killed row: a signalled leader has no
// exit status and reports -1, so asking "did it exit non-zero?" before "how did
// it end?" would draw the user's own Esc as a failure and leave shellGlyph's
// cancelled arm unreachable.
func TestShellGlyphNamesHowItEnded(t *testing.T) {
	m := sized(t)
	completed, _ := m.statusGlyph("completed")
	failed, _ := m.statusGlyph("failed")
	cancelled, _ := m.statusGlyph("cancelled")
	for _, tc := range []struct {
		name  string
		entry shellEntry
		want  string
	}{
		{"still running", shellEntry{}, m.spinnerGlyph()},
		{"a clean exit", shellEntry{done: true}, completed},
		{"a non-zero exit", shellEntry{done: true, exit: 7}, failed},
		{"killed", shellEntry{done: true, exit: -1, why: shellKilled}, cancelled},
		{"timed out", shellEntry{done: true, exit: -1, why: shellTimedOut}, cancelled},
		{"would not stop", shellEntry{done: true, exit: -1, why: shellAbandoned}, failed},
		{"never started", shellEntry{done: true, start: io.EOF}, failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.entry
			if got, _ := m.shellGlyph(&e); got != tc.want {
				t.Fatalf("glyph %q, want %q", got, tc.want)
			}
		})
	}
	// The suffixes are the row's other half and they are unchanged: a killed
	// command says so in words, dimmed, beside the cancelled mark.
	killed := shellEntry{done: true, exit: -1, why: shellKilled}
	if got, _ := m.shellSuffix(&killed); got != "killed" {
		t.Fatalf("suffix %q, want \"killed\"", got)
	}
}

// TestShellRowShowsTheCommandThenItsOutput is the transcript half: the command
// on one row, its output under it, and an exit code only when there is one.
func TestShellRowShowsTheCommandThenItsOutput(t *testing.T) {
	m := runShellThrough(t, sized(t), "!echo one; echo two")
	rows := shellRowsDrawn(m)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, want := range []string{"✓ ! echo one; echo two", "  one", "  two"} {
		if !strings.Contains(rows[0], want) {
			t.Fatalf("row is missing %q:\n%s", want, rows[0])
		}
	}
	if strings.Contains(rows[0], "exit ") {
		t.Fatalf("a clean exit says nothing about its code:\n%s", rows[0])
	}

	m = runShellThrough(t, m, "!echo bad 1>&2; exit 7")
	rows = shellRowsDrawn(m)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, want := range []string{"✗ ! echo bad 1>&2; exit 7", "exit 7", "  bad"} {
		if !strings.Contains(rows[1], want) {
			t.Fatalf("row is missing %q:\n%s", want, rows[1])
		}
	}
}

// TestShellRowSpinsWhileItRunsAndCollapsesLongOutput covers the two things the
// row's own rendering owes: the spinner before it settles, and the same Ctrl+O
// every long tool row answers to.
func TestShellRowSpinsWhileItRunsAndCollapsesLongOutput(t *testing.T) {
	m := sized(t)
	m.addShell(1, "sleep 300")
	m.setViewportContent(true)
	if rows := shellRowsDrawn(m); len(rows) != 1 || !strings.HasPrefix(rows[0], spinnerGlyphs[0]+" ! sleep 300") {
		t.Fatalf("a running row draws the spinner, got %q", shellRowsDrawn(m))
	}

	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "output line")
	}
	m.finishShell(shellDoneMsg{gen: 1, res: shellResult{out: strings.Join(lines, "\n")}})
	m.setViewportContent(true)
	collapsed := shellRowsDrawn(m)[0]
	if got := strings.Count(collapsed, "output line"); got != shellPreviewLines {
		t.Fatalf("collapsed row shows %d lines, want %d:\n%s", got, shellPreviewLines, collapsed)
	}
	if !strings.Contains(collapsed, "… 10 more lines · ⌃o expand") {
		t.Fatalf("collapsed row does not offer the toggle:\n%s", collapsed)
	}

	m.expanded = true
	m.setViewportContent(true)
	expanded := shellRowsDrawn(m)[0]
	if got := strings.Count(expanded, "output line"); got != 30 {
		t.Fatalf("expanded row shows %d lines, want 30", got)
	}
	if strings.Contains(expanded, "⌃o expand") {
		t.Fatalf("an expanded row still offers the toggle:\n%s", expanded)
	}
}

// TestShellResultOutlivingItsRowIsStillShown: /clear takes the row a command is
// drawing into, and the output is still the answer to something the user asked
// for, so it is written again rather than dropped.
func TestShellResultOutlivingItsRowIsStillShown(t *testing.T) {
	m := sized(t)
	m.addShell(1, "echo hi")
	m.clearTranscript()
	if shellEntries(m) != 0 {
		t.Fatal("setup: /clear left the row behind")
	}
	m.finishShell(shellDoneMsg{gen: 1, res: shellResult{out: "hi"}})
	m.setViewportContent(true)
	rows := shellRowsDrawn(m)
	if len(rows) != 1 || !strings.Contains(rows[0], "hi") {
		t.Fatalf("rows %q", rows)
	}
}

// TestShellRunningKeepsTheSpinnerTicking: the row's spinner is drawn from
// inside the transcript, whose render cache would hold one frame forever
// without a tick to ask for the rebuild.
func TestShellRunningKeepsTheSpinnerTicking(t *testing.T) {
	m, _, _ := runningShell(t, sized(t))
	if !m.wantFastTick() {
		t.Fatal("a running command must keep the fast tick chain alive")
	}
	m.main.dirty = false
	m.handleTick(tickMsg{gen: m.tickGen})
	if !m.main.dirty {
		t.Fatal("a tick with a command running must ask for a repaint")
	}
}

// TestShellModeDoesNotColourTheSubAgentView: the view replaces the composer
// with a band of its own and borrows only its bottom rule, so a `!` draft the
// user left behind must not paint a line that says nothing true there.
func TestShellModeDoesNotColourTheSubAgentView(t *testing.T) {
	m := sized(t)
	m.input.SetValue("!ls")
	if !strings.Contains(m.composerRule(false), ansiFG(string(m.theme.Shell))) {
		t.Fatal("setup: the composer's own rule is not in the shell colour")
	}
	m.viewing = "task-a"
	if strings.Contains(m.composerRule(false), ansiFG(string(m.theme.Shell))) {
		t.Fatal("the sub-agent view's rule took the shell colour")
	}
	if !strings.Contains(m.composerRule(false), ansiFG(string(m.theme.Rule))) {
		t.Fatal("the sub-agent view's rule is not the ordinary one")
	}
}
