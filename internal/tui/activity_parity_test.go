package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/host"
)

// A17: across the driver's scenarios, the ENGINE's State derives the same
// host.Status as the TUI's own mirror does.
//
// It matters because S4 makes a session host publish that status with no TUI in
// the process at all (session control SD-33): the engine's State has to carry
// everything host.Derive's Input reads (plan 021 §3.2), and mean the same thing
// by it. The real function is used on both sides — one reducer, two inputs — so
// what is being compared is the state and nothing else.
//
// The test lives here rather than in internal/engine because the TUI's own Input
// builder is here and the engine may not import this package (the depguard rule
// of §3.1). It drives the real Model over the Stub, through the pump, and
// compares at every observable step.
//
// **Where the comparison is made.** An engine event trails the state it
// describes (R2), so the model is behind the engine for as long as an event is
// in the outbox: the engine is working the instant Submit returns, and the model
// draws that turn in the same Update, but a turn's ENDING is an event, and until
// the model has applied it the two disagree by design. So every assertion is
// made at a point where everything that has happened has been applied
// (pumpDrained), which is exactly where a host would publish.

// inputFromState is host.Input built from the engine's State alone: what a
// session host with no TUI has to work from. It is the other half of
// Model.hostInput, field for field, and the mapping is the whole claim — Ready
// is the two activities that are not ready, Working and Errored are activities,
// the card is the head ask the registry reports, and the rest is the session's
// snapshot, which both sides read.
func inputFromState(st engine.State) host.Input {
	in := host.Input{
		// sessionReady() is "started and not replaying". The engine says the
		// same with its activity: starting is before the session is up, and
		// replaying is a load restoring itself. A start that FAILED is the one
		// state Derive publishes without being ready, and it says so itself.
		Ready:       st.Activity != engine.ActivityStarting && st.Activity != engine.ActivityReplaying && !st.StartFailed,
		StartFailed: st.StartFailed,
		Working:     st.Activity == engine.ActivityWorking,
		Errored:     st.Activity == engine.ActivityError,
		ForeignTurn: st.ForeignTurn,
		Cancelled:   st.Cancelled,
		NoTurnYet:   !st.Prompted,
		SessionID:   st.SessionID,
		Provider:    hostMeta(st.Provider.Name),
		Model:       hostMeta(st.CurrentModel),
	}
	if in.Errored {
		in.Err = hostMessage(host.FirstLine(st.Err))
	}
	if st.PendingAsks > 0 {
		// HeadAsk.Label is agent.AskLabel, which is what hostCard derives from
		// the card the TUI drew: one derivation, so a host driven by a session
		// with no TUI publishes the same reason.
		in.Card, in.CardLabel = hostCardOf(st.HeadAsk.Kind), hostMessage(st.HeadAsk.Label)
	}
	return in
}

// hostCardOf is hostCard's other half: the head ask's kind as a host.Card. The
// TUI reads it off the card it drew; a client with no cards reads the kind the
// engine reports.
func hostCardOf(k agent.AskKind) host.Card {
	switch k {
	case agent.AskPermission:
		return host.PermissionCard
	case agent.AskQuestion:
		return host.QuestionCard
	case agent.AskPlan:
		return host.PlanCard
	}
	return host.NoCard
}

