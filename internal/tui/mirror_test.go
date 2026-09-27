package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// republish is a fixture's set-up made visible the way a live session makes
// its state visible: the Stub publishes its settings as one install delta —
// every section, as its Start does (installStateLocked) — and every event it
// has published is applied, as the event reader would apply it. A fixture that
// builds its session by writing the Stub's snapshot (a Set* set-up helper, or
// a write under its lock) used to show it by reading the snapshot back; the
// mirror is the fold now (plan 027 §3.13), and the fold holds what the stream
// said.
func republish(t *testing.T, m Model) Model {
	t.Helper()
	stub := stubOf(t, m)
	stub.mu.Lock()
	stub.enqueueDeltaLocked("", stub.installStateLocked())
	stub.mu.Unlock()
	return feed(t, m, stubDeltas(t, stub)...)
}

// applyPending applies every event the session has published and the model
// has not read yet, as the event reader would: what a fixture that acts on the
// engine directly (another client, in effect) owes the model before it looks
// — the mirror shows the fold, and the fold holds what the stream said (plan
// 027 §3.13).
func applyPending(t *testing.T, m Model) Model {
	t.Helper()
	drainPending(t, m, func(ev agent.Event) {
		tm, _ := m.Update(eventMsg{ev})
		m = tm.(Model)
	})
	return m
}

// pendingEvents is every event the session has published and the model has
// not read yet, flushed first so "published" means "there", without applying
// any: a test that wants each one's effect on the mirror feeds them itself.
func pendingEvents(t *testing.T, m Model) []agent.Event {
	t.Helper()
	var out []agent.Event
	drainPending(t, m, func(ev agent.Event) { out = append(out, ev) })
	return out
}

// drainPending flushes m's session and hands each event on its primary to
// each as it is read, until none is waiting: applyPending applies each before
// reading the next — so an event that application publishes is read too —
// and pendingEvents keeps them.
func drainPending(t *testing.T, m Model, each func(agent.Event)) {
	t.Helper()
	eng := engineOf(t, m)
	if _, err := eng.SyncSeq(context.Background()); err != nil {
		t.Fatalf("flushing what the session published: %v", err)
	}
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				return
			}
			each(ev)
		default:
			return
		}
	}
}

// otherClient is a second client of m's engine, as another attached client
// would be: its commands name it, and the events they cause carry its cause.
func otherClient(t *testing.T, m Model) engine.Command {
	t.Helper()
	return engine.Command{Client: engineOf(t, m).NewClientID(), ID: "1"}
}

// noOverlays fails the test when any overlay is still laid over the fold.
func noOverlays(t *testing.T, m Model, when string) {
	t.Helper()
	if o := m.ov; o.mode.set || o.model.set || len(o.config) > 0 || len(o.results) > 0 {
		t.Fatalf("%s: overlays still stand: %+v", when, o)
	}
}

// queuedRowOf is the fold's row whose text is text.
func queuedRowOf(t *testing.T, m Model, text string) agent.QueuedPrompt {
	t.Helper()
	for _, p := range m.shared.State().Queue {
		if p.Text == text {
			return p
		}
	}
	t.Fatalf("no row %q in the fold's queue %+v", text, m.shared.State().Queue)
	return agent.QueuedPrompt{}
}

// inner hands m back to the rig: what a test did to the model by a direct
// call — a verb with no key of its own for this schedule — is the rig's model
// from here.
func (r *schedRun) inner(m Model) { r.f.inner = m }

