package transcript

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// catalogOf is a catalog section of revision rev offering ids, each named for
// itself.
func catalogOf(rev uint64, ids ...string) *agent.CatalogState {
	c := &agent.CatalogState{Revision: rev}
	for _, id := range ids {
		c.Models = append(c.Models, agent.ModelInfo{ID: id, Name: id})
	}
	return c
}

// catalogDeltaAt is the event a native session's reload reaches a client as:
// the catalog section alone.
func catalogDeltaAt(sec int, c *agent.CatalogState) agent.Event {
	return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Catalog: c}, At: at(sec)}
}

// TestACatalogDeltaFoldsAndRestores (plan 034 §3.4, C5): the catalog section
// folds through the delta table — the model's own copy — and a snapshot
// carries it in its settings and restores it, so a client that attaches
// after a reload folds the list the session offers now. A model no delta gave
// a catalog to — every ACP session's, a native one's before its first reload
// — writes no "catalog" key at all, so no snapshot recorded before the section
// existed moves.
func TestACatalogDeltaFoldsAndRestores(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Model: strp("test/a")}, At: at(1)})
	raw, err := EncodeSnapshot(mustSnapshot(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"catalog"`)) {
		t.Fatalf("a snapshot with no catalog writes the key: %s", raw)
	}

	first := catalogOf(1, "test/a", "test/b")
	foldAll(t, m, true, catalogDeltaAt(2, first))
	want := catalogOf(1, "test/a", "test/b")
	first.Models[0].ID = "written after the fold"
	if got := m.State().Settings.Catalog; !reflect.DeepEqual(got, want) {
		t.Fatalf("the settings' catalog is %+v, want %+v (the model's own copy)", got, want)
	}
	if n := len(m.Main.live()); n != 0 {
		t.Fatalf("a catalog delta draws nothing: %v", facts(m.Main))
	}

	s, b := snapshotOf(t, m, capBudget)
	if !bytes.Contains(b, []byte(`"catalog":{"models":[{"id":"test/a","name":"test/a"},{"id":"test/b","name":"test/b"}],"revision":1}`)) {
		t.Fatalf("the snapshot's settings do not carry the catalog section: %s", b)
	}
	r1, r2 := restoredBoth(t, s, b, Options{})
	for _, r := range []*Model{r1, r2} {
		if got := r.State().Settings.Catalog; !reflect.DeepEqual(got, want) {
			t.Fatalf("restored, the catalog is %+v, want %+v", got, want)
		}
	}
}

// TestAnOlderCatalogNeverReplacesANewerOne (plan 034 A25: reconnect and
// replay never restore an older revision): a model that holds a catalog of
// revision 3 keeps it when a section of revision 2 is folded after it — what a
// replay that hands a fold an older section after a newer one would do — and
// takes revision 3 again (the same list) and revision 4 (a newer one). A
// model restored from a snapshot holds the guard too. Negative control: a
// fold that applies every section it is handed ends this holding revision 2's
// list, with the model revision 3 took away back in it.
func TestAnOlderCatalogNeverReplacesANewerOne(t *testing.T) {
	rev2, rev3, rev4 := catalogOf(2, "test/a", "test/gone"), catalogOf(3, "test/a", "test/b"), catalogOf(4, "test/a", "test/b", "test/c")
	m := New(Options{})
	foldAll(t, m, true, catalogDeltaAt(1, rev3), catalogDeltaAt(2, rev2))
	if got := m.State().Settings.Catalog; !reflect.DeepEqual(got, rev3) {
		t.Fatalf("after revision 2 folded behind revision 3 the catalog is %+v; want revision 3's %+v", got, rev3)
	}
	foldAll(t, m, true, catalogDeltaAt(3, rev3))
	if got := m.State().Settings.Catalog; !reflect.DeepEqual(got, rev3) {
		t.Fatalf("revision 3 again: %+v", got)
	}

	s, b := snapshotOf(t, m, capBudget)
	r1, r2 := restoredBoth(t, s, b, Options{})
	for i, r := range []*Model{m, r1, r2} {
		foldAll(t, r, true, catalogDeltaAt(4, rev2))
		if got := r.State().Settings.Catalog; !reflect.DeepEqual(got, rev3) {
			t.Fatalf("model %d: an older section replaced the catalog: %+v", i, got)
		}
		foldAll(t, r, true, catalogDeltaAt(5, rev4))
		if got := r.State().Settings.Catalog; !reflect.DeepEqual(got, rev4) {
			t.Fatalf("model %d: the newer section was not taken: %+v", i, got)
		}
	}
}
