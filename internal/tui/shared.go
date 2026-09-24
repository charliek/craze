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
	in := &foldInputs{}
	m.foldIn = in
	m.shared = transcript.New(sharedOptions(in.now, in.errorText))
}

// sharedOptions is how this client makes a shared model: it stamps an event
// that carries no At with clock — only a unit test's fixture is unstamped — and
// takes an error event's text from errText (X14: the TUI folds outside every
// publishing boundary, on its own goroutine, from the publisher's own error
// values). The parity watch makes its models with it too, handing them the
// instant and the text the TUI's fold was handed, so they differ from m.shared
// in nothing but the events they fold.
func sharedOptions(clock func() time.Time, errText func(error) string) transcript.Options {
	return transcript.Options{Clock: clock, ErrText: errText}
}

// scopeOf is the transcript of sm that the pane of scope shows ("" is main),
// nil for a child sm holds nothing for.
func scopeOf(sm *transcript.Model, scope string) *transcript.Transcript {
	if scope == "" {
		return sm.Main
	}
	return sm.Sub(scope)
}

// foldInputs is what the shared model takes from this client while it folds
// one event, armed by foldEvent before every fold: the clock it stamps an
// unstamped event with, and the text of the error an EventError carries.
//
// The model is shared by every copy of Model and outlives each, and a Model's
// clock is a plain field a test may set on its copy after the model was made
// (lateModel), so the model cannot hold any one copy's clock: each fold is
// handed the folding copy's. It is read at most once per event, so every row
// one event draws is stamped at one instant.
//
// The error's text is read by the caller, once, on the Update goroutine and
// before the fold, and that one reading is the transcript row, m.err and so the
// host status alike: Error is foreign code, which the TUI has always called
// once per event (r17), and it is never called under the model's lock.
type foldInputs struct {
	clock func() time.Time
	at    time.Time
	read  bool
	// errText is the armed event's error text; armed says there is one.
	errText string
	armed   bool
}

// begin arms the inputs for one fold: the folding copy's clock, and the text
// of the error ev carries when it is a main-session EventError (text, already
// read), which is the only error the fold reads.
func (in *foldInputs) begin(clock func() time.Time, ev agent.Event, text string) {
	in.clock, in.at, in.read = clock, time.Time{}, false
	in.armed = errorEvent(ev)
	in.errText = text
}

// now is Model.now, read once per fold.
func (in *foldInputs) now() time.Time {
	if !in.read {
		in.read = true
		if in.clock != nil {
			in.at = in.clock()
		} else {
			in.at = time.Now()
		}
	}
	return in.at
}

// errorText is the model's Options.ErrText: the armed text, read before the
// fold. An error the fold asks for with nothing armed — no path does — is read
// where it stands.
func (in *foldInputs) errorText(err error) string {
	if in.armed {
		return in.errText
	}
	return err.Error()
}

// errorEvent reports whether ev is an error the shared model reads the text
// of: a main-session EventError carrying one (a child's is ignored, §3.3).
func errorEvent(ev agent.Event) bool {
	return ev.Type == agent.EventError && ev.Agent == "" && ev.Err != nil
}

// foldHook, when a test sets it, is called after every fold with the event
// folded, its Change and the kind of appended entry it was folded to hide,
// before the pane consumes the Change; the func it returns is called once the
// Change has been consumed. It is the parity test's window onto every fold
// (A11), which needs both sides of the consumption. It is nil in production.
var foldHook func(m *Model, ev agent.Event, ch transcript.Change, hide transcript.Kind) (consumed func())

// foldEvent folds one event the primary delivered into the shared model, and
// hands what the fold changed to the pane that shows its transcript. Both
// arguments besides the event are decided by the caller from this client's
// own state, before anything moves it:
//
//   - hide is the kind of the entries this fold appends that get no row,
//     because the pane already shows what they say: the started of the turn
//     Submit handed back, whose user row Enter drew (KindUser, X26), and a todo
//     note the pane's own dedupe does not owe (KindNote, X31 revised). Zero is
//     none;
//   - errText is the text of the error an EventError carries, read once
//     (foldInputs).
func (m *Model) foldEvent(ev agent.Event, hide transcript.Kind, errText string) transcript.Change {
	if m.shared == nil {
		m.newShared()
	}
	m.foldIn.begin(m.clock, ev, errText)
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
	if ev.Type == agent.EventSubagent {
		m.detachEvicted()
	}
	if consumed != nil {
		consumed()
	}
	return ch
}

// detachEvicted lets go of the children the shared model no longer holds. A
// roster fold is where the model evicts a finished child past the roster's
// bound, and its transcript with it (§3.2 (b)); a pane this client keeps for
// that child — pruneSubs keeps the one being viewed — would otherwise show
// entries nothing holds. Its rows stay on screen as this client's own, as a
// replaced session's do (pane.detach).
func (m *Model) detachEvicted() {
	for id, p := range m.subs {
		if p != nil && m.shared.Sub(id) == nil {
			p.detach()
		}
	}
}

// ownStarted reports whether ev is the started of the turn Submit handed back
// synchronously (ownTurn): the one whose row, working status and turnStart the
// Update that pressed Enter has already applied (applyTurnStarted).
func (m *Model) ownStarted(ev agent.Event) bool {
	return ev.Type == agent.EventTurn && ev.Agent == "" && ev.Turn != nil &&
		ev.Turn.Phase == agent.TurnStarted && ev.Turn.ID != "" && ev.Turn.ID == m.ownTurn
}
