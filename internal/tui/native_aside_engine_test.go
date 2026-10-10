package tui

import (
	"context"
	"errors"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// TestNativeEngineAnswersAsides (plan 040 §3.3, §3.4): through the engine, a
// real native session (agent.NewNative over the scripted model) says it
// answers side questions from the moment it exists — the engine refuses one
// not_accepting until the session is up — and once started streams the
// answer's pieces to onDelta and returns the whole answer, its model and its
// usage. It lives here, not in internal/engine, whose tests may not import
// the harness the scripted model is planted through.
func TestNativeEngineAnswersAsides(t *testing.T) {
	isolateSkillsHome(t)
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{append(nativeTextParts("side ", "answer"), nativeFinishParts()...)}
	sess := agent.NewNative(agent.Options{Workspace: t.TempDir(), ContentHome: t.TempDir()},
		nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model))
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	ctx := context.Background()
	req := agent.AsideRequest{Question: "what is going on?"}

	if !e.Asides() {
		t.Fatal("the engine of an unstarted native session says it has no side questions")
	}
	if _, err := e.Aside(ctx, req, nil); !errors.Is(err, engine.ErrNotAccepting) {
		t.Fatalf("before Start the side question answered %v, want ErrNotAccepting", err)
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var deltas []string
	r, err := e.Aside(ctx, req, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Aside: %v", err)
	}
	if !slices.Equal(deltas, []string{"side ", "answer"}) {
		t.Fatalf("the pieces are %q, want the stream's two", deltas)
	}
	if r.Text != "side answer" || r.Model != "Echo" || r.Usage == nil || r.Usage.Input != 10 || r.Usage.Output != 5 {
		t.Fatalf("the answer is %+v (usage %+v); want the whole text, Echo and the step's usage", r, r.Usage)
	}
	if model.calls != 1 {
		t.Fatalf("the model saw %d requests, want the side question's one", model.calls)
	}
}
