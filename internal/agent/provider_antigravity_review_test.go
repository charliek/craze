package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// Regression tests for the M1 review's findings (plan 053 reviews/M1.md),
// each on an antigravity session with no agent behind it unless it says so.

func agySession(t *testing.T) *session {
	t.Helper()
	p := AntigravityProvider()
	s := newTestSession(t, Options{Provider: &p})
	return s
}

// agySub is one invoke_subagent update of call id with status and role.
func agySub(id, status, role string) toolDelta {
	title := "Run invoke_subagent"
	return toolDelta{id: id, title: &title, status: &status, wireName: "invoke_subagent",
		rawInput: mustJSON(map[string]any{"Prompt": "p", "Role": role, "TypeName": "self"}), hasRawInput: true}
}

// inTurn opens prompt turn n on s, as prompt does under s.mu.
func agyInTurn(s *session, n int) {
	s.mu.Lock()
	s.inPrompt, s.turn = true, n
	s.mu.Unlock()
}

func agyRowOf(s *session, id string) (SubagentInfo, bool) {
	for _, sa := range s.Snapshot().Subagents {
		if sa.ID == id {
			return sa, true
		}
	}
	return SubagentInfo{}, false
}

// TestAgySettlementIsOrderedWithToolPublication (finding 1): an update the
// read loop merged before the turn's settlement, and is still publishing, is
// published before the settlement's finish — never a running row event after
// it.
func TestAgySettlementIsOrderedWithToolPublication(t *testing.T) {
	s := agySession(t)
	agyInTurn(s, 1)
	s.publishToolDelta(agySub("c1", "completed", "Runner"))
	drainEvents(s)
	merged, release := make(chan struct{}), make(chan struct{})
	s.hooks.toolPublish = func() { close(merged); <-release }
	late := make(chan struct{})
	go func() { s.publishToolDelta(agySub("c1", "completed", "Runner, renamed")); close(late) }()
	<-merged
	s.hooks.toolPublish = nil
	settled := make(chan struct{})
	go func() { s.settleToolRows(1, toolCompleted); close(settled) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-late
	<-settled
	var last *SubagentInfo
	for _, ev := range pendingEvents(s) {
		if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == "c1" {
			last = ev.Subagent
		}
	}
	if last == nil || last.Status != SubagentCompleted {
		t.Fatalf("the last row event %+v: a running row was published after its settlement", last)
	}
}

// TestAgyEvictedRowNeverReopens (finding 2): a settled row retention evicted
// is not created again, running, by a late update of its call in a later
// turn.
func TestAgyEvictedRowNeverReopens(t *testing.T) {
	s := agySession(t)
	for i := 0; i <= subagentFinishedCap; i++ {
		s.publishToolDelta(agySub(fmt.Sprintf("c%d", i), "completed", "Runner"))
	}
	if _, ok := agyRowOf(s, "c0"); ok {
		t.Fatal("retention kept the oldest row; the test needs it evicted")
	}
	agyInTurn(s, 7)
	s.publishToolDelta(agySub("c0", "completed", "Runner, late"))
	if row, ok := agyRowOf(s, "c0"); ok {
		t.Fatalf("an evicted, settled row came back: %+v", row)
	}
	if n := countStatus(s, SubagentRunning); n != 0 {
		t.Fatalf("%d rows running", n)
	}
	var tomb idTombstones
	for i := 0; i < childEvictedCap+1; i++ {
		tomb.note(fmt.Sprint(i))
	}
	if len(tomb.set) != childEvictedKeep || tomb.has("0") || !tomb.has(fmt.Sprint(childEvictedCap)) {
		t.Fatalf("tombstones unbounded: %d kept", len(tomb.set))
	}
}

// TestAgyOutsideTurnRowRetiredWithTheAgent (finding 3): a row created
// outside a turn whose call never completes is failed when the agent dies
// with no turn in flight — no turn's settlement, and no failed turn's open
// tools, would ever end it — and a turn settles only the rows it launched.
func TestAgyOutsideTurnRowRetiredWithTheAgent(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{LoadSessionID: fakeSessionIDForTests},
		"CRAZE_FAKE_AGY_REPLAY_RUNNING=1", "CRAZE_FAKE_AGY_EXIT_AFTER_LOAD=200ms").start(t)
	if row := r.row(t, "call_replay_running"); row.Status != SubagentRunning {
		t.Fatalf("the replayed running row %+v", row)
	}
	waitFor(t, "the outside-turn row retired", func() bool { return r.row(t, "call_replay_running").Status == SubagentFailed })
	// And its call's transcript row with it (plan 053 M3): the last tool
	// event of the call says failed.
	waitFor(t, "the retired call's tool event", func() bool {
		var last *ToolEvent
		for _, ev := range r.log.snapshot() {
			if ev.Type == EventTool && ev.Tool != nil && ev.Tool.ID == "call_replay_running" {
				last = ev.Tool
			}
		}
		return last != nil && last.Task != nil && last.Task.Status == SubagentFailed
	})
	// And from its own later terminal update, while the agent lives.
	s := agySession(t)
	s.publishToolDelta(agySub("c1", "in_progress", "Runner"))
	agyInTurn(s, 1)
	s.settleToolRows(1, toolCompleted)
	if row, _ := agyRowOf(s, "c1"); row.Status != SubagentRunning {
		t.Fatalf("a turn settled a row it did not launch: %+v", row)
	}
	s.publishToolDelta(agySub("c1", "completed", "Runner"))
	if row, _ := agyRowOf(s, "c1"); row.Status != SubagentCompleted {
		t.Fatalf("its own completion left it %+v", row)
	}
}

