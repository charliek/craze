package engine_test

import (
	"context"
	"io"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// TestEngineAsidesOverTheFakeGrok (plan 040 §7.6): through the engine, a real
// grok-dialect live session over cmd/craze-fake-agent says it answers side
// questions (the session capability aside, control.Info's source), serves one
// through x.ai/btw, and a cursor session says it does not.
func TestEngineAsidesOverTheFakeGrok(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
	defer cancel()

	start := func(script string, p agent.Provider) *engine.Engine {
		t.Helper()
		sess := agent.New(agent.Options{
			Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=" + script},
			Workspace: t.TempDir(), Force: true, Provider: &p, Stderr: io.Discard,
		})
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

	grok := start("grok-btw", agent.GrokProvider())
	if !grok.Asides() {
		t.Fatal("a grok session does not say it answers side questions")
	}
	res, err := grok.Aside(ctx, agent.AsideRequest{Question: "what now?"}, nil)
	if err != nil {
		t.Fatalf("Aside: %v", err)
	}
	if res.Text != "btw: what now?" || res.Usage != nil {
		t.Fatalf("result %+v", res)
	}

	gx := start("grok-btw", agent.GxProvider())
	if !gx.Asides() {
		t.Fatal("a gx session does not say it answers side questions")
	}

	if start("echo", agent.CursorProvider()).Asides() {
		t.Fatal("a cursor session says it answers side questions")
	}
}
