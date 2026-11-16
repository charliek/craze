package transcript

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestAWebStatusDeltaFoldsIntoTheSettings (issues #137, #141): the web status
// section folds through the delta table as the usage section does — a full
// replacement, never an addition, the model's own copy, session-wide (stamped
// with no turn, so it outlives the turn it arrived in), untouched by a delta
// without it, a child's ignored — and a snapshot carries it and restores it.
// A model no delta gave one to — every ACP session's — writes no "webStatus"
// key at all.
func TestAWebStatusDeltaFoldsIntoTheSettings(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Model: strp("test/a")}, At: at(1)})
	if w := m.State().Settings.WebStatus; w != nil {
		t.Fatalf("no delta carried a web status and the settings have %+v", w)
	}
	raw, err := EncodeSnapshot(mustSnapshot(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"webStatus"`)) {
		t.Fatalf("a snapshot with no web status writes the key: %s", raw)
	}

	first := &agent.WebStatusState{Provider: "exa", Key: "keyless", Consent: "allowed", Searches: 2, Fetches: 1}
	foldAll(t, m, true,
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}, At: at(2)},
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{WebStatus: first}, At: at(3)},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn"}, At: at(4)},
	)
	want := *first
	first.Searches = 1 << 30 // the event's value, written after the fold
	if got := m.State().Settings.WebStatus; got == nil || *got != want {
		t.Fatalf("the settings' web status is %+v, want %+v (the model's own copy, kept past the turn)", got, want)
	}

	// A delta without the section leaves it; a child's is ignored; the next
	// replaces it whole — its totals are the whole of them, never added.
	foldAll(t, m, true,
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Title: strp("hello")}, At: at(5)},
		agent.Event{Type: agent.EventMeta, Agent: "sub", State: &agent.StateDelta{WebStatus: &agent.WebStatusState{Searches: 7}}, At: at(6)},
	)
	if got := m.State().Settings.WebStatus; got == nil || *got != want {
		t.Fatalf("a delta without a web status, or a child's, moved it: %+v", got)
	}
	second := agent.WebStatusState{Provider: "exa", Key: "stored", Consent: "allowed for this session",
		Searches: 3, Fetches: 1, Failed: 1, RateLimited: 1, Retries: 2}
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{WebStatus: &second}, At: at(7)})
	if got := m.State().Settings.WebStatus; got == nil || *got != second {
		t.Fatalf("the settings' web status is %+v, want the later one %+v (replaced, not added)", got, second)
	}

	// Through the snapshot codec and back into a model.
	snap := mustSnapshot(t, m)
	raw, err = EncodeSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"webStatus":{"provider":"exa","key":"stored",`)) {
		t.Fatalf("the snapshot does not carry the web status section: %s", raw)
	}
	back, err := DecodeSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored := Restore(back, Options{})
	if got, want := restored.State().Settings, m.State().Settings; !reflect.DeepEqual(got, want) {
		t.Fatalf("restored settings %+v, want %+v", got, want)
	}
	back.Settings.WebStatus.Searches = 1 << 30 // the snapshot's value, written after the restore
	if got := restored.State().Settings.WebStatus; got == nil || *got != second {
		t.Fatalf("the restored model shares the snapshot's web status: %+v", got)
	}

	// The restored model folds the next section as the live one does.
	third := second
	third.Searches, third.Consent = 4, "disabled"
	foldAll(t, restored, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{WebStatus: &third}, At: at(8)})
	if got := restored.State().Settings.WebStatus; got == nil || *got != third {
		t.Fatalf("after a restore the settings' web status is %+v, want %+v", got, third)
	}
}

// TestProjectionsNeverShareTheWebStatus: every projection the model hands
// out — State, Snapshot, SnapshotFor, Mirror — carries a web status of its
// own: writing through one moves no other and not the model.
func TestProjectionsNeverShareTheWebStatus(t *testing.T) {
	m := New(Options{})
	want := agent.WebStatusState{Provider: "exa", Key: "env", Consent: "allowed", Searches: 5, Retries: 1}
	w := want
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{WebStatus: &w}, At: at(1)})

	a := mustSnapshot(t, m)
	f, err := m.SnapshotFor("", 0)
	if err != nil {
		t.Fatal(err)
	}
	st := m.State()
	mr := m.Mirror()
	for name, got := range map[string]*agent.WebStatusState{"snapshot": a.Settings.WebStatus,
		"SnapshotFor": f.Settings.WebStatus, "State": st.Settings.WebStatus, "Mirror": mr.Settings.WebStatus} {
		if got == nil || *got != want {
			t.Fatalf("control: %s's web status is %+v, want %+v", name, got, want)
		}
	}
	a.Settings.WebStatus.Searches = 99
	st.Settings.WebStatus.Retries = 99
	mr.Settings.WebStatus.Failed = 99
	for name, got := range map[string]*agent.WebStatusState{"SnapshotFor": f.Settings.WebStatus,
		"the model's next State": m.State().Settings.WebStatus, "the model's next Snapshot": mustSnapshot(t, m).Settings.WebStatus,
		"the model's next Mirror": m.Mirror().Settings.WebStatus} {
		if got == nil || *got != want {
			t.Errorf("%s's web status moved to %+v when another projection's was written; want %+v", name, got, want)
		}
	}
}
