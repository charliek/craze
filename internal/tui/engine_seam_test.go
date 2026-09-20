package tui

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/host"
	"github.com/charliek/craze/internal/sessions"
)

// The seam between the model and the engine it drives its session through (plan
// 021 §3.1, §3.4): who owns the engine and closes it, and the echo rule — which
// EventTurn{started} the model skips and which it draws.

// closeCounter is a session that counts the Closes it was given, so a test can say
// that closing the engine closed the session, and exactly once from each caller.
type closeCounter struct {
	*Stub
	closes atomic.Int32
}

func newCloseCounter() *closeCounter { return &closeCounter{Stub: NewStub()} }

func (s *closeCounter) Close() error {
	s.closes.Add(1)
	return s.Stub.Close()
}

// ------------------------------------------------------------------ ownership

// TestClosingTheOwnerClosesTheEngineAndTheSession is A18's first clause. The owner
// every copy of the model shares holds the ENGINE, so the exit tail closes that —
// which stops the driver and then closes the session. Closing only the session
// would leave the engine's goroutine running and whatever its outbox still held
// unpublished.
func TestClosingTheOwnerClosesTheEngineAndTheSession(t *testing.T) {
	isolateSkillsHome(t)
	sess := newCloseCounter()
	m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
	if m.owner.current() == nil {
		t.Fatal("the owner holds no engine")
	}
	if m.owner.current() != m.eng {
		t.Fatal("the owner and the model name different engines")
	}
	if m.eng.Session() != agent.Session(sess) {
		t.Fatalf("the engine wraps %T, want the session it was given", m.eng.Session())
	}

	eng := m.owner.current()
	if err := eng.Close(); err != nil {
		t.Fatalf("closing the owner's engine: %v", err)
	}
	if got := sess.closes.Load(); got != 1 {
		t.Fatalf("the session was closed %d times, want once", got)
	}
	// Close joined the engine's goroutines before it returned, and the engine
	// admits nothing afterwards: there is no driver left to admit it to.
	if _, err := eng.Submit(engine.Command{}, "after the close", engine.SubmitQueue, ""); err == nil {
		t.Fatal("a closed engine accepted a prompt")
	}
	// Idempotent, as the exit tail relies on: requestQuit closes it and finishRun
	// closes the owner's again.
	if err := eng.Close(); err != nil {
		t.Fatalf("the second close: %v", err)
	}
	if got := sess.closes.Load(); got != 1 {
		t.Fatalf("the second close reached the session: %d closes", got)
	}
}

// TestSwappingTheSessionClosesTheOldEngine is A18's second clause for both dialogs:
// the provider picker and the resume picker each close the engine they are
// replacing, not just its session, so nothing of the old one is left running. The
// proof is the old engine itself: its Close returned — which joins its driver — and
// its session is closed, so no goroutine of its own can still be reading state that
// no longer exists.
func TestSwappingTheSessionClosesTheOldEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(t *testing.T, m Model, built func(agent.Provider) agent.Session) Model
	}{
		{
			name: "provider picker",
			swap: func(t *testing.T, m Model, built func(agent.Provider) agent.Session) Model {
				t.Helper()
				tm, _ := m.confirmProvider(agent.GrokProvider(), true)
				return tm.(Model)
			},
		},
		{
			name: "resume picker",
			swap: func(t *testing.T, m Model, built func(agent.Provider) agent.Session) Model {
				t.Helper()
				tm, _ := m.confirmResume(resumeRow("s-1", "grok", "yesterday", time.Hour))
				return tm.(Model)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateSkillsHome(t)
			first := newCloseCounter()
			second := newCloseCounter()
			build := func(agent.Provider) agent.Session { return second }
			m := New(Config{
				Session:     first,
				Theme:       "tokyo-night",
				Workspace:   t.TempDir(),
				Yolo:        true,
				Provider:    agent.CursorProvider(),
				NewSession:  build,
				LoadSession: func(agent.Provider, sessions.Row) agent.Session { return second },
			})
			old := m.eng
			if old == nil {
				t.Fatal("setup: no engine to replace")
			}

			m = tc.swap(t, m, build)
			t.Cleanup(func() { _ = m.eng.Close() })

			if m.eng == old {
				t.Fatal("the swap kept the old engine")
			}
			if m.owner.current() != m.eng {
				t.Fatal("the owner was not moved to the new engine")
			}
			if got := first.closes.Load(); got != 1 {
				t.Fatalf("the old session was closed %d times, want once", got)
			}
			// The old engine is closed, which is what says its driver was joined.
			if _, err := old.Submit(engine.Command{}, "after the swap", engine.SubmitQueue, ""); err == nil {
				t.Fatal("the old engine still admits prompts")
			}
			if got := second.closes.Load(); got != 0 {
				t.Fatalf("the new session was closed %d times before anything asked", got)
			}
		})
	}
}

