package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// modeDelta is the event another client's mode change reaches this model as:
// the delta the session enqueues under its own lock, with the Seq the log gave
// it, and **no Event.Mode** — that field is an agent-initiated update's, and a
// client that is not this one is not the agent (plan 021 correction 20).
func modeDelta(seq uint64, id string) agent.Event {
	return agent.Event{Type: agent.EventMeta, Seq: seq, State: &agent.StateDelta{Mode: &id}}
}

func modelDelta(seq uint64, id string) agent.Event {
	return agent.Event{Type: agent.EventMeta, Seq: seq, State: &agent.StateDelta{Model: &id}}
}

// agentSetsModel is agentSetsMode for the model: somebody else's change, in the
// session's own snapshot, which the test then announces as a delta.
func agentSetsModel(s *Stub, id string) {
	s.mu.Lock()
	s.snap.CurrentModel = id
	s.mu.Unlock()
}

// TestADelayedModeRefusalDoesNotUndoANewerChange is A14 for the mode, through
// the real reducers: another client changes the mode while this model's own
// request is in flight, and the refusal that comes back afterwards may show its
// error but may not roll that change back.
//
// The guard is the revision, not the value: a setting is not monotonic — plan →
// agent → plan leaves the snapshot exactly where it started — so "has it
// changed since?" can only be answered by the Seq of the last delta applied
// (panel astra 15).
func TestADelayedModeRefusalDoesNotUndoANewerChange(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	stub.FailNextSetMode()
	m, cmd := askMode(t, m, "plan")

	// Another client sets the mode to ask, and this model applies that delta.
	agentSetsMode(stub, "ask")
	m = feed(t, m, modeDelta(7, "ask"))

	// Now the refusal of the model's own request arrives.
	m = deliver(t, m, runCmd(cmd))
	if got := m.snap.CurrentMode; got != "ask" {
		t.Fatalf("the chip is on %q, want the newer change the refusal must not undo", got)
	}
	if len(texts(m, entryError)) == 0 {
		t.Fatal("the refusal is still the user's to see")
	}
	if m.modeInFlight != "" {
		t.Fatal("the refusal is the last word on its request: nothing may stay in flight")
	}
}

// TestADelayedModeRefusalStillRevertsWhenNothingElseChanged is the same path
// with nothing newer applied: the revert happens exactly as it always did.
func TestADelayedModeRefusalStillRevertsWhenNothingElseChanged(t *testing.T) {
	m := sized(t)
	m.sess.(*Stub).FailNextSetMode()
	m, cmd := askMode(t, m, "plan")
	m = deliver(t, m, runCmd(cmd))
	if got := m.snap.CurrentMode; got != "agent" {
		t.Fatalf("the chip is on %q, want the mode the request left", got)
	}
}

// TestAModeRefusalThenAnotherClientsChangeEndsOnTheDelta is the third
// permutation: this model's own failure lands first and reverts, and the other
// client's delta — which is newer — then overwrites it. The model ends on the
// shared state, not on its own optimism.
func TestAModeRefusalThenAnotherClientsChangeEndsOnTheDelta(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	stub.FailNextSetMode()
	m, cmd := askMode(t, m, "plan")
	m = deliver(t, m, runCmd(cmd))
	agentSetsMode(stub, "ask")
	m = feed(t, m, modeDelta(7, "ask"))
	if got := m.snap.CurrentMode; got != "ask" {
		t.Fatalf("the chip is on %q, want the delta's value", got)
	}
}

// TestADelayedModelRefusalDoesNotUndoANewerChange is A14 for /model, which had
// no guard at all before C10: revertModelMsg restored its prev blindly, so a
// refusal that arrived after somebody else's change put the old model back with
// nothing to correct it afterwards (panel astra 15).
func TestADelayedModelRefusalDoesNotUndoANewerChange(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	stub.FailNextSetModel()
	m.input.SetValue("/model fast")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.snap.CurrentModel != "fast" {
		t.Fatalf("the optimistic value is %q", m.snap.CurrentModel)
	}

	// Another client picks a model, and this model applies that delta.
	agentSetsModel(stub, "composer")
	m = feed(t, m, modelDelta(11, "composer"))

	m = flushCmd(t, m, cmd)
	if got := m.snap.CurrentModel; got != "composer" {
		t.Fatalf("the model is %q, want the newer change the refusal must not undo", got)
	}
	if m.model != "composer" {
		t.Fatalf("the status row says %q", m.model)
	}
	if len(texts(m, entryError)) == 0 {
		t.Fatal("the refusal is still the user's to see")
	}
}

