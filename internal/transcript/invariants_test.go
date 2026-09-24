package transcript

import (
	"slices"
	"testing"
	"unicode/utf8"

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
		// The open entry's stored Bytes is its opening's; what it accounts
		// now is on the transcript (X24), as a reader's copy carries it.
		if got := tr.current(e).Bytes; got != want {
			t.Fatalf("%s: accounts %d bytes, retains %d", where, got, want)
		}
		sum += tr.bytesOf(e)
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
	// A restored window can have dropped the open run's entry (omittedRun):
	// the run is open with no entry of its own, in a transcript that has
	// drawn nothing since.
	if open := len(live) > 0 && live[len(live)-1].Streaming; tr.streamOpen != (open || tr.omittedRun != 0) {
		t.Fatalf("%s: streamOpen %v, but the last entry streaming is %v (omitted run %v)", tr.agent, tr.streamOpen, open, tr.omittedRun)
	}
	if tr.omittedRun != 0 && (!tr.streamOpen || len(live) > 0) {
		t.Fatalf("%s: an omitted %v run beside %d entries (open %v)", tr.agent, tr.omittedRun, len(live), tr.streamOpen)
	}
	for tid := range tr.omitted {
		if _, held := tr.tools[tid]; held {
			t.Fatalf("%s: tool %q is both held and omitted", tr.agent, tid)
		}
	}
	if !tr.streamOpen && (len(tr.buf) > 0 || tr.bufCut || tr.tailAt != 0) {
		t.Fatalf("%s: no run is open but the builder holds %d bytes (cut at %d)", tr.agent, len(tr.buf), tr.tailAt)
	}
	// The kept cut against a scan from scratch: capText's, taken on buf.
	if tr.streamOpen && tr.cutRun() {
		start := max(len(tr.buf)-(tr.streamCap-len(ellipsis)), 0)
		for start < len(tr.buf) && !utf8.RuneStart(tr.buf[start]) {
			start++
		}
		if tr.tailAt != start {
			t.Fatalf("%s: the kept cut is at %d, a scan finds %d", tr.agent, tr.tailAt, start)
		}
	}
	if len(tr.buf) > 2*tr.streamCap {
		t.Fatalf("%s: the builder holds %d bytes, past 2 × %d", tr.agent, len(tr.buf), tr.streamCap)
	}
	if len(live) > tr.maxEntries {
		t.Fatalf("%s: %d entries, the bound is %d", tr.agent, len(live), tr.maxEntries)
	}
	// The budget holds but for what tool updates in place have added since it
	// was last enforced: an update in place never trims (upsertTool).
	if tr.bytes-tr.grown > tr.maxBytes && len(live) > 1 {
		t.Fatalf("%s: %d bytes (%d of them grown in place) over %d entries, the bound is %d", tr.agent, tr.bytes, tr.grown, len(live), tr.maxBytes)
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