// TestASecondEngineOnOneSessionIsRefused is A18's last clause, and the reason no
// path may wrap twice: the engine installs the log's single observer, so a second
// one on the same session would be a second driver — two of them deciding what runs
// next. The model handles the refusal as a start failure rather than panicking.
func TestASecondEngineOnOneSessionIsRefused(t *testing.T) {
	isolateSkillsHome(t)
	sess := NewStub()
	t.Cleanup(func() { _ = sess.Close() })
	first, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := engine.New(sess, engine.Options{}); err == nil {
		t.Fatal("a second engine on one session was accepted")
	}

	// And through the model: the failure rides out as the start failure it is.
	m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
	if m.eng != nil || m.engErr == nil {
		t.Fatalf("the model wrapped an already-wrapped session: eng=%v err=%v", m.eng != nil, m.engErr)
	}
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	msg := runCmd(m.startCmd())
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("startCmd answered %T, want the start failure", msg)
	}
	m = deliver(t, m, em)
	if m.status != statusError || m.startErr == nil {
		t.Fatalf("status %s startErr %v, want the error state", m.status, m.startErr)
	}
}

// TestAStaleStartMessageLeavesTheNewEnginesGateShut is the hole naming the engine
// on the start messages closes. A start command outlives the model copy that made
// it: a picker's choice closes one engine and builds another in the same Update,
// and the old command then reports into a model that holds the new one. Neither of
// its two answers may speak for that engine — a startedMsg would open a gate whose
// own Start has not returned, and an errMsg would fail a session that is starting
// perfectly well.
//
// It is not reachable from internal/cli today, and the test says why by building
// the shape that is: a Config with both a Session and a NewSession leaves the
// picker up over an engine that has been started, where internal/cli only ever
// fills Session when the provider is locked and no picker shows (cli/tui.go's
// `cfg.Session == nil && cfg.Resume == nil && resolved.Locked`) and Init returns no
// command at all while picking. One field away from reachable is close enough to
// guard, and to pin.
func TestAStaleStartMessageLeavesTheNewEnginesGateShut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale func(eng *engine.Engine) tea.Msg
	}{
		{"a stale startedMsg", func(eng *engine.Engine) tea.Msg { return startedMsg{eng: eng} }},
		{"a stale errMsg", func(eng *engine.Engine) tea.Msg {
			return errMsg{err: errors.New("the old session never came up"), eng: eng}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateSkillsHome(t)
			second := NewStub()
			m := New(Config{
				Session:    NewStub(),
				Theme:      "tokyo-night",
				Workspace:  t.TempDir(),
				Yolo:       true,
				Provider:   agent.CursorProvider(),
				NewSession: func(agent.Provider) agent.Session { return second },
			})
			old := m.eng
			if old == nil {
				t.Fatal("setup: no engine to leave behind")
			}
			tm, _ := m.confirmProvider(agent.GrokProvider(), true)
			m = tm.(Model)
			t.Cleanup(func() { _ = m.eng.Close() })
			if m.eng == old {
				t.Fatal("setup: the swap kept the old engine")
			}

			m = deliver(t, m, tc.stale(old))
			if m.started {
				t.Fatal("a message about the engine that went made the model started")
			}
			if m.status == statusError || m.startErr != nil {
				t.Fatalf("a stale message failed the new session: status %s startErr %v", m.status, m.startErr)
			}
			if got := m.eng.State().Activity; got != engine.ActivityStarting {
				t.Fatalf("the new engine's activity is %q, want it still starting", got)
			}
			if _, err := m.eng.Submit(engine.Command{}, "too early", engine.SubmitQueue, ""); !errors.Is(err, engine.ErrNotAccepting) {
				t.Fatalf("the new engine's gate opened: Submit = %v", err)
			}

			// The control: the message for the engine the model does hold opens it.
			m = deliver(t, m, startedMsg{eng: m.eng})
			if !m.started {
				t.Fatal("the current engine's own startedMsg was ignored too")
			}
			if got := m.eng.State().Activity; got != engine.ActivityIdle {
				t.Fatalf("the new engine's activity is %q, want idle", got)
			}
		})
	}
}

