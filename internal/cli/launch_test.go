package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// The launch flow (plan 030 C4, §3.5): runTUI resolving what to run and the
// TUI spawning it — a new session, --continue's row with --load <crazeId> or
// <provider>:<sessionId>, a held session attached to through the rendezvous,
// the pickers' choices — every host this test binary run as craze serve with
// the fake agent; a spawn failure as a start failure naming the opt-out, a
// refusal as a refusal; a quit while starting leaving no host; and the
// opt-out's three switches keeping the in-process path. Most tests take the
// Config a run builds through the tuiRun seam and act as the TUI would with
// it; one drives the real TUI in a pty, end to end. Every wait is bounded on
// its own (serveStep).

// fakeRun makes runTUI hand its Config to run instead of the real TUI, for
// one test. The host-status hub a Config carries is closed after run, as
// tui.Run closes it.
func fakeRun(t *testing.T, run func(cfg tui.Config) (tui.Result, error)) {
	t.Helper()
	prev := tuiRun
	tuiRun = func(cfg tui.Config) (tui.Result, error) {
		if cfg.Host != nil {
			defer cfg.Host.Close(context.Background())
		}
		return run(cfg)
	}
	t.Cleanup(func() { tuiRun = prev })
}

// launchHome is serveHome with the launch flow on — the package runs under
// the opt-out (TestMain) — every host spawned this test binary run as craze
// serve (spawnAsChild, extra its environment), and the handshake's bounds
// each serveStep.
func launchHome(t *testing.T, extra func(n int) []string) (rundir.Env, string, *childCmds) {
	t.Helper()
	env, ws := serveHome(t)
	t.Setenv(detachEnv, "")
	cmds := spawnAsChild(t, extra)
	spawnBounds(t, serveStep, serveStep, serveStep)
	return env, ws, cmds
}

// launchFlags is a command line in ws with the fake agent.
func launchFlags(t *testing.T, ws string) *tuiFlags {
	t.Helper()
	return &tuiFlags{agentBin: fakeAgentPath(t), workspace: ws, force: true}
}

// argvOf is the n-th spawned host's command line (from 0), as the spawner
// handed it to craze serve.
func argvOf(t *testing.T, cmds *childCmds, n int) []string {
	t.Helper()
	cmds.mu.Lock()
	defer cmds.mu.Unlock()
	if n >= len(cmds.cmds) {
		t.Fatalf("host %d was never spawned (%d were)", n, len(cmds.cmds))
	}
	var argv []string
	for _, kv := range cmds.cmds[n].Env {
		if v, ok := strings.CutPrefix(kv, cliChildEnv+"="); ok {
			argv = nil
			if err := json.Unmarshal([]byte(v), &argv); err != nil {
				t.Fatal(err)
			}
		}
	}
	return argv
}

// hasArg reports whether argv carries arg, or — for a name ending in "=" — an
// argument with that prefix.
func hasArg(argv []string, arg string) bool {
	return slices.ContainsFunc(argv, func(a string) bool {
		if strings.HasSuffix(arg, "=") {
			return strings.HasPrefix(a, arg)
		}
		return a == arg
	})
}

// started is b started as the TUI starts the backend it adopts, within a
// step, and its start's answer taken as the TUI takes it while not quitting:
// acknowledged (AckStarted, which the model calls where it applies the
// answer). A test standing in for the TUI that did not would be a TUI that
// quit before it heard the answer, whose host finish stops.
func started(t *testing.T, b backend.Backend) {
	t.Helper()
	if err := b.Start(stepCtx(t)); err != nil {
		t.Fatalf("the adopted backend's start: %v", err)
	}
	if a, ok := b.(interface{ AckStarted() }); ok {
		a.AckStarted()
	}
}

// stopEntry stops the host e names and waits for pid to be reaped.
func stopEntry(t *testing.T, e rundir.Entry, pid int) {
	t.Helper()
	if err := stopHost(e); err != nil {
		t.Fatalf("session.stop: %v", err)
	}
	waitReaped(t, pid)
}

// TestDetachOnFollowsItsSwitches is the opt-out's decision (plan 030 §3.5):
// CRAZE_DETACH, `detach` in config.toml and the control socket, each of which
// only turns detached hosts off. A CRAZE_DETACH or a `detach` craze cannot
// read is off with one line naming it; an explicit false, and a control
// socket that is off for any reason — which the in-process path explains
// itself — are silent.
func TestDetachOnFollowsItsSwitches(t *testing.T) {
	envs := []struct {
		name  string
		value string
		on    bool
		why   string
	}{
		{name: "blank", value: "  ", on: true},
		{name: "1", value: "1", on: true},
		{name: "true", value: " true ", on: true},
		{name: "0", value: "0"},
		{name: "false", value: "false"},
		{name: "garbage", value: "maybe", why: `craze: detached sessions off: CRAZE_DETACH="maybe" is not a bool`},
	}
	configs := []struct {
		name   string
		body   string
		socket string // CRAZE_CONTROL_SOCKET
		on     bool
		why    string
	}{
		{name: "no key", body: "theme = \"gruvbox\"\n", on: true},
		{name: "detach = true", body: "detach = true\n", on: true},
		{name: "detach = false", body: "detach = false\n"},
		{name: "a string", body: "detach = \"false\"\n", why: "craze: detached sessions off: config.toml detach is not a bool"},
		{name: "control_socket = false", body: "control_socket = false\n"},
		{name: "CRAZE_CONTROL_SOCKET=0", body: "", socket: "0"},
		{name: "unparseable", body: "detach = \n"},
	}
	for _, e := range envs {
		for _, c := range configs {
			t.Run(e.name+"/"+c.name, func(t *testing.T) {
				writeCrazeConfig(t, c.body)
				t.Setenv(detachEnv, e.value)
				t.Setenv(controlSocketEnv, c.socket)
				var diag bytes.Buffer
				got := detachOn(&diag)
				want, why := e.on && c.on, e.why
				if e.on {
					why = c.why
				}
				if got != want {
					t.Fatalf("detachOn = %v, want %v (diag %q)", got, want, diag.String())
				}
				wantDiag := ""
				if why != "" {
					wantDiag = why + "\n"
				}
				if diag.String() != wantDiag {
					t.Fatalf("diag %q, want %q", diag.String(), wantDiag)
				}
			})
		}
	}
}

