package tui

import (
	"fmt"
	"time"
)

// These helpers are the vocabulary the transcript's model-fact tests assert in
// (plan 024 §3.8, T1a). Those tests pin what a transcript holds — its entries'
// kinds, texts, open runs and spans, whether it trimmed, the tool index — and
// not what a frame shows, which their …Renders companions pin. C1 carries them
// into internal/transcript under the same names: every assertion line goes
// through these helpers only, so C1 copies the lines byte for byte and
// re-declares the helpers over the package's Transcript. Only the setup lines,
// which build and drive a transcript, differ between the two packages.
//
// A test takes its transcript once, as tr := m.main right after sized. That is
// the pane every copy of the model shares (see pane), so tr keeps reading the
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

// facts is every entry the transcript holds, oldest first.
func facts(tr *pane) []fact {
	entries := tr.entries()
	out := make([]fact, 0, len(entries))
	for i := range entries {
		out = append(out, factOf(&entries[i]))
	}
	return out
}

// factsOf is the entries of one kind, oldest first.
func factsOf(tr *pane, kind string) []fact {
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
func factTexts(tr *pane, kind string) []string {
	var out []string
	for _, f := range factsOf(tr, kind) {
		out = append(out, f.Text)
	}
	return out
}

// trimmed reports whether the transcript has dropped entries off its front.
func trimmed(tr *pane) bool { return tr.trimmed }

// streamOpen reports whether a run is open: the next chunk of the last entry's
// kind grows that entry instead of starting one.
func streamOpen(tr *pane) bool { return tr.streamOpen }

// toolIndexed is the tool index's lookup. ok reports whether the index holds id
// at all, and the fact is the entry it names — the zero fact when that name
// dangles, so a stale index entry still fails a test that expected it gone.
func toolIndexed(tr *pane, id string) (fact, bool) {
	idx, ok := tr.toolLine[id]
	if !ok {
		return fact{}, false
	}
	entries := tr.entries()
	if idx < 0 || idx >= len(entries) {
		return fact{}, true
	}
	return factOf(&entries[idx]), true
}
