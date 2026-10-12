package engine_test

import (
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// TestEnginePublishesTheSlashCatalogOverTheFakeAgent (plan 043 P2, D4):
// through the engine, a real live session over cmd/craze-fake-agent — cursor's
// dialect and grok's — says it publishes the slash catalog (the session
// capability slashCatalog, control.Info's source) and does: the catalog its
// install built is in the engine's own fold, so an attach's snapshot carries
// it, equal to the session's once the agent's own commands have landed in
// it. A session that is not craze's own, the plain
// Stub, says it does not, and its snapshot holds none. (Native says it too:
// internal/agent's TestLiveAndNativeSessionsPublishTheSlashCatalog.) Negative
// control: an engine whose PublishesSlashCatalog ignores the session's answer
// says true for the Stub.
func TestEnginePublishesTheSlashCatalogOverTheFakeAgent(t *testing.T) {
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
	snapshotSlash := func(e *engine.Engine) []agent.SlashItem {
		t.Helper()
		// The install is enqueued, not flushed: the engine's fold has it once
		// the log is synced.
		if _, err := e.SyncSeq(ctx); err != nil {
			t.Fatal(err)
		}
		a, err := e.Attach(ctx, engine.AttachOptions{})
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		a.Sub.Close()
		return a.Snapshot.Settings.Slash
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
		if !e.PublishesSlashCatalog() {
			t.Fatalf("a %s session does not say it publishes the slash catalog", p.Name())
		}
		// The agent's catalog lands after Start, on the read loop: once the
		// session holds it the catalog is final — the scans find no skills in
		// this test's empty directories — and the delta that restated it is
		// enqueued, which snapshotSlash's sync commits.
		for len(e.State().Commands) == 0 {
			select {
			case <-ctx.Done():
				t.Fatalf("a %s session's agent never advertised its commands", p.Name())
			case <-time.After(time.Millisecond):
			}
		}
		want := e.State().Slash
		if got := snapshotSlash(e); len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("a %s session's attach snapshot carries the catalog %+v, want the session's %+v", p.Name(), got, want)
		}
	}

	stub := start(tui.NewStubNoPrimary())
	if stub.PublishesSlashCatalog() {
		t.Fatal("a plain Stub says it publishes the slash catalog")
	}
	if got := snapshotSlash(stub); got != nil {
		t.Fatalf("a plain Stub's snapshot carries a catalog: %+v", got)
	}
}
