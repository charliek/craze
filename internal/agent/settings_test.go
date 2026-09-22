package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The four updates onUpdate turns into a settings delta, as the agent sends
// them. sessionInfoNotification is in load_test.go beside the replay tests.
func currentModeNotification(id string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update:    mustJSON(map[string]any{"sessionUpdate": acp.UpdateCurrentMode, "currentModeId": id}),
	}
}

func configOptionNotification(id, value string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update: mustJSON(map[string]any{
			"sessionUpdate": acp.UpdateConfigOption,
			"configOptions": []map[string]any{{
				"id": id, "name": "Effort", "category": "thought_level", "type": "select",
				"currentValue": value,
				"options": []map[string]any{
					{"value": "low", "name": "low"}, {"value": "high", "name": "high"},
				},
			}},
		}),
	}
}

func availableCommandsNotification(names ...string) acp.SessionNotification {
	cmds := make([]map[string]any, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, map[string]any{"name": n, "description": "d-" + n})
	}
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update:    mustJSON(map[string]any{"sessionUpdate": acp.UpdateAvailableCommands, "availableCommands": cmds}),
	}
}

// TestTheAgentsOwnUpdatesCarryTheirSectionAndAreBufferedWhenTheyReturn covers
// all four of onUpdate's meta sites at once (plan 021 §3.8). Each one now
// enqueues its delta **under s.mu, in the section that mutates the snapshot**,
// and then flushes on the read loop — so:
//
//   - the section is there, in full, and agrees with the snapshot;
//   - Event.Mode and Event.Text are filled exactly as they always were, because
//     these are the agent's own changes and a client reads those two fields as
//     "retire the plan offer" and "write the index / print a title line";
//   - "emitted means buffered" still holds when onUpdate returns, which is what
//     keeps the title line's position in `craze prompt --json`.
func TestTheAgentsOwnUpdatesCarryTheirSectionAndAreBufferedWhenTheyReturn(t *testing.T) {
	s := newTestSession(t, Options{})
	t.Cleanup(func() { _ = s.Close() })

	s.onUpdate(availableCommandsNotification("research", "plan"))
	ev := oneBufferedEvent(t, s)
	if ev.Mode != "" || ev.Text != "" {
		t.Fatalf("a commands update filled Mode %q / Text %q", ev.Mode, ev.Text)
	}
	if ev.State.Commands == nil || len(ev.State.Commands.Commands) != 2 ||
		ev.State.Commands.Commands[0].Name != "research" {
		t.Fatalf("the commands section is %+v", ev.State.Commands)
	}
	if ev.State.Plugins == nil {
		t.Fatalf("a commands update re-resolves the plugins and must say so: %+v", ev.State)
	}

	s.onUpdate(currentModeNotification("plan"))
	ev = oneBufferedEvent(t, s)
	if ev.Mode != "plan" {
		t.Fatalf("an agent mode update must keep Event.Mode: %+v", ev)
	}
	if ev.State.Mode == nil || *ev.State.Mode != "plan" || *ev.State.Mode != s.Snapshot().CurrentMode {
		t.Fatalf("the mode section is %+v, snapshot %q", ev.State.Mode, s.Snapshot().CurrentMode)
	}

	s.onUpdate(configOptionNotification("effort", "high"))
	ev = oneBufferedEvent(t, s)
	if ev.Mode != "" || ev.Text != "" {
		t.Fatalf("a config update filled Mode %q / Text %q", ev.Mode, ev.Text)
	}
	if ev.State.Config == nil || len(ev.State.Config.Options) != 1 ||
		ev.State.Config.Options[0].Current != "high" {
		t.Fatalf("the config section is %+v", ev.State.Config)
	}

	s.onUpdate(sessionInfoNotification("the agent's own title"))
	ev = oneBufferedEvent(t, s)
	if ev.Text != "the agent's own title" {
		t.Fatalf("an agent title must keep Event.Text: %+v", ev)
	}
	if ev.State.Title == nil || *ev.State.Title != "the agent's own title" {
		t.Fatalf("the title section is %+v", ev.State.Title)
	}
}

// settle drops everything a started session has already published — Start's
// install delta, and the catalog the fake agent advertises on its way up — so
// that what a case drives next is the only thing in the buffer. The wait is
// for the catalog: it is sent on the read loop right after session/new's reply
// and can otherwise land after Start has returned.
func settle(t *testing.T, s *session) {
	t.Helper()
	awaitCatalog(t, s)
	_ = s.log.Flush(context.Background(), s.done)
	drainBuffered(s)
}

// awaitCatalog is settle's barrier without its draining, for a case that reads
// the events rather than discarding them: the catalog is advertised on the read
// loop right after session/new's reply, so it can land after Start has
// returned, and a fold drained before it — compared with a snapshot read after
// it — would differ on the commands section alone.
//
// The channel is the whole barrier: commandsApplied is closed in the section
// that applies the catalog, after that section has enqueued the delta saying so
// (live.go's UpdateAvailableCommands), so a flush made on waking from it covers
// that delta.
func awaitCatalog(t *testing.T, s *session) {
	t.Helper()
	select {
	case <-s.commandsApplied:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent never advertised its commands")
	}
}

// oneBufferedEvent is the single event the site just driven left in the
// primary's buffer — read without blocking, because the site flushed before it
// returned — and it must carry a state delta: after plan 021's C10 no
// production path publishes a bare EventMeta.
func oneBufferedEvent(t *testing.T, s *session) Event {
	t.Helper()
	evs := drainBuffered(s)
	if len(evs) != 1 {
		t.Fatalf("want exactly one buffered event, got:\n%s", formatEvents(evs))
	}
	if evs[0].Type != EventMeta || evs[0].State == nil {
		t.Fatalf("want a meta carrying a delta, got:\n%s", formatEvents(evs))
	}
	return evs[0]
}

