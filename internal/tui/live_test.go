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
	m.started = true

	// Two whole turns against the real agent, driven through the runtime: the
	// prompt runs where the runtime runs it, its events come back on the
	// session's own stream, and the wait is for the reply on screen and the turn
	// being over — not for a message this test wrote.
	m = pumpEnter(t, m, "one")
	if m.status != statusWorking {
		t.Fatalf("the send was refused: status %s", m.status)
	}
	m = pumpUntil(t, m, allOf(isIdle, viewHas("first reply")))

	m = pumpEnter(t, m, "two")
	m = pumpUntil(t, m, allOf(isIdle, viewHas("second reply")))

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
	m.started = true
	m.input.SetValue("hang")
	tm, promptCmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || promptCmd == nil {
		t.Fatal("expected a working prompt")
	}
	promptDone := make(chan struct{})
	go func() {
		defer close(promptDone)
		_ = runCmd(promptCmd)
	}()

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

	select {
	case <-promptDone:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not return after quit")
	}

	if processRunning(t, bin) {
		t.Fatal("fake-agent child still running after quit while working")
	}
}

// TestWiredFakeAgentTurnFailDrawsOneErrorRow is #20's regression: Prompt
// both emits EventError and returns the same error, so the TUI hears about one
// failure twice — the eventMsg from waitEvent and the promptDoneMsg from the
// prompt Cmd — and must draw exactly one row for it.
//
// It stays hand-fed. Its subject is the delivery order of those two endings, and
// an errored turn has no "the turn is over" a test can wait for the way an
// ordinary one has its idle status: nothing on screen changes when the second
// ending lands, so a pumped wait could return before it had.
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
	// processRunning whether its own child was reaped.
	t.Cleanup(func() { _ = sess.Close() })

	m := New(Config{Session: sess, Workspace: ws, Yolo: true, Model: "default"})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	m.started = true

	m.input.SetValue("go")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if cmd == nil {
		t.Fatal("expected prompt cmd")
	}
	// Prompt emits EventError before it returns, so once the Cmd has run the
	// event is already on the channel. Event first, then promptDoneMsg: the
	// order that used to draw the row twice.
	doneMsg := runCmd(cmd)
	m = drainEvents(t, m, sess)
	tm, _ = m.Update(doneMsg)
	m = tm.(Model)

	if m.status != statusError {
		t.Fatalf("status %s", m.status)
	}
	got := texts(m, entryError)
	if len(got) != 1 {
		t.Fatalf("want exactly one error row, got %d: %q", len(got), got)
	}
	const want = "json-rpc error -32000: the turn failed"
	if got[0] != want {
		t.Fatalf("row text %q, want %q", got[0], want)
	}
}

func drainEvents(t *testing.T, m Model, sess agent.Session) Model {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-sess.Events():
			tm, _ := m.Update(eventMsg{ev})
			m = tm.(Model)
			if ev.Type == agent.EventDone || ev.Type == agent.EventError {
				return m
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("timed out draining events")
	return m
}

// TestMain records the environment before any test rewrites HOME, so the one
// fake-agent build below keeps the developer's warm build cache. It also forces
// a true-colour profile: `go test` has no TTY, so lipgloss would otherwise
// render every theme as no colour at all and the raw-ANSI assertions in
// theme_test.go would pass against an empty palette.
func TestMain(m *testing.M) {
	pristineEnv = os.Environ()
	lipgloss.SetColorProfile(termenv.TrueColor)
	// No test may shell out to xclip, overwrite the developer's clipboard or
	// read it. The seam itself stays real so the OSC 52 bytes are still
	// asserted; only the native tools are stubbed out, and the tests that care
	// install their own.
	swapClipboardSeams(io.Discard,
		func(string) error { return nil },
		func() (string, error) { return "", nil })
	code := m.Run()
	if fakeAgentDir != "" {
		_ = os.RemoveAll(fakeAgentDir)
	}
	os.Exit(code)
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
