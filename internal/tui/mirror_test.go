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
		tm, _ := m.Update(eventMsg{ev: ev})
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
	// It reruns later as a turn of its own (X1 pin 4), not the Submit's. The
	// rerun is the restore's paced recheck (the driver's tick, 10 ms after the
	// restore), and until returns everything already on the stream, not only up
	// to the restore: a run slower than the tick has the rerun's started in evs
	// already, and a read that waited for it would wait for ever.
	rerun := func(evs []agent.Event) bool {
		return slices.ContainsFunc(evs, func(ev agent.Event) bool {
			return ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnStarted && ev.Turn.ID != submitTurn
		})
	}
	r.stepEvents(evs[at+1:])
	if !rerun(evs[at+1:]) {
		r.feedUntil(rerun)
	}
	if id := r.m().turnID; id == submitTurn || id == "" {
		t.Fatalf("the rerun's turn is %q, want a new one, not the Submit's %q", id, submitTurn)
	}
	if r.m().queueHasID(row.ID) {
		t.Fatalf("the row reran and the band still draws %+v", r.m().queue)
	}
	noOverlays(t, r.m(), "the row restored and rerun")
}

// editRow is a schedule's keys for an edit of the band's first row: the band
// focused, the row loaded into the composer, its text replaced by text — not
// saved.
func editRow(r *schedRun, text string) {
	r.t.Helper()
	r.key(tea.KeyMsg{Type: tea.KeyUp})
	r.key(enter())
	if r.m().queueEdit == "" {
		r.t.Fatal("prepare: no row is being edited")
	}
	for range r.m().input.Value() {
		r.key(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	r.typeText(text)
}

// TestASentRowShowsTheTextThatWasSent (plan 027 §3.13, "Two-client
// correctness"; A23): another client edits a queued row after this client
// read it from its band and before this client's send-now of it reaches the
// engine. The turn starts with the row's text as the engine took it — the
// other client's — and so does the user row this client draws for it: the
// optimistic row is SubmitResult.Text, not the text this client read, and the
// transcript never shows a message the agent was not sent.
func TestASentRowShowsTheTextThatWasSent(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			r := newSchedRun(t, mode.sync)
			strandedRows(r, "ROW")
			row := r.m().queue[0]
			r.key(tea.KeyMsg{Type: tea.KeyUp})
			if sel, ok := r.m().queueSelected(); !ok || sel.ID != row.ID || sel.Text != "ROW" {
				t.Fatalf("fixture: the band's selection is %+v, %v", sel, ok)
			}
			// Another client edits the row; this client has not folded it.
			if err := r.eng.EditQueued(otherClient(t, r.m()), row.ID, "THEIRS", nil); err != nil {
				t.Fatal(err)
			}
			r.key(tea.KeyMsg{Type: tea.KeyCtrlL})
			if r.m().turnID == "" || r.m().queueHasID(row.ID) {
				t.Fatalf("fixture: the send-now: turn %q, band %+v — want a turn begun from the row", r.m().turnID, r.m().queue)
			}
			if got := texts(r.m(), entryUser); !slices.Equal(got, []string{"THEIRS"}) {
				t.Fatalf("the sent row reads %q, want the text the turn started with, THEIRS", got)
			}
			// Folded, the turn's own started is the row's echo: still the one
			// row, the text the agent was sent.
			r.feedUntil(endings(1))
			if got := texts(r.m(), entryUser); !slices.Equal(got, []string{"THEIRS"}) {
				t.Fatalf("once folded the user rows are %q, want THEIRS once", got)
			}
			assertPrompts(t, r.stub, "THEIRS")
			noOverlays(t, r.m(), "the send folded")
		})
	}
}

