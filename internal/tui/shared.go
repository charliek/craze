package tui

import (
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// The TUI is the shared transcript model's first client (plan 024 §3.8): it
// folds its own transcript.Model (m.shared) from every event its primary
// delivers, and every shared row of every pane shows one of that model's
// entries. The model is the session's facts — what every client folding the
// same events agrees on — and the panes are what this client draws of them,
// beside the rows only it writes (pane).

// newShared gives the model a fresh shared transcript, for a session that is
// starting: its entries are that session's events' alone. A model it replaces
// lets go of its rows (pane.detach), which stay on screen as this client's own.
func (m *Model) newShared() {
	if m.shared != nil {
		if m.main != nil {
			m.main.detach()
		}
		for _, p := range m.subs {
			p.detach()
		}
	}
	fc := &foldClock{}
	m.foldClock = fc
	m.shared = transcript.New(sharedOptions(fc.now))
}

// sharedOptions is how this client makes a shared model: it stamps an event
// that carries no At with clock — only a unit test's fixture is unstamped — and
// reads an error event's text by calling Error, which it may: the TUI folds
// outside every publishing boundary, on its own goroutine, from the
// publisher's own error values (X14). The parity watch makes its models with
// it too, so they differ from m.shared in nothing but the events they fold.
func sharedOptions(clock func() time.Time) transcript.Options {
	return transcript.Options{
		Clock:   clock,
		ErrText: func(err error) string { return err.Error() },
	}
}

// scopeOf is the transcript of sm that the pane of scope shows ("" is main),
// nil for a child sm holds nothing for.
func scopeOf(sm *transcript.Model, scope string) *transcript.Transcript {
	if scope == "" {
		return sm.Main
	}
	return sm.Sub(scope)
}

// foldClock is the clock the shared model stamps an unstamped event with. The
// model is shared by every copy of Model and outlives each, and a Model's clock
// is a plain field a test may set on its copy after the model was made
// (lateModel), so the model cannot hold any one copy's clock: foldEvent hands
// it the folding copy's before every fold. It is read at most once per event,
// so every row one event draws is stamped at one instant.
type foldClock struct {
	clock func() time.Time
	at    time.Time
	read  bool
}

// begin arms the clock for one fold with the folding copy's clock.
func (c *foldClock) begin(clock func() time.Time) {
	c.clock, c.at, c.read = clock, time.Time{}, false
}

// now is Model.now, read once per fold.
func (c *foldClock) now() time.Time {
	if !c.read {
		c.read = true
		if c.clock != nil {
			c.at = c.clock()
		} else {
			c.at = time.Now()
		}
	}
	return c.at
}

// foldHook, when a test sets it, is called after every fold with the event
// folded, its Change and the echo decision it was folded with, before the pane
// consumes the Change; the func it returns is called once the Change has been
// consumed. It is the parity test's window onto every fold (A11), which needs
// both sides of the consumption. It is nil in production.
var foldHook func(m *Model, ev agent.Event, ch transcript.Change, hide bool) (consumed func())

// foldEvent folds one event the primary delivered into the shared model, and
// hands what the fold changed to the pane that shows its transcript. hide is
// the echo rule, decided from this client's state before anything moves it:
// the event is the started of the turn Submit handed back (ownTurn), whose
// user row Enter already drew, so the entry it appends gets no row (X26).
func (m *Model) foldEvent(ev agent.Event, hide bool) transcript.Change {
	if m.shared == nil {
		m.newShared()
	}
	m.foldClock.begin(m.clock)
	ch := m.shared.Fold(ev)
	var consumed func()
	if foldHook != nil {
		consumed = foldHook(m, ev, ch, hide)
	}
	if ch.Entries() {
		tr := scopeOf(m.shared, ch.Scope)
		// ensureSub makes the pane of a child whose pane was pruned, as a
		// child's event always has.
		if p := m.ensureSub(ch.Scope); p != nil && tr != nil {
			p.consume(ch, tr, hide)
		}
	}
	if consumed != nil {
		consumed()
	}
	return ch
}

// ownStarted reports whether ev is the started of the turn Submit handed back
// synchronously (ownTurn): the one whose row, working status and turnStart the
// Update that pressed Enter has already applied (applyTurnStarted).
func (m *Model) ownStarted(ev agent.Event) bool {
	return ev.Type == agent.EventTurn && ev.Agent == "" && ev.Turn != nil &&
		ev.Turn.Phase == agent.TurnStarted && ev.Turn.ID != "" && ev.Turn.ID == m.ownTurn
}
