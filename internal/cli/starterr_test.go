package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// A start failure's words (plan 035 C7, SF-125/SF-126): craze serve's start
// error carries its agent's last stderr lines, and on macOS a hint the host
// builds when its agent exited outside the login session; craze prompt's does
// not move.

// TestPromptStartFailureTextDoesNotMove is the V2 guard (plan 035 A6, P4):
// `craze prompt --json`'s startup failure is byte for byte what it was before
// craze serve's start errors took the agent's stderr — its one error event on
// stdout, and its stderr: the agent's own lines as the agent wrote them, then
// craze's — whether the agent exited (with stderr and without) or refused the
// start alive (with stderr and without). craze prompt never sets
// agent.Options.StartErrAgentStderr.
func TestPromptStartFailureTextDoesNotMove(t *testing.T) {
	fake := fakeAgentPath(t)
	silent := filepath.Join(t.TempDir(), "silent-exit")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nread line\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, bin, script, stderrLine string
		stdout, stderr                string
	}{
		{
			name: "a non-zero exit with stderr", bin: fake, script: "exit-two-lines",
			stdout: `{"type":"error","message":"acp: agent exited: exit status 1"}` + "\n",
			stderr: "Error: KEYCHAIN LOCKED\nRun unlock and retry.\nacp: agent exited: exit status 1\n",
		},
		{
			name: "a non-zero exit without stderr", bin: silent, script: "echo",
			stdout: `{"type":"error","message":"acp: agent exited: exit status 3"}` + "\n",
			stderr: "acp: agent exited: exit status 3\n",
		},
		{
			name: "a refused start with stderr", bin: fake, script: "authfail", stderrLine: "fake: SAID AT START",
			stdout: `{"type":"error","message":"json-rpc error -32000: authentication failed (run ` + "`agent login`" + `)"}` + "\n",
			stderr: "fake: SAID AT START\njson-rpc error -32000: authentication failed (run `agent login`)\n",
		},
		{
			name: "a refused start without stderr", bin: fake, script: "authfail",
			stdout: `{"type":"error","message":"json-rpc error -32000: authentication failed (run ` + "`agent login`" + `)"}` + "\n",
			stderr: "json-rpc error -32000: authentication failed (run `agent login`)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			t.Setenv("CRAZE_JOURNAL", "0")
			t.Setenv("CRAZE_FAKE_SCRIPT", tc.script)
			t.Setenv("CRAZE_FAKE_STDERR", tc.stderrLine)
			stdout, stderr, code := executeErr([]string{"prompt", "--json", "--agent-bin", tc.bin, "--workspace", t.TempDir(), "go"})
			if code != 1 || stdout != tc.stdout || stderr != tc.stderr {
				t.Fatalf("craze prompt --json exited %d\nstdout %q\nstderr %q\nwant 1\nstdout %q\nstderr %q", code, stdout, stderr, tc.stdout, tc.stderr)
			}
		})
	}
}

// TestOnlyServeOptsIntoTheAgentsWords (plan 035 P4): the session options
// the in-process TUI builds (sessionOptions, which craze serve builds on)
// leave the agent's words and the hint off — craze serve sets both itself
// (TestServeTakesEverySessionFlag) — so the in-process TUI's start failures
// read as before.
func TestOnlyServeOptsIntoTheAgentsWords(t *testing.T) {
	cursor := agent.CursorProvider()
	t.Setenv(rundir.GUISessionEnv, "0")
	if o := sessionOptions(&tuiFlags{force: true}, t.TempDir(), "", nil, nil, nil, cursor, cursor, sessions.Row{}); o.StartErrAgentStderr || o.StartErrExitHint != "" {
		t.Fatalf("the TUI's session options opt in: %v, %q", o.StartErrAgentStderr, o.StartErrExitHint)
	}
}