// TestACrazeInitiatedSettingsChangeFillsNeitherModeNorText is A20 at the seam:
// every setter craze drives carries its payload in Event.State alone. A delta
// with Event.Mode set would retire whatever plan offer is on screen when it
// arrives, and one with Event.Text set would print a `title` line for a
// /rename and write it to the index as an agent title (plan 021 correction 20).
func TestACrazeInitiatedSettingsChangeFillsNeitherModeNorText(t *testing.T) {
	s := startScript(t, "echo", false)
	if _, err := s.SetMode(t.Context(), "c-1/1", "plan"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if err := s.SetTitle("c-1/2", "renamed"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	_ = s.log.Flush(context.Background(), s.done)
	var seen []string
	for _, ev := range drainBuffered(s) {
		if ev.Type != EventMeta || ev.State == nil {
			continue
		}
		if ev.Cause == "" {
			// The agent's own updates, which do fill those fields and are what
			// the rule is a distinction from: this session's fake agent answers
			// a set_mode with a current_mode_update of its own.
			continue
		}
		if ev.Mode != "" || ev.Text != "" {
			t.Fatalf("a craze-initiated delta filled Mode %q / Text %q: %+v", ev.Mode, ev.Text, ev)
		}
		switch {
		case ev.State.Mode != nil:
			seen = append(seen, "mode="+*ev.State.Mode+" cause="+ev.Cause)
		case ev.State.Title != nil:
			seen = append(seen, "title="+*ev.State.Title+" cause="+ev.Cause)
		}
	}
	want := []string{"mode=plan cause=c-1/1", "title=renamed cause=c-1/2"}
	if !equalStrings(seen, want) {
		t.Fatalf("the deltas are %q, want %q", seen, want)
	}
}

// TestStateOrderIsEventOrderForTitle is CodeRabbit's finding 7, as a test: the
// agent naming the session and a /rename racing each other.
//
// The schedule it is about was "the read loop mutates, unlocks, and publishes
// later, while a rename mutates, pins and publishes first": a client folding by
// Seq then ends on the agent's title while Snapshot answers the pinned one, for
// ever. It cannot happen now because each mutation and its delta are one
// locked section, and the property is the same whichever way the race goes —
// **the last title delta by Seq is what the snapshot holds**.
func TestStateOrderIsEventOrderForTitle(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := newTestSession(t, Options{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.onUpdate(sessionInfoNotification("the agent's own title")) }()
		go func() {
			defer wg.Done()
			if err := s.SetTitle("c-1/1", "renamed"); err != nil {
				t.Errorf("SetTitle: %v", err)
			}
		}()
		wg.Wait()
		_ = s.log.Flush(context.Background(), s.done)
		last, n := "", 0
		for _, ev := range drainBuffered(s) {
			if ev.Type == EventMeta && ev.State != nil && ev.State.Title != nil {
				last, n = *ev.State.Title, n+1
			}
		}
		if n == 0 {
			t.Fatal("neither the agent nor the rename published a title")
		}
		if got := s.Snapshot().Title; got != last {
			t.Fatalf("the last title delta is %q and the snapshot says %q", last, got)
		}
		_ = s.Close()
	}
}

// TestStateOrderIsEventOrderForMode is the same finding on the mode: a SetMode
// the agent has taken, and a current_mode_update the agent sent itself, racing
// each other over a real session. Whatever order they land in, the snapshot
// holds what the last mode delta by Seq says.
func TestStateOrderIsEventOrderForMode(t *testing.T) {
	for i := 0; i < 20; i++ {
		s := newTestSession(t, Options{
			Binary:    fakeAgentPath(t),
			ExtraArgs: []string{"-script=echo"},
			Workspace: t.TempDir(),
			Stderr:    io.Discard,
		})
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Start's own install delta carries a Mode section too, so it is taken
		// off here: counting it as one of the three below would stop the read
		// one delta short and compare the snapshot against something that is
		// not the last.
		settle(t, s)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.SetMode(context.Background(), "c-1/1", "plan"); err != nil {
				t.Errorf("SetMode: %v", err)
			}
		}()
		go func() { defer wg.Done(); s.onUpdate(currentModeNotification("ask")) }()
		wg.Wait()
		// Three deltas, and then the session is quiet: the two changes above
		// and the current_mode_update the fake agent answers a set_mode with.
		// Waiting for all three is what makes the comparison below a fact
		// rather than a race with the read loop.
		last := lastModeDelta(t, s, 3)
		if got := s.Snapshot().CurrentMode; got != last {
			t.Fatalf("the last mode delta is %q and the snapshot says %q", last, got)
		}
		_ = s.Close()
	}
}

// lastModeDelta reads the session's events until it has seen want mode deltas,
// and answers with the last one's value.
func lastModeDelta(t *testing.T, s *session, want int) string {
	t.Helper()
	last, n := "", 0
	for n < want {
		select {
		case ev := <-s.Events():
			if ev.Type == EventMeta && ev.State != nil && ev.State.Mode != nil {
				last, n = *ev.State.Mode, n+1
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d mode deltas arrived", n, want)
		}
	}
	return last
}

// TestForcedStateOrderForTheTitle is TestStateOrderIsEventOrderForTitle with
// the sampling taken out. SetTitle's whole body is one locked section and it
// asks no provider, so there are exactly TWO schedules — the agent's update
// entirely before it, or entirely after it — and both are run here, once each.
func TestForcedStateOrderForTheTitle(t *testing.T) {
	t.Run("the agent names the session first", func(t *testing.T) {
		s := newTestSession(t, Options{})
		t.Cleanup(func() { _ = s.Close() })
		s.onUpdate(sessionInfoNotification("the agent's own title"))
		if err := s.SetTitle("c-1/1", "renamed"); err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		wantLastTitleDelta(t, s, "renamed", 2)
	})
	t.Run("the rename lands first and pins", func(t *testing.T) {
		s := newTestSession(t, Options{})
		t.Cleanup(func() { _ = s.Close() })
		if err := s.SetTitle("c-1/1", "renamed"); err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		// Refused by the pin, so it publishes nothing: one delta, not two.
		s.onUpdate(sessionInfoNotification("the agent's own title"))
		wantLastTitleDelta(t, s, "renamed", 1)
	})
}

// wantLastTitleDelta flushes, reads what the session buffered, and holds the
// property both orders share: the snapshot is what the LAST title delta says,
// and there were exactly want of them.
func wantLastTitleDelta(t *testing.T, s *session, title string, want int) {
	t.Helper()
	_ = s.log.Flush(context.Background(), s.done)
	last, n := "", 0
	evs := drainBuffered(s)
	for _, ev := range evs {
		if ev.Type == EventMeta && ev.State != nil && ev.State.Title != nil {
			last, n = *ev.State.Title, n+1
		}
	}
	if n != want {
		t.Fatalf("%d title deltas, want %d:\n%s", n, want, formatEvents(evs))
	}
	if last != title || s.Snapshot().Title != last {
		t.Fatalf("the last title delta is %q, the snapshot says %q, want %q", last, s.Snapshot().Title, title)
	}
}

// TestForcedStateOrderForTheMode is the mode's forced schedule, and the one
// that needs a barrier: unlike SetTitle, SetMode asks the provider first, and
// the gap between the provider taking the change and the session writing it is
// the only window in which the agent's own update could interleave. The whole
// of that update — mutate, enqueue, flush — is run inside it here.
//
// Whatever else the fake agent says about the mode it was asked for, the
// property is the one the sampled test samples for: the snapshot holds what the
// last mode delta by Seq says, and the update forced into the window is ordered
// before the setter's own delta rather than lost behind it.
func TestForcedStateOrderForTheMode(t *testing.T) {
	s := startScript(t, "echo", false)
	settle(t, s)
	var once sync.Once
	s.beforeSetSection = func() {
		once.Do(func() { s.onUpdate(currentModeNotification("ask")) })
	}
	if _, err := s.SetMode(context.Background(), "c-1/1", "plan"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	// Three deltas: the forced update, the setter's own, and the
	// current_mode_update the fake answers a set_mode with.
	evs := modeDeltas(t, s, 3)
	forced, own := -1, -1
	last := ""
	for i, ev := range evs {
		if *ev.State.Mode == "ask" && forced < 0 {
			forced = i
		}
		if ev.Cause == "c-1/1" {
			own = i
		}
		last = *ev.State.Mode
	}
	if forced < 0 || own < 0 || forced > own {
		t.Fatalf("the forced update is at %d and the setter's delta at %d:\n%s", forced, own, formatEvents(evs))
	}
	if got := s.Snapshot().CurrentMode; got != last {
		t.Fatalf("the last mode delta is %q and the snapshot says %q", last, got)
	}
}

// modeDeltas reads the session's events until it has seen want mode deltas and
// answers with them, in order.
func modeDeltas(t *testing.T, s *session, want int) []Event {
	t.Helper()
	var out []Event
	for len(out) < want {
		select {
		case ev := <-s.Events():
			if ev.Type == EventMeta && ev.State != nil && ev.State.Mode != nil {
				out = append(out, ev)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d mode deltas arrived", len(out), want)
		}
	}
	return out
}

// TestAConfigBackedModelChangeMovesTheModelSection is r23 finding 3 at the
// author. An agent with no session/set_model keeps its model among its config
// options, and `/model`'s fallback sets it there. Before the fix that moved the
// option and nothing else: Snapshot().CurrentModel stayed on the old model for
// `craze prompt` and the status row, and the MODEL section's revision stood
// still while the display had already moved — so a delayed answer about the
// model was judged against a revision no config-backed change could ever
// advance.
func TestAConfigBackedModelChangeMovesTheModelSection(t *testing.T) {
	t.Run("the setter", func(t *testing.T) {
		s := startScript(t, "modelconfig", false)
		if got := s.Snapshot().CurrentModel; got != "default" {
			t.Fatalf("the session started on %q", got)
		}
		out, err := s.SetConfig(context.Background(), "c-1/1", "model", "composer", "")
		if err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		if out.Value != "composer" {
			t.Fatalf("SetConfig confirmed %q", out.Value)
		}
		if got := s.Snapshot().CurrentModel; got != "composer" {
			t.Fatalf("the snapshot's model is %q: a config-backed model change is a model change", got)
		}
		_ = s.log.Flush(context.Background(), s.done)
		var own *Event
		for _, ev := range drainBuffered(s) {
			if ev.Type == EventMeta && ev.Cause == "c-1/1" {
				own = &ev
			}
		}
		if own == nil {
			t.Fatal("the setter published no delta of its own")
		}
		if own.State.Model == nil || *own.State.Model != "composer" || own.State.Config == nil {
			t.Fatalf("the delta carries %+v, want both sections", own.State)
		}
		if own.State.Config.Options[len(own.State.Config.Options)-1].Current != "composer" {
			t.Fatalf("the config section is %+v", own.State.Config)
		}
	})
	t.Run("the agent's own update", func(t *testing.T) {
		s := startScript(t, "modelconfig", false)
		settle(t, s)
		s.onUpdate(modelConfigNotification("composer"))
		ev := oneBufferedEvent(t, s)
		if ev.State.Model == nil || *ev.State.Model != "composer" || ev.State.Config == nil {
			t.Fatalf("the update carries %+v, want both sections", ev.State)
		}
		if got := s.Snapshot().CurrentModel; got != "composer" {
			t.Fatalf("the snapshot's model is %q", got)
		}
		// A second update that merely re-lists the same value says nothing
		// about the model: it is not a change, and adopting it would let a
		// stale echo of the option write over a model session/set_model set.
		s.onUpdate(modelConfigNotification("composer"))
		ev = oneBufferedEvent(t, s)
		if ev.State.Model != nil {
			t.Fatalf("a config update that changed no model carried the model section: %+v", ev.State)
		}
	})
	t.Run("an agent with no model option is untouched", func(t *testing.T) {
		s := startScript(t, "echo", false)
		settle(t, s)
		before := s.Snapshot().CurrentModel
		s.onUpdate(configOptionNotification("effort", "high"))
		ev := oneBufferedEvent(t, s)
		if ev.State.Model != nil {
			t.Fatalf("an ordinary config update carried the model section: %+v", ev.State)
		}
		if got := s.Snapshot().CurrentModel; got != before {
			t.Fatalf("the model moved from %q to %q on a config update", before, got)
		}
	})
	// r25 finding 3: the model option's FIRST appearance. A session that started
	// without one — `echo` advertises an effort and a fast toggle and no model
	// category at all — learns its model from the update that introduces it.
	// Before the fix "there was no previous value" meant "say nothing", and
	// since every later update then HAD a previous value equal to the new one,
	// nothing ever repaired CurrentModel: the status row and the snapshot
	// disagreed with the agent's own option for the rest of the session.
	t.Run("the option's first appearance", func(t *testing.T) {
		s := startScript(t, "echo", false)
		settle(t, s)
		if got := s.Snapshot().CurrentModel; got != "default" {
			t.Fatalf("the session started on %q", got)
		}
		if ModelConfigOption(s.Snapshot()) != nil {
			t.Fatal("the session already had a model option, so nothing is introduced here")
		}
		s.onUpdate(modelConfigNotification("composer"))
		ev := oneBufferedEvent(t, s)
		if ev.State.Model == nil || *ev.State.Model != "composer" || ev.State.Config == nil {
			t.Fatalf("the introduction carries %+v, want both sections", ev.State)
		}
		if got := s.Snapshot().CurrentModel; got != "composer" {
			t.Fatalf("the snapshot's model is %q", got)
		}
	})
	// The same first appearance, naming the model the session is ALREADY on:
	// nothing changed, so nothing is said about the model.
	t.Run("a first appearance that agrees says nothing", func(t *testing.T) {
		s := startScript(t, "echo", false)
		settle(t, s)
		s.onUpdate(modelConfigNotification(s.Snapshot().CurrentModel))
		ev := oneBufferedEvent(t, s)
		if ev.State.Model != nil {
			t.Fatalf("an option that agrees with the model carried the model section: %+v", ev.State)
		}
	})
	// The stale re-list, which is the case the "only a change speaks" rule
	// exists for and which the first-appearance rule must not reopen:
	// session/set_model moved the model, and the agent then re-lists its options
	// carrying the OLD value. That is not a change — the option is at the value
	// it was already at — and it must not write itself over the model the setter
	// set.
	//
	// Since plan 025 a SetModel over a catalog with a model option does not use
	// session/set_model at all: it sets that option (design 2), and the option
	// then reads the new model — so a list carrying the old value afterwards is
	// the agent moving back, not a stale re-list, and the next case says so.
	// What is left of a direct set_model with an option that did not move is
	// the one below: set_model on an agent that advertised no model option, and
	// the option's first appearance carrying the pre-set value, which the
	// marker suppresses (r27 finding 2) and which leaves the option behind the
	// model. The same list again is then a re-list and still says nothing.
	t.Run("a stale re-list after SetModel", func(t *testing.T) {
		s := startScript(t, "modellate", false)
		settle(t, s)
		if _, err := s.SetModel(context.Background(), "c-1/1", "composer"); err != nil {
			t.Fatalf("SetModel: %v", err)
		}
		if got := s.Snapshot().CurrentModel; got != "composer" {
			t.Fatalf("SetModel left the session on %q", got)
		}
		// The first appearance, stale and suppressed: the option reads
		// "default", the model stays where SetModel put it.
		s.onUpdate(modelConfigNotification("default"))
		// And the same list again: the option is at the value it was already
		// at, which is not a change.
		s.onUpdate(modelConfigNotification("default"))
		if got := s.Snapshot().CurrentModel; got != "composer" {
			t.Fatalf("a stale re-list put the model back to %q", got)
		}
		_ = s.log.Flush(context.Background(), s.done)
		for _, ev := range drainBuffered(s) {
			if ev.Type == EventMeta && ev.State != nil && ev.State.Config != nil && ev.State.Model != nil &&
				ev.Cause == "" {
				t.Fatalf("a stale re-list carried the model section: %+v", ev.State)
			}
		}
	})
	t.Run("a list after a one-call SetModel is a change", func(t *testing.T) {
		s := startScript(t, "modelconfig", false)
		awaitCatalog(t, s)
		if _, err := s.SetModel(context.Background(), "c-1/1", "composer"); err != nil {
			t.Fatalf("SetModel: %v", err)
		}
		if opt := ModelConfigOption(s.Snapshot()); opt == nil || opt.Current != "composer" {
			t.Fatalf("SetModel went through the option, which should read the new model: %+v", opt)
		}
		// The option moved, so this list moves it back: the agent's word, in
		// arrival order, after its answer to the set.
		s.onUpdate(modelConfigNotification("default"))
		if got := s.Snapshot().CurrentModel; got != "default" {
			t.Fatalf("the agent moved its option back and the model stayed on %q", got)
		}
		_ = s.log.Flush(context.Background(), s.done)
		wantFoldMatchesSnapshot(t, drainBuffered(s), s.Snapshot())
	})
}

// modelConfigNotification is the `modelconfig` script's option list with the
// model option at value: what that agent sends when its model changes.
func modelConfigNotification(model string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update: mustJSON(map[string]any{
			"sessionUpdate": acp.UpdateConfigOption,
			"configOptions": []map[string]any{{
				"id": "model", "name": "Model", "category": "model", "type": "select",
				"currentValue": model,
				"options": []map[string]any{
					{"value": "default", "name": "Default"},
					{"value": "composer", "name": "Composer"},
				},
			}},
		}),
	}
}

// TestNativeConfirmsTheValueItResolved is r23 finding 4 at the session that
// really does resolve what it is asked for: an empty effort is the model's own
// default, and a model alias is not its canonical id. The outcome carries what
// the session ENDED at, captured in the section that wrote it, so it can never
// contradict the delta whose revision it is answered with.
func TestNativeConfirmsTheValueItResolved(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if got := EffortOption(s.Snapshot()); got == nil || got.Current == "" {
		t.Fatalf("the fixture's model has no default effort to resolve to: %+v", got)
	}
	want := EffortOption(s.Snapshot()).Current
	out, err := s.SetConfig(context.Background(), "", nativeEffortID, "", "")
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if out.Value != want {
		t.Fatalf("an empty effort was confirmed as %q, want the model's default %q", out.Value, want)
	}
	evs := deltaSettled(t, s)
	last := evs[len(evs)-1]
	if last.State == nil || last.State.Config == nil || last.State.Config.Options[0].Current != out.Value {
		t.Fatalf("the delta carries %+v and the answer said %q", last.State, out.Value)
	}
	// A model by a spelling that is not its id — its display name — is resolved
	// to the canonical one, which is what the snapshot and the delta both hold.
	var alias, canonical string
	for _, m := range s.Snapshot().Models {
		if m.Name != "" && m.Name != m.ID && m.ID != s.Snapshot().CurrentModel {
			alias, canonical = m.Name, m.ID
			break
		}
	}
	if alias == "" {
		t.Fatal("no model in the fixture is spelled two ways, so nothing is resolved here")
	}
	got, err := s.SetModel(context.Background(), "", alias)
	if err != nil {
		t.Fatalf("SetModel(%q): %v", alias, err)
	}
	if got.Value != canonical {
		t.Fatalf("SetModel(%q) confirmed %q, want the canonical %q", alias, got.Value, canonical)
	}
	if snap := s.Snapshot(); snap.CurrentModel != got.Value {
		t.Fatalf("the snapshot is on %q and the answer said %q", snap.CurrentModel, got.Value)
	}
}

// TestNoLiveMetaIsBare drives a whole scripted run of the live session over the
// fake agent and holds plan 021's rule that **EventMeta is never bare again**:
// after C10 every meta a production path publishes carries a state delta, so a
// client never has to read one as "call Snapshot() and diff it".
func TestNoLiveMetaIsBare(t *testing.T) {
	s := startScript(t, "tool", true)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMode(t.Context(), "", "plan"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if err := s.SetTitle("", "renamed"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	_ = s.log.Flush(context.Background(), s.done)
	metas := 0
	for _, ev := range drainBuffered(s) {
		if ev.Type != EventMeta {
			continue
		}
		metas++
		if ev.State == nil {
			t.Fatalf("a bare EventMeta: %+v", ev)
		}
	}
	if metas == 0 {
		t.Fatal("the run published no meta at all, so nothing was checked")
	}
}

// TestNoNativeMetaIsBare is the same rule for the native adapter, whose every
// meta is a model or effort switch (announceCurrent).
func TestNoNativeMetaIsBare(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "high", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTitle("", "renamed"); err != nil {
		t.Fatal(err)
	}
	metas := 0
	for _, ev := range deltaSettled(t, s) {
		if ev.Type != EventMeta {
			continue
		}
		metas++
		if ev.State == nil {
			t.Fatalf("a bare EventMeta: %+v", ev)
		}
	}
	if metas != 3 {
		t.Fatalf("%d metas, want one per change", metas)
	}
}

// TestADeltaSharesNoMemoryWithTheSnapshot: a section is carried in full, and it
// is a copy — the session goes on mutating its own snapshot, and an event is
// immutable once it has been enqueued (plan 021 X41 made the same rule for the
// ask registry).
func TestADeltaSharesNoMemoryWithTheSnapshot(t *testing.T) {
	s := newTestSession(t, Options{})
	t.Cleanup(func() { _ = s.Close() })
	s.onUpdate(configOptionNotification("effort", "high"))
	ev := oneBufferedEvent(t, s)
	s.mu.Lock()
	s.snap.Config[0].Current = "mutated after the fact"
	s.mu.Unlock()
	if got := ev.State.Config.Options[0].Current; got != "high" {
		t.Fatalf("the delta followed the snapshot: %q", got)
	}
	s.onUpdate(availableCommandsNotification("research"))
	ev = oneBufferedEvent(t, s)
	s.mu.Lock()
	s.snap.Commands[0].Name = "mutated after the fact"
	s.mu.Unlock()
	if got := ev.State.Commands.Commands[0].Name; got != "research" {
		t.Fatalf("the delta followed the snapshot: %q", got)
	}
}

// folded is what a client that folds the settings sections of the stream ends
// up believing: the six sections a StateDelta carries, each one the last delta
// that named it. It is the thing that has to equal Snapshot() — if a section is
// installed without a delta, the fold stays behind for ever and no later event
// repairs it (r23 finding 2).
type folded struct {
	Title, Mode, Model string
	Config             []ConfigOption
	Commands           []CommandInfo
	Plugins            []PluginCommand
}

func foldDeltas(evs []Event) folded {
	var f folded
	for _, ev := range evs {
		if ev.Type != EventMeta || ev.State == nil {
			continue
		}
		st := ev.State
		if st.Title != nil {
			f.Title = *st.Title
		}
		if st.Mode != nil {
			f.Mode = *st.Mode
		}
		if st.Model != nil {
			f.Model = *st.Model
		}
		if st.Config != nil {
			f.Config = st.Config.Options
		}
		if st.Commands != nil {
			f.Commands = st.Commands.Commands
		}
		if st.Plugins != nil {
			f.Plugins = st.Plugins.Plugins
		}
	}
	return f
}

// snapSections is the same six sections read off the snapshot, which is the
// answer the fold has to match.
func snapSections(snap Snapshot) folded {
	return folded{
		Title: snap.Title, Mode: snap.CurrentMode, Model: snap.CurrentModel,
		Config: snap.Config, Commands: snap.Commands, Plugins: snap.Plugins,
	}
}

// sameList compares two list sections, reading nil and empty as the same
// thing: a client folding one section at a time ends on the same values either
// way, and the values are what this is about.
func sameList[T any](a, b []T) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// wantFoldMatchesSnapshot compares the two section by section, so a failure
// names the section that drifted rather than printing two structs.
func wantFoldMatchesSnapshot(t *testing.T, evs []Event, snap Snapshot) {
	t.Helper()
	got, want := foldDeltas(evs), snapSections(snap)
	for _, c := range []struct{ name, got, want string }{
		{"title", got.Title, want.Title},
		{"mode", got.Mode, want.Mode},
		{"model", got.Model, want.Model},
	} {
		if c.got != c.want {
			t.Errorf("the folded %s is %q, the snapshot's is %q\n%s", c.name, c.got, c.want, formatEvents(evs))
		}
	}
	if !sameList(got.Config, want.Config) {
		t.Errorf("the folded config is %+v, the snapshot's is %+v\n%s", got.Config, want.Config, formatEvents(evs))
	}
	if !sameList(got.Commands, want.Commands) {
		t.Errorf("the folded commands are %+v, the snapshot's are %+v\n%s", got.Commands, want.Commands, formatEvents(evs))
	}
	if !sameList(got.Plugins, want.Plugins) {
		t.Errorf("the folded plugins are %+v, the snapshot's are %+v\n%s", got.Plugins, want.Plugins, formatEvents(evs))
	}
}

// TestStartAndLoadFoldToTheSnapshot is r23 finding 2: every author that
// INSTALLS or overwrites a section says so in a delta, so a client that folds
// the stream and a client that reads Snapshot() agree — at start-up and after a
// resume, not only once the session is running.
//
// The concrete bug: a session/load whose replay carried a mode and a config
// update produced deltas for both, and then the load result silently replaced
// them (another mode, and no options at all, because cursor's load result
// carries none). The folding client kept the replayed values for ever.
func TestStartAndLoadFoldToTheSnapshot(t *testing.T) {
	t.Run("a new session", func(t *testing.T) {
		s := startScript(t, "echo", false)
		awaitCatalog(t, s)
		_ = s.log.Flush(context.Background(), s.done)
		wantFoldMatchesSnapshot(t, drainBuffered(s), s.Snapshot())
	})
	t.Run("a new session with --mode and --model", func(t *testing.T) {
		s := startScriptOpts(t, "echo", Options{Mode: "plan", Model: "gpt-5"})
		awaitCatalog(t, s)
		_ = s.log.Flush(context.Background(), s.done)
		evs := drainBuffered(s)
		wantFoldMatchesSnapshot(t, evs, s.Snapshot())
		if got := foldDeltas(evs); got.Mode != "plan" || got.Model != "gpt-5" {
			t.Fatalf("the fold is on %q/%q, want the overrides", got.Mode, got.Model)
		}
	})
	t.Run("a load the replay contradicts", func(t *testing.T) {
		s := newLoadSession(t, "load-settings", nil)
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		_ = s.log.Flush(context.Background(), s.done)
		evs := drainBuffered(s)
		// The replay really did say something else, or the case is vacuous.
		var replayed []string
		for _, ev := range evs {
			if ev.Type == EventMeta && ev.State != nil && ev.State.Mode != nil && ev.Replayed && ev.State.Config == nil {
				replayed = append(replayed, *ev.State.Mode)
			}
		}
		if !slices.Contains(replayed, "plan") {
			t.Fatalf("the replay carried no mode update to contradict: %v\n%s", replayed, formatEvents(evs))
		}
		snap := s.Snapshot()
		if snap.CurrentMode != "agent" || len(snap.Config) != 0 {
			t.Fatalf("the load result did not contradict the replay: %q %+v", snap.CurrentMode, snap.Config)
		}
		wantFoldMatchesSnapshot(t, evs, snap)
	})
	t.Run("a load with a pinned title", func(t *testing.T) {
		s := newLoadSession(t, "grok-load", func(o *Options) {
			o.Title = "yesterday's thread"
			o.TitlePinned = true
		})
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		_ = s.log.Flush(context.Background(), s.done)
		evs := drainBuffered(s)
		if got := foldDeltas(evs).Title; got != "yesterday's thread" {
			t.Fatalf("the seeded title folded to %q", got)
		}
		// The seed is craze's own row from the index, so it prints no title
		// line and is never written back as an agent title (A20).
		for _, ev := range evs {
			if ev.Type == EventMeta && ev.State != nil && ev.State.Title != nil && ev.Text != "" {
				t.Fatalf("the seeded title filled Event.Text: %+v", ev)
			}
		}
		wantFoldMatchesSnapshot(t, evs, s.Snapshot())
	})
	t.Run("a load with --mode and --model over it", func(t *testing.T) {
		s := newLoadSession(t, "grok-load", func(o *Options) {
			o.Mode = "plan"
			o.Model = "grok-4.6"
		})
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		_ = s.log.Flush(context.Background(), s.done)
		evs := drainBuffered(s)
		if got := foldDeltas(evs); got.Mode != "plan" || got.Model != "grok-4.6" {
			t.Fatalf("the post-load overrides folded to %q/%q", got.Mode, got.Model)
		}
		wantFoldMatchesSnapshot(t, evs, s.Snapshot())
	})
	t.Run("native start", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.session(Options{})
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		evs := deltaSettled(t, s)
		snap := s.Snapshot()
		// Native has no title at start-up and never publishes the one its first
		// prompt gives it (X47), so the title is the one section the fold does
		// not carry — and it is empty here, which is what makes that safe.
		if snap.Title != "" {
			t.Fatalf("a freshly started native session is named %q", snap.Title)
		}
		wantFoldMatchesSnapshot(t, evs, snap)
	})
}

// TestTheInstallDeltaIsCrazesOwn: initialisation is nobody's agent update, so
// the install carries its payload in Event.State alone — Event.Mode empty (it
// would retire a plan offer) and Event.Text empty (it would print a `title`
// line and write an agent title to the index).
func TestTheInstallDeltaIsCrazesOwn(t *testing.T) {
	s := startScript(t, "echo", false)
	_ = s.log.Flush(context.Background(), s.done)
	metas := 0
	for _, ev := range drainBuffered(s) {
		if ev.Type != EventMeta || ev.State == nil || ev.Cause != "" {
			continue
		}
		if ev.State.Commands == nil && ev.State.Mode == nil && ev.State.Model == nil {
			continue
		}
		metas++
		if ev.Mode != "" || ev.Text != "" {
			t.Fatalf("an install delta filled Mode %q / Text %q: %+v", ev.Mode, ev.Text, ev)
		}
	}
	if metas == 0 {
		t.Fatal("the session published no install delta at all, so nothing was checked")
	}
}

// TestStartReturnsWithThePrimaryFullAndNobodyReading is r25 finding 1, as the
// reviewer's own schedule: a caller — `craze prompt` is one — calls Start and
// does not begin reading the primary until it has returned, with the primary
// already full when the session comes up.
//
// With Start flushing its install delta that was a deadlock: the log's drainer
// waited for room in the primary, and the only goroutine that could make room
// was waiting for Start. The flush used context.Background(), so nothing of the
// caller's could break it either. The install is enqueued and left in the
// outbox now, so Start returns whatever the primary holds and whether or not
// anyone is reading it (Session.Start).
//
// The live half uses `nocommands` — a fake agent that advertises no catalog —
// on purpose, and the reason is worth recording: an update that arrives BEFORE
// session/new's reply is buffered by the ACP client and flushed inside
// NewSession, on **Start's own goroutine** (acp/client.go's
// flushSessionUpdates), where onUpdate's meta arms wait for the primary exactly
// as they do on the read loop. That is a second way to wedge a start with a
// full primary, it is older than this plan — before settings were deltas the
// same arms called emit, which publishes straight into the primary — and it is
// bounded by the session's own close rather than by the caller. An agent that
// says nothing before its reply takes it out of the schedule, which leaves this
// test measuring the one thing finding 1 is about.
func TestStartReturnsWithThePrimaryFullAndNobodyReading(t *testing.T) {
	t.Run("a live session", func(t *testing.T) {
		s := newTestSession(t, Options{
			Binary:    fakeAgentPath(t),
			ExtraArgs: []string{"-script=nocommands"},
			Workspace: t.TempDir(),
			Stderr:    io.Discard,
		})
		fillPrimary(t, s.log)
		within(t, "Start with a primary nobody reads", func() {
			if err := s.Start(t.Context()); err != nil {
				t.Errorf("Start: %v", err)
			}
		})
	})
	t.Run("a native session", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.session(Options{})
		fillPrimary(t, s.log)
		within(t, "Start with a primary nobody reads", func() {
			if err := s.Start(t.Context()); err != nil {
				t.Errorf("Start: %v", err)
			}
		})
	})
}

// TestAnUpdateAppliedBeforeTheInstallSurvivesIt is r27 finding 1. session/new's
// reply installs the session id in the ACP client, and from that instant the
// read loop dispatches live updates to onUpdate — while Start is still on its
// way to the section that installs the snapshot that reply carried. An update
// that lands in that window is NEWER than the response, and the install used to
// assign the response over it: the snapshot rolled back, and the install delta
// contradicted the delta the update had already enqueued, so a client folding
// the stream ended on the older value for good.
//
// Both halves of the window are here. "After the reply" is the read loop's
// version, forced with the session's install barrier rather than sampled;
// "before the reply" is the other half, where the agent speaks first and the
// ACP client flushes its buffer inside NewSession, on Start's own goroutine
// (acp's flushSessionUpdates). In each case the update contradicts the response
// on three sections at once, and Snapshot() and the fold must both end on the
// update's values.
func TestAnUpdateAppliedBeforeTheInstallSurvivesIt(t *testing.T) {
	t.Run("after session/new's reply", func(t *testing.T) {
		s := newTestSession(t, Options{
			Binary:    fakeAgentPath(t),
			ExtraArgs: []string{"-script=modelconfig"},
			Workspace: t.TempDir(),
			Stderr:    io.Discard,
		})
		// The barrier stands exactly where the read loop's update would land:
		// after the reply, before the install. Driving onUpdate from it is the
		// read loop's own call, on a goroutine the install cannot outrun.
		s.beforeInstall = func() {
			s.onUpdate(currentModeNotification("plan"))
			s.onUpdate(modelConfigNotification("composer"))
			s.onUpdate(sessionInfoNotification("named before the install"))
		}
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		wantTheInstallKeptTheUpdate(t, s, "named before the install")
	})
	t.Run("before session/new's reply", func(t *testing.T) {
		s := startScript(t, "preinstall", false)
		wantTheInstallKeptTheUpdate(t, s, "named before the reply")
	})
}

// wantTheInstallKeptTheUpdate is the assertion both halves share: the mode, the
// title, the model and the config option the update moved all survive the
// install — in Snapshot(), and in the fold a client builds from the deltas,
// which is the half the finding is really about.
func wantTheInstallKeptTheUpdate(t *testing.T, s *session, title string) {
	t.Helper()
	awaitCatalog(t, s)
	_ = s.log.Flush(context.Background(), s.done)
	evs := drainBuffered(s)
	snap := s.Snapshot()
	if snap.CurrentMode != "plan" {
		t.Fatalf("the install put the mode back to %q", snap.CurrentMode)
	}
	if snap.Title != title {
		t.Fatalf("the install left the title %q, want %q", snap.Title, title)
	}
	if snap.CurrentModel != "composer" {
		t.Fatalf("the install put the model back to %q", snap.CurrentModel)
	}
	if opt := ModelConfigOption(snap); opt == nil || opt.Current != "composer" {
		t.Fatalf("the install put the config option back: %+v", snap.Config)
	}
	// State order is event order: the install delta is the LAST word and it
	// agrees with the update rather than undoing it.
	wantFoldMatchesSnapshot(t, evs, snap)
	if got := foldDeltas(evs); got.Mode != "plan" || got.Title != title || got.Model != "composer" {
		t.Fatalf("a client folding the stream ends on mode %q, title %q, model %q\n%s",
			got.Mode, got.Title, got.Model, formatEvents(evs))
	}
}

// TestAFirstModelOptionDoesNotUndoASetModel is r27 finding 2 and r28 finding
// 3, over an agent that really does introduce its model option late:
// `modellate` advertises none at session/new, and its first config list —
// still carrying the value the option held BEFORE session/set_model — is
// delivered by THIS TEST, synchronously, once SetModel has already returned,
// rather than raced over the wire (r28 finding 3's own fix; see
// cmd/craze-fake-agent/server.go's modellate case and main.go's doc for why).
//
// The first-appearance rule of r25 finding 3 exists because a session that
// starts without a model option has to learn its model from the update that
// introduces one. This is that rule's blind spot: with no previous option to
// compare against, a first report that is merely STALE looked exactly like a
// change, and adopting it put the model back where the setter had moved it
// from. A direct set_model made with no option advertised now leaves a marker
// that first report consumes and derives nothing from, when the report's
// value is one the marker actually names.
//
// # Why the schedule has to be forced
//
// SetModel's own locked mutation — and the marker it arms — run on the
// CALLER's goroutine, synchronously, before the call returns: that part is
// never racy. What is racy on a real wire is which goroutine reaches its own
// work first when the agent's first config list follows right behind
// session/set_model's reply: the response is delivered to a BUFFERED channel,
// so the read loop is free to move straight on to the next frame and call
// onUpdate before the caller's goroutine — which still has to be scheduled,
// return through client.SetModel, and take s.mu — gets there. On that
// schedule the notification is judged with NO marker armed yet (was == nil,
// stale == false because the map is still empty), agrees with the
// not-yet-moved CurrentModel (so no delta either), and the setter's own
// unconditional mutation lands afterwards regardless — so the test's own
// assertions passed whether or not the marker logic was there at all. Calling
// onUpdate here, after SetModel has already returned, rules that schedule
// OUT and forces the one that actually exercises the marker.
func TestAFirstModelOptionDoesNotUndoASetModel(t *testing.T) {
	s := startScript(t, "modellate", false)
	settle(t, s)
	if ModelConfigOption(s.Snapshot()) != nil {
		t.Fatal("the session already advertises a model option, so nothing is introduced here")
	}
	if got := s.Snapshot().CurrentModel; got != "default" {
		t.Fatalf("the session started on %q", got)
	}
	if _, err := s.SetModel(t.Context(), "c-1/1", "composer"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	// The marker is armed with the value CurrentModel held before this set —
	// the whole point of forcing the schedule is to check this BEFORE
	// delivering the option, not to infer it from the outcome.
	if !s.modelBeforeSet["default"] || len(s.modelBeforeSet) != 1 {
		t.Fatalf("the marker after SetModel is %v, want {default}", s.modelBeforeSet)
	}
	_ = s.log.Flush(context.Background(), s.done)
	drainBuffered(s) // SetModel's own delta, not what this test is about.
	s.onUpdate(modelConfigNotification("default"))
	ev := oneBufferedEvent(t, s)
	if ev.State.Model != nil {
		t.Fatalf("the first appearance carried the model section: %+v", ev.State)
	}
	if ev.State.Config == nil {
		t.Fatalf("the first appearance did not carry the config section: %+v", ev.State)
	}
	if s.modelBeforeSet != nil {
		t.Fatalf("the marker survived the option's first appearance: %v", s.modelBeforeSet)
	}
	snap := s.Snapshot()
	if opt := ModelConfigOption(snap); opt.Current != "default" {
		t.Fatalf("the first list carried %q, so there was nothing stale to adopt", opt.Current)
	}
	if snap.CurrentModel != "composer" {
		t.Fatalf("the option's first appearance put the model back to %q", snap.CurrentModel)
	}
	// The marker is spent, and an option that agrees with the model still says
	// nothing: nothing changed.
	s.onUpdate(modelConfigNotification("composer"))
	if ev := oneBufferedEvent(t, s); ev.State.Model != nil {
		t.Fatalf("an option that agrees with the model carried the model section: %+v", ev.State)
	}
	if got := s.Snapshot().CurrentModel; got != "composer" {
		t.Fatalf("the model moved to %q on an update that changed nothing", got)
	}
	// And a report that really MOVES the option is a change like any other —
	// even carrying "default", the very value the marker suppressed above: the
	// marker's whole life was that one first appearance, and a genuine return
	// to the pre-set value much later is indistinguishable from another stale
	// report and is NOT suppressed a second time (r28 finding 2's documented
	// residual).
	s.onUpdate(modelConfigNotification("default"))
	if ev := oneBufferedEvent(t, s); ev.State.Model == nil || *ev.State.Model != "default" {
		t.Fatalf("a real change of the option carried %+v", ev.State)
	}
	if got := s.Snapshot().CurrentModel; got != "default" {
		t.Fatalf("a real change of the option left the model on %q", got)
	}
}

// TestAFirstModelOptionCarryingAThirdValueIsARealChange is r28 finding 2's
// "ANY OTHER value" arm: the marker suppresses only a value the agent could
// honestly have held before craze's own set (or leaves alone a value that
// already equals the current model, a no-op by the surrounding "only a
// change speaks" rule) — it never again suppresses the model outright the
// way the pre-fix marker did for every value on the option's first
// appearance, regardless of what it carried.
func TestAFirstModelOptionCarryingAThirdValueIsARealChange(t *testing.T) {
	s := startScript(t, "modellate", false)
	settle(t, s)
	if _, err := s.SetModel(t.Context(), "c-1/1", "composer"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	_ = s.log.Flush(context.Background(), s.done)
	drainBuffered(s) // SetModel's own delta, not what this test is about.
	// Neither "default" (the pre-set value the marker names) nor "composer"
	// (the current model): a value the marker must not touch.
	s.onUpdate(modelConfigNotification("opus"))
	ev := oneBufferedEvent(t, s)
	if ev.State.Model == nil || *ev.State.Model != "opus" || ev.State.Config == nil {
		t.Fatalf("a third value on the first appearance carried %+v, want both sections", ev.State)
	}
	if got := s.Snapshot().CurrentModel; got != "opus" {
		t.Fatalf("a third value on the first appearance left the model on %q", got)
	}
	if s.modelBeforeSet != nil {
		t.Fatalf("the marker survived the option's first appearance: %v", s.modelBeforeSet)
	}
}

// TestTwoSetModelsBeforeTheOptionAppearsKeepBothPreSetValues is r28 finding
// 2's own worked example: two direct set_models made before the model option
// ever appears leave a stale first report free to carry EITHER value the
// model held before one of them — never just the most recent — because a
// stale report can only echo what the agent held before a set it has not yet
// processed, and with two sets in flight that could honestly be either
// earlier value. modelBeforeSet is the SET of pre-set values since the
// marker was last empty, not a single latest one, for exactly this reason.
func TestTwoSetModelsBeforeTheOptionAppearsKeepBothPreSetValues(t *testing.T) {
	newTwoSets := func(t *testing.T) *session {
		t.Helper()
		s := startScript(t, "modellate", false)
		settle(t, s)
		if _, err := s.SetModel(t.Context(), "c-1/1", "composer"); err != nil {
			t.Fatalf("first SetModel: %v", err)
		}
		if _, err := s.SetModel(t.Context(), "c-1/2", "opus"); err != nil {
			t.Fatalf("second SetModel: %v", err)
		}
		if !s.modelBeforeSet["default"] || !s.modelBeforeSet["composer"] || len(s.modelBeforeSet) != 2 {
			t.Fatalf("the marker after two sets is %v, want {default, composer}", s.modelBeforeSet)
		}
		_ = s.log.Flush(context.Background(), s.done)
		drainBuffered(s) // both SetModels' own deltas, not what this test is about.
		return s
	}
	t.Run("the value before the FIRST set is still suppressed", func(t *testing.T) {
		s := newTwoSets(t)
		s.onUpdate(modelConfigNotification("default"))
		if ev := oneBufferedEvent(t, s); ev.State.Model != nil {
			t.Fatalf("a pre-set value from before the first set carried the model section: %+v", ev.State)
		}
		if got := s.Snapshot().CurrentModel; got != "opus" {
			t.Fatalf("a suppressed first appearance moved the model to %q", got)
		}
	})
	t.Run("the value before the SECOND set is also suppressed", func(t *testing.T) {
		s := newTwoSets(t)
		s.onUpdate(modelConfigNotification("composer"))
		if ev := oneBufferedEvent(t, s); ev.State.Model != nil {
			t.Fatalf("a pre-set value from before the second set carried the model section: %+v", ev.State)
		}
		if got := s.Snapshot().CurrentModel; got != "opus" {
			t.Fatalf("a suppressed first appearance moved the model to %q", got)
		}
	})
}

// TestTheModelMarkerIsBounded is r30 finding 6. modelBeforeSet holds one entry
// per direct session/set_model made while no model option is advertised, and
// an agent that never advertises one — `modellate` until its first config list
// — would let a long session grow it with every model change it makes.
//
// Past modelBeforeSetCap the set is dropped for a single conservative bit:
// suppress the option's FIRST appearance whatever it carries, which is what
// the marker did for every value before r28 finding 2 narrowed it. A value
// that would otherwise have been a real change is the price, and it is paid
// only by a session that has already made nine direct sets with no option in
// sight. Both are consumed by that first appearance, exactly as the set alone
// always was.
func TestTheModelMarkerIsBounded(t *testing.T) {
	s := startScript(t, "modellate", false)
	settle(t, s)
	if got := s.Snapshot().CurrentModel; got != "default" {
		t.Fatalf("the session started on %q", got)
	}
	// One more than the cap: the first modelBeforeSetCap sets fill it — the
	// value before each, starting at "default" — and the next overflows it.
	for i := 1; i <= modelBeforeSetCap+1; i++ {
		id := fmt.Sprintf("m-%d", i)
		if _, err := s.SetModel(t.Context(), fmt.Sprintf("c-1/%d", i), id); err != nil {
			t.Fatalf("SetModel %q: %v", id, err)
		}
		s.mu.Lock()
		n, any := len(s.modelBeforeSet), s.modelBeforeAny
		s.mu.Unlock()
		if n > modelBeforeSetCap {
			t.Fatalf("after %d sets the marker holds %d values, past the cap of %d", i, n, modelBeforeSetCap)
		}
		if want := i > modelBeforeSetCap; any != want {
			t.Fatalf("after %d sets the overflow bit is %v, want %v", i, any, want)
		}
	}
	s.mu.Lock()
	set, any := s.modelBeforeSet, s.modelBeforeAny
	s.mu.Unlock()
	if len(set) != 0 || !any {
		t.Fatalf("the overflowed marker is %v / %v, want the set dropped for the bit", set, any)
	}
	_ = s.log.Flush(context.Background(), s.done)
	drainBuffered(s) // every SetModel's own delta, not what this test is about.

	// The option's first appearance, carrying a value the agent never held
	// under craze: under the cap this would be a real change, and the
	// overflowed marker suppresses it instead.
	s.onUpdate(modelConfigNotification("sonnet"))
	ev := oneBufferedEvent(t, s)
	if ev.State.Model != nil {
		t.Fatalf("the first appearance under an overflowed marker carried the model section: %+v", ev.State)
	}
	if ev.State.Config == nil {
		t.Fatalf("the first appearance did not carry the config section: %+v", ev.State)
	}
	last := fmt.Sprintf("m-%d", modelBeforeSetCap+1)
	if got := s.Snapshot().CurrentModel; got != last {
		t.Fatalf("the suppressed first appearance moved the model to %q, want %q", got, last)
	}
	// And the marker is spent, both halves of it: the next report that really
	// moves the option is a change like any other.
	s.mu.Lock()
	set, any = s.modelBeforeSet, s.modelBeforeAny
	s.mu.Unlock()
	if set != nil || any {
		t.Fatalf("the marker survived the option's first appearance: %v / %v", set, any)
	}
	s.onUpdate(modelConfigNotification("opus"))
	if ev := oneBufferedEvent(t, s); ev.State.Model == nil || *ev.State.Model != "opus" {
		t.Fatalf("a real change after the first appearance carried %+v", ev.State)
	}
	if got := s.Snapshot().CurrentModel; got != "opus" {
		t.Fatalf("a real change after the first appearance left the model on %q", got)
	}
}

// TestSetTitleRefusedForRoomChangesNothing: SetTitle is the one settings verb
// with no provider behind it, so its check for room in the log is atomic with
// its mutation and it can honestly refuse — with nothing renamed and nothing
// pinned, so the agent's own title still wins afterwards.
func TestSetTitleRefusedForRoomChangesNothing(t *testing.T) {
	s := newTestSession(t, Options{})
	t.Cleanup(func() { _ = s.Close() })
	// A primary nobody reads: the drainer parks on the first send it cannot
	// make, and everything behind it stays enqueued.
	filler := make([]Event, 256)
	for i := range filler {
		filler[i] = Event{Type: EventText, Text: fmt.Sprintf("fill-%d", i)}
	}
	for s.log.OutboxRoom() {
		s.log.Enqueue(filler...)
	}
	if err := s.SetTitle("c-1/1", "renamed"); !errors.Is(err, ErrSetUnavailable) {
		t.Fatalf("SetTitle with no room: %v", err)
	}
	// Nothing renamed and nothing pinned — the pin is read here rather than
	// driven through an agent title, because the read loop's own flush would
	// wait for the drainer this test has deliberately parked.
	s.mu.Lock()
	title, pinned := s.snap.Title, s.titlePinned
	s.mu.Unlock()
	if title != "" || pinned {
		t.Fatalf("a refused rename left title %q, pinned %v", title, pinned)
	}
}
