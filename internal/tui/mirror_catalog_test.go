package tui

import (
	"slices"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// catalogDelta is the event a native session's reload reaches a client as:
// the catalog section alone, the whole list and its revision (plan 034 §3.4).
func catalogDelta(seq, revision uint64, models []agent.ModelInfo) agent.Event {
	return agent.Event{Type: agent.EventMeta, Seq: seq, State: &agent.StateDelta{
		Catalog: &agent.CatalogState{Models: models, Revision: revision},
	}}
}

// offeredModelIDs are a model list's ids, in order.
func offeredModelIDs(models []agent.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// TestTheMirrorTakesTheNewerCatalog (plan 034 C4r, r9 #8): in process the
// mirror reads the session's model list two ways — Info, live, and the fold's
// catalog section, in stream order — and shows the newer by revision. A
// session that published revisions 1 and 2 before this client folded the
// first shows revision 2 from Info, and the queued revision-1 delta folded
// after it does not bring back the model revision 2 took away. Once the fold
// reaches revision 2 the two are one list, and a fold ahead of Info
// (revision 3) is shown over it. The control: with no delta folded, Info's
// list is shown. Negative control: a mirror that prefers any folded catalog
// shows revision 1's list once its delta is folded.
func TestTheMirrorTakesTheNewerCatalog(t *testing.T) {
	m := sized(t)
	stub := stubOf(t, m)
	list := func(ids ...string) []agent.ModelInfo {
		out := make([]agent.ModelInfo, 0, len(ids))
		for _, id := range ids {
			out = append(out, agent.ModelInfo{ID: id, Name: id})
		}
		return out
	}
	rev1, rev2, rev3 := list("test/a", "test/gone"), list("test/a", "test/b"), list("test/a", "test/b", "test/c")
	stub.mu.Lock()
	stub.snap.Models, stub.snap.CatalogRevision = rev2, 2
	stub.mu.Unlock()
	m.recompute()
	if got := offeredModelIDs(m.snap.Models); !slices.Equal(got, offeredModelIDs(rev2)) {
		t.Fatalf("control: with nothing folded the list is %v; want Info's %v", got, offeredModelIDs(rev2))
	}
	m = feed(t, m, catalogDelta(101, 1, rev1))
	if got := offeredModelIDs(m.snap.Models); !slices.Equal(got, offeredModelIDs(rev2)) {
		t.Fatalf("after the older revision folded the list is %v; want revision 2's %v", got, offeredModelIDs(rev2))
	}
	m = feed(t, m, catalogDelta(102, 2, rev2))
	if got := offeredModelIDs(m.snap.Models); !slices.Equal(got, offeredModelIDs(rev2)) {
		t.Fatalf("with the fold at Info's revision the list is %v; want %v", got, offeredModelIDs(rev2))
	}
	m = feed(t, m, catalogDelta(103, 3, rev3))
	if got := offeredModelIDs(m.snap.Models); !slices.Equal(got, offeredModelIDs(rev3)) {
		t.Fatalf("with the fold ahead of Info the list is %v; want the fold's %v", got, offeredModelIDs(rev3))
	}
}
