package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// Starting a session from the list (plan 030 §3.13, C15), production's pieces
// end to end, as the TUI's background dispatch drives them (tui's
// runDispatch): Spawn in another workspace, Open — a connection of the
// dispatch's own — its stream drained, Start, the prompt with that
// connection's own first command id, LeaveRunning, Close. Every host is this
// test binary run as craze serve with the fake agent, and every wait is
// bounded on its own (serveStep).

// TestTheSessionListDispatchesANewSession: a new session from the list runs
// in the workspace it was given, with the permission mode of the session the
// list came from, the launch's agent binary and none of the command line's
// own session's --plan, model or provider; a prompt sent on the dispatch's
// own connection under its first command id starts a turn; left running and
// the connection closed, the host outlives the quit, and the session —
// prompted — has its index row.
func TestTheSessionListDispatchesANewSession(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	elsewhere := absDir(t.TempDir())
	f := launchFlags(t, ws)
	// The command line's own session's mode and model: not a new session's
	// from the list.
	f.plan, f.model = true, "launch-model"
	var id string
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		ref, err := cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: elsewhere, Provider: agent.CursorProvider(), PermissionMode: backend.PermissionPrompt})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		b, err := cfg.Sessions.Open(ref)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for {
				if _, err := b.Read(ctx); err != nil {
					return
				}
			}
		}()
		if err := b.Start(stepCtx(t)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		info := b.Info()
		id = info.CrazeSessionID
		if info.Workspace != elsewhere || info.PermissionMode != backend.PermissionPrompt {
			t.Fatalf("the new session runs in %q with permissions %q, want %q and prompt", info.Workspace, info.PermissionMode, elsewhere)
		}
		res, err := b.Submit(stepCtx(t), engine.Command{Client: b.ClientID(), ID: "1"}, "hello from the list", engine.SubmitQueue, "")
		if err != nil || res.Turn == "" {
			t.Fatalf("the prompt on the dispatch's own connection: %+v, %v", res, err)
		}
		if err := cfg.Sessions.LeaveRunning(ref); err != nil {
			t.Fatalf("LeaveRunning: %v", err)
		}
		_ = b.Close()
		cancel()
		select {
		case <-drained:
		case <-time.After(serveStep):
			t.Fatal("the drain did not end with the connection")
		}
		return tui.Result{}, nil
	})
	if err := runTUI(nil, f, hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	argv := argvOf(t, cmds, 0)
	for _, want := range []string{"--workspace=" + elsewhere, "--provider=cursor", "--no-force", "--agent-bin="} {
		if !hasArg(argv, want) {
			t.Fatalf("the host's command line %q lacks %s", argv, want)
		}
	}
	for _, flag := range []string{"--plan", "--ask", "--model=", "--continue", "--load=", "--force"} {
		if hasArg(argv, flag) {
			t.Fatalf("the host's command line %q passes %s", argv, flag)
		}
	}
	e := onlyHost(t, env)
	if e.CrazeSessionID != id || e.Workspace != elsewhere {
		t.Fatalf("after the quit %+v, want the dispatched session %s in %q still served", e, id, elsewhere)
	}
	deadline := time.Now().Add(serveStep)
	for {
		rows, err := (&sessions.Store{}).ByCrazeID()
		if err != nil {
			t.Fatal(err)
		}
		if row, ok := rows[id]; ok {
			if row.CWD != elsewhere {
				t.Fatalf("the index row's workspace %q, want %q", row.CWD, elsewhere)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the prompted session has no index row within %s", serveStep)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopEntry(t, e, cmds.pids()[0])
}

// TestANewNativeSessionFromTheListTakesNoAgentBinary: --agent-bin is the
// launch's binary for an ACP provider; a new session of a provider craze runs
// in process is spawned without it — which that provider's host would refuse
// — whatever the command line gave.
func TestANewNativeSessionFromTheListTakesNoAgentBinary(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	native, err := agent.ProviderByName("native")
	if err != nil {
		t.Fatal(err)
	}
	var spawnErr error
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		_, spawnErr = cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: ws, Provider: native})
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if spawnErr != nil && strings.Contains(spawnErr.Error(), "agent-bin") {
		t.Fatalf("the native spawn was refused its agent binary: %v", spawnErr)
	}
	if spawnErr != nil {
		t.Fatalf("the native spawn: %v", spawnErr)
	}
	argv := argvOf(t, cmds, 0)
	if !hasArg(argv, "--provider=native") || hasArg(argv, "--agent-bin=") {
		t.Fatalf("the native host's command line %q", argv)
	}
	// Neither opened nor left running: the quit stopped it.
	assertNoHosts(t, env)
}