// TestResultOverlaysRetireOnTheirOwnEvent (plan 027 §3.12, "Retirement is by
// the command's own event for that item"; astra r2 9–10, r3 9–10): a gated
// command's effect is drawn from its result as an overlay on the fold, keyed by
// the command's cause and the item, and retired only when the fold applies that
// command's own event for that item — never by equality with the fold, never
// by the command's first event for another item, and never for a success that
// published nothing.
func TestResultOverlaysRetireOnTheirOwnEvent(t *testing.T) {
	t.Run("the ABA title", func(t *testing.T) {
		m := sized(t)
		m = applyPending(t, runSlash(t, m, "/rename X"))
		if m.snap.Title != "X" {
			t.Fatalf("fixture: the fold's title is %q", m.snap.Title)
		}
		noOverlays(t, m, "the first rename folded")
		// Another client renames to Y, and this one renames back to X, before
		// either delta is folded.
		if err := engineOf(t, m).SetTitle(otherClient(t, m), "Y"); err != nil {
			t.Fatal(err)
		}
		m = runSlash(t, m, "/rename X")
		evs := pendingEvents(t, m)
		if len(evs) != 2 || evs[0].State == nil || *evs[0].State.Title != "Y" || evs[1].State == nil || *evs[1].State.Title != "X" {
			t.Fatalf("fixture: the stream is %+v, want Y's delta then X's", evs)
		}
		if m.snap.Title != "X" {
			t.Fatalf("the rename's result shows %q, want X", m.snap.Title)
		}
		// Y's delta is another command's: the overlay holds X through it,
		// though the fold showed X when it was installed.
		m = feed(t, m, evs[0])
		if m.snap.Title != "X" {
			t.Fatalf("the title shows %q through another client's rename, want this client's X until its own delta", m.snap.Title)
		}
		m = feed(t, m, evs[1])
		if m.snap.Title != "X" {
			t.Fatalf("the title shows %q, want X", m.snap.Title)
		}
		noOverlays(t, m, "the rename's own delta folded")
	})

	t.Run("a disarm over a still-pending arm", func(t *testing.T) {
		r := newSchedRun(t, true)
		aWorkingTurn(r)
		// Another client arms a send-now; the arm's own cancel is held, so
		// the turn it cancels cannot settle into it.
		release := r.sess.HoldNextCancelPastItsDeadline()
		t.Cleanup(release)
		if res, err := r.eng.Submit(otherClient(t, r.m()), "THEIRS", engine.SubmitSendNow, ""); err != nil || !res.Armed {
			t.Fatalf("fixture: the other client's send-now came back %+v, %v", res, err)
		}
		// This client takes it back before the arm's delta is folded.
		m, _ := r.m().withdrawSendNow("send now dropped", linkDone)
		r.inner(m)
		if r.m().sendNowPending() {
			t.Fatal("the Disarm's result shows the send-now armed")
		}
		evs := r.pending()
		if len(evs) != 2 || evs[0].State == nil || evs[0].State.SendNow == nil || !evs[0].State.SendNow.Armed ||
			evs[1].State == nil || evs[1].State.SendNow == nil || evs[1].State.SendNow.Armed {
			t.Fatalf("fixture: the stream is %+v, want the arm's delta then the withdrawal's", evs)
		}
		// The arm's delta is another command's: the disarm overlay survives
		// it, though the fold said "not armed" when it was installed.
		r.stepEvents(evs[:1])
		if r.m().sendNowPending() {
			t.Fatal("an earlier arm's delta, folded after this client's Disarm answered, shows the send-now armed")
		}
		r.stepEvents(evs[1:])
		if r.m().sendNowPending() {
			t.Fatal("the withdrawal folded, and the send-now shows armed")
		}
		noOverlays(t, r.m(), "the Disarm's own delta folded")
	})

	t.Run("a clear with another client's add not yet folded", func(t *testing.T) {
		r := newSchedRun(t, true)
		queuedBehind(r, "MINE")
		mine := queuedRowOf(t, r.m(), "MINE")
		theirs, err := r.eng.Queue(otherClient(t, r.m()), "THEIRS")
		if err != nil {
			t.Fatal(err)
		}
		r.typeText("/clear")
		r.key(enter())
		if len(r.m().queue) != 0 {
			t.Fatalf("the clear's result leaves the band %+v", r.m().queue)
		}
		evs := r.pending()
		if len(evs) != 3 {
			t.Fatalf("fixture: the stream is %+v, want the add and the two removals", evs)
		}
		for i := range evs {
			r.stepEvents(evs[i : i+1])
			if m := r.m(); m.queueHasID(theirs.ID) || m.queueHasID(mine.ID) {
				t.Fatalf("after event %d (%s %s) the band draws %+v: a row the clear removed flashed back", i, evs[i].Type, evs[i].QueueChange, m.queue)
			}
		}
		noOverlays(t, r.m(), "every removal folded")
	})

	t.Run("a multi-row clear", func(t *testing.T) {
		r := newSchedRun(t, true)
		queuedBehind(r, "A", "B")
		a, b := queuedRowOf(t, r.m(), "A"), queuedRowOf(t, r.m(), "B")
		r.typeText("/clear")
		r.key(enter())
		evs := r.pending()
		if len(evs) != 2 || evs[0].Queue == nil || evs[0].Queue.ID != a.ID || evs[1].Queue == nil || evs[1].Queue.ID != b.ID {
			t.Fatalf("fixture: the stream is %+v, want A's removal then B's", evs)
		}
		// A's removal is the clear's own event for A, and for A alone: B stays
		// hidden, though the fold still holds it.
		r.stepEvents(evs[:1])
		if m := r.m(); m.queueHasID(b.ID) || m.queueHasID(a.ID) {
			t.Fatalf("after A's removal the band draws %+v", m.queue)
		}
		r.stepEvents(evs[1:])
		if m := r.m(); len(m.queue) != 0 {
			t.Fatalf("after both removals the band draws %+v", m.queue)
		}
		noOverlays(t, r.m(), "every removal folded")
	})

	t.Run("an edit followed by another client's edit", func(t *testing.T) {
		r := newSchedRun(t, true)
		queuedBehind(r, "ROW")
		row := queuedRowOf(t, r.m(), "ROW")
		r.key(tea.KeyMsg{Type: tea.KeyUp})
		r.key(enter())
		for range "ROW" {
			r.key(tea.KeyMsg{Type: tea.KeyBackspace})
		}
		r.typeText("MINE")
		r.key(enter())
		if got := r.m().queue[0]; got.Text != "MINE" || got.Version != row.Version+1 {
			t.Fatalf("the edit's result shows %+v, want MINE at version %d", got, row.Version+1)
		}
		if err := r.eng.EditQueued(otherClient(t, r.m()), row.ID, "THEIRS", nil); err != nil {
			t.Fatal(err)
		}
		evs := r.pending()
		if len(evs) != 2 || evs[0].Queue == nil || evs[0].Queue.Text != "MINE" || evs[1].Queue == nil || evs[1].Queue.Text != "THEIRS" {
			t.Fatalf("fixture: the stream is %+v, want this edit then the other client's", evs)
		}
		r.stepEvents(evs[:1])
		if got := r.m().queue[0].Text; got != "MINE" {
			t.Fatalf("after its own edit the row reads %q", got)
		}
		noOverlays(t, r.m(), "the edit's own event folded")
		r.stepEvents(evs[1:])
		if got := r.m().queue[0]; got.Text != "THEIRS" || got.Version != row.Version+2 {
			t.Fatalf("the other client's later edit shows %+v, want THEIRS at version %d", got, row.Version+2)
		}
	})

	t.Run("a success that says nothing", func(t *testing.T) {
		r := newSchedRun(t, true)
		queuedBehind(r, "ROW")
		row := queuedRowOf(t, r.m(), "ROW")
		// A row sent now that cannot start — a turn is running — is answered
		// with the row, still queued, and nothing is mutated or said
		// (engine.go's submit): no overlay.
		m, _ := r.m().submit(row.Text, engine.SubmitQueue, row.ID, submitted)
		r.inner(m)
		if evs := r.pending(); len(evs) != 0 {
			t.Fatalf("fixture: the Submit published %+v", evs)
		}
		noOverlays(t, r.m(), "a success that published nothing")
		// So nothing of it outlives the row: another client's removal takes
		// it out of the band.
		if _, err := r.eng.Unqueue(otherClient(t, r.m()), row.ID); err != nil {
			t.Fatal(err)
		}
		r.feed()
		if r.m().queueHasID(row.ID) {
			t.Fatalf("the band still draws %+v after the row was removed", r.m().queue)
		}
	})
}

