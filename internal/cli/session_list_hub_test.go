package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/tui"
)

// The launch's session list on the hub (plan 032 §3.13): its roster is the
// hub's list roster, which in a Go test binary — no hub unless a test
// installs one (hub.ErrNoHub) — runs its poller; and a running row as the hub
// lists it, which carries no socket, is reached by the socket the registry
// lists for its host id when it is opened, stopped or cancelled.

// TestTheSessionListResolvesAHubRowsSocket: the production list's roster is
// hub.Roster's, on its poller here; the launch's own host's row, put through
// the hub's own row pipeline (hub.RosterRow, then roster.FromRosterRow, which
// is what a subscription's rows are) carries no socket, and Open — attached,
// in this CRAZE_HOME's namespace, so the socket resolved is the host's own —
// Cancel and Stop each resolve it from the registry as they act. Once the
// host has gone, the same row is refused `session not reachable` by all
// three. The negative control is the row itself: it names no socket, so an
// action that did not resolve one would dial nothing.
func TestTheSessionListResolvesAHubRowsSocket(t *testing.T) {
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
		lr, ok := r.(*hub.ListRoster)
		if !ok {
			t.Fatalf("the list's roster is a %T, not the hub's list roster", r)
		}
		s := waitList(t, r, "the host listed, its turn over", func(s roster.Snapshot) bool {
			row := runningRow(s, id)
			return row != nil && row.Status == roster.Reachable && row.Session != nil &&
				row.Session.Prompted && row.Session.Activity == engine.ActivityIdle
		})
		if m := lr.Mode(); m != hub.ModePoller {
			t.Fatalf("the list's roster is %v in a test binary with no hub, not on its poller", m)
		}
		polled := runningRow(s, id)
		wire, ok := hub.RosterRow(*polled, time.Now())
		if !ok {
			t.Fatalf("the hub would not list the host's row: %+v", polled.Host)
		}
		hubRow, err := roster.FromRosterRow(wire)
		if err != nil {
			t.Fatal(err)
		}
		ref := hubRow.Ref()
		if ref.Host.Socket != "" || ref.Host.ID != polled.Host.ID || ref.Host.CrazeSessionID != id {
			t.Fatalf("the hub's row names its host %+v", ref.Host)
		}

		var o backend.Backend
		if err := withinStep(t, "Open", func() (err error) { o, err = cfg.Sessions.Open(ref); return err }); err != nil {
			t.Fatalf("Open of the hub's row: %v", err)
		}
		if got := o.Info().CrazeSessionID; got != id {
			t.Fatalf("Open of the hub's row answered session %q, want %q", got, id)
		}
		if !readsAttachments(o) {
			t.Fatal("the hub's row opened in this CRAZE_HOME says it cannot read the attachments: its socket was not the host's")
		}
		_ = o.Close()
		if err := withinStep(t, "Cancel", func() error { return cfg.Sessions.Cancel(ref) }); err != nil {
			t.Fatalf("Cancel of the hub's row: %v", err)
		}
		_ = b.Close()
		if err := withinStep(t, "Stop", func() error { return cfg.Sessions.Stop(ref) }); err != nil {
			t.Fatalf("Stop of the hub's row: %v", err)
		}
		waitReaped(t, cmds.pids()[0])
		waitList(t, r, "the session saved once its host is gone", func(s roster.Snapshot) bool {
			return runningRow(s, id) == nil && len(s.Saved) == 1 && s.Saved[0].CrazeID == id
		})

		// The registry no longer lists the row's host.
		if _, err := cfg.Sessions.Open(ref); !errors.Is(err, errNotReachable) || !strings.Contains(err.Error(), "session not reachable") {
			t.Fatalf("Open of a row whose host is gone: %v", err)
		}
		if err := cfg.Sessions.Stop(ref); !errors.Is(err, errNotReachable) {
			t.Fatalf("Stop of a row whose host is gone: %v", err)
		}
		if err := cfg.Sessions.Cancel(ref); !errors.Is(err, errNotReachable) {
			t.Fatalf("Cancel of a row whose host is gone: %v", err)
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