// TestTheOptOutRunsTheSessionInProcess (plan 030 §3.5, AC6): each switch —
// CRAZE_DETACH=0, `detach = false`, `control_socket = false` and
// CRAZE_CONTROL_SOCKET=0 — keeps the in-process path: the TUI is handed the
// session it hosts (Config.Session, the engine hook, the claims), no launch
// closure and no session list (plan 030 §3.5, §3.9), and no host is spawned.
func TestTheOptOutRunsTheSessionInProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T)
	}{
		{"CRAZE_DETACH=0", func(t *testing.T) { t.Setenv(detachEnv, "0") }},
		{"detach = false", func(t *testing.T) { writeCrazeConfig(t, "detach = false\n") }},
		{"control_socket = false", func(t *testing.T) { writeCrazeConfig(t, "control_socket = false\n") }},
		{"CRAZE_CONTROL_SOCKET=0", func(t *testing.T) { t.Setenv(controlSocketEnv, "0") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ws, cmds := launchHome(t, nil)
			tc.set(t)
			ran := false
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				ran = true
				if cfg.Session != nil {
					defer func() { _ = cfg.Session.Close() }()
				}
				switch {
				case cfg.Session == nil || cfg.OnEngine == nil || cfg.ClaimSession == nil || cfg.SessionIndex == nil:
					t.Errorf("the opt-out's Config is not the in-process path's: %+v", cfg)
				case cfg.NewBackend != nil || cfg.LoadBackend != nil || cfg.Continue != nil:
					t.Error("the opt-out's Config launches")
				case cfg.Sessions != nil:
					t.Error("the opt-out's Config has a session list: closing a backend there closes its engine")
				}
				return tui.Result{}, nil
			})
			if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if !ran || cmds.count() != 0 {
				t.Fatalf("ran %v; %d hosts spawned, want none", ran, cmds.count())
			}
		})
	}
}

// TestALaunchIsItsHostsClient: by default the TUI is handed no session and
// nothing of the in-process path — no engine hook, no claim, no index, no
// provider persistence (the host's) — but the launch closures and the session
// list (§3.9); it is its session's client and not a Viewer, so the
// host-status hub stays its own and reports for the session it shows (plan
// 030 §3.7). Building it spawns nothing, binds nothing and claims nothing:
// the TUI spawns when it runs.
func TestALaunchIsItsHostsClient(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		switch {
		case cfg.NewBackend == nil || cfg.LoadBackend == nil:
			t.Error("the launch has no spawn closures")
		case cfg.Sessions == nil:
			t.Error("the launch has no session list")
		case cfg.Session != nil || cfg.NewSession != nil || cfg.LoadSession != nil || cfg.OnEngine != nil ||
			cfg.ClaimSession != nil || cfg.SessionIndex != nil || cfg.Backend != nil || cfg.Continue != nil:
			t.Errorf("the launch carries the in-process path: %+v", cfg)
		case cfg.PersistProvider || cfg.Viewer:
			t.Errorf("persist %v, viewer %v: the host persists, and the TUI is no viewer", cfg.PersistProvider, cfg.Viewer)
		case cfg.Host == nil:
			t.Error("the launching TUI has no host-status hub with a host's gate met")
		case !cfg.ProviderLocked || cfg.Provider.Name() != "cursor":
			t.Errorf("provider %q locked %v", cfg.Provider.Name(), cfg.ProviderLocked)
		}
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), fakeHostEnv(herdrGate(t))); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if cmds.count() != 0 {
		t.Fatalf("%d hosts spawned by building the TUI", cmds.count())
	}
	if entries, _ := rundir.Hosts(env); len(entries) != 0 {
		t.Fatalf("the launcher is in the registry: %+v", entries)
	}
	if locks, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "locks", "*")); len(locks) != 0 {
		t.Fatalf("the launcher claimed %q", locks)
	}
}

// TestALaunchSpawnsOneHostTheTUIAdopts: the TUI's new session is one host —
// spawned with the provider (so the host persists it) and the launching TUI's
// --no-host-status, no load — whose socket the backend is; the TUI adopts it (Start) and quits (a view close), and the
// host goes on serving the session, in the registry; its own stop ends it.
func TestALaunchSpawnsOneHostTheTUIAdopts(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	// The launch attaches now, while the host's start runs (plan 030 C5):
	// a load's replay streams in (TestALoadReplaysProgressivelyToANowAttach).
	var dialledWhen []protocol.When
	prevDial := spawnDial
	spawnDial = func(ctx context.Context, path string, o remote.SessionOptions) (*remote.Session, error) {
		dialledWhen = append(dialledWhen, o.When)
		return prevDial(ctx, path, o)
	}
	t.Cleanup(func() { spawnDial = prevDial })
	var crazeID string
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		b, err := cfg.NewBackend(cfg.Provider, false)
		if err != nil {
			t.Fatalf("NewBackend: %v", err)
		}
		started(t, b)
		crazeID = b.Info().CrazeSessionID
		_ = b.Close()
		return tui.Result{}, nil
	})
	f := launchFlags(t, ws)
	f.noHostStatus = true
	if err := runTUI(nil, f, hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	argv := argvOf(t, cmds, 0)
	switch {
	case cmds.count() != 1:
		t.Fatalf("%d hosts spawned, want 1", cmds.count())
	case !hasArg(argv, "--provider=cursor") || hasArg(argv, "--load=") || hasArg(argv, "--continue"):
		t.Fatalf("the host's command line %q", argv)
	case !hasArg(argv, "--no-host-status"):
		t.Fatalf("the launching TUI's --no-host-status did not reach its host: %q", argv)
	}
	e := onlyHost(t, env)
	if e.CrazeSessionID != crazeID || e.CrazeSessionID == "" {
		t.Fatalf("the host serves %q, the TUI was given %q", e.CrazeSessionID, crazeID)
	}
	if len(dialledWhen) != 1 || dialledWhen[0] != protocol.WhenNow {
		t.Fatalf("the launch dialled its host attaching %q, want now", dialledWhen)
	}
	stopEntry(t, e, cmds.pids()[0])
}

// TestANewSessionIsPersistedAsTheTUIWouldPersistIt: the host persists the
// provider it starts (serve), so a new session's provider reaches it as
// --provider exactly when the in-process TUI would have written it. With the
// resolved default only a fallback (an unknown CRAZE_PROVIDER), the picker's
// Esc — no explicit choice — passes none: the host resolves the same fallback
// and writes nothing. An explicit choice of that same provider is passed, and
// written once the host's start succeeds.
func TestANewSessionIsPersistedAsTheTUIWouldPersistIt(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit %v", explicit), func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			t.Setenv("CRAZE_PROVIDER", "no-such-provider")
			config := writeCrazeConfig(t, "theme = \"gruvbox\"\n")
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				if !cfg.FallbackDefault || cfg.Provider.Name() != "cursor" {
					t.Fatalf("the premise: provider %q, fallback %v", cfg.Provider.Name(), cfg.FallbackDefault)
				}
				b, err := cfg.NewBackend(cfg.Provider, explicit)
				if err != nil {
					t.Fatalf("NewBackend: %v", err)
				}
				started(t, b)
				_ = b.Close()
				return tui.Result{}, nil
			})
			if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if got := hasArg(argvOf(t, cmds, 0), "--provider=cursor"); got != explicit {
				t.Fatalf("--provider passed %v, want %v: %q", got, explicit, argvOf(t, cmds, 0))
			}
			e := onlyHost(t, env)
			// The start succeeded (the TUI's Start answered nil), and a host
			// persists right after its start: stopping it joins that start.
			stopEntry(t, e, cmds.pids()[0])
			body, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(body), `provider = "cursor"`); got != explicit {
				t.Fatalf("the provider persisted %v, want %v: %q", got, explicit, body)
			}
		})
	}
}