// parityModel is a started model over the scripted session, with the provider
// named on BOTH sides: Config.Provider is where the TUI's own label comes from,
// and the session's snapshot is where a client with no TUI reads it. A live
// session fills its snapshot with the provider it was started with
// (live.go's Start), so naming it on the Stub is reproducing production and not
// papering over anything; the bare Stub names none.
//
// Cursor, because it is the provider that cannot interject: Ctrl+L there is the
// send-now this test needs, where on grok it would merge into the running turn.
func parityModel(t *testing.T) (Model, *scriptedSession) {
	t.Helper()
	isolateSkillsHome(t)
	sess := newScriptedSession()
	sess.SetProvider(agent.CursorProvider())
	t.Cleanup(func() { _ = sess.Close() })
	m := New(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
		Provider:  agent.CursorProvider(),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	return deliver(t, m, startedMsg{}), sess
}

// assertParity is the comparison itself, with no pumping: the two Inputs, one
// reducer, and the ok flag included — "publish nothing" has to agree too.
func assertParity(t *testing.T, m Model, what string) {
	t.Helper()
	want, wantOK := host.Derive(m.hostInput())
	got, gotOK := host.Derive(inputFromState(m.eng.State()))
	if wantOK != gotOK || want != got {
		t.Fatalf("%s:\n the TUI's mirror derives ok=%v %+v\n the engine's State  ok=%v %+v\n%s",
			what, wantOK, want, gotOK, got, plainView(m))
	}
}

// parityAt applies everything that has happened and then compares. pumpDrained
// and not pumpSettled, because most of these steps are taken with a turn
// deliberately held open.
func parityAt(t *testing.T, m Model, what string) Model {
	t.Helper()
	m = pumpDrained(t, m)
	assertParity(t, m, what)
	return m
}

// parityPermission is the card these tests raise, under an id of the test's
// own: two cards in one scenario must not share one, or the second would be
// refused as an id already open.
func parityPermission(id string) *agent.PermissionEvent {
	p := stubPermissionEvent(false)
	p.ID = id
	return p
}

// TestEngineStateDerivesTheSameHostStatusAsTheTUIsMirror is A17, scenario by
// scenario.
func TestEngineStateDerivesTheSameHostStatusAsTheTUIsMirror(t *testing.T) {
	t.Run("idle, working, idle, closing", func(t *testing.T) {
		m, sess := parityModel(t)
		assertParity(t, m, "the session just up")
		if s, _ := host.Derive(m.hostInput()); s.Detail != host.DetailReady {
			t.Fatalf("setup: the session comes up %s", s.Detail)
		}
		sc := scriptHeld()
		m = startScripted(t, m, sess, "go", sc)
		m = parityAt(t, m, "a turn running")
		sc.Release()
		m = pumpUntil(t, m, isIdle)
		m = pumpSettled(t, m)
		assertParity(t, m, "the turn over")
		if s, _ := host.Derive(m.hostInput()); s.Detail != host.DetailStop {
			t.Fatalf("setup: a finished turn is %s", s.Detail)
		}
		// Closing: the engine's last activity. A host hears nothing new from it —
		// the TUI releases its host before it closes the session — and the two
		// still agree on what the session's state is.
		if err := m.eng.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if act := m.eng.State().Activity; act != engine.ActivityClosing {
			t.Fatalf("setup: the closed engine is %s", act)
		}
		assertParity(t, m, "the engine closing")
	})

	t.Run("a queued row drains into a successor", func(t *testing.T) {
		m, sess := parityModel(t)
		first, drained := scriptHeld(), scriptHeld()
		m = startScripted(t, m, sess, "go", first)
		sess.Script(drained)
		m = pumpEnter(t, m, "PINEAPPLE")
		if got := queueTexts(m); len(got) != 1 {
			t.Fatalf("setup: the follow-up should be queued: %q", got)
		}
		m = parityAt(t, m, "a turn running with a row queued")
		first.Release()
		awaitBarrier(t, drained.opened, "the drained turn opening")
		m = pumpUntil(t, m, turnsDrawn(2))
		// The settlement that ended the first turn started the second in the same
		// batch, so neither side ever passes through idle.
		m = parityAt(t, m, "the successor running")
		if s, _ := host.Derive(m.hostInput()); s.Kind != host.Working {
			t.Fatalf("setup: the successor is %s", s.Kind)
		}
		drained.Release()
		m = pumpUntil(t, m, isIdle)
		m = pumpSettled(t, m)
		assertParity(t, m, "the chain over")
	})

	t.Run("a cancelled turn with a row queued", func(t *testing.T) {
		m, sess := parityModel(t)
		first, drained := scriptHeld(), scriptHeld()
		m = startScripted(t, m, sess, "go", first)
		sess.Script(drained)
		m = pumpEnter(t, m, "PINEAPPLE")
		m = pumpEsc(t, m)
		awaitBarrier(t, drained.opened, "the drained turn opening")
		m = pumpUntil(t, m, turnsDrawn(2))
		m = parityAt(t, m, "the row that drained behind a cancel")
		drained.Release()
		m = pumpUntil(t, m, isIdle)
		m = pumpSettled(t, m)
		assertParity(t, m, "the chain over")
	})

	t.Run("a cancelled turn with nothing behind it", func(t *testing.T) {
		m, sess := parityModel(t)
		sc := scriptHeld()
		m = startScripted(t, m, sess, "go", sc)
		m = pumpEsc(t, m)
		m = pumpUntil(t, m, isIdle)
		m = pumpSettled(t, m)
		assertParity(t, m, "idle after Esc")
		if s, _ := host.Derive(m.hostInput()); s.Detail != host.DetailCancelled {
			t.Fatalf("setup: the cancelled idle is %s", s.Detail)
		}
	})

	t.Run("a send-now armed and fired", func(t *testing.T) {
		m, sess := parityModel(t)
		first, sent := scriptHeld(), scriptHeld()
		m = startScripted(t, m, sess, "go", first)
		sess.Script(sent)
		m.input.SetValue("PINEAPPLE")
		m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
		// The cancel the arm asks for is held, so "armed" is a state the test can
		// stand in rather than a moment.
		release := sess.HoldNextCancel()
		m = pumpKey(t, m, enter())
		awaitBarrier(t, sess.Cancels(), "the arm's cancel reaching the session")
		if !sendNowArmed(m) {
			t.Fatalf("setup: the send-now was not armed:\n%s", plainView(m))
		}
		m = parityAt(t, m, "a send-now armed against the running turn")
		release()
		awaitBarrier(t, sent.opened, "the armed send's turn opening")
		m = pumpUntil(t, m, turnsDrawn(2))
		m = parityAt(t, m, "the fired send running")
		sent.Release()
		m = pumpUntil(t, m, isIdle)
		m = pumpSettled(t, m)
		assertParity(t, m, "the fired send over")
	})

	t.Run("an ask pending while working and between turns", func(t *testing.T) {
		m, sess := parityModel(t)
		sc := scriptHeld()
		m = startScripted(t, m, sess, "go", sc)
		sess.Emit(agent.Event{Type: agent.EventPermission, Permission: parityPermission("perm-1")})
		m = pumpUntil(t, m, hasCard)
		m = parityAt(t, m, "a card while the turn works")
		if s, _ := host.Derive(m.hostInput()); s.Kind != host.Blocked || s.Message != "permission Shell" {
			t.Fatalf("setup: the card publishes %+v", s)
		}
		m, _ = press(m, runeKey('a'))
		if m.cardOpen() {
			t.Fatal("setup: the answer did not close the card")
		}
		m = parityAt(t, m, "the card answered, the turn still working")
		sc.Release()
		m = pumpUntil(t, m, isIdle)
		m = pumpSettled(t, m)
		assertParity(t, m, "the turn over")

		// Between turns: a request that belongs to no turn of craze's own. It
		// blocks the session just the same, and overlays an idle activity.
		sess.Emit(agent.Event{Type: agent.EventPermission, Permission: parityPermission("perm-2")})
		m = pumpUntil(t, m, hasCard)
		m = parityAt(t, m, "a card between turns")
		if s, _ := host.Derive(m.hostInput()); s.Kind != host.Blocked {
			t.Fatalf("setup: the between-turns card publishes %+v", s)
		}
		m, _ = press(m, runeKey('a'))
		_ = parityAt(t, m, "the between-turns card answered")
	})

	t.Run("a foreign turn", func(t *testing.T) {
		m, sess := parityModel(t)
		sess.SetForeignTurn(agent.ForeignTurnInfo{ID: "p-1", Running: true})
		m = pumpUntil(t, m, func(m Model) bool { return m.snap.ForeignTurn })
		m = parityAt(t, m, "the agent running a turn of its own")
		if s, _ := host.Derive(m.hostInput()); s.Detail != host.DetailForeignTurn {
			t.Fatalf("setup: a foreign turn publishes %+v", s)
		}
		sess.SetForeignTurn(agent.ForeignTurnInfo{ID: "p-1", Running: false})
		m = pumpUntil(t, m, func(m Model) bool { return !m.snap.ForeignTurn })
		_ = parityAt(t, m, "the foreign turn over")
	})

	t.Run("an errored turn", func(t *testing.T) {
		m, sess := parityModel(t)
		sess.Script(scriptFailed(errors.New("agent exited: status 1\npanic: boom")))
		m = pumpEnter(t, m, "go")
		m = pumpUntil(t, m, allOf(isErrored, errorRows(1)))
		m = pumpSettled(t, m)
		assertParity(t, m, "the failed turn")
		if s, _ := host.Derive(m.hostInput()); s.Kind != host.Failed || s.Message != "agent exited: status 1" {
			t.Fatalf("setup: the failure publishes %+v", s)
		}
		// And the next turn retires it on both sides.
		next := scriptHeld()
		m = startScripted(t, m, sess, "again", next)
		m = parityAt(t, m, "the turn after the failure")
		next.Release()
		m = pumpUntil(t, m, isIdle)
		m = pumpSettled(t, m)
		assertParity(t, m, "the turn after the failure, over")
	})

	t.Run("a start that failed", func(t *testing.T) {
		isolateSkillsHome(t)
		stub := NewStub()
		stub.SetProvider(agent.CursorProvider())
		t.Cleanup(func() { _ = stub.Close() })
		m := New(Config{
			Session:   stub,
			Theme:     "tokyo-night",
			Workspace: t.TempDir(),
			Model:     "grok",
			Yolo:      true,
			Provider:  agent.CursorProvider(),
		})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = tm.(Model)
		// Nothing is published before the session is up or down, and the engine
		// agrees: both say "publish nothing".
		assertParity(t, m, "before the session is up")
		if _, ok := host.Derive(m.hostInput()); ok {
			t.Fatal("setup: a status was publishable before the session came up")
		}
		m = deliver(t, m, errMsg{err: errors.New("authentication failed: no key\nsee cursor-agent login")})
		assertParity(t, m, "the session that never came up")
		s, ok := host.Derive(m.hostInput())
		if !ok || s.Kind != host.Failed || s.Detail != host.DetailStartFailed {
			t.Fatalf("setup: a failed start publishes %v %+v", ok, s)
		}
		// The engine refuses everything from here, which is the state this
		// parity is about: StartFailed with the error beside it.
		if _, err := m.eng.Submit(engine.Command{}, "anything", engine.SubmitQueue, ""); !errors.Is(err, engine.ErrNotAccepting) {
			t.Fatalf("a submit after a failed start: %v", err)
		}
	})
}

// TestInputFromStateReadsEveryFieldDeriveDoes is the guard on the helper above:
// if host.Input grows a field, this fails until inputFromState fills it from
// State — which is exactly the moment to find out whether State can (plan 021
// §3.2 says it must).
func TestInputFromStateReadsEveryFieldDeriveDoes(t *testing.T) {
	m, sess := parityModel(t)
	sc := scriptHeld()
	m = startScripted(t, m, sess, "go", sc)
	sess.Emit(agent.Event{Type: agent.EventPermission, Permission: parityPermission("perm-1")})
	m = pumpUntil(t, m, hasCard)
	m = pumpDrained(t, m)
	// Every field the TUI fills for a blocked, working session, filled from the
	// engine's State as well.
	mine, theirs := m.hostInput(), inputFromState(m.eng.State())
	if mine != theirs {
		t.Fatalf("the two Inputs differ:\n TUI    %+v\n engine %+v", mine, theirs)
	}
	if mine.Card == host.NoCard || !mine.Working || !mine.Ready || mine.NoTurnYet {
		t.Fatalf("setup: the state under test is not a blocked, working session: %+v", mine)
	}
	sc.Release()
	m = pumpUntil(t, m, isIdle)
	_ = pumpSettled(t, m)
}
