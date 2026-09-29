package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/tui"
)

// The launch's session list (plan 030 §3.9), production's pieces end to end:
// the roster over the registry and the index, Open over a host's socket, Stop
// over one of its own — against a host this test binary runs as craze serve
// with the fake agent. Every wait is bounded on its own (serveStep).

// waitList reads r's snapshots until pred holds for one, within a step.
func waitList(t *testing.T, r tui.SessionRoster, what string, pred func(roster.Snapshot) bool) roster.Snapshot {
	t.Helper()
	deadline := time.After(serveStep)
	var last roster.Snapshot
	for {
		select {
		case s, ok := <-r.Updates():
			if !ok {
				t.Fatalf("%s: the roster stopped", what)
			}
			last = s
			if pred(s) {
				return s
			}
		case <-deadline:
			t.Fatalf("%s: never; the last snapshot %+v", what, last)
		}
	}
}

// runningRow is the running row of craze session id in s, nil when none.
func runningRow(s roster.Snapshot, id string) *roster.Row {
	for i := range s.Running {
		if s.Running[i].Host.CrazeSessionID == id {
			return &s.Running[i]
		}
	}
	return nil
}

// TestTheSessionListListsOpensAndStopsAHost: the host a launch spawned is
// listed, reachable, with the row facts — prompted once its turn has run,
// its reply the last reply — and opened as a second client, attached; Stop
// ends it on its host, which leaves the registry, and the session, prompted,
// is saved.
func TestTheSessionListListsOpensAndStopsAHost(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	ran := false
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		ran = true
		if cfg.Sessions == nil {
			t.Fatal("the launch has no session list")
		}
		b, err := cfg.NewBackend(cfg.Provider, false)
		if err != nil {
			t.Fatalf("NewBackend: %v", err)
		}
		started(t, b)
		id := b.Info().CrazeSessionID
		if _, err := b.Submit(stepCtx(t), engine.Command{Client: b.ClientID(), ID: "1"}, "hello there", engine.SubmitQueue, ""); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		r := cfg.Sessions.Roster()
		defer r.Close()
		s := waitList(t, r, "the host listed, its turn over", func(s roster.Snapshot) bool {
			row := runningRow(s, id)
			return row != nil && row.Status == roster.Reachable && row.Session != nil &&
				row.Session.Prompted && row.Session.Activity == engine.ActivityIdle && row.Session.LastReply != ""
		})
		row := runningRow(s, id)
		switch se := row.Session; {
		case !se.RowFacts || se.Since.IsZero() || !se.Stop:
			t.Fatalf("a craze serve host's row: %+v", se)
		case row.Version == "" || row.Host.Workspace == "":
			t.Fatalf("the row's host: %+v, version %q", row.Host, row.Version)
		}
		o, err := cfg.Sessions.Open(row.Ref())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got := o.Info().CrazeSessionID; got != id {
			t.Fatalf("Open answered session %q, want %q", got, id)
		}
		_ = o.Close()
		_ = b.Close()
		if err := cfg.Sessions.Stop(row.Ref()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		waitReaped(t, cmds.pids()[0])
		s = waitList(t, r, "the session saved once its host is gone", func(s roster.Snapshot) bool {
			return runningRow(s, id) == nil && len(s.Saved) == 1 && s.Saved[0].CrazeID == id
		})
		if err := cfg.Sessions.Stop(roster.SavedRef(s.Saved[0])); !errors.Is(err, errStopSaved) {
			t.Fatalf("Stop of a saved session: %v", err)
		}
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if !ran {
		t.Fatal("the TUI never ran")
	}
	assertNoHosts(t, env)
}
