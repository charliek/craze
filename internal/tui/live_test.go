package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/rundir"
)

func TestWiredFakeAgentStreamFollowUpQuit(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := t.TempDir()
	sess := agent.New(agent.Options{
		Binary:    bin,
		ExtraArgs: []string{"-script=followup"},
		Workspace: ws,
		Force:     true,
		Stderr:    io.Discard,
	})
	if err := sess.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !processRunning(t, bin) {
		t.Fatal("expected fake-agent child after Start")
	}

	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "default"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	// startedMsg and not m.started = true: the session was started above, and the
	// message is how the model — and the engine it drives, whose gate opens on the
	// same fact — learns it.
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)

	// Two whole turns against the real agent, driven through the runtime: the
	// prompt runs where the runtime runs it, its events come back on the
	// session's own stream, and the wait is for the reply on screen and the turn
	// being over — not for a message this test wrote.
	m = pumpEnter(t, m, "one")
	if m.status != statusWorking {
		t.Fatalf("the send was refused: status %s", m.status)
	}
	m = pumpUntil(t, m, allOf(isIdle, viewHas("first reply")))
	m = pumpSettled(t, m)

	m = pumpEnter(t, m, "two")
	m = pumpUntil(t, m, allOf(isIdle, viewHas("second reply")))
	m = pumpSettled(t, m)

	tm, qcmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting || qcmd == nil {
		t.Fatal("expected quit")
	}
	msg := runCmd(qcmd)
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("quit cmd returned %T, want tea.QuitMsg", msg)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(t, bin) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake-agent child still running after quit/close")
}

func TestWiredQuitWhileWorkingReapsChild(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := t.TempDir()
	sess := agent.New(agent.Options{
		Binary:    bin,
		ExtraArgs: []string{"-script=hang"},
		Workspace: ws,
		Force:     true,
		Stderr:    io.Discard,
	})
	if err := sess.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "default"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	m.input.SetValue("hang")
	// The turn runs on the engine's own goroutine now, so Enter returns no
	// command for the prompt: what says it is running is the status, and what
	// waits for it is the engine's Close, which joins its continuations.
	tm, _ = m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatal("expected a working prompt")
	}

	tm, qcmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting || qcmd == nil {
		t.Fatal("expected quit")
	}

	quitDone := make(chan tea.Msg, 1)
	go func() { quitDone <- runCmd(qcmd) }()
	select {
	case msg := <-quitDone:
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Fatalf("quit cmd returned %T, want tea.QuitMsg", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quit cmd hung waiting for cancel")
	}
	// The quit closed the engine, which joins the turn it was running, so a quit
	// that returned is a prompt that returned — a stronger statement than the
	// separate wait this used to make on the prompt's own command.

	if processRunning(t, bin) {
		t.Fatal("fake-agent child still running after quit while working")
	}
}

// TestWiredFakeAgentTurnFailDrawsOneErrorRow is #20's regression against the real
// wire: a turn that fails reaches the TUI as the session's own EventError and
// then as the engine's EventTurn{ended} carrying the same failure, and exactly
// one row is drawn for it — the event's, because a non-synthetic ending draws
// none and only settles the status.
//
// It is driven through the runtime rather than hand-fed. Its old subject was the
// delivery order of two racing endings, which no longer race: the engine settles
// the turn once its continuation has returned, after the session has published
// everything. pumpSettled is the barrier that makes "exactly one row" a claim
// about a finished turn.
func TestWiredFakeAgentTurnFailDrawsOneErrorRow(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := t.TempDir()
	sess := agent.New(agent.Options{
		Binary:    bin,
		ExtraArgs: []string{"-script=turnfail"},
		Workspace: ws,
		Force:     true,
		Stderr:    io.Discard,
	})
	if err := sess.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Nothing here quits, so the child is reaped by hand: a fake left running
	// would still be in the process table when another wired test asks
	// processRunning whether its own child was reaped. The pump's cleanup closes
	// the engine, which closes this session too; both are idempotent.
	t.Cleanup(func() { _ = sess.Close() })

	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "default"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)

	m = pumpEnter(t, m, "go")
	if m.status != statusWorking {
		t.Fatalf("the send was refused: status %s", m.status)
	}
	m = pumpUntil(t, m, allOf(isErrored, errorRows(1)))
	m = pumpSettled(t, m)

	got := texts(m, entryError)
	if len(got) != 1 {
		t.Fatalf("want exactly one error row, got %d: %q", len(got), got)
	}
	const want = "json-rpc error -32000: the turn failed"
	if got[0] != want {
		t.Fatalf("row text %q, want %q", got[0], want)
	}
	if m.err != want {
		t.Fatalf("m.err %q, want %q", m.err, want)
	}
}

