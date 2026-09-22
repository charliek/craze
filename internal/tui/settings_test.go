package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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

// stubDeltas is every event the session has published and not yet been read,
// flushed first so "published" means "there": the REAL deltas, with the Seq the
// log gave them and the sections the session really wrote. A test that feeds
// these instead of hand-built events is the only kind that can catch a delta
// whose SECTIONS are wrong — flushCmd never consumes a session event, so a
// command-only test sees none of them (review r23, "TUI revision tests' blind
// spots").
func stubDeltas(t *testing.T, s *Stub) []agent.Event {
	t.Helper()
	if err := s.EventLog().Flush(context.Background(), nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	var out []agent.Event
	for {
		select {
		case ev := <-s.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// configBackedModel gives the model's session cursor's shape: the model lives
// in a config option, and the Stub's SetModel switches through it, as the live
// session's one-call path does (plan 025 design 2). The model's own mirror is
// refreshed, as a frame would refresh it, so the dialog and the command see
// the option.
func configBackedModel(t *testing.T, m Model) (Model, *Stub) {
	t.Helper()
	stub := m.sess.(*Stub)
	stub.ModelConfigOption("model")
	m.refreshSnap()
	if agent.ModelConfigOption(m.snap) == nil {
		t.Fatal("the session did not take the model option")
	}
	return m, stub
}

// TestTheStubsModelOptionIsTheFirstOfItsCategory is astra r7 item 2: the
// Stub agrees with the live session on which option is the model's — the
// first of category "model" (agent.ModelConfigOptionIn, plan 025 X16). A write
// to a second one moves that selector alone, with no Model section; SetModel
// switches through the first and leaves the second at its own value, the
// Model and Config sections in one delta; and a write to the first is a model
// change.
func TestTheStubsModelOptionIsTheFirstOfItsCategory(t *testing.T) {
	m := sized(t)
	stub := m.sess.(*Stub)
	stub.mu.Lock()
	stub.snap.Models = append(stub.snap.Models, agent.ModelInfo{ID: "composer", Name: "Composer"})
	stub.mu.Unlock()
	stub.ModelConfigOption("p")
	stub.ModelConfigOption("q")
	stubDeltas(t, stub)
	ctx := context.Background()
	check := func(step, model, p, q string, wantModelSection bool) {
		t.Helper()
		snap := stub.Snapshot()
		if snap.CurrentModel != model || optionCurrent(snap.Config, "p") != p || optionCurrent(snap.Config, "q") != q {
			t.Errorf("%s: the session is on %q, p=%q q=%q; want %q, p=%q q=%q", step, snap.CurrentModel,
				optionCurrent(snap.Config, "p"), optionCurrent(snap.Config, "q"), model, p, q)
		}
		evs := stubDeltas(t, stub)
		if len(evs) != 1 || evs[0].State == nil || evs[0].State.Config == nil || (evs[0].State.Model != nil) != wantModelSection {
			t.Errorf("%s: want one delta with the Config section and the Model section %v: %+v", step, wantModelSection, evs)
		}
	}

	if out, err := stub.SetConfig(ctx, "c-9/1", "q", "composer", ""); err != nil || out.Value != "composer" {
		t.Fatalf("the second selector's write answered %+v, %v", out, err)
	}
	check("a write to the second selector", "grok", "grok", "composer", false)

	if out, err := stub.SetModel(ctx, "c-9/2", "fast"); err != nil || out.Value != "fast" {
		t.Fatalf("SetModel answered %+v, %v", out, err)
	}
	check("SetModel", "fast", "fast", "composer", true)

	if out, err := stub.SetConfig(ctx, "c-9/3", "p", "grok", ""); err != nil || out.Value != "grok" {
		t.Fatalf("the model option's write answered %+v, %v", out, err)
	}
	check("a write to the model option", "grok", "grok", "composer", true)
}

// TestAModelChangeThroughTheModelOptionEndsOnTheNewModel is r23 finding 3
// through the real reducers and the real deltas, as the session owns the path
// now (plan 025 design 2). `/model fast` with no effort, on a catalog that
// keeps the model in an option: the session switches through that option, and
// the command returns no message of its own — so the ONLY thing that can
// correct the screen afterwards is the delta that change published, and it
// must carry the Model section beside the Config one.
//
// Before r23's fix a model set as a config option moved the config section
// alone: refreshSnap put CurrentModel back to the old model and nothing ever
// restored it. That write was the TUI's own fallback then; it is the session's
// one call now, and the TUI no longer has a fallback (astra r7 item 1). Both
// orders are run, because which of the command's answer and the session's
// delta reaches the model first is a race.
func TestAModelChangeThroughTheModelOptionEndsOnTheNewModel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deltaFirst bool
	}{
		{"the delta arrives after the command's answer", false},
		{"the delta arrives first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := configBackedModel(t, sized(t))
			stubDeltas(t, stub) // whatever the set-up published
			m.input.SetValue("/model fast")
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			if m.snap.CurrentModel != "fast" {
				t.Fatalf("the optimistic value is %q", m.snap.CurrentModel)
			}
			msg := runCmd(cmd)
			if msg != nil {
				t.Fatalf("a /model with no effort answers with nothing, got %T", msg)
			}
			evs := stubDeltas(t, stub)
			if len(evs) != 1 || evs[0].State == nil || evs[0].State.Model == nil || evs[0].State.Config == nil {
				t.Fatalf("want the one delta carrying the Model and Config sections: %+v", evs)
			}
			if tc.deltaFirst {
				m = feed(t, m, evs...)
			} else {
				m = flushCmd(t, m, cmd)
				m = feed(t, m, evs...)
			}
			if got := m.snap.CurrentModel; got != "fast" {
				t.Fatalf("the screen ended on %q, want the model the option was set to", got)
			}
			if m.model != "fast" {
				t.Fatalf("the status row says %q", m.model)
			}
			snap := stub.Snapshot()
			if snap.CurrentModel != "fast" {
				t.Fatalf("the session's own model is %q", snap.CurrentModel)
			}
			if opt := agent.ModelConfigOption(snap); opt == nil || opt.Current != "fast" {
				t.Fatalf("the model option is %+v: the switch goes through it", opt)
			}
			if n := stubConfigCalls(stub); n != 0 {
				t.Fatalf("%d option writes reached the agent: the switch is the session's one call", n)
			}
			if len(texts(m, entryError)) != 0 {
				t.Fatalf("the change succeeded, so nothing is the user's to see: %q", texts(m, entryError))
			}
		})
	}
}

// errUnreadModelAnswer is what the live session answers a model change with
// when the agent's reply could not be read: agent.ErrBadCatalog as its
// settings handler wraps it for a member that is not an option, under the
// call's method.
var errUnreadModelAnswer = fmt.Errorf("session/set_config_option: %w: member 3 is not an option", agent.ErrBadCatalog)

// unreadModelRow is the one error row a model change whose answer could not be
// read leaves, naming the model the screen then shows.
func unreadModelRow(shown string) string {
	return "model: the agent's answer could not be read — it may have switched; craze still shows " + shown
}

// TestAModelAnswerThatCouldNotBeReadIsNotARefusal is astra r3 C at `/model`: a
// model change whose answer the session could not read (agent.ErrBadCatalog)
// RAN — the agent answered, and may have switched — so it is not reverted as a
// refused model. Nothing more is written to the agent, the effort included;
// the screen reads the session back rather than putting its own prev back; and
// the one error row says the outcome is unknown and names the model the screen
// shows.
//
// The second case is why it reads back: another client's change lands after
// the answer and before this model has seen its delta, so prev is no longer
// what the session is on, and only the snapshot says so.
//
// Before r3's fix the TUI's fallback set the model again as a config option
// under another command id, and the Stub took it: a second write for a change
// craze could not confirm, and the effort sent after it. The TUI has no
// fallback at all now (astra r7 item 1); the count of writes still says
// nothing followed the answer.
func TestAModelAnswerThatCouldNotBeReadIsNotARefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		// moved is the other client's model, "" for none.
		moved string
	}{
		{"nothing moved since", ""},
		{"another client moved it since", "composer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := configBackedModel(t, sized(t))
			stubDeltas(t, stub)
			stub.FailNextSetModelWith(errUnreadModelAnswer)
			m.input.SetValue("/model fast high")
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			if m.snap.CurrentModel != "fast" {
				t.Fatalf("the optimistic value is %q", m.snap.CurrentModel)
			}
			msg := runCmd(cmd)
			if _, ok := msg.(modelUnreadMsg); !ok {
				t.Fatalf("an answer that could not be read came back as %T %+v", msg, msg)
			}
			if n := stubConfigCalls(stub); n != 0 {
				t.Fatalf("%d option writes reached the agent after an answer that could not be read", n)
			}
			want := "grok"
			if tc.moved != "" {
				agentSetsModel(stub, tc.moved)
				want = tc.moved
			}
			m = deliver(t, m, msg)
			if got := stub.Snapshot().CurrentModel; got != want {
				t.Fatalf("the session is on %q, want %q", got, want)
			}
			if m.snap.CurrentModel != want || m.model != want {
				t.Fatalf("the screen is on %q / %q, want the session's %q", m.snap.CurrentModel, m.model, want)
			}
			if errs := texts(m, entryError); len(errs) != 1 || errs[0] != unreadModelRow(want) {
				t.Fatalf("errors %q, want the one row %q", errs, unreadModelRow(want))
			}
			if notes := texts(m, entryNote); len(notes) != 0 {
				t.Fatalf("notes %q: nothing landed", notes)
			}
		})
	}
}

