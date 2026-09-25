package transcript

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/kaptinlin/jsonschema"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
)

// The snapshot codec's twin of internal/protocol's
// TestSchemaCoversEveryWireField (plan 027 §3.11, CodeRabbit 19): the
// codec's wire structs are unexported, so the check that the published
// snapshot.json describes them lives here, beside them.
//
// The Go structs are the decoder's: the encoder writes the header through
// wireHeader and each transcript member by member (appendTranscript,
// appendScalars, appendOmitted), omitting every zero member, so the
// transcript members' presence is stated here rather than read from their
// tags, and what the encoder writes is held to the schema by validating its
// real output: every fixture the codec's own tests build, the completeness
// filler's passes (every field set), and snapshots cut from a folded session,
// whole and windowed.

// snapshotRaw accounts for every leaf payload the snapshot codec carries
// through the event codec's exported wrappers: each is a $ref to the event
// codec's own $def in event.json, never a second description of it.
var snapshotRaw = map[string]string{
	"wireHeader.todos[]":     "event.json#/$defs/todo",
	"wireHeader.queue[]":     "event.json#/$defs/queued",
	"wireAgentRow.info":      "event.json#/$defs/subagent",
	"wireAskBody.permission": "event.json#/$defs/permission",
	"wireAskBody.question":   "event.json#/$defs/question",
	"wireAskBody.plan":       "event.json#/$defs/plan",
	"wireTurn.foreign":       "event.json#/$defs/foreignTurn",
	"wireSettings.config":    "event.json#/$defs/config",
	"wireSettings.commands":  "event.json#/$defs/commands",
	"wireSettings.plugins":   "event.json#/$defs/plugins",
	"wireSettings.sendNow":   "event.json#/$defs/sendNow",
	"wireEntry.tool":         "event.json#/$defs/tool",
	"wireEntry.plan":         "event.json#/$defs/plan",
}

// snapshotPresence is what the encoder writes where the decoder's tags do not
// say: subs only when there are children (EncodeSnapshot), and every
// transcript member only when it is not zero (appendScalars, appendTranscript).
var snapshotPresence = map[string]bool{
	"wireSnapshotIn.subs":          false,
	"wireTranscriptIn.trimmed":     false,
	"wireTranscriptIn.windowed":    false,
	"wireTranscriptIn.dropped":     false,
	"wireTranscriptIn.streamOpen":  false,
	"wireTranscriptIn.tailCut":     false,
	"wireTranscriptIn.todoPlanned": false,
	"wireTranscriptIn.todoDone":    false,
	"wireTranscriptIn.omittedRun":  false,
	"wireTranscriptIn.omitted":     false,
	"wireTranscriptIn.entries":     false,
}

// snapshotSchema is snapshot.json's root, compiled with every $ref resolved
// from the embedded schema set.
func snapshotSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.RegisterLoader("https", protocol.SchemaLoader)
	c.RegisterLoader("http", protocol.SchemaLoader)
	s, err := c.Schema(protocol.SchemaURI(protocol.SchemaSnapshot))
	if err != nil {
		t.Fatalf("compile snapshot.json: %v", err)
	}
	if u := s.UnresolvedReferenceURIs(); len(u) > 0 {
		t.Fatalf("snapshot.json leaves $refs unresolved: %v", u)
	}
	return s
}

