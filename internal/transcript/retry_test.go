package transcript

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// retryScript is a turn with a finished step, then a step whose attempt thinks,
// says something, starts a call and is steered, and is then retried; the next
// attempt answers. Its events are sequenced from 1; the failed attempt's are 6
// to 10.
func retryScript() []agent.Event {
	evs := sequenced([]agent.Event{
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}, At: at(1)},
		{Type: agent.EventText, Text: "first step", At: at(2)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Name: "bash", Status: "pending", At: at(3)}, At: at(3)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Name: "bash", Status: "completed", At: at(4)}, At: at(4)},
		{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{}}, At: at(5)},
		{Type: agent.EventThought, Text: "abandoned thought", At: at(6)},
		{Type: agent.EventText, Text: "abandoned ", At: at(7)},
		{Type: agent.EventText, Text: "text", At: at(8)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.2.1", Name: "bash", Status: "pending", At: at(9)}, At: at(9)},
		{Type: agent.EventUser, Text: "a steer", Interjection: true, At: at(10)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.2.1", Name: "bash", Status: "failed", At: at(11)}, At: at(11)},
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 2, Attempt: 1, Total: 1, Reason: "INTERNAL_ERROR", DelayMs: 5000, From: 6, Discarded: true}, At: at(12)},
		{Type: agent.EventText, Text: "whole answer", At: at(13)},
	})
	return evs
}

// shown is the entries' kinds and text, a retracted one marked.
func shown(es []Entry) []string {
	var out []string
	for _, e := range es {
		line := e.Kind.String() + ":" + e.Text
		if e.Tool != nil {
			line = e.Kind.String() + ":" + e.Tool.ID
		}
		if e.Retracted {
			line = "retracted " + line
		}
		out = append(out, line)
	}
	return out
}

func entriesOf(t *Transcript) []Entry {
	var out []Entry
	for _, e := range t.Entries() {
		c := *e
		if c.Streaming {
			c.Text = t.Tail()
		}
		out = append(out, c)
	}
	return out
}

// TestARetryRetractsTheAttemptsRows: what the failed attempt drew is marked and
// nothing else is: a steer among its rows stays, the step before it stays, and
// the run it left open is closed, so the next attempt's text is a row of its own
// below the note.
func TestARetryRetractsTheAttemptsRows(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, retryScript()...)
	want := []string{
		"user:go",
		"assistant:first step",
		"tool:t1.1.1",
		"retracted thought:abandoned thought",
		"retracted assistant:abandoned text",
		"retracted tool:t1.2.1",
		"user:a steer",
		"note:" + NoteStepRetried,
		"assistant:whole answer",
	}
	if got := shown(entriesOf(m.Main)); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries:\n got %q\nwant %q", got, want)
	}
	if st := m.State(); st.Tools[ToolKey{ID: "t1.2.1"}] != nil || st.Tools[ToolKey{ID: "t1.1.1"}] == nil {
		t.Fatalf("State's tools = %+v; want the finished call's and not the abandoned one's", st.Tools)
	}
	for _, tool := range m.Tools() {
		if tool.ID == "t1.2.1" {
			t.Fatal("Tools lists the abandoned call")
		}
	}
	if m.Mirror().Turn.Retries != 1 || m.State().Turn.Retries != 1 {
		t.Fatalf("the turn's retries = %d, %d; want 1", m.Mirror().Turn.Retries, m.State().Turn.Retries)
	}
}

// TestARetryOfNothingChangesNoRow: an attempt that had streamed nothing leaves
// the run above it open and draws no note, whatever From says.
func TestARetryOfNothingChangesNoRow(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		{Type: agent.EventText, Text: "above", At: at(1)},
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Step: 1, Attempt: 1, Total: 1, From: 2}, At: at(2)},
		{Type: agent.EventText, Text: " still", At: at(3)},
	})...)
	if got := shown(entriesOf(m.Main)); !reflect.DeepEqual(got, []string{"assistant:above still"}) {
		t.Fatalf("entries = %q", got)
	}
	if m.State().Turn.Retries != 1 {
		t.Fatal("the retry was not counted")
	}
}

// TestAChildsRetryIsItsOwn: a child's attempt is retracted in its transcript, by
// the same rule, a parent's rows from the same Seqs on untouched, and the
// parent's count is not moved.
func TestAChildsRetryIsItsOwn(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true,
		agent.Event{Seq: 1, Type: agent.EventText, Agent: "sub-1", Text: "a finished step", At: at(1)},
		agent.Event{Seq: 2, Type: agent.EventTool, Agent: "sub-1", Tool: &agent.ToolEvent{ID: "c1", Status: "completed"}, At: at(2)},
		agent.Event{Seq: 3, Type: agent.EventText, Text: "the parent's", At: at(3)},
		agent.Event{Seq: 4, Type: agent.EventText, Agent: "sub-1", Text: "abandoned", At: at(4)},
	)
	ch := m.Fold(agent.Event{Seq: 5, Type: agent.EventStepRetry, Agent: "sub-1", StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 1, From: 3, Discarded: true}, At: at(5)})
	if ch.Scope != "sub-1" || ch.RetractFrom != 3 || !ch.Entries() {
		t.Fatalf("Change = %+v", ch)
	}
	want := []string{"assistant:a finished step", "tool:c1", "retracted assistant:abandoned", "note:" + NoteStepRetried}
	if got := shown(entriesOf(m.Sub("sub-1"))); !reflect.DeepEqual(got, want) {
		t.Fatalf("the child's entries = %q; want %q", got, want)
	}
	if got := shown(entriesOf(m.Main)); !reflect.DeepEqual(got, []string{"assistant:the parent's"}) {
		t.Fatalf("the parent's entries = %q", got)
	}
	if m.State().Turn.Retries != 0 {
		t.Fatal("a child's retry moved the turn's count")
	}
}

