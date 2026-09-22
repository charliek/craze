package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
// treats a hidden id as it treats one this build has never heard of — kept in
// the file, never offered — until its sessions have a loader (plan 018 §3.4).
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

// TestInProcessRefusalSkipsALoad: --continue and --resume start the indexed
// row's provider, never the resolved one, so an in-process provider resolved
// from the environment does not turn --agent-bin or --ask into a usage error
// there. With nothing to continue the run ends the way it always has.
func TestInProcessRefusalSkipsALoad(t *testing.T) {
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
}