// TestContinueSpawnsALoadOfItsRow: --continue's row is found in the index —
// claiming nothing — and handed to the TUI (Config.Continue, the provider the
// row's, loading), whose load spawns a host with --load: the row's craze id,
// or for a legacy row its <provider>:<sessionId> (R2-6), in the row's own
// workspace, never --continue; the host loads it, under that id or the one
// it gives the legacy row.
func TestContinueSpawnsALoadOfItsRow(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy %v", legacy), func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "load")
			row := sessions.Row{SessionID: "load-1", Provider: "cursor", CWD: absDir(ws), UpdatedAt: time.Now()}
			want := "--load=cursor:load-1"
			if !legacy {
				row.CrazeID = "0199aaaa-bbbb-7ccc-8ddd-0000000000d1"
				want = "--load=" + row.CrazeID
			}
			seedIndexRow(t, row)
			var crazeID string
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				switch {
				case cfg.Continue == nil || cfg.Continue.SessionID != "load-1" || cfg.Continue.CrazeID != row.CrazeID:
					t.Fatalf("Continue %+v, want the row", cfg.Continue)
				case !cfg.Loading || !cfg.ProviderLocked || cfg.Provider.Name() != "cursor":
					t.Fatalf("loading %v, locked %v, provider %q", cfg.Loading, cfg.ProviderLocked, cfg.Provider.Name())
				}
				if locks, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "locks", "*.lock")); len(locks) != 0 {
					t.Fatalf("the launcher claimed %q", locks)
				}
				b, err := cfg.LoadBackend(cfg.Provider, *cfg.Continue)
				if err != nil {
					t.Fatalf("LoadBackend: %v", err)
				}
				started(t, b)
				crazeID = b.Info().CrazeSessionID
				_ = b.Close()
				return tui.Result{}, nil
			})
			f := launchFlags(t, ws)
			f.cont = true
			if err := runTUI(nil, f, hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			argv := argvOf(t, cmds, 0)
			if cmds.count() != 1 || !hasArg(argv, want) || hasArg(argv, "--continue") || hasArg(argv, "--workspace=") {
				t.Fatalf("%d spawned; the host's command line %q, want %s", cmds.count(), argv, want)
			}
			e := onlyHost(t, env)
			switch {
			case !legacy && (crazeID != row.CrazeID || e.CrazeSessionID != row.CrazeID):
				t.Fatalf("the host serves %q, the TUI was given %q, want the row's %s", e.CrazeSessionID, crazeID, row.CrazeID)
			case legacy && (crazeID == "" || e.CrazeSessionID != crazeID || indexRowByID(t, crazeID).SessionID != "load-1"):
				t.Fatalf("the legacy row's host serves %q, the TUI was given %q", e.CrazeSessionID, crazeID)
			}
			stopEntry(t, e, cmds.pids()[0])
		})
	}
}

// TestContinueOfAHeldSessionAttaches (SQ16 over the launch): a --continue of a
// session a host holds that no host serves yet when the launch looks — the
// holder claimed it and is held there (cliChildGate), not yet in the registry
// — spawns a host that is refused held, and the TUI is given the holder,
// found through the rendezvous once it serves the session (let go at the
// rendezvous's first look): its socket, its session. The held answer is what
// covers a host taking the session between the launch's look and its spawn.
// The TUI's quit leaves the holder running, and the flags a new session would
// have taken are named as ignored, once the screen is back.
func TestContinueOfAHeldSessionAttaches(t *testing.T) {
	gate, release, letGo := gateFIFO(t)
	env, ws, cmds := launchHome(t, func(n int) []string {
		if n == 1 {
			return []string{cliChildGate + "=" + gate}
		}
		return nil
	})
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000d2"
	seedIndexRow(t, sessions.Row{SessionID: "held-2", Provider: "cursor", CWD: absDir(ws), CrazeID: id, UpdatedAt: time.Now()})
	polled := pollSignal(t)
	holding := goSpawn(t, context.Background(), spawnOptions{env: env, flags: tuiFlags{force: true, agentBin: fakeAgentPath(t)}, load: id})
	// After goSpawn's cleanup, so it runs first: a failing test lets the
	// holder go before waiting for its spawn.
	t.Cleanup(letGo)
	waitClaimed(t, env, id)
	if _, ok := hostServing(env, id); ok {
		t.Fatal("the premise: a host serves the session before its holder is let go")
	}
	go func() {
		select {
		case <-polled:
			release()
		case <-time.After(serveStep):
		}
	}()
	stderr := captureStderr(t)
	var socket string
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		b, err := cfg.LoadBackend(cfg.Provider, *cfg.Continue)
		if err != nil {
			t.Fatalf("LoadBackend: %v", err)
		}
		lb, ok := b.(*launchedBackend)
		if !ok || !lb.ref.held || lb.ref.child != nil {
			t.Fatalf("the backend %T %+v, want the holder's", b, lb)
		}
		socket = lb.ref.entry.Socket
		started(t, b)
		if got := b.Info().CrazeSessionID; got != id {
			t.Fatalf("attached to session %q, want %s", got, id)
		}
		_ = b.Close()
		return tui.Result{}, nil
	})
	f := launchFlags(t, ws)
	f.cont, f.model = true, "m-ignored"
	if err := runTUI(nil, f, hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	h := spawned(t, holding)
	if h.err != nil {
		t.Fatalf("the holder's spawn: %v", h.err)
	}
	holder := h.ref
	switch {
	case socket != holder.entry.Socket:
		t.Fatalf("attached through %q, the holder serves %q", socket, holder.entry.Socket)
	case cmds.count() != 2:
		t.Fatalf("%d hosts spawned, want the holder and the one refused held", cmds.count())
	case holder.child.Exited():
		t.Fatal("the TUI's quit ended the holder")
	}
	waitReaped(t, cmds.pids()[1])
	// With no command line (a direct call), every flag set counts as given:
	// --agent-bin is the fake agent's.
	if got := stderr(); !strings.Contains(got, "; attached to it (ignored: --model, --agent-bin)") {
		t.Fatalf("stderr %q, want the ignored flag named", got)
	}
	stopEntry(t, onlyHost(t, env), holder.child.PID())
}