// TestModelDialogAnAnswerThatCouldNotBeReadIsNotARefusal is the same for the
// dialog's model step (applyModelStep): no option step after it — the chain
// ends as on any failure, since an option cannot be judged against a catalog
// nobody could read — the rows read back from the session, and the row says
// the outcome is unknown instead of naming a refusal.
func TestModelDialogAnAnswerThatCouldNotBeReadIsNotARefusal(t *testing.T) {
	m, stub := configBackedModel(t, sized(t))
	stubDeltas(t, stub)
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	m = pressKey(t, m, tea.KeyDown)
	m = pressKey(t, m, tea.KeyTab)
	m = pressKey(t, m, tea.KeyRight)
	stub.FailNextSetModelWith(errUnreadModelAnswer)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.snap.CurrentModel != "fast" || optionCurrent(m.snap.Config, "effort") != "high" {
		t.Fatalf("the optimistic rows are %q, effort %q", m.snap.CurrentModel, optionCurrent(m.snap.Config, "effort"))
	}
	m = flushCmd(t, m, cmd)
	if n := stubConfigCalls(stub); n != 0 {
		t.Fatalf("%d option writes reached the agent after an answer that could not be read", n)
	}
	snap := stub.Snapshot()
	if snap.CurrentModel != "grok" || optionCurrent(snap.Config, "effort") != "medium" {
		t.Fatalf("the session is on %q, effort %q", snap.CurrentModel, optionCurrent(snap.Config, "effort"))
	}
	if m.snap.CurrentModel != "grok" || m.model != "grok" || optionCurrent(m.snap.Config, "effort") != "medium" {
		t.Fatalf("the rows are %q / %q, effort %q: want the session's", m.snap.CurrentModel, m.model,
			optionCurrent(m.snap.Config, "effort"))
	}
	if errs := texts(m, entryError); len(errs) != 1 || errs[0] != unreadModelRow("grok") {
		t.Fatalf("errors %q, want the one row %q", errs, unreadModelRow("grok"))
	}
	if notes := texts(m, entryNote); len(notes) != 0 {
		t.Fatalf("notes %q: nothing landed", notes)
	}
}

