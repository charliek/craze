package transcript

import (
	"fmt"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// These helpers are the vocabulary the transcript's model-fact tests assert in
// (plan 024 §3.8, T1a): internal/tui/model_facts_test.go's, re-declared over
// this package's Transcript so the tests T1a split carry their assertion lines
// here byte for byte (transcript_test.go, clock_test.go). fact, its String,
// factsOf and factTexts are copied verbatim; facts, factOf, trimmed,
// streamOpen and toolIndexed read this package's storage. Only the setup
// lines, which build and drive a model, differ between the two packages.
//
// They read the transcript's fields directly, without the model's lock: a
// test drives its model from one goroutine.

// fact is one transcript entry as the render-free model will hold it.
type fact struct {
	Kind      string // "user", "assistant", "thought", "tool", "note", "plan", "error"
	Text      string
	Open      bool
	Interject bool
	At, End   time.Time
	ToolID    string // the tool's id for a tool entry, else ""
}

// String keeps a failure message readable: a time.Time prints its monotonic
// reading, and the span is what a test cares about.
func (f fact) String() string {
	s := fmt.Sprintf("{%s %q", f.Kind, f.Text)
	if f.Open {
		s += " open"
	}
	if f.Interject {
		s += " interject"
	}
	if d := f.End.Sub(f.At); d != 0 {
		s += " span " + d.String()
	}
	if f.ToolID != "" {
		s += " tool " + f.ToolID
	}
	return s + "}"
}

// factOf is one entry as a fact. The open stream entry's end and text are its
// transcript's — the run's last stamp and the builder's tail (X24) — and an
// error value's is what a reader would draw: its Error(), read here, outside
// the fold.
func factOf(tr *Transcript, e *Entry) fact {
	e = tr.current(e) // the open entry's end is the transcript's (X24)
	f := fact{
		Kind:      e.Kind.String(),
		Text:      e.Text,
		Open:      e.Open,
		Interject: e.Interject,
		At:        e.At,
		End:       e.End,
	}
	if e.Streaming {
		f.Text = tr.tail()
	}
	if e.Err != nil && f.Text == "" {
		f.Text = e.Err.Error()
	}
	if e.Kind == KindTool && e.Tool != nil {
		f.ToolID = e.Tool.ID
	}
	return f
}

// facts is every entry the transcript holds, oldest first: the deque, head to
// tail.
func facts(tr *Transcript) []fact {
	out := make([]fact, 0, tr.len())
	for _, e := range tr.live() {
		out = append(out, factOf(tr, e))
	}
	return out
}

// factsOf is the entries of one kind, oldest first.
func factsOf(tr *Transcript, kind string) []fact {
	var out []fact
	for _, f := range facts(tr) {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// factTexts is the text of each entry of one kind, oldest first: texts(m, kind)
// over a transcript instead of a Model.
func factTexts(tr *Transcript, kind string) []string {
	var out []string
	for _, f := range factsOf(tr, kind) {
		out = append(out, f.Text)
	}
	return out
}

// trimmed reports whether the transcript has dropped entries off its front.
func trimmed(tr *Transcript) bool { return tr.trimmed }

// streamOpen reports whether a run is open: the next chunk of the last entry's
// kind grows that entry instead of starting one.
func streamOpen(tr *Transcript) bool { return tr.streamOpen }

// toolIndexed is the tool index's lookup. ok reports whether the index holds id
// at all, and the fact is the entry it names — the zero fact when that name
// dangles, so a stale index entry still fails a test that expected it gone.
func toolIndexed(tr *Transcript, id string) (fact, bool) {
	eid, ok := tr.tools[id]
	if !ok {
		return fact{}, false
	}
	_, e := tr.lookup(eid)
	if e == nil {
		return fact{}, true
	}
	return factOf(tr, e), true
}

// ------------------------------------------------------------ setup helpers

// toolEvent folds one tool event into the main transcript: the TUI's helper of
// the same name, over a model that is a pointer, so nothing is handed back.
func toolEvent(m *Model, t *agent.ToolEvent) {
	m.Fold(agent.Event{Type: agent.EventTool, Tool: t})
}

// feed folds each event in turn, as the TUI's feed hands each to Update.
func feed(t *testing.T, m *Model, evs ...agent.Event) {
	t.Helper()
	for _, ev := range evs {
		m.Fold(ev)
	}
}
