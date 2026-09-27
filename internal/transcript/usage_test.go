package transcript

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestAUsageDeltaFoldsIntoTheSettings (plan 028 §3.14, seam 8; A32): the usage
// section folds through the delta table like every section — a full
// replacement, the model's own copy, untouched by a delta without it, a
// child's ignored — and a snapshot carries it and restores it, so a client
// that re-derives its mirror from the fold (S2's) reads what the session
// reported. A model no delta gave usage to — every ACP session's — writes no
// "usage" key at all.
func TestAUsageDeltaFoldsIntoTheSettings(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Model: strp("test/a")}, At: at(1)})
	if u := m.State().Settings.Usage; u != nil {
		t.Fatalf("no delta carried usage and the settings have %+v", u)
	}
	raw, err := EncodeSnapshot(mustSnapshot(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"usage"`)) {
		t.Fatalf("a snapshot with no usage writes the key: %s", raw)
	}

	first := &agent.UsageState{ContextTokens: 12_300, ContextWindow: 100_000,
		Turn: agent.Spend{Input: 10, Output: 5, CostPicoUSD: 20_000_000}, Session: agent.Spend{Input: 10, Output: 5, CostPicoUSD: 20_000_000}}
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: first}, At: at(2)})
	want := *first
	first.Session.Input = 1 << 40 // the event's value, written after the fold
	if got := m.State().Settings.Usage; got == nil || *got != want {
		t.Fatalf("the settings' usage is %+v, want %+v (the model's own copy)", got, want)
	}
	if n := len(m.Main.live()); n != 0 {
		t.Fatalf("a usage delta draws nothing: %v", facts(m.Main))
	}

	// A delta without the section leaves it; a child's is ignored; the next
	// report replaces it whole.
	foldAll(t, m, true,
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Title: strp("hello")}, At: at(3)},
		agent.Event{Type: agent.EventMeta, Agent: "sub", State: &agent.StateDelta{Usage: &agent.UsageState{ContextTokens: 1}}, At: at(4)},
	)
	if got := m.State().Settings.Usage; got == nil || *got != want {
		t.Fatalf("a delta without usage, or a child's, moved it: %+v", got)
	}
	second := agent.UsageState{ContextTokens: 24_600, ContextWindow: 100_000,
		Turn: agent.Spend{Input: 10, Output: 5, Unpriced: true}, Session: agent.Spend{Input: 20, Output: 10, CostPicoUSD: 20_000_000, Unpriced: true}}
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &second}, At: at(5)})
	if got := m.State().Settings.Usage; got == nil || *got != second {
		t.Fatalf("the settings' usage is %+v, want the later report %+v", got, second)
	}

	// Through the snapshot codec and back into a model.
	snap := mustSnapshot(t, m)
	raw, err = EncodeSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"usage":{"contextTokens":24600,`)) {
		t.Fatalf("the snapshot does not carry the usage section: %s", raw)
	}
	back, err := DecodeSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored := Restore(back, Options{})
	if got, want := restored.State().Settings, m.State().Settings; !reflect.DeepEqual(got, want) {
		t.Fatalf("restored settings %+v, want %+v", got, want)
	}
	back.Settings.Usage.Session.Input = 1 << 40 // the snapshot's value, written after the restore
	if got := restored.State().Settings.Usage; got == nil || *got != second {
		t.Fatalf("the restored model shares the snapshot's usage: %+v", got)
	}
}

// TestProjectionsNeverShareTheUsage (astra r1-c17): every projection the
// model hands out — State, Snapshot, SnapshotFor — carries a usage section of
// its own, as the fold keeps the delta's copy on the way in: writing through
// one projection's Usage moves no other projection and not the model, whose
// next State and Snapshot still read what the session reported.
func TestProjectionsNeverShareTheUsage(t *testing.T) {
	m := New(Options{})
	want := agent.UsageState{ContextTokens: 12_300, ContextWindow: 100_000,
		Turn: agent.Spend{Input: 10, Output: 5, CostPicoUSD: 20_000_000}, Session: agent.Spend{Input: 30, Output: 15, CostPicoUSD: 60_000_000}}
	u := want
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &u}, At: at(1)})

	a, b := mustSnapshot(t, m), mustSnapshot(t, m)
	f, err := m.SnapshotFor("", 0)
	if err != nil {
		t.Fatal(err)
	}
	st := m.State()
	for name, got := range map[string]*agent.UsageState{"snapshot a": a.Settings.Usage, "snapshot b": b.Settings.Usage,
		"SnapshotFor": f.Settings.Usage, "State": st.Settings.Usage} {
		if got == nil || *got != want {
			t.Fatalf("control: %s's usage is %+v, want %+v", name, got, want)
		}
	}

	a.Settings.Usage.Session.Input = 999   // one snapshot's copy, written after the cut
	st.Settings.Usage.Turn.CostPicoUSD = 1 // and one State's
	for name, got := range map[string]*agent.UsageState{"snapshot b": b.Settings.Usage, "SnapshotFor": f.Settings.Usage,
		"the model's next State": m.State().Settings.Usage, "the model's next Snapshot": mustSnapshot(t, m).Settings.Usage} {
		if got == nil || *got != want {
			t.Errorf("%s's usage moved to %+v when another projection's was written; want %+v", name, got, want)
		}
	}
	if a.Settings.Usage.Turn.CostPicoUSD != want.Turn.CostPicoUSD {
		t.Errorf("snapshot a shares State's usage: %+v", a.Settings.Usage)
	}
}

// mustSnapshot is m's snapshot at the default budget.
func mustSnapshot(t *testing.T, m *Model) *Snapshot {
	t.Helper()
	s, err := m.Snapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
