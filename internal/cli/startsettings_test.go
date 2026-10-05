package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// Plan 032 §3.11 (C14) on the command line: --effort and --fast/--no-fast as
// session flags, carried to every host and session a command line starts; and
// the agent binary resolved per provider (P7) — the launch's --agent-bin, else
// CRAZE_AGENT_BIN, both only for the launch's own provider, then
// `[agents].<provider>`, then PATH — with the launcher passing the flag only
// to, and leaving the variable only in the environment of, a host of that
// provider (A13). Every host is this test binary run as craze serve with the
// fake agent, and every wait is bounded on its own (serveStep).

// TestTheFastFlagsAreOneSetting: --fast, --no-fast and neither are the
// session's three fast settings, and both at once is the usage error --ask
// with --plan is.
func TestTheFastFlagsAreOneSetting(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want *bool
	}{
		{nil, nil},
		{[]string{"--fast"}, boolPtr(true)},
		{[]string{"--no-fast"}, boolPtr(false)},
	} {
		_, f := parseTUIFlags(t, tc.argv...)
		if err := f.settle(); err != nil {
			t.Fatalf("%q: %v", tc.argv, err)
		}
		got := f.fastSetting()
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("%q: fast %v, want %v", tc.argv, got, tc.want)
		}
		var back tuiFlags
		back.setFast(got)
		if back.fast != f.fast || back.noFast != f.noFast {
			t.Fatalf("%q: setFast(fastSetting()) is %+v", tc.argv, back)
		}
	}
	_, f := parseTUIFlags(t, "--fast", "--no-fast")
	err := f.settle()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || ee.msg != "craze: --fast and --no-fast are mutually exclusive" {
		t.Fatalf("--fast with --no-fast: %v, want the usage error", err)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestAgentBinaryOrder is P7's resolution, step by step: for the launch's own
// provider --agent-bin, else CRAZE_AGENT_BIN (left to the agent's own lookup,
// which reads it), else [agents], else PATH; for any other provider [agents],
// else PATH, the variable left out of the lookup — the cases a launch's
// override used to reach (SF-93's negative control). A config value craze
// cannot take is said on diag and ignored; native has no binary.
func TestAgentBinaryOrder(t *testing.T) {
	cursor, grok, native := agent.CursorProvider(), agent.GrokProvider(), agent.NativeProvider()
	for _, tc := range []struct {
		name      string
		p, launch agent.Provider
		flag, env string
		config    string
		want      agentBin
		wantDiag  bool
	}{
		{name: "own: the flag first", p: cursor, launch: cursor, flag: "/f", env: "/e", config: "cursor = \"/c\"", want: agentBin{path: "/f"}},
		{name: "own: then the variable", p: cursor, launch: cursor, env: "/e", config: "cursor = \"/c\"", want: agentBin{}},
		{name: "own: then [agents]", p: cursor, launch: cursor, config: "cursor = \"/c\"", want: agentBin{path: "/c", noEnv: true}},
		{name: "own: then PATH", p: cursor, launch: cursor, want: agentBin{noEnv: true}},
		{name: "another: [agents], never the flag or the variable", p: grok, launch: cursor, flag: "/f", env: "/e", config: "grok = \"/g\"", want: agentBin{path: "/g", noEnv: true}},
		{name: "another: PATH, never the flag or the variable", p: grok, launch: cursor, flag: "/f", env: "/e", config: "cursor = \"/c\"", want: agentBin{noEnv: true}},
		{name: "a relative [agents] is ignored", p: grok, launch: cursor, config: "grok = \"bin/grok\"", want: agentBin{noEnv: true}, wantDiag: true},
		{name: "native has none", p: native, launch: cursor, flag: "/f", env: "/e", config: "cursor = \"/c\"", want: agentBin{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envAgentBin, tc.env)
			crazeHome(t)
			if tc.config != "" {
				writeCrazeConfig(t, "[agents]\n"+tc.config+"\n")
			}
			var diag bytes.Buffer
			if got := agentBinary(tc.p, tc.launch, tc.flag, &diag); got != tc.want {
				t.Fatalf("agentBinary = %+v, want %+v", got, tc.want)
			}
			if (diag.Len() > 0) != tc.wantDiag {
				t.Fatalf("diag %q", diag.String())
			}
			var opts agent.Options
			agentBinary(tc.p, tc.launch, tc.flag, nil).apply(&opts)
			if opts.Binary != tc.want.path || opts.NoBinaryEnv != tc.want.noEnv {
				t.Fatalf("applied %q, %v", opts.Binary, opts.NoBinaryEnv)
			}
		})
	}
}

