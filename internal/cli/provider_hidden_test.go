package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// hiddenID is the id of the hidden provider these cases plant: a stand-in for
// the native provider holding the same rules, so they are tested as rules of
// the hidden list and not of one entry in it.
const hiddenID = "hush"

func plantHidden(t *testing.T) agent.Provider {
	t.Helper()
	p, restore := agent.RegisterHiddenProviderForTest(hiddenID, "Hush")
	t.Cleanup(restore)
	return p
}

// TestHiddenProviderResolvesThroughEveryEntryPoint is plan 018 §3.4's
// "resolvable by flag, env, and hand-written config": the hidden list is
// behind ProviderByName, so each precedence level reaches it without a
// warning, and PATH is empty throughout because an in-process provider never
// asks it.
func TestHiddenProviderResolvesThroughEveryEntryPoint(t *testing.T) {
	plantHidden(t)
	t.Setenv("CRAZE_AGENT_BIN", "")
	t.Setenv("PATH", t.TempDir())
	check := func(t *testing.T, got resolvedProvider, stderr string, locked bool) {
		t.Helper()
		if got.Provider.Name() != hiddenID || !got.Provider.Hidden() || got.Fallback || got.Locked != locked {
			t.Fatalf("resolved %+v", got)
		}
		if strings.Contains(stderr, "unknown provider") {
			t.Fatalf("a hidden id was warned about as unknown: %q", stderr)
		}
	}
	t.Run("flag", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "")
		crazeHome(t)
		got, stderr, err := providerFor(t, false, "--provider", hiddenID)
		if err != nil {
			t.Fatal(err)
		}
		check(t, got, stderr, true)
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", hiddenID)
		crazeHome(t)
		got, stderr, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		check(t, got, stderr, false)
	})
	t.Run("config", func(t *testing.T) {
		t.Setenv("CRAZE_PROVIDER", "")
		writeCrazeConfig(t, "provider = \""+hiddenID+"\"\n")
		got, stderr, err := providerFor(t, false)
		if err != nil {
			t.Fatal(err)
		}
		check(t, got, stderr, false)
	})
}

// TestPersistProviderSkipsAHiddenProvider is the headless half of "never
// persisted": craze prompt calls persistProvider once Start succeeds, and a
// hidden provider must leave the previous default where it was. The whole
// prompt run needs the in-process session that lands with the native
// provider, so this holds the skip itself; tests/cli drives it end to end.
func TestPersistProviderSkipsAHiddenProvider(t *testing.T) {
	plantHidden(t)
	t.Setenv("CRAZE_PROVIDER", "")
	path := writeCrazeConfig(t, "provider = \"grok\"\ntheme = \"gruvbox\"\n")
	got, _, err := providerFor(t, false, "--provider", hiddenID)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistProvider(got); err != nil {
		t.Fatalf("persistProvider: %v", err)
	}
	if p := tui.ConfigProvider(); p != "grok" {
		body, _ := os.ReadFile(path)
		t.Fatalf("a hidden provider replaced the default: provider %q\n%s", p, body)
	}
}