// TestARestoredRowReappears is X1's pin (plan 027, execution amendment X1):
// Submit(fromRow) starts a turn from row R, and the agent's own turn refuses
// it (ErrForeignTurn) — the engine puts R back at the head, with the Submit's
// own cause (SF-21). The "R absent" overlay the Submit's result installed is
// retired by whichever of the Submit's events for R the fold applies, the sent
// or the restoring queued, so R shows again once it is restored — never kept
// hidden past a queued for the same (cause, R) — and it reruns later as a new
// turn, not the Submit's.
func TestARestoredRowReappears(t *testing.T) {
	r := newSchedRun(t, true)
	// A row in the band of an idle session, which nothing drains until a
	// turn is sent.
	strandedRows(r, "ROW")
	row := r.m().queue[0]
	// Sent now, it starts a turn of its own, and the agent's own turn refuses
	// it.
	r.sess.Script(scriptRefused(agent.ErrForeignTurn))
	r.key(tea.KeyMsg{Type: tea.KeyUp})
	r.key(tea.KeyMsg{Type: tea.KeyCtrlL})
	submitTurn := r.m().turnID
	if submitTurn == "" || r.m().queueHasID(row.ID) {
		t.Fatalf("the Submit's result: turn %q, band %+v — want a turn begun and the row gone", submitTurn, r.m().queue)
	}
	restoredIn := func(evs []agent.Event) bool {
		return slices.ContainsFunc(evs, func(ev agent.Event) bool {
			return ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueQueued && ev.Queue != nil && ev.Queue.ID == row.ID
		})
	}
	evs := r.until(restoredIn)
	at := slices.IndexFunc(evs, func(ev agent.Event) bool { return restoredIn([]agent.Event{ev}) })
	r.stepEvents(evs[:at+1])
	if !r.m().queueHasID(row.ID) {
		t.Fatalf("R was restored and the band draws %+v: the overlay kept it hidden past its restoring queued", r.m().queue)
	}
	// It reruns later as a turn of its own (X1 pin 4), not the Submit's.
	r.stepEvents(evs[at+1:])
	r.feedUntil(func(evs []agent.Event) bool {
		return slices.ContainsFunc(evs, func(ev agent.Event) bool {
			return ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnStarted && ev.Turn.ID != submitTurn
		})
	})
	if id := r.m().turnID; id == submitTurn || id == "" {
		t.Fatalf("the rerun's turn is %q, want a new one, not the Submit's %q", id, submitTurn)
	}
	if r.m().queueHasID(row.ID) {
		t.Fatalf("the row reran and the band still draws %+v", r.m().queue)
	}
	noOverlays(t, r.m(), "the row restored and rerun")
}

