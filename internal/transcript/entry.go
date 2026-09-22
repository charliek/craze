package transcript

import (
	"fmt"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// EntryID names one entry: the Seq of the event that created it and its index
// among that event's entries. It is comparable and it is not packed (plan 024
// §3.2); the zero value means "none".
//
// An EntryID is unique within a model even for event sequences production
// never produces (execution amendment X2): an event whose Seq is past the
// model's names its entries {ev.Seq, 0..}; an event with Seq 0, or one whose
// Seq the model has already reached — a re-applied event, a unit fixture's
// unsequenced one — names them {0, n} from a counter of the model's own that
// starts at 1. So no index is ever keyed on an id two entries share.
type EntryID struct {
	Seq uint64
	N   uint32
}

// IsZero reports whether id is the zero EntryID, which names no entry.
func (id EntryID) IsZero() bool { return id == EntryID{} }

func (id EntryID) String() string { return fmt.Sprintf("%d.%d", id.Seq, id.N) }

// Kind is what an entry is.
type Kind int

// The kinds of entry. The zero Kind is none of them.
const (
	KindUser Kind = iota + 1
	KindAssistant
	KindThought
	KindTool
	KindNote
	KindPlan
	KindError
)

func (k Kind) String() string {
	switch k {
	case KindUser:
		return "user"
	case KindAssistant:
		return "assistant"
	case KindThought:
		return "thought"
	case KindTool:
		return "tool"
	case KindNote:
		return "note"
	case KindPlan:
		return "plan"
	case KindError:
		return "error"
	}
	return "kind(" + fmt.Sprint(int(k)) + ")"
}

// Entry is one transcript entry. It is IMMUTABLE once it is in a transcript:
// every change the fold makes — a chunk, a closed run, a tool update — builds a
// new Entry with the same ID and stores that pointer instead, so a reader
// holding an *Entry holds a value that never changes (plan 024 §3.4).
type Entry struct {
	ID   EntryID
	Kind Kind
	// Text is a streamed entry's text once its run has closed — at most
	// Bounds.StreamText bytes, the tail, led by "…" when the beginning was
	// dropped (today's rule) — and a user row's, a note's or an error row's
	// whole text (uncapped, today's rule). The open stream entry's Text is
	// empty: its text so far is Transcript.Tail.
	Text string
	// Tool is a tool entry's payload and Plan a plan entry's: the event's own,
	// retained by pointer and never written.
	Tool *agent.ToolEvent
	Plan *agent.PlanEvent
	// Err is an EventError's error value, held and never read by the fold: its
	// text is the reader's to take, outside the model's lock (§3.2). An error
	// row drawn from text — a synthetic ending's, a delta's Detail or IndexErr
	// — carries Text instead.
	Err error
	// At is when the event that created the entry says it happened, and End
	// when its run ended: the last chunk's At while it streams, and for a
	// thought run the At of the event that closed it.
	At, End time.Time
	// Open marks a thought run that is still streaming.
	Open bool
	// Interject marks a user row that came from an interjection.
	Interject bool
	// Streaming marks the transcript's open stream entry — the last entry,
	// whose text lives in the transcript's builder until the run closes.
	Streaming bool
	// Bytes is what this entry accounts for against its transcript's byte
	// budget: its text (for the open entry, the tail it would close with) plus
	// every string its payload carries, or errValueBytes for an error value.
	Bytes int
}

// errValueBytes is what an entry holding an error value accounts for. The fold
// may not call Error() to learn the real length (§3.2), so an error is charged
// a fixed amount — generous for the one-line failures craze's sessions report.
const errValueBytes = 256

// entryBytes is the retained bytes of a closed entry: its text, its payload's
// strings, and errValueBytes for an error value.
func entryBytes(e *Entry) int {
	n := len(e.Text) + toolBytes(e.Tool) + planBytes(e.Plan)
	if e.Err != nil {
		n += errValueBytes
	}
	return n
}

// toolBytes is every string a ToolEvent carries (tools.go's payload caps bound
// them: RawInput, ContentText, the output heads and tails, the diffs' old and
// new texts dominate). TestTheAccountingCountsEveryPayloadString holds this
// against the type by reflection, so a string field added to ToolEvent fails
// until it is counted here.
func toolBytes(t *agent.ToolEvent) int {
	if t == nil {
		return 0
	}
	n := len(t.ID) + len(t.Name) + len(t.Status) + len(t.Kind) + len(t.Title) +
		len(t.ToolName) + len(t.RawInput) + len(t.ContentText)
	for _, l := range t.Locations {
		n += len(l)
	}
	if o := t.Output; o != nil {
		n += len(o.Stdout) + len(o.Stderr) + len(o.Content) + len(o.StdoutHead) + len(o.StderrHead)
	}
	for i := range t.Diffs {
		d := &t.Diffs[i]
		n += len(d.Path) + len(d.OldText) + len(d.NewText)
	}
	if k := t.Task; k != nil {
		n += len(k.Description) + len(k.Prompt) + len(k.Model) + len(k.AgentID) +
			len(k.SubagentType) + len(k.Status)
	}
	return n
}

// planBytes is every string a PlanEvent carries: its body, overview and steps.
func planBytes(p *agent.PlanEvent) int {
	if p == nil {
		return 0
	}
	n := len(p.ID) + len(p.Name) + len(p.Overview) + len(p.Plan)
	for i := range p.Todos {
		td := &p.Todos[i]
		n += len(td.ID) + len(td.Content) + len(td.Status)
	}
	return n
}