// TestSessionOptionsCarryTheStartSettings: the in-process path's session
// description (and craze serve's) takes --effort, trimmed, and --fast/--no-fast
// for every session the command line starts, and its binary by P7: the
// launch's own provider gets the flag, another (the provider picker's other
// rows) its own.
func TestSessionOptionsCarryTheStartSettings(t *testing.T) {
	t.Setenv(envAgentBin, "")
	crazeHome(t)
	_, f := parseTUIFlags(t, "--effort", " high ", "--no-fast", "--agent-bin", "/f")
	cursor, grok := agent.CursorProvider(), agent.GrokProvider()
	own := sessionOptions(f, t.TempDir(), "", nil, nil, nil, cursor, cursor, sessions.Row{})
	if own.Effort != "high" || own.Fast == nil || *own.Fast || own.Binary != "/f" || own.NoBinaryEnv {
		t.Fatalf("the launch's own session: %+v", own)
	}
	other := sessionOptions(f, t.TempDir(), "", nil, nil, nil, grok, cursor, sessions.Row{})
	if other.Effort != "high" || other.Fast == nil || other.Binary != "" || !other.NoBinaryEnv {
		t.Fatalf("another provider's session: %+v", other)
	}
	_, none := parseTUIFlags(t)
	if o := sessionOptions(none, t.TempDir(), "", nil, nil, nil, cursor, cursor, sessions.Row{}); o.Effort != "" || o.Fast != nil {
		t.Fatalf("no start settings: %+v", o)
	}
}

