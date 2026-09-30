package engine

import (
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The catalog hook (plan 030 §3.14): Options.CatalogChanged hears each
// committed event that may have installed a model catalog — a Config section
// sent now, a replay's end — and nothing else: not a replayed Config section
// (a load's history, which its install overrules), not a sub-agent's event,
// not an event that moves no catalog.

// TestCatalogChangedHearsEachInstall drives each kind of event through the
// observer and counts what the hook heard once the event is committed.
func TestCatalogChangedHearsEachInstall(t *testing.T) {
	config := func() *agent.StateDelta {
		return &agent.StateDelta{Config: &agent.ConfigState{Options: []agent.ConfigOption{{ID: "model", Category: "model"}}}}
	}
	model := "m"
	for _, tc := range []struct {
		name string
		ev   agent.Event
		want int32
	}{
		{"a Config section", agent.Event{Type: agent.EventMeta, State: config()}, 1},
		{"a replay's end", agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}}, 1},
		{"a replayed Config section", agent.Event{Type: agent.EventMeta, State: config(), Replayed: true}, 0},
		{"a replay's start", agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}}, 0},
		{"a sub-agent's Config section", agent.Event{Type: agent.EventMeta, State: config(), Agent: "sub-1"}, 0},
		{"a model moved, no catalog", agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Model: &model}}, 0},
		{"a text chunk", agent.Event{Type: agent.EventText, Text: "hi"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var heard atomic.Int32
			r := newRig(t, Options{CatalogChanged: func() { heard.Add(1) }})
			r.s.emit(tc.ev)
			r.sync()
			if got := heard.Load(); got != tc.want {
				t.Fatalf("the hook heard %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNoCatalogHookHearsNothing: an engine without the hook — every host but
// craze serve — commits a catalog install as it always has.
func TestNoCatalogHookHearsNothing(t *testing.T) {
	r := newRig(t, Options{})
	r.s.emit(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Config: &agent.ConfigState{}}})
	r.sync()
}