// TestTheModeChipHoldsUntilItsDelta (§3.13, astra r2 11): a mode change's
// success reaches the reducer with its result, and the chip holds the
// confirmed mode until the fold's mode revision reaches it — it does not snap
// back to the folded mode when the answer lands ahead of its delta.
func TestTheModeChipHoldsUntilItsDelta(t *testing.T) {
	m := sized(t)
	m, cmd := askMode(t, m, "plan")
	applied, ok := runCmd(cmd).(modeAppliedMsg)
	if !ok || applied.rev == 0 {
		t.Fatalf("the mode change answered %#v, want its result with a revision", applied)
	}
	m = deliver(t, m, applied)
	if m.snap.CurrentMode != "plan" || m.shared.State().Settings.Mode != "agent" {
		t.Fatalf("the chip %q over the fold's %q: the answer, ahead of its delta, must not take the chip back", m.snap.CurrentMode, m.shared.State().Settings.Mode)
	}
	if m.modeInFlight != "" {
		t.Fatalf("modeInFlight %q: the answer is in", m.modeInFlight)
	}
	m = applyPending(t, m)
	if m.snap.CurrentMode != "plan" || m.modeRev < applied.rev {
		t.Fatalf("the chip %q at mode revision %d, want plan at %d", m.snap.CurrentMode, m.modeRev, applied.rev)
	}
	noOverlays(t, m, "the delta folded")
}

// TestAModeSetThatPublishesNothingRetiresOnTheFold (decision A): a mode change
// to the value the session already holds can succeed and publish nothing — its
// answer carries revision 0. Its overlay is retired once something newer has
// been applied to the mode section, or the fold shows the confirmed value, and
// never outlives that to mask a later change of another client's.
func TestAModeSetThatPublishesNothingRetiresOnTheFold(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	m := startSession(t, quietMode{stub}, t.TempDir(), 80, 24)
	eng := engineOf(t, m)
	other := otherClient(t, m)
	later := other
	later.ID = "2"
	// Another client moves the session to plan; its delta is not folded yet.
	if _, err := eng.Set(context.Background(), other, engine.Setting{Kind: engine.SettingMode, Value: "plan"}); err != nil {
		t.Fatal(err)
	}
	// This client asks for plan too: the session is already there, so the
	// answer publishes nothing.
	m, cmd := askMode(t, m, "plan")
	applied, ok := runCmd(cmd).(modeAppliedMsg)
	if !ok || applied.rev != 0 {
		t.Fatalf("the mode change answered %#v, want a success with no revision", applied)
	}
	m = deliver(t, m, applied)
	if m.snap.CurrentMode != "plan" {
		t.Fatalf("the chip %q, want the confirmed plan", m.snap.CurrentMode)
	}
	m = applyPending(t, m)
	noOverlays(t, m, "the other client's delta folded")
	// Another client's later change shows: nothing of this request masks it.
	if _, err := eng.Set(context.Background(), later, engine.Setting{Kind: engine.SettingMode, Value: "ask"}); err != nil {
		t.Fatal(err)
	}
	m = applyPending(t, m)
	if m.snap.CurrentMode != "ask" {
		t.Fatalf("the chip %q, want another client's later ask", m.snap.CurrentMode)
	}
}