// agentWrapper writes an executable named name in dir that notes each run in
// marker and runs the fake agent as script: an agent binary a test can tell
// apart from the fake itself.
func agentWrapper(t *testing.T, dir, name, script, marker string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := fmt.Sprintf("#!/bin/sh\necho ran >> '%s'\nCRAZE_FAKE_SCRIPT=%s exec '%s' \"$@\"\n", marker, script, fakeAgentPath(t))
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// ran reports whether a wrapper noted a run in marker.
func ran(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// grokBinary sets up a grok agent binary that runs the fake as script, the
// way where says — "config" in `[agents].grok` of this test's CRAZE_HOME,
// "path" as `grok` on PATH — and answers its marker.
func grokBinary(t *testing.T, where, script string) string {
	t.Helper()
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	marker := filepath.Join(t.TempDir(), "grok-ran")
	dir := t.TempDir()
	bin := agentWrapper(t, dir, "grok", script, marker)
	switch where {
	case "config":
		// Into the CRAZE_HOME the test already has: the registry's namespace
		// is keyed by it.
		home := os.Getenv("CRAZE_HOME")
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[agents]\ngrok = %q\n", bin)), 0o600); err != nil {
			t.Fatal(err)
		}
	case "path":
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return marker
}

// hostEnvOf is the n-th spawned host's environment, as it was started.
func hostEnvOf(t *testing.T, cmds *childCmds, n int) []string {
	t.Helper()
	cmds.mu.Lock()
	defer cmds.mu.Unlock()
	if n >= len(cmds.cmds) {
		t.Fatalf("host %d was never spawned (%d were)", n, len(cmds.cmds))
	}
	return cmds.cmds[n].Env
}

// hasEnv reports whether env sets key to something, as the agent's lookup
// reads it: set and non-empty.
func hasEnv(env []string, key string) bool {
	return slices.ContainsFunc(env, func(kv string) bool {
		v, ok := strings.CutPrefix(kv, key+"=")
		return ok && v != ""
	})
}

// TestTheListGivesAnotherProvidersSessionItsOwnBinary is A13's second leg: a
// cursor launch with --agent-bin, or with CRAZE_AGENT_BIN, starts a cursor
// session and then a grok one from the list. The cursor host is the launch's
// own provider's — the flag on its command line, the variable in its
// environment, and the fake (not the grok binary) runs — the negative control
// of the grok host, which is passed neither, the variable left out of its
// environment, and runs the grok binary `[agents].grok` names, or `grok` on
// PATH. A saved grok row resumed from the list is the same.
func TestTheListGivesAnotherProvidersSessionItsOwnBinary(t *testing.T) {
	for _, via := range []string{"flag", "env"} {
		for _, where := range []string{"config", "path"} {
			t.Run(via+"/"+where, func(t *testing.T) {
				env, ws, cmds := launchHome(t, nil)
				t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
				marker := grokBinary(t, where, "grok-echo")
				f := launchFlags(t, ws)
				fake := f.agentBin
				if via == "env" {
					t.Setenv(envAgentBin, fake)
					f.agentBin = ""
				}
				elsewhere := absDir(t.TempDir())
				open := func(cfg tui.Config, ref roster.Ref) {
					b, err := cfg.Sessions.Open(ref)
					if err != nil {
						t.Fatalf("Open: %v", err)
					}
					defer func() { _ = b.Close() }()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					go func() {
						for {
							if _, err := b.Read(ctx); err != nil {
								return
							}
						}
					}()
					if err := b.Start(stepCtx(t)); err != nil {
						t.Fatalf("Start: %v", err)
					}
				}
				fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
					for _, p := range []agent.Provider{agent.CursorProvider(), agent.GrokProvider()} {
						ref, err := cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: elsewhere, Provider: p, PermissionMode: backend.PermissionBypass})
						if err != nil {
							t.Fatalf("Spawn %s: %v", p.Name(), err)
						}
						open(cfg, ref)
						if got, want := ran(marker), p.Name() == "grok"; got != want {
							t.Fatalf("after the %s session started the grok binary ran: %v, want %v", p.Name(), got, want)
						}
					}
					return tui.Result{}, nil
				})
				if err := runTUI(nil, f, hostEnv{}); err != nil {
					t.Fatalf("runTUI: %v", err)
				}
				cursorArgv, grokArgv := argvOf(t, cmds, 0), argvOf(t, cmds, 1)
				if !hasArg(cursorArgv, "--provider=cursor") || !hasArg(grokArgv, "--provider=grok") {
					t.Fatalf("the hosts' command lines: %q, %q", cursorArgv, grokArgv)
				}
				if got := hasArg(cursorArgv, "--agent-bin="+fake); got != (via == "flag") {
					t.Fatalf("the cursor host's command line %q passes the launch's --agent-bin: %v", cursorArgv, got)
				}
				if got := hasEnv(hostEnvOf(t, cmds, 0), envAgentBin); got != (via == "env") {
					t.Fatalf("the cursor host's environment has %s: %v", envAgentBin, got)
				}
				if hasArg(grokArgv, "--agent-bin=") || hasEnv(hostEnvOf(t, cmds, 1), envAgentBin) {
					t.Fatalf("the grok host was handed the cursor launch's binary: %q", grokArgv)
				}
				if left, _ := os.ReadDir(filepath.Join(env.Home, ".cache", "craze", "hosts")); len(left) != 0 {
					t.Fatalf("the hosts the list started and never left running are still there: %v", left)
				}
			})
		}
	}
	t.Run("a saved grok row", func(t *testing.T) {
		env, ws, cmds := launchHome(t, nil)
		t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
		// The grok binary loads: the row's session is the fake's to replay.
		marker := grokBinary(t, "config", "grok-load")
		t.Setenv(envAgentBin, fakeAgentPath(t))
		f := launchFlags(t, ws)
		elsewhere := absDir(t.TempDir())
		row := sessions.Row{SessionID: "saved-grok-1", Provider: "grok", CWD: elsewhere, Title: "a grok one",
			CrazeID: "0199aaaa-bbbb-7ccc-8ddd-0000000000f1", UpdatedAt: time.Now()}
		seedIndexRow(t, row)
		fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
			b, err := cfg.Sessions.Open(roster.SavedRef(row))
			if err != nil {
				t.Fatalf("Open of the saved row: %v", err)
			}
			started(t, b)
			if info := b.Info(); info.Provider != "grok" {
				t.Fatalf("the resumed session is %q", info.Provider)
			}
			_ = b.Close()
			return tui.Result{}, nil
		})
		if err := runTUI(nil, f, hostEnv{}); err != nil {
			t.Fatalf("runTUI: %v", err)
		}
		argv := argvOf(t, cmds, 0)
		if !hasArg(argv, "--load="+row.CrazeID) || hasArg(argv, "--agent-bin=") || hasEnv(hostEnvOf(t, cmds, 0), envAgentBin) {
			t.Fatalf("the saved grok row's host: %q", argv)
		}
		if !ran(marker) {
			t.Fatal("the saved grok row's host did not run [agents].grok")
		}
		stopEntry(t, onlyHost(t, env), cmds.pids()[0])
	})
}

