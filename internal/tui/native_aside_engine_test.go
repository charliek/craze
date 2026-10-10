package tui

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
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

// asideHeldModel answers every request with the start of an answer, says so
// on reached (once), and holds until the request is cancelled, which it ends
// with the context's error part, as a provider's stream ends.
type asideHeldModel struct {
	*nativeScriptedModel
	reached chan struct{}
}

func (m *asideHeldModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "0"}) ||
			!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "partial"}) {
			return
		}
		close(m.reached)
		<-ctx.Done()
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
	}, nil
}

// TestNativeEngineAsideCutShortByCloseIsAborted (plan 040 §3.3, branch
// review P2): through the engine, a native side question streaming when the
// engine — and with it the session — closes, its caller's context still live
// (another client stopping the session while this one's aside streams: the
// host closes the engine before its sockets), is agent.ErrAsideInterrupted:
// aborted, reason context — stopped — and not not_accepting, a refusal that
// says nothing was asked.
func TestNativeEngineAsideCutShortByCloseIsAborted(t *testing.T) {
	isolateSkillsHome(t)
	model := &asideHeldModel{nativeScriptedModel: &nativeScriptedModel{provider: "test", wire: "wire-echo"}, reached: make(chan struct{})}
	tweak := nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model.nativeScriptedModel)
	sess := agent.NewNative(agent.Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		tweak(o)
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return model, nil }
	})
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	ctx := context.Background()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	out := make(chan error, 1)
	go func() {
		_, err := e.Aside(ctx, agent.AsideRequest{Question: "q"}, nil)
		out <- err
	}()
	select {
	case <-model.reached:
	case <-time.After(wakeWatchdog):
		t.Fatal("the side question's request never streamed")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-out:
	case <-time.After(wakeWatchdog):
		t.Fatal("the side question never returned after Close")
	}
	if ctx.Err() != nil {
		t.Fatal("control: the caller's context ended")
	}
	if !errors.Is(err, agent.ErrAsideInterrupted) || !errors.Is(err, context.Canceled) ||
		errors.Is(err, engine.ErrNotAccepting) || errors.Is(err, agent.ErrAsideNotReady) {
		t.Fatalf("a side question the session's Close cut short answered %v, want agent.ErrAsideInterrupted alone", err)
	}
	if code, reason := engine.Code(err), engine.Reason(err); code != "aborted" || reason != "context" {
		t.Fatalf("it is %s/%s on the wire, want aborted/context", code, reason)
	}
}
