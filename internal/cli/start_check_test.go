package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
)

// A start's own feedback (LM-2(a), plan 037 §3.5): what a start this process
// makes with no picker says before it starts — the not-GUI verdict alone,
// reason and fix — in the TUI (Config.StartCheck, a note) and in craze prompt
// (one stderr line); nothing is refused.

// TestStartWarningIsTheNotGUIVerdictAlone: over injected inputs, only cursor
// outside the login session — its binary found — has something to say, the
// check's own reason and fix; cursor in the GUI's session or one not known,
// cursor missing (a missing binary fails on its own, LC-3's words), grok
// outside the session, and native unreadable or needing setup say nothing.
func TestStartWarningIsTheNotGUIVerdictAlone(t *testing.T) {
	cursorMissing := map[string]binResolution{"cursor": {Source: binFromPath}}
	for _, tc := range []struct {
		name        string
		p           agent.Provider
		resolve     map[string]binResolution
		gui         func() (uint32, bool, bool)
		native      func() (availState, string, string)
		reason, fix string
	}{
		{name: "cursor outside the login session", p: agent.CursorProvider(), gui: session(false, true), reason: notGUIReason, fix: notGUIFix},
		{name: "cursor in the login session", p: agent.CursorProvider(), gui: session(true, true)},
		{name: "cursor, the session not known", p: agent.CursorProvider(), gui: session(false, false)},
		{name: "cursor missing, outside the session", p: agent.CursorProvider(), resolve: cursorMissing, gui: session(false, true)},
		{name: "grok outside the session", p: agent.GrokProvider(), gui: session(false, true)},
		{name: "native unreadable", p: agent.NativeProvider(), gui: session(false, true),
			native: nativeAs(availUnavailable, "models.toml could not be read", nativeUnreadFix)},
		{name: "native needing setup", p: agent.NativeProvider(), gui: session(false, true),
			native: nativeAs(availNeedsSetup, nativeNoKeyReason, nativeNoKeyFix)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := availInputs{
				resolve:    resolvesAs(tc.resolve),
				native:     nativeAs(availReady, "", ""),
				gui:        tc.gui,
				configPath: availConfig,
			}
			if tc.native != nil {
				in.native = tc.native
			}
			reason, fix := startWarning(in, tc.p)
			if reason != tc.reason || fix != tc.fix {
				t.Fatalf("startWarning = %q, %q; want %q, %q", reason, fix, tc.reason, tc.fix)
			}
		})
	}
}

// TestStartCheckForReadsThisProcess: the TUI's check is this process's —
// binaries found as its sessions find them, its login session — so cursor on
// PATH outside the login session has the not-GUI reason and fix, in the
// GUI's session nothing, and with an --agent-bin that names no binary nothing
// either (the start fails on its own).
func TestStartCheckForReadsThisProcess(t *testing.T) {
	availPath(t, "cursor-agent", "grok")
	crazeHome(t)
	t.Setenv(envAgentBin, "")
	cursor := agent.CursorProvider()

	t.Setenv(rundir.GUISessionEnv, "0")
	if reason, fix := startCheckFor("")(cursor); reason != notGUIReason || fix != notGUIFix {
		t.Fatalf("cursor outside the login session: %q, %q", reason, fix)
	}
	if reason, _ := startCheckFor("")(agent.GrokProvider()); reason != "" {
		t.Fatalf("grok outside the login session: %q", reason)
	}
	missing := filepath.Join(t.TempDir(), "no-cursor-agent")
	if reason, _ := startCheckFor(missing)(cursor); reason != "" {
		t.Fatalf("cursor's --agent-bin missing: %q", reason)
	}
	t.Setenv(rundir.GUISessionEnv, "1")
	if reason, _ := startCheckFor("")(cursor); reason != "" {
		t.Fatalf("cursor in the login session: %q", reason)
	}
}