// TestServeTakesTheHubsPID (plan 035 P3): craze serve's hidden --hub-pid —
// the hub that created the host, for the login-session hint — is serve's
// alone, not the root's, and 0 when not given.
func TestServeTakesTheHubsPID(t *testing.T) {
	if _, f := parseServeFlags(t, "--hub-pid=4242"); f.hubPID != 4242 {
		t.Fatalf("--hub-pid=4242 parsed as %d", f.hubPID)
	}
	if _, f := parseServeFlags(t); f.hubPID != 0 {
		t.Fatalf("no --hub-pid parsed as %d", f.hubPID)
	}
	serve, root := newServeCmd(), NewRootCmd()
	if fl := serve.Flags().Lookup("hub-pid"); fl == nil || !fl.Hidden || root.Flags().Lookup("hub-pid") != nil {
		t.Fatalf("--hub-pid: hidden on craze serve alone, got %+v", fl)
	}
}

// The cause A6 pins: craze's own error, then the fake agent's two lines.
const a6Cause = "acp: agent exited: exit status 1: Error: KEYCHAIN LOCKED / Run unlock and retry."

// a6Words runs the fake agent's exit-two-lines and makes a test that pins
// a6Cause wait for the fake's whole stderr, whatever the machine's load,
// rather than the production bounds (acp.StderrWaitEnv): in this process and
// in every hub and host child it starts, which inherit its environment. The
// waits end with the copy, and nothing holds the pipe after the fake's exit.
func a6Words(t *testing.T) {
	t.Setenv("CRAZE_FAKE_SCRIPT", "exit-two-lines")
	t.Setenv(acp.StderrWaitEnv, "10s")
}

// The macOS login-session hint (plan 035 P3), spelled out so the test pins
// the text: the shared head, then the hub's form or the terminal's.
const (
	loginHintHead = "; this session runs outside your macOS login session (its launcher was started over ssh): " +
		"if the agent needs the login keychain, as cursor does,"
	terminalHint = loginHintHead + " start this session from a terminal in your Mac's login session (a Roost tab, Terminal.app)"
)

func hubHint(pid int) string {
	return loginHintHead + " stop the hub with kill " + strconv.Itoa(pid) +
		" (no session ends) and run this again from a terminal in your Mac's login session (a Roost tab, Terminal.app)"
}

// TestLoginSessionHint (plan 035 P3): a host outside the GUI login session
// says so — naming the hub that created it, or, launched from a terminal
// with no hub, telling the user to start it from the login session's — and
// a host in it, or on an OS where the session is not known, says nothing.
func TestLoginSessionHint(t *testing.T) {
	for _, tc := range []struct {
		forced string
		hubPID int
		want   string
	}{
		{"0", 4242, hubHint(4242)},
		{"0", 0, terminalHint},
		{"1", 4242, ""},
		{"1", 0, ""},
	} {
		t.Setenv(rundir.GUISessionEnv, tc.forced)
		if got := loginSessionHint(tc.hubPID); got != tc.want {
			t.Fatalf("forced %q, hub %d: %q, want %q", tc.forced, tc.hubPID, got, tc.want)
		}
	}
	if runtime.GOOS != "darwin" {
		t.Setenv(rundir.GUISessionEnv, "")
		if got := loginSessionHint(4242); got != "" {
			t.Fatalf("on %s, unforced: %q, want nothing", runtime.GOOS, got)
		}
	}
	if strings.ContainsAny(hubHint(4242)+terminalHint, "\n\r") {
		t.Fatal("a hint is not one line")
	}
}

