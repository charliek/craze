package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "high"); err != nil {
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