// TestTheInProcessPathScopesTheBinaryAndAppliesTheStartSettings is the
// opt-out's half of A13 and P6: under `detach = false` the TUI builds its
// sessions itself, in a process that has CRAZE_AGENT_BIN. Its own provider's
// session runs the launch's binary at --effort; the provider picker's grok
// runs `[agents].grok`, or `grok` on PATH — the agent's own lookup leaves the
// variable out (agent.Options.NoBinaryEnv) — the fake never standing in for
// it.
func TestTheInProcessPathScopesTheBinaryAndAppliesTheStartSettings(t *testing.T) {
	for _, where := range []string{"config", "path"} {
		t.Run(where, func(t *testing.T) {
			_, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "effort")
			marker := grokBinary(t, where, "grok-echo")
			t.Setenv(envAgentBin, fakeAgentPath(t))
			f := &tuiFlags{workspace: ws, force: true, effort: "low"}
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				own := cfg.NewSession(agent.CursorProvider())
				defer func() { _ = own.Close() }()
				if err := own.Start(stepCtx(t)); err != nil {
					t.Fatalf("the cursor session: %v", err)
				}
				if ran(marker) {
					t.Fatal("the launch's own session ran the grok binary")
				}
				if opt := agent.EffortOption(own.Snapshot()); opt == nil || opt.Current != "low" {
					t.Fatalf("the cursor session started at effort %+v, want low", opt)
				}
				other := cfg.NewSession(agent.GrokProvider())
				defer func() { _ = other.Close() }()
				if err := other.Start(stepCtx(t)); err != nil {
					t.Fatalf("the grok session: %v", err)
				}
				if !ran(marker) {
					t.Fatal("the picker's grok session did not run its own binary")
				}
				return tui.Result{}, nil
			})
			if err := runTUI(nil, f, hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
		})
	}
}

// TestALaunchsOwnProviderFollowsItsLoad: the launcher's own provider is the
// resolved one until a load names another — --continue's row, or the resume
// picker's — and hostOptions hands the launch's binary to a host of that
// provider alone, leaving CRAZE_AGENT_BIN out of every other's environment.
func TestALaunchsOwnProviderFollowsItsLoad(t *testing.T) {
	cursor, grok, native := agent.CursorProvider(), agent.GrokProvider(), agent.NativeProvider()
	l := newLauncher(nil, &tuiFlags{agentBin: "/f"}, resolvedProvider{Provider: cursor}, nil)
	defer l.finish()
	check := func(p agent.Provider, own bool) {
		t.Helper()
		o := l.hostOptions(l.flags, p, "")
		if (o.flags.agentBin == "/f") != own || o.foreign == own {
			t.Fatalf("%s's host under a %s launch: --agent-bin %q, foreign %v", p.Name(), l.launchProvider().Name(), o.flags.agentBin, o.foreign)
		}
	}
	check(cursor, true)
	check(grok, false)
	check(native, false)
	l.setLaunchProvider(grok)
	check(cursor, false)
	check(grok, true)
}