// captureStderr points os.Stderr at a file for the rest of the test — the
// deferred stderr is flushed there — and answers a read of what it holds.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = prev
		_ = f.Close()
	})
	return func() string {
		b, _ := os.ReadFile(f.Name())
		return string(b)
	}
}

// TestALaunchFailureIsAStartFailure: a host that cannot come up — here, one
// that exits before it is ready — is a start failure: runTUI's error, exit 1,
// the spawn's own words naming the host's log and the opt-out that runs the
// session inside craze. A host's refusal of what was asked — a session it
// cannot find — is a *tui.Refusal in craze's words, which a picker shows as
// its error row: it names no opt-out, which would not change it.
func TestALaunchFailureIsAStartFailure(t *testing.T) {
	t.Run("the host exits", func(t *testing.T) {
		_, ws, cmds := launchHome(t, func(int) []string { return []string{cliChildPanic + "=craze test: a forced panic"} })
		t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
		fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
			_, err := cfg.NewBackend(cfg.Provider, false)
			if err == nil {
				t.Fatal("a host that exited answered a backend")
			}
			var refused *tui.Refusal
			if errors.As(err, &refused) {
				t.Fatalf("a host that exited is a refusal: %v", err)
			}
			// As the TUI ends a run whose session never came up.
			return tui.Result{StartErr: err}, err
		})
		err := runTUI(nil, launchFlags(t, ws), hostEnv{})
		if err == nil {
			t.Fatal("runTUI answered nil for a session that never started")
		}
		msg, code := diagnose(nil, err)
		switch {
		case code != 1:
			t.Fatalf("exit %d, want 1: %s", code, msg)
		case !strings.HasPrefix(msg, "craze: the session host exited before it was ready"),
			!strings.Contains(msg, "; its log: "),
			!strings.HasSuffix(msg, "; CRAZE_DETACH=0 runs sessions inside craze instead"):
			t.Fatalf("the failure %q", msg)
		}
		waitReaped(t, cmds.pids()[0])
	})
	t.Run("the session's start fails", func(t *testing.T) {
		env, ws, cmds := launchHome(t, nil)
		t.Setenv("CRAZE_FAKE_SCRIPT", "authfail")
		var logPath string
		fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
			b, err := cfg.NewBackend(cfg.Provider, false)
			if err != nil {
				t.Fatalf("NewBackend: %v", err)
			}
			logPath = b.(*launchedBackend).ref.log
			err = b.Start(stepCtx(t))
			var se *remote.StartError
			if !errors.As(err, &se) {
				t.Fatalf("the start: %v, want the host's start failure", err)
			}
			_ = b.Close()
			return tui.Result{StartErr: err}, err
		})
		err := runTUI(nil, launchFlags(t, ws), hostEnv{})
		if err == nil || !strings.HasSuffix(err.Error(), " (the session host's log: "+logPath+")") || logPath == "" {
			t.Fatalf("runTUI: %v, want the start failure naming the log %q", err, logPath)
		}
		// The session never came up: its host is not left running.
		waitReaped(t, cmds.pids()[0])
		assertNoHosts(t, env)
	})
	t.Run("the host refuses", func(t *testing.T) {
		_, ws, _ := launchHome(t, nil)
		t.Setenv("CRAZE_FAKE_SCRIPT", "load")
		fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
			_, err := cfg.LoadBackend(agent.CursorProvider(), sessions.Row{SessionID: "gone", Provider: "cursor", CrazeID: "0199aaaa-bbbb-7ccc-8ddd-0000000000d3"})
			var refused *tui.Refusal
			switch {
			case !errors.As(err, &refused):
				t.Fatalf("a host's refusal: %v (%T), want a *tui.Refusal", err, err)
			case err.Error() != "craze: no session 0199aaaa-bbbb-7ccc-8ddd-0000000000d3":
				t.Fatalf("the refusal %q", err)
			}
			return tui.Result{}, nil
		})
		if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
			t.Fatalf("runTUI: %v", err)
		}
	})
}

// TestAQuitWhileStartingLeavesNoHost: a session the TUI never took is not
// left running once it has quit. A host answered and dialled but never
// adopted is stopped (session.stop); a spawn still waiting for its host's
// answer is cancelled, and the host terminated — and runTUI returns only once
// each is gone: the host is already reaped, and out of the registry, when
// runTUI's answer is read (finish waits for both before it returns).
func TestAQuitWhileStartingLeavesNoHost(t *testing.T) {
	t.Run("answered, never adopted", func(t *testing.T) {
		env, ws, cmds := launchHome(t, nil)
		t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
		fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
			if _, err := cfg.NewBackend(cfg.Provider, false); err != nil {
				t.Fatalf("NewBackend: %v", err)
			}
			return tui.Result{}, nil
		})
		if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
			t.Fatalf("runTUI: %v", err)
		}
		// No wait: the stop's own (hostRef.abandon) was over before runTUI
		// returned.
		if pid := cmds.pids()[0]; processAlive(pid) {
			t.Fatalf("runTUI returned with the unadopted host %d still there (%s)", pid, procState(pid))
		}
		assertNoHosts(t, env)
		dir, _ := rundir.HostLogDir(env)
		logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
		if len(logs) != 1 {
			t.Fatalf("host logs %q", logs)
		}
		waitLog(t, logs[0], "craze serve: stopping: session.stop from client")
	})
	t.Run("in flight", func(t *testing.T) {
		env, ws, cmds := launchHome(t, func(int) []string { return []string{cliChildReady + "=skip"} })
		t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
		answered := make(chan error, 1)
		fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
			go func() {
				_, err := cfg.NewBackend(cfg.Provider, false)
				answered <- err
			}()
			// Serving, so the host acts on the SIGTERM a cancel sends it.
			onlyHost(t, env)
			return tui.Result{}, nil
		})
		if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
			t.Fatalf("runTUI: %v", err)
		}
		// The barrier is runTUI's return itself: finish waited for the spawn
		// in flight, whose cancel terminated its host and waited for it to be
		// reaped. So the host is gone now, with no wait — while the closure's
		// own answer, sent after the spawn has let finish go, may still be on
		// its way, and is read with a step's bound (astra r7-c4 4).
		if pid := cmds.pids()[0]; processAlive(pid) {
			t.Fatalf("runTUI returned with the host of the spawn in flight %d still there (%s)", pid, procState(pid))
		}
		assertNoHosts(t, env)
		select {
		case err := <-answered:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the spawn in flight answered %v, want it cancelled", err)
			}
		case <-time.After(serveStep):
			t.Fatalf("the spawn in flight had not answered %v after runTUI returned", serveStep)
		}
	})
}

