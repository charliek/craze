package tui

import (
	"context"
	"io"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// Plan 025 C1 at the client: a model change is now ONE delta carrying the Model
// and the Config section (design 2), so it moves the config revision as well as
// the model's. The revision guards the TUI already has (mayApply, panel astra
// 15) are exercised here against that shape, in both orders of a command's
// answer and the deltas — and the chips are read over a live session whose
// reply and push race at the ACP boundary.

// fastOnly is composer's catalog in the Stub's shapes: the fast toggle and no
// effort, as cursor's composer-2.5 advertises.
func fastOnly(stub *Stub) []agent.ConfigOption {
	for _, o := range stub.Snapshot().Config {
		if o.ID == "fast" {
			return []agent.ConfigOption{o}
		}
	}
	panic("the Stub advertises no fast toggle")
}

// perModelStub is a sized model over a Stub whose catalog is per model: grok
// has effort and fast (the Stub's static catalog), "fast" and "composer" fast
// alone. Whatever the set-up published is drained.
func perModelStub(t *testing.T) (Model, *Stub) {
	t.Helper()
	m := sized(t)
	stub := m.sess.(*Stub)
	stub.SetModelCatalogs(map[string][]agent.ConfigOption{
		"grok":     stub.Snapshot().Config,
		"fast":     fastOnly(stub),
		"composer": fastOnly(stub),
	})
	m.refreshSnap()
	stubDeltas(t, stub)
	return m, stub
}

// TestAModelChangeMovesTheConfigRevisionWithIt: the dialog's model step lands
// as one delta carrying both sections, and whichever of that delta and the
// chain's answer reaches the model first, it ends on the new model with the new
// model's catalog — effort gone — and both section revisions at that delta.
func TestAModelChangeMovesTheConfigRevisionWithIt(t *testing.T) {
	for _, deltaFirst := range []bool{false, true} {
		name := "the answer, then the delta"
		if deltaFirst {
			name = "the delta, then the answer"
		}
		t.Run(name, func(t *testing.T) {
			m, stub := perModelStub(t)
			m.input.SetValue("/model")
			tm, _ := m.Update(enter())
			m = tm.(Model)
			m = typeInto(t, m, "FAST")
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			applied, ok := runCmd(cmd).(modelApplyMsg)
			if !ok || applied.err != nil || len(applied.done) != 1 {
				t.Fatalf("the chain came back %+v", applied)
			}
			evs := stubDeltas(t, stub)
			if len(evs) != 1 || evs[0].State == nil || evs[0].State.Model == nil || evs[0].State.Config == nil {
				t.Fatalf("the model change published %+v, want one delta with both sections", evs)
			}
			if deltaFirst {
				m = feed(t, m, evs...)
				m = deliver(t, m, applied)
			} else {
				m = deliver(t, m, applied)
				m = feed(t, m, evs...)
			}
			if m.snap.CurrentModel != "fast" || m.model != "fast" {
				t.Fatalf("the screen ended on %q / %q", m.snap.CurrentModel, m.model)
			}
			if agent.EffortOption(m.snap) != nil || agent.FastOption(m.snap) == nil {
				t.Fatalf("the rows show %+v, want the new model's fast toggle alone", m.snap.Config)
			}
			if seq := evs[0].Seq; m.modelRev != seq || m.configRev != seq {
				t.Fatalf("the revisions are model %d / config %d, want both at %d", m.modelRev, m.configRev, seq)
			}
		})
	}
}

// TestADelayedModelRefusalDoesNotUndoAnotherClientsModelAndCatalog is
// revertModelMsg against the new shape: this model's /model is refused, and
// another client's model change — with its catalog — is applied around the
// refusal. Delta first, the refusal may show its error but may not put the old
// model back over the newer one; refusal first, it reverts, and the newer delta
// then carries the screen to the other client's model and catalog.
func TestADelayedModelRefusalDoesNotUndoAnotherClientsModelAndCatalog(t *testing.T) {
	for _, deltaFirst := range []bool{true, false} {
		name := "the refusal, then the delta"
		if deltaFirst {
			name = "the delta, then the refusal"
		}
		t.Run(name, func(t *testing.T) {
			m, stub := perModelStub(t)
			stub.FailNextSetModel()
			m.input.SetValue("/model fast")
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			refusal, ok := runCmd(cmd).(revertModelMsg)
			if !ok {
				t.Fatalf("the refused /model answered %T", refusal)
			}
			// Another client moves the session to composer, catalog and all.
			if _, err := stub.SetModel(context.Background(), "c-9/1", "composer"); err != nil {
				t.Fatalf("the other client's change: %v", err)
			}
			evs := stubDeltas(t, stub)
			if deltaFirst {
				m = feed(t, m, evs...)
				m = deliver(t, m, refusal)
			} else {
				m = deliver(t, m, refusal)
				m = feed(t, m, evs...)
			}
			if m.snap.CurrentModel != "composer" || m.model != "composer" {
				t.Fatalf("the screen ended on %q / %q, want the other client's model", m.snap.CurrentModel, m.model)
			}
			if agent.EffortOption(m.snap) != nil {
				t.Fatalf("the rows show %+v, want composer's catalog", m.snap.Config)
			}
			if len(texts(m, entryError)) == 0 {
				t.Fatal("the refusal is still the user's to see")
			}
		})
	}
}

// TestAnOlderOptionStepIsNotWrittenIntoANewModelsCatalog is the newer config
// revision: this model's dialog turns fast on, and another client's model
// change lands after that step's own delta and before its answer is read. The
// model change now moves the config revision too, so the older step is not
// written back into the catalog of a model it was never chosen for — before,
// a model delta left the config revision where it was and the step's value was
// written over the new model's rows. With the answer read first it is written,
// as it always was, and the newer delta then replaces it.
func TestAnOlderOptionStepIsNotWrittenIntoANewModelsCatalog(t *testing.T) {
	for _, deltasFirst := range []bool{true, false} {
		name := "the answer, then the deltas"
		if deltasFirst {
			name = "the deltas, then the answer"
		}
		t.Run(name, func(t *testing.T) {
			m, stub := perModelStub(t)
			m.input.SetValue("/model")
			tm, _ := m.Update(enter())
			m = tm.(Model)
			m = pressKey(t, m, tea.KeyTab)
			m = pressKey(t, m, tea.KeyTab)
			m = pressKey(t, m, tea.KeyRight)
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			applied, ok := runCmd(cmd).(modelApplyMsg)
			if !ok || applied.err != nil || len(applied.done) != 1 || applied.done[0].label != "fast" {
				t.Fatalf("the chain came back %+v", applied)
			}
			if _, err := stub.SetModel(context.Background(), "c-9/1", "composer"); err != nil {
				t.Fatalf("the other client's change: %v", err)
			}
			evs := stubDeltas(t, stub)
			if len(evs) != 2 || evs[0].Seq != applied.done[0].rev {
				t.Fatalf("want the step's delta and the model change's, got %+v", evs)
			}
			if deltasFirst {
				m = feed(t, m, evs...)
				m = deliver(t, m, applied)
			} else {
				m = deliver(t, m, applied)
				m = feed(t, m, evs...)
			}
			if m.snap.CurrentModel != "composer" {
				t.Fatalf("the screen ended on %q", m.snap.CurrentModel)
			}
			if agent.FastOn(m.snap) {
				t.Fatalf("an older step's fast was written over composer's catalog: %+v", m.snap.Config)
			}
			if got := m.modelLabel(); got != "composer" {
				t.Fatalf("the status row reads %q", got)
			}
		})
	}
}

// foldCatalog reads sub until the title sentinel arrives, folding the Model and
// Config sections of every delta before it: what a client that mirrors the
// stream holds once it has seen everything the session had published by then.
func foldCatalog(t *testing.T, sub *agent.Subscription, sentinel string) (string, []agent.ConfigOption) {
	t.Helper()
	var model string
	var cfg []agent.ConfigOption
	deadline := time.After(10 * time.Second)
	for {
		select {
		case rec, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the subscription ended: %v", sub.Err())
			}
			if rec.Omitted != nil {
				continue
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type != agent.EventMeta || ev.State == nil {
				continue
			}
			if st := ev.State; st.Title != nil && *st.Title == sentinel {
				return model, cfg
			}
			if ev.State.Model != nil {
				model = *ev.State.Model
			}
			if ev.State.Config != nil {
				cfg = ev.State.Config.Options
			}
		case <-deadline:
			t.Fatal("the sentinel never arrived")
		}
	}
}

// TestTheChipsFollowTheLaterOfAReplyAndAPush is panel astra 2's last surface:
// the chips. A live session over F1's scripts switches to claude-opus-5 while
// the agent pushes a catalog just ahead of its reply, just behind it, or moves
// the model behind it; whichever the agent said last is what the status row
// draws — from the TUI's own snapshot, and from a model fed nothing but the
// deltas a mirroring client folds. The orders themselves are forced in
// internal/agent (TestAReplysCatalogIsOrderedWithPushesAtTheBoundary); here the
// chips are drawn by the real status render over whatever order the race took,
// which must end in the same place.
func TestTheChipsFollowTheLaterOfAReplyAndAPush(t *testing.T) {
	for _, tc := range []struct {
		script, want string
	}{
		{"permodel-pushbefore", "Claude Opus 5 (high)"},
		{"permodel-pushafter", "Claude Opus 5 (high · fast)"},
		{"permodel-pushmodel-after", "GLM 5.2 (high)"},
	} {
		t.Run(tc.script, func(t *testing.T) {
			isolateSkillsHome(t)
			bin := buildFakeAgent(t)
			ws := t.TempDir()
			sess := agent.New(agent.Options{
				Binary:    bin,
				ExtraArgs: []string{"-script=" + tc.script},
				Workspace: ws,
				Force:     true,
				Stderr:    io.Discard,
			})
			// Subscribed before Start, so the fold sees every delta there is.
			sub, err := sess.(agent.EventSource).Subscribe(agent.SubscribeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sub.Close)
			if err := sess.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sess.Close() })

			m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "grok-4.6"})
			tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			m = tm.(Model)
			tm, _ = m.Update(startedMsg{})
			m = tm.(Model)
			if got := m.modelLabel(); got != "Grok 4.6 (high · fast)" {
				t.Fatalf("the session started as %q", got)
			}
			m = pumpEnter(t, m, "/model claude-opus-5")
			m = pumpSettled(t, m)
			// The wire barrier: a push behind the switch's reply is read before
			// the reply to any later request, so once this set_mode has been
			// answered the push has been applied; the pump then applies what it
			// published.
			if _, err := sess.SetMode(context.Background(), "", "agent"); err != nil {
				t.Fatalf("the barrier: %v", err)
			}
			m = pumpSettled(t, m)
			if got := m.modelLabel(); got != tc.want {
				t.Fatalf("the status row reads %q, want %q", got, tc.want)
			}

			if err := sess.SetTitle("", "fold-sentinel"); err != nil {
				t.Fatal(err)
			}
			model, cfg := foldCatalog(t, sub, "fold-sentinel")
			folded := m
			folded.snap.CurrentModel, folded.snap.Config = model, cfg
			if got := folded.modelLabel(); got != tc.want {
				t.Fatalf("over the folded deltas the status row reads %q, want %q", got, tc.want)
			}
		})
	}
}
