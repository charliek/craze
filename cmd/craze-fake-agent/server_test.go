package main

import "testing"

// The planmode goldens rest on one guarantee: a prompt did not overtake the mode
// change meant for it. Method names alone cannot hold it — an earlier turn's
// set_mode made every later prompt look ordered, and a set_mode that put the
// session back into plan mode still read as "implement" — so the recording
// carries the mode and which prompt each transition belongs to.
func TestPlanTurnFor(t *testing.T) {
	setMode := func(mode string) orderEntry { return orderEntry{method: orderSetMode, mode: mode} }
	prompt := orderEntry{method: orderPrompt}

	cases := []struct {
		name  string
		order []orderEntry
		n     int
		want  planTurn
	}{
		{"first plan turn", []orderEntry{prompt}, 1, planTurnPlan},
		{"its own set_mode arrived first", []orderEntry{prompt, setMode("agent"), prompt}, 2, planTurnImplement},
		{"a refinement has no mode change", []orderEntry{prompt, prompt}, 2, planTurnPlan},
		{"the prompt overtook its set_mode", []orderEntry{prompt, prompt, setMode("agent")}, 2, planTurnRaced},
		{
			// The reviewed defect: the last set_mode before the prompt is the one
			// in force, and it put the session back into plan mode.
			"switched back to plan mode",
			[]orderEntry{prompt, setMode("ask"), setMode("plan"), prompt},
			2,
			planTurnPlan,
		},
		{
			// The other half: an earlier turn's set_mode says nothing about a
			// later prompt, which here overtook its own.
			"an earlier turn's set_mode is not this prompt's",
			[]orderEntry{setMode("agent"), prompt, prompt, setMode("agent")},
			2,
			planTurnRaced,
		},
		{
			"an earlier turn's set_mode with no mode change of its own",
			[]orderEntry{setMode("agent"), prompt, prompt},
			2,
			planTurnPlan,
		},
		{
			// A set_mode after a prompt that already had its own belongs to the
			// turn after it, not to this one.
			"the next turn's set_mode has already arrived",
			[]orderEntry{setMode("agent"), prompt, setMode("plan")},
			1,
			planTurnImplement,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{order: tc.order}
			if got := s.planTurnFor(tc.n); got != tc.want {
				t.Fatalf("prompt %d of %v = %d, want %d", tc.n, tc.order, got, tc.want)
			}
		})
	}
}