// agyCatalog is a config option list whose model option offers models.
func agyCatalog(current string, models ...string) json.RawMessage {
	opts := make([]map[string]string, 0, len(models))
	for _, m := range models {
		opts = append(opts, map[string]string{"value": m, "name": "Name " + m})
	}
	return mustJSON([]map[string]any{{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": current, "options": opts}})
}

func agyModelIDs(s *session) []string {
	var ids []string
	for _, m := range s.Snapshot().Models {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestAgyModelsFollowTheCatalog (finding 4): a later catalog — an update of
// the agent's own, or a settings reply — rebuilds the model list from its
// model option.
func TestAgyModelsFollowTheCatalog(t *testing.T) {
	s := agySession(t)
	// An installed session: before the install the list is the install's to
	// build (TestAgyInstallKeepsTheRefreshedModels).
	s.mu.Lock()
	s.installPending = false
	s.mu.Unlock()
	upd := mustJSON(map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": agyCatalog("a", "a", "b")})
	s.onUpdate(acp.SessionNotification{SessionID: "s", Update: upd})
	if got := agyModelIDs(s); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("after an update: %q", got)
	}
	if err := s.onSettingsReply(acp.SettingsReply{Method: acp.MethodSessionSetConfig, ConfigID: "model", Value: "c", ModelChange: true,
		Catalog: acp.ConfigCatalog{Present: true, Options: agyCatalog("c", "a", "c")}}); err != nil {
		t.Fatal(err)
	}
	if got := agyModelIDs(s); !slices.Equal(got, []string{"a", "c"}) || s.Snapshot().Models[1].Name != "Name c" {
		t.Fatalf("after a model reply: %q", got)
	}
	if err := s.onSettingsReply(acp.SettingsReply{Method: acp.MethodSessionSetConfig, ConfigID: "thought_level", Value: "low",
		Catalog: acp.ConfigCatalog{Present: true, Options: agyCatalog("c", "c", "d")}}); err != nil {
		t.Fatal(err)
	}
	if got := agyModelIDs(s); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("after another option's reply: %q", got)
	}
	// Cursor keeps its models block: the option does not replace it.
	c := CursorProvider()
	cs := newTestSession(t, Options{Provider: &c})
	cs.onUpdate(acp.SessionNotification{SessionID: "s", Update: upd})
	if got := agyModelIDs(cs); len(got) != 0 {
		t.Fatalf("cursor's models moved: %q", got)
	}
}

// TestAgySettledRowsEndTheirToolRows (live smoke, plan 053 M3): a settled
// invoke_subagent's call ends with the row — its last tool event carries the
// settled status, which the transcript draws the call from, and it is
// published before the turn's terminal event — at end_turn, on a cancel and
// on a failed turn.
func TestAgySettledRowsEndTheirToolRows(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		calls        []string
		cancel       bool
		want         SubagentStatus
	}{
		{"end_turn", "SUBAGENTS", []string{"call_sub_a", "call_sub_b"}, false, SubagentCompleted},
		{"cancelled", "SUBAGENT hold", []string{"call_sub_hold"}, true, SubagentCancelled},
		{"failed", "SUBAGENT error", []string{"call_sub_err"}, false, SubagentFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
			done := make(chan struct{})
			go func() { _, _ = r.s.Prompt(context.Background(), tc.prompt); close(done) }()
			if tc.cancel {
				waitFor(t, "the held call completed", func() bool {
					for _, tl := range r.s.Snapshot().Tools {
						if tl.ID == tc.calls[0] && tl.Status == toolCompleted {
							return true
						}
					}
					return false
				})
				if _, err := r.s.Cancel(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			<-done
			r.log.caughtUp(t)
			evs := r.log.snapshot()
			end := slices.IndexFunc(evs, func(ev Event) bool { return ev.Type == EventDone || ev.Type == EventError })
			if end < 0 {
				t.Fatal("no terminal event")
			}
			for _, id := range tc.calls {
				last := -1
				for i, ev := range evs {
					if ev.Type == EventTool && ev.Tool != nil && ev.Tool.ID == id {
						last = i
					}
				}
				if last < 0 {
					t.Fatalf("%s: no tool event", id)
				}
				if evs[last].Tool.Task == nil || evs[last].Tool.Task.Status != tc.want || last > end {
					t.Fatalf("%s: last tool event at %d (turn ended at %d): %+v", id, last, end, evs[last].Tool.Task)
				}
			}
		})
	}
}

// agyCatalogDeltas is every catalog section the session's deltas carried, in
// order, its outbox drained first.
func agyCatalogDeltas(t *testing.T, s *session) []CatalogState {
	t.Helper()
	s.flushDelta()
	var out []CatalogState
	for _, ev := range pendingEvents(s) {
		if ev.Type == EventMeta && ev.State != nil && ev.State.Catalog != nil {
			out = append(out, *ev.State.Catalog)
		}
	}
	return out
}

func catalogIDs(c CatalogState) []string {
	var ids []string
	for _, m := range c.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestAgyModelListReachesTheStream (review branch item 5, first finding): a
// model list rebuilt from a later catalog — an update of the agent's own, or
// a settings reply announced by its setter — reaches a client that folds the
// stream as the catalog section, each with a revision past the last; a
// catalog whose models did not change sends none.
func TestAgyModelListReachesTheStream(t *testing.T) {
	s := agySession(t)
	s.mu.Lock()
	s.installPending = false
	s.mu.Unlock()
	drainEvents(s)
	upd := func(cat json.RawMessage) {
		s.onUpdate(acp.SessionNotification{SessionID: "s", Update: mustJSON(map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": cat})})
	}
	upd(agyCatalog("a", "a", "b"))
	upd(agyCatalog("b", "a", "b"))
	got := agyCatalogDeltas(t, s)
	if len(got) != 1 || !slices.Equal(catalogIDs(got[0]), []string{"a", "b"}) || got[0].Revision != 1 || s.Snapshot().CatalogRevision != 1 {
		t.Fatalf("after two updates: %+v (revision %d)", got, s.Snapshot().CatalogRevision)
	}
	// A reply's list rides its setter's delta (here SetTitle's, the next one
	// craze enqueues).
	if err := s.onSettingsReply(acp.SettingsReply{Method: acp.MethodSessionSetConfig, ConfigID: "model", Value: "c", ModelChange: true,
		Catalog: acp.ConfigCatalog{Present: true, Options: agyCatalog("c", "a", "c")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTitle("", "x"); err != nil {
		t.Fatal(err)
	}
	got = agyCatalogDeltas(t, s)
	if len(got) != 1 || !slices.Equal(catalogIDs(got[0]), []string{"a", "c"}) || got[0].Revision != 2 {
		t.Fatalf("after a reply: %+v", got)
	}
}

// TestAgyInstallKeepsTheRefreshedModels (review branch item 5, second
// finding): a catalog update that changes the model list between session/new
// and the install — kept by the install over the reply's own — leaves the
// installed list matching it, with or without a --model reply copied back, and
// the install delta says so.
func TestAgyInstallKeepsTheRefreshedModels(t *testing.T) {
	for _, opts := range []Options{{}, {Model: "gemini-4-argon"}, {Effort: "low"}} {
		r := newAgyRig(t, "antigravity-basic", opts, "CRAZE_FAKE_AGY_PUSH_ON_NEW=gemini-5-preview").start(t)
		snap := r.s.Snapshot()
		if len(snap.Models) != 10 || snap.Models[9].ID != "gemini-5-preview" || snap.CatalogRevision == 0 {
			t.Fatalf("--model %q --effort %q: installed models %q revision %d", opts.Model, opts.Effort, agyModelIDs(r.s), snap.CatalogRevision)
		}
		r.s.flushDelta()
		r.log.caughtUp(t)
		var last *CatalogState
		for _, ev := range r.log.snapshot() {
			if ev.Type == EventMeta && ev.State != nil && ev.State.Catalog != nil {
				last = ev.State.Catalog
			}
		}
		if last == nil || last.Revision != snap.CatalogRevision || len(last.Models) != 10 {
			t.Fatalf("--model %q --effort %q: the stream's catalog %+v", opts.Model, opts.Effort, last)
		}
	}
}
