package transcript

import (
	"slices"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// checkInvariants recounts the model from scratch and holds every incremental
// structure against the recount: each transcript's byte counter against the
// sum of its entries' Bytes, each entry's Bytes against what it retains, the
// EntryID → ordinal map against the deque, the tool index against the tool
// entries, the open run against the builder, the bounds, and the roster and
// child maps against their orders. The bounds tests call it after every fold.
func checkInvariants(t testing.TB, m *Model) {
	t.Helper()
	checkTranscript(t, m.Main)
	if len(m.subs) != len(m.subOrder) {
		t.Fatalf("%d child transcripts, %d in their order", len(m.subs), len(m.subOrder))
	}
	for _, id := range m.subOrder {
		tr := m.subs[id]
		if tr == nil || tr.agent != id {
			t.Fatalf("child %q is in the order but not the map (or under another id)", id)
		}
		checkTranscript(t, tr)
	}
	if len(m.agents) != len(m.agentOrder) {
		t.Fatalf("%d roster rows, %d in their order", len(m.agents), len(m.agentOrder))
	}
	finished := 0
	for _, id := range m.agentOrder {
		row, ok := m.agents[id]
		if !ok || row.info.ID != id {
			t.Fatalf("roster row %q is in the order but not the map", id)
		}
		if s := row.info.Status; s != agent.SubagentRunning && s != "" {
			finished++
		}
	}
	if finished > m.bounds.Agents {
		t.Fatalf("%d finished roster rows, the bound is %d", finished, m.bounds.Agents)
	}
}

func checkTranscript(t testing.TB, tr *Transcript) {
	t.Helper()
	live := tr.live()
	sum := 0
	seen := make(map[EntryID]bool, len(live))
	for i, e := range live {
		where := tr.agent + "#" + e.ID.String()
		if e.ID.IsZero() {
			t.Fatalf("%s: an entry has the zero id", where)
		}
		if seen[e.ID] {
			t.Fatalf("%s: two entries share an id", where)
		}
		seen[e.ID] = true
		if ord, ok := tr.slot[e.ID]; !ok || ord != tr.base+tr.head+i {
			t.Fatalf("%s: the slot map says %d (%v), the deque %d", where, ord, ok, tr.base+tr.head+i)
		}
		want := entryBytes(e)
		if e.Streaming {
			if i != len(live)-1 || !tr.streamOpen {
				t.Fatalf("%s: a streaming entry that is not the open run's last entry", where)
			}
			if e.Text != "" {
				t.Fatalf("%s: the open entry carries text %q; it lives in the builder", where, e.Text)
			}
			want = tr.tailLen() + toolBytes(e.Tool) + planBytes(e.Plan)
		}
		if e.Bytes != want {
			t.Fatalf("%s: accounts %d bytes, retains %d", where, e.Bytes, want)
		}
		sum += e.Bytes
		if e.Kind == KindTool && e.Tool != nil && e.Tool.ID != "" {
			if id, ok := tr.tools[e.Tool.ID]; !ok || id != e.ID {
				t.Fatalf("%s: tool %q is held but the index names %v (%v)", where, e.Tool.ID, id, ok)
			}
		}
	}
	if len(tr.slot) != len(live) {
		t.Fatalf("%s: the slot map has %d ids for %d entries", tr.agent, len(tr.slot), len(live))
	}
	if sum != tr.bytes {
		t.Fatalf("%s: the counter says %d bytes, a recount %d", tr.agent, tr.bytes, sum)
	}
	for id, eid := range tr.tools {
		_, e := tr.lookup(eid)
		if e == nil || e.Kind != KindTool || e.Tool == nil || e.Tool.ID != id {
			t.Fatalf("%s: the tool index names %q → %v, which is not that tool's entry", tr.agent, id, eid)
		}
	}
	if open := len(live) > 0 && live[len(live)-1].Streaming; tr.streamOpen != open {
		t.Fatalf("%s: streamOpen %v, but the last entry streaming is %v", tr.agent, tr.streamOpen, open)
	}
	if !tr.streamOpen && (len(tr.buf) > 0 || tr.bufCut) {
		t.Fatalf("%s: no run is open but the builder holds %d bytes", tr.agent, len(tr.buf))
	}
	if len(tr.buf) > 2*tr.streamCap {
		t.Fatalf("%s: the builder holds %d bytes, past 2 × %d", tr.agent, len(tr.buf), tr.streamCap)
	}
	if len(live) > tr.maxEntries {
		t.Fatalf("%s: %d entries, the bound is %d", tr.agent, len(live), tr.maxEntries)
	}
	if tr.bytes > tr.maxBytes && len(live) > 1 {
		t.Fatalf("%s: %d bytes over %d entries, the bound is %d", tr.agent, tr.bytes, len(live), tr.maxBytes)
	}
	if slices.ContainsFunc(tr.ents[:tr.head], func(e *Entry) bool { return e != nil }) {
		t.Fatalf("%s: a trimmed slot still holds its entry", tr.agent)
	}
}

// sequenced numbers events 1..n, as the log's commit order does.
func sequenced(evs []agent.Event) []agent.Event {
	out := slices.Clone(evs)
	for i := range out {
		out[i].Seq = uint64(i + 1)
	}
	return out
}

// foldAll folds every event, checking the invariants after each when check is
// set.
func foldAll(t testing.TB, m *Model, check bool, evs ...agent.Event) {
	t.Helper()
	for _, ev := range evs {
		m.Fold(ev)
		if check {
			checkInvariants(t, m)
		}
	}
}
