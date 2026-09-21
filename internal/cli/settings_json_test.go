package cli

import (
	"encoding/json"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSettingsDeltasPrintOnlyAnAgentTitle is plan 021's A20 at the `--json`
// boundary, and the whole reason a craze-initiated change carries its payload
// in Event.State alone (correction 20).
//
// `craze prompt --json` gains no line kind in S1b (§3.9): a settings delta
// falls to eventJSON's EventMeta arm, which prints a `title` line for an
// EventMeta with Text and nothing at all for one without. Event.Text is filled
// only by the agent naming the session, so an agent title still prints exactly
// the line it always did, and a /rename — which sets the same title, pins it,
// and fills no Text — prints nothing.
func TestSettingsDeltasPrintOnlyAnAgentTitle(t *testing.T) {
	title := "Fake Title"
	mode := "plan"
	for _, tc := range []struct {
		name string
		ev   agent.Event
		want string
	}{
		{
			name: "the agent names the session",
			ev: agent.Event{Type: agent.EventMeta, Seq: 4, Text: title,
				State: &agent.StateDelta{Title: &title}},
			want: `{"type":"title","seq":4,"title":"Fake Title"}`,
		},
		{
			name: "a /rename",
			ev: agent.Event{Type: agent.EventMeta, Seq: 5, Cause: "c-1/2",
				State: &agent.StateDelta{Title: &title}},
		},
		{
			name: "a mode change craze asked for",
			ev: agent.Event{Type: agent.EventMeta, Seq: 6, Cause: "c-1/3",
				State: &agent.StateDelta{Mode: &mode}},
		},
		{
			name: "a mode change the agent made itself",
			ev: agent.Event{Type: agent.EventMeta, Seq: 7, Mode: mode,
				State: &agent.StateDelta{Mode: &mode}},
		},
		{
			name: "a config update",
			ev: agent.Event{Type: agent.EventMeta, Seq: 8, State: &agent.StateDelta{
				Config: &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Current: "high"}}},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, ok := eventJSON(tc.ev)
			if tc.want == "" {
				if ok {
					t.Fatalf("it printed %s, want nothing", mustMarshal(t, j))
				}
				return
			}
			if !ok {
				t.Fatalf("it printed nothing, want %s", tc.want)
			}
			if got := mustMarshal(t, j); got != tc.want {
				t.Fatalf("line\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}
