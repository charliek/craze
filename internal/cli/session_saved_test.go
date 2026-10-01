package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// The session list's saved rows (plan 030 §3.12, C12), production's pieces
// end to end: Sessions.Open of a saved ref spawns a host that loads the row —
// by its craze id, or a legacy row by <provider>:<sessionId> — in the row's
// own workspace, and the backend it answers is the launch's as any launch's
// is; two resumes of one legacy row at once — forced, both hosts held where
// they have read the row — end as one host, the other attached to it held; a
// saved row that is running after all is the holder's, attached to without a
// spawn; and a row this craze cannot run is refused before anything is
// spawned. Every host is this test binary run as craze serve with the fake
// agent, and every wait is bounded on its own (serveStep).

// resumeFlags is launchFlags with the session flags a command line gives its
// own session — a provider filter, a model — none of which a resume from the
// list may pass on.
func resumeFlags(t *testing.T, ws string) *tuiFlags {
	t.Helper()
	f := launchFlags(t, ws)
	f.provider, f.model = "cursor", "launch-model"
	return f
}

// assertResumeArgv holds the n-th spawned host's command line to a resume's:
// --load=load, the launch's agent binary and permission mode, and none of
// --provider, --model, --workspace, --continue, --ask, --plan.
func assertResumeArgv(t *testing.T, cmds *childCmds, n int, load string) {
	t.Helper()
	argv := argvOf(t, cmds, n)
	if !hasArg(argv, "--load="+load) || !hasArg(argv, "--agent-bin=") {
		t.Fatalf("the host's command line %q, want --load=%s and the launch's --agent-bin", argv, load)
	}
	for _, flag := range []string{"--provider=", "--model=", "--workspace=", "--continue", "--ask", "--plan", "--no-force"} {
		if hasArg(argv, flag) {
			t.Fatalf("the host's command line %q passes %s", argv, flag)
		}
	}
}

// TestTheSessionListResumesASavedSession (§3.12, R2-6): Open of a saved row —
// one with its craze id, and a legacy one with none — spawns one host, with
// --load (the craze id, or <provider>:<sessionId>) and none of the command
// line's own session flags, in the row's own workspace, not the launch's;
// the host loads it, under the row's id or the one it gives the legacy row
// (which the index then carries). The backend, its start taken, is the
// launch's: kept running at quit.
func TestTheSessionListResumesASavedSession(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy %v", legacy), func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "load")
			elsewhere := absDir(t.TempDir())
			row := sessions.Row{SessionID: "saved-1", Provider: "cursor", CWD: elsewhere, Title: "a saved one", UpdatedAt: time.Now()}
			load := "cursor:saved-1"
			if !legacy {
				row.CrazeID = "0199aaaa-bbbb-7ccc-8ddd-0000000000e1"
				load = row.CrazeID
			}
			seedIndexRow(t, row)
			var crazeID string
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				b, err := cfg.Sessions.Open(roster.SavedRef(row))
				if err != nil {
					t.Fatalf("Open of the saved row: %v", err)
				}
				started(t, b)
				info := b.Info()
				crazeID = info.CrazeSessionID
				if info.Workspace != elsewhere || info.Provider != "cursor" {
					t.Fatalf("the resumed session runs %q in %q, want cursor in the row's %q", info.Provider, info.Workspace, elsewhere)
				}
				_ = b.Close()
				return tui.Result{}, nil
			})
			if err := runTUI(nil, resumeFlags(t, ws), hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if n := cmds.count(); n != 1 {
				t.Fatalf("%d hosts spawned, want the resume's one", n)
			}
			assertResumeArgv(t, cmds, 0, load)
			e := onlyHost(t, env)
			switch {
			case !legacy && (crazeID != row.CrazeID || e.CrazeSessionID != row.CrazeID):
				t.Fatalf("the host serves %q, the TUI was given %q, want the row's %s", e.CrazeSessionID, crazeID, row.CrazeID)
			case legacy && (crazeID == "" || e.CrazeSessionID != crazeID || indexRowByID(t, crazeID).SessionID != "saved-1"):
				t.Fatalf("the legacy row's host serves %q, the TUI was given %q", e.CrazeSessionID, crazeID)
			case e.Workspace != elsewhere:
				t.Fatalf("the host runs in %q, want the row's %q", e.Workspace, elsewhere)
			}
			stopEntry(t, e, cmds.pids()[0])
		})
	}
}

