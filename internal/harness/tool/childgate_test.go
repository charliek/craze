package tool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// A sub-agent's gate (plan 026 §3.5): the mode it opened in, tightened — and
// never loosened — by the strictness its parent raises. The push from the
// parent's SetMode through the runner's registry is C3b's
// (TestChildGateTightensWithParentMode); these pin the gate's half.

// TestStrictnessRanksTheModes: agent below plan below ask, and a word that is
// no mode ranks as agent, as ModeGate judges one.
func TestStrictnessRanksTheModes(t *testing.T) {
	if Strictness(ModeAgent) >= Strictness(ModePlan) || Strictness(ModePlan) >= Strictness(ModeAsk) {
		t.Fatalf("ranks agent %d, plan %d, ask %d; want strictly increasing",
			Strictness(ModeAgent), Strictness(ModePlan), Strictness(ModeAsk))
	}
	if Strictness("bogus") != Strictness(ModeAgent) {
		t.Fatalf("an unknown mode ranks %d, want agent's %d", Strictness("bogus"), Strictness(ModeAgent))
	}
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		if got := modeOf(Strictness(mode)); got != mode {
			t.Errorf("modeOf(Strictness(%q)) = %q", mode, got)
		}
	}
}

// TestRaiseIsAMonotonicMax: a raise only ever grows the rank, so a switch
// back towards agent loosens nothing — agent→ask→agent leaves ask, and
// agent→plan→agent leaves plan — and raises racing each other settle on the
// strictest of them. A nil strictness is nothing to raise.
func TestRaiseIsAMonotonicMax(t *testing.T) {
	for _, tc := range []struct {
		raises []string
		want   string
	}{
		{[]string{ModeAgent, ModeAsk, ModeAgent}, ModeAsk},
		{[]string{ModeAgent, ModePlan, ModeAgent}, ModePlan},
		{[]string{ModeAsk, ModePlan}, ModeAsk},
		{[]string{ModePlan, ModeAsk, ModePlan, ModeAgent}, ModeAsk},
		{[]string{ModeAgent}, ModeAgent},
	} {
		var s atomic.Int32
		for _, m := range tc.raises {
			Raise(&s, m)
		}
		if got := modeOf(s.Load()); got != tc.want {
			t.Errorf("raises %v left %q, want %q", tc.raises, got, tc.want)
		}
	}
	Raise(nil, ModeAsk) // must not panic

	var s atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 64 {
		mode := []string{ModeAgent, ModePlan, ModeAsk, ModePlan}[i%4]
		wg.Go(func() {
			<-start
			Raise(&s, mode)
		})
	}
	close(start)
	wg.Wait()
	if got := modeOf(s.Load()); got != ModeAsk {
		t.Fatalf("64 racing raises settled on %q, want the strictest, ask", got)
	}
}

// TestChildGateTightensFromTheNextCheck: a child's gate judges each call under
// the stricter of the mode it opened in and its strictness, read at every
// Check, so a raise tightens from the very next call and a later raise towards
// agent never loosens it. A refusal only the raise makes says the parent
// switched; one the child's own mode makes says the mode it is in, with no
// plan file named, since a child has none. The control is a child whose
// strictness is nil: its own mode, and nothing else, judges it.
func TestChildGateTightensFromTheNextCheck(t *testing.T) {
	edit := Request{Tool: "write", Kind: KindEdit, Targets: []string{"/work/a.txt"}}
	bash := Request{Tool: "bash", Kind: KindExecute, Command: "ls"}
	read := Request{Tool: "read", Kind: KindRead, ReadOnly: true, Paths: []string{"/work/a.txt"}}
	type check struct {
		req  Request
		deny string // "" = allowed
	}
	judge := func(t *testing.T, g *ModeGate, when string, checks ...check) {
		t.Helper()
		for _, c := range checks {
			dec, err := g.Check(context.Background(), c.req)
			if err != nil {
				t.Fatalf("%s: Check(%s): %v", when, c.req.Tool, err)
			}
			switch d, denied := dec.(Deny); {
			case c.deny == "" && denied:
				t.Errorf("%s: %s refused with %q, want it allowed", when, c.req.Tool, d.Reason)
			case c.deny != "" && (!denied || d.Reason != c.deny):
				t.Errorf("%s: %s = %#v, want Deny{%q}", when, c.req.Tool, dec, c.deny)
			}
		}
	}

	t.Run("opened in agent mode", func(t *testing.T) {
		var s atomic.Int32
		g := NewChildModeGate(ModeAgent, &s, nil)
		judge(t, g, "before any raise", check{edit, ""}, check{bash, ""}, check{read, ""})
		Raise(&s, ModePlan)
		judge(t, g, "after the parent entered plan", check{edit, childPlanSwitchedText}, check{bash, ""}, check{read, ""})
		Raise(&s, ModeAgent)
		judge(t, g, "after the parent went back to agent", check{edit, childPlanSwitchedText}, check{bash, ""})
		Raise(&s, ModeAsk)
		judge(t, g, "after the parent entered ask",
			check{edit, childAskSwitchedText}, check{bash, childAskSwitchedText}, check{read, ""})
		Raise(&s, ModeAgent)
		judge(t, g, "after agent→ask→agent", check{bash, childAskSwitchedText}, check{read, ""})
		if g.Mode() != ModeAgent {
			t.Fatalf("the gate's own mode moved to %q; a raise tightens the judgement, not the mode", g.Mode())
		}
	})
	t.Run("opened in plan mode", func(t *testing.T) {
		var s atomic.Int32
		g := NewChildModeGate(ModePlan, &s, nil)
		judge(t, g, "its own mode", check{edit, childPlanRejectedText}, check{bash, ""}, check{read, ""})
		if g.PlanPath() != "" {
			t.Fatalf("a child's gate names a plan file: %q", g.PlanPath())
		}
		Raise(&s, ModePlan) // no stricter than its own mode: its own text still
		judge(t, g, "the parent still in plan", check{edit, childPlanRejectedText})
		Raise(&s, ModeAsk)
		judge(t, g, "after the parent entered ask", check{edit, childAskSwitchedText}, check{bash, childAskSwitchedText}, check{read, ""})
	})
	t.Run("opened in ask mode", func(t *testing.T) {
		var s atomic.Int32
		g := NewChildModeGate(ModeAsk, &s, nil)
		Raise(&s, ModePlan) // looser than ask: nothing changes, and nothing says it did
		judge(t, g, "the parent in plan", check{edit, askRejectedText}, check{bash, askRejectedText}, check{read, ""})
	})
	t.Run("control: no strictness", func(t *testing.T) {
		g := NewChildModeGate(ModePlan, nil, nil)
		judge(t, g, "plan", check{edit, childPlanRejectedText}, check{bash, ""})
		g = NewChildModeGate(ModeAgent, nil, nil)
		judge(t, g, "agent", check{edit, ""}, check{bash, ""})
	})
}