// ----------------------------------------------------- endings and their turn

// endedEvent is one EventTurn{ended}, built by hand. Hand-built is right for the
// cases below: their subject is the model's reducer — which turn an ending settles
// — and the engine cannot be made to publish an ending for a turn that is not its
// current one, because it only ever ends the turn it is running.
func endedEvent(turn string, fill func(*agent.TurnInfo)) tea.Msg {
	t := &agent.TurnInfo{ID: turn, Phase: agent.TurnEnded}
	fill(t)
	return eventMsg{agent.Event{Type: agent.EventTurn, Turn: t}}
}

// twoTurnsDeep is a model on its second turn, with the first turn's id: the state
// every stale-ending case below starts from. The second turn is held, so "still
// working on turn two" is a state and not a moment.
func twoTurnsDeep(t *testing.T) (Model, *scriptedSession, *recHost, string) {
	t.Helper()
	m, sess, rec := startedScriptedHostModel(t)
	first, second := scriptHeld(), scriptHeld()
	m = startScripted(t, m, sess, "one", first)
	stale := m.turnID
	if stale == "" {
		t.Fatal("setup: the model is not displaying a turn")
	}
	first.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)

	sess.Script(second)
	m = pumpEnter(t, m, "two")
	if m.status != statusWorking {
		t.Fatalf("setup: the second turn did not start: %s", m.status)
	}
	if m.turnID == stale {
		t.Fatalf("setup: both turns have the id %q", stale)
	}
	awaitBarrier(t, second.opened, "the second turn opening")
	// From here on, what a host hears is this test's subject.
	rec.statuses = nil
	return m, sess, rec, stale
}

// TestAStaleEndingSettlesNothing is the race an engine event's trailing makes
// reachable: a turn's ending arrives after the model has started another one. It
// is reachable for real — a turn that fails puts the model in its error state from
// the session's own EventError, and a direct Submit is admitted from an error
// state, so Enter in that window starts the next turn with the old ending still in
// the outbox (TestALateEndingDoesNotErrorTheTurnThatFollowedIt drives exactly
// that). Nothing it carries may settle the turn on screen: no error and no idle
// under work in flight, m.err untouched, the confirm still up, and not one status
// published to the host.
//
// What it does still write is the rows it owes its own turn, because the
// transcript is a record and a late fact is still a fact — which is also what the
// baseline did, drawing both unconditionally from a promptDoneMsg that carried no
// turn at all.
func TestAStaleEndingSettlesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill func(*agent.TurnInfo)
		// notes and rows are what the ending owes its own turn, however late.
		notes, rows int
	}{
		{"a turn that failed", func(t *agent.TurnInfo) {
			t.Err, t.ErrClass = "boom", agent.EventErrOther
		}, 0, 0},
		{"a clean ending with no successor", func(t *agent.TurnInfo) {
			t.StopReason = "end_turn"
		}, 0, 0},
		{"a cancelled ending", func(t *agent.TurnInfo) {
			t.Synthetic, t.StopReason = true, stopCancelled
		}, 1, 0},
		{"a refused prompt", func(t *agent.TurnInfo) {
			t.Synthetic, t.Err, t.ErrClass = true, "agent: a prompt is already in flight", agent.EventErrPromptInFlight
		}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, rec, stale := twoTurnsDeep(t)
			// A confirm is up, so "the turn ended first" has something to clear
			// if it wrongly settles.
			m.input.SetValue("PINEAPPLE")
			m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
			if m.confirm == nil {
				t.Fatalf("setup: no confirm:\n%s", plainView(m))
			}
			notes, rows := len(texts(m, entryNote)), len(texts(m, entryError))

			m = deliver(t, m, endedEvent(stale, tc.fill))

			if m.status != statusWorking {
				t.Fatalf("a stale ending settled the turn on screen: status %s", m.status)
			}
			if m.err != "" {
				t.Fatalf("a stale ending set m.err to %q", m.err)
			}
			if m.confirm == nil {
				t.Fatal("a stale ending answered the confirm the running turn owns")
			}
			if m.cancelled {
				t.Fatal("a stale ending said the turn on screen was cancelled")
			}
			assertStatuses(t, rec)
			// And the record: exactly the rows that ending owes its own turn.
			if got := len(texts(m, entryNote)) - notes; got != tc.notes {
				t.Fatalf("the ending wrote %d notes, want %d: %q", got, tc.notes, texts(m, entryNote))
			}
			if got := len(texts(m, entryError)) - rows; got != tc.rows {
				t.Fatalf("the ending wrote %d error rows, want %d: %q", got, tc.rows, texts(m, entryError))
			}
		})
	}
}

