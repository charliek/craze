package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
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

// started is b started as the TUI starts the backend it adopts, within a step.
func started(t *testing.T, b backend.Backend) {
	t.Helper()
	if err := b.Start(stepCtx(t)); err != nil {
		t.Fatalf("the adopted backend's start: %v", err)
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
// closure, and no host is spawned.
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
// provider persistence (the host's) — but the launch closures; it is its
// session's client and not a Viewer, so the host-status hub stays its own and
// reports for the session it shows (plan 030 §3.7). Building it spawns
// nothing, binds nothing and claims nothing: the TUI spawns when it runs.
func TestALaunchIsItsHostsClient(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		switch {
		case cfg.NewBackend == nil || cfg.LoadBackend == nil:
			t.Error("the launch has no spawn closures")
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
// session a running host holds spawns a host that is refused held, and the
// TUI is given the holder, found through the rendezvous: its socket, its
// session. The TUI's quit leaves the holder running, and the flags a new
// session would have taken are named as ignored, once the screen is back.
func TestContinueOfAHeldSessionAttaches(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000d2"
	seedIndexRow(t, sessions.Row{SessionID: "held-2", Provider: "cursor", CWD: absDir(ws), CrazeID: id, UpdatedAt: time.Now()})
	holder, err := spawnNow(t, spawnOptions{env: env, flags: tuiFlags{force: true, agentBin: fakeAgentPath(t)}, load: id})
	if err != nil {
		t.Fatalf("the holder's spawn: %v", err)
	}
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
	switch {
	case socket != holder.entry.Socket:
		t.Fatalf("attached through %q, the holder serves %q", socket, holder.entry.Socket)
	case cmds.count() != 2:
		t.Fatalf("%d hosts spawned, want the holder and the one refused held", cmds.count())
	case holder.child.exited():
		t.Fatal("the TUI's quit ended the holder")
	}
	waitReaped(t, cmds.pids()[1])
	// With no command line (a direct call), every flag set counts as given:
	// --agent-bin is the fake agent's.
	if got := stderr(); !strings.Contains(got, "; attached to it (ignored: --model, --agent-bin)") {
		t.Fatalf("stderr %q, want the ignored flag named", got)
	}
	stopEntry(t, onlyHost(t, env), holder.child.pid)
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
// each is gone.
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
		pid := cmds.pids()[0]
		if processAlive(pid) && !reapedSoon(pid) {
			t.Fatal("runTUI returned with the unadopted host still running")
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
		select {
		case err := <-answered:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the spawn in flight answered %v, want it cancelled", err)
			}
		default:
			t.Fatal("runTUI returned before the spawn in flight had")
		}
		waitReaped(t, cmds.pids()[0])
		assertNoHosts(t, env)
	})
}

// reapedSoon waits a step for pid to be gone.
func reapedSoon(pid int) bool {
	deadline := time.Now().Add(serveStep)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
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

// TestADetachedSessionOutlivesItsTUI is the launch end to end, the real TUI in
// a pty (plan 030 AC2): craze starts a session in a host of its own, the TUI
// its client; a prompt goes through it and its turn ends; the TUI quits and
// the host goes on serving the session, which the host has indexed; a second
// craze -c finds the row, is refused held and attaches to that host through
// the rendezvous, its screen the transcript the first one left; it quits too,
// and the host still serves until its own stop.
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
	if err := first.ctrlD(t); err != nil {
		t.Fatalf("the first craze: %v", err)
	}
	if got := onlyHost(t, env); got.HostID != e.HostID || cmds.count() != 1 {
		t.Fatalf("after the TUI quit: %+v (%d spawned), want the host %s still serving", got, cmds.count(), e.HostID)
	}
	if row := indexRowByID(t, e.CrazeSessionID); row.CWD != absDir(ws) {
		t.Fatalf("the host's index row %+v", row)
	}

	f := launchFlags(t, ws)
	f.cont = true
	second := runInPTY(t, f)
	second.see(t, " craze ─")
	second.see(t, prompt)
	if err := second.ctrlD(t); err != nil {
		t.Fatalf("craze -c: %v", err)
	}
	if cmds.count() != 2 {
		t.Fatalf("%d hosts spawned, want the first and the one refused held", cmds.count())
	}
	waitReaped(t, cmds.pids()[1])
	if got := onlyHost(t, env); got.HostID != e.HostID {
		t.Fatalf("after craze -c quit: %+v, want the host %s still serving", got, e.HostID)
	}
	stopEntry(t, e, cmds.pids()[0])
	assertNoHosts(t, env)
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