// TestAReplacementGetsItsOwnRow: a row is keyed by the harness's id, new for each
// attempt whatever the provider called the replacement, so the retracted row and
// the replacement's stand apart.
func TestAReplacementGetsItsOwnRow(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Status: "failed"}, At: at(1)},
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 1, From: 1, Discarded: true}, At: at(2)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.2", Status: "completed"}, At: at(3)},
	})...)
	want := []string{"retracted tool:t1.1.1", "note:" + NoteStepRetried, "tool:t1.1.2"}
	if got := shown(entriesOf(m.Main)); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %q; want %q", got, want)
	}
}

// TestASnapshotMidAttemptDoesNotResurrectIt is the property snapshots owe a
// retry: cut anywhere — before the attempt, in the middle of it with its run
// open, after the retry — through the codec, a restored model that folds the rest
// holds exactly the first model's rows under the same ids, marks and count, and
// the Progress that reads the answer finds the same.
func TestASnapshotMidAttemptDoesNotResurrectIt(t *testing.T) {
	script := retryScript()
	first := New(Options{})
	foldAll(t, first, true, script...)
	want := entriesOf(first.Main)
	for cut := range len(script) + 1 {
		m := New(Options{})
		foldAll(t, m, false, script[:cut]...)
		snap, err := m.Snapshot(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := EncodeSnapshot(snap)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := DecodeSnapshot(raw)
		if err != nil {
			t.Fatal(err)
		}
		r := Restore(dec, Options{})
		foldAll(t, r, true, script[cut:]...)
		got := entriesOf(r.Main)
		if len(got) != len(want) {
			t.Fatalf("cut at %d: entries %q; want %q", cut, shown(got), shown(want))
		}
		for i := range got {
			if got[i].ID != want[i].ID || got[i].Retracted != want[i].Retracted || got[i].Text != want[i].Text || got[i].Bytes != want[i].Bytes {
				t.Fatalf("cut at %d, entry %d: %+v; want %+v", cut, i, got[i], want[i])
			}
		}
		if r.State().Turn.Retries != 1 {
			t.Fatalf("cut at %d: retries %d", cut, r.State().Turn.Retries)
		}
		if p, q := r.Progress(), first.Progress(); p != q {
			t.Fatalf("cut at %d: Progress %+v; want %+v", cut, p, q)
		}
	}
}

// TestARetractionThroughAWindow: a snapshot whose window leaves out the attempt's
// oldest rows folds the retraction just the same: the rows it holds are marked,
// and its accounting stays the first model's.
func TestARetractionThroughAWindow(t *testing.T) {
	script := sequenced([]agent.Event{
		{Type: agent.EventText, Text: strings.Repeat("o", 4<<10), At: at(1)},
		{Type: agent.EventThought, Text: strings.Repeat("a", 8<<10), At: at(2)},
		{Type: agent.EventText, Text: "abandoned", At: at(3)},
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 1, From: 2, Discarded: true}, At: at(4)},
	})
	first := New(Options{})
	foldAll(t, first, true, script...)
	m := New(Options{})
	foldAll(t, m, false, script[:3]...)
	snap, err := m.Snapshot(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Main.Entries) >= 3 {
		t.Fatalf("the window kept %d entries: it omits nothing", len(snap.Main.Entries))
	}
	r := Restore(snap, Options{})
	foldAll(t, r, true, script[3:]...)
	if got, want := r.Main.Bytes(), first.Main.Bytes(); got != want {
		t.Fatalf("the restored model accounts %d; the first %d", got, want)
	}
	for _, e := range entriesOf(r.Main) {
		if e.Kind == KindAssistant && !e.Retracted {
			t.Fatalf("the attempt's row was not marked: %+v", e)
		}
	}
}

// TestProgressReadsNoRetractedRow: a list's "last reply" is the answer's, and a
// retracted call is not running.
func TestProgressReadsNoRetractedRow(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		{Type: agent.EventText, Text: "the answer", At: at(1)},
		{Type: agent.EventThought, Text: "and", At: at(2)},
		{Type: agent.EventText, Text: "abandoned", At: at(3)},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", Name: "bash", Status: "pending"}, At: at(4)},
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 1, From: 2, Discarded: true}, At: at(5)},
	})...)
	if p := m.Progress(); p.LastReply != "the answer" || p.Tool != "" || p.Responding {
		t.Fatalf("Progress = %+v", p)
	}
}

// TestAStartedTurnStartsItsCount.
func TestAStartedTurnStartsItsCount(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, sequenced([]agent.Event{
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 2, From: 1}, At: at(1)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn"}, At: at(2)},
	})...)
	if m.State().Turn.Retries != 2 {
		t.Fatal("an ended turn lost its count")
	}
	foldAll(t, m, true, sequencedAt(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-2", Phase: agent.TurnStarted, Text: "x"}, At: at(3)}, 3))
	if m.State().Turn.Retries != 0 {
		t.Fatal("a new turn kept the last one's count")
	}
}
