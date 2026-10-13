package engine_test

import (
	"context"
	"io"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// TestAnACPSessionStatesNoEffortFacts (plan 047 D2, §7 A2): through the
// engine, a real live session over cmd/craze-fake-agent — cursor's dialect
// and grok's — says it states no effort facts (the session capability
// effortFacts, control.Info's source), and its catalog's models carry none:
// an ACP provider's levels are its current model's alone, in its effort
// option. A Stub says so only when opted in (PublishEffortFacts), whatever
// provider it names. (Native says it does: internal/agent's
// TestNativeCatalogStatesEffortFacts.) Negative control: an engine whose
// PublishesEffortFacts ignores the session's answer says true for the plain
// Stub.
func TestAnACPSessionStatesNoEffortFacts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
	defer cancel()
	start := func(sess agent.Session) *engine.Engine {
		t.Helper()
		e, err := engine.New(sess, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, tc := range []struct {
		p      agent.Provider
		script string
	}{{agent.CursorProvider(), "echo"}, {agent.GrokProvider(), "grok-btw"}} {
		p := tc.p
		e := start(agent.New(agent.Options{
			Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=" + tc.script},
			Workspace: t.TempDir(), Force: true, Provider: &p, Stderr: io.Discard,
		}))
		if e.PublishesEffortFacts() {
			t.Fatalf("a %s session says it states effort facts", p.Name())
		}
		models := e.State().Models
		if len(models) == 0 {
			t.Fatalf("control: a %s session over the fake agent offers no models", p.Name())
		}
		for _, m := range models {
			if m.Efforts != nil || m.DefaultEffort != "" || m.SwitchEffort != "" {
				t.Fatalf("a %s session's model %s carries effort facts: %+v", p.Name(), m.ID, m)
			}
		}
	}
	plain := tui.NewStubNoPrimary()
	plain.SetProvider(agent.NativeProvider())
	if start(plain).PublishesEffortFacts() {
		t.Fatal("a plain Stub naming native says it states effort facts")
	}
	opted := tui.NewStubNoPrimary()
	opted.PublishEffortFacts = true
	if !start(opted).PublishesEffortFacts() {
		t.Fatal("a PublishEffortFacts Stub does not say it states effort facts")
	}
}
