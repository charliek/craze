package tui

import (
	"testing"
	"time"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
)

// TestTheListsStateIsTheRostersRule (plan 032 §3.12): the list groups a
// running row by roster.Row.State — the rule craze ps prints — and dates it
// by roster.Row.Since: for every status and every kind of answer, the row's
// group, its connecting mark and its since are the shared rule's. The
// negative control: a list with a rule of its own disagrees on some row.
func TestTheListsStateIsTheRostersRule(t *testing.T) {
	since := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	started := since.Add(-time.Hour)
	sessions := []*roster.Session{
		nil,
		{Activity: engine.ActivityIdle},
		{Activity: engine.ActivityWorking, Doing: "Reading"},
		{Activity: engine.ActivityStarting},
		{Activity: engine.ActivityIdle, ForeignTurn: true},
		{Activity: engine.ActivityIdle, PendingAsks: 1},
		{Activity: engine.ActivityIdle, StartFailed: true, StartErr: "no login"},
		{Activity: engine.ActivityIdle, LastTurn: &engine.LastTurn{Outcome: engine.TurnFailed, Err: "boom"}},
		{Activity: engine.ActivityError},
		{Activity: engine.ActivityError, ForeignTurn: true},
	}
	want := map[roster.State]sessState{
		roster.StateNeedsYou: sessNeedsYou, roster.StateWorking: sessWorking, roster.StateStarting: sessWorking,
		roster.StateFailed: sessFailed, roster.StateIdle: sessIdle, roster.StateUnreachable: sessUnreachable,
	}
	seen := map[roster.State]bool{}
	for _, status := range []roster.Status{roster.Connecting, roster.Reachable, roster.Unreachable} {
		for _, ready := range []bool{false, true} {
			for i, s := range sessions {
				if s != nil {
					c := *s
					c.Since = since
					s = &c
				}
				r := roster.Row{Status: status, Session: s,
					Host: roster.Host{ID: "0123456789ab", CrazeSessionID: "s-1", Ready: ready, StartedAt: started}}
				st := r.State()
				seen[st] = true
				got := sessRunningRow(r, sessKey{})
				if got.state != want[st] || got.connecting != (st == roster.StateStarting) || !got.since.Equal(r.Since()) {
					t.Errorf("%v, ready %v, session %d: the list has state %d (connecting %v) since %v; the rule says %v since %v",
						status, ready, i, got.state, got.connecting, got.since, st, r.Since())
				}
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("the table reached %d of the %d states: %v", len(seen), len(want), seen)
	}
}