// TestWiredModelChangeGoesThroughTheModelOption is r25 finding 3's last gap,
// closed against the real wire: a model the catalog keeps in a config option,
// changed by `/model`, end to end over ACP and through the engine, against an
// agent that really does refuse session/set_model (-32601, the live shape for
// a method an agent does not implement).
//
// It was first written for `/model`'s own SetModel → SetConfig fallback. Since
// plan 025 (design 2) the session's SetModel sets such a model through its
// option, so the one Set lands by session/set_config_option, and the TUI has
// no fallback left at all (astra r7 item 1). What it pins is the chain that
// fallback was for: set_config_option moving the model, the session moving
// CurrentModel with it and publishing both sections, and the model's own
// mirror ending on the new model rather than being put back by the
// refreshSnap that every meta triggers.
func TestWiredModelChangeGoesThroughTheModelOption(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := t.TempDir()
	sess := agent.New(agent.Options{
		Binary:    bin,
		ExtraArgs: []string{"-script=modelconfig-refuse"},
		Workspace: ws,
		Force:     true,
		Stderr:    io.Discard,
	})
	if err := sess.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Nothing here quits, so the child is reaped by hand; the pump's cleanup
	// closes the engine, which closes this session too, and both are idempotent.
	t.Cleanup(func() { _ = sess.Close() })

	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "default"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = applyPending(t, tm.(Model))
	if opt := agent.ModelConfigOption(m.snap); opt == nil || opt.Current != "default" {
		t.Fatalf("the agent advertised %+v, want the model option at its starting value", opt)
	}
	if m.snap.CurrentModel != "default" {
		t.Fatalf("the session started on %q", m.snap.CurrentModel)
	}

	m = pumpEnter(t, m, "/model composer")
	m = pumpSettled(t, m)

	if got := m.snap.CurrentModel; got != "composer" {
		t.Fatalf("the screen ended on %q, want the model the option was set to", got)
	}
	if m.model != "composer" {
		t.Fatalf("the status row says %q", m.model)
	}
	snap := sess.Snapshot()
	if snap.CurrentModel != "composer" {
		t.Fatalf("the session's own model is %q: a config-backed model change IS a model change", snap.CurrentModel)
	}
	// The proof that the OPTION is what moved it: session/set_model is
	// refused, so the only thing that can have set it is session/set_config_option.
	if opt := agent.ModelConfigOption(snap); opt == nil || opt.Current != "composer" {
		t.Fatalf("the model option is %+v, so the change never reached it", opt)
	}
	if rows := texts(m, entryError); len(rows) != 0 {
		t.Fatalf("the change succeeded, so nothing is the user's to see: %q", rows)
	}
}