// TestADetachedHostSetsItsStartSettingsBeforeAnyPrompt is P6 on a spawned
// host, with the schedule forced: the host is handed --effort and --fast, its
// ok comes before its agent starts, and each set is held on the fake's set
// gate while a client's prompt is sent — refused, the session still starting
// — so the agent reads the one prompt that is admitted, once the host is
// ready, after both sets.
func TestADetachedHostSetsItsStartSettingsBeforeAnyPrompt(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "effort")
	callsPath := filepath.Join(t.TempDir(), "calls")
	t.Setenv("CRAZE_FAKE_DUMP_CALLS", callsPath)
	calls := func() []string {
		b, _ := os.ReadFile(callsPath)
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	// What the agent read of each prompt: the calls record a prompt by its
	// method alone, so only this tells "first" from "early" (SF-136).
	promptsPath := filepath.Join(t.TempDir(), "prompts")
	t.Setenv("CRAZE_FAKE_DUMP_PROMPTS", promptsPath)
	gate := filepath.Join(t.TempDir(), "set-gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SET_GATE", gate)
	release := func() {
		t.Helper()
		w, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte{1})
		_ = w.Close()
	}
	cmds := spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	opts := spawnFor(t, env, ws)
	opts.flags.effort, opts.flags.fast = "low", true
	ref, err := spawnNow(t, opts)
	if err != nil {
		t.Fatalf("spawnHost: %v", err)
	}
	argv := argvOf(t, cmds, 0)
	if !slices.Contains(argv, "--effort=low") || !slices.Contains(argv, "--fast") {
		t.Fatalf("the host's command line %q", argv)
	}
	for _, set := range []string{"effort=low", "fast=true"} {
		deadline := time.Now().Add(serveStep)
		for !slices.Contains(calls(), "session/set_config_option "+set) {
			if time.Now().After(deadline) {
				t.Fatalf("the agent never read the %s set: %q", set, calls())
			}
			time.Sleep(10 * time.Millisecond)
		}
		s := dialServe(t, ref.entry)
		if res, err := s.Submit(stepCtx(t), engine.Command{Client: s.ClientID(), ID: "1"}, "early", engine.SubmitQueue, ""); !errors.Is(err, engine.ErrNotAccepting) {
			t.Fatalf("a prompt while %s is on the wire: %+v, %v; want refused", set, res, err)
		}
		_ = s.Close()
		release()
	}
	waitServingEntry(t, env, true, func() error { return nil }, &lockedBuffer{})
	promptUnattached(t, ref.entry, "first")
	if lt := waitLastTurn(t, ref.entry); lt.Outcome != protocol.TurnDone {
		t.Fatalf("the turn ended %+v", lt)
	}
	got := calls()
	prompt := slices.Index(got, "session/prompt")
	if prompt < 0 || slices.Index(got, "session/set_config_option effort=low") > prompt ||
		slices.Index(got, "session/set_config_option fast=true") > prompt {
		t.Fatalf("the agent's calls: %q", got)
	}
	if n := strings.Count(strings.Join(got, "\n"), "session/prompt"); n != 1 {
		t.Fatalf("%d prompts reached the agent, want the admitted one: %q", n, got)
	}
	// The fake records a prompt before it answers it, so the turn that has
	// ended has its prompt here.
	if b, _ := os.ReadFile(promptsPath); string(b) != `{"prompt":[{"type":"text","text":"first"}]}`+"\n" {
		t.Fatalf("the agent read %q, want the admitted prompt alone", b)
	}
	_, s := attachedTranscript(t, ref.entry)
	stopOver(t, s, "1")
	waitReaped(t, ref.child.PID())
}

// TestAnInProcessLoadIsTheLaunchsOwnProvider: under the opt-out a load's
// provider is the launch's own whatever was resolved — `craze -c --agent-bin X`
// of a cursor row with grok the configured default runs X — while a new
// session of a provider other than the resolved one (the provider picker's
// cursor, under that same grok default) does not take X: with nothing else
// naming cursor's binary its start fails, which is the negative control.
func TestAnInProcessLoadIsTheLaunchsOwnProvider(t *testing.T) {
	for _, load := range []bool{true, false} {
		t.Run(fmt.Sprintf("load %v", load), func(t *testing.T) {
			_, ws := serveHome(t)
			fake := fakeAgentPath(t)
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CRAZE_FAKE_SCRIPT", "load")
			if err := os.WriteFile(filepath.Join(os.Getenv("CRAZE_HOME"), "config.toml"), []byte("provider = \"grok\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f := &tuiFlags{agentBin: fake, workspace: ws, force: true}
			if load {
				f.cont = true
				seedIndexRow(t, sessions.Row{SessionID: "sess-load-1", Provider: "cursor", CWD: absDir(ws), Title: "a cursor one",
					CrazeID: "0199aaaa-bbbb-7ccc-8ddd-0000000000f2", UpdatedAt: time.Now()})
			}
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				s := cfg.Session
				if !load {
					s = cfg.NewSession(agent.CursorProvider())
				}
				if s == nil {
					t.Fatal("no session was built")
				}
				defer func() { _ = s.Close() }()
				err := s.Start(stepCtx(t))
				switch {
				case load && err != nil:
					t.Fatalf("the cursor row's load under a grok default: %v", err)
				case !load && (err == nil || !strings.Contains(err.Error(), "agent binary not found")):
					t.Fatalf("the picker's cursor under a grok default: %v, want no binary found", err)
				}
				return tui.Result{}, nil
			})
			if err := runTUI(nil, f, hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
		})
	}
}