// TestACompletedStepOlderThanAnAppliedDeltaIsNotWritten is the fourth
// permutation, and the one only the dialog can reach: its apply chain confirms
// a step, another client's change is applied while the chain is still running,
// and the completed step's own revision is the older one — so settleStep must
// not write it back over what is now current.
func TestACompletedStepOlderThanAnAppliedDeltaIsNotWritten(t *testing.T) {
	m := sized(t)
	// The step landed at revision 5; the model has since applied a delta at 11.
	m.modelRev = 11
	st := applyStep{value: "fast", note: "model → fast", label: "model", at: 4, rev: 5}
	m.snap.CurrentModel = "composer"
	m.model = "composer"
	if got := m.settleStep(st); got.snap.CurrentModel != "composer" {
		t.Fatalf("a step older than an applied delta wrote %q", got.snap.CurrentModel)
	}
	// The same step, with nothing newer applied, is written as it always was.
	m.modelRev = 5
	if got := m.settleStep(st); got.snap.CurrentModel != "fast" {
		t.Fatalf("a current step was not written: %q", got.snap.CurrentModel)
	}
}

// TestMayApply is the rule itself, as a table: a refusal is judged against
// where its request started and a confirmation against where it landed, and in
// both cases anything newer wins.
func TestMayApply(t *testing.T) {
	for _, tc := range []struct {
		name             string
		applied, at, rev uint64
		want             bool
	}{
		{"nothing applied at all", 0, 0, 0, true},
		{"a refusal, nothing since", 7, 7, 0, true},
		{"a refusal, something since", 9, 7, 0, false},
		{"its own echo applied", 9, 7, 9, true},
		{"its own echo, then another", 11, 7, 9, false},
		{"a confirmation nobody has applied yet", 7, 7, 9, true},
	} {
		if got := mayApply(tc.applied, tc.at, tc.rev); got != tc.want {
			t.Errorf("%s: mayApply(%d, %d, %d) = %v", tc.name, tc.applied, tc.at, tc.rev, got)
		}
	}
}

// TestAModeDeltaIsMaskedWhileAChangeOfItsOwnIsInFlight: the delta path is
// masked exactly as refreshSnap has always been (modeInFlight). Mode A is
// requested, then mode B; A's delta arrives, and it must not put the chip back
// on A — the newer request of the user's own is what the chip shows until it is
// answered (plan 021 §3.8's "make sure the delta path is masked the same way").
func TestAModeDeltaIsMaskedWhileAChangeOfItsOwnIsInFlight(t *testing.T) {
	m := sized(t)
	m, first := askMode(t, m, "plan")
	m, second := askMode(t, m, "ask")
	// A's delta lands while B is still in flight.
	m = feed(t, m, modeDelta(7, "plan"))
	if got := m.snap.CurrentMode; got != "ask" {
		t.Fatalf("the chip is on %q, want the request that is still in flight", got)
	}
	// Both answers arrive; the chip ends on what the session holds.
	m = deliver(t, m, runCmd(first))
	m = deliver(t, m, runCmd(second))
	if got := m.snap.CurrentMode; got != sessionMode(m.sess.(*Stub)) {
		t.Fatalf("the chip says %q and the session %q", got, sessionMode(m.sess.(*Stub)))
	}
}

// TestASettingsDeltaDrawsNoRow is plan 021 §3.8's rule for S1b: the TUI renders
// no transcript row from a settings delta — not another client's change, not
// the agent's, and not the echo of its own. Its own notes stay exactly where
// they are written today, which the mode and model tests above pin.
func TestASettingsDeltaDrawsNoRow(t *testing.T) {
	m := sized(t)
	before := len(m.main.entries)
	title := "another client renamed it"
	m = feed(t, m,
		modeDelta(7, "plan"),
		modelDelta(8, "composer"),
		agent.Event{Type: agent.EventMeta, Seq: 9, State: &agent.StateDelta{Title: &title}},
		agent.Event{Type: agent.EventMeta, Seq: 10, State: &agent.StateDelta{
			Config: &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Current: "high"}}},
		}},
	)
	if got := len(m.main.entries); got != before {
		t.Fatalf("settings deltas drew %d rows:\n%q", got-before, texts(m, entryNote))
	}
}