// TestTheCurrentTurnsEndingStillSettles is the other half, and what stops the
// check above from being a way to settle nothing at all: the ending of the turn the
// model IS displaying does everything it always did, and the host hears it.
func TestTheCurrentTurnsEndingStillSettles(t *testing.T) {
	t.Run("a clean ending idles it", func(t *testing.T) {
		m, _, rec, _ := twoTurnsDeep(t)
		m = deliver(t, m, endedEvent(m.turnID, func(t *agent.TurnInfo) { t.StopReason = "end_turn" }))
		if m.status != statusIdle {
			t.Fatalf("status %s, want idle", m.status)
		}
		assertStatuses(t, rec, idleStatus(host.DetailStop))
	})

	t.Run("a cancelled ending idles it and says so", func(t *testing.T) {
		m, _, rec, _ := twoTurnsDeep(t)
		m = deliver(t, m, endedEvent(m.turnID, func(t *agent.TurnInfo) {
			t.Synthetic, t.StopReason = true, stopCancelled
		}))
		if m.status != statusIdle || !m.cancelled {
			t.Fatalf("status %s cancelled %v, want an idle that followed a cancel", m.status, m.cancelled)
		}
		assertStatuses(t, rec, idleStatus(host.DetailCancelled))
	})

	t.Run("a failed ending errors it", func(t *testing.T) {
		m, _, rec, _ := twoTurnsDeep(t)
		m = deliver(t, m, endedEvent(m.turnID, func(t *agent.TurnInfo) {
			t.Err, t.ErrClass = "boom", agent.EventErrOther
		}))
		if m.status != statusError || m.err != "boom" {
			t.Fatalf("status %s err %q, want the error state", m.status, m.err)
		}
		if m.confirm != nil {
			t.Fatal("a failed turn takes the confirm with it")
		}
		assertStatuses(t, rec, hostStatus(host.Failed, host.DetailError, "boom"))
	})

	t.Run("an ending with a successor keeps it working", func(t *testing.T) {
		m, _, rec, _ := twoTurnsDeep(t)
		m = deliver(t, m, endedEvent(m.turnID, func(t *agent.TurnInfo) {
			t.StopReason, t.Next, t.Pending = stopCancelled, "turn-99", 0
		}))
		if m.status != statusWorking {
			t.Fatalf("status %s: an ending with a successor never idles (A7)", m.status)
		}
		assertStatuses(t, rec)
	})
}