// TestSnapshotSchemaCoversTheCodec: the snapshot codec's wire structs
// against snapshot.json, both ways, every $def of snapshot.json describing
// something the codec writes and every leaf a $ref into event.json; and real
// EncodeSnapshot output validates against it.
func TestSnapshotSchemaCoversTheCodec(t *testing.T) {
	if SnapshotVersion != 1 {
		t.Fatalf("snapshot.json describes snapshot codec version 1, and the codec is version %d: publish its schema", SnapshotVersion)
	}
	if protocol.AskItemCap != ItemCap {
		t.Fatalf("protocol.AskItemCap is %d and the snapshot's ItemCap %d: asks.get caps its record at the snapshot's own cap (§3.2)", protocol.AskItemCap, ItemCap)
	}
	if protocol.SnapshotBytesMax < DefaultSnapshotBytes {
		t.Fatalf("the snapshot cap %d is below the default budget %d", protocol.SnapshotBytesMax, DefaultSnapshotBytes)
	}
	c, err := protocol.NewCoverage(protocol.CoverageOptions{Raw: snapshotRaw, Presence: snapshotPresence})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(protocol.SchemaSnapshot+"#", reflect.TypeFor[wireSnapshotIn]())
	for _, p := range c.Problems() {
		t.Error(p)
	}
	for _, def := range c.Unvisited(protocol.SchemaSnapshot) {
		t.Errorf("%s describes nothing the codec writes", def)
	}

	s := snapshotSchema(t)
	valid := func(what string, snap *Snapshot) {
		t.Helper()
		b, err := EncodeSnapshot(snap)
		if err != nil {
			t.Fatalf("%s: EncodeSnapshot: %v", what, err)
		}
		if r := s.Validate(b); !r.IsValid() {
			t.Errorf("%s does not validate against snapshot.json: %v\nbody: %.2000s", what, r.Errors, b)
		}
	}

	for _, tc := range pinnedSnapshots() {
		tc.s.Version = SnapshotVersion
		valid("the pinned "+tc.name, &tc.s)
	}

	// The filler's passes set every field reachable from a Snapshot.
	all := func(int) bool { return true }
	f := &snapFiller{t: t, boolOn: all}
	valid("the distinct pass", filledSnapshot(t, f))
	for _, on := range f.bools {
		valid(fmt.Sprintf("the one-hot pass for the bool drawn %d", on),
			filledSnapshot(t, &snapFiller{t: t, boolOn: func(n int) bool { return n == on }}))
	}
	valid("the distinct pass with errors the codec did not make", filledSnapshot(t, &snapFiller{t: t, boolOn: all, plainErrs: true}))
	zero := filledSnapshot(t, &snapFiller{t: t, zero: true})
	zero.Main.Entries = append(zero.Main.Entries, Entry{Tool: &agent.ToolEvent{}, Plan: &agent.PlanEvent{}, Err: fmt.Errorf("")})
	zero.Turn.Foreign = &agent.ForeignTurnInfo{}
	valid("the zero pass", zero)

	// Snapshots cut from a folded session: whole at every cut, then windowed
	// to its newest entries, so the ledger and the window's members are
	// written by the model's own window.
	evs := sessionScript()
	evs = append(evs, laterScript(len(evs))...)
	for cutAt := 0; cutAt <= len(evs); cutAt += 7 {
		m := New(Options{})
		foldAll(t, m, false, evs[:cutAt]...)
		snap, _ := snapshotOf(t, m, 0)
		valid(fmt.Sprintf("the session cut at %d", cutAt), snap)
	}
	m := New(Options{})
	foldAll(t, m, false, evs...)
	full, _ := snapshotOf(t, m, 1<<40)
	for _, keep := range []int{1, 2, 4} {
		var ks []int
		for _, ts := range transcriptsOf(full) {
			ks = append(ks, min(keep, len(ts.Entries)))
		}
		budget := encodedLen(t, windowedTo(full, ks...))
		snap, _ := snapshotOf(t, m, budget)
		if !snap.Main.Windowed {
			t.Fatalf("a budget of %d bytes did not window the session", budget)
		}
		valid(fmt.Sprintf("the session windowed to %d entries a transcript", keep), snap)
	}
	_, twins := ledgerFixture(t, Options{})
	for i, r := range twins {
		snap, _ := snapshotOf(t, r, 0)
		valid(fmt.Sprintf("a restored model's own ledger (twin %d)", i), snap)
	}
}