// TestTwoEditorsDoNotOverwriteEachOther (plan 027 §3.13, "Two-client
// correctness"; §4 behaviour change 9; A23): two clients edit one queued row.
// The other client saves first; this client's save, begun from the row's
// earlier version, is refused stale_version — the other client's text stands
// — and keeps the edit open with the text the user wrote, with a note saying
// the message changed and that Enter again saves over it; nothing is
// installed, and the band is the fold's row. The refusal refreshes the edit's
// version to the row the band shows then, so the second Enter saves over the
// change the note told of — and only that change: one the fold had not shown
// at the refusal (still on its way, or made since) refuses that Enter too, and
// refreshes the version again.
//
// The brief's v0/v1 schedule for astra r53 1 (B's unfolded edit, then A's
// save) is the "on its way" case: with the check, A's save is a stale refusal
// that installs no overlay, so no version is shown that the engine did not
// commit.
func TestTwoEditorsDoNotOverwriteEachOther(t *testing.T) {
	type step func(r *schedRun, row agent.QueuedPrompt)
	// theirs is the other client's edit of the row, checked against version v.
	theirs := func(text string, v int) step {
		return func(r *schedRun, row agent.QueuedPrompt) {
			r.t.Helper()
			if err := r.eng.EditQueued(otherClient(r.t, r.m()), row.ID, text, &v); err != nil {
				r.t.Fatalf("fixture: the other client's save of %q: %v", text, err)
			}
		}
	}
	fold := func(r *schedRun, _ agent.QueuedPrompt) { r.feed() }
	// refused is this client's Enter, refused: the edit kept with MINE, the
	// note this Enter's own, the engine's row the other client's want at its
	// version, nothing installed, and the edit's version refreshed to ver.
	refused := func(want string, at, ver int) step {
		return func(r *schedRun, row agent.QueuedPrompt) {
			r.t.Helper()
			m := r.m()
			m.copyNote = ""
			r.inner(m)
			r.key(enter())
			m = r.m()
			if m.queueEdit != row.ID || m.input.Value() != "MINE" {
				r.t.Fatalf("a refused save: editing %q with %q, want the edit of %s kept with MINE", m.queueEdit, m.input.Value(), row.ID)
			}
			if m.copyNote != staleEditNote {
				r.t.Fatalf("a refused save: the note is %q, want %q", m.copyNote, staleEditNote)
			}
			if got := queuedRows(m); len(got) != 1 || got[0].Text != want || got[0].Version != at {
				r.t.Fatalf("a refused save: the engine's queue is %+v, want the other client's %q at version %d", got, want, at)
			}
			noOverlays(r.t, m, "a refused save")
			if m.queueEditVer != ver {
				r.t.Fatalf("a refused save: the edit's version is %d, want %d", m.queueEditVer, ver)
			}
		}
	}
	// saved is this client's Enter, saving over the other client's change at
	// version at: the edit done, and the band's row, from the result, the
	// engine's own — text and version — before the edit's event is folded.
	saved := func(at int) step {
		return func(r *schedRun, row agent.QueuedPrompt) {
			r.t.Helper()
			m := r.m()
			m.copyNote = ""
			r.inner(m)
			r.key(enter())
			m = r.m()
			if m.queueEdit != "" || m.copyNote != "" {
				r.t.Fatalf("the save: editing %q, note %q — want it saved", m.queueEdit, m.copyNote)
			}
			eq := queuedRows(m)
			if len(eq) != 1 || eq[0].Text != "MINE" || eq[0].Version != at+1 {
				r.t.Fatalf("the save: the engine's queue is %+v, want MINE at version %d", eq, at+1)
			}
			if len(m.queue) != 1 || m.queue[0].Text != eq[0].Text || m.queue[0].Version != eq[0].Version {
				r.t.Fatalf("the save's result shows %+v, want the engine's row %+v", m.queue, eq[0])
			}
			r.feed()
			if got := r.m().queue; len(got) != 1 || got[0].Text != "MINE" || got[0].Version != at+1 {
				r.t.Fatalf("once folded the band draws %+v, want MINE at version %d", got, at+1)
			}
			noOverlays(r.t, r.m(), "the save folded")
		}
	}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"the fold has shown the change", []step{
			theirs("THEIRS", 0), fold,
			refused("THEIRS", 1, 1),
			saved(1),
		}},
		{"the change still on its way", []step{
			theirs("THEIRS", 0),
			// Nothing of it in the fold: nothing to save over yet.
			refused("THEIRS", 1, 0),
			refused("THEIRS", 1, 0),
			fold,
			refused("THEIRS", 1, 1),
			saved(1),
		}},
		{"the other client edits again", []step{
			theirs("THEIRS", 0), fold,
			refused("THEIRS", 1, 1),
			theirs("AGAIN", 1), fold,
			refused("AGAIN", 2, 2),
			saved(2),
		}},
	} {
		for _, mode := range frameGateModes {
			t.Run(c.name+"/"+mode.name, func(t *testing.T) {
				r := newSchedRun(t, mode.sync)
				queuedBehind(r, "ROW")
				row := queuedRowOf(t, r.m(), "ROW")
				if row.Version != 0 {
					t.Fatalf("fixture: the row is at version %d", row.Version)
				}
				editRow(r, "MINE")
				if r.m().queueEditVer != row.Version {
					t.Fatalf("the edit began from version %d, want the row's %d", r.m().queueEditVer, row.Version)
				}
				for _, s := range c.steps {
					s(r, row)
				}
			})
		}
	}
}