// assertNoHosts: nothing is left in env's registry.
func assertNoHosts(t *testing.T, env rundir.Env) {
	t.Helper()
	if entries, _ := rundir.Hosts(env); len(entries) != 0 {
		t.Fatalf("hosts left in the registry: %+v", entries)
	}
}

// ttyRun is runTUI driving the real TUI in a pty of its own, the process's
// stdio pointed at it while it runs.
type ttyRun struct {
	ptmx *os.File
	tail *ptyTail
	done chan error
}

// runInPTY starts runTUI(nil, f) in a fresh 100×30 pty. The test ends it
// (ctrlD) before it returns; its pty is closed when the test ends.
func runInPTY(t *testing.T, f *tuiFlags) *ttyRun {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() {
		_ = ptmx.Close()
		_ = tty.Close()
	})
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Skipf("pty resize: %v", err)
	}
	r := &ttyRun{ptmx: ptmx, tail: newPTYTail(ptmx), done: make(chan error, 1)}
	prevIn, prevOut, prevErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = tty, tty, tty
	go func() { r.done <- runTUI(nil, f, hostEnv{}) }()
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = prevIn, prevOut, prevErr })
	return r
}

// see waits a step for the pty to have shown text, read with its escape
// sequences stripped: every part of a row is styled on its own.
func (r *ttyRun) see(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for !strings.Contains(ansi.Strip(r.tail.text()), text) {
		if time.Now().After(deadline) {
			t.Fatalf("the TUI never showed %q; got %q", text, ansi.Strip(r.tail.text()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ctrlD quits the TUI and waits a step for runTUI's answer.
func (r *ttyRun) ctrlD(t *testing.T) error {
	t.Helper()
	if _, err := r.ptmx.Write([]byte{0x04}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		return err
	case <-time.After(serveStep):
		t.Fatal("craze did not quit after ctrl+d")
		return nil
	}
}

// hangUp closes the TUI's terminal as far as the TUI can tell — the SIGHUP a
// closed tab sends, to this process, which the running TUI has caught
// (tui.Run) — and waits a step for runTUI's answer. The TUI registers for it
// before its first frame, and a test calls this only once one is on screen.
func (r *ttyRun) hangUp(t *testing.T) error {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		return err
	case <-time.After(serveStep):
		t.Fatal("craze did not quit after its terminal hung up")
		return nil
	}
}

// TestADetachedSessionOutlivesItsTUI is the launch end to end, the real TUI in
// a pty (plan 030 AC2, AC3): craze starts a session in a host of its own, the
// TUI its client; a prompt goes through it and its turn ends; the terminal
// closes (SIGHUP) — a view close — and the host goes on serving the session,
// which the host has indexed; a second craze -c finds the row and the host
// serving it, and attaches to that host — no host spawned for it, no log
// written — its screen the transcript the first one left. Its /exit (Ctrl+D) is
// the explicit quit, in every client
// (decision 11): the session's host is stopped — the TUI quits, saying nothing
// about the end it asked for, the host goes, and the index row stays.
func TestADetachedSessionOutlivesItsTUI(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	first := runInPTY(t, launchFlags(t, ws))
	first.see(t, " craze ─")
	e := waitServingEntry(t, env, true, func() error { return nil }, &lockedBuffer{})
	// The session is up in the TUI: status row 1 counts its elapsed time.
	first.see(t, statusElapsed)
	const prompt = "hello-detached-host"
	if _, err := first.ptmx.Write([]byte(prompt)); err != nil {
		t.Fatal(err)
	}
	first.see(t, prompt)
	if _, err := first.ptmx.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}
	if lt := waitLastTurn(t, e); lt.TurnID == "" {
		t.Fatalf("the turn ended %+v", lt)
	}
	if err := first.hangUp(t); err != nil {
		t.Fatalf("the first craze, its terminal closed: %v", err)
	}
	if got := onlyHost(t, env); got.HostID != e.HostID || cmds.count() != 1 {
		t.Fatalf("after the terminal closed: %+v (%d spawned), want the host %s still serving", got, cmds.count(), e.HostID)
	}
	if row := waitIndexRowByID(t, e.CrazeSessionID); row.CWD != absDir(ws) {
		t.Fatalf("the host's index row %+v", row)
	}

	f := launchFlags(t, ws)
	f.cont = true
	second := runInPTY(t, f)
	second.see(t, " craze ─")
	second.see(t, prompt)
	second.see(t, statusElapsed)
	// The quit's own bound (2 s) and the host's stop sequence's are the
	// code's (tui.quitStopWait; flushWait, closeWait, serveStartJoin): each
	// wait here is a step's, since this also runs starved.
	if err := second.ctrlD(t); err != nil {
		t.Fatalf("craze -c's /exit: %v", err)
	}
	if n := cmds.count(); n != 1 || len(hostLogs(t, env)) != 1 {
		t.Fatalf("%d hosts spawned, host logs %q: want the first alone, which craze -c attached to", n, hostLogs(t, env))
	}
	waitReaped(t, cmds.pids()[0])
	assertNoHosts(t, env)
	if row := indexRowByID(t, e.CrazeSessionID); row.CWD != absDir(ws) {
		t.Fatalf("the index row after the stop: %+v", row)
	}
	if out := ansi.Strip(second.tail.text()); strings.Contains(out, "session ended") {
		t.Fatalf("the client that stopped the session reported its end: %q", out)
	}
}

// TestSIGTERMToAClientTUIDetaches (plan 030 §3.6): SIGTERM to the launching
// TUI — caught by the running program, which quits through finishRun without
// the explicit quit — is a view close: runTUI exits 0 and the session's host
// goes on serving it.
func TestSIGTERMToAClientTUIDetaches(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runInPTY(t, launchFlags(t, ws))
	r.see(t, " craze ─")
	e := waitServingEntry(t, env, true, func() error { return nil }, &lockedBuffer{})
	r.see(t, statusElapsed)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("runTUI after SIGTERM: %v", err)
		}
	case <-time.After(serveStep):
		t.Fatal("craze did not quit after SIGTERM")
	}
	if got := onlyHost(t, env); got.HostID != e.HostID {
		t.Fatalf("after SIGTERM to the TUI: %+v, want the host %s still serving", got, e.HostID)
	}
	stopEntry(t, e, cmds.pids()[0])
}

// TestADetachedStartFailureStaysOnScreen, the real TUI in a pty: a detached
// session whose start fails is the in-process TUI's start failure — the TUI
// stays up with the start's own error and where the host's log is (the
// agent's stderr is there now), never a session that "ended" — and the run
// exits with it once the user quits. The host, whose session never came up,
// is not left running.
func TestADetachedStartFailureStaysOnScreen(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "authfail")
	r := runInPTY(t, launchFlags(t, ws))
	r.see(t, " craze ─")
	r.see(t, "authentication failed")
	r.see(t, "(the session host's log: ")
	err := r.ctrlD(t)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || !strings.Contains(err.Error(), "host-logs") {
		t.Fatalf("runTUI: %v, want the start failure naming the host's log", err)
	}
	waitReaped(t, cmds.pids()[0])
	assertNoHosts(t, env)
}