// waitStartFailed polls the host's row — sessions.list attaches nothing —
// until its start has failed, within serveStep.
func waitStartFailed(t *testing.T, r *serveRun, e rundir.Entry) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for got := listRow(t, e); got.Activity != protocol.ActivityError; got = listRow(t, e) {
		if time.Now().After(deadline) {
			t.Fatalf("the start has not failed after %v: %+v; stderr: %s", serveStep, got, r.stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeStartFailureSaysTheAgentsWords (plan 035 A6, A7): craze serve's
// session whose agent dies at its start (exit-two-lines) fails it with the
// agent's two lines after craze's own error — what an attach after the
// failure is refused with (data.cause) and what the host logs — and, its
// login session forced outside the GUI's (rundir.GUISessionEnv), the hint:
// with the hub's pid for a host the hub created (--hub-pid), the terminal's
// form for one a TUI launched. A host in the GUI session, one on Linux whose
// session is not known, and a start that failed with no agent exit (no
// agent binary) carry no hint.
func TestServeStartFailureSaysTheAgentsWords(t *testing.T) {
	fake := fakeAgentPath(t)
	for _, tc := range []struct {
		name, forced, bin string
		argv              []string
		want, wantPrefix  string
	}{
		{name: "the hub's host, outside the login session", forced: "0", bin: fake, argv: []string{"--hub-pid=4242"},
			want: a6Cause + hubHint(4242)},
		{name: "a TUI's host, outside the login session", forced: "0", bin: fake, want: a6Cause + terminalHint},
		{name: "in the login session", forced: "1", bin: fake, argv: []string{"--hub-pid=4242"}, want: a6Cause},
		{name: "unforced, its session not known", bin: fake, argv: []string{"--hub-pid=4242"}, want: a6Cause},
		{name: "no agent exit", forced: "0", bin: filepath.Join(t.TempDir(), "no-such-agent"), argv: []string{"--hub-pid=4242"},
			wantPrefix: "agent binary not found: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.forced == "" && runtime.GOOS == "darwin" {
				t.Skip("unforced, macOS's session is known")
			}
			env, ws := serveHome(t)
			a6Words(t)
			t.Setenv(rundir.GUISessionEnv, tc.forced)
			r := runServeIn(t, hostEnv{}, append([]string{"--agent-bin", tc.bin, "--workspace", ws}, tc.argv...)...)
			e := r.waitServing(t, env, false)
			waitStartFailed(t, r, e)
			var re *remote.Error
			if err := dialServe(t, e).Attach(stepCtx(t)); !errors.As(err, &re) || re.Reason != protocol.ReasonStartFailed {
				t.Fatalf("an attach after the failure: %v, want refused start_failed", err)
			}
			if tc.want != "" && re.Cause != tc.want {
				t.Fatalf("data.cause %q\nwant       %q", re.Cause, tc.want)
			}
			if tc.wantPrefix != "" && (!strings.HasPrefix(re.Cause, tc.wantPrefix) || strings.Contains(re.Cause, "login session")) {
				t.Fatalf("data.cause %q, want %q… and no hint", re.Cause, tc.wantPrefix)
			}
			waitServeLog(t, r, "craze serve: the session did not start: "+re.Cause+"\n")
		})
	}
}

// waitServeLog waits, within serveStep, for craze serve's log to hold want.
// Engine.Started publishes a failed start before serveHost.start writes it
// on the log, so a row, a state or an attach refusal that says it can come
// first (astra r11 6; plan 035 C5 found the same in
// TestServeFailsAStartWhoseAgentCannotBeRecorded).
func waitServeLog(t *testing.T, r *serveRun, want string) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for !strings.Contains(r.stderr.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the host's log does not say %q after %v: %s", want, serveStep, r.stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeStartFailureWordsReachAnAttachedClient (plan 035 A7): the same
// words reach a client attached while the start ran, in the ready
// notification's failed form — the host builds them into its own error, so
// they ride whichever way the failure is told. The start is held after the
// agent's spawn (serveAgentGroup) while the client attaches "now".
func TestServeStartFailureWordsReachAnAttachedClient(t *testing.T) {
	env, ws := serveHome(t)
	a6Words(t)
	t.Setenv(rundir.GUISessionEnv, "0")
	held, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	prevGroup := serveAgentGroup
	serveAgentGroup = func(int, error) {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { serveAgentGroup = prevGroup })
	var released atomic.Bool
	let := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--hub-pid=4242")
	t.Cleanup(let)
	if line := readyFrom(t, rd); !line.OK {
		t.Fatalf("the ready line: %+v", line)
	}
	select {
	case <-held:
	case <-time.After(serveStep):
		t.Fatalf("the agent was never spawned; stderr: %s", r.stderr)
	}
	e := waitServingEntry(t, env, false, func() error { return nil }, r.stderr)
	now, err := remote.DialSession(stepCtx(t), e.Socket, remote.SessionOptions{
		Client:    remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())},
		SessionID: e.CrazeSessionID, When: protocol.WhenNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = now.Close() })
	if err := now.Attach(stepCtx(t)); err != nil {
		t.Fatalf("an attach now while the start runs: %v", err)
	}
	let()
	var se *remote.StartError
	if err := now.Start(stepCtx(t)); !errors.As(err, &se) || se.Text != a6Cause+hubHint(4242) {
		t.Fatalf("the attached client's start: %v\nwant %q", err, a6Cause+hubHint(4242))
	}
}