// rowGate is a FIFO for cliChildRowGate, gateFIFO's release and letGo with
// it: the spawned host given it is held there once it has read its load's
// row, before it gives the row a craze id or claims it.
type rowGate struct {
	path           string
	release, letGo func()
}

func newRowGate(t *testing.T) rowGate {
	t.Helper()
	path, release, letGo := gateFIFO(t)
	return rowGate{path: path, release: release, letGo: letGo}
}

// arrived is the row the host held at g read, once it is held there (the
// row it writes as it gets there), within serveStep.
func (g rowGate) arrived(t *testing.T) sessions.Row {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		b, err := os.ReadFile(g.path + ".row")
		if err == nil {
			var row sessions.Row
			if err := json.Unmarshal(b, &row); err != nil {
				t.Fatalf("the row a held host read: %v (%q)", err, b)
			}
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("no host was held at %s after %v", g.path, serveStep)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheSessionListResumesALegacyRowTwiceAtOnce (§3.18 PR 2: "including two
// at once"): two Opens of one legacy saved row at the same moment — a legacy
// row has no craze id for either to find a host by, so both spawn — end as
// one host: whichever claims it first loads it under the craze id it gives
// the row, and the other's host is answered held and exits, its Open attached
// to the first through the rendezvous. Both backends serve that one session,
// and the quit keeps it running.
//
// The moment is forced (sol r23-c12 1), as
// TestServeTwoLegacyLoadsAtOnceOneWins forces it in process: each Open's host
// is held once it has read the row (cliChildRowGate) — each finding it with
// no craze id — until the other's is held too, and both are let go together,
// so both race the id's assignment and then the claim. Neither Open can
// answer before both hosts have read the row: two Opens made one after the
// other never get past the first's hold.
func TestTheSessionListResumesALegacyRowTwiceAtOnce(t *testing.T) {
	gates := []rowGate{newRowGate(t), newRowGate(t)}
	env, ws, cmds := launchHome(t, func(n int) []string {
		if n <= len(gates) {
			return []string{cliChildRowGate + "=" + gates[n-1].path}
		}
		return nil
	})
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	row := sessions.Row{SessionID: "legacy-2", Provider: "cursor", CWD: absDir(t.TempDir()), Title: "a legacy one", UpdatedAt: time.Now()}
	seedIndexRow(t, row)
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		// A failing test lets a host still held go before the launch's
		// finish waits for the Opens.
		defer func() {
			for _, g := range gates {
				g.letGo()
			}
		}()
		type opened struct {
			b   backend.Backend
			err error
		}
		results := make(chan opened, 2)
		for range 2 {
			go func() {
				b, err := cfg.Sessions.Open(roster.SavedRef(row))
				results <- opened{b, err}
			}()
		}
		for i, g := range gates {
			if got := g.arrived(t); got.SessionID != "legacy-2" || got.CrazeID != "" {
				t.Fatalf("host %d read %+v, want the legacy row with no craze id yet", i+1, got)
			}
		}
		for _, g := range gates {
			g.release()
		}
		var bs []*launchedBackend
		for range 2 {
			select {
			case r := <-results:
				if r.err != nil {
					t.Fatalf("an Open of the legacy row: %v", r.err)
				}
				lb, ok := r.b.(*launchedBackend)
				if !ok {
					t.Fatalf("Open answered a %T", r.b)
				}
				bs = append(bs, lb)
			case <-time.After(serveStep):
				t.Fatal("an Open of the legacy row never answered")
			}
		}
		for _, b := range bs {
			started(t, b)
		}
		host, held := bs[0], bs[1]
		if host.ref.held {
			host, held = held, host
		}
		stored, ok, err := (&sessions.Store{}).Find("cursor", "legacy-2")
		switch {
		case err != nil || !ok || stored.CrazeID == "":
			t.Fatalf("the legacy row after the resumes: %+v, %v, %v", stored, ok, err)
		case host.ref.held || host.ref.child == nil:
			t.Fatalf("neither resume spawned the serving host: %+v, %+v", host.ref, held.ref)
		case !held.ref.held || held.ref.child != nil || held.ref.entry.HostID != host.ref.entry.HostID:
			t.Fatalf("the other resume answered %+v, want the serving host %s, held", held.ref, host.ref.entry.HostID)
		}
		for _, b := range bs {
			if got := b.Info().CrazeSessionID; got != stored.CrazeID {
				t.Fatalf("a resume serves %q, want the row's new id %s", got, stored.CrazeID)
			}
			_ = b.Close()
		}
		return tui.Result{}, nil
	})
	if err := runTUI(nil, resumeFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if n := cmds.count(); n != 2 {
		t.Fatalf("%d hosts spawned, want two: the one that serves it and the one answered held", n)
	}
	e := onlyHost(t, env)
	pids := cmds.pids()
	served, other := pids[0], pids[1]
	if e.PID != served {
		served, other = other, served
	}
	if e.PID != served {
		t.Fatalf("the registry's host %d is neither spawn's (%v)", e.PID, pids)
	}
	waitReaped(t, other)
	stopEntry(t, e, served)
}

// TestTheSessionListResumesTheHolderOfASavedRow (§3.12, §3.4): a saved row
// that is running after all — the list read the index, and a host has taken
// the session since — is the holder's: Open attaches to the host that serves
// it (the launch's direct reattach, the rendezvous's first half), spawning
// nothing and leaving no note of flags ignored, since a resume passes none;
// and the quit leaves the holder running, as it leaves every holder.
func TestTheSessionListResumesTheHolderOfASavedRow(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000e3"
	row := sessions.Row{SessionID: "held-3", Provider: "cursor", CWD: absDir(t.TempDir()), CrazeID: id, Title: "a held one", UpdatedAt: time.Now()}
	seedIndexRow(t, row)
	h := spawned(t, goSpawn(t, context.Background(), spawnOptions{env: env, flags: tuiFlags{force: true, agentBin: fakeAgentPath(t)}, load: id}))
	if h.err != nil {
		t.Fatalf("the holder's spawn: %v", h.err)
	}
	holder := h.ref
	stderr := captureStderr(t)
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		b, err := cfg.Sessions.Open(roster.SavedRef(row))
		if err != nil {
			t.Fatalf("Open of the held row: %v", err)
		}
		lb, ok := b.(*launchedBackend)
		if !ok || !lb.ref.held || lb.ref.child != nil || lb.ref.entry.HostID != holder.entry.HostID {
			t.Fatalf("the backend %T %+v, want the holder %s's", b, lb, holder.entry.HostID)
		}
		started(t, b)
		if got := b.Info().CrazeSessionID; got != id {
			t.Fatalf("attached to session %q, want %s", got, id)
		}
		_ = b.Close()
		return tui.Result{}, nil
	})
	if err := runTUI(nil, resumeFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	switch {
	case cmds.count() != 1:
		t.Fatalf("%d hosts spawned, want the holder alone", cmds.count())
	case holder.child.Exited():
		t.Fatal("the quit ended the holder")
	}
	if got := stderr(); strings.Contains(got, "attached to it") {
		t.Fatalf("a resume of a held row left the ignored-flags note: %q", got)
	}
	stopEntry(t, onlyHost(t, env), holder.child.PID())
}

// TestTheSessionListRefusesASavedRowItCannotRun (§3.12): a saved row whose
// workspace is no longer a directory, or whose provider this craze does not
// know, is refused by Open before anything is spawned — a *tui.Refusal in
// the words the host would refuse it in — and leaves no host and no log.
func TestTheSessionListRefusesASavedRowItCannotRun(t *testing.T) {
	gone := filepath.Join(absDir(t.TempDir()), "gone")
	for _, tc := range []struct {
		name string
		row  sessions.Row
		want string
	}{
		{"its workspace gone", sessions.Row{SessionID: "r-1", Provider: "cursor", CWD: gone, CrazeID: "0199aaaa-bbbb-7ccc-8ddd-0000000000e4"},
			"craze: that session ran in " + gone + ", which is no longer a directory"},
		{"its provider unknown", sessions.Row{SessionID: "r-2", Provider: "zed", CWD: absDir(t.TempDir())},
			`craze: that session's provider "zed" is not one this craze can resume`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "load")
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				b, err := cfg.Sessions.Open(roster.SavedRef(tc.row))
				var refusal *tui.Refusal
				if b != nil || !errors.As(err, &refusal) || err.Error() != tc.want {
					t.Fatalf("Open: %v, %v (%T), want the refusal %q", b, err, err, tc.want)
				}
				return tui.Result{}, nil
			})
			if err := runTUI(nil, resumeFlags(t, ws), hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if n := cmds.count(); n != 0 {
				t.Fatalf("%d hosts spawned for a row craze cannot run", n)
			}
			if logs, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "host-logs", "*")); len(logs) != 0 {
				t.Fatalf("a refused resume left %q", logs)
			}
			if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the refused resume made its workspace: %v", err)
			}
			assertNoHosts(t, env)
		})
	}
}