// TestRenameGoesThroughControlAndDrawsItsNote: /rename renames through the
// engine, pins the title, writes its own note where it always did, and the
// delta it causes carries no Event.Text — so nothing prints a title line and
// nothing writes it to the index as an *agent* title (A20).
func TestRenameGoesThroughControlAndDrawsItsNote(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	m.input.SetValue("/rename the flaky pty test")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if !strings.Contains(strings.Join(texts(m, entryNote), "|"), "renamed to the flaky pty test") {
		t.Fatalf("the note is %q", texts(m, entryNote))
	}
	if got := stub.Snapshot().Title; got != "the flaky pty test" {
		t.Fatalf("the session's title is %q", got)
	}
	if err := stub.EventLog().Flush(context.Background(), nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	ev := <-stub.Events()
	if ev.Type != agent.EventMeta || ev.Text != "" || ev.State == nil || ev.State.Title == nil {
		t.Fatalf("a rename published %+v", ev)
	}
	if ev.Cause == "" {
		t.Fatal("a rename's delta names no command, so no client can tell its own echo")
	}
	// The pin holds: a title the agent invents afterwards does not replace it.
	stub.AgentTitle("the agent's own title")
	if got := stub.Snapshot().Title; got != "the flaky pty test" {
		t.Fatalf("the pin did not hold: %q", got)
	}
}

// TestRenameRefusedForRoomWritesNothing: with the log's outbox backed up the
// rename is refused before it changes anything, and the model draws the error
// instead of the note — and writes no index row for a rename that did not
// happen.
func TestRenameRefusedForRoomWritesNothing(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	idx := &fakeIndex{}
	m.sessionIndex = idx
	saturateStub(t, stub)

	m.input.SetValue("/rename never")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if len(texts(m, entryError)) == 0 {
		t.Fatal("a refused rename draws its error")
	}
	if notes := strings.Join(texts(m, entryNote), "|"); strings.Contains(notes, "renamed to") {
		t.Fatalf("a refused rename wrote its note: %q", notes)
	}
	if len(idx.rows) != 0 {
		t.Fatalf("a refused rename wrote %d index rows", len(idx.rows))
	}
	if got := stub.Snapshot().Title; got != "" {
		t.Fatalf("a refused rename renamed the session to %q", got)
	}
}

// saturateStub fills the Stub's outbox past its soft bound, with nobody reading
// the primary: the state in which every rejectable command refuses.
func saturateStub(t *testing.T, s *Stub) {
	t.Helper()
	log := s.EventLog()
	filler := make([]agent.Event, 256)
	for i := range filler {
		filler[i] = agent.Event{Type: agent.EventText, Text: "fill"}
	}
	for log.OutboxRoom() {
		log.Enqueue(filler...)
	}
}

// TestAPlanOfferSurvivesAModeClickMadeJustBeforeTheTurn is CodeRabbit's finding
// 8, as a test over the Stub (A20). A craze-initiated mode change carries its
// payload in Event.State alone; if it filled Event.Mode, the delta arriving
// after the next turn had started would run retirePlanOffer against that turn
// and the offer would never appear.
func TestAPlanOfferSurvivesAModeClickMadeJustBeforeTheTurn(t *testing.T) {
	// The mode click, whose answer and whose delta are both still in flight.
	m, cmd := askMode(t, sized(t), "plan")
	// The turn the user sends straight afterwards, run for real, and the offer
	// its plan-mode ending earns.
	m = planTurn(t, m)
	if !m.planOffering() {
		t.Fatalf("the plan offer was never made:\n%s", plainView(m))
	}
	// Only now does the mode change's delta arrive, carrying its mode in the
	// section and nothing in Event.Mode. Were it in Event.Mode, applyEvent
	// would retire the offer against the turn that has just ended.
	m = feed(t, m, modeDelta(7, "plan"))
	m = deliver(t, m, runCmd(cmd))
	if !m.planOffering() {
		t.Fatalf("the mode click's delta retired a plan offer made after it:\n%s", plainView(m))
	}
}