// statusElapsed is status row 1's last part once a session is up — its
// elapsed time, 0m for a minute — which the starting row does not draw.
const statusElapsed = " │ 0m"

// hostLogs is every host log in env's host logs' directory.
func hostLogs(t *testing.T, env rundir.Env) []string {
	t.Helper()
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	return logs
}

// unusableRuntimeDir is a CRAZE_RUNTIME_DIR no host can bind its socket
// under: a directory of the user's own whose mode is 0755. A socket base must
// be 0700, and an explicit one is neither repaired nor fallen through
// (rundir.socketDir), so a host's bind fails — and nothing a launcher does,
// nor anything a host does before its bind, reads the runtime tree.
func unusableRuntimeDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(shortRuntimeDir(t), "open")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// holdStarted holds every launched backend's Start where it has answered nil
// and the answer has not yet gone back to the TUI (launchStarted): held is
// closed when the first gets there, and release lets each go — the test's
// call, or its end's.
func holdStarted(t *testing.T) (held <-chan struct{}, release func()) {
	t.Helper()
	h, r := make(chan struct{}), make(chan struct{})
	var heldOnce, releaseOnce sync.Once
	release = func() { releaseOnce.Do(func() { close(r) }) }
	prev := launchStarted
	launchStarted = func() {
		heldOnce.Do(func() { close(h) })
		<-r
	}
	t.Cleanup(func() {
		release()
		launchStarted = prev
	})
	return h, release
}

// TestAQuitBeforeTheTUIHeardItsStartStopsTheHost (astra r7-c4 1, X27), the
// real TUI in a pty: the host's start has answered — the launched backend's
// Start returned nil — and the TUI has not heard it: the answer, its
// startedMsg, is held there (launchStarted) while a SIGTERM arrives or the
// terminal hangs up, and the program quits on that first. The session never
// came up in the TUI, so the host this launch spawned is stopped, and gone
// before runTUI returns; the answer, let go once the program is over,
// acknowledges nothing.
func TestAQuitBeforeTheTUIHeardItsStartStopsTheHost(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			held, release := holdStarted(t)
			r := runInPTY(t, launchFlags(t, ws))
			r.see(t, " craze ─")
			select {
			case <-held:
			case <-time.After(serveStep):
				t.Fatal("the host's start never answered")
			}
			if e := onlyHost(t, env); e.CrazeSessionID == "" {
				t.Fatalf("the premise: the host serves no session: %+v", e)
			}
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-r.done:
				if err != nil {
					t.Fatalf("runTUI after %v: %v", sig, err)
				}
			case <-time.After(serveStep):
				t.Fatalf("craze did not quit after %v", sig)
			}
			// No wait: finish stopped the host (hostRef.abandon) before
			// runTUI returned.
			if pid := cmds.pids()[0]; processAlive(pid) {
				t.Fatalf("after %v the host %d the TUI never saw come up is still there (%s)", sig, pid, procState(pid))
			}
			assertNoHosts(t, env)
			release()
		})
	}
}

// TestAHostThatCannotBindIsAStartFailure (astra r7-c4 2, X24): a host that
// cannot come up — here, one whose control socket cannot bind, under an
// unusable CRAZE_RUNTIME_DIR — is a start failure wherever the spawn was
// asked for: Init's for a provider known, and a choice on the provider picker
// or the resume picker. Its words are the host's reason, its log and the
// opt-out that runs the session inside craze (which binds no socket to
// start); it is no *tui.Refusal, and no picker comes back for it — choosing
// again would not change it. The host has gone, and its log says why.
func TestAHostThatCannotBindIsAStartFailure(t *testing.T) {
	for _, from := range []string{"a known provider", "the provider picker", "the resume picker"} {
		t.Run(from, func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000d6"
			seedIndexRow(t, sessions.Row{SessionID: "bind-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id, UpdatedAt: time.Now()})
			t.Setenv("CRAZE_RUNTIME_DIR", unusableRuntimeDir(t))
			var spawnErr error
			var view string
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				newB, loadB := cfg.NewBackend, cfg.LoadBackend
				cfg.NewBackend = func(p agent.Provider, explicit bool) (backend.Backend, error) {
					b, err := newB(p, explicit)
					spawnErr = err
					return b, err
				}
				cfg.LoadBackend = func(p agent.Provider, row sessions.Row) (backend.Backend, error) {
					b, err := loadB(p, row)
					spawnErr = err
					return b, err
				}
				var m tea.Model = tui.New(cfg)
				// Wide enough that the failure's row is not wrapped.
				m, _ = m.Update(tea.WindowSizeMsg{Width: 400, Height: 30})
				cmd := m.Init()
				if from != "a known provider" {
					// The provider picker's one command is the read of its
					// rows' availability (plan 036 §3.3), taken here.
					if cmd != nil {
						m = feed(m, runCmd(t, cmd, serveStep))
					}
					if spawnErr != nil || cmds.count() != 0 {
						t.Fatal("the premise: a picker is up, and Init spawns nothing")
					}
					m, cmd = m.Update(enterKey)
				}
				m = feed(m, runCmd(t, cmd, serveStep))
				view = ansi.Strip(m.View())
				return tui.Result{StartErr: spawnErr}, spawnErr
			})
			argv := []string{"--workspace", ws, "--agent-bin", fakeAgentPath(t)}
			switch from {
			case "a known provider":
				argv = append(argv, "--provider", "cursor")
			case "the resume picker":
				argv = append(argv, "--resume")
			}
			cmd, f := parseTUIFlags(t, argv...)
			err := runTUI(cmd, f, hostEnv{})
			var refused *tui.Refusal
			switch {
			case spawnErr == nil:
				t.Fatal("a host that could not bind answered a backend")
			case errors.As(spawnErr, &refused):
				t.Fatalf("a host that could not bind is a refusal of the choice: %v", spawnErr)
			case err == nil || err.Error() != spawnErr.Error():
				t.Fatalf("runTUI: %v, want the start failure %v", err, spawnErr)
			}
			logs := hostLogs(t, env)
			msg := spawnErr.Error()
			switch {
			case len(logs) != 1:
				t.Fatalf("host logs %q, want the one host's", logs)
			case !strings.HasPrefix(msg, "craze: the session host could not start: the control socket: "),
				!strings.Contains(msg, "; its log: "+logs[0]+";"),
				!strings.HasSuffix(msg, "; CRAZE_DETACH=0 runs sessions inside craze instead"):
				t.Fatalf("the failure %q", msg)
			case !strings.Contains(view, "CRAZE_DETACH=0 runs sessions inside craze instead"):
				t.Fatalf("the start failure is not on screen:\n%s", view)
			case strings.Contains(view, "enter starts") || strings.Contains(view, "enter loads"):
				t.Fatalf("a picker came back for a host that could not come up:\n%s", view)
			}
			if code := exitOf(err); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			waitLog(t, logs[0], "craze serve: the control socket: ")
			waitReaped(t, cmds.pids()[0])
			assertNoHosts(t, env)
		})
	}
}