// quietMode is a Stub whose SetMode to the mode it is already in publishes
// nothing — a success with no delta, and so no revision.
type quietMode struct{ *Stub }

func (s quietMode) SetMode(ctx context.Context, cause, id string) (agent.SetOutcome, error) {
	s.mu.Lock()
	same := s.snap.CurrentMode == id
	s.mu.Unlock()
	if same {
		return agent.SetOutcome{Value: id}, nil
	}
	return s.Stub.SetMode(ctx, cause, id)
}

// TestAModelWithNoEffortRetiresOnItsRevision (§3.13, astra r2 11): `/model
// <id>` with no effort step answers with its result, and its overlay holds the
// confirmed model until the fold's model revision reaches it — then it is
// gone, and a later change of another client's shows.
func TestAModelWithNoEffortRetiresOnItsRevision(t *testing.T) {
	m := sized(t)
	m.input.SetValue("/model fast")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	set, ok := runCmd(cmd).(modelSetMsg)
	if !ok || set.landed.res.Value != "fast" || set.landed.res.Rev == 0 {
		t.Fatalf("/model fast answered %#v", set)
	}
	m = deliver(t, m, set)
	if m.snap.CurrentModel != "fast" || !m.ov.model.set || !m.ov.model.confirmed {
		t.Fatalf("the model %q, overlay %+v: want fast, confirmed and held ahead of its delta", m.snap.CurrentModel, m.ov.model)
	}
	m = applyPending(t, m)
	if m.modelRev < set.landed.res.Rev {
		t.Fatalf("fixture: the model revision %d, want its delta's %d", m.modelRev, set.landed.res.Rev)
	}
	noOverlays(t, m, "the model's delta folded")
	if _, err := engineOf(t, m).Set(context.Background(), otherClient(t, m), engine.Setting{Kind: engine.SettingModel, Value: "grok"}); err != nil {
		t.Fatal(err)
	}
	m = applyPending(t, m)
	if m.snap.CurrentModel != "grok" {
		t.Fatalf("the model %q, want another client's later grok", m.snap.CurrentModel)
	}
}

// TestTwoRefusalsLandOnTheFoldsModel is SF-38: from grok-4.6, `/model
// composer-2.5` then `/model claude-opus-5` before either answers, both refused with no model delta.
// Each refusal takes down its own overlay and puts nothing back, so the screen
// lands on the fold's model — never on the first command's optimistic value,
// which the second command captured as its predecessor — in either order.
func TestTwoRefusalsLandOnTheFoldsModel(t *testing.T) {
	for _, secondFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("the second refusal first %v", secondFirst), func(t *testing.T) {
			m, stub := cursorStub(t, "grok-4.6")
			m.input.SetValue("/model composer-2.5")
			tm, first := m.Update(enter())
			m = tm.(Model)
			m.input.SetValue("/model claude-opus-5")
			tm, second := m.Update(enter())
			m = tm.(Model)
			if m.snap.CurrentModel != "claude-opus-5" {
				t.Fatalf("fixture: the optimistic model is %q", m.snap.CurrentModel)
			}
			stub.FailNextSetModel()
			r1, ok1 := runCmd(first).(revertModelMsg)
			stub.FailNextSetModel()
			r2, ok2 := runCmd(second).(revertModelMsg)
			if !ok1 || !ok2 || r2.prev != "composer-2.5" {
				t.Fatalf("fixture: the refusals are %+v and %+v", r1, r2)
			}
			if secondFirst {
				m = deliver(t, m, r2)
				m = deliver(t, m, r1)
			} else {
				m = deliver(t, m, r1)
				m = deliver(t, m, r2)
			}
			if m.snap.CurrentModel != "grok-4.6" || m.model != "grok-4.6" {
				t.Fatalf("the screen is on %q / %q, want the fold's grok-4.6", m.snap.CurrentModel, m.model)
			}
			if got := stub.Snapshot().CurrentModel; got != "grok-4.6" {
				t.Fatalf("fixture: the session moved to %q", got)
			}
			noOverlays(t, m, "both refusals in")
			if n := len(texts(m, entryError)); n != 2 {
				t.Fatalf("%d error rows, want both refusals'", n)
			}
		})
	}
}

