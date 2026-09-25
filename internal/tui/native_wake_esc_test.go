package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/host"
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

// The review round on that Esc (plan 026 X48, astra r20's four MINORs). The
// Stub drives these, its foreign flag flipped by hand, so each schedule — the
// cancel's answer against the follow-up's started, a cancel landing on a
// drained craze turn, Esc pressed again on one turn, an opening in flight at
// the Esc — is forced rather than hoped for: the Esc's command is run, and its
// answer applied, exactly where the schedule puts them.

// wakeRunning is a Stub model on the native provider with a wake running
// whose opening bracket the model has applied. pumpSettled rather than a
// predicate over the flag: a refresh on an earlier event can read the flag
// before the bracket that raises it has been applied.
func wakeRunning(t *testing.T, id string) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(agent.NativeProvider())
	m := startStub(t, stub, t.TempDir(), 80, 24)
	startWake(stub, id)
	m = pumpSettled(t, m)
	if !m.snap.ForeignTurn {
		t.Fatal("setup: the model has not seen the wake")
	}
	return m, stub
}

// startWake and endWake are the wake's brackets, as the native session
// publishes them.
func startWake(stub *Stub, id string) {
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: id, Text: "sub-agent result", Reason: agent.ReasonSubagentWake, Running: true})
}

func endWake(stub *Stub, id string) {
	stub.SetForeignTurn(agent.ForeignTurnInfo{ID: id, Reason: agent.ReasonSubagentWake, Running: false})
}

// foreignCancelAnswer runs the command Esc made on a foreign turn and returns
// the engine's acceptance unapplied: the test decides when the model hears it.
func foreignCancelAnswer(t *testing.T, cmd tea.Cmd) foreignCancelledMsg {
	t.Helper()
	msg := runCmd(cmd)
	got, ok := msg.(foreignCancelledMsg)
	if !ok {
		t.Fatalf("Esc's cancel answered %T %+v, want the engine's acceptance", msg, msg)
	}
	return got
}

// cancelledNotes is how many cancelled notes the transcript holds: the count
// is the claim, since a second one is the bug.
func cancelledNotes(m Model) int {
	n := 0
	for _, s := range texts(m, entryNote) {
		if s == stopCancelled {
			n++
		}
	}
	return n
}

// TestAForeignCancelsNoteSurvivesAFollowUpStartedFirst (astra r20 finding 1,
// the missing note): Esc cancels the wake, the wake ends, and the follow-up
// queued behind it drains — and the model applies the follow-up's started
// before the cancel's answer. What was cancelled was the agent's turn, so the
// note is drawn although a craze turn is working when the answer lands; and
// the cancelled state a host reads is the follow-up's, which ended on its own
// (finding 3's other half: the answer does not settle a turn begun after it).
func TestAForeignCancelsNoteSurvivesAFollowUpStartedFirst(t *testing.T) {
	m, stub := wakeRunning(t, "wake-1")
	m = pumpEnter(t, m, "follow")
	if got := queueTexts(m); len(got) != 1 {
		t.Fatalf("setup: Enter under the wake queues, got %q", got)
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	answer := foreignCancelAnswer(t, cmd)

	endWake(stub, "wake-1")
	m = pumpUntil(t, m, func(m Model) bool { return m.status == statusWorking })
	tm, _ = m.Update(answer)
	m = pumpSettled(t, tm.(Model))

	assertPrompts(t, stub, "follow")
	if n := cancelledNotes(m); n != 1 {
		t.Fatalf("%d cancelled notes, want the wake's one: %q", n, texts(m, entryNote))
	}
	if s, _ := host.Derive(m.hostInput()); s.Kind != host.Idle || s.Detail != host.DetailStop {
		t.Fatalf("the host reads %+v after the follow-up ended on its own, want idle %q", s, host.DetailStop)
	}
}

// TestAForeignCancelThatLandsOnACrazeTurnDrawsOneNote (astra r20 finding 1,
// the duplicate; SF-48's race): between the Esc and its command the wake ends
// and the owed drain opens the follow-up's turn, so the cancel that names no
// turn lands on that craze turn. Its own ending draws the note, and the
// engine's answer names the craze turn, so the model draws nothing more —
// whether the answer is handled after that ending has put the model back to
// idle, or before its started has put it to work.
func TestAForeignCancelThatLandsOnACrazeTurnDrawsOneNote(t *testing.T) {
	for _, tc := range []struct {
		name        string
		answerFirst bool
	}{
		{"the ending first", false},
		{"the answer first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := wakeRunning(t, "wake-1")
			m = pumpEnter(t, m, "follow")
			if got := queueTexts(m); len(got) != 1 {
				t.Fatalf("setup: Enter under the wake queues, got %q", got)
			}
			hung := stub.HangNext()
			tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
			m = tm.(Model)
			endWake(stub, "wake-1")
			awaitBarrier(t, hung, "the follow-up's turn opening")
			answer := foreignCancelAnswer(t, cmd)
			if answer.turn == "" {
				t.Fatal("setup: the cancel did not land on the follow-up's turn")
			}

			if tc.answerFirst {
				tm, _ = m.Update(answer)
				m = pumpSettled(t, tm.(Model))
			} else {
				m = pumpSettled(t, m)
				if !isIdle(m) {
					t.Fatalf("setup: the cancelled follow-up left the model %s", m.status)
				}
				tm, _ = m.Update(answer)
				m = tm.(Model)
			}
			assertPrompts(t, stub, "follow")
			if n := cancelledNotes(m); n != 1 {
				t.Fatalf("%d cancelled notes, want the follow-up's one: %q", n, texts(m, entryNote))
			}
			if s, _ := host.Derive(m.hostInput()); s.Kind != host.Idle || s.Detail != host.DetailCancelled {
				t.Fatalf("the host reads %+v, want the cancelled follow-up's idle", s)
			}
		})
	}
}