// promptRun is one craze prompt over the fake agent: its stdout, its stderr
// and its error.
func promptRun(t *testing.T, agentBin string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"prompt", "--agent-bin", agentBin, "--workspace", t.TempDir()}, args...))
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

// TestPromptSaysWhyItMayNotStart: craze prompt of cursor outside the login
// session writes one stderr line before it starts — the check's reason and
// fix — and runs the turn as ever: the same stdout and no error as in the
// login session, where it writes no line. A missing binary writes none: its
// own failure speaks.
func TestPromptSaysWhyItMayNotStart(t *testing.T) {
	isolateRunEnv(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv(envAgentBin, "")
	fake := fakeAgentPath(t)
	line := "craze prompt: cursor may not start here: " + notGUIReason + "; " + notGUIFix + "\n"

	t.Setenv(rundir.GUISessionEnv, "0")
	outside, errOutside, err := promptRun(t, fake, "--provider", "cursor", "go")
	if err != nil {
		t.Fatalf("outside the login session: %v\nstderr: %s", err, errOutside)
	}
	if !strings.HasPrefix(errOutside, line) || strings.Count(errOutside, "may not start here") != 1 {
		t.Fatalf("stderr outside the login session %q, want it to begin with %q, once", errOutside, line)
	}

	t.Setenv(rundir.GUISessionEnv, "1")
	inside, errInside, err := promptRun(t, fake, "--provider", "cursor", "go")
	if err != nil {
		t.Fatalf("in the login session: %v\nstderr: %s", err, errInside)
	}
	if strings.Contains(errInside, "may not start here") {
		t.Fatalf("stderr in the login session %q", errInside)
	}
	if outside != inside || outside == "" {
		t.Fatalf("stdout outside the login session %q, inside %q: want the same turn", outside, inside)
	}

	t.Setenv(rundir.GUISessionEnv, "0")
	_, errMissing, err := promptRun(t, filepath.Join(t.TempDir(), "no-cursor-agent"), "--provider", "cursor", "go")
	if err == nil || strings.Contains(errMissing, "may not start here") {
		t.Fatalf("a missing binary: err %v, stderr %q", err, errMissing)
	}
}

// TestTheTUIsStartCheckIsWired: both of the TUI's paths — in process and the
// launch flow — hand it the check, answering the not-GUI verdict for an
// explicit cursor outside the login session and nothing for grok.
func TestTheTUIsStartCheckIsWired(t *testing.T) {
	for _, launch := range []bool{false, true} {
		name := "in process"
		if launch {
			name = "launch"
		}
		t.Run(name, func(t *testing.T) {
			var ws string
			if launch {
				_, ws, _ = launchHome(t, nil)
			} else {
				_, ws = serveHome(t)
			}
			t.Setenv(rundir.GUISessionEnv, "0")
			fake := fakeAgentPath(t)
			ran := false
			fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
				ran = true
				if s := cfg.Session; s != nil {
					defer func() { _ = s.Close() }()
				}
				if launch != (cfg.NewBackend != nil) {
					t.Fatalf("the run took the wrong path: launch %v, NewBackend %v", launch, cfg.NewBackend != nil)
				}
				if cfg.StartCheck == nil {
					t.Fatal("no start check")
				}
				if reason, fix := cfg.StartCheck(agent.CursorProvider()); reason != notGUIReason || fix != notGUIFix {
					t.Fatalf("the check of cursor: %q, %q", reason, fix)
				}
				if reason, _ := cfg.StartCheck(agent.GrokProvider()); reason != "" {
					t.Fatalf("the check of grok: %q", reason)
				}
				return tui.Result{}, nil
			})
			cmd, f := parseTUIFlags(t, "--provider", "cursor", "--agent-bin", fake, "--workspace", ws)
			if err := runTUI(cmd, f, hostEnv{}); err != nil {
				t.Fatalf("runTUI: %v", err)
			}
			if !ran {
				t.Fatal("runTUI never reached the TUI")
			}
		})
	}
}