// TestAClearingTodosEventNotesNothingOld (§3.13, GLM 5): the todos arm reads the
// event's own list — the fold's rule is replacement — so an EventTodos that
// clears the list notes nothing, even where the pane's dedupe was reset by
// /clear and the mirror, before this event's fold, still holds the old list.
func TestAClearingTodosEventNotesNothingOld(t *testing.T) {
	m, stub, _ := todoModel(t)
	m = sendTodos(t, m, stub, openTodos())
	if got := noteTexts(m.main); len(got) != 1 {
		t.Fatalf("fixture: the notes %q", got)
	}
	m = runSlash(t, m, "/clear")
	if got := noteTexts(m.main); len(got) != 0 {
		t.Fatalf("fixture: /clear left the notes %q", got)
	}
	if len(m.snap.Todos) != 3 {
		t.Fatalf("fixture: the mirror holds %+v before the clearing event", m.snap.Todos)
	}
	m = sendTodos(t, m, stub, nil)
	if got := noteTexts(m.main); len(got) != 0 {
		t.Fatalf("a clearing todo list noted %q: the old list, re-noted", got)
	}
	if len(m.snap.Todos) != 0 || m.tasksPanelVisible() {
		t.Fatalf("the mirror holds %+v after the clearing event", m.snap.Todos)
	}
}

// TestAGrokChildsActivityFollowsItsToolTitles (decision B): the live session
// keeps a child's activity as the title of its most recent tool call without
// publishing a roster event for it, so the mirror follows the child's own tool
// events — the live rule, and the live 256-byte cap — until the next roster
// event carries the row whole.
func TestAGrokChildsActivityFollowsItsToolTitles(t *testing.T) {
	m := sized(t)
	stubOf(t, m).SetProvider(agent.GrokProvider())
	child := agent.SubagentInfo{ID: "c1", Status: agent.SubagentRunning, Description: "explore"}
	activity := func(m Model) string {
		info, _ := liveInfo(m.snap.Subagents, "c1")
		return info.Activity
	}
	m = feed(t, m, agent.Event{Type: agent.EventSubagent, Subagent: &child, SubagentChange: agent.SubagentChangeSpawned})
	if got := activity(m); got != "" {
		t.Fatalf("fixture: the spawned row's activity is %q", got)
	}
	m = feed(t, m, agent.Event{Type: agent.EventTool, Agent: "c1", Tool: &agent.ToolEvent{ID: "t1", Title: "read_file", Status: "in_progress"}})
	if got := activity(m); got != "read_file" {
		t.Fatalf("the child's activity is %q, want its tool's title", got)
	}
	if info, _ := liveInfo(m.shared.State().Agents, "c1"); info.Activity != "" {
		t.Fatalf("fixture: the fold's row carries %q: this schedule needs one that does not", info.Activity)
	}
	progressed := child
	progressed.Activity = "thinking"
	m = feed(t, m, agent.Event{Type: agent.EventSubagent, Subagent: &progressed, SubagentChange: agent.SubagentChangeProgress})
	if got := activity(m); got != "thinking" {
		t.Fatalf("the child's activity is %q, want the roster's own word once it carries the row", got)
	}
	long := strings.Repeat("é", 200)
	m = feed(t, m, agent.Event{Type: agent.EventTool, Agent: "c1", Tool: &agent.ToolEvent{ID: "t2", Title: long, Status: "in_progress"}})
	if got := activity(m); len(got) > agent.SubagentActivityCap || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("the activity is %d bytes (%q...), want it cut at %d on a rune boundary with an ellipsis", len(got), got[:10], agent.SubagentActivityCap)
	}
}