// TestKnownProviderIsFalseForAHiddenProvider: the index's view of the registry
// treats the planted hidden id as it treats one this build has never heard of —
// kept in the file, never offered — because it is not resumable (plan 028
// §3.5; plan 018 §3.4 keyed it on hidden).
func TestKnownProviderIsFalseForAHiddenProvider(t *testing.T) {
	plantHidden(t)
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"cursor", true},
		{"grok", true},
		{"gx", true},
		{hiddenID, false},
		{"codex", false},
	} {
		if got := knownProvider(tc.id); got != tc.want {
			t.Fatalf("knownProvider(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestPlantedHiddenIndexRowIsNeverOffered: a hidden-provider row that reached
// sessions.jsonl — craze never writes one, but the file is user-editable — is
// not Latest, is not in --resume's list, is not what --continue loads even
// when it is the newest row, and stays in the file.
func TestPlantedHiddenIndexRowIsNeverOffered(t *testing.T) {
	plantHidden(t)
	indexHome(t)
	ws := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "cursor-1", Provider: "cursor", CWD: ws, Title: "cursor row", TitleKind: sessions.TitleKindAgent}, time.Hour)
	seedRow(t, sessions.Row{SessionID: "hush-1", Provider: hiddenID, CWD: ws, Title: "hidden row", TitleKind: sessions.TitleKindAgent}, time.Minute)

	store := &sessions.Store{KnownProvider: knownProvider}
	row, ok, err := store.Latest(ws, "")
	if err != nil || !ok || row.SessionID != "cursor-1" {
		t.Fatalf("Latest = %+v, %v, %v; want the cursor row", row, ok, err)
	}

	_, built, err := runResolveLoad(t, ws, "--continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if len(built) != 1 || built[0].LoadSessionID != "cursor-1" {
		t.Fatalf("--continue built %+v, want the cursor row", built)
	}

	cfg, _, err := runResolveLoad(t, ws, "--resume")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(cfg.Resume) != 1 || cfg.Resume[0].SessionID != "cursor-1" {
		t.Fatalf("--resume offered %+v, want the cursor row alone", cfg.Resume)
	}

	// Asking for the hidden provider explicitly finds nothing to continue:
	// the row is not a candidate, whatever filter is applied.
	_, _, err = runResolveLoad(t, ws, "--continue", "--provider", hiddenID)
	code, msg := exitCode(t, err)
	if code != 1 || msg != "craze: no session to continue in "+ws+" for provider "+hiddenID {
		t.Fatalf("exit %d %q", code, msg)
	}

	body, err := os.ReadFile(indexPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"sessionId":"hush-1"`) {
		t.Fatalf("the hidden row was dropped from the file:\n%s", body)
	}
}

// TestProviderIDsNeverNameAHiddenProvider: the --provider help and the
// unknown-provider error are computed from ProviderNames, so recomputing them
// with a hidden provider registered gives the same text, and the error an
// unknown id gets is still exactly the one
// TestUnknownProviderErrorNamesEveryProvider pins.
func TestProviderIDsNeverNameAHiddenProvider(t *testing.T) {
	plantHidden(t)
	if got := joinOr(agent.ProviderNames()); got != providerIDs || strings.Contains(got, hiddenID) {
		t.Fatalf("provider ids %q with a hidden provider registered, want %q", got, providerIDs)
	}
	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"prompt", "--provider", "codex", "hi"})
	var ee *exitError
	if err := cmd.Execute(); !errors.As(err, &ee) {
		t.Fatalf("%v", err)
	}
	if want := `craze: unknown provider "codex" (want cursor, grok, or gx)`; ee.msg != want {
		t.Fatalf("msg %q, want %q", ee.msg, want)
	}
}

// TestInProcessProviderRefusesSpawnFlags is plan 018 §3.4's usage errors: an
// in-process provider has no binary to spawn, so --agent-bin and
// CRAZE_AGENT_BIN are refused with exit 2 in all three commands, whether the
// provider came from the flag or the environment.
//
// --ask/--plan is the other half, and from plan 023 §3.6 it is keyed on the
// MODES capability rather than on "in-process": native has the harness's three
// modes now and takes both flags (TestRefuseInProcessLeavesModedProvidersAlone),
// so the mode rows here are the planted hidden provider, which is in-process
// with no capabilities at all — exactly the shape the refusal is still for.
func TestInProcessProviderRefusesSpawnFlags(t *testing.T) {
	const (
		binMsg = ": --agent-bin cannot be used with provider native, which runs inside craze"
		envMsg = ": CRAZE_AGENT_BIN cannot be used with provider native, which runs inside craze; unset it"
	)
	askMsg, planMsg := modeRefusal("ask"), modeRefusal("plan")
	for _, tc := range []struct {
		name string
		tui  []string // the root command's flags; nil runs NewRootCmd with argv instead
		argv []string
		env  map[string]string
		want string
	}{
		{name: "craze --agent-bin", tui: []string{"--provider", "native", "--agent-bin", "/bin/true"}, want: "craze" + binMsg},
		{name: "craze CRAZE_AGENT_BIN", tui: []string{"--provider", "native"},
			env: map[string]string{"CRAZE_AGENT_BIN": "/bin/true"}, want: "craze" + envMsg},
		{name: "craze --ask", tui: []string{"--provider", hiddenID, "--ask"}, want: askMsg},
		{name: "craze --plan from the environment's provider", tui: []string{"--plan"},
			env: map[string]string{"CRAZE_PROVIDER": hiddenID}, want: planMsg},
		{name: "prompt --agent-bin", argv: []string{"prompt", "--provider", "native", "--agent-bin", "/bin/true", "hi"}, want: "craze" + binMsg},
		{name: "prompt CRAZE_AGENT_BIN", argv: []string{"prompt", "--provider", "native", "hi"},
			env: map[string]string{"CRAZE_AGENT_BIN": "/bin/true"}, want: "craze" + envMsg},
		{name: "prompt --ask", argv: []string{"prompt", "--provider", hiddenID, "--ask", "hi"}, want: askMsg},
		{name: "prompt --plan from the environment's provider", argv: []string{"prompt", "--plan", "hi"},
			env: map[string]string{"CRAZE_PROVIDER": hiddenID}, want: planMsg},
		{name: "frame --agent-bin", argv: []string{"frame", "--provider", "native", "--agent-bin", "/bin/true"}, want: "craze frame" + binMsg},
		{name: "frame CRAZE_AGENT_BIN", argv: []string{"frame", "--provider", "native"},
			env: map[string]string{"CRAZE_AGENT_BIN": "/bin/true"}, want: "craze frame" + envMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plantHidden(t)
			crazeHome(t)
			t.Setenv("CRAZE_PROVIDER", "")
			t.Setenv("CRAZE_AGENT_BIN", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var err error
			if tc.tui != nil {
				cmd, f := parseTUIFlags(t, tc.tui...)
				err = runTUI(cmd, f, hostEnv{})
			} else {
				cmd := NewRootCmd()
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				cmd.SetArgs(tc.argv)
				err = cmd.Execute()
			}
			code, msg := exitCode(t, err)
			if code != 2 || msg != tc.want {
				t.Fatalf("exit %d %q, want 2 %q", code, msg, tc.want)
			}
		})
	}
}

// TestRefuseInProcessLeavesSpawnedProvidersAlone: the binary half of the
// refusal is for in-process providers only; every ACP provider takes
// --agent-bin and the modes exactly as before, and native with neither is let
// through.
func TestRefuseInProcessLeavesSpawnedProvidersAlone(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "/bin/true")
	for _, p := range agent.Providers() {
		if err := refuseInProcess("craze", p, "/bin/true", "plan"); err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
	}
	t.Setenv("CRAZE_AGENT_BIN", "")
	if err := refuseInProcess("craze", agent.NativeProvider(), "", ""); err != nil {
		t.Fatalf("native with no spawn flags: %v", err)
	}
}

// TestRefuseInProcessLeavesModedProvidersAlone is plan 023 §3.6's key: the
// mode half asks the provider whether it HAS modes, not whether craze spawns
// it. Native runs in process and has the harness's three, so --ask and --plan
// go through on all three callers; an in-process provider with none is still
// refused, which is what keeps the refusal a live rule rather than dead code.
//
// The one function is every caller that can carry a mode (prompt and a fresh
// TUI session; frame has no mode flag and passes ""), so this is the whole
// surface.
func TestRefuseInProcessLeavesModedProvidersAlone(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "")
	p := plantHidden(t)
	for _, mode := range []string{"ask", "plan"} {
		if err := refuseInProcess("craze", agent.NativeProvider(), "", mode); err != nil {
			t.Fatalf("native --%s: %v", mode, err)
		}
		err := refuseInProcess("craze", p, "", mode)
		if err == nil || err.Error() != modeRefusal(mode) {
			t.Fatalf("%s --%s = %v, want %q", hiddenID, mode, err, modeRefusal(mode))
		}
	}
}

// modeRefusal is the usage error refuseInProcess gives the planted hidden
// provider for --ask or --plan: one spelling of the message, for the table
// above and the case beside it.
func modeRefusal(mode string) string {
	return "craze: --" + mode + " cannot be used with provider " + hiddenID + ", which has no modes"
}

// TestALoadIsRefusedByItsRowsProvider (A10; plan 028 §3.5, seam 6): a load is
// held to the spawn flags by the provider of the row it loads, never by the
// one resolved. --agent-bin or CRAZE_AGENT_BIN with a native row is exit 2 with
// the in-process usage error, before the index is written, a craze id minted
// or a session claimed — in craze and in craze frame — while --plan goes
// through, since native has modes. A native provider resolved from the
// environment refuses nothing for a cursor row, nor when there is nothing to
// continue. The resume picker asks the same closure (Config.RefuseLoad) of the
// row chosen: a refusal is its error row, before any claim command exists, and
// nothing is built.
func TestALoadIsRefusedByItsRowsProvider(t *testing.T) {
	const (
		binMsg = "craze: --agent-bin cannot be used with provider native, which runs inside craze"
		envMsg = "craze: CRAZE_AGENT_BIN cannot be used with provider native, which runs inside craze; unset it"
	)
	nativeRow := func(ws string) sessions.Row {
		return sessions.Row{SessionID: "native-1", Provider: "native", CWD: ws, Title: "a native thread", TitleKind: sessions.TitleKindAgent}
	}
	cursorRow := func(ws string) sessions.Row {
		return sessions.Row{SessionID: "cursor-1", Provider: "cursor", CWD: ws, Title: "a cursor thread", TitleKind: sessions.TitleKindAgent}
	}
	// noClaim fails if anything was claimed or minted: the lock tree holds no
	// lock, and the index is byte for byte what was seeded.
	noClaim := func(t *testing.T, home string, before []byte) {
		t.Helper()
		if after, err := os.ReadFile(indexPath(t)); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("a refused load wrote the index (%v):\nbefore %s\nafter  %s", err, before, after)
		}
		locks, err := os.ReadDir(filepath.Join(home, ".cache", "craze", "locks"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if len(locks) != 0 {
			t.Fatalf("a refused load claimed %v", locks)
		}
	}

	for _, tc := range []struct {
		name string
		argv []string
		env  string
		want string
	}{
		{"--agent-bin", []string{"--agent-bin", "/bin/true", "--plan"}, "", binMsg},
		{"CRAZE_AGENT_BIN", nil, "/bin/true", envMsg},
	} {
		t.Run("continue with "+tc.name, func(t *testing.T) {
			home := indexHome(t)
			t.Setenv("CRAZE_AGENT_BIN", tc.env)
			ws := t.TempDir()
			seedRow(t, nativeRow(ws), time.Minute)
			before, err := os.ReadFile(indexPath(t))
			if err != nil {
				t.Fatal(err)
			}
			cmd, f := parseTUIFlags(t, append([]string{"--continue", "--workspace", ws}, tc.argv...)...)
			code, msg := exitCode(t, runTUI(cmd, f, hostEnv{}))
			if code != 2 || msg != tc.want {
				t.Fatalf("exit %d %q, want 2 %q", code, msg, tc.want)
			}
			noClaim(t, home, before)
		})
	}

	t.Run("frame continue with --agent-bin", func(t *testing.T) {
		frameHome(t)
		var stdout, stderr bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"frame", "--cols", "100", "--rows", "30", "--agent-bin", "/bin/true",
			"--seed-session", "native:native-1:a native thread", "--continue", "--keys", ""})
		code, msg := exitCode(t, cmd.Execute())
		if want := "craze frame" + strings.TrimPrefix(binMsg, "craze"); code != 2 || msg != want {
			t.Fatalf("exit %d %q, want 2 %q", code, msg, want)
		}
	})

	t.Run("frame picker with --agent-bin", func(t *testing.T) {
		frameHome(t)
		got := runFrame(t, "frame", "--cols", "100", "--rows", "30", "--agent-bin", "/bin/true",
			"--seed-session", "native:native-1:a native thread", "--resume",
			"--keys", "<enter><wait:text:craze frame: --agent-bin cannot be used>")
		if !strings.Contains(got, "enter loads") || !strings.Contains(got, "a native thread") {
			t.Fatalf("the picker is not up with the refused row:\n%s", got)
		}
	})

	t.Run("continue with --plan", func(t *testing.T) {
		indexHome(t)
		t.Setenv("CRAZE_AGENT_BIN", "")
		ws := t.TempDir()
		seedRow(t, nativeRow(ws), time.Minute)
		cfg, built, err := runResolveLoad(t, ws, "--continue", "--plan")
		if err != nil {
			t.Fatalf("--continue --plan of a native row: %v", err)
		}
		if len(built) != 1 || built[0].LoadSessionID != "native-1" || cfg.Provider.Name() != "native" || cfg.CrazeSessionID == "" {
			t.Fatalf("built %+v on %s (craze id %q); want the native row, claimed", built, cfg.Provider.Name(), cfg.CrazeSessionID)
		}
	})

	t.Run("the resolved provider has no say", func(t *testing.T) {
		ws := indexHome(t)
		t.Setenv("CRAZE_PROVIDER", "native")
		t.Setenv("CRAZE_AGENT_BIN", "")
		for _, flag := range []string{"--continue", "--resume"} {
			cmd, f := parseTUIFlags(t, flag, "--agent-bin", "/bin/true", "--ask", "--workspace", ws)
			code, msg := exitCode(t, runTUI(cmd, f, hostEnv{}))
			if code != 1 || !strings.HasPrefix(msg, "craze: no session to continue in ") {
				t.Fatalf("%s: exit %d %q, want the no-session exit 1", flag, code, msg)
			}
		}
		seedRow(t, cursorRow(ws), time.Minute)
		_, built, err := runResolveLoad(t, ws, "--continue", "--agent-bin", "/bin/true", "--ask")
		if err != nil || len(built) != 1 || built[0].LoadSessionID != "cursor-1" {
			t.Fatalf("--continue of a cursor row under a native default: %+v, %v", built, err)
		}
	})

	t.Run("the picker", func(t *testing.T) {
		indexHome(t)
		t.Setenv("CRAZE_AGENT_BIN", "")
		ws := t.TempDir()
		seedRow(t, cursorRow(ws), time.Hour)
		seedRow(t, nativeRow(ws), time.Minute)
		cfg, _, err := runResolveLoad(t, ws, "--resume", "--agent-bin", "/bin/true", "--plan")
		if err != nil || cfg.RefuseLoad == nil || len(cfg.Resume) != 2 || cfg.Resume[0].SessionID != "native-1" {
			t.Fatalf("--resume: %v, refusal set %v, rows %+v; want the native row offered first", err, cfg.RefuseLoad != nil, cfg.Resume)
		}
		for _, claiming := range []bool{false, true} {
			var claims atomic.Int32
			var loaded []string
			pcfg := tui.Config{
				Theme:      "tokyo-night",
				Workspace:  t.TempDir(),
				Provider:   agent.CursorProvider(),
				Resume:     cfg.Resume,
				RefuseLoad: cfg.RefuseLoad,
				LoadSession: func(p agent.Provider, row sessions.Row) agent.Session {
					loaded = append(loaded, row.SessionID)
					s := tui.NewStub()
					s.SetProvider(p)
					t.Cleanup(func() { _ = s.Close() })
					return s
				},
			}
			if claiming {
				pcfg.ClaimSession = func(sessions.Row) (string, func(), error) {
					claims.Add(1)
					return "018f-claimed", func() {}, nil
				}
			}
			var m tea.Model = tui.New(pcfg)
			m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			m, cmd := m.Update(enterKey)
			if cmd != nil {
				t.Fatalf("claiming %v: Enter on the refused native row returned a command", claiming)
			}
			if view := ansi.Strip(m.View()); !strings.Contains(view, "craze: --agent-bin cannot be used with provider") {
				t.Fatalf("claiming %v: the picker does not show the refusal:\n%s", claiming, view)
			}
			if claims.Load() != 0 || len(loaded) != 0 {
				t.Fatalf("claiming %v: the refused row was claimed %d times, built %v", claiming, claims.Load(), loaded)
			}
			// The cursor row goes through under the same flags.
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
			_, cmd = m.Update(enterKey)
			if claiming {
				if cmd == nil {
					t.Fatal("Enter on the cursor row returned no claim command")
				}
				runCmd(t, cmd, 5*time.Second)
				if claims.Load() != 1 {
					t.Fatalf("the cursor row was claimed %d times, want once", claims.Load())
				}
				continue
			}
			if len(loaded) != 1 || loaded[0] != "cursor-1" {
				t.Fatalf("Enter on the cursor row built %v, want it", loaded)
			}
		}
	})
}