// TestALateEndingDoesNotErrorTheTurnThatFollowedIt is the same hole driven through
// the engine rather than fed to the reducer, which is possible for exactly one of
// the shapes: a turn that FAILS reaches the model as the session's own EventError
// before the engine has settled it, and a direct Submit is admitted from an error
// state — so the next turn can be started, synchronously, with the failed turn's
// ending still queued behind it.
//
// The pump is what makes it deterministic: pumpEnter calls Update with the key and
// drains nothing, so the ending that is already waiting in the queue is applied
// only by the pumpUntil after it. The clean-ending shape has no such window — a
// clean turn's settlement is the only thing that could make the model idle, and
// until it is applied Enter queues rather than sends — so that half is
// TestAStaleEndingSettlesNothing's.
func TestALateEndingDoesNotErrorTheTurnThatFollowedIt(t *testing.T) {
	m, sess := scriptedModel(t)
	failing, second := scriptFailed(errors.New("boom")).held(), scriptHeld()
	m = startScripted(t, m, sess, "one", failing)
	sess.Script(second)

	failing.Release()
	// Stops on the EventError, which is published before the continuation
	// returns: the engine cannot have settled the turn yet, and its ending is
	// therefore still to come.
	m = pumpUntil(t, m, allOf(isErrored, errorRows(1)))
	stale := m.turnID

	// Enter from the error state, before the ending is applied: the engine has
	// settled by now — it is what freed the prompt slot — but the model has not
	// heard, so this is precisely the window.
	m = pumpEnter(t, m, "two")
	if m.status != statusWorking {
		t.Fatalf("the second turn did not start from the error state: %s", m.status)
	}
	if m.turnID == stale {
		t.Fatalf("both turns have the id %q", stale)
	}
	awaitBarrier(t, second.opened, "the second turn opening")

	// Now the late ending lands, along with everything else the settlement made.
	m = pumpSettled(t, m)
	if m.status != statusWorking {
		t.Fatalf("the failed turn's late ending errored the turn after it: status %s err %q", m.status, m.err)
	}
	if m.err != "" {
		t.Fatalf("m.err is %q under a running turn", m.err)
	}
	if got := len(texts(m, entryError)); got != 1 {
		t.Fatalf("one failure, one error row, got %d: %q", got, texts(m, entryError))
	}
	// And the turn that is running ends normally.
	second.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if m.err != "" {
		t.Fatalf("m.err %q after a clean second turn", m.err)
	}
	assertPrompts(t, sess, "one", "two")
}

// ------------------------------------------------------------- the echo rule

// TestEchoRuleTheSynchronousStartIsSkipped is the one started the model skips: the
// turn Submit handed back in the Update that pressed Enter. That Update has already
// drawn the user row, gone working and stamped the turn, so drawing the event too
// would draw the row twice — and drawing neither would leave Enter with nothing on
// screen until an event came back, which a frame capture right after Enter would
// see.
func TestEchoRuleTheSynchronousStartIsSkipped(t *testing.T) {
	m, sess := scriptedModel(t)
	sc := scriptHeld()
	m = startScripted(t, m, sess, "typed it", sc)
	// In this very Update, before any event has been delivered.
	if got := texts(m, entryUser); len(got) != 1 || got[0] != "typed it" {
		t.Fatalf("Enter draws its own row in its own Update: %q", got)
	}
	if m.status != statusWorking || m.turnStart.IsZero() {
		t.Fatalf("Enter goes working and stamps the turn: status %s stamped %v", m.status, !m.turnStart.IsZero())
	}
	// Now the started event arrives, and is skipped.
	sc.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if got := texts(m, entryUser); len(got) != 1 {
		t.Fatalf("the started event drew the row a second time: %q", got)
	}
	assertPrompts(t, sess, "typed it")
}

// TestEchoRuleADrainedRowDrawsItsOwnRow is every other started: the model applied
// nothing for it, so it draws its row, enters working and stamps the turn from the
// event. A rule that skipped every event sharing the original Submit's command id
// would lose this row entirely — nothing was drawn when the text was queued.
func TestEchoRuleADrainedRowDrawsItsOwnRow(t *testing.T) {
	m, sess := scriptedModel(t)
	first, drained := scriptHeld(), scriptHeld()
	m = startScripted(t, m, sess, "go", first)
	sess.Script(drained)
	m = pumpEnter(t, m, "PINEAPPLE")
	// Queued, and nothing drawn for it.
	if got := texts(m, entryUser); len(got) != 1 {
		t.Fatalf("a queued row draws no user entry: %q", got)
	}
	first.Release()
	awaitBarrier(t, drained.opened, "the drained turn opening")
	m = pumpUntil(t, m, turnsDrawn(2))
	if got := texts(m, entryUser); got[1] != "PINEAPPLE" {
		t.Fatalf("the drained row draws its own entry: %q", got)
	}
	if m.status != statusWorking {
		t.Fatalf("the drained start enters working: %s", m.status)
	}
	drained.Release()
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if got := texts(m, entryUser); len(got) != 2 {
		t.Fatalf("one entry per turn: %q", got)
	}
	assertPrompts(t, sess, "go", "PINEAPPLE")
}