// exitOf is the exit status err makes (diagnose).
func exitOf(err error) int {
	_, code := diagnose(nil, err)
	return code
}

// TestAResumeChoiceIsHeldToItsFlagsByItsHost (astra r7-c4 3, X25): under
// --agent-bin, which a native session cannot take, the launch's resume picker
// does not refuse a native row itself — its Config carries no refusal to ask
// — and Enter spawns a load of it; the host holds the row to the flags only
// after its claim, as --continue's does:
//   - held: a session another craze holds (a craze of this process, which has
//     claimed it and serves it only once the rendezvous is looking, so the
//     launch's look finds no host serving it) is attached to — the host is
//     refused held, the flags go unasked — naming --agent-bin as ignored;
//   - free: nobody holds it, and the host that claims it refuses --agent-bin:
//     a refusal of the choice, which the picker shows as its error row, the
//     picker up again; the claim is given back and nothing is left.
func TestAResumeChoiceIsHeldToItsFlagsByItsHost(t *testing.T) {
	const binRefusal = "--agent-bin cannot be used with provider native, which runs inside craze"
	for _, held := range []bool{true, false} {
		name := "free"
		if held {
			name = "held"
		}
		t.Run(name, func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			id := "0199aaaa-bbbb-7ccc-8ddd-0000000000d7"
			if held {
				id = "0199aaaa-bbbb-7ccc-8ddd-0000000000d8"
			}
			seedIndexRow(t, sessions.Row{SessionID: "native-1", Provider: "native", CWD: absDir(ws), CrazeID: id, UpdatedAt: time.Now()})
			var holderID string
			if held {
				var rh *runHost
				rh, holderID = servingHost(t, env, io.Discard)
				if _, err := rh.claims.claimSession(id); err != nil {
					t.Fatalf("the holder's claim: %v", err)
				}
				if _, ok := hostServing(env, id); ok {
					t.Fatal("the premise: a host serves the session before the rendezvous looks")
				}
				eng := nativeStubEngine(t, id)
				polled := pollSignal(t)
				go func() {
					select {
					case <-polled:
						rh.onEngine(eng)
						_ = eng.Start(context.Background())
					case <-time.After(serveStep):
					}
				}()
			}
			stderr := captureStderr(t)
			var got backend.Backend
			var gotErr error
			var view string
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				if cfg.RefuseLoad != nil {
					t.Error("the launch's resume picker is handed the flags to refuse a row with")
				}
				load := cfg.LoadBackend
				cfg.LoadBackend = func(p agent.Provider, row sessions.Row) (backend.Backend, error) {
					got, gotErr = load(p, row)
					return got, gotErr
				}
				var m tea.Model = tui.New(cfg)
				m, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
				m, cmd := m.Update(enterKey)
				if cmd == nil {
					t.Fatal("Enter on the native row under --agent-bin spawned nothing")
				}
				msgs := runCmd(t, cmd, serveStep)
				if held {
					if got != nil {
						started(t, got)
						if gotID := got.Info().CrazeSessionID; gotID != id {
							t.Errorf("attached to session %q, want %s", gotID, id)
						}
						_ = got.Close()
					}
					return tui.Result{}, nil
				}
				m = feed(m, msgs)
				view = ansi.Strip(m.View())
				return tui.Result{}, nil
			})
			cmd, f := parseTUIFlags(t, "--resume", "--workspace", ws, "--agent-bin", fakeAgentPath(t))
			if err := runTUI(cmd, f, hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if cmds.count() != 1 {
				t.Fatalf("%d hosts spawned, want the row's", cmds.count())
			}
			waitReaped(t, cmds.pids()[0])
			if held {
				lb, ok := got.(*launchedBackend)
				switch {
				case gotErr != nil:
					t.Fatalf("a held native row under --agent-bin: %v", gotErr)
				case !ok || !lb.ref.held || lb.ref.entry.HostID != holderID:
					t.Fatalf("the backend %T %+v, want the holder %s's", got, lb, holderID)
				case !strings.Contains(stderr(), "; attached to it (ignored: --agent-bin)"):
					t.Fatalf("stderr %q, want --agent-bin named as ignored", stderr())
				}
				return
			}
			// The picker's error row is the refusal less "craze: ", wrapped at
			// the dialog's width: its first words are on one line.
			var refused *tui.Refusal
			switch {
			case got != nil || !errors.As(gotErr, &refused) || gotErr.Error() != "craze: "+binRefusal:
				t.Fatalf("a free native row under --agent-bin: %T %v, want the host's refusal", got, gotErr)
			case !strings.Contains(view, "--agent-bin cannot be used with provider native") || !strings.Contains(view, "enter loads"):
				t.Fatalf("the resume picker is not back with the refusal:\n%s", view)
			case strings.Contains(view, "CRAZE_DETACH=0"):
				t.Fatalf("a refusal of the choice names the opt-out:\n%s", view)
			}
			assertNoHosts(t, env)
			if _, err := anotherCraze(t).claimSession(id); err != nil {
				t.Fatalf("the refusing host kept its claim: %v", err)
			}
		})
	}
}

