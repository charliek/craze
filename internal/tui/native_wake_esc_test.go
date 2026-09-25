package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// Esc on a turn the agent runs on its own (plan 026 X44, found by the live
// smoke's V10e): with nothing of craze's own working, Esc cancels a running
// wake — the engine accepts a cancel that names no turn while ForeignTurn()
// is true — and the TUI draws the cancelled note itself, since a wake
// publishes no EventDone. Esc while idle with a background child running
// cancels nothing: the child is not a turn. And a cancel that arrives after
// the agent's turn ended is refused as not accepting, which the TUI swallows.
// The first two run a real native session in the frame runner; the third
// runs the Stub, whose foreign flag a test flips by hand.

// TestFrameEscStopsARunningWake: the wake is held before it has said
// anything; Esc cancels it, the note is drawn once, and the next prompt runs
// after it — delivering the result the cancelled wake set aside, so its
// request ends with the result and is routed as a wake's.
func TestFrameEscStopsARunningWake(t *testing.T) {
	ws := frameWorkspace(t)
	echo := newFrameRouter("test", "wire-echo")
	const task = "child work"
	parentDone := make(chan struct{})
	echo.route("go", frameCalls(frameBackgroundCall("a1", "job", task)),
		thenClose(frameParts(nativeTextParts("started"), nativeFinishParts()), parentDone))
	echo.route(task, after(parentDone, frameParts(nativeTextParts("did work"), nativeFinishParts())))
	// The wake: held until its context ends, which Esc's cancel does.
	echo.routeWake(heldFrameStep(nil, nil, make(chan struct{}), make(chan struct{})))
	reply := frameParts(nativeTextParts("you are welcome"), nativeFinishParts())
	echo.route("thanks", reply)
	echo.routeWake(reply)
	sess := frameBackground(t, ws, echo, &frameClock{})

	got := runNativeSubagentFrame(t, sess, ws, 100, 30,
		"<wait:idle>go<enter><wait:text:started><wait:idle><wait:text:the agent continues><esc>"+
			"<wait:text:cancelled>thanks<enter><wait:text:you are welcome><wait:idle>")
	assertFrameGolden(t, "", 100, 30, got, []string{
		"sub-agent finished — the agent continues",
		"cancelled",
		"you are welcome",
		"✓ general-purpose  job  bg · echo",
	}, nil)
	if n := strings.Count(got, "cancelled"); n != 1 {
		t.Fatalf("the cancelled note is drawn %d times, want once:\n%s", n, got)
	}
}

// TestFrameEscWhileIdleLeavesABackgroundChildRunning: Esc with nothing of
// craze's own working and no wake running cancels nothing — the background
// child is not a turn, and it runs on.
func TestFrameEscWhileIdleLeavesABackgroundChildRunning(t *testing.T) {
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n")
	echo := newFrameRouter("test", "wire-echo")
	const task = "Find every Go file and say what it does."
	echo.route("go", frameCalls(frameBackgroundCall("a1", "Scan the repo", task)),
		frameParts(nativeTextParts("started the scan in the background"), nativeFinishParts()))
	echo.route(task, frameRead("r1", "main.go"))
	echo.hold(task, frameOpenText("main.go is the entry point"))
	sess := frameBackground(t, ws, echo, &frameClock{})

	got := runNativeSubagentFrame(t, sess, ws, 80, 24,
		"<wait:idle>go<enter><wait:text:started the scan in the background><wait:idle><wait:text:15 tok><esc><wait:text:15 tok>")
	assertFrameGolden(t, "", 80, 24, got,
		[]string{"○ general-purpose  Scan the repo  bg · 0s · 15 tok", "started the scan in the background"},
		[]string{"cancelled"})
}

// TestEscOnAForeignTurnCancelsItOnceAndSwallowsALateRefusal (the Stub): Esc
// on a foreign turn the model has seen reaches the session as a cancel
// naming no turn, and the note is drawn once; when the agent's turn ended at
// the session before the model heard of it, the engine refuses the cancel as
// not accepting and the TUI says nothing — no note, no error.
func TestEscOnAForeignTurnCancelsItOnceAndSwallowsALateRefusal(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(agent.NativeProvider())
	m := startStub(t, stub, t.TempDir(), 80, 24)
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "wake-1", Text: "sub-agent result", Reason: agent.ReasonSubagentWake, Running: true})
	m = pumpUntil(t, m, func(m Model) bool { return m.snap.ForeignTurn })

	m = pumpEsc(t, m)
	m = pumpSettled(t, m)
	if n := stub.CancelsSent(); n != 1 {
		t.Fatalf("Esc on the agent's turn sent %d cancels, want one", n)
	}
	if notes := texts(m, entryNote); len(notes) != 2 || notes[0] != transcript.NoteSubagentWake || notes[1] != stopCancelled {
		t.Fatalf("notes %q, want the wake's and one %q", notes, stopCancelled)
	}
	if m.err != "" {
		t.Fatalf("err %q", m.err)
	}

	// The agent's turn ends at the session; the event is still in the
	// channel, so the model's mirror still says the turn runs.
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "wake-1", Reason: agent.ReasonSubagentWake, Running: false})
	if !m.snap.ForeignTurn {
		t.Fatal("setup: the model has already heard the turn ended")
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	if msg := runCmd(cmd); msg != nil {
		t.Fatalf("a cancel the engine refused as not accepting produced %T %+v", msg, msg)
	}
	m = pumpSettled(t, m)
	if n := stub.CancelsSent(); n != 1 {
		t.Fatalf("the refused cancel reached the session: %d cancels", n)
	}
	if notes := texts(m, entryNote); len(notes) != 2 {
		t.Fatalf("notes %q after the refused cancel; want no new one", notes)
	}
	if m.err != "" || m.snap.ForeignTurn {
		t.Fatalf("err %q, foreign %v", m.err, m.snap.ForeignTurn)
	}
}