// TestEchoRuleAnArmedSendDrawsItsRowWhenItFires is the third case, and the one a
// blanket "skip your own command" rule gets wrong: arming draws nothing — nothing
// leaves the queue or the composer — so the started that fires it is the only chance
// the row has. The draft goes with it only if the composer still holds the text that
// was armed; a draft the user has changed since is theirs to keep.
func TestEchoRuleAnArmedSendDrawsItsRowWhenItFires(t *testing.T) {
	for _, tc := range []struct {
		name string
		// after is what the composer holds by the time the send fires.
		after string
		// wantDraft is what must be left in it afterwards.
		wantDraft string
	}{
		{name: "the draft still matches", after: "", wantDraft: ""},
		{name: "the draft changed since", after: "something else", wantDraft: "something else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sess := scriptedModel(t)
			first, sent := scriptHeld(), scriptHeld()
			m = startScripted(t, m, sess, "go", first)
			sess.Script(sent)
			m.input.SetValue("PINEAPPLE")
			m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
			release := sess.HoldNextCancel()
			m = pumpKey(t, m, enter())
			awaitBarrier(t, sess.Cancels(), "the arm's cancel reaching the session")
			// Nothing drawn for the arm, and the draft still where it was.
			if got := texts(m, entryUser); len(got) != 1 {
				t.Fatalf("arming draws no row: %q", got)
			}
			if m.input.Value() != "PINEAPPLE" {
				t.Fatalf("arming takes nothing from the composer: %q", m.input.Value())
			}
			if tc.after != "" {
				m.input.SetValue(tc.after)
			}

			release()
			awaitBarrier(t, sent.opened, "the armed send's turn opening")
			m = pumpUntil(t, m, turnsDrawn(2))
			if got := texts(m, entryUser); got[1] != "PINEAPPLE" {
				t.Fatalf("the fired send draws its own row: %q", got)
			}
			if m.input.Value() != tc.wantDraft {
				t.Fatalf("the composer holds %q, want %q", m.input.Value(), tc.wantDraft)
			}
			sent.Release()
			m = pumpUntil(t, m, isIdle)
			m = pumpSettled(t, m)
			assertPrompts(t, sess, "go", "PINEAPPLE")
		})
	}
}

// TestSendNowDeltasBecomeTheNotesTheyAlwaysWere walks the disarm reasons a user can
// reach and checks each says what it always said: a withdrawal the model asked for
// says so in the Update that asked (and the delta for that same command is its own
// echo, so it is not said twice), and a row that had already gone says that instead.
func TestSendNowDeltasBecomeTheNotesTheyAlwaysWere(t *testing.T) {
	t.Run("withdrawn says it once", func(t *testing.T) {
		m, _ := armedSendNow(t, "PINEAPPLE")
		m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
		if !strings.Contains(plainView(m), "send now dropped") {
			t.Fatalf("Esc says it in its own Update:\n%s", plainView(m))
		}
		// The delta for that same Disarm is this model's own echo: it changes
		// nothing, and the note simply lingers as it always did.
		m = pumpSettled(t, m)
		if sendNowArmed(m) {
			t.Fatal("the send is still armed")
		}
	})

	t.Run("a row that had already gone", func(t *testing.T) {
		m, sess := scriptedModel(t)
		first, sent := scriptHeld(), scriptHeld()
		m = startScripted(t, m, sess, "go", first)
		sess.Script(sent)
		m = pumpEnter(t, m, "PINEAPPLE")
		m = pumpEnter(t, m, "MANGO")
		// Arm the first row, then take it away underneath the arm.
		m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
		m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
		m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
		release := sess.HoldNextCancel()
		m = pumpKey(t, m, enter())
		awaitBarrier(t, sess.Cancels(), "the arm's cancel reaching the session")
		unqueueRow(t, m, queueIDs(m)[0])

		release()
		m = pumpUntil(t, m, viewHas("that message has already gone"))
		if sendNowArmed(m) {
			t.Fatal("a send whose row had gone is disarmed")
		}
		// And the head of the queue took its place, as the drain always did.
		awaitBarrier(t, sent.opened, "the row behind it opening")
		sent.Release()
		m = pumpUntil(t, m, allOf(isIdle, turnsReached(sess, 2)))
		_ = pumpSettled(t, m)
		assertPrompts(t, sess, "go", "MANGO")
	})
}