// twoModelSelectors is astra r7 item 1's session: two options of category
// "model", p then q, both on the current model. The screen is refreshed from
// that catalog, so the model option it holds is p; then the session's catalog
// is reordered to q, p with no delta the screen has handled — the agent's own
// update still on its way, which makes q the model's option and p a second
// selector.
func twoModelSelectors(t *testing.T, m Model) (Model, *Stub) {
	t.Helper()
	stub := m.sess.(*Stub)
	stub.ModelConfigOption("p")
	stub.ModelConfigOption("q")
	m.refreshSnap()
	if opt := agent.ModelConfigOption(m.snap); opt == nil || opt.ID != "p" {
		t.Fatalf("the screen's model option is %+v, want p", opt)
	}
	stub.mu.Lock()
	cfg := stub.snap.Config
	n := len(cfg)
	cfg[n-2], cfg[n-1] = cfg[n-1], cfg[n-2]
	stub.mu.Unlock()
	if opt := agent.ModelConfigOption(stub.Snapshot()); opt == nil || opt.ID != "q" {
		t.Fatalf("the session's model option is %+v, want q", opt)
	}
	stubDeltas(t, stub)
	return m, stub
}

// TestARefusedModelIsNotRetriedAsAConfigWrite is astra r7 item 1: a model
// change the session refuses sends nothing more, from `/model` and from the
// dialog. The TUI used to retry it as a write of the model's config option,
// by an id taken from the screen's snapshot before the first call; here the
// agent has since made q the model's option, so that write landed on p, a
// second selector, which the session answers as an ordinary option's success
// — and the dialog announced "model → fast" and settled fast, its Config
// delta handled first, while the session stayed on grok. Which call moves the
// model is the session's to choose (plan 025 design 2); its refusal is the
// row, and the screen stays where the session is.
func TestARefusedModelIsNotRetriedAsAConfigWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		// run issues the change and delivers its answer after the session's
		// deltas — astra's order — reporting whether it came back a refusal.
		run func(t *testing.T, m Model, stub *Stub) (Model, bool)
	}{
		{
			name: "/model",
			run: func(t *testing.T, m Model, stub *Stub) (Model, bool) {
				m.input.SetValue("/model fast")
				tm, cmd := m.Update(enter())
				m = tm.(Model)
				msg := runCmd(cmd)
				_, refused := msg.(revertModelMsg)
				return deliverAnswers(t, m, stub, true, msg), refused
			},
		},
		{
			name: "the dialog",
			run: func(t *testing.T, m Model, stub *Stub) (Model, bool) {
				m = openDialog(t, m)
				m = typeInto(t, m, "FAST")
				tm, cmd := m.Update(enter())
				m = tm.(Model)
				applied, _ := runCmd(cmd).(modelApplyMsg)
				return deliverAnswers(t, m, stub, true, applied), applied.step == "model" && applied.err != nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := twoModelSelectors(t, sized(t))
			writes := configWrites(stub)
			stub.FailNextSetModel()
			m, refused := tc.run(t, m, stub)
			if !refused {
				t.Errorf("the change did not come back as the model refused")
			}
			if got := writes(); len(got) != 0 {
				t.Errorf("the agent was sent %q after it refused the model", got)
			}
			snap := stub.Snapshot()
			if snap.CurrentModel != "grok" || optionCurrent(snap.Config, "p") != "grok" || optionCurrent(snap.Config, "q") != "grok" {
				t.Errorf("the session is on %q with %+v, want grok untouched", snap.CurrentModel, snap.Config)
			}
			if m.snap.CurrentModel != "grok" || m.model != "grok" {
				t.Errorf("the screen is on %q / %q, want the session's grok", m.snap.CurrentModel, m.model)
			}
			if notes := texts(m, entryNote); len(notes) != 0 {
				t.Errorf("notes %q: nothing landed", notes)
			}
			if errs := texts(m, entryError); len(errs) != 1 {
				t.Errorf("errors %q, want the refusal alone", errs)
			}
		})
	}
}