// TestAnEditsResultIsTheVersionTheEngineCommitted (astra r53 1): a saved
// edit's row shows, until its own event is folded, the version the engine
// committed — the check's answer, not a guess from the band — so an edit of
// the same row begun from it before that fold is checked against the right
// version and saves, rather than being refused by this client's own edit.
func TestAnEditsResultIsTheVersionTheEngineCommitted(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			r := newSchedRun(t, mode.sync)
			queuedBehind(r, "ROW")
			for i, text := range []string{"ONE", "TWO"} {
				editRow(r, text)
				r.key(enter())
				m := r.m()
				if m.queueEdit != "" {
					t.Fatalf("save %d: still editing, note %q", i+1, m.copyNote)
				}
				eq := queuedRows(m)
				if len(eq) != 1 || eq[0].Text != text || eq[0].Version != i+1 {
					t.Fatalf("save %d: the engine's queue is %+v, want %s at version %d", i+1, eq, text, i+1)
				}
				if len(m.queue) != 1 || m.queue[0].Text != eq[0].Text || m.queue[0].Version != eq[0].Version {
					t.Fatalf("save %d: the result shows %+v, want the engine's row %+v", i+1, m.queue, eq[0])
				}
			}
			r.feed()
			if got := r.m().queue; len(got) != 1 || got[0].Text != "TWO" || got[0].Version != 2 {
				t.Fatalf("once folded the band draws %+v, want TWO at version 2", got)
			}
			noOverlays(t, r.m(), "both edits folded")
		})
	}
}

// TestAnEditBegunBeforeARefusedDrainAppliesAfterIt is X1's pin for C22 (plan
// 027, execution amendment X1): a row the drain takes and the agent's own turn
// refuses is put back with its own id and version, so an edit begun before
// that drain — checked against the version it began from — applies after it,
// and the row then runs with the edited text.
func TestAnEditBegunBeforeARefusedDrainAppliesAfterIt(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			r := newSchedRun(t, mode.sync)
			queuedBehind(r, "ROW")
			row := queuedRowOf(t, r.m(), "ROW")
			editRow(r, "MINE")
			// The working turn ends and the drain takes the row; the agent
			// has begun a turn of its own by the time the claim is answered,
			// and refuses it. The agent's turn is up before the refusal, so
			// the restored row's paced recheck finds it running and leaves the
			// row queued until it ends.
			sc := r.sess.Script(&scriptedTurn{claimed: make(chan struct{}), openWhen: make(chan struct{}), refuse: agent.ErrForeignTurn})
			endHeldTurn(t, r.sess)
			awaitBarrier(t, sc.claimed, "the drain claiming the row")
			r.stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "agent-1", Text: "the agent's own", Running: true})
			sc.Open()
			restored := func(evs []agent.Event) bool {
				return slices.ContainsFunc(evs, func(ev agent.Event) bool {
					return ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueQueued && ev.Queue != nil && ev.Queue.ID == row.ID
				})
			}
			// The drain's sent and the restoring queued, not folded here: the
			// edit is still open over the row as this client last saw it.
			evs := r.until(restored)
			if got := queuedRows(r.m()); len(got) != 1 || got[0].ID != row.ID || got[0].Version != row.Version {
				t.Fatalf("fixture: the engine's queue is %+v, want the row restored at version %d", got, row.Version)
			}
			r.key(enter())
			if m := r.m(); m.queueEdit != "" || m.copyNote == staleEditNote {
				t.Fatalf("the save after the restore: editing %q, note %q — want it saved", m.queueEdit, m.copyNote)
			}
			if got := queuedRows(r.m()); len(got) != 1 || got[0].ID != row.ID || got[0].Text != "MINE" || got[0].Version != row.Version+1 {
				t.Fatalf("the engine's queue is %+v, want the restored row edited to MINE at version %d", got, row.Version+1)
			}
			r.stepEvents(evs)
			r.feed()
			if got := r.m().queue; len(got) != 1 || got[0].Text != "MINE" {
				t.Fatalf("once folded the band draws %+v, want MINE", got)
			}
			noOverlays(t, r.m(), "the restore and the edit folded")
			// The agent's turn ends, and the row runs with the edited text.
			r.stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "agent-1", Running: false})
			r.feedUntil(func(evs []agent.Event) bool {
				return slices.ContainsFunc(evs, func(ev agent.Event) bool {
					return ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnStarted && ev.Turn.Text == "MINE"
				})
			})
			assertPrompts(t, r.stub, "go", "ROW", "MINE")
		})
	}
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
// publishing a roster event for it, so the fold carries it from the child's
// own tool events — the live rule, and the live 256-byte cap — until the next
// roster event carries the row whole (SF-54), and the mirror draws the fold's
// row.
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
	if info, _ := liveInfo(m.shared.State().Agents, "c1"); info.Activity != "read_file" {
		t.Fatalf("the fold's row carries %q, want its tool's title", info.Activity)
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