// TestRepeatedEscOnOneForeignTurnDrawsOneNote (astra r20 finding 2): Esc
// pressed again on the same wake — once while the first cancel is in flight,
// once after both answered with the wake still running — is another cancel
// the engine accepts, and no second note. The next wake is another episode,
// and its cancel draws its own.
func TestRepeatedEscOnOneForeignTurnDrawsOneNote(t *testing.T) {
	m, stub := wakeRunning(t, "wake-1")
	esc := tea.KeyMsg{Type: tea.KeyEsc}
	tm, first := m.Update(esc)
	tm, second := tm.(Model).Update(esc)
	m = tm.(Model)
	for _, cmd := range []tea.Cmd{first, second} {
		tm, _ = m.Update(foreignCancelAnswer(t, cmd))
		m = tm.(Model)
	}
	if !m.snap.ForeignTurn {
		t.Fatal("setup: the wake should still be running")
	}
	tm, third := m.Update(esc)
	m = tm.(Model)
	tm, _ = m.Update(foreignCancelAnswer(t, third))
	m = pumpSettled(t, tm.(Model))
	if n := stub.CancelsSent(); n != 3 {
		t.Fatalf("%d cancels reached the session, want one per Esc", n)
	}
	if n := cancelledNotes(m); n != 1 {
		t.Fatalf("%d cancelled notes for one wake, want one: %q", n, texts(m, entryNote))
	}

	endWake(stub, "wake-1")
	m = pumpSettled(t, m)
	startWake(stub, "wake-2")
	m = pumpSettled(t, m)
	m = pumpEsc(t, m)
	m = pumpSettled(t, m)
	if n := cancelledNotes(m); n != 2 {
		t.Fatalf("%d cancelled notes after the next wake's cancel, want one each: %q", n, texts(m, entryNote))
	}
}

// TestAForeignCancelIsACancellationToTheHost (astra r20 finding 3): a parent
// turn ends on its own, its wake runs, and Esc stops the wake. The host reads
// the cancellation Esc made — not the completed turn the parent's ending left,
// which roost would announce as "Turn complete".
func TestAForeignCancelIsACancellationToTheHost(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(agent.NativeProvider())
	m := startStub(t, stub, t.TempDir(), 80, 24)
	m = pumpEnter(t, m, "go")
	m = pumpSettled(t, m)
	if s, _ := host.Derive(m.hostInput()); s.Kind != host.Idle || s.Detail != host.DetailStop {
		t.Fatalf("setup: the parent's ending reads %+v", s)
	}
	startWake(stub, "wake-1")
	m = pumpSettled(t, m)
	if s, _ := host.Derive(m.hostInput()); s.Kind != host.Working || s.Detail != host.DetailForeignTurn {
		t.Fatalf("setup: the wake reads %+v", s)
	}

	m = pumpEsc(t, m)
	m = pumpSettled(t, m)
	endWake(stub, "wake-1")
	m = pumpSettled(t, m)
	if s, _ := host.Derive(m.hostInput()); s.Kind != host.Idle || s.Detail != host.DetailCancelled {
		t.Fatalf("the host reads %+v after Esc stopped the wake, want idle %q", s, host.DetailCancelled)
	}
}

// TestEscOnAForeignTurnMasksAnOpeningItsCancelAnswered (astra r20 finding 4):
// the wake opens an ask whose opening has not reached the model when Esc
// stops the wake. The cancel answers the ask where it lies, and the cards are
// masked before it as cancelTurn masks them, so the opening arriving after it
// raises no card — which would take the keyboard, and a host's blocked status,
// until the ask's ending took it down again.
func TestEscOnAForeignTurnMasksAnOpeningItsCancelAnswered(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(agent.NativeProvider())
	m := startStub(t, stub, t.TempDir(), 80, 24)
	// Fed by hand, as the card tests are: no pump reads the primary, so the
	// opening below reaches the model only where this test applies it.
	startWake(stub, "wake-1")
	tm, _ := m.Update(eventMsg{awaitStubEvent(t, stub, agent.EventForeignTurn)})
	m = tm.(Model)
	if !m.snap.ForeignTurn {
		t.Fatal("setup: the model has not seen the wake")
	}
	opening := agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}
	stub.Emit(opening)

	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	answer := foreignCancelAnswer(t, cmd)
	if open := stub.Asks().Asks(); len(open) != 0 {
		t.Fatalf("setup: the cancel left %+v open", open)
	}
	tm, _ = m.Update(eventMsg{opening})
	m = tm.(Model)
	if m.cardOpen() {
		t.Fatalf("an opening the cancel had already answered raised a card: %+v", m.cards)
	}
	if s, _ := host.Derive(m.hostInput()); s.Kind == host.Blocked {
		t.Fatalf("the host reads %+v for an ask nobody can answer", s)
	}
	tm, _ = m.Update(answer)
	m = tm.(Model)
	if n := cancelledNotes(m); n != 1 {
		t.Fatalf("%d cancelled notes, want the wake's one: %q", n, texts(m, entryNote))
	}
}