// TestAModelOptionCompletionOlderThanAnotherClientsChangeIsNotWritten is the
// second failure in r23 finding 3, and the one the revision guard exists for:
// the dialog's model step lands through the model option — the session's one
// call (plan 025 design 2) — another client changes the same config-backed
// model afterwards, and only then does the first step's completion reach the
// model.
//
// It works because a change made on the model option carries the MODEL section
// beside the Config one, so the model revision the completion is judged
// against is one a config-backed change can advance. Judged against the config
// revision alone — or against a model revision nothing moves — the older
// answer would be written over the newer value with nothing left to correct
// it. The step used to land here by the TUI's own fallback, which it no longer
// has (astra r7 item 1); the guard is the same.
func TestAModelOptionCompletionOlderThanAnotherClientsChangeIsNotWritten(t *testing.T) {
	m, stub := configBackedModel(t, sized(t))
	stubDeltas(t, stub)

	// The dialog picks a model; the session switches through its option.
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.dialog != dialogModel {
		t.Fatal("expected the model dialog")
	}
	m = typeInto(t, m, "FAST")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	// Its answer is held: nothing of it has reached the model yet.
	msg := runCmd(cmd)
	applied, ok := msg.(modelApplyMsg)
	if !ok {
		t.Fatalf("the dialog answered with %T", msg)
	}
	if applied.err != nil || len(applied.done) != 1 {
		t.Fatalf("the dialog's chain came back %+v", applied)
	}
	snap := stub.Snapshot()
	if opt := agent.ModelConfigOption(snap); snap.CurrentModel != "fast" || opt == nil || opt.Current != "fast" {
		t.Fatalf("the switch left the session on %q, its option %+v", snap.CurrentModel, opt)
	}
	m = feed(t, m, stubDeltas(t, stub)...)

	// Another client moves the same option, and this model applies that delta.
	if _, err := stub.SetConfig(context.Background(), "c-9/1", "model", "grok", ""); err != nil {
		t.Fatalf("the other client's change: %v", err)
	}
	newer := stubDeltas(t, stub)
	m = feed(t, m, newer...)
	if got := m.snap.CurrentModel; got != "grok" {
		t.Fatalf("the newer change left the screen on %q", got)
	}

	// Only now does the dialog's own completion arrive.
	m = deliver(t, m, applied)
	if got := m.snap.CurrentModel; got != "grok" {
		t.Fatalf("an older completion wrote %q over the newer change", got)
	}
	if m.model != "grok" {
		t.Fatalf("the status row says %q", m.model)
	}
}