// TestNewSaysTheAgentsWordsThroughAHub (plan 035 A6, A7): craze new through a
// real hub child whose hosts are real craze serve children, the fake agent
// dying at its start. The hub's refusal is start_failed, data.cause the
// host's words — A6's exactly, in the GUI login session — and the message
// "the session did not start: " and that cause, which craze new prints on
// one line. Outside the login session the cause ends with the hint, naming
// the hub child's pid.
func TestNewSaysTheAgentsWordsThroughAHub(t *testing.T) {
	for _, tc := range []struct {
		forced string
		hint   bool
	}{
		{"1", false},
		{"0", true},
	} {
		t.Run("forced "+tc.forced, func(t *testing.T) {
			env, _ := serveHome(t)
			config := fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t))
			if err := os.WriteFile(filepath.Join(env.CrazeDir, "config.toml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			a6Words(t)
			kids := hubAsChild(t, cliChildHubHosts+"=1", rundir.GUISessionEnv+"="+tc.forced)
			ws := t.TempDir()
			id, err := newRequestID()
			if err != nil {
				t.Fatal(err)
			}
			_, err = createThroughHub(stepCtx(t), env, protocol.CreateParams{Cwd: ws, Provider: "cursor", RequestID: id})
			var perr *protocol.Error
			if !errors.As(err, &perr) || perr.Data.Code != protocol.CodeNotAccepting || perr.Data.Reason != protocol.ReasonStartFailed {
				t.Fatalf("the create: %v, want refused start_failed (hub log: %s)", err, hubLog(env)())
			}
			want := a6Cause
			if tc.hint {
				pids := kids.pids()
				if len(pids) != 1 {
					t.Fatalf("hub children %v, want one", pids)
				}
				want += hubHint(pids[0])
			}
			if perr.Data.Cause != want || perr.Message != "the session did not start: "+want {
				t.Fatalf("data.cause %q\nmessage    %q\nwant cause %q", perr.Data.Cause, perr.Message, want)
			}
			stdout, stderr, code := executeErr([]string{"new", "-C", ws, "--provider", "cursor"})
			if code != 1 || stdout != "" || stderr != "craze new: the session did not start: "+want+"\n" {
				t.Fatalf("craze new exited %d: stdout %q, stderr %q", code, stdout, stderr)
			}
			// --json (plan 035 P5, A10): the same refusal on stdout, whole, so
			// a script reads .error.data.cause where it reads it on the wire.
			stdout, stderr, code = executeErr([]string{"new", "--json", "-C", ws, "--provider", "cursor"})
			var got struct {
				Error protocol.Error `json:"error"`
			}
			if code != 1 || stderr != "craze new: the session did not start: "+want+"\n" || strings.Count(stdout, "\n") != 1 ||
				json.Unmarshal([]byte(stdout), &got) != nil || got.Error.Data.Code != protocol.CodeNotAccepting ||
				got.Error.Data.Reason != protocol.ReasonStartFailed || got.Error.Data.Cause != want {
				t.Fatalf("craze new --json exited %d: stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
}