// TestMain records the environment before any test rewrites HOME, so the one
// fake-agent build below keeps the developer's warm build cache. It also forces
// a true-colour profile: `go test` has no TTY, so lipgloss would otherwise
// render every theme as no colour at all and the raw-ANSI assertions in
// theme_test.go would pass against an empty palette. And it installs the
// parity watch (parity_test.go), so every fold of every test in the package is
// held against a model folded from the same events (plan 024 A11); a broken
// rule fails the test that broke it, and the run.
//
// It also pins the command gate's mode for the package (plan 027 §3.12 "Unit
// tests"): a model a test builds runs its gated calls inline, today's control
// flow, so a test that drives Update directly sees Enter's effect in the
// Update that pressed it. The frame runner takes its mode from FrameOpts, and
// every frame golden runs in both (runFrameModes); the pump and a test that
// asks (asyncGate) run asynchronously. And it installs the gate's
// invisibility watch (gate_test.go), so every gate any test opens is held
// still from its issuing Update to its reply.
//
// And it turns the Stub's install on for the package (plan 027 §3.13, "The
// Stub publishes what the live session would"): every Stub a test here builds
// publishes, at Start, the install delta a live session's Start publishes, so
// the fold holds the settings a live session's stream would give it. Other
// packages' Stubs, and production's, keep today's Start; a test here that
// needs it says so by setting InstallOnStart false on its Stub.
func TestMain(m *testing.M) {
	// The shipped model catalog brings its providers' variable names into
	// every native table (plan 031 §3.13, astra 13): a key exported in the
	// developer's shell would otherwise reach every session and every frame
	// built from pristineEnv, so they are unset before it is captured.
	unsetCatalogEnv()
	// The ChatGPT plan's endpoints (plan 033 §3.10) are aimed at a loopback
	// port nothing serves, for the package and every child it starts, as
	// internal/cli's are: /connect's sign-in tests stand in for chatgptauth
	// through connect_signin.go's seams or serve a fake issuer of their own
	// (t.Setenv over this), and a path that reached the real package
	// anyway — a native session's model refresh, a sign-in left at its
	// production seam — gets a refused connection on this machine instead of
	// reaching OpenAI (common.md: fake servers only).
	_ = os.Setenv(chatgptauth.IssuerEnv, chatgptFence)
	_ = os.Setenv(chatgptauth.APIEnv, chatgptFence+"/v1")
	// The macOS login session is pinned to the GUI's for the package and
	// every child it starts, as internal/cli's is (plan 036): the provider
	// availability check marks cursor unavailable outside it, so a Mac runner
	// whose session is not the GUI one would otherwise see a fake cursor
	// unavailable. Set before pristineEnv is captured, so frames built from
	// it carry it too; a test about the session sets rundir.GUISessionEnv
	// itself, over this.
	_ = os.Setenv(rundir.GUISessionEnv, "1")
	// Every model here looks for its workspace's repository no higher than
	// the test's own temporary directory (ownTempRoot): a .git above it —
	// /tmp/.git, which another program can leave — would otherwise put a
	// branch in every status row and frame built on a temporary workspace
	// (plan 037). Set before any test runs, and never again.
	gitBoundary = ownTempRoot(filepath.Clean(os.TempDir()))
	pristineEnv = os.Environ()
	lipgloss.SetColorProfile(termenv.TrueColor)
	installParityWatch()
	installPaintWatch()
	gateSyncDefault = true
	stubInstallOnStartDefault = true
	installGateWatch()
	installFrameSocketHost()
	installFrameProductions()
	// No test may shell out to xclip, overwrite the developer's clipboard or
	// read it. The seam itself stays real so the OSC 52 bytes are still
	// asserted; only the native tools are stubbed out, and the tests that care
	// install their own.
	swapClipboardSeams(io.Discard,
		func(string) error { return nil },
		func() (string, error) { return "", nil })
	// The clipboard's image half (plan 033 §3.3) the same way: Ctrl+V reads
	// it first, and no test may run wl-paste, xclip or osascript.
	swapImagePaste(noClipboardImage)
	// The composer `@` search's fake rg and git, written before any test's
	// goroutine can fork (installAtFakeTools says why).
	if err := installAtFakeTools(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	parity.report()
	reportSocketRuns()
	if err := parity.err(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := gateWatch.err(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := paints.err(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if code == 0 {
		if err := goldenCoverage(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	if fakeAgentDir != "" {
		_ = os.RemoveAll(fakeAgentDir)
	}
	_ = os.RemoveAll(atFakeBin)
	os.Exit(code)
}

// TestTheLoginSessionIsPinned (plan 036): TestMain pins the macOS login
// session to the GUI's for the package, so the provider availability check
// never marks a fake cursor unavailable for the machine's own session, on
// either OS: known, and the GUI's, here too where it would not be known.
func TestTheLoginSessionIsPinned(t *testing.T) {
	if _, gui, known := rundir.GUISession(); !gui || !known {
		t.Fatalf("the package's login session: gui %v, known %v; want the GUI's, pinned by TestMain (%s=1)", gui, known, rundir.GUISessionEnv)
	}
}

// unsetCatalogEnv unsets every variable the shipped model catalog takes a key
// from (plan 031 §3.13), and stops the run if there are none to unset — a
// catalog that names none is broken, and the scrub would silently do nothing.
func unsetCatalogEnv() {
	names := modeltable.CatalogEnvNames()
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "FAIL: the shipped model catalog names no variables to unset")
		os.Exit(1)
	}
	for _, name := range names {
		_ = os.Unsetenv(name)
	}
}

var (
	pristineEnv   []string
	fakeAgentOnce sync.Once
	fakeAgentDir  string
	fakeAgentBin  string
	fakeAgentErr  error
)

// buildFakeAgent builds the scripted ACP server once per test binary: several
// suites spawn it and a rebuild per test dominates the run.
func buildFakeAgent(t *testing.T) string {
	t.Helper()
	fakeAgentOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			fakeAgentErr = errors.New("runtime.Caller")
			return
		}
		root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		fakeAgentDir, fakeAgentErr = os.MkdirTemp("", "craze-fake-agent")
		if fakeAgentErr != nil {
			return
		}
		fakeAgentBin = filepath.Join(fakeAgentDir, "craze-fake-agent-wire")
		cmd := exec.Command("go", "build", "-o", fakeAgentBin, "./cmd/craze-fake-agent")
		cmd.Dir = root
		cmd.Env = pristineEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeAgentErr = fmt.Errorf("build fake agent: %w\n%s", err, out)
		}
	})
	if fakeAgentErr != nil {
		t.Fatal(fakeAgentErr)
	}
	return fakeAgentBin
}

// processRunning asks whether anything in the process table still has bin in
// its argv, which is how the leak checks above see a child craze failed to
// reap. Only Linux has /proc; everywhere else ps is the one portable view of
// another process's argv.
//
// It takes t so that failing to read the process table is a test failure and
// not a quiet "nothing is running": every caller reads a false as proof the
// child is gone, so an errored ps would turn each of these assertions green
// without having looked at anything.
func processRunning(t *testing.T, bin string) bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		out, err := exec.Command("ps", "-axww", "-o", "args=").Output()
		if err != nil {
			t.Fatalf("ps: %v", err)
		}
		return bytes.Contains(out, []byte(bin))
	}
	entries, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Fatalf("glob /proc: %v", err)
	}
	for _, p := range entries {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(bin)) {
			return true
		}
	}
	return false
}