// TestAZeroRevisionSuccessIsWrittenUnlessSomethingNewerWas is hunt A's last
// question: a Set that succeeded but could not learn its revision — the flush
// gave up, or the log was closing — answers Rev 0, and a client must read that
// as "no revision" rather than as one older than every other. With nothing
// applied since, the step is written; with a newer delta applied, it is not.
func TestAZeroRevisionSuccessIsWrittenUnlessSomethingNewerWas(t *testing.T) {
	m := sized(t)
	m.snap.CurrentModel, m.model = "grok", "grok"
	st := applyStep{value: "fast", note: "model → fast", label: "model", at: 4, rev: 0}
	m.modelRev = 4
	if got := m.settleStep(st); got.snap.CurrentModel != "fast" {
		t.Fatalf("a zero-revision success with nothing newer applied wrote %q", got.snap.CurrentModel)
	}
	m.modelRev = 9
	if got := m.settleStep(st); got.snap.CurrentModel != "grok" {
		t.Fatalf("a zero-revision success wrote %q over a newer delta", got.snap.CurrentModel)
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
	isolateSkillsHome(t)
	// The index goes in through Config, because the engine is built in New and
	// is what writes it now (plan 021 §3.8): a store assigned to the model
	// afterwards would never reach the engine, and this test would pass because
	// nothing was ever configured to write.
	idx := &fakeIndex{}
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	m := New(Config{
		Session:      stub,
		Theme:        "tokyo-night",
		Workspace:    t.TempDir(),
		Model:        "grok",
		Yolo:         true,
		SessionIndex: idx,
	})
	m = deliver(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = deliver(t, m, startedMsg{})
	saturateStub(t, stub)

	m = runSlash(t, m, "/rename never")
	if len(texts(m, entryError)) == 0 {
		t.Fatal("a refused rename draws its error")
	}
	if notes := strings.Join(texts(m, entryNote), "|"); strings.Contains(notes, "renamed to") {
		t.Fatalf("a refused rename wrote its note: %q", notes)
	}
	if idx.count() != 0 {
		t.Fatalf("a refused rename wrote %d index rows", idx.count())
	}
	if got := stub.Snapshot().Title; got != "" {
		t.Fatalf("a refused rename renamed the session to %q", got)
	}
}

// saturateStub fills the Stub's outbox past its soft bound, with nobody reading
// the primary: the state in which every rejectable command refuses.
//
// The state has to be one the log's drainer cannot leave, or a test that reads
// OutboxRoom after it races the drainer. Filling the outbox alone is not that:
// the drainer moves whole batches onto the primary while the primary has room,
// and each batch it commits gives the outbox that much room back — so on a slow
// runner the loop could end with the drainer still to commit its first batch,
// and the room return a moment later (a macOS CI run of
// TestRenameRefusedForRoomWritesNothing saw the "refused" rename succeed). So
// the primary is filled FIRST, to exactly its capacity, and flushed: every
// filler is committed and the outbox is empty. Only then is the outbox filled,
// and the drainer's very next send blocks on the full primary for good — it
// commits nothing, and no room comes back while nobody reads.
func saturateStub(t *testing.T, s *Stub) {
	t.Helper()
	log := s.EventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Everything already enqueued is on the primary first, so what is left of
	// its buffer is known and stays so: nothing else writes to it here.
	if err := log.Flush(ctx, nil); err != nil {
		t.Fatalf("flushing before the fill: %v", err)
	}
	fill := func(n int) []agent.Event {
		out := make([]agent.Event, n)
		for i := range out {
			out[i] = agent.Event{Type: agent.EventText, Text: "fill"}
		}
		return out
	}
	primary := s.Events()
	if free := cap(primary) - len(primary); free > 0 {
		log.Enqueue(fill(free)...)
		// They all fit, so the drainer publishes and commits them without
		// waiting for a reader, and this returns once it has.
		if err := log.Flush(ctx, nil); err != nil {
			t.Fatalf("flushing the primary full: %v", err)
		}
	}
	batch := fill(256)
	for log.OutboxRoom() {
		log.Enqueue(batch...)
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