// nativeStubEngine is an engine over a Stub whose provider is native, for the
// session id: a native session another craze of this process holds.
func nativeStubEngine(t *testing.T, id string) *engine.Engine {
	t.Helper()
	s := tui.NewStubNoPrimary()
	s.SetProvider(agent.NativeProvider())
	eng, err := engine.New(s, engine.Options{CrazeSessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// TestAReattachSpawnsNothing (plan 030 C4r): --continue, and a --resume
// choice, of a session a running host serves attach to that host directly —
// no host spawned, no host log written, nothing claimed — ignoring the flags
// of a new session as a held answer does, and saying so. The host goes on
// serving after the TUI's quit, a view close.
func TestAReattachSpawnsNothing(t *testing.T) {
	for _, via := range []string{"--continue", "--resume"} {
		t.Run(via, func(t *testing.T) {
			env, ws, cmds := launchHome(t, nil)
			t.Setenv("CRAZE_FAKE_SCRIPT", "load")
			const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000d9"
			seedIndexRow(t, sessions.Row{SessionID: "re-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id, UpdatedAt: time.Now()})
			holder, err := spawnNow(t, spawnOptions{env: env, flags: tuiFlags{force: true, agentBin: fakeAgentPath(t)}, load: id})
			if err != nil {
				t.Fatalf("the host's spawn: %v", err)
			}
			logs := hostLogs(t, env)
			stderr := captureStderr(t)
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				row := cfg.Continue
				if row == nil && len(cfg.Resume) > 0 {
					row = &cfg.Resume[0]
				}
				if row == nil || row.CrazeID != id {
					t.Fatalf("the row to load: %+v", row)
				}
				b, err := cfg.LoadBackend(cfg.Provider, *row)
				if err != nil {
					t.Fatalf("LoadBackend: %v", err)
				}
				lb, ok := b.(*launchedBackend)
				if !ok || !lb.ref.held || lb.ref.child != nil || lb.ref.entry.HostID != holder.entry.HostID {
					t.Fatalf("the backend %T %+v, want the host %s serving the session", b, lb, holder.entry.HostID)
				}
				started(t, b)
				if got := b.Info().CrazeSessionID; got != id {
					t.Fatalf("attached to session %q, want %s", got, id)
				}
				_ = b.Close()
				return tui.Result{}, nil
			})
			f := launchFlags(t, ws)
			f.model = "m-ignored"
			f.cont, f.resume = via == "--continue", via == "--resume"
			if err := runTUI(nil, f, hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			switch after := hostLogs(t, env); {
			case cmds.count() != 1:
				t.Fatalf("%d hosts spawned, want only the one serving the session", cmds.count())
			case !slices.Equal(after, logs):
				t.Fatalf("host logs %q after the reattach, were %q", after, logs)
			case holder.child.Exited():
				t.Fatal("the TUI's quit ended the host")
			case !strings.Contains(stderr(), "; attached to it (ignored: --model, --agent-bin)"):
				t.Fatalf("stderr %q, want the ignored flags named", stderr())
			}
			if locks, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "locks", "*.lock")); len(locks) != 1 {
				t.Fatalf("locks %q, want the host's claim alone", locks)
			}
			stopEntry(t, onlyHost(t, env), holder.child.PID())
		})
	}
}

// TestAReattachToAStoppingHostSpawnsInstead (plan 030 C5r2, astra r9-fix45
// 2): the direct reattach takes a host only once its attach has succeeded. A
// host serving the session that is stopping — its attach fence up, which
// refuses every attach closing while hello is still answered — is not taken
// on its hello, which would be the TUI's failed start: the candidate is
// closed, and the launch falls back to a spawn of its own, whose host loads
// the session once the stopping one has gone — here as that spawn begins, the
// test's host command finishing the stop (its socket closed, its entry and
// its claim given up) — and the TUI ends with a working session.
func TestAReattachToAStoppingHostSpawnsInstead(t *testing.T) {
	// finishStop is the stopping host's end, run as the first spawn begins
	// (spawnAsChild's extra runs before the host command starts).
	var finishStop func()
	env, ws, cmds := launchHome(t, func(n int) []string {
		if n == 1 && finishStop != nil {
			finishStop()
		}
		return nil
	})
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000da"
	seedIndexRow(t, sessions.Row{SessionID: "re-2", Provider: "cursor", CWD: absDir(ws), CrazeID: id, UpdatedAt: time.Now()})

	// The stopping host: serving the session, started, its attach fence up.
	rh, _ := servingHost(t, env, io.Discard)
	if _, err := rh.claims.claimSession(id); err != nil {
		t.Fatalf("the stopping host's claim: %v", err)
	}
	stub := tui.NewStubNoPrimary()
	stub.SetProvider(agent.CursorProvider())
	eng, err := engine.New(stub, engine.Options{CrazeSessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	select {
	case <-rh.publish(eng):
	case <-time.After(serveStep):
		t.Fatal("the stopping host never published its session")
	}
	if err := eng.Start(stepCtx(t)); err != nil {
		t.Fatalf("the stopping host's start: %v", err)
	}
	_, release := rh.ctl.server.FenceAttaches()
	t.Cleanup(release)
	e, ok := hostServing(env, id)
	if !ok {
		t.Fatal("the premise: no host serves the session")
	}
	probe, err := remote.DialSession(stepCtx(t), e.Socket, remote.SessionOptions{SessionID: id, When: protocol.WhenNow})
	if err != nil {
		t.Fatalf("the premise: the stopping host does not answer hello: %v", err)
	}
	var refused *remote.Error
	if err := probe.Attach(stepCtx(t)); !errors.As(err, &refused) || refused.Reason != protocol.ReasonClosing {
		t.Fatalf("the premise: the stopping host's attach answered %v, want refused closing", err)
	}
	_ = probe.Close()
	finishStop = rh.close

	var spawned hostRef
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		if cfg.Continue == nil || cfg.Continue.CrazeID != id {
			t.Fatalf("the row to load: %+v", cfg.Continue)
		}
		b, err := cfg.LoadBackend(cfg.Provider, *cfg.Continue)
		if err != nil {
			t.Fatalf("LoadBackend: %v", err)
		}
		started(t, b)
		if got := b.Info().CrazeSessionID; got != id {
			t.Fatalf("the session %q, want %s", got, id)
		}
		lb, ok := b.(*launchedBackend)
		if !ok || lb.ref.held || lb.ref.child == nil {
			t.Fatalf("the backend %T %+v, want a host this launch spawned, not the stopping one", b, lb)
		}
		spawned = lb.ref
		_ = b.Close()
		return tui.Result{}, nil
	})
	f := launchFlags(t, ws)
	f.cont = true
	if err := runTUI(nil, f, hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if cmds.count() != 1 {
		t.Fatalf("%d hosts spawned, want the fallback's one", cmds.count())
	}
	if spawned.child.Exited() {
		t.Fatal("the TUI's quit ended the session it came up with")
	}
	stopEntry(t, onlyHost(t, env), spawned.child.PID())
}
