package tui

import (
	"fmt"
	"time"
)

// These helpers are the vocabulary the transcript's fact tests assert in (plan
// 024 §3.8, T1a), read off the rows a pane displays: each row's kind, text,
// open run and span. T1a wrote the model-fact tests in it, and C1 carried them
// into internal/transcript under the same names, where they are asserted now
// (A13); what still asserts in it here is the clock rule (clock_test.go), which
// drives the TUI's whole pipeline — the event, the shared model's fold, the
// pane's rows — end to end.
//
// A test takes its pane once, as tr := m.main right after sized. That is the
// pane every copy of the model shares (see pane), so tr keeps reading the
// current model through every m = tm.(Model) that follows; a test that starts
// over with m = sized(t) is a new model, and takes its pane again.

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

// factKind names an entry's kind the way fact.Kind spells it. The shell kind
// is the TUI's own (plan 022 §3.6) and never reaches the shared model.
func factKind(k entryKind) string {
	switch k {
	case entryUser:
		return "user"
	case entryAssistant:
		return "assistant"
	case entryThought:
		return "thought"
	case entryTool:
		return "tool"
	case entryNote:
		return "note"
	case entryPlan:
		return "plan"
	case entryError:
		return "error"
	case entryShell:
		return "shell"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

func factOf(e *entry) fact {
	f := fact{
		Kind:      factKind(e.kind),
		Text:      e.text,
		Open:      e.open,
		Interject: e.interject,
		At:        e.at,
		End:       e.end,
	}
	if e.kind == entryTool && e.tool != nil {
		f.ToolID = e.tool.ID
	}
	return f
}

// facts is every row the pane displays, oldest first.
func facts(tr *pane) []fact {
	entries := tr.entries()
	out := make([]fact, 0, len(entries))
	for i := range entries {
		out = append(out, factOf(&entries[i]))
	}
	return out
}

// factsOf is the rows of one kind, oldest first.
func factsOf(tr *pane, kind string) []fact {
	var out []fact
	for _, f := range facts(tr) {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// streamOpen reports whether the pane shows a run still open: its last row is
// the shared model's open stream entry, which the next chunk of its kind grows.
func streamOpen(tr *pane) bool { return tr.streamOpen() }
